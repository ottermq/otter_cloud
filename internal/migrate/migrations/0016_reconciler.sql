-- The node's reconciler (architecture.md §5, #96) queues a delete for a
-- database whose instance is deleted: the intent is known. That is all a
-- worker may insert: a delete, asked by the system, on its own node; it
-- already reads only its own instances (0012).

GRANT INSERT (instance_id, account_id, node_id, kind, requested_by) ON operations TO otter_worker;

-- Restrictive: own_node (0012) would let any kind through.
CREATE POLICY reconciler_queues_delete ON operations AS RESTRICTIVE FOR INSERT TO otter_worker
    WITH CHECK (kind = 'delete' AND requested_by = 'system' AND node_id = worker_node_id());
