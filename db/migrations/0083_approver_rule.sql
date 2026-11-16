-- Who approves a blocking finding (design/PERMISSIONS.md; #2204): approves_blocking is true when the approval was
-- filed under that rule — by an admin or a responder who did not start the run, from a browser, or on a host with
-- sign-in off. Only such an approval covers a blocking finding, in either gate; every row written before this
-- column reads false, so an approval filed under the old rule covers none (fail closed).
ALTER TABLE approvals ADD COLUMN approves_blocking boolean NOT NULL DEFAULT false;
-- receipt_run is the review a promotion's receipt accepted it under, recorded as the promotion starts, in the
-- transaction that evaluated the receipt: the review its deploy step clears again, live, before it starts
-- production's container. Null on every other run, and on a promotion started before this column: one
-- recovered then ships nothing (fail closed).
ALTER TABLE runs ADD COLUMN receipt_run text;
