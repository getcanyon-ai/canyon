-- The AI reviewer is shown every common language, not JavaScript and TypeScript alone (#1382), so a
-- cached review of a tree and policy that has not changed read less than a review now would: it is
-- forgotten, and the next review of each asks the model again. review_cache is a cache, never a record.
DELETE FROM review_cache;
