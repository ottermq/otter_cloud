# Open Questions

Decisions that need the project owner's input. Numbering is stable across
revisions. Last updated 2026-11-01 (Q27 decided).

## Decided

| # | Question | Decision | Where |
|---|---|---|---|
| Q1 | Infrastructure layer | **Extend JailDeck as the host agent; remove Sylve.** JailDeck's scope widens to bhyve and (phased, if needed) PF | [host-layer.md](host-layer.md), [jaildeck-scope.md](jaildeck-scope.md) |
| Q2 | Split api / worker by privilege | **Yes**, two processes from the start | `architecture.md` §1 |
| Q2b | Separate machine for the web tier? | **Designed for, not required.** Deployment profiles: *minimum* (control plane in jails on the node) now; *split* (separate box or VPS jail) optional; *multi-node* when demand grows. The code is identical in all three | `architecture.md` §1 |
| Q6 | Storage | Hardware is the **minimum node spec**, not a design target. After the 2026-10-20 reinstall of the reference node: OS on its own pool, jails and all platform and tenant data on a mirrored pool, and a local replica on separate disks. Nothing irreplaceable on the OS pool. Node-specific layout is in the operator's notes | `feasibility.md` §2, §7 |
| Q3 | Customer authentication | **A.** Local email + password (argon2id), invite-only; TOTP after the MVP | `domain-model.md` User, `security.md` |
| Q4 | PostgreSQL TLS termination | **A.** At Traefik on the VPS with a Let's Encrypt wildcard (DNS-01); passthrough stays a deferred improvement. Confirmed through the edge 2026-11-02 (#23) | `architecture.md` §7, AD-7, `feasibility.md` §8.1 |
| Q7 | Expiry and deletion | **Proposal accepted:** stop at `expires_at`; after 7 days, final snapshot then delete, snapshot kept 7 days; user delete keeps a final snapshot 3 days; `ephemeral=true` skips both. The final snapshot is moved out of the jail's datasets before the jail is destroyed. For shared-cluster databases the "final snapshot" is a snapshot of the tenant's tablespace dataset (if Q22 adopts it) or a final `pg_dump` | `domain-model.md` §6, `architecture.md` §6 |
| Q9 | Off-site backups | **Pluggable target**, not a fixed service: `none` (no-op), `fs` (our own storage, e.g. another machine), `s3` (any S3-compatible provider, commercial or self-hosted). Encryption and retention sit above the interface | `architecture.md` §10, AD-11; `domain-model.md` BackupRecord |
| Q14 | DNS provider | **Cloudflare** (where the zone already is). DNS-only (grey cloud) records; token scoped to `Zone:Zone:Read` and `Zone:DNS:Edit` on `otter-labs.com` | `architecture.md` §7 |
| Q17 | Message bus | **No bus on the provisioning path.** The platform DB `operations` table is the queue; outbox → bus only when event fan-out appears; OtterMQ dogfooded as a managed service | `architecture.md` §2, AD-3 |
| Q18 | Worker placement | **Node-local workers that pull** from the platform DB; the control plane never connects to nodes | `architecture.md` §1, AD-2 |
| Q20 | PostgreSQL product model | **Shared clusters now, dedicated later** (2026-10-21). The MVP and the Founder Beta sell **databases** in a shared PostgreSQL cluster per node: the customer gets one database and one role, never a server. The jail-per-instance design is kept as a future **dedicated** tier | `architecture.md` §6, AD-5 |
| Q21 | Connection pooling | **PgBouncer from the start** (2026-10-21), in front of every shared cluster. It enforces per-database client limits and keeps server connections few | `architecture.md` §6, AD-12 |
| Q22 | Per-database storage quota | **Soft quota with a cluster backstop** (decided 2026-10-26 by the POC). The worker polls `pg_database_size()` and acts over quota; a ZFS quota on the cluster's `data` dataset, sized well above what the cluster has sold, protects the node. The tablespace-per-tenant POC **failed**: a tenant at its `refquota` made other tenants' queries and every checkpoint fail, and after a crash kept the whole cluster unavailable for 50 minutes. Per-tenant datasets without a quota: deferred pending research (#61) | [poc-tablespace-quota.md](poc-tablespace-quota.md) |
| Q16 | Operator access | **`otter admin` over SSH** on the control plane (or the node, in the minimum profile), plus the JailDeck UI through an SSH `LocalForward`. No web admin for Otter in the MVP; a separate operator portal comes later (Q24). Only the owner has operator access for the beta. `otter admin` connects as its own **`otter_admin`** role with only the grants its commands use (column-level `UPDATE` on user status and session revocation), never as `otter_api` (2026-10-26) | `security.md` §6 Authorization, `deploy/platform-db/bootstrap.sql` |
| Q11 | Payment abstraction | **The boundary in data now** (2026-10-28): every subscription carries `billing_provider` (`none` in the MVP) and `payment_status`; a plan with a price cannot become `active` without a verified payment event, enforced in `billing` and tested; the `PaymentProvider` interface and webhooks arrive with the first real provider (M12). Founder Beta is `payment_status=not_required` | `domain-model.md` Subscription, `architecture.md` §8, AD-9 |
| Q12 | Founder Beta and flavors | **Decided 2026-10-28**, sized for a first cohort of five founders. Plan *Founder Beta* (0 BRL, no time limit, assigned by the operator): **2 databases, 10 GB, 60 client connections** in total. Flavors: **`db-small` 2 GB / 20 connections, `db-medium` 5 GB / 30 connections**. The plan is exactly two of the largest flavor, so any two databases fit. Per database: `statement_timeout` 60 s, `idle_in_transaction_session_timeout` 5 min, `temp_file_limit` = the flavor's storage, a server pool of 5 connections; `work_mem` stays the cluster's default, set with its memory. Five founders at their limits: 10 databases, 50 GB, 300 client connections. Plan limits don't depend on node memory; the cluster's memory settings do. The earlier per-jail values (`pg-small`, `pg-medium`) are kept for the dedicated tier (M13) | `domain-model.md` Plan, Flavor |
| Q19 | Apply the JailDeck doc changes | **Applied 2026-10-21** (reversed the earlier "not now" once the owner's JailDeck work merged): merged into JailDeck `dev` with JD-016 to JD-020 and the new milestones, tracked as [ottermq/jaildeck#1–#18](https://github.com/ottermq/jaildeck/issues). See [jaildeck-scope.md](jaildeck-scope.md) | — |
| Q23 | PgBouncer default pool mode | **Transaction pooling by default** (2026-10-28), documented for customers: session state (`SET`, session advisory locks, `LISTEN`, temporary tables across transactions) doesn't survive between transactions; protocol-level prepared statements work (`max_prepared_statements`). **Session pooling as a per-database option** (`pool_mode` on the database's entry); how a customer asks for it is decided when it is offered | `architecture.md` §6 |
| Q26 | Final dump of a deleted shared database | **Decided 2026-10-28.** Until M7's backup target exists, the final `pg_dump` of Q7 is kept **on the node, outside the cluster jail, in a directory only the worker's user can read**; the worker deletes it after **3 days** (user delete) or 7 (expiry); **only an operator restores it**, on request. **A failed dump fails the delete** and the database stays. `ephemeral` skips it | `architecture.md` §6 |
| Q27 | Storage quota enforcement steps (Q22) | **Decided 2026-11-01.** Polled with `pg_database_size()`. **80 %:** a warning (API and metric). **100 %:** the database defaults to read-only and its sessions are ended so it takes effect; reason `over_storage_quota`. Advisory: the tenant can turn it off in a session to delete data, and the message says how. **120 %:** connections refused, at PgBouncer and in PostgreSQL, like a stopped database; reason `storage_blocked`. **Way out:** back under 95 % → read-write again by itself; a blocked database is reopened only by an operator (`otter admin`, audited) for a 24-hour cleanup window in read-only, blocked again if still over 120 % when it ends | `architecture.md` §5 Storage quota |

## Awaiting decision

### Q5. Hosting terms

- VPS providers' terms may forbid commercial resale or exploitation without
  written approval.
- Residential ISP contracts often forbid servers.

A free beta is arguably fine; paid plans need written confirmation.
**Blocks:** the beta terms (M7, #125; ISP and VPS terms checked), M12 (VPS
terms).

The facts about the connection and the VPS are recorded in the operator's
private notes (#26). What they imply:

- **Reachability:** the node needs no inbound port and no fixed address. It
  dials the edge over WireGuard and keeps the tunnel up, and customers only
  ever see the edge's address. When the node's public address changed, the
  tunnel came back by itself.
- **Bandwidth:** the node's upload is a home connection's, far below a
  datacentre's, and everything leaving the node shares it: every customer's
  query results, the portal, and later off-site backups (#119). That is
  enough for the beta's small databases, but not something to promise. So
  the beta states no throughput (#125); a large result set or a large export
  takes a while; one customer reading heavily slows the others, since
  nothing limits bandwidth per customer; and a first full off-site copy is
  measured in hours, so the target and schedule are chosen with that in mind.

### Q8. Do stopped instances keep their memory reservation?

**Recommendation: yes** (start always succeeds). Since Q20 this only
concerns **dedicated** instances: a shared-cluster database holds no memory
reservation of its own.
**Blocks:** the dedicated tier (no longer M2).

### Q10. PostgreSQL version and customer privileges

- **Version:** PostgreSQL 18, decided 2026-11-02 (#25): one major version
  per cluster, as the `pg18` template installs it.
- **Privileges:** the customer owns one database and is not a superuser.
  In a shared cluster (Q20) that is enforced, not optional: **no
  `CREATEROLE`** (roles are cluster-wide), no `CREATEDB`, no
  `pg_read_server_files`/`pg_write_server_files`/`pg_execute_server_program`,
  no untrusted procedural languages, and `CONNECT` on every database revoked
  from `PUBLIC`.
- **Extensions, decided 2026-11-02 (#25):** every extension PostgreSQL marks
  trusted among those the template installs (`plpgsql` and `postgresql18-contrib`),
  and nothing else. The customer owns its database, so it creates them
  itself, and PostgreSQL refuses it the untrusted ones (`file_fdw`, `dblink`,
  `postgres_fdw`, `pg_stat_statements`), which a test checks. So the
  allowlist is the template's package list. The list today:
  `btree_gin`, `btree_gist`, `citext`, `cube`, `dict_int`, `fuzzystrmatch`,
  `hstore`, `intarray`, `isn`, `lo`, `ltree`, `pg_trgm`, `pgcrypto`,
  `plpgsql`, `seg`, `tablefunc`, `tcn`, `tsm_system_rows`,
  `tsm_system_time`, `unaccent`, `uuid-ossp`. `pg_stat_statements` isn't
  preloaded. PostGIS and pgvector are later requests, each a platform
  decision: in a shared cluster an extension is installed once for everyone.

**Blocks:** nothing now.

### Q13. Isolation level per service

**Recommendation:** hardened VNET jails for protocol-only managed services
(Redis, RabbitMQ, OtterMQ, GoodiesDB, dedicated PostgreSQL). bhyve for
anything with shell access. Re-evaluate for services that run
customer-supplied code.

Shared PostgreSQL (Q20) is the exception by design: tenants share one
server process inside one hardened jail, and are separated by PostgreSQL's
own privileges (Q10). See `security.md` R17.
**Blocks:** confirm before M9/M10.

### Q24. Operator portal

Decided in principle (2026-10-28): the operator gets a web portal of its
own, served by a **different server from the customer portal**. The MVP
keeps Q16 (`otter admin` over SSH). Open:

- when it is built: after the beta, or when the CLI stops being enough;
- how the operator authenticates, given that the operator is not a
  customer account;
- which pages, decided by what the operator actually does with the CLI.

Proposed constraints are in [portals.md](portals.md) §2: off the public
edge, its own process and listener, the `otter_admin` grants, the CLI's
audit rules, and no direct JailDeck access. **Blocks:** nothing in the MVP.

### Q25. FreeBSD as a product

FreeBSD is the platform's operating system, but no MVP product puts a
customer on FreeBSD: they buy a database. Two possible later products have
very different threat models, so they should not be folded into each
other or into the dedicated PostgreSQL tier, which also uses jails:

| Product | Customer access | Isolation |
|---|---|---|
| Managed FreeBSD jail | Shell and limited administration | Shared kernel |
| FreeBSD VM | Root on a whole system | bhyve |

A customer with a shell in a jail breaks the assumption behind managed
jails today: that customers run no OS commands in them (Q13, `security.md`
§7). **Recommendation:** root access means bhyve (M9); a jail product is
evaluated separately, with its own threat model, if demand appears.
**Blocks:** M9.

### Q15. Licence, privacy, terms

- **Licence:** ~~open~~ **MIT, decided 2026-10-21** (`LICENSE`), matching
  JailDeck ("Copyright (c) 2026 Otter Labs"). Any Sylve code ported verbatim
  into either repo keeps its BSD-2-Clause header, which is compatible with MIT.
- **LGPD:** a privacy notice and beta terms before onboarding (M7). They
  should cover:
  - data location;
  - retention;
  - no uptime guarantee;
  - an acceptable-use policy.
- Is this a personal project or a company?

**Blocks:** licence before the first public push; terms by M7.

## Facts needed (not decisions)

- ~~Pool layout, disk health, VT-x/UG~~: verified 2026-10-20 (`feasibility.md` §2).
- ~~The edge VPS plan, and the home connection (CGNAT, static IP, upload
  bandwidth)~~: recorded privately 2026-11-02 (#26); what they imply is in Q5.
- Whether the existing Prometheus and Grafana can scrape the node and the
  VPS.
