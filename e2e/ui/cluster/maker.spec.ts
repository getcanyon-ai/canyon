// The browser halves of M (e2e/cluster/CASES.md, M — Maker alice) that #488's Go cases skip: M4's
// /whoami and M5's click, given M3 (e2e-app live in preview, the Go harness's), and M8's sign-in
// through the gateway, given the Go half of M8 (e2e-peer redeployed under choice 1). M1 is the Go
// harness's whole: its device code is approved by harness/confirm-device.ts.
import { expect, test, type Page } from "@playwright/test";
import { api } from "../harness/seed";
import { signIn } from "../harness/signin";
import { appUrl, audit, email, kube, parseWhoami, podNamed, portalSession } from "../harness/cluster";

const app = "e2e-app";
type Project = { project: { envs: { preview?: { image_digest: string; gate_run: string }; prod?: { image_digest: string } } } };

// openApp loads one of e2e-app's pages as the signed-in person: the proxy sends the browser to the
// provider, where the sign-in to Canyon already happened, and back without a keystroke.
async function openApp(page: Page, env: "preview" | "prod", path: string, name = app): Promise<string> {
  const url = appUrl(name, env) + path;
  await page.goto(url);
  await page.waitForURL((u) => u.origin === new URL(url).origin, { timeout: 60_000 });
  return page.locator("body").innerText();
}



test("M4 alice opens the preview and sees herself", async ({ page }) => {
  await signIn(page, "alice");
  const who = parseWhoami(await openApp(page, "preview", "/whoami"));
  expect(who).toEqual({ email: email("alice"), roles: "-", aud: app, verified: "true" });
  await expect(page.locator("#visits")).toHaveCount(0); // the whoami page, not /db
  await portalSession(page, app, "preview");
  for (const n of ["1", "2"]) {
    await page.goto(appUrl(app, "preview") + "/db");
    await expect(page.locator("#visits")).toHaveText(n);
  }
});

test("M5 promote with approvals, then the prod address", async ({ page }) => {
  const alice = await signIn(page, "alice");
  const before = (await api<Project>(alice, "GET", `/api/v1/projects/${app}`)).project;
  expect(before.envs.preview, `${app} is not running in preview: M5 needs M3`).toBeDefined();
  expect(before.envs.prod, `${app} is already promoted: M5 would prove nothing`).toBeUndefined();
  const builds = () => kube("canyon-builds", "builds.kpack.io").map((b) => b.metadata.name).sort();
  const buildsBefore = builds();

  await page.goto(`/#/apps/${app}`);
  await page.getByRole("button", { name: /^Promote .*to production$/ }).click();
  await expect(page.getByText("Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.")).toBeVisible();
  await page.getByRole("button", { name: /^Promote \S+$/ }).click();
  const prodHost = `${app}.prod.${process.env.E2E_APPS_DOMAIN}`.replaceAll(".", "\\.");
  await expect(page.getByRole("link", { name: "Open production" })).toHaveAttribute("href", new RegExp(`^https://${prodHost}/?$`), { timeout: 5 * 60_000 });

  const after = (await api<Project>(alice, "GET", `/api/v1/projects/${app}`)).project;
  expect(after.envs.prod?.image_digest).toBe(before.envs.preview!.image_digest);
  expect(builds(), "a Build was created during the promotion").toEqual(buildsBefore);
  const pod = podNamed("canyon-apps", `${app}-prod`);
  expect(pod?.images.app, `${app}-prod's app container`).toMatch(new RegExp(`@${before.envs.preview!.image_digest}$`));
  const who = parseWhoami(await openApp(page, "prod", "/whoami"));
  expect(who).toMatchObject({ email: email("alice"), aud: app, verified: "true" });
  await portalSession(page, app, "prod");
  await page.goto(appUrl(app, "prod") + "/db");
  await expect(page.locator("#visits")).toHaveText("1"); // a database of its own
  const rows = (await audit(alice, `project=${app}`)).slice(0, 3).reverse().map((e) => e.action);
  expect(rows).toEqual(["POST /api/v1/projects/{id}/promotions", "ensure resource", "deploy prod"]);
});

test("M8 under choice 1 the gateway signs alice in, and none of its credentials reach the app", async ({ page }) => {
  await signIn(page, "alice");
  const text = await openApp(page, "preview", "/whoami", "e2e-peer"); // the ALB, the provider, and back
  expect(parseWhoami(text)).toMatchObject({ email: email("alice"), aud: "e2e-peer", verified: "true" });
  await expect(page.locator("#door")).toHaveText("door: -"); // no x-amzn-oidc-*, no AWSELBAuthSessionCookie
});
