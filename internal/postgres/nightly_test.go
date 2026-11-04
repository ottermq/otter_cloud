package postgres_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// nightly runs the node's nightly dump as of now.
func (p *provisioning) nightly(now time.Time) ([]operations.Backup, error) {
	p.t.Helper()
	return p.driver.NightlyDump(context.Background(), p.db.PoolAs(p.db.NodeRole("node-a")), now)
}

type nightlyRecord struct {
	Ref, Status string
	Kept        time.Duration // from its creation to its expiry
	Purged      bool
}

// nightlies are the instance's nightly dump records, oldest first.
func (p *provisioning) nightlies(c instances.Created) []nightlyRecord {
	p.t.Helper()
	rows, err := p.db.ConnAs(dbtest.RoleMigrator).Query(context.Background(), `
		SELECT ref, status, coalesce(extract(epoch FROM expires_at - created_at), 0), purged_at IS NOT NULL
		FROM backup_records WHERE instance_id = $1 AND kind = 'dump' AND target = 'local'
		ORDER BY created_at, id`, c.Instance.ID)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []nightlyRecord
	for rows.Next() {
		var r nightlyRecord
		var secs float64
		if err := rows.Scan(&r.Ref, &r.Status, &secs, &r.Purged); err != nil {
			p.t.Fatal(err)
		}
		r.Kept = time.Duration(secs) * time.Second
		out = append(out, r)
	}
	return out
}

// dumpOnce dumps the one due database, which must be c.
func (p *provisioning) dumpOnce(c instances.Created, now time.Time) {
	p.t.Helper()
	done, err := p.nightly(now)
	if err != nil || len(done) != 1 || done[0].InstanceID != c.Instance.ID || done[0].Status != "ok" {
		p.t.Fatalf("nightly dump = %+v, %v; want %s dumped", done, err, c.Instance.InfraName)
	}
}

// The acceptance of #117: a database's nightly dump restores into an empty
// database, with the customer's table and row.
func TestNightlyDumpRestores(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.dumpOnce(c, time.Now())
	ds := p.nightlies(c)
	if len(ds) != 1 || ds[0].Status != "ok" {
		t.Fatalf("records %+v, want one ok dump", ds)
	}

	ctx := context.Background()
	const scratch = "restore_check"
	admin := p.admin()
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn := p.admin()
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+scratch+" WITH (FORCE)")
	})
	certs := filepath.Dir(p.driver.Dumper.CertFile)
	root := filepath.Join(certs, "server.crt")
	if err := os.WriteFile(root, p.File("server.crt"), 0o600); err != nil {
		t.Fatal(err)
	}
	conninfo := "host=" + p.Host() + " port=" + strconv.Itoa(p.Port()) + " dbname=" + scratch + " user=" + postgres.AdminRole +
		" sslmode=verify-ca sslrootcert=" + root + " sslcert=" + p.driver.Dumper.CertFile + " sslkey=" + p.driver.Dumper.KeyFile
	argv := p.Docker([]string{certs, p.driver.Dumper.Dir}, "pg_restore", "--no-owner", "--no-privileges", "--exit-on-error",
		"--dbname="+conninfo, filepath.Join(p.driver.Dumper.Dir, ds[0].Ref))
	if out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("pg_restore: %v\n%s", err, out)
	}
	restored, err := postgres.ConnectDB(ctx, p.Target(), p.Creds("admin"), scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(ctx)
	var body string
	if err := restored.QueryRow(ctx, `SELECT body FROM notes`).Scan(&body); err != nil || body != "keep me" {
		t.Errorf("restored notes: %q, %v; want the customer's row", body, err)
	}
}

// Each running database is dumped once a night, one a call, the one with
// the oldest dump first; the first dump in a week is kept 28 days, the
// others 7.
func TestNightlyDumpSchedule(t *testing.T) {
	p := newProvisioning(t)
	a, b := p.running("alpha"), p.running("beta")
	now := time.Now()
	first, err := p.nightly(now)
	if err != nil || len(first) != 1 {
		t.Fatalf("first: %+v, %v", first, err)
	}
	second, err := p.nightly(now)
	if err != nil || len(second) != 1 || second[0].InstanceID == first[0].InstanceID {
		t.Fatalf("second: %+v, %v; want the other database", second, err)
	}
	if done, err := p.nightly(now); err != nil || len(done) != 0 {
		t.Fatalf("both dumped, yet: %+v, %v", done, err)
	}
	tomorrow := now.Add(24 * time.Hour)
	p.dumpOnce(a, tomorrow)
	p.dumpOnce(b, tomorrow)
	for _, c := range []instances.Created{a, b} {
		ds := p.nightlies(c)
		if len(ds) != 2 || !near(ds[0].Kept, postgres.WeeklyDumpRetention) || !near(ds[1].Kept, postgres.DailyDumpRetention) {
			t.Errorf("%s: %+v, want a weekly dump then a daily one", c.Instance.Name, ds)
		}
	}
}

