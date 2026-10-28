-- Plans and subscriptions (domain-model.md Plan, Subscription; Q11).
--
-- Plans come from internal/billing/plans.json, synced by otter migrate;
-- nobody else writes them. A subscription copies its plan's limits when it
-- is created, so editing a plan changes only later subscriptions.
--
-- The payment boundary is in the table itself (Q11): a subscription that
-- needs payment can only be active with payment_status 'current', and
-- 'current' needs a real billing provider. With 'none' the only provider
-- there is, no paid subscription can become active, whatever writes the
-- row. The first real provider is a migration that adds it, with its
-- webhooks (M12).

CREATE TABLE plans (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    code             text        NOT NULL UNIQUE CHECK (code ~ '^[a-z][a-z0-9-]{1,31}$'),
    name             text        NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    price_minor      bigint      NOT NULL CHECK (price_minor >= 0),
    currency         text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    "interval"       text        NOT NULL CHECK ("interval" IN ('month', 'none')),
    requires_payment boolean     NOT NULL GENERATED ALWAYS AS (price_minor > 0) STORED,
    max_instances    integer     NOT NULL CHECK (max_instances >= 0),
    max_storage_gb   integer     NOT NULL CHECK (max_storage_gb >= 0),
    max_connections  integer     NOT NULL CHECK (max_connections >= 0),
    max_memory_mb    integer     NOT NULL CHECK (max_memory_mb >= 0),
    max_vcpu         integer     NOT NULL CHECK (max_vcpu >= 0),
    -- Offered for self-service; false means assigned by an operator.
    public           boolean     NOT NULL DEFAULT false,
    active           boolean     NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE subscriptions (
    id               uuid        PRIMARY KEY DEFAULT uuidv7(),
    account_id       uuid        NOT NULL REFERENCES accounts (id),
    plan_id          uuid        NOT NULL REFERENCES plans (id),
    status           text        NOT NULL CHECK (status IN ('pending', 'active', 'suspended', 'canceled', 'expired')),
    payment_status   text        NOT NULL CHECK (payment_status IN ('not_required', 'pending', 'current', 'past_due')),
    billing_provider text        NOT NULL DEFAULT 'none' CHECK (billing_provider IN ('none')),
    external_ref     text,
    -- Copied from the plan at creation (domain-model.md §2.4).
    requires_payment boolean     NOT NULL,
    max_instances    integer     NOT NULL CHECK (max_instances >= 0),
    max_storage_gb   integer     NOT NULL CHECK (max_storage_gb >= 0),
    max_connections  integer     NOT NULL CHECK (max_connections >= 0),
    max_memory_mb    integer     NOT NULL CHECK (max_memory_mb >= 0),
    max_vcpu         integer     NOT NULL CHECK (max_vcpu >= 0),
    current_period_end timestamptz,
    starts_at        timestamptz NOT NULL DEFAULT now(),
    ends_at          timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- A free subscription owes nothing; a paid one always has a payment state.
    CHECK (requires_payment = (payment_status <> 'not_required')),
    -- Active only when nothing is owed, or the payment is current.
    CHECK (status <> 'active' OR payment_status IN ('not_required', 'current')),
    -- Only a real provider's verified event can make a payment current.
    CHECK (payment_status <> 'current' OR billing_provider <> 'none'),
    CHECK (external_ref IS NULL OR billing_provider <> 'none'),
    CHECK (ends_at IS NULL OR ends_at >= starts_at)
);

-- At most one subscription per account that isn't over.
CREATE UNIQUE INDEX subscriptions_one_open_per_account ON subscriptions (account_id)
    WHERE status IN ('pending', 'active', 'suspended');

GRANT SELECT ON plans, subscriptions TO otter_api;
-- otter admin subscription create|cancel (Q16): it may create and end
-- subscriptions, never change their limits or payment state.
GRANT SELECT ON plans TO otter_admin;
GRANT SELECT, INSERT, UPDATE (status, ends_at, updated_at) ON subscriptions TO otter_admin;
