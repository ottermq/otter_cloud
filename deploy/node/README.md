# otter worker on a node

`otter worker` runs on each node as its own unprivileged user, under
daemon(8), from `rc.d/otter_worker` (#48). It reaches JailDeck over its
unix socket with a token, the clusters with the node's client certificate,
and the platform DB with its node's role.

## Files

| Path | Owner, mode | What |
|---|---|---|
| `/usr/local/bin/otter` | `root:wheel 0555` | The binary |
| `/usr/local/etc/rc.d/otter_worker` | `root:wheel 0555` | [`rc.d/otter_worker`](rc.d/otter_worker) |
| `/usr/local/etc/otter/` | `root:otter 0750` | Settings and secrets |
| `/usr/local/etc/otter/worker.env` | `root:otter 0640` | [`worker.env.sample`](worker.env.sample): paths, no secrets |
| `/usr/local/etc/otter/jaildeck-token` | `otter:otter 0400` | The worker's JailDeck token |
| `/usr/local/etc/otter/cluster-admin.key` | `otter:otter 0400` | The node's key, made on the node (`security.md`, Secrets) |
| `/usr/local/etc/otter/cluster-admin.crt` | `otter:otter 0444` | Its certificate, from the admin CA |
| `/usr/local/etc/otter/db-password` | `otter:otter 0400` | The node role's password, if the platform DB asks for one |
| `/var/db/otter/dumps/` | `otter:otter 0700` | Final dumps (Q26) |

The worker refuses to start when a secret file or the dump directory is
open to group or others. rc.d runs `otter worker -check` as the worker's
user before it starts it, so the refusal stops the start with its reason
instead of daemon(8) restarting a refused worker.

## Install

JailDeck v0.14.0 or later, from its package, with its unix socket on
(JD-016). The socket is mode `0660` for one group; the token is still
required on it. The web UI stays on loopback (`JAILDECK_HOST=127.0.0.1`).
In `/usr/local/etc/jaildeck/jaildeck.env`:

```sh
JAILDECK_SOCKET=/var/run/jaildeck.sock
JAILDECK_SOCKET_GROUP=jaildeck
```

As root:

```sh
pw groupadd jaildeck
pw groupadd otter
pw useradd otter -g otter -d /nonexistent -s /usr/sbin/nologin -c "Otter worker"
pw groupmod jaildeck -m otter
service jaildeck restart       # ls -l /var/run/jaildeck.sock → srw-rw---- root jaildeck

install -o root -g wheel -m 0555 otter /usr/local/bin/otter
install -d -o root -g otter -m 0750 /usr/local/etc/otter
install -o root -g otter -m 0640 worker.env /usr/local/etc/otter/worker.env
# The worker's token, written straight to its file; it is shown only once
# and never needs to leave the node.
sh -c 'umask 077; jaildeck token add otter-worker > /usr/local/etc/otter/jaildeck-token'
chown otter:otter /usr/local/etc/otter/jaildeck-token
chmod 0400 /usr/local/etc/otter/jaildeck-token
# The key is made here and never leaves the node; its CSR goes to the
# admin CA (security.md, Secrets).
install -o otter -g otter -m 0444 cluster-admin.crt /usr/local/etc/otter/cluster-admin.crt
install -d -o root -g wheel -m 0755 /var/db/otter
install -d -o otter -g otter -m 0700 /var/db/otter/dumps
install -o root -g wheel -m 0555 rc.d/otter_worker /usr/local/etc/rc.d/otter_worker

sysrc otter_worker_enable=YES
service otter_worker start
```

`pg_dump` comes from `postgresqlNN-client`, the clusters' major version,
installed on the host in M0.

## Check

```sh
# The worker's token on the socket lists jails; nobody outside the group connects.
doas -u otter sh -c 'curl -s --unix-socket /var/run/jaildeck.sock \
  -H "Authorization: Bearer $(cat /usr/local/etc/otter/jaildeck-token)" http://localhost/api/jails'
service otter_worker status
ps -o user,pid,command -U otter          # otter worker, as otter
grep otter-worker /var/log/messages      # its JSON log lines
otter worker -h
```

## PF

[`pf.conf`](pf.conf) is the node's static policy (#17,
`docs/runbook-m0.md` Part D). Tenants open no connections, and only the
node host reaches them, on `:5432` and `:6432`. JailDeck's anchors are
included once for its networks; everything else on the host is unchanged.
No tool writes this file, and it never changes per tenant. It needs
`net.link.bridge.pfil_member=1` in `/etc/sysctl.conf`: FreeBSD 15's
default, `0`, keeps traffic between members of one bridge away from PF.

Load it with the dead-man switch in the runbook (D3), then check:

```sh
doas pfctl -si | head -1        # Status: Enabled
doas pfctl -s nat               # nat-anchor "jaildeck/*" all
doas pfctl -t tenants -T show   # the node's tenant subnet, not its gateway
sysctl net.link.bridge.pfil_member   # 1
```
