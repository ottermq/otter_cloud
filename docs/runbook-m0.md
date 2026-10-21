# Runbook: M0 node preparation

Status: **draft for review**, 2026-10-20. Nothing here has been run yet.

Goal: prepare the reference node by hand and prove the design's host-level
assumptions (`feasibility.md` §8) **before** JailDeck or Otter code depends
on them. Everything is done with stock FreeBSD tools. The jail built in
Part E is the reference that JailDeck's J1–J5 must reproduce.

This runbook covers the **node** (parts A–H). The edge tests (WireGuard and
Traefik) are outlined in Part I. They wait on the VPS and home-connection
facts (Q5).

Conventions:

- Commands run as an unprivileged admin user on the node, with `doas` where
  root is needed.
- `#` lines in code blocks are comments. `→` describes the expected result.
- Each part ends with a **Check** block. Record the outcome (VERIFIED /
  FAILED + notes) in `feasibility.md` §8.
- Host config files written here (`loader.conf` lines, `pf.conf`, jail
  stanzas) should also be committed, later, to a host-config folder in this
  repo (to be created when this runbook is approved).
- The node's own values (pool, interface, LAN addresses) are **not** kept in
  this repo. Fill them in from the operator's private notes.

Values used throughout. Placeholders in `<angle brackets>` are per node:

| Name | Value | Notes |
|---|---|---|
| Jail pool | `<pool>` | The pool holding `<pool>/jails`; never the OS pool |
| OS pool | `<ospool>` | Snapshotted in Part A only |
| LAN interface | `<lan_if>` | Management / home LAN; never bridged |
| Release template | `<pool>/jails/templates/<release>` | e.g. `15.1-RELEASE`, with `@base` |
| Template patch level | `<pN>` | The host's current patch level, e.g. `p5` |
| Tenant bridge | `tenant0` | No physical member, ever |
| Tenant subnet (node 1) | `10.64.0.0/22` | From `architecture.md` §7 |
| Tenant gateway (host) | `10.64.0.1` | The host's address on `tenant0` |
| Reference jail | `pgref`, `10.64.0.10`, epair `epair10` | |
| Second test jail | `pgref2`, `10.64.0.11`, epair `epair11` | For tenant-to-tenant tests |
| Template | `<pool>/jails/templates/pg18@base` | Derived from `<release>@<pN>` |
| Non-Otter jails | `<own_jail>`, LAN alias `<own_jail_ip>` each | Only if the node also runs the operator's own jails (Part C) |

---

## Part A: safety net

```sh
doas zfs snapshot -r <pool>@pre-m0
doas zfs snapshot -r <ospool>@pre-m0
mkdir -p ~/m0 && cd ~/m0
for f in /etc/rc.conf /boot/loader.conf /etc/jail.conf.d/*.conf; do
  cp "$f" ~/m0/"$(basename "$f").orig"
done
```

Rollback for the whole runbook is in Part H.

---

## Part B: host baseline (one reboot)

### B1. Resource accounting and ARC cap

```sh
# rctl needs racct; it can only be enabled at boot.
doas sh -c 'echo kern.racct.enable=1 >> /boot/loader.conf'
# Cap ARC (feasibility.md §7 budget). 3 GiB shown; size it to the node's RAM.
doas sh -c 'echo vfs.zfs.arc.max=3221225472 >> /boot/loader.conf'
```

### B2. Patch the template userland

Bring the release template to the host's patch level. For a classic
(non-pkgbase) tree, use `freebsd-update -b`:

```sh
doas freebsd-update -b /usr/local/jails/templates/<release> \
  --currently-running <release>-<template pN> fetch install
/usr/local/jails/templates/<release>/bin/freebsd-version   # → <release>-<pN>
doas zfs snapshot <pool>/jails/templates/<release>@<pN>
```

`@base` stays as it is if existing jails are clones of it. New tenant
templates derive from `@<pN>`.

### B3. Reboot

```sh
doas shutdown -r now
```

**Check B:**

```sh
sysctl kern.racct.enable           # → 1
sysctl vfs.zfs.arc.max             # → the value set in B1
jls                                # → any pre-existing jails back up
```

---

## Part C: non-Otter jails off the shared host stack (R16)

Skip this part on a node that runs only Otter workloads.

