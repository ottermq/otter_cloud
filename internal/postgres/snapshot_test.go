package postgres_test

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ottermq/otter_cloud/internal/catalog"
	"github.com/ottermq/otter_cloud/internal/dbtest"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/jaildeck/jaildecktest"
	"github.com/ottermq/otter_cloud/internal/operations"
	"github.com/ottermq/otter_cloud/internal/postgres"
)

// snapshots is a platform DB with a cluster on node-a and one on node-b,
// and node-a's worker driving a fake JailDeck that knows both jails, its
// token limited as the worker's is (#169, JD-057). No PostgreSQL cluster
// is needed: a snapshot is JailDeck's.
type snapshots struct {
	t      *testing.T
	db     *dbtest.DB
	jd     *jaildecktest.Server
	driver *postgres.Driver
}

const (
	snapJail      = "otc-snap-a"
	otherNodeJail = "otc-snap-b"
)

func newSnapshots(t *testing.T) *snapshots {
	db := dbtest.New(t)
	ctx := context.Background()
	admin := db.ConnAs(dbtest.RoleAdmin)
	cluster := map[string]string{"otter:role": "cluster"}
	for i, n := range []struct{ node, cluster, jail string }{{"node-a", "pgc1", snapJail}, {"node-b", "pgc2", otherNodeJail}} {
		if _, err := instances.AddNode(ctx, admin, instances.Node{Name: n.node, CapacityMemoryMB: 8192, CapacityStorageGB: 100,
			TenantSubnet: netip.MustParsePrefix("10.9." + string(rune('1'+i)) + ".0/24"), ServiceTypes: []catalog.ServiceType{catalog.PostgreSQL}}); err != nil {
			t.Fatal(err)
		}
		if _, err := instances.AddCluster(ctx, admin, instances.Cluster{Name: n.cluster, Node: n.node, PGMajor: 18,
			JailName: n.jail, PoolAddr: netip.MustParseAddr("10.9." + string(rune('1'+i)) + ".10"), PoolPort: 6432, MemoryMB: 2048,
			CapacityStorageGB: 60, CapacityConnections: 300, CapacityDatabases: 12}); err != nil {
			t.Fatal(err)
		}
	}
	jd := jaildecktest.New(t,
		jaildeck.Jail{Name: snapJail, Status: "running", Properties: cluster},
		jaildeck.Jail{Name: otherNodeJail, Status: "running", Properties: cluster})
	jd.Limit("otter:role", "cluster")
	return &snapshots{t: t, db: db, jd: jd, driver: &postgres.Driver{JailDeck: jd.Client()}}
}

// node is node-a's worker's connection to the platform DB.
func (s *snapshots) node() *pgxpool.Pool { return s.db.PoolAs(s.db.NodeRole("node-a")) }

func (s *snapshots) take(now time.Time, every, keep time.Duration) ([]operations.Backup, error) {
	s.t.Helper()
	return s.driver.SnapshotBacker(s.node(), every, keep)(context.Background(), now)
}

func (s *snapshots) purge(now time.Time, on bool) ([]operations.Expiry, error) {
	s.t.Helper()
	return s.driver.SnapshotExpirer(s.node(), on)(context.Background(), now)
}

type snapshotRecord struct {
	Ref, Status string
	Kept        time.Duration
	Purged      bool
}

// records are the cluster's snapshot records, oldest first.
func (s *snapshots) records(cluster string) []snapshotRecord {
	s.t.Helper()
	rows, err := s.db.ConnAs(dbtest.RoleMigrator).Query(context.Background(), `
		SELECT b.ref, b.status, coalesce(extract(epoch FROM b.expires_at - b.created_at), 0), b.purged_at IS NOT NULL
		FROM backup_records b JOIN clusters c ON c.id = b.cluster_id
		WHERE c.name = $1 AND b.kind = 'snapshot' ORDER BY b.created_at, b.id`, cluster)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []snapshotRecord
	for rows.Next() {
		var r snapshotRecord
		var secs float64
		if err := rows.Scan(&r.Ref, &r.Status, &secs, &r.Purged); err != nil {
			s.t.Fatal(err)
		}
		r.Kept = time.Duration(secs) * time.Second
		out = append(out, r)
	}
	return out
}

