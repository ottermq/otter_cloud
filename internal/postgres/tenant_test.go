package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/postgres"
	"github.com/ottermq/otter_cloud/internal/scram"
)

// verifier is password's verifier, as otter api makes it: these tests
// are also what shows PostgreSQL accepts scram's verifiers.
func verifier(t *testing.T, password string) string {
	t.Helper()
	v, err := scram.Verifier(password)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// tenant is a fresh tenant name, its role and database dropped when the
// test ends: the test cluster outlives the tests.
func (c testCluster) tenant(password string) postgres.Tenant {
	c.t.Helper()
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	tn := postgres.Tenant{Name: "otc_" + hex.EncodeToString(id), Verifier: verifier(c.t, password), StorageGB: 2}
	c.t.Cleanup(func() {
		ctx := context.Background()
		conn := c.admin()
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+tn.Name+" WITH (FORCE)")
		_, _ = conn.Exec(ctx, "DROP ROLE IF EXISTS "+tn.Name)
	})
	return tn
}

func (c testCluster) admin() *pgx.Conn {
	c.t.Helper()
	conn, err := postgres.Connect(context.Background(), c.Target(), c.Creds("admin"))
	if err != nil {
		c.t.Fatal(err)
	}
	return conn
}

func (c testCluster) connectFunc() func(context.Context, string) (*pgx.Conn, error) {
	return func(ctx context.Context, db string) (*pgx.Conn, error) {
		return postgres.ConnectDB(ctx, c.Target(), c.Creds("admin"), db)
	}
}

func (c testCluster) ensure(conn *pgx.Conn, tn postgres.Tenant) error {
	ctx := context.Background()
	if err := postgres.EnsureRole(ctx, conn, tn); err != nil {
		return err
	}
	return postgres.EnsureDatabase(ctx, conn, c.connectFunc(), tn)
}

// asTenant logs in as the tenant, with its password, over TLS.
func (c testCluster) asTenant(tn postgres.Tenant, password, database string) (*pgx.Conn, error) {
	return c.login("port", tn, password, database)
}

// asTenantVia logs in through the listener whose port is in file: the
// pool's, for PgBouncer.
func (c testCluster) asTenantVia(file string, tn postgres.Tenant, password string) (*pgx.Conn, error) {
	return c.login(file, tn, password, tn.Name)
}

func (c testCluster) login(portFile string, tn postgres.Tenant, password, database string) (*pgx.Conn, error) {
	t := c.Target()
	t.Port, _ = strconv.Atoi(strings.TrimSpace(string(c.File(portFile))))
	u := url.URL{Scheme: "postgres", User: url.UserPassword(tn.Name, password), Host: net.JoinHostPort(t.Host, strconv.Itoa(t.Port)),
		Path: "/" + database, RawQuery: "sslmode=require"}
	return pgx.Connect(context.Background(), u.String())
}

