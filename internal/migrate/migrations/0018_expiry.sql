-- Expiry and deletion (Q7, Q26, #100). A customer may give an instance an
-- expiry at create, and may mark it ephemeral. The control plane applies
-- the policy, as it records any intent: stopped at expires_at, deleted 7
-- days later after a final dump kept 7 days; an ephemeral instance is
-- deleted at expires_at with no dump. The node's worker runs those
-- operations, and removes expired final dumps and marks them purged.

ALTER TABLE instances ADD COLUMN ephemeral boolean NOT NULL DEFAULT false;

ALTER TABLE backup_records ADD COLUMN purged_at timestamptz;

-- The customer changes or clears an expiry; nothing else about it.
GRANT UPDATE (expires_at) ON instances TO otter_api;

-- The worker records that it removed a dump; the record is otherwise
-- written once and kept (0014).
GRANT UPDATE (purged_at) ON backup_records TO otter_worker;
