-- A pull request's package names its pull request (#1947): its number and head branch, so the runs and
-- preview, which a person may deploy it to, say "PR #12 (fix-login)". 0 and '' on every other package.
ALTER TABLE packages ADD COLUMN pr_number integer NOT NULL DEFAULT 0;
ALTER TABLE packages ADD COLUMN pr_branch text NOT NULL DEFAULT '';
