import { expect, test, type Locator, type Page } from "@playwright/test";
import { answered } from "./host";

// Every menu and dialog opens inside the window, or scrolls to its end inside itself (#126): on a phone, on a phone held
// sideways and on a laptop. A box off the left edge widens no page, so each is measured, never the page's scrollWidth.
const WINDOWS = [[390, 844], [390, 560], [667, 375], [1280, 900]] as const;

// The box on screen, and each of the controls in it reachable: on screen once the box has scrolled to it.
async function opensOnScreen(page: Page, box: Locator) {
  await expect(box).toBeVisible();
  const { width, height } = page.viewportSize()!;
  const at = await box.evaluate((el) => {
    const { left, right, top, bottom } = el.getBoundingClientRect();
    const own = getComputedStyle(el);
    const over = (el.scrollHeight > el.clientHeight && /auto|scroll/.test(own.overflowY));
    let fixed = false;
    for (let up: Element | null = el; up !== null; up = up.parentElement) fixed ||= getComputedStyle(up).position === "fixed";
    const room = document.documentElement.scrollHeight - document.documentElement.scrollTop; // what the page itself can scroll to
    return { left, right, top, bottom, over, fixed, room };
  });
  expect(at.left, `its left edge, in a ${width}x${height} window`).toBeGreaterThanOrEqual(0);
  expect(at.right, `its right edge, in a ${width}x${height} window`).toBeLessThanOrEqual(width);
  expect(at.top, `its top edge, in a ${width}x${height} window`).toBeGreaterThanOrEqual(0);
  expect(at.bottom, `its bottom edge, in a ${width}x${height} window`).toBeLessThanOrEqual(at.fixed ? height : Math.max(height, at.room));
  for (const control of await box.locator("button, select, input, textarea").all()) {
    if (!await control.isVisible()) continue;
    await control.scrollIntoViewIfNeeded();
    await expect(control, `a control of the box, in a ${width}x${height} window`).toBeInViewport({ ratio: 1 });
  }
}

const dialog = (page: Page) => page.getByRole("dialog");
const more = async (page: Page, app: string, item: RegExp) => {
  await page.goto(`/#/apps/${app}`);
  await page.getByRole("button", { name: "More actions" }).click();
  await page.getByRole("menuitem", { name: item }).click();
};

const OPEN: Record<string, (page: Page) => Promise<Locator>> = {
  "Stop production": async (page) => { await more(page, "premium-ledger", /^Stop production/); return dialog(page); },
  "Take production offline": async (page) => { await more(page, "premium-ledger", /^Take production offline/); return dialog(page); },
  "Delete app": async (page) => { await more(page, "premium-ledger", /^Delete/); await expect(dialog(page).getByRole("textbox")).toBeVisible(); return dialog(page); },
  "Lift the hold": async (page) => { await page.goto("/#/apps/held"); await page.getByRole("button", { name: /^Lift the hold/ }).first().click(); return dialog(page); },
  "the fleet's Filter": async (page) => { await page.goto("/#/fleet"); await page.getByRole("button", { name: /^Filter/ }).click(); return page.locator("#fleet-filter"); },
  "the hand-off's one-time values": async (page) => {
    await page.goto("/#/settings/hosting");
    await page.getByRole("button", { name: "Replace the host token" }).first().click();
    await page.getByRole("button", { name: "Replace the host token" }).last().click();
    return dialog(page);
  },
};

for (const [width, height] of WINDOWS) {
  test.describe(`${width}x${height}`, () => {
    test.use({ viewport: { width, height } });
    test.beforeEach(async ({ page }) => { await answered(page); });
    for (const [name, open] of Object.entries(OPEN)) {
      test(`${name} opens on screen`, async ({ page }) => { await opensOnScreen(page, await open(page)); });
    }

    // The tab shown is whole in its strip once the fonts, wider than what stood in for them, are in.
    test("the tab shown is whole", async ({ page }) => {
      // the fonts late, as on a first load: the strip is drawn, and its tab brought in, before they come
      await page.route(/\.woff2?(\?|$)/, async (route) => { await new Promise((done) => setTimeout(done, 700)); await route.continue(); });
      await page.goto("/#/settings/databases");
      const strip = page.getByRole("navigation", { name: "Settings" });
      await expect(strip.getByRole("link", { name: "Databases" })).toBeVisible();
      await page.evaluate(async () => { await document.fonts.ready; await new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done))); });
      const at = await strip.evaluate((nav) => {
        const words = document.createRange();
        words.selectNodeContents(nav.querySelector("[aria-current=page]")!);
        const [text, box] = [words.getBoundingClientRect(), nav.getBoundingClientRect()];
        return { left: text.left - box.left, right: box.right - text.right };
      });
      expect(at.left, "room left of the tab's text in its strip").toBeGreaterThanOrEqual(0);
      expect(at.right, "room right of the tab's text in its strip").toBeGreaterThanOrEqual(0);
    });

    // What acts on a grant or an organization is on screen where its row is: never past the edge of a table that scrolls.
    test("who can open an app: its selects and Remove are on screen", async ({ page }) => {
      await page.goto("/#/apps/claims-portal");
      const card = page.getByRole("region", { name: "Access" });
      await card.locator("summary").click();
      await expect(card.getByRole("button", { name: /^Remove / }).first()).toBeVisible();
      for (const one of [...await card.getByRole("combobox", { name: /^Access for / }).all(), ...await card.getByRole("button", { name: /^Remove / }).all()]) {
        const box = (await one.boundingBox())!;
        expect(box.x, "its left edge").toBeGreaterThanOrEqual(0);
        expect(box.x + box.width, `its right edge, in a ${width}px window`).toBeLessThanOrEqual(width);
      }
    });
    test("a connected GitHub organization: what it does with new repositories is on screen", async ({ page }) => {
      await page.goto("/#/settings/github");
      const box = (await page.getByRole("combobox", { name: /^New repositories in / }).boundingBox())!;
      expect(box.x, "its left edge").toBeGreaterThanOrEqual(0);
      expect(box.x + box.width, `its right edge, in a ${width}px window`).toBeLessThanOrEqual(width);
      const last = (await page.getByText("Receiving events").boundingBox())!;
      expect(last.x + last.width, `the row's end, in a ${width}px window`).toBeLessThanOrEqual(width);
    });
  });
}
