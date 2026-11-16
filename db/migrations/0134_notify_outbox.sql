-- What Canyon posted to the org's chat channels, and what it still has to post (#2725; contracts/routes.md,
-- *Integrations*; design/INTEGRATIONS.md). A channel that Slack or Teams refuses for good is failing until an admin
-- saves it again, which is an admin's need (org_needs, the fleet read). notify_posted is each channel's last word on
-- each need: posted open, with what change it posted at and the message it posted, which its clearing names again, or cleared; cleared_seen_at is when the tick first found it
-- gone, so a need that flaps back within 30 minutes posts nothing. notify_outbox is each message to post, a model
-- naming names only, never a value, leased by the sender and kept 30 days as the record of what was sent.
ALTER TABLE notify_channels ADD COLUMN failing_since timestamptz, ADD COLUMN failing_status integer;

CREATE TABLE notify_posted (
    channel_id      text NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    need_key        text NOT NULL,
    fingerprint     text NOT NULL,
    open            boolean NOT NULL,
    message         jsonb NOT NULL,
    posted_at       timestamptz NOT NULL DEFAULT now(),
    cleared_seen_at timestamptz,
    PRIMARY KEY (channel_id, need_key)
);

CREATE TABLE notify_outbox (
    id          bigserial PRIMARY KEY,
    channel_id  text NOT NULL REFERENCES notify_channels (id) ON DELETE CASCADE,
    message     jsonb NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    next_at     timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    attempts    integer NOT NULL DEFAULT 0,
    status      text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sent', 'dropped')),
    answered    integer,
    done_at     timestamptz
);
CREATE INDEX notify_outbox_due ON notify_outbox (next_at) WHERE status = 'queued';
