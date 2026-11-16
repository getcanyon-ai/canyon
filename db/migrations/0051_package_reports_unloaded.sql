-- The rules an attached report says its tool could not load (#1383; contracts/finding.md, In a run):
-- Opengrep's Rule parse error names one it dropped while it ran the rest, and the run names each as not
-- checked. A report attached before this release named none.
ALTER TABLE package_reports ADD COLUMN unloaded text[] NOT NULL DEFAULT '{}';
