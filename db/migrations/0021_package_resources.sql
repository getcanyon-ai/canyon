-- What a package's canyon.toml declares under [resources], read and checked at upload: an object
-- of name to type ({"db": "postgres"}). Part of the reviewed tree, so it is fixed with the
-- package and changes only with a newer one. NULL declares nothing, as every row before this
-- column. Expand only: nullable, no default (as 0017's key_ref).
ALTER TABLE packages ADD COLUMN resources jsonb;
