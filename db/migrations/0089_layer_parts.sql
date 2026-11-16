-- Canyon's layers stored by content, read as the project's own (#2075; contracts/finding.md, Canyon's
-- layers): what trivy lists in a layer depends on the whole image, and two apps may run one digest, so
-- 0087's rows keyed by digest or diff ID alone let one app's scan decide what another reads. Nothing reads
-- or writes 0087's image_scans, layer_components and layer_matches any more; a release carries them, so
-- they stay until a later one drops them, expand only (db/expand_contract_test.go).

-- One image scan per project and image digest, the project the deploying run's: its diff IDs bottom first
-- and its lifecycle label as the image carried it, or the executor's not_finished, which the next deploy or
-- rescan tick replaces. unplaced is the packages trivy named no layer of it for. rematched_at is when every
-- layer of it was last matched; rescan_failed_at and rescan_failed when and why a later attempt did not
-- match them all, so its read says it could not check until one does. seen_at is when a deploy or a tick
-- last found the scan stored and stood on it: a tick's sweep drops none seen since it began.
CREATE TABLE scanned_images (
  project_id   text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  digest       text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  scanned_at   timestamptz NOT NULL,
  diff_ids     text[],
  lifecycle    text,
  unplaced     jsonb NOT NULL DEFAULT '[]',
  not_finished text,
  reason       text,
  rematched_at     timestamptz,
  rescan_failed_at timestamptz,
  rescan_failed    text,
  seen_at          timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (project_id, digest),
  CHECK ((diff_ids IS NULL) = (not_finished IS NOT NULL))
);

-- A layer's slice as the re-match sends it: the layer's components and the image's operating-system
-- component, stored once under the sha256 of that slice. Shared by content alone: a slice is its bytes.
-- seen_at is when a scan last stored it: a tick's sweep drops none stored since it began.
CREATE TABLE layer_parts (
  part_sha   text PRIMARY KEY CHECK (part_sha ~ '^sha256:[0-9a-f]{64}$'),
  os         jsonb,
  components jsonb NOT NULL,
  seen_at    timestamptz NOT NULL DEFAULT now()
);

-- Each scan's own slice of each of its image's layers.
CREATE TABLE image_layers (
  project_id text NOT NULL,
  digest     text NOT NULL,
  diff_id    text NOT NULL CHECK (diff_id ~ '^sha256:[0-9a-f]{64}$'),
  part_sha   text NOT NULL REFERENCES layer_parts (part_sha),
  PRIMARY KEY (project_id, digest, diff_id),
  FOREIGN KEY (project_id, digest) REFERENCES scanned_images (project_id, digest) ON DELETE CASCADE
);
CREATE INDEX image_layers_part ON image_layers (part_sha);

-- A slice's re-match on one database build: a function of the slice and the database alone, read by
-- every scan naming the slice. The newest is the one read, and the only one kept.
CREATE TABLE part_matches (
  part_sha    text NOT NULL REFERENCES layer_parts (part_sha) ON DELETE CASCADE,
  db_built_at timestamptz NOT NULL,
  matched_at  timestamptz NOT NULL,
  report      jsonb NOT NULL,
  unscanned   text[] NOT NULL DEFAULT '{}',
  PRIMARY KEY (part_sha, db_built_at)
);
