package postgres

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ottermq/otter_cloud/internal/audit"
	"github.com/ottermq/otter_cloud/internal/instances"
	"github.com/ottermq/otter_cloud/internal/operations"
)

// The steps of Q27, as a share of a database's allocation (#99).
const (
	WarnAt        = 0.80 // warning
	ReadOnlyAt    = 1.00 // read_only: default_transaction_read_only on
	BlockAt       = 1.20 // blocked: connections refused
	WritableBelow = 0.95 // read_only back to read-write
)

// NextStorageState is the step a database in state cur goes to at used of
// allocated bytes (Q27). A blocked database stays blocked: only an
// operator reopens it, in read_only with a cleanup window, during which it
// isn't blocked again. Read-only lasts until it is under WritableBelow, so
// a database near its allocation doesn't flip between the two.
func NextStorageState(cur string, used, allocated int64, cleanup bool) string {
	if cur == "blocked" {
		return cur
	}
	r := float64(used) / float64(allocated)
	switch {
	case r >= BlockAt && !cleanup:
		return "blocked"
	case r >= ReadOnlyAt, cur == "read_only" && r >= WritableBelow:
		return "read_only"
	case r >= WarnAt:
		return "warning"
	}
	return "ok"
}

// StorageChecker returns the node's storage checker for operations.Worker:
// db is the platform DB as the node's worker.
func (d *Driver) StorageChecker(db operations.TxDB) operations.StorageChecker {
	return func(ctx context.Context, now time.Time) ([]operations.Storage, error) {
		return d.CheckStorage(ctx, db, now)
	}
}

// CheckStorage measures each database on the node's clusters, records its
// size and its step of Q27, and makes the cluster match the step
// (architecture.md §5 Storage quota, #99):
//
//   - read_only and blocked: ALTER DATABASE … SET
//     default_transaction_read_only = on, and its sessions ended so that
//     new ones get it; ok and warning: RESET, and the same;
//   - blocked, for a database that should run: ALLOW_CONNECTIONS false, its
//     sessions ended, and its PgBouncer entry left out (PoolEntries);
//     reopened, the reverse.
//
// The cluster is made to match every time, not only on a change, so a
// crash between recording a step and enforcing it is put right next time.
// An instance with an operation in flight is measured but left as it is:
// the operation owns the database meanwhile. A stopped database keeps its
// connections refused, whatever its step.
func (d *Driver) CheckStorage(ctx context.Context, db operations.TxDB, now time.Time) ([]operations.Storage, error) {
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
	var all []operations.Storage
	var errs []error
	for _, c := range clusters {
		ss, err := d.checkClusterStorage(ctx, db, c.t, c.name, now)
		all = append(all, ss...)
		if err != nil {
			errs = append(errs, fmt.Errorf("cluster %s: %w", c.name, err))
		}
	}
	if len(errs) > 0 {
		return all, errs[0]
	}
	return all, nil
}

type storageRow struct {
	operations.Storage
	accountID, status, desired string
	busy, cleanup              bool
}

