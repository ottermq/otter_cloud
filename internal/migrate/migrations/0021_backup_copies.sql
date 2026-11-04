-- Copies on the backup target (#118, architecture.md §10). The worker seals
-- each dump on its node and copies it to the node's target; the copy is a
-- backup_records row of its own, with the dump's kind, the target's name,
-- and the dump it copies, whose expiry it takes. Retention removes it from
-- the target when it expires, as it does the dump from the node.
ALTER TABLE backup_records ADD COLUMN source_id uuid REFERENCES backup_records (id);

-- A copy has a source and a target other than the node; a dump on the node
-- has neither.
ALTER TABLE backup_records ADD CONSTRAINT backup_records_copy
    CHECK ((source_id IS NULL) = (target = 'local'));

-- One good copy of a dump on each target.
CREATE UNIQUE INDEX backup_records_copied ON backup_records (source_id, target) WHERE status = 'ok';
