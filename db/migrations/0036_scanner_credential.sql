-- The credential the server reads a private Opengrep rules repo with (contracts/routes.md, the scanner
-- credential row; finding.md, Canyon-run tools): one, bound to the repo it was set for, its token sealed
-- as a secret is (0008, vault/) as the row ("scanner", "opengrep", "token"); key_ref is the key version
-- its data key is wrapped under (0017). The username is no secret: the audit row names it.
CREATE TABLE scanner_credentials (
    tool           text PRIMARY KEY CHECK (tool = 'opengrep'),
    repo           text NOT NULL,
    username       text NOT NULL,
    key_ref        integer NOT NULL,
    sealed_key     bytea NOT NULL,
    sealed_value   bytea NOT NULL,
    set_at         timestamptz NOT NULL DEFAULT now(),
    set_by_kind    text NOT NULL,
    set_by_subject text NOT NULL,
    set_by_display text NOT NULL
);
