import { expect, test, type Page } from "@playwright/test";

// The app page at 390px as a contributor and as a responder (#102): e2e/oidc/narrow_test.go signs each in on the team
// setup (make e2e-up OIDC=1) and runs one of these with their session. A blocked finding's line is far wider than the
// phone, and the page never scrolls sideways (ui/Table.tsx's rule): the snippet scrolls in its own box.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/narrow_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  const name = env("NARROW_COOKIE_NAME");
  await context.addCookies([{ name, value: env("NARROW_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

const fits = async (page: Page, who: string) => {
  await page.goto(`/#/apps/${env("NARROW_APP")}`);
  await expect(page.getByRole("heading", { name: /^What blocks/ })).toBeVisible();
  await page.screenshot({ path: `.work/walk/narrow-${who}.png`, fullPage: true });
  const [scroll, width] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(scroll, `the page is ${scroll}px wide in a ${width}px window: it scrolls sideways`).toBeLessThanOrEqual(width);
};

test("the app page on a phone as contributor", async ({ page }) => { await fits(page, "contributor"); });
test("the app page on a phone as responder", async ({ page }) => { await fits(page, "responder"); });
