package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

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
		fmt.Fprintf(w, "  %-14s  %s\n", c.name, c.summary)
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
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	case fs.NArg() != 1:
		fmt.Fprintln(stderr, "otter admin invite create: expected one email address")
		fs.Usage()
		return 2
	}
	email := fs.Arg(0)
	cfg, ok := loadConfig("admin", config.Needs{Database: true, PublicURL: true}, stderr)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, err := platformdb.Connect(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin invite create: %v\n", err)
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
	cfg, ok := loadConfig("admin", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, err := platformdb.Connect(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin invite list: %v\n", err)
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
