import { expect, test, type Locator, type Page } from "@playwright/test";
import { opensInside } from "../harness/menu";
import { answered, unbroken } from "./host";

// Every menu and dialog opens inside the window, or scrolls to its end inside itself (#126): on a phone, on a phone held
// sideways and on a laptop. A box off the left edge widens no page, so each is measured, never the page's scrollWidth.
const WINDOWS = [[360, 740], [390, 844], [390, 560], [667, 375], [1280, 900]] as const;

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
  "Hold production": async (page) => { await more(page, "endorsement-hub", /^Hold production/); return dialog(page); },
  "Rename": async (page) => { await more(page, "premium-ledger", /^Rename/); return page.locator("form, div").filter({ has: page.getByRole("textbox") }).filter({ has: page.getByRole("button", { name: "Cancel" }) }).last(); },
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

    test("Rename opens under its button on a scrolled page", async ({ page }) => {
      await page.goto("/#/apps/blocked");
      const opener = page.getByRole("button", { name: "More actions" });
      await expect(page.getByText(/apps\/web\/src\/app\/api\/admin\/policies\/route\.ts/).first()).toBeVisible();
      await page.evaluate(() => scrollTo(0, 60));
      expect(await page.evaluate(() => scrollY), "the page scrolls").toBe(60);
      await opener.click();
      await page.getByRole("menuitem", { name: /^Rename/ }).click();
      const [button, box] = [(await opener.boundingBox())!, (await page.getByRole("button", { name: "Cancel" }).locator("xpath=ancestor::*[contains(@class,'shadow-overlay')]").boundingBox())!];
      expect(box.y - button.y - button.height, "from its button down to it").toBeGreaterThanOrEqual(0);
      expect(box.y - button.y - button.height, "from its button down to it").toBeLessThanOrEqual(30);
    });

    for (const length of [30, 48]) {
      test(`an app's name of ${length} unbroken letters leaves the page its width, and its ⋯ on screen`, async ({ page }) => {
        await page.goto(`/#/apps/${unbroken(length)}`);
        const opener = page.getByRole("button", { name: "More actions" });
        await expect(opener).toBeVisible();
        expect(await page.evaluate(() => document.documentElement.scrollWidth), "the page's width").toBeLessThanOrEqual(width);
        const at = (await opener.boundingBox())!;
        expect(at.x, "the ⋯'s left edge").toBeGreaterThanOrEqual(0);
        expect(at.x + at.width, "the ⋯'s right edge").toBeLessThanOrEqual(width);
      });
    }
    // The fleet names every app in its list, in Needs you and in Recent activity, and on a host with sign-in off the first
    // blocked one in its Start here; the audit in its rows and its App filter.
    for (const [at, role, banner] of [["fleet", "admin", "none"], ["fleet", "admin", "exposed"], ["fleet", "responder", "none"], ["fleet", "member", "none"], ["audit", "admin", "none"]] as const) {
      test(`an app's name of 48 unbroken letters leaves the ${at} its width, as ${role}${banner === "none" ? "" : ", told where to start"}`, async ({ page }) => {
        await answered(page, role, banner);
        await page.goto(`/#/${at}`);
        await expect(page.getByText(unbroken(48)).first()).toBeAttached();
        if (at === "fleet") {
          await expect(page.getByRole("region", { name: "Recent activity" }).getByText(unbroken(48)).first()).toBeVisible();
          const needs = page.getByRole("region", { name: role === "admin" ? "Needs attention" : "Needs you" });
          await needs.getByRole("button", { name: /^Show \d+ more/ }).click();
          await expect(needs.getByText(unbroken(48)).first()).toBeVisible();
          await expect(page.getByRole("link", { name: `Open ${unbroken(48)} →` })).toHaveCount(banner === "none" ? 0 : 1);
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth), "the page's width").toBeLessThanOrEqual(width);
      });
    }

    test("a finding's long path leaves the page its width", async ({ page }) => {
      await page.goto("/#/apps/blocked");
      await expect(page.getByText(/apps\/web\/src\/app\/api\/admin\/policies\/route\.ts/).first()).toBeVisible();
      expect(await page.evaluate(() => document.documentElement.scrollWidth), "the page's width").toBeLessThanOrEqual(width);
    });

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
      // each repository's buttons, and the form one opens
      await page.getByRole("button", { name: "Add another folder" }).click();
      for (const one of await page.getByRole("button", { name: /^(Make it an app|Link to an app|Unlink billing|Add another folder|Make the app|Cancel)$/ }).all()) {
        const at = (await one.boundingBox())!;
        expect(at.x, `${await one.textContent()}: its left edge`).toBeGreaterThanOrEqual(0);
        expect(at.x + at.width, `${await one.textContent()}: its right edge, in a ${width}px window`).toBeLessThanOrEqual(width);
      }
      const last = (await page.getByText("Receiving events").boundingBox())!;
      expect(last.x + last.width, `the row's end, in a ${width}px window`).toBeLessThanOrEqual(width);
    });
  });
}

// Menus, at every width and on a scrolled page (harness/menu.ts): the dialogs above cover their buttons, so not they.
test.describe("a menu at every width", () => {
  test.use({ viewport: { width: 390, height: 844 } });
  test.setTimeout(180_000);
  test.beforeEach(async ({ page }) => { await answered(page); });
  test("the app's ⋯", async ({ page }) => {
    await page.goto("/#/apps/premium-ledger");
    await opensInside(page, page.getByRole("button", { name: "More actions" }), page.getByRole("menu").getByRole("menuitem"));
  });
  test("the audit's Export", async ({ page }) => {
    await page.goto("/#/audit");
    await opensInside(page, page.locator("summary", { hasText: "Export" }), page.locator("details[open]").filter({ hasText: "Export" }).getByRole("button"));
  });
  test("the fleet's Filter", async ({ page }) => {
    await page.goto("/#/fleet");
    await opensInside(page, page.getByRole("button", { name: /^Filter/ }), page.locator("#fleet-filter"));
  });
});

// Settings › Hosting at 320px, an admin's narrowest window or a laptop at 400%: the page is no wider than the window (#129).
test.describe("Settings › Hosting at 320px", () => {
  test.use({ viewport: { width: 320, height: 640 } });
  test.beforeEach(async ({ page }) => { await answered(page); });
  test("leaves the page its width", async ({ page }) => {
    await page.goto("/#/settings/hosting");
    await expect(page.getByText("Who signs people in", { exact: true })).toBeVisible(); // the legend, not the line above it that says the same words
    expect(await page.evaluate(() => document.documentElement.scrollWidth), "the page's width").toBeLessThanOrEqual(320);
  });
});
