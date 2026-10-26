package migrate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/migrate"
)

func TestMigrations_EmbeddedAreWellFormed(t *testing.T) {
	ms, err := migrate.Migrations()
	if err != nil {
		t.Fatalf("Migrations() error = %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no migrations embedded")
	}
}

func TestLoad_RejectsBadSets(t *testing.T) {
	tests := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{"gap", fstest.MapFS{"0001_a.sql": {}, "0003_c.sql": {}}, "without gaps"},
		{"repeat", fstest.MapFS{"0001_a.sql": {}, "0001_b.sql": {}}, "without gaps"},
		{"starts at 2", fstest.MapFS{"0002_b.sql": {}}, "expected 0001"},
		{"bad name", fstest.MapFS{"1_a.sql": {}}, "is not named"},
		{"upper case", fstest.MapFS{"0001_Add.sql": {}}, "is not named"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := migrate.Load(tt.files)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestApply_AppliesInOrderAndRecords(t *testing.T) {
	db := dbtest.Empty(t)
	conn := db.ConnAs(dbtest.RoleMigrator)
	ctx := context.Background()
	ms, err := migrate.Migrations()
	if err != nil {
		t.Fatal(err)
	}

	done, err := migrate.Apply(ctx, conn, ms)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(done) != len(ms) {
		t.Errorf("applied %d migrations, want %d", len(done), len(ms))
	}

	status, err := migrate.List(ctx, conn, ms)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, s := range status {
		if s.AppliedAt == nil {
			t.Errorf("%04d_%s not recorded as applied", s.Version, s.Name)
		}
	}

	again, err := migrate.Apply(ctx, conn, ms)
	if err != nil {
		t.Fatalf("second Apply() error = %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Apply() applied %d migrations, want 0", len(again))
	}
}

func TestApply_FailedMigrationLeavesNothingBehind(t *testing.T) {
	db := dbtest.Empty(t)
	conn := db.ConnAs(dbtest.RoleMigrator)
	ctx := context.Background()
	ms, err := migrate.Load(fstest.MapFS{
		"0001_ok.sql":     {Data: []byte("CREATE TABLE a (id int);")},
		"0002_broken.sql": {Data: []byte("CREATE TABLE b (id int); SELECT * FROM missing;")},
	})
	if err != nil {
		t.Fatal(err)
	}

	done, err := migrate.Apply(ctx, conn, ms)
	if err == nil || !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("Apply() error = %v, want it to name 0002_broken", err)
	}
	if len(done) != 1 {
		t.Errorf("applied %d before the failure, want 1", len(done))
	}
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('b') IS NOT NULL").Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("table b exists: the failed migration was not rolled back")
	}
}

func TestApply_RefusesChangedOrUnknownMigrations(t *testing.T) {
	db := dbtest.Empty(t)
	conn := db.ConnAs(dbtest.RoleMigrator)
	ctx := context.Background()
	v1, _ := migrate.Load(fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id int);")},
		"0002_b.sql": {Data: []byte("CREATE TABLE b (id int);")},
	})
	if _, err := migrate.Apply(ctx, conn, v1); err != nil {
		t.Fatal(err)
	}

	edited, _ := migrate.Load(fstest.MapFS{
		"0001_a.sql": {Data: []byte("CREATE TABLE a (id bigint);")},
		"0002_b.sql": {Data: []byte("CREATE TABLE b (id int);")},
	})
	if _, err := migrate.Apply(ctx, conn, edited); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Errorf("edited: error = %v, want a changed-migration error", err)
	}

	older, _ := migrate.Load(fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id int);")}})
	if _, err := migrate.Apply(ctx, conn, older); err == nil || !strings.Contains(err.Error(), "does not know") {
		t.Errorf("older binary: error = %v, want an unknown-migration error", err)
	}
}

func TestApply_ConcurrentRunsDoNotInterleave(t *testing.T) {
	db := dbtest.Empty(t)
	ms, err := migrate.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	// Connections are opened up front: ConnAs may call t.Fatal, which a
	// goroutine must not.
	conns := []*pgx.Conn{db.ConnAs(dbtest.RoleMigrator), db.ConnAs(dbtest.RoleMigrator), db.ConnAs(dbtest.RoleMigrator)}
	var wg sync.WaitGroup
	counts := make([]int, len(conns))
	errs := make([]error, len(conns))
	for i := range conns {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			done, err := migrate.Apply(context.Background(), conns[i], ms)
			counts[i], errs[i] = len(done), err
		}(i)
	}
	wg.Wait()
	total := 0
	for i := range conns {
		if errs[i] != nil {
			t.Errorf("runner %d: %v", i, errs[i])
		}
		total += counts[i]
	}
	if total != len(ms) {
		t.Errorf("runners applied %d migrations in total, want exactly %d", total, len(ms))
	}
}

func TestPrivileges_APICannotRunDDL(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	for _, role := range []string{dbtest.RoleAPI, dbtest.RoleWorker} {
		t.Run(role, func(t *testing.T) {
			conn := db.ConnAs(role)
			for _, stmt := range []string{
				"CREATE TABLE intruder (id int)",
				"CREATE SCHEMA intruder",
				"SELECT * FROM schema_migrations",
			} {
				_, err := conn.Exec(ctx, stmt)
				var pgErr *pgconn.PgError
				if err == nil {
					t.Errorf("%s: %q succeeded, want permission denied", role, stmt)
				} else if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
					t.Errorf("%s: %q error = %v, want insufficient_privilege (42501)", role, stmt, err)
				}
			}
		})
	}
}

func TestPrivileges_PublicCannotConnect(t *testing.T) {
	db := dbtest.New(t)
	conn := db.ConnAs(dbtest.RoleMigrator)
	var publicCanConnect bool
	if err := conn.QueryRow(context.Background(),
		"SELECT has_database_privilege('public', current_database(), 'CONNECT')").Scan(&publicCanConnect); err != nil {
		t.Fatal(err)
	}
	if publicCanConnect {
		t.Error("PUBLIC can connect to the platform database")
	}
}
