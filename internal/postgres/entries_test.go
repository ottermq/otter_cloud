package postgres_test

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// PoolEntries lists what the cluster should serve: its instances that are
// neither deleted nor being deleted, read as the node's worker.
func TestPoolEntries(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	admin, api := db.ConnAs(dbtest.RoleAdmin), db.ConnAs(dbtest.RoleAPI)
	if _, err := instances.AddNode(ctx, admin, instances.Node{Name: "node-a", CapacityMemoryMB: 8192, CapacityStorageGB: 100,
		TenantSubnet: netip.MustParsePrefix("10.64.0.0/24"), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
		t.Fatal(err)
	}
	cl, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: "pgc1", Node: "node-a", PGMajor: 18, JailName: "otc-pgc1",
		PoolAddr: netip.MustParseAddr("10.64.0.10"), PoolPort: 6432, MemoryMB: 2048,
		CapacityStorageGB: 60, CapacityConnections: 300, CapacityDatabases: 12})
	if err != nil {
		t.Fatal(err)
	}
	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(ctx, admin, u.AccountID, "founder-beta"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range []string{"db-small", "db-medium"} {
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: u.AccountID, RequestedBy: "user:" + u.ID, Name: f, FlavorCode: f})
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		names = append(names, c.Instance.InfraName)
	}
	// The second is being deleted: its entry goes first.
	if _, err := db.ConnAs(dbtest.RoleMigrator).Exec(ctx, `UPDATE instances SET desired_state = 'deleted' WHERE infra_name = $1`, names[1]); err != nil {
		t.Fatal(err)
	}

	es, err := postgres.PoolEntries(ctx, db.ConnAs(db.NodeRole("node-a")), cl.ID)
	want := []postgres.PoolEntry{{Name: names[0], MaxConnections: 20, PoolMode: "transaction"}}
	if err != nil || !slices.Equal(es, want) {
		t.Errorf("PoolEntries = %+v, %v; want %+v", es, err, want)
	}
}
