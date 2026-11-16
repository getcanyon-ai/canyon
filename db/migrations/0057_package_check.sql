-- A check's upload (canyon check, #1251) is reviewed as a package that is never the project's current
-- one, as a pull request's is: source_kind 'check', which every read of "current" leaves out with
-- 'repo-pr'. Expand only: the check widens.
ALTER TABLE packages DROP CONSTRAINT packages_source_kind_check;
ALTER TABLE packages ADD CONSTRAINT packages_source_kind_check CHECK (source_kind IN ('upload', 'repo', 'repo-pr', 'check'));
