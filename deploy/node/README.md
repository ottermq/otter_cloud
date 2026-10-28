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

As root:

```sh
pw groupadd otter
pw useradd otter -g otter -d /nonexistent -s /usr/sbin/nologin -c "Otter worker"
# JailDeck's socket is mode 0660 for one group (jaildeck-scope.md §3.1).
pw groupmod <jaildeck-socket-group> -m otter

install -o root -g wheel -m 0555 otter /usr/local/bin/otter
install -d -o root -g otter -m 0750 /usr/local/etc/otter
install -o root -g otter -m 0640 worker.env /usr/local/etc/otter/worker.env
install -o otter -g otter -m 0400 jaildeck-token /usr/local/etc/otter/jaildeck-token
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
service otter_worker status
ps -o user,pid,command -U otter          # otter worker, as otter
grep otter-worker /var/log/messages      # its JSON log lines
otter worker -h
```
