-- The deploy repo (design/HAND-OFF.md §3b; contracts/routes.md, the platform-repo row and The deploy repo;
-- #2470): under delivery: repo Canyon commits each release to the company's git repo instead of calling a
-- webhook. One row, one host. The token is sealed as the webhook secret is (0038, vault/) as the row
-- ("platform-repo", "repo", "token"), key_ref the key version its data key is wrapped under; the server's two
-- boot checks and rotate-kek count it. checked_at is the last check of the repo that passed since the last
-- save; a save clears it. token_hash is the row's own host token's SHA-256, which reports and the access-token
-- fetch take beside the platform webhook's, and fetch_token_hash the fetch-only token's, the one External Secrets
-- holds: it opens GET /releases/:id/access-token and nothing else. Both are made by the first save and kept here,
-- never on a platform_webhook row, which this migration leaves as it was (the lead's decision 26, amended: a
-- release before this one boots and serves on a host that saved only a deploy repo).
CREATE TABLE platform_repo (
    id             integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    url            text NOT NULL,
    username       text NOT NULL,
    key_ref        integer NOT NULL,
    sealed_key     bytea NOT NULL,
    sealed_value   bytea NOT NULL,
    preview_branch text NOT NULL,
    prod_branch    text NOT NULL,
    prod_mode      text NOT NULL CHECK (prod_mode IN ('pull_request', 'push')),
    root           text NOT NULL,
    api            text NOT NULL CHECK (api IN ('github', 'none')),
    api_url        text NOT NULL,
    store          text NOT NULL,
    ca             text,
    token_hash     bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    fetch_token_hash bytea NOT NULL CHECK (octet_length(fetch_token_hash) = 32),
    checked_at     timestamptz,
    set_at         timestamptz NOT NULL DEFAULT now(),
    set_by_kind    text NOT NULL,
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL,
    CHECK (api = 'github' OR prod_mode = 'push')
);

-- How releases reach the org's platform (the preferences row's delivery): webhook, as before, or repo.
ALTER TABLE org_preferences ADD COLUMN delivery text NOT NULL DEFAULT 'webhook' CHECK (delivery IN ('webhook', 'repo'));

-- Each release records how it was made, so a remove follows the environment's last deploy's way and never the
-- setting; a repo release records where it landed — the commit, its branch, the tree Canyon rendered at the
-- directory (what a check-in compares the branch with), the pull request it opened — what the templates said
-- (address, endpoints: the check-in's deployed), and the problem code its last attempt ended on.
ALTER TABLE releases ADD COLUMN via text NOT NULL DEFAULT 'webhook' CHECK (via IN ('webhook', 'repo')),
    ADD COLUMN commit_sha text,
    ADD COLUMN branch text,
    ADD COLUMN tree text,
    ADD COLUMN pull_request text,
    ADD COLUMN pull_number integer,
    ADD COLUMN address text,
    ADD COLUMN endpoints jsonb,
    ADD COLUMN repo_problem text;
