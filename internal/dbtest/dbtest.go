// Package dbtest gives each test a fresh platform database on a real
// PostgreSQL (architecture.md §3: no repository mocks).
//
// OTTER_TEST_DATABASE_URL names a superuser connection to a throwaway
// server: CI provides one, and `make test-db` starts one locally. Without it
// the tests that need a database are skipped, except in CI, where they fail.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ottermq/otter_cloud/internal/migrate"
)

// EnvURL names the superuser connection the helpers use.
const EnvURL = "OTTER_TEST_DATABASE_URL"

// The platform roles, as deploy/platform-db/bootstrap.sql creates them.
const (
	RoleMigrator = "otter_migrator"
	RoleAPI      = "otter_api"
	RoleWorker   = "otter_worker"
)

// rolesLock serializes role creation across test packages, which run in
// parallel against the same server; roles are cluster-wide.
const rolesLock int64 = 0x6f74746572 + 1

// DB is a fresh database that exists for one test.
type DB struct {
	t    *testing.T
	Name string
	url  *url.URL // superuser, pointing at this database
}

// Empty creates a database owned by otter_migrator with nothing applied, for
// testing the migrations themselves. It is dropped when the test ends.
func Empty(t *testing.T) *DB {
	t.Helper()
	admin := adminURL(t)
	ctx := context.Background()

	conn := connect(t, admin)
	defer conn.Close(ctx)
	ensureRoles(t, conn)

	name := "otter_test_" + randomSuffix(t)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" OWNER "+RoleMigrator); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		c := connect(t, admin)
		defer c.Close(context.Background())
		if _, err := c.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database %s: %v", name, err)
		}
	})

	u := *admin
	u.Path = "/" + name
	return &DB{t: t, Name: name, url: &u}
}

// New creates a database with every migration applied, as otter migrate
// would leave it.
func New(t *testing.T) *DB {
	t.Helper()
	db := Empty(t)
	conn := db.ConnAs(RoleMigrator)
	ms, err := migrate.Migrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if _, err := migrate.Apply(context.Background(), conn, ms); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// URL is a superuser connection URL for this database. It may carry the test
// server's password.
func (db *DB) URL() *url.URL {
	u := *db.url
	return &u
}

// ConnAs opens a connection to the database acting as role, closed when the
// test ends. It connects as the superuser and then SET ROLE, so privileges
// are checked exactly as for that role, without the test needing its
// password.
func (db *DB) ConnAs(role string) *pgx.Conn {
	db.t.Helper()
	conn := connect(db.t, db.url)
	db.t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(context.Background(), "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		db.t.Fatalf("SET ROLE %s: %v", role, err)
	}
	return conn
}

// PoolAs opens a connection pool acting as role, closed when the test ends.
// Every connection runs SET ROLE as it is made, like ConnAs.
func (db *DB) PoolAs(role string) *pgxpool.Pool {
	db.t.Helper()
	pc, err := pgxpool.ParseConfig(db.url.String())
	if err != nil {
		db.t.Fatal(err)
	}
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		db.t.Fatalf("pool as %s: %v", role, err)
	}
	db.t.Cleanup(pool.Close)
	return pool
}

func adminURL(t *testing.T) *url.URL {
	t.Helper()
	raw := os.Getenv(EnvURL)
	if raw == "" {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s is not set in CI", EnvURL)
		}
		t.Skipf("%s is not set; run `make test-db` and export it to run database tests", EnvURL)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s is not a valid URL", EnvURL)
	}
	return u
}

func connect(t *testing.T, u *url.URL) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	return conn
}

func ensureRoles(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", rolesLock); err != nil {
		t.Fatal(err)
	}
	for role, login := range map[string]bool{RoleMigrator: true, RoleAPI: true, RoleWorker: false} {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			continue
		}
		attr := "NOLOGIN"
		if login {
			attr = "LOGIN"
		}
		if _, err := tx.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+attr); err != nil {
			t.Fatalf("create role %s: %v", role, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(hex.EncodeToString(b))
}
