-- Org playbooks (design/specs/mcp.md §3; contracts/routes.md, the playbooks row): how the org builds, which agents
-- read and Canyon never checks. Every version is kept whole, each save one more: an admin reads them all, a removed
-- playbook's included, and a restore saves an old one's text as the next.
CREATE TABLE playbook_versions (
    slug           text        NOT NULL CHECK (slug ~ '^[a-z][a-z0-9-]{0,39}$'),
    version        integer     NOT NULL CHECK (version >= 1),
    name           text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    kind           text        NOT NULL CHECK (kind IN ('skill', 'note')),
    when_line      text        NOT NULL CHECK (char_length(when_line) BETWEEN 1 AND 200),
    body           text        NOT NULL CHECK (octet_length(body) BETWEEN 1 AND 65536),
    set_by_kind    text        NOT NULL,
    set_by_subject text        NOT NULL,
    set_by_display text        NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (slug, version)
);

-- A removal names the version it removed: a playbook is removed while its newest version has a row here, so a save
-- or a restore after it, the next version, stands again with nothing deleted.
CREATE TABLE playbook_removals (
    slug           text        NOT NULL,
    version        integer     NOT NULL,
    set_by_kind    text        NOT NULL,
    set_by_subject text        NOT NULL,
    set_by_display text        NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (slug, version),
    FOREIGN KEY (slug, version) REFERENCES playbook_versions (slug, version)
);

-- A custom rule's version may point at a playbook, by slug (contracts/routes.md, the custom-rules row): kept per
-- version, and carried in the snapshot only while the playbook stands (contracts/snapshot.md, Custom rules).
ALTER TABLE custom_rule_edits ADD COLUMN playbook text CHECK (playbook ~ '^[a-z][a-z0-9-]{0,39}$');
