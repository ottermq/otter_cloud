# 🦦 Otter Labs Cloud

A small, self-hosted cloud built on FreeBSD. It starts with managed
PostgreSQL, each instance in its own hardened jail, and adds Linux virtual
machines on bhyve later.

> **Status: planning.** This repository holds the design documents. There is
> no application code yet: implementation starts once the plan is reviewed.

## What it is

- **Managed PostgreSQL in jails.** One VNET jail per instance, a quota'd ZFS
  dataset for its data, rctl limits, and no network egress. Customers get a
  database, never a shell.
- **Linux VMs later**, on bhyve, once the jail service is stable.
- **Plans and quotas without real payments.** Billing is modelled in the
  data from day one, but the first users are on a free *Founder Beta* plan.
  A payment provider comes later.
- **Small on purpose.** It runs on hardware the operator owns, and grows by
  adding compute nodes.

## How it fits together

```
Customers ──▶ Edge VPS (Traefik, WireGuard hub) ──▶ Control plane (API + SPA, platform PostgreSQL)
                         │
                         └──▶ Compute nodes: JailDeck (host agent) + otter worker + tenant jails
```

- The **control plane** holds users, subscriptions and the queue of work.
  It never connects to nodes.
- Each **compute node** runs an `otter worker` that pulls its own work from
  the platform database and carries it out through
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
| [runbook-m0.md](docs/runbook-m0.md) | Preparing a node by hand, before any code depends on it |
| [goodiesdb-assessment.md](docs/goodiesdb-assessment.md) | GoodiesDB as a future managed service |

## Related projects

- [JailDeck](https://github.com/ottermq/jaildeck): FreeBSD jail and bhyve
  dashboard, and the host agent on every node.
- [OtterMQ](https://github.com/ottermq/ottermq) and
  [GoodiesDB](https://github.com/ottermq/goodiesdb): planned as managed
  services once PostgreSQL is stable.

## License

[MIT](LICENSE)
