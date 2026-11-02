package instances_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
)

// running is a created instance the worker has made: running, nothing in
// flight. expires is how far from now it expires; zero for never.
func (f *fixture) running(account, actor, name string, expires time.Duration, ephemeral bool) instances.Created {
	f.t.Helper()
	ctx := context.Background()
	r := instances.CreateRequest{AccountID: account, Name: name, FlavorCode: "db-small", RequestedBy: actor, Ephemeral: ephemeral}
	if expires != 0 {
		at := time.Now().Add(time.Hour + time.Minute)
		r.ExpiresAt = &at
	}
	c, err := create(ctx, f.db.ConnAs(dbtest.RoleAPI), r)
	if err != nil {
		f.t.Fatal(err)
	}
	f.migrate(`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, c.Operation.ID)
	f.migrate(`UPDATE instances SET status = 'running' WHERE id = $1`, c.Instance.ID)
	if expires != 0 {
		// Past MinLifetime by hand: the clock the tests move is Expire's.
		f.migrate(`UPDATE instances SET expires_at = now() + $2 * interval '1 second' WHERE id = $1`, c.Instance.ID, expires.Seconds())
	}
	return c
}

func (f *fixture) migrate(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

// finish completes the instance's operation in flight as the worker would,
// leaving the instance status.
func (f *fixture) finish(c instances.Created, status string) {
	f.t.Helper()
	f.migrate(`UPDATE operations SET status = 'succeeded', finished_at = now()
		WHERE instance_id = $1 AND status IN ('pending', 'running')`, c.Instance.ID)
	f.migrate(`UPDATE instances SET status = $2 WHERE id = $1`, c.Instance.ID, status)
}

func (f *fixture) expire(now time.Time) []instances.Expired {
	f.t.Helper()
	done, err := instances.Expire(context.Background(), f.db.PoolAs(dbtest.RoleAPI), now)
	if err != nil {
		f.t.Fatal(err)
	}
	return done
}

func (f *fixture) state(c instances.Created) (desired string, inFlight string) {
	f.t.Helper()
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT i.desired_state, coalesce((SELECT o.kind || ':' || o.requested_by FROM operations o
			WHERE o.instance_id = i.id AND o.status IN ('pending', 'running')), '')
		FROM instances i WHERE i.id = $1`, c.Instance.ID).Scan(&desired, &inFlight); err != nil {
		f.t.Fatal(err)
	}
	return desired, inFlight
}

