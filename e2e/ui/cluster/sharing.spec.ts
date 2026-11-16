// S1-S4 (e2e/cluster/CASES.md, S — Sharing) on the test cluster, serially: S3 compares the pod S1 noted.
// Given M5: e2e-app runs in preview, owned by alice. Alice shares through the CLI as the case says,
// as herself (a config home holding her browser session); bob reads the app in a browser of his
// own; every API request names its session.
import { expect, test, type Browser, type Page } from "@playwright/test";
import { api, host } from "../harness/seed";
import { signIn } from "../harness/signin";
import { appUrl, audit, canyon, cliHome, configMapAnnotations, email, parseWhoami, podNamed, raw, until, type PodSummary } from "../harness/cluster";

test.describe.configure({ mode: "serial" });


const app = "e2e-app";
const whoami = () => appUrl(app, "preview") + "/whoami"; // read when a case runs, so listing needs no test cluster
const noAccess = `${host}/#/no-access/${app}`;
const reach = 120_000; // the kubelet's ConfigMap sync plus the sidecar's re-read (README, Waits)

type Grant = { kind: string; grantee: string; level: string; roles: string[] };
type Access = { entries: Grant[]; requests: { email: string }[] };
type Project = { project: { owners: { display: string }[]; envs: Record<string, unknown> } };

let noted: PodSummary | undefined; // e2e-app-preview's pod as S1 found it

// given signs alice in, checks M5's state is there, and opens bob's own browser.
async function given(page: Page, browser: Browser) {
  const alice = await signIn(page, "alice");
  const { project } = await api<Project>(alice, "GET", `/api/v1/projects/${app}`);
  expect(project.envs.preview, `${app} is not running in preview: S needs M5`).toBeDefined();
  const owner = project.owners[0]?.display;
  expect(owner, `${app} has no owner`).toBeDefined();
  const bobs = await browser.newContext();
  const bobPage = await bobs.newPage();
  const bob = await signIn(bobPage, "bob");
  return { alice, bob, bobPage, owner: owner!, home: cliHome(alice), close: () => bobs.close() };
}

// bobReads loads /whoami as bob and says whether the app answered as him, else where he landed.
async function bobReads(bobPage: Page): Promise<{ done: boolean; saw: string }> {
  const res = await bobPage.goto(whoami());
  const url = bobPage.url();
  if (new URL(url).origin !== new URL(whoami()).origin) return { done: false, saw: `${res?.status() ?? "-"} ${url}` };
  const who = parseWhoami(await bobPage.locator("body").innerText());
  return { done: who.email === email("bob") && who.verified === "true", saw: `${res?.status()} ${JSON.stringify(who)}` };
}

const share = (home: string, ...args: string[]) => canyon(home, ["share", "--app", app, ...args]);

test("S1 share with bob as viewer; bob opens the app within two minutes", async ({ page, browser }) => {
  const { alice, bob, bobPage, owner, home, close } = await given(page, browser);
  try {
    noted = podNamed("canyon-apps", `${app}-preview`);
    expect(noted, `no pod named ${app}-preview-* in canyon-apps`).toBeDefined();
    // The thing about to change: bob is refused today, at the no-access screen, not the app.
    const before = await bobReads(bobPage);
    expect(before.done, `bob already reaches ${app}: ${before.saw}`).toBe(false);
    expect(bobPage.url()).toBe(noAccess);

    const shared = share(home, "--level", "read-only", email("bob"));
    expect(shared.code, shared.stderr).toBe(0);
    const from = Date.now();
    const saw = await until(reach, `bob to reach ${app}`, () => bobReads(bobPage));
    test.info().annotations.push({ type: "reached", description: `${Math.round((Date.now() - from) / 1000)}s: ${saw}` });

    const after = podNamed("canyon-apps", `${app}-preview`);
    expect(after?.uid, "the pod was replaced").toBe(noted!.uid);
    expect(after?.restarts, "a container restarted").toBe(noted!.restarts);
    const access = await api<Access>(alice, "GET", `/api/v1/projects/${app}/access`);
    expect(access.entries).toContainEqual(expect.objectContaining({ kind: "email", grantee: email("bob"), level: "viewer", roles: [] }));
    await page.goto("/#/audit");
    await expect(page.getByRole("listitem").filter({ hasText: `shared the app with ${email("bob")} as read-only` })).toHaveCount(1);
    // Bob's dashboard: the reduced row -- name, owner, summary -- and the app page without the team's controls.
    await bobPage.goto("/#/fleet");
    const row = bobPage.getByRole("row").filter({ has: bobPage.getByRole("link", { name: app, exact: true }) });
    await expect(row).toContainText(owner); // the owner cell names its owner in full (web/src/screens/Fleet.tsx, Owner)
    await expect(row.getByRole("button")).toHaveCount(0);
    await bobPage.goto(`/#/apps/${app}`);
    await expect(bobPage.getByRole("heading", { name: app })).toBeVisible();
    await expect(bobPage.getByRole("button", { name: /^Promote .*to production$/ })).toHaveCount(0);
    await expect(bobPage.getByRole("link", { name: "Open preview" })).toHaveCount(0);
    await expect(bobPage.getByRole("region", { name: "Review" })).toHaveCount(0);
    const secrets = await raw(bob, "GET", `/api/v1/projects/${app}/secrets`);
    expect(secrets.status, secrets.text).toBe(403);
    expect(secrets.json).toMatchObject({ error: "team_required", message: `Ask ${owner} to add you to ${app}'s team: on its page in Canyon, use Ask to join its team.` });
  } finally {
    await close();
  }
});

