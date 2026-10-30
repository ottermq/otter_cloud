# 🦦 Otter Labs Cloud

A small, self-hosted cloud built on FreeBSD. It starts with managed
PostgreSQL databases on shared, hardened clusters, and adds dedicated
instances and Linux virtual machines on bhyve later.

> **Status: early implementation.** The design is in `docs/`. The latest
> release, [v0.3.0](CHANGELOG.md), completes milestone M3
> ([roadmap](docs/roadmap.md)): a node's worker creates and deletes real
> databases in a shared cluster, behind the invite-only API. Customers
> reach them from outside with M5.

## What it is

- **Managed PostgreSQL databases.** Each customer gets a database and a
  role in a shared PostgreSQL cluster, behind PgBouncer. The cluster runs
  in a hardened VNET jail with no network egress. Customers get a database,
  never a server or a shell.
- **Dedicated instances later:** one jail per instance, for customers who
  need their own settings, extensions or isolation.
- **Linux VMs later**, on bhyve.
- **Plans and quotas without real payments.** Billing is modelled in the
  data from day one, but the first users are on a free *Founder Beta* plan.
  A payment provider comes later.
- **Small on purpose.** It runs on hardware the operator owns, and grows by
  adding compute nodes.

## How it fits together

```
Customers ──▶ Edge VPS (Traefik, WireGuard hub) ──▶ Control plane (API + SPA, platform PostgreSQL)
                         │
                         └──▶ Compute nodes: JailDeck (host agent) + otter worker
                                             + cluster jails (PgBouncer + PostgreSQL)
```

- The **control plane** holds users, subscriptions and the queue of work.
  It never connects to nodes.
