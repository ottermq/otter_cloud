package operations_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// instanceOf is the instance an operation is for, and its state.
func (f *fixture) instanceOf(opID string) (id, status, desired, reason string) {
	f.t.Helper()
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT i.id, i.status, i.desired_state, coalesce(i.reason, '')
		FROM instances i JOIN operations o ON o.instance_id = i.id WHERE o.id = $1`, opID).
		Scan(&id, &status, &desired, &reason); err != nil {
		f.t.Fatal(err)
	}
	return
}

// failedDelete is a running instance whose delete ran out of attempts, as
// the reference node's first end-to-end delete did (#102).
func (f *fixture) failedDelete() string {
	f.t.Helper()
	create := f.creates(1)[0]
	instance, _, _, _ := f.instanceOf(create)
	f.exec(`UPDATE operations SET status = 'succeeded', finished_at = now(), payload = '{}' WHERE id = $1`, create)
	f.exec(`UPDATE instances SET status = 'running' WHERE id = $1`, instance)
	var del string
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		SELECT id, account_id, node_id, 'delete', 'system' FROM instances WHERE id = $1 RETURNING id`, instance).Scan(&del); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE instances SET desired_state = 'deleted' WHERE id = $1`, instance)
	ctx := context.Background()
	conn := f.worker("node-a")
	op, ok, err := operations.Claim(ctx, conn, "w1", []string{"delete"}, operations.DefaultLease)
	if err != nil || !ok || op.ID != del {
		f.t.Fatalf("claim the delete = %+v, %v, %v", op, ok, err)
	}
	if err := operations.Checkpoint(ctx, conn, &op, "closed"); err != nil {
		f.t.Fatal(err)
	}
	f.exec(`UPDATE operations SET attempts = $2 WHERE id = $1`, del, operations.MaxAttempts)
	op.Attempts = operations.MaxAttempts
	if failed, err := operations.Retry(ctx, conn, op, errors.New(`pg_dump: executable file not found in $PATH`)); err != nil || !failed {
		f.t.Fatalf("run out of attempts = %v, %v", failed, err)
	}
	return del
}

// The acceptance of #102: only a failed operation is retried. It is due
// again from its last step, with fresh attempts, and its worker claims it.
func TestRetryFailed(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	del := f.failedDelete()
	if _, status, _, reason := f.instanceOf(del); status != "failed" || reason != "delete_failed" {
		t.Fatalf("before: instance %s, %s", status, reason)
	}

	c, err := operations.RetryFailed(ctx, admin, del)
	if err != nil || c.Status != "pending" || c.Kind != "delete" || c.AccountID == "" {
		t.Fatalf("retry = %+v, %v", c, err)
	}
	if r := f.op(del); r.Status != "pending" || r.Attempts != 0 || r.Step != "closed" || r.Reason != "" ||
		!strings.Contains(r.LastError, "pg_dump") || r.Locked || r.Due > 0 {
		t.Errorf("after retry: %+v", r)
	}
	if _, status, desired, reason := f.instanceOf(del); status != "deleting" || desired != "deleted" || reason != "" {
		t.Errorf("instance after retry: %s, %s, %q", status, desired, reason)
	}
	op, ok, err := operations.Claim(ctx, f.worker("node-a"), "w2", []string{"delete"}, operations.DefaultLease)
	if err != nil || !ok || op.ID != del || op.Step != "closed" || op.Attempts != 1 {
		t.Fatalf("claim after retry = %+v, %v, %v", op, ok, err)
	}

	if _, err := operations.RetryFailed(ctx, admin, del); err == nil || !strings.Contains(err.Error(), "only a failed operation") {
		t.Errorf("retry a running one = %v", err)
	}
	if _, err := operations.RetryFailed(ctx, admin, "01a18db4-5b55-7418-b900-1b492a6e2290"); !errors.Is(err, operations.ErrNotFound) {
		t.Errorf("retry an unknown id = %v", err)
	}
	if _, err := operations.RetryFailed(ctx, admin, "not-an-id"); !errors.Is(err, operations.ErrNotFound) {
		t.Errorf("retry a malformed id = %v", err)
	}
}

// A failed create isn't retried: its verifier went with its payload.
func TestRetryFailed_RefusesACreate(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	create := f.creates(1)[0]
	conn := f.worker("node-a")
	op, ok, err := operations.Claim(ctx, conn, "w1", creates, operations.DefaultLease)
	if err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if err := operations.Fail(ctx, conn, op, "create_failed", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	_, err = operations.RetryFailed(ctx, f.db.ConnAs(dbtest.RoleAdmin), create)
	if err == nil || !strings.Contains(err.Error(), "can't be retried") {
		t.Errorf("retry a failed create = %v", err)
	}
	if r := f.op(create); r.Status != "failed" {
		t.Errorf("the create changed: %+v", r)
	}
}

// Another operation in flight on the instance keeps a retry out of the
// queue: one at a time per instance.
func TestRetryFailed_AnotherInFlight(t *testing.T) {
	f := newFixture(t, "node-a")
	del := f.failedDelete()
	instance, _, _, _ := f.instanceOf(del)
	f.exec(`INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		SELECT id, account_id, node_id, 'restart', 'system' FROM instances WHERE id = $1`, instance)
	if _, err := operations.RetryFailed(context.Background(), f.db.ConnAs(dbtest.RoleAdmin), del); !errors.Is(err, operations.ErrInFlight) {
		t.Errorf("retry with another in flight = %v", err)
	}
}

// The acceptance of #102: only a pending operation is canceled, and only
// one that never ran. A canceled create leaves nothing behind, its quota
// included.
func TestCancelPending(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	ids := f.creates(2)
	before, err := instances.AccountUsage(ctx, admin, f.lastAccount)
	if err != nil || before.Instances != 2 {
		t.Fatalf("usage before = %+v, %v", before, err)
	}

	c, err := operations.CancelPending(ctx, admin, ids[0])
	if err != nil || c.Status != "canceled" || c.Reason != "canceled" {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	if r := f.op(ids[0]); r.Status != "canceled" || r.Payload != "{}" {
		t.Errorf("after cancel: %+v", r)
	}
	if _, status, desired, reason := f.instanceOf(ids[0]); status != "deleted" || desired != "deleted" || reason != "canceled" {
		t.Errorf("instance after cancel: %s, %s, %s", status, desired, reason)
	}
	if after, err := instances.AccountUsage(ctx, admin, f.lastAccount); err != nil || after.Instances != 1 {
		t.Errorf("usage after = %+v, %v", after, err)
	}
	if _, err := operations.CancelPending(ctx, admin, ids[0]); err == nil || !strings.Contains(err.Error(), "only a pending") {
		t.Errorf("cancel twice = %v", err)
	}

	// The second has run once and waits to retry: refused.
	conn := f.worker("node-a")
	op, ok, err := operations.Claim(ctx, conn, "w1", creates, operations.DefaultLease)
	if err != nil || !ok || op.ID != ids[1] {
		t.Fatalf("claim = %+v, %v, %v", op, ok, err)
	}
	if _, err := operations.Retry(ctx, conn, op, errors.New("transient")); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.CancelPending(ctx, admin, ids[1]); err == nil || !strings.Contains(err.Error(), "waits to retry") {
		t.Errorf("cancel one in backoff = %v", err)
	}
}

// A canceled delete withdraws the customer's intent; the database stays.
func TestCancelPending_Delete(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	create := f.creates(1)[0]
	instance, _, _, _ := f.instanceOf(create)
	f.exec(`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, create)
	f.exec(`UPDATE instances SET status = 'running', desired_state = 'deleted' WHERE id = $1`, instance)
	var del string
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(ctx, `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		SELECT id, account_id, node_id, 'delete', 'system' FROM instances WHERE id = $1 RETURNING id`, instance).Scan(&del); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.CancelPending(ctx, f.db.ConnAs(dbtest.RoleAdmin), del); err != nil {
		t.Fatal(err)
	}
	if _, status, desired, _ := f.instanceOf(del); status != "running" || desired != "running" {
		t.Errorf("instance after a canceled delete: %s, %s", status, desired)
	}
}

