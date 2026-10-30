# Changelog

Every released version is recorded here, with a
[release](https://github.com/ottermq/otter_cloud/releases) for each. Releases
follow milestones: the minor version is the milestone it completes
([roadmap](docs/roadmap.md), "How the work is organised"). Dates are the tag
date; numbers link to [the tracker](https://github.com/ottermq/otter_cloud/issues).

Changes to the HTTP API are recorded under an **API** heading, and to the
platform database under **Migrations**, so an upgrade knows what to expect.
Before 1.0 breaking changes are allowed, but never silent.

## Unreleased

### Added

- **Stop, start and restart a shared database** (#95). Stopped, its
  PgBouncer entry is gone, which ends its clients and refuses new ones, and
  PostgreSQL refuses connections to it (`ALLOW_CONNECTIONS false`) with its
  sessions ended; start reverses both. Its role, data and quota stay. A
  restart ends its connections at PgBouncer (`KILL`, `RESUME`) and in
  PostgreSQL, and clients connect again. A failed stop, start or restart
  leaves the database `stopping` or `starting` with its reason, and the
  customer can ask again. Deleting a stopped database opens it for the
  final dump first.
- **Operator commands for the queue** (#102): `otter admin instances list`
  (owner, status, node, cluster and infra name; `-all` for deleted ones),
  `otter admin ops list` (filters by status and instance; the only place an
  operation's `last_error` is shown), `otter admin ops retry` and
  `otter admin ops cancel`, each audited with the operator's `-reason`.
  - **Retry** takes only a failed operation. It's due at once, with its
    attempts counted from zero, and resumes from its last step; a delete's
    instance is `deleting` again. A failed create is refused: failing
    cleared its password verifier, so the customer deletes it and creates
    another.
  - **Cancel** takes only a pending operation that has never run. A
    canceled create leaves its instance deleted, which releases its quota;
    a canceled delete withdraws the customer's intent. One waiting out a
    retry is refused, since the node may hold part of it.

### API

- `POST /api/instances/{id}/stop`, `/start` and `/restart` (#95): no
  body; `202` with the instance and its queued operation; `409` while
  another operation is in flight, or when the status doesn't allow it
  (restart a stopped database, start a running one); audited as
  `instance.stop`, `instance.start` and `instance.restart`.

### Changed

- A failed operation that a later one on the same instance superseded
  can't be retried: `otter admin ops retry` says so (#95).

### Migrations

- `0015_operator_operations`: `otter_admin` may read instances and
  operations, put an operation back in the queue or cancel it, and set an
  instance's status, reason, desired state and `deleted_at`. Never an
  operation's kind, target or `last_error`, or an instance's allocation.

## v0.3.0 (2026-10-30): M3 Database Provisioning

The worker provisions real databases. On a node, `otter worker` claims the
node's operations from the platform database and, through JailDeck,
creates a customer's role and database in a shared PostgreSQL cluster
behind PgBouncer, checks its privileges, and deletes it again after a
final dump. The minimum profile runs the API and the platform database in
their own jails on the same node. On the reference node, a database was
created through the API, used through PgBouncer and deleted end to end.
Customers can't reach their databases from outside yet: the edge comes
with M5, and `connection` stays `null` until then.

The release attaches a FreeBSD amd64 archive: the `otter` binary, built
and attested in CI, with `deploy/` and the isolation check.

### Added

- **Operations queue** for the node worker: claims with `SKIP LOCKED` and
  a renewed lease, checkpoints, retries with backoff up to five attempts,
  and a `LISTEN/NOTIFY` wake-up. Each node's worker logs in as its own
  role, `otter_node_<name>`, which row-level security keeps to its node's
  rows; `otter admin node add` prints the role to create (#47).
- **JailDeck client** for the worker, over the node's unix socket with a
  token: jails, exec and the audit log. Every call a handler makes carries
  the operation id as `X-Request-Id`. A fake JailDeck for unit tests, and
  contract tests to run against the real one on a node (#49). New worker
  settings: `OTTER_JAILDECK_SOCKET`, `OTTER_JAILDECK_TOKEN_FILE`.
- **Cluster admin connection:** the worker reaches each cluster as
  `otter_admin` with its node's client certificate, over TLS only, and
  accepts only the cluster's own certificate, pinned through JailDeck; a
  password is refused by `pg_hba` (#50). New worker settings:
  `OTTER_CLUSTER_ADMIN_CERT_FILE`, `OTTER_CLUSTER_ADMIN_KEY_FILE`.
- **Tenant role and database:** `postgres.EnsureRole` and `EnsureDatabase`
  make a customer's `otc_*` role and database, or check the ones there and
  refuse, untouched, a role with anything a tenant must not have; the
  password only as a SCRAM verifier; the limits of Q12 (#51).
- **PgBouncer entries:** `postgres.EnsurePoolEntries` renders the
  cluster's `[databases]` file from the platform DB, checks it, writes it
  through JailDeck, reloads and verifies `SHOW DATABASES`, and puts the
  previous file back if PgBouncer doesn't take it (#52).
- **Generated passwords:** a create's `202` carries a 32-character password,
  the only time it is shown; the platform keeps its SCRAM-SHA-256 verifier
  (`internal/scram`), in the create operation's payload for the worker, and
  a replay says `"password_shown": false` (#54).
- **`otter worker` provisions databases:** the `create` operation's
  handler (`postgres.Driver`) ensures the role, the database and the
  cluster's PgBouncer entries, then records the instance `running`. A create
  that fails for good leaves the instance `failed` with `create_failed`
  (#134).
- **Privilege check after every create:** `postgres.CheckTenant` confirms
  the new role has no forbidden attribute or membership, may connect to no
  database but its own, and that its own grants nothing to PUBLIC; a failure
  fails the create (#55). The worker serves `/metrics` on
  `OTTER_METRICS_ADDR` (default `127.0.0.1:9102`), with
  `otter_operation_runs_total{kind,outcome}` for the "operation failed" alert.
- **Delete a database:** the `delete` handler stops routing to it, closes
  its role, takes the final `pg_dump` into `OTTER_DUMP_DIR` (Q26) and records
  it, and only then drops the database and the role; the instance is
  `deleted` and its quota released. A failed dump drops nothing; a role or
  database the worker did not make is left for an operator (#56). Dumps
  are recorded to expire after 3 days; pruning them comes with #100.
- **Crash test:** a worker killed after each step of a create or a delete,
  and in the middle of a final dump, leaves its operation to its lease;
  the restarted worker completes it with one role, one database, one
  PgBouncer entry and one recorded dump, and nothing left over (#57).
- **Worker service:** `deploy/node/rc.d/otter_worker` runs `otter worker`
  as its own user under daemon(8), restarted if it exits, logging to
  syslog; `otter worker -check` validates the settings and secrets without
  connecting, and rc.d runs it first, so a secret readable by others stops
  the start, and so does a missing `pg_dump`; rc.d adds `/usr/local/bin`
  to the `PATH` service(8) gives it. daemon(8) runs with `-f`, so a start
  over SSH or JailDeck exec returns at once. Install notes in
  `deploy/node/README.md` (#48).
- **JailDeck v0.18.1:** `jaildeck.AuditEntry` carries an exec's
  `exit_code`, `timed_out` and `duration_ms`, which the fake records and the
  contract checks (#140).
- **Node PF policy:** `deploy/node/pf.conf`, the static policy a node
  loads: tenants open no connections and are reached only from the node
  host on `:5432` and `:6432`, JailDeck's anchors are included once for
  its networks' NAT and rules, and the rest of the host is unchanged. The
  runbook loads it with a dead-man switch that cannot hang the session
  (#17). It needs `net.link.bridge.pfil_member=1`: FreeBSD 15's default,
  `0`, keeps traffic between jails on one bridge away from PF (#58).
- **Node network:** the shared clusters' bridge is a JailDeck network,
  `clusters`, on the first `/24` of the node's `/22`
  (`JAILDECK_NETWORK_RANGE`), and the policy's tenant table is that `/24`.
  The runbook creates it through JailDeck's API and sets the shared
  `exec.timeout` (#16).
- **JailDeck on the node:** the install notes turn on JailDeck's unix
  socket for a `jaildeck` group the worker's user is in, and write the
  worker's token straight to its `0400` file (#18).
- **Cluster bootstrap:** `deploy/cluster/otter-pg-bootstrap` sets up a
  shared cluster in its jail as the worker expects it: `otter_admin` by the
  node's client certificate only, tenants only through PgBouncer,
  PgBouncer's SCRAM lookup, a self-signed certificate to pin, and the
  worker's databases file included. Its two passwords are made in the jail.
  The test cluster is now built by the same script. The runbook builds a
  node's first cluster with it through JailDeck: the `pg18` template, the
  cluster jail on the `clusters` network, the node's client certificate
  from the admin CA, and checks F16–F22. A cluster jail's limits are
  `memoryuse`, `pcpu` and `maxproc` only: rctl's `vmemoryuse` and
  `openfiles` count what PostgreSQL's processes share, and stop it forking
  (#20).
- **Runbook Part E through JailDeck only:** the cluster jail is ensured
  with `PUT /api/jails/{name}`, and the bootstrap and the admin CA's
  certificate go in through exec as arguments, so no step writes into a
  jail from the host. The first cluster is `otc-pgc1`, as architecture §6.1
  names clusters. JailDeck v0.20.0 keeps a child's own mountpoint on retain
  and restore (jaildeck#121), so a cluster's `data` stays mounted at
  `/var/db/postgres` (#19).
- **Isolation check:** `scripts/isolation-check.sh` probes a node's PF
  promises by TCP connect: the cluster jail reaches nothing (the internet,
  the host, the LAN, other jails, the VM networks), the node host reaches
  the tenant ports, and no non-Otter jail does (R16). A control probe
  proves the probes can succeed, and PF's tenant block rule must count the
  drops. The node's own addresses are parameters (#24).
- **Minimum profile:** the control plane in two jails on the node, made
  through JailDeck on its network `platform` (`deploy/control-plane`, #58).
  `otter-db` is set up by `deploy/platform-db/otter-platform-db-bootstrap`:
  TLS 1.3 to a pinned self-signed certificate, SCRAM, and each role only
  from where its process runs. `otter-api` runs under `rc.d/otter_api` and
  holds no JailDeck credential. `otter-db-password` sets each role's
  password from the host, through `jexec` stdin, straight into its file.
  The node's `pf.conf` lets the host and the api reach the DB, and the host
  reach the api, and nothing else; `scripts/isolation-check.sh -c` probes
  it. `otter api -check` validates the settings and secrets without
  connecting, and rc.d runs it first. `bootstrap.sql` can be run again.
- **JailDeck v0.21.0 and v0.22.0:** a jail's limits change in place
  (jaildeck#119), and retained and child datasets can be destroyed through
  the API (jaildeck#120); both gaps are closed in the roadmap.

### API

- `POST /api/instances`: the `202` carries the database's `password`, the
  only time it is shown, and a replay with the same `Idempotency-Key` says
  `"password_shown": false` (#54).
- An instance's `status` now moves on: `running` once the worker has
  created it, `deleted` once it has deleted it, or `failed` (a failed
  create carries `"reason": "create_failed"`) (#134, #56).

### Migrations

- `0012_worker`: row-level security on nodes, clusters, instances and
  operations, the worker's grants and the wake-up trigger. Before a node's
  worker connects, a superuser creates its role (`deploy/platform-db/bootstrap.sql`).
- `0013_instance_outcome`: the worker may update an instance's observed
  state (`status`, `health`, `reason`) on its node, and nothing else.
- `0014_backup_records`: the operator-only `backup_records` table; the
  worker inserts its node's rows and may set an instance's `deleted_at`.

## v0.2.0 (2026-10-28): M1 Skeleton and Identity, M2 Plans and Placement

The first release. Everything that decides whether a customer may have a
database and where it goes is recorded as rows, behind an invite-only API.
Nothing provisions a database yet: operations are queued and stay pending
until the worker of M3 runs them. There is nothing to deploy, so the release
has no binaries; they start with M3.

### Added

- **The `otter` binary** with `api`, `worker` (not yet implemented), `admin`,
  `migrate` and `version`; configuration from the environment, secrets only
  from owner-only files (#27, #28). CI runs lint, vulnerability checks and
  race tests against PostgreSQL 18 (#29).
- **Migrations:** a built-in runner with checksums and an advisory lock, and
  one database role per process: `otter_migrator`, `otter_api`,
  `otter_worker`, `otter_admin` (#30, #38).
- **Identity:** accounts and users with argon2id passwords and a password
  policy (#31); invite-only sign-up (#32); server-side sessions in a
  `__Host-` cookie (#33); cross-origin protection and login rate limits
  (#34); an append-only audit log (#35).
- **Operations:** `/healthz`, Prometheus metrics on an internal listener,
  request ids on every log line (#36).
- **Operator commands** (`otter admin`): invites, users (disable, enable),
  plans, subscriptions, nodes and clusters (#32, #37, #42, #43).
- **Catalog and billing:** flavors and plans from reviewed files synced by
  `otter migrate`, with the Founder Beta and `db-small`/`db-medium` of Q12
  (#41, #42); subscriptions and `billing.AccountEntitlement`; a paid plan
  can't become active without a payment provider (Q11).
- **Creating a database record:** entitlement, quota and placement in one
  transaction, with the create operation queued but not run (#44, #45).
- **Operations read API** with customer-safe reason codes and messages;
  internal errors stay operator-only (#77).
- **Instances API:** create (with an `Idempotency-Key`), list, show and
  delete (confirmed by name) the account's databases, and the subscription
  with what it allows and what is allocated (#46). Deleting queues an
  operation; nothing runs it yet.

### API

- `POST /api/signup`, `POST /api/login`, `POST /api/logout`, `GET /api/me`.
- `GET /api/catalog`.
- `GET /api/operations`, `GET /api/operations/{id}`,
  `GET /api/instances/{id}/operations` (#77).
- `GET /api/subscription`; `GET /api/instances`, `POST /api/instances`,
  `GET /api/instances/{id}`, `DELETE /api/instances/{id}` (#46).

### Migrations

- `0001_privileges` to `0011_instance_delete`. Before `0006`, a superuser runs
  `CREATE ROLE otter_admin LOGIN;` (`deploy/platform-db/bootstrap.sql`).
