-- Backup records (domain-model.md BackupRecord) and the delete of a shared
-- database (#56).
--
-- One row per backup artifact the platform knows about, so retention and
-- restore are answered from here, not by listing storage. Operator-only:
-- no API reads it. M3 writes only final dumps, kept on the node (Q26);
-- M7 adds the scheduled ones and the off-site target.

CREATE TABLE backup_records (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    instance_id  uuid        REFERENCES instances (id),
    node_id      uuid        NOT NULL REFERENCES infrastructure_nodes (id),
    -- The operation that made it, so a rerun finds its own dump.
    operation_id uuid        REFERENCES operations (id),
    kind         text        NOT NULL CHECK (kind IN ('snapshot', 'dump', 'final_snapshot', 'final_dump')),
    target       text        NOT NULL CHECK (target ~ '^[a-z][a-z0-9_-]{0,31}$'),
    -- Opaque to all but the target: for a local dump, its file name.
    ref          text        NOT NULL CHECK (length(ref) BETWEEN 1 AND 512),
    size_bytes   bigint      CHECK (size_bytes >= 0),
    sha256       text        CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    status       text        NOT NULL CHECK (status IN ('ok', 'failed', 'skipped')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz
);

CREATE UNIQUE INDEX backup_records_final ON backup_records (operation_id) WHERE kind = 'final_dump' AND status = 'ok';
CREATE INDEX backup_records_instance ON backup_records (instance_id, created_at);
CREATE INDEX backup_records_expiry ON backup_records (node_id, expires_at) WHERE expires_at IS NOT NULL;

ALTER TABLE backup_records ENABLE ROW LEVEL SECURITY;
CREATE POLICY control_plane ON backup_records TO otter_admin USING (true) WITH CHECK (true);
CREATE POLICY own_node ON backup_records TO otter_worker
    USING (node_id = worker_node_id()) WITH CHECK (node_id = worker_node_id());

-- The worker records what it dumped; the operator lists it.
GRANT SELECT, INSERT ON backup_records TO otter_worker;
GRANT SELECT ON backup_records TO otter_admin;

-- Marking an instance deleted, which releases its quota and capacity.
GRANT UPDATE (deleted_at) ON instances TO otter_worker;
