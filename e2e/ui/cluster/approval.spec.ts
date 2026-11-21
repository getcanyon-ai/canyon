// M10 · a person approves production the CLI asked for (design/AUTH.md §4.5; e2e/cluster/CASES.md, M10).
// Given M5 (e2e-app live in preview), with the org's switch on: alice's CLI session — a device-flow
// session, as `canyon login` leaves one — asks to promote and is answered with an approval, never a
// run, and cannot approve it itself; alice approves it in her browser, and production goes live. The
// switch is the admin's, set from the admin's browser and set back after.
import { expect, test } from "@playwright/test";
import { api, host } from "../harness/seed";
import { signIn, type Session } from "../harness/signin";
import { audit, raw } from "../harness/cluster";

const app = "e2e-app";

// deviceSession is a CLI session for the person signed in on page: the device flow's three calls,
// its one click on /#/device (cmd/canyon-server/routes_device.go), as `canyon login` makes them.
async function deviceSession(page: import("@playwright/test").Page): Promise<Session> {
  const start = await (await fetch(`${host}/auth/device`, { method: "POST" })).json() as { device_code: string; user_code: string };
  await page.goto(`/#/device?code=${start.user_code}`);
  await page.getByRole("button", { name: "Approve" }).click();
  await page.getByRole("heading", { name: "The CLI is signed in" }).waitFor({ timeout: 15_000 });
  const token = await (await fetch(`${host}/auth/device/token`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ device_code: start.device_code }) })).json() as { session: string };
  const name = new URL(host).protocol === "https:" ? "__Host-canyon_session" : "canyon_session";
  return { cookie: `${name}=${token.session}` };
}

test("M10 the CLI asks, alice approves in the dashboard, and production goes live", async ({ page, browser }) => {
  const adminPage = await (await browser.newContext()).newPage();
  const admin = await signIn(adminPage, "admin");
  const alice = await signIn(page, "alice");
  const cli = await deviceSession(page);
  await api(admin, "PUT", "/api/v1/preferences", { prod_approval: true });
  try {
    const status = await api<{ promote_run: string }>(cli, "GET", `/api/v1/projects/${app}/status`);
    const asked = await api<{ approval: { code: string; url: string } }>(cli, "POST", `/api/v1/projects/${app}/promotions`, { gate_run: status.promote_run });
    expect(asked.approval.url).toBe(`${host}/#/approve?code=${asked.approval.code}`);
    const self = await raw(cli, "POST", `/api/v1/promotion-approvals/${asked.approval.code}/approve`);
    expect(self.status, "the CLI approves its own request").toBe(403);

    await page.goto(`/#/approve?code=${asked.approval.code}`);
    await expect(page.getByRole("heading", { name: new RegExp(`^Promote ${app} \\S+ to production\\?$`) })).toBeVisible(); // it names the version that ships (D5)
    await expect(page.getByText(/asked from the CLI,/)).toBeVisible();
    await expect(page.getByRole("region", { name: "Approving as" })).toContainText("Approving as ");
    await page.getByRole("button", { name: "Approve" }).click();
    await expect(page.getByRole("heading", { name: "Promotion started" })).toBeVisible();
    await page.getByRole("link", { name: "Follow it on its page →" }).click();
    await expect(page.getByRole("link", { name: "Open production" })).toBeVisible({ timeout: 5 * 60_000 });

    const rows = (await audit(alice, `project=${app}`)).map((e) => e.action);
    expect(rows).toContain("request promotion");
    expect(rows).toContain("approve promotion");
  } finally {
    await api(admin, "PUT", "/api/v1/preferences", { prod_approval: false });
  }
});
