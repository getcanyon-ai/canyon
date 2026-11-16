// CASES.md H5's browser half, given its Go half (e2e/cluster/handoff_repo_test.go): the app handed to the
// platform through the deploy repo and live in preview, behind the test cluster's own gateway, which signs alice in.
import { expect, test } from "@playwright/test";
import { signIn } from "../harness/signin";
import { appUrl, email, parseWhoami, portalSession } from "../harness/cluster";

const app = process.env.E2E_HANDOFF_APP || "e2e-repo"; // the Go half's repoApp

test("H5 alice opens the handed-off preview through the gateway and sees herself", async ({ page }) => {
  await signIn(page, "alice");
  const url = appUrl(app, "preview");
  await page.goto(url + "/whoami"); // the gateway, the provider, and back
  await page.waitForURL((u) => u.origin === url, { timeout: 60_000 });
  const who = parseWhoami(await page.locator("body").innerText());
  expect(who).toEqual({ email: email("alice"), roles: "-", aud: app, verified: "true" });
  await portalSession(page, app, "preview");
  for (const n of ["1", "2"]) { // the database CloudNativePG runs, through the platform's template
    await page.goto(url + "/db");
    await expect(page.locator("#visits")).toHaveText(n);
  }
});
