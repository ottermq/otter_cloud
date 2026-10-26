# POC: hard storage quotas with a tablespace per tenant

Status: **done, failed** (2026-10-26). Decided Q22: the **soft quota with a
cluster backstop** (see [Results](#results)). Script:
[`scripts/poc-tablespace-quota.sh`](../scripts/poc-tablespace-quota.sh).

## Question

Shared clusters (Q20) put many tenants' databases in one PostgreSQL cluster.
PostgreSQL has no per-database storage quota. Can a **tablespace per tenant,
on its own ZFS dataset with `refquota`**, give each tenant a hard storage
limit **without a full tablespace hurting the cluster or other tenants**?

If yes, it is the quota mechanism. If not, the fallback is the **soft
quota**: the worker polls `pg_database_size()` and makes an over-quota
database read-only, with a ZFS quota on the whole cluster dataset as the
node's backstop.

## Layout under test

```
<pool>/jails/containers/pgc1           ← cluster jail root (clone of the pg18 template)
<pool>/jails/containers/pgc1/data      ← PGDATA, including pg_wal (shared); quota = cluster backstop
<pool>/jails/containers/pgc1/ts/<db>   ← one dataset per tenant: refquota = flavor storage
                                          mounted at <root>/var/db/postgres/ts/<db>
```

```sql
CREATE TABLESPACE ts_<db> OWNER otter_admin LOCATION '/var/db/postgres/ts/<db>';
CREATE DATABASE <db> OWNER <role> TABLESPACE ts_<db>;
```

`temp_tablespaces` stays empty, so temporary files go to the database's
default tablespace, which is the tenant's own dataset.

## What is known, and what isn't

- **VERIFIED.** ZFS `refquota` limits the space a dataset references,
  excluding snapshots and descendants.
  [zfsprops(7)](https://man.freebsd.org/cgi/man.cgi?query=zfsprops&sektion=7)
- **VERIFIED (general disk-full cases).** When a write that PostgreSQL must
  complete fails for lack of space, the server can PANIC and then get stuck
  in recovery until space is freed. The reported cases are about a whole
  volume or `pg_wal` filling up, not a single tablespace.
  [Railway report](https://station.railway.com/questions/postgre-sql-stuck-in-recovery-mode-after-fe7cf4a3),
  [pgsql thread](https://postgrespro.ru/list/thread-id/1410786)
- **INFERRED.** Extending a relation in a full tablespace fails with an
  `ERROR` for that statement only (`could not extend file "pg_tblspc/…"`).
  This part is expected to be fine.
- **INFERRED, the main risk.** ZFS is copy-on-write: **overwriting an
  existing block also needs free space**. Every checkpoint writes the dirty
  buffers of every tenant, so a tenant at its quota could make the
  **checkpointer** fail. Failed checkpoints stop WAL from being recycled, and
  `pg_wal` is shared, so all tenants would feel it.
- **INFERRED, the worst case.** After a crash, **WAL replay** has to write
  that tenant's pages. If it can't, the startup process may fail and the
  **whole cluster stays down** until someone raises the tenant's quota.
- **UNKNOWN.** How much ZFS overshoots `refquota` (quota enforcement is per
  transaction group), and whether that slack is enough for PostgreSQL to
  finish a checkpoint.

## Tests

On a scratch cluster jail built as in `runbook-m0.md` Part E, with three
tenant databases (`t1`, `t2`, `t3`), each on its own `refquota=200M`
tablespace dataset, and a write load on `t2` and `t3` running throughout
(e.g. `pgbench`).

| # | Test | Pass condition |
|---|---|---|
| Q1 | Fill `t1` with inserts until it fails | `t1` gets `ERROR` on writes; `t2`/`t3` unaffected; no PANIC |
| Q2 | With `t1` full, run `UPDATE`s on existing `t1` rows, then `CHECKPOINT` | Either the checkpoint succeeds, or it fails without stopping the cluster. Record which, and the log lines |
| Q3 | Keep `t1` full for 30 minutes under `t2`/`t3` load | `pg_wal` size stays bounded (checkpoints complete); no restart |
| Q4 | `kill -9` the postmaster while `t1` is full and has dirty pages | Cluster restarts and finishes recovery **without** raising `t1`'s quota |
| Q5 | Free space in `t1` (`DELETE` + `VACUUM`, or `DROP TABLE`) | Writes to `t1` work again without operator action |
| Q6 | `VACUUM FULL` / `CREATE INDEX` on a large `t1` table near its quota | Fails cleanly for `t1` only; temporary files counted in `t1`'s dataset |
| Q7 | Tenant tries to create objects elsewhere: `CREATE TABLE … TABLESPACE pg_default`, `SET temp_tablespaces = pg_default` | Denied (no `CREATE` on other tablespaces) |
| Q8 | `pg_dump` of `t1` and restore into a fresh database on another tablespace (`--no-tablespaces`) | Restores cleanly |
| Q9 | Snapshot `ts/t1`, rename it to `<pool>/jails/retained/` and clear its mountpoint, `DROP DATABASE t1`, `DROP TABLESPACE` | Works; the retained dataset survives (the deletion path) |
| Q10 | Measure ZFS overshoot: bytes written past `refquota` before `EDQUOT` | Recorded; informs the margin below |

Run Q2–Q4 three times each; a single pass is not enough for a crash path.

## Decision rule

- **All of Q1–Q9 pass:** adopt tablespace-per-tenant. Combine it with the
  soft check anyway: the worker makes a database read-only at 100 % of its
  quota, and the dataset's `refquota` is set a margin above it (from Q10),
  so the hard limit is only hit when the soft check lags.
- **Q2, Q3 or Q4 fails** (a full tenant can stall checkpoints or block
  recovery): adopt the soft quota with the cluster backstop. Tenant data may
  still go on its own dataset, without `refquota`, if per-tenant snapshots
  and the deletion path (Q9) are worth it.
- **Q7 fails:** fix the privileges first; the result of the others still
  stands.

Results go into this document and into `feasibility.md` §8, and Q22 is
closed.

## Results

Run 2026-10-26 on the reference node (FreeBSD 15.1-RELEASE-p5, PostgreSQL
18.6 from packages, mirrored HDD pool, `compression=on`). Tenant datasets
had `recordsize=16K` and **inherited mountpoints with flat names**
(`ts-t1`), per the roadmap's JailDeck conventions. t1 had
`refquota=200M`; t2 and t3 had 2G and ran `pgbench` (scale 5, 2 clients
each). A backup tool snapshotted the datasets every 15 minutes throughout,
as it will in production.

| # | Result | What happened |
|---|---|---|
| Q1 | **FAIL** | t1's own inserts failed cleanly (`could not extend file "pg_tblspc/…": Disc quota exceeded`). But **t2's and t3's pgbench clients aborted** with `could not write blocks … in file "pg_tblspc/<t1's tablespace>/…": Disc quota exceeded`, and restarting pgbench failed the same way |
| Q2 | **FAIL** | `CHECKPOINT` failed (`checkpoint request failed`). Nearly every query that needed a buffer, in **any** database, failed while trying to evict one of t1's dirty pages; even `select pg_reload_conf()` in `postgres` failed. Only queries touching no buffers (`select 1`) still worked |
| Q3 | not run | Q2 already shows checkpoints cannot complete while t1 is full |
| Q4 | **FAIL** | After `kill -9`, crash recovery did not error out: it **crawled** (about 200 KB of WAL per 10 s; startup process waiting on ZFS transaction-group sync). **Every tenant was refused for 50 minutes.** Raising t1's quota to 400M finished the remaining redo in 11 s, after which redo filled that too (419 MB, twice the original quota) and the end-of-recovery checkpoint crawled until the quota was removed |
| Q5, Q6 | not run | Moot once Q1–Q4 failed. Where temporary files go is documented: the database's default tablespace when `temp_tablespaces` is empty |
| Q7 | pass | `CREATE TABLE … TABLESPACE pg_default`, another tenant's tablespace, and `SET temp_tablespaces = pg_default` were all denied |
| Q8 | pass | `pg_dump -Fc`, then `pg_restore --no-tablespaces` into a database on another tablespace: all 340 000 rows back |
| Q9 | pass | Snapshot, `DROP DATABASE`, `DROP TABLESPACE`, `zfs rename -u` into `retained/`, unmount, mount: the dataset mounted under `retained/` through its inherited mountpoint, snapshots intact |
| Q10 | measured in Q1 | ZFS stopped t1 4 096 bytes past its `refquota` |

### Why it fails

PostgreSQL's shared buffers are shared by every database in the cluster.
On ZFS, **overwriting** a block needs new space (copy-on-write), so once
t1's dataset is at its quota, t1's dirty pages can no longer be written
back at all, not only extended. Whoever needs that buffer next fails: the
checkpointer, the background writer, and any backend of any tenant that
picks it for eviction. Near a quota ZFS also forces a transaction-group
sync on nearly every write, which is why recovery crawled instead of
failing. And crash recovery replays every insert in the WAL, including
those of transactions that failed, so it can need **more** than the
tenant's quota to finish.

### Decision

Per the decision rule: **soft quota with a cluster backstop** (Q22).

- No `refquota` on anything PostgreSQL writes to, per tenant. The general
  rule the POC teaches: **a PostgreSQL dataset must never reach a ZFS
  quota.** The cluster's backstop has the same failure mode for the whole
  cluster, so it sits well above the sum of what the cluster has sold, and
  usage alerts fire long before it (80 %). The same applies to `refquota`
  on a dedicated instance's `data` (M13): it protects the node, but
  reaching it is an outage for that customer, so it needs the same soft
  check in front of it.
- Tenant databases stay in the cluster's default tablespace; there are no
  per-tenant datasets. Q7–Q9 show that per-tenant datasets *without* a
  quota would work, and would give per-tenant snapshots and a ZFS deletion
  path, but they add a JailDeck call per create, a tablespace per
  database, and the dataset-destroy gap, for a final copy that a
  `pg_dump` (Q7) already provides. **Deferred pending research** (#61):
  until it is decided there are none, and adopting them later needs no
  data migration (`ALTER DATABASE … SET TABLESPACE`).
- The soft quota needs care in M4: `default_transaction_read_only` is a
  setting a tenant can turn off in its own session, so making a database
  read-only is advisory. The enforcement that can't be bypassed is at the
  connection level (PgBouncer `DISABLE`, `ALLOW_CONNECTIONS false`), and it
  also blocks the tenant from deleting data to get back under quota. M4
  designs the steps between the two.
