-- otter_admin: the role `otter admin` connects as (Q16). Operators reach the
-- platform only through `otter admin` over SSH; this role holds exactly
-- what those commands use, so a leaked otter_api password can't run
-- operator actions, and operator writes can be told apart by role.
--
-- Column-level UPDATE: an operator command may change a user's status and
-- revoke sessions, nothing else (not a password hash, not an email).

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_admin') THEN
        RAISE EXCEPTION 'role otter_admin does not exist: run CREATE ROLE otter_admin LOGIN; as a superuser (see deploy/platform-db/bootstrap.sql)';
    END IF;
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO otter_admin', current_database());
END
$$;

GRANT USAGE ON SCHEMA public TO otter_admin;

GRANT SELECT ON accounts TO otter_admin;
GRANT SELECT, UPDATE (status) ON users TO otter_admin;
GRANT SELECT, UPDATE (revoked_at) ON sessions TO otter_admin;
GRANT SELECT, INSERT ON invites TO otter_admin;
GRANT SELECT, INSERT ON audit_events TO otter_admin;
