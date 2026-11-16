-- Canyon's layers (design/PATCHING.md §3, contracts/finding.md, Canyon's layers): what an image's SBOM
-- listed, stored per layer and shared by every image holding that layer, and each layer's matches, so
-- platform.known-vuln reads each layer's newest match for the digest preview runs. Nothing here is a
-- secret: package names and versions, trivy's own output.

-- One image scan per image digest: its diff IDs bottom first and its lifecycle label as the image
-- carried it, or the executor's not_finished. unplaced is the packages trivy named no layer of it for.
CREATE TABLE image_scans (
  digest       text PRIMARY KEY CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  scanned_at   timestamptz NOT NULL,
  diff_ids     text[],
  lifecycle    text,
  unplaced     jsonb NOT NULL DEFAULT '[]',
  not_finished text,
  reason       text,
  CHECK ((diff_ids IS NULL) = (not_finished IS NOT NULL))
);

-- A layer's components as trivy wrote them, and the image's operating-system component trivy needs
-- to match its os packages (contracts/executor.md, target sbom).
CREATE TABLE layer_components (
  diff_id    text PRIMARY KEY CHECK (diff_id ~ '^sha256:[0-9a-f]{64}$'),
  os         jsonb,
  components jsonb NOT NULL
);

-- A layer's re-match on one database build: trivy's JSON report and the bom-refs it lists but never
-- matches. A rescan on a newer database adds a row; the newest is the one read.
CREATE TABLE layer_matches (
  diff_id     text NOT NULL REFERENCES layer_components (diff_id) ON DELETE CASCADE,
  db_built_at timestamptz NOT NULL,
  matched_at  timestamptz NOT NULL,
  report      jsonb NOT NULL,
  unscanned   text[] NOT NULL DEFAULT '{}',
  PRIMARY KEY (diff_id, db_built_at)
);

-- The values-only HMAC (design/PATCHING.md §5, contracts/routes.md, the deploys row): over the project
-- and preview's where: build names and values alone, under a key of its own derived from the vault's,
-- with that key's version. Stored wherever the build digest is. NULL on a row written before it.
ALTER TABLE runs ADD COLUMN values_hmac text, ADD COLUMN values_key integer;
ALTER TABLE deploys ADD COLUMN values_hmac text, ADD COLUMN values_key integer;
