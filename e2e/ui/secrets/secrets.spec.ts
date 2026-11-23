import { expect, test, type Page } from "@playwright/test";

// The app page's Secrets section at each level (design/specs/mcp.md §6 item 13, §10.1): e2e/oidc/secrets_test.go signs
// each person in on the team setup (make e2e-up OIDC=1) and runs one of these with their session. A screenshot of each
// is kept in .work/walk, the walk the section is reviewed by.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/secrets_test.go`);
  return value;
};
const VALUE = "sk_test_51Hjourney0000000000000000"; // a synthetic shape, never a real value

test.beforeEach(async ({ context }) => {
  const name = env("SECRETS_COOKIE_NAME");
  await context.addCookies([{ name, value: env("SECRETS_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

const open = async (page: Page) => { await page.goto(`/#/apps/${env("SECRETS_APP")}`); };
const section = (page: Page) => page.getByRole("region", { name: "Secrets" });
const form = (page: Page) => page.getByRole("form", { name: "Set a secret" });
// The form opens from its button (#102).
const opened = (page: Page) => section(page).getByRole("button", { name: "Set a secret" }).click();
const walked = (page: Page, who: string) => page.screenshot({ path: `.work/walk/secrets-${who}.png`, fullPage: true });

test("the Secrets section as owner", async ({ page }) => {
  await open(page);
  await expect(section(page).getByText("No secrets set.")).toBeVisible();
  await expect(form(page)).toHaveCount(0); // closed until asked for
  await opened(page);
  await expect(form(page).getByLabel("Value")).toHaveAttribute("type", "password");
  await walked(page, "owner");
});

test("the Secrets section as contributor", async ({ page }) => {
  await open(page);
  await opened(page);
  await form(page).getByLabel("Name").fill("JOURNEY_TOKEN");
  await form(page).getByLabel("Value").fill(VALUE);
  await form(page).getByRole("button", { name: "Set secret" }).click();
  await expect(section(page).getByRole("status")).toHaveText("Set JOURNEY_TOKEN for preview. It takes effect at the next deploy.");
  await expect(form(page)).toHaveCount(0); // done: the form closes, the value with it
  await expect(section(page).getByRole("cell", { name: "JOURNEY_TOKEN", exact: true })).toBeVisible();
  expect(await page.content()).not.toContain(VALUE);
  await walked(page, "contributor");
});

test("the Secrets section as viewer", async ({ page }) => {
  await open(page);
  await expect(page.getByText("Your access: read-only.")).toBeVisible();
  await expect(section(page)).toHaveCount(0);
  await expect(form(page)).toHaveCount(0);
  await walked(page, "viewer");
});

test("the Secrets section as none", async ({ page }) => {
  await open(page);
  await expect(page).toHaveURL(/#\/no-access\//);
  await expect(section(page)).toHaveCount(0);
  await walked(page, "none");
});

test("the Secrets section as admin", async ({ page }) => {
  await open(page);
  const row = section(page).getByRole("row").filter({ hasText: "JOURNEY_TOKEN" });
  await row.getByRole("button", { name: "Remove JOURNEY_TOKEN from Preview" }).click();
  await expect(section(page).getByText("Remove JOURNEY_TOKEN from preview?", { exact: true })).toBeVisible();
  await walked(page, "admin-asking");
  await row.getByRole("button", { name: "Remove JOURNEY_TOKEN from Preview" }).click();
  await expect(section(page).getByRole("status")).toHaveText("Removed JOURNEY_TOKEN from preview. It takes effect at the next deploy.");
  await expect(section(page).getByText("No secrets set.")).toBeVisible();
  await walked(page, "admin");
});
