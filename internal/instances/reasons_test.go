package instances

import (
	"regexp"
	"strings"
	"testing"
)

// Every code fits the column's check, and every message is something a
// customer can read: a sentence, naming nothing inside the platform.
func TestReasons(t *testing.T) {
	column := regexp.MustCompile(`^[a-z][a-z_]{1,62}$`) // instances.reason, operations.reason
	for code, msg := range reasons {
		if !column.MatchString(code) {
			t.Errorf("%q doesn't fit the reason column", code)
		}
		if !strings.HasSuffix(msg, ".") || msg[0] < 'A' || msg[0] > 'Z' {
			t.Errorf("%s: %q is not a sentence", code, msg)
		}
		for _, internal := range []string{"node", "cluster", "jail", "PgBouncer", "SQL", "worker", "otc_", "step"} {
			if strings.Contains(msg, internal) {
				t.Errorf("%s: %q names %q", code, msg, internal)
			}
		}
	}
	if ReasonMessage("") != "" || ReasonMessage("no_such_code") != unknownReason || !IsReason("create_failed") {
		t.Error("ReasonMessage or IsReason")
	}
}

func TestRequester(t *testing.T) {
	for actor, want := range map[string]string{
		"user:0190a000-0000-7000-8000-000000000000": "you",
		"operator:ops": "operator",
		"system":       "system",
	} {
		if got := requester(actor); got != want {
			t.Errorf("requester(%q) = %q, want %q", actor, got, want)
		}
	}
}
