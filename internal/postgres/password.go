package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/operations"
)

// StepPassword is the step of a reset_password, in operations.step once
// done.
const StepPassword = "password"

// ResetPassword sets the new password of a reset_password operation on the
// tenant's role (#101): its SCRAM verifier, as create set the first. The
// instance's status doesn't change, and a stopped database takes it too.
// PgBouncer looks the password up at each login (auth_query), so the old
// one is refused from then on; connections already open stay open.
func (d *Driver) ResetPassword(ctx context.Context, job *operations.Job) error {
	const reason = "reset_password_failed"
	t, err := d.load(ctx, job.DB(), job.InstanceID, reason)
	if err != nil {
		return err
	}
	if t.desired == "deleted" || !slices.Contains([]string{"running", "stopped", "stopping", "starting"}, t.status) {
		return operations.Permanent(reason, fmt.Errorf("instance %s is %s, desired %s", t.instanceID, t.status, t.desired))
	}
	var payload struct {
		Verifier string `json:"verifier"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.Verifier == "" {
		return operations.Permanent(reason, errors.New("the operation has no password verifier"))
	}
	t.tenant.Verifier = payload.Verifier
	if err := t.tenant.check(); err != nil {
		return operations.Permanent(reason, err)
	}

	conn, err := d.connect(ctx, &t)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	// The role must be there, as the worker made it: a missing one is the
	// reconciler's to report, never recreated here.
	exists, err := roleExists(ctx, conn, t.tenant.Name)
	if err != nil {
		return err
	}
	if !exists {
		return operations.Permanent(reason, fmt.Errorf("role %s does not exist", t.tenant.Name))
	}
	if err := checkRole(ctx, conn, t.tenant.Name); err != nil {
		if errors.Is(err, ErrUnsafeRole) {
			return operations.Permanent(reason, err)
		}
		return err
	}
	// Nothing in the verifier can close the literal (check).
	if _, err := conn.Exec(ctx, "ALTER ROLE "+pgx.Identifier{t.tenant.Name}.Sanitize()+" PASSWORD '"+t.tenant.Verifier+"'"); err != nil {
		return fmt.Errorf("set the password of %s: %w", t.tenant.Name, err)
	}
	return job.Checkpoint(ctx, StepPassword)
}
