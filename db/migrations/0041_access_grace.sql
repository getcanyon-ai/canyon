-- How long a handed-off app keeps serving its last access list when Canyon is unreachable
-- (design/HAND-OFF.md §1; contracts/routes.md, the preferences row): a duration as Go spells it,
-- the shape guardrails.schema.json's accessGrace pins; the range, 0 to 24h, is the route's to
-- refuse. Read into every release's sidecar environment as CANYON_ACCESS_GRACE (runs/release.go).
ALTER TABLE org_preferences ADD COLUMN access_grace text NOT NULL DEFAULT '1h' CHECK (access_grace <> '' AND access_grace ~ '^(0|([0-9]+h)?([0-9]+m)?([0-9]+s)?)$');
