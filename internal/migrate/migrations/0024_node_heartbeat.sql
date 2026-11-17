-- When each node's worker last recorded that it is alive and reaches the
-- platform DB (#200). The control plane exports it, so a node whose worker
-- stopped, or lost the platform DB, is seen from off the node. NULL until
-- the worker's first heartbeat.
ALTER TABLE infrastructure_nodes ADD COLUMN last_seen_at timestamptz;

-- The worker's policy (own_node) limits it to its own node's row.
GRANT UPDATE (last_seen_at) ON infrastructure_nodes TO otter_worker;
