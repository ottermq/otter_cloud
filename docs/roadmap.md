# Development Roadmap

Status: **proposal for review**, revision 2 (2026-10-19): JailDeck replaces
Sylve, and the system is a control plane plus compute nodes.
Update 2026-10-21: **shared clusters** (Q20–Q22). The MVP sells databases
in a shared PostgreSQL cluster, so M3 no longer waits for JailDeck's jail
provisioning (J4) or exec (J5). Dedicated instances become a post-MVP
milestone (M13) that does.

Two tracks run in parallel. They meet at Otter M7 (metrics, packaging) and,
after the beta, at M13 (dedicated instances):

- **JailDeck track (J):** host-agent capabilities, in the JailDeck repo
  ([jaildeck-scope.md](jaildeck-scope.md) §6).
- **Otter track (M):** the cloud platform, in this repo.

Estimates assume one developer, part-time. They are for sequencing only.

```mermaid
flowchart LR
    subgraph JailDeck
        J1[J1 Host Setup] --> J2[J2 Storage<br/>read + write]
        J2 --> J3[J3 Templates]
        J3 --> J4[J4 Jail Provisioning<br/>VNET, limits, datasets]
        J4 --> J5[J5 Exec + automation<br/>contract]
        J5 --> J6[J6 Metrics]
        J6 --> J7[J7 Packaging]
    end
    subgraph Otter
        M0[M0 Host foundation, shared<br/>cluster, quota POC, network] --> M3
        M1[M1 Skeleton + identity] --> M2[M2 Plans, quotas,<br/>placement]
        M2 --> M3[M3 Operations +<br/>database provisioning]
        M3 --> M4[M4 Lifecycle,<br/>reconciler, expiry]
        M3 --> M5[M5 Public connectivity]
        M4 --> M6[M6 Portal]
        M5 --> M6
        M6 --> M7[M7 Beta readiness]
    end
    J1 -.-> M0
    J2 -.->|only if Q22 adopts tablespaces| M3
    J6 --> M7
    J7 --> M7
    M7 --> BETA((Beta))
    J5 --> M13[M13 Dedicated<br/>instances]
    BETA --> M13
```

Critical path: **M1 → M2 → M3**, with M0 alongside. JailDeck is needed for
the beta through metrics and packaging (J6, J7); J4 and J5 now matter for
dedicated instances (M13), cluster creation by API, and VMs. The cluster
jail for the MVP can be built by hand from the M0 runbook.

---

## JailDeck track (summary; details belong in JailDeck's ROADMAP)