- Each **compute node** runs an `otter worker` that pulls its own work from
  the platform database and carries it out: SQL against the node's shared
  clusters, and host changes through
  [JailDeck](https://github.com/ottermq/jaildeck), which drives FreeBSD's own
  tools.
- The **edge** is the only thing on the public internet. It terminates TLS
  and routes PostgreSQL connections by SNI to the right instance.

The full design and its reasoning are in [docs/architecture.md](docs/architecture.md).

## Documents

| Document | What it covers |
|---|---|
| [architecture.md](docs/architecture.md) | System shape, deployment profiles, networking, backups, decision records |
| [domain-model.md](docs/domain-model.md) | Accounts, plans, subscriptions, quotas, instances, operations |
| [security.md](docs/security.md) | Threat model, risks, controls, secrets |
| [portals.md](docs/portals.md) | What the customer portal shows and the API behind it; the operator portal to come |
| [roadmap.md](docs/roadmap.md) | Milestones M0–M7 to a beta, and what comes after |
| [open-questions.md](docs/open-questions.md) | Decisions made, and the ones still open |
| [feasibility.md](docs/feasibility.md) | Research behind the design, with sources |
| [host-layer.md](docs/host-layer.md) | Why JailDeck, not Sylve, is the host agent |
| [jaildeck-scope.md](docs/jaildeck-scope.md) | What JailDeck had to gain to serve as the host agent |
| [runbook-m0.md](docs/runbook-m0.md) | Preparing a node and its shared cluster by hand, before any code depends on it |
| [poc-tablespace-quota.md](docs/poc-tablespace-quota.md) | Whether a tablespace per tenant can give hard storage quotas (it can't; Q22 → soft quota) |
| [CHANGELOG.md](CHANGELOG.md) | What each release changed; releases follow milestones |

## Building

Requires Go 1.26 or later.

```sh
make build        # bin/otter
make check        # gofmt, go vet, tests
make lint         # staticcheck, govulncheck
./bin/otter -h    # subcommands: api, worker, admin, migrate, version
```

The database tests need a PostgreSQL they can create databases on. Without
one they are skipped:

```sh
make test-db      # starts postgres:18 in a container and prints the export line
export OTTER_TEST_DATABASE_URL='postgres://otter:otter@127.0.0.1:55432/otter?sslmode=disable'
make test
make test-db-stop
```

## Platform database

Once per PostgreSQL server, as a superuser, create the roles and the
database, then set the passwords with `\password` (psql sends only the SCRAM
verifier):

```sh
psql -v ON_ERROR_STOP=1 -f deploy/platform-db/bootstrap.sql
psql -c '\password otter_migrator'
psql -c '\password otter_api'
psql -c '\password otter_admin'
```

Then, with `OTTER_DATABASE_URL` naming `otter_migrator` and its password in
the file `OTTER_DATABASE_PASSWORD_FILE` names:

```sh
otter migrate -status   # what is applied, what is pending; changes nothing
otter migrate           # apply what is pending
```

Each migration runs in its own transaction and is recorded with a checksum.
An applied migration is never edited: a fix is a new migration. After the
migrations, `otter migrate` syncs the flavors from
`internal/catalog/flavors.json` and the plans from
`internal/billing/plans.json`, and says what it changed.

## Running

Each process connects as its own role. `otter api`, with
`OTTER_DATABASE_URL` naming `otter_api`:

```sh
otter api                                    # serves on OTTER_LISTEN_ADDR
```

`otter admin`, run by an operator over SSH (Q16), with `OTTER_DATABASE_URL`
naming `otter_admin`:

```sh
otter admin invite create ana@example.com   # prints a single-use sign-up link
otter admin invite list
otter admin user list
otter admin user disable -reason "…" ana@example.com   # ends their sessions now
otter admin user enable ana@example.com
otter admin plan list
otter admin subscription create ana@example.com <plan>   # a free plan starts active
otter admin subscription list
otter admin subscription cancel ana@example.com
otter admin node add -memory-mb N -storage-gb N -tenant-subnet 10.64.0.0/22 node-01
otter admin cluster add -node node-01 -pool-addr 10.64.0.10 -memory-mb N \
    -storage-gb N -connections N -databases N pgc1
otter admin node list | cluster list
otter admin node drain node-01        # nothing new is placed there; activate undoes it
```

The API so far: `POST /api/signup` (`{"token", "password", "account_name"}`,
the token from the invite link), `POST /api/login` (`{"email",
"password"}`), `POST /api/logout`, `GET /api/me`, `GET /api/catalog`
(each service type with the flavors that can be created),
`GET /api/subscription` (the plan, what it allows and what is allocated),
the account's databases: `GET /api/instances`, `POST /api/instances`
(`{"name", "flavor"}`, with an optional `Idempotency-Key` header; `202`
with the queued operation), `GET /api/instances/{id}` and
`DELETE /api/instances/{id}` (`{"confirm": "<name>"}`),
`POST /api/instances/{id}/stop`, `…/start` and `…/restart` (no body;
`202` with the queued operation, `409` when another is in flight or the
database can't from its status), and the
operations on them: `GET /api/operations`,
`GET /api/operations/{id}` (for the portal to poll) and
`GET /api/instances/{id}/operations`, newest first, at most 100. An
operation shows a reason code and a message, never internal errors.
Another account's resource is a `404`. Requests are JSON; errors are
`{"error": "…"}`. The session is a `__Host-otter_session` cookie.

Operations: `GET /healthz` answers `200` when the platform database does and
`503` when it doesn't. Prometheus metrics (`otter_http_requests_total`,
`otter_http_request_duration_seconds`, Go runtime; the worker's
`otter_operation_runs_total`, `otter_reconcile_findings_total` and
`otter_reconcile_runs_total`) are on the internal `OTTER_METRICS_ADDR`
listener only. The worker reconciles its node at startup and every 5
minutes: anything the platform database doesn't account for is an error
log line and a finding; it never destroys what it doesn't know. Logs are JSON on stderr, one `request`
line per request, and every line in a request carries its `request_id`,
also returned as `X-Request-Id`.

## Configuration

otter reads its settings from the environment and refuses to start if one is
missing or invalid, naming it. Secrets never come from the environment: a
`*_FILE` variable names a file only its owner can read (`chmod 0400`).
[`.env.sample`](.env.sample) has a starting point for development.

| Variable | Used by | Default | |
|---|---|---|---|
| `OTTER_LOG_LEVEL` | all | `info` | `debug`, `info`, `warn` or `error` |
| `OTTER_DATABASE_URL` | api, worker, admin, migrate | required | `postgres://<role>@<host>:<port>/<db>?sslmode=…`, with no password |
| `OTTER_DATABASE_PASSWORD_FILE` | api, worker, admin, migrate | none | File holding the role's password. Optional when the server authenticates otherwise |
| `OTTER_JAILDECK_SOCKET` | worker | `/var/run/jaildeck.sock` | JailDeck's unix socket on the node |
| `OTTER_CLUSTER_ADMIN_CERT_FILE` | worker | required | The node's client certificate for the clusters' `otter_admin` role, `CN=otter_admin`, from the admin CA (`security.md`, Secrets) |
| `OTTER_CLUSTER_ADMIN_KEY_FILE` | worker | required | Its private key, mode `0400`, made on the node and never copied off it |
| `OTTER_DUMP_DIR` | worker | `/var/db/otter/dumps` | Final dumps of deleted databases (Q26). Outside any jail, mode `0700`, owned by the worker's user; the worker refuses one open to others |
| `OTTER_JAILDECK_TOKEN_FILE` | worker | required | File holding the worker's JailDeck token, mode `0400`, owned by the worker's user. Root-equivalent on the node: never copied elsewhere |
| `OTTER_LISTEN_ADDR` | api | `127.0.0.1:8080` | `host:port` |
| `OTTER_METRICS_ADDR` | api, worker | `127.0.0.1:9101` (api), `127.0.0.1:9102` (worker) | Internal `host:port` for `/metrics`, or `off`. Must differ from `OTTER_LISTEN_ADDR` |
| `OTTER_TRUSTED_PROXIES` | api | none | Comma-separated addresses or CIDR prefixes of the reverse proxy; only their `X-Forwarded-For` is believed |
| `OTTER_PUBLIC_URL` | api, `admin invite create` | required | The origin customers use, e.g. `https://cloud.otter-labs.com`; invite links point there. `http://` only on localhost |

## Related projects

- [JailDeck](https://github.com/ottermq/jaildeck): FreeBSD jail and bhyve
  dashboard, and the host agent on every node.
- [OtterMQ](https://github.com/ottermq/ottermq) and
  [GoodiesDB](https://github.com/ottermq/goodiesdb): planned as managed
  services once PostgreSQL is stable.

## License

[MIT](LICENSE)
