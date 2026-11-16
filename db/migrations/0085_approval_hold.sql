-- The hold that outdated an approval (contracts/routes.md, Production approvals; #2243): who held production,
-- when and why, recorded on each pending approval the hold turns outdated, so its page says the hold, still
-- once the lift deletes the hold's row. Expand only: all null on every other approval, one the previous
-- binary outdated included.
ALTER TABLE promotion_approvals
    ADD COLUMN held_by_kind    text CHECK (held_by_kind = 'person'),
    ADD COLUMN held_by_subject text,
    ADD COLUMN held_by_display text,
    ADD COLUMN held_at         timestamptz,
    ADD COLUMN held_reason     text,
    ADD CONSTRAINT promotion_approvals_held_check CHECK ((held_at IS NULL) = (held_by_kind IS NULL) AND (held_at IS NULL) = (held_by_subject IS NULL)
        AND (held_at IS NULL) = (held_by_display IS NULL) AND (held_at IS NULL) = (held_reason IS NULL));
