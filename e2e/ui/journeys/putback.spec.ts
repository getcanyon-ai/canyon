import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, deploy, promote, review, upload, uploadVersion } from "../harness/seed";

type Finding = { partialFingerprints: Record<string, string>; properties: { canyon: { blocks: boolean } } };

// Production ran the core app's first version, then a second (design/OPERATIONS.md §1): the journey
// puts the first back from the production card, a review under today's rules and then the exact
// image production ran, promoted by the person who clicked.
journey("put an earlier version back in production", {
  given: async () => {
    const app = appId("ux-putback");
    const first = await upload(app, "f-core");
    const run = await review(app);
    const { run: read } = await api<{ run: { findings: Finding[] } }>(null, "GET", `/api/v1/gate-runs/${run}`);
    const expires_at = new Date(Date.now() + 30 * 864e5).toISOString().slice(0, 10) + "T00:00:00Z";
    for (const f of read.findings.filter((one) => one.properties.canyon.blocks)) {
      await api(null, "POST", `/api/v1/gate-runs/${run}/findings/${f.partialFingerprints["canyon/v1"]}/approve`, { reason: "UX journey fixture", expires_at });
    }
    await promote(app, await deploy(app));
    await uploadVersion(app, "f-core", { "PUT-BACK.md": "the second version\n" }, null, first);
    await promote(app, await deploy(app));
    return { app, v1: first.slice("pkg_".length) };
  },
  steps: [
    {
      name: "put the first version back: Canyon reviews it again",
      do: async (page, { app, v1 }) => {
        await page.goto(`/#/apps/${app}`);
        const card = page.getByRole("region", { name: "Production", exact: true });
        await card.getByText(/^Earlier versions · /).click();
        await card.getByRole("button", { name: `Put back ${v1}` }).click();
      },
      see: async (page, { v1 }) => {
        await expect(page.getByRole("button", { name: `Promote ${v1}` })).toBeEnabled({ timeout: 5 * 60_000 });
        await expect(page.getByText("Production gets the image it ran on", { exact: false })).toBeVisible();
      },
      settle: 5 * 60_000,
      changes: {
        runs: { added: 1 },
        audit_events: { added: 2, actions: ["POST /api/v1/projects/{id}/gate-runs", "verdict"] },
        package_reports: { added: 5 },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
      },
    },
    {
      name: "promote it, and production runs the first version again",
      do: async (page, { v1 }) => {
        const card = page.getByRole("region", { name: "Production", exact: true });
        await card.getByRole("button", { name: `Promote ${v1}`, exact: true }).click();
        // the question before production changes, the keyboard on Cancel, as every promote's (#157, #188)
        await expect(card.getByText(/^Put .+ back in production(, replacing .+)?\?$/)).toBeVisible();
        await expect(card.getByRole("button", { name: "Cancel", exact: true })).toBeFocused();
        await card.getByRole("button", { name: `Promote ${v1}`, exact: true }).click();
      },
      see: async (page, { v1 }) => {
        // what production runs is named on the card's first line; the earlier rows carry a date after theirs. Watched as
        // it switches: it reads starting, never down, the moment after Promote (#230)
        const runs = page.getByRole("region", { name: "Production", exact: true }).getByText(v1, { exact: true });
        for (const until = Date.now() + 5 * 60_000; !(await runs.isVisible()); await page.waitForTimeout(250)) {
          expect(await page.getByText(/^Production is down/).count(), "the band said production was down while it switched").toBe(0);
          expect(Date.now(), `production never showed ${v1}`).toBeLessThan(until);
        }
        await expect(runs).toBeVisible();
      },
      settle: 5 * 60_000,
      changes: {
        runs: { added: 1 },
        deploys: { changed: 1 },
        deploy_history: { added: 1 },
        audit_events: { added: 2, actions: ["POST /api/v1/projects/{id}/promotions", "deploy prod"] },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
        containers: { changed: 1 },
      },
    },
  ],
});