func near(d, want time.Duration) bool { return d > want-time.Minute && d <= want+time.Minute }

func TestDumpDueSince(t *testing.T) {
	loc := time.FixedZone("node", -3*3600)
	for _, tc := range []struct{ now, want time.Time }{
		{time.Date(2026, 11, 4, 2, 59, 0, 0, loc), time.Date(2026, 11, 3, postgres.DumpHour, 0, 0, 0, loc)},
		{time.Date(2026, 11, 4, 3, 0, 0, 0, loc), time.Date(2026, 11, 4, postgres.DumpHour, 0, 0, 0, loc)},
		{time.Date(2026, 11, 4, 23, 0, 0, 0, loc), time.Date(2026, 11, 4, postgres.DumpHour, 0, 0, 0, loc)},
	} {
		if got := postgres.DumpDueSince(tc.now); !got.Equal(tc.want) {
			t.Errorf("DumpDueSince(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}

// A failed dump is recorded and left for the next night, and the other
// databases are still dumped; a stopped database is not dumped.
func TestNightlyDumpFailsAndStopped(t *testing.T) {
	p := newProvisioning(t)
	a := p.running("alpha")
	dumper := p.driver.Dumper
	p.driver.Dumper.Command = []string{"false"}
	done, err := p.nightly(time.Now())
	if err == nil || len(done) != 1 || done[0].Status != "failed" {
		t.Fatalf("with pg_dump failing: %+v, %v", done, err)
	}
	if ds := p.nightlies(a); len(ds) != 1 || ds[0].Status != "failed" {
		t.Errorf("records %+v, want the failure", ds)
	}
	if entries, _ := os.ReadDir(dumper.Dir); len(entries) != 0 {
		t.Errorf("a failed dump left %d files", len(entries))
	}
	p.driver.Dumper = dumper
	b := p.running("beta")
	p.dumpOnce(b, time.Now())
	if done, err := p.nightly(time.Now()); err != nil || len(done) != 0 {
		t.Errorf("the failed one retried the same night: %+v, %v", done, err)
	}

	p.request("stop", a)
	p.runOnce()
	if s := p.status(a); s != "stopped" {
		t.Fatalf("after the stop: %s", s)
	}
	p.dumpOnce(b, time.Now().Add(24*time.Hour))
	if done, err := p.nightly(time.Now().Add(24 * time.Hour)); err != nil || len(done) != 0 {
		t.Errorf("a stopped database was dumped: %+v, %v", done, err)
	}
}

// Retention removes expired nightly dumps but never a live database's
// newest. A delete shortens them to its final dump's, and an ephemeral
// database's go at its delete.
func TestNightlyDumpRetention(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("production")
	p.dumpOnce(c, time.Now())
	p.dumpOnce(c, time.Now().Add(24*time.Hour))
	ds := p.nightlies(c)

	// Both expired, the newest kept.
	if done := p.purge(time.Now().Add(postgres.WeeklyDumpRetention + time.Hour)); len(done) != 1 {
		t.Fatalf("purged %+v, want only the older dump", done)
	}
	if ds = p.nightlies(c); !ds[0].Purged || ds[1].Purged {
		t.Fatalf("records %+v, want the older purged and the newest kept", ds)
	}
	if _, err := os.Stat(filepath.Join(p.driver.Dumper.Dir, ds[1].Ref)); err != nil {
		t.Errorf("the newest dump is gone: %v", err)
	}

	c = p.delete(c)
	p.runOnce()
	if s := p.status(c); s != "deleted" {
		t.Fatalf("after the delete: %s", s)
	}
	if done := p.purge(time.Now().Add(postgres.UserDeleteRetention - time.Hour)); len(done) != 0 {
		t.Errorf("before the final dump's expiry: %+v", done)
	}
	if done := p.purge(time.Now().Add(postgres.UserDeleteRetention + time.Minute)); len(done) != 2 {
		t.Errorf("at the final dump's expiry: %+v, want it and the nightly dump", done)
	}
	if entries, _ := os.ReadDir(p.driver.Dumper.Dir); len(entries) != 0 {
		t.Errorf("%d files left after a deleted database's dumps expired", len(entries))
	}
}

func TestNightlyDumpsOfAnEphemeralDatabase(t *testing.T) {
	p := newProvisioning(t)
	c := p.running("scratch")
	if _, err := p.db.ConnAs(dbtest.RoleMigrator).Exec(context.Background(),
		`UPDATE instances SET ephemeral = true WHERE id = $1`, c.Instance.ID); err != nil {
		t.Fatal(err)
	}
	p.dumpOnce(c, time.Now())
	c = p.delete(c)
	p.runOnce()
	if done := p.purge(time.Now().Add(time.Second)); len(done) != 1 {
		t.Errorf("purged %+v, want the nightly dump at once", done)
	}
	if entries, _ := os.ReadDir(p.driver.Dumper.Dir); len(entries) != 0 {
		t.Errorf("%d files left after an ephemeral database's delete", len(entries))
	}
}
