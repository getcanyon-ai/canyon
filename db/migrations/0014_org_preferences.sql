-- The org's preferences (contracts/routes.md, the preferences row): one org per host, one row.
-- The single row is enforced in SQL — the check admits only true, the primary key admits true
-- once, so a second insert is refused by Postgres and no handler counts rows. No ON DELETE on the
-- foreign key: no route deletes a design system. No updated_at or updated_by: the audit row is the
-- record of who changed what, and when.
CREATE TABLE org_preferences (
    one              boolean PRIMARY KEY DEFAULT true CHECK (one),
    stacks           jsonb NOT NULL DEFAULT '[]',
    design_system_id text REFERENCES design_systems (id)
);
-- The row always exists from here on, so a PUT is a plain UPDATE and a fresh host answers an
-- empty list with no design system.
INSERT INTO org_preferences DEFAULT VALUES;
