-- A pull request's head is reviewed as a package that is never the project's current one:
-- source_kind 'repo-pr', which every read of "current" leaves out. Expand only: the check widens.
ALTER TABLE packages DROP CONSTRAINT packages_source_kind_check;
ALTER TABLE packages ADD CONSTRAINT packages_source_kind_check CHECK (source_kind IN ('upload', 'repo', 'repo-pr'));
