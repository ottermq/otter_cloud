package audit

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/user"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Actions. The table only checks the shape (area.verb); listing them here
// keeps the vocabulary in one place.
const (
	ActionSignUp             = "auth.signup"
	ActionLogin              = "auth.login"
	ActionLogout             = "auth.logout"
	ActionCrossOriginRefuse  = "auth.cross_origin_refused"
	ActionPasswordChange     = "user.password_change"
	ActionSessionRevoke      = "session.revoke"
	ActionInviteCreate       = "invite.create"
	ActionUserDisable        = "user.disable"
	ActionUserEnable         = "user.enable"
	ActionSubscriptionCreate = "subscription.create"
	ActionSubscriptionCancel = "subscription.cancel"
	ActionNodeAdd            = "node.add"
	ActionNodeStatus         = "node.set_status"
	ActionClusterAdd         = "cluster.add"
	ActionClusterStatus      = "cluster.set_status"
	ActionInstanceCreate     = "instance.create"
	ActionInstanceDelete     = "instance.delete"
	ActionInstanceStop       = "instance.stop"
	ActionInstanceStart      = "instance.start"
	ActionInstanceRestart    = "instance.restart"
	ActionOperationRetry     = "operation.retry"
	ActionOperationCancel    = "operation.cancel"
)

// Outcome is how the action ended.
type Outcome string

const (
	OK     Outcome = "ok"
	Denied Outcome = "denied"
	Error  Outcome = "error"
)

// Actors.
const (
	System    = "system"
	Anonymous = "anonymous"
)

// User is the actor for a logged-in user.
func User(id string) string { return "user:" + id }

var operatorName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// Operator is the actor for whoever runs otter admin: the login of the
// Unix user, as SSH delivered it.
func Operator() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	name = operatorName.ReplaceAllString(name, "_")
	if name == "" {
		name = "unknown"
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return "operator:" + name
}

// Event is one audit row. Details must never hold secrets: no passwords,
// tokens or hashes.
type Event struct {
	Actor      string
	AccountID  string // empty when there is none
	Action     string
	TargetType string // with TargetID, or both empty
	TargetID   string
	IP         netip.Addr // zero when not from a request
	UserAgent  string
	Outcome    Outcome
	Details    map[string]any
}

// Execer is what Record needs: a connection, pool or transaction. Pass a
// transaction to make the event commit or roll back with the change it
// records.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

const maxUserAgent = 512

// Record writes e.
func Record(ctx context.Context, db Execer, e Event) error {
	var ip *netip.Addr
	if e.IP.IsValid() {
		ip = &e.IP
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	ua := strings.ToValidUTF8(e.UserAgent, "")
	if len(ua) > maxUserAgent {
		ua = strings.ToValidUTF8(ua[:maxUserAgent], "")
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO audit_events (actor, account_id, action, target_type, target_id, ip, user_agent, outcome, details)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.Actor, nullable(e.AccountID), e.Action, nullable(e.TargetType), nullable(e.TargetID),
		ip, nullable(ua), string(e.Outcome), details); err != nil {
		return fmt.Errorf("record audit event %s: %w", e.Action, err)
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
