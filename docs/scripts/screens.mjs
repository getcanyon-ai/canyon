// make docs-screens: the admin guides' dashboard screenshots, taken from the laptop demo
// (docker compose --profile demo-apps up, at http://localhost:7777) with the e2e/ui harness's
// Playwright, in light and dark, and written as WebP to docs/public/screens/. Re-run it when a screen
// the guides show changes.
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const { chromium } = createRequire(here("../../e2e/ui/package.json"))("playwright");
const sharp = createRequire(here("../package.json"))("sharp");
const base = process.env.CANYON_DEMO_URL ?? "http://localhost:7777";

// Each shot: the page, what to do before it, and which band of the page to keep. Every shot drops
// the rail (x < 224), and the fleet drops the demo's no-sign-in banner above the org's name.
const shots = [
  { name: "fleet", path: "/#/fleet", top: 80, height: 1000, before: openPanel },
  { name: "app-blocked", path: "/#/apps/claims-portal", top: 0, height: 1000 },
  { name: "guardrails-policies", path: "/#/guardrails/policies", top: 0, height: 900 },
  { name: "audit", path: "/#/audit", top: 0, height: 800 },
];

// The panel button scrolls the list sideways to show itself; scroll it back so the app names show.
async function openPanel(page) {
  await page.getByRole("button", { name: "Details for claims-portal" }).click();
  await page.evaluate(() => {
    document.activeElement.blur();
    for (const el of document.querySelectorAll("*")) el.scrollLeft = 0;
  });
}

const browser = await chromium.launch();
for (const scheme of ["light", "dark"]) {
  const page = await browser.newPage({ viewport: { width: 1280, height: 900 }, colorScheme: scheme, timezoneId: "UTC" });
  for (const shot of shots) {
    await page.goto(base + shot.path, { waitUntil: "networkidle" });
    if (shot.before) await shot.before(page);
    await page.waitForTimeout(500);
    const png = await page.screenshot({ clip: { x: 224, y: shot.top, width: 1280 - 224, height: shot.height }, fullPage: true });
    const out = here(`../public/screens/${shot.name}-${scheme}.webp`);
    await sharp(png).webp({ quality: 72 }).toFile(out);
    console.log(out);
  }
  await page.close();
}
await browser.close();
