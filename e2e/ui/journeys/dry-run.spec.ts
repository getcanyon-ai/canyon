import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, review, uploadVersion } from "../harness/seed";
import { psql } from "../harness/surfaces";

// A rule's slug is letters and "-" alone: a fresh one per run, from letters.
const letters = () => Array.from({ length: 6 }, () => String.fromCharCode(97 + Math.floor(Math.random() * 26))).join("");

journey("dry run a custom rule checked by code on the apps", {
  given: async () => {
    const marker = `DRY-RUN-${letters().toUpperCase()}.md`;
    const [has, lacks] = [appId("ux-dry-has"), appId("ux-dry-lacks")];
    for (const [app, files] of [[has, { [marker]: "here\n" }], [lacks, {}]] as const) {
      await api(null, "POST", "/api/v1/projects", { id: app });
      await uploadVersion(app, "f-clean", files);
      await review(app);
    }
    // A rule checked by code as a save with a model on the host writes one; this stack has none (facts version 3).
    const slug = `dry-run-${letters()}`;
    const name = `Has ${marker}`;
    psql(`INSERT INTO custom_rule_edits (rule, version, name, instructions, check_by, check_expr, check_says, check_facts_version, set_by_kind, set_by_subject, set_by_display)
      VALUES ('custom.${slug}', 1, '${name}', 'Every app has ${marker} at its root.', 'code', '''${marker}'' in files', 'The app has ${marker} at its root.', 3, 'person', 'anonymous', '')`);
    return { has, lacks, name };
  },
  steps: [
    {
      name: "open the custom rules",
      do: async (page) => { await page.goto("/#/guardrails/policies"); },
      see: async (page, { name }) => { await expect(page.getByRole("region", { name })).toContainText("Checked by code: The app has"); },
    },
    {
      name: "try it on the apps: one would pass, one would fail, nothing written",
      do: async (page, { name }) => { await page.getByRole("button", { name: `Try it on your apps: ${name}` }).click(); },
      see: async (page, { has, lacks, name }) => {
        const list = page.getByRole("list", { name: `${name} on your apps` });
        await expect(list.getByText(`${has}: Would pass`)).toBeVisible({ timeout: 60_000 });
        await expect(list.getByText(`${lacks}: Would fail`)).toBeVisible();
      },
    },
  ],
});
