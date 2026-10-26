// Package postgres is the service driver for databases in a shared
// PostgreSQL cluster: the steps that create, start, stop, delete and reset
// one (later, a dedicated instance too). It runs in otter worker.
//
// Drivers speak in domain steps (EnsureRole, EnsureDatabase,
// EnsurePoolEntry), each idempotent.
//
// See docs/architecture.md §3.
package postgres
