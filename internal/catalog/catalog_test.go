package catalog_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/migrate"
)

func ptr(n int) *int { return &n }

func small() catalog.Flavor {
	return catalog.Flavor{Code: "db-small", ServiceType: catalog.PostgreSQL, Name: "Small", StorageGB: 2, MaxConnections: ptr(20)}
}

func medium() catalog.Flavor {
	return catalog.Flavor{Code: "db-medium", ServiceType: catalog.PostgreSQL, Name: "Medium", StorageGB: 5, MaxConnections: ptr(30)}
}

// TestSeedIsQ12 pins flavors.json to the decision it comes from
// (open-questions.md Q12). Changing one means changing the other.
func TestSeedIsQ12(t *testing.T) {
	got, err := catalog.Seed()
	if err != nil {
		t.Fatal(err)
	}
	if want := []catalog.Flavor{small(), medium()}; !reflect.DeepEqual(got, want) {
		t.Errorf("seed = %+v, want %+v", got, want)
	}
}

func codes(t *testing.T, db catalog.DB) []string {
	t.Helper()
	fs, err := catalog.ActiveFlavors(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func TestSync(t *testing.T) {
	db := dbtest.Empty(t)
	ctx := context.Background()
	mig := db.ConnAs(dbtest.RoleMigrator)
	applyMigrations(t, mig)

	changes, err := catalog.Sync(ctx, mig, []catalog.Flavor{small(), medium()})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"added flavor db-small", "added flavor db-medium"}; !slices.Equal(changes, want) {
		t.Errorf("first sync = %q, want %q", changes, want)
	}

	// Idempotent: the same file again changes nothing.
	if changes, err := catalog.Sync(ctx, mig, []catalog.Flavor{small(), medium()}); err != nil || len(changes) != 0 {
		t.Errorf("second sync = %q, %v; want no changes", changes, err)
	}

	// Dropped from the file: deactivated, not deleted.
	if changes, err := catalog.Sync(ctx, mig, []catalog.Flavor{small()}); err != nil ||
		!slices.Equal(changes, []string{"deactivated flavor db-medium"}) {
		t.Errorf("drop medium = %q, %v", changes, err)
	}
	api := db.ConnAs(dbtest.RoleAPI)
	if got := codes(t, api); !slices.Equal(got, []string{"db-small"}) {
		t.Errorf("active = %q, want db-small only", got)
	}
	var rows int
	if err := mig.QueryRow(ctx, `SELECT count(*) FROM flavors`).Scan(&rows); err != nil || rows != 2 {
		t.Errorf("rows = %d, %v; want 2 (nothing deleted)", rows, err)
	}

	// Listed again, and renamed: both reported, in one sync.
	renamed := medium()
	renamed.Name = "Medium (5 GB)"
	if changes, err := catalog.Sync(ctx, mig, []catalog.Flavor{small(), renamed}); err != nil ||
		!slices.Equal(changes, []string{`renamed flavor db-medium: "Medium" → "Medium (5 GB)"`, "reactivated flavor db-medium"}) {
		t.Errorf("rename and reactivate = %q, %v", changes, err)
	}
	if got := codes(t, api); !slices.Equal(got, []string{"db-small", "db-medium"}) {
		t.Errorf("active = %q, want both, smallest first", got)
	}
}

func TestSyncRefusesChangedSizes(t *testing.T) {
	db := dbtest.New(t) // synced from flavors.json
	ctx := context.Background()
	mig := db.ConnAs(dbtest.RoleMigrator)

	bigger := small()
	bigger.StorageGB = 3
	newOne := catalog.Flavor{Code: "db-large", ServiceType: catalog.PostgreSQL, Name: "Large", StorageGB: 10, MaxConnections: ptr(60)}
	_, err := catalog.Sync(ctx, mig, []catalog.Flavor{bigger, medium(), newOne})
	if !errors.Is(err, catalog.ErrSizeChanged) || !strings.Contains(err.Error(), "db-small") {
		t.Fatalf("err = %v, want ErrSizeChanged naming db-small", err)
	}
	// One transaction: the new flavor listed alongside wasn't added either.
	if got := codes(t, db.ConnAs(dbtest.RoleAPI)); !slices.Equal(got, []string{"db-small", "db-medium"}) {
		t.Errorf("active = %q, want the catalog unchanged", got)
	}
}

func TestSyncRefusesBadFlavors(t *testing.T) {
	db := dbtest.New(t)
	mig := db.ConnAs(dbtest.RoleMigrator)
	withVCPU := small()
	withVCPU.VCPU = ptr(1)
	noConns := medium()
	noConns.MaxConnections = nil
	unknown := small()
	unknown.ServiceType = "redis"
	badCode := small()
	badCode.Code = "DB_Small"

	for name, fs := range map[string][]catalog.Flavor{
		"twice":           {small(), small()},
		"vcpu on shared":  {withVCPU},
		"no connections":  {noConns},
		"unknown type":    {unknown},
		"bad code":        {badCode},
		"no storage":      {{Code: "db-zero", ServiceType: catalog.PostgreSQL, Name: "Zero", MaxConnections: ptr(1)}},
		"no name":         {{Code: "db-x", ServiceType: catalog.PostgreSQL, StorageGB: 1, MaxConnections: ptr(1)}},
		"zero connection": {{Code: "db-x", ServiceType: catalog.PostgreSQL, Name: "X", StorageGB: 1, MaxConnections: ptr(0)}},
	} {
		if _, err := catalog.Sync(context.Background(), mig, fs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFlavorPrivileges(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	for _, role := range []string{dbtest.RoleAPI, dbtest.RoleWorker, dbtest.RoleAdmin} {
		conn := db.ConnAs(role)
		_, err := conn.Exec(ctx, `UPDATE flavors SET storage_gb = 100`)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: UPDATE flavors = %v, want permission denied", role, err)
		}
	}
	// Only otter_api reads the catalog.
	for _, role := range []string{dbtest.RoleWorker, dbtest.RoleAdmin} {
		if _, err := catalog.ActiveFlavors(ctx, db.ConnAs(role)); err == nil {
			t.Errorf("%s: read flavors, want permission denied", role)
		}
	}
}

// applyMigrations leaves the schema as otter migrate would, without the
// catalog sync dbtest.New adds, so the flavors table starts empty.
func applyMigrations(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ms, err := migrate.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Apply(context.Background(), conn, ms); err != nil {
		t.Fatal(err)
	}
}
