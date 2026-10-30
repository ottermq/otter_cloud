package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/identity"
	"github.com/ottermq/otter_cloud/internal/platformdb"
)

// adminTimeout bounds one admin command; each is a few queries.
const adminTimeout = time.Minute

// adminCommands are the operator commands, as "otter admin <name> …".
func adminCommands() []command {
	return []command{
		{"invite create", "invite an email address to sign up and print the link", runInviteCreate},
		{"invite list", "list invites and whether each is pending, used or expired", runInviteList},
		{"user list", "list users, their status and live sessions", runUserList},
		{"user disable", "disable a user: end their sessions and refuse their logins", runUserDisable},
		{"user enable", "enable a disabled user again", runUserEnable},
		{"plan list", "list plans and their limits", runPlanList},
		{"subscription create", "subscribe a user's account to a plan", runSubscriptionCreate},
		{"subscription list", "list subscriptions, newest first", runSubscriptionList},
		{"subscription cancel", "end a user's account's subscription now", runSubscriptionCancel},
		{"node add", "record a node and its configured capacity", runNodeAdd},
		{"node list", "list nodes and their capacity", runNodeList},
		{"node drain", "stop placing anything new on a node", runNodeDrain},
		{"node activate", "let placement use a drained node again", runNodeActivate},
		{"cluster add", "record a shared PostgreSQL cluster on a node", runClusterAdd},
		{"cluster list", "list clusters and their capacity", runClusterList},
		{"cluster drain", "stop placing new databases on a cluster", runClusterDrain},
		{"cluster activate", "let placement use a drained cluster again", runClusterActivate},
		{"instances list", "list instances, whose they are and where they live", runInstancesList},
		{"ops list", "list operations, newest first, with their last error", runOpsList},
		{"ops retry", "put a failed operation back in the queue", runOpsRetry},
		{"ops cancel", "cancel a pending operation that has never run", runOpsCancel},
	}
}

func runAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		adminUsage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		// stderr, like every other subcommand's -h (the flag package's habit).
		adminUsage(stderr)
		return 0
	}
	if len(args) >= 2 {
		for _, c := range adminCommands() {
			if c.name == args[0]+" "+args[1] {
				return c.run(args[2:], stdout, stderr)
			}
		}
	}
	fmt.Fprintf(stderr, "otter admin: unknown command %q\n\n", args[0])
	adminUsage(stderr)
	return 2
}

func adminUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: otter admin <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Operator commands, run on the control plane over SSH. Commands:")
	for _, c := range adminCommands() {
		fmt.Fprintf(w, "  %-19s  %s\n", c.name, c.summary)
	}
}

func runInviteCreate(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin invite create", "Invite an email address to sign up and print the single-use link", stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: otter admin invite create [-ttl 72h] <email>")
		fmt.Fprintln(fs.Output(), "\nPrints a single-use sign-up link. Send it to the person; it can't be shown again.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	ttl := fs.Duration("ttl", identity.DefaultInviteTTL, "how long the link stays valid")
	email, code, done := parseEmailArg(fs, args)
	if done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, cfg, ok := adminConn(ctx, "invite create", config.Needs{Database: true, PublicURL: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	// The invite and its audit event commit together, or neither does.
	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin invite create: %v\n", err)
		return 1
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	inv, token, err := identity.CreateInvite(ctx, tx, email, *ttl)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin invite create: %v\n", err)
		if errors.Is(err, identity.ErrInvalidEmail) {
			return 2
		}
		return 1
	}
	if err := audit.Record(ctx, tx, audit.Event{
		Actor:      audit.Operator(),
		Action:     audit.ActionInviteCreate,
		TargetType: "invite",
		TargetID:   inv.ID,
		Outcome:    audit.OK,
		Details:    map[string]any{"email": inv.Email, "expires_at": inv.ExpiresAt},
	}); err != nil {
		fmt.Fprintf(stderr, "otter admin invite create: %v\n", err)
		return 1
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(stderr, "otter admin invite create: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Invite for %s, valid until %s:\n\n  %s\n",
		inv.Email, inv.ExpiresAt.UTC().Format(time.RFC3339), identity.InviteLink(cfg.PublicURL, token))
	return 0
}

func runInviteList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin invite list", "List invites, newest first", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, "invite list", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	invites, err := identity.ListInvites(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin invite list: %v\n", err)
		return 1
	}
	now := time.Now()
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tSTATE\tCREATED\tEXPIRES")
	for _, i := range invites {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", i.Email, i.State(now),
			i.CreatedAt.UTC().Format(time.RFC3339), i.ExpiresAt.UTC().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

// parseEmailArg parses flags followed by exactly one email address.
func parseEmailArg(fs *flag.FlagSet, args []string) (email string, code int, done bool) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return "", 0, true
	case err != nil:
		return "", 2, true
	case fs.NArg() != 1:
		fmt.Fprintf(fs.Output(), "%s: expected one email address\n", fs.Name())
		fs.Usage()
		return "", 2, true
	}
	return fs.Arg(0), 0, false
}

