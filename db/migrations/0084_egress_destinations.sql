-- An app's calls out become one row per destination an app env calls (design/EGRESS.md §2.4, #2222): the
-- minute rows and their hourly roll-ups go, their daily partitions with them. They never held a row: egress
-- is inert until the executors route an app through Envoy.
DROP TABLE egress_minutes;
DROP TABLE egress_hours;

-- One row per (app, env, destination, port) — when the receiver took the first and the latest call's entry,
-- the calls since, and the latest call's result — never a path, a query, a header value, a body or a
-- resolved address, which the receiver drops on receipt. At most 500 an app env, invalid not among them; a
-- row unseen for 30 days is deleted. A call no app is named for is no row, only a gap. No foreign key:
-- records are evidence, and the receiver writes what the executor named.
CREATE TABLE egress_destinations (
    app                 text NOT NULL,
    env                 text NOT NULL CHECK (env IN ('preview', 'prod')),
    destination         text NOT NULL,
    port                integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    first_seen_at       timestamptz NOT NULL,
    last_seen_at        timestamptz NOT NULL CHECK (last_seen_at >= first_seen_at),
    calls               bigint NOT NULL CHECK (calls >= 1),
    last_outcome        text NOT NULL CHECK (last_outcome IN ('allowed', 'refused')),
    last_status_class   text NOT NULL CHECK (last_status_class IN ('1xx', '2xx', '3xx', '4xx', '5xx', 'none')),
    last_protocol       text NOT NULL CHECK (last_protocol IN ('connect', 'http', 'https')),
    last_response_flags text[] NOT NULL,
    PRIMARY KEY (app, env, destination, port)
);
-- The age: a row unseen for 30 days.
CREATE INDEX egress_destinations_last_seen ON egress_destinations (last_seen_at);

-- over_cap: calls to destinations new to an app env already holding 500, the one gap that names its app env,
-- one row an app env, extended by every burst past the cap. id names the one row a continuing gap extends.
ALTER TABLE egress_gaps
    ADD COLUMN id bigserial PRIMARY KEY,
    ADD COLUMN app text,
    ADD COLUMN env text CHECK (env IN ('preview', 'prod')),
    DROP CONSTRAINT egress_gaps_reason_check,
    ADD CONSTRAINT egress_gaps_reason_check CHECK (reason IN ('receiver_down', 'dropped', 'not_recorded', 'unattributed', 'envoy_dropped', 'over_cap')),
    ADD CONSTRAINT egress_gaps_app_check CHECK ((app IS NOT NULL) = (reason = 'over_cap') AND (app IS NULL) = (env IS NULL));
CREATE UNIQUE INDEX egress_gaps_over_cap ON egress_gaps (app, env) WHERE reason = 'over_cap';
-- An app's read: its own over_cap gaps in the window.
CREATE INDEX egress_gaps_app ON egress_gaps (app, to_at) WHERE app IS NOT NULL;
