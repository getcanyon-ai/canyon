-- The build digest of an image Canyon built (#1244, contracts/routes.md, the deploys row): HMAC-SHA256
-- under a key derived from the vault's, over the project, preview's where: build names and values, and
-- the image, so a redeploy of the same package with the same build values reuses the image instead of
-- building it again. Never a value, never a plain hash. A run keeps what its build made, whatever its
-- start did after; a deploy row keeps its image's. NULL on a row written before it: never reused.
ALTER TABLE runs ADD COLUMN build_image text, ADD COLUMN build_digest text;
ALTER TABLE deploys ADD COLUMN build_digest text;