// The acceptance of #100, under a moved clock: stopped at expires_at, then
// deleted 7 days later by a delete that takes the final dump; each step
// audited as the system's, and none taken twice.
func TestExpire(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	c := f.running(account, actor, "staging", -time.Minute, false)
	keep := f.running(account, actor, "production", 0, false)
	bob, bobActor := f.customer("bob@example.com", true)
	later := f.running(bob, bobActor, "later", 8*24*time.Hour, false)
	now := time.Now()

	if done := f.expire(now); len(done) != 1 || done[0] != (instances.Expired{InstanceID: c.Instance.ID, Kind: "stop"}) {
		t.Fatalf("at expiry: %+v, want one stop", done)
	}
	if d, op := f.state(c); d != "stopped" || op != "stop:system" {
		t.Errorf("after the stop is queued: desired %s, in flight %q", d, op)
	}
	if done := f.expire(now); len(done) != 0 {
		t.Errorf("again while the stop is in flight: %+v", done)
	}
	f.finish(c, "stopped")
	if done := f.expire(now.Add(time.Hour)); len(done) != 0 {
		t.Errorf("stopped, in the grace period: %+v", done)
	}
	v, err := instances.InstanceByID(context.Background(), f.db.ConnAs(dbtest.RoleAPI), account, c.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Reason != "expired" || v.Message != instances.ReasonMessage("expired") {
		t.Errorf("stopped past its expiry: reason %q, message %q", v.Reason, v.Message)
	}

	done := f.expire(now.Add(instances.ExpiryGrace))
	if len(done) != 1 || done[0] != (instances.Expired{InstanceID: c.Instance.ID, Kind: "delete"}) {
		t.Fatalf("after the grace period: %+v, want one delete", done)
	}
	if d, op := f.state(c); d != "deleted" || op != "delete:system" {
		t.Errorf("after the delete is queued: desired %s, in flight %q", d, op)
	}
	for _, other := range []instances.Created{keep, later} {
		if d, op := f.state(other); d != "running" || op != "" {
			t.Errorf("%s: desired %s, in flight %q; want untouched", other.Instance.Name, d, op)
		}
	}
	var steps []string
	rows, err := f.db.ConnAs(dbtest.RoleAPI).Query(context.Background(), `
		SELECT details->>'step' FROM audit_events
		WHERE action = 'instance.expire' AND actor = 'system' AND target_id = $1 ORDER BY id`, c.Instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil || len(steps) != 2 || steps[0] != "stop" || steps[1] != "delete" {
		t.Errorf("audited steps %v, %v; want stop, delete", steps, err)
	}
}

// An ephemeral instance skips the grace period (Q7): deleted at its expiry.
func TestExpireEphemeral(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	c := f.running(account, actor, "scratch", -time.Minute, true)
	if done := f.expire(time.Now()); len(done) != 1 || done[0].Kind != "delete" {
		t.Fatalf("%+v, want a delete", done)
	}
	if d, op := f.state(c); d != "deleted" || op != "delete:system" {
		t.Errorf("desired %s, in flight %q", d, op)
	}
}

// A database the customer stopped and that then expired is left stopped,
// and deleted after the grace period like any other.
func TestExpireAStoppedDatabase(t *testing.T) {
	f := newFixture(t)
	account, actor := f.customer("ana@example.com", true)
	c := f.running(account, actor, "staging", -time.Minute, false)
	f.migrate(`UPDATE instances SET status = 'stopped', desired_state = 'stopped' WHERE id = $1`, c.Instance.ID)
	if done := f.expire(time.Now()); len(done) != 0 {
		t.Errorf("%+v, want nothing to do", done)
	}
	if done := f.expire(time.Now().Add(instances.ExpiryGrace)); len(done) != 1 || done[0].Kind != "delete" {
		t.Errorf("after the grace period: %+v, want a delete", done)
	}
}

// A customer moves or clears an expiry; an expired database can't be
// started until they do, and then the policy leaves it alone.
func TestSetExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, actor := f.customer("ana@example.com", true)
	c := f.running(account, actor, "staging", -time.Minute, false)
	f.expire(time.Now())
	f.finish(c, "stopped")
	api := f.db.ConnAs(dbtest.RoleAPI)

	inTx := func(fn func(tx pgx.Tx) error) error {
		tx, err := api.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	start := func() error {
		return inTx(func(tx pgx.Tx) error {
			_, _, err := instances.Start(ctx, tx, account, c.Instance.ID, actor)
			return err
		})
	}
	setExpiry := func(at *time.Time) error {
		return inTx(func(tx pgx.Tx) error {
			_, err := instances.SetExpiry(ctx, tx, account, c.Instance.ID, at)
			return err
		})
	}

	var state *instances.StateError
	if err := start(); !errors.As(err, &state) {
		t.Errorf("start an expired database: %v, want a StateError", err)
	}
	soon := time.Now().Add(10 * time.Minute)
	if err := setExpiry(&soon); !errors.Is(err, instances.ErrExpiryTooSoon) {
		t.Errorf("an expiry 10 minutes away: %v, want ErrExpiryTooSoon", err)
	}
	if err := setExpiry(nil); err != nil {
		t.Fatalf("clear the expiry: %v", err)
	}
	if err := start(); err != nil {
		t.Errorf("start once the expiry is cleared: %v", err)
	}
	f.finish(c, "running")
	if done := f.expire(time.Now().Add(30 * 24 * time.Hour)); len(done) != 0 {
		t.Errorf("no expiry: %+v, want nothing", done)
	}
	other, _ := f.customer("bob@example.com", true)
	next := time.Now().Add(48 * time.Hour)
	err := inTx(func(tx pgx.Tx) error {
		_, err := instances.SetExpiry(ctx, tx, other, c.Instance.ID, &next)
		return err
	})
	if !errors.Is(err, instances.ErrNotFound) {
		t.Errorf("another account's instance: %v, want ErrNotFound", err)
	}
}

// An expiry at create is at least MinLifetime away; ephemeral and the
// expiry are part of what an Idempotency-Key names.
func TestCreateWithExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	account, actor := f.customer("ana@example.com", true)
	api := f.db.ConnAs(dbtest.RoleAPI)
	soon := time.Now().Add(30 * time.Minute)
	if _, err := create(ctx, api, instances.CreateRequest{AccountID: account, Name: "scratch", FlavorCode: "db-small",
		RequestedBy: actor, ExpiresAt: &soon}); !errors.Is(err, instances.ErrExpiryTooSoon) {
		t.Errorf("expiry in 30 minutes: %v, want ErrExpiryTooSoon", err)
	}
	at := time.Now().Add(72 * time.Hour)
	r := instances.CreateRequest{AccountID: account, Name: "scratch", FlavorCode: "db-small", RequestedBy: actor,
		ExpiresAt: &at, Ephemeral: true, IdempotencyKey: "k1"}
	c, err := create(ctx, api, r)
	if err != nil {
		t.Fatal(err)
	}
	if c.Instance.ExpiresAt == nil || !c.Instance.ExpiresAt.Equal(at) || !c.Instance.Ephemeral {
		t.Errorf("instance expires %v, ephemeral %t", c.Instance.ExpiresAt, c.Instance.Ephemeral)
	}
	if again, err := create(ctx, api, r); err != nil || !again.Replayed {
		t.Errorf("replay: %v, replayed %t", err, again.Replayed)
	}
	r.Ephemeral = false
	if _, err := create(ctx, api, r); !errors.Is(err, instances.ErrIdempotencyMismatch) {
		t.Errorf("same key, not ephemeral: %v, want ErrIdempotencyMismatch", err)
	}
}
