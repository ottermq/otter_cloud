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
	"github.com/ottermq/otter_cloud/internal/billing"
	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/identity"
)

func runPlanList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin plan list", "List plans, active or not, by code", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, "plan list", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	plans, err := billing.ListPlans(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin plan list: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tNAME\tPRICE\tINSTANCES\tSTORAGE\tCONNECTIONS\tMEMORY\tVCPU\tPUBLIC\tACTIVE")
	for _, p := range plans {
		price := "free"
		if p.RequiresPayment() {
			price = fmt.Sprintf("%d.%02d %s/%s", p.PriceMinor/100, p.PriceMinor%100, p.Currency, p.Interval)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d GB\t%d\t%d MB\t%d\t%s\t%s\n", p.Code, p.Name, price,
			p.Instances, p.StorageGB, p.Connections, p.MemoryMB, p.VCPU, yesNo(p.Public), yesNo(p.Active))
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func runSubscriptionCreate(args []string, stdout, stderr io.Writer) int {
	const name = "subscription create"
	fs := newFlagSet("admin "+name, "", stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: otter admin subscription create [-reason text] <email> <plan>")
		fmt.Fprintln(fs.Output(), "\nSubscribes the account of the user with <email> to the active plan with code <plan>")
		fmt.Fprintln(fs.Output(), "(see otter admin plan list). A free plan starts active; a paid one waits for payment.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "why, for the audit log")
	rest, code, done := parseArgs(fs, args, 2, "an email address and a plan code")
	if done {
		return code
	}
	if code, ok := checkReason(name, *reason, stderr); !ok {
		return code
	}
	return subscriptionChange(name, rest[0], *reason, stdout, stderr,
		func(ctx context.Context, db billing.DB, accountID string) (billing.Subscription, error) {
			return billing.Subscribe(ctx, db, accountID, rest[1])
		},
		audit.ActionSubscriptionCreate,
		func(s billing.Subscription, email string) string {
			return fmt.Sprintf("%s is subscribed to %s (%s)", email, s.PlanName, s.Status)
		})
}

func runSubscriptionCancel(args []string, stdout, stderr io.Writer) int {
	const name = "subscription cancel"
	fs := newFlagSet("admin "+name, "", stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: otter admin subscription cancel [-reason text] <email>")
		fmt.Fprintln(fs.Output(), "\nEnds the subscription of the account of the user with <email>, now. The")
		fmt.Fprintln(fs.Output(), "account's databases are not touched: the expiry policy decides what happens to them.")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "why, for the audit log")
	rest, code, done := parseArgs(fs, args, 1, "an email address")
	if done {
		return code
	}
	if code, ok := checkReason(name, *reason, stderr); !ok {
		return code
	}
	return subscriptionChange(name, rest[0], *reason, stdout, stderr, billing.Cancel,
		audit.ActionSubscriptionCancel,
		func(s billing.Subscription, email string) string {
			return fmt.Sprintf("%s's subscription to %s is canceled", email, s.PlanName)
		})
}

// subscriptionChange runs change on the account of the user with email,
// with its audit event, in one transaction.
func subscriptionChange(name, email, reason string, stdout, stderr io.Writer,
	change func(context.Context, billing.DB, string) (billing.Subscription, error),
	action string, done func(billing.Subscription, string) string) int {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, name, config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	tx, err := conn.Begin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	u, err := identity.UserByEmail(ctx, tx, email)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		if errors.Is(err, identity.ErrInvalidEmail) {
			return 2
		}
		return 1
	}
	s, err := change(ctx, tx, u.AccountID)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	details := map[string]any{"email": u.Email, "plan": s.PlanCode, "status": s.Status}
	if reason != "" {
		details["reason"] = reason
	}
	if err := audit.Record(ctx, tx, audit.Event{
		Actor:      audit.Operator(),
		AccountID:  u.AccountID,
		Action:     action,
		TargetType: "subscription",
		TargetID:   s.ID,
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
	fmt.Fprintln(stdout, done(s, u.Email))
	return 0
}

func runSubscriptionList(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin subscription list", "List subscriptions, newest first", stderr)
	if code, done := parse(fs, args); done {
		return code
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, "subscription list", config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())

	subs, err := billing.ListSubscriptions(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin subscription list: %v\n", err)
		return 1
	}
	users, err := identity.ListUsers(ctx, conn)
	if err != nil {
		fmt.Fprintf(stderr, "otter admin subscription list: %v\n", err)
		return 1
	}
	// One user per account in the MVP; billing knows accounts, not people.
	emails := map[string]string{}
	for _, u := range users {
		emails[u.AccountID] = u.Email
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "EMAIL\tPLAN\tSTATUS\tPAYMENT\tSTARTS\tENDS")
	for _, s := range subs {
		ends := "-"
		if s.EndsAt != nil {
			ends = s.EndsAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", emails[s.AccountID], s.PlanCode, s.Status, s.PaymentStatus,
			s.StartsAt.UTC().Format(time.RFC3339), ends)
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

// parseArgs parses flags followed by exactly n arguments, described by what.
func parseArgs(fs *flag.FlagSet, args []string, n int, what string) ([]string, int, bool) {
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return nil, 0, true
	case err != nil:
		return nil, 2, true
	case fs.NArg() != n:
		fmt.Fprintf(fs.Output(), "%s: expected %s\n", fs.Name(), what)
		fs.Usage()
		return nil, 2, true
	}
	return fs.Args(), 0, false
}

func checkReason(name, reason string, stderr io.Writer) (int, bool) {
	if len(reason) > maxReason {
		fmt.Fprintf(stderr, "otter admin %s: -reason is longer than %d bytes\n", name, maxReason)
		return 2, false
	}
	return 0, true
}
