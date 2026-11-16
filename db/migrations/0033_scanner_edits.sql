-- Canyon-run tools (contracts/routes.md, the policies row; contracts/snapshot.md): the org's changes to
-- whether Canyon runs trivy, opengrep, grype or checkov on this host and with which settings, appended
-- and never rewritten, as judgement_edits are for modes. Each row is the tool's run and settings as the
-- edit leaves them, both, so a tool's are its newest row's, and not run with no settings when it has
-- none. In effect once written. Its own table for judgement_edits' reason: such an edit has no state.
CREATE TABLE scanner_edits (
    id       bigserial PRIMARY KEY,  -- written under the ledger lock, so the newest row is the highest id
    rule     text    NOT NULL CHECK (rule IN ('sarif.trivy', 'sarif.opengrep', 'sarif.grype', 'sarif.checkov')),
    run      boolean NOT NULL,
    settings jsonb   NOT NULL CHECK (jsonb_typeof(settings) = 'object'),  -- {} is none; the route checks each tool's allow-list
    set_at   timestamptz NOT NULL DEFAULT now()
);
