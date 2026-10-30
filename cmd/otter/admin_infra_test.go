package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/dbtest"
)

func TestAdminNodeCluster(t *testing.T) {
	db := dbtest.New(t)
	withEnv(t, adminEnv(t, db))
	admin := func(args ...string) (string, string, int) {
		var out, errOut bytes.Buffer
		code := run(append([]string{"admin"}, args...), &out, &errOut)
		return out.String(), errOut.String(), code
	}

	if out, errOut, code := admin("node", "add", "-memory-mb", "8192", "-storage-gb", "200", "-tenant-subnet", "10.64.0.0/22", "node-01"); code != 0 ||
		!strings.Contains(out, "node node-01 added") || !strings.Contains(out, "CREATE ROLE otter_node_node_01 LOGIN IN ROLE otter_worker;") {
		t.Fatalf("node add = %d %q %q", code, out, errOut)
	}
	if _, errOut, code := admin("node", "add", "-memory-mb", "8192", "node-02"); code != 2 || !strings.Contains(errOut, "required") {
		t.Errorf("node add without subnet = %d %q", code, errOut)
	}
	if out, errOut, code := admin("cluster", "add", "-node", "node-01", "-pool-addr", "10.64.0.10", "-memory-mb", "2048",
		"-storage-gb", "60", "-connections", "300", "-databases", "12", "pgc1"); code != 0 || !strings.Contains(out, "cluster pgc1 added on node node-01") {
		t.Fatalf("cluster add = %d %q %q", code, out, errOut)
	}
	if out, _, code := admin("cluster", "list"); code != 0 || !strings.Contains(out, "otc-pgc1") || !strings.Contains(out, "10.64.0.10:6432") {
		t.Errorf("cluster list = %d %q", code, out)
	}

	if out, errOut, code := admin("node", "drain", "-reason", "disk swap", "node-01"); code != 0 || !strings.Contains(out, "node node-01 is draining") {
		t.Fatalf("node drain = %d %q %q", code, out, errOut)
	}
	if out, _, code := admin("node", "drain", "node-01"); code != 0 || !strings.Contains(out, "already draining") {
		t.Errorf("drain again = %d %q", code, out)
	}
	if out, _, code := admin("node", "list"); code != 0 || !strings.Contains(out, "draining") || !strings.Contains(out, "10.64.0.0/22") {
		t.Errorf("node list = %d %q", code, out)
	}
	if out, _, code := admin("node", "activate", "node-01"); code != 0 || !strings.Contains(out, "is active") {
		t.Errorf("node activate = %d %q", code, out)
	}
	if out, _, code := admin("cluster", "drain", "pgc1"); code != 0 || !strings.Contains(out, "cluster pgc1 is draining") {
		t.Errorf("cluster drain = %d %q", code, out)
	}
	if _, errOut, code := admin("cluster", "activate", "pgc9"); code != 1 || !strings.Contains(errOut, "no cluster") {
		t.Errorf("unknown cluster = %d %q", code, errOut)
	}

	var actions string
	if err := db.ConnAs(dbtest.RoleAPI).QueryRow(context.Background(), `
		SELECT string_agg(action || ':' || coalesce(details->>'to', '') || ':' || coalesce(details->>'reason', ''), ',' ORDER BY at, id)
		FROM audit_events WHERE target_type IN ('node', 'cluster')`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	// The second drain changed nothing and wrote no row.
	want := "node.add::,cluster.add::,node.set_status:draining:disk swap,node.set_status:active:,cluster.set_status:draining:"
	if actions != want {
		t.Errorf("audit = %q\nwant    %q", actions, want)
	}
}
