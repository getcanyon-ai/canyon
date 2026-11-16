-- How each version of a custom rule is checked (contracts/snapshot.md, Custom rules; #2639): by code, a CEL
-- expression over the app's facts with the condition in plain words, or by AI. Decided once, when the
-- version is saved, and never rewritten, as the version itself is not. A version saved before this was
-- checked by AI, which is what it reads as; its facts version is the one this release reads.
ALTER TABLE custom_rule_edits
    ADD COLUMN check_by text NOT NULL DEFAULT 'ai' CHECK (check_by IN ('code', 'ai')),
    ADD COLUMN check_expr text CHECK (char_length(check_expr) BETWEEN 1 AND 4096),
    ADD COLUMN check_says text CHECK (char_length(check_says) BETWEEN 1 AND 280),
    ADD COLUMN check_facts_version int NOT NULL DEFAULT 2 CHECK (check_facts_version > 0),
    ADD CONSTRAINT custom_rule_edits_check CHECK ((check_by = 'code' AND check_expr IS NOT NULL AND check_says IS NOT NULL)
        OR (check_by = 'ai' AND check_expr IS NULL AND check_says IS NULL));
