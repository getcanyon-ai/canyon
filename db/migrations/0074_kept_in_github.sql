-- Kept in GitHub (#1926). keeps: Canyon made the app's repository and commits its versions there;
-- kept_commit is the commit main was last known at, Canyon's own or a delivered push, and kept_error
-- why the last version isn't in GitHub. A package's kept_base is kept_commit when it was made current,
-- its commit's parent. A project's keep_error is why its repository couldn't be made.
ALTER TABLE project_repositories ADD COLUMN keeps boolean NOT NULL DEFAULT false;
ALTER TABLE project_repositories ADD COLUMN kept_commit text NOT NULL DEFAULT '';
ALTER TABLE project_repositories ADD COLUMN kept_error text NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN kept_base text NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN keep_error text NOT NULL DEFAULT '';
