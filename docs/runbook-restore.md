# Restore runbook

How to get data back from each backup layer in `architecture.md` §10
(#120): one customer database, the platform DB, and a whole cluster. Each
procedure has been drilled on a real node, timed, and its write-up is on
#120. Rebuilding a lost node from these docs is the drill still to come.

Conventions are those of [runbook-secrets.md](runbook-secrets.md):
`run <jail> <cmd>` is `doas jexec <jail> <cmd>`; a cluster runs in
`otc-<cluster>`, the platform DB in `otter-db`.

- **The age identity never goes to the node.** Copies are opened on the
  operator's machine, where the private key is kept (`runbook-secrets.md`,
  "Backup age key pair"). Plaintext goes from there to where it's needed
  over ssh, never through a file on the node.
- **Restore next to, never over.** Every procedure here restores into
  something new (a scratch database, a throwaway server, a scratch jail),
  and checks it, before anything live is touched. Replacing a customer's
  database in place is not a drilled procedure; see the end of
  [One customer database](#one-customer-database).
- **Clean up the same day.** A restored copy holds customer data and, for
  a cluster, the cluster's TLS key. Remove it when the check is done.

## Summary

| What | From | Data loss, worst case | Measured |
|---|---|---|---|
| [One customer database](#one-customer-database) | Its nightly dump, off-site copy | A day | Seconds for a 218 KB dump |
| [The platform DB](#the-platform-db) | Its nightly dump, off-site copy | A day; then `otter migrate` | 3 s for an 85 KB dump |
| [A whole cluster](#a-whole-cluster) | The replication tool's newest snapshot of the cluster jail | The snapshot interval (15 min with zrepl) | 13 s, clone to accepting |

## Finding a copy

Every dump and copy is a row in `backup_records`. A database's copies,
newest first (`<infra>` is its `otc_…` name, shown in the admin view):

```sh
run otter-db su -m postgres -c "psql -XAt -d otter -c \"
  SELECT b.kind, b.target, b.ref, b.created_at, b.expires_at
  FROM backup_records b JOIN instances i ON i.id = b.instance_id
  WHERE i.infra_name = '<infra>' AND b.status = 'ok' AND b.purged_at IS NULL
  ORDER BY b.created_at DESC\""
```

A row with `target = 'local'` is the dump on the node, in
`OTTER_DUMP_DIR`; any other target is its off-site copy, and `ref` is the
key on that target (`dumps/<file>.age`). `otter backup list` shows what
the target itself holds. The platform DB's copies are under `platform/`
and aren't in `backup_records`.

To fetch and open a copy, on the operator's machine, with the target's
settings in the environment (as on the node or the control plane):

```sh
otter backup get -key <key> -identity <age identity file> -o <file>
```

On an `fs` target that another machine replicates, fetching the `.age`
file from that machine's replica and running `age -d -i <identity>` does
the same.

## One customer database

Drilled 2026-11-17: restored into a scratch database in the same cluster,
with the database and its neighbour unaffected.

1. Find the copy (above). Prefer the local dump while it exists: same
   contents, no fetch.
2. Create a scratch database in the cluster:
   `run otc-<cluster> su -m postgres -c "createdb restore_<infra>"`.
3. Restore. On the operator's machine, open the copy to a file there,
   then stream it to the node:

   ```sh
   otter backup get -key <key> -identity <identity> -o <file>
   ssh <node> "doas jexec otc-<cluster> su -m postgres -c \
     'pg_restore --no-owner --no-acl --exit-on-error -d restore_<infra>'" < <file>
   ```

   Remove `<file>` when done. The plaintext never lands on the node's disk.
4. Check it: counts of the tables the customer cares about, or a checksum
   query as in the drill.
5. Give the customer what they asked for (usually a `pg_dump` of a table,
   or of the whole scratch database, over a channel you agree with them),
   then `dropdb restore_<infra>`.

`--no-owner --no-acl` leaves everything owned by `postgres`, which is fine
for a scratch copy. A restore the customer will use as their own database
would run as `otter_admin` with `--role <infra>`, so the objects belong to
the tenant role.

**Not drilled: replacing the customer's database in place.** It needs the
database stopped, its current contents kept until the customer confirms,
and a decision on what the customer is told about the data written since
the dump. Until that is designed, restores are on request, into a scratch
database, and the customer moves what they need.

## The platform DB

Drilled 2026-11-16: restored into a throwaway PostgreSQL 18 on the
operator's machine and compared with the live DB.

1. Fetch and open the newest copy, `platform/otter-<stamp>.dump.age`.
2. In the target server, create the platform's roles and an empty `otter`
   owned by `otter_migrator`: `deploy/platform-db/bootstrap.sql` does
   both. `pg_restore` doesn't create roles.
3. `pg_restore --exit-on-error -d otter <file>`.
4. Give each login role its password again (`runbook-secrets.md`,
   "Platform DB role passwords").
5. Run `otter migrate` before the api or any worker starts. A dump is up
   to a day old and may predate the last migration; `migrate` is
   idempotent.
6. Check: the schema version, and counts of users, instances,
   audit events and backup records against what you expect as of the
   dump's time.
7. Remove the plaintext dump.

While the platform DB is down, customer databases keep running
(`architecture.md` §10); the portal and new operations wait. What a
restore loses is the platform's own changes since the dump: sign-ups,
operations, audit events. Compare what the restored DB lists with what
each node actually has before the workers start again.

A whole control-plane loss also needs the `otter-db` and `otter-api` jails
rebuilt from `deploy/platform-db` and `deploy/control-plane`; the
node-rebuild drill covers that.

## A whole cluster

Drilled 2026-11-17: the newest replicated snapshot of a cluster jail,
cloned into a scratch jail with no network; every database identical to
the live cluster as of the snapshot.

This restores all of a cluster's databases to one point in time. **It is
never used to restore one tenant** (`architecture.md` §10): roll back a
cluster and every other tenant loses their recent writes too. To recover
one database from a snapshot, clone it as below and `pg_dump` that one
database from the scratch jail.

The replication tool snapshots the jail's root and its `data` dataset
separately, seconds apart and under different names. That is fine: all of
PGDATA, `pg_wal` included, is in `data`, so take the newest snapshot of
each.

```sh
R=<replica pool>/<path to>/otc-<cluster>   # the read-only replica
M=/mnt/restore-<cluster>
S=$(zfs list -H -t snapshot -o name -s creation $R | tail -1 | cut -d@ -f2)
D=$(zfs list -H -t snapshot -o name -s creation $R/data | tail -1 | cut -d@ -f2)

# 1. Writable clones, outside the replication tool's datasets
doas zfs clone -o readonly=off -o mountpoint=$M $R@$S <pool>/restore-<cluster>
doas zfs clone -o readonly=off -o mountpoint=$M/var/db/postgres $R/data@$D <pool>/restore-<cluster>/data

# 2. A jail with no network: vnet, no interfaces, only lo0
doas jail -c name=restore-<cluster> path=$M host.hostname=restore-<cluster> vnet persist \
  mount.devfs devfs_ruleset=4 securelevel=3 enforce_statfs=2 children.max=0 \
  sysvshm=new sysvsem=new sysvmsg=new
doas rctl -a jail:restore-<cluster>:memoryuse:deny=<the cluster's memory>
doas jexec restore-<cluster> ifconfig lo0 inet 127.0.0.1/8 up

# 3. PostgreSQL on loopback, logging to a file (the jail has no syslogd)
doas jexec restore-<cluster> su -m postgres -c "pg_ctl -D /var/db/postgres/data18 \
  -o '-c listen_addresses=127.0.0.1 -c log_destination=stderr' -l /tmp/restore.log -w -t 300 start"
```

PostgreSQL does crash recovery, as after a power loss, and `pg_ctl`
warns about the stale `postmaster.pid`: expected. PgBouncer and rc don't
start.

4. Check. `postgres` connects only over the local socket (pg_hba refuses
   TCP without TLS), so no `-h`:

   ```sh
   doas jexec restore-<cluster> su -m postgres -c "psql -XAt -c 'SELECT pg_is_in_recovery()'"   # f
   doas jexec restore-<cluster> su -m postgres -c "psql -XAt -l"
   ```

   Then counts or checksums in the databases concerned, against the live
   cluster where it still exists.
5. Use it: `pg_dump` the databases you need from here. Turning a restored
   copy into the live cluster on a new node is part of the node-rebuild
   drill, still to be written.
6. Clean up, in this order:

   ```sh
   doas jexec restore-<cluster> su -m postgres -c "pg_ctl -D /var/db/postgres/data18 -m fast -w stop"
   doas jail -r restore-<cluster>
   doas rctl -r jail:restore-<cluster>
   doas umount $M/dev              # jail -r with no config file leaves devfs mounted
   doas zfs destroy -r <pool>/restore-<cluster>
   doas rmdir $M
   zfs get -H -o value clones $R@$S $R/data@$D   # "-": the snapshots are free again
   ```

   **Keep it short.** While a clone exists, its origin snapshot can't be
   pruned and a receive that has to roll back past it fails. Check the
   replication tool's log for errors once the clones are gone. Never
   change `readonly` on the replica itself; the clones get their own.

The clone carries the cluster's `server.key` and every tenant's data:
destroy it the same day.
