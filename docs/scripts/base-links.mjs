// node scripts/base-links.mjs <site> <base>: puts <base> (such as /docs) in front of every root-relative link in
// <site>/src/content/docs, for a site served under that base (CANYON_DOCS_BASE in astro.config.mjs). Starlight
// writes a page's links as the page has them, and the .md copies and llms*.txt carry them verbatim, so the pages
// themselves must name the base. scripts/publish-docs.sh runs it on its copy, never on docs/: the pages here link
// from the root. It rewrites a Markdown link, image or definition, an href or src in a page's HTML, and a link: in
// its frontmatter (a hero action's), each found by a parser (micromark, parse5, yaml): code is never touched, nor
// an external, protocol-relative or relative link, nor one that already starts with the base.
import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { parse, postprocess, preprocess } from "micromark";
import { parseFragment } from "parse5";
import { isScalar, parseDocument, visit } from "yaml";

const [site, base] = process.argv.slice(2);
if (!site || !/^\/[a-z0-9-]+(\/[a-z0-9-]+)*$/.test(base ?? "")) {
  console.error("usage: node scripts/base-links.mjs <site directory> <base, such as /docs>");
  process.exit(1);
}
const rooted = (url) => url.startsWith("/") && !url.startsWith("//") && url !== base && !url.startsWith(`${base}/`);

// The offset in html of each root-relative href or src value.
function htmlLinks(html, offset) {
  const at = [];
  const walk = (node) => {
    for (const { name, value } of node.attrs ?? []) {
      if ((name !== "href" && name !== "src") || !rooted(value)) continue;
      const { startOffset, endOffset } = node.sourceCodeLocation.attrs[name];
      const attr = html.slice(startOffset, endOffset);
      const start = attr.indexOf(value, attr.indexOf("=") + 1);
      if (start < 0) throw new Error(`can't find ${name}="${value}" in ${attr}`);
      at.push(offset + startOffset + start);
    }
    for (const child of [...(node.childNodes ?? []), ...(node.content?.childNodes ?? [])]) walk(child);
  };
  walk(parseFragment(html, { sourceCodeLocationInfo: true }));
  return at;
}

// The offset in md of each root-relative link: Markdown destinations, and links in HTML, by micromark's tokens.
function markdownLinks(md, offset) {
  const at = [];
  const events = postprocess(parse().document().write(preprocess()(md, undefined, true)));
  for (const [kind, token] of events) {
    if (kind !== "enter") continue;
    const text = md.slice(token.start.offset, token.end.offset);
    if (token.type === "resourceDestinationString" || token.type === "definitionDestinationString") {
      if (rooted(text)) at.push(offset + token.start.offset);
    } else if (token.type === "htmlFlow" || token.type === "htmlText") {
      at.push(...htmlLinks(text, offset + token.start.offset));
    }
  }
  return at;
}

// The offset in yaml of each root-relative link: value.
function frontmatterLinks(yaml, offset) {
  const at = [];
  visit(parseDocument(yaml), {
    Pair(_, pair) {
      if (!isScalar(pair.key) || pair.key.value !== "link" || !isScalar(pair.value) || typeof pair.value.value !== "string") return;
      if (!rooted(pair.value.value)) return;
      const start = yaml.indexOf(pair.value.value, pair.value.range[0]);
      if (start < 0 || start >= pair.value.range[1]) throw new Error(`can't find link: ${pair.value.value} in the frontmatter`);
      at.push(offset + start);
    },
  });
  return at;
}

const pages = join(site, "src/content/docs");
let links = 0;
for (const file of readdirSync(pages, { recursive: true }).filter((f) => f.endsWith(".md"))) {
  const path = join(pages, file);
  const source = readFileSync(path, "utf8");
  const front = /^---\n([\s\S]*?\n)---\n/.exec(source);
  const body = front ? front[0].length : 0;
  const at = [...(front ? frontmatterLinks(front[1], 4) : []), ...markdownLinks(source.slice(body), body)];
  let out = source;
  for (const i of at.sort((a, b) => b - a)) out = out.slice(0, i) + base + out.slice(i);
  if (out !== source) writeFileSync(path, out);
  links += at.length;
}
console.log(`base-links: ${links} links now start with ${base}`);
