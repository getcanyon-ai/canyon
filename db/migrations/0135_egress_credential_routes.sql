-- Credential routes (the owner's decision, 2026-11-09, #2143; contracts/routes.md, the egress setting row): whether
-- Canyon adds a route's credential to apps' calls, off until an admin turns it on. While off, no /run carries
-- egress.ca, the policy names no route and a route's write is refused. It is written like via and mode, so turning
-- it either way moves seq (0096's trigger on egress_settings), and the policy stops naming routes at once.
ALTER TABLE egress_settings ADD COLUMN credential_routes boolean NOT NULL DEFAULT false;