**Why:** a jail with `ip4 = inherit` can send from *any* host address,
including the future `10.64.0.1`, so PF could not tell it apart from the
worker. Giving each such jail its own LAN alias fixes that: a non-VNET jail
can only use its own addresses. Dropping `allow.raw_sockets` removes
spoofing and raw ICMP. The jails keep serving the LAN as before.

This is the minimal fix. Moving them to VNET on a separate bridge is
stronger, and can come later.

For each `<own_jail>`, edit `/etc/jail.conf.d/<own_jail>.conf`:

```diff
-  allow.raw_sockets;
...
-  ip4 = inherit;
-  interface = <lan_if>;
+  ip4.addr = <own_jail_ip>;
+  interface = <lan_if>;
```

Pick addresses outside the router's DHCP range. A service that listens on
`localhost` in a non-VNET jail ends up on the jail's address anyway, so set
its listen address to the alias explicitly. For a jail that already has its
own alias, remove only `allow.raw_sockets;`.

```sh
doas service jail restart <own_jail> ...
```

**Check C:**

```sh
jls -n name ip4.addr allow.raw_sockets | tr ' ' '\n' | grep -E 'name|ip4.addr|raw'
# → each jail has exactly one ip4.addr, allow.raw_sockets=false
sockstat -4l
# → their listeners bound to their aliases instead of *:
```

Then update whatever clients used these services on the host's address.

---

## Part D: tenant bridge and static PF policy

### D1. Bridge

```sh
doas sysrc cloned_interfaces+="bridge0"
doas sysrc ifconfig_bridge0_name="tenant0"
doas sysrc ifconfig_tenant0="inet 10.64.0.1/22 up"
doas service netif cloneup
doas ifconfig bridge0 name tenant0 2>/dev/null   # only if cloneup did not rename it
doas ifconfig tenant0 inet 10.64.0.1/22 up
```

IP forwarding stays **off** (`gateway_enable` unset). Nothing is routed
through the node until WireGuard arrives in Part I.

### D2. `/etc/pf.conf`

Design rules:

- Rules use **addresses, not interfaces**. With bridges, packets are seen on
  member interfaces (`epairNa`) rather than on `tenant0`, and
  address-based rules behave the same either way.
- No `antispoof` on `tenant0`, for the same reason: it would block traffic
  arriving on the epair members.
- The policy is stateful. Replies to connections the host opens pass on
  state, before any rule is evaluated.
- The rest of the host's behaviour is unchanged (final `pass`).
  Tightening the host's own inbound policy (SSH from the LAN only, etc.)
  is a separate, later change.

```pf
# /etc/pf.conf: Otter node. Static, hand-maintained; no tool rewrites this file.

tenant_net = "10.64.0.0/22"
tenant_gw  = "10.64.0.1"
# edge_tun = "10.62.0.1"        # Part I: WireGuard tunnel address of the VPS

table <tenants> const { $tenant_net, !$tenant_gw }

set skip on lo0
set block-policy drop

# --- Tenant isolation (the policy Otter depends on) --------------------------

# Node host (worker) → tenant PostgreSQL. In M0 this is you, running psql on the host.
pass  quick inet proto tcp  from $tenant_gw to <tenants> port 5432
pass  quick inet proto icmp from $tenant_gw to <tenants> icmp-type echoreq

# Edge → tenant PostgreSQL. Enabled in Part I.
# pass quick inet proto tcp from $edge_tun to <tenants> port 5432

# Tenants never open connections: not to the host, LAN, internet, or each other.
block drop log quick from <tenants>
# Nobody else reaches tenants: LAN hosts, non-Otter jails, other tenants.
block drop log quick to <tenants>

# --- Everything else: unchanged host behaviour for now ------------------------
pass all
```

### D3. Load PF with a dead-man switch

Over SSH, a bad ruleset can lock you out. Arm an automatic disable first:

```sh
doas sysrc pf_enable=YES pflog_enable=YES
doas pfctl -nf /etc/pf.conf                   # syntax check → no output
doas kldload pf pflog 2>/dev/null
doas sh -c '(sleep 120; pfctl -d) >/dev/null 2>&1 &'   # auto-disable in 2 min
doas pfctl -f /etc/pf.conf -e
# Still connected? Open a NEW ssh session to confirm, then cancel the switch:
doas pkill -f 'sleep 120'
doas service pflog start
```

**Check D:**

```sh
ifconfig tenant0                     # → inet 10.64.0.1/22, no members
sysctl net.inet.ip.forwarding        # → 0
doas pfctl -sr                       # → rules as above
doas pfctl -si | head -3             # → Status: Enabled
```