func (d *Driver) checkClusterStorage(ctx context.Context, db operations.TxDB, t target, name string, now time.Time) ([]operations.Storage, error) {
	rows, err := db.Query(ctx, `
		SELECT i.id, i.account_id, i.infra_name, i.storage_gb, i.storage_state, i.status, i.desired_state,
			EXISTS (SELECT FROM operations o WHERE o.instance_id = i.id AND o.status IN ('pending', 'running')),
			coalesce(i.cleanup_until > $2, false)
		FROM instances i
		WHERE i.cluster_id = $1 AND i.status IN ('running', 'starting', 'stopping', 'stopped')
		ORDER BY i.infra_name`, t.clusterID, now)
	if err != nil {
		return nil, fmt.Errorf("list the cluster's databases: %w", err)
	}
	insts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (storageRow, error) {
		x := storageRow{Storage: operations.Storage{Cluster: name}}
		var gb int64
		err := r.Scan(&x.InstanceID, &x.accountID, &x.Database, &gb, &x.From, &x.status, &x.desired, &x.busy, &x.cleanup)
		x.Allocated = gb * d.bytesPerGB()
		return x, err
	})
	if err != nil || len(insts) == 0 {
		return nil, err
	}

	conn, err := d.connect(ctx, &t)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	names := make([]string, len(insts))
	for i, x := range insts {
		names[i] = x.Database
	}
	type dbState struct {
		size          int64
		allowConn, ro bool
	}
	found := map[string]dbState{}
	srows, err := conn.Query(ctx, `
		SELECT d.datname, pg_database_size(d.oid), d.datallowconn,
			coalesce('default_transaction_read_only=on' = ANY (s.setconfig), false)
		FROM pg_database d
		LEFT JOIN pg_db_role_setting s ON s.setdatabase = d.oid AND s.setrole = 0
		WHERE d.datname = ANY ($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("measure databases: %w", err)
	}
	for srows.Next() {
		var n string
		var s dbState
		if err := srows.Scan(&n, &s.size, &s.allowConn, &s.ro); err != nil {
			srows.Close()
			return nil, fmt.Errorf("measure databases: %w", err)
		}
		found[n] = s
	}
	if err := srows.Err(); err != nil {
		return nil, fmt.Errorf("measure databases: %w", err)
	}

	var out []operations.Storage
	poolChanged := false
	for _, x := range insts {
		s, ok := found[x.Database]
		if !ok {
			continue // gone: the reconciler's to report
		}
		x.Used = s.size
		x.To = x.From
		if !x.busy {
			x.To = NextStorageState(x.From, x.Used, x.Allocated, x.cleanup)
		}
		if err := recordStorage(ctx, db, x); err != nil {
			return out, err
		}
		out = append(out, x.Storage)
		if x.busy {
			continue
		}
		wantRO := x.To == "read_only" || x.To == "blocked"
		serving := x.status == "running" && x.desired == "running"
		wantAllow := s.allowConn
		if serving {
			wantAllow = x.To != "blocked"
		}
		if wantRO != s.ro {
			verb := "RESET default_transaction_read_only"
			if wantRO {
				verb = "SET default_transaction_read_only = on"
			}
			if _, err := conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{x.Database}.Sanitize()+" "+verb); err != nil {
				return out, fmt.Errorf("database %s: %s: %w", x.Database, verb, err)
			}
		}
		if wantAllow != s.allowConn {
			if err := allowConnections(ctx, conn, x.Database, wantAllow); err != nil {
				return out, err
			}
		}
		// Sessions keep the defaults they started with: ending them puts
		// the change in force. PgBouncer also remembers the value from its
		// first server connection and sets it on every later one, so a
		// database it serves is reconnected there too (KILL, RESUME).
		if wantRO != s.ro || (wantAllow != s.allowConn && !wantAllow) {
			if err := endSessions(ctx, conn, x.Database); err != nil {
				return out, err
			}
		}
		if wantRO != s.ro && serving && x.To != "blocked" {
			pool := d.Pool
			pool.Jail = t.jail
			if err := Reconnect(ctx, d.JailDeck, pool, x.Database); err != nil {
				return out, err
			}
		}
		if (x.From == "blocked") != (x.To == "blocked") || wantAllow != s.allowConn {
			poolChanged = true
		}
	}
	if poolChanged || slices.ContainsFunc(out, func(s operations.Storage) bool { return s.To == "blocked" }) {
		if err := d.ensurePool(ctx, db, t); err != nil {
			return out, err
		}
	}
	return out, nil
}

// recordStorage writes the size, and the step with its audit event when it
// changed, in one transaction.
func recordStorage(ctx context.Context, db operations.TxDB, x storageRow) error {
	if x.To == x.From {
		if _, err := db.Exec(ctx, `UPDATE instances SET storage_used_bytes = $2, storage_measured_at = now() WHERE id = $1`,
			x.InstanceID, x.Used); err != nil {
			return fmt.Errorf("record the size of %s: %w", x.InstanceID, err)
		}
		return nil
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE instances SET storage_used_bytes = $2, storage_measured_at = now(), storage_state = $3
		WHERE id = $1`, x.InstanceID, x.Used, x.To); err != nil {
		return fmt.Errorf("record the storage step of %s: %w", x.InstanceID, err)
	}
	if err := audit.Record(ctx, tx, audit.Event{Actor: audit.System, AccountID: x.accountID, Action: audit.ActionInstanceStorage,
		TargetType: "instance", TargetID: x.InstanceID, Outcome: audit.OK,
		Details: map[string]any{"from": x.From, "to": x.To, "used_bytes": x.Used, "allocated_bytes": x.Allocated}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (d *Driver) bytesPerGB() int64 {
	if d.BytesPerGB > 0 {
		return d.BytesPerGB
	}
	return instances.GiB
}
