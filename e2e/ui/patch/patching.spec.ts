import { expect, test } from "@playwright/test";

// Settings › Patching as the admin reads it while Canyon patches no app (#2588, #2081): e2e/oidc/patch_test.go runs this
// project on the team setup with patching on (make e2e-up OIDC=1, e2e/compose.patch.yml), with trivy's database stale,
// so no preview's image could be checked. The page says what Canyon's last check said of each app, never nothing.

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/patch_test.go`);
  return value;
};

test.beforeEach(async ({ context }) => {
  const name = env("PATCH_COOKIE_NAME");
  await context.addCookies([{ name, value: env("PATCH_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

test("the admin reads why Canyon's last check patched none of the apps", async ({ page }) => {
  await page.goto("/#/settings/hosting");
  const patching = page.getByRole("region", { name: "Patching" });
  await expect(patching.getByText("Canyon can't tell which system libraries it uses now, or whether apps wait for newer ones.")).toBeVisible();
  await expect(patching.getByText(/^Canyon's last check, /)).toBeVisible();
  const cause = patching.getByRole("listitem").filter({ hasText: "Canyon couldn't check its image yet" });
  for (const app of env("PATCH_APPS").split(",")) {
    await expect(cause.getByRole("link", { name: app, exact: true })).toHaveAttribute("href", `#/apps/${app}`);
  }
});
