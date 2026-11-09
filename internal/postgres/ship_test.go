package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/ottermq/otter_cloud/internal/backup"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// offsite is a node's shipper to a target, and the identity, kept off the
// node, that opens what it sends.
type offsite struct {
	*backup.Shipper
	id *age.X25519Identity
}

func (p *provisioning) offsite(name string, target backup.Target) offsite {
	p.t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		p.t.Fatal(err)
	}
	return offsite{&backup.Shipper{Name: name, Target: target, Sealer: backup.Sealer{Recipients: []age.Recipient{id.Recipient()}},
		Dir: p.driver.Dumper.Dir}, id}
}

func (p *provisioning) ship(o offsite, now time.Time) ([]operations.Backup, error) {
	return o.Ship(context.Background(), p.db.PoolAs(p.db.NodeRole("node-a")), now)
}

func (p *provisioning) purgeCopies(o offsite, now time.Time) []operations.Expiry {
	p.t.Helper()
	done, err := o.Purge(context.Background(), p.db.PoolAs(p.db.NodeRole("node-a")), now)
	if err != nil {
		p.t.Fatal(err)
	}
	return done
}

type copyRecord struct {
	Kind, Ref, Status string
	SameExpiry        bool // as its source's
	Purged            bool
}

// copies are the records of the instance's copies on target, oldest first.
func (p *provisioning) copies(c instances.Created, target string) []copyRecord {
	p.t.Helper()
	rows, err := p.db.ConnAs(dbtest.RoleMigrator).Query(context.Background(), `
		SELECT b.kind, b.ref, b.status, b.expires_at IS NOT DISTINCT FROM s.expires_at, b.purged_at IS NOT NULL
		FROM backup_records b JOIN backup_records s ON s.id = b.source_id
		WHERE b.instance_id = $1 AND b.target = $2 ORDER BY b.created_at, b.id`, c.Instance.ID, target)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []copyRecord
	for rows.Next() {
		var r copyRecord
		if err := rows.Scan(&r.Kind, &r.Ref, &r.Status, &r.SameExpiry, &r.Purged); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// The acceptance of #118: a nightly dump leaves the node sealed, and the
// identity kept off it opens the copy into the dump itself.
func TestShipNightlyDump(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	dir := t.TempDir()
	o := p.offsite("fs", backup.FS{Dir: dir})
	now := time.Now()
	p.dumpOnce(c, now)

	done, err := p.ship(o, now)
	if err != nil || len(done) != 1 || done[0].Status != "ok" || done[0].Target != "fs" || done[0].Kind != "dump" {
		t.Fatalf("ship: %+v, %v", done, err)
	}
	ds, cs := p.nightlies(c), p.copies(c, "fs")
	if len(cs) != 1 || cs[0].Status != "ok" || !cs[0].SameExpiry || cs[0].Ref != backup.Key(ds[0].Ref) {
		t.Fatalf("copies %+v of %+v", cs, ds)
	}
	sealed, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(cs[0].Ref)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte("age-encryption.org/v1\n")) || bytes.Contains(sealed, []byte("PGDMP")) {
		t.Error("the copy is not sealed")
	}
	r, err := age.Decrypt(bytes.NewReader(sealed), o.id)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(r)
	local, _ := os.ReadFile(filepath.Join(p.driver.Dumper.Dir, ds[0].Ref))
	if !bytes.Equal(plain, local) {
		t.Error("the unsealed copy is not the dump")
	}

	if done, err := p.ship(o, now); err != nil || len(done) != 0 {
		t.Errorf("shipped again: %+v, %v", done, err)
	}
	// A copy is not a dump: the next night's is still due.
	p.dumpOnce(c, now.Add(24*time.Hour))
}

// With no target, each dump is recorded as not copied, once.
func TestShipNone(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	o := p.offsite("none", nil)
	p.dumpOnce(c, time.Now())
	if done, err := p.ship(o, time.Now()); err != nil || len(done) != 1 || done[0].Status != "skipped" {
		t.Fatalf("ship: %+v, %v", done, err)
	}
	if done, err := p.ship(o, time.Now().Add(2*backup.RetryAfter)); err != nil || len(done) != 0 {
		t.Errorf("recorded twice: %+v, %v", done, err)
	}
	if cs := p.copies(c, "none"); len(cs) != 1 || cs[0].Status != "skipped" {
		t.Errorf("copies %+v", cs)
	}
}

type brokenTarget struct{ backup.FS }

func (brokenTarget) Put(context.Context, string, backup.Blob) error {
	return errors.New("the target is down")
}

// A copy that fails is recorded and tried again after RetryAfter; one over
// MaxBytes fails without reaching the target.
func TestShipFails(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	now := time.Now()
	p.dumpOnce(c, now)

	down := p.offsite("fs", brokenTarget{})
	if done, err := p.ship(down, now); err == nil || len(done) != 1 || done[0].Status != "failed" {
		t.Fatalf("ship to a target that is down: %+v, %v", done, err)
	}
	if done, err := p.ship(down, now.Add(time.Minute)); err != nil || len(done) != 0 {
		t.Errorf("retried at once: %+v, %v", done, err)
	}

	dir := t.TempDir()
	full := p.offsite("fs", backup.FS{Dir: dir})
	full.MaxBytes = 100
	if done, err := p.ship(full, now.Add(backup.RetryAfter+time.Minute)); err == nil || len(done) != 1 || done[0].Status != "failed" {
		t.Fatalf("ship over MaxBytes: %+v, %v", done, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a copy over MaxBytes reached the target")
	}

	full.MaxBytes = 0
	if done, err := p.ship(full, now.Add(2*backup.RetryAfter+2*time.Minute)); err != nil || len(done) != 1 || done[0].Status != "ok" {
		t.Fatalf("ship once the target is back: %+v, %v", done, err)
	}
	if cs := p.copies(c, "fs"); len(cs) != 3 || cs[2].Status != "ok" {
		t.Errorf("copies %+v, want two failures and the copy", cs)
	}
}

// Retention removes copies from the target as it does dumps from the node,
// each keeping a live database's newest. A delete's final dump is copied
// first, and the copies go with it.
func TestShipRetention(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	dir := t.TempDir()
	o := p.offsite("fs", backup.FS{Dir: dir})
	now := time.Now()
	p.dumpOnce(c, now)
	p.dumpOnce(c, now.Add(24*time.Hour))
	for range 2 {
		if done, err := p.ship(o, now.Add(24*time.Hour)); err != nil || len(done) != 1 {
			t.Fatalf("ship: %+v, %v", done, err)
		}
	}

	later := now.Add(postgres.WeeklyDumpRetention + time.Hour)
	if done := p.purgeCopies(o, later); len(done) != 1 {
		t.Fatalf("purged copies %+v, want the older", done)
	}
	if done := p.purge(later); len(done) != 1 {
		t.Fatalf("purged dumps %+v, want the older: the copies are not the node's newest", done)
	}
	if cs := p.copies(c, "fs"); len(cs) != 2 || !cs[0].Purged || cs[1].Purged {
		t.Fatalf("copies %+v, want the older purged", cs)
	}
	if objects, _ := (backup.FS{Dir: dir}).List(context.Background(), ""); len(objects) != 1 {
		t.Errorf("the target holds %v", objects)
	}

	c = p.delete(c)
	p.runOnce()
	if s := p.status(c); s != "deleted" {
		t.Fatalf("after the delete: %s", s)
	}
	if done, err := p.ship(o, time.Now()); err != nil || len(done) != 1 || done[0].Kind != "final_dump" {
		t.Fatalf("ship after the delete: %+v, %v; want the final dump first", done, err)
	}
	if done := p.purgeCopies(o, time.Now().Add(postgres.UserDeleteRetention-time.Hour)); len(done) != 0 {
		t.Errorf("before the final dump's expiry: %+v", done)
	}
	if done := p.purgeCopies(o, time.Now().Add(postgres.UserDeleteRetention+time.Minute)); len(done) != 2 {
		t.Errorf("at the final dump's expiry: %+v, want its copy and the nightly one", done)
	}
	if objects, _ := (backup.FS{Dir: dir}).List(context.Background(), ""); len(objects) != 0 {
		t.Errorf("the target still holds %v", objects)
	}
}
