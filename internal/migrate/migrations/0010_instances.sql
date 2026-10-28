-- Instances and operations (domain-model.md Instance, Operation, §4, §5).
--
-- An instance is what a customer owns; an operation is the durable job
-- that makes it so. otter api inserts both in the transaction that checks
-- entitlement, quota and placement (AD-8), and a node's worker runs the
-- operation later (M3).
--
-- Nothing deletes a row: a deleted instance keeps its row (status
-- 'deleted') for audit and billing history.

CREATE TABLE instances (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    name            text        NOT NULL CHECK (name ~ '^[a-z][a-z0-9-]{1,62}$'),
    service_type    text        NOT NULL CHECK (service_type IN ('postgresql')),
    flavor_code     text        NOT NULL REFERENCES flavors (code),
    -- The allocation, copied from the flavor at creation.
    storage_gb      integer     NOT NULL CHECK (storage_gb > 0),
    max_connections integer     CHECK (max_connections > 0),
    memory_mb       integer     CHECK (memory_mb > 0),
    vcpu            integer     CHECK (vcpu > 0),
    cluster_id      uuid        REFERENCES clusters (id),
    node_id         uuid        NOT NULL REFERENCES infrastructure_nodes (id),
    -- The database and role name in the cluster: otc_<12 hex>. Chosen before
    -- anything touches the node, so a retry finds what it made.
    infra_name      text        NOT NULL UNIQUE CHECK (infra_name ~ '^otc_[0-9a-f]{12}$'),
    desired_state   text        NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped', 'deleted')),
    status          text        NOT NULL DEFAULT 'provisioning'
                                CHECK (status IN ('provisioning', 'running', 'stopping', 'stopped', 'starting', 'failed', 'deleting', 'deleted')),
    health          text        NOT NULL DEFAULT 'unknown' CHECK (health IN ('ok', 'degraded', 'unknown')),
    -- A customer-safe code (portals.md §3), never free text.
    reason          text        CHECK (reason ~ '^[a-z][a-z_]{1,62}$'),
    endpoint_host   text,
    endpoint_port   integer     CHECK (endpoint_port BETWEEN 1 AND 65535),
    expires_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz,
    -- A shared database lives in a cluster.
    CHECK (service_type <> 'postgresql' OR (cluster_id IS NOT NULL AND max_connections IS NOT NULL)),
    CHECK ((status = 'deleted') = (deleted_at IS NOT NULL))
);

-- Names are unique per account among what the account still has.
CREATE UNIQUE INDEX instances_account_name ON instances (account_id, name) WHERE status <> 'deleted';
CREATE INDEX instances_cluster_id ON instances (cluster_id) WHERE status <> 'deleted';

CREATE TABLE operations (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    instance_id     uuid        NOT NULL REFERENCES instances (id),
    -- Copied from the instance: the account for its history (#77), the
    -- node for the worker that claims it (M3).
    account_id      uuid        NOT NULL REFERENCES accounts (id),
    node_id         uuid        NOT NULL REFERENCES infrastructure_nodes (id),
    kind            text        NOT NULL CHECK (kind IN ('create', 'start', 'stop', 'restart', 'delete', 'reset_password', 'expire')),
    status          text        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'canceled')),
    step            text,
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_by       text,
    locked_until    timestamptz,
    requested_by    text        NOT NULL CHECK (requested_by ~ '^(user:[0-9a-f-]{36}|operator:[A-Za-z0-9._-]{1,64}|system)$'),
    idempotency_key text        CHECK (length(idempotency_key) BETWEEN 1 AND 255),
    -- Small and non-secret (a SCRAM verifier at most); cleared when done.
    payload         jsonb       NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(payload) = 'object' AND pg_column_size(payload) <= 4096),
    reason          text        CHECK (reason ~ '^[a-z][a-z_]{1,62}$'),
    -- Operator only: never in an API response (portals.md §3).
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz
);

-- At most one operation in flight per instance (architecture.md §5).
CREATE UNIQUE INDEX operations_one_in_flight ON operations (instance_id) WHERE status IN ('pending', 'running');
-- A client's Idempotency-Key names one request per account.
CREATE UNIQUE INDEX operations_idempotency_key ON operations (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX operations_instance_id ON operations (instance_id, created_at);
CREATE INDEX operations_node_pending ON operations (node_id, next_attempt_at) WHERE status = 'pending';

GRANT SELECT, INSERT ON instances, operations TO otter_api;
