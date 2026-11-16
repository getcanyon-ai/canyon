import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { api } from "../harness/seed";

const rule = "No hand-built SQL from user input";
// The rule's one control is a segmented choice: its words are labels over visually hidden radios.
const dial = (page: Page) => page.getByRole("radiogroup", { name: rule, exact: true }); // exact: its AI review twin is named "… (AI review)"
const pick = (page: Page, word: string) => dial(page).getByText(word, { exact: true }).click();
const state = (page: Page, word: string) => dial(page).getByRole("radio", { name: word });
const bar = (page: Page) => page.getByRole("region", { name: "Unsaved changes" });

journey("change a policy, then change it back", {
  given: async () => {
    await api(null, "PUT", "/api/v1/policies/sql.string-concat?apply=true", { state: "blocking" }); // a run stopped midway may have left it advisory
    return {};
  },
  steps: [
    {
      name: "open the guardrails",
      do: async (page) => { await page.goto("/#/guardrails/policies"); },
      see: async (page) => { await expect(state(page, "Blocking")).toBeChecked(); },
    },
    {
      name: "pick advisory: a preview in the save bar, nothing written",
      do: async (page) => { await pick(page, "Advisory"); },
      see: async (page) => {
        await expect(bar(page).getByText(`${rule}, Blocking → Advisory`)).toBeVisible();
        await expect(bar(page).getByText("Apps with these findings will be able to reach production.")).toBeVisible();
      },
    },
    {
      name: "discard: still nothing written",
      do: async (page) => { await bar(page).getByRole("button", { name: "Discard" }).click(); },
      see: async (page) => {
        await expect(bar(page)).toBeHidden();
        await expect(state(page, "Blocking")).toBeChecked();
      },
    },
    {
      name: "set it to advisory",
      do: async (page) => {
        await pick(page, "Advisory");
        await bar(page).getByRole("button", { name: "Save", exact: true }).click();
      },
      see: async (page) => {
        await expect(bar(page)).toBeHidden();
        await expect(state(page, "Advisory")).toBeChecked();
      },
      changes: { policy_edits: { added: 1 }, audit_events: { added: 1, actions: ["policy.set"] } },
    },
    {
      name: "set it back to blocking, now",
      do: async (page) => {
        await pick(page, "Blocking");
        await expect(bar(page).getByRole("button", { name: "Save, tightening in 14 days" })).toBeVisible();
        await bar(page).getByRole("button", { name: "Save", exact: true }).click();
      },
      see: async (page) => {
        await expect(bar(page)).toBeHidden();
        await expect(state(page, "Blocking")).toBeChecked();
      },
      changes: { policy_edits: { added: 1 }, audit_events: { added: 1, actions: ["policy.set"] } },
    },
  ],
});
