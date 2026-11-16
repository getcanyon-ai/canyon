import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Finding as FindingRead } from "../api";
import type { RunRead } from "../api";
import { Finding, forAgent } from "./Finding";
import { verdict } from "./Verdict";

const golden = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/finding/claims-portal.sarif.json"), "utf8")) as { runs: [{ results: [] }, { results: [FindingRead, FindingRead, FindingRead] }, { results: [FindingRead] }] }).runs;
const [advisory] = golden[1].results; // the reviewer's routes and refetch; gitleaks' key is golden[2]
// a blocking finding of Canyon's own: the golden's first route finding, under a snapshot where its rule blocks
const blocking: FindingRead = { ...advisory, entry: 0, properties: { canyon: { ...advisory.properties.canyon, blocks: true } } };
const [imported] = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/finding/imported-semgrep.sarif.json"), "utf8")) as { runs: [{ results: [FindingRead] }] }).runs[0].results;
// The code the imported finding's region hides, read from the fixture app it points into (line 8, columns 51–84).
const context = "const onlyTheToolSaysThis = 1;"; // what a tool's own context region might carry
const clipboard = (writeText: (text: string) => Promise<void>) => Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
const copy = () => fireEvent.click(screen.getByRole("button", { name: "Copy for your agent" }));

afterEach(() => {
  cleanup();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
});

