-- AI judgement modes (contracts/routes.md, the policies row; contracts/snapshot.md, judgement): the org's
-- changes to the mode of the two rules that take one, appended and never rewritten, as policy_edits are
-- for states. A mode has no grace and is in effect once written, so a rule's mode is its newest row, and
-- advise when it has none. Its own table rather than a column on policy_edits: a mode-only edit has no
-- state, and a NULL state there would fail the previous binary's composer while both run.
CREATE TABLE judgement_edits (
    id     bigserial PRIMARY KEY,  -- written under the ledger lock, so the newest row is the highest id
    rule   text NOT NULL,
    mode   text NOT NULL,
    set_at timestamptz NOT NULL DEFAULT now(),
    CHECK ((rule = 'gate.unreadable' AND mode IN ('advise', 'off'))
        OR (rule = 'auth.unauthenticated-route' AND mode IN ('advise', 'approve', 'off')))  -- the pairs the route offers
);
