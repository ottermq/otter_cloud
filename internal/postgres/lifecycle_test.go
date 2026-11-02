package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// request asks for a stop, start or restart through the API's path; the
// returned operation is that one.
func (p *provisioning) request(kind string, c instances.Created) instances.Created {
	p.t.Helper()
	queue := map[string]func(context.Context, pgx.Tx, string, string, string) (instances.InstanceView, instances.OperationView, error){
		"stop": instances.Stop, "start": instances.Start, "restart": instances.Restart}[kind]
	ctx := context.Background()
	tx, err := p.db.ConnAs(dbtest.RoleAPI).Begin(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, op, err := queue(ctx, tx, p.account, c.Instance.ID, "user:"+p.user)
	if err != nil {
		p.t.Fatalf("%s: %v", kind, err)
	}
	if err := tx.Commit(ctx); err != nil {
		p.t.Fatal(err)
	}
	c.Operation.ID = op.ID
	return c
}

// viaPool logs in as the customer through PgBouncer.
func (p *provisioning) viaPool(c instances.Created) (*pgx.Conn, error) {
	return p.asTenantVia("pool-port", postgres.Tenant{Name: c.Instance.InfraName}, c.Password)
}

// adminTo connects as the worker's admin straight to the tenant's database.
func (p *provisioning) adminTo(c instances.Created) error {
	conn, err := postgres.ConnectDB(context.Background(), p.Target(), p.Creds("admin"), c.Instance.InfraName)
	if err == nil {
		conn.Close(context.Background())
	}
	return err
}

func alive(conn *pgx.Conn) bool {
	_, err := conn.Exec(context.Background(), "SELECT 1")
	return err == nil
}

// The acceptance of #95: stopped, new connections are refused in
// PostgreSQL and at PgBouncer and open ones ended; start restores both,
// with the data; a restart ends the connections and takes new ones.
func TestStopStartRestart(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	held, err := p.viaPool(c)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close(context.Background())

	stop := p.request("stop", c)
	p.runOnce()
	if o := p.outcome(stop); o != (outcome{Instance: "stopped", Op: "succeeded", Step: postgres.StepClosed, Payload: "{}"}) {
		t.Fatalf("after the stop: %+v", o)
	}
	if alive(held) {
		t.Error("a connection open through PgBouncer survived the stop")
	}
	if conn, err := p.viaPool(c); err == nil {
		conn.Close(context.Background())
		t.Error("PgBouncer took a new connection to a stopped database")
	}
	if err := p.adminTo(c); err == nil || !strings.Contains(err.Error(), "not currently accepting connections") {
		t.Errorf("PostgreSQL took a connection to a stopped database: %v", err)
	}
	if strings.Contains(p.cat(p.JailDeck().Client(), postgres.DatabasesFile), c.Instance.InfraName) {
		t.Error("the PgBouncer entry of a stopped database is still there")
	}
	if !p.exists("role", c.Instance.InfraName) || !p.exists("database", c.Instance.InfraName) {
		t.Error("a stop dropped something")
	}

	start := p.request("start", c)
	p.runOnce()
	if o := p.outcome(start); o != (outcome{Instance: "running", Op: "succeeded", Step: postgres.StepPool, Payload: "{}"}) {
		t.Fatalf("after the start: %+v", o)
	}
	conn, err := p.viaPool(c)
	if err != nil {
		t.Fatalf("log in after the start: %v", err)
	}
	defer conn.Close(context.Background())
	var body string
	if err := conn.QueryRow(context.Background(), `SELECT body FROM notes`).Scan(&body); err != nil || body != "keep me" {
		t.Errorf("the data after a stop and a start: %q, %v", body, err)
	}

	restart := p.request("restart", c)
	p.runOnce()
	if o := p.outcome(restart); o != (outcome{Instance: "running", Op: "succeeded", Step: postgres.StepEnded, Payload: "{}"}) {
		t.Fatalf("after the restart: %+v", o)
	}
	if alive(conn) {
		t.Error("a connection survived the restart")
	}
	again, err := p.viaPool(c)
	if err != nil {
		t.Fatalf("log in after the restart: %v", err)
	}
	again.Close(context.Background())
}

// A stopped database refuses everyone, pg_dump too: its delete opens it
// for the final dump, and only then drops it.
func TestDeleteAStoppedDatabase(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.request("stop", c)
	p.runOnce()
	c = p.delete(c)
	p.runOnce()
	if o := p.outcome(c); o.Instance != "deleted" || o.Op != "succeeded" {
		t.Fatalf("after the delete: %+v", o)
	}
	if ds := p.dumps(c); len(ds) != 1 {
		t.Errorf("%d final dumps, want 1", len(ds))
	}
	if p.exists("database", c.Instance.InfraName) {
		t.Error("the database is still there")
	}
}

// A stop whose database is gone fails for an operator, with the database
// left as the reconciler will find it (#96): never recreated. The instance
// stays stopping, with the reason, so the customer can ask again.
func TestStopOfAMissingDatabaseFails(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	conn := p.admin()
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), "DROP DATABASE "+c.Instance.InfraName+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	stop := p.request("stop", c)
	p.runOnce()
	if o := p.outcome(stop); o.Instance != "stopping" || o.InstanceReason != "stop_failed" || o.Op != "failed" || o.OpReason != "stop_failed" {
		t.Errorf("after the stop: %+v", o)
	}
	if p.exists("database", c.Instance.InfraName) {
		t.Error("the stop recreated the database")
	}
}

// atStatus kills right after a stop, start or restart recorded status on
// the instance.
func atStatus(status string) func(string, []any) bool {
	return func(sql string, args []any) bool {
		return strings.HasPrefix(strings.TrimSpace(sql), "UPDATE instances SET status = $2") && len(args) == 2 && args[1] == status
	}
}

// Killed after each step, a stop, a start and a restart each finish on the
// worker's restart, and leave the database as asked.
func TestLifecycleSurvivesAKillAtEachStep(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		steps []string
		final string
	}{
		{"stop", []string{"stopping", postgres.StepUnrouted, postgres.StepClosed, "stopped"}, "stopped"},
		{"start", []string{"starting", postgres.StepOpened, postgres.StepPool, "running"}, "running"},
		{"restart", []string{"starting", postgres.StepOpened, postgres.StepPool, postgres.StepEnded, "running"}, "running"},
	} {
		for _, step := range tc.steps {
			t.Run(tc.kind+"/"+step, func(t *testing.T) {
				p := newProvisioning(t)
				c := p.running("production")
				if tc.kind == "start" {
					p.request("stop", c)
					p.runOnce()
				}
				at := atStep(step)
				if step == "stopping" || step == "starting" || step == tc.final {
					at = atStatus(step)
				}
				op := p.request(tc.kind, c)
				p.crashThenRestart(op, at)
				if o := p.outcome(op); o.Instance != tc.final || o.Op != "succeeded" {
					t.Fatalf("after the restart: %+v", o)
				}
				conn, err := p.viaPool(c)
				if tc.final == "running" && err != nil {
					t.Errorf("log in: %v", err)
				}
				if tc.final == "stopped" && err == nil {
					t.Error("PgBouncer took a connection to a stopped database")
				}
				if err == nil {
					conn.Close(context.Background())
				}
			})
		}
	}
}
