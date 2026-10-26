// Package operations claims operations for this node with a lease, runs
// their steps with retries, and runs the reconciler loop. It runs in otter
// worker.
//
// The platform database's operations table is the queue (AD-3); the
// control plane never connects to a node (AD-2).
//
// See docs/architecture.md §3.
package operations
