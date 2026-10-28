package instances

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound means no such resource in the account: it doesn't exist, or
// it belongs to another account. The two are deliberately the same answer.
var ErrNotFound = errors.New("not found")

// OperationView is an operation as its owner sees it (portals.md §3). It
// has no step, attempts, lease, payload or last_error: those are the
// platform's business.
type OperationView struct {
	ID          string     `json:"id"`
	InstanceID  string     `json:"instance_id"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	RequestedBy string     `json:"requested_by"` // you, operator or system
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at"`
}

// MaxOperations bounds a list of operations; the newest come first.
const MaxOperations = 100

// OperationByID returns the account's operation with id.
func OperationByID(ctx context.Context, db DB, accountID, id string) (OperationView, error) {
	if !isUUID(id) {
		return OperationView{}, ErrNotFound
	}
	rows, err := db.Query(ctx, `SELECT `+operationViewColumns+` FROM operations
		WHERE id = $1 AND account_id = $2`, id, accountID)
	if err != nil {
		return OperationView{}, err
	}
	op, err := pgx.CollectExactlyOneRow(rows, scanOperationView)
	if errors.Is(err, pgx.ErrNoRows) {
		return OperationView{}, ErrNotFound
	}
	return op, err
}

// AccountOperations lists the account's operations on all its instances,
// newest first, at most MaxOperations.
func AccountOperations(ctx context.Context, db DB, accountID string) ([]OperationView, error) {
	return listOperations(ctx, db, `WHERE account_id = $1`, accountID)
}

// InstanceOperations lists the operations on one of the account's
// instances, newest first, at most MaxOperations. Another account's
// instance is ErrNotFound, not an empty list.
func InstanceOperations(ctx context.Context, db DB, accountID, instanceID string) ([]OperationView, error) {
	if !isUUID(instanceID) {
		return nil, ErrNotFound
	}
	var mine bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT FROM instances WHERE id = $1 AND account_id = $2)`,
		instanceID, accountID).Scan(&mine); err != nil {
		return nil, fmt.Errorf("look up instance: %w", err)
	}
	if !mine {
		return nil, ErrNotFound
	}
	return listOperations(ctx, db, `WHERE account_id = $1 AND instance_id = $2`, accountID, instanceID)
}

func listOperations(ctx context.Context, db DB, where string, args ...any) ([]OperationView, error) {
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT %s FROM operations %s ORDER BY created_at DESC, id DESC LIMIT %d`,
		operationViewColumns, where, MaxOperations), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanOperationView)
}

// operationViewColumns are only what a customer may see; last_error is
// never selected, so no later change to the view can leak it.
const operationViewColumns = `id, instance_id, kind, status, coalesce(reason, ''), requested_by, created_at, finished_at`

func scanOperationView(row pgx.CollectableRow) (OperationView, error) {
	var (
		v  OperationView
		by string
	)
	err := row.Scan(&v.ID, &v.InstanceID, &v.Kind, &v.Status, &v.Reason, &by, &v.CreatedAt, &v.FinishedAt)
	v.Message = ReasonMessage(v.Reason)
	v.RequestedBy = requester(by)
	// UTC, so a response doesn't tell the server's time zone.
	v.CreatedAt = v.CreatedAt.UTC()
	if v.FinishedAt != nil {
		t := v.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	return v, err
}

// requester says who asked, without naming them: an operator's login and a
// user id stay inside the platform. With one user per account, any user is
// "you".
func requester(actor string) string {
	switch {
	case strings.HasPrefix(actor, "user:"):
		return "you"
	case strings.HasPrefix(actor, "operator:"):
		return "operator"
	default:
		return "system"
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// isUUID reports whether id can name a row. An id from a URL that can't is
// simply not found, rather than a database error.
func isUUID(id string) bool { return uuidPattern.MatchString(id) }
