-- Baseline privileges for the platform database.
--
-- Runs as otter_migrator, which owns the database. The roles themselves are
-- cluster-wide and need a superuser to create, so they come from
-- deploy/platform-db/bootstrap.sql, run once by the operator.
--
-- Least privilege from the start: nobody but the owner can connect or create
-- anything, and each later migration grants otter_api and otter_worker
-- exactly what they use, table by table.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_api')
       OR NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'otter_worker') THEN
        RAISE EXCEPTION 'roles otter_api and otter_worker do not exist: run deploy/platform-db/bootstrap.sql first';
    END IF;

    EXECUTE format('REVOKE ALL ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO otter_api, otter_worker', current_database());
END
$$;

REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO otter_api, otter_worker;

-- Functions are executable by PUBLIC by default; tables and sequences are
-- not granted to anyone by default, so they need no change here.
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
