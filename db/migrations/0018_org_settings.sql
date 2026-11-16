-- The organisation's settings on the preferences row (design/ONBOARDING.md §1): the values that
-- were environment variables (design/AUTH.md §3), which stay as bootstrap and override. Expand
-- only: every column defaulted or nullable, so the previous binary's row still reads and writes.
ALTER TABLE org_preferences
    ADD COLUMN org_name        text   NOT NULL DEFAULT '',
    ADD COLUMN allowed_domains text[] NOT NULL DEFAULT '{}',   -- bare lower-case domains, each with a dot
    ADD COLUMN allowed_emails  text[] NOT NULL DEFAULT '{}',   -- lower-case addresses; membership is the union of the two
    ADD COLUMN admin_emails    text[] NOT NULL DEFAULT '{}',
    ADD COLUMN admin_claim     text,                           -- with admin_value: the one claim and value that mean admin, both or neither
    ADD COLUMN admin_value     text,
    ADD COLUMN onboarded_at    timestamptz;                    -- null while the host is in the setup state (ONBOARDING.md §2)
-- An upgrade is not a setup. A host somebody has used — an audit trail, or projects that predate
-- the audit table — is not a host somebody needs to onboard, and leaving it null would drop a
-- working install into the setup state. A database nothing has touched is a fresh install: the
-- row stays null and the first bootstrap admin is shown onboarding.
UPDATE org_preferences SET onboarded_at = now() WHERE EXISTS (SELECT 1 FROM audit_events) OR EXISTS (SELECT 1 FROM projects);
