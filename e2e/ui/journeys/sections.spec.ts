import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, review, upload } from "../harness/seed";

const value = "ux-secret-value-never-shown";

journey("open an app's versions, runs, secrets, owners and audit", {
  given: async () => {
    const app = appId("ux-sections");
    await upload(app, "f-cp");
    await review(app);
    await api(null, "PUT", `/api/v1/projects/${app}/secrets/API_TOKEN`, { value });
    return { app };
  },
  steps: [
    {
      name: "open the app",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}`); },
      see: async (page) => { await expect(page.getByRole("main")).toContainText("Blocked"); },
    },
    {
      // The app page's Secrets section: the secrets by name, never a value.
      name: "read the secrets section",
      do: async (page) => { await page.getByRole("region", { name: "Secrets" }).scrollIntoViewIfNeeded(); },
      see: async (page) => {
        await expect(page.getByRole("region", { name: "Secrets" }).getByRole("cell", { name: "API_TOKEN", exact: true })).toBeVisible();
        await expect(page.locator("body")).not.toContainText(value); // names only, never a value
      },
    },
    {
      name: "open the activity tab",
      do: async (page) => { await page.getByRole("region", { name: "History" }).locator("summary").click(); await page.getByRole("tab", { name: "Activity" }).click(); }, // history is closed until asked for
      see: async (page) => {
        await expect(page.getByText("started a review of", { exact: false }).first()).toBeVisible();
        await expect(page.locator("body")).not.toContainText(value);
      },
    },
  ],
});
