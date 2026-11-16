-- The key version a secret's data key is wrapped under (design/ARCHITECTURE.md, the vault):
-- NULL is version 1, the key file every row before this column was sealed under, so nothing
-- moves at migration. Version N is the file kek.N beside it. A lookup for Open and the progress
-- ledger of a re-wrap, never part of the AAD: binding it would make a rotation touch the value.
-- Expand only: nullable, no default, no data movement.
ALTER TABLE secrets ADD COLUMN key_ref integer;
