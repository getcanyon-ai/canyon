import { expect, test, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { api } from "../harness/seed";

// Settings › Outbound's mode (#2422): an admin sends apps' calls through Canyon, switches the org to allowlist
// through the page's question, back to observe with none, and calls out directly again. The stack runs
// CANYON_AUTH=demo, so every caller is an admin; a member's read-only page is the cluster's A3 (e2e/cluster/CASES.md).
// Each change is one egress.set row, the setting's row and the policy's version (egress_policy, its trigger).
type Setting = { via: "off" | "canyon"; mode: "observe" | "allowlist" };

// The stack's setting as every other journey expects it: off, and observe. The mode first, which is taken only
// through Canyon (the body's via counts); both before the journey, and after it however it ended, so a failed
// run never leaves the next journey's apps routed through Canyon or refused.
async function putBack() {
  const was = await api<Setting>(null, "GET", "/api/v1/egress");
  if (was.mode !== "observe") await api(null, "PUT", "/api/v1/egress", { via: "canyon", mode: "observe" });
  if (was.via !== "off" || was.mode !== "observe") await api(null, "PUT", "/api/v1/egress", { via: "off" });
}
test.afterEach(putBack);
const set = { egress_settings: { changed: 1 }, egress_policy: { changed: 1 }, audit_events: { added: 1, actions: ["egress.set"] } };
const region = (page: Page) => page.getByRole("region", { name: "Outbound" });
const radio = (page: Page, name: string) => region(page).getByRole("radio", { name, exact: true });
const saved = "Saved. Running apps follow it within seconds.";
// A change answers once every gateway replica holds it (contracts/routes.md, A change waits for the gateway; #2789):
// up to the server's 30 s, so what a save shows is waited for that long, never Playwright's 5 s.
const taken = { timeout: 35_000 };

journey("set the org's outbound mode to allowlist and back", {
  given: async () => {
    await putBack(); // a run stopped midway may have left it changed
    return {};
  },
  steps: [
    {
      name: "open Settings › Outbound: the mode is off while calls don't go through Canyon",
      do: async (page) => { await page.goto("/#/settings/outbound"); },
      see: async (page) => {
        await expect(region(page).getByText("Turn on Through Canyon first: the mode applies only to calls that go through Canyon.")).toBeVisible();
        await expect(radio(page, "Observe")).toBeChecked();
        await expect(radio(page, "Observe")).toBeDisabled();
        await expect(radio(page, "Allowlist")).toBeDisabled();
      },
    },
    {
      name: "send apps' calls through Canyon",
      do: async (page) => {
        await radio(page, "Through Canyon").check();
        // It changes what apps can do, so it asks first (design/EXPERIENCE.md, D16).
        await page.getByRole("group", { name: "Send apps' calls through Canyon?" }).getByRole("button", { name: "Turn on Through Canyon" }).click();
      },
      see: async (page) => { await expect(radio(page, "Allowlist")).toBeEnabled(taken); },
      changes: set,
    },
    {
      name: "choose allowlist: the question with what changes, nothing sent",
      do: async (page) => {
        await radio(page, "Allowlist").check();
        const changes = page.getByRole("group", { name: /^(Refuse \d+ destinations? now\?|Switch to allowlist now\?)/ });
        await expect(changes.getByText("This applies to running apps within seconds, not at their next deploy.")).toBeVisible();
        await expect(changes.getByRole("button", { name: "Switch to allowlist" })).toBeEnabled(); // the list is read
      },
      see: async (page) => {
        const changes = page.getByRole("group", { name: /^(Refuse \d+ destinations? now\?|Switch to allowlist now\?)/ });
        await expect(changes.getByRole("button", { name: "Cancel" })).toBeVisible();
      },
    },
    {
      name: "switch to allowlist",
      do: async (page) => { await page.getByRole("group", { name: /^(Refuse \d+ destinations? now\?|Switch to allowlist now\?)/ }).getByRole("button", { name: "Switch to allowlist" }).click(); },
      see: async (page) => {
        await expect(region(page).getByText(saved)).toBeVisible(taken);
        await expect(radio(page, "Allowlist")).toBeChecked();
        expect((await api<Setting>(null, "GET", "/api/v1/egress")).mode).toBe("allowlist");
      },
      changes: set,
    },
    {
      name: "back to observe, after its question",
      do: async (page) => {
        await radio(page, "Observe").check();
        const ask = page.getByRole("group", { name: "Switch back to Observe?" });
        await expect(ask.getByText("Every app environment that follows the org's mode goes back to Observe.")).toBeVisible();
        await ask.getByRole("button", { name: "Switch to Observe" }).click();
      },
      see: async (page) => {
        await expect(region(page).getByText(saved)).toBeVisible(taken);
        expect((await api<Setting>(null, "GET", "/api/v1/egress")).mode).toBe("observe");
      },
      changes: set,
    },
    {
      name: "apps call out directly again",
      do: async (page) => {
        await radio(page, "Off").check();
        await page.getByRole("group", { name: "Let apps call out directly?" }).getByRole("button", { name: "Let apps call out directly" }).click();
      },
      see: async (page) => { await expect(radio(page, "Allowlist")).toBeDisabled(taken); },
      changes: set,
    },
  ],
});
