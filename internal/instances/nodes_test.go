package instances_test

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
)

func node(name, subnet string) instances.Node {
	return instances.Node{Name: name, CapacityMemoryMB: 8192, CapacityStorageGB: 200,
		TenantSubnet: netip.MustParsePrefix(subnet), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}
}

func cluster(name, node, addr string) instances.Cluster {
	return instances.Cluster{Name: name, Node: node, PGMajor: 18, JailName: "otc-" + name,
		PoolAddr: netip.MustParseAddr(addr), PoolPort: 6432, MemoryMB: 2048,
		CapacityStorageGB: 60, CapacityConnections: 300, CapacityDatabases: 12}
}

func names(cs []instances.Cluster) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func TestAddNodeAndCluster(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	admin := db.ConnAs(dbtest.RoleAdmin)

	n, err := instances.AddNode(ctx, admin, node("node-01", "10.64.0.0/22"))
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == "" || n.Status != instances.StatusActive {
		t.Errorf("node = %+v", n)
	}
	c, err := instances.AddCluster(ctx, admin, cluster("pgc1", "node-01", "10.64.0.10"))
	if err != nil {
		t.Fatal(err)
	}
	if c.NodeID != n.ID || c.Status != instances.StatusActive {
		t.Errorf("cluster = %+v", c)
	}

	nodes, err := instances.ListNodes(ctx, admin)
	if err != nil || len(nodes) != 1 || nodes[0].TenantSubnet.String() != "10.64.0.0/22" ||
		!slices.Equal(nodes[0].ServiceTypes, []catalog.ServiceType{catalog.PostgreSQL}) {
		t.Errorf("nodes = %+v, %v", nodes, err)
	}
	clusters, err := instances.ListClusters(ctx, admin)
	if err != nil || len(clusters) != 1 || clusters[0].Node != "node-01" || clusters[0].PoolAddr.String() != "10.64.0.10" {
		t.Errorf("clusters = %+v, %v", clusters, err)
	}
}

