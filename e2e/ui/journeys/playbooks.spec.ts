import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";

// Guardrails › Playbooks (design/specs/mcp.md §3.2): an admin imports a SKILL.md, edits it, restores its first version
// and removes it. The stack runs CANYON_AUTH=demo, so the caller is an admin; a member's refusal is the cluster's A3.
// A fresh name per run, so a run stopped midway never leaves the next one a playbook that stands.
const letters = () => Array.from({ length: 6 }, () => String.fromCharCode(97 + Math.floor(Math.random() * 26))).join("");
const card = (page: Page, name: string) => page.getByRole("region", { name, exact: true });

journey("import a playbook, edit it, restore it, remove it", {
  given: async () => {
    const name = `Release checklist ${letters()}`;
    return { name, slug: name.toLowerCase().replace(/ /g, "-") };
  },
  steps: [
    {
      name: "open Guardrails › Playbooks",
      do: async (page) => { await page.goto("/#/guardrails/playbooks"); },
      see: async (page) => { await expect(page.getByRole("button", { name: "Import a SKILL.md" })).toBeVisible(); },
    },
    {
      name: "import a SKILL.md and save it: its first version",
      do: async (page, { name }) => {
        await page.getByRole("button", { name: "Import a SKILL.md" }).click();
        await page.getByLabel("Or paste it").fill(`---\nname: ${name}\ndescription: Read before you promote an app.\n---\n\n# Before you promote\n\n1. Check the migration runs twice.\n`);
        await page.getByRole("button", { name: "Read the SKILL.md" }).click();
        await page.getByRole("button", { name: "Save the new playbook" }).click();
      },
      see: async (page, { name, slug }) => { await expect(card(page, name)).toContainText(`${slug} · version 1`); },
      changes: { playbook_versions: { added: 1 }, audit_events: { added: 1, actions: ["set playbook"] } },
    },
    {
      name: "edit its body: its second version",
      do: async (page, { name }) => {
        await card(page, name).getByRole("button", { name: `Edit ${name}` }).click();
        await page.getByLabel("Body", { exact: true }).fill("# Before you promote\n\n1. Check the migration runs twice.\n2. Read the preview's logs.\n");
        await page.getByRole("button", { name: `Save ${name}` }).click();
      },
      see: async (page, { name, slug }) => { await expect(card(page, name)).toContainText(`${slug} · version 2`); },
      changes: { playbook_versions: { added: 1 }, audit_events: { added: 1, actions: ["set playbook"] } },
    },
    {
      name: "its history: the diff, then restore version 1 as version 3",
      do: async (page, { name }) => {
        await card(page, name).getByRole("button", { name: `History of ${name}` }).click();
        const history = page.getByRole("region", { name: `History of ${name}` });
        await expect(history.getByText("+ 2. Read the preview's logs.")).toBeVisible();
        await history.getByRole("button", { name: "Restore version 1" }).click();
      },
      see: async (page, { name, slug }) => { await expect(card(page, name)).toContainText(`${slug} · version 3`); },
      changes: { playbook_versions: { added: 1 }, audit_events: { added: 1, actions: ["restore playbook"] } },
    },
    {
      name: "remove it: its versions kept",
      do: async (page, { name }) => {
        await card(page, name).getByRole("button", { name: `Remove ${name}` }).click();
        await card(page, name).getByRole("button", { name: `Remove ${name}` }).click();
      },
      see: async (page, { name }) => { await expect(card(page, name)).toHaveCount(0); },
      changes: { playbook_removals: { added: 1 }, audit_events: { added: 1, actions: ["remove playbook"] } },
    },
  ],
});
