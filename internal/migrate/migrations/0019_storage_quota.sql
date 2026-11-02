-- Storage quota enforcement (Q22, Q27, #99). The node's worker measures
-- each database with pg_database_size() and moves it through the steps of
-- Q27: ok, warning (80 %), read_only (100 %, advisory), blocked (120 %,
-- connections refused); back to read-write under 95 %. A blocked database
-- is reopened only by an operator, in read_only, until cleanup_until.

ALTER TABLE instances
    ADD COLUMN storage_used_bytes  bigint      CHECK (storage_used_bytes >= 0),
    ADD COLUMN storage_measured_at timestamptz,
    ADD COLUMN storage_state       text        NOT NULL DEFAULT 'ok'
                                   CHECK (storage_state IN ('ok', 'warning', 'read_only', 'blocked')),
    ADD COLUMN cleanup_until       timestamptz;

-- What the worker measures, and the step it puts the database in: observed
-- state, as status is.
GRANT UPDATE (storage_used_bytes, storage_measured_at, storage_state) ON instances TO otter_worker;

-- An operator reopens a blocked database for a cleanup window.
GRANT UPDATE (storage_state, cleanup_until) ON instances TO otter_admin;
