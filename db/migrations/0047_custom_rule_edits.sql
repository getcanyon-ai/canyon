-- Custom rules (contracts/snapshot.md, Custom rules; contracts/routes.md, the custom-rules row): every
-- saved version of an org's own rule, appended and never rewritten. A rule's newest version is the live
-- one, the older rows its history; its state is policy_edits', as every rule's is. There is no delete:
-- Off retires a rule.
CREATE TABLE custom_rule_edits (
    id             bigserial PRIMARY KEY,
    rule           text NOT NULL CHECK (rule ~ '^custom\.[a-z][a-z-]{0,39}$'),
    version        int  NOT NULL CHECK (version > 0),
    name           text NOT NULL,
    instructions   text NOT NULL,
    set_by_kind    text NOT NULL CHECK (set_by_kind IN ('person', 'agent', 'canyon')),
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL DEFAULT '',
    set_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (rule, version)
);
