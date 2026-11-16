-- Who allowed each allowlist entry and when, and the latest refusal of the requests that Allow closed (#2722;
-- contracts/routes.md, the egress read's allows[]): canyon status and canyon logs say a destination was allowed
-- and that its calls go through now, so an agent doesn't read the app's failures from before the Allow as
-- current. Expand only: an entry allowed before this records none, as nothing recorded who allowed it, and the
-- previous binary's inserts still write.
ALTER TABLE egress_allowed
    ADD COLUMN allowed_by_kind    text CHECK (allowed_by_kind = 'person'),
    ADD COLUMN allowed_by_subject text,
    ADD COLUMN allowed_by_display text,
    ADD COLUMN allowed_at         timestamptz,
    ADD COLUMN last_refused_at    timestamptz,
    ADD CONSTRAINT egress_allowed_by_check CHECK ((allowed_at IS NULL) = (allowed_by_kind IS NULL)
        AND (allowed_at IS NULL) = (allowed_by_subject IS NULL) AND (allowed_at IS NULL) = (allowed_by_display IS NULL)
        AND (last_refused_at IS NULL OR allowed_at IS NOT NULL));
