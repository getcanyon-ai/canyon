-- The design-system registry (contracts/routes.md, the design-systems row). Canyon ships two rows
-- and a person registers the rest; source_type says which, and no route edits or deletes a row, so
-- the shipped two are unchangeable. No project column: the per-project override is not in v1, and
-- org_preferences.design_system_id with its foreign key arrives with the preferences row.
CREATE TABLE design_systems (
    id           text PRIMARY KEY,
    name         text NOT NULL,
    namespace    text NOT NULL,
    source_type  text NOT NULL CHECK (source_type IN ('shipped', 'registry')),
    registry_url text NOT NULL,
    tokens_url   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   text NOT NULL
);
-- Two rows under one namespace make the resolved system ambiguous, so the namespace is unique
-- across both kinds: Postgres compares bytes, which is the contract's "matched literally".
CREATE UNIQUE INDEX design_systems_namespace ON design_systems (namespace);
-- The two Canyon ships. Canyon's own registry_url is a placeholder until the registry is served;
-- the seeded row moves with the serving route, in that route's migration.
INSERT INTO design_systems (id, name, namespace, source_type, registry_url, created_at, created_by) VALUES
    ('ds_0001', 'shadcn/ui', '@shadcn/ui', 'shipped', 'https://ui.shadcn.com/r',                timestamptz '2026-01-05T09:00:00Z', 'canyon'),
    ('ds_0002', 'Canyon',    '@canyon/ui', 'shipped', 'https://canyon.example/registry/canyon', timestamptz '2026-01-05T09:00:00Z', 'canyon');
