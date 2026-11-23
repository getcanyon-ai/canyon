import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { insideWindow } from "../harness/menu";
import { api, appId, review, upload } from "../harness/seed";

journey("read the audit trail by actor and time range", {
  // Canyon's own rows: a review's verdict is canyon's. Seeded here, as this journey may run first on a fresh stack,
  // before any other journey has written one (its shard's order moves with every journey added).
  given: async () => {
    const app = appId("ux-audit");
    await upload(app, "f-cp");
    await review(app);
    return {};
  },
  steps: [
    {
      name: "open the audit trail",
      do: async (page) => { await page.goto("/#/audit"); },
      see: async (page) => { await expect(page.getByRole("heading", { name: "Audit" })).toBeVisible(); },
    },
    {
      name: "the Export menu opens inside the window",
      do: async (page) => { await page.locator("summary", { hasText: "Export" }).click(); },
      see: async (page) => {
        await insideWindow(page, page.locator("details[open]").filter({ hasText: "Export" }).getByRole("button"));
        await page.locator("summary", { hasText: "Export" }).click(); // closed again
      },
    },
    {
      name: "Canyon's own rows only",
      do: async (page) => { await page.getByRole("link", { name: "Canyon", exact: true }).click(); },
      see: async (page) => {
        await expect(page).toHaveURL(/actor=canyon/);
        await expect(page.getByRole("main").locator("tbody tr:has(button[aria-expanded])").first()).toContainText("Canyon");
      },
    },
    {
      name: "people over the last 30 days",
      do: async (page) => {
        await page.getByRole("link", { name: "People", exact: true }).click();
        await expect(page.getByRole("link", { name: "People", exact: true })).toHaveAttribute("aria-current", "true"); // drawn, as a person would see it
        await page.getByLabel("Time range").selectOption({ label: "Last 30 days" });
      },
      see: async (page) => { await expect(page).toHaveURL(/actor=person/); await expect(page).toHaveURL(/range=30d/); },
    },
    {
      // It waits in a bar above the table, so the rows never move under the reader, and comes in when asked for.
      name: "a change made elsewhere appears on the open screen, no reload",
      do: async (page) => {
        await api(null, "PUT", "/api/v1/policies/sql.string-concat?apply=true", { state: "advisory" });
        await page.getByRole("button", { name: /new events? · Show$/ }).click({ timeout: 10_000 });
      },
      see: async (page) => { await expect(page.getByRole("main").locator("tbody tr:has(button[aria-expanded])").first()).toContainText("Set the policy No hand-built SQL from user input to advisory", { timeout: 10_000 }); },
      changes: { policy_edits: { added: 1 }, audit_events: { added: 1, actions: ["policy.set"] } },
    },
    {
      name: "and back",
      do: async (page) => {
        await api(null, "PUT", "/api/v1/policies/sql.string-concat?apply=true", { state: "blocking" });
        await page.getByRole("button", { name: /new events? · Show$/ }).click({ timeout: 10_000 });
      },
      see: async (page) => { await expect(page.getByRole("main").locator("tbody tr:has(button[aria-expanded])").first()).toContainText("Set the policy No hand-built SQL from user input to blocking", { timeout: 10_000 }); },
      changes: { policy_edits: { added: 1 }, audit_events: { added: 1, actions: ["policy.set"] } },
    },
  ],
});
