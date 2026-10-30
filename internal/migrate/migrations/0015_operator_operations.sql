-- Operators list instances and operations, retry a failed operation and
-- cancel one that never ran (`otter admin ops`, `otter admin instances
-- list`, #102). Row-level security already lets otter_admin see every row
-- (0012); these are the columns it may read and change.

GRANT SELECT ON instances, operations TO otter_admin;

-- Retry puts a failed operation back in the queue; cancel ends a pending
-- one. Never which instance or node it is for, what it does, or who asked.
GRANT UPDATE (status, attempts, next_attempt_at, locked_by, locked_until, payload, reason, finished_at)
    ON operations TO otter_admin;

-- What a retry or a cancel means for the instance: deleting again, deleted
-- (a create canceled before it ran), or the customer's delete withdrawn.
GRANT UPDATE (status, reason, desired_state, deleted_at) ON instances TO otter_admin;
