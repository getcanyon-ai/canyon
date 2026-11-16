-- Where apps run (design/HAND-OFF.md §1; contracts/routes.md, the preferences row): canyon, the
-- default, or org, once the platform webhook and a front door are set. Not yet settable through
-- PUT /api/v1/preferences (that route stays canyon-only until it enforces the ordering); a test or
-- an operator sets the row directly until then.
ALTER TABLE org_preferences ADD COLUMN run_by text NOT NULL DEFAULT 'canyon' CHECK (run_by IN ('canyon', 'org'));
