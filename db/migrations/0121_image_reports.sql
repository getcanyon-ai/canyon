-- Imported image reports (design/PATCHING.md §3.4; contracts/routes.md, PUT …/images/:digest/reports): one tool's report
-- of one image of a project, per category, as platform.known-vuln reads it when its settings.source names the tool.
-- Names, versions and ids alone, never a report's prose: each result's vulnerability, severity and package.
CREATE TABLE image_reports (
  project_id          text NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
  digest              text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
  tool                text NOT NULL,
  category            text NOT NULL DEFAULT '',
  report_digest       text NOT NULL CHECK (report_digest ~ '^sha256:[0-9a-f]{64}$'),
  version             text NOT NULL,
  attached_by_kind    text NOT NULL,
  attached_by_subject text NOT NULL,
  attached_by_display text NOT NULL,
  attached_at         timestamptz NOT NULL DEFAULT now(),
  results             jsonb NOT NULL,
  dropped             integer NOT NULL DEFAULT 0 CHECK (dropped >= 0),
  first_dropped       text,
  PRIMARY KEY (project_id, digest, tool, category)
);

-- platform.known-vuln's settings {source} are written as a tool's run and settings are (its run always false).
-- Widening the check keeps every value the previous binary writes.
ALTER TABLE scanner_edits DROP CONSTRAINT scanner_edits_rule_check;
ALTER TABLE scanner_edits ADD CONSTRAINT scanner_edits_rule_check CHECK (rule IN ('sarif.trivy', 'sarif.opengrep', 'sarif.eslint', 'sarif.grype', 'sarif.checkov', 'platform.known-vuln'));
