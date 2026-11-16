-- When a call to an egress destination last went through (#2116): set only by a call whose outcome is allowed,
-- never by one refused. The policy's known[] reads it, so no app makes a name known — and so not new to the rate
-- limit — by being refused. A row from before it is layer 1's, where nothing was refused but a call Envoy
-- flagged UAEX: one last allowed was admitted when last seen.
ALTER TABLE egress_destinations ADD COLUMN admitted_at timestamptz;
UPDATE egress_destinations SET admitted_at = last_seen_at WHERE last_outcome = 'allowed';