---

## Part E: the reference PostgreSQL jail

### E1. Template `pg18`

Tenant jails have no egress, so packages are installed into the template
from the host:

```sh
doas zfs clone <pool>/jails/templates/<release>@<pN> <pool>/jails/templates/pg18
T=/usr/local/jails/templates/pg18
doas cp /etc/resolv.conf $T/etc/             # for pkg only; removed below
doas pkg -c $T install -y postgresql18-server postgresql18-contrib
doas rm $T/etc/resolv.conf
doas tee $T/etc/rc.conf >/dev/null <<'EOF'
postgresql_enable="YES"
postgresql_data="/var/db/postgres/data18"
sendmail_enable="NONE"
syslogd_flags="-ss"
cron_enable="NO"
clear_tmp_enable="YES"
EOF
doas zfs snapshot <pool>/jails/templates/pg18@base
```

`pkg -c` chroots into the template, so it uses the template's ABI. The
`otter-pg-bootstrap` script and the admin CA certificate are M3 work. In M0
the bootstrap steps are done by hand (E4).

### E2. Datasets

```sh
J=<pool>/jails/containers
doas zfs clone -o jaildeck:owner=otter-m0 <pool>/jails/templates/pg18@base $J/pgref
doas zfs create \
  -o refquota=5G -o recordsize=16k -o atime=off -o compression=lz4 \
  -o mountpoint=/usr/local/jails/containers/pgref/var/db/postgres \
  $J/pgref/data
```

### E3. `/etc/jail.conf.d/pgref.conf`

```
pgref {
  $id     = "10";
  $ip     = "10.64.0.${id}/22";
  $gw     = "10.64.0.1";
  $bridge = "tenant0";
  $epair  = "epair${id}";

  host.hostname = "${name}";
  path = "/usr/local/jails/containers/${name}";
  exec.consolelog = "/var/log/jail_console_${name}.log";
  exec.clean;
  mount.devfs;
  devfs_ruleset = 4;          # devfsrules_jail: no bpf, no pf device
  securelevel = 3;
  enforce_statfs = 2;
  children.max = 0;
  sysvshm = new;
  sysvsem = new;
  sysvmsg = new;
  # Absent on purpose: allow.raw_sockets, allow.mount*, allow.chflags.

  vnet;
  vnet.interface = "${epair}b";
  exec.prestart  = "/sbin/ifconfig ${epair} create up";
  exec.prestart += "/sbin/ifconfig ${epair}a up descr jail:${name}";
  exec.prestart += "/sbin/ifconfig ${bridge} addm ${epair}a private ${epair}a";
  exec.start     = "/sbin/ifconfig ${epair}b ${ip} up";
  exec.start    += "/sbin/route add default ${gw}";
  exec.start    += "/bin/sh /etc/rc";
  exec.stop      = "/bin/sh /etc/rc.shutdown";

  # pg-small: 512 MB, 1 vCPU (Q12). vmemoryuse is set generously: PostgreSQL maps more than it touches.
  exec.poststart  = "/usr/bin/rctl -a jail:${name}:memoryuse:deny=512m";
  exec.poststart += "/usr/bin/rctl -a jail:${name}:vmemoryuse:deny=2g";
  exec.poststart += "/usr/bin/rctl -a jail:${name}:pcpu:deny=100";
  exec.poststart += "/usr/bin/rctl -a jail:${name}:maxproc:deny=200";
  exec.poststart += "/usr/bin/rctl -a jail:${name}:openfiles:deny=4096";
  exec.poststop   = "/usr/bin/rctl -r jail:${name}";
  exec.poststop  += "/sbin/ifconfig ${bridge} deletem ${epair}a";
  exec.poststop  += "/sbin/ifconfig ${epair}a destroy";
}
```

`pgref2` is the same stanza with `$id = "11"`, its own dataset pair, and
the jail name changed.

### E4. First start and bootstrap (by hand; becomes `otter-pg-bootstrap` in M3)

