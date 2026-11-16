-- Telemetry (#2807; contracts/routes.md, Telemetry): the one row a server's daily report reads and claims.
-- instance_id is random and names nothing; DELETE FROM telemetry resets it, and the next read makes a new one.
-- Nothing is sent until an admin has seen what is sent (seen_at), and never while it is off.
CREATE TABLE telemetry (
    one         boolean PRIMARY KEY DEFAULT true CHECK (one),        -- one row, ever
    instance_id uuid NOT NULL DEFAULT gen_random_uuid(),
    off_because text CHECK (off_because IN ('admin', 'unreachable')), -- the environment's off is never stored: it is read at each send
    seen_at     timestamptz,                                         -- when an admin first saw what is sent
    claimed_at  timestamptz,                                         -- the day the last send claimed, reached or not: one try a day
    sent_at     timestamptz,                                         -- the last send the collector answered
    last_report text                                                 -- what that send carried, byte for byte (jsonb would reorder it), as Settings › Telemetry shows it
);
