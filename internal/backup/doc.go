// Package backup is the off-site half of backups (#118): the Target
// (none, fs, s3), age sealing above it, and the Shipper that copies the
// node's dumps there and removes the copies when they expire. The dumps
// themselves are made by internal/postgres. It runs in otter worker, and in
// otter backup for the platform DB's dumps and restores.
//
// See docs/architecture.md §10.
package backup
