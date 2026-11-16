-- The interception root's rotation and an admin's sub-CA (design/EGRESS.md §4.5; contracts/routes.md, POST
-- /api/v1/egress/ca/rotate and GET /egress/leaf, #2704).

-- byo: the root is an admin's sub-CA rather than Canyon's own; its key is sealed as Canyon's is, under ("egress",
-- "root", <fingerprint>), and chain holds the certificates above it, PEM, up to and including a self-signed root.
-- retires_at: 60 days after the rotation that replaced it began. A root's end is the sooner of not_after and
-- retires_at: past it the root leaves every /run's roots and signs nothing. Each change moves seq (0124's trigger).
ALTER TABLE egress_ca_roots ADD COLUMN byo boolean NOT NULL DEFAULT false;
ALTER TABLE egress_ca_roots ADD COLUMN chain text CHECK (chain IS NULL OR byo);
ALTER TABLE egress_ca_roots ADD COLUMN retires_at timestamptz;

-- signing_root: the signing root the policy's last answer named. A root passing its end, or a deploy row written
-- with no roots, changes the signing root with no row written that moves seq, so an answer whose read finds
-- another moves seq first and records it here.
ALTER TABLE egress_policy ADD COLUMN signing_root text CHECK (signing_root ~ '^[0-9a-f]{64}$');