Applied to JailDeck's docs on 2026-10-21 and tracked as GitHub issues
[ottermq/jaildeck#1–#18](https://github.com/ottermq/jaildeck/issues), with milestones
named after JailDeck's and an `otter` label.
JailDeck names its milestones instead of numbering them; J1–J9 are this
document's shorthand. J2 is JailDeck's *Storage Visibility* plus *Storage
Writes*, J5 is *Exec and Automation Contract*, J8 is *Firewall Visibility*,
J9 is *Virtual Machines*; the rest keep their names.

| | Milestone | Acceptance (as Otter needs it) |
|---|---|---|
| J1 | Host Setup | `jaildeck` reports pool layout, racct status, VT-x/EPT/UG, tenant bridge and devfs ruleset presence |
| J2 | Storage read + write | List datasets, snapshots, user properties; create child datasets with properties; snapshot, rollback, destroy (with confirm) |
| J3 | Templates | Release template to `@base`. A derived template (base + `postgresql18-server` + bootstrap script) can be produced and snapshotted |
| J4 | Jail Provisioning | `PUT /api/jails/{name}` with the full spec (VNET private member, rctl, cpuset, allowlisted params, child datasets, user properties) is idempotent; destroy with confirm removes everything |
| J5 | Exec + automation | Exec endpoint, unix socket listener, `X-Request-Id` in the audit log, API change policy in CHANGELOG |
| J6 | Metrics | `/metrics` with per-jail CPU, memory and processes |
| J7 | Packaging | rc.d service, install docs. A node can be rebuilt from docs |
| J8 | PF visibility | Read-only PF view |
| J9 | VMs | JD-019 spike → `vms` domain (needed for Otter M9) |

---

## M0: Host foundation and network spikes

**Goal:** a correctly prepared node, plus proof that the network path and
TLS routing work, before platform code depends on them.

**Scope (scripts and notes; no Otter application code):**

Step-by-step procedure: [runbook-m0.md](runbook-m0.md).

1. ~~Record node facts~~: done 2026-10-20 after the reinstall (UG present;
   storage layout as planned; disk health monitored). See `feasibility.md`
   §2; the node's specifics are in the operator's notes.
2. Host baseline:
   - FreeBSD 15.1 (done, p5);
   - `kern.racct.enable=1` in `/boot/loader.conf` (currently off) + reboot;
   - `vfs.zfs.arc.max`;
   - jail `exec.timeout` per JailDeck JD-014;
   - devfs ruleset;
   - tenant bridge (no physical member);
   - **pf.conf** (static policy) in a Git-tracked host-config directory;
   - decide where any non-Otter jails live relative to tenants
     (`security.md` R16).
3. JailDeck installed and running as root (current `dev` plus J1).
4. The **shared cluster jail** created **by hand** following
   `architecture.md` §6.1 (runbook Part E):
   - root clone plus `data` dataset with a backstop quota;
   - VNET private member;
   - rctl on the jail as a whole;
   - PostgreSQL plus PgBouncer (`auth_query`, `max_prepared_statements`);
   - two test tenant databases created with the §6.1 privilege set, and a
     check that each tenant can reach only its own database.

   This is the MVP's cluster, and later the reference that J3/J4 automate.
5. **Tablespace quota POC** ([poc-tablespace-quota.md](poc-tablespace-quota.md)),
   which decides Q22.
6. WireGuard node ↔ VPS: CGNAT check, MTU, keepalive.
7. Traefik ≥ 3.6.8 on the VPS, in front of PgBouncer:
   - SNI routing with psql 16 (SSLRequest), psql 17/18 (direct TLS), pgx and
     JDBC;
   - TLS termination vs passthrough;
   - HTTP-provider config pull.
8. Isolation script: `nc` probes from a tenant jail to the LAN, host, other
   tenant, control-plane addresses and the internet; from the VPS to ports
   that should not be allowed.

**Acceptance:**

- From outside the home network, `psql "host=spike.db.otter-labs.com
  sslmode=verify-full"` (or `require`, per Q4) reaches a test database
  through Traefik and PgBouncer.
- The quota POC has a recorded result, and Q22 is closed.
- The isolation script passes.
- `feasibility.md` §8 items are marked VERIFIED or FAILED.

**Risks:**

- ~~UG missing~~: UG confirmed 2026-10-20.
- CGNAT MTU problems.
- Traefik PostgreSQL TLS behaviour.

**Effort:** 1–2 weeks. It can overlap with J1–J3 and with M1.

---

## M1: Platform skeleton and identity

- Repo layout and the `otter` binary (`api`, `worker`, `admin`, `migrate`).
- CI: build, vet, staticcheck, govulncheck, tests against PostgreSQL.
- Schema: users, accounts, sessions, invites, audit_events.
- Invite → sign-up → login → logout → `/api/me`.
- CSRF, login rate limiting, slog, `/healthz`, `/metrics`.
- `otter admin invite create`, `user disable`.

**Acceptance:**

- Integration tests: invite valid, expired and reused; login ok, fail and
  rate-limited; logout revokes the session; audit rows written.
- The api DB role cannot modify audit_events.

**Dependencies:** none. **Effort:** 1–2 weeks.

---

## M2: Plans, subscriptions, entitlements, quotas, placement

- Tables: plans, subscriptions, flavors, **infrastructure_nodes** and
  **clusters** (seeded from reviewed config).
- `billing.Entitlement`.
- `instances` records with the full eligibility transaction, including
  **placement across clusters and nodes**. Operations rows are created but not yet
  executed.
- `otter admin subscription create`, `otter admin node add|drain`,
  `otter admin cluster add|drain`.
- `GET /api/catalog`, `GET /api/subscription`, `POST/GET/DELETE /api/instances`.

**Acceptance:**

- No subscription → 403.
- A paid plan cannot activate.
- Over quota → 409, naming the limit.
- No cluster with capacity → 409/503.
- **20 parallel creates against a quota of 2 → exactly 2.**
- Placement picks the cluster with the most free storage that still has
  connection headroom. A draining cluster or node is never chosen.
- Cross-account access → 404.
- No plan codes or prices referenced outside `billing` (CI grep).

**Dependencies:** M1. **Effort:** 1 week.

---

## M3: Operations engine and database provisioning

- `operations`:
  - node-scoped claim with lease;
  - checkpoints, retries, backoff;
  - `LISTEN/NOTIFY` wake-up;
  - a node-specific DB role restricted to its own rows.
- `clusters` table; `otter admin cluster add` registers the hand-built
  cluster jail.
- `postgres` driver for shared databases (cert-auth admin connection):
  - EnsureTablespace (dataset through JailDeck, only if Q22 adopts it),
    EnsureRole (SCRAM verifier), EnsureDatabase (privileges and limits),
    EnsurePoolEntry (PgBouncer `RELOAD`);
  - delete.
- A privilege test run after every create: the new role cannot connect to
  any other database and holds none of the forbidden attributes.
- SCRAM-verifier password flow.
- Deployment, minimum profile:
  - `otter-api` and `otter-db` jails created by hand (or via JailDeck once
    J4 lands);
  - worker as an unprivileged rc.d service on the node.

**Acceptance:**

- Via curl on the real node: create → `running` → connect through
  PgBouncer with the returned password → delete. Database, role, pool entry
  (and tablespace dataset) are gone, and quota usage returns to 0.
- **Crash test:** kill the worker at each step. On restart the operation
  completes without duplicates or orphans.
- **20 databases** created on one cluster; each role reaches only its own
  database.

**Dependencies:** M0, M2 (J2 only if Q22 adopts tablespaces).
**Effort:** 1–2 weeks. Much smaller than the jail-per-instance version.

---

## M4: Lifecycle, reconciler, expiry

- Start, stop, restart. One operation per instance (409).
- Per-node reconciler comparing each cluster's `otc_*` databases and
  PgBouncer entries with the platform DB (and JailDeck inventory for jails).
- Health checks → `health`, `otter_instance_up`.
- Expiry policy (Q7). Password reset.
- `otter admin ops list|retry|cancel`, `instances list`.

**Acceptance:**

- A database dropped behind the platform's back → `failed` plus an alert.
- An orphan of a `deleted` row is cleaned up. An unknown `otc_*` database
  only raises an alert.
- Storage quota enforcement (Q22 mechanism) turns an over-quota database
  read-only and notifies; freeing space restores writes.
- Expiry stop and delete behave correctly under a fake clock.

**Dependencies:** M3. **Effort:** 1–2 weeks.

---

## M5: Public connectivity

- An edge config endpoint on `otter api` (WireGuard-bound) for Traefik's
  HTTP provider.
- `deploy/vps/` Traefik and nftables config, with the TLS mode per Q4.
- Connection info in the API.
- Portal published at `cloud.otter-labs.com`.

**Acceptance:**

- External `psql` connects within 60 seconds of `running`.
- After delete, the route is gone within the poll interval.
- The isolation script still passes.

**Dependencies:** M0, M3. **Effort:** 1 week.

---

## M6: Customer portal (Svelte 5 + Vite SPA)

Pages:

- login and invite sign-up;
- dashboard (subscription, quota usage);
- create instance;
- instance detail: status, connection info, start/stop/restart, reset
  password, delete with name confirmation;
- operation history.

**Acceptance:** the brief's 10-step MVP scenario, done by a person on another
network, recorded as a checklist run.

**Dependencies:** M4, M5. **Effort:** 1–2 weeks.

---

## M7: Beta readiness

- Backups:
  - snapshot schedule for cluster datasets (and tablespace datasets);
  - nightly `pg_dump` per database and of the platform DB;
  - the `backup.Target` interface with `none`, `fs` and `s3`; age
    encryption; retention from `backup_records` (Q9, AD-11). Which target
    the beta runs on is a deployment choice; the drills use that target.
- Restore drills: one database (without touching its neighbours), a whole
  cluster, the platform DB, node rebuild from docs.
- Prometheus scrape configs (workers, api, JailDeck `/metrics`,
  node_exporter), a Grafana dashboard, alerts, and a blackbox probe from
  the VPS.
- Scheduled isolation script with alerting.
- Secret rotation runbook.
- Security checklist (`security.md` §9).
- Beta terms and a privacy notice (Q15). User onboarding doc.

**Acceptance:**

- Every checklist item ticked.
- Drills timed and documented.
- Alerts fire in simulations: worker stopped, WireGuard down.

**Dependencies:** M6, J6, J7. **Effort:** 1–2 weeks.

### → Beta launch (≈5 founder users, minimum profile)

---

## Post-MVP (re-evaluate after the beta)

| Milestone | Content | Prerequisite |
|---|---|---|
| M8 Hardening | TOTP 2FA; per-tenant ZFS encryption evaluation; I/O limits; beta feedback | Beta |
| M8b Split profile | Move `otter api` + platform DB to a separate box or VPS; dedicated WireGuard link for worker → DB | Optional; any time after M5 |
| M9 Linux VMs | JailDeck J9 (`vms`); Otter `vm` service driver with cloud-init; SSH via edge (port-per-VM or bastion); VM egress via edge with abuse controls | UG verified; RAM capacity; J9 |
| M10 More managed services | Redis, RabbitMQ, then **OtterMQ and GoodiesDB as dogfooded managed services** (new driver plus template each) | M3 patterns stable; GoodiesDB production readiness ([ottermq/goodiesdb#36](https://github.com/ottermq/goodiesdb/issues/36): auth, crash durability, packaging) |
| M11 Second node | `otter admin node add`, second worker + JailDeck, tenant subnet + WireGuard peer; placement already supports it | Demand |
| M12 Real payments | Payment provider in `billing`, webhooks → `payment_status`; evaluate Brazilian options (Pix, Mercado Pago, Stripe BR) | Legal/tax; TOTP; external review |
| M13 Dedicated instances | One VNET jail per instance (`architecture.md` §6.2) as a paid tier: isolation, own settings and extensions, PITR; `postgresql-dedicated` service type and flavors (Q12's original values) | JailDeck J4, J5; beta feedback |
| Later | Outbox → event bus (possibly OtterMQ) for notifications and metering; teams; customer-visible backups | Real need |

## Technical risks (ranked)

| # | Risk | Mitigation | Known by |
|---|---|---|---|
| T1 | JailDeck track takes longer than expected | Since Q20 it is off the MVP's critical path except J6/J7 (metrics, packaging); the hand-built cluster jail covers M3 | J6/J7 |
| T2 | Scope creep inside JailDeck (VMs, PF writes) before Otter's MVP | JailDeck order puts Otter-critical items first; VMs and PF writes after J7 | Ongoing |
| T3 | Traefik PostgreSQL SNI/TLS behaviour | M0 test matrix; port-per-instance fallback | M0 |
| T4 | Residential connectivity (CGNAT, MTU, ISP ToS) | WireGuard tuning; external monitoring; users informed | M0 |
| T5 | Node RAM on a minimum-spec node | Strict capacity checks; small flavors; more RAM or a second node | M2/M3 |
| T6 | HDD IOPS on the minimum spec | Monitor; NVMe mirror later (Q6) | M3/beta |
| T7 | Single node failure | Backups, drills, honest beta terms; control plane survives node loss | M7 |
| T8 | VNET + rctl + `service jail` interplay on 15.1 (autostart, hooks) | Hand-built reference jail in M0 | M0 |
| T9 | **Tablespace quota fails the POC** (a full tenant stalls checkpoints or recovery) | Soft quota with a cluster backstop (Q22 fallback) | M0 |
| T10 | **Noisy neighbours in a shared cluster** (no per-tenant memory/CPU/I/O limits) | Per-database limits, PgBouncer pools, monitoring per database; move heavy users to the dedicated tier | Beta |
| T11 | **A PostgreSQL vulnerability crosses tenants** in a shared cluster (`security.md` R17) | Privilege hardening, extension allowlist, prompt minor updates | Ongoing |

## Suggested order for the next weeks

1. ~~Apply the JailDeck doc changes~~: done 2026-10-21 (Q19). **Now:** start
   M1 in this repo.
2. M0 items 1–3 on the node.
3. M0 items 4–5: the hand-built shared cluster and the tablespace quota POC
   (closes Q22).
4. M0 items 6–8 (network) while M2 is implemented.
5. M3 on the hand-built cluster. On JailDeck, J1, J2, J6 and J7 matter for
   the beta; J3–J5 for dedicated instances afterwards.
