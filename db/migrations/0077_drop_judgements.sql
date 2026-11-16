-- #1421: the AI judgement is gone. No released binary reads or writes judgement_edits or
-- review_cache.judgements (v0.1.11 and v0.1.12 name neither), so the release after them drops both.
DROP TABLE judgement_edits;
ALTER TABLE review_cache DROP COLUMN judgements;
