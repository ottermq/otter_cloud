-- One-time setup of the platform database's roles and database.
--
-- Run once per PostgreSQL server, as a superuser:
--
--   psql -v ON_ERROR_STOP=1 -f deploy/platform-db/bootstrap.sql
--
-- Then give each login role a password with psql's \password (it sends only
-- the SCRAM verifier, so the password never reaches the server's logs), and
-- write each password to a 0400 file owned by the process that uses it,
-- named by OTTER_DATABASE_PASSWORD_FILE:
--
--   \password otter_migrator     -- otter migrate
--   \password otter_api          -- otter api, otter admin
--
-- After that, `otter migrate` as otter_migrator does the rest.

-- Owns the database and every table in it; the only role that runs DDL.
CREATE ROLE otter_migrator LOGIN;

-- The control plane's API. Gets only what each migration grants it.
CREATE ROLE otter_api LOGIN;

-- The group every node's worker role joins. It cannot log in: each node gets
-- a login role of its own that is a member of this one, so a node's
-- credentials reach only its own rows (security.md B4, R15).
CREATE ROLE otter_worker NOLOGIN;

CREATE DATABASE otter OWNER otter_migrator;
