-- Nightly dumps (architecture.md §10, #117). The worker dumps each running
-- database on its node once a day, kept 7 days, the first of each week 28;
-- they are backup_records of kind 'dump', on the node like the final dumps
-- (0014). A delete shortens its database's nightly dumps to its final
-- dump's expiry, and an ephemeral database's to the delete itself, so none
-- outlives what the customer was told.

-- The worker shortens an expiry at a delete; the record is otherwise
-- written once and kept (0014, 0018).
GRANT UPDATE (expires_at) ON backup_records TO otter_worker;

-- Shortens, never lengthens: a record's expiry only comes forward, and one
-- that has an expiry keeps one. Retention may be hurried, never undone.
CREATE FUNCTION backup_records_expiry_forward() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.expires_at IS DISTINCT FROM OLD.expires_at
       AND (NEW.expires_at IS NULL OR NEW.expires_at > OLD.expires_at) THEN
        RAISE EXCEPTION 'permission denied: a backup record''s expiry only comes forward'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END $$;

CREATE TRIGGER expiry_forward BEFORE UPDATE OF expires_at ON backup_records
    FOR EACH ROW EXECUTE FUNCTION backup_records_expiry_forward();
