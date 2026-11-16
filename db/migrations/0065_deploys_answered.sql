-- What the app answered the one GET / of its url Canyon made after the deploy that started it: {status, said}
-- or {error} (contracts/routes.md, envs.<env>.answered; #1740). NULL when nothing was asked; read only where Canyon runs it.
ALTER TABLE deploys ADD COLUMN answered jsonb;