// adminConn loads the configuration and connects, for an admin command
// named name. The caller closes the connection.
func adminConn(ctx context.Context, name string, needs config.Needs, stderr io.Writer) (*pgx.Conn, *config.Config, bool) {
	cfg, ok := loadConfig("admin", needs, stderr)
	if !ok {
		return nil, nil, false
	}
	conn, err := platformdb.Connect(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return nil, nil, false
	}
	return conn, cfg, true
}

func runUserList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin user list", "List users, oldest first", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, "user list", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	users, err := identity.ListUsers(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin user list: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tSTATUS\tACCOUNT\tOPERATOR\tSESSIONS\tLAST SEEN\tCREATED")
	for _, u := range users {
		last := "-"
		if u.LastSeen != nil {
			last = u.LastSeen.UTC().Format(time.RFC3339)
		}
		op := "no"
		if u.IsOperator {
			op = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", u.Email, u.Status, u.AccountStatus, op,
			u.ActiveSessions, last, u.CreatedAt.UTC().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

func runUserDisable(args []string, stdout, stderr io.Writer) int {
	return setUserStatus("disable", "disabled", audit.ActionUserDisable,
		"Disable a user: their sessions end at once and their logins are refused", args, stdout, stderr)
}

func runUserEnable(args []string, stdout, stderr io.Writer) int {
	return setUserStatus("enable", "active", audit.ActionUserEnable,
		"Enable a disabled user again; they log in afresh", args, stdout, stderr)
}

// maxReason bounds -reason, which goes into the audit event's details.
const maxReason = 500

func setUserStatus(verb, status, action, summary string, args []string, stdout, stderr io.Writer) int {
	name := "user " + verb
	fs := newFlagSet("admin "+name, summary, stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: otter admin %s [-reason text] <email>\n\n%s.\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "why, for the audit log")
	email, code, done := parseEmailArg(fs, args)
	if done {
		return code
	}
	if len(*reason) > maxReason {
		fmt.Fprintf(stderr, "otter admin %s: -reason is longer than %d bytes\n", name, maxReason)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, name, config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	// The change and its audit event commit together, or neither does.
	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	c, err := identity.SetUserStatus(ctx, tx, email, status)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if !c.Changed() && c.SessionsRevoked == 0 {
		fmt.Fprintf(stdout, "%s is already %s; nothing changed\n", c.User.Email, status)
		return 0
	}
	details := map[string]any{"email": c.User.Email, "from": c.Previous, "to": status}
	if status == "disabled" {
		details["sessions_revoked"] = c.SessionsRevoked
	}
	if *reason != "" {
		details["reason"] = *reason
	}
	if err := audit.Record(ctx, tx, audit.Event{
		Actor:      audit.Operator(),
		AccountID:  c.User.AccountID,
		Action:     action,
		TargetType: "user",
		TargetID:   c.User.ID,
		Outcome:    audit.OK,
		Details:    details,
	}); err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if err := tx.Commit(ctx); err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if status == "disabled" {
		fmt.Fprintf(stdout, "%s is disabled; %d session(s) ended\n", c.User.Email, c.SessionsRevoked)
	} else {
		fmt.Fprintf(stdout, "%s is active again\n", c.User.Email)
	}
	return 0
}
