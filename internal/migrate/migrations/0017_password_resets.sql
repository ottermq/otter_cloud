-- Password reset links (#160): the platform sends no email, so an operator
-- recovers an account with `otter admin user reset-link`, which prints a
-- single-use link like an invite's. Only the token's SHA-256 is stored.
--
-- A link lives at most a day (the command's default is an hour), is used
-- once, and is revoked when a newer one is issued for the same user. Rows
-- are kept after use, as the record of who was let back in when.

CREATE TABLE password_resets (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id    uuid        NOT NULL REFERENCES users (id),
    token_hash bytea       NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
               CHECK (expires_at > created_at AND expires_at <= created_at + interval '24 hours'),
    used_at    timestamptz,
    revoked_at timestamptz,
    CHECK (used_at IS NULL OR revoked_at IS NULL)
);

CREATE INDEX password_resets_user_id ON password_resets (user_id);

-- otter_admin issues links and revokes older ones; it never sees a
-- password and can't mark a link used. otter_api uses them.
GRANT SELECT, INSERT, UPDATE (revoked_at) ON password_resets TO otter_admin;
GRANT SELECT, UPDATE (used_at) ON password_resets TO otter_api;
