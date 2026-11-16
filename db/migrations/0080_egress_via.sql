-- The host's egress setting (design/EGRESS.md §2.5): one row. via is off until an admin sets canyon;
-- any person's preview deploy (a redeploy included) reads it, and a promotion, a rollback or a patch
-- of that package carries what that deploy recorded.
CREATE TABLE egress_settings (
    one boolean PRIMARY KEY DEFAULT true CHECK (one),
    via text NOT NULL DEFAULT 'off' CHECK (via IN ('off', 'canyon'))
);
INSERT INTO egress_settings DEFAULT VALUES;

-- The egress spec the run that started this row's version used: {"via": "off" | "canyon"}. NULL on a row
-- written before it, which ran with none: via off.
ALTER TABLE deploys ADD COLUMN egress jsonb;

-- The egress spec a deploy run started preview under: what a promotion of its package, and a rollback to
-- it, carry to production — preview is the canary (design/EGRESS.md §2.5). NULL on a run before it: via off.
ALTER TABLE runs ADD COLUMN egress jsonb;
