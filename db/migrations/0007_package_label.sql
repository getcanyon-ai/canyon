-- A package may carry a label: a friendly name, unique within the project, set when the package
-- is made and never moved (contracts/routes.md, POST /projects/:id/packages).
ALTER TABLE packages ADD COLUMN label text CONSTRAINT packages_label_shape CHECK (label ~ '^[a-z0-9][a-z0-9._-]{0,63}$');
CREATE UNIQUE INDEX packages_label ON packages (project_id, label) WHERE label IS NOT NULL;
