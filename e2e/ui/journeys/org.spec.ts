import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { api } from "../harness/seed";

const put = "PUT /api/v1/preferences";

journey("rename the organization, then name it back", {
  given: async () => {
    await api(null, "PUT", "/api/v1/preferences", { org_name: "Meridian" }); // a run stopped midway may have left it renamed
    return { was: "Meridian" };
  },
  steps: [
    {
      name: "open Settings › Organization",
      do: async (page) => { await page.goto("/#/settings/organisation"); },
      see: async (page, { was }) => { await expect(page.getByLabel("Organization name")).toHaveValue(was); },
    },
    {
      name: "rename it",
      do: async (page) => {
        await page.getByLabel("Organization name").fill("Meridian UX");
        await page.getByRole("region", { name: "Organization" }).getByRole("button", { name: "Save the organization name" }).click();
      },
      see: async (page) => {
        await expect(page.getByRole("region", { name: "Organization" }).getByText("Saved.")).toBeVisible();
        const menu = page.getByRole("button", { name: "Menu" }); // on a phone the rail folds its views behind it
        if (await menu.isVisible()) await menu.click();
        await page.getByRole("link", { name: "Fleet" }).click();
        await expect(page.getByRole("heading", { name: "Meridian UX" })).toBeVisible();
      },
      changes: { org_preferences: { changed: 1 }, audit_events: { added: 1, actions: [put] } },
    },
    {
      name: "name it back",
      do: async (page, { was }) => {
        await page.goto("/#/settings/organisation");
        await page.getByLabel("Organization name").fill(was);
        await page.getByRole("region", { name: "Organization" }).getByRole("button", { name: "Save the organization name" }).click();
      },
      see: async (page) => { await expect(page.getByRole("region", { name: "Organization" }).getByText("Saved.")).toBeVisible(); },
      changes: { org_preferences: { changed: 1 }, audit_events: { added: 1, actions: [put] } },
    },
  ],
});
