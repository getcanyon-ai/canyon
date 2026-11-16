-- When the run that started what runs in an environment began (#1597, contracts/routes.md,
-- envs.<env>.stopped): the deploy run in preview, the promote run in prod. It read the values it
-- handed the container after this, so a secret set or removed since may differ from what the app
-- was handed, and its stopped line is not shown. NULL on a row written before it: no line.
ALTER TABLE deploys ADD COLUMN run_started_at timestamptz;
