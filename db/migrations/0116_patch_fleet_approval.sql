-- The fleet approval (design/PATCHING.md §4.5; design/PERMISSIONS.md, Platform patches; contracts/routes.md, the
-- Platform rows): an admin approves one platform identity for production across the fleet, from a browser, with a
-- reason. It never expires, and v1 withdraws none; each decision it makes reads that admin's standing again from the
-- address their session carried. Expand only: every column nullable, so the previous binary reads and writes its
-- identities as before.
ALTER TABLE patch_identities
    ADD COLUMN approved_by_kind    text,
    ADD COLUMN approved_by_subject text,
    ADD COLUMN approved_by_display text,
    ADD COLUMN approved_at         timestamptz,
    ADD COLUMN approved_reason     text,
    ADD COLUMN approved_by_email   text,
    ADD CONSTRAINT patch_identities_approved_check
        CHECK (num_nulls(approved_by_kind, approved_by_subject, approved_by_display, approved_at, approved_reason, approved_by_email) IN (0, 6));
