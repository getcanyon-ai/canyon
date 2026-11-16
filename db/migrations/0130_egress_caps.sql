-- The three egress caps, an admin's to set (design/EGRESS.md §3.3, §14, the owner's decision 2026-11-05; #2691):
-- destinations per app env (the receiver's rows), new names an app env may call a minute across every replica (the
-- check's rate limit, which the policy carries), and pending egress requests per app env. Today's numbers are the
-- defaults, and the one row holds each in its range, so no read ever finds a cap missing or out of range: a write
-- out of range fails, never a cap that means none. A change moves the policy's seq (0096's trigger on this table).
ALTER TABLE egress_settings
    ADD COLUMN cap_destinations integer NOT NULL DEFAULT 500 CHECK (cap_destinations BETWEEN 50 AND 5000),
    ADD COLUMN cap_new_names integer NOT NULL DEFAULT 30 CHECK (cap_new_names BETWEEN 1 AND 600),
    ADD COLUMN cap_pending integer NOT NULL DEFAULT 50 CHECK (cap_pending BETWEEN 1 AND 500);
