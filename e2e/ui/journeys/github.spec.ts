import { expect } from "@playwright/test";
import { journey } from "../harness/factory";

// Settings › GitHub is an admin's page (#1914); at phone width its form fits, as every page does (the harness checks).
journey("open Settings › GitHub and name an organization", {
  given: async () => ({}),
  steps: [
    {
      name: "open Settings › GitHub",
      do: async (page) => { await page.goto("/#/settings/github"); },
      see: async (page) => { await expect(page.getByRole("heading", { name: "Connect a GitHub organization" })).toBeVisible(); },
    },
    {
      name: "name an organization",
      do: async (page) => { await page.getByLabel("GitHub organization").fill("acme-canyon"); },
      see: async (page) => { await expect(page.getByLabel("GitHub organization")).toHaveValue("acme-canyon"); }, // the register link needs a public address; a laptop stack has none
    },
  ],
});
