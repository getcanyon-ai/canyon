import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { appId, review, upload } from "../harness/seed";

// The rows, a table or the phone's cards: Needs you above them can offer the same Claim (D15).
const rows = (page: Page) => page.locator("[aria-label='Apps']");

journey("claim an app from the fleet", {
  given: async () => {
    const app = appId("ux-claim");
    await upload(app, "f-clean");
    await review(app);
    return { app };
  },
  steps: [
    {
      name: "find the app by name",
      do: async (page, { app }) => {
        await page.goto("/#/fleet");
        await page.getByRole("searchbox", { name: "Search apps" }).fill(app); // sent once the typing settles, no Apply
      },
      see: async (page, { app }) => {
        await expect(page).toHaveURL(new RegExp(`q=${app}`));
        await expect(rows(page).getByRole("button", { name: `Claim ${app}` })).toBeVisible();
      },
    },
    {
      name: "claim it under a name",
      do: async (page, { app }) => {
        await rows(page).getByRole("button", { name: `Claim ${app}` }).click();
        await expect(page.getByRole("button", { name: "Claim", exact: true })).toBeDisabled();
        await page.getByLabel("Your name").fill("Riley Ux");
        await page.getByRole("button", { name: "Claim", exact: true }).click();
      },
      see: async (page, { app }) => { await expect(page.locator("tbody tr, [aria-label='Apps'] > li").filter({ hasText: app })).toContainText("Riley Ux"); },
      changes: { project_access: { added: 1 }, audit_events: { added: 1, actions: ["PUT /api/v1/projects/{id}/owner"] } },
    },
  ],
});
