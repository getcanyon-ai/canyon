import { writeFileSync } from "node:fs";
import { expect, test } from "@playwright/test";

// An MCP client's consent page (design/AUTH.md §4.7) as a person reads it and answers it: e2e/oidc/oauth_test.go runs
// this project on the team setup, as the admin and as a member, with a request it began; the client's redirect, a
// port nothing listens on, is caught here and written back for the journey to redeem.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/oauth_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  const name = env("CONSENT_COOKIE_NAME");
  await context.addCookies([{ name, value: env("CONSENT_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

test("the person reads who asks, as whom, and what it may do, and answers it", async ({ page }) => {
  let sent = "";
  await page.route("http://127.0.0.1:47123/**", async (route) => { sent = route.request().url(); await route.fulfill({ status: 200, body: "back at the client" }); });
  await page.goto(`/#/oauth?request=${env("CONSENT_REQUEST")}`);
  await expect(page.getByRole("heading", { name: "Allow Canyon e2e agent to use Canyon as you?" })).toBeVisible();
  await expect(page.getByText(new RegExp(`^Canyon e2e agent will act as .* \\(${env("CONSENT_WHO").replace(".", "\\.")}\\) on .*\\. Then Canyon sends you back to 127\\.0\\.0\\.1\\.$`))).toBeVisible();
  await expect(page.getByText("Canyon can't verify this name: the app chose it itself.")).toBeVisible();
  await expect(page.getByRole("region", { name: "What it may do" }).getByText("See your org's rules, and the apps you can see: their status, and logs where you're a contributor")).toBeVisible();
  await page.getByRole("button", { name: env("CONSENT_ANSWER") === "allow" ? "Allow" : "Don't allow" }).click();
  await expect.poll(() => sent).not.toBe("");
  writeFileSync(env("CONSENT_OUT"), sent);
});
