-- The worker's side of the operations queue (architecture.md §2, §5;
-- security.md B4).
--
-- Each node's worker logs in as a role of its own, otter_node_<name> with
-- hyphens as underscores, that is a member of otter_worker. The operator
-- creates it as a superuser after otter admin node add, which prints the
-- statement. Row-level security limits otter_worker to its node's rows, so
-- one node's credentials neither read nor change another node's work, and a
-- worker needs no configuration to know which node it is.

-- The login role of a node's worker.
CREATE FUNCTION node_role(node_name text) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    RETURN 'otter_node_' || replace(node_name, '-', '_');

-- The node whose worker is asking; NULL for any other role.
CREATE FUNCTION worker_node_id() RETURNS uuid
    LANGUAGE sql STABLE
    BEGIN ATOMIC
        SELECT id FROM infrastructure_nodes WHERE node_role(name) = current_user;
    END;

-- The channel a node's worker listens on to wake up for new work.
CREATE FUNCTION ops_channel(node_id uuid) RETURNS text
    LANGUAGE sql IMMUTABLE STRICT
    RETURN 'otter_ops_' || replace(node_id::text, '-', '');

GRANT EXECUTE ON FUNCTION node_role(text), worker_node_id(), ops_channel(uuid) TO otter_worker;

-- An operation that becomes pending wakes its node's worker. The
-- notification carries nothing: any role may LISTEN on any channel. It runs
-- as the owner, so the roles that queue operations need no EXECUTE on
-- ops_channel(); a trigger function can't be called on its own.
CREATE FUNCTION notify_operation() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public
    AS $$
BEGIN
    PERFORM pg_notify(ops_channel(NEW.node_id), '');
    RETURN NULL;
END
$$;

CREATE TRIGGER operations_notify
    AFTER INSERT OR UPDATE OF status ON operations
    FOR EACH ROW WHEN (NEW.status = 'pending')
    EXECUTE FUNCTION notify_operation();

-- Row-level security: a worker sees its own node, and on it the clusters,
-- instances and operations. The other roles keep what their grants give
-- them; the owner, otter_migrator, isn't subject to policies.
ALTER TABLE infrastructure_nodes ENABLE ROW LEVEL SECURITY;
ALTER TABLE clusters ENABLE ROW LEVEL SECURITY;
ALTER TABLE instances ENABLE ROW LEVEL SECURITY;
ALTER TABLE operations ENABLE ROW LEVEL SECURITY;

CREATE POLICY control_plane ON infrastructure_nodes TO otter_api, otter_admin USING (true) WITH CHECK (true);
CREATE POLICY control_plane ON clusters TO otter_api, otter_admin USING (true) WITH CHECK (true);
CREATE POLICY control_plane ON instances TO otter_api, otter_admin USING (true) WITH CHECK (true);
CREATE POLICY control_plane ON operations TO otter_api, otter_admin USING (true) WITH CHECK (true);

-- Compared by name, not through worker_node_id(), which reads this table.
CREATE POLICY own_node ON infrastructure_nodes TO otter_worker USING (node_role(name) = current_user);
CREATE POLICY own_node ON clusters TO otter_worker USING (node_id = worker_node_id());
CREATE POLICY own_node ON instances TO otter_worker USING (node_id = worker_node_id());
CREATE POLICY own_node ON operations TO otter_worker
    USING (node_id = worker_node_id()) WITH CHECK (node_id = worker_node_id());

GRANT SELECT ON infrastructure_nodes, clusters, instances TO otter_worker;
-- Claiming, checkpointing and finishing an operation: never which instance
-- or node it is for, what it does, or who asked.
GRANT SELECT, UPDATE (status, step, attempts, next_attempt_at, locked_by, locked_until, payload, reason,
    last_error, started_at, finished_at) ON operations TO otter_worker;
