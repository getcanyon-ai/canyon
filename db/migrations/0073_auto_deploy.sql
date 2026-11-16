-- Whether a push to the app's linked repository deploys to preview (#1923): off until an owner or an
-- admin turns it on, and never production.
ALTER TABLE projects ADD COLUMN auto_deploy boolean NOT NULL DEFAULT false;
