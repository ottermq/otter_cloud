-- Accounts and users (docs/domain-model.md). One user per account in the
-- MVP; resources will belong to the account.
--
-- otter_api may read, create and update them but never delete: closing an
-- account or disabling a user is a status change, and nothing removes the
-- rows that audit entries and resources point at. The worker needs neither.

CREATE TABLE accounts (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    name       text        NOT NULL CHECK (name <> ''),
    status     text        NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'suspended', 'closed')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    account_id    uuid        NOT NULL REFERENCES accounts (id),
    -- Normalised by the application; the check keeps "A@x" and "a@x" from
    -- both getting in through some other path.
    email         text        NOT NULL UNIQUE CHECK (email = lower(email) AND email <> ''),
    password_hash text        NOT NULL CHECK (password_hash LIKE '$argon2id$%'),
    status        text        NOT NULL DEFAULT 'active'
                              CHECK (status IN ('active', 'disabled')),
    is_operator   boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX users_account_id ON users (account_id);

GRANT SELECT, INSERT, UPDATE ON accounts, users TO otter_api;
