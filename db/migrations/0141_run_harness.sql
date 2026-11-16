-- The coding agent a review was asked for under, from the CLI's harness markers (#2953): claude_code or codex, NULL
-- when the CLI named none, as an older CLI, the dashboard and a person's terminal do. Counted, never named further,
-- in the usage report's agents.
ALTER TABLE runs ADD COLUMN harness text CHECK (harness IN ('claude_code', 'codex'));
