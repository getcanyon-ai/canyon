-- Apps are host-routed (contracts/routes.md, App surfaces): the project id is the first label
-- of the app's hostname, and RFC 1123 refuses a trailing hyphen. The container's name on the app
-- network, <id>-preview, is a label too: 63 - 8 = 55 characters.
ALTER TABLE projects DROP CONSTRAINT projects_id_shape;
ALTER TABLE projects ADD CONSTRAINT projects_id_shape CHECK (id ~ '^[a-z]([a-z0-9-]*[a-z0-9])?$' AND length(id) <= 55);
