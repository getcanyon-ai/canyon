import { parse } from "yaml";
import { S } from "../strings";

// skillOf reads a SKILL.md as a coding agent's skill is written (design/specs/mcp.md §3.2): YAML frontmatter between
// two --- lines, read by a YAML parser, its name and description each one string; the rest is the body. It answers the
// three, or why it can't, in D5's words. Windows line endings become newlines, as the server takes a body.
export function skillOf(raw: string): { name: string; description: string; body: string } | string {
  const text = raw.replace(/\r\n?/g, "\n");
  const lines = text.split("\n");
  const end = lines.indexOf("---", 1);
  if (lines[0] !== "---" || end < 0) return S.playbookImportNoFrontmatter;
  let front: unknown;
  try {
    front = parse(lines.slice(1, end).join("\n"));
  } catch {
    return S.playbookImportNoFrontmatter;
  }
  const { name, description } = (front ?? {}) as { name?: unknown; description?: unknown };
  if (typeof name !== "string" || name.trim() === "" || typeof description !== "string" || description.trim() === "") return S.playbookImportNoName;
  return { name: name.trim(), description: description.trim().replace(/\s*\n\s*/g, " "), body: lines.slice(end + 1).join("\n").replace(/^\n+/, "") };
}
