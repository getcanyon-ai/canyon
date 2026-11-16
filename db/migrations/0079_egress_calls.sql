-- An app's calls out (design/EGRESS.md §2.4; contracts/schema/egress.schema.json): one row per key and
-- minute, and their hourly roll-ups. Never a path, a query, a header value, a body or a resolved
-- address: the receiver drops them on receipt, so there is no column to put one in. Both tables are
-- partitioned by day; the server creates today's and tomorrow's partitions and drops a day's whole once
-- its minutes are 48 h old, its hours 30 days — never a DELETE. A null app, env and kind is a caller no
-- one held (contracts/executor.md, POST /callers); a null run, a caller started without one. No foreign
-- key: records are evidence about a day, and outlive nothing they name.
CREATE TABLE egress_minutes (
    at             timestamptz NOT NULL,
    app            text,
    env            text CHECK (env IN ('preview', 'prod')),
    kind           text CHECK (kind IN ('web', 'job')),
    run            text,
    destination    text NOT NULL,
    port           integer NOT NULL CHECK (port BETWEEN 1 AND 65535),
    method         text NOT NULL,
    status_class   text NOT NULL CHECK (status_class IN ('1xx', '2xx', '3xx', '4xx', '5xx', 'none')),
    outcome        text NOT NULL CHECK (outcome IN ('allowed', 'refused')),
    response_flags text[] NOT NULL,
    count          bigint NOT NULL CHECK (count >= 1),
    bytes_out      bigint NOT NULL CHECK (bytes_out >= 0),
    bytes_in       bigint NOT NULL CHECK (bytes_in >= 0),
    duration_ms    bigint NOT NULL CHECK (duration_ms >= 0),
    CHECK ((app IS NULL) = (env IS NULL) AND (app IS NULL) = (kind IS NULL) AND (app IS NOT NULL OR run IS NULL)),
    UNIQUE NULLS NOT DISTINCT (at, app, env, kind, run, destination, port, method, status_class, outcome, response_flags)
) PARTITION BY RANGE (at);

CREATE TABLE egress_hours (LIKE egress_minutes INCLUDING CONSTRAINTS,
    UNIQUE NULLS NOT DISTINCT (at, app, env, kind, run, destination, port, method, status_class, outcome, response_flags)
) PARTITION BY RANGE (at);
-- An app's read and the receiver's day of destinations are by app, env and time.
CREATE INDEX egress_minutes_app ON egress_minutes (app, env, at);
CREATE INDEX egress_hours_app ON egress_hours (app, env, at);

-- What the records are missing (contracts/schema/egress.schema.json, gaps): calls the receiver dropped,
-- could not write or recorded under no app, counted; calls Envoy dropped before sending; and the time the
-- receiver was not receiving at all, whose calls no one counted. NULL calls is a span no one could count:
-- the receiver down, or a message too large to read. Nothing about a call is lost without a row here;
-- kept 30 days, as the hours are.
CREATE TABLE egress_gaps (
    from_at timestamptz NOT NULL,
    to_at   timestamptz NOT NULL CHECK (to_at >= from_at),
    reason  text NOT NULL CHECK (reason IN ('receiver_down', 'dropped', 'not_recorded', 'unattributed', 'envoy_dropped')),
    calls   bigint CHECK (calls >= 1),
    CHECK (reason IN ('receiver_down', 'dropped') OR calls IS NOT NULL),
    CHECK (reason <> 'receiver_down' OR calls IS NULL)
);
CREATE INDEX egress_gaps_to_at ON egress_gaps (to_at);

-- When the receiver last flushed: a boot finding it older than a few flushes records the time between as
-- receiver_down. One row, written every flush.
CREATE TABLE egress_receiver (
    one     boolean PRIMARY KEY DEFAULT true CHECK (one),
    seen_at timestamptz NOT NULL
);
