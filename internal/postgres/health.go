package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/jaildeck"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// healthTimeout bounds one database's check, connecting included.
const healthTimeout = 5 * time.Second

// HealthChecker returns the node's health checker for operations.Worker:
// db is the platform DB as the node's worker.
func (d *Driver) HealthChecker(db operations.DB) operations.HealthChecker {
	return func(ctx context.Context) ([]operations.Health, error) { return d.CheckHealth(ctx, db) }
}

// CheckHealth checks each running database on the node's clusters and
// records its health (architecture.md §5, §9; #97). A database is ok when
// PostgreSQL takes a connection to it, as the worker's admin, and
// PgBouncer serves its entry, neither paused nor disabled: the two halves
// of a customer's way in. The worker has no tenant's password, so it can't
// log in as one. A cluster it can't reach has every database on it
// degraded: customers can't reach them either.
//
// Only an instance that is running, should be, and has no operation in
// flight is checked; an operation changes health on its way (a restart
// ends connections). Any other instance's health is unknown: a stopped
// database refuses connections by design.
func (d *Driver) CheckHealth(ctx context.Context, db operations.DB) ([]operations.Health, error) {
	if _, err := db.Exec(ctx, `
		UPDATE instances i SET health = 'unknown'
		WHERE health <> 'unknown' AND status <> 'deleted'
			AND NOT (status = 'running' AND desired_state = 'running')
			AND NOT EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running'))`); err != nil {
		return nil, fmt.Errorf("record health unknown: %w", err)
	}
	rows, err := db.Query(ctx, `
		SELECT c.id, c.name, c.jail_name, host(c.pool_addr), c.pg_major FROM clusters c ORDER BY c.name`)
	if err != nil {
		return nil, fmt.Errorf("list the node's clusters: %w", err)
	}
	type cl struct {
		t    target
		name string
	}
	clusters, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cl, error) {
		c := cl{t: target{cluster: Cluster{Port: d.Port}}}
		return c, r.Scan(&c.t.clusterID, &c.name, &c.t.jail, &c.t.cluster.Host, &c.t.pgMajor)
	})
	if err != nil {
		return nil, fmt.Errorf("list the node's clusters: %w", err)
	}
	var hs []operations.Health
	for _, c := range clusters {
		h, err := d.checkCluster(ctx, db, c.t, c.name)
		hs = append(hs, h...)
		if err != nil {
			return hs, fmt.Errorf("cluster %s: %w", c.name, err)
		}
	}
	return hs, nil
}

func (d *Driver) checkCluster(ctx context.Context, db operations.DB, t target, name string) ([]operations.Health, error) {
	rows, err := db.Query(ctx, `
		SELECT i.id, i.infra_name FROM instances i
		WHERE i.cluster_id = $1 AND i.status = 'running' AND i.desired_state = 'running'
			AND NOT EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running'))
		ORDER BY i.infra_name`, t.clusterID)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's running instances: %w", err)
	}
	hs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (operations.Health, error) {
		h := operations.Health{Cluster: name}
		return h, r.Scan(&h.InstanceID, &h.Database)
	})
	if err != nil || len(hs) == 0 {
		return nil, err
	}

	pool := d.Pool
	pool.Jail = t.jail
	serving, poolErr := serving(ctx, d.JailDeck, pool)
	cert, certErr := ServerCert(ctx, d.JailDeck, t.jail, t.pgMajor)
	t.cluster.ServerCert = cert
	for i := range hs {
		h := &hs[i]
		switch {
		case poolErr != nil:
			h.Detail = "PgBouncer: " + poolErr.Error()
		case serving.about(h.Database) != "":
			h.Detail = "PgBouncer: " + serving.about(h.Database)
		case certErr != nil:
			h.Detail = certErr.Error()
		default:
			h.Detail = d.connects(ctx, t.cluster, h.Database)
		}
		h.Up = h.Detail == ""
		health := "ok"
		if !h.Up {
			health = "degraded"
		}
		tag, err := db.Exec(ctx, `UPDATE instances SET health = $2 WHERE id = $1 AND status = 'running' AND health <> $2`,
			h.InstanceID, health)
		if err != nil {
			return hs[:i], fmt.Errorf("record instance %s %s: %w", h.InstanceID, health, err)
		}
		h.Changed = tag.RowsAffected() == 1
	}
	return hs, nil
}

// connects reports why PostgreSQL doesn't take a connection to database;
// "" when it does.
func (d *Driver) connects(ctx context.Context, c Cluster, database string) string {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	conn, err := ConnectDB(ctx, c, d.Credentials, database)
	if err != nil {
		return err.Error()
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
		return err.Error()
	}
	return ""
}

// servingMap is PgBouncer's otc_ entries: "" for one it serves, else why
// not.
type servingMap map[string]string

// about says why PgBouncer doesn't serve name; "" when it does.
func (s servingMap) about(name string) string {
	why, ok := s[name]
	if !ok {
		return "no entry"
	}
	return why
}

// serving reads what PgBouncer serves, from SHOW DATABASES.
func serving(ctx context.Context, jd *jaildeck.Client, p Pool) (servingMap, error) {
	out, err := p.defaults().console(ctx, jd, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	col := map[string]int{}
	for i, name := range strings.Split(lines[0], "|") {
		col[name] = i
	}
	for _, c := range []string{"name", "paused", "disabled"} {
		if _, ok := col[c]; !ok {
			return nil, fmt.Errorf("SHOW DATABASES has no %s column", c)
		}
	}
	s := servingMap{}
	for _, line := range lines[1:] {
		f := strings.Split(line, "|")
		if len(f) != len(col) || !namePattern.MatchString(f[col["name"]]) {
			continue
		}
		var why []string
		if f[col["paused"]] != "0" {
			why = append(why, "paused")
		}
		if f[col["disabled"]] != "0" {
			why = append(why, "disabled")
		}
		s[f[col["name"]]] = strings.Join(why, ", ")
	}
	return s, nil
}
