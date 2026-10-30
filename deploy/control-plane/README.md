# The control plane on a node (minimum profile)

In the minimum profile (`architecture.md` §2) the control plane runs on the
node, in two jails made through JailDeck (#58):

- **`otter-db`**: the platform database, PostgreSQL 18 from the `pg18`
  template, set up by
  [`otter-platform-db-bootstrap`](../platform-db/otter-platform-db-bootstrap);
- **`otter-api`**: `otter api`, from the release template, under
  [`rc.d/otter_api`](rc.d/otter_api).

The worker and JailDeck stay on the node host
([`deploy/node`](../node/README.md)). The api jail holds **no JailDeck
credential**: no token, no socket. Everything it can do to a node goes
through the platform DB as a queued operation, which the worker runs.

## Network

Both jails are on JailDeck network `platform`, the second `/24` of the
node's range, next to the clusters' (`architecture.md` §7):

| Who | Address | Membership |
|---|---|---|
| Node host (worker, `otter migrate`, `otter admin`) | `10.64.1.1` | The bridge's gateway |
| `otter-db` | `10.64.1.10`, `:5432` | Non-private |
| `otter-api` | `10.64.1.20`, `:8080` | Private |

The api must reach the DB, and two private members can't reach each other,
so the DB is the non-private one; it reaches nothing else, by PF. The
node's [`pf.conf`](../node/pf.conf) allows exactly:

- the node host → `otter-db:5432`;
- `otter-api` → `otter-db:5432`;
- the node host → `otter-api:8080`, and the edge, with #22.

Everything else from or to the network is dropped and logged: neither jail
has egress, reaches the host or a tenant, or is reached by one.

## The platform DB's logins

SCRAM passwords, over TLS 1.3 only, to `otter-db`'s own self-signed
certificate. Every client pins it (`sslmode=verify-full&sslrootcert=…`),
and its subject names the jail's address. `pg_hba` admits each role only
from where its process runs:

| Role | From | Password file |
|---|---|---|
| `otter_api` | `otter-api` | `otter-api:/usr/local/etc/otter/db-password`, `otter 0400` |
| `otter_migrator` | the node host | `/usr/local/etc/otter/migrator-password`, `root 0400` |
| `otter_admin` | the node host | `/usr/local/etc/otter/admin-password`, `root 0400` |
| `otter_node_<name>` (in `otter_worker`) | the node host | `/usr/local/etc/otter/db-password`, `otter 0400` |

[`otter-db-password`](otter-db-password) makes each password on the host
and sends it only on stdin through `jexec`, to the file and to `otter-db`.
A JailDeck exec would record it: its arguments are in the audit log. The
migrator and admin passwords stay on the host, never in the api jail: a
compromised api can't change the schema or act as an operator.

## Files in `otter-api`

| Path | Owner, mode | What |
|---|---|---|
| `/usr/local/bin/otter` | `root:wheel 0555` | The binary, `GOOS=freebsd` |
| `/usr/local/etc/rc.d/otter_api` | `root:wheel 0555` | [`rc.d/otter_api`](rc.d/otter_api) |
| `/usr/local/etc/otter/` | `root:otter 0750` | Settings and secrets |
| `/usr/local/etc/otter/api.env` | `root:otter 0640` | [`api.env.sample`](api.env.sample): paths, no secrets |
| `/usr/local/etc/otter/platform-db.crt` | `root:wheel 0444` | `otter-db`'s certificate, pinned |
| `/usr/local/etc/otter/db-password` | `otter:otter 0400` | `otter_api`'s password |

## Install

On the node, after the [PF policy](../node/pf.conf) with the platform
rules is loaded (`docs/runbook-m0.md` D3). As in the runbook's Part E,
with an operator's JailDeck token in `$TOKEN`:

```sh
J='curl -s -H "Authorization: Bearer $TOKEN" -H Content-Type:application/json'
API=http://127.0.0.1:8888/api
# put <jail> <path> <mode> <local file>: one exec that writes a file in the jail.
put() {
  jq -n --arg p "$2" --arg m "$3" --rawfile c "$4" '{timeout: "30s", argv: ["sh", "-c",
    "umask 022; mkdir -p \"$(dirname \"$1\")\" && printf %s \"$3\" > \"$1\" && chmod \"$2\" \"$1\"",
    "_", $p, $m, $c]}' | $J -X POST $API/jails/$1/exec -d @-
}
run() { # run <jail> <argv...>: one exec
  jail=$1; shift
  jq -n '{timeout: "3m", argv: $ARGS.positional}' --args "$@" | $J -X POST $API/jails/$jail/exec -d @-
}
```

### 1. The network

```sh
$J -X POST $API/networks -d '{"name":"platform","tenant":"otter"}'
# → {"name":"platform","bridge":"jdn1","subnet":"10.64.1.0/24","gateway":"10.64.1.1",...}
```

### 2. `otter-db`

```sh
$J -X PUT $API/jails/otter-db -d '{
  "name": "otter-db",
  "template": "<pool>/jails/templates/pg18@base",
  "network": {"mode": "vnet", "bridge": "jdn1", "private": false,
              "ip4": "10.64.1.10/24", "gateway": "10.64.1.1"},
  "limits": {"memoryuse": "1G", "maxproc": 200, "pcpu": 100},
  "params": {"devfs_ruleset": 4, "securelevel": 3, "enforce_statfs": 2, "children.max": 0,
             "sysvshm": "new", "sysvsem": "new", "sysvmsg": "new"},
  "datasets": [{"name": "data", "properties": {
    "mountpoint": "/usr/local/jails/containers/otter-db/var/db/postgres",
    "refquota": "10G", "recordsize": "16k", "compression": "lz4", "atime": "off"}}],
  "properties": {"otter:role": "platform-db"},
  "autostart": true
}'
put otter-db /usr/local/sbin/otter-platform-db-bootstrap 0555 deploy/platform-db/otter-platform-db-bootstrap
put otter-db /usr/local/etc/otter/bootstrap.sql 0444 deploy/platform-db/bootstrap.sql
run otter-db /usr/local/sbin/otter-platform-db-bootstrap \
  -l 10.64.1.10 -a 10.64.1.20/32 -n 10.64.1.1/32 -N otter-db
# → exit_code 0; stdout ends "otter-platform-db-bootstrap: roles and database"
run otter-db cat /var/db/postgres/data18/server.crt | jq -r .stdout > platform-db.crt
```

The limits are `memoryuse`, `pcpu` and `maxproc` only, as for a cluster
(`docs/runbook-m0.md` E2); `PATCH /api/jails/otter-db/limits` changes them
in place.

### 3. `otter-api`

```sh
$J -X PUT $API/jails/otter-api -d '{
  "name": "otter-api",
  "template": "<pool>/jails/templates/<release>@<pN>",
  "network": {"mode": "vnet", "bridge": "jdn1", "private": true,
              "ip4": "10.64.1.20/24", "gateway": "10.64.1.1"},
  "limits": {"memoryuse": "512M", "maxproc": 100, "pcpu": 100},
  "params": {"devfs_ruleset": 4, "securelevel": 3, "enforce_statfs": 2, "children.max": 0},
  "properties": {"otter:role": "api"},
  "autostart": true
}'
run otter-api pw useradd otter -d /nonexistent -s /usr/sbin/nologin -c "Otter API"
run otter-api install -d -o root -g otter -m 0750 /usr/local/etc/otter
put otter-api /usr/local/etc/rc.d/otter_api 0555 deploy/control-plane/rc.d/otter_api
put otter-api /usr/local/etc/otter/api.env 0640 api.env        # from api.env.sample
run otter-api chown root:otter /usr/local/etc/otter/api.env
put otter-api /usr/local/etc/otter/platform-db.crt 0444 platform-db.crt
```

The binary is too large for an exec argument. It goes in on stdin through
`jexec`, so the jail's own view of its paths decides where it lands: a
symlink in the jail can't redirect a write onto the host.

```sh
GOOS=freebsd GOARCH=amd64 make build          # where the code is → bin/otter
doas jexec otter-api sh -c 'umask 022 && cat > /usr/local/bin/otter.new &&
  chmod 0555 /usr/local/bin/otter.new && mv -f /usr/local/bin/otter.new /usr/local/bin/otter' < bin/otter
```

### 4. Passwords and the schema

On the host, with the certificate pinned there too:

```sh
doas install -o root -g wheel -m 0444 platform-db.crt /usr/local/etc/otter/platform-db.crt
doas install -o root -g wheel -m 0555 deploy/control-plane/otter-db-password /usr/local/sbin/
doas otter-db-password -j otter-api otter_api /usr/local/etc/otter/db-password otter
doas otter-db-password otter_migrator /usr/local/etc/otter/migrator-password root
doas otter-db-password otter_admin /usr/local/etc/otter/admin-password root

PDB='10.64.1.10:5432/otter?sslmode=verify-full&sslrootcert=/usr/local/etc/otter/platform-db.crt'
doas env OTTER_DATABASE_URL="postgres://otter_migrator@$PDB" \
  OTTER_DATABASE_PASSWORD_FILE=/usr/local/etc/otter/migrator-password otter migrate
```

`otter admin` runs the same way, as `otter_admin` with its password file,
and with `OTTER_PUBLIC_URL` for invites.

### 5. Start the api

```sh
run otter-api sysrc otter_api_enable=YES
run otter-api service otter_api start
# → "otter api: configuration ok", then the start
```

## Check

```sh
curl -s http://10.64.1.20:8080/healthz          # from the host → ok
# No JailDeck credential in the api jail (#58): no socket, and no file in
# its root holds a token. The worker's is searched for here; repeat with
# any other token file on the node.
run otter-api ls /var/run/jaildeck.sock          # → exit_code 1, no such file
R=$($J $API/jails/otter-api | jq -r .path)
doas grep -rlF -f /usr/local/etc/otter/jaildeck-token "$R"; echo $?   # → 1, nothing found
scripts/isolation-check.sh -c -t <lan_gateway>:80 -t <host_lan_ip>:22 -n <own_jail>
# → "isolation-check: all probes as expected"
```

The node's worker connects next: `otter admin node add <name>` prints the
role to create in `otter-db` (`deploy/platform-db/bootstrap.sql`); its
password is `otter-db-password otter_node_<name> /usr/local/etc/otter/db-password otter`,
and its `worker.env` names `otter_node_<name>@10.64.1.10` with the same
pinned certificate.
