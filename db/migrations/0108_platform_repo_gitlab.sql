-- GitLab's merge requests for the deploy repo (#2480; contracts/routes.md, the platform-repo row): api gains
-- gitlab, and production goes through a pull request under either API that opens one; with none it is still push
-- alone. Both checks only widen, so the release before this one reads and writes every row it could before. A row
-- this release saves with api gitlab and prod_mode pull_request is one that release cannot deliver to production:
-- before rolling back to it, save the repo with api none (push): a remove follows its environment's last deploy,
-- whatever delivery says.
ALTER TABLE platform_repo
    DROP CONSTRAINT platform_repo_api_check,
    ADD CONSTRAINT platform_repo_api_check CHECK (api IN ('github', 'gitlab', 'none')),
    DROP CONSTRAINT platform_repo_check,
    ADD CONSTRAINT platform_repo_check CHECK (api <> 'none' OR prod_mode = 'push');
