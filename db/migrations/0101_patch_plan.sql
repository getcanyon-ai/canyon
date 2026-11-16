-- The patch plan (design/PATCHING.md §4.2, §4.3; contracts/routes.md, Patch runs and the identities row):
-- runs of kind patch, the platform identities Canyon patches onto, each app's place in an identity's waves,
-- and the rebases refused for good. Nothing here is a secret: digests, project ids and states.
ALTER TABLE runs DROP CONSTRAINT runs_kind_check, ADD CONSTRAINT runs_kind_check CHECK (kind IN ('gate', 'promote', 'patch'));

-- One platform identity per target run image digest, as GET /builder named it: the wave whose watches run
-- now, 0 the canary to 3 the rest. counted_since is when its breaker last counted afresh: from its first
-- plan, then an admin's resume. paused while two reverts or failed starts since tripped it, stalled while
-- one did, each with the projects that did.
CREATE TABLE patch_identities (
  id               text PRIMARY KEY CHECK (id ~ '^sha256:[0-9a-f]{64}$'),
  run_image        text NOT NULL,
  planned_at       timestamptz NOT NULL DEFAULT now(),
  wave             integer NOT NULL DEFAULT 0 CHECK (wave BETWEEN 0 AND 3),
  counted_since    timestamptz NOT NULL DEFAULT now(),
  paused_at        timestamptz,
  paused_projects  text[],
  stalled_at       timestamptz,
  stalled_projects text[],
  CHECK ((paused_at IS NULL) = (paused_projects IS NULL)),
  CHECK ((stalled_at IS NULL) = (stalled_projects IS NULL))
);

-- Each app an identity's plan named: the image its preview ran when planned, the wave that took it (NULL
-- until one does), what became of its patch, and the newest patch run of it. no_canary once a watch of
-- it ended early: it is planned again, never as the identity's canary.
CREATE TABLE patch_wave_apps (
  identity   text NOT NULL REFERENCES patch_identities (id) ON DELETE CASCADE,
  project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  image      text NOT NULL,
  wave       integer CHECK (wave BETWEEN 0 AND 3),
  outcome    text NOT NULL DEFAULT 'planned'
    CHECK (outcome IN ('planned', 'running', 'passed', 'ended_early', 'reverted', 'failed_start', 'refused', 'failed')),
  run        text REFERENCES runs (id) ON DELETE SET NULL,
  outcome_at timestamptz NOT NULL DEFAULT now(),
  no_canary  boolean NOT NULL DEFAULT false,
  PRIMARY KEY (identity, project_id)
);

-- A rebase of an image onto a run image refused for what the images are (contracts/executor.md, POST
-- /rebase): permanent for the pair, so the plan never plans it again (#2089).
CREATE TABLE patch_refusals (
  project_id text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  digest     text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  run_image  text NOT NULL,
  reason     text NOT NULL CHECK (reason IN ('not_rebasable', 'another_run_image', 'another_stack')),
  refused_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, digest, run_image)
);