func TestAddNodeRefuses(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	admin := db.ConnAs(dbtest.RoleAdmin)
	if _, err := instances.AddNode(ctx, admin, node("node-01", "10.64.0.0/22")); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.AddNode(ctx, admin, node("node-01", "10.64.4.0/22")); !errors.Is(err, instances.ErrNameTaken) {
		t.Errorf("same name: %v", err)
	}
	if _, err := instances.AddNode(ctx, admin, node("node-02", "10.64.2.0/24")); !errors.Is(err, instances.ErrSubnetOverlaps) {
		t.Errorf("overlapping subnet: %v", err)
	}
	if _, err := instances.AddNode(ctx, admin, node("node-02", "10.64.4.0/22")); err != nil {
		t.Errorf("next subnet: %v", err)
	}

	hostBits := node("node-03", "10.64.8.0/22")
	hostBits.TenantSubnet = netip.MustParsePrefix("10.64.8.1/22")
	vm := node("node-03", "10.64.8.0/22")
	vm.ServiceTypes = []catalog.ServiceType{"vm"}
	for name, n := range map[string]instances.Node{
		"bad name":      node("Node_03", "10.64.8.0/22"),
		"host bits":     hostBits,
		"unknown type":  vm,
		"no types":      {Name: "node-03", TenantSubnet: netip.MustParsePrefix("10.64.8.0/22")},
		"negative size": {Name: "node-03", CapacityMemoryMB: -1, TenantSubnet: netip.MustParsePrefix("10.64.8.0/22"), ServiceTypes: vm.ServiceTypes},
	} {
		if _, err := instances.AddNode(ctx, admin, n); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAddClusterRefuses(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	admin := db.ConnAs(dbtest.RoleAdmin)
	if _, err := instances.AddNode(ctx, admin, node("node-01", "10.64.0.0/22")); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.AddCluster(ctx, admin, cluster("pgc1", "node-01", "10.64.0.10")); err != nil {
		t.Fatal(err)
	}

	sameAddr := cluster("pgc2", "node-01", "10.64.0.10")
	if _, err := instances.AddCluster(ctx, admin, sameAddr); !errors.Is(err, instances.ErrAddressTaken) {
		t.Errorf("same pool address: %v", err)
	}
	sameJail := cluster("pgc2", "node-01", "10.64.0.11")
	sameJail.JailName = "otc-pgc1"
	if _, err := instances.AddCluster(ctx, admin, sameJail); !errors.Is(err, instances.ErrJailTaken) {
		t.Errorf("same jail: %v", err)
	}
	if _, err := instances.AddCluster(ctx, admin, cluster("pgc1", "node-01", "10.64.0.11")); !errors.Is(err, instances.ErrNameTaken) {
		t.Errorf("same name: %v", err)
	}
	if _, err := instances.AddCluster(ctx, admin, cluster("pgc2", "node-09", "10.64.0.11")); !errors.Is(err, instances.ErrNodeNotFound) {
		t.Errorf("unknown node: %v", err)
	}
	if _, err := instances.AddCluster(ctx, admin, cluster("pgc2", "node-01", "10.65.0.11")); err == nil ||
		!strings.Contains(err.Error(), "outside") {
		t.Errorf("address outside the subnet: %v", err)
	}

	// The node has 8192 MB and 200 GB; pgc1 holds 2048 MB and 60 GB.
	tooMuchMemory := cluster("pgc2", "node-01", "10.64.0.11")
	tooMuchMemory.MemoryMB = 6145
	if _, err := instances.AddCluster(ctx, admin, tooMuchMemory); !errors.Is(err, instances.ErrNodeCapacity) {
		t.Errorf("memory oversold: %v", err)
	}
	tooMuchStorage := cluster("pgc2", "node-01", "10.64.0.11")
	tooMuchStorage.CapacityStorageGB = 141
	if _, err := instances.AddCluster(ctx, admin, tooMuchStorage); !errors.Is(err, instances.ErrNodeCapacity) {
		t.Errorf("storage oversold: %v", err)
	}
	exact := cluster("pgc2", "node-01", "10.64.0.11")
	exact.MemoryMB, exact.CapacityStorageGB = 6144, 140
	if _, err := instances.AddCluster(ctx, admin, exact); err != nil {
		t.Errorf("exactly the rest of the node: %v", err)
	}
}

// The acceptance of #43: placement never chooses a draining (or offline)
// node or cluster.
func TestEligibleClustersSkipDrainingAndOffline(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	admin := db.ConnAs(dbtest.RoleAdmin)
	api := db.ConnAs(dbtest.RoleAPI)
	for _, n := range []instances.Node{node("node-01", "10.64.0.0/22"), node("node-02", "10.64.4.0/22")} {
		if _, err := instances.AddNode(ctx, admin, n); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []instances.Cluster{
		cluster("pgc1", "node-01", "10.64.0.10"),
		cluster("pgc2", "node-01", "10.64.0.11"),
		cluster("pgc3", "node-02", "10.64.4.10"),
	} {
		if _, err := instances.AddCluster(ctx, admin, c); err != nil {
			t.Fatal(err)
		}
	}
	eligible := func() []string {
		t.Helper()
		cs, err := instances.EligibleClusters(ctx, api, catalog.PostgreSQL)
		if err != nil {
			t.Fatal(err)
		}
		got := names(cs)
		slices.Sort(got)
		return got
	}
	if got := eligible(); !slices.Equal(got, []string{"pgc1", "pgc2", "pgc3"}) {
		t.Fatalf("all active: %q", got)
	}

	c, err := instances.SetClusterStatus(ctx, admin, "pgc1", instances.StatusDraining)
	if err != nil || !c.Changed() || c.Previous != "active" {
		t.Fatalf("drain pgc1 = %+v, %v", c, err)
	}
	if got := eligible(); !slices.Equal(got, []string{"pgc2", "pgc3"}) {
		t.Errorf("pgc1 draining: %q", got)
	}
	if _, err := instances.SetNodeStatus(ctx, admin, "node-02", instances.StatusDraining); err != nil {
		t.Fatal(err)
	}
	if got := eligible(); !slices.Equal(got, []string{"pgc2"}) {
		t.Errorf("node-02 draining too: %q, want its active cluster skipped", got)
	}
	if _, err := instances.SetClusterStatus(ctx, admin, "pgc2", instances.StatusOffline); err != nil {
		t.Fatal(err)
	}
	if got := eligible(); len(got) != 0 {
		t.Errorf("nothing active: %q", got)
	}

	if _, err := instances.SetNodeStatus(ctx, admin, "node-02", instances.StatusActive); err != nil {
		t.Fatal(err)
	}
	if got := eligible(); !slices.Equal(got, []string{"pgc3"}) {
		t.Errorf("node-02 active again: %q", got)
	}
	again, err := instances.SetNodeStatus(ctx, admin, "node-02", instances.StatusActive)
	if err != nil || again.Changed() {
		t.Errorf("activate an active node = %+v, %v; want no change", again, err)
	}
	if _, err := instances.SetNodeStatus(ctx, admin, "node-09", instances.StatusDraining); !errors.Is(err, instances.ErrNodeNotFound) {
		t.Errorf("unknown node: %v", err)
	}
}

func TestNodePrivileges(t *testing.T) {
	db := dbtest.New(t)
	ctx := context.Background()
	if _, err := instances.AddNode(ctx, db.ConnAs(dbtest.RoleAdmin), node("node-01", "10.64.0.0/22")); err != nil {
		t.Fatal(err)
	}
	denied := func(role, sql string) {
		t.Helper()
		_, err := db.ConnAs(role).Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %s = %v, want permission denied", role, sql, err)
		}
	}
	denied(dbtest.RoleAPI, `UPDATE infrastructure_nodes SET status = 'draining'`)
	denied(dbtest.RoleAPI, `INSERT INTO infrastructure_nodes (name, capacity_memory_mb, capacity_vcpu, capacity_storage_gb, tenant_subnet, service_types)
		VALUES ('node-x', 1, 1, 1, '10.99.0.0/24', '{postgresql}')`)
	denied(dbtest.RoleAdmin, `UPDATE infrastructure_nodes SET capacity_storage_gb = 100000`)
	denied(dbtest.RoleAdmin, `UPDATE clusters SET capacity_connections = 100000`)
	denied(dbtest.RoleAdmin, `DELETE FROM infrastructure_nodes`)
	denied(dbtest.RoleWorker, `UPDATE infrastructure_nodes SET status = 'draining'`)
	// A worker reads only its own node (#47); otter_worker itself is none.
	var n int
	if err := db.ConnAs(dbtest.RoleWorker).QueryRow(ctx, `SELECT count(*) FROM infrastructure_nodes`).Scan(&n); err != nil || n != 0 {
		t.Errorf("otter_worker sees %d nodes, %v", n, err)
	}
}
