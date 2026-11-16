import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, deploy, review, upload } from "../harness/seed";

type Finding = { partialFingerprints: Record<string, string>; properties: { canyon: { blocks: boolean } } };

// The core app (a declared Postgres) is approved and deployed to preview without the browser; the
// journey is the promotion a person clicks: prod's database comes from the stub's adapter.
journey("promote to production from the app page", {
  given: async () => {
    const app = appId("ux-promote");
    await upload(app, "f-core");
    const run = await review(app);
    const { run: read } = await api<{ run: { findings: Finding[] } }>(null, "GET", `/api/v1/gate-runs/${run}`);
    const expires_at = new Date(Date.now() + 30 * 864e5).toISOString().slice(0, 10) + "T00:00:00Z";
    for (const f of read.findings.filter((one) => one.properties.canyon.blocks)) {
      await api(null, "POST", `/api/v1/gate-runs/${run}/findings/${f.partialFingerprints["canyon/v1"]}/approve`, { reason: "UX journey fixture", expires_at });
    }
    await deploy(app);
    return { app };
  },
  steps: [
    {
      name: "open the app running in preview",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}`); },
      see: async (page, { app }) => {
        await expect(page.getByRole("link", { name: /in preview$/ })).toHaveAttribute("href", new RegExp(`${app}\\.preview\\.localhost:7777`));
        await expect(page.getByRole("button", { name: /^Promote .*to production$/ })).toBeEnabled();
      },
    },
    {
      name: "promote, confirm, and watch it go live",
      do: async (page) => {
        await page.getByRole("button", { name: /^Promote .*to production$/ }).click();
        await expect(page.getByText("Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.")).toBeVisible();
        await page.getByRole("button", { name: /^Promote \S+$/ }).click();
      },
      see: async (page, { app }) => {
        // Green and live, its Open production in the headline: the fixture's secret, approved with a reason, is no need (#2765).
        await expect(page.getByRole("link", { name: "Open production" })).toHaveAttribute("href", new RegExp(`${app}\\.prod\\.localhost:7777`), { timeout: 5 * 60_000 });
      },
      settle: 5 * 60_000,
      changes: {
        runs: { added: 1 },
        deploys: { added: 1 },
        deploy_history: { added: 1 },
        resources: { added: 1 },
        secrets: { added: 1 },
        audit_events: { added: 3, actions: ["POST /api/v1/projects/{id}/promotions", "ensure resource", "deploy prod"] },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
        containers: { started: 1 },
        stub: { added: 1 },
      },
    },
  ],
});
