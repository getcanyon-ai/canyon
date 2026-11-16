-- Policy edits (contracts/routes.md, PUT /api/v1/policies/:rule): the org's changes to a rule's
-- state, appended and never rewritten. The snapshot is composed from them per request — a rule's
-- effective state is its newest edit whose effective_at has passed, the canonical default when it
-- has none — so nothing here is a foreign key and nothing here is ever updated. grace_days is the
-- edit's, kept for GET /policies; it is not in the snapshot and not in the hash.
CREATE TABLE policy_edits (
    id             bigserial PRIMARY KEY,
    rule           text NOT NULL,                      -- the rule id the snapshot carries; a row naming one it no longer does is not read (a retired rule cannot loosen a gate)
    version        int  NOT NULL CHECK (version > 0),  -- the org's edit count at this write, assigned under the ledger lock: the composed snapshot's version is the boot version plus the newest in effect
    state          text NOT NULL CHECK (state IN ('blocking', 'advisory', 'off')),
    grace_days     int  NOT NULL DEFAULT 0 CHECK (grace_days BETWEEN 0 AND 365),
    effective_at   timestamptz NOT NULL,               -- set_at + grace_days; the instant the edit starts to bite
    set_by_kind    text NOT NULL CHECK (set_by_kind IN ('person', 'agent', 'canyon')),
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL DEFAULT '',
    set_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX policy_edits_rule_set ON policy_edits (rule, set_at DESC); -- the newest edit per rule, for the composer and GET /policies alike
