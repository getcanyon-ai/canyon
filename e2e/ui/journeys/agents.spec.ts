import { expect, type Page } from "@playwright/test";
import { journey } from "../harness/factory";

// The tab shown lies wholly inside its strip of tabs, which on a phone scrolls sideways (#100's review), and on a phone
// the strip runs to the screen's edges with the tab 16px in from them (#118): the first tab and the last.
async function shownInStrip(page: Page, strip: string) {
  await expect.poll(() => page.evaluate((name) => {
    const nav = document.querySelector(`nav[aria-label="${name}"]`);
    const shown = nav?.querySelector("[aria-current=page]");
    if (!nav || !shown) return "no strip or no tab shown";
    const [n, t] = [nav.getBoundingClientRect(), shown.getBoundingClientRect()];
    const phone = window.innerWidth < 860;
    const [left, right] = phone ? [16, window.innerWidth - 16] : [n.left, n.right];
    return t.left >= Math.max(n.left, left) - 0.5 && t.right <= Math.min(n.right, right) + 0.5 ? "inside" : `${Math.round(t.left)}–${Math.round(t.right)}px in a strip of ${Math.round(n.left)}–${Math.round(n.right)}px`;
  }, strip)).toBe("inside");
}

// Every rule's name on Pipeline reads in at most two lines (#118): at 1280 its gates squeezed names over four to six.
async function namesFit(page: Page) {
  await expect.poll(() => page.evaluate(() => {
    const names = [...document.querySelectorAll('section[aria-label="Pipeline"] section > span.grid > span:first-child')] as HTMLElement[];
    if (names.length === 0) return "no rule names";
    const tall = names.filter((one) => one.getBoundingClientRect().height > 2 * parseFloat(getComputedStyle(one).lineHeight) + 1);
    return tall.length === 0 ? "fits" : tall.map((one) => `${one.textContent} (${Math.round(one.getBoundingClientRect().height)}px)`).join(", ");
  })).toBe("fits");
}

// The two places a person meets agents (#99): what every agent is told, and how to connect one. Both read, and change
// nothing; the harness holds each to the page's layout rules at desktop and phone width.
journey("read what agents are told, then how to connect one", {
  steps: [
    {
      name: "open Guardrails, which opens on Policies, its first tab",
      do: async (page) => { await page.goto("/#/guardrails"); },
      see: async (page) => {
        await expect(page.getByRole("navigation", { name: "Guardrails" }).getByRole("link", { name: "Policies", exact: true })).toHaveAttribute("aria-current", "page");
        await shownInStrip(page, "Guardrails");
      },
    },
    {
      name: "open Guardrails › Pipeline",
      do: async (page) => { await page.goto("/#/guardrails/pipeline"); },
      see: async (page) => { await namesFit(page); },
    },
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
