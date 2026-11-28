// A1-A5 (e2e/cluster/CASES.md, A — Admin first run) on the test cluster. A1-A3 then A5 are phase 1, in this order
// on a fresh install; A4 is phase 3 and needs phase 2's e2e-peer running: the runner picks them by
// name (`--grep ' A[123] '`, later `--grep ' A4 '`: Playwright matches the pattern against the project,
// the file and the title joined by spaces, so the id is never at the start). Every request names its session; a request as
// nobody passes null. No screenshot is taken anywhere: the provider's pages take passwords.
import { execFileSync } from "node:child_process";
import { expect, test, type Page } from "@playwright/test";
import { api, host } from "../harness/seed";
import { signIn } from "../harness/signin";
import { audit, bed, email, kube, podNamed, raw } from "../harness/cluster";

test.describe.configure({ mode: "serial" });

type Preferences = { onboarded_at: string | null };
type FrontDoor = { front_door: null | { choice: number; gateway?: string; signin?: { issuer: string; client_id: string } } };

const settings = (page: Page) => page.getByRole("region", { name: "How people reach apps" });

test("A1 sign in through the identity provider, then onboard", async ({ page, browser }) => {
  const [admin, alice, bob] = [email("admin"), email("alice"), email("bob")];
  await page.goto("/");
  // The sign-in screen: one control, saying it goes to the company's sign-in, and the provider unnamed anywhere on it.
  const main = page.getByRole("main");
  await expect(main.getByRole("link", { name: "Continue to company sign-in", exact: true })).toBeVisible();
  await expect(main.getByRole("link").or(main.getByRole("button"))).toHaveCount(1);
  const provider = new URL(bed("E2E_OIDC_ISSUER")).hostname;
  expect(await main.innerText(), `the sign-in screen names the provider ${provider}`).not.toContain(provider);

  const session = await signIn(page, "admin");
  // A fresh install: still in the setup state, and onboarding rendered rather than the fleet.
  const before = await api<Preferences>(session, "GET", "/api/v1/preferences");
  expect(before.onboarded_at, "the install is not fresh: onboarded_at is already set").toBeNull();
  await expect(page.getByRole("heading", { name: "Set up Canyon" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "What is this organization called?" })).toBeVisible();
  await page.getByLabel("Organization name").fill("E2E Org");
  await page.getByRole("button", { name: "Continue" }).click();
  // Step 2: the three addresses, no domain. The admin's own address is already the first row.
  await expect(page.getByRole("heading", { name: "Who may sign in?" })).toBeVisible();
  await expect(page.getByLabel("Address 1", { exact: true })).toHaveValue(admin);
  for (const [i, who] of [alice, bob].entries()) {
    await page.getByRole("button", { name: "Add person" }).click();
    await page.getByLabel(`Address ${i + 2}`, { exact: true }).fill(who);
  }
  await expect(page.getByLabel("Domain 1", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("checkbox", { name: /^Allow everyone at / })).not.toBeChecked();
  await page.getByRole("button", { name: "Continue" }).click();
  // Step 3: the admin alone.
  await expect(page.getByRole("heading", { name: "Who administers Canyon?" })).toBeVisible();
  await expect(page.getByLabel("Admin 1", { exact: true })).toHaveValue(admin);
  await expect(page.getByLabel("Admin 2", { exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Continue" }).click();
  // Step 4: Ready, 4 of 4, the summary read-only.
  const ready = page.getByRole("region", { name: "Ready" });
  await expect(page.getByRole("heading", { name: "Ready", exact: true })).toBeVisible();
  await expect(page.getByText("4 of 4")).toBeVisible();
  await expect(ready.getByRole("textbox")).toHaveCount(0);
  await expect(ready).toContainText(`${admin}, ${alice}, and ${bob}`);

  // Before the finish, alice -- her own browser, her own sign-in at the provider -- is refused
  // with the setup notice, and no session: her cookies for Canyon hold no session.
  const hers = await browser.newContext();
  try {
    const herPage = await hers.newPage();
    const her = await signIn(herPage, "alice");
    await expect(herPage).toHaveURL(`${host}/#/setup-required`);
    await expect(herPage.getByText("Canyon isn't set up yet. Ask the person who installed it to finish setup.")).toBeVisible();
    expect(her.cookie, "alice was given a session before setup finished").not.toMatch(/canyon_session=/);
  } finally {
    await hers.close();
  }

  await page.getByRole("button", { name: "Finish setup" }).click();
  await expect(page.getByRole("heading", { name: "Set up Canyon" })).toHaveCount(0);
  await expect(page.getByText("No apps yet")).toBeVisible();
  const after = await api<Preferences>(session, "GET", "/api/v1/preferences");
  expect(after.onboarded_at).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
  await page.goto("/#/audit");
  await expect(page.getByRole("list").getByRole("listitem").first()).toContainText("finished setting Canyon up");
  const trail = await audit(session);
  expect(trail[0]?.action).toBe("complete setup");
  // A fresh install has seen one sign-in, the admin's: alice's refusal wrote none.
  const signIns = trail.filter((e) => e.action === "sign in");
  expect(signIns, `sign in rows: ${JSON.stringify(signIns.map((e) => e.actor))}`).toHaveLength(1);
});

test("A2 front door, choice 2: the probe passes", async ({ page }) => {
  const session = await signIn(page, "admin");
  const secret = bed("E2E_OIDC_CLIENT_SECRET");
  await page.goto("/#/settings/hosting");
  const door = settings(page);
  await expect(door).toContainText("Nobody can reach an app until this is set, so no app can run on this Canyon yet.");
  await door.getByRole("radio", { name: "Canyon signs people in" }).check();
  await expect(door.getByText("A sign-in proxy runs beside each app, against your sign-in provider.")).toBeVisible();
  await expect(door.getByText(/^Your sign-in provider must accept every app's sign-in address in both environments/)).toBeVisible();
  await door.getByLabel("Issuer").fill(bed("E2E_OIDC_ISSUER"));
  await door.getByLabel("Client ID").fill(bed("E2E_OIDC_CLIENT_ID"));
  await door.getByLabel("Client secret").fill(secret);
  await door.getByLabel("Domain").fill(bed("E2E_APPS_DOMAIN"));
  await door.getByRole("radio", { name: `https://claims-portal.preview.${bed("E2E_APPS_DOMAIN")}` }).check(); // the app.env.domain form, by its example address
  await door.getByRole("button", { name: "Save how people reach apps" }).click();
  const status = door.getByRole("status");
  await expect(status).toHaveText("Checking this cluster and the certificate…");
  await expect(status).toHaveText("Saved. Apps change at their next deploy.", { timeout: 90_000 });

  const read = await raw(session, "GET", "/api/v1/front-door");
  expect(read.status).toBe(200);
  const { front_door } = read.json as FrontDoor;
  expect(front_door).toMatchObject({ choice: 2, signin: { issuer: bed("E2E_OIDC_ISSUER"), client_id: bed("E2E_OIDC_CLIENT_ID") } });
  expect(read.text).not.toContain("client_secret");
  expect(read.text).not.toContain(secret);
  const trail = await audit(session);
  const row = trail.find((e) => e.action === "set front door");
  expect(row?.detail.secret_replaced, `set front door row: ${JSON.stringify(row?.detail)}`).toBe(true);
  expect(JSON.stringify(trail), "the trail holds the client secret's value").not.toContain(secret);
  await expect.poll(() => kube("canyon-apps", "pods,jobs,services,networkpolicies", "canyon.probe=true").map((o) => o.metadata.name), { timeout: 30_000, message: "objects labelled canyon.probe left in canyon-apps" }).toEqual([]);
});

test("A3 the front door, outbound and playbooks refuse a member", async ({ page }) => {
  const session = await signIn(page, "alice");
  await page.goto("/#/settings/hosting");
  const door = settings(page);
  await expect(door).toContainText("Only an admin can change how people reach apps.");
  await expect(door).toContainText("Canyon signs people in"); // the stored door is shown, read-only: A2 did save one
  await expect(door.getByRole("textbox")).toHaveCount(0);
  await expect(door.getByRole("button", { name: "Save how people reach apps" })).toHaveCount(0);
  const newest = (await audit(session))[0]?.id;
  const put = await raw(session, "PUT", "/api/v1/front-door", { choice: 1, gateway: "alb", alb_arn: "arn:aws:elasticloadbalancing:eu-central-1:000000000000:loadbalancer/app/e2e/0", form: "app.env.domain" });
  expect(put.status, put.text).toBe(403);
  expect((put.json as { error: string }).error).toBe("admin_required");
  expect((await audit(session))[0]?.id, "the refused PUT wrote an audit row").toBe(newest);
  expect(((await raw(session, "GET", "/api/v1/front-door")).json as FrontDoor).front_door?.choice).toBe(2);

  // Settings › Outbound is every member's to read and an admin's to change (#2422; design/PERMISSIONS.md, Outbound).
  await page.goto("/#/settings/outbound");
  const outbound = page.getByRole("region", { name: "Outbound" });
  await expect(outbound).toContainText("Only an admin can change how apps call out.");
  await expect(outbound.getByRole("group", { name: /^How apps call out/ })).toBeVisible();
  await expect(outbound.getByRole("radio")).toHaveCount(0);
  await expect(outbound.getByRole("button")).toHaveCount(0);
  const mode = await raw(session, "PUT", "/api/v1/egress", { mode: "allowlist" });
  expect(mode.status, mode.text).toBe(403);
  expect((mode.json as { error: string }).error).toBe("admin_required");

  // The org's playbooks are every member's to read, through the API an agent reads them by, and an admin's to write,
  // in Guardrails, an admin's screen (design/specs/mcp.md §3.2; design/PERMISSIONS.md, Playbooks).
  await page.goto("/#/guardrails/playbooks");
  await expect(page.getByText("Only an admin can open Guardrails.")).toBeVisible();
  const list = await raw(session, "GET", "/api/v1/playbooks");
  expect(list.status, list.text).toBe(200);
  const write = await raw(session, "PUT", "/api/v1/playbooks/a3-member", { name: "A3", kind: "note", when: "Never.", body: "Never saved.\n" });
  expect(write.status, write.text).toBe(403);
  expect((write.json as { error: string }).error).toBe("admin_required");
  expect((await raw(session, "GET", "/api/v1/playbooks/a3-member")).status).toBe(404);
});

test("A5 apps call out through Canyon", async ({ page }) => {
  const session = await signIn(page, "admin");
  await page.goto("/#/settings/outbound");
  const outbound = page.getByRole("region", { name: "Outbound" });
  await outbound.getByRole("radio", { name: "Through Canyon" }).check();
  // It changes what apps can do, so it asks first (design/EXPERIENCE.md, D16).
  await outbound.getByRole("group", { name: "Send apps' calls through Canyon?" }).getByRole("button", { name: "Turn on Through Canyon" }).click();
  await expect(outbound.getByRole("radio", { name: "Allowlist" })).toBeEnabled();
  expect((await raw(session, "GET", "/api/v1/egress")).json).toMatchObject({ via: "canyon", mode: "observe", receiver: true, canyon_refused: null });
  expect((await audit(session))[0], "the save's audit row").toMatchObject({ action: "egress.set" });
});

test("A4 front door, choice 1: the gateway signs people in", async ({ page }) => {
  // Given phase 2: e2e-peer runs under choice 2, its pod carrying oauth2-proxy as an init container.
  const peer = podNamed("canyon-apps", "e2e-peer-preview");
  expect(peer, "A4 needs phase 2's e2e-peer-preview pod, and there is none").toBeDefined();
  const proxied = Object.entries(peer!.initImages).find(([, image]) => image.includes("oauth2-proxy"));
  expect(proxied, `e2e-peer-preview's init containers ${JSON.stringify(peer!.initImages)} hold no oauth2-proxy image: it was not deployed under choice 2`).toBeDefined();
  // The hook: the test cluster's OIDC-authenticating listener rule in front of the apps (README, The hook).
  // Its stdout is shown, never captured into an assertion: it is the test cluster's, not this suite's. The
  // runner switches back at the run's end (CASES A4, Cleanup), not this test: M8 and M9 need choice 1.
  execFileSync(bed("E2E_SWITCH_FRONT_DOOR"), ["alb"], { stdio: "inherit" });

  const session = await signIn(page, "admin");
  await page.goto("/#/settings/hosting");
  const door = settings(page);
  await door.getByRole("radio", { name: "Your sign-in proxy signs people in" }).check();
  await door.getByRole("radio", { name: "AWS load balancer sign-in" }).check();
  await door.getByLabel("Load balancer ARN").fill(bed("E2E_ALB_ARN"));
  await door.getByRole("button", { name: "Save how people reach apps" }).click();
  await expect(door.getByRole("status")).toHaveText("Saved. Apps change at their next deploy.", { timeout: 90_000 });
  const { front_door } = (await raw(session, "GET", "/api/v1/front-door")).json as FrontDoor;
  expect(front_door).toMatchObject({ choice: 1, gateway: "alb" });
  // e2e-peer keeps the door it was deployed with until M8 redeploys it: the same pod, still proxied.
  const still = podNamed("canyon-apps", "e2e-peer-preview");
  expect(still?.uid, "e2e-peer-preview's pod was replaced by the front-door change").toBe(peer!.uid);
  expect(still?.initImages).toEqual(peer!.initImages);
});
