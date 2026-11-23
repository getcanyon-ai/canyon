import { expect, type Locator, type Page } from "@playwright/test";

// An open menu's every item inside the window, and the page no wider than it. Measured with the menu open and against the
// window the project sets: the factory's own check reads each step's end, the menu closed, and on a phone a page a menu
// widens widens innerWidth with it.
export async function insideWindow(page: Page, items: Locator) {
  await expect(items.first()).toBeVisible();
  const { width, height } = page.viewportSize()!;
  const boxes = await items.evaluateAll((all) => all.map((one) => { const { left, right, top, bottom } = one.getBoundingClientRect(); return { item: one.textContent, left, right, top, bottom }; }));
  expect(boxes.filter((at) => at.left < 0 || at.right > width || at.top < 0 || at.bottom > height), `the menu's items outside the ${width}x${height} window`).toEqual([]);
  const scroll = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(scroll, `with the menu open the page is ${scroll}px wide in a ${width}px window: it scrolls sideways`).toBeLessThanOrEqual(width);
}
