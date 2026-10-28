package jaildeck_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/jaildeck/jaildecktest"
)

// contract is what the worker relies on JailDeck for. It runs against the
// fake here, and against the real JailDeck in contract_test.go, so the
// fake can't drift from what it stands in for without a test saying so.
// jail is a running jail the test may run harmless commands in.
func contract(t *testing.T, c, badToken *jaildeck.Client, jail string) {
	ctx := context.Background()
	reqID := "otter-contract-" + time.Now().UTC().Format("20060102T150405.000000")
	rctx := jaildeck.WithRequestID(ctx, reqID)

	t.Run("token required", func(t *testing.T) {
		_, err := badToken.Jails(ctx)
		var e *jaildeck.Error
		if !errors.As(err, &e) || e.Status != http.StatusUnauthorized || e.Message == "" {
			t.Errorf("Jails with a bad token = %v, want a 401 with a message", err)
		}
	})
	t.Run("jails", func(t *testing.T) {
		js, err := c.Jails(ctx)
		if err != nil || !slices.ContainsFunc(js, func(j jaildeck.Jail) bool { return j.Name == jail }) {
			t.Errorf("Jails = %v, %v; want %s in it", js, err, jail)
		}
		j, err := c.Jail(ctx, jail)
		if err != nil || j.Name != jail || !j.Running() {
			t.Errorf("Jail(%s) = %+v, %v; want it running", jail, j, err)
		}
		if _, err := c.Jail(ctx, "otter-no-such-jail"); !jaildeck.IsNotFound(err) {
			t.Errorf("Jail(missing) = %v, want not found", err)
		}
	})
	t.Run("exec", func(t *testing.T) {
		r, err := c.Exec(rctx, jail, jaildeck.ExecRequest{Argv: []string{"echo", "hello", "otter"}})
		if err != nil || r.ExitCode != 0 || r.Stdout != "hello otter\n" || r.TimedOut {
			t.Errorf("echo = %+v, %v", r, err)
		}
		// A command's failure is its exit code, not an error (JD-016).
		r, err = c.Exec(rctx, jail, jaildeck.ExecRequest{Argv: []string{"sh", "-c", "exit 3"}})
		if err != nil || r.ExitCode != 3 {
			t.Errorf("exit 3 = %+v, %v", r, err)
		}
		r, err = c.Exec(rctx, jail, jaildeck.ExecRequest{Argv: []string{"printenv", "OTTER_CONTRACT"}, Env: map[string]string{"OTTER_CONTRACT": "yes"}})
		if err != nil || r.Stdout != "yes\n" {
			t.Errorf("env = %+v, %v", r, err)
		}
		r, err = c.Exec(rctx, jail, jaildeck.ExecRequest{Argv: []string{"sleep", "5"}, Timeout: time.Second})
		if err != nil || !r.TimedOut {
			t.Errorf("timeout = %+v, %v; want timed out", r, err)
		}
		if _, err := c.Exec(rctx, "otter-no-such-jail", jaildeck.ExecRequest{Argv: []string{"true"}}); !jaildeck.IsNotFound(err) {
			t.Errorf("exec in a missing jail = %v, want not found", err)
		}
	})
	t.Run("request id in the audit log", func(t *testing.T) {
		es, err := c.Audit(ctx, reqID, 50)
		if err != nil || len(es) < 4 {
			t.Fatalf("Audit(%s) = %d entries, %v; want the execs above", reqID, len(es), err)
		}
		var exit3, timeout bool
		for _, e := range es {
			// Success is the exec's, not the command's: exit 3 and the
			// timeout are recorded as done.
			if e.RequestID != reqID || e.Operation != "exec" || !e.Success {
				t.Errorf("entry %+v, want a successful exec with request id %s", e, reqID)
			}
			// What the command did is the entry's too (v0.18.1, jaildeck#141).
			switch e.Command {
			case `["sh","-c","exit 3"]`:
				exit3 = e.ExitCode == 3 && !e.TimedOut
			case `["sleep","5"]`:
				timeout = e.TimedOut && e.ExitCode == -1 && e.DurationMS >= 900 && e.DurationMS < 5000
			}
		}
		if !exit3 || !timeout {
			t.Errorf("exit 3 audited as such %t, the timeout %t:\n%+v", exit3, timeout, es)
		}
	})
}

func TestContractAgainstTheFake(t *testing.T) {
	jd := jaildecktest.New(t, jaildeck.Jail{Name: "otc-pg-test", Status: "running"})
	contract(t, jd.Client(), jaildeck.New(jd.Socket, "wrong"), "otc-pg-test")
}

func TestRequestIDIsDroppedWhenJailDeckWouldDropIt(t *testing.T) {
	jd := jaildecktest.New(t, jaildeck.Jail{Name: "j1", Status: "running"})
	ctx := jaildeck.WithRequestID(context.Background(), "has a space")
	if _, err := jd.Client().Exec(ctx, "j1", jaildeck.ExecRequest{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if log := jd.AuditLog(); len(log) != 1 || log[0].RequestID != "" {
		t.Errorf("audit = %+v", log)
	}
}

func TestExecRefusals(t *testing.T) {
	jd := jaildecktest.New(t, jaildeck.Jail{Name: "stopped", Status: "stopped"})
	c := jd.Client()
	ctx := context.Background()
	if _, err := c.Exec(ctx, "stopped", jaildeck.ExecRequest{}); err == nil || !strings.Contains(err.Error(), "needs a command") {
		t.Errorf("empty argv = %v", err)
	}
	var e *jaildeck.Error
	if _, err := c.Exec(ctx, "stopped", jaildeck.ExecRequest{Argv: []string{"true"}}); !errors.As(err, &e) || e.Status != http.StatusConflict {
		t.Errorf("exec in a stopped jail = %v, want 409", err)
	}
	if !strings.Contains(e.Error(), "jail is not running") {
		t.Errorf("error = %q, want JailDeck's message", e.Error())
	}
}

func TestTokenNeverInErrors(t *testing.T) {
	c := jaildeck.New("/nonexistent/jd.sock", "jdt_secret_value")
	_, err := c.Jails(context.Background())
	if err == nil || strings.Contains(err.Error(), "jdt_secret_value") {
		t.Errorf("err = %v", err)
	}
}
