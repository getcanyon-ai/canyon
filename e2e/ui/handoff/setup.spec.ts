import { writeFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// The hand-off set-up, clicked through by the admin in a real browser (#2439): e2e/handoff/journey_test.go runs this
// project in two parts around starting the org's pipeline. H1 saves the platform webhook and the front door from Settings
// alone, reading the two values the dialog shows once the way a person copies them, and hands them to the journey in
// HANDOFF_VALUES; the journey starts the pipeline with them. H2 sends a test delivery, which the pipeline verifies under
// the secret it was given, and hands apps to the platform. Trace, video and screenshots are off: a trace would hold the
// values. Nothing here asserts the clipboard.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/handoff/journey_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  await context.addCookies([{ name: env("HANDOFF_COOKIE_NAME"), value: env("HANDOFF_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: true, httpOnly: true, sameSite: "Lax" }]);
});

test("H1 the admin saves the webhook and the front door, and stores the two values", async ({ page }) => {
  await page.goto("/#/settings/hosting");
  await page.getByRole("radio", { name: "Your platform runs apps" }).check();
  await page.getByLabel("Webhook URL", { exact: true }).fill(env("HANDOFF_HOOK_URL"));
  await page.getByRole("button", { name: "Save the webhook" }).click();
  const dialog = page.getByRole("dialog", { name: "Store these now" });
  const secret = await dialog.getByLabel("Webhook secret", { exact: true }).inputValue();
  const token = await dialog.getByLabel("Host token", { exact: true }).inputValue();
  expect(secret).not.toBe("");
  expect(token).not.toBe("");
  writeFileSync(env("HANDOFF_VALUES"), JSON.stringify({ secret, token }), { mode: 0o600 });
  await dialog.getByRole("checkbox", { name: "I've stored both" }).check();
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toHaveCount(0);

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
  await expect(page.getByRole("heading", { name: "✓ Your gateway signs people in" })).toBeVisible();
});

test("H2 a test delivery reaches the platform, then the admin hands apps to it", async ({ page }) => {
  await page.goto("/#/settings/hosting");
  await expect(page.getByRole("radio", { name: "Canyon runs apps on this cluster" })).toBeChecked(); // a webhook is saved, and run_by still canyon's (#2560)
  await page.getByRole("radio", { name: "Your platform runs apps" }).click(); // the checklist, its saved steps ticked
  await page.getByRole("button", { name: "Send a test delivery" }).click();
  await expect(page.getByText("Your platform answered 202. Your platform is reachable.", { exact: false })).toBeVisible({ timeout: 20_000 });
  await page.getByRole("button", { name: "Hand apps to your platform" }).click();
  await expect(page.getByText("Hand apps to your platform? From the next deploy or promotion", { exact: false })).toBeVisible();
  await page.getByRole("button", { name: "Hand apps to your platform" }).last().click();
  await expect(page.getByText("Your platform runs apps. Canyon sends each release to", { exact: false })).toBeVisible();
  await expect(page.getByText("No release has been sent yet. Deploy an app to see its first release here.")).toBeVisible();
});

test("H3 the app page says a pod checked in", async ({ page }) => {
  await page.goto(`/#/apps/${env("HANDOFF_APP")}`);
  await expect(page.getByRole("region", { name: "Preview" }).getByText(/^Your platform reported it running (just now|\d+m ago)$/)).toBeVisible({ timeout: 30_000 });
});
