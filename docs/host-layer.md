# Host Layer: Sylve, a Sylve Fork, or JailDeck?

Status: **decided 2026-10-19: option C (extend JailDeck) accepted by the
owner.** The owner also widened JailDeck's scope to bhyve and, if needed,
PF. That supersedes the "out of scope" rows in §4; see
[jaildeck-scope.md](jaildeck-scope.md).

The owner's question: Sylve and JailDeck are both, broadly, UIs over FreeBSD
CLI tools. Should Otter Labs Cloud fork one of them and repurpose it as its
infrastructure layer, rather than drive Sylve's REST API?

## 1. What the host layer must do

The host layer covers only the work that needs root on the host. Business
rules (ownership, quotas, billing, PostgreSQL setup) stay in the Otter
control plane.

| Capability | MVP | Later |
|---|---|---|
| Prepare and list templates (a ZFS dataset plus `@base` snapshot) | ✔ | |
| Create a jail from a template: dataset, `jail.conf.d` stanza, VNET epair on the tenant bridge, rctl, cpuset | ✔ | |
| Create a per-jail data dataset with `refquota` | ✔ | |
| Start, stop, restart, destroy (jail plus datasets) | ✔ | |
| Run a command inside a jail (`jexec`) | ✔ (bootstrap) | |
| Snapshot, rollback, list snapshots | ✔ | |
| Inventory with ownership tags, for reconciliation | ✔ | |
| Per-jail CPU and memory usage | ✔ | |
| bhyve VMs, cloud-init | | M9 |
| Firewall rules per tenant | Not needed in the MVP (see §5) | VMs, maybe |

## 2. The candidates, measured

### Sylve (commit `a401a68`)

- **About 286k lines of non-test Go** across 125 packages, plus about 139k
  lines of Svelte. Of that, `internal/services` is 171k (jail 16k, network
  26k, libvirt 17k, zfs 6k).
- FreeBSD logic is **mixed into the services**:
  - Each service holds `*gorm.DB` plus other services, and operations start
    by loading DB models.
  - The jail service indirectly depends on the libvirt service.
  - Globals are used throughout.
  - `pkg/` is independent of the services but holds generic helpers, not
    jail, zfs or pf wrappers.
  - (`internal/services/registry.go:168-190`,
    `internal/services/jail/jail.go:47-93`)
- **Sylve's own SQLite DB is the source of truth.** Configs and hooks are
  regenerated from the DB, so changes made by other tools get overwritten.
- **It owns `/etc/pf.conf`.** It overwrites the file, keeps a one-time
  backup, and disables pf autostart in rc.conf
  (`internal/services/network/firewall.go:2457,2586`). It also appends to
  `/etc/devfs.rules` without de-duplication.
- **CGO is required** (sysctl, ioctl, PAM, sqlite3). VMs need **libvirtd**.
  It also pulls in raft, badger, a torrent library and webauthn.
- **Churn is high.** In 6 months, 348 commits touched `internal/services`,
  about 58 a month. 123 of those touched jail, network or zfs. About 90% come
  from one author.
- Quality is good: 738 test files, defensive rollbacks, validate-then-apply
  for PF. But it is engineered for a much bigger scope than ours.

### JailDeck (`../jaildeck`)

- **About 7.4k lines of Go including tests.** It has jail list and
  start/stop/restart, auth (sessions plus named API tokens), a file-based
  audit log, and a Svelte SPA.
- Clean, domain-owned ports. `os/exec` is called in exactly one place, with
  argument arrays. There are fake adapters, tests, and recorded decisions
  (JD-001 to JD-015).
- Its design philosophy is almost exactly what a host agent should have:
  - **JD-011:** no database. The system itself is the source of truth
    (`jls`, `jail.conf.d`, `zfs get origin/creation`, user properties).
  - **JD-007:** jails are created as ZFS clones of a template's `@base`
    snapshot.
  - **JD-008:** jail creation is synchronous and fast; only template
    preparation is slow.
  - **JD-012:** it builds on native tools rather than reimplementing them.
