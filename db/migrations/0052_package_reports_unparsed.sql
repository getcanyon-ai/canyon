-- The files a Canyon-run tool did not read whole, as the executor's scan answer carries them (#1426;
-- contracts/executor.md, the scan row's unparsed): Opengrep's JSON errors' paths, stored with its report,
-- and the run names each that is a file of the package as not checked (finding.md, In a run). A report
-- attached before this release, or by an upload, carries none.
ALTER TABLE package_reports ADD COLUMN unparsed text[] NOT NULL DEFAULT '{}';
