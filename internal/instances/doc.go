// Package instances owns instance records: create, read, delete,
// ownership, quota, placement and capacity, and turning lifecycle commands
// into operations. It runs in otter api.
//
// Quota, placement and capacity are decided in one database transaction
// with row locks (AD-8).
//
// See docs/architecture.md §3.
package instances
