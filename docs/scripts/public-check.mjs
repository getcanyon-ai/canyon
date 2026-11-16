// npm run build, last: the built site is public, so it must not point back at the people, customers or
// internals behind Canyon. Fails on a real name, a customer's, an issue or pull request number, a path
// into the private repository, or an internal address, in any page, .md copy, llms*.txt or schema in dist/.
// _astro/ and pagefind/ are skipped: they are the npm packages' bundles (one credits a translator by name),
// and every word of the site's own in them is in a page too.
import { createHash } from "node:crypto";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

// Our people's and customers' names, by the first 16 hex digits of the sha256 of the word in lower case, from
// CANYON_PUBLIC_CHECK_NAMES (comma-separated): a CI secret, never this file, since a dictionary reverses a short hash
// of one word and the repository is public. Unset on CI or in a publishing build fails closed; unset on a
// contributor's machine, or in a fork's or Dependabot's pull request, which GitHub hands no secret (CANYON_PUBLIC_CHECK_FORK=true, the
// docs workflow's), the names alone are not checked, and it says so: the published site's build checks them.
const listed = (process.env.CANYON_PUBLIC_CHECK_NAMES ?? "").split(",").map((h) => h.trim()).filter(Boolean);
const fork = process.env.CANYON_PUBLIC_CHECK_FORK === "true" && !process.env.VERCEL;
if (listed.length === 0 && !fork && (process.env.CI || process.env.VERCEL)) {
  console.error("public-check: CANYON_PUBLIC_CHECK_NAMES is empty here, so the site can't be checked for names: set it from the secret");
  process.exit(1);
}
if (listed.length === 0) console.warn("public-check: CANYON_PUBLIC_CHECK_NAMES is not set, so names are not checked; the published site's build checks them");
if (listed.some((h) => !/^[0-9a-f]{16}$/.test(h))) {
  console.error("public-check: CANYON_PUBLIC_CHECK_NAMES holds something that is not 16 hex digits");
  process.exit(1);
}
const names = new Set(listed);
const hashed = new Map();
const named = (word) => {
  const w = word.toLowerCase();
  if (!hashed.has(w)) hashed.set(w, names.has(createHash("sha256").update(w).digest("hex").slice(0, 16)));
  return hashed.get(w);
};
const banned = [
  /\bthe bed\b/i,
  /(^|[^\w-])design\//m,
  /workers\.dev/i,
  // what a reader can't open: Canyon's source, tests and fixtures, a make target, an unpublished contract or a design
  /(?<![\w/.-])(cmd|gate|runs|executor|provisioners|db|contracts(?!\/schema\/(finding|release)\.schema\.json\b)|designsystem|e2e|server|web\/src)\/|(?<![\w-]|\/\/[^\s"'<>()`]*)testdata\/|\.\.\/(cmd|gate|runs|executor|provisioners|db|contracts(?!\/schema\/(finding|release)\.schema\.json\b))\/|\b[\w-]+\.go\b(?!\.\w)|\bcli\.md §|`make [\w-]+|\b(routes|executor|events|snapshot)\.md\b|\bseam [2-5]\b|\bthe (permissions|egress|hand-off|operations|experience|patching) design\b|\bthe CLI spec\b|\b(EXPERIENCE|ARCHITECTURE|PATCHING|EGRESS|PERMISSIONS|OPERATIONS|HAND-OFF)\.md\b|\bEXPERIENCE D\d|\[Test\w+/i,
];
// An issue or pull request number, anywhere but after another word, a link's ](, or a CSS property's colon.
// Read in the .md copies and text files only: a page's HTML carries colours, and its text is in its .md copy.
const issue = /(?<![\w&#/=.:-]|\]\(|:\s)#\d{3,}(?![\w-])/;
const dist = fileURLToPath(new URL("../dist/", import.meta.url));
const found = [];
for (const file of readdirSync(dist, { recursive: true })) {
  if (file.startsWith("_astro") || file.startsWith("pagefind") || !/\.(html|md|txt|json|xml|svg)$/.test(file)) continue;
  readFileSync(join(dist, file), "utf8").split("\n").forEach((line, i) => {
    for (const re of file.endsWith(".html") ? banned : [...banned, issue]) if (re.test(line)) found.push(`dist/${file}:${i + 1}: ${line.match(re)[0].trim()}`);
    for (const word of line.match(/[a-z]+/gi) ?? []) if (named(word)) found.push(`dist/${file}:${i + 1}: ${word}`);
  });
}
if (found.length) {
  console.error(`public-check: the built site names what a public reader must not see:\n${found.join("\n")}`);
  process.exit(1);
}
