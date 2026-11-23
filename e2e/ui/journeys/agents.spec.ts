import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";

// The tab shown lies wholly inside its strip of tabs, which on a phone scrolls sideways: the last tab too (#100's review).
async function shownInStrip(page: Page, strip: string) {
  await expect.poll(() => page.evaluate((name) => {
    const nav = document.querySelector(`nav[aria-label="${name}"]`);
    const shown = nav?.querySelector("[aria-current=page]");
    if (!nav || !shown) return "no strip or no tab shown";
    const [n, t] = [nav.getBoundingClientRect(), shown.getBoundingClientRect()];
    return t.left >= n.left - 0.5 && t.right <= n.right + 0.5 ? "inside" : `${Math.round(t.left)}–${Math.round(t.right)}px in a strip of ${Math.round(n.left)}–${Math.round(n.right)}px`;
  }, strip)).toBe("inside");
}

// The two places a person meets agents (#99): what every agent is told, and how to connect one. Both read, and change
// nothing; the harness holds each to the page's layout rules at desktop and phone width.
journey("read what agents are told, then how to connect one", {
  steps: [
    {
      name: "open Guardrails › What agents are told",
      do: async (page) => { await page.goto("/#/guardrails/agents"); },
      see: async (page) => {
        await expect(page.getByRole("region", { name: "What agents are told" })).toBeVisible();
        await shownInStrip(page, "Guardrails"); // the last of Guardrails' tabs
      },
    },
    {
      name: "open Settings › Agents",
      do: async (page) => { await page.goto("/#/settings/agents"); },
      see: async (page) => { await expect(page.getByRole("region", { name: "Connect an agent" })).toBeVisible(); },
    },
    {
      name: "open Settings › Databases, the last of Settings' tabs",
      do: async (page) => { await page.goto("/#/settings/databases"); },
      see: async (page) => { await shownInStrip(page, "Settings"); },
    },
  ],
});