```sh
doas service jail start pgref
doas jexec pgref chown postgres:postgres /var/db/postgres
doas jexec pgref su -m postgres -c \
  'initdb -D /var/db/postgres/data18 -U postgres --auth-local=peer --auth-host=reject -E UTF8 --locale=C.UTF-8'
# Self-signed cert for the edge→jail hop (M0 only)
doas jexec pgref su -m postgres -c 'cd /var/db/postgres/data18 && \
  openssl req -new -x509 -days 30 -nodes -subj /CN=pgref -keyout server.key -out server.crt && chmod 600 server.key'
doas tee -a /usr/local/jails/containers/pgref/var/db/postgres/data18/postgresql.conf >/dev/null <<'EOF'
listen_addresses = '*'
ssl = on
shared_buffers = 128MB
max_connections = 20
EOF
doas tee /usr/local/jails/containers/pgref/var/db/postgres/data18/pg_hba.conf >/dev/null <<'EOF'
local   all  all                    peer
hostssl all  m0test  10.64.0.1/32  scram-sha-256
host    all  all     0.0.0.0/0     reject
EOF
doas jexec pgref service postgresql start
doas jexec pgref su -m postgres -c "psql -c \"CREATE ROLE m0test LOGIN PASSWORD 'change-me'\" -c 'CREATE DATABASE m0test OWNER m0test'"
```

**Check E:**

```sh
psql "host=10.64.0.10 dbname=m0test user=m0test sslmode=require" -c 'select version()'
# → PostgreSQL 18.x (needs a psql client on the host: pkg install postgresql18-client)
doas rctl -h -u jail:pgref | grep -E 'memoryuse|maxproc'   # → usage reported
doas rctl | grep pgref                                     # → 5 rules
zfs get -H -o property,value refquota,recordsize,mountpoint <pool>/jails/containers/pgref/data
```

---

## Part F: verification (maps to `feasibility.md` §8)

Run each, write the result into §8.

| # | Test | Command | Expect |
|---|---|---|---|
| F1 (§8.3) | Memory limit enforced | `doas jexec pgref dd if=/dev/zero of=/dev/null bs=1500m count=1` | Fails (cannot allocate) |
| F2 (§8.3) | Limits removed on stop | `doas service jail stop pgref; doas rctl \| grep pgref` | No rules left |
| F3 (§8.4) | Epair lifecycle | `ifconfig -l` before and after start/stop | `epair10a/b` appear, then vanish |
| F4 (§8.4) | Private membership | `ifconfig tenant0 \| grep -A1 member` | `epair10a`, flags include `PRIVATE` |
| F5 (§8.5) | Quota enforced | `doas jexec pgref dd if=/dev/urandom of=/var/db/postgres/fill bs=1m count=6000` | "Disc quota exceeded" near 5G. Then `rm` the file |
| F6 (§8.5) | Snapshots outside `refquota` | Write 2G, `zfs snapshot …/data@t`, delete the file, write 4G | Succeeds; `usedbysnapshots` ≈ 2G |
| F7 (§8.8) | No egress | `doas jexec pgref nc -z -w3 1.1.1.1 443; echo $?` | Non-zero |
| F8 (§8.8) | No reaching the host | `doas jexec pgref nc -z -w3 10.64.0.1 22` (also `514/udp` with `-u`) | Fails |
| F9 (§8.8) | No reaching the LAN | `doas jexec pgref nc -z -w3 <lan_gateway> 80` | Fails |
| F10 (§8.8) | No DNS | `doas jexec pgref host freebsd.org` | Fails |
| F11 (§8.8) | Tenant ↛ tenant | Start `pgref2`; `doas jexec pgref2 nc -z -w3 10.64.0.10 5432` | Fails |
| F12 (R16) | Non-Otter jail ↛ tenant | `doas jexec <own_jail> nc -z -w3 10.64.0.10 5432` (if Part C applies) | Fails |
| F13 | Host → tenant works | `nc -z 10.64.0.10 5432` on the host | Succeeds |
| F14 | Blocks are logged | `doas tcpdump -n -e -ttt -r /var/log/pflog \| tail` | F7–F12 attempts listed |
| F15 (§8.4) | Autostart | `doas sysrc jail_list+=pgref`, reboot | `pgref` up, rctl rules present, PF enabled |
| F16 (§8.6) | Bootstrap needs no secrets on the command line | Review E4: only the M0 test password, which M3 replaces with a SCRAM verifier | Noted |

If the node also runs non-Otter jails, record their memory use
(`rctl -h -u jail:<name> | grep memoryuse`) and subtract it from the node's
tenant capacity (`feasibility.md` §7).

---

## Part G: root replacement keeping `data` (the upgrade path, §8.7)

