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

- **Operations queue** for the node worker: claims with `SKIP LOCKED` and
  a renewed lease, checkpoints, retries with backoff up to five attempts,
  and a `LISTEN/NOTIFY` wake-up. Each node's worker logs in as its own
  role, `otter_node_<name>`, which row-level security keeps to its node's
  rows; `otter admin node add` prints the role to create (#47). Nothing
  runs operations yet: `otter worker` gets its handlers in later M3 issues.
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

### Migrations

- `0012_worker`: row-level security on nodes, clusters, instances and
  operations, the worker's grants and the wake-up trigger. Before a node's
  worker connects, a superuser creates its role (`deploy/platform-db/bootstrap.sql`).

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
