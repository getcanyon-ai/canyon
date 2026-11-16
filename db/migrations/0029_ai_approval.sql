-- Canyon's AI approval (contracts/finding.md, The AI judgement). shown_digest is the judgement's digest an
-- approval by Canyon's AI was written under; it covers the finding only while the finding's digest equals it.
-- A person's approval stores none. review_cache.judgements is what the cached review attached to the log it
-- was given, keyed "<ruleId> <canyon/v1>" for every result of that log, null where none: a hit restores them,
-- and a row whose keys do not cover the log it is asked about (NULL: written before this column) is a miss.
ALTER TABLE approvals ADD COLUMN shown_digest text;
ALTER TABLE review_cache ADD COLUMN judgements jsonb;
