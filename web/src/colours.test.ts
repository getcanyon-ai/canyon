// @vitest-environment node
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import ts from "typescript";
import { describe, expect, it } from "vitest";

// A colour literal is a hex colour or an rgb()/rgba() call; every colour in a component is a token.
const LITERAL = /#[0-9a-fA-F]{3,8}\b|\brgba?\(/g;

// colourLiterals is every colour literal a source states, comments never read: in TypeScript only what
// the compiler parses as text — strings, template parts and JSX text — and in CSS everything outside
// its comments. A "#1645" naming an issue in a comment is no colour; one in a className is.
export function colourLiterals(source: string, file: string): string[] {
  if (file.endsWith(".css")) return source.replace(/\/\*[\s\S]*?\*\//g, "").match(LITERAL) ?? [];
  const found: string[] = [];
  const visit = (node: ts.Node) => {
    if (ts.isStringLiteralLike(node) || ts.isTemplateHead(node) || ts.isTemplateMiddle(node) || ts.isTemplateTail(node) || ts.isJsxText(node)) found.push(...(node.text.match(LITERAL) ?? []));
    ts.forEachChild(node, visit);
  };
  visit(ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS));
  return found;
}

const files = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
  entry.isDirectory() ? files(join(dir, entry.name)) : /\.(tsx?|css)$/.test(entry.name) && entry.name !== "tokens.css" && entry.name !== "colours.test.ts" ? [join(dir, entry.name)] : []);

describe("colour literals", () => {
  it("are read in code, never in comments", () => {
    expect(colourLiterals("// fixed in #1645\n/* see #1645 */\nconst a = 1;", "a.ts")).toEqual([]);
    expect(colourLiterals('const c = "#1645";\nconst d = <p className="text-[#abcdef]">x</p>;', "a.tsx")).toEqual(["#1645", "#abcdef"]);
    expect(colourLiterals("/* #1645 */ a { color: rgba(0,0,0,1); }", "a.css")).toEqual(["rgba("]);
  });

  it("appear in no component: every colour is a token", () => {
    const src = new URL(".", import.meta.url).pathname;
    expect(files(src).flatMap((file) => colourLiterals(readFileSync(file, "utf8"), file).map((one) => `${file.slice(src.length)}: ${one}`))).toEqual([]);
  });
});
