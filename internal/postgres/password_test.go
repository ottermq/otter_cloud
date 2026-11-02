package postgres_test

import (
	"context"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// reset asks for a new password through the API's path; the returned
// operation is the reset, and Password the new one.
func (p *provisioning) reset(c instances.Created) instances.Created {
	p.t.Helper()
	ctx := context.Background()
	tx, err := p.db.ConnAs(dbtest.RoleAPI).Begin(ctx)
	if err != nil {
		p.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	r, err := instances.ResetPassword(ctx, tx, p.account, c.Instance.ID, "user:"+p.user, "")
	if err != nil {
		p.t.Fatalf("reset password: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		p.t.Fatal(err)
	}
	c.Operation.ID, c.Password = r.Operation.ID, r.Password
	return c
}

// The acceptance of #101: after a reset the new password logs in through
// PgBouncer and the old one is refused; the verifier is gone from the
// payload. A connection already open stays open.
func TestResetPassword(t *testing.T) {
	p := newProvisioning(t)
	old := p.running("production")
	held, err := p.viaPool(old)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close(context.Background())

	c := p.reset(old)
	if c.Password == "" || c.Password == old.Password {
		t.Fatalf("new password %q", c.Password)
	}
	p.runOnce()
	if o := p.outcome(c); o != (outcome{Instance: "running", Op: "succeeded", Step: postgres.StepPassword, Payload: "{}"}) {
		t.Fatalf("after the reset: %+v", o)
	}
	if conn, err := p.viaPool(old); err == nil {
		conn.Close(context.Background())
		t.Error("the old password still logs in through PgBouncer")
	}
	conn, err := p.viaPool(c)
	if err != nil {
		t.Fatalf("log in with the new password: %v", err)
	}
	conn.Close(context.Background())
	if !alive(held) {
		t.Error("a reset ended a connection already open")
	}
}

// A stopped database takes a new password too, and has it when started.
func TestResetPasswordOfAStoppedDatabase(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.request("stop", c)
	p.runOnce()
	c = p.reset(c)
	p.runOnce()
	if o := p.outcome(c); o.Instance != "stopped" || o.Op != "succeeded" {
		t.Fatalf("after the reset: %+v", o)
	}
	p.request("start", c)
	p.runOnce()
	conn, err := p.viaPool(c)
	if err != nil {
		t.Fatalf("log in with the new password after the start: %v", err)
	}
	conn.Close(context.Background())
}

// Killed after it set the password, a reset sets it again on the worker's
// restart, and finishes.
func TestResetPasswordSurvivesAKill(t *testing.T) {
	p := newProvisioning(t)
	c := p.reset(p.running("production"))
	p.crashThenRestart(c, atStep(postgres.StepPassword))
	if o := p.outcome(c); o.Instance != "running" || o.Op != "succeeded" || o.Payload != "{}" {
		t.Fatalf("after the restart: %+v", o)
	}
	conn, err := p.viaPool(c)
	if err != nil {
		t.Fatalf("log in with the new password: %v", err)
	}
	conn.Close(context.Background())
}
