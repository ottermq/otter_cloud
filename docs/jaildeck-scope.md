# JailDeck Scope Review (for Otter Labs Cloud)

Status: **applied**, 2026-10-21, to JailDeck's `docs/SPEC.md`,
`docs/DECISIONS.md`, `docs/ROADMAP.md` and `docs/ARCHITECTURE.md`. Merged into
`dev` as Codeberg PR #74, shortly before the repo moved to
[github.com/ottermq/jaildeck](https://github.com/ottermq/jaildeck). Written 2026-10-19 as a proposal; kept here as the
record of why. JailDeck's docs are now authoritative where the two differ.

Differences from this proposal, made when applying it:

- JailDeck milestones are named, not numbered. §6's storage milestone is
  split into *Storage Visibility* (existing) and *Storage Writes* (new).
- Access Control and Web UI shipped in JailDeck v0.5.0 and v0.6.0, so §6's
  "Web UI (in progress)" is done.
- JD-016, JD-017, JD-018 and JD-020 are recorded as Locked. JD-019 has its
  scope locked and its mechanism open until the spike.
- Host Setup also reports `exec.timeout` (JD-014) and whether PF is loaded.
- A `GET /api/host/readiness` endpoint is sketched for the Host Setup report.

Reviewed: JailDeck `SPEC.md`, `ARCHITECTURE.md`, `DECISIONS.md` (JD-001 to
JD-015), `ROADMAP.md`, `UI.md` intro, and the jails adapter code.

## 1. Summary

JailDeck gains **a second consumer** (automation via its API, Otter being the
first) and **two new domains** (bhyve VMs and PF). It stays what makes it
good:

| Stays | Changes |
|---|---|
| Single host per JailDeck instance (multi-host is Otter's job) | Adds `vms` (bhyve) and `firewall` (PF) domains |
| Single operator credential + named API tokens (multi-tenancy is Otter's job) | Automation is a first-class API consumer: idempotent specs, correlation ids, unix socket |
| No database; the system is the source of truth (JD-011) | VNET jails, rctl/cpuset limits and ZFS write operations move from "deferred" to "in scope" |
| Native tools, thin layer (JD-012), readability (JD-013), bounded commands (JD-014) | Run-a-command-in-jail exec becomes an API primitive |
| Pure Go, single binary, Svelte SPA | A Prometheus `/metrics` endpoint fulfils the Metrics milestone without a DB |

**The boundary to protect:** JailDeck exposes *generic* host primitives.
Anything that only makes sense for a cloud platform (customers, quotas,
billing, PostgreSQL setup) stays in Otter. Test for any feature: "Would a
single operator running JailDeck on their own box ever want this?" If no, it
belongs in Otter.

## 2. Philosophy check: do the new consumers violate JailDeck's principles?

| JailDeck principle | Tension | Resolution |
|---|---|---|
| "System state is the source of truth; avoid an internal desired-state model" (SPEC) | Otter has a desired-state model | It lives in **Otter**. JailDeck remains a desired-state-free executor and reporter. The split actually *protects* the principle |
| "No unsafe magic" / destructive confirmation | An automated client cannot click a dialog | API-level confirmation: destructive calls require `confirm=<exact name>`, as ARCHITECTURE already anticipates. It applies equally to UI and automation |
| JD-008: jail creation is synchronous | Otter wants async | Otter provides the async layer. JailDeck staying synchronous and idempotent is *better* for automation |
| "Not a multi-host / multi-tenant / cloud platform" (SPEC) | Otter is | Still true of JailDeck. Reword to say JailDeck can be *driven by* such a platform through its API |
| "Not a virtualization/bhyve platform" (SPEC, ROADMAP "Not planned") | The owner wants bhyve | **Scope change**: new JD-019 below |
| "Automatic edits to critical configuration files beyond what jail creation needs" (deferred) | VNET needs epairs and bridge membership; PF | Bridge creation stays host setup. JailDeck only adds and removes *its own* epairs. **pf.conf is never written** (JD-020) |

## 3. Proposed changes by area

### 3.1 Automation API contract (new JD-016)

- **Idempotent create:** `PUT /api/jails/{name}` with a full spec. If absent,
  create it. If present and matching, `200` with no change. If present and
  different, `409` with a diff. `POST /api/jails` can remain for the UI.
- **Destroy:** `DELETE /api/jails/{name}?confirm={name}`. Destroys the jail's
  datasets and config, and refuses if there are clones or holds.
- **Correlation:** an `X-Request-Id` header is recorded in the audit entry,
  so Otter's operation id links both audit logs.
- **Listener:** an optional **unix socket** (`JAILDECK_SOCKET`, mode 0660,
  group-restricted) next to, or instead of, TCP. Local automation needs no
  TCP port and no TLS, and file permissions are the first access check (the
  token is still required).
- **Change policy:** pre-1.0 breaking changes are allowed but recorded in
  CHANGELOG under "API". Otter pins a JailDeck version per node and runs
  contract tests on upgrade.

### 3.2 Jail spec: VNET, limits, datasets, metadata (new JD-017, JD-018)

A jail spec, extending JD-007:

```json
{
  "template": "pg18",
  "hostname": "otc-pg-ab12",
  "network": { "mode": "vnet", "bridge": "bridge-tenant", "private": true,
               "ip4": "10.64.0.23/22", "gateway": "10.64.0.1" },
  "limits":  { "memoryuse": "640M", "vmemoryuse": "1G", "maxproc": 100,
               "openfiles": 2048, "pcpu": 100, "cpuset": "0-3" },
  "params":  { "sysvshm": "new", "sysvsem": "new", "sysvmsg": "new",
               "devfs_ruleset": 4, "securelevel": 3, "enforce_statfs": 2 },
  "datasets": [ { "name": "data", "mountpoint": "/var/db/postgres",
                  "properties": { "refquota": "5G", "recordsize": "16k",
                                  "compression": "lz4", "atime": "off" } } ],
  "properties": { "otter:instance": "4f0c…", "otter:node": "node-01" },
  "autostart": true
}
```

- **VNET:**
  - epair created and destroyed in the generated jail.conf stanza
    (`exec.prestart` / `exec.poststop`);
  - host end added to the bridge as a `private` member;
  - IP and route set inside the jail.

  Pattern reference: Sylve `internal/services/jail/network.go:1259-1360`,
  `internal/services/network/epair.go:35-115` (BSD-2-Clause; keep headers if
  copying verbatim). The bridge itself is created by **Host Setup** or the
  operator, never by jail creation.
- **Limits:** rctl rules applied on start and removed on stop (`exec.poststart`
  / `exec.prestop` or the adapter), plus cpuset. Requires
  `kern.racct.enable=1`, which Host Setup checks and reports. Pattern:
  Sylve `internal/services/jail/hardware.go:72-106`.
- **params:** only from an **allowlist** of jail(8) parameters. Never
  `allow.mount*`, `allow.raw_sockets`, `children.max>0`, or `vnet` with
  `inherit`. The allowlist is the security boundary for the API.
- **Child datasets:** created under the jail dataset with properties. Quotas
  go here, because a clone's `referenced` space includes template blocks.
  This also enables **"re-clone root from a newer template, keep data"**,
  which is a generally useful JailDeck feature and Otter's upgrade path.
- **ZFS user properties:** free-form, passed through, returned in listings,
  never interpreted. They are derived state, so JD-011 holds.
- **Autostart:** maintained in rc.conf `jail_list` via `sysrc`. **To
  verify:** how `jail_enable`/`jail_list` interact with `jail.conf.d` on
  15.1.
- **devfs:** reference a pre-installed ruleset number. Never append to
  `/etc/devfs.rules` at runtime (Sylve does, without de-duplication).

### 3.3 Exec (amends the SPEC's deferred "in-jail service management")

- `POST /api/jails/{name}/exec` with `{ "argv": [...], "timeout": "60s",
  "env": {…} }`. It returns stdout, stderr and exit code.
- The argv array goes through the existing `CommandRunner` as
  `jexec <name> argv...`. There is no shell, the call is bounded by JD-014,
  and it is audited.
- This is root-in-jail, which is no more than the API already implies
  (JD-001). It is the "hand off to in-jail provisioning (`jexec` + `pkg`)"
  already described in JD-007, made callable.

### 3.4 PF (new JD-020): visibility first, owned anchors later, pf.conf never

| Phase | Capability | Commands | Needed by Otter |
|---|---|---|---|
| **1. Visibility** (read-only) | Show the loaded ruleset, anchors, tables, state count, per-rule counters, `pf` enabled/disabled, last pflog lines | `pfctl -s rules -v`, `-s Anchors`, `-s Tables`, `-s info`, `-t <t> -T show` | Operator insight; the isolation check script can call it |
| **2. Tables** | Add or remove addresses in **named tables** the operator declared in pf.conf | `pfctl -t <table> -T add/delete/show` | VMs (e.g. per-tenant egress allowlists), if ever |
| **3. Owned anchors** | Load rules into `jaildeck/*` anchors that pf.conf includes once; validate with `pfctl -nf` before loading, roll back on failure | `pfctl -a jaildeck/<n> -nf … && pfctl -a jaildeck/<n> -f …` | Only if per-VM port rules appear |
| Never | Write `/etc/pf.conf`, toggle `pf_enable` | — | — |

Rationale: the host's firewall policy is the most security-critical file
on the box. The operator keeps authorship of pf.conf. JailDeck may only
operate inside boxes the operator explicitly opened (tables, anchors).
Sylve overwrites pf.conf; this is the deliberate opposite. Its
validate-then-rollback idea (`firewall_apply_rollback.go`) is worth
adopting for phase 3.

**Otter needs only phase 1 for the MVP.** Its tenant policy is static
(`architecture.md` §7).

### 3.5 bhyve VMs (new JD-019): a `vms` domain

The needed scope mirrors jails:

- list and inspect;
- create from an image template;
- start, stop, restart, destroy;
- console access (serial over the API, later);
- cloud-init;
- zvol sizing;
- VNET-equivalent tap on a bridge;
- CPU and memory sizing and cpuset.

The key decision is the driving mechanism. JD-005 chose `service jail`
because "it's the standard administrative interface". bhyve has no
equivalent in base. Options:

| Option | Fit with JailDeck principles | Pros | Cons |
|---|---|---|---|
| **A. `vm-bhyve`** (`sysutils/vm-bhyve`), driven like `service jail` | JD-005/JD-012: integrate an established admin tool; its per-VM config files and datasets are derivable state (JD-011) | Mature; handles supervision, taps, UEFI, ISO/cloud images; cloud-init support (INFERRED, verify) | A shell dependency outside base; its maintenance cadence must be checked (UNKNOWN); less learning about bhyve internals |
| B. Direct `bhyve(8)` + `bhyvectl` + `daemon(8)` supervision, config generated by JailDeck | JD-012: native base tools only | Full control; maximal learning; no extra dependency | JailDeck must own process supervision across its own restarts, tap lifecycle and reboot handling; more code |
| C. libvirt (Sylve's choice) | Weak: a daemon and abstraction layer foreign to FreeBSD conventions | Feature-rich | Heavy runtime dependency; requires libvirtd |

**Recommendation:** spike A and B in JailDeck's VM milestone and decide
then. Lean **A** for consistency with JD-005, unless the spike shows
cloud-init or maintenance gaps. **Reject C.** Prerequisite on any host:
`VT-x … EPT,UG` in dmesg. Cloud-init seed ISO pattern: `makefs -t cd9660
-o rockridge -o label=cidata` (Sylve `libvirt_utils.go:587-665`).

### 3.6 Metrics endpoint (amends the Metrics milestone)

- A `/metrics` endpoint in Prometheus text format: host summary plus
  per-jail CPU, memory and processes from `rctl -u jail:<name>`, falling
  back to `ps --libxo` when racct is off (Sylve uses jls + ps libxo,
  `stats.go:370-420`). Later, per-VM metrics.
- JD-011's trigger for a database ("a chart over time that survives a
  restart") is satisfied by Prometheus, so JailDeck itself stays DB-free.
- The endpoint is authenticated (token) or bound to a separate
  loopback/socket listener, operator's choice.

## 4. Decision-by-decision review

| ID | Verdict | Change |
|---|---|---|
| JD-001 Root | Keep | — |
| JD-002 Binding | Amend | Add the unix socket listener option (§3.1) |
| JD-003 Auth | Keep | Named tokens are exactly what automation needs. Add a note that a token is root-equivalent and should be held only on the same host |
| JD-004 Discovery | Amend | Listings also return ZFS user properties and child datasets |
| JD-005 Lifecycle via `service jail` | Keep | Verify behaviour with VNET stanzas and `jail.conf.d` autostart |
| JD-006 ZFS required | Keep | — |
| JD-007 Template cloning | Amend | Child datasets with properties; "re-clone root, keep data" |
| JD-008 Long-running ops | Amend | Jail operations stay synchronous; template prep and VM image import get the progress mechanism. Note that automation callers handle async themselves |
| JD-009 Svelte SPA | Keep | — |
| JD-010 Domain packages | Keep | New domains: `vms`, `firewall`; `storage` gains writes |
| JD-011 No DB | Keep | Clarify: user properties are derived state; Prometheus holds metric history |
| JD-012 Native integration | Amend | Add `pfctl`, `rctl`, `cpuset`, `ifconfig` (epair/bridge), and bhyve tooling per JD-019 |
| JD-013 Readability | Keep | — |
| JD-014 Timeouts | Keep | Exec endpoint timeouts are caller-supplied but capped |
| JD-015 Frontend | Keep | — |
| **JD-016** (new) | Add | Automation API contract (§3.1) |
| **JD-017** (new) | Add | VNET networking model (§3.2) |
| **JD-018** (new) | Add | Resource limits and jail parameter allowlist (§3.2) |
| **JD-019** (new) | Add | bhyve scope and mechanism (§3.5), decision pending spike |
| **JD-020** (new) | Add | PF scope: visibility → tables → owned anchors; never pf.conf (§3.4) |

## 5. SPEC.md edits

- **One-liner:** "…web dashboard and API for inspecting, operating, and
  creating FreeBSD jails **and bhyve virtual machines** through native
  system tools, usable by an operator or by automation."
- **"Origin and inspiration":** replace "bhyve/VM management … is
  intentionally not a direction" with the new scope. Keep the "not matching
  Sylve feature-for-feature" stance.
- **"What Jail Deck is":** add "a host agent that higher-level tooling (e.g.
  Otter Labs Cloud) can drive through its API".
- **"What Jail Deck is not":**
  - Remove "a virtualization/bhyve platform".
  - Keep "multi-host or multi-tenant platform" and "cloud management
    platform", adding "(but it can be driven by one)".
  - Add "a firewall policy manager: it never writes pf.conf".
- **"Explicitly deferred":** remove "VNET jails". Keep "thin jails" (shared
  nullfs base; clones already give cheap creation, and shared nullfs mounts
  are implicated in SA-26:04).
- **"Core domains":** add Virtual machines and Firewall (read-only first).

## 6. ROADMAP.md: proposed milestone order

Otter dependencies are marked ★.

| # | Milestone | Content | Otter needs it for |
|---|---|---|---|
| 1 | Web UI (in progress) | — | — |
| 2 | Host Setup | Pool and layout discovery; **plus**: check racct, VT-x/UG report, tenant bridge presence, devfs ruleset presence (report, don't create) | ★ M0 |
| 3 | Storage Visibility + Writes | Datasets, snapshots, **user properties, child datasets, snapshot/rollback/destroy, rename (move a child dataset out of a jail before the jail is destroyed)** | ★ M3 |
| 4 | Templates | As planned. Plus "derived template" (template + packages via exec, re-snapshotted) for the PostgreSQL template | ★ M0/M3 |
| 5 | Jail Provisioning | As planned, **plus** VNET, limits, param allowlist, child datasets, idempotent `PUT`, confirm-destroy, autostart | ★ M3 (critical path) |
| 6 | Exec + Automation contract | Exec endpoint, unix socket, `X-Request-Id`, API changelog policy | ★ M3 |
| 7 | Metrics | `/metrics` endpoint (rctl), host summary in the UI | ★ M7 |
| 8 | Firewall visibility | PF phase 1 | Operator / M7 checks |
| 9 | Packaging | As planned | ★ before beta (rc.d service on nodes) |
| 10 | Virtual machines | JD-019 spike, then the `vms` domain | ★ Otter M9 |
| later | Firewall tables and anchors | PF phases 2–3, only on demand | Maybe VMs |

The "Not planned" list loses "bhyve/VM management". "Remote cloud control
plane" stays (JailDeck is not one).

## 7. Open questions for JailDeck

1. bhyve mechanism: vm-bhyve vs direct bhyve (spike in milestone 10).
2. Autostart semantics with `jail.conf.d` on FreeBSD 15.1.
3. Should rctl rules be applied by jail.conf `exec.*` hooks (they survive a
   JailDeck outage and work with `service jail start` from a shell) or by
   the adapter? **Lean: hooks**, so a jail behaves the same however it is
   started.
4. Exec timeout cap and output size cap.
5. Does the `/metrics` listener share auth with the API, or bind separately?
6. Does the template domain own "derived templates" (base + packages), or is
   that just exec + snapshot from the caller's point of view?
