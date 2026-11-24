import { expect, test, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { opensInside } from "../harness/menu";
import { api, appId, deploy, promote, review, upload } from "../harness/seed";

type Finding = { partialFingerprints: Record<string, string>; properties: { canyon: { blocks: boolean } } };

// The core app runs in production; the journey stops and holds it from the fleet, reads the banner on the
// app page and, where Promote would be, what lifting the hold starts (preview runs the version the hold
// stopped: #2723), and lifts the hold from the fleet, starting what production ran again (design/OPERATIONS.md §3.8). On Docker nobody signs in and every caller is the anonymous admin,
// who holds and lifts: a responder's session needs a sign-in, which only the test cluster has.
const fleetRow = (page: Page, app: string) => page.goto(`/#/fleet?q=${app}`);
// The rows, a table or the phone's cards; on a desktop a row's side pane is in the same group.
const rows = (page: Page) => page.locator("[aria-label='Apps']");
const wide = (page: Page) => (page.viewportSize()?.width ?? 1440) > 700;
// A team row's side pane, on a desktop: Stop production and Lift the hold are there, never on the row (the UX review).
const pane = async (page: Page, app: string) => { if (wide(page)) await page.getByRole("button", { name: `Details for ${app}` }).click(); };
// Stop production where a person finds it: the pane on a desktop; on a phone, whose cards offer no stop, the app page's
// ⋯ menu (#102), and back to the fleet.
const stop = async (page: Page, app: string, reason: string) => {
  await fleetRow(page, app);
  if (wide(page)) {
    await pane(page, app);
    await page.getByRole("button", { name: `Stop production of ${app}` }).click();
  } else {
    await rows(page).getByRole("link", { name: app, exact: true }).click();
    const more = page.getByRole("button", { name: "More actions" });
    await opensInside(page, more, page.getByRole("menu").getByRole("menuitem"));
    await more.click();
    await page.getByRole("menuitem", { name: "Stop production…" }).click();
  }
  const dialog = page.getByRole("dialog", { name: `Stop and hold ${app} in production?` });
  await dialog.getByLabel("Reason").fill(reason);
  await dialog.getByRole("button", { name: "Stop production" }).click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  if (!wide(page)) await fleetRow(page, app);
};
// Lift the hold on the fleet: in the pane on a desktop, which offers it once, in Needs attention on a phone.
const lift = (page: Page, app: string) => (wide(page) ? rows(page) : page.getByRole("main")).getByRole("button", { name: `Lift the hold on ${app}` });

// A journey that failed part way leaves its app held, and the outbound journeys after it on the shard are then refused 409 held.
let held = "";
test.afterEach(async () => {
  if (held === "") return;
  const { project } = await api<{ project: { hold?: { at: string } } }>(null, "GET", `/api/v1/projects/${held}`);
  if (project.hold !== undefined) await api(null, "DELETE", `/api/v1/projects/${held}/envs/prod/hold?held_at=${encodeURIComponent(project.hold.at)}`);
});

journey("stop production, read the banner, lift the hold", {
  given: async () => {
    const app = held = appId("ux-hold");
    const pkg = await upload(app, "f-core");
    const run = await review(app);
    const { run: read } = await api<{ run: { findings: Finding[] } }>(null, "GET", `/api/v1/gate-runs/${run}`);
    const expires_at = new Date(Date.now() + 30 * 864e5).toISOString().slice(0, 10) + "T00:00:00Z";
    for (const f of read.findings.filter((one) => one.properties.canyon.blocks)) {
      await api(null, "POST", `/api/v1/gate-runs/${run}/findings/${f.partialFingerprints["canyon/v1"]}/approve`, { reason: "UX journey fixture", expires_at });
    }
    await promote(app, await deploy(app));
    return { app, version: pkg.slice("pkg_".length) };
  },
  steps: [
    {
      name: "stop production from the fleet, with a reason",
      do: async (page, { app }) => stop(page, app, "UX journey: exports claimant data unauthenticated"),
      see: async (page, { app }) => {
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await expect(page.getByRole("main").getByText("Held", { exact: true }).first()).toBeVisible(); // the row's, and on a desktop its pane's too
        await expect(lift(page, app)).toBeVisible();
      },
      changes: {
        prod_holds: { added: 1 },
        deploys: { removed: 1 },
        audit_events: { added: 1, actions: ["stop app"] },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
        containers: { removed: "any", changed: "any" },
      },
    },
    {
      name: "the app page says who stopped it and why, and what lifting the hold starts",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}`); },
      see: async (page, { app, version }) => {
        await expect(page.getByRole("heading", { name: "Production is held" })).toBeVisible();
        await expect(page.getByRole("region", { name: "Where it stands" })).toContainText(": UX journey: exports claimant data unauthenticated. It stays stopped until a responder or an admin lets it run again.");
        await expect(page.getByRole("button", { name: `Lift the hold on ${app}` })).toBeVisible();
        await expect(page.getByText(`${version} is production's version, stopped by the hold. Lifting the hold can start it again.`)).toBeVisible();
        await expect(page.getByRole("button", { name: /^Promote .*to production$/ })).toHaveCount(0);
      },
    },
    {
      name: "lift the hold from the fleet, starting what production ran again",
      do: async (page, { app, version }) => {
        await fleetRow(page, app);
        await pane(page, app);
        await lift(page, app).click();
        await page.getByRole("dialog", { name: `Lift the hold on ${app}?` }).getByRole("button", { name: `Lift the hold and start ${version}` }).click();
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await page.goto(`/#/apps/${app}`);
      },
      see: async (page, { version }) => {
        await expect(page.getByRole("region", { name: "Production", exact: true }).getByText(version, { exact: true })).toBeVisible({ timeout: 5 * 60_000 });
        await expect(page.getByRole("heading", { name: "Production is held" })).toHaveCount(0);
      },
      settle: 5 * 60_000,
      changes: {
        prod_holds: { removed: 1 },
        runs: { added: 1 },
        deploys: { added: 1 },
        deploy_history: { added: 1 },
        audit_events: { added: 2, actions: ["lift hold", "deploy prod"] },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
        containers: { started: "any", changed: "any", removed: "any" },
      },
    },
    {
      name: "stop production again from the fleet",
      do: async (page, { app }) => stop(page, app, "UX journey: held again, to lift it leaving production stopped"),
      see: async (page, { app }) => {
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await expect(lift(page, app)).toBeVisible();
      },
      changes: {
        prod_holds: { added: 1 },
        deploys: { removed: 1 },
        audit_events: { added: 1, actions: ["stop app"] },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
        containers: { removed: "any", changed: "any" },
      },
    },
    {
      name: "lift the hold leaving production stopped: production is empty",
      do: async (page, { app }) => {
        await lift(page, app).click();
        await page.getByRole("dialog", { name: `Lift the hold on ${app}?` }).getByRole("button", { name: "Lift the hold, keep it stopped" }).click();
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await page.goto(`/#/apps/${app}`);
      },
      see: async (page) => {
        await expect(page.getByRole("region", { name: "Production", exact: true }).getByText("Nothing running", { exact: true })).toBeVisible();
        await expect(page.getByRole("heading", { name: "Production is held" })).toHaveCount(0);
      },
      changes: {
        prod_holds: { removed: 1 },
        audit_events: { added: 1, actions: ["lift hold"] },
        projects: { changed: "any" },
      },
    },
  ],
});
