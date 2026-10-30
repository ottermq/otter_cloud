package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/postgres"
	"github.com/ottermq/otter_cloud/internal/scram"
)

func TestRenderDatabases(t *testing.T) {
	a := postgres.PoolEntry{Name: "otc_00000000000b", MaxConnections: 20, PoolMode: "transaction"}
	b := postgres.PoolEntry{Name: "otc_00000000000a", MaxConnections: 30, PoolMode: "session"}
	one, err := postgres.RenderDatabases([]postgres.PoolEntry{a, b})
	if err != nil {
		t.Fatal(err)
	}
	two, _ := postgres.RenderDatabases([]postgres.PoolEntry{b, a})
	if one != two || strings.Index(one, b.Name) > strings.Index(one, a.Name) {
		t.Errorf("render is not sorted and stable:\n%s\n%s", one, two)
	}
	for name, es := range map[string][]postgres.PoolEntry{
		"twice":     {a, a},
		"name":      {{Name: "otc_x; drop", MaxConnections: 20, PoolMode: "transaction"}},
		"newline":   {{Name: "otc_00000000000c\nadmin_users = x", MaxConnections: 20, PoolMode: "transaction"}},
		"mode":      {{Name: "otc_00000000000c", MaxConnections: 20, PoolMode: "statement"}},
		"no limit":  {{Name: "otc_00000000000c", MaxConnections: 0, PoolMode: "transaction"}},
		"too large": {{Name: "otc_00000000000c", MaxConnections: 100000, PoolMode: "transaction"}},
	} {
		if _, err := postgres.RenderDatabases(es); !errors.Is(err, postgres.ErrPoolRender) {
			t.Errorf("%s: %v, want ErrPoolRender", name, err)
		}
	}
}

// The acceptance of #52: idempotent, and the tenant reaches its database
// through PgBouncer once its entry is there.
func TestEnsurePoolEntries(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	jd := c.JailDeck().Client()
	p := c.Pool(jd)
	conn := c.admin()
	defer conn.Close(ctx)
	// A generated password, as a customer gets: it logs in through PgBouncer.
	password := scram.Password()
	tn := c.tenant(password)
	if err := c.ensure(conn, tn); err != nil {
		t.Fatal(err)
	}
	entry := postgres.PoolEntry{Name: tn.Name, MaxConnections: 20, PoolMode: "transaction"}

	if changed, err := postgres.EnsurePoolEntries(ctx, jd, p, []postgres.PoolEntry{entry}); err != nil || !changed {
		t.Fatalf("first: changed %t, %v", changed, err)
	}
	if changed, err := postgres.EnsurePoolEntries(ctx, jd, p, []postgres.PoolEntry{entry}); err != nil || changed {
		t.Errorf("again: changed %t, %v; want nothing to do", changed, err)
	}

	db, err := c.asTenantVia("pool-port", tn, password)
	if err != nil {
		t.Fatalf("tenant through PgBouncer: %v", err)
	}
	var user string
	if err := db.QueryRow(ctx, "SELECT current_user").Scan(&user); err != nil || user != tn.Name {
		t.Errorf("through PgBouncer as %q, %v", user, err)
	}
	db.Close(ctx)

	// Removing the entry takes it out of PgBouncer at once.
	if changed, err := postgres.EnsurePoolEntries(ctx, jd, p, nil); err != nil || !changed {
		t.Fatalf("remove: changed %t, %v", changed, err)
	}
	if db, err := c.asTenantVia("pool-port", tn, password); err == nil {
		db.Close(ctx)
		t.Error("the tenant still reaches its database after its entry was removed")
	}
}

// The acceptance of #52: a file PgBouncer refuses never stays: the
// previous one is put back, and PgBouncer keeps serving it.
func TestEnsurePoolEntriesRestoresOnRefusal(t *testing.T) {
	c := cluster(t)
	ctx := context.Background()
	jd := c.JailDeck().Client()
	p := c.Pool(jd)
	good := postgres.PoolEntry{Name: "otc_00000000000a", MaxConnections: 20, PoolMode: "transaction"}
	if _, err := postgres.EnsurePoolEntries(ctx, jd, p, []postgres.PoolEntry{good}); err != nil {
		t.Fatal(err)
	}
	before := c.cat(jd, postgres.DatabasesFile)

	// A render that fails the worker's check is never written.
	if _, err := postgres.EnsurePoolEntries(ctx, jd, p, []postgres.PoolEntry{good, good}); !errors.Is(err, postgres.ErrPoolRender) {
		t.Fatalf("duplicate entry: %v, want ErrPoolRender", err)
	}
	if after := c.cat(jd, postgres.DatabasesFile); after != before {
		t.Errorf("a refused render changed the file:\n%s", after)
	}

	// One that passes it but that PgBouncer won't take: the grammar is
	// narrow enough that this is a version mismatch on the node, so it is
	// simulated by a RELOAD that fails. The new file was written; it must
	// not stay.
	broken := p
	broken.AdminUser = "nobody"
	if _, err := postgres.EnsurePoolEntries(ctx, jd, broken, []postgres.PoolEntry{good, {Name: "otc_00000000000b", MaxConnections: 20, PoolMode: "transaction"}}); !errors.Is(err, postgres.ErrPoolReload) {
		t.Fatalf("err = %v, want ErrPoolReload", err)
	}
	if after := c.cat(jd, postgres.DatabasesFile); after != before {
		t.Errorf("the file changed:\n%s\nwant\n%s", after, before)
	}
	if changed, err := postgres.EnsurePoolEntries(ctx, jd, p, []postgres.PoolEntry{good}); err != nil || changed {
		t.Errorf("after the restore: changed %t, %v; want the good file in place and live", changed, err)
	}
}

func (c testCluster) cat(jd *jaildeck.Client, path string) string {
	c.t.Helper()
	r, err := jd.Exec(context.Background(), "otc-pg-test", jaildeck.ExecRequest{Argv: []string{"cat", path}})
	if err != nil || r.ExitCode != 0 {
		c.t.Fatalf("cat %s: %+v, %v", path, r, err)
	}
	return r.Stdout
}
