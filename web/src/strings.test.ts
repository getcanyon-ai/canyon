// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import ts from "typescript";
import { describe, expect, it } from "vitest";
import { S } from "./strings";

const experience = readFileSync(new URL("../../design/EXPERIENCE.md", import.meta.url), "utf8");

describe("the strings registry", () => {
  for (const [key, value] of Object.entries(S)) {
    it(`${key} appears verbatim in design/EXPERIENCE.md`, () => {
      expect(experience).toContain(value);
    });
  }
});

// Every text the dashboard's code holds, as the compiler parses it: strings, template parts and JSX text, never a
// comment. Tests are left out: they may name the old separator to prove it gone.
const texts = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
  const path = join(dir, entry.name);
  if (entry.isDirectory()) return texts(path);
  if (!/\.tsx?$/.test(entry.name) || /\.test\.tsx?$/.test(entry.name)) return [];
  const found: string[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isStringLiteralLike(node) || ts.isTemplateHead(node) || ts.isTemplateMiddle(node) || ts.isTemplateTail(node) || ts.isJsxText(node)) found.push(`${path}: ${node.text}`);
    ts.forEachChild(node, visit);
  };
  visit(ts.createSourceFile(path, readFileSync(path, "utf8"), ts.ScriptTarget.Latest, true, path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS));
  return found;
});

// A settings path has one separator, the menu's: Settings › Outbound, never Settings → Outbound (#2848).
describe("a settings path", () => {
  it("is written Settings › X in every text the dashboard shows", () => {
    expect(texts(new URL(".", import.meta.url).pathname).filter((text) => text.includes("Settings →"))).toEqual([]);
  });
});
