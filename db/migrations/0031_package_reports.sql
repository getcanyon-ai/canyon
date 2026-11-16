-- Imported SARIF reports (contracts/finding.md, In: external SARIF; contracts/routes.md, the reports
-- row). The raw report is never stored: its digest, tool, category, driver version, normalised results
-- (without partialFingerprints), drops and attacher are. Every attach is one row, written once and never
-- updated or deleted: a run journals the rows it includes and reads them back by id. A package's current
-- report for a (tool, category) is its row with the highest id, so a report attached again after another
-- (a scan, an upload, the same scan) is current again without an UPDATE, and one digest may be two rows.
CREATE TABLE package_reports (
    id                  bigserial PRIMARY KEY,  -- written under the ledger lock, so the newest attach is the highest id
    project_id          text NOT NULL,
    package_id          text NOT NULL,
    tool                text NOT NULL,
    category            text NOT NULL,
    digest              text NOT NULL CHECK (digest ~ '^sha256:[0-9a-f]{64}$'),
    version             text NOT NULL,
    results             jsonb NOT NULL,
    dropped             integer NOT NULL CHECK (dropped >= 0),
    first_dropped       text,
    attached_by_kind    text NOT NULL CHECK (attached_by_kind IN ('person', 'agent', 'canyon')),
    attached_by_subject text NOT NULL,
    attached_by_display text NOT NULL,
    attached_at         timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (package_id, project_id) REFERENCES packages (id, project_id)  -- a report is its project's package's
);
CREATE INDEX package_reports_current ON package_reports (package_id, tool, category, id);
-- The project's report key (finding.md: 32 random bytes, sealed in the vault, never exported or shown),
-- generated at its first attach and sealed as a secret is (0008, vault/) as the row (project, "report",
-- "key"); report_key_ref is the key version its data key is wrapped under (0017). All three or none.
-- Expand only: nullable, no default, no data movement.
ALTER TABLE projects ADD COLUMN report_key_ref integer, ADD COLUMN report_sealed_key bytea, ADD COLUMN report_sealed_value bytea,
    ADD CHECK ((report_key_ref IS NULL) = (report_sealed_key IS NULL) AND (report_sealed_key IS NULL) = (report_sealed_value IS NULL));
