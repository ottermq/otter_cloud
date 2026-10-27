-- Audit events (domain-model.md AuditEvent, security.md §6 Audit).
--
-- Append-only at the permission level: otter_api may insert and read,
-- otter_worker may only insert, and neither may update or delete. Only the
-- owner (otter_migrator) can remove rows, which retention (at least a year)
-- will do by a deliberate, separate step.

CREATE TABLE audit_events (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    at          timestamptz NOT NULL DEFAULT now(),
    -- user:<id>, operator:<login>, system, or anonymous (a failed login).
    actor       text        NOT NULL CHECK (actor ~ '^(user:[0-9a-f-]{36}|operator:[A-Za-z0-9._-]{1,64}|system|anonymous)$'),
    account_id  uuid        REFERENCES accounts (id),
    action      text        NOT NULL CHECK (action ~ '^[a-z][a-z_]*(\.[a-z_]+)+$'),
    target_type text,
    target_id   text,
    ip          inet,
    user_agent  text        CHECK (length(user_agent) <= 512),
    outcome     text        NOT NULL CHECK (outcome IN ('ok', 'denied', 'error')),
    -- Small, and never secrets.
    details     jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(details) = 'object' AND pg_column_size(details) <= 4096),
    CHECK ((target_type IS NULL) = (target_id IS NULL))
);

CREATE INDEX audit_events_at ON audit_events (at);
CREATE INDEX audit_events_account_at ON audit_events (account_id, at) WHERE account_id IS NOT NULL;

GRANT SELECT, INSERT ON audit_events TO otter_api;
GRANT INSERT ON audit_events TO otter_worker;
