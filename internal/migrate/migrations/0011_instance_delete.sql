-- Deleting a database (#46): otter api records the intent, desired_state
-- 'deleted', and queues a delete operation; the worker does the rest (M3).
-- otter api may change nothing else about an instance.

GRANT UPDATE (desired_state) ON instances TO otter_api;
