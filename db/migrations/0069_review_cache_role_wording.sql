-- The signed-in auth.unauthenticated-route fix no longer tells every route that changes data to check a role
-- (contracts/finding.md; #1860), so a cached review would replay the old words for a tree and policy that
-- have not changed: it is forgotten, as 0067 forgot the model's, and the next review of each asks again.
DELETE FROM review_cache;
