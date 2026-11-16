-- What the newest rescan tick's patch plan said (design/PATCHING.md §4.2, #2588), which the identities read serves
-- (contracts/routes.md, the identities row's plan): why it read no app, or the identity it planned onto and each app it
-- read with its one cause, the contract's words. One row, as org_preferences is, replaced by each tick. Expand only: a
-- table the previous binary never reads.
CREATE TABLE patch_plan_said (
  one      boolean PRIMARY KEY DEFAULT true CHECK (one),
  said_at  timestamptz NOT NULL,
  identity text CHECK (identity ~ '^sha256:[0-9a-f]{64}$'),
  why      text,
  apps     jsonb NOT NULL CHECK (jsonb_typeof(apps) = 'object')
);
