# Feasibility Assessment

Status: **research, 2026-10-19**. Nothing has been tested on the target host yet.

Every finding carries one of three labels:

- **VERIFIED**: confirmed in primary documentation or source code (cited).
- **INFERRED**: follows from verified facts, forum reports, or general
  experience, but not confirmed for this exact setup.
- **UNKNOWN**: must be tested on the hardware or asked of a third party.

"Requires testing" items are collected in §8 and become the Milestone 0 spike
checklist in `roadmap.md`.

## 1. Summary

| Area | Verdict |
|---|---|
| FreeBSD 15.1 on the reference node | **Feasible.** 15.1 was released 2026-06-16 and is supported until 2027-03-31; the 15 branch is supported until 2029-12-31. The reference hardware is old but well supported. |
| bhyve with Linux guests | **Feasible.** The reference node's CPU has EPT and UG (verified 2026-10-20). VM performance on older CPUs is modest. |
| Sylve as the infrastructure layer | **Not adopted** (Q1, 2026-10-19). JailDeck becomes the host agent; see `host-layer.md`. Original finding: **feasible, with significant caveats.** It has a broad REST API with async jail creation. But automation needs a full-admin (≈ root) credential, there is no exec-in-jail API, jail limits are limited to memory and CPU pinning, and the API is pre-1.0 with breaking changes between minor versions. |
| Shared cluster, one database per tenant (Q20) | **Feasible**, and much denser. PgBouncer covers per-database client limits, SCRAM `auth_query` and prepared statements in transaction mode ([PgBouncer config](https://www.pgbouncer.org/config.html)). Isolation rests on PostgreSQL privileges; a hard per-database storage quota needs the tablespace POC (§8.13). |
| One jail per PostgreSQL instance (dedicated tier, later) | **Feasible.** PostgreSQL in jails is well documented (`sysvshm=new`). Isolation is adequate for a managed service where customers get no shell; it is weaker than bhyve. |
| ZFS isolation (quota, snapshots, encryption) | **Feasible.** All needed primitives exist. Sylve does not set a quota at jail creation, so a quota must be applied afterwards (to test). |
| Public access from a residential host | **Feasible** via WireGuard to an edge VPS, plus a TCP proxy on the VPS. Several details need testing (MTU, CGNAT, TLS routing). |
| Capacity | **Small on the minimum node spec.** Tens of small PostgreSQL instances *or* a handful of Linux VMs, not both. Enough for a small beta, but quotas must be sized to the node (§7). |

## 2. Hardware and bhyve

### Facts

- **VERIFIED.** bhyve on Intel requires VT-x with EPT. *"Hosting Linux guests
  or FreeBSD guests with more than one vCPU requires VMX unrestricted mode
  support (UG)."*
  [FreeBSD Handbook: Virtualization](https://docs.freebsd.org/en/books/handbook/virtualization/),
  [vmm(4)](https://man.freebsd.org/cgi/man.cgi?query=vmm&sektion=4)
- **INFERRED (strong).** Westmere is the first Intel generation with UG.
  Nehalem has EPT but no UG. Evidence: forum reports of users fixing exactly
  this by moving from X5550 to X5650. Intel documentation was not found.
  [FreeBSD forums 58539](https://forums.freebsd.org/threads/58539/)
  Check any candidate node with `dmesg | grep VT-x`: the line must list
  `EPT`, and `UG` for Linux guests.
- **INFERRED.** CPUs without APICv/posted interrupts make VMs see more exits
  on I/O-heavy workloads. CPUs without AVX cannot run guest software built
  for x86-64-v3; x86-64-v2 (SSE4.2, POPCNT) is the safe target for images.
- **INFERRED.** On two-socket NUMA machines, pin each guest or jail to one
  socket.
- **VERIFIED.** FreeBSD 15.0 renamed `uefi-edk2-bhyve` to `edk2-bhyve` and
  changed boot ROM BAR handling. `pci.enable_bars='true'` is the workaround for
  older firmware builds.
  [15.0 release notes](https://www.freebsd.org/releases/15.0R/relnotes/)

### Reference node inspection (2026-10-20)

The reference node was inspected read-only after a reinstall. Its hardware,
addresses and pool names are kept in the operator's private notes, not in
this repo. What matters for the design:

- **UG present:** Linux guests are possible. **VERIFIED.**
- **Storage matches the plan:** OS on its own pool; jails, the platform DB
  and tenant data on a mirrored pool; a read-only local replica on separate
  disks. When the OS pool has no redundancy, **nothing irreplaceable may
  live on it**: an OS loss is a reinstall plus a config restore.
- **A local replica is not off-site.** It sits in the same box, on the same
  PSU. It speeds up restores. It does not replace the `s3` target or a
  second machine.
- **rctl was off** (`kern.racct.enable=0`). It needs
  `kern.racct.enable=1` in `/boot/loader.conf` and a reboot (runbook Part B).
- **PF was not loaded**, and there was no bridge or WireGuard yet.
- **The operator's own jails share the host network stack** (`ip4=inherit`,
  a LAN alias). Once tenants exist, PF rules that let the host reach tenant
  port 5432 also let these jails reach it. See `security.md` R16.
- Remaining facts: the VPS plan, CGNAT, Prometheus reachability (Q5 and
  `open-questions.md`).

## 3. Sylve## 3. Sylve

> **Evaluated, not adopted.** Kept as a research record. The internal code
> analysis and the decision are in `host-layer.md`. Sylve remains a
> reference for patterns, ported with BSD-2-Clause attribution when copied
> verbatim.

Source: repository [AlchemillaHQ/Sylve](https://github.com/AlchemillaHQ/Sylve)
at commit `a401a68` (2026-10-19), plus [sylve.io](https://sylve.io/) docs.
Paths are relative to that repo.

### 3.1 What it is

- **VERIFIED.** A Go (gin, gorm, SQLite) and SvelteKit platform for managing
  jails, bhyve VMs (via **libvirt**'s bhyve driver), ZFS, networking, PF,
  WireGuard, backups and clustering. BSD-2-Clause, sponsored by the FreeBSD
  Foundation. (`README.md`, `go.mod`, `LICENSE`)
- **VERIFIED.** The latest stable release is **v0.3.1 (2026-09-07)**. It is
  pre-1.0 and the repo was created 2025-03.
- **VERIFIED.** It requires **FreeBSD 15.0 or newer**; 15-STABLE with pkgbase
  is recommended. VMs require libvirt ≥ 12.5.0. (`README.md`,
  `docs/app-docs/src/content/docs/getting-started.mdx`)
- **VERIFIED.** It is effectively a **single-maintainer** project: about 97% of
  the last 6 months' commits come from one author. It is very active, with 469
  commits in 3 months.
- **VERIFIED.** A fresh install has default credentials `admin/admin`.

### 3.2 API

- **VERIFIED.** REST under `/api`, with all routes in
  `internal/handlers/routes.go`. An OpenAPI 2.0 spec is generated from
  annotations (`docs/swagger/swagger.yaml`, 336 paths) and published at
  [sylve.io/api-reference](https://sylve.io/api-reference/).
- **VERIFIED.** There is **no API versioning and no stability policy**. The
  v0.3.0 changelog says clients written for v0.2.x need updating
  (`docs/changelogs/v0.3.0.md`).
- **VERIFIED.** Auth works through `POST /api/auth/login`, which returns a JWT
  that expires after 24 h (7 d with `remember`). **There are no long-lived API
  keys or service accounts.** The client must log in again periodically.
- **VERIFIED.** **Authorization is all-or-nothing.** `User.Admin` is the only
  privilege bit. Non-admins may only `GET`; every write requires admin
  (`internal/handlers/middleware/authz.go`). An admin can configure host-side
  jail hooks and open root consoles, so **a provisioning credential is
  equivalent to root on the host**.

### 3.3 Jails

- **VERIFIED.** Endpoints:
  - Create: `POST /api/jail`, with `?async=true` returning `202` plus a task ID
    pollable at `/api/tasks/lifecycle/:id`.
  - Validate: `POST /api/jail/validate`.
  - Actions: `POST /api/jail/:ctid/actions/{start,stop,restart}`.
  - Delete: `DELETE /api/jail/:ctid?deletemacs=&deleterootfs=`.
  - Stats, logs, templates and snapshots each have their own endpoints.
- **VERIFIED.** The caller chooses the `ctId` (1–9999, unique across the
  node or cluster). This gives a natural idempotency key: the platform
  allocates the ctId before calling Sylve.
- **VERIFIED.** Create parameters include:
  - the root filesystem source (`base`, `bootstrapName` or `zfsSource`);
  - network (a switch, VNET);
  - resources: `cores` (cpuset pinning), `memory` and `resourceLimits`;
  - `allowedOptions` and `additionalOptions` (jail parameters);
  - lifecycle `hooks`;
  - `metadataMeta` / `metadataEnv` (the jail `meta`/`env` parameters, new in 15.0);
  - `fstab` and `resolvConf`.
- **VERIFIED.** Resource limits cover only **rctl `memoryuse:deny` and cpuset
  pinning** (`internal/services/jail/hardware.go`). There is no CPU-percentage,
  process-count, open-file or I/O limit.
- **VERIFIED.** There is **no disk quota at creation**. A ZFS dataset quota can
  be patched via `PATCH /api/zfs/datasets/filesystem/:guid`.
  **UNKNOWN**: whether that is safe on Sylve-managed jail datasets.
- **VERIFIED.** Jails are thick, with the root filesystem copied into their own
  dataset. A jail can be converted to a template and new jails created from
  it.
- **VERIFIED.** There is **no HTTP exec into a jail** and no cloud-init for
  jails. Exec exists only as an admin WebSocket console or the local
  `sylve jails exec` CLI run as root.
- **VERIFIED.** One lifecycle task runs per guest at a time. A concurrent
  request fails with `lifecycle_task_in_progress`. Tasks interrupted by a
  Sylve restart are marked failed, and the caller must retry
  (`internal/services/lifecycle/service.go`).
- **VERIFIED.** Per-jail stats cover CPU and memory only. There is **no
  Prometheus endpoint** and no outbound webhooks; there is an SSE event stream
  and notification channels (ntfy, SMTP, …).

### 3.4 VMs, ZFS, networking (relevant for later milestones)

- **VERIFIED.** VMs: create/start/stop/delete, **cloud-init**
  (user-data/meta-data/network-config), UEFI boot ROM, CPU pinning, VNC,
  templates, snapshots.
- **VERIFIED.** ZFS: datasets and zvols, property patching, snapshots,
  periodic snapshot policies with retention, and backup and replication jobs
  (replication is "early access", via zelta).
- **VERIFIED.** Networking: bridges ("switches"), VLANs, DHCP (dnsmasq), static
  routes, PF rules and NAT, WireGuard.
- **UNKNOWN.** How Sylve's PF management coexists with hand-written PF rules
  (anchors? overwrite?). **This is critical**, because tenant isolation
  depends on PF (see `security.md`).
- **UNKNOWN.** Whether Sylve switches can mark bridge members `private`
  (if_bridge L2 isolation between members).

### 3.5 Assessment

Sylve can do the infrastructure work, but it is not designed to be driven by
an untrusted-facing service:

1. **The credential is root-equivalent.** The process holding it must be
   isolated from the internet-facing API (see `architecture.md` §4).
2. **API breakage between releases is expected.** Pin the Sylve version.
   Keep all Sylve calls in one adapter package, with contract tests run
   against the pinned version before every upgrade.
3. **Bootstrapping PostgreSQL inside a jail needs a mechanism Sylve does not
   provide.** The proposed solution (`architecture.md` §6) is a jail template
   with PostgreSQL pre-installed and a first-boot script driven by
   non-secret jail `env` parameters. After that the platform connects to
   PostgreSQL over the network with certificate authentication. No exec or
   secret ever passes through Sylve.
4. **Some limits are missing.** These are process count, open files and I/O.
   Options: apply them through a Sylve host-side `exec.poststart` hook running
   `rctl -a` (to test), or accept memory and cpuset only for the MVP and rely on
   PostgreSQL's own limits (`max_connections`, `work_mem`,
   `statement_timeout`).
5. **Bus factor of one.** Mitigation: the platform adapter exposes
   domain-level operations (`CreateJail`, `SetQuota`, …), so it could be
   reimplemented against `jail(8)`/`zfs(8)`/`rctl(8)` through a small host
   agent if Sylve stalls or breaks. This is a documented fallback, **not
   something to build now**.

**Alternative considered:** skip Sylve for jails and drive `jail(8)`, ZFS and
`rctl` directly from a small root-privileged host agent. That gives full
control (exec, every rctl limit, quotas at creation) and removes the
pre-1.0 dependency. But it duplicates what Sylve does, contradicts the
project brief, and loses Sylve's UI for operators. **Recommendation: use
Sylve, and decide in Milestone 0 using explicit pass/fail criteria**
(`roadmap.md` M0, open question Q1).

## 4. FreeBSD jails for PostgreSQL

- **VERIFIED.** PostgreSQL on FreeBSD needs only a tiny SysV shared memory
  segment and uses POSIX semaphores. In a jail, set `sysvshm=new`.
  [PostgreSQL: kernel resources](https://www.postgresql.org/docs/current/kernel-resources.html)
- **VERIFIED.** The relevant `jail(8)` parameters
  ([jail(8)](https://man.freebsd.org/cgi/man.cgi?query=jail&sektion=8)):
  - `sysvshm`, `sysvsem` and `sysvmsg` accept `new`; `allow.sysvipc` is
    deprecated.
  - `enforce_statfs` defaults to 2 and `children.max` to 0.
  - `allow.raw_sockets` is off by default.
  - `devfs_ruleset` defaults to 0, meaning *no ruleset*, so it must be set
    explicitly.
  - `securelevel` can only be raised above the host's.
- **Proposed jail baseline (INFERRED):** VNET; `sysvshm=new`, `sysvsem=new`,
  `sysvmsg=new`; `devfs_ruleset=4`; `enforce_statfs=2`; `children.max=0`;
  `securelevel=3`; no `allow.mount*`, `allow.raw_sockets` or `allow.mlock`;
  no nullfs mounts shared between tenants.
- **VERIFIED.** FreeBSD 15.0 adds jail descriptors, the `zfs.dataset`, `meta`
  and `env` parameters, and kevent notifications for jail events.
  [15.0 release notes](https://www.freebsd.org/releases/15.0R/relnotes/)
- **VERIFIED.** Recent jail escapes:
  - [SA-26:02.jail](https://www.freebsd.org/security/advisories/FreeBSD-SA-26:02.jail.asc):
    a chroot escape through `allow.mount.nullfs`.
  - [SA-26:04.jail](https://www.freebsd.org/security/advisories/FreeBSD-SA-26:04.jail.asc):
    an escape between sibling jails by passing directory fds over a Unix socket
    across a shared nullfs mount.

  Both depend on configurations the baseline above forbids.

## 5. Resource limits (rctl and ZFS)

- **VERIFIED.** rctl needs `kern.racct.enable=1` in `/boot/loader.conf` and a
  reboot. Resources include `memoryuse`, `vmemoryuse`, `pcpu`, `maxproc`,
  `openfiles`, `readbps`/`writebps`/`readiops`/`writeiops` (these support
  `throttle`, not `deny`) and `swapuse`. The BUGS section says: *"Limiting
  memoryuse may kill the machine due to thrashing."*
  [rctl(8)](https://man.freebsd.org/cgi/man.cgi?query=rctl&sektion=8),
  [Handbook: resource limits](https://docs.freebsd.org/en/books/handbook/security/)
- **INFERRED.** `memoryuse:deny` behaves like a soft working-set limit, not a
  cgroup OOM kill. The effective memory controls for PostgreSQL are
  `shared_buffers`, `max_connections` and `work_mem`, set from the flavor,
  with rctl as the backstop.
- **VERIFIED.** ZFS `quota` includes snapshots and descendants; `refquota`
  excludes them. `refreservation` guarantees space.
  [zfsprops(7)](https://man.freebsd.org/cgi/man.cgi?query=zfsprops&sektion=7)
- **Proposed (INFERRED).** Each instance dataset gets
  `refquota = flavor storage`. Platform-managed snapshots live under a policy
  with bounded retention, so customers' quota is not consumed by snapshots
  they did not take. For PostgreSQL data: `recordsize=16k`, `compression=lz4`,
  `atime=off`.
- **INFERRED.** Cap the ARC with `vfs.zfs.arc.max` (about a fifth to a
  quarter of RAM on a small node). Otherwise it competes with jail and VM
  memory.

## 6. Networking

Full design in `architecture.md` §7. Facts:

- **VERIFIED.** FreeBSD has had in-kernel WireGuard (`wg(4)`) since 13.2.
  [wg(4)](https://man.freebsd.org/cgi/man.cgi?query=wg&sektion=4)
  The home side behind NAT must initiate the tunnel and use
  `PersistentKeepalive = 25`. [WireGuard quickstart](https://www.wireguard.com/quickstart/)
- **VERIFIED.** if_bridge `private` members do not forward to each other,
  which gives cheap L2 isolation between tenants on one bridge.
  `net.link.bridge.pfil_*` sysctls control PF on bridges.
  [if_bridge(4)](https://man.freebsd.org/cgi/man.cgi?query=if_bridge&sektion=4),
  [ifconfig(8)](https://man.freebsd.org/cgi/man.cgi?query=ifconfig&sektion=8)
- **VERIFIED.** PostgreSQL 17+ supports `sslnegotiation=direct`, which starts
  TLS immediately with ALPN `postgresql` and lets generic TLS tools route
  it. Older clients send the PostgreSQL `SSLRequest` first, which plain
  SNI proxies cannot route.
  [libpq connect](https://www.postgresql.org/docs/17/libpq-connect.html),
  [protocol flow](https://www.postgresql.org/docs/17/protocol-flow.html)
- **VERIFIED.** Traefik TCP routers understand PostgreSQL STARTTLS and can
  route on `HostSNI`.
  [Traefik TCP TLS](https://doc.traefik.io/traefik/reference/routing-configuration/tcp/tls/)
  CVE-2026-25949 (a Postgres STARTTLS readTimeout bypass) is fixed in 3.6.8, so
  require ≥ 3.6.8.
- **UNKNOWN.** Whether Traefik **terminates** PostgreSQL STARTTLS (versus
  passthrough only) in the way this design wants, and how it behaves with
  direct-TLS clients. Must be tested.
- **VERIFIED.** PostgreSQL has no native PROXY protocol support. Client
  source IPs are not visible behind a TCP proxy, so allowlists and rate limits
  must live on the VPS.
  [commitfest 3032](https://commitfest.postgresql.org/37/3032)
- **VERIFIED.** Cloudflare Tunnel needs `cloudflared` on the client for
  arbitrary TCP, so it is unsuitable for public PostgreSQL.
  [Cloudflare docs](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/non-http/cloudflared-authentication/arbitrary-tcp/)
- **To check per VPS provider (Q5):** how many IPv4 addresses a VPS gets,
  outbound port 25 limits, and whether the terms of service allow reselling
  or commercial use. The provider considered for the beta and its terms are
  in the operator's notes.
- **UNKNOWN.**
  - Whether the home connection is behind CGNAT.
  - The path MTU.
  - The residential ISP's terms for hosting services.
  - Whether the VPS provider's terms allow a paid gateway.
- **VERIFIED.** Wildcard certificates need the ACME DNS-01 challenge.
  [Let's Encrypt challenge types](https://letsencrypt.org/docs/challenge-types/)

## 7. Capacity model for the minimum node spec (INFERRED)

The reference node is treated as the **minimum supported node spec**
(`architecture.md` §1): VT-x with EPT and UG, a mirrored ZFS pool, and
modest RAM. Capacity is configured per node in `infrastructure_nodes`;
nothing in the design assumes one machine. The concrete numbers for the
reference node are in the operator's notes.

| Budget item | Memory |
|---|---|
| Host OS, JailDeck, ZFS metadata | ~1.5 GB |
| Operator's own jails, if any | measure in M0; subtract from tenant capacity |
| ZFS ARC (capped) | ~20–25 % of RAM |
| Platform: API, worker, platform PostgreSQL (minimum profile) | ~1 GB |
| **Available for tenants** | **the rest** |

- A small PostgreSQL instance (`shared_buffers` 64–128 MB) has a real RSS of
  about 300–400 MB. A 512 MB flavor runs at about 600–700 MB.
- Linux VMs at 1 GB cost about 1.2 GB each, so on a small node a few VMs
  consume most of the budget.
- Quota is oversubscribed by design: users each holding a 2 GB quota can
  add up to more than the node's tenant memory. That is fine as long as the
  node-level capacity check (`domain-model.md` §5) is enforced. Users may be
  refused for lack of capacity before reaching their quota. This is
  acceptable for a beta, but the founder plan values must be chosen
  knowingly (Q12).
- With HDD-backed storage, **disk IOPS will saturate before CPU**.

**Shared clusters (Q20) change the arithmetic.** The memory above is paid
once per cluster, not per customer: one `shared_buffers`, one set of
background processes, and a small PgBouncer server pool per database. An
idle database costs disk only, and an active one costs its share of the
server pool (a few backends of a few MB each plus `work_mem` per query). A
minimum-spec node can therefore hold **hundreds of small databases** instead
of tens of instances; in practice the limits become disk space, total
connections, and I/O from the busiest tenants. INFERRED; measured in M0
and the beta.

## 8. Requires testing on the target host

These become the Milestone 0 checklist (revised for JailDeck, 2026-10-19).

1. ~~The `VT-x` dmesg line shows `EPT` and `UG`.~~ Done 2026-10-20 (§2).
2. ~~Pool layout and boot device; HDD health.~~ Done 2026-10-20 (§2).
3. `kern.racct.enable=1` set in `/boot/loader.conf` (currently off) and
   active after a reboot; rctl rules (`memoryuse`, `vmemoryuse`,
   `maxproc`, `openfiles`, `pcpu`) applied from jail.conf `exec.poststart`
   and removed on stop.
4. A VNET jail defined in `jail.conf.d`, started with `service jail start`:
   - the epair is created and destroyed by the stanza;
   - the host end is a `private` member of a bridge with no physical NIC;
   - autostart at boot works with `jail.conf.d` (`jail_enable`/`jail_list`
     semantics on 15.1).
5. Root as a clone of a template's `@base` snapshot, plus a child `data`
   dataset with `refquota`, mounted inside the jail. The quota is enforced;
   snapshots of `data` don't count against `refquota`.
6. PostgreSQL in the jail with `sysvshm=new`, the parameter baseline, and no
   network egress. The bootstrap script runs via `jexec` with non-secret
   arguments.
7. Re-cloning the jail root from a newer template while keeping `data`
   (the upgrade path).
8. Static PF policy: tenant → LAN, management, host, control plane and other
   tenants all blocked. Verified with `nc` from inside a jail.
9. WireGuard to the VPS from behind the home NAT: stability, MTU, CGNAT.
10. Traefik ≥ 3.6.8 routing PostgreSQL by SNI: psql 16 (SSLRequest), psql
    17/18 (`sslnegotiation=direct`), pgx, JDBC; TLS termination vs
    passthrough; HTTP-provider config pull.
11. Worker → control-plane DB over WireGuard or the platform bridge, with PF
    allowing only that port. The control plane cannot reach the node
    (`nc` from `otter-api`).
12. Shared cluster (Q20, Q21): PgBouncer with `auth_query`, a tenant can
    reach only its own database, no escalation, prepared statements in
    transaction mode, per-database client limits (runbook F17–F22).
13. Tablespace-per-tenant storage quota: a full tenant must not stall
    checkpoints or block crash recovery
    ([poc-tablespace-quota.md](poc-tablespace-quota.md)). Decides Q22.
