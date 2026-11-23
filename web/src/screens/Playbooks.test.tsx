import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Playbook, PlaybookEntry, PlaybookText, PlaybookVersion } from "../api";
import { S } from "../strings";
import { Markdown } from "../ui/Markdown";
import { lineDiff } from "./lineDiff";
import { Playbooks, playbookSlugOf } from "./Playbooks";
import { skillOf } from "./skillFile";

// Every playbook, version and body here is the contract's sample (contracts/testdata/guardrails/preferences.json).
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as {
  playbooks: { playbooks: PlaybookEntry[] };
  playbook: { playbook: Playbook };
  playbook_put: { slug: string; request: PlaybookText; response: { playbook: Playbook } };
  playbook_restore: { slug: string; request: { version: number }; response: { playbook: Playbook } };
  playbook_versions: { slug: string; removed: boolean; versions: PlaybookVersion[] };
};
const [notes, service] = golden.playbooks.playbooks as [PlaybookEntry, PlaybookEntry];
const AT = "/api/v1/playbooks";

type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const { status = 200, body } = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const routes: Record<string, Answer> = {
  [`GET ${AT}`]: { body: golden.playbooks },
  [`GET ${AT}/${service.slug}`]: { body: golden.playbook },
  [`GET ${AT}/${service.slug}/versions`]: { body: golden.playbook_versions },
};
const card = async (name: string) => within(await screen.findByRole("region", { name }));

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("Guardrails › Playbooks", () => {
  it("lists each playbook: its name, kind, when to read it, slug, version and who changed it last", async () => {
    server(routes);
    render(<Playbooks />);
    const one = await card(service.name);
    expect(one.getByText("Skill")).toBeDefined();
    expect(one.getByText(service.when)).toBeDefined();
    expect(one.getByText(`${service.slug} · version 3 · Dana Kim · 19 Nov`)).toBeDefined();
    expect((await card(notes.name)).getByText("Note")).toBeDefined();
  });

  // Two counts, neither naming anyone (design/specs/mcp.md §3.5): reads on every card, machines on a skill's alone, and
  // one line saying what machines can't see.
  it("shows each playbook's reads in 30 days, and a skill's machines", async () => {
    server(routes);
    render(<Playbooks />);
    expect((await card(service.name)).getByText(`${service.reads} reads in 30 days · on ${service.machines} machines`)).toBeDefined();
    expect((await card(notes.name)).getByText(`${notes.reads} reads in 30 days`)).toBeDefined();
    expect(screen.getByText(/Canyon can't see an agent reading its local copy of a skill/)).toBeDefined();
  });

  // The tab says what a playbook is and what its two kinds are before anything about its counts (#118): the line on what
  // machines can't see sits under the list, beside the counts it explains.
  it("opens with what a playbook is and its two kinds, and explains the counts under the list", async () => {
    server(routes);
    render(<Playbooks />);
    const last = await card(service.name);
    const order = (text: string | RegExp) => screen.getByText(text);
    const item = (text: string) => screen.getByText((_, one) => one?.tagName === "LI" && one.textContent === text);
    const kinds = [item(`${S.playbookSkill}: ${S.playbookSkillMeans}`), item(`${S.playbookNote}: ${S.playbookNoteMeans}`)];
    const intro = order(S.playbooksIntro);
    const caveat = order(/Canyon can't see an agent reading its local copy of a skill/);
    const follows = (a: Element, b: Element) => (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;
    expect(follows(intro, kinds[0]!) && follows(kinds[0]!, kinds[1]!)).toBe(true);
    expect(follows(kinds[1]!, screen.getByRole("region", { name: notes.name }))).toBe(true);
    expect(last.getByText(service.name) && follows(screen.getByRole("region", { name: service.name }), caveat)).toBe(true);
  });

  it("says when the org has none", async () => {
    server({ [`GET ${AT}`]: { body: { playbooks: [] } } });
    render(<Playbooks />);
    expect(await screen.findByText("No playbooks yet.")).toBeDefined();
  });

  it("edits a playbook: reads its body, previews it, and saves the next version whole", async () => {
    const { request } = golden.playbook_put;
    const sent = server({ ...routes, [`PUT ${AT}/${service.slug}`]: { body: golden.playbook_put.response } });
    render(<Playbooks />);
    fireEvent.click((await card(service.name)).getByRole("button", { name: `Edit ${service.name}` }));
    const body = await screen.findByLabelText("Body");
    expect((body as HTMLTextAreaElement).value).toBe(golden.playbook.playbook.body);
    fireEvent.click(screen.getByRole("tab", { name: "Preview" }));
    expect(within(screen.getByLabelText("Preview")).getByRole("heading", { name: "Writing a Meridian service" })).toBeDefined();
    fireEvent.click(screen.getByRole("tab", { name: "Write" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: request.name } });
    fireEvent.change(screen.getByLabelText("Body"), { target: { value: request.body } });
    fireEvent.click(screen.getByRole("button", { name: `Save ${request.name}` }));
    await waitFor(() => expect(sent()).toContain(`PUT ${AT}/${service.slug} ${JSON.stringify({ name: request.name, kind: request.kind, when: request.when, body: request.body })}`));
  });

  it("says what the server refuses in a save, in its words", async () => {
    const message = "A playbook may carry no secret: agents read it, and a model reads what they read.";
    server({ ...routes, [`PUT ${AT}/${service.slug}`]: { status: 400, body: { error: "invalid_playbook", message } } });
    render(<Playbooks />);
    fireEvent.click((await card(service.name)).getByRole("button", { name: `Edit ${service.name}` }));
    fireEvent.click(await screen.findByRole("button", { name: `Save ${service.name}` }));
    expect(await screen.findByText(message)).toBeDefined();
  });

  it("writes a new playbook under the slug its name makes, and refuses one that stands", async () => {
    const sent = server({ ...routes, [`PUT ${AT}/release-checklist`]: { body: golden.playbook } });
    render(<Playbooks />);
    fireEvent.click(await screen.findByRole("button", { name: "New playbook" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Incident notes" } });
    fireEvent.click(screen.getByRole("button", { name: "Save the new playbook" }));
    expect(await screen.findByText("A playbook with this slug already exists: edit that one instead.")).toBeDefined();
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Release checklist" } });
    fireEvent.click(screen.getByRole("radio", { name: "Note" }));
    fireEvent.change(screen.getByLabelText("When to read it"), { target: { value: "Read before you promote." } });
    fireEvent.change(screen.getByLabelText("Body"), { target: { value: "1. Check the migration.\n" } });
    expect(screen.getByText("17 / 80 · its slug: release-checklist")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Save the new playbook" }));
    await waitFor(() => expect(sent()).toContain(`PUT ${AT}/release-checklist ${JSON.stringify({ name: "Release checklist", kind: "note", when: "Read before you promote.", body: "1. Check the migration.\n" })}`));
  });

  it("imports a SKILL.md into the editor, unsaved: its name, its description as when, and the rest as the body", async () => {
    const sent = server(routes);
    render(<Playbooks />);
    fireEvent.click(await screen.findByRole("button", { name: "Import a SKILL.md" }));
    fireEvent.change(screen.getByLabelText("Or paste it"), { target: { value: "---\nname: Release checklist\ndescription: >\n  Read before you\n  promote.\n---\n\n# Steps\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Read the SKILL.md" }));
    const editor = await card("New playbook");
    expect((editor.getByLabelText("Name") as HTMLInputElement).value).toBe("Release checklist");
    expect((editor.getByLabelText("Slug") as HTMLInputElement).value).toBe("release-checklist");
    expect((editor.getByLabelText("When to read it") as HTMLInputElement).value).toBe("Read before you promote.");
    expect((editor.getByLabelText("Body") as HTMLTextAreaElement).value).toBe("# Steps\n");
    expect((editor.getByRole("radio", { name: "Skill" }) as HTMLInputElement).checked).toBe(true);
    expect(sent().filter((one) => one.startsWith("PUT"))).toHaveLength(0);
  });

  it("refuses a file that isn't a SKILL.md, in D5's words", async () => {
    server(routes);
    render(<Playbooks />);
    fireEvent.click(await screen.findByRole("button", { name: "Import a SKILL.md" }));
    fireEvent.change(screen.getByLabelText("Or paste it"), { target: { value: "# Just Markdown\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Read the SKILL.md" }));
    expect(await screen.findByText("This isn't a SKILL.md: it needs a frontmatter block between two --- lines.")).toBeDefined();
  });

  it("shows every version, a diff between any two, and restores an older one as the next", async () => {
    const sent = server({ ...routes, [`POST ${AT}/${service.slug}/restore`]: { body: golden.playbook_restore.response } });
    render(<Playbooks />);
    fireEvent.click((await card(service.name)).getByRole("button", { name: `History of ${service.name}` }));
    const history = await card(`History of ${service.name}`);
    expect(await history.findByText("Version 3 · Dana Kim · 19 Nov · what agents read")).toBeDefined();
    const diff = history.getByLabelText("Version 2 to version 3");
    expect(within(diff).getByText("name: Meridian services → How Meridian writes a service")).toBeDefined();
    expect(within(diff).getByText("- Every query passes its input as a bound parameter.")).toBeDefined();
    expect(within(diff).getByText("  # Writing a Meridian service", { normalizer: (text) => text })).toBeDefined();
    fireEvent.change(history.getByLabelText("Compare version"), { target: { value: "1" } });
    expect(history.getByText("The body is the same.")).toBeDefined(); // 1 and 3: the restore's own text
    expect(history.queryByRole("button", { name: "Restore version 3" })).toBeNull(); // what agents read already
    fireEvent.click(history.getByRole("button", { name: "Restore version 1" }));
    await waitFor(() => expect(sent()).toContain(`POST ${AT}/${service.slug}/restore ${JSON.stringify(golden.playbook_restore.request)}`));
  });

  it("asks before it removes a playbook, and says its versions are kept", async () => {
    const sent = server({ ...routes, [`DELETE ${AT}/${notes.slug}`]: { status: 204, body: null } });
    render(<Playbooks />);
    fireEvent.click((await card(notes.name)).getByRole("button", { name: `Remove ${notes.name}` }));
    expect(screen.getByText(`Remove ${notes.name}? Agents stop being handed it. Its versions are kept.`)).toBeDefined();
    expect(sent().filter((one) => one.startsWith("DELETE"))).toHaveLength(0);
    fireEvent.click(screen.getAllByRole("button", { name: `Remove ${notes.name}` })[0]!);
    await waitFor(() => expect(sent()).toContain(`DELETE ${AT}/${notes.slug}`));
  });
});

describe("the parts", () => {
  // The body is an admin's text shown in an admin's browser: never markup on the page, never a fetch, never a script link.
  it("previews Markdown as text: raw HTML is its own text, an image its alt, a javascript: link no link", () => {
    const { container } = render(<Markdown text={"# Title\n\n<img src=x onerror=alert(1)>\n\n![alt words](https://example.com/a.png) [bad](javascript:alert(1)) [good](https://example.com)\n"} />);
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
    expect(container.textContent).toContain("alt words");
    expect([...container.querySelectorAll("a")].map((one) => one.getAttribute("href"))).toEqual(["https://example.com"]);
  });

  // Nothing an admin writes reaches the DOM as something that runs: no script, no element with a handler, no fetched
  // image, and a link only to an http(s) address. The rest is the body's own text.
  it("renders a body with a script, a handler, a javascript: link and a data: image inert", () => {
    const body = "<script>alert(1)</script>\n\nText <img src=x onerror=alert(2)> inline <b onclick=alert(3)>b</b>\n\n" +
      "[run](javascript:alert(4)) [vb](vbscript:msgbox) [rel](/admin) ![pixel](data:image/png;base64,AAAA) [ok](https://example.com)\n";
    const { container } = render(<Markdown text={body} />);
    expect(container.querySelectorAll("script, img, iframe, object, embed, b")).toHaveLength(0);
    expect([...container.querySelectorAll("*")].flatMap((one) => one.getAttributeNames()).filter((name) => name.startsWith("on"))).toEqual([]);
    expect([...container.querySelectorAll("a")].map((one) => [one.getAttribute("href"), one.getAttribute("rel")])).toEqual([["https://example.com", "noopener noreferrer"]]);
    for (const text of ["<script>alert(1)</script>", "<img src=x onerror=alert(2)>", "<b onclick=alert(3)>", "[run](javascript:alert(4))", "[rel](/admin)", "pixel"]) expect(container.textContent).toContain(text);
  });

  it("reads a SKILL.md's frontmatter with a YAML parser, and refuses one without a name or a description", () => {
    expect(skillOf("---\r\nname: 'A: b'\r\ndescription: c\r\n---\r\nbody\r\n")).toEqual({ name: "A: b", description: "c", body: "body\n" });
    expect(skillOf("---\nname: a\n---\nbody")).toBe("The SKILL.md's frontmatter needs a name and a description.");
    expect(skillOf("---\nname: [\n---\n")).toBe("This isn't a SKILL.md: it needs a frontmatter block between two --- lines.");
  });

  it("diffs two bodies line by line", () => {
    expect(lineDiff("a\nb\nc\n", "a\nc\nd\n")).toEqual([{ op: " ", line: "a" }, { op: "-", line: "b" }, { op: " ", line: "c" }, { op: "+", line: "d" }]);
    expect(lineDiff("", "x")).toEqual([{ op: "+", line: "x" }]);
  });

  it("makes a slug of a-z, 0-9 and hyphens, a letter first, at most 40", () => {
    expect(playbookSlugOf("How Meridian writes a service")).toBe("how-meridian-writes-a-service");
    expect(playbookSlugOf("2026 incidents")).toBe("incidents");
    expect(playbookSlugOf("Секреты")).toBe("");
    expect(playbookSlugOf("a".repeat(39) + " b c")).toBe("a".repeat(39));
  });
});
