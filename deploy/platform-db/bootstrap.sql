-- One-time setup of the platform database's roles and database.
--
-- Run as a superuser, with psql (it uses \gexec):
--
--   psql -v ON_ERROR_STOP=1 -f deploy/platform-db/bootstrap.sql
--
-- Running it again changes nothing that exists. In the minimum profile,
-- otter-platform-db-bootstrap runs it in the otter-db jail (#58).
--
-- Then give each login role a password, sent as a SCRAM verifier or kept
-- out of the server's logs, and write it to a 0400 file owned by the
-- process that uses it, named by OTTER_DATABASE_PASSWORD_FILE. With psql's
-- \password (it sends only the verifier):
--
--   \password otter_migrator     -- otter migrate
--   \password otter_api          -- otter api
--   \password otter_admin        -- otter admin, run by operators over SSH
--
-- or, in the minimum profile, deploy/control-plane/otter-db-password.
-- After that, `otter migrate` as otter_migrator does the rest.

-- Owns the database and every table in it; the only role that runs DDL.
SELECT 'CREATE ROLE otter_migrator LOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_migrator') \gexec

-- The control plane's API. Gets only what each migration grants it.
SELECT 'CREATE ROLE otter_api LOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_api') \gexec

-- Operator commands (`otter admin`, Q16). Only what those commands use.
SELECT 'CREATE ROLE otter_admin LOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_admin') \gexec

-- The group every node's worker role joins. It cannot log in: each node gets
-- a login role of its own that is a member of this one, so a node's
-- credentials reach only its own rows (security.md B4, R15). After
-- `otter admin node add <name>`, which prints it, run as a superuser:
--
--   CREATE ROLE otter_node_<name, hyphens as underscores> LOGIN IN ROLE otter_worker;
--   \password otter_node_<name>
SELECT 'CREATE ROLE otter_worker NOLOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_worker') \gexec

SELECT 'CREATE DATABASE otter OWNER otter_migrator'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'otter') \gexec
