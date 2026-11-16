import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Finding } from "../api";
import { ReviewGate } from "./ReviewGate";

type Log = { version: string; $schema: string; runs: { tool: { driver: { name: string } }; results: { locations: { physicalLocation: { region: { snippet: { text: string } } } }[] }[] }[] };
const golden = (file: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/finding", file), "utf8")) as Log;
// The rows the gate writes, one per producer (runs/pipeline.go), cut from the seam's goldens: the
// scanners' run of the claims-portal log, and the reviewer's run on a host with no model.
const claims = golden("claims-portal.sarif.json");
const row = (id: number, log: Log) => ({ id, kind: "producer.sarif", data: { producer: log.runs[0]!.tool.driver.name, sarif: { ...log, runs: log.runs.slice(0, 1) } } });
const scanners = row(60, claims);
const skipped = row(61, golden("skipped-review.sarif.json"));
const TIMELINE = "/api/v1/gate-runs/gr_8f2a/timeline";
// An imported tool's row (contracts/events.md): one per report, its category and normalised results, and no log.
const imported = (id: number, tool: string, category: string, results: unknown[]) => ({ id, kind: "producer.sarif", data: { producer: `sarif/${tool}`, tool, category, version: "1.0.0", digest: `sha256:${"ab".repeat(32)}`, attached_by: "ci-bot", dropped: 0, results } });

function server(pages: Record<string, unknown>) {
  const fetch = vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => pages[url] ?? { rows: [], next: null } }) as unknown as Response);
  vi.stubGlobal("fetch", fetch);
  return fetch;
}
const gate = () => within(screen.getByRole("list", { name: "Review gate" }));
// Each tool's line as read: its name, then what it said of its run.
const lines = () => gate().queryAllByRole("listitem").map((one) => [...one.querySelectorAll("p:first-child > span")].slice(1).map((span) => span.textContent).join(" "));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("what Canyon checked", () => {
  // No model is said once, on the AI review's row (cli.md §3.1's off row; #2013's review): never again under
  // it, and never as what nothing read — a rule nothing read because no model is set up is that row's to say.
  it("says once, on the AI review's row, that no AI model is set up", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const unread = { level: "warning", message: { text: "not checked: auth.unauthenticated-route: no model is configured on this Canyon host, so the AI review read nothing" },
      properties: { canyon: { policy_name: "Every route needs sign-in", why: "no model is configured on this Canyon host, so the AI review read nothing" } } };
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, invocations: [{ toolExecutionNotifications: [unread] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [{ id: 41, kind: "run.state", data: { state: "running" } }, ours, skipped], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["AI review skipped, no AI model set up"]); // never a pass: this one never ran; Canyon's own run is no tool
    expect(gate().queryByText(/no model configured/)).toBeNull();
    expect(screen.queryByRole("region", { name: "Not checked" })).toBeNull();
  });

  // Canyon's own run is no tool: what nothing read goes to its group, and it has no line of its own (the test cluster
  // audit: "Canyon's scanners 0 findings" beside every tool's count).
  it("gives Canyon's own run no line, and puts what nothing read in its group", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const said = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run,
      results: [{}], invocations: [{ toolExecutionNotifications: [{ message: { text: "not checked: deps.known-vuln: 2 unpinned requirements" } }] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [said], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual([]);
    expect(within(screen.getByRole("region", { name: "Not checked" })).getByText("not checked: deps.known-vuln: 2 unpinned requirements")).toBeDefined();
  });

  // A scan that did not finish wrote no report, only its run in the verdict's log: it is still on the list,
  // and says so — the test cluster's blocked Ruby app listed every tool but the two that didn't run.
  it("lists a tool whose scan didn't finish, from the verdict's log", async () => {
    const verdict = { id: 70, kind: "verdict", data: { state: "blocked", findings: { runs: [
      { tool: { driver: { name: "sarif/opengrep" } }, results: [], invocations: [{ toolExecutionNotifications: [{ level: "warning", message: { text: "opengrep · could not check: this Canyon host stayed too busy to run it" } }] }] },
    ] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [skipped, verdict], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["AI review skipped, no AI model set up", "opengrep couldn't finish"]);
  });

  // A scan that did not finish says so whatever report the package already had (contracts/finding.md,
  // Canyon-run tools): the earlier report's clean count is never this run's.
  it("says a tool couldn't finish even when an earlier report of it is on the run", async () => {
    const note = (level: string, text: string) => ({ level, message: { text } });
    const kept = note("warning", "trivy · not checked: go.sum was not read whole");
    const trivy = { id: 62, kind: "producer.sarif", data: { producer: "sarif/trivy", sarif: { runs: [{ results: [], invocations: [{ toolExecutionNotifications: [kept] }] }] } } };
    const verdict = { id: 70, kind: "verdict", data: { state: "clean", findings: { runs: [
      { tool: { driver: { name: "sarif/trivy" } }, results: [], invocations: [{ toolExecutionNotifications: [kept, note("warning", "trivy · could not check: it did not finish in time")] }] },
    ] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [trivy, verdict], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()[0]).toMatch(/^trivy couldn't finish/);
    expect(within(screen.getByRole("region", { name: "Not checked" })).getAllByRole("listitem")).toHaveLength(1); // the note both carry, once
  });

  // A tool that had no file to read says why, never "nothing found" (contracts/finding.md; #1742).
  it("says a tool had no file to read, in its own words", async () => {
    const said = (level: string, text: string) => ({ level, message: { text } });
    const eslint = { id: 63, kind: "producer.sarif", data: { producer: "sarif/eslint", sarif: { runs: [{ results: [], invocations: [{ toolExecutionNotifications: [said("note", "eslint · report attached by Canyon")] }] }] } } };
    const verdict = { id: 70, kind: "verdict", data: { state: "clean", findings: { runs: [
      { tool: { driver: { name: "sarif/eslint" } }, results: [], invocations: [{ toolExecutionNotifications: [said("note", "eslint · read no file: no JavaScript or TypeScript here"), said("note", "eslint · report attached by Canyon")] }] },
    ] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [eslint, verdict], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["eslint no JavaScript or TypeScript here"]);
    expect(gate().queryByText(/read no file/)).toBeNull(); // the why is the line's word, never repeated under it
  });

  it("says a tool had no file to read when it wrote no report", async () => {
    const verdict = { id: 70, kind: "verdict", data: { state: "clean", findings: { runs: [
      { tool: { driver: { name: "sarif/eslint" } }, results: [], invocations: [{ toolExecutionNotifications: [{ level: "warning", message: { text: "eslint · no report for this package" } }, { level: "note", message: { text: "eslint · read no file: no JavaScript or TypeScript here" } }] }] },
    ] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [verdict], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["eslint no JavaScript or TypeScript here"]); // the why outranks no report
    expect(gate().queryByText(/no report for this package/)).toBeNull(); // nor is no report said under it
  });

  // A line of what nothing read names its policy, as a finding's card does, and keeps its reason whole (#1668).
  it("names the policy of each line of what nothing read", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const line = { level: "warning", message: { text: "not checked: auth.unauthenticated-route: the AI review read 400 files and stopped there" },
      associatedRule: { id: "auth.unauthenticated-route" }, properties: { canyon: { policy_name: "Routes without sign-in", why: "the AI review read 400 files and stopped there" } } };
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, results: [], invocations: [{ toolExecutionNotifications: [line] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [ours], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    const item = within(await screen.findByRole("region", { name: "Not checked" })).getByRole("listitem");
    expect(item.textContent).toBe("Routes without sign-in: the AI review read 400 files and stopped there");
  });

  // What the AI review read where no scanner rule reads the language is said too, by policy (contracts/finding.md).
  it("names what the AI review checked in place of a scanner rule", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const line = { level: "note", message: { text: "checked by the AI review: sql.string-concat: Ruby (2 files) — no scanner rule reads Ruby" },
      associatedRule: { id: "sql.string-concat" }, properties: { canyon: { policy_name: "SQL built from strings", why: "Ruby (2 files) — no scanner rule reads Ruby" } } };
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, results: [], invocations: [{ toolExecutionNotifications: [line] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [ours], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    const group = within(await screen.findByRole("region", { name: "Checked by the AI review" }));
    expect(group.getByRole("listitem").textContent).toBe("SQL built from strings: Ruby (2 files) — no scanner rule reads Ruby");
    expect(screen.queryByRole("region", { name: "Not checked" })).toBeNull(); // read, so never not checked
  });

  // An app with no UI framework had no UI for the design-system rule to check (contracts/finding.md, design.org-system):
  // a note-level line, said by policy in its own group, never as what nothing read.
  it("says a rule that didn't apply in its own group, never as not checked", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const line = { level: "note", message: { text: "design.org-system: no package.json in this app lists a UI framework, so there was no UI to check" },
      associatedRule: { id: "design.org-system" }, properties: { canyon: { policy_name: "Apps use the org's design system", why: "no package.json in this app lists a UI framework, so there was no UI to check" } } };
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, results: [], invocations: [{ toolExecutionNotifications: [line] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [ours], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    const group = await screen.findByRole("region", { name: "Didn't apply" });
    expect(group.querySelector("p")?.textContent).toBe("Didn't apply · 1");
    expect(within(group).getByRole("listitem").textContent).toBe("Apps use the org's design system: no package.json in this app lists a UI framework, so there was no UI to check");
    expect(screen.queryByRole("region", { name: "Not checked" })).toBeNull();
  });

  it("puts what the AI review read before what nothing read, as the scanners' run orders them", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const say = (level: string, text: string) => ({ level, message: { text } });
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, results: [], invocations: [{ toolExecutionNotifications: [
      say("note", "checked by the AI review: sql.string-concat: Ruby (2 files) — no scanner rule reads Ruby"),
      say("warning", "not checked: deps.known-vuln: 1 unpinned requirement in requirements.txt — trivy reads == pins only"),
    ] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [ours], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("region", { name: "Not checked" });
    expect([...document.querySelectorAll("section[aria-label] section[aria-label]")].map((one) => one.getAttribute("aria-label"))).toEqual(["Checked by the AI review", "Not checked"]);
  });

  // What nothing read stands on its own, above the counts, never a grey line under a green one: every
  // producer's "not checked" note, in one amber block (the test cluster audit, case-not-checked).
  it("gathers every note of what nothing read into one block above the counts", async () => {
    const run = scanners.data.sarif.runs[0]!;
    const noted = (texts: string[]) => [{ invocations: [{ toolExecutionNotifications: texts.map((text) => ({ message: { text } })) }] }];
    const ours = { ...scanners, data: { ...scanners.data, sarif: { ...scanners.data.sarif, runs: [{ ...run, results: [], ...noted(["not checked: deps.known-vuln: 2 unpinned requirements in requirements.txt"])[0] }] } } };
    const eslint = { id: 63, kind: "producer.sarif", data: { producer: "sarif/eslint", sarif: { runs: [{ results: [], ...noted(["eslint · report attached by Canyon", "eslint · not checked: tools/report.ts was not read whole"])[0] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [ours, eslint], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    const block = within(await screen.findByRole("region", { name: "Not checked" }));
    expect(block.getByText("Not checked · 2")).toBeDefined();
    expect(block.getAllByRole("listitem").map((one) => one.textContent)).toEqual(["not checked: deps.known-vuln: 2 unpinned requirements in requirements.txt", "eslint · not checked: tools/report.ts was not read whole"]);
    expect(lines()).toEqual(["eslint nothing found · 1 not checked"]); // its line names what it missed
    expect(gate().getByText("eslint · report attached by Canyon")).toBeDefined();
    expect(gate().queryByText(/^eslint · not checked/)).toBeNull(); // the note itself is the block's, never repeated under the row
  });

  // A review whose check ended skipped read nothing, whatever its other parts found (contracts/finding.md):
  // its line takes the check's status, never green, and says under it what it could not read and why (#2294).
  it("never shows a skipped review green, and says under its line what it could not read", async () => {
    const say = (level: string, text: string) => ({ level, message: { text } });
    const split = { id: 62, kind: "producer.sarif", data: { producer: "canyon-review", sarif: { runs: [{ tool: { driver: { name: "canyon-review" } }, results: [], invocations: [{ toolExecutionNotifications: [
      say("note", "ai review · reviewed in 3 parts: the source is more than one model call takes"),
      say("warning", 'ai review · not checked: too large for one AI review call: "subset/woff2-wasm.ts"'),
    ] }] }] } } };
    const done = { id: 63, kind: "check.completed", data: { check: "canyon-review", status: "skipped", findings: 0, duration_ms: 0 } };
    server({ [`${TIMELINE}?after=0`]: { rows: [split, done], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["AI review skipped"]);
    const line = gate().getByRole("listitem");
    expect(line.querySelector(".text-ok")).toBeNull();
    // under its own row a line drops the row's name: "ai review · " would repeat it (#2302's review)
    expect(within(line).getByText('not checked: too large for one AI review call: "subset/woff2-wasm.ts"')).toBeDefined();
    expect(within(line).getByText("reviewed in 3 parts: the source is more than one model call takes")).toBeDefined();
    expect(within(line).queryByText(/^ai review · /)).toBeNull();
    expect(screen.queryByRole("region", { name: "Not checked" })).toBeNull(); // said once, under its line
  });

  // A check that read what it could keeps its unread lines in the group, whole: there they sit under no
  // row, so they keep the name of what said them.
  it("keeps a check's name on its lines in the group of what nothing read", async () => {
    const say = (level: string, text: string) => ({ level, message: { text } });
    const timed = { id: 62, kind: "producer.sarif", data: { producer: "canyon-review", sarif: { runs: [{ tool: { driver: { name: "canyon-review" } }, results: [], invocations: [{ toolExecutionNotifications: [
      say("note", "ai review · reused from an earlier review of the same code and policy"),
      say("warning", "ai review · not checked: tools/report.ts was not read whole"),
    ] }] }] } } };
    const done = { id: 63, kind: "check.completed", data: { check: "canyon-review", status: "warning", findings: 0, duration_ms: 10 } };
    server({ [`${TIMELINE}?after=0`]: { rows: [timed, done], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    const block = within(await screen.findByRole("region", { name: "Not checked" }));
    expect(block.getByText("ai review · not checked: tools/report.ts was not read whole")).toBeDefined();
    expect(gate().getByText("reused from an earlier review of the same code and policy")).toBeDefined();
  });

  // Never green while anything went unread: a tool with no check of its own counts what it missed, grey.
  it("never shows a tool green while it left something unread", async () => {
    const eslint = { id: 63, kind: "producer.sarif", data: { producer: "sarif/eslint", sarif: { runs: [{ results: [], invocations: [{ toolExecutionNotifications: [{ message: { text: "eslint · not checked: tools/report.ts was not read whole" } }] }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [eslint], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["eslint nothing found · 1 not checked"]);
    expect(gate().getByRole("listitem").querySelector(".text-ok")).toBeNull();
  });

  // A tool's line counts the entries it found, as the CLI's does (contracts/finding.md, One problem, one entry;
  // #2013): two of opengrep's rules on one line are one finding.
  it("counts a tool's findings as entries, two rules on one line one", async () => {
    const result = { ruleId: "sql.injection.opengrep.tainted-sql", locations: [{ physicalLocation: { artifactLocation: { uri: "server.js" }, region: { startLine: 10 } } }] };
    const verdict = { id: 70, kind: "verdict", data: { state: "blocked", findings: { runs: [{ tool: { driver: { name: "sarif/opengrep" } }, results: [result, { ...result, ruleId: "sql.injection.opengrep.express-sqli" }] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [verdict], next: null } });
    const read = [result, result].map((one) => ({ ...one, entry: 0 })) as unknown as Finding[];
    render(<ReviewGate run="gr_8f2a" findings={read} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["opengrep 1 finding"]);
  });

  it("never shows a finding's source, only how many there were", async () => {
    server({ [`${TIMELINE}?after=0`]: { rows: [scanners], next: null } });
    const { container } = render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    for (const result of claims.runs[0]!.results) expect(container.textContent).not.toContain(result.locations[0]!.physicalLocation.region.snippet.text);
  });

  it("reads a producer's words as text, never as markup", async () => {
    const said = { ...skipped, data: { ...skipped.data, sarif: { runs: [{ invocations: [{ toolExecutionNotifications: [{ message: { text: "<b>ai review</b>" } }] }], results: [] }] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [said], next: null } });
    const { container } = render(<ReviewGate run="gr_8f2a" findings={[]} />);
    expect(await screen.findByText("<b>ai review</b>")).toBeDefined();
    expect(container.querySelector("b")).toBeNull();
  });

  it("counts an imported tool's report from its row, which carries no log", async () => {
    server({ [`${TIMELINE}?after=0`]: { rows: [scanners, imported(62, "semgrep", "", golden("imported-semgrep.sarif.json").runs[0]!.results)], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["semgrep 1 finding"]);
  });

  // An imported tool's row is its attach's summary: what the run said about the tool, a file it could not
  // read among it, is in the verdict's log, and a warning there is never folded away (the test cluster: ESLint read
  // "0 findings" with tools/report.ts unread). Its note-level line, who attached it, stays off the screen.
  it("says what the run said about an imported tool, from the verdict's log", async () => {
    const say = (level: string, text: string) => ({ level, message: { text } });
    const verdict = { id: 70, kind: "verdict", data: { state: "clean", findings: { runs: [
      { tool: { driver: { name: "sarif/eslint" } }, results: [], invocations: [{ toolExecutionNotifications: [say("note", "eslint · report attached by Canyon"), say("warning", "eslint · not checked: tools/report.ts was not read whole")] }] },
    ] } } };
    server({ [`${TIMELINE}?after=0`]: { rows: [imported(62, "eslint", "", []), verdict], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    expect(await screen.findByText("eslint · not checked: tools/report.ts was not read whole")).toBeDefined();
    expect(screen.queryByText("eslint · report attached by Canyon")).toBeNull();
    // Its row ties its count to what it could not read, never a bare "0 findings" under the amber block.
    expect(lines()).toContain("eslint nothing found · 1 not checked");
  });

  it("gives each category of one tool its own row", async () => {
    const error = vi.spyOn(console, "error");
    server({ [`${TIMELINE}?after=0`]: { rows: [imported(62, "codeql", "javascript", [{}, {}]), imported(63, "codeql", "python", [{}])], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await screen.findByRole("list", { name: "Review gate" });
    expect(lines()).toEqual(["codeql (javascript) 2 findings", "codeql (python) 1 finding"]);
    expect(error).not.toHaveBeenCalled(); // two rows under one key are one row to React
  });

  it("follows the server's cursor to the last page", async () => {
    const fetch = server({ [`${TIMELINE}?after=0`]: { rows: [scanners], next: 60 }, [`${TIMELINE}?after=60`]: { rows: [skipped], next: null } });
    render(<ReviewGate run="gr_8f2a" findings={[]} />);
    await waitFor(() => expect(lines()).toEqual(["AI review skipped, no AI model set up"])); // the second page's row; the first's, Canyon's own, is no tool
    expect(fetch.mock.calls.map(([url]) => url)).toEqual([`${TIMELINE}?after=0`, `${TIMELINE}?after=60`]);
  });
});
