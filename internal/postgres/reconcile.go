package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/operations"
)

// ReasonMissing is the reason of an instance whose database is gone from
// its cluster.
const ReasonMissing = "infrastructure_missing"

// Reconciler returns the node's reconciler for operations.Worker: db is
// the platform DB as the node's worker.
func (d *Driver) Reconciler(db operations.DB) operations.Reconciler {
	return func(ctx context.Context) ([]operations.Finding, error) { return d.Reconcile(ctx, db) }
}

// Reconcile compares each of the node's clusters with the platform DB
// (architecture.md §5, #96):
//
//   - an instance that should have a database (running, stopped, or on its
//     way between the two) whose database is gone is failed, with
//     ReasonMissing. Never recreated: an empty database where the
//     customer's data was would hide the loss;
//   - a database whose instance is deleted gets a delete queued, which
//     takes the final dump before dropping it: the intent is known;
//   - an otc_ database no instance on the cluster names is reported, and
//     left alone;
//   - PgBouncer is made to serve exactly what the rows say (an entry holds
//     no data), and a change is reported.
//
// An instance with an operation in flight is the operation's business and
// is skipped. A cluster the worker can't reach is reported and judged no
// further: an outage is not a loss. The worker runs it between operations,
// so none of its own runs alongside.
func (d *Driver) Reconcile(ctx context.Context, db operations.DB) ([]operations.Finding, error) {
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
	var findings []operations.Finding
	for _, c := range clusters {
		fs, err := d.reconcileCluster(ctx, db, c.t, c.name)
		findings = append(findings, fs...)
		if err != nil {
			return findings, fmt.Errorf("cluster %s: %w", c.name, err)
		}
	}
	return findings, nil
}

type rowState struct {
	id, status   string
	inFlight     bool
	deleteFailed bool // a delete queued after the instance was deleted failed
}

func (d *Driver) reconcileCluster(ctx context.Context, db operations.DB, t target, name string) ([]operations.Finding, error) {
	found := func(kind, subject, detail string) operations.Finding {
		return operations.Finding{Kind: kind, Cluster: name, Subject: subject, Detail: detail}
	}
	// The rows first, then the cluster: a row read before the listing
	// can't name a database made after it.
	rows, err := db.Query(ctx, `
		SELECT i.infra_name, i.id, i.status,
			EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running')),
			EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.kind = 'delete' AND o.status = 'failed'
				AND o.created_at > i.deleted_at)
		FROM instances i WHERE i.cluster_id = $1`, t.clusterID)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's instances: %w", err)
	}
	byName := map[string]rowState{}
	var r rowState
	var infra string
	if _, err := pgx.ForEachRow(rows, []any{&infra, &r.id, &r.status, &r.inFlight, &r.deleteFailed}, func() error {
		byName[infra] = r
		return nil
	}); err != nil {
		return nil, fmt.Errorf("list the cluster's instances: %w", err)
	}

	conn, err := d.connect(ctx, &t)
	if err != nil {
		return []operations.Finding{found("cluster_unreachable", t.jail, err.Error())}, nil
	}
	defer conn.Close(context.WithoutCancel(ctx))
	dbRows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datname ~ '^otc_[0-9a-f]{12}$'`)
	if err != nil {
		return []operations.Finding{found("cluster_unreachable", t.jail, err.Error())}, nil
	}
	databases, err := pgx.CollectRows(dbRows, pgx.RowTo[string])
	if err != nil {
		return []operations.Finding{found("cluster_unreachable", t.jail, err.Error())}, nil
	}
	exists := map[string]bool{}
	for _, n := range databases {
		exists[n] = true
	}

	var findings []operations.Finding
	for _, n := range databases {
		if _, ok := byName[n]; !ok {
			findings = append(findings, found("unknown_database", n, "no instance on this cluster names it; left alone"))
		}
	}
	for n, r := range byName {
		if r.inFlight {
			continue
		}
		switch {
		case !exists[n] && (r.status == "running" || r.status == "stopped" || r.status == "stopping" || r.status == "starting"):
			tag, err := db.Exec(ctx, `
				UPDATE instances i SET status = 'failed', reason = $3 WHERE id = $1 AND status = $2
					AND NOT EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running'))`,
				r.id, r.status, ReasonMissing)
			if err != nil {
				return findings, fmt.Errorf("record instance %s failed: %w", r.id, err)
			}
			if tag.RowsAffected() == 1 {
				findings = append(findings, found("database_missing", n, fmt.Sprintf("instance %s was %s; now failed, nothing recreated", r.id, r.status)))
			}
		case exists[n] && r.status == "deleted" && r.deleteFailed:
			// Queued once and failed for good: an operator's, not a loop.
			findings = append(findings, found("orphan_delete_failed", n, fmt.Sprintf("instance %s is deleted; its delete failed, see otter admin ops list", r.id)))
		case exists[n] && r.status == "deleted":
			_, err := db.Exec(ctx, `
				INSERT INTO operations (instance_id, account_id, node_id, kind, requested_by)
				SELECT id, account_id, node_id, 'delete', 'system' FROM instances WHERE id = $1`, r.id)
			if err != nil {
				// operations_one_in_flight: something queued it meanwhile.
				return findings, fmt.Errorf("queue the delete of instance %s: %w", r.id, err)
			}
			findings = append(findings, found("orphan_queued", n, fmt.Sprintf("instance %s is deleted; delete queued, which dumps first", r.id)))
		}
	}

	entries, err := PoolEntries(ctx, db, t.clusterID)
	if err != nil {
		return findings, err
	}
	pool := d.Pool
	pool.Jail = t.jail
	wrote, err := EnsurePoolEntries(ctx, d.JailDeck, pool, entries)
	if wrote {
		findings = append(findings, found("pool_repaired", pool.defaults().File, "PgBouncer did not serve what the platform DB says; rewritten"))
	}
	if err != nil {
		findings = append(findings, found("cluster_unreachable", t.jail, "PgBouncer: "+err.Error()))
	}
	return findings, nil
}
