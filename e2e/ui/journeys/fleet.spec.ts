import { expect } from "@playwright/test";
import { journey } from "../harness/factory";

// Reading the fleet writes nothing: every line of what needs a person, filter and panel is a request, never a change.
journey("narrow the fleet with tiles, filters and the details panel", {
  steps: [
    {
      name: "open the fleet",
      do: async (page) => { await page.goto("/#/fleet"); },
      // What needs a person heads the fleet, one line a need (D15), whoever reads it; a table, or cards on a phone.
      see: async (page) => { await expect(page.getByRole("region", { name: /^Needs (attention|you)$/ })).toBeVisible(); },
    },
    {
      name: "the blocked tile narrows to blocked apps",
      // A tile is its address: ?tile=blocked selects exactly the apps it counts.
      do: async (page) => { await page.goto("/#/fleet?tile=blocked"); },
      see: async (page) => {
        await expect(page).toHaveURL(/tile=blocked/);
        const latest = page.locator("tbody tr, [aria-label='Apps'] > li");
        // until the filtered page replaces the one the click left: rows that are not blocked, none
        await expect.poll(async () => (await latest.allInnerTexts()).filter((row) => !row.includes("Blocked")).length).toBe(0);
        await expect(latest.first()).toContainText("Blocked");
      },
    },
    {
      name: "Needs you, then clear the filters",
      do: async (page) => {
        await page.getByRole("button", { name: /^Filter/ }).click();
        await page.getByRole("checkbox", { name: "Only apps that need you" }).click(); // the address is the state: it ticks once the query lands
        await expect(page).toHaveURL(/needs_you=true/);
        await page.keyboard.press("Escape");
        await page.getByRole("link", { name: "Clear filters" }).click();
      },
      see: async (page) => { await expect(page).toHaveURL(/#\/fleet$|#\/fleet\?$/); },
    },
    {
      // A phone draws cards that open the app page; the panel is the wide screen's.
      name: "open a row's details",
      do: async (page) => { if ((page.viewportSize()?.width ?? 1440) > 700) await page.getByRole("button", { name: /^Details for / }).first().click(); },
      see: async (page) => { if ((page.viewportSize()?.width ?? 1440) > 700) await expect(page.getByRole("complementary").first()).toBeVisible(); },
    },
  ],
});
