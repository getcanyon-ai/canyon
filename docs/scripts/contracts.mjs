// npm run dev and npm run build: the published contracts as pages of the site. Each contracts/<name>.md (links to
// the repository's contracts/) becomes src/content/docs/reference/contracts/<name>.md, its first line's
// heading as the page's title, and contracts/schema/'s links are served at /contracts/schema/. Both outputs are
// generated, never committed (.gitignore).
import { cpSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const pages = here("../src/content/docs/reference/contracts/");
rmSync(pages, { recursive: true, force: true });
mkdirSync(pages);
for (const file of readdirSync(here("../contracts/")).filter((f) => f.endsWith(".md"))) {
  const [first, ...rest] = readFileSync(here(`../contracts/${file}`), "utf8").split("\n");
  if (!first.startsWith("# ")) throw new Error(`contracts/${file}: the first line is not a "# " heading`);
  writeFileSync(`${pages}${file}`, `---\ntitle: ${JSON.stringify(first.slice(2))}\n---\n${rest.join("\n")}`);
}
rmSync(here("../public/contracts/"), { recursive: true, force: true });
cpSync(here("../contracts/schema/"), here("../public/contracts/schema/"), { recursive: true, dereference: true });
