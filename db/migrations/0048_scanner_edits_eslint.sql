-- ESLint is a tool Canyon runs (contracts/routes.md, the policies row; #1376), so an edit may set its run
-- and settings. Widening the check keeps every value the previous binary writes.
ALTER TABLE scanner_edits DROP CONSTRAINT scanner_edits_rule_check;
ALTER TABLE scanner_edits ADD CONSTRAINT scanner_edits_rule_check CHECK (rule IN ('sarif.trivy', 'sarif.opengrep', 'sarif.eslint', 'sarif.grype', 'sarif.checkov'));
