import { expect, test, type Page } from "@playwright/test";

// Settings › Agents › Connect an agent, as a member and as an admin, who see the same panel
// (design/specs/mcp.md §6 item 14, §12):
// e2e/oidc/connect_test.go signs each in on the team setup (make e2e-up OIDC=1) and runs one of these with their session.
// A screenshot of each is kept in .work/walk, the walk the panel is reviewed by.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/connect_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  const name = env("CONNECT_COOKIE_NAME");
  await context.addCookies([{ name, value: env("CONNECT_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

const panel = (page: Page) => page.getByRole("region", { name: "Connect an agent" });
const opened = async (page: Page) => {
  await page.goto("/#/settings/agents");
  // the textbox alone: the Copy button's label names the address too
  await expect(panel(page).getByRole("textbox", { name: "Canyon's address for agents", exact: true })).toHaveValue(env("CONNECT_ADDRESS"));
  const laptop = panel(page).getByRole("region", { name: "On your laptop" });
  await expect(laptop.getByText(`canyon login ${env("E2E_CANYON_URL")}`, { exact: true })).toBeVisible();
  await expect(laptop.getByText("canyon skill install", { exact: true })).toBeVisible();
  await expect(panel(page).getByText("Claude Code on the web", { exact: true })).toBeVisible();
  await expect(panel(page).getByText("claude mcp add", { exact: false })).toHaveCount(0); // the address is never a laptop's
};
const walked = (page: Page, who: string) => page.screenshot({ path: `.work/walk/connect-${who}.png`, fullPage: true });

// A member and an admin see the same panel: each cloud agent's line says who adds the address.
test("Connect an agent as member", async ({ page }) => {
  await opened(page);
  await walked(page, "member");
});

test("Connect an agent as admin", async ({ page }) => {
  await opened(page);
  await walked(page, "admin");
});
