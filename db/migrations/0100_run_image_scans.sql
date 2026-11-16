-- The run image GET /builder names (design/PATCHING.md §0.4, §3.2; contracts/executor.md, The run image):
-- its own image scan, kept under its digest and no project's, so the patch plan can tell a run image that
-- carries a fix from one that does not. Only the run image GET /builder answers now is kept. Its diff IDs
-- bottom first, or the executor's not_finished, which the next tick replaces; unplaced is the packages
-- trivy named no layer of it for. rematched_at, rescan_failed_at and rescan_failed are as scanned_images'.
CREATE TABLE run_image_scans (
  digest       text PRIMARY KEY CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  run_image    text NOT NULL,
  scanned_at   timestamptz NOT NULL,
  diff_ids     text[],
  unplaced     jsonb NOT NULL DEFAULT '[]',
  not_finished text,
  reason       text,
  rematched_at     timestamptz,
  rescan_failed_at timestamptz,
  rescan_failed    text,
  CHECK ((diff_ids IS NULL) = (not_finished IS NOT NULL))
);

-- Each of its layers' slices: layer_parts' rows, shared by content with the apps' own, and matched each
-- tick as theirs are. A slice's row goes with its slice: the release before sweeps every slice no app's scan
-- names, this table unknown to it, and a scan missing a layer is read again (runs, rescanRunImage).
CREATE TABLE run_image_layers (
  digest   text NOT NULL REFERENCES run_image_scans (digest) ON DELETE CASCADE,
  diff_id  text NOT NULL CHECK (diff_id ~ '^sha256:[0-9a-f]{64}$'),
  part_sha text NOT NULL REFERENCES layer_parts (part_sha) ON DELETE CASCADE,
  PRIMARY KEY (digest, diff_id)
);
CREATE INDEX run_image_layers_part ON run_image_layers (part_sha);
