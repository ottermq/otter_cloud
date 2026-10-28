package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/clustertest"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// provisioning is a platform DB whose one cluster is the test cluster, and
// a worker for its node with the shared-database driver.
type provisioning struct {
	testCluster
	db      *dbtest.DB
	account string
	user    string
	worker  *operations.Worker
}

func newProvisioning(t *testing.T) *provisioning {
	c := cluster(t)
	db := dbtest.New(t)
	ctx := context.Background()
	admin, api := db.ConnAs(dbtest.RoleAdmin), db.ConnAs(dbtest.RoleAPI)
	if _, err := instances.AddNode(ctx, admin, instances.Node{Name: "node-a", CapacityMemoryMB: 8192, CapacityStorageGB: 100,
		TenantSubnet: netip.MustParsePrefix("127.0.0.0/24"), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: "pgc1", Node: "node-a", PGMajor: clustertest.PGMajor,
		JailName: clustertest.Jail, PoolAddr: netip.MustParseAddr(c.Host()), PoolPort: 6432, MemoryMB: 2048,
		CapacityStorageGB: 60, CapacityConnections: 300, CapacityDatabases: 12}); err != nil {
		t.Fatal(err)
	}
	u, err := identity.CreateUser(ctx, api, identity.NewUser{Email: "ana@example.com", Password: "correct horse battery staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := billing.Subscribe(ctx, admin, u.AccountID, "founder-beta"); err != nil {
		t.Fatal(err)
	}
	jd := c.JailDeck().Client()
	c.Pool(jd) // emptied before and after
	driver := &postgres.Driver{JailDeck: jd, Credentials: c.Creds("admin"), Port: c.Port()}
	return &provisioning{testCluster: c, db: db, account: u.AccountID, user: u.ID,
		worker: &operations.Worker{DB: db.PoolAs(db.NodeRole("node-a")), Name: "test", Handlers: driver.Handlers(),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
}

// create is a create through the API's path, with its role and database
// dropped when the test ends: the cluster outlives the tests.
func (p *provisioning) create(name string) instances.Created {
	p.t.Helper()
	ctx := context.Background()
	tx, err := p.db.ConnAs(dbtest.RoleAPI).Begin(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := instances.Create(ctx, tx, instances.CreateRequest{AccountID: p.account, RequestedBy: "user:" + p.user, Name: name, FlavorCode: "db-small"})
	if err != nil {
		p.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		p.t.Fatal(err)
	}
	p.t.Cleanup(func() {
		conn := p.admin()
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+c.Instance.InfraName+" WITH (FORCE)")
		_, _ = conn.Exec(ctx, "DROP ROLE IF EXISTS "+c.Instance.InfraName)
	})
	return c
}

func (p *provisioning) runOnce() {
	p.t.Helper()
	if ran, err := p.worker.RunOnce(context.Background()); !ran || err != nil {
		p.t.Fatalf("RunOnce = %t, %v", ran, err)
	}
}

type outcome struct {
	Instance, InstanceReason, Op, OpReason, Step, Payload string
}

func (p *provisioning) outcome(c instances.Created) outcome {
	p.t.Helper()
	var o outcome
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT i.status, coalesce(i.reason, ''), o.status, coalesce(o.reason, ''), coalesce(o.step, ''), o.payload::text
		FROM instances i JOIN operations o ON o.instance_id = i.id WHERE o.id = $1`, c.Operation.ID).
		Scan(&o.Instance, &o.InstanceReason, &o.Op, &o.OpReason, &o.Step, &o.Payload); err != nil {
		p.t.Fatal(err)
	}
	return o
}

// The acceptance of #134: a database created through the API runs on the
// cluster, and its customer logs in through PgBouncer with the password
// from the create; a second run changes nothing.
func TestCreateProvisions(t *testing.T) {
	p := newProvisioning(t)
	c := p.create("production")
	p.runOnce()
	want := outcome{Instance: "running", Op: "succeeded", Step: postgres.StepPool, Payload: "{}"}
	if o := p.outcome(c); o != want {
		t.Fatalf("after the create: %+v, want %+v", o, want)
	}
	tn := postgres.Tenant{Name: c.Instance.InfraName}
	db, err := p.asTenantVia("pool-port", tn, c.Password)
	if err != nil {
		t.Fatalf("log in through PgBouncer: %v", err)
	}
	_ = db.Close(context.Background())

	// As after a crash between the work and the operation's end: run it
	// again from the start.
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), `
		UPDATE instances SET status = 'provisioning' WHERE id = $1;`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), `
		UPDATE operations SET status = 'pending', step = NULL, finished_at = NULL, payload = $2 WHERE id = $1`,
		c.Operation.ID, map[string]string{"verifier": verifier(t, c.Password)}); err != nil {
		t.Fatal(err)
	}
	p.runOnce()
	if o := p.outcome(c); o != want {
		t.Errorf("after a rerun: %+v, want %+v", o, want)
	}
	if db, err := p.asTenantVia("pool-port", tn, c.Password); err != nil {
		t.Errorf("log in after a rerun: %v", err)
	} else {
		_ = db.Close(context.Background())
	}
}

// A role of the instance's name that the worker did not make fails the
// create, and the instance, at once; the role is left as it was.
func TestCreateRefusesAnUnsafeRole(t *testing.T) {
	p := newProvisioning(t)
	c := p.create("production")
	ctx := context.Background()
	conn := p.admin()
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE ROLE "+c.Instance.InfraName+" LOGIN CREATEDB"); err != nil {
		t.Fatal(err)
	}
	p.runOnce()
	want := outcome{Instance: "failed", InstanceReason: "create_failed", Op: "failed", OpReason: "create_failed", Payload: "{}"}
	if o := p.outcome(c); o != want {
		t.Errorf("after the create: %+v, want %+v", o, want)
	}
	var createdb bool
	if err := conn.QueryRow(ctx, `SELECT rolcreatedb FROM pg_roles WHERE rolname = $1`, c.Instance.InfraName).Scan(&createdb); err != nil || !createdb {
		t.Errorf("the role was changed: createdb %t, %v", createdb, err)
	}
}