describe("Finding", () => {
  // A finding an earlier review made, carried while its code is unchanged (contracts/finding.md, A blocking finding an
  // earlier review made stays while its code stands): a tag, the line under its problem, and the agent's block says
  // what to do, as canyon check --ai does. A finding not carried says none of it.
  it("says a carried finding was found in an earlier review", () => {
    const carried: FindingRead = { ...blocking, properties: { canyon: { ...blocking.properties.canyon, carried_from: "gr_7d3e8f2a91c04b56" } } };
    render(<Finding finding={carried} app="claims-portal" />);
    expect(screen.getByText("Found in an earlier review")).toBeDefined();
    expect(screen.getByText("Canyon's AI review found this in an earlier review. This review didn't mention it, but the code hasn't changed, so it still blocks until the code changes or someone approves it.")).toBeDefined();
    expect(forAgent(carried, "claims-portal")).toContain("earlier review:\n  This was found in an earlier review and this review didn't report it.\n" +
      "  The code hasn't changed, so it still blocks: change the code so the rule\n  holds, or ask for an approval. Don't wait for another review to drop it.\n");
    cleanup();
    render(<Finding finding={blocking} app="claims-portal" />);
    expect(screen.queryByText("Found in an earlier review")).toBeNull();
    expect(forAgent(blocking, "claims-portal")).not.toContain("earlier review");
  });

  // A finding no Canyon policy covers keeps the tool's rule under sarif.<tool>: the block an agent gets reads
  // its title, then the tool's own rule under found by, never sarif.semgrep.… as if it were Canyon's (QA round 5).
  it("copies a finding no policy covers by its title and the tool's rule", () => {
    expect(forAgent(imported, "claims-portal").split("\n").slice(2, 4)).toEqual([
      "MEDIUM User input from the q search parameter reaches db.claims.search unvalidated",
      "found by: semgrep's rule typescript.nextjs.unvalidated-search-param"]);
  });

  // A tool's finding a policy covers reads as --ai's head reads it: the Canyon rule, then the tool's rule under
  // found by, never secrets.client-bundle.gitleaks.… (QA round 7); Canyon's own finding is its rule.
  it("copies a tool's finding by its Canyon rule and the tool's rule", () => {
    const key = { ...golden[2].results[0], tool: "sarif/gitleaks" };
    expect(forAgent(key, "claims-portal").split("\n").slice(2, 5)).toEqual([
      "CRITICAL secrets.client-bundle", "found by: gitleaks's rule stripe-access-token", "blocks: yes"]);
    expect(forAgent({ ...advisory, tool: "canyon-review" }, "claims-portal").split("\n")[2]).toBe("HIGH auth.unauthenticated-route");
  });

  // A check that couldn't finish is a short row, never a code finding: no severity, no file and line, no code
  // block; why, fix, and its digest behind Details. It still blocks, and a person can still approve it.
  it("draws a check that couldn't finish as a short row", () => {
    const unread: FindingRead = { ...blocking, ruleId: "gate.unreadable", properties: { canyon: { ...blocking.properties.canyon, severity: "high", title: "Canyon could not check this package with trivy",
      problem: "trivy did not finish in time.", fix: "Review it again." } },
      locations: [{ physicalLocation: { artifactLocation: { uri: "Gemfile" }, region: { startLine: 1, startColumn: 1, snippet: { text: "tree sha256:f832eb" } } } }] };
    const { container } = render(<Finding finding={unread} app="orders-api" approve={{ org: "Meridian", send: async () => {} }} />);
    expect(screen.getByText("couldn't finish")).toBeDefined();
    expect(screen.queryByText("High")).toBeNull();
    expect(container.querySelector("pre")).toBeNull();
    expect(screen.getByText("trivy did not finish in time.")).toBeDefined();
    expect(screen.getByText("Details").closest("details")?.textContent).toContain("Gemfile · tree sha256:f832eb");
    expect(screen.getByRole("button", { name: "Approve with a reason" })).toBeDefined();
  });

  it("names an imported finding's tool and rule, and shows where it is and the code it flags, never the tool's own context", () => {
    // A tool's own context region is never read; Canyon shows the snippet it read from the package (contracts/finding.md).
    const { physicalLocation } = imported.locations[0];
    const carrying = { ...imported, locations: [{ physicalLocation: { ...physicalLocation, contextRegion: { snippet: { text: context } } } }] } as unknown as FindingRead;
    const { container } = render(<Finding finding={carrying} app="claims-portal" />);
    expect(screen.getByText("Found by semgrep · rule typescript.nextjs.unvalidated-search-param")).toBeDefined();
    expect(screen.getByText("apps/web/src/app/api/claims/route.ts:8")).toBeDefined();
    expect(container.querySelector("pre")?.textContent).toContain('await db.claims.search(req.nextUrl.searchParams.get("q") ?? "")');
    expect(container.textContent).not.toContain(context);
  });

  // A card names what found it, from the run read's tool, in the words the review list uses (the test cluster
  // audit: an AI finding couldn't be told from a scanner's on the card itself).
  it("says which tool found it", () => {
    for (const [tool, said] of [["canyon-review", "Found by AI review"], ["sarif/opengrep", "Found by opengrep"]] as const) {
      render(<Finding finding={{ ...blocking, tool }} app="claims-portal" />);
      expect(screen.getByText(said)).toBeDefined();
      cleanup();
    }
    render(<Finding finding={blocking} app="claims-portal" />);
    expect(screen.queryByText(/^Found by/)).toBeNull(); // a finding read from a log itself names no tool
  });

  // Why it matters and what to do, each under its own word, as the approved card reads.
  it("labels the problem Why and the fix Fix", () => {
    const { container } = render(<Finding finding={blocking} app="claims-portal" />);
    const terms = [...container.querySelectorAll("dt")].map((one) => one.textContent);
    expect(terms).toEqual(["Why", "Fix"]);
    expect(container.querySelectorAll("dd")[0]!.textContent).toBe(blocking.properties.canyon.problem);
    expect(container.querySelectorAll("dd")[1]!.textContent).toBe(blocking.properties.canyon.fix);
  });

  it("names no tool on a finding of Canyon's own", () => {
    const { container } = render(<Finding finding={blocking} app="claims-portal" />);
    expect(container.textContent).not.toContain(" · rule ");
  });

  it("copies the paste-ready block, with the end state and the wrong moves a blocking finding carries", async () => {
    const writeText = vi.fn(async () => {});
    clipboard(writeText);
    render(<Finding finding={blocking} app="claims-portal" />);
    copy();
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Copied");
    expect(writeText).toHaveBeenCalledWith(
      `Fix this finding from Canyon's review of claims-portal:

HIGH auth.unauthenticated-route
blocks: yes
apps/web/src/app/api/admin/policies/route.ts:8 — ${blocking.properties.canyon.problem}

${blocking.properties.canyon.fix}

required end state:
  /api/admin/policies answers only a request that carries a session: the middleware matcher covers it, or the handler returns before anything else when the session cookie is missing.

do not:
  - check a query parameter or header the caller sets instead of the session cookie
  - move the handler's logic to another route outside the matcher
  - add an ignore comment or suppression pragma
  Each of these produces the same finding on the next run.

Then run: canyon check
This runs the identical gate without deploying. It takes about 15 seconds.
If it prints RESULT: PASSED, run: canyon deploy

If canyon check still prints RESULT: BLOCKED after two full rounds of fixes,
stop and show the user the remaining findings. Do not keep looping.
`);
  });

  it("copies an advisory finding as one that does not block, with no wrong moves to avoid", async () => {
    const writeText = vi.fn(async () => {});
    clipboard(writeText);
    render(<Finding finding={advisory} app="claims-portal" />);
    copy();
    await screen.findByRole("status");
    expect(writeText).toHaveBeenCalledWith(
      `Fix this finding from Canyon's review of claims-portal:

HIGH auth.unauthenticated-route
blocks: no
apps/web/src/app/api/admin/policies/route.ts:8 — ${advisory.properties.canyon.problem}

${advisory.properties.canyon.fix}

required end state:
  Not required. This severity does not block deployment.

Then run: canyon check
This runs the identical gate without deploying. It takes about 15 seconds.
If it prints RESULT: PASSED, run: canyon deploy

If canyon check still prints RESULT: BLOCKED after two full rounds of fixes,
stop and show the user the remaining findings. Do not keep looping.
`);
  });

  it("says the copy failed when the browser refuses, and never that it copied", async () => {
    clipboard(() => Promise.reject(new Error("denied")));
    render(<Finding finding={blocking} app="claims-portal" />);
    copy();
    expect((await screen.findByRole("alert")).textContent).toBe("Couldn't copy. The browser did not allow it.");
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("says the copy failed on a host with no clipboard at all", async () => {
    render(<Finding finding={blocking} app="claims-portal" />);
    copy();
    expect((await screen.findByRole("alert")).textContent).toBe("Couldn't copy. The browser did not allow it.");
  });

  // The one way to approve is on the finding's own card, and it approves that finding alone, with an
  // expiry that starts 30 days out (design/specs/screens.md, The approval).
  it("approves a blocking finding from its own card, with 30 days filled in", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-25T12:00:00Z"));
    const sent: unknown[] = [];
    render(<Finding finding={blocking} app="claims-portal" approve={{ org: "Meridian", send: async (body) => { sent.push(body); } }} />);
    fireEvent.click(screen.getByRole("button", { name: "Approve with a reason" }));
    expect((screen.getByLabelText("Approval expires") as HTMLInputElement).value).toBe("2026-10-25");
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "  a test key  " } });
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await vi.waitFor(() => expect(sent).toEqual([{ reason: "a test key", expires_at: "2026-10-25T00:00:00Z" }]));
    vi.useRealTimers();
  });

  it("offers no approval on an advisory finding, an approved one, or a card shown without it", () => {
    const approved: FindingRead = { ...blocking, approval: { reason: "r", expires_at: "2026-10-07T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } } };
    for (const [one, approve] of [[advisory, true], [approved, true], [blocking, false]] as const) {
      render(<Finding finding={one} app="claims-portal" approve={approve ? { org: "Meridian", send: async () => {} } : undefined} />);
      expect(screen.queryByRole("button", { name: "Approve with a reason" })).toBeNull();
      cleanup();
    }
  });

  // An entry of several findings carries the highest severity among them, as the CLI's does (cli.md §3.3; #2013).
  it("an entry's card and its copy carry the highest severity of its findings", async () => {
    const writeText = vi.fn(async () => {});
    clipboard(writeText);
    const medium = { ...blocking, properties: { canyon: { ...blocking.properties.canyon, severity: "medium" as const } } };
    render(<Finding finding={medium} also={[{ ...blocking, properties: { canyon: { ...blocking.properties.canyon, severity: "critical" as const } } }]} app="claims-portal" />);
    expect(screen.getByText("Critical")).toBeDefined();
    copy();
    await screen.findByRole("status");
    expect(String((writeText.mock.calls[0] as unknown[])[0]).split("\n")[2]).toMatch(/^CRITICAL /);
  });

  // D1: a run every blocking finding of which carries an approval reads approved, never blocked or clean.
  it("a run whose every blocking finding is approved reads approved, by whom and until when", () => {
    const by = (display: string, until: string) => ({ reason: "r", expires_at: until, approved_by: { kind: "person" as const, subject: display, display } });
    const run = (findings: FindingRead[]): RunRead => ({ id: "gr_8f2a", package: "pkg_9e40", state: "blocked", blocking: findings.filter((one) => one.properties.canyon.blocks).length, warnings: 0, findings });
    expect(verdict(run([advisory, { ...blocking, approval: by("Dana Whitfield", "2026-10-07T00:00:00Z") }]))).toBe("! Approved — 1 blocking finding, approved by Dana Whitfield until 7 Oct");
    const second = { ...blocking, entry: 1, properties: { canyon: { ...blocking.properties.canyon } } }; // another problem: its own entry
    expect(verdict(run([{ ...blocking, approval: by("Dana", "2026-10-07T00:00:00Z") }, { ...second, approval: by("Sam", "2026-11-01T00:00:00Z") }]))).toBe("! Approved — 2 blocking findings, each approved");
    expect(verdict(run([{ ...blocking, approval: by("Dana", "2026-10-07T00:00:00Z") }, second]))).toBe("✗ Blocked — 2 blocking findings");
    // The demo's session has no display: nobody signed in, never the subject as a name (#2696).
    const anonymous = { reason: "r", expires_at: "2026-10-07T00:00:00Z", approved_by: { kind: "person" as const, subject: "anonymous", display: "" } };
    expect(verdict(run([{ ...blocking, approval: anonymous }]))).toBe("! Approved — 1 blocking finding, approved by someone not signed in until 7 Oct");
  });
});
