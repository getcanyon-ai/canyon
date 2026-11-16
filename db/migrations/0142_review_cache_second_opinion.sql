-- The second-opinion note now says a blocking finding stays while its code is unchanged (contracts/finding.md; #2900),
-- so a cached review would replay the words before it for a tree and policy that have not changed: it is forgotten,
-- and the next review asks again.
DELETE FROM review_cache;
