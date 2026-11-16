-- A destination's latest call Canyon refused while adding a route's credential (contracts/routes.md, Interception,
-- A record; #2692): the reason and when the receiver took its entry, both or neither, so canyon status can name it.
ALTER TABLE egress_destinations
    ADD COLUMN last_credential_reason text
        CHECK (last_credential_reason IN ('inject_requires_tls', 'inject_unavailable', 'misdirected', 'upstream_failed')),
    ADD COLUMN last_credential_refused_at timestamptz,
    ADD CONSTRAINT egress_destinations_credential_refusal_check
        CHECK ((last_credential_reason IS NULL) = (last_credential_refused_at IS NULL));
