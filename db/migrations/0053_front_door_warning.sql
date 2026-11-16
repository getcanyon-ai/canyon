-- The probe's warning from the save that stored the front door (contracts/routes.md, the front-door row;
-- contracts/executor.md, /probe): the node ports an app reaches on the server it runs on, which the
-- page and `canyon status` show beside the door. '' when the probe had none, or for a door saved before.
ALTER TABLE front_doors ADD COLUMN warning text NOT NULL DEFAULT '';
