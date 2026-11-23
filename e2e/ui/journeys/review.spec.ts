import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { appId, upload } from "../harness/seed";

journey("run the first review of an uploaded app", {
  given: async () => {
    const app = appId("ux-review");
    await upload(app, "f-cp");
    return { app };
  },
  steps: [
    {
      name: "open an app no run has looked at",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}`); },
      see: async (page) => {
        await expect(page.getByText("Never reviewed. Canyon can't tell you whether this app is safe.")).toBeVisible();
        await expect(page.getByRole("button", { name: /^Review \S+$/ })).toBeVisible();
      },
    },
    {
      name: "run the review",
      do: async (page) => { await page.getByRole("button", { name: /^Review \S+$/ }).click(); },
      see: async (page) => {
        // 180 s: when this journey runs first on a fresh stack, it is trivy's first cold run there, which downloads its
        // database while the other scanners wait their turn (seed.ts's review allows ten minutes for the same reason)
        await expect(page.getByRole("main")).toContainText("Blocked", { timeout: 180_000 });
        await expect(page.getByRole("article", { name: /^A secret is in / })).toBeVisible();
      },
      changes: {
        runs: { added: 1 },
        // the five tools Canyon runs by default each attach their report (#1381, #1388), writing no audit row (#1704)
        audit_events: { added: 2, actions: ["POST /api/v1/projects/{id}/gate-runs", "verdict"] },
        package_reports: { added: 5 },
        timeline_rows: { added: "any" },
        projects: { changed: "any" },
      },
    },
  ],
});
