-- Whether the stored front door passed the cluster probe (contracts/routes.md, the front-door row; #2438):
-- Canyon never runs an app behind one that has not. A host that never saved a platform webhook never ran
-- under run_by: org, so every door it saved passed the probe. On a host that saved one, a door may have been
-- saved under org, where nothing was probed, and nothing tells which: its door starts unprobed (fail
-- closed), and an admin saves it again once before Canyon runs apps there. probe_fix is why a door is
-- unprobed: the probe's fix, or the probe_unavailable sentence; '' for a door stored before this.
ALTER TABLE front_doors ADD COLUMN probed_at timestamptz;
ALTER TABLE front_doors ADD COLUMN probe_fix text NOT NULL DEFAULT '';
UPDATE front_doors SET probed_at = set_at WHERE NOT EXISTS (SELECT 1 FROM platform_webhook);

-- Each attempt to deliver a release: when it was made and the HTTP status it got, 0 for no answer
-- (GET /api/v1/platform-webhook's last_delivery, #2439). sent_at stays the first send, which the
-- environment's pending read waits from.
ALTER TABLE releases ADD COLUMN attempted_at timestamptz;
ALTER TABLE releases ADD COLUMN attempt_status integer CHECK (attempt_status BETWEEN 0 AND 599);
