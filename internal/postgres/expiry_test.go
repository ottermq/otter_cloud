package postgres_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// expireAt sets the instance's expiry to now plus d (negative: past).
func (p *provisioning) expireAt(c instances.Created, d time.Duration) {
	p.t.Helper()
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE instances SET expires_at = now() + $2 * interval '1 second' WHERE id = $1`, c.Instance.ID, d.Seconds()); err != nil {
		p.t.Fatal(err)
	}
}

// expire runs the control plane's half of the policy as of now.
func (p *provisioning) expire(now time.Time) []instances.Expired {
	p.t.Helper()
	done, err := instances.Expire(context.Background(), p.db.PoolAs(dbtest.RoleAPI), now)
	if err != nil {
		p.t.Fatal(err)
	}
	return done
}

// purge runs the node's half as of now.
func (p *provisioning) purge(now time.Time) []operations.Expiry {
	p.t.Helper()
	done, err := p.driver.PurgeDumps(context.Background(), p.db.PoolAs(p.db.NodeRole("node-a")), now)
	if err != nil {
		p.t.Fatal(err)
	}
	return done
}

func (p *provisioning) status(c instances.Created) string {
	p.t.Helper()
	s, _ := p.instance(c)
	return s
}

// The acceptance of #100 end to end, under a moved clock: stopped at
// expires_at; 7 days later deleted, after a final dump kept 7 days; the
// dump removed when that runs out, and audited.
func TestExpiry(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("staging")
	p.expireAt(c, -time.Minute)
	now := time.Now()

	if done := p.expire(now); len(done) != 1 || done[0].Kind != "stop" {
		t.Fatalf("at expiry: %+v", done)
	}
	p.runOnce()
	if s := p.status(c); s != "stopped" {
		t.Fatalf("after the expiry stop: %s", s)
	}
	if _, err := p.viaPool(c); err == nil {
		t.Error("an expired database still takes connections")
	}

	if done := p.expire(now.Add(instances.ExpiryGrace)); len(done) != 1 || done[0].Kind != "delete" {
		t.Fatalf("after the grace period: %+v", done)
	}
	p.runOnce()
	if s := p.status(c); s != "deleted" || p.exists("database", c.Instance.InfraName) {
		t.Fatalf("after the expiry delete: %s, database %t", s, p.exists("database", c.Instance.InfraName))
	}
	ds := p.dumps(c)
	if len(ds) != 1 {
		t.Fatalf("%d final dumps, want 1", len(ds))
	}
	if d := ds[0].Expires; d < postgres.ExpiryRetention-time.Minute || d > postgres.ExpiryRetention {
		t.Errorf("dump kept %s, want %s", d, postgres.ExpiryRetention)
	}
	file := filepath.Join(p.driver.Dumper.Dir, ds[0].Ref)

	if done := p.purge(time.Now().Add(postgres.ExpiryRetention - time.Hour)); len(done) != 0 {
		t.Errorf("before the dump expires: %+v", done)
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the dump is gone early: %v", err)
	}
	if done := p.purge(time.Now().Add(postgres.ExpiryRetention + time.Minute)); len(done) != 1 || done[0].Action != "purge" {
		t.Fatalf("after the dump expires: %+v", done)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("the dump is still there: %v", err)
	}
	if done := p.purge(time.Now().Add(postgres.ExpiryRetention + time.Minute)); len(done) != 0 {
		t.Errorf("purged twice: %+v", done)
	}
	var n int
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events a JOIN backup_records b ON b.id::text = a.target_id
		WHERE a.action = 'backup.purge' AND a.actor = 'system' AND b.instance_id = $1 AND b.purged_at IS NOT NULL`,
		c.Instance.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d backup.purge events for a purged record, %v; want 1", n, err)
	}
}

// An ephemeral database is deleted at its expiry, with no final dump (Q7).
func TestExpiryOfAnEphemeralDatabase(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("scratch")
	p.expireAt(c, -time.Minute)
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE instances SET ephemeral = true WHERE id = $1`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	if done := p.expire(time.Now()); len(done) != 1 || done[0].Kind != "delete" {
		t.Fatalf("%+v, want a delete", done)
	}
	p.runOnce()
	if s := p.status(c); s != "deleted" || p.exists("database", c.Instance.InfraName) || p.exists("role", c.Instance.InfraName) {
		t.Errorf("instance %s; database %t, role %t", s, p.exists("database", c.Instance.InfraName), p.exists("role", c.Instance.InfraName))
	}
	if ds := p.dumps(c); len(ds) != 0 {
		t.Errorf("%d final dumps of an ephemeral database, want 0", len(ds))
	}
}

// A customer's delete keeps the dump 3 days; one already gone from the
// node is still marked purged.
func TestPurgeAMissingDump(t *testing.T) {
	p := newProvisioning(t)
	c := p.delete(p.running("staging"))
	p.runOnce()
	ds := p.dumps(c)
	if len(ds) != 1 || ds[0].Expires > postgres.UserDeleteRetention || ds[0].Expires < postgres.UserDeleteRetention-time.Minute {
		t.Fatalf("dumps %+v, want one kept %s", ds, postgres.UserDeleteRetention)
	}
	if err := os.Remove(filepath.Join(p.driver.Dumper.Dir, ds[0].Ref)); err != nil {
		t.Fatal(err)
	}
	if done := p.purge(time.Now().Add(postgres.UserDeleteRetention + time.Minute)); len(done) != 1 {
		t.Errorf("%+v, want the record purged", done)
	}
	var gone string
	if err := p.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(),
		`SELECT details->>'already_gone' FROM audit_events WHERE action = 'backup.purge'`).Scan(&gone); err != nil || gone != "true" {
		t.Errorf("already_gone = %q, %v", gone, err)
	}
}
