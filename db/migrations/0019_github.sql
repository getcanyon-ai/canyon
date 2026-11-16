-- The GitHub App: one per host, one row. The key the App signs with and the secret its webhooks
-- are signed with are sealed as a secret is (0008, vault/), each under its own data key, the rows
-- ("github", "app", "private_key") and ("github", "app", "webhook_secret"); key_ref is the key
-- version both data keys are wrapped under (0017). The client secret is not stored: nothing uses it.
CREATE TABLE github_app (
    id                 integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    app_id             bigint NOT NULL,
    slug               text NOT NULL,
    client_id          text NOT NULL,
    key_ref            integer NOT NULL,
    sealed_pem_key     bytea NOT NULL,
    sealed_pem         bytea NOT NULL,
    sealed_hook_key    bytea NOT NULL,
    sealed_hook_secret bytea NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);
-- An installation of the App on an account. An uninstall sets deleted_at; a delivery naming an
-- installation with no live row is refused.
CREATE TABLE github_installations (
    installation_id bigint PRIMARY KEY,
    account         text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);
-- Every delivery accepted, by GitHub's delivery id: a redelivery inserts nothing and starts nothing.
CREATE TABLE github_deliveries (
    delivery_id text PRIMARY KEY,
    event       text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
-- The check run a gate run reports to. reported_at is null until the conclusion is written.
CREATE TABLE github_checks (
    run_id          text PRIMARY KEY REFERENCES runs (id),
    installation_id bigint NOT NULL REFERENCES github_installations (installation_id),
    repo            text NOT NULL,
    sha             text NOT NULL,
    check_run_id    bigint NOT NULL,
    reported_at     timestamptz
);
