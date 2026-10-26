# 🦦 Otter Labs Cloud

A small, self-hosted cloud built on FreeBSD. It starts with managed
PostgreSQL databases on shared, hardened clusters, and adds dedicated
instances and Linux virtual machines on bhyve later.

> **Status: early implementation.** The design is in `docs/`; the code is
> starting with milestone M1 ([roadmap](docs/roadmap.md)). Nothing runs yet.

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
| [roadmap.md](docs/roadmap.md) | Milestones M0–M7 to a beta, and what comes after |
| [open-questions.md](docs/open-questions.md) | Decisions made, and the ones still open |
| [feasibility.md](docs/feasibility.md) | Research behind the design, with sources |
| [host-layer.md](docs/host-layer.md) | Why JailDeck, not Sylve, is the host agent |
| [jaildeck-scope.md](docs/jaildeck-scope.md) | What JailDeck had to gain to serve as the host agent |
| [runbook-m0.md](docs/runbook-m0.md) | Preparing a node and its shared cluster by hand, before any code depends on it |
| [poc-tablespace-quota.md](docs/poc-tablespace-quota.md) | Whether a tablespace per tenant can give hard storage quotas (it can't; Q22 → soft quota) |

## Building

Requires Go 1.26 or later.

```sh
make build        # bin/otter
make check        # gofmt, go vet, tests
./bin/otter -h    # subcommands: api, worker, admin, migrate, version
```

## Related projects

- [JailDeck](https://github.com/ottermq/jaildeck): FreeBSD jail and bhyve
  dashboard, and the host agent on every node.
- [OtterMQ](https://github.com/ottermq/ottermq) and
  [GoodiesDB](https://github.com/ottermq/goodiesdb): planned as managed
  services once PostgreSQL is stable.

## License

[MIT](LICENSE)
