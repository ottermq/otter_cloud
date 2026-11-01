package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/config"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// The operator's view of instances and the operations queue (#102). Only
// here is an operation's last_error shown: the API never returns it
// (portals.md §3).

func runInstancesList(args []string, stdout, stderr io.Writer) int {
	const name = "instances list"
	fs := newFlagSet("admin "+name, "List instances, newest first, with their owner and where they live", stderr)
	all := fs.Bool("all", false, "include deleted instances")
	if code, done := parse(fs, args); done {
		return code
	}
	return adminList(name, stdout, stderr, func(ctx context.Context, db instances.DB, w io.Writer) error {
		list, err := instances.OperatorInstances(ctx, db, *all)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "ID\tOWNER\tNAME\tFLAVOR\tSTATUS\tDESIRED\tREASON\tNODE\tCLUSTER\tINFRA NAME\tCREATED")
		for _, v := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", v.ID, v.Owner, v.Name, v.Flavor, v.Status,
				v.DesiredState, dash(v.Reason), v.Node, dash(v.Cluster), v.InfraName, v.CreatedAt.UTC().Format(time.RFC3339))
		}
		return nil
	})
}

// opsErrorWidth is how much of last_error a line of `ops list` shows;
// -full shows all of it.
const opsErrorWidth = 100

func runOpsList(args []string, stdout, stderr io.Writer) int {
	const name = "ops list"
	fs := newFlagSet("admin "+name, "List operations, newest first, with the last error of those not yet succeeded or canceled (operators only)", stderr)
	status := fs.String("status", "", "only operations with this status: pending, running, succeeded, failed or canceled")
	instance := fs.String("instance", "", "only this instance's operations, by id (see otter admin instances list)")
	limit := fs.Int("limit", 50, "at most this many")
	full := fs.Bool("full", false, "show each last error whole")
	if code, done := parse(fs, args); done {
		return code
	}
	return adminList(name, stdout, stderr, func(ctx context.Context, db instances.DB, w io.Writer) error {
		list, err := operations.List(ctx, db, operations.Filter{Status: *status, InstanceID: *instance, Limit: *limit})
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "ID\tINSTANCE\tNODE\tKIND\tSTATUS\tATTEMPTS\tSTEP\tREASON\tCREATED\tLAST ERROR")
		for _, o := range list {
			msg := strings.Join(strings.Fields(o.LastError), " ")
			if o.Status == "succeeded" || o.Status == "canceled" {
				// An earlier attempt's error, outlived by the outcome:
				// shown, it reads as the reason for one.
				msg = ""
			}
			if r := []rune(msg); !*full && len(r) > opsErrorWidth {
				msg = string(r[:opsErrorWidth]) + "…"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", o.ID, o.InstanceName, o.Node, o.Kind, o.Status,
				o.Attempts, dash(o.Step), dash(o.Reason), o.CreatedAt.UTC().Format(time.RFC3339), dash(msg))
		}
		return nil
	})
}

func runOpsRetry(args []string, stdout, stderr io.Writer) int {
	return opsChange("ops retry", "Put a failed operation back in the queue, due now, with its attempts counted afresh. "+
		"It resumes from its last step. A failed create can't be retried: the customer deletes it and creates another",
		operations.RetryFailed, audit.ActionOperationRetry, "back in the queue", args, stdout, stderr)
}

func runOpsCancel(args []string, stdout, stderr io.Writer) int {
	return opsChange("ops cancel", "Cancel a pending operation that has never run. A canceled create leaves its "+
		"database deleted; a canceled delete leaves it as it was",
		operations.CancelPending, audit.ActionOperationCancel, "canceled", args, stdout, stderr)
}

func opsChange(name, summary string, change func(context.Context, operations.DB, string) (operations.Change, error),
	action, done string, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("admin "+name, summary, stderr)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: otter admin %s [-reason text] <operation id>\n\n%s.\n\nFlags:\n", name, summary)
		fs.PrintDefaults()
	}
	reason := fs.String("reason", "", "why, for the audit log")
	rest, code, stop := parseArgs(fs, args, 1, "an operation id (see otter admin ops list)")
	if stop {
		return code
	}
	if code, ok := checkReason(name, *reason, stderr); !ok {
		return code
	}
	return infraChange(name, stdout, stderr, func(ctx context.Context, db instances.DB) (audit.Event, string, error) {
		c, err := change(ctx, db, rest[0])
		if err != nil {
			return audit.Event{}, "", err
		}
		details := map[string]any{"kind": c.Kind, "instance_id": c.InstanceID, "status": c.Status}
		if *reason != "" {
			details["reason"] = *reason
		}
		return audit.Event{Action: action, AccountID: c.AccountID, TargetType: "operation", TargetID: c.ID, Details: details},
			fmt.Sprintf("%s of %s (%s) is %s", c.Kind, c.InstanceName, c.ID, done), nil
	})
}

// adminList runs list on a tabwriter over an admin connection.
func adminList(name string, stdout, stderr io.Writer, list func(context.Context, instances.DB, io.Writer) error) int {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	conn, _, ok := adminConn(ctx, name, config.Needs{Database: true}, stderr)
	if !ok {
		return 1
	}
	defer conn.Close(context.Background())
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if err := list(ctx, conn, tw); err != nil {
		fmt.Fprintf(stderr, "otter admin %s: %v\n", name, err)
		return 1
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
