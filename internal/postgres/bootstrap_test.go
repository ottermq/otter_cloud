package postgres_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/clustertest"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// #216: running the cluster's bootstrap again, as the rerun after a reclone
// does, keeps PgBouncer's passwords and the worker's routing, and saves
// its arguments in the etc dataset.
func TestBootstrapRerunKeepsPasswords(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	container := strings.TrimSpace(string(p.File("container")))
	inJail := func(script string) string {
		t.Helper()
		out, err := exec.Command("docker", "exec", "-u", "root", container, "sh", "-euc", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}
	read := func() string {
		return inJail("cat /usr/local/etc/otter/pgbouncer.users /usr/local/etc/otter/pgbouncer.pgpass /usr/local/etc/otter/pgbouncer.d/otter-databases.ini")
	}
	before := read()
	if !strings.Contains(before, c.Instance.InfraName) {
		t.Fatalf("the database isn't routed before the rerun:\n%s", before)
	}
	out := inJail(`PGB_USER=postgres PGB_RUN=/var/run/postgresql PGB_LOG=/var/log/postgresql \
		/otter/otter-pg-bootstrap -n 0.0.0.0/0 -l "$(awk 'END { print $1 }' /etc/hosts)" pgbouncer`)
	if !strings.Contains(out, "otter-pg-bootstrap: pgbouncer") {
		t.Fatalf("the rerun: %s", out)
	}
	if after := read(); after != before {
		t.Errorf("the rerun changed the passwords or the routing:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if args := inJail("cat /usr/local/etc/otter/pg-bootstrap.args"); !strings.HasPrefix(args, "-n\n0.0.0.0/0\n-l\n") {
		t.Errorf("saved arguments: %q", args)
	}

	// PgBouncer, still running on the files it read at start, takes the
	// tenant and the worker's admin console as before: the role's password
	// was set again, to the same value.
	db, err := p.asTenantVia("pool-port", postgres.Tenant{Name: c.Instance.InfraName}, c.Password)
	if err != nil {
		t.Fatalf("log in through PgBouncer after the rerun: %v", err)
	}
	_ = db.Close(context.Background())
	if _, err := postgres.EnsurePoolEntries(context.Background(), p.JailDeck().Client(), postgres.Pool{Jail: clustertest.Jail}, nil); err != nil {
		t.Errorf("the admin console after the rerun: %v", err)
	}
}
