-- Cluster snapshots (#116, architecture.md §10). Where a node's replication
-- tool doesn't already snapshot the jails' datasets, the worker snapshots
-- each cluster jail on its node through JailDeck, its root and its data at
-- one point in time, and destroys each when it expires. A snapshot is a
-- backup_records row of kind 'snapshot', on the node (target local), of a
-- cluster rather than of one database: every tenant on it at once.
ALTER TABLE backup_records ADD COLUMN cluster_id uuid REFERENCES clusters (id);

ALTER TABLE backup_records ADD CONSTRAINT backup_records_snapshot
    CHECK (kind <> 'snapshot' OR (cluster_id IS NOT NULL AND instance_id IS NULL AND target = 'local'));

CREATE INDEX backup_records_cluster ON backup_records (cluster_id, created_at) WHERE cluster_id IS NOT NULL;
