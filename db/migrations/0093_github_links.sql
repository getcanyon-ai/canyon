-- A person links their own GitHub account (#2228): they sign in to GitHub through Canyon's App, which
-- proves the account is theirs, and a push by it is theirs from then on. Linking needs the App's client
-- secret, which Canyon now keeps sealed beside its key, as ("github", "app", "client_secret") under the
-- row's key_ref; an App registered before this has none, and links nothing.
ALTER TABLE github_app
    ADD COLUMN sealed_client_key    bytea,
    ADD COLUMN sealed_client_secret bytea,
    ADD CONSTRAINT github_app_client_secret CHECK ((sealed_client_key IS NULL) = (sealed_client_secret IS NULL));

-- A person's link to a GitHub account by GitHub's numeric id, which a renamed login keeps; login is the
-- account's as it was when linked, for display. A person may link more than one account, and an account
-- may be linked to more than one person: each link binds only the person who made it.
CREATE TABLE github_links (
    github_id bigint NOT NULL,
    subject   text NOT NULL,
    login     text NOT NULL,
    linked_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (github_id, subject)
);
CREATE INDEX github_links_subject ON github_links (subject);

-- The numeric id of the GitHub account whose push started a run; null for every other run.
ALTER TABLE runs ADD COLUMN started_by_github bigint;
