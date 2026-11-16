-- The buildpacks a build ran (contracts/executor.md, The buildpacks that ran), kept on the run that built
-- and travelling with its image to every deploy row; NULL when a build recorded none. The org's approved
-- list is what stack.blessed reads them against (contracts/routes.md, the promote receipt; #636).
ALTER TABLE runs ADD COLUMN buildpacks jsonb;
ALTER TABLE deploys ADD COLUMN buildpacks jsonb;
ALTER TABLE org_preferences ADD COLUMN approved_buildpacks jsonb NOT NULL DEFAULT '[]';