// The operator's list shows last_error; a customer's view of the same
// operation has no field for it (portals.md §3).
func TestList_ShowsLastErrorToOperators(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	del := f.failedDelete()
	list, err := operations.List(ctx, f.db.ConnAs(dbtest.RoleAdmin), operations.Filter{Status: "failed"})
	if err != nil || len(list) != 1 || list[0].ID != del || !strings.Contains(list[0].LastError, "pg_dump") || list[0].Node != "node-a" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if all, err := operations.List(ctx, f.db.ConnAs(dbtest.RoleAdmin), operations.Filter{InstanceID: list[0].InstanceID}); err != nil || len(all) != 2 {
		t.Errorf("by instance = %d, %v; want the create and the delete", len(all), err)
	}
	if _, err := operations.List(ctx, f.db.ConnAs(dbtest.RoleAdmin), operations.Filter{InstanceID: "x"}); err == nil {
		t.Error("a malformed instance id was accepted")
	}
	v, err := instances.OperationByID(ctx, f.db.ConnAs(dbtest.RoleAPI), f.lastAccount, del)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := json.Marshal(v); err != nil || strings.Contains(string(b), "pg_dump") {
		t.Errorf("the customer's view = %s, %v; it must not hold the error", b, err)
	}
}

// running is a created instance the worker has made running.
func (f *fixture) running() (instance string) {
	f.t.Helper()
	create := f.creates(1)[0]
	instance, _, _, _ = f.instanceOf(create)
	f.exec(`UPDATE operations SET status = 'succeeded', finished_at = now(), payload = '{}' WHERE id = $1`, create)
	f.exec(`UPDATE instances SET status = 'running' WHERE id = $1`, instance)
	return instance
}

