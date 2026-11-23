import { expect, type Locator, type Page } from "@playwright/test";

// A menu opened at every width from 320 to 1280, on the project's height and on a phone held sideways, the page scrolled
// a little where it scrolls: every item inside the window's left and right edges, the page no wider than the window, the
// menu under its button, which stays where it was, and its last item brought on screen by scrolling. Swept, as a toolbar wraps where its words
// put it and takes the menu to the other edge there; a page's width says nothing of its left edge.
export async function opensInside(page: Page, opener: Locator, items: Locator) {
  const own = page.viewportSize()!;
  // where the button is on the page, not in the window: a click scrolls a button half out of sight into it
  const place = () => opener.evaluate((one) => { const { left, top, width, height } = one.getBoundingClientRect(); return { left: Math.round(left + scrollX), top: Math.round(top + scrollY), width: Math.round(width), height: Math.round(height) }; });
  const wrong: string[] = [];
  for (const height of [own.height, 375]) {
    for (const width of [390, 667, 859, ...Array.from({ length: 49 }, (_, i) => 320 + 20 * i)]) {
      await page.setViewportSize({ width, height });
      await page.evaluate(() => scrollTo(0, 60));
      const closed = JSON.stringify(await place());
      await opener.click();
      await expect(items.first()).toBeVisible();
      const button = (await opener.boundingBox())!;
      const boxes = await items.evaluateAll((all) => all.map((one) => { const { left, right, top } = one.getBoundingClientRect(); return { left, right, top }; }));
      const left = Math.min(...boxes.map((box) => box.left)), right = Math.max(...boxes.map((box) => box.right));
      const at = `${width}x${height}: `;
      const opened = JSON.stringify(await place());
      if (opened !== closed) wrong.push(`${at}its button moved from ${closed} to ${opened}`);
      if (left < 0 || right > width) wrong.push(`${at}its items run from ${left} to ${right}`);
      const scroll = await page.evaluate(() => document.documentElement.scrollWidth);
      if (scroll > width) wrong.push(`${at}the page is ${scroll}px wide`);
      const under = boxes[0]!.top - (button.y + button.height);
      if (under < 0 || under > 30) wrong.push(`${at}its first item is ${under}px under its button`);
      await items.last().scrollIntoViewIfNeeded();
      const last = (await items.last().boundingBox())!;
      if (last.y < 0 || last.y + last.height > height) wrong.push(`${at}its last item, at ${last.y} to ${last.y + last.height}, can't be reached`);
      await opener.click();
    }
  }
  await page.setViewportSize(own);
  await page.evaluate(() => scrollTo(0, 0));
  expect(wrong, "a menu opened outside its window").toEqual([]);
}
