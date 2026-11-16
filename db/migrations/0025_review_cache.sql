-- The AI reviewer's completed run, kept by what decides it: the package's tree, the policy snapshot,
-- the resolved model id and the agent. A run reviewing the same four replays it, no model call.
-- A failed or skipped review is never kept. Rows are small and never expire; a cleanup is its own ticket.
CREATE TABLE review_cache (
    tree_hash     text NOT NULL,
    snapshot_hash text NOT NULL,
    model_id      text NOT NULL,
    agent         text NOT NULL,
    run           jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tree_hash, snapshot_hash, model_id, agent)
);