// queue inserts an operation of kind for the instance, with the desired
// state its request records ("" keeps it).
func (f *fixture) queue(instance, kind, desired string) string {
	f.t.Helper()
	var id string
	if err := f.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
		SELECT id, account_id, node_id, $2, 'system' FROM instances WHERE id = $1 RETURNING id`, instance, kind).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	if desired != "" {
		f.exec(`UPDATE instances SET desired_state = $2 WHERE id = $1`, instance, desired)
	}
	return id
}

// A failed stop leaves the instance stopping with its reason (#95); a
// retry clears the reason, and is refused once a later operation exists:
// retrying the stop after a start would undo the start.
func TestRetryFailed_Stop(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	admin := f.db.ConnAs(dbtest.RoleAdmin)
	instance := f.running()
	stop := f.queue(instance, "stop", "stopped")
	conn := f.worker("node-a")
	op, ok, err := operations.Claim(ctx, conn, "w1", []string{"stop"}, operations.DefaultLease)
	if err != nil || !ok || op.ID != stop {
		t.Fatalf("claim = %+v, %v, %v", op, ok, err)
	}
	f.exec(`UPDATE instances SET status = 'stopping' WHERE id = $1`, instance)
	if err := operations.Fail(ctx, conn, op, "stop_failed", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if _, status, _, reason := f.instanceOf(stop); status != "stopping" || reason != "stop_failed" {
		t.Fatalf("after a failed stop: %s, %s; want stopping, stop_failed", status, reason)
	}

	if _, err := operations.RetryFailed(ctx, admin, stop); err != nil {
		t.Fatal(err)
	}
	if _, status, desired, reason := f.instanceOf(stop); status != "stopping" || desired != "stopped" || reason != "" {
		t.Errorf("after the retry: %s, %s, %q", status, desired, reason)
	}

	// It fails again; the customer starts the database instead.
	op, _, _ = operations.Claim(ctx, conn, "w1", []string{"stop"}, operations.DefaultLease)
	if err := operations.Fail(ctx, conn, op, "stop_failed", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	start := f.queue(instance, "start", "running")
	f.exec(`UPDATE operations SET status = 'succeeded', finished_at = now() WHERE id = $1`, start)
	if _, err := operations.RetryFailed(ctx, admin, stop); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Errorf("retry a superseded stop = %v", err)
	}
}

// A canceled stop withdraws the customer's intent: the database stays
// running, and so desired.
func TestCancelPending_Stop(t *testing.T) {
	f := newFixture(t, "node-a")
	instance := f.running()
	stop := f.queue(instance, "stop", "stopped")
	if _, err := operations.CancelPending(context.Background(), f.db.ConnAs(dbtest.RoleAdmin), stop); err != nil {
		t.Fatal(err)
	}
	if _, status, desired, _ := f.instanceOf(stop); status != "running" || desired != "running" {
		t.Errorf("after a canceled stop: %s, %s", status, desired)
	}
}

// A failed reset_password leaves the instance as it was, without a reason:
// the old password still works. It isn't retried, its verifier gone with
// its payload; the customer asks for another (#101).
func TestFailedResetPassword(t *testing.T) {
	f := newFixture(t, "node-a")
	ctx := context.Background()
	instance := f.running()
	reset := f.queue(instance, "reset_password", "")
	conn := f.worker("node-a")
	op, ok, err := operations.Claim(ctx, conn, "w1", []string{"reset_password"}, operations.DefaultLease)
	if err != nil || !ok || op.ID != reset {
		t.Fatalf("claim = %+v, %v, %v", op, ok, err)
	}
	if err := operations.Fail(ctx, conn, op, "reset_password_failed", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	if _, status, _, reason := f.instanceOf(reset); status != "running" || reason != "" {
		t.Errorf("after a failed reset: %s, %q; want running, no reason", status, reason)
	}
	if _, err := operations.RetryFailed(ctx, f.db.ConnAs(dbtest.RoleAdmin), reset); err == nil || !strings.Contains(err.Error(), "verifier is gone") {
		t.Errorf("retry a failed reset = %v", err)
	}
}
