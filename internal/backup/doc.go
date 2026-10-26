// Package backup runs snapshot and dump schedules, applies retention from
// backup_records, and implements the off-site Target (none, fs, s3). It
// runs in otter worker.
//
// See docs/architecture.md §3.
package backup
