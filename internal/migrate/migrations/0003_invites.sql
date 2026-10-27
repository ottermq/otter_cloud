-- Invites (security.md §6): sign-up is invite-only. An operator creates an
-- invite for an email address; the link carries a random token, of which
-- only the SHA-256 is stored, so a read of this table can't sign anyone up.
-- An invite is used once and expires.
--
-- Rows are kept after use, as the record of who was invited when.

CREATE TABLE invites (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    email      text        NOT NULL CHECK (email = lower(email) AND email <> ''),
    token_hash bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    used_at    timestamptz,
    user_id    uuid        REFERENCES users (id),
    CHECK ((used_at IS NULL) = (user_id IS NULL))
);

CREATE INDEX invites_email ON invites (email);

GRANT SELECT, INSERT, UPDATE ON invites TO otter_api;