// The acceptance of #51: idempotent; the role has none of the forbidden
// attributes; the per-database settings are applied.
func TestEnsureTenant(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	conn := c.admin()
	defer conn.Close(ctx)
	tn := c.tenant("correct horse battery staple")
	for run := range 2 {
		if err := c.ensure(conn, tn); err != nil {
			t.Fatalf("run %d: %v", run+1, err)
		}
	}

	var super, createdb, createrole, repl, bypass, login bool
	var limit int
	var settings []string
	if err := conn.QueryRow(ctx, `
		SELECT rolsuper, rolcreatedb, rolcreaterole, rolreplication, rolbypassrls, rolcanlogin, rolconnlimit,
			coalesce((SELECT setconfig FROM pg_db_role_setting WHERE setrole = r.oid AND setdatabase = 0), '{}')
		FROM pg_roles r WHERE rolname = $1`, tn.Name).Scan(&super, &createdb, &createrole, &repl, &bypass, &login, &limit, &settings); err != nil {
		t.Fatal(err)
	}
	if super || createdb || createrole || repl || bypass || !login || limit != postgres.ServerPool {
		t.Errorf("role: super %t createdb %t createrole %t replication %t bypassrls %t login %t limit %d",
			super, createdb, createrole, repl, bypass, login, limit)
	}
	slices.Sort(settings)
	if want := []string{"idle_in_transaction_session_timeout=5min", "statement_timeout=60s", "temp_file_limit=2GB"}; !slices.Equal(settings, want) {
		t.Errorf("settings = %v, want %v", settings, want)
	}
	var owner string
	var publicConnect bool
	if err := conn.QueryRow(ctx, `SELECT pg_get_userbyid(datdba), has_database_privilege('public', datname, 'CONNECT')
		FROM pg_database WHERE datname = $1`, tn.Name).Scan(&owner, &publicConnect); err != nil || owner != tn.Name || publicConnect {
		t.Errorf("database: owner %q, PUBLIC may connect %t, %v", owner, publicConnect, err)
	}

	// What the tenant sees, logged in with its password.
	db, err := c.asTenant(tn, "correct horse battery staple", tn.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	for name, want := range map[string]string{"temp_file_limit": "2GB", "statement_timeout": "1min", "idle_in_transaction_session_timeout": "5min"} {
		var got string
		if err := db.QueryRow(ctx, "SHOW "+name).Scan(&got); err != nil || got != want {
			t.Errorf("SHOW %s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := db.Exec(ctx, `CREATE TABLE notes (id int); INSERT INTO notes VALUES (1)`); err != nil {
		t.Errorf("the tenant can't use its own database: %v", err)
	}
	for _, sql := range []string{
		`SET temp_file_limit = '100GB'`,
		`CREATE ROLE intruder`,
		`CREATE DATABASE another`,
		`SELECT pg_read_file('/etc/passwd')`,
		`COPY notes TO '/tmp/notes'`,
		`ALTER ROLE ` + tn.Name + ` SET temp_file_limit = '100GB'`,
	} {
		if _, err := db.Exec(ctx, sql); err == nil {
			t.Errorf("tenant ran %s", sql)
		}
	}
	if _, err := c.asTenant(tn, "wrong password", tn.Name); err == nil {
		t.Error("the wrong password logged in")
	}
}

func TestEnsureTenantRefuses(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	conn := c.admin()
	defer conn.Close(ctx)

	for name, tn := range map[string]postgres.Tenant{
		"name":     {Name: "customer_db", Verifier: verifier(t, "x"), StorageGB: 2},
		"injected": {Name: "otc_0123456789ab", Verifier: "SCRAM-SHA-256$4096:abc$def:ghi' SUPERUSER --", StorageGB: 2},
		"storage":  {Name: "otc_0123456789ab", Verifier: verifier(t, "x"), StorageGB: 0},
	} {
		if err := postgres.EnsureRole(ctx, conn, tn); err == nil {
			t.Errorf("%s: accepted %+v", name, tn)
		}
	}

	// A role of a tenant's name that someone gave too much: never repaired.
	tn := c.tenant("x")
	if _, err := conn.Exec(ctx, "CREATE ROLE "+tn.Name+" LOGIN CREATEDB"); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureRole(ctx, conn, tn); !errors.Is(err, postgres.ErrUnsafeRole) || !strings.Contains(err.Error(), "createdb") {
		t.Errorf("role with CREATEDB: %v", err)
	}
	// Refused before anything was changed on it: no settings, and not
	// granted to otter_admin.
	var touched bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1)
		OR EXISTS (SELECT FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles u ON u.oid = m.member
			WHERE g.rolname = $1 AND u.rolname = 'otter_admin' AND m.inherit_option)`, tn.Name).Scan(&touched); err != nil {
		t.Fatal(err)
	}
	if touched {
		t.Error("the unsafe role was changed before it was refused")
	}
	other := c.tenant("x")
	if _, err := conn.Exec(ctx, "CREATE ROLE "+other.Name+" LOGIN; GRANT "+tn.Name+" TO "+other.Name); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureRole(ctx, conn, other); !errors.Is(err, postgres.ErrUnsafeRole) || !strings.Contains(err.Error(), "member of") {
		t.Errorf("role that is a member of another: %v", err)
	}

	// A database of the name owned by someone else.
	third := c.tenant("x")
	if err := postgres.EnsureRole(ctx, conn, third); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+third.Name); err != nil {
		t.Fatal(err)
	}
	if err := postgres.EnsureDatabase(ctx, conn, c.connectFunc(), third); !errors.Is(err, postgres.ErrUnsafeRole) {
		t.Errorf("database owned by otter_admin: %v", err)
	}
}