test("S2 roles from [roles] reach the app", async ({ page, browser }) => {
  const { alice, bobPage, home, close } = await given(page, browser);
  try {
    const before = await bobReads(bobPage);
    expect(before, "S2 starts from S1's grant").toMatchObject({ done: true });
    expect(parseWhoami(await bobPage.locator("body").innerText()).roles).toBe("-");
    const granted = share(home, "--level", "read-only", "--role", "approver", email("bob"));
    expect(granted.code, granted.stderr).toBe(0);
    await until(reach, `bob's roles to reach ${app}`, async () => {
      const read = await bobReads(bobPage);
      return { done: read.done && parseWhoami(await bobPage.locator("body").innerText()).roles === "approver", saw: read.saw };
    });
    const held = await api<Access>(alice, "GET", `/api/v1/projects/${app}/access`);
    const refused = share(home, "--level", "read-only", "--role", "nosuch", email("bob"));
    expect(refused.code, `canyon share --role nosuch: exit ${refused.code}\n${refused.stdout}${refused.stderr}`).toBe(1);
    expect(refused.stderr).toContain(`no package of ${app} declares the role "nosuch"`);
    expect(await api<Access>(alice, "GET", `/api/v1/projects/${app}/access`)).toEqual(held);
    const row = (await audit(alice, `project=${app}`)).find((e) => e.action === "share app");
    expect(row?.detail.roles, `newest share app row: ${JSON.stringify(row?.detail)}`).toEqual({ before: [], after: ["approver"] });
  } finally {
    await close();
  }
});

test("S3 unshare: bob is refused within two minutes, the pod not restarted", async ({ page, browser }) => {
  const { bobPage, owner, home, close } = await given(page, browser);
  try {
    expect(noted, "S3 needs the pod S1 noted").toBeDefined();
    const before = await bobReads(bobPage);
    expect(before, "S3 starts from S1's grant").toMatchObject({ done: true });
    const stamp = configMapAnnotations("canyon-apps", `canyon-access-${app}-preview`)["canyon.updated_at"];
    expect(stamp, "the access ConfigMap carries no canyon.updated_at").toBeDefined();

    const removed = share(home, "--remove", email("bob"));
    expect(removed.code, removed.stderr).toBe(0);
    await until(reach, `bob to be refused at ${app}`, async () => {
      const read = await bobReads(bobPage);
      return { done: bobPage.url() === noAccess, saw: read.saw };
    });
    await expect(bobPage.getByRole("heading", { name: `You don't have access to ${app}.` })).toBeVisible();
    await expect(bobPage.getByText(`${owner} owns ${app}. Ask them or a Canyon admin for access.`)).toBeVisible();
    await expect(bobPage.getByRole("button", { name: "Ask for access" })).toBeVisible();

    const after = podNamed("canyon-apps", `${app}-preview`);
    expect(after?.uid, "the pod was replaced").toBe(noted!.uid);
    expect(after?.restarts, "a container restarted").toBe(noted!.restarts);
    const advanced = configMapAnnotations("canyon-apps", `canyon-access-${app}-preview`)["canyon.updated_at"];
    expect(advanced > stamp!, `canyon.updated_at ${advanced} did not advance past ${stamp}`).toBe(true);
    await page.goto("/#/audit");
    await expect(page.getByRole("listitem").filter({ hasText: `removed ${email("bob")} from the app` })).toHaveCount(1);
  } finally {
    await close();
  }
});

test("S4 the reduced read and the access request", async ({ page, browser }) => {
  const { alice, bob, bobPage, owner, close } = await given(page, browser);
  try {
    await bobPage.goto(noAccess);
    await bobPage.getByRole("button", { name: "Ask for access" }).click();
    await expect(bobPage.getByRole("status")).toHaveText(`Asked. ${owner} will see your request.`);
    await page.goto(`/#/apps/${app}`);
    const access = page.getByRole("region", { name: "Access" });
    await expect(access.getByRole("heading", { name: "Requests" })).toBeVisible();
    await expect(access).toContainText(email("bob"));
    for (let i = 0; i < 2; i++) expect((await raw(bob, "POST", `/api/v1/projects/${app}/access-requests`)).status).toBe(201);
    const read = await api<Access>(alice, "GET", `/api/v1/projects/${app}/access`);
    expect(read.requests.filter((r) => r.email === email("bob"))).toHaveLength(1);
    const carol = await raw(bob, "PUT", `/api/v1/projects/${app}/access/email/carol@e2e.invalid`, { level: "viewer", roles: [] });
    expect(carol.status, carol.text).toBe(403);
    expect(carol.json).toMatchObject({ error: "owner_required" });
    const last = await raw(alice, "DELETE", `/api/v1/projects/${app}/access/email/${encodeURIComponent(email("alice"))}`);
    expect(last.status, last.text).toBe(409);
    expect(last.json).toMatchObject({ error: "last_owner" });
    await access.getByRole("button", { name: `Dismiss ${email("bob")}` }).click();
    await expect(access.getByRole("heading", { name: "Requests" })).toHaveCount(0);
    expect((await api<Access>(alice, "GET", `/api/v1/projects/${app}/access`)).requests).toEqual([]);
    await bobPage.goto(noAccess);
    await bobPage.reload(); // goto to the #-URL bob is on is a same-document navigation: the screen would keep its "Asked." state
    await expect(bobPage.getByRole("heading", { name: `You don't have access to ${app}.` })).toBeVisible();
    await expect(bobPage.getByRole("button", { name: "Ask for access" })).toBeVisible();
  } finally {
    await close();
  }
});
