-- How and from where each action was done (contracts/routes.md, the audit read): the channel, the person's role
-- when they acted, the session that acted — a digest, never its secret — and the client address. Nullable: a row
-- written before carries none, and a binary before this one writes none.
ALTER TABLE audit_events
    ADD COLUMN via     text CHECK (via IN ('dashboard', 'cli', 'github', 'platform', 'canyon')),
    ADD COLUMN role    text CHECK (role IN ('admin', 'member')),
    ADD COLUMN session text CHECK (session ~ '^ses_[0-9a-f]{16}$'),
    ADD COLUMN source  text CHECK (length(source) BETWEEN 1 AND 64);
