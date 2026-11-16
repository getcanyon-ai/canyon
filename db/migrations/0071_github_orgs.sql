-- One App per GitHub organization (#1914): an App registered from a personal account can be installed
-- only there, so each org registers its own, keyed by GitHub's App id. account is the org that owns it;
-- an App registered before this names the account it is first installed on, '' until then.
-- keeps_code is what GitHub granted (contents: write), imports_new the admin's answer at registration.
-- id stays for the binary before this one, which inserts without naming it and on conflict with it
-- (expand only): its App is id 1, the one App a host had; the server from here on writes NULL.
ALTER TABLE github_app
    DROP CONSTRAINT github_app_pkey,
    DROP CONSTRAINT github_app_id_check,
    ALTER COLUMN id DROP NOT NULL,
    ADD PRIMARY KEY (app_id),
    ADD COLUMN account     text NOT NULL DEFAULT '',
    ADD COLUMN keeps_code  boolean NOT NULL DEFAULT false,
    ADD COLUMN imports_new boolean NOT NULL DEFAULT false;
CREATE UNIQUE INDEX github_app_id ON github_app (id);
CREATE UNIQUE INDEX github_app_account ON github_app (lower(account)) WHERE account <> '';
CREATE UNIQUE INDEX github_app_one_keeper ON github_app (keeps_code) WHERE keeps_code;

-- An installation and a delivery are one App's: the delivery's X-GitHub-Hook-Installation-Target-ID.
-- A row the binary before this one writes names none, and is the App it knew, id 1.
CREATE FUNCTION github_first_app() RETURNS bigint LANGUAGE sql STABLE AS 'SELECT app_id FROM github_app WHERE id = 1';
ALTER TABLE github_installations ADD COLUMN app_id bigint REFERENCES github_app (app_id) DEFAULT github_first_app();
ALTER TABLE github_deliveries ADD COLUMN app_id bigint REFERENCES github_app (app_id) DEFAULT github_first_app();
UPDATE github_installations SET app_id = github_first_app();
UPDATE github_deliveries SET app_id = github_first_app();
UPDATE github_app SET account = coalesce((SELECT account FROM github_installations ORDER BY created_at LIMIT 1), '');
