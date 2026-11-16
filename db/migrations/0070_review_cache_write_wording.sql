-- The signed-in auth.unauthenticated-route fix now reads that a destructive or admin-only route checks a role
-- and an ordinary write records who did it (contracts/finding.md; #1868), so a cached review would replay the
-- 0069 words for a tree and policy that have not changed: it is forgotten, and the next review asks again.
DELETE FROM review_cache;