`data` is a **child** of the root clone, so destroying the root would
destroy it. It must be moved aside first. This is the same move that
deletion with a final snapshot needs (`architecture.md` §6), and it is
JailDeck's dataset-rename primitive (Storage Writes).

```sh
J=<pool>/jails/containers
doas service jail stop pgref
doas zfs create -o mountpoint=none -o canmount=off <pool>/jails/retained
doas zfs snapshot $J/pgref/data@pre-reclone
doas zfs rename -u $J/pgref/data <pool>/jails/retained/pgref-data
doas zfs destroy $J/pgref                       # the old root only
doas zfs clone -o jaildeck:owner=otter-m0 <pool>/jails/templates/pg18@base $J/pgref
doas zfs rename -u <pool>/jails/retained/pgref-data $J/pgref/data
doas zfs set mountpoint=/usr/local/jails/containers/pgref/var/db/postgres $J/pgref/data
doas zfs mount $J/pgref/data
doas service jail start pgref
```

**Check G:** `psql` from Check E still works, and `m0test` still exists.
For a real upgrade, clone from a newer `pg18@<date>` snapshot instead of
`@base`.

If a replication tool (e.g. zrepl) snapshots and replicates `<pool>`, its
snapshots of `pgref/data` move with the rename. Confirm in its logs that
replication copes with the rename. If it doesn't, the replication filter or
the procedure needs adjusting before M3.

---

## Part H: rollback

To undo everything from this runbook:

```sh
doas service jail stop pgref pgref2
doas zfs destroy -r <pool>/jails/containers/pgref
doas zfs destroy -r <pool>/jails/containers/pgref2
doas zfs destroy -r <pool>/jails/templates/pg18
doas zfs destroy -r <pool>/jails/retained
doas rm /etc/jail.conf.d/pgref.conf /etc/jail.conf.d/pgref2.conf
doas pfctl -d; doas sysrc -x pf_enable pflog_enable
doas sysrc cloned_interfaces-="bridge0"; doas sysrc -x ifconfig_bridge0_name ifconfig_tenant0
doas ifconfig tenant0 destroy
doas sysrc jail_list-="pgref pgref2"
# Non-Otter jails: restore the .orig stanzas from ~/m0 and restart them.
# loader.conf: remove the two lines (racct can stay; it is harmless).
```

Destroying `<pool>@pre-m0` and `<ospool>@pre-m0` once M0 is accepted is a
separate, explicit step.

---

## Part I: edge tests (outline; blocked on Q5 facts)

Needs: the VPS plan (RAM, OS), whether the home connection is behind
CGNAT, and the upload bandwidth.

1. **WireGuard** (`feasibility.md` §8.9). VPS hub `10.62.0.1`, node peer
   with `AllowedIPs = 10.64.0.0/22` on the VPS side. The node keeps the
   tunnel up with `PersistentKeepalive = 25` (works behind NAT or CGNAT).
   Then on the node:
   - `gateway_enable=YES`;
   - uncomment `edge_tun` and its pass rule in `pf.conf`.

   Measure the path MTU (`ping -D -s`) and set the `wg0` MTU accordingly.
2. **Traefik ≥ 3.6.8** (§8.10). A TCP router with `HostSNI(pgref.db.otter-labs.com)`
   and TLS termination using a Let's Encrypt wildcard (DNS-01 through the
   DNS provider's API).
   Test matrix: psql 16 (SSLRequest), psql 17/18 with
   `sslnegotiation=direct`, pgx, and JDBC, each with `sslmode=verify-full`.
   Also test re-encryption to the jail's self-signed certificate.
3. **Control-plane path** (§8.11). Comes with the `otter-db` jail in M1/M3:
   - the worker reaches the DB port only;
   - `nc` from `otter-api` to the node host and to `tenant0` fails.

---

## Notes

- **Classic vs pkgbase templates.** A host installed with pkgbase can still
  run classic (non-pkgbase) release templates; `freebsd-update -b` patches
  them. A pkgbase template is a JailDeck Templates-milestone decision.
- **Host syslogd.** If syslogd listens on `*:514/udp`, PF still blocks
  tenants from reaching it, but `syslogd_flags="-ss"` on the host is worth
  setting anyway.
- **Re-cloning the root needs a rename** (Part G), which confirms the
  JailDeck dataset-rename primitive is needed for upgrades as well as for
  deletion.
- **Link speed.** Check that the node's LAN link negotiated gigabit; a
  100 Mb/s link caps WireGuard and backup throughput at about 11 MB/s.
