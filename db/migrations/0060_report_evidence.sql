-- Canyon-run scan evidence (contracts/finding.md, Canyon-run tools, Evidence): the image that ran, by
-- digest, and for trivy and grype the date their vulnerability database was built. NULL on an upload,
-- which no image of Canyon's ran (#1114).
-- First shipped as 0058_report_evidence.sql beside 0058_build_digest.sql: a database that applied it
-- under that name runs it again under this one, so each column is added only if it is missing.
ALTER TABLE package_reports ADD COLUMN IF NOT EXISTS image text, ADD COLUMN IF NOT EXISTS db_built_at timestamptz;