- Its own roadmap already lists what Otter needs next: *Host Setup → Storage
  Visibility → Templates → Jail Provisioning → Metrics*.
- It does **not** have creation, ZFS, templates, VNET networking, rctl or
  exec yet. Its stated scope is "Not bhyve. Not multi-host."

## 3. Options

| | A. Sylve REST API (previous plan) | B. Fork Sylve | **C. Extend JailDeck as the host agent** | D. New Otter-specific agent |
|---|---|---|---|---|
| Code to own | ~0 (but an adapter) | ~286k lines, most to delete | ~7k now, +2–4k for the MVP | ~2–4k |
| Fits our needs | Gaps (no exec, no quota, memory-only rctl), root-equivalent API, pre-1.0 breakage | After heavy surgery | Built to fit | Built to fit |
| Coexistence | Owns pf.conf | Same unless rewritten | Touches only `jail.conf.d`, its datasets, the bridge members it adds | Same as C |
| Upstream | Pinned dependency | Diverges within weeks; merges become impossible | You are upstream | You are upstream |
| Build | n/a | CGO, libvirt, raft… | Pure Go, cross-compiles | Pure Go |
| Learning and portfolio value | Low | Low (mostly deletion) | **High: one tool used standalone and as a cloud's node agent** | Medium |
| Time to MVP | Fastest *if* the M0 gate passes | Slowest | Medium | Medium |

### Why not fork Sylve

A fork means deleting well over half the code. The jail, zfs and network
services that would remain still depend on gorm models, cluster replication
guards, and the libvirt service. With about 58 commits a month in that area,
the fork would stop tracking upstream almost immediately. You would be
maintaining a large, unfamiliar codebase whose architecture assumes things
you don't want (a SQLite source of truth, pf.conf ownership, CGO). Every
unfamiliar line you inherit is code you must understand to secure.

### Why not keep the Sylve API

The gaps found in the first round still hold:

- the credential is root-equivalent;
- there is no exec;
- there is no quota at creation;
- rctl is memory-only;
- the API breaks between releases.

Sylve also owns pf.conf, which collides with a hand-maintained isolation
policy, and that policy is the control we care about most.

### Why JailDeck rather than a new agent

A new agent would rebuild what JailDeck already has: the command runner,
auth tokens, audit log, jail.conf.d parsing, and lifecycle semantics. JailDeck
already intends to grow the exact features needed. The one risk is scope: a
general-purpose operator dashboard and a cloud node agent could pull the
design in different directions. The rules in §4 handle that.

## 4. Recommendation: C, extend JailDeck, and study Sylve for patterns

### Division of responsibility

| JailDeck (generic, root, one host) | Otter control plane (business, unprivileged) |
|---|---|
| Templates, jails, datasets, snapshots, exec, stats | Users, accounts, plans, subscriptions, quotas |
| Idempotent, **synchronous** primitives: "ensure jail X exists with spec S" | Operations queue, retries, async UX, reconciliation decisions |
| Truth = the system (`jls`, `jail.conf.d`, `zfs`) | Truth = intent (platform DB) |
| Knows nothing about PostgreSQL, customers or billing | Knows nothing about `jail.conf` syntax or `zfs` flags |

Rules to keep JailDeck honest:

1. **Nothing Otter-specific goes into JailDeck.** If a feature only makes
   sense for Otter, it belongs in the Otter worker. PostgreSQL bootstrap is
   a template plus a script driven through the generic exec endpoint.
2. **Ownership tags are generic.** Free-form ZFS user properties
   (`jaildeck:owner=otter`, `otter:instance=<uuid>`) and the jail `meta`
   parameter are passed through, never interpreted.
3. **The machine API is the same API the UI uses.** Otter authenticates with
   a named JailDeck API token, which already exists per JD-003.
