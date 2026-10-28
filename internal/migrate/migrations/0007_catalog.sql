-- Flavors (domain-model.md Flavor): the technical sizes an instance can be
-- created with. Rows come from internal/catalog/flavors.json, synced by
-- otter migrate; nobody else writes them.
--
-- A flavor's sizes never change once it exists: instances copy their
-- allocation from it, and a code should mean one size for its whole life.
-- A different size is a new code. Flavors are never deleted, only
-- deactivated, so an instance's flavor_code always names a row.

CREATE TABLE flavors (
    code            text        PRIMARY KEY CHECK (code ~ '^[a-z][a-z0-9-]{1,31}$'),
    -- The service type enum lives in code (internal/catalog); this list
    -- grows with it, by migration.
    service_type    text        NOT NULL CHECK (service_type IN ('postgresql')),
    name            text        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    storage_gb      integer     NOT NULL CHECK (storage_gb > 0),
    max_connections integer     CHECK (max_connections > 0),
    memory_mb       integer     CHECK (memory_mb > 0),
    vcpu            integer     CHECK (vcpu > 0),
    active          boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- A database in a shared cluster has storage and connections, and no
    -- memory or CPU of its own.
    CHECK (service_type <> 'postgresql' OR (max_connections IS NOT NULL AND memory_mb IS NULL AND vcpu IS NULL))
);

GRANT SELECT ON flavors TO otter_api;
