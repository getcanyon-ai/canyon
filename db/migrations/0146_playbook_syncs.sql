-- Playbook syncs (design/specs/mcp.md §3.5; contracts/routes.md, the playbooks row): a session's native skill holding
-- a playbook's version, by a keyed hash of the session under a key derived for each 30-day period and never stored,
-- so no row joins back to a session or a person. One row per holder and version, its last day seen; rows older than
-- 30 days are deleted.
CREATE TABLE playbook_syncs (
    slug    text    NOT NULL,
    version integer NOT NULL,
    holder  bytea   NOT NULL CHECK (octet_length(holder) = 32),
    seen_on date    NOT NULL,
    PRIMARY KEY (slug, version, holder),
    FOREIGN KEY (slug, version) REFERENCES playbook_versions (slug, version)
);
CREATE INDEX playbook_syncs_seen_on ON playbook_syncs (seen_on);