// The acceptance of #116: through JailDeck, on a schedule, with retention,
// and listed in backup_records. Each of the node's clusters is snapshotted
// once every interval, and never another node's.
func TestClusterSnapshotSchedule(t *testing.T) {
	s := newSnapshots(t)
	const every, keep = time.Hour, 3 * time.Hour
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

	if done, err := s.take(t0, 0, keep); err != nil || len(done) != 0 {
		t.Fatalf("off: %+v, %v; want nothing", done, err)
	}
	done, err := s.take(t0, every, keep)
	if err != nil || len(done) != 1 || done[0].Kind != "snapshot" || done[0].Cluster != "pgc1" || done[0].Status != "ok" {
		t.Fatalf("first: %+v, %v; want pgc1 snapshotted", done, err)
	}
	want := jaildecktest.Dataset(snapJail) + "@" + postgres.SnapshotPrefix + "20261117T120000Z"
	if got := s.jd.Snapshots(snapJail); !slices.Equal(got, []string{want}) {
		t.Errorf("JailDeck has %v, want %s", got, want)
	}
	if rs := s.records("pgc1"); len(rs) != 1 || rs[0] != (snapshotRecord{Ref: want, Status: "ok", Kept: keep}) {
		t.Errorf("records %+v, want %s ok, kept %s", rs, want, keep)
	}
	for _, at := range []time.Time{t0, t0.Add(every - time.Minute)} {
		if done, err := s.take(at, every, keep); err != nil || len(done) != 0 {
			t.Errorf("at %s: %+v, %v; want nothing due", at, done, err)
		}
	}
	if done, err := s.take(t0.Add(every), every, keep); err != nil || len(done) != 1 {
		t.Errorf("an interval later: %+v, %v; want one", done, err)
	}
	if got := s.jd.Snapshots(snapJail); len(got) != 2 {
		t.Errorf("JailDeck has %v, want two", got)
	}
	if got, rs := s.jd.Snapshots(otherNodeJail), s.records("pgc2"); len(got) != 0 || len(rs) != 0 {
		t.Errorf("node-b's cluster: snapshots %v, records %+v; want none", got, rs)
	}
}

// A refused snapshot is recorded as failed and tried again at the next
// interval, not in a loop.
func TestClusterSnapshotFailure(t *testing.T) {
	s := newSnapshots(t)
	const every, keep = time.Hour, 3 * time.Hour
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	// The grant refuses a jail without the cluster role.
	s.jd.SetJail(jaildeck.Jail{Name: snapJail, Status: "running"})

	done, err := s.take(t0, every, keep)
	if err == nil || !strings.Contains(err.Error(), "pgc1") || len(done) != 1 || done[0].Status != "failed" {
		t.Fatalf("refused: %+v, %v; want a failed snapshot of pgc1", done, err)
	}
	if rs := s.records("pgc1"); len(rs) != 1 || rs[0].Status != "failed" || rs[0].Kept != 0 {
		t.Errorf("records %+v, want one failed, with no expiry", rs)
	}
	if done, err := s.take(t0.Add(time.Minute), every, keep); err != nil || len(done) != 0 {
		t.Errorf("a minute later: %+v, %v; want nothing until the next interval", done, err)
	}
	s.jd.SetJail(jaildeck.Jail{Name: snapJail, Status: "running", Properties: map[string]string{"otter:role": "cluster"}})
	if done, err := s.take(t0.Add(every), every, keep); err != nil || len(done) != 1 || done[0].Status != "ok" {
		t.Errorf("next interval: %+v, %v; want ok", done, err)
	}
}

// Expired snapshots are destroyed and marked purged, with an audit event;
// the newest stays while the schedule is on; one already gone is purged
// all the same; one JailDeck refuses stays, and doesn't hold up the rest.
func TestClusterSnapshotRetention(t *testing.T) {
	s := newSnapshots(t)
	const every, keep = time.Hour, 90 * time.Minute
	t0 := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for i := range 4 {
		if done, err := s.take(t0.Add(time.Duration(i)*every), every, keep); err != nil || len(done) != 1 {
			t.Fatalf("take %d: %+v, %v", i, done, err)
		}
	}
	names := s.jd.Snapshots(snapJail)
	s.jd.Hold(names[0])
	// The operator destroyed the second by hand.
	short := names[1][strings.Index(names[1], "@")+1:]
	if _, err := s.jd.Client().DestroySnapshot(context.Background(), snapJail, short, names[1]); err != nil {
		t.Fatal(err)
	}

	// At 15:30 the first three have expired; the fourth (15:00 + 90 min)
	// hasn't.
	done, err := s.purge(t0.Add(3*every+30*time.Minute), true)
	if err == nil || !strings.Contains(err.Error(), names[0]) || len(done) != 2 {
		t.Fatalf("purge: %+v, %v; want two purged and the held one refused", done, err)
	}
	if got := s.jd.Snapshots(snapJail); !slices.Equal(got, []string{names[0], names[3]}) {
		t.Errorf("JailDeck has %v, want the held one and the newest", got)
	}
	purged := func() []bool {
		var out []bool
		for _, r := range s.records("pgc1") {
			out = append(out, r.Purged)
		}
		return out
	}
	if got := purged(); !slices.Equal(got, []bool{false, true, true, false}) {
		t.Errorf("purged %v, want the second and third", got)
	}
	var events int
	if err := s.db.ConnAs(dbtest.RoleMigrator).QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE action = 'backup.purge' AND details ? 'snapshot'`).Scan(&events); err != nil || events != 2 {
		t.Errorf("audit events: %d, %v; want 2", events, err)
	}

	// Long after, the newest is kept while the schedule is on, and goes
	// once it's off.
	if done, _ := s.purge(t0.Add(48*time.Hour), true); len(done) != 0 {
		t.Errorf("schedule on: purged %+v, want the newest kept", done)
	}
	if done, _ := s.purge(t0.Add(48*time.Hour), false); len(done) != 1 {
		t.Errorf("schedule off: purged %+v, want the newest", done)
	}
	if got := purged(); !slices.Equal(got, []bool{false, true, true, true}) {
		t.Errorf("purged %v, want all but the held one", got)
	}
}
