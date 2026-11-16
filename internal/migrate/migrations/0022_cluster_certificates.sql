-- Each cluster's certificate, as its node's worker last read it from the
-- jail (#171): observed state, so the edge can pin it on its hop to the
-- cluster's PgBouncer instead of skipping verification. NULL until the
-- worker has read it; the edge routes nothing to the cluster meanwhile.
ALTER TABLE clusters ADD COLUMN server_cert text
    CHECK (server_cert LIKE '-----BEGIN CERTIFICATE-----%');

GRANT UPDATE (server_cert) ON clusters TO otter_worker;
