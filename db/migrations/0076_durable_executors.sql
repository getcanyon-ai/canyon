-- One row per running canyon-server: its run driver's executor id, written before it launches, kept
-- fresh while it runs and deleted once it has stopped (#1715). A live server puts back on their queues
-- the journaled runs of an executor whose row is gone, or stale while its own stayed fresh.
CREATE TABLE durable_executors (
    id      text PRIMARY KEY,
    seen_at timestamptz NOT NULL DEFAULT now()
);
