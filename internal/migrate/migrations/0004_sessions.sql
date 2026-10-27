-- Server-side sessions (security.md §6). The cookie carries a random token;
-- only its SHA-256 is stored, so a read of this table can't take over a
-- session. Logout sets revoked_at rather than deleting, so the row stays
-- as a record of the login; a later job will purge old rows.

CREATE TABLE sessions (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    token_hash   bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    user_id      uuid        NOT NULL REFERENCES users (id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    created_ip   inet,
    user_agent   text        CHECK (length(user_agent) <= 512)
);

CREATE INDEX sessions_user_id ON sessions (user_id);

GRANT SELECT, INSERT, UPDATE ON sessions TO otter_api;
