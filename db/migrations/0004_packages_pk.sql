-- A package id is per project (ingest picks the shortest prefix unused in that project's repo),
-- so the same tree uploaded to two projects is two rows with one id. Runs already tie a package
-- to its project through UNIQUE (id, project_id), and both columns are NOT NULL: that is the key.
ALTER TABLE packages DROP CONSTRAINT packages_pkey;
