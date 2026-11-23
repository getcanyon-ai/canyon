// node scripts/diagram-hints.mjs, after npm run build: every mermaid diagram in dist/ says "Scroll sideways to see all
// of it." exactly when its frame scrolls sideways by more than 4px, on a phone (390px) and on a desktop (1280px), and
// no page scrolls sideways. It serves dist/ itself and drives the e2e/ui harness's Chromium, as scripts/screens.mjs
// does, so it needs `npm ci` and `npx playwright install chromium` in e2e/ui first.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { createServer } from "node:http";
import { createRequire } from "node:module";
import { extname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const { chromium } = createRequire(here("../../e2e/ui/package.json"))("playwright");
const dist = here("../dist/");

const types = { ".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".webp": "image/webp", ".png": "image/png", ".json": "application/json" };
const server = createServer((req, res) => {
  let path = join(dist, decodeURIComponent(new URL(req.url, "http://x").pathname));
  if (!path.startsWith(dist)) return res.writeHead(403).end();
  try {
    if (statSync(path).isDirectory()) path = join(path, "index.html");
    res.writeHead(200, { "content-type": types[extname(path)] ?? "application/octet-stream" }).end(readFileSync(path));
  } catch {
    res.writeHead(404).end();
  }
});
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;

const pages = readdirSync(dist, { recursive: true })
  .filter((file) => file.endsWith("index.html") && readFileSync(join(dist, file), "utf8").includes('class="diagram-frame"'))
  .map((file) => `/${file.slice(0, -"index.html".length)}`);
if (pages.length === 0) throw new Error("diagram-hints: no page in dist/ has a mermaid diagram: run npm run build first");

const browser = await chromium.launch();
const wrong = [];
let checked = 0;
for (const width of [390, 1280]) {
  const page = await browser.newPage({ viewport: { width, height: 900 } });
  for (const path of pages) {
    await page.goto(origin + path);
    await page.evaluate(() => document.fonts.ready);
    await page.waitForFunction(() => [...document.querySelectorAll(".diagram-frame")].every((frame) => frame.parentElement.matches(".scrolls") === frame.scrollWidth > frame.clientWidth + 4), null, { timeout: 2000 }).catch(() => {});
    const found = await page.evaluate(() => ({
      pageScrolls: document.documentElement.scrollWidth > document.documentElement.clientWidth,
      diagrams: [...document.querySelectorAll("figure.diagram")].map((figure, i) => {
        const frame = figure.querySelector(".diagram-frame");
        const scrolls = frame.scrollWidth - frame.clientWidth;
        const hint = figure.querySelector(".diagram-hint");
        return { i, scrolls, hint: hint.checkVisibility() && hint.getBoundingClientRect().height > 0 };
      }),
    }));
    if (found.pageScrolls) wrong.push(`${path} at ${width}px: the page scrolls sideways`);
    for (const { i, scrolls, hint } of found.diagrams) {
      checked++;
      if ((scrolls > 4) !== hint) wrong.push(`${path} at ${width}px, diagram #${i}: its frame scrolls ${scrolls}px, and the hint is ${hint ? "shown" : "hidden"}`);
    }
  }
  await page.close();
}
await browser.close();
server.close();
if (wrong.length) {
  console.error(`diagram-hints: the hint is wrong on ${wrong.length} of ${checked}:\n${wrong.join("\n")}`);
  process.exit(1);
}
console.log(`diagram-hints: ${checked} diagrams on ${pages.length} pages, at 390px and 1280px: each says it scrolls exactly when it does`);
