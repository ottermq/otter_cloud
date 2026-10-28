-- The worker records what its operations did to an instance (#134).
--
-- Only the observed state: never which account owns the instance, what it
-- is allocated, where it lives, or what the customer asked for
-- (desired_state). Row-level security (0012) keeps the worker to its
-- node's instances. The public endpoint is the edge's, from M5.
GRANT UPDATE (status, health, reason) ON instances TO otter_worker;
