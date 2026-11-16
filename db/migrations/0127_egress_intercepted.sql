-- An inner request, one inside a tunnel Envoy ended to add a route's credential, is a call of its own (design/EGRESS.md
-- §2.4, §4.2; #2740): last_protocol intercepted. 0084's CHECK took connect, http and https alone, and a flush writes
-- every app's rows of a batch in one transaction, so one row it refused would lose them all.
ALTER TABLE egress_destinations
    DROP CONSTRAINT egress_destinations_last_protocol_check,
    ADD CONSTRAINT egress_destinations_last_protocol_check CHECK (last_protocol IN ('connect', 'http', 'https', 'intercepted'));
