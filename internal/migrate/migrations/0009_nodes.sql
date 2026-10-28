-- Nodes and clusters (domain-model.md InfrastructureNode, Cluster): where
-- instances can go. Operators add them with otter admin; placement reads
-- them inside the create transaction (AD-8).
--
-- Capacities are configured, not discovered. Used capacity is never
-- stored: it is the sum of what is placed, so it can't drift.
--
-- A node row holds no credentials and no endpoint. The control plane never
-- connects to a node; the node's worker pulls its own work (AD-2).

CREATE TABLE infrastructure_nodes (
    id                  uuid        PRIMARY KEY DEFAULT uuidv7(),
    name                text        NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,31}$'),
    -- draining: nothing new is placed here; offline: not running either.
    status              text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'draining', 'offline')),
    capacity_memory_mb  integer     NOT NULL CHECK (capacity_memory_mb >= 0),
    capacity_vcpu       integer     NOT NULL CHECK (capacity_vcpu >= 0),
    capacity_storage_gb integer     NOT NULL CHECK (capacity_storage_gb >= 0),
    -- Tenant addresses come from here, and the edge routes it to this node.
    tenant_subnet       cidr        NOT NULL,
    service_types       text[]      NOT NULL CHECK (service_types <@ ARRAY['postgresql']::text[] AND cardinality(service_types) > 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- Two nodes' tenant subnets never overlap.
    EXCLUDE USING gist (tenant_subnet inet_ops WITH &&)
);

CREATE TABLE clusters (
    id                   uuid        PRIMARY KEY DEFAULT uuidv7(),
    name                 text        NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9-]{1,31}$'),
    node_id              uuid        NOT NULL REFERENCES infrastructure_nodes (id),
    status               text        NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'draining', 'offline')),
    pg_major             integer     NOT NULL CHECK (pg_major BETWEEN 13 AND 99),
    jail_name            text        NOT NULL UNIQUE CHECK (jail_name ~ '^[a-z][a-z0-9-]{1,62}$'),
    -- PgBouncer's address in the node's tenant subnet, for the edge.
    pool_addr            inet        NOT NULL UNIQUE CHECK (masklen(pool_addr) = CASE WHEN family(pool_addr) = 4 THEN 32 ELSE 128 END),
    pool_port            integer     NOT NULL DEFAULT 6432 CHECK (pool_port BETWEEN 1 AND 65535),
    -- The cluster jail's own memory limit, reserved on its node.
    memory_mb            integer     NOT NULL CHECK (memory_mb > 0),
    capacity_storage_gb  integer     NOT NULL CHECK (capacity_storage_gb > 0),
    capacity_connections integer     NOT NULL CHECK (capacity_connections > 0),
    capacity_databases   integer     NOT NULL CHECK (capacity_databases > 0),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX clusters_node_id ON clusters (node_id);

GRANT SELECT ON infrastructure_nodes, clusters TO otter_api;
-- otter admin node|cluster add, drain, activate (Q16). Capacities are set
-- once, at add; changing them is a later, deliberate command.
GRANT SELECT, INSERT, UPDATE (status, updated_at) ON infrastructure_nodes, clusters TO otter_admin;
