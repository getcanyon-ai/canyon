-- The gate pipeline (contracts/snapshot.md, Two gates; contracts/routes.md, the policies row): an edit
-- names the gates it sets — state before preview (a go-live rule's before production, its only gate),
-- production before production, 'off' taking the rule out of that gate — and at least one. In each gate a
-- rule's state is its newest edit naming that gate whose start has passed. An edit that does not name the
-- gate before preview still writes state, as the rule's state there when it was set, so the previous
-- binary, which reads every row's state and no other column, reads it as a change to nothing — but for
-- one case: written while an edit before preview is still in its grace, it supersedes that edit for the
-- previous binary, which then never wakes it. Every row the previous binary writes names state alone.
ALTER TABLE policy_edits ADD COLUMN names_state boolean NOT NULL DEFAULT true;
ALTER TABLE policy_edits ADD COLUMN production text CHECK (production IN ('blocking', 'advisory', 'off'));
ALTER TABLE policy_edits ADD CONSTRAINT policy_edits_names_a_gate CHECK (names_state OR production IS NOT NULL);
