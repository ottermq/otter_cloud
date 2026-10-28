# Changelog

Every released version is recorded here, with a
[release](https://github.com/ottermq/otter_cloud/releases) for each. Releases
follow milestones: the minor version is the milestone it completes
([roadmap](docs/roadmap.md), "How the work is organised"). Dates are the tag
date; numbers link to [the tracker](https://github.com/ottermq/otter_cloud/issues).

Changes to the HTTP API are recorded under an **API** heading, and to the
platform database under **Migrations**, so an upgrade knows what to expect.
Before 1.0 breaking changes are allowed, but never silent.

## Unreleased: v0.2.0 (M1 Skeleton and Identity, M2 Plans and Placement)

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

### API

- `POST /api/signup`, `POST /api/login`, `POST /api/logout`, `GET /api/me`.
- `GET /api/catalog`.
- `GET /api/operations`, `GET /api/operations/{id}`,
  `GET /api/instances/{id}/operations` (#77).

### Migrations

- `0001_privileges` to `0010_instances`. Before `0006`, a superuser runs
  `CREATE ROLE otter_admin LOGIN;` (`deploy/platform-db/bootstrap.sql`).
