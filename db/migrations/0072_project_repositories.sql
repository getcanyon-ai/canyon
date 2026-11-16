-- An app's link to a GitHub repository (#1917): by GitHub's repository id and a folder, "" its root,
-- never by the repository's name. One link per app, one app per (repository, folder). full_name is
-- the name GitHub last gave the repository, shown and never matched on; installation_id the one that
-- sees it; state what GitHub last said of it; last_tree the folder's GitHub tree id at the link's
-- last push, so a push that left the folder alone packages nothing.
CREATE TABLE project_repositories (
    project_id      text PRIMARY KEY REFERENCES projects (id),
    repo_id         bigint NOT NULL,
    folder          text NOT NULL DEFAULT '',
    installation_id bigint NOT NULL REFERENCES github_installations (installation_id),
    full_name       text NOT NULL,
    state           text NOT NULL DEFAULT 'linked' CHECK (state IN ('linked', 'deleted', 'archived', 'removed')),
    last_tree       text NOT NULL DEFAULT '',
    linked_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (repo_id, folder)
);

-- The org an App is installed on is GitHub's numeric account id, never its login, which an org renames
-- (#1914's security review, P3-b): set at registration, and taken by an App from before this at its
-- first installation on the org of its login.
ALTER TABLE github_app ADD COLUMN account_id bigint;
CREATE UNIQUE INDEX github_app_account_id ON github_app (account_id);
