import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, deploy, promote, review, upload } from "../harness/seed";

type Finding = { partialFingerprints: Record<string, string>; properties: { canyon: { blocks: boolean } } };

// The core app runs in production; the journey stops and holds it from the fleet, reads the banner on the
// app page and, where Promote would be, what lifting the hold starts (preview runs the version the hold
// stopped: #2723), and lifts the hold from the fleet, starting what production ran again (design/OPERATIONS.md §3.8). On Docker nobody signs in and every caller is the anonymous admin,
// who holds and lifts: a responder's session needs a sign-in, which only the test cluster has.
const fleetRow = (page: Page, app: string) => page.goto(`/#/fleet?q=${app}`);
// The rows, a table or the phone's cards, with a row's side pane: Needs you above them offers the same Lift on a held
// production (D15).
const rows = (page: Page) => page.locator("[aria-label='Apps']");
// A team row's side pane, where Stop production and Lift the hold are on a desktop (the UX review: never on every
// row); a phone's card, which has no pane, offers them itself.
const peek = async (page: Page, app: string) => {
  await rows(page).getByRole("link", { name: app, exact: true }).first().waitFor(); // the page read, before asking what it offers
  const details = page.getByRole("button", { name: `Details for ${app}` });
  if (await details.count() > 0) await details.click();
};

journey("stop production, read the banner, lift the hold", {
  given: async () => {
    const app = appId("ux-hold");
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
      do: async (page, { app }) => {
        await fleetRow(page, app);
        await peek(page, app);
        await page.getByRole("button", { name: `Stop production of ${app}` }).click();
        const dialog = page.getByRole("dialog", { name: `Stop and hold ${app} in production?` });
        await dialog.getByLabel("Reason").fill("UX journey: exports claimant data unauthenticated");
        await dialog.getByRole("button", { name: "Stop production" }).click();
      },
      see: async (page, { app }) => {
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await expect(page.getByRole("main").getByText("Held", { exact: true })).toBeVisible();
        await expect(rows(page).getByRole("button", { name: `Lift the hold on ${app}` })).toBeVisible();
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
        await peek(page, app);
        await rows(page).getByRole("button", { name: `Lift the hold on ${app}` }).click();
        await page.getByRole("dialog", { name: `Lift the hold on ${app}?` }).getByRole("button", { name: `Start ${version} again` }).click();
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
      do: async (page, { app }) => {
        await fleetRow(page, app);
        await peek(page, app);
        await page.getByRole("button", { name: `Stop production of ${app}` }).click();
        const dialog = page.getByRole("dialog", { name: `Stop and hold ${app} in production?` });
        await dialog.getByLabel("Reason").fill("UX journey: held again, to lift it leaving production stopped");
        await dialog.getByRole("button", { name: "Stop production" }).click();
      },
      see: async (page, { app }) => {
        await expect(page.getByRole("dialog")).toHaveCount(0);
        await expect(rows(page).getByRole("button", { name: `Lift the hold on ${app}` })).toBeVisible();
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
        await rows(page).getByRole("button", { name: `Lift the hold on ${app}` }).click();
        await page.getByRole("dialog", { name: `Lift the hold on ${app}?` }).getByRole("button", { name: "Leave production stopped" }).click();
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
