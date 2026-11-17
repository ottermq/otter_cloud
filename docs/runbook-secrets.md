# Secret rotation runbook

How to replace each secret Otter Labs Cloud holds (#123), what breaks while
you do, and how to check it worked. The inventory is `security.md` §6,
"Secrets"; this is the procedure for each line of it, and for the few
secrets the services keep for themselves.

Rotate a secret **at once** when it may have leaked: a host or jail
compromised, a file readable by the wrong user, a value seen in a log, a
person who had it leaves. Otherwise on the schedule in each section. Every
rotation is written up in the operator's runbook, with the date, so the
next one is due from it.

Conventions:

- `run <jail> <cmd>` is `doas jexec <jail> <cmd>`, as in
  `deploy/control-plane/README.md`. The node host runs `otter migrate`,
  `otter admin` and the worker; the api runs in `otter-api`; the platform
  DB in `otter-db`; a cluster in its own jail (`otc-<cluster>`).
- A secret is written **straight to its file**, never as a command argument
  (ps(1), shell history and JailDeck's audit log record arguments), and
  never pasted through chat or a terminal that logs.
- Keep the old secret until the new one is checked, unless it leaked: then
  revoke first and accept the downtime.

## Summary

| Secret | Where | Lifetime | Downtime while rotating |
|---|---|---|---|
| [Platform DB role passwords](#platform-db-role-passwords) | Host and `otter-api`, one file per role | On leak, or yearly | A restart of the process using it |
| [Platform DB server certificate](#platform-db-server-certificate) | `otter-db`; pinned by every client | 10 years | None, with the two-step swap |
| [Node client certificate](#node-client-certificate-otter_admin) | Node host, worker user | As issued (expires; 1 year by default) | A worker restart |
| [Admin CA](#admin-ca) | Offline | 10 years | None, with the two-step swap |
| [Cluster server certificates](#cluster-server-certificates) | Each cluster jail; pinned by the worker and the edge | 10 years | Up to a minute of refused connections through the edge |
| [PgBouncer's internal passwords](#pgbouncers-internal-passwords) | Each cluster jail | On leak | A PgBouncer restart |
| [JailDeck token](#jaildeck-token) | Node host, worker user | On leak, or yearly | A worker restart |
| [WireGuard keys](#wireguard-keys) | Node and edge | On leak | Seconds of tunnel loss |
| [DNS API token](#dns-api-token) | Edge | On leak, or as the provider allows | None |
| [Backup age key pair](#backup-age-key-pair) | Public on nodes; private offline | On leak | None; the old key stays until its copies expire |
| [Backup target credentials](#backup-target-credentials) | Node / control plane | On leak, or as the provider allows | Copies fail until the new one is in |
| [Portal users' passwords](#portal-users-passwords-and-sessions) | Platform DB (hash only) | The user's | None |
| [Customer database passwords](#customer-database-passwords) | Nowhere (verifier only) | The customer's | That database's connections |

## Platform DB role passwords

`otter_api` (the api), `otter_node_<name>` (each node's worker),
`otter_migrator` (`otter migrate`) and `otter_admin` (`otter admin`).
`deploy/control-plane/otter-db-password` makes a new password, sets it as a
SCRAM verifier and writes it to the file in one step; nothing else sees it.

```sh
# on the node host
doas otter-db-password -j otter-api otter_api /usr/local/etc/otter/db-password otter
run otter-api service otter_api restart

doas otter-db-password otter_node_<name> /usr/local/etc/otter/db-password otter
doas service otter_worker restart

doas otter-db-password otter_migrator /usr/local/etc/otter/migrator-password root
doas otter-db-password otter_admin /usr/local/etc/otter/admin-password root
```

The api and the worker keep their open connections until the restart, and
fail to open new ones after the password changes: restart them right after.
The worker waits for the database and logs at most a few lines (#199).

**Check:** `curl https://<portal>/healthz` is 200; the worker logs
"worker running"; `otter admin node list` works with the new admin
password.

## Platform DB server certificate

Its clients (the api and every worker, `otter migrate`, `otter admin`) pin
it with `sslmode=verify-full` and `sslrootcert`, so no CA signs it and its
subjectAltName is the address they dial. libpq and pgx read `sslrootcert`
as a bundle: put the new certificate in **beside** the old one first, then
swap the server's, then drop the old.

```sh
# 1. a new key and certificate in otter-db, beside the live ones
run otter-db su -m postgres -c 'cd /var/db/postgres/data18 && \
  openssl req -x509 -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -days 3650 -subj "/CN=otter-db" -addext "subjectAltName=IP:10.64.1.10" \
  -keyout server.key.new -out server.crt.new && chmod 0600 server.key.new'

# 2. every client trusts both: old + new in one file
run otter-db cat /var/db/postgres/data18/server.crt /var/db/postgres/data18/server.crt.new > both.crt
doas install -o root -g wheel -m 0444 both.crt /usr/local/etc/otter/platform-db.crt
doas install -o root -g wheel -m 0444 both.crt <otter-api root>/usr/local/etc/otter/platform-db.crt
# and the same on every other node's host

# 3. the server swaps; new connections get the new certificate
run otter-db su -m postgres -c 'cd /var/db/postgres/data18 && \
  mv server.crt server.crt.old && mv server.key server.key.old && \
  mv server.crt.new server.crt && mv server.key.new server.key'
run otter-db service postgresql reload

# 4. a while later, every client trusts only the new one
run otter-db cat /var/db/postgres/data18/server.crt > new.crt
# install new.crt as platform-db.crt everywhere, as in step 2
```

Then remove `server.*.old`. Use the CN and address the bootstrap used
(`deploy/platform-db/otter-platform-db-bootstrap`: the jail's hostname,
and its `-l` address).

**Check:** after step 3 and again after step 4, the api answers and the
worker and `otter admin node list` connect. A client left out fails with a
certificate error, not silently.

## Node client certificate (`otter_admin`)

Each node's worker administers its clusters with a client certificate,
`CN=otter_admin`, from the admin CA (`security.md`, "The admin CA and node
client certificates"). It expires: `security.md`'s command issues it for
365 days, though a node's may have been issued for longer. Renew it before
then, or the worker can't reach any cluster. Its expiry is
`doas openssl x509 -in /usr/local/etc/otter/cluster-admin.crt -noout -enddate`.

```sh
# on the node, as the worker's user: a new key, which never leaves it
doas -u otter sh -c 'cd /usr/local/etc/otter && umask 077 && \
  openssl req -new -nodes -newkey ec -pkeyopt ec_paramgen_curve:P-256 \
  -subj "/CN=otter_admin" -keyout cluster-admin.key.new -out node.csr'
# where the CA key is: sign the request
openssl x509 -req -in node.csr -CA admin-ca.crt -CAkey admin-ca.key \
  -CAcreateserial -days 365 -out cluster-admin.crt.new
# back on the node
doas install -o otter -g otter -m 0444 cluster-admin.crt.new /usr/local/etc/otter/cluster-admin.crt.new
doas sh -c 'cd /usr/local/etc/otter && chmod 0400 cluster-admin.key.new && \
  mv cluster-admin.key.new cluster-admin.key && mv cluster-admin.crt.new cluster-admin.crt'
doas service otter_worker restart
```

The old certificate stays valid until it expires: a leaked one is not
revoked by this, since the clusters have no revocation list. If it leaked,
rotate the [admin CA](#admin-ca) instead, which ends every certificate it
issued.

**Check:** the worker logs no `connect to cluster` error, and the health
check keeps the node's databases `ok` (`otter admin instances list`).

## Admin CA

Its key is offline and only signs node certificates. Each cluster trusts
the CA certificate as its `ssl_ca_file` (`ca.crt` in the data directory,
copied by `otter-pg-bootstrap` from the jail's
`/usr/local/etc/otter/admin-ca.crt`). Replacing it ends every node
certificate it signed, so it goes in two steps, as the platform DB's
certificate does.

```sh
# 1. offline: a new CA (security.md, "The admin CA and node client certificates")
# 2. each cluster trusts both: old + new in one file, then the postgres
#    part of the bootstrap copies it in and reloads PostgreSQL
cat admin-ca.crt admin-ca-new.crt > both-ca.crt
doas install -o root -g wheel -m 0444 both-ca.crt <cluster root>/usr/local/etc/otter/admin-ca.crt
run <cluster jail> otter-pg-bootstrap -n <node-host>/32 -l <jail-ip> [the cluster's -b -m] postgres
# 3. every node gets a certificate from the new CA (above), and its worker
#    a restart
# 4. each cluster trusts only the new CA: install admin-ca-new.crt as
#    admin-ca.crt, and run the bootstrap's postgres part again
```

Run only the `postgres` part: `all` would also restart PgBouncer and give
its passwords new values. Update the cluster template's `admin-ca.crt`
too, or a new cluster trusts the old CA. Destroy the old CA key when step
4 is done everywhere.

**Check:** after step 4, every node's worker reaches its clusters (no
`connect to cluster` errors, databases `ok`).

## Cluster server certificates

Each cluster's self-signed certificate. The worker pins it, reading it
through JailDeck before each connection, and records it for the edge,
which pins it on its hop to PgBouncer (#171). There is nothing to
distribute; the cost is a short window in which the edge still pins the old
one.

```sh
# in the cluster jail: move the old pair aside, then the bootstrap makes a
# new one and gives PgBouncer a copy
run <cluster jail> sh -c 'cd /var/db/postgres/data18 && mv server.key server.key.old && mv server.crt server.crt.old'
run <cluster jail> otter-pg-bootstrap -n <node-host>/32 -l <jail-ip> [the cluster's -b -m] all
# on the node host: the worker's health check at startup records it at once
doas service otter_worker restart
```

From the PgBouncer restart until the worker has recorded the new
certificate and the edge has polled it (10 s), connections through the
edge are refused: restart the worker straight after. The worker reads the
new certificate itself the next time it connects.

**Check:** `clusters.server_cert` is the new certificate, and a connection
through the edge with `sslmode=verify-full` works.

## PgBouncer's internal passwords

`pgbouncer_auth`, which looks up tenants' SCRAM verifiers, and
`pgbouncer_admin`, the console the worker uses through JailDeck. Both are
made inside the cluster jail and never leave it. Running the bootstrap
again gives both new values, and restarts PgBouncer:

```sh
run <cluster jail> otter-pg-bootstrap -n <node-host>/32 -l <jail-ip> [the cluster's -b -m] pgbouncer
```

Clients' open connections drop at the restart.

**Check:** the worker's next health check keeps the cluster's databases
`ok` (it reads `SHOW DATABASES` through the console).

## JailDeck token

The worker's token is limited to reading and to running commands in jails
labelled `otter:role=cluster` (#169). A node with `OTTER_SNAPSHOT_EVERY`
set also gives it `--allow 'snapshot:otter-*@otter:role=cluster'` (#116):
the new token must have every grant the old one had. JailDeck shows a token once, when it
is made, and keeps only its hash.

```sh
# a new token under a new name, straight to a new file
doas sh -c 'umask 077; jaildeck token add otter-worker-<date> --allow read \
  --allow exec:otter:role=cluster > /usr/local/etc/otter/jaildeck-token.new'
doas sh -c 'cd /usr/local/etc/otter && chown otter:otter jaildeck-token.new && \
  chmod 0400 jaildeck-token.new && mv jaildeck-token.new jaildeck-token'
doas service otter_worker restart
# when the worker runs on the new one
doas jaildeck token revoke <the old token's name>
```

JailDeck's own UI password and any other token it holds rotate by
JailDeck's documentation.

**Check:** JailDeck's log shows the worker's requests under the new token's
name, with status 200.

## WireGuard keys

The node's and the edge's tunnel keys. Each side has its private key in a
file and the other side's public key in its config. Changing one side's key
breaks the tunnel until the other side has the new public key: do both
within a minute. The portal and every database route go through the
tunnel; Traefik keeps its last routes meanwhile.

```sh
# on the side being rotated (here a node): the new key beside the old
doas sh -c 'umask 077; wg genkey > /usr/local/etc/wireguard/wg0.key.new'
doas wg pubkey < /usr/local/etc/wireguard/wg0.key.new   # the new public key
# on the edge: replace the node's PublicKey in its [Peer], then
sudo systemctl restart wg-quick@wg0
# on the node: switch to the new key
doas sh -c 'cd /usr/local/etc/wireguard && mv wg0.key.new wg0.key && wg set wg0 private-key wg0.key'
```

Rotating the edge's key is the mirror image: `/etc/wireguard/wg0.key` on
the edge, and the `PublicKey` of the edge's `[Peer]` in every node's
`wg0.conf` (`wg syncconf` there).

**Check:** `wg show` on both ends has a recent handshake;
`scripts/tunnel-check.sh` from the edge passes; the portal answers.

## DNS API token

The edge's token for ACME DNS-01, scoped to the one zone
(`deploy/vps/README.md`). Traefik uses it only when it asks for or renews a
certificate.

1. In the DNS provider, make a new token with the same scope and address
   limits.
2. On the edge, write it to its file (`sh -c 'umask 077; cat >
   /etc/traefik/dns-token'`, paste, Ctrl-D) and
   `systemctl restart traefik`.
3. Revoke the old token in the provider.

**Check:** Traefik's log has no ACME error after the restart. The
certificates in `acme.json` keep serving; the next renewal uses the new
token. A wrong token shows only at the next renewal, so check the token's
"last used" in the provider after that.

## Backup age key pair

Copies are sealed to the public key in `OTTER_BACKUP_RECIPIENTS_FILE` on
each node and on the control plane; the private key is offline, in two
copies (`architecture.md` §10). A copy can be opened only with the key it
was sealed to, so **the old private key is kept until the last copy sealed
to it is gone**. That is 28 days for a running database's weekly dumps, 14
for the platform DB's, and longer for the newest dump of a database that is
stopped and not dumped again (`otter backup list` shows them).

1. Offline: `age-keygen -o otter-backup-<date>.key`, kept in two places,
   as the old one is.
2. Replace the public key in every recipients file, the node's and the
   control plane's (a line per key; to keep sealing to both for a while,
   list both). The worker reads it at start: restart it.
3. Keep the old private key until no copy sealed to it remains; then
   destroy both its copies. If it leaked, also remove the copies sealed to
   it, which are what it can open, once new ones exist.

**Check:** the next copy opens with the new key only:
`otter backup get -key <a new copy> -identity otter-backup-<date>.key -o /tmp/x`.

## Backup target credentials

- **`fs`:** the worker writes to a directory; what moves it off-site has
  its own credentials, rotated per that system's documentation (in the
  operator's runbook).
- **`s3`:** an access key per node and one for the control plane, each
  scoped to its prefix, in the file `OTTER_BACKUP_S3_CREDENTIALS_FILE`
  names. Make a new key with the same scope in the provider, write it to
  that file, restart the worker (the platform push reads it at each run),
  then delete the old key in the provider. A copy that fails in between is
  retried an hour later.

**Check:** the next copy is `ok` in `otter_backups_total{status="ok"}` and
`otter backup list`.

## Portal users' passwords and sessions

Users change their own password in the portal, which ends their other
sessions. For a user who lost theirs, or whose account may be taken over:

```sh
otter admin user disable <email>      # ends every session now, refuses logins
otter admin user enable <email>
otter admin user reset-link <email>   # a single-use link, handed over by you
```

There is no signing key to rotate: a session is a random token whose hash
the platform DB keeps.

## Customer database passwords

The customer resets theirs in the portal (#101): a new password, shown once,
and the old one stops working; existing connections stay until they end.
Otter keeps only the SCRAM verifier. If a customer's password leaked
through Otter's fault, reset it for them through the same path and tell
them.
