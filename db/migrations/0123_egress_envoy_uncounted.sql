-- envoy_dropped may count no calls (contracts/schema/egress.schema.json, gap; #2374): entries Envoy lost around a
-- restart, on a stream to the receiver that failed to open, or while its count could not be read, which no one
-- counted, as dropped may. egress_gaps_check1 is 0079's unnamed rule that every other reason counts its calls.
ALTER TABLE egress_gaps
    DROP CONSTRAINT egress_gaps_check1,
    ADD CONSTRAINT egress_gaps_counted_check CHECK (reason IN ('receiver_down', 'dropped', 'envoy_dropped') OR calls IS NOT NULL);
