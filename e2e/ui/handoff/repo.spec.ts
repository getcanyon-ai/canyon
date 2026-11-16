import { writeFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// The hand-off set-up through a deploy repo, clicked through by the admin (#2470): e2e/handoff/repo_test.go runs R1, then
// applies deploy/gitops with the fetch-only token R1 stored, then runs R2. R1 picks the deploy repo, saves it, stores the
// two values its first save shows once in HANDOFF_VALUES, checks the repo, and saves the front door at choice 1; R2 hands
// apps to the platform. Trace, video and screenshots are off (setup.spec.ts): a trace would hold the values.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/handoff/repo_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  await context.addCookies([{ name: env("HANDOFF_COOKIE_NAME"), value: env("HANDOFF_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: true, httpOnly: true, sameSite: "Lax" }]);
});

test("R1 the admin saves and checks the deploy repo, stores the two values, and saves the front door", async ({ page }) => {
  await page.goto("/#/settings/hosting");
  await page.getByRole("radio", { name: "Your platform runs apps" }).check();
  const repo = page.getByRole("radio", { name: "Canyon commits to your deploy repo (recommended)" });
  await repo.click(); // checked once the server saves the preference: the radio follows the record, not the click
  await expect(repo).toBeChecked();
  await page.getByLabel("Repository URL", { exact: true }).fill(env("HANDOFF_REPO_URL"));
  await page.getByLabel("User name", { exact: true }).fill(env("HANDOFF_REPO_USER"));
  await page.getByLabel("Token", { exact: true }).fill(env("HANDOFF_REPO_TOKEN"));
  await page.getByLabel("API address", { exact: true }).fill(env("HANDOFF_API_URL"));
  await page.getByLabel("CA for the sign-in sidecar and External Secrets (optional)", { exact: true }).fill(env("HANDOFF_CA"));
  await page.getByRole("button", { name: "Save the deploy repo" }).click();
  const dialog = page.getByRole("dialog", { name: "Store these now" });
  const fetch = await dialog.getByLabel("Fetch-only token", { exact: true }).inputValue();
  const token = await dialog.getByLabel("Host token", { exact: true }).inputValue();
  expect(fetch).not.toBe("");
  expect(token).not.toBe("");
  writeFileSync(env("HANDOFF_VALUES"), JSON.stringify({ fetch, token }), { mode: 0o600 });
  await dialog.getByRole("checkbox", { name: "I've stored both" }).check();
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toHaveCount(0);

  await page.getByRole("button", { name: "Check the repo" }).click();
  await expect(page.getByText("Canyon reached the repo, read both branches, pushed a test branch to it and rendered your templates.")).toBeVisible({ timeout: 60_000 });
  await expect(page.getByText(/^Templates found: .*route\.yaml/)).toBeVisible();

  await page.getByRole("button", { name: "Go to the front door" }).click();
  await expect(page.getByRole("heading", { name: "How people reach apps" })).toBeFocused();
  await page.getByRole("radio", { name: "Your gateway signs people in" }).check();
  await page.getByLabel("Issuer", { exact: true }).fill(env("HANDOFF_ISSUER"));
  await page.getByLabel("Audience", { exact: true }).fill(env("HANDOFF_AUDIENCE"));
  await page.getByLabel("JWKS URL", { exact: true }).fill(`${env("HANDOFF_ISSUER")}/keys`);
  await page.getByLabel("Session cookie", { exact: true }).fill("_gw");
  await page.getByLabel("Domain", { exact: true }).fill(env("HANDOFF_DOMAIN"));
  await page.getByRole("button", { name: "Save the front door" }).click();
  await expect(page.getByText("Saved. Apps change at their next deploy.")).toBeVisible({ timeout: 180_000 }); // the probe takes its time
});

test("R2 the admin hands apps to the platform, through the deploy repo", async ({ page }) => {
  await page.goto("/#/settings/hosting");
  await expect(page.getByRole("radio", { name: "Canyon runs apps on this cluster" })).toBeChecked(); // a deploy repo is saved, and run_by still canyon's (#2560)
  await page.getByRole("radio", { name: "Your platform runs apps" }).click(); // the checklist, its saved steps ticked
  await page.getByRole("button", { name: "Hand apps to your platform" }).click();
  await page.getByRole("button", { name: "Hand apps to your platform" }).last().click();
  await expect(page.getByText("Your platform runs apps. Canyon commits each release to", { exact: false })).toBeVisible();
});