4. **Every mutating endpoint is idempotent ("ensure").** That makes the
   Otter worker's retry and adopt logic trivial, and it matches JD-011's
   derived-state philosophy.

### Changes this implies in JailDeck (proposals for the JailDeck repo, not decided here)

| Area | Proposal |
|---|---|
| Jail spec | Creation takes a structured spec: template, VNET bridge plus IP/gateway, rctl rules (memory, vmemory, maxproc, openfiles, pcpu), cpuset, jail parameters from an **allowlist**, `meta`, and a list of child datasets with properties (`refquota`, `recordsize`, mountpoint under the jail root) |
| Data layout | `containers/<name>` is the root (a clone of the template); `containers/<name>/data` is the **customer data dataset** with `refquota`. A clone's `referenced` space includes the template blocks, so the quota goes on the data dataset only. This also lets a jail's root be **replaced from a newer template while keeping the data**, which is the PostgreSQL upgrade path |
| VNET | Epair creation and naming, `private` membership on a given bridge, IP/route setup via jail.conf `exec.prestart`/`exec.poststop`. Pattern from Sylve `internal/services/jail/network.go:1259-1360` and `internal/services/network/epair.go` |
| rctl / cpuset | Apply on start, remove on stop, idempotent. Pattern from Sylve `internal/services/jail/hardware.go:72-106` |
| Exec | `POST /api/jails/{name}/exec` with an argv array (no shell), a timeout, and captured output; every call is audited. It is root-equivalent, but so is the whole API |
| Listening | Add a **unix socket** listener (file-permission access control) and/or binding to a platform-bridge IP, so the Otter worker can reach it without exposing TCP on the LAN |
| devfs | A static, pre-installed ruleset referenced by number. Never append to `/etc/devfs.rules` at runtime |
| PF | **Out of scope for JailDeck in the MVP.** See §5 |
| bhyve | Out of scope (JD scope statement). Decide at Otter M9: a JailDeck `vms` domain, or `vm-bhyve` driven via exec |

Sylve is BSD-2-Clause. Any code ported verbatim keeps its copyright and SPDX
header. Re-implementing a pattern after reading it does not carry that
obligation, but crediting it in a NOTICE is good practice.

## 5. A simplification that falls out: static firewall policy

Tenant isolation does not need per-instance firewall rules:

- The tenant network is one subnet on one bridge, with `private` members.
- The policy is subnet-level: tenants → anything = block; VPS → tenants:5432
  = pass; worker → tenants:5432 = pass.

So PF stays a **hand-maintained, version-controlled host config file**,
changed rarely, reviewed carefully, and checked by the isolation test
script. No tool on the host rewrites pf.conf. This is safer than any
generated approach, and it removes the main reason the Sylve API option
collided with our design. Per-instance rules only become necessary if
customers later get custom allowlists (enforced at Traefik anyway) or VMs
with public ports.

## 6. Impact on the plan if C is accepted

- **Sylve is removed from the stack entirely.** It cannot sensibly coexist,
  because it would overwrite pf.conf. The operator UI becomes JailDeck's UI.
- `architecture.md` changes:
  - "Sylve adapter" becomes a "JailDeck client";
  - the worker holds a JailDeck token instead of a Sylve credential, with the
    same privilege-separation argument;
  - the operations, reconciliation, security and billing design is
    unchanged.
- The roadmap gains a dependency: **JailDeck milestones (Host Setup,
  Templates, Jail Provisioning plus the additions above) must land before
  Otter M3.** M0 becomes "host setup plus a JailDeck-driven spike" instead of
  "Sylve spike".
- The platform's own jails (`otter-api`, `otter-worker`, `otter-db`) are
  created with JailDeck too, which makes us JailDeck's first real user.
- Risks shift from "dependency we don't control" to "work we must do".
  Mitigations: keep the JailDeck additions small and generic, and use the
  M0 spike to drive the first JailDeck provisioning milestone end to end.
