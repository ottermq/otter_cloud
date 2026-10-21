# Open Questions

Decisions that need the project owner's input. Numbering is stable across
revisions. Last updated 2026-10-21 (shared clusters: Q20–Q22).

## Decided

| # | Question | Decision | Where |
|---|---|---|---|
| Q1 | Infrastructure layer | **Extend JailDeck as the host agent; remove Sylve.** JailDeck's scope widens to bhyve and (phased, if needed) PF | [host-layer.md](host-layer.md), [jaildeck-scope.md](jaildeck-scope.md) |
| Q2 | Split api / worker by privilege | **Yes**, two processes from the start | `architecture.md` §1 |
| Q2b | Separate machine for the web tier? | **Designed for, not required.** Deployment profiles: *minimum* (control plane in jails on the node) now; *split* (separate box or VPS jail) optional; *multi-node* when demand grows. The code is identical in all three | `architecture.md` §1 |
| Q6 | Storage | Hardware is the **minimum node spec**, not a design target. After the 2026-10-20 reinstall of the reference node: OS on its own pool, jails and all platform and tenant data on a mirrored pool, and a local replica on separate disks. Nothing irreplaceable on the OS pool. Node-specific layout is in the operator's notes | `feasibility.md` §2, §7 |
| Q3 | Customer authentication | **A.** Local email + password (argon2id), invite-only; TOTP after the MVP | `domain-model.md` User, `security.md` |
| Q4 | PostgreSQL TLS termination | **A.** At Traefik on the VPS with a Let's Encrypt wildcard (DNS-01); passthrough stays a deferred improvement | `architecture.md` §7, AD-7 |
| Q7 | Expiry and deletion | **Proposal accepted:** stop at `expires_at`; after 7 days, final snapshot then delete, snapshot kept 7 days; user delete keeps a final snapshot 3 days; `ephemeral=true` skips both. The final snapshot is moved out of the jail's datasets before the jail is destroyed. For shared-cluster databases the "final snapshot" is a snapshot of the tenant's tablespace dataset (if Q22 adopts it) or a final `pg_dump` | `domain-model.md` §6, `architecture.md` §6 |
| Q9 | Off-site backups | **Pluggable target**, not a fixed service: `none` (no-op), `fs` (our own storage, e.g. another machine), `s3` (any S3-compatible provider, commercial or self-hosted). Encryption and retention sit above the interface | `architecture.md` §10, AD-11; `domain-model.md` BackupRecord |
| Q14 | DNS provider | **Cloudflare** (where the zone already is). DNS-only (grey cloud) records; token scoped to `Zone:DNS:Edit` on `otter-labs.com` | `architecture.md` §7 |
| Q17 | Message bus | **No bus on the provisioning path.** The platform DB `operations` table is the queue; outbox → bus only when event fan-out appears; OtterMQ dogfooded as a managed service | `architecture.md` §2, AD-3 |
| Q18 | Worker placement | **Node-local workers that pull** from the platform DB; the control plane never connects to nodes | `architecture.md` §1, AD-2 |
| Q20 | PostgreSQL product model | **Shared clusters now, dedicated later** (2026-10-21). The MVP and the Founder Beta sell **databases** in a shared PostgreSQL cluster per node: the customer gets one database and one role, never a server. The jail-per-instance design is kept as a future **dedicated** tier | `architecture.md` §6, AD-5 |
| Q21 | Connection pooling | **PgBouncer from the start** (2026-10-21), in front of every shared cluster. It enforces per-database client limits and keeps server connections few | `architecture.md` §6, AD-12 |
| Q22 | Per-database storage quota | **POC first** (2026-10-21): try a tablespace per tenant on its own ZFS dataset with `refquota`. If the POC can't show that a full tenant leaves the cluster healthy, use the **soft quota** (poll `pg_database_size()`, make the database read-only) with a ZFS quota on the cluster dataset as backstop. **Result pending** | [poc-tablespace-quota.md](poc-tablespace-quota.md) |
| Q19 | Apply the JailDeck doc changes | **Applied 2026-10-21** (reversed the earlier "not now" once the owner's JailDeck work merged): merged into JailDeck `dev` with JD-016 to JD-020 and the new milestones, tracked as [ottermq/jaildeck#1–#18](https://github.com/ottermq/jaildeck/issues). See [jaildeck-scope.md](jaildeck-scope.md) | — |

## Awaiting decision

### Q5. Hosting terms

- VPS providers' terms may forbid commercial resale or exploitation without
  written approval.
- Residential ISP contracts often forbid servers.

A free beta is arguably fine; paid plans need written confirmation.
Also: is the home connection behind CGNAT? Is the IP static?
**Blocks:** M0 (ISP / CGNAT), M12 (VPS terms).

### Q8. Do stopped instances keep their memory reservation?

**Recommendation: yes** (start always succeeds). Since Q20 this only
concerns **dedicated** instances: a shared-cluster database holds no memory
reservation of its own.
**Blocks:** the dedicated tier (no longer M2).

### Q10. PostgreSQL version and customer privileges

- **Version:** one major version, the latest packaged for FreeBSD at M0
  (likely 18).
- **Privileges:** the customer owns one database and is not a superuser.
  In a shared cluster (Q20) that is enforced, not optional: **no
  `CREATEROLE`** (roles are cluster-wide), no `CREATEDB`, no
  `pg_read_server_files`/`pg_write_server_files`/`pg_execute_server_program`,
  no untrusted procedural languages, and `CONNECT` on every database revoked
  from `PUBLIC`.
- **Extensions:** trusted contrib extensions (`pgcrypto`, `uuid-ossp`,
  `citext`, `pg_trgm`, `hstore`), with `pg_stat_statements` preloaded.
  PostGIS and pgvector on request. In a shared cluster an extension is
  installed once for everyone, so the allowlist is a platform decision.
- **Still open:** the version, and the extension allowlist.

**Blocks:** M0 (template), M3.

### Q12. Founder Beta and flavors (reopened by Q20)

The accepted values (Founder Beta: 2 instances, 2 GB, 2 vCPU, 20 GB;
`pg-small` 512 MB / 1 vCPU / 5 GB; `pg-medium` 1 GB / 1 vCPU / 10 GB) were
sized for one jail per instance. They now apply to the **dedicated** tier.
A shared-cluster database has no memory or vCPU of its own, so its flavor
is storage plus connections.

**Proposal:**

| | Databases | Storage | Client connections |
|---|---|---|---|
| Founder Beta (plan, 0 BRL, no time limit) | 2 | 10 GB total | 40 total |
| `db-small` (flavor) | — | 2 GB | 20 |
| `db-medium` (flavor) | — | 5 GB | 40 |

Per database, also: `statement_timeout` 60 s,
`idle_in_transaction_session_timeout` 5 min, `temp_file_limit` = the
flavor's storage. **Blocks:** M2.

### Q11. Payment abstraction now or with the first real provider?

**Recommendation:** express the boundary in data now (`billing_provider`,
`payment_status`; paid plans cannot activate without a verified payment
event). Add the interface with the first real provider. **Blocks:** M2.

### Q13. Isolation level per service

**Recommendation:** hardened VNET jails for protocol-only managed services
(Redis, RabbitMQ, OtterMQ, GoodiesDB, dedicated PostgreSQL). bhyve for
anything with shell access. Re-evaluate for services that run
customer-supplied code.

Shared PostgreSQL (Q20) is the exception by design: tenants share one
server process inside one hardened jail, and are separated by PostgreSQL's
own privileges (Q10). See `security.md` R17.
**Blocks:** confirm before M9/M10.

### Q23. PgBouncer default pool mode

- **Transaction pooling** shares server connections between clients, which
  is what makes a shared cluster dense. Protocol-level prepared statements
  work (`max_prepared_statements`), but session state does not: `SET`,
  session advisory locks, `LISTEN`, and temporary tables across
  transactions.
- **Session pooling** behaves like a direct connection but gives each
  client a server connection for its whole session.

**Recommendation:** transaction pooling by default, documented for
customers, with session pooling as a per-database option (PgBouncer allows
`pool_mode` per database). **Blocks:** M3.

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

### Q16. Operator access

**Recommendation:** `otter admin` over SSH (on the control plane, or the node
in the minimum profile), plus the JailDeck UI via an SSH `LocalForward`. No
web admin for Otter in the MVP. Who besides you gets operator access?
**Blocks:** M1.

## Facts needed (not decisions)

- ~~Pool layout, disk health, VT-x/UG~~: verified 2026-10-20 (`feasibility.md` §2).
- The edge VPS plan (RAM, bandwidth, OS).
- Home connection: CGNAT? Static IP? Upload bandwidth?
- Whether the existing Prometheus and Grafana can scrape the node and the
  VPS.
