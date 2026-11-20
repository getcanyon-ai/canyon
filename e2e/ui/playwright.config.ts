// The UX journeys (README.md): one at a time, every surface being shared, at desktop and phone width.
import { defineConfig } from "@playwright/test";

// No fourth recorder: a failed test otherwise writes the open page's accessibility tree to
// error-context.md, and on the cluster project that page may be the identity provider's, with the
// address (and, mid-fill, the password) typed into it (e2e/cluster/README.md). Set here, at the top
// of the config module, not in a test helper: the reporter that writes error-context.md runs in
// the *main* process, while a helper like signIn runs in a *worker* process, and process.env set in
// one is invisible to the other. The config module itself is not shared, though -- each process
// loads its own copy of it. The worker asks for its config by calling deserializeConfig(), which
// calls loadConfig() -> loadUserConfig(), which requireOrImport()s the resolved config *file* again
// (playwright/lib/worker/workerMain.js:164 -> playwright/lib/common/configLoader.js:101,119,107,
// @playwright/test 1.55.0) -- i.e. this file's top-level code runs fresh in every worker too, not
// just in the main process that first resolved the project list from it.
if (process.env.E2E_CANYON_URL) process.env.PLAYWRIGHT_NO_COPY_PROMPT = "1";

export default defineConfig({
  testDir: "journeys",
  outputDir: ".work/results",
  globalSetup: "./harness/still.ts", // the stack still before the first journey
  globalTeardown: "./harness/teardown.ts",
  workers: 1,
  fullyParallel: false,
  timeout: 30 * 60_000,
  reporter: [["list"], ["json", { outputFile: ".work/run.json" }]], // run.json: CI fails a shard that ran no journey (--shard passes an empty one)
  use: { baseURL: "http://localhost:7777", trace: "retain-on-failure", actionTimeout: 15_000 },
  projects: [
    { name: "desktop", use: { viewport: { width: 1360, height: 900 } } },
    { name: "phone", use: { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true } },
    // The test cluster (e2e/cluster/README.md): Canyon behind a real identity provider, whose pages take
    // passwords, so nothing is recorded; no trace, no video, no HAR.
    {
      name: "cluster",
      testDir: "cluster",
      use: { baseURL: process.env.E2E_CANYON_URL, trace: "off", video: "off", viewport: { width: 1360, height: 900 } },
    },
    {
      // The hand-off set-up in the kube journeys (e2e/handoff/journey_test.go and repo_test.go run it, never make e2e-ui): Canyon behind the
      // journey's own private CA, and no trace, video or screenshot, which would hold the values the dialog shows once.
      name: "handoff",
      testDir: "handoff",
      retries: 0,
      use: { baseURL: process.env.E2E_CANYON_URL, ignoreHTTPSErrors: true, trace: "off", video: "off", screenshot: "off", viewport: { width: 1360, height: 900 } },
    },
    // The app page's Secrets section at each level, which e2e/oidc/secrets_test.go runs once per person with their session
    // (never make e2e-ui): levels need sign-in on, which the Docker projects' stack has off.
    {
      name: "secrets",
      testDir: "secrets",
      retries: 0,
      use: { baseURL: process.env.E2E_CANYON_URL, trace: "off", video: "off", viewport: { width: 1360, height: 900 } },
    },
    // An MCP client's consent page on the team setup, which e2e/oidc/oauth_test.go runs as the admin and as a member (never
    // make e2e-ui): the authorization server is served only with sign-in on, which the Docker projects' stack has off.
    {
      name: "consent",
      testDir: "consent",
      retries: 0,
      use: { baseURL: process.env.E2E_CANYON_URL, trace: "retain-on-failure", viewport: { width: 1360, height: 900 } },
    },
    // Settings › Patching on the team setup with patching on, which e2e/oidc/patch_test.go runs mid-journey (never make
    // e2e-ui): patching needs sign-in on (design/PATCHING.md §4.5), which the Docker projects' stack has off.
    {
      name: "patch",
      testDir: "patch",
      retries: 0,
      use: { baseURL: process.env.E2E_CANYON_URL, trace: "retain-on-failure", viewport: { width: 1360, height: 900 } },
    },
  ],
});
