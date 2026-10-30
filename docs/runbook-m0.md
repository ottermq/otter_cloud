# Runbook: M0 node preparation

Status: drafted 2026-10-20. Parts D and E were run on the reference node on
2026-10-29 (#16, #17, #18, #20) and are written as run; the rest is still a
draft.

Goal: prepare the reference node and prove the design's host-level
assumptions (`feasibility.md` §8) **before** Otter code depends on them.
Host settings are made with stock FreeBSD tools; the template, the
network and the jail are made through JailDeck's API, and everything
inside the jail through JailDeck exec: the calls the worker makes later.
The jail built in Part E is the **shared PostgreSQL cluster** (Q20):
PostgreSQL plus PgBouncer in one jail. It is the node's first real
cluster; its test tenants are dropped after Part F. The storage quota POC
([poc-tablespace-quota.md](poc-tablespace-quota.md)) ran on a scratch
cluster before it.

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
| Tenant bridge | `jdn0`, JailDeck network `clusters` | No physical member, ever; JailDeck's (JD-043) |
| Tenant subnet (node 1) | `10.64.0.0/24` | The first `/24` of the node's `10.64.0.0/22` (`architecture.md` §7) |
| Tenant gateway (host) | `10.64.0.1` | The host's address on `jdn0` |
| Cluster jail | `otc-pgc1`, `10.64.0.10` | PostgreSQL `:5432` (admin only), PgBouncer `:6432` |
| Test tenants | databases and roles `otc_000000000001`, `otc_000000000002` | Named as the worker names them; dropped after Part F |
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

JailDeck makes the bridge, as a network of its own (JD-043): a bridge
`jdn<i>` on the lowest free `/24` of its range, the host on `.1`, and its
rules in JailDeck's PF anchor. Set the range to the node's `/22` first,
then create the network with a token:

```sh
doas cp -p /usr/local/etc/jaildeck/jaildeck.env /usr/local/etc/jaildeck/jaildeck.env.orig
echo 'JAILDECK_NETWORK_RANGE=10.64.0.0/22' | doas tee -a /usr/local/etc/jaildeck/jaildeck.env
doas service jaildeck restart
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"clusters","tenant":"otter"}' http://127.0.0.1:8888/api/networks
# → {"name":"clusters","bridge":"jdn0","subnet":"10.64.0.0/24","gateway":"10.64.0.1",...}
```

The network's own rules let its members reach their gateway and the
outside; Otter's `quick` rules in D2 come first and allow neither.
JailDeck turns IP forwarding on for its VM networks (`gateway_enable`);
the policy, not the absence of routing, keeps tenants in.

Set the shared `exec.timeout` too (JD-014: above `rcshutdown_timeout`, at
most 140 s); the stanzas in `/etc/jail.conf.d` are read through it:

```sh
printf 'exec.timeout = 120;\n.include "/etc/jail.conf.d/*.conf";\n' | doas tee /etc/jail.conf
jail -f /etc/jail.conf -e '|'      # → every jail, with exec.timeout=120
```

### D2. `/etc/pf.conf`

Design rules:

- Rules use **addresses, not interfaces**. With bridges, packets are seen on
  member interfaces (`epairNa`) rather than on the bridge, and
  address-based rules behave the same either way.
- No `antispoof` on the bridge, for the same reason: it would block traffic
  arriving on the epair members.
- The policy is stateful. Replies to connections the host opens pass on
  state, before any rule is evaluated.
- The rest of the host's behaviour is unchanged (final `pass`).
  Tightening the host's own inbound policy (SSH from the LAN only, etc.)
  is a separate, later change.

The policy is [`deploy/node/pf.conf`](../deploy/node/pf.conf); copy it to
`/etc/pf.conf`. Besides the tenant rules above, it includes JailDeck's
anchors once, as JailDeck asks of the operator (JD-040): `nat-anchor` and
`anchor "jaildeck/*"`, and `load anchor "jaildeck/vmnet"` from JailDeck's
own file, which carries the NAT of its default VM network and the rules
between its tenant networks (JD-043). JailDeck rewrites that file; it never
writes `pf.conf` or enables PF (JD-020). Otter's `quick` rules come before
its anchor, so nothing JailDeck loads can open a path to a tenant.

### D3. Load PF with a dead-man switch

Over SSH, a bad ruleset can lock you out. Arm an automatic disable first.
`daemon -f` detaches it from the session: without `-f` the switch holds
the SSH session's output open, and the command hangs until it fires. Its
pidfile cancels it; `pkill -f` would match the SSH command line itself.

```sh
doas install -o root -g wheel -m 0644 pf.conf /etc/pf.conf
doas sysrc pf_enable=YES pflog_enable=YES
doas kldload -n pf pflog
doas pfctl -nf /etc/pf.conf                   # syntax check → no output
doas daemon -f -p /var/run/pf-deadman.pid sh -c 'sleep 120; pfctl -d'
doas pfctl -f /etc/pf.conf -e
# Still connected? Open a NEW ssh session to confirm, then cancel the switch:
doas kill "$(doas cat /var/run/pf-deadman.pid)"
doas service pflog start
```

**Check D:**

```sh
ifconfig jdn0                        # → inet 10.64.0.1/24, no members
doas pfctl -sr                       # → rules as above
doas pfctl -s nat                    # → nat-anchor "jaildeck/*" all
doas pfctl -si | head -3             # → Status: Enabled
```

---

## Part E: the shared cluster jail

Built through JailDeck's API, with a token (`$TOKEN`), and set up by
[`otter-pg-bootstrap`](../deploy/cluster/otter-pg-bootstrap) run through
JailDeck exec: the steps the worker automates later. Below,
`J='curl -s -H "Authorization: Bearer $TOKEN" -H Content-Type:application/json'`
and `API=http://127.0.0.1:8888/api`.

### E1. Template `pg18`

Tenant jails have no egress, so packages come only from the template.
JailDeck derives it from the release template with `pkg -c` and snapshots
it as `@base` (JD-027):

```sh
$J -X PUT -d '{"from":"<release>","packages":["postgresql18-server","postgresql18-contrib","pgbouncer"]}' \
  $API/templates/pg18
# → {"name":"pg18","snapshot":"<pool>/jails/templates/pg18@base","state":"ready",...}
```

The host needs `postgresql18-client` too: the worker's final dumps run
`pg_dump` on the host (Q26), and the checks below run `psql` there.

```sh
doas pkg install -y postgresql18-client
```

### E2. The cluster jail

One spec: VNET on the `clusters` network as a private member, rctl limits,
the runbook's hardened parameters, and the `data` dataset carrying the
cluster backstop quota.

```sh
$J -X PUT $API/jails/otc-pgc1 -d '{
  "name": "otc-pgc1",
  "template": "<pool>/jails/templates/pg18@base",
  "network": {"mode": "vnet", "bridge": "jdn0", "private": true,
              "ip4": "10.64.0.10/24", "gateway": "10.64.0.1"},
  "limits": {"memoryuse": "2G", "maxproc": 300, "pcpu": 200},
  "params": {"devfs_ruleset": 4, "securelevel": 3, "enforce_statfs": 2, "children.max": 0,
             "sysvshm": "new", "sysvsem": "new", "sysvmsg": "new"},
  "datasets": [{"name": "data", "properties": {
    "mountpoint": "/usr/local/jails/containers/otc-pgc1/var/db/postgres",
    "refquota": "20G", "recordsize": "16k", "compression": "lz4", "atime": "off"}}],
  "properties": {"otter:role": "cluster"},
  "autostart": true
}'
```

**No `vmemoryuse` or `openfiles`.** rctl sums both per process, and
PostgreSQL shares what they count: every process maps all of
`shared_buffers`, and every process's descriptor table is sized by the
postmaster's probe. Either limit makes the cluster refuse to fork long
before it is full (`feasibility.md` §5). Since JailDeck v0.21.0
(jaildeck#119), `PATCH /api/jails/otc-pgc1/limits` changes them in place,
live on a running jail: only the limits named, and `null` removes one.

### E3. Bootstrap

The script and the admin CA's certificate go into the jail through exec,
each passed as an argument (exec has no stdin), then the script runs. Its
arguments are not secret; the two passwords it needs are made in the jail.

```sh
# put <path> <mode> <local file>: one exec that writes the file in the jail.
put() {
  jq -n --arg p "$1" --arg m "$2" --rawfile c "$3" '{timeout: "30s", argv: ["sh", "-c",
    "umask 022; mkdir -p \"$(dirname \"$1\")\" && printf %s \"$3\" > \"$1\" && chmod \"$2\" \"$1\"",
    "_", $p, $m, $c]}' | $J -X POST $API/jails/otc-pgc1/exec -d @-
}
put /usr/local/sbin/otter-pg-bootstrap 0555 deploy/cluster/otter-pg-bootstrap
put /usr/local/etc/otter/admin-ca.crt 0444 admin-ca.crt
$J -X POST $API/jails/otc-pgc1/exec -d '{"timeout":"3m","argv":["/usr/local/sbin/otter-pg-bootstrap",
  "-n","10.64.0.1/32","-l","10.64.0.10","-N","otc-pgc1","-b","512MB","-m","100"]}'
# → exit_code 0 each; the last stdout ends "otter-pg-bootstrap: pgbouncer"
```

### E4. The node's client certificate

The admin CA is made once, off the node (`security.md`, Secrets). The
node's key is made on the node and never leaves it; only its request
travels to the CA and back as a certificate.

```sh
doas sh -c 'cd /usr/local/etc/otter && umask 077 &&
  openssl req -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 -subj /CN=otter_admin \
    -keyout cluster-admin.key -out /tmp/<node>.csr &&
  chown otter:otter cluster-admin.key && chmod 0400 cluster-admin.key'
# Where the CA is:
openssl x509 -req -in <node>.csr -CA admin-ca.crt -CAkey admin-ca.key -CAcreateserial -days 825 -out <node>.crt
# Back on the node:
doas install -o otter -g otter -m 0444 <node>.crt /usr/local/etc/otter/cluster-admin.crt
```

**Check E:** as the worker's user, with the cluster's certificate pinned:

```sh
doas cat $R/var/db/postgres/data18/server.crt > /tmp/otc-pgc1.crt
C="host=10.64.0.10 dbname=postgres user=otter_admin sslmode=verify-ca sslrootcert=/tmp/otc-pgc1.crt"
doas -u otter psql "$C sslcert=/usr/local/etc/otter/cluster-admin.crt sslkey=/usr/local/etc/otter/cluster-admin.key" \
  -c 'select current_user'                        # → otter_admin, over TLSv1.3
PGPASSWORD=x psql "$C" -c 'select 1'              # → requires a valid client certificate
# A certificate from another CA → "unknown ca"; another server pin → "certificate verify failed".
doas rctl | grep otc-pgc1                          # → memoryuse, maxproc, pcpu
```

### E5. Test tenants

F17–F22 need two tenants. Make them as the worker does
(`internal/postgres`: `EnsureRole`, `EnsureDatabase`, `EnsurePoolEntries`),
named as it names them, `otc_000000000001` and `otc_000000000002`, with
throwaway passwords; write their two lines into
`/usr/local/etc/pgbouncer.d/otter-databases.ini` in the worker's format and
`RELOAD` PgBouncer through its admin console in the jail. Drop both, and
empty the file, once Part F is done: the cluster is the node's real one.

## Part F: verification (maps to `feasibility.md` §8)

Run each, write the result into §8.

The network ones (F7–F14) are [`scripts/isolation-check.sh`](../scripts/isolation-check.sh),
run after every PF change (#24). It probes, by TCP connect, from the
cluster jail to the internet, the host's sshd on the tenant network and
every listener passed with `-t` (the LAN, the host's other addresses,
other jails' services, the VM switches); from the host to the tenant
ports; and from every non-Otter jail passed with `-n` to the tenant ports
(R16). A control probe to the jail's own loopback proves the probes can
succeed, and the tenant block rule's packet counter must rise:

```sh
scripts/isolation-check.sh -t <lan_gateway>:80 -t <host_lan_ip>:22 -t <own_jail_ip>:<port> -n <own_jail>
# → "isolation-check: all probes as expected", exit 0
```

| # | Test | Command | Expect |
|---|---|---|---|
| F1 (§8.3) | Memory limit enforced | `doas jexec otc-pgc1 dd if=/dev/zero of=/dev/null bs=1500m count=1` | Fails (cannot allocate) |
| F2 (§8.3) | Limits removed on stop | `doas service jail stop otc-pgc1; doas rctl \| grep otc-pgc1` | No rules left |
| F3 (§8.4) | Epair lifecycle | `ifconfig -l` before and after start/stop | `epair10a/b` appear, then vanish |
| F4 (§8.4) | Private membership | `ifconfig jdn0 \| grep -A1 member` | `epair10a`, flags include `PRIVATE` |
| F5 (§8.5) | Quota enforced | `doas jexec otc-pgc1 dd if=/dev/urandom of=/var/db/postgres/fill bs=1m count=6000` | "Disc quota exceeded" near 5G. Then `rm` the file |
| F6 (§8.5) | Snapshots outside `refquota` | Write 2G, `zfs snapshot …/data@t`, delete the file, write 4G | Succeeds; `usedbysnapshots` ≈ 2G |
| F7 (§8.8) | No egress | `doas jexec otc-pgc1 nc -z -w3 1.1.1.1 443; echo $?` | Non-zero |
| F8 (§8.8) | No reaching the host | `doas jexec otc-pgc1 nc -z -w3 10.64.0.1 22` (also `514/udp` with `-u`) | Fails |
| F9 (§8.8) | No reaching the LAN | `doas jexec otc-pgc1 nc -z -w3 <lan_gateway> 80` | Fails |
| F10 (§8.8) | No DNS | `doas jexec otc-pgc1 host freebsd.org` | Fails |
| F11 (§8.8) | Tenant ↛ tenant | Start `pgref2`; `doas jexec pgref2 nc -z -w3 10.64.0.10 5432` | Fails |
| F12 (R16) | Non-Otter jail ↛ tenant | `doas jexec <own_jail> nc -z -w3 10.64.0.10 5432` (if Part C applies) | Fails |
| F13 | Host → tenant works | `nc -z 10.64.0.10 5432` on the host | Succeeds |
| F14 | Blocks are logged | `doas tcpdump -n -e -ttt -r /var/log/pflog \| tail` | F7–F12 attempts listed |
| F15 (§8.4) | Autostart | `doas sysrc jail_list+=otc-pgc1`, reboot | `otc-pgc1` up, rctl rules present, PF enabled |
| F16 (§8.6) | Bootstrap needs no secrets on the command line | Review E3: `otter-pg-bootstrap` takes addresses and sizes only; its two passwords are made in the jail | Verified |
| F17 (Q21) | Tenant through PgBouncer | `psql "host=10.64.0.10 port=6432 dbname=otc_000000000001 user=otc_000000000001 sslmode=require" -c 'select 1'` | Succeeds |
| F18 (R17) | Tenant ↛ another tenant's database | Same with `dbname=otc_000000000002 user=otc_000000000001`; also `dbname=postgres user=otc_000000000001` | Both fail |
| F19 (R17) | Tenant can't escalate | As `otc_000000000001`: `CREATE ROLE x;`, `CREATE DATABASE x;`, `CREATE EXTENSION plperlu;`, `COPY (select 1) TO PROGRAM 'id'` | All denied |
| F20 (Q23) | Prepared statements in transaction mode | `pgbench -i` then `pgbench -M prepared -c 4 -T 20` against `otc_000000000001` on `:6432` | No "prepared statement does not exist" errors |
| F21 (Q21) | Client limit per database | Open 21 sessions to `otc_000000000001` through PgBouncer | The 21st is refused; `otc_000000000002` unaffected |
| F22 (R18) | What a tenant can see | As `otc_000000000001`: `select datname from pg_database; select rolname from pg_roles; select * from pg_stat_activity;` | Names visible, other sessions' queries not. Recorded as accepted (R18) |

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
doas service jail stop otc-pgc1
doas zfs create -o mountpoint=none -o canmount=off <pool>/jails/retained
doas zfs snapshot $J/otc-pgc1/data@pre-reclone
doas zfs rename -u $J/otc-pgc1/data <pool>/jails/retained/otc-pgc1-data
doas zfs destroy $J/otc-pgc1                       # the old root only
doas zfs clone -o jaildeck:owner=otter-m0 <pool>/jails/templates/pg18@base $J/otc-pgc1
doas zfs rename -u <pool>/jails/retained/otc-pgc1-data $J/otc-pgc1/data
doas zfs set mountpoint=/usr/local/jails/containers/otc-pgc1/var/db/postgres $J/otc-pgc1/data
doas zfs mount $J/otc-pgc1/data
doas service jail start otc-pgc1
```

**Check G:** `psql` from Check E still works, and `m0test` still exists.
For a real upgrade, clone from a newer `pg18@<date>` snapshot instead of
`@base`.

If a replication tool (e.g. zrepl) snapshots and replicates `<pool>`, its
snapshots of `otc-pgc1/data` move with the rename. Confirm in its logs that
replication copes with the rename. If it doesn't, the replication filter or
the procedure needs adjusting before M3.

---

## Part H: rollback

To undo everything from this runbook:

```sh
doas service jail stop otc-pgc1 pgref2
doas zfs destroy -r <pool>/jails/containers/otc-pgc1
doas zfs destroy -r <pool>/jails/containers/pgref2
doas zfs destroy -r <pool>/jails/templates/pg18
doas zfs destroy -r <pool>/jails/retained
doas rm /etc/jail.conf.d/otc-pgc1.conf /etc/jail.conf.d/pgref2.conf
doas pfctl -d; doas sysrc -x pf_enable pflog_enable
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8888/api/networks/clusters
doas rm /etc/jail.conf
doas sysrc jail_list-="otc-pgc1 pgref2"
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
2. **Traefik ≥ 3.6.8** (§8.10). TCP routers with `HostSNI(t1.db.otter-labs.com)`
   and `HostSNI(t2.db.otter-labs.com)`, both to PgBouncer at
   `10.64.0.10:6432`, with TLS termination using a Let's Encrypt wildcard
   (DNS-01 through the DNS provider's API).
   Test matrix: psql 16 (SSLRequest), psql 17/18 with
   `sslnegotiation=direct`, pgx, and JDBC, each with `sslmode=verify-full`.
   Also test re-encryption to PgBouncer's self-signed certificate.
3. **Control-plane path** (§8.11). The minimum profile's jails are on the
   node (`deploy/control-plane`, #58); `scripts/isolation-check.sh -c`
   probes them: the worker reaches the DB port only, and `otter-api`
   reaches nothing but it, not the node host and not `jdn0`. The edge's
   path to `otter-api:8080` waits on the tunnel (#22).

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
