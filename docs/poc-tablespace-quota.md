# POC: hard storage quotas with a tablespace per tenant

Status: **planned**, 2026-10-21. Decides between the two quota mechanisms in
`open-questions.md` Q22. Nothing has been run yet.

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
<pool>/otter/pgc1                      ← cluster jail root (clone of the pg18 template)
<pool>/otter/pgc1/data                 ← PGDATA, including pg_wal (shared); quota = cluster backstop
<pool>/otter/pgc1/ts/<db>              ← one dataset per tenant: refquota = flavor storage
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
| Q9 | Snapshot `ts/t1`, rename it out of the cluster's datasets, `DROP DATABASE t1`, `DROP TABLESPACE` | Works; the retained dataset survives (the deletion path) |
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
