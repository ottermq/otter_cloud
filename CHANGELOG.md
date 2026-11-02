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

- **Edge VPS configuration** in `deploy/vps/` (#104):
  - nftables lets in 443, 5432, 51820/udp, and SSH from an allowlist. It
    forwards nothing, lets in nothing from the tunnel, and its stop and
    reload leave other tools' tables alone.
  - The WireGuard hub has one peer per node.
  - Traefik runs as its own user under a hardened systemd unit, with the
    DNS token handed over as a systemd credential. It serves the portal's
    route, the databases' wildcard as the default certificate, TLS 1.2 or
    later with strict SNI, and no dashboard or API.
  - Install and check steps are in the README. The database routes come
    with #103.
- **Tunnel from a node to the edge** (#22), with base-system WireGuard
  only: `deploy/node/wireguard/` (the config and an rc `start_if.wg0`
  hook) and a README section. `pf.conf` lets the edge's tunnel address
  reach PgBouncer `:6432` and the api `:8080`, and blocks everything else
  arriving on `wg0`. Checked on the reference node: the portal answers
  through the edge end to end.

- **Edge routes** (#103): `otter api` serves Traefik's dynamic
  configuration on its own listener (`OTTER_EDGE_ADDR`, `GET /traefik`),
  to the trusted proxies alone. It has a TCP router per running database,
  by SNI of its hostname, to its cluster's PgBouncer, re-encrypted. A
  database blocked over its storage gets no route. With `OTTER_DB_DOMAIN`
  set, a new database gets its hostname at creation:
  `<infra name>.<domain>`, port 5432, with `-` for the infra name's `_`.
  The node's `pf.conf` opens the listener to the edge, and the edge's
  Traefik sample polls it.
- **Client matrix through the edge** (#23): psql 16, 17 and 18 (both
  negotiation modes) and pgx connect with `sslmode=verify-full`, recorded
  in `docs/feasibility.md` §8. The database routes name new TLS options,
  `postgres` in the edge's `dynamic/edge.yml`, which offer ALPN
  `postgresql`. Without them libpq 17+ and pgx's direct TLS fail the
  handshake ("no application protocol"). The edge needs these options
  before it gets routes from this version.

- **External check** (#107): `scripts/external-check.sh` checks the platform
  from outside, as a customer would:
  - the edge keeps internal ports closed, serves HTTPS only and doesn't
    route `/metrics` or `/traefik`;
  - neither certificate is near its expiry (a warning under 21 days, so a
    failed renewal is caught, #105);
  - an ephemeral database accepts `psql` with `verify-full` within 60 s of
    running, by SSLRequest and direct TLS;
  - its route is gone soon after the delete.

  It uses a customer account kept for it.

### API

- `GET /api/instances/{id}`: `connection` is filled in for a running
  database created with `OTTER_DB_DOMAIN` set. It was always `null`
  before.

### Changed

- **The worker's JailDeck token is limited** (#169, JailDeck's JD-055): it
  reads everything and runs commands only in jails labelled
  `otter:role=cluster`. A leaked token no longer reaches the control
  plane's jails or the rest of the host. Needs JailDeck v0.28.0 or later;
  `deploy/node/README.md` has the new `token add` line, and a warning:
  revoke the token before rolling JailDeck back past v0.28.0, since older
  versions ignore the grants. The contract test now checks that an exec
  outside the grant gets a `403` (`OTTER_JAILDECK_CONTRACT_DENIED_JAIL`).
- The DNS token needs `Zone:Zone:Read` as well as `Zone:DNS:Edit` (the
  ACME client looks the zone up first). If it is limited by address, the
  VPS's IPv6 must be listed too (Q14).

## v0.4.0 (2026-11-02): M4 Lifecycle and Expiry

A database has a lifecycle after its creation. A customer can stop, start
and restart it and reset its password. The worker checks its health, holds
it to its storage allocation in the steps of Q27, and reconciles the node
against the platform database. The api stops a database at its
`expires_at` and deletes it 7 days later, after a final dump. Users manage
their own passwords and sessions, and an operator can issue a reset link
until the platform sends email. Operators inspect the queue, and retry or
cancel its operations, from `otter admin`. CI tests each of these against
a real PostgreSQL cluster behind PgBouncer. The release runs on the
reference node; customers still can't reach a database from outside,
which comes with M5.

The release attaches a FreeBSD amd64 archive, built and attested in CI, as
v0.3.0 did.

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
- **Per-node reconciler** (#96), in the worker at startup and every 5
  minutes, between operations. Per cluster: a database gone behind the
  platform's back fails its instance (`infrastructure_missing`) and is
  never recreated; a database whose instance is deleted gets a delete
  queued, which dumps it first; an unknown `otc_*` database is only
  reported; PgBouncer is put back to what the rows say. Instances with an
  operation in flight are skipped, and an unreachable cluster is reported
  without judging its databases. Findings are error log lines and
  `otter_reconcile_findings_total{kind}`.
- **Reset a shared database's password** (#101). The API generates a new
  password and shows it once, as at create; only its SCRAM verifier goes
  to the worker, which sets it on the role. From then on the old password
  is refused at PgBouncer; connections already open stay open, and a
  restart ends them. A stopped database takes it too. A failed reset
  leaves the old password working, and isn't retried by the operator: its
  verifier is gone, so the customer asks again.
- **Health checks** (#97), in the worker at startup and every minute. A
  running database is `ok` when PostgreSQL takes a connection to it and
  PgBouncer serves it, and `degraded` when either doesn't: an error log
  line, `health` on the instance, and `otter_instance_up{cluster,database}`
  at 0. A stopped database's health is `unknown`. Health never changes the
  status.
- **Account security** (#78): a user changes their password, giving the
  current one, and sees and ends their sessions. A wrong current password
  counts against the login limits. A change ends every session of the
  user's, and the request gets a new one.
- **Storage quota enforcement** (#99, Q27): every minute the worker
  measures each database (`pg_database_size()`, an allocated GB being a
  GiB) and moves it through Q27's steps: `warning` at 80 %; `read_only` at
  100 %, the database's default, which `BEGIN READ WRITE` gets round to
  delete data; `blocked` at 120 %, connections refused at PgBouncer and in
  PostgreSQL, start and restart refused; back to read-write under 95 %.
  The cluster is made to match the step at every check. Each change is an
  `instance.storage` audit event; `otter_db_size_bytes` per database.
  `otter admin instances reopen` opens a blocked database read-only for
  24 hours, audited as `instance.reopen`. A change of read-only reconnects
  the database in PgBouncer, which would otherwise keep the old value on
  its pooled connections.
- **Expiry and deletion** (#100, Q7): a database may get an `expires_at`
  at create, and be marked `ephemeral`. Every minute `otter api` queues a
  stop for a running database past its expiry, and a delete 7 days later,
  whose final dump is kept 7 days; an ephemeral one is deleted at its
  expiry with no dump. Each step is an `instance.expire` audit event by
  `system`. A database stopped past its expiry shows the reason `expired`
  and can't be started until the customer moves or clears its expiry. The
  worker removes final dumps past their expiry from the node (3 days after
  a customer's delete, 7 after an expiry's) and audits each
  (`backup.purge`, `otter_expiry_actions_total`).
- **Password reset links** (#160), until the platform sends email: `otter
  admin user reset-link <email>` prints a single-use link the operator
  hands over, valid for an hour (`-ttl`, at most 24 hours), audited with
  `-reason`. Using it sets a new password, ends every session of the
  user's and logs the request in. A newer link replaces an unused one; a
  disabled user or an inactive account gets none, and a link issued before
  stops working.
- **Operator commands for the queue** (#102): `otter admin instances list`
  (owner, status, node, cluster and infra name; `-all` for deleted ones),
  `otter admin ops list` (filters by status and instance; the only place an
  operation's `last_error` is shown, and only while it isn't succeeded or
  canceled, so an earlier attempt's error isn't read as the outcome), `otter admin ops retry` and
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

- `POST /api/account/password` (`{"current_password", "new_password"}`,
  #78): `200` with the user and a new session cookie; `403` for a wrong
  current password, `400` for one the policy refuses, `429` past the login
  limits. Audited as `user.password_change`, refusals included.
- `POST /api/instances` takes an optional `expires_at` (at least an hour
  away, else `422`) and `ephemeral`; the instance shows both (#100). An
  `Idempotency-Key` replayed with a different value of either is a `422`.
- An instance has `storage` (`used_bytes` and `measured_at` once
  measured, `allocated_bytes`, `state`), and the reasons
  `over_storage_quota` and `storage_blocked` (#99). Starting or
  restarting a blocked database is a `409`.
- `PATCH /api/instances/{id}` (`{"expires_at": time | null}`, #100): moves
  or clears the expiry; `200` with the instance; `400` without
  `expires_at`, `422` for one less than an hour away, `409` once it is
  being deleted. Audited as `instance.set_expiry`. Starting a database
  stopped past its expiry is a `409` until then.
- `POST /api/password-reset` (`{"token", "password"}`, #160): the token
  from the link's fragment (`/reset-password#token=…`). `200` with the user
  and a new session cookie; `400` for a link that isn't valid, is used or
  expired, or a password the policy refuses; `429` past the address's
  limit, which bad links count against. Audited as `auth.password_reset`.
- `GET /api/account/sessions`: the user's live sessions (`id`,
  `created_at`, `last_seen_at`, `ip`, `user_agent`, `current`), newest
  first. `DELETE /api/account/sessions/{id}` ends one (`204`; `404` for
  one that isn't the user's or is over); ending the current one is a
  logout. `DELETE /api/account/sessions` ends all but the current one
  (`200`, `{"ended": n}`). Audited as `session.revoke`.
- `POST /api/instances/{id}/stop`, `/start` and `/restart` (#95): no
  body; `202` with the instance and its queued operation; `409` while
  another operation is in flight, or when the status doesn't allow it
  (restart a stopped database, start a running one); audited as
  `instance.stop`, `instance.start` and `instance.restart`.
- `POST /api/instances/{id}/reset-password` (#101): no body, an optional
  `Idempotency-Key`; `202` with the instance, the queued operation and the
  new `password`, shown only in this response. A replay gets the same
  operation and `"password_shown": false`. `409` while another operation
  is in flight, or unless the database is running or stopped. Audited as
  `instance.reset_password`.

### Changed

- A delete runs its steps for an instance already recorded deleted, rather
  than returning at once: the reconciler's orphan delete needs it, and
  every step is an ensure. A failed delete leaves a deleted instance
  deleted (#96).
- A failed operation that a later one on the same instance superseded
  can't be retried: `otter admin ops retry` says so (#95).

### Migrations

- `0019_storage_quota`: an instance's `storage_used_bytes`,
  `storage_measured_at`, `storage_state` and `cleanup_until`. The worker
  sets the first three; an operator, the step and the window (#99).
- `0018_expiry`: `instances.ephemeral`, `backup_records.purged_at`.
  `otter_api` may change an instance's `expires_at`; `otter_worker` may
  set a backup record's `purged_at`, nothing else of it (#100).
- `0017_password_resets`: the reset links, by token hash, at most 24 hours
  long. `otter_admin` issues and revokes them; `otter_api` only marks one
  used (#160).
- `0016_reconciler`: `otter_worker` may insert an operation, and a
  restrictive policy allows only a `delete` asked by `system` on its own
  node, never with a payload (#96).
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
