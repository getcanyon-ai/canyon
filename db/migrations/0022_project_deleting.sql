-- A confirmed project delete sets deleting_since before it stops anything, under the ledger lock a
-- run's start takes: from then no run starts and no promotion lands (409 deleting). It stays set
-- through a failed or pending teardown, so the project is frozen until the delete finishes.
-- Expand only: nullable, no default, no data movement.
ALTER TABLE projects ADD COLUMN deleting_since timestamptz;
