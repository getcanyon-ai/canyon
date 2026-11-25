import { readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { createRequire } from "node:module";
import ts from "typescript";
import { __unstable__loadDesignSystem } from "tailwindcss";
import { describe, expect, it } from "vitest";

// Every class a className attribute names makes a rule in the dashboard's CSS (#142): Tailwind skips a class it can't
// build, so `font-normal`, with no `normal` among the design system's weights, left text semibold and failed nowhere.
// Read with TypeScript's parser: every string in a className attribute's value, a word at a `${…}` edge left out, as
// it is half a class. A class held in a constant elsewhere isn't read here.
export function classesIn(source: string, file: string): string[] {
  const found: string[] = [];
  const words = (text: string, openStart: boolean, openEnd: boolean) => {
    const parts = text.split(/\s+/);
    parts.forEach((word, i) => {
      if (word === "" || (i === 0 && openStart) || (i === parts.length - 1 && openEnd)) return;
      found.push(word);
    });
  };
  // A string the value can be, never one it is compared with: `state === "blocked" ? "text-red" : ""` names text-red.
  const inClass = (node: ts.Node): void => {
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) words(node.text, false, false);
    else if (ts.isTemplateExpression(node)) {
      words(node.head.text, false, true);
      node.templateSpans.forEach((span) => {
        inClass(span.expression);
        words(span.literal.text, true, ts.isTemplateMiddle(span.literal));
      });
    } else if (ts.isJsxExpression(node) || ts.isParenthesizedExpression(node)) { if (node.expression) inClass(node.expression); }
    else if (ts.isConditionalExpression(node)) { inClass(node.whenTrue); inClass(node.whenFalse); }
    else if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.AmpersandAmpersandToken) inClass(node.right);
    else if (ts.isBinaryExpression(node) && [ts.SyntaxKind.BarBarToken, ts.SyntaxKind.QuestionQuestionToken, ts.SyntaxKind.PlusToken].includes(node.operatorToken.kind)) { inClass(node.left); inClass(node.right); }
    else if (ts.isArrayLiteralExpression(node)) node.elements.forEach(inClass);
    else if (ts.isObjectLiteralExpression(node)) node.properties.forEach((p) => { if (ts.isPropertyAssignment(p)) inClass(p.initializer); });
    else if (ts.isElementAccessExpression(node)) inClass(node.expression);
  };
  const visit = (node: ts.Node) => {
    if (ts.isJsxAttribute(node) && node.name.getText() === "className" && node.initializer) inClass(node.initializer);
    else ts.forEachChild(node, visit);
  };
  visit(ts.createSourceFile(file, source, ts.ScriptTarget.Latest, true, file.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS));
  return found;
}

const src = import.meta.dirname + "/";
const require = createRequire(join(src, "classes.test.ts"));

// The design system app.css builds, its imports read from disk as Vite's plugin reads them.
async function designSystem() {
  const css = readFileSync(join(src, "app.css"), "utf8");
  return __unstable__loadDesignSystem(css, {
    base: src,
    loadStylesheet: async (id: string, base: string) => {
      const path = id === "tailwindcss" ? require.resolve("tailwindcss/index.css") : resolve(base, id);
      return { path, base: dirname(path), content: readFileSync(path, "utf8") };
    },
  });
}

const files = (dir: string): string[] => readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
  entry.isDirectory() ? files(join(dir, entry.name)) : entry.name.endsWith(".tsx") && !entry.name.endsWith(".test.tsx") ? [join(dir, entry.name)] : []);

describe("classes", () => {
  it("are read from className alone, never half of one", () => {
    expect(classesIn('const a = <p className={`${H2} inline text-${tone} mt-1`} title="font-bold">x</p>;', "a.tsx")).toEqual(["inline", "mt-1"]);
    expect(classesIn('const a = <p className={on ? "font-regular" : "font-semibold"}>x</p>;', "a.tsx")).toEqual(["font-regular", "font-semibold"]);
  });

  // `group` alone makes no rule, by design: it is the mark a group-hover: class reads, and makes rules there.
  it("each make a rule in the built CSS", async () => {
    const ds = await designSystem();
    const named = new Set<string>();
    for (const file of files(src)) for (const one of classesIn(readFileSync(file, "utf8"), file)) if (one !== "group") named.add(`${file.slice(src.length)}: ${one}`);
    const all = [...named];
    const css = ds.candidatesToCss(all.map((pair) => pair.slice(pair.indexOf(": ") + 2)));
    expect(all.filter((_, i) => css[i] === null)).toEqual([]);
  });
});
