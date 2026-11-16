-- Where the org's platform says each resource it provisioned is (design/HAND-OFF.md §4, #2432): one row per
-- app, environment and resource name, written by the deployed report that named it and replaced by the next
-- release's, gone when a project delete's remove reports removed. An endpoint names a place, never a credential.
CREATE TABLE platform_resources (
    project_id  text NOT NULL REFERENCES projects(id),
    env         text NOT NULL CHECK (env IN ('preview', 'prod')),
    name        text NOT NULL,
    endpoint    text NOT NULL,
    release_id  text NOT NULL REFERENCES releases(id),
    reported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, env, name)
);

-- A project delete's removes alone ask the platform to delete what it provisioned (the release body's delete: true);
-- an undeploy's, a hold's and the brake's keep it.
ALTER TABLE releases ADD COLUMN deletes_resources boolean NOT NULL DEFAULT false;
