-- The AI review names what it is not shown (#2280): a file that is not valid UTF-8 leaves it unread, and
-- .envoy.ts or .environment.js is code it now reads. A cached review of a tree and policy that has not
-- changed may have skipped such a file in silence and read clean: it is forgotten, and the next review of
-- each asks the model again. review_cache is a cache, never a record.
DELETE FROM review_cache;
