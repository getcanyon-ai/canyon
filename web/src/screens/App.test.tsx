import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AccessRead, App as AppRead, AuditPage, Earlier, Env, Finding, FleetPage, ImageRead, Need, Package, ProjectEgress, Receipt, RunRead, Secret, Status, TimelineRow } from "../api";
import { AUTH, LIBRARIES, S } from "../strings";
import { RailContext } from "../ui/Rail";
import { forAgent } from "../ui/Finding";
import { App } from "./App";
import { CANCEL, UNLOCK_MS } from "./Promote";

const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
const page = golden<{ status: Status; refusal: { receipt: Receipt }; promotion: { promotion: Env }; approve: { reason: string; expires_at: string }; approved: unknown; packages: { packages: Package[]; next: string | null }[]; audit: AuditPage[] }>("app-page/claims-portal.json");
const approved = golden<{ run_approved: { run: RunRead }; run_placed: { run: RunRead }; status_approved: Status }>("app-page/claims-portal.json");
const project = golden<FleetPage>("fleet/page.json").apps.find((app) => app.id === "claims-portal") as AppRead;
// The findings are the gate's own log, flattened as the run read flattens it; every fingerprint on
// screen and in a request comes from the golden, never from a literal written here.
// The run read numbers each finding's entry: in this log, each its own (contracts/finding.md, One problem, one entry).
const findings = golden<{ runs: { results: Finding[] }[] }>("finding/claims-portal.sarif.json").runs.flatMap((one) => one.results).map((one, entry) => ({ ...one, entry }));
// The cards in the order the page draws them: blocking first, then the rest, the run's order within each.
const shown = [...findings.filter((one) => one.properties.canyon.blocks), ...findings.filter((one) => !one.properties.canyon.blocks)];
// Read by someone who may approve its blocking findings: an admin or a responder who did not start it (#2204).
const run: RunRead = { id: "gr_8f2a", package: "pkg_9e40", state: "blocked", approvable: true, blocking: 1, warnings: 1, findings };
const blocks = (one: Finding) => one.properties.canyon.blocks;
const APPROVE = (one: Finding) => `POST /api/v1/gate-runs/gr_8f2a/findings/${one.partialFingerprints["canyon/v1"]}/approve`;
const RUN = "GET /api/v1/gate-runs/gr_8f2a";
const TIMELINE = "GET /api/v1/gate-runs/gr_8f2a/timeline?after=0";
// The reviewer's row on a host with no model, as the gate writes it: one producer.sarif row.
const skipped = { id: 61, kind: "producer.sarif", data: { producer: "canyon-review", sarif: golden<object>("finding/skipped-review.sarif.json") } };
const PACKAGES = "GET /api/v1/projects/claims-portal/packages";
// History's versions read each version's review from the runs (#2696): none here.
const NO_RUNS = { "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [], next: null } } };
const pages = page.packages;
// The contract's own list, so a change to its shape breaks this screen's test rather than passing it.
const secrets = golden<{ secrets: Secret[] }>("secrets/claims-portal.json");
const first = findings.find(blocks) as Finding;
const flush = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));
const override = (reason: string, expires: string) => {
  fireEvent.change(screen.getByLabelText("Reason"), { target: { value: reason } });
  fireEvent.change(screen.getByLabelText("Approval expires"), { target: { value: expires } });
  fireEvent.click(screen.getByRole("button", { name: "Approve" }));
};
const why = (receipt: Receipt, id: string) => receipt.rules.find((rule) => rule.id === id)?.why ?? "";
// The one way to approve: the button on the finding's own card (design/specs/screens.md, The approval).
// A finding's card by its name: a secret's is where it is (#102), any other's its title.
const cardName = (one: Finding) => one.properties.canyon.secret_name === undefined ? one.properties.canyon.title
  : `A secret is in ${one.locations[0].physicalLocation.artifactLocation.uri.split("/").pop()}:${one.locations[0].physicalLocation.region.startLine}`;
const approveOn = async (one: Finding) => within(await screen.findByRole("article", { name: cardName(one) })).getByRole("button", { name: "Approve with a reason" });
const open: Status = { ...page.status, receipt: { ...page.status.receipt, blocked_by: [] } };
// The status read with preview's own run blocked: the refusal's clean-gate row in place of preview's
// passing one. The golden's preview is clean, because the gate stopped v1.4 before it got there.
const blocked: Status = { ...page.status, promote_run: "gr_8f2a", receipt: { ...page.status.receipt, blocked_by: page.refusal.receipt.blocked_by, rules: page.status.receipt.rules.map((rule) => (rule.id === "prod.clean-gate" ? page.refusal.receipt.rules[0] ?? rule : rule)) } };
const promoted: Status = { ...page.status, envs: { ...page.status.envs, prod: page.promotion.promotion } };
const PROMOTE = "POST /api/v1/projects/claims-portal/promotions";
// The gate clears: the blocking rule now passes and says something new, so the reason the button
// still carries through the hold can only be the one that has just gone.
const cleared: Status = { ...page.status, receipt: { ...page.status.receipt, blocked_by: [], rules: page.status.receipt.rules.map((rule) => (rule.id === "prod.clean-gate" ? { ...rule, passed: true, why: "gr_8f2a ended clean" } : rule)) } };
const answer = (body: unknown) => ({ ok: true, status: 200, json: async () => body }) as unknown as Response;
// The golden project is read by an admin, so the page reads its Access card too: the catch-all
// mocks below answer that route with the contract's access read and every other with the project.
const access = golden<{ read: AccessRead }>("access/claims-portal.json").read;
// A history tab, once chosen, reads its list: an empty one here.
// Egress is off on this host and the app's network open (contracts/testdata/egress): the Outbound card draws nothing.
const egressOff = { ...golden<{ project_read: object }>("egress/claims-portal.json").project_read, floor: null };
const projectOr = (url: string) => Promise.resolve(answer(url.endsWith("/access") ? access : url.includes("/packages") ? { packages: [], next: null } : url.endsWith("/secrets") ? { secrets: [] }
  : url === "/api/v1/egress" ? { via: "off" } : url.endsWith("/egress") ? egressOff : { project }));
// The headline of the fixture's newest version, v1.4, blocked by one finding nobody approved.
const BLOCKED = "v1.4 is blocked: 1 blocking finding";
// The question's button shows once production's secrets are read (#1698).
const confirmPromote = async () => fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3" }));

// The server answers "<METHOD> <url>" from a table of plain objects and 404s anything else; the
// returned reader lists every request as "<METHOD> <url>", with its body when it has one.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer | (() => Answer)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status = 200, body } = typeof route === "function" ? route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const reads = (status: () => Status) => ({
  "GET /api/v1/projects/claims-portal": { body: { project } },
  "GET /api/v1/projects/claims-portal/status": () => ({ body: status() }),
  [RUN]: { body: { run } },
  [TIMELINE]: { body: { rows: [skipped], next: null } },
});
const region = (name: string) => within(screen.getByRole("region", { name }));
// The status moves on to a run that has only just started; the run read answers with that run's own
// state, so "Reviewing…" is the server's word and not the absence of an answer.
// History is closed until asked for: a test opens it the way a reader does, by its summary.
const openHistory = () => fireEvent.click(screen.getByRole("region", { name: "History" }).querySelector("summary") as HTMLElement);

const reviewing = (next: RunRead) => {
  let last = page.status.last_run;
  vi.stubGlobal("fetch", vi.fn((url: string) => {
    if (url.includes("/timeline")) return Promise.resolve(answer({ rows: [], next: null }));
    if (url.includes(`/gate-runs/${next.id}`)) return Promise.resolve(answer({ run: next }));
    if (url.includes("/gate-runs/")) return Promise.resolve(answer({ run }));
    if (url.endsWith("/status")) return Promise.resolve(answer({ ...page.status, last_run: last }));
    return projectOr(url);
  }));
  return () => { last = next.id; };
};

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("the app page", () => {
  it("renders the server's receipt verbatim, and none of the machine's words", async () => {
    server(reads(() => blocked));
    const { container } = render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    for (const rule of blocked.receipt.rules) {
      expect(region("Go-live checklist").getByText(rule.name)).toBeDefined();
      expect(region("Go-live checklist").getByText(rule.why)).toBeDefined();
    }
    expect(region("Go-live checklist").getByText("Meridian requires a passing review to reach production.")).toBeDefined();
    expect(container.textContent).not.toMatch(/pkg_|gr_|package|receipt|stage|fingerprint/i); // versions, never a package's or a run's id
  });

  // The app page's checklist says what the approval's does of platform.known-vuln (#2579): where its packages are, and what
  // fixes each, from the contract's own row — the platform golden's, as the server derives it for a contributor.
  it("says where the known-vulnerable packages are and what fixes each", async () => {
    const row = golden<{ receipt_row: Receipt["rules"][number] }>("platform/claims-portal.json").receipt_row;
    server(reads(() => ({ ...open, receipt: { ...open.receipt, rules: [...open.receipt.rules, row] } })));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    const checks = region("Go-live checklist");
    expect(checks.getByText(row.why)).toBeDefined();
    expect(checks.getByText("None of these are in the app's own code, which its review checks.")).toBeDefined();
    expect(checks.getByText("No fix from Canyon yet")).toBeDefined();
    expect(checks.getByText("Waits for Canyon's next build tools update")).toBeDefined();
    expect(checks.getByText("Go's standard library v1.22.5")).toBeDefined();
    expect(checks.getByText("· in the app launcher")).toBeDefined();
    expect(checks.getByText("· in paketo-buildpacks/node-engine")).toBeDefined();
  });

  // #2594: while preview's patch is planned, the system libraries Canyon uses now fix its os packages.
  it("says Canyon is trying a fix in preview while preview's patch is planned", async () => {
    const row = golden<{ receipt_row: Receipt["rules"][number] }>("platform/claims-portal.json").receipt_row;
    const planned = { ...open.envs.preview!, patch: { state: "planned" as const, since: "2026-10-30T09:00:00Z" } };
    server(reads(() => ({ ...open, envs: { ...open.envs, preview: planned }, receipt: { ...open.receipt, rules: [...open.receipt.rules, row] } })));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    const checks = region("Go-live checklist");
    expect(checks.getByText("Canyon is trying a fix in preview")).toBeDefined();
    expect(checks.queryByText("No fix from Canyon yet")).toBeNull();
    expect(checks.getByText("Waits for Canyon's next build tools update")).toBeDefined();
  });

  // A preview waiting on its first release (env_pending alone, routes.md) is drawn by its release on
  // the Preview card: the page renders, and nothing links to an address the app does not have yet.
  it("draws a preview waiting on its first release by the release it waits on", async () => {
    // broker-portal is the contract's app waiting on its first release: its status read, its fleet row as the project read.
    const waiting = golden<Status>("app-page/broker-portal-status.json");
    const row = golden<FleetPage>("fleet/member.json").apps.find((app) => app.id === "broker-portal") as AppRead;
    const reviewing: RunRead = { id: "gr_9c04", package: "pkg_5e21", state: "running", blocking: 0, warnings: 0, findings: [] };
    server({
      "GET /api/v1/projects/broker-portal": { body: { project: row } },
      "GET /api/v1/projects/broker-portal/status": { body: waiting },
      "GET /api/v1/gate-runs/gr_9c04": { body: { run: reviewing } },
      "GET /api/v1/gate-runs/gr_9c04/timeline?after=0": { body: { rows: [], next: null } },
    });
    render(<App id="broker-portal" auth="oidc" />);
    const clock = new Date("2026-09-02T10:00:00Z").toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    expect(within(await screen.findByRole("region", { name: "Preview" })).getByText(`waiting on your platform since ${clock}`)).toBeDefined();
    expect(region("Preview").queryByRole("link")).toBeNull();
    expect(screen.queryByRole("link", { name: "Open v1.3 in production" })).toBeNull();
  });

  // A release open over what prod runs (a promotion handed to the org): the live version and the release, both.
  it("says the release a running prod waits on beside what runs there", async () => {
    const pending = { via: "webhook" as const, release: "rel_9b31", event: "deploy" as const, sent_at: "2026-09-27T10:00:00Z", state: "refused" as const };
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, prod: { ...page.promotion.promotion, run_by: "org", pending } } })));
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByRole("link", { name: "Open v1.3 in production" })).getAttribute("href")).toBe(page.promotion.promotion.url);
    expect(region("Production").getByText("Your platform refused the release")).toBeDefined();
  });

  // A prod waiting on its first release (a promotion handed to the org) is drawn the same way.
  it("draws a prod waiting on its first release by the release it waits on", async () => {
    const pending = { via: "webhook" as const, release: "rel_8f2a", event: "deploy" as const, sent_at: "2026-09-27T10:00:00Z", state: "undelivered" as const };
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, prod: { run_by: "org", pending } } })));
    render(<App id="claims-portal" auth="oidc" />);
    expect(within(await screen.findByRole("region", { name: "Production" })).getByText("Your platform did not accept the release")).toBeDefined();
    expect(screen.queryByRole("link", { name: "Open v1.3 in production" })).toBeNull();
  });

  // What Canyon checked sits beside the review, unfolded, its chevron saying it folds (#102): a host with no model says so there.
  it("shows each tool's line in what Canyon checked, the skipped review among them", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    const said = await within(await screen.findByRole("region", { name: "What Canyon checked" })).findByText("skipped, no AI model set up"); // said once, on its row
    expect(region("What Canyon checked").getByText("AI review")).toBeDefined(); // a tool in plain words, never canyon-review
    expect(said.closest("details")?.open).toBe(true);
  });

  // The custom-rules agent's row reads in plain words too, its notes as it said them.
  it("names the custom rules' review in plain words", async () => {
    const custom = JSON.parse(JSON.stringify(skipped).replaceAll("canyon-review", "custom-rules").replaceAll("ai review ·", "custom rules ·"));
    server({ ...reads(() => page.status), [TIMELINE]: { body: { rows: [skipped, { ...custom, id: 62 }], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    expect(await within(await screen.findByRole("region", { name: "What Canyon checked" })).findByText("custom rules")).toBeDefined();
    expect(region("What Canyon checked").getAllByText("skipped, no AI model set up")).toHaveLength(2);
  });

  // An audit row's view → pins the page to the run it ruled on: its findings, not the newest run's.
  it("shows the pinned run's verdict and links back to the newest run", async () => {
    const older: RunRead = { id: "gr_6c12", package: "pkg_7a1c", state: "clean", blocking: 0, warnings: 0, findings: [] };
    const sent = server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: older } } });
    render(<App id="claims-portal" run="gr_6c12" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Review" })).findByText("✓ v1.3 passed review")).toBeDefined(); // the run names its version, as preview names it
    expect(region("Review").getByRole("link", { name: "the newest run →" }).getAttribute("href")).toBe("#/apps/claims-portal");
    expect(sent()).not.toContain(RUN);
  });

  // A clean run whose review names something it did not check is never called clean (contracts/finding.md).
  it("never calls a run clean when its review names something it did not check", async () => {
    const older: RunRead = { id: "gr_6c12", package: "pkg_7a1c", state: "clean", not_checked: true, blocking: 0, warnings: 0, findings: [] };
    server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: older } } });
    render(<App id="claims-portal" run="gr_6c12" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Review" })).findByText(S.verdictNotChecked)).toBeDefined();
  });

  // A pinned run is not the app's state, but its findings are its own: its run read says whether the reader
  // approves them there — an older run, a rollback review (#2242, #2198) — and the approval is that run's.
  // Promote still names the newest run.
  it("approves a pinned older run's finding on that run when its read lets the reader, and promotes what preview runs", async () => {
    const older: RunRead = { ...run, id: "gr_6c12", package: "pkg_7a1c" };
    const pinned = { "GET /api/v1/gate-runs/gr_6c12": { body: { run: older } } };
    const onOlder = `POST /api/v1/gate-runs/gr_6c12/findings/${first.partialFingerprints["canyon/v1"]}/approve`;
    const approving = server({ ...reads(() => page.status), ...pinned, [onOlder]: { body: page.approved } });
    render(<App id="claims-portal" run="gr_6c12" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Review" })).findByText(S.verdictBlocked)).toBeDefined();
    expect(screen.queryByText(S.approvers)).toBeNull();
    fireEvent.click(await approveOn(first));
    override(page.approve.reason, page.approve.expires_at.slice(0, 10));
    await waitFor(() => expect(screen.queryByLabelText("Reason")).toBeNull());
    expect(approving().filter((request) => request.startsWith("POST"))).toEqual([`${onOlder} ${JSON.stringify(page.approve)}`]);
    cleanup();
    const sent = server({ ...reads(() => open), ...pinned, [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" run="gr_6c12" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await waitFor(() => expect(sent().some((request) => request.startsWith(PROMOTE))).toBe(true));
    expect(sent().find((request) => request.startsWith(PROMOTE))).toContain(page.status.promote_run as string);
  });

  // A pinned run its read does not let the reader approve says who approves, in place of the button (#2242).
  it("says who approves on a pinned run the reader may not approve", async () => {
    const { approvable: _, ...at } = run;
    server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: { ...at, id: "gr_6c12", package: "pkg_7a1c" } } } });
    render(<App id="claims-portal" run="gr_6c12" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Review" })).findByText(S.approvers)).toBeDefined();
    expect(within(await screen.findByRole("article", { name: cardName(first) })).queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  // A push's, where the org asks approvers to link GitHub and the reader linked none: why, and the link to link (#2342).
  it("tells an approver of a push to link GitHub first, and links to linking", async () => {
    const { approvable: _, ...at } = run;
    server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: { ...at, id: "gr_6c12", package: "pkg_7a1c", link_github: true } } } });
    render(<RailContext value={{ org: null, viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: true }, admin: true }}><App id="claims-portal" run="gr_6c12" auth="oidc" /></RailContext>);
    const review = within(await screen.findByRole("region", { name: "Review" }));
    expect(await review.findByText(S.pushApproversLink.replace("<org>", page.status.receipt.org), { exact: false })).toBeDefined();
    expect(review.getByRole("link", { name: AUTH.githubLink }).getAttribute("href")).toBe("/api/v1/github/link");
    expect(review.queryByText(S.approvers)).toBeNull();
  });

  // A host that can't link an account (no GitHub App with a client secret, the viewer's github_link false) never
  // offers the link, which would end on the route's 409: it says why, and an admin is sent to Settings › GitHub (#2351).
  it("never offers Link GitHub account on a host that can't link one", async () => {
    const { approvable: _, ...at } = run;
    for (const role of ["admin", "responder"] as const) {
      server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: { ...at, id: "gr_6c12", package: "pkg_7a1c", link_github: true } } } });
      const { unmount } = render(<RailContext value={{ org: null, viewer: { display: "Dana Whitfield", role, github_logins: [], github_link: false }, admin: role === "admin" }}><App id="claims-portal" run="gr_6c12" auth="oidc" /></RailContext>);
      const review = within(await screen.findByRole("region", { name: "Review" }));
      const line = await review.findByText(S.pushApproversLink.replace("<org>", page.status.receipt.org), { exact: false });
      expect(line.textContent).toContain(S.pushApproversCantLink.replace("<settings>", "Settings › GitHub"));
      expect(review.queryByRole("link", { name: AUTH.githubLink })).toBeNull();
      expect(within(line).queryByRole("link", { name: "Settings › GitHub" })?.getAttribute("href") ?? null).toBe(role === "admin" ? "#/settings/github" : null);
      unmount();
    }
  });

  // Every version on the same four stages, stopping where it stopped (the redesign): v1.4 stops at
  // review and never reaches preview; v1.3 passed, runs in preview and is what Promote ships.
  it("puts each version on the pipeline at the stage where it stopped", async () => {
    const cleared13: RunRead = { id: "gr_6c12", package: "pkg_7a1c", state: "clean", blocking: 0, warnings: 0, findings: [] };
    server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: cleared13 } } });
    render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    const row = (name: string) => within(table).getByRole("rowheader", { name: new RegExp(`^${name.replace(".", "\\.")}`) }).closest("[role=row]") as HTMLElement;
    const stage = (name: string, at: string) => within(row(name)).getByRole("cell", { name: at }).textContent;
    await waitFor(() => expect(stage("v1.3", "Review")).toBe("Passed"));
    expect([stage("v1.4", "Review"), stage("v1.4", "Preview"), stage("v1.4", "Production")]).toEqual(["Blocked1 blocking finding", "Never reached preview", "—"]);
    expect(stage("v1.3", "Preview")).toBe("Running");
    expect(region("Preview").getByRole("link", { name: "Open v1.3 in preview" }).getAttribute("rel")).toBe("noreferrer"); // where it answers is its card's
    expect(stage("v1.3", "Go-live checks")).toMatch(/of \d+ checks$/);
    expect(stage("v1.3", "Production")).toBe(page.status.receipt.blocked_by.length === 0 ? "Ready to promote" : "Waiting on the go-live checks");
  });

  // What runs now, never what the last deploy recorded: a stopped preview says Stopped and
  // opens nothing; one Canyon could not check says so and keeps the way to open it.
  it("says a stopped preview stopped, and one Canyon could not check, never Running", async () => {
    const at = (state: "stopped" | "unknown"): Status => ({ ...open, envs: { preview: { ...(open.envs.preview as Env), state } } });
    for (const [state, want, opens, runsIn] of [["stopped", "Stopped", false, ["—", "Preview · stopped"]], ["unknown", "Couldn't check", true, ["—", "Preview · couldn't check"]]] as const) {
      server({ ...reads(() => at(state)), [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
      const { unmount } = render(<App id="claims-portal" auth="oidc" />);
      const table = await screen.findByRole("table", { name: "Versions" });
      const preview = () => within(within(table).getByRole("rowheader", { name: /^v1\.3/ }).closest("[role=row]") as HTMLElement).getByRole("cell", { name: "Preview" });
      await waitFor(() => expect(preview().textContent).toBe(want));
      expect(screen.queryByRole("link", { name: "Open preview" }) !== null).toBe(opens); // the header's way into the app, gone with the app
      fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
      const versions = within(screen.getByRole("tabpanel", { name: "Versions" }));
      await versions.findAllByText("v1.3");
      expect(versions.getAllByRole("row").slice(1).map((row) => row.lastElementChild?.textContent)).toEqual(runsIn); // Running in
      unmount();
    }
  });

  // A preview that keeps restarting says how it last exited when the executor said it, as a stopped one
  // says why: the test cluster's case-dies-later never reads stopped on Kubernetes (#1597).
  it("says how a restarting preview last exited, on its card and in Versions", async () => {
    const restarting = { restarts: 7, exit_code: 1, said: "lost the queue: exiting" };
    server({ ...reads(() => ({ ...open, envs: { preview: { ...(open.envs.preview as Env), state: "restarting", restarting } } })), [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
    render(<App id="claims-portal" auth="oidc" />);
    const card = await screen.findByRole("region", { name: "Preview" });
    await waitFor(() => expect(card.textContent).toContain("last exited 1 — lost the queue: exiting · canyon logs has the rest"));
    fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
    const versions = within(screen.getByRole("tabpanel", { name: "Versions" }));
    await versions.findAllByText("v1.3");
    expect(versions.getAllByRole("row").slice(1).map((row) => row.lastElementChild?.textContent)[1]).toMatch(/^Preview · keeps restarting: 7 times since it was deployed .* · last exited 1 — lost the queue: exiting$/);
  });

  // An environment that keeps restarting has its last log lines under the band, read masked (#2744), in place of the one line; a body the server doesn't
  // say it masked is never shown, and a log it can't mask, or couldn't read, keeps the one line and says why.
  it("shows a restarting production's last log lines under the band, masked", async () => {
    const restarting = { restarts: 7, exit_code: 1, said: "lost the queue: exiting" };
    const LOGS = "GET /api/v1/projects/claims-portal/logs?env=prod&lines=20&redaction=masked";
    for (const [status, redaction, text, want] of [
      [200, "masked", "GET /login 200 12ms\nlost the queue at <API_TOKEN>: exiting\n", null],
      [200, "none", "lost the queue at tok-91c2: exiting\n", S.lastLinesUnread.replaceAll("<env>", "prod")],
      [409, null, "", S.lastLinesUnmasked.replaceAll("<env>", "prod")],
      [422, null, "", S.lastLinesTooLong.replaceAll("<env>", "prod")], // over 1 MiB: never cut and masked
      [500, null, "", S.lastLinesUnread.replaceAll("<env>", "prod")],
      [403, null, "", ""], // the reader's access, never a fault: the one line alone
    ] as const) {
      const calls = server({ ...reads(() => ({ ...promoted, envs: { ...promoted.envs, prod: { ...(promoted.envs.prod as Env), state: "restarting", restarting } } })), [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
      const routed = vi.mocked(fetch);
      vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => (`${init?.method} ${url}` !== LOGS ? routed(url, init)
        : { ok: status < 300, status, headers: new Headers(redaction === null ? {} : { "Canyon-Log-Redaction": redaction }), text: async () => text, json: async () => ({ error: "not_maskable" }) } as unknown as Response)));
      const { unmount } = render(<App id="claims-portal" auth="oidc" />);
      const band = within(await screen.findByRole("region", { name: "Where it stands" }));
      if (want === null) {
        expect(await band.findByText(S.lastLines)).toBeDefined();
        expect(band.getByText((_, node) => node?.tagName === "PRE").textContent).toBe("GET /login 200 12ms\nlost the queue at <API_TOKEN>: exiting");
        expect(band.queryByText("lost the queue: exiting")).toBeNull();
      } else if (want === "") {
        await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).includes("/logs?"))).toBe(true));
        await act(async () => { await Promise.resolve(); });
        expect(band.getByText("lost the queue: exiting")).toBeDefined();
        expect(band.queryByText(/last lines/)).toBeNull();
      } else {
        expect(await band.findByText(want)).toBeDefined();
        expect(band.getByText("lost the queue: exiting")).toBeDefined();
        expect(band.queryByText(S.lastLines)).toBeNull();
        expect(document.body.textContent).not.toContain("tok-91c2");
      }
      expect(vi.mocked(fetch).mock.calls.filter(([url, init]) => `${init?.method} ${url}` === LOGS)).toHaveLength(1);
      expect(calls()).not.toContain(LOGS); // never through the JSON routes
      unmount();
    }
    // A reader below the masked read's minimum level (a viewer, not an admin) is never asked for it: its band keeps the one line.
    server({ ...reads(() => ({ ...promoted, envs: { ...promoted.envs, prod: { ...(promoted.envs.prod as Env), state: "restarting", restarting } } })),
      "GET /api/v1/projects/claims-portal": { body: { project: { ...project, access: { ...project.access, level: "viewer" } } } }, [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
    render(<RailContext.Provider value={{ org: null, viewer: null, admin: false }}><App id="claims-portal" auth="oidc" /></RailContext.Provider>);
    const band = within(await screen.findByRole("region", { name: "Where it stands" }));
    expect(band.getByText("lost the queue: exiting")).toBeDefined();
    await act(async () => { await Promise.resolve(); });
    expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).includes("/logs?"))).toBe(false);
    expect(band.queryByText(/last lines/)).toBeNull();
  });

  // A restarting environment the band doesn't speak of, a preview on an older version while the newest is live in
  // production, shows its last lines on its own card, so the need's "Its last log lines, on its page" is always so.
  it("shows a restarting preview's last log lines on its card when the band is about another version", async () => {
    const restarting = { restarts: 3, exit_code: 1, said: "lost the queue: exiting" };
    const LOGS = "GET /api/v1/projects/claims-portal/logs?env=preview&lines=20&redaction=masked";
    server({ ...reads(() => ({ ...promoted, envs: { ...promoted.envs, preview: { ...(promoted.envs.preview as Env), package: "pkg_older", state: "restarting", restarting } } })), [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
    const routed = vi.mocked(fetch);
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => (`${init?.method} ${url}` !== LOGS ? routed(url, init)
      : { ok: true, status: 200, headers: new Headers({ "Canyon-Log-Redaction": "masked" }), text: async () => "lost the queue at <API_TOKEN>: exiting\n" } as unknown as Response)));
    render(<App id="claims-portal" auth="oidc" />);
    const band = within(await screen.findByRole("region", { name: "Where it stands" }));
    expect(band.queryByText(S.lastLines)).toBeNull(); // the band is production's, live
    const card = within(screen.getByRole("region", { name: "Preview" }));
    expect(await card.findByText(S.lastLines)).toBeDefined();
    expect(card.getByText((_, node) => node?.tagName === "PRE").textContent).toBe("lost the queue at <API_TOKEN>: exiting");
    expect(vi.mocked(fetch).mock.calls.filter(([url, init]) => `${init?.method} ${url}` === LOGS)).toHaveLength(1);
  });

  // A stopped preview says why when the executor said it: its exit code and its last line, masked by
  // the server, as text — markup in the line is shown, never rendered — or its exit code alone.
  it("says why a stopped preview stopped, as text, on its card", async () => {
    const said = `<img src=x onerror="alert(1)"> Error: <DATABASE_URL> refused`;
    const at = (stopped: Env["stopped"]): Status => ({ ...open, envs: { preview: { ...(open.envs.preview as Env), state: "stopped", stopped } } });
    for (const [stopped, why] of [[{ exit_code: 1, said }, `exited 1 — ${said}`], [{ exit_code: 137, said: "" }, "exited 137"]] as const) {
      server({ ...reads(() => at(stopped)), [PACKAGES]: { body: pages[0] }, ...NO_RUNS });
      const { unmount } = render(<App id="claims-portal" auth="oidc" />);
      const card = await screen.findByRole("region", { name: "Preview" });
      await waitFor(() => expect(card.textContent).toContain(`${why} · canyon logs has the rest`));
      expect(document.querySelector("img[src=x]")).toBeNull();
      fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
      const versions = within(screen.getByRole("tabpanel", { name: "Versions" }));
      await versions.findAllByText("v1.3");
      expect(versions.getAllByRole("row").slice(1).map((row) => row.lastElementChild?.textContent)).toEqual(["—", `Preview · stopped: ${why}`]);
      unmount();
    }
  });

  // The stages are joined as the mockup draws them: dashed where a version never got, solid through
  // what it passed; and the version preview runs, ready, says so where production is, as a label: the
  // one promote button is beside the go-live checks.
  it("joins each version's stages, and says where production is that preview's version is ready", async () => {
    server(reads(() => open));
    render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    const cell = (name: string, at: string) => within(within(table).getByRole("rowheader", { name: new RegExp(`^${name.replace(".", "\\.")}`) }).closest("[role=row]") as HTMLElement).getByRole("cell", { name: at });
    await waitFor(() => expect(cell("v1.3", "Preview").textContent).toBe("Running"));
    expect(cell("v1.4", "Review").querySelector("span[aria-hidden=true].border-dashed")).not.toBeNull();
    expect(cell("v1.3", "Preview").querySelector("span[aria-hidden=true].border-ok")).not.toBeNull();
    expect(cell("v1.3", "Production").querySelector("span[aria-hidden=true]")).toBeNull();
    expect(cell("v1.3", "Production").textContent).toBe("Ready to promote");
    expect(within(cell("v1.3", "Production")).queryByRole("button")).toBeNull();
    expect(screen.getAllByRole("button", { name: /^Promote v1\.3/ }).map((one) => one.textContent)).toEqual(["Promote v1.3 to production"]);
  });

  // A version in production that has left preview never reached… nothing: no dashed line says it stopped.
  // What production runs is its card's, never a row here: the pipeline holds versions on their way, and
  // on an app whose newest version is what production runs it is not drawn at all (the redesign, block 3).
  it("draws only versions on their way, and no pipeline when production runs the newest", async () => {
    server(reads(() => ({ ...page.status, envs: { prod: page.promotion.promotion } })));
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    expect(within(table).getAllByRole("rowheader").map((row) => row.querySelector("span")?.textContent)).toEqual(["v1.4"]); // v1.3 is production's, on its card
    unmount();
    const live = { ...project, latest: { ...project.latest, package: page.promotion.promotion.package, label: "v1.3", stage: "production" as const } };
    server({ ...reads(() => ({ ...page.status, envs: { prod: page.promotion.promotion } })), "GET /api/v1/projects/claims-portal": { body: { project: live } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Production" });
    await flush();
    expect(screen.queryByRole("table", { name: "Versions" })).toBeNull();
  });

  // A clean run that names something nothing read is never called a pass, here or in the verdict.
  it("never calls a running version Passed when its review left something unchecked", async () => {
    const partly: RunRead = { id: "gr_6c12", package: "pkg_7a1c", state: "clean", not_checked: true, blocking: 0, warnings: 0, findings: [] };
    server({ ...reads(() => page.status), "GET /api/v1/gate-runs/gr_6c12": { body: { run: partly } } });
    render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    await waitFor(() => expect(within(table).getAllByRole("cell", { name: "Review" }).map((cell) => cell.textContent)).toContain("Not fully checked"));
    expect(within(table).queryByText("Passed")).toBeNull();
  });

  // Nothing checked is never "all met", and a preview the platform has not taken is never ready.
  it("never reads all met with nothing checked, nor ready while preview waits on the platform", async () => {
    const empty: Status = { ...open, receipt: { ...open.receipt, rules: [] } };
    server(reads(() => empty));
    render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    await waitFor(() => expect(within(table).getAllByRole("cell", { name: "Go-live checks" }).map((cell) => cell.textContent)).not.toContain("All met0 of 0 checks"));
    cleanup();
    server(reads(() => ({ ...open, receipt: { ...open.receipt, waiting: "Nothing in preview to promote." } })));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("table", { name: "Versions" });
    expect(screen.queryByText("Ready to promote")).toBeNull();
  });

  // Below contributor nothing of the app is read but its row, whatever run the link pins.
  it("reads no run for a read-only reader, even one the link pins", async () => {
    const sent = server({
      "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [], summary: "x", access: { level: "viewer" } } } },
      [RUN]: { body: { run } },
    });
    render(<App id="pricing-lab" run="gr_8f2a" auth="oidc" />);
    await screen.findByText(`${S.readOnly} ${S.readOnlyUse}`);
    await flush();
    expect(sent()).toEqual(["GET /api/v1/projects/pricing-lab", "GET /api/v1/preferences"]); // the app's row, and whether the reader is a responder
  });

  // The name is for people and may repeat, so the id, the app's address, is beside it when they differ (#1911).
  it("heads the page with the app's name, and its id beside it when they differ", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, name: "Claims portal" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    const heading = await screen.findByRole("heading", { level: 1, name: /^Claims portal/ });
    expect(heading.textContent).toBe("Claims portal claims-portal");
    expect(screen.getByRole("navigation", { name: "Breadcrumb" }).textContent).toContain("Claims portal");
  });

  // The body waits on the app's row and its status read, one after the other (#2561): until both answer the
  // page says it is reading, never an empty page under the title; a failure says itself in its place.
  it("says it is reading the app until its first reads answer", async () => {
    let answerStatus = () => {};
    const routes = reads(() => page.status);
    server(routes);
    const answered = vi.mocked(fetch).getMockImplementation()!;
    vi.stubGlobal("fetch", vi.fn((url: string, init?: RequestInit) => (url === "/api/v1/projects/claims-portal/status" && init?.method === "GET"
      ? new Promise<Response>((resolve) => { answerStatus = () => resolve(answered(url, init) as unknown as Response); })
      : answered(url, init))));
    render(<App id="claims-portal" auth="oidc" />);
    await flush();
    expect(screen.getByRole("status").textContent).toBe(S.appReading);
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("claims-portal");
    answerStatus();
    await screen.findByText(BLOCKED);
    expect(screen.queryByText(S.appReading)).toBeNull();
    cleanup();
    server({ "GET /api/v1/projects/claims-portal": { status: 500, body: { error: "internal", message: "internal error" } } });
    render(<App id="claims-portal" auth="oidc" />);
    await flush();
    expect(screen.queryByText(S.appReading)).toBeNull();
  });

  // Until the newest review's read answers, the Review section says it's reading it: never a heading over white space
  // beside the go-live checks, which a person reads as a review that found nothing (the v0.11.0 role walk).
  it("says the review is being read until its read answers", async () => {
    server(reads(() => page.status));
    const stub = vi.mocked(globalThis.fetch);
    const answered = stub.getMockImplementation() as (url: string, init?: RequestInit) => Promise<Response>;
    let release: (value: Response) => void = () => {};
    stub.mockImplementation((url, init) => (`${init?.method} ${String(url)}` === RUN ? new Promise<Response>((resolve) => { release = resolve; }) : answered(String(url), init)));
    render(<App id="claims-portal" auth="oidc" />);
    const review = within(await screen.findByRole("region", { name: "Review" }));
    expect(await review.findByText(S.reviewReading)).toBeDefined();
    await act(async () => { release(answer({ run })); });
    await waitFor(() => expect(review.queryByText(S.reviewReading)).toBeNull());
  });

  it("heads the page with the id alone when the name is the id", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    await flush();
    expect((await screen.findByRole("heading", { level: 1 })).textContent).toBe("claims-portal");
  });

  // No single status for the app in its header: one badge always lies about some of its versions. The
  // headline says it once, of the newest version, with what keeps running.
  it("carries no stage badge in the header, and says why the newest version stopped", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    const header = (await screen.findByRole("heading", { level: 1 })).closest("header") as HTMLElement;
    expect(within(header).queryByText("Blocked")).toBeNull();
    expect(await screen.findByRole("heading", { name: BLOCKED })).toBeDefined();
    expect(screen.getByRole("region", { name: "Where it stands" }).textContent).toMatch(/v1\.3 keeps running in preview\./);
    expect(screen.getAllByText(/blocking finding/).filter((one) => one.closest("[aria-label='Review']") !== null && one.tagName !== "SPAN")).toEqual([]); // said once
  });

  // A preview that stopped is never said to keep running.
  it("does not say a stopped preview keeps running", async () => {
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, preview: { ...page.status.envs.preview!, state: "stopped" } } })));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: BLOCKED });
    expect(screen.getByRole("region", { name: "Where it stands" }).textContent).not.toContain("keeps running");
  });

  // An app with no current package has no review of its own: its newest check's run is shown,
  // read-only and said to be a check, while the app itself stays never reviewed.
  it("shows the newest check's review, read-only, on an app nothing was uploaded to", async () => {
    const unshipped = { ...project, gate_state: "never_reviewed" as const, envs: {}, latest: { package: null, stage: "created" as const, since: "2026-09-02T09:00:00Z" } };
    const row = { id: run.id, package: run.package, check: true, state: run.state, blocking: run.blocking, warnings: run.warnings, started_by: null, started_at: "2026-09-02T09:31:04Z", ended_at: "2026-09-02T09:31:40Z" };
    server({ ...reads(() => ({ ...page.status, envs: {}, last_run: null, promote_run: null })), "GET /api/v1/projects/claims-portal": { body: { project: unshipped } },
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [row], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    const review = await screen.findByRole("region", { name: "Review" });
    await waitFor(() => expect(review.textContent).toContain("From a check — not deployed yet"));
    expect(region("Where it stands").getByRole("heading").textContent).toBe("Nothing uploaded yet");
    expect(await within(review).findByRole("article", { name: cardName(first) })).toBeDefined();
    expect(within(review).queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  // A poll that finds a current package ends the check's stand-in: a version not yet reviewed shows
  // no check's findings, and once the app's own run is the last its findings can be approved.
  it("drops the check's review once the app has a current package", async () => {
    vi.useFakeTimers();
    const unshipped = { ...project, gate_state: "never_reviewed" as const, envs: {}, latest: { package: null, stage: "created" as const, since: "2026-09-02T09:00:00Z" } };
    const row = { id: run.id, package: run.package, check: true, state: run.state, blocking: run.blocking, warnings: run.warnings, started_by: null, started_at: "2026-09-02T09:31:04Z", ended_at: "2026-09-02T09:31:40Z" };
    let app: AppRead = unshipped, last: string | null = null;
    server({ ...reads(() => ({ ...page.status, envs: {}, last_run: last, promote_run: null })), "GET /api/v1/projects/claims-portal": () => ({ body: { project: app } }),
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [row], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Review").getByRole("article", { name: cardName(first) })).toBeDefined());
    app = { ...unshipped, latest: { package: "pkg_7a1c", stage: "awaiting_review", since: "2026-09-02T09:40:00Z" } };
    await vi.advanceTimersByTimeAsync(2000);
    await act(async () => {});
    expect(screen.getByRole("region", { name: "Review" }).textContent).not.toContain("From a check");
    expect(region("Review").queryAllByRole("article")).toEqual([]);
    app = { ...project, latest: { ...project.latest, package: run.package } };
    last = run.id;
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(within(region("Review").getByRole("article", { name: cardName(first) })).getByRole("button", { name: "Approve with a reason" })).toBeDefined());
    expect(screen.getByRole("region", { name: "Review" }).textContent).not.toContain("From a check");
  });

  // A newer version awaiting review on an app that has had runs is reviewed from its own page.
  it("offers Review v1.4 when the newest version is awaiting review", async () => {
    const waitingProject = { ...project, gate_state: "clean" as const, latest: { ...project.latest, stage: "awaiting_review" as const } };
    const sent = server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: waitingProject } }, "POST /api/v1/projects/claims-portal/gate-runs": { body: { run: { id: "gr_next", state: "pending" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Review v1.4" }));
    await waitFor(() => expect(sent()).toContain("POST /api/v1/projects/claims-portal/gate-runs"));
  });

  // The name at the top of the page is the way into what else that person is on the hook for.
  it("links the app's owner to the same owner view, by subject", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByRole("link", { name: "Dana Whitfield" })).getAttribute("href")).toBe("#/fleet?owner=dana%40meridian.example");
  });

  // Below contributor the project read is the app's name, owners and summary and nothing else, and
  // every other read of the app is refused (contracts/routes.md, App teams): the page asks for none.
  it("shows a read-only member the app's name, owners and summary and their access, and reads nothing else", async () => {
    const nora = { kind: "person", subject: "nora@meridian.example", display: "Nora Lind" };
    const sent = server({
      "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [nora], summary: "Tries new premium curves", access: { level: "viewer" } } } },
      "GET /api/v1/projects/pricing-lab/status": { status: 403, body: { error: "team_required", message: "Ask Nora Lind to add you to pricing-lab", owners: [nora] } },
    });
    const { container } = render(<App id="pricing-lab" auth="oidc" />);
    expect((await screen.findByRole("link", { name: "Nora Lind" })).getAttribute("href")).toBe("#/fleet?owner=nora%40meridian.example");
    expect(screen.getByText(/Tries new premium curves/)).toBeDefined();
    expect(screen.getByText(`${S.readOnly} ${S.readOnlyUse}`)).toBeDefined();
    expect(S.readOnly).toBe("Your access: read-only.");
    expect(screen.getAllByRole("heading", { level: 2 }).map((one) => one.textContent)).toEqual(["Not running", S.whoLooksAfter]); // the headline, and who looks after it
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint|answered/i);
    expect(sent()).toEqual(["GET /api/v1/projects/pricing-lab", "GET /api/v1/preferences"]); // the app's row, and whether the reader is a responder
    expect(screen.queryByRole("link", { name: S.askForAccess })).toBeNull(); // read-only can already open it
  });

  // A read-only member can ask its owners to let them change it (#1706): one request, answered in the
  // owner's name, as the no-access screen answers it.
  it("lets a read-only member ask for access", async () => {
    const nora = { kind: "person", subject: "nora@meridian.example", display: "Nora Lind" };
    const sent = server({
      "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [nora], summary: "x", access: { level: "viewer" } } } },
      "POST /api/v1/projects/pricing-lab/access-requests": { status: 201, body: null },
    });
    render(<App id="pricing-lab" auth="oidc" />);
    // the button says what it asks for: to join the team, which changes the app (#2774)
    fireEvent.click(await within(await screen.findByRole("region", { name: "Where it stands" })).findByRole("button", { name: "Ask to join its team" }));
    expect(await screen.findByText("Asked. Nora Lind will see your request.")).toBeDefined();
    expect(sent().filter((one) => one.startsWith("POST"))).toEqual(["POST /api/v1/projects/pricing-lab/access-requests"]);
    expect(screen.queryByRole("button", { name: "Ask to join its team" })).toBeNull();
    expect(screen.queryByRole("button", { name: S.askForAccess })).toBeNull();
  });

  // A read-only member opens the app from its page (contracts/routes.md, the project read): a link to
  // each environment, and the team's words for one that does not run — one that stopped opens nothing.
  it("shows a read-only member a link to each environment, and in the team's words one that does not run", async () => {
    const PREVIEW = "http://pricing-lab.preview.localhost:7777", PROD = "https://pricing.apps.meridian.example";
    const shared = (envs: object) => ({ "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [], summary: "x", access: { level: "viewer" }, envs } } } });
    server(shared({ preview: { url: PREVIEW, state: "running" }, prod: { url: PROD } }));
    const { unmount } = render(<App id="pricing-lab" auth="oidc" />);
    expect((await screen.findByRole("link", { name: "Open preview" })).getAttribute("href")).toBe(PREVIEW);
    expect(screen.getByRole("link", { name: "Open production" }).getAttribute("href")).toBe(PROD);
    expect(screen.queryByText(/Stopped|Couldn't check/)).toBeNull();
    unmount();
    server(shared({ preview: { url: PREVIEW, state: "stopped" }, prod: { url: PROD, state: "unknown" } }));
    render(<App id="pricing-lab" auth="oidc" />);
    expect((await screen.findByRole("link", { name: "Open production" })).getAttribute("href")).toBe(PROD);
    expect(screen.getByText("Couldn't check whether it runs")).toBeDefined();
    expect(screen.queryByRole("link", { name: "Open preview" })).toBeNull();
    expect(screen.getByText("Stopped")).toBeDefined();
  });

  // design/PATCHING.md §8: the environment cards say what the fleet's cells say — when Canyon's patch reached
  // preview, and that a fix waits for the team in production, linked to its approval.
  it("says a patch on each environment's card: patched by Canyon in preview, a fix waiting in production", async () => {
    const patch = { since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32) };
    const patchedStatus: Status = { ...promoted, envs: {
      preview: { ...promoted.envs.preview!, patch: { ...patch, state: "patched_preview", run: "gr_9c4e1a" } } as Env,
      prod: { ...promoted.envs.prod!, patch: { ...patch, state: "asked", approval: "K7QF-2M9X" } } as Env,
    } };
    server(reads(() => patchedStatus));
    render(<App id="claims-portal" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Preview" })).findByText("patched by Canyon · 10 Oct")).toBeDefined();
    const waiting = within(screen.getByRole("region", { name: "Production" })).getByRole("link", { name: "fix ready for production" });
    expect(waiting.getAttribute("href")).toBe("#/approve?code=K7QF-2M9X");
    expect(waiting.className).toContain("text-amber");
  });

  // #2602: the cards say what canyon status's platform row says of a patch that went wrong — a revert in its run's own
  // words, red; a hold in D5's, amber, a patch run's end by its own last line — from the same envs.<env>.patch.
  it("says a reverted patch on preview's card and a held one on production's", async () => {
    const patch = { since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32) };
    const reverted = "Canyon reverted preview at 19:31 UTC: the patched app restarted, so the image it replaced runs again";
    const at = (preview: Env["patch"], prod: Env["patch"]): Status => ({ ...promoted, envs: {
      preview: { ...promoted.envs.preview!, patch: preview } as Env, prod: { ...promoted.envs.prod!, patch: prod } as Env } });
    server({ ...reads(() => at({ ...patch, state: "reverted", run: "gr_patch01" }, { ...patch, state: "held", why: "paused" })),
      "GET /api/v1/gate-runs/gr_patch01/timeline?after=0": { body: { rows: [{ id: 1, kind: "build.log", data: { line: "Watching preview until 19:40 UTC: if the patched app exits or restarts, Canyon puts back the image it replaced" } }], next: 1 } },
      "GET /api/v1/gate-runs/gr_patch01/timeline?after=1": { body: { rows: [{ id: 2, kind: "build.log", data: { line: reverted } }], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    const line = await within(await screen.findByRole("region", { name: "Preview" })).findByText(reverted);
    expect(line.className).toContain("text-critical");
    const held = region("Production").getByText("held: Canyon paused this fix until an admin resumes it");
    expect(held.className).toContain("text-amber");
  });

  it("names what holds production's patch, and a patch run's end by its own line", async () => {
    const patch = { since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32) };
    const ineffective = "The patched image still has every package the patch was for: Canyon did not deploy it";
    const preview = { ...promoted.envs.preview!, label: "v1.4", patch: { ...patch, state: "held", why: "patch_ineffective", run: "gr_patch02" } } as Env;
    server({ ...reads(() => ({ ...promoted, envs: { preview, prod: { ...promoted.envs.prod!, patch: { ...patch, state: "held", why: "preview_ahead" } } as Env } })),
      "GET /api/v1/gate-runs/gr_patch02/timeline?after=0": { body: { rows: [{ id: 1, kind: "build.log", data: { line: ineffective } }], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Preview" })).findByText(`held: ${ineffective}`)).toBeDefined();
    expect(region("Production").getByText("held: promote v1.4 to take the fix to production")).toBeDefined();
  });

  // #2601: production's team declined the patch, so production says it still runs what the patch fixes, each package
  // once from the denied approval's read, and that Canyon won't ask again; the read failing still says the denial.
  it("says production declined Canyon's fix and what it still runs", async () => {
    const patch = { since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32) };
    const fix = { to: "3.0.13-0ubuntu3.5", vulnerability: "CVE-2024-5535", class: "os" };
    const denied: Status = { ...promoted, envs: { preview: { ...promoted.envs.preview!, patch: { ...patch, state: "patched_preview", run: "gr_9c4e1a" } } as Env,
      prod: { ...promoted.envs.prod!, patch: { ...patch, state: "denied", run: "gr_9c4e1a", approval: "K7QF-2M9X" } } as Env } };
    server({ ...reads(() => denied), "GET /api/v1/promotion-approvals/K7QF-2M9X": { body: { state: "denied", kind: "patch", fixes: [
      { ...fix, name: "libssl3", from: "3.0.13-0ubuntu3.4" }, { ...fix, name: "libssl3", from: "3.0.13-0ubuntu3.4", vulnerability: "CVE-2024-6119" }, { ...fix, name: "openssl", from: "3.0.13-0ubuntu3.4" }] } } });
    render(<App id="claims-portal" auth="oidc" />);
    const line = await within(await screen.findByRole("region", { name: "Production" }))
      .findByText("Your team turned down Canyon's fix for production. Production still has libssl3 3.0.13-0ubuntu3.4, openssl 3.0.13-0ubuntu3.4. Canyon will ask again when a newer fix is out.");
    expect(line.className).toContain("text-amber");
    expect(region("Preview").getByText("patched by Canyon · 10 Oct")).toBeDefined();
    cleanup();
    server(reads(() => denied));
    render(<App id="claims-portal" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Production" })).findByText("Your team turned down Canyon's fix for production. Canyon will ask again when a newer fix is out.")).toBeDefined();
  });

  it("shows a read-only member no patch line, whatever the row carries", async () => {
    const PREVIEW = "http://pricing-lab.preview.localhost:7777";
    const patch = { state: "patched_preview", since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32) };
    server({ "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [], summary: "x", access: { level: "viewer" },
      envs: { preview: { url: PREVIEW, state: "running", patch }, prod: { url: PREVIEW, state: "running", patch: { ...patch, state: "asked", approval: "K7QF-2M9X" } } } } } } });
    render(<App id="pricing-lab" auth="oidc" />);
    await screen.findByRole("link", { name: "Open preview" });
    expect(screen.queryByText(/patched by Canyon/)).toBeNull();
    expect(screen.queryByText("fix ready for production")).toBeNull();
  });

  // A security responder approves on any app (design/PERMISSIONS.md, Security responders; #2204): with no grant,
  // the page stays theirs, reads the status and the newest run, and offers what they may approve.
  it("shows a responder with no access the newest review's findings to approve", async () => {
    window.location.hash = "#/apps/claims-portal";
    const row = { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "none" } };
    const sent = server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: row } },
      "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } }, [APPROVE(first)]: { body: page.approved } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByText(S.responderAccess)).toBeDefined();
    fireEvent.click(await approveOn(first));
    override(page.approve.reason, page.approve.expires_at.slice(0, 10));
    await waitFor(() => expect(sent().filter((request) => request.startsWith("POST"))).toEqual([`${APPROVE(first)} ${JSON.stringify(page.approve)}`]));
    expect(window.location.hash).toBe("#/apps/claims-portal");
    expect(sent().some((request) => request.includes("/timeline") || request.includes("/packages"))).toBe(false);
    window.location.hash = "";
  });

  // A responder reads the newest version by its label, as everyone else does, from the need the status read names it in;
  // and, never deploying, is given no block to copy for an agent (#102).
  it("names the newest version to a responder by its label, and gives them nothing to copy for an agent", async () => {
    const row = { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "none" } };
    const need: Need = { kind: "blocked", env: null, rank: 6, since: null, you: true, ask: [], version: "v1.4", count: 1 };
    server({ ...reads(() => ({ ...page.status, needs: [need] })), "GET /api/v1/projects/claims-portal": { body: { project: row } },
      "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("heading", { name: /^What blocks v1\.4/ })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Copy for your agent" })).toBeNull();
    cleanup();
    // with no need to name it, it is the newest version: never its package's id
    server({ ...reads(() => ({ ...page.status, needs: [] })), "GET /api/v1/projects/claims-portal": { body: { project: row } },
      "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("heading", { name: /^What blocks the newest version/ })).toBeDefined();
  });

  // Approving is one click from the headline: it opens the first finding's form, the keyboard on its reason, whose label is
  // in sentence case as every form's is (#102).
  it("opens the first finding's approval from the headline's Approve with a reason", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    const headed = await screen.findByRole("region", { name: "Where it stands" });
    fireEvent.click(await within(headed).findByRole("button", { name: "Approve with a reason: go to the findings" }));
    const reason = await screen.findByLabelText("Reason");
    await waitFor(() => expect(document.activeElement).toBe(reason));
    expect(screen.getByText("Reason").className).not.toContain("uppercase");
  });

  // A package only the built app holds is approved from deps.known-vuln's go-live check (#2610), on the review the
  // receipt is read for: a responder with no grant, whose status read names no preview, reads the check and approves
  // it, every fingerprint of the package at once; whoever that review's run read lets approve nothing — an admin who
  // started it — sees no Approve to be refused.
  it("lets a responder with no access approve a package only the built app holds, and nobody the review refuses", async () => {
    window.location.hash = "#/apps/claims-portal";
    const row = { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "none" } };
    const fingerprints = [`f_${"a".repeat(64)}`, `f_${"b".repeat(64)}`];
    const deps = { id: "deps.known-vuln", name: "No known-vulnerable dependencies", blocks: true, passed: false,
      why: "lodash 4.17.20 is in v1.4's built app, but its review didn't see it: 4.17.21 fixes it",
      built_app: [{ name: "lodash", version: "4.17.20", advisories: [{ id: "CVE-2021-23337", fixed: "4.17.21" }], fingerprints, approved: false }] };
    const status: Status = { ...page.status, envs: {}, receipt: { ...page.status.receipt, blocked_by: ["deps.known-vuln"], rules: [...page.status.receipt.rules, deps] } };
    const BUILT = `POST /api/v1/gate-runs/${status.promote_run}/findings/approve`;
    for (const approvable of [true, false]) {
      const sent = server({ ...reads(() => status), "GET /api/v1/projects/claims-portal": { body: { project: row } },
        "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } },
        [`GET /api/v1/gate-runs/${status.promote_run}`]: { body: { run: { ...run, id: status.promote_run, approvable } } }, [BUILT]: { body: { approvals: [] } } });
      render(<App id="claims-portal" auth="oidc" />);
      expect(await screen.findByText("lodash 4.17.20")).toBeDefined();
      if (!approvable) {
        await flush();
        expect(within(screen.getByRole("complementary", { name: "Promote" })).queryByRole("button", { name: "Approve with a reason" })).toBeNull();
        cleanup();
        continue;
      }
      fireEvent.click(within(await screen.findByRole("complementary", { name: "Promote" })).getByRole("button", { name: "Approve with a reason" }));
      override(page.approve.reason, page.approve.expires_at.slice(0, 10));
      await waitFor(() => expect(sent().filter((request) => request.startsWith("POST"))).toEqual([`${BUILT} ${JSON.stringify({ fingerprints, ...page.approve })}`]));
      cleanup();
    }
    window.location.hash = "";
  });

  // Someone nobody has shared the app with is not on its team: the no-access screen is theirs, never
  // the reduced page, and the page's poll stops with the move.
  it("sends a person with no access to the no-access screen, showing nothing of the app first", async () => {
    window.location.hash = "#/apps/pricing-lab";
    const sent = server({ "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [], summary: "Tries new premium curves", access: { level: "none" } } } } });
    render(<App id="pricing-lab" auth="oidc" />);
    await waitFor(() => expect(window.location.hash).toBe("#/no-access/pricing-lab"));
    expect(screen.queryByText("Tries new premium curves")).toBeNull();
    expect(screen.queryByText(`${S.readOnly} ${S.readOnlyUse}`)).toBeNull();
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 2100)); });
    expect(sent()).toEqual(["GET /api/v1/projects/pricing-lab", "GET /api/v1/preferences"]); // the app's row, and whether the reader is a responder
    window.location.hash = "";
  });

  it("carries the Access card for a team member, with the org the server names", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, access: { level: "contributor", everyone: false, people: 2, groups: 0 } } } } });
    render(<App id="claims-portal" auth="oidc" />);
    const line = await screen.findByText("Shared with 2 people besides its owners · Ask Dana Whitfield or an admin to change it.");
    expect(line.closest("section")?.getAttribute("aria-label")).toBe("Access");
  });

  it("says No owner in the header when nobody owns the app", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, owners: [] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByText("No owner")).closest("header")).not.toBeNull();
  });

  // Production already runs what preview runs: a promotion would change nothing, so none is offered.
  it("says the version is in production, and offers no promote, when production runs what preview runs", async () => {
    const preview = page.status.envs.preview!;
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, prod: { ...preview, promoted_by: null } } })));
    render(<App id="claims-portal" auth="oidc" />);
    const aside = await screen.findByRole("complementary", { name: "Promote" });
    expect(await within(aside).findByText("v1.3 is in production.")).toBeDefined();
    expect(within(aside).queryByRole("button", { name: /Promote/ })).toBeNull();
  });

  // A production that stopped is not in production: promoting the same version again runs it again.
  it("offers the promote again when production runs the same version but stopped", async () => {
    const preview = page.status.envs.preview!;
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, prod: { ...preview, promoted_by: null, state: "stopped" } } })));
    render(<App id="claims-portal" auth="oidc" />);
    const aside = await screen.findByRole("complementary", { name: "Promote" });
    expect(await within(aside).findByRole("button", { name: "Promote v1.3 to production" })).toBeDefined();
    expect(within(aside).queryByText("v1.3 is in production.")).toBeNull();
  });

  // A stopped preview is never promoted: the receipt waits on the server's line, said once, as the disabled promote's
  // reason, never again by the checklist above it (#2774).
  it("says a stopped preview isn't running and offers no promote from it", async () => {
    const line = "v1.3 isn't running in preview: canyon logs says why.";
    server(reads(() => ({ ...page.status, envs: { ...page.status.envs, preview: { ...page.status.envs.preview!, state: "stopped" } }, receipt: { ...page.status.receipt, blocked_by: [], rules: [], waiting: line } })));
    render(<App id="claims-portal" auth="oidc" />);
    const aside = await screen.findByRole("complementary", { name: "Promote" });
    const promote = await within(aside).findByRole("button", { name: "Promote v1.3 to production" });
    expect(promote.hasAttribute("disabled")).toBe(true);
    expect(within(aside).getAllByText(line)).toHaveLength(1);
    expect(promote.parentElement?.textContent).toContain(line);
  });

  it("the disabled promote carries the receipt's reason", async () => {
    server(reads(() => blocked));
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByRole("button", { name: "Promote v1.3 to production" })).hasAttribute("disabled")).toBe(true);
    expect(within(screen.getByRole("complementary", { name: "Promote" })).getAllByText(why(blocked.receipt, blocked.receipt.blocked_by[0] ?? "")).length).toBeGreaterThan(0);
  });

  it("a 409 renders the receipt that refused", async () => {
    const sent = server({ ...reads(() => open), [PROMOTE]: { status: 409, body: page.refusal } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    // The checklist already says this line before the 409: the disabled button is what says the refusal landed.
    await waitFor(() => expect(screen.getByRole("button", { name: "Promote v1.3 to production" }).hasAttribute("disabled")).toBe(true));
    expect(region("Go-live checklist").getByText(why(page.refusal.receipt, "prod.human-approval"))).toBeDefined();
    expect(sent()).toContain(`${PROMOTE} ${JSON.stringify({ gate_run: page.status.promote_run })}`);
    expect(screen.queryByText(S.promoted.replace("<app>", "claims-portal"))).toBeNull();
  });

  it("a promotion re-reads the status", async () => {
    let status = open;
    server({ ...reads(() => status), [PROMOTE]: () => { status = promoted; return { body: page.promotion }; } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    expect((await screen.findByRole("link", { name: "Open v1.3 in production" })).getAttribute("href")).toBe(page.promotion.promotion.url);
  });

  it("a second click while the promotion is out sends nothing", async () => {
    const sent = server({ ...reads(() => open), [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    const button = await screen.findByRole("button", { name: "Promote v1.3" });
    await act(async () => { fireEvent.click(button); fireEvent.click(button); });
    expect(sent().filter((request) => request.startsWith(PROMOTE))).toHaveLength(1);
  });

  // A version whose preview answered an error is refused to the CLI; here the question says so and its
  // button is the person's override, sent as promote_anyway (contracts/routes.md, preview_answered).
  it("a preview that answered an error asks for Promote anyway and sends the override", async () => {
    const answered: Status = { ...open, envs: { ...open.envs, preview: { ...open.envs.preview!, answered: { status: 400, said: "" } } } };
    const sent = server({ ...reads(() => answered), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    expect(screen.getByText("v1.3 answered 400 at / on preview. Check the app before promoting it.")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
    const anyway = await screen.findByRole("button", { name: "Promote anyway" });
    expect(anyway.className).not.toContain("bg-accent"); // the warning draws the eye, never the risky button
    fireEvent.click(anyway);
    await waitFor(() => expect(sent()).toContain(`${PROMOTE} {"gate_run":"${open.promote_run}","promote_anyway":true}`));
  });

  // Preview was deployed again between the page's read and the click: the refusal is not shown as the CLI's
  // sentence; the page reads preview again, and the question then offers the override.
  it("a promotion refused for what a newer preview answered reads the page again", async () => {
    let answered = false;
    const now = (): Status => (answered ? { ...open, envs: { ...open.envs, preview: { ...open.envs.preview!, answered: { status: 500, said: "" } } } } : open);
    server({ ...reads(now), [PROMOTE]: () => { answered = true; return { status: 409, body: { error: "preview_answered", message: "Not promoted: v1.3 answers 500 Internal Server Error at / on preview. Fix the app and deploy again, or have a person promote it from the dashboard after checking it.", version: "v1.3", answered: { status: 500, said: "" } } }; } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    expect(await screen.findByRole("button", { name: "Promote anyway" })).toBeDefined();
    expect(screen.queryByText(/have a person promote it from the dashboard/)).toBeNull();
  });

  it("a preview that answered nothing odd sends no override", async () => {
    const sent = server({ ...reads(() => open), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await waitFor(() => expect(sent()).toContain(`${PROMOTE} {"gate_run":"${open.promote_run}"}`));
  });

  it("promote asks before it sends", async () => {
    const sent = server({ ...reads(() => open), [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    expect(screen.getByText(S.promoteConfirm)).toBeDefined();
    expect(sent().filter((request) => request.startsWith(PROMOTE))).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(S.promoteConfirm)).toBeNull();
    expect(sent().filter((request) => request.startsWith(PROMOTE))).toHaveLength(0);
  });

  // Production runs preview's image with its own secrets (#1698): the question names each runtime value set
  // for preview and not for production, with the line that sets it; a resource's value, a build value and
  // one set for both say nothing, and none of it stops the promotion.
  it("the promote question names what production lacks", async () => {
    const row = (name: string, env: "preview" | "prod", where: "runtime" | "build", origin: string) => ({ name, env, where, origin, version: 1, set_by: { kind: "person", subject: "dana", display: "Dana" }, set_at: "2026-10-01T00:00:00Z" });
    const sent = server({ ...reads(() => open), [PROMOTE]: { body: page.promotion }, "GET /api/v1/projects/claims-portal/secrets": { body: { secrets: [
      row("STRIPE_KEY", "preview", "runtime", "user"), row("DATABASE_URL", "preview", "runtime", "provisioned"), row("SENTRY_RELEASE", "preview", "build", "user"),
      row("BOTH", "preview", "runtime", "user"), row("BOTH", "prod", "runtime", "user")] } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    expect(await screen.findByText("STRIPE_KEY is set for preview but not production. Without it, v1.3 may fail to start in production.", { exact: false })).toBeDefined();
    expect(screen.getByRole("link", { name: "Set it for production" }).getAttribute("href")).toBe("#/apps/claims-portal/secrets?name=STRIPE_KEY&env=prod");
    expect(within(screen.getByRole("complementary", { name: "Promote" })).queryByText(/canyon secret set/)).toBeNull(); // the dashboard sets it, never the CLI's line (#102)
    expect(screen.getAllByText(/is set for preview but not production/)).toHaveLength(1);
    await confirmPromote();
    await waitFor(() => expect(sent().some((request) => request.startsWith(PROMOTE))).toBe(true));
  });

  // Promoting under prod.egress-allowed while it is advisory names what production will start refusing (#2723): the
  // row's own destinations, in the question, beside what production lacks; a row that names none says nothing.
  // From the status read's will_refuse (D15), never the rule's own line: host names a person can allow, apart from what only
  // a change to the code fixes.
  it("the promote question names the refusals production will start", async () => {
    const egress = (why: string, passed = false) => ({ ...open, receipt: { ...open.receipt, rules: [...open.receipt.rules, { id: "prod.egress-allowed", name: "Production allows everything preview connects to", blocks: false, passed, why }] } });
    const refuses: Need = { kind: "will_refuse", env: "prod", rank: 7, since: null, you: true, ask: [], version: "v1.3", blocks: false,
      denied: [{ destination: "1.1.1.1", port: 443, allowable: false }, { destination: "example.com", port: 443, allowable: true }, { destination: "example.com", port: 8443, allowable: false }] };
    server({ ...reads(() => ({ ...egress("production does not allow a line the page never reads"), needs: [refuses] })), [PROMOTE]: { body: page.promotion } });
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    expect(await screen.findByText("Production will refuse example.com once v1.3 goes live.")).toBeDefined();
    expect(screen.getByText("Nobody can allow 1.1.1.1, example.com:8443: the code has to change.")).toBeDefined();
    expect(within(screen.getByRole("region", { name: "Go-live checklist" })).getByText("Production doesn't allow example.com yet. Nobody can allow 1.1.1.1, example.com:8443: the code has to change.")).toBeDefined();
    expect(screen.queryByText(/a line the page never reads/)).toBeNull();
    unmount();
    for (const quiet of [egress("could not check: Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound."), egress("production allows everything v1.3 has connected to so far", true)]) {
      server({ ...reads(() => quiet), [PROMOTE]: { body: page.promotion } });
      const { unmount: gone } = render(<App id="claims-portal" auth="oidc" />);
      fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
      expect(screen.getByText(S.promoteConfirm)).toBeDefined();
      expect(screen.queryByText(/^Production will refuse/)).toBeNull();
      gone();
    }
  });

  // The 202 is a promotion under way, never one done: production says so when it is live.
  it("the promotion says it started and where it shows, and none of the machine's words", async () => {
    server({ ...reads(() => open), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } } });
    const { container } = render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    expect(await screen.findByText(S.promoted.replace("<app>", "claims-portal"))).toBeDefined();
    expect(container.textContent).not.toMatch(/Promoted claims-portal|pkg_|package|receipt|stage|fingerprint/i);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(S.promoted.replace("<app>", "claims-portal"))).toBeNull();
  });

  // The run's own lines follow the button as the server writes them, from the first page to the
  // last, and production is read again when the run ends: live, or the reason it failed.
  const promoting = (pages: [TimelineRow[], number | null][], status?: () => void) => {
    const run = "GET /api/v1/gate-runs/gr_9c41/timeline";
    return Object.fromEntries(pages.map(([rows, next], at) => [`${run}?after=${at === 0 ? 0 : pages[at - 1]![1]}`, () => { if (next === null) status?.(); return { body: { rows, next } }; }]));
  };
  const line = (id: number, text: string): TimelineRow => ({ id, kind: "build.log", data: { line: text } });
  it("the promotion's lines show live under the button and end on live", async () => {
    let status = open;
    server({ ...reads(() => status), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } },
      ...promoting([[[line(11, "postgres db: creating"), line(12, "postgres db: ready")], 12], [[line(13, "deploying to production"), line(14, "live at http://claims-portal.prod.localhost:7777")], null]], () => { status = promoted; }) });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await waitFor(() => expect(within(screen.getByRole("list", { name: "Promotion" })).getAllByRole("listitem").map((item) => item.textContent)).toEqual(
      ["postgres db: creating", "postgres db: ready", "deploying to production", "live at http://claims-portal.prod.localhost:7777"]));
    expect((await screen.findByRole("link", { name: "Open v1.3 in production" })).getAttribute("href")).toBe(page.promotion.promotion.url);
  });

  it("a failed promotion ends on its reason and production stays as it was", async () => {
    server({ ...reads(() => open), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } },
      ...promoting([[[line(11, "postgres db: creating"), line(12, "postgres db: failed: quota exceeded")], null]]) });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await waitFor(() => expect(within(screen.getByRole("list", { name: "Promotion" })).getAllByRole("listitem").at(-1)?.textContent).toBe("postgres db: failed: quota exceeded"));
  });

  // A promotion can be cancelled from under its lines while it goes: at once while pending, after the
  // step in flight while running — which says so when it lands — and never once production switches.
  const CANCEL_RUN = "POST /api/v1/gate-runs/gr_9c41/cancel";
  const waitingLine = line(11, "postgres db: waiting on the platform");
  const cancelled: TimelineRow = { id: 12, kind: "run.state", data: { state: "failed", failure_reason: "interrupted" } };
  for (const [name, answer, said] of [
    ["pending", { status: 200, body: { run: { id: "gr_9c41", state: "failed" } } }, CANCEL.done],
    ["too late", { status: 409, body: { error: "too_late", message: CANCEL.tooLate } }, CANCEL.tooLate],
  ] as const) {
    it(`cancelling a ${name} promotion says "${said}"`, async () => {
      server({ ...reads(() => open), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } }, [CANCEL_RUN]: answer, ...promoting([[[waitingLine], 11], [[], 11]]) });
      render(<App id="claims-portal" auth="oidc" />);
      fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
      await confirmPromote();
      fireEvent.click(await screen.findByRole("button", { name: "Cancel promotion" }));
      expect(await screen.findByText(said)).toBeDefined();
      expect(screen.queryByRole("button", { name: "Cancel promotion" })).toBeNull();
    });
  }

  it("cancelling a running promotion waits for the step in flight, then says what stays", async () => {
    let landed = false; // the step in flight ends when the test says so
    const reader = server({ ...reads(() => open), [PROMOTE]: { status: 202, body: { run: { id: "gr_9c41", state: "pending" } } },
      [CANCEL_RUN]: { status: 202, body: { run: { id: "gr_9c41", state: "running" } } },
      ...promoting([[[waitingLine], 11]]),
      "GET /api/v1/gate-runs/gr_9c41/timeline?after=11": () => ({ body: landed ? { rows: [cancelled], next: null } : { rows: [], next: 11 } }) });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await screen.findByText("postgres db: waiting on the platform"); // the step the copy names
    fireEvent.click(screen.getByRole("button", { name: "Cancel promotion" }));
    expect(await screen.findByText(CANCEL.cancelling("postgres db: waiting on the platform"))).toBeDefined();
    expect(CANCEL.cancelling("postgres db: waiting on the platform")).toBe("Cancelling after the current step (postgres db: waiting on the platform)…");
    landed = true;
    expect(await screen.findByText(CANCEL.kept, undefined, { timeout: 3000 })).toBeDefined();
    expect(reader().filter((call) => call === CANCEL_RUN)).toHaveLength(1);
  });

  // run_in_progress and deleting carry no receipt: the server's sentence, and the page stays.
  for (const refusal of [{ error: "run_in_progress", message: "a run of this project is running", run: { id: "gr_busy", state: "running" } }, { error: "deleting", message: "this project is being deleted" }]) {
    it(`a ${refusal.error} refusal says the server's sentence`, async () => {
      server({ ...reads(() => open), [PROMOTE]: { status: 409, body: refusal } });
      render(<App id="claims-portal" auth="oidc" />);
      fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
      await confirmPromote();
      expect(await screen.findByText(refusal.message)).toBeDefined();
      expect(region("Go-live checklist")).toBeDefined();
    });
  }

  // Clicking the button takes it off the screen, so the keyboard has nowhere to stand unless the
  // confirmation catches it: the next Enter has to be the answer to the question just asked.
  it("hands the keyboard to the confirmation", async () => {
    server({ ...reads(() => open), [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    const confirm = await screen.findByRole("button", { name: "Promote v1.3" }); // once production's secrets are read
    await waitFor(() => expect(document.activeElement).toBe(confirm));
  });

  it("hands the keyboard back when the question is cancelled", async () => {
    server({ ...reads(() => open), [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Promote v1.3 to production" }));
  });

  it("hands the keyboard back to the promote button when the line is dismissed", async () => {
    server({ ...reads(() => open), [PROMOTE]: { body: page.promotion } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Promote v1.3 to production" }));
    await confirmPromote();
    await screen.findByText(S.promoted.replace("<app>", "claims-portal"));
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Promote v1.3 to production" }));
  });

  it("the checklist row flips before the button fills", async () => {
    vi.useFakeTimers();
    let status = blocked;
    server(reads(() => status));
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(screen.getByRole("button", { name: "Promote v1.3 to production" })).toBeDefined());
    status = cleared;
    await vi.advanceTimersByTimeAsync(2000);
    await act(async () => {});
    expect(region("Go-live checklist").getAllByRole("listitem")[0]?.textContent).toMatch(/^Met/);
    expect(screen.getByRole("button", { name: "Promote v1.3 to production" }).hasAttribute("disabled")).toBe(true);
    expect(within(screen.getByRole("complementary", { name: "Promote" })).getAllByText(why(blocked.receipt, "prod.clean-gate")).length).toBeGreaterThan(0);
    await act(async () => { await vi.advanceTimersByTimeAsync(UNLOCK_MS); });
    expect(screen.getByRole("button", { name: "Promote v1.3 to production" }).hasAttribute("disabled")).toBe(false);
  });

  it("a page that loads unblocked fills at once", async () => {
    server(reads(() => cleared));
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByRole("button", { name: "Promote v1.3 to production" })).hasAttribute("disabled")).toBe(false);
  });

  it("the hold and the token are the same 160ms", () => {
    const tokens = readFileSync(join(import.meta.dirname, "../tokens.css"), "utf8");
    expect(tokens).toContain(`--ds-motion-delay-unlock: ${UNLOCK_MS}ms`);
  });

  it("stops asking the server once the page is gone", async () => {
    vi.useFakeTimers();
    const sent = server(reads(() => page.status));
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    await vi.advanceTimersByTimeAsync(2000);
    await act(async () => {});
    expect(sent().filter((request) => request.endsWith("/status"))).toHaveLength(2);
    const asked = sent().length;
    unmount();
    await vi.advanceTimersByTimeAsync(10000);
    expect(sent()).toHaveLength(asked);
  });

  it("an unknown app says so", async () => {
    server({});
    render(<App id="nope" auth="oidc" />);
    expect(await screen.findByText("There's no app called nope on this Canyon.", { exact: false })).toBeDefined();
    expect(screen.getByRole("link", { name: "Back to the fleet" }).getAttribute("href")).toBe("#/fleet");
    expect(within(screen.getByRole("navigation", { name: "Breadcrumb" })).getByRole("link", { name: "Fleet" }).getAttribute("href")).toBe("#/fleet");
    expect(within(screen.getByRole("main")).queryByRole("button")).toBeNull(); // the rail's menu button is not the page's
  });

  it("production carries its review only when its run came back clean", async () => {
    for (const [state, shown] of [["clean", 1], ["blocked", 0]] as const) {
      const sent = server({ ...reads(() => promoted), "GET /api/v1/gate-runs/gr_6c12": { body: { run: { ...run, id: "gr_6c12", package: "pkg_7a1c", state } } } });
      render(<App id="claims-portal" auth="oidc" />);
      await screen.findByRole("link", { name: "Open v1.3 in production" });
      await waitFor(() => expect(sent()).toContain("GET /api/v1/gate-runs/gr_6c12"));
      await flush();
      await waitFor(() => expect(within(screen.getByRole("region", { name: "Production" })).queryAllByText("v1.3 · passed review")).toHaveLength(shown));
      cleanup();
    }
  });

  // Found by the UX journeys: after a failed build the gate is still in the way, but not because of
  // a finding nobody approved, so the page must not ask for approvals that already exist.
  it("offers no approval when every blocking finding is already approved", async () => {
    const approved: RunRead = { ...run, findings: run.findings.map((one) => (one.properties.canyon.blocks
      ? { ...one, approval: { reason: "fixture", expires_at: "2099-01-01T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } } }
      : one)) };
    server({ ...reads(() => page.status), [RUN]: { body: { run: approved } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("heading", { name: "v1.4 was approved past its review" })).toBeDefined(); // D1: approved, not blocked, and not clean
    expect(region("Where it stands").getByText(/Approved — 1 blocking finding, approved by Dana until 1 Jan/)).toBeDefined();
    expect(screen.queryByText(S.nothingBuilt)).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  it("an empty reason never reaches the server", async () => {
    const sent = server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await approveOn(first));
    override("   ", page.approve.expires_at.slice(0, 10));
    expect(sent().filter((request) => request.includes("/approve")).length).toBe(0);
    expect(screen.getByText("Needs a reason")).toBeDefined();
  });

  it("approve sends the golden body to the golden fingerprint, and promotes nothing", async () => {
    const sent = server({ ...reads(() => page.status), [APPROVE(first)]: { body: page.approved } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await approveOn(first));
    override(page.approve.reason, page.approve.expires_at.slice(0, 10));
    await waitFor(() => expect(screen.queryByLabelText("Reason")).toBeNull());
    expect(sent().filter((request) => request.startsWith("POST"))).toEqual([`${APPROVE(first)} ${JSON.stringify(page.approve)}`]);
    const after = sent().slice(sent().findIndex((request) => request.startsWith("POST")) + 1);
    expect(after).toContain(RUN); // the run again, so the card says approved and the verdict counts it
    expect(after).toContain("GET /api/v1/projects/claims-portal/status");
  });

  // One Canyon rule on one line that two of opengrep's rules found is one problem (contracts/finding.md, One
  // problem, one entry; #2013): one card naming both rules' tool, one in every count, and one approval
  // that approves both findings.
  it("one rule on one line found by two rules is one card, one count and one approval", async () => {
    const sqli = (ruleId: string, fingerprint: string): Finding => ({ ...first, ruleId, tool: "sarif/opengrep", entry: 0, partialFingerprints: { "canyon/v1": fingerprint },
      properties: { canyon: { ...first.properties.canyon, secret_name: undefined, title: "SQL injection in the claims search" } } });
    const [one, two] = [sqli("sql.injection.opengrep.tainted-sql", `f_${"a".repeat(64)}`), sqli("sql.injection.opengrep.express-sqli", `f_${"b".repeat(64)}`)];
    const sent = server({ ...reads(() => blocked), [RUN]: { body: { run: { ...run, blocking: 1, warnings: 0, findings: [one, two] } } }, [APPROVE(one)]: { body: page.approved }, [APPROVE(two)]: { body: page.approved } });
    render(<App id="claims-portal" auth="oidc" />);
    const review = within(await screen.findByRole("region", { name: "Review" }));
    const cards = await review.findAllByRole("article", { name: "SQL injection in the claims search" });
    expect(cards).toHaveLength(1);
    expect(within(cards[0]!).getByText("Found by opengrep (2 rules)")).toBeDefined();
    expect(screen.getAllByText(BLOCKED).length).toBeGreaterThan(0);
    expect(review.getByRole("heading", { level: 3 }).textContent).toMatch(/1$/);
    fireEvent.click(within(cards[0]!).getByRole("button", { name: "Approve with a reason" }));
    override(page.approve.reason, page.approve.expires_at.slice(0, 10));
    await waitFor(() => expect(screen.queryByLabelText("Reason")).toBeNull());
    expect(sent().filter((request) => request.startsWith("POST"))).toEqual([`${APPROVE(one)} ${JSON.stringify(page.approve)}`, `${APPROVE(two)} ${JSON.stringify(page.approve)}`]);
  });

  // One dependency's blocking findings are one card (screens.md, The approval; #1848, #1875): the QA run's
  // five lodash CVEs, read from the contract's fixture, in trivy's own fields, approved with one form.
  const lodash = golden<{ run: { run: RunRead }; approve_findings: { fingerprints: string[]; reason: string; expires_at: string } }>("app-page/birthday-list-dependency-approval.json");
  const lodashBlocking = lodash.run.run.findings.filter(blocks);
  const HEAD = "lodash 4.17.4: 5 known vulnerabilities";

  it("draws one dependency's blocking findings as one card, in trivy's fields, never its message", async () => {
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...lodash.run.run, approvable: true, id: run.id, package: run.package } } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("article", { name: HEAD }));
    for (const one of lodashBlocking) expect(screen.queryByRole("article", { name: cardName(one) })).toBeNull(); // no card of its own
    expect(card.getByText("Critical")).toBeDefined(); // the highest severity among them
    expect(card.getByText("package-lock.json:17 · lodash 4.17.4")).toBeDefined();
    expect(card.queryByText(/••••/)).toBeNull();
    expect(card.queryByText(/Installed Version|Link: \[/)).toBeNull(); // trivy's message is its fields, never a paragraph
    const rows = card.getAllByRole("listitem");
    expect(rows).toHaveLength(5);
    const first = within(rows[0]!);
    expect(first.getByText(lodashBlocking[0]!.properties.canyon.title)).toBeDefined();
    expect(rows[0]!.textContent).toContain("CVE-2018-16487 · High · fixed in 4.17.11");
    expect(first.getByRole("link", { name: "CVE-2018-16487" }).getAttribute("href")).toBe("https://avd.aquasec.com/nvd/cve-2018-16487");
    expect(first.getByRole("button", { name: "Approve with a reason" })).toBeDefined(); // each finding stays its own to approve
    expect(card.getByText("Upgrade lodash to 4.18.0 (fixes all 5) and commit package-lock.json.")).toBeDefined();
    expect(card.getByRole("button", { name: "Approve all 5 for lodash 4.17.4" })).toBeDefined();
    // Copy for your agent: the group as `canyon check --ai` prints it (cli.md §5.2), then the steps after the fix.
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    fireEvent.click(card.getByRole("button", { name: "Copy for your agent" }));
    expect(await card.findByText("Copied")).toBeDefined();
    const copied = (writeText.mock.calls[0] as unknown as [string])[0];
    expect(copied.startsWith([
      "Fix these findings from Canyon's review of claims-portal:",
      "",
      `title: ${HEAD}`, "severity: critical", "blocks: yes", "rule: deps.known-vuln", "found by: trivy", "file: package-lock.json", "line: 17", "findings: 5, one per CVE:",
      ...lodashBlocking.map((one) => `  - ${one.dependency!.vulnerability} (${one.properties.canyon.severity}): ${one.properties.canyon.title} · fixed in ${one.dependency!.fixed}`),
    ].join("\n"))).toBe(true);
    expect(copied).toContain("suggested fix:\n  Upgrade lodash to 4.18.0 (fixes all 5) and commit package-lock.json.\n\ndo not:\n");
    expect(copied).toContain("Then run: canyon check");
  });

  it("approves one dependency's blocking findings together, from its card", async () => {
    const ALL = "POST /api/v1/gate-runs/gr_8f2a/findings/approve";
    const sent = server({ ...reads(() => page.status), [RUN]: { body: { run: { ...lodash.run.run, approvable: true, id: run.id, package: run.package } } }, [ALL]: { body: { approvals: [] } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("article", { name: HEAD }));
    fireEvent.click(card.getByRole("button", { name: "Approve all 5 for lodash 4.17.4" }));
    expect(card.getByText("Approves these 5 blocking findings of lodash 4.17.4, found by trivy:")).toBeDefined();
    expect(card.getByText("After that date the problems come back in the checklist.")).toBeDefined();
    fireEvent.change(card.getByLabelText("Reason"), { target: { value: lodash.approve_findings.reason } });
    fireEvent.change(card.getByLabelText("Approval expires"), { target: { value: lodash.approve_findings.expires_at.slice(0, 10) } });
    fireEvent.click(card.getByRole("button", { name: "Approve all 5" }));
    await waitFor(() => expect(screen.queryByLabelText("Reason")).toBeNull());
    expect(sent().filter((request) => request.startsWith("POST"))).toEqual([`${ALL} ${JSON.stringify(lodash.approve_findings)}`]);
    expect(sent().slice(sent().findIndex((request) => request.startsWith("POST")) + 1)).toContain(RUN); // the rows then say approved
  });

  it("keeps an approved row in its card, and offers no group once fewer than two are left", async () => {
    const [one, two] = lodashBlocking as [Finding, Finding];
    let its: RunRead = { ...lodash.run.run, approvable: true, id: run.id, package: run.package, findings: [one, two] };
    const SINGLE = `POST /api/v1/gate-runs/gr_8f2a/findings/${two.partialFingerprints["canyon/v1"]}/approve`;
    const approval = { reason: "fixture", expires_at: "2099-01-01T00:00:00Z", approved_by: { kind: "person" as const, subject: "dana", display: "Dana" } };
    server({ ...reads(() => page.status), [RUN]: () => ({ body: { run: its } }), [SINGLE]: () => { its = { ...its, findings: [one, { ...two, approval }] }; return { body: {} }; } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("article", { name: "lodash 4.17.4: 2 known vulnerabilities" }));
    expect(card.getByRole("button", { name: "Approve all 2 for lodash 4.17.4" })).toBeDefined();
    const row = within(card.getAllByRole("listitem")[1]!);
    fireEvent.click(row.getByRole("button", { name: "Approve with a reason" }));
    expect(card.queryByRole("button", { name: /^Approve all/ })).toBeNull(); // one form at a time
    fireEvent.change(card.getByLabelText("Reason"), { target: { value: "r" } });
    fireEvent.click(card.getByRole("button", { name: "Approve" }));
    await waitFor(() => expect(card.queryByLabelText("Reason")).toBeNull());
    expect(within(card.getAllByRole("listitem")[1]!).getByText(/approved by Dana until/)).toBeDefined(); // still in the card
    expect(card.queryByRole("button", { name: /^Approve all/ })).toBeNull();
    expect(within(card.getAllByRole("listitem")[0]!).getByRole("button", { name: "Approve with a reason" })).toBeDefined();
    expect(screen.getByRole("heading", { name: /^What blocks/ }).textContent).toMatch(/1$/); // what still blocks, not the approved row
  });

  it("says how many a version fixes when some name no fix, and draws an advisory dependency's entry as its own advisory card", async () => {
    const unfixed = { ...lodashBlocking[1]!, dependency: { ...lodashBlocking[1]!.dependency!, fixed: undefined } };
    const advisory = lodash.run.run.findings.find((one) => one.dependency !== undefined && !blocks(one))!;
    const its = { ...lodash.run.run, approvable: true, id: run.id, package: run.package, findings: [lodashBlocking[0]!, unfixed, advisory] };
    server({ ...reads(() => page.status), [RUN]: { body: { run: its } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("article", { name: "lodash 4.17.4: 2 known vulnerabilities" }));
    expect(card.getByText("Upgrade lodash to 4.18.0 (fixes 1 of 2; 1 with no fixed version yet) and commit package-lock.json.")).toBeDefined();
    expect(card.getAllByRole("listitem")[1]!.textContent).toContain("· no fixed version yet");
    fireEvent.click(screen.getByText("Doesn't block", { exact: false }));
    const own = within(screen.getByRole("article", { name: "lodash 4.17.4: 1 known vulnerability" })); // its entry: advisory CVEs never join blocking ones
    expect(own.getByText("advisory · does not block")).toBeDefined();
    expect(own.getByText("package-lock.json:17 · lodash 4.17.4")).toBeDefined();
    expect(own.getByText(`· ${advisory.properties.canyon.severity[0]!.toUpperCase()}${advisory.properties.canyon.severity.slice(1)} · fixed in ${advisory.dependency!.fixed}`, { exact: false })).toBeDefined();
    expect(own.queryByText(/Installed Version/)).toBeNull();
    expect(own.getByText(`Upgrade lodash to ${advisory.dependency!.fixed_in} and commit package-lock.json.`)).toBeDefined(); // its version, as its card names it
  });

  it("draws a lone blocking finding of a dependency as a card of one, and never groups a finding that names none", async () => {
    const misconfig = lodash.run.run.findings.find((one) => one.ruleId.startsWith("iac."))!;
    const its = { ...lodash.run.run, approvable: true, id: run.id, package: run.package, findings: [lodashBlocking[0]!, { ...misconfig, properties: { ...misconfig.properties, canyon: { ...misconfig.properties.canyon, blocks: true } } }] };
    server({ ...reads(() => page.status), [RUN]: { body: { run: its } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("article", { name: "lodash 4.17.4: 1 known vulnerability" }));
    expect(card.getByText("Upgrade lodash to 4.18.0 and commit package-lock.json.")).toBeDefined();
    expect(screen.getByRole("article", { name: cardName(misconfig) })).toBeDefined();
    expect(screen.getAllByRole("button", { name: "Approve with a reason" })).toHaveLength(2);
    expect(screen.queryByRole("button", { name: /^Approve all/ })).toBeNull();
  });

  it("approve renders the server's 400 and asks for nothing after it", async () => {
    const blocking = { ...run, findings: findings.map((one, at) => (at < 2 ? { ...one, properties: { ...one.properties, canyon: { ...one.properties.canyon, blocks: true } } } : one)) };
    const refused = { status: 400, body: { error: "invalid_expires_at", message: "expires_at is at most 90 days from now" } };
    const sent = server({ ...reads(() => page.status), [RUN]: { body: { run: blocking } }, [APPROVE(findings[0] as Finding)]: refused });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await approveOn(findings[0] as Finding));
    override(page.approve.reason, page.approve.expires_at.slice(0, 10));
    expect(await screen.findByText(refused.body.message)).toBeDefined();
    expect(sent().filter((request) => request.includes("/approve")).length).toBe(1);
  });

  it("approves once per click, trimmed, and the card then says who approved it", async () => {
    let at = run;
    const sent = server({ ...reads(() => page.status), [RUN]: () => ({ body: { run: at } }), [APPROVE(first)]: () => { at = approved.run_approved.run; return { body: page.approved }; } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await approveOn(first));
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: `  ${page.approve.reason}  ` } });
    fireEvent.change(screen.getByLabelText("Approval expires"), { target: { value: page.approve.expires_at.slice(0, 10) } });
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Approve" })); fireEvent.click(screen.getByRole("button", { name: "Approve" })); });
    expect(sent().filter((request) => request.includes("/approve"))).toEqual([`${APPROVE(first)} ${JSON.stringify(page.approve)}`]);
    // Read the card each time: the run read after the approval draws it anew, and the one found before it is gone.
    const card = () => within(screen.getByRole("article", { name: cardName(first) }));
    await waitFor(() => expect(card().getByText(/^approved by /)).toBeDefined());
    expect(card().queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  it("the approve form starts with no reason and the expiry 30 days out", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-09-25T12:00:00Z")); // 30 days on: 2026-10-25
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, findings } } }, [APPROVE(first)]: { body: page.approved } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await approveOn(first));
    expect((screen.getByLabelText("Reason") as HTMLTextAreaElement).value).toBe("");
    expect((screen.getByLabelText("Approval expires") as HTMLInputElement).value).toBe("2026-10-25");
  });

  it("a new review dims the findings of the one before it", async () => {
    vi.useFakeTimers();
    const start = reviewing({ ...run, id: "gr_new", state: "running", blocking: 0, warnings: 0, findings: [] });
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Review").getAllByRole("article").length).toBeGreaterThan(0));
    start();
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(region("Where it stands").getByRole("heading").textContent).toBe("Reviewing v1.4"));
    expect(region("Review").getAllByRole("article").map((card) => card.getAttribute("aria-label"))).toEqual(shown.map(cardName));
    expect(screen.getByRole("region", { name: "Review" }).getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("region", { name: "Where it stands" }).closest(".opacity-45")).toBeNull();
  });

  it("a gate that blocks again takes the confirm away with it", async () => {
    vi.useFakeTimers();
    let receipt = open.receipt;
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      if (url.includes("/gate-runs/")) return Promise.resolve(answer({ run }));
      if (url.endsWith("/status")) return Promise.resolve(answer({ ...page.status, receipt }));
      return projectOr(url);
    }));
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await vi.waitFor(() => screen.getByRole("button", { name: "Promote v1.3 to production" })));
    expect(screen.getByText(S.promoteConfirm)).toBeDefined();
    // Each poll's answer is drawn inside act, never by a waitFor whose checks move the fake clock: on a
    // loaded machine fewer checks fit in its second, and the hold never ran out (#2189).
    receipt = blocked.receipt;
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(screen.queryByText(S.promoteConfirm)).toBeNull();
    receipt = open.receipt;
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    await act(async () => { await vi.advanceTimersByTimeAsync(UNLOCK_MS); });
    expect(screen.getByRole("button", { name: "Promote v1.3 to production" }).hasAttribute("disabled")).toBe(false);
    expect(screen.queryByText(S.promoteConfirm)).toBeNull();
  });

  it("the verdict of a review the page has left is never shown as current", async () => {
    vi.useFakeTimers();
    let last = page.status.last_run;
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      if (url.includes("/gate-runs/gr_new")) return new Promise<Response>(() => {}); // still in flight
      if (url.includes("/gate-runs/")) return Promise.resolve(answer({ run: { ...run, state: "clean", blocking: 0, findings: [] } }));
      if (url.endsWith("/status")) return Promise.resolve(answer({ ...page.status, last_run: last }));
      return projectOr(url);
    }));
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Where it stands").getByText("v1.4 passed review")).toBeDefined());
    last = "gr_new";
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(region("Where it stands").queryByText("v1.4 passed review")).toBeNull());
    expect(screen.getByRole("region", { name: "Review" }).getAttribute("aria-busy")).toBe("true");
  });

  it("the findings kept while a new review goes are named by the version they belong to", async () => {
    vi.useFakeTimers();
    const start = reviewing({ ...run, id: "gr_new", package: "pkg_new", state: "running", blocking: 0, warnings: 0, findings: [] });
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Review").getByRole("heading", { name: /^What blocks v1\.4/ })).toBeDefined());
    start();
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(screen.getByRole("region", { name: "Review" }).getAttribute("aria-busy")).toBe("true"));
    expect(region("Review").getByRole("heading", { name: /^What blocks v1\.4/ })).toBeDefined();
  });

  it("the new review's findings replace the old ones when it lands", async () => {
    vi.useFakeTimers();
    const start = reviewing({ ...run, id: "gr_new", state: "clean", blocking: 0, warnings: 0, findings: [] });
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Review").getAllByRole("article").length).toBeGreaterThan(0));
    start();
    await vi.advanceTimersByTimeAsync(2000);
    await vi.waitFor(() => expect(region("Where it stands").getByText("v1.4 passed review")).toBeDefined());
    expect(region("Review").queryAllByRole("article")).toHaveLength(0);
    expect(region("Review").getByText("nothing blocking · no advisories")).toBeDefined(); // an empty review says so, never reads as missing
    expect(screen.getByRole("region", { name: "Review" }).getAttribute("aria-busy")).toBe("false");
  });

  // A clean review with a finding that blocks production only never reads "nothing blocking" (#2723): its heading says
  // what blocks where, the advisories after.
  it("says on a clean review's heading what blocks production, never nothing blocking", async () => {
    const advisory = findings.filter((one) => !blocks(one));
    const placed: RunRead = { ...run, state: "clean", blocking: 0, findings: advisory.map((one, at) => (at === 0 ? { ...one, blocks_production: true as const } : one)) };
    server({ ...reads(() => open), [RUN]: { body: { run: placed } } });
    render(<App id="claims-portal" auth="oidc" />);
    const others = new Set(advisory.slice(1).map((one) => one.entry)).size;
    await screen.findByRole("region", { name: "Review" });
    // Beside what blocks production, "no advisories" is no news: the count follows only when there are some.
    expect(await region("Review").findByText(others === 0 ? "nothing blocking preview · 1 blocking production" : `nothing blocking preview · 1 blocking production · ${others} ${others === 1 ? "advisory" : "advisories"}`)).toBeDefined();
    expect(region("Review").queryByText(/^nothing blocking · /)).toBeNull();
    cleanup();
    // Approved, the finding blocks production no more: the heading counts it with the advisories.
    const approval = { reason: "a test fixture", expires_at: "2099-01-01T00:00:00Z", approved_by: { kind: "person" as const, subject: "dana", display: "Dana" } };
    const cleared: RunRead = { ...placed, findings: placed.findings.map((one, at) => (at === 0 ? { ...one, approval } : one)) };
    server({ ...reads(() => open), [RUN]: { body: { run: cleared } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Review" });
    const all = new Set(advisory.map((one) => one.entry)).size;
    expect(await region("Review").findByText(`nothing blocking · ${all} advisories`)).toBeDefined();
    expect(region("Review").queryByText(/blocking production/)).toBeNull();
    cleanup();
    // One that blocks production alone says so, and no "no advisories" after it (#2754's review).
    server({ ...reads(() => open), [RUN]: { body: { run: { ...placed, findings: [placed.findings[0]!] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Review" });
    expect(await region("Review").findByText("nothing blocking preview · 1 blocking production")).toBeDefined();
  });

  it("stops reading a review once it has ended, and reads production's once", async () => {
    vi.useFakeTimers();
    let spare = 0;
    const fetched = vi.fn((url: string) => {
      if (url.includes("/gate-runs/")) return Promise.resolve(answer({ run: url.includes(page.promotion.promotion.gate_run) ? { ...run, id: page.promotion.promotion.gate_run, state: "clean" } : run }));
      if (url.endsWith("/status")) return Promise.resolve(answer({ ...promoted, spare: (spare += 1) }));
      return projectOr(url);
    });
    vi.stubGlobal("fetch", fetched);
    render(<App id="claims-portal" auth="oidc" />);
    await vi.waitFor(() => expect(region("Review").getAllByRole("article").length).toBeGreaterThan(0));
    const reads = () => fetched.mock.calls.map(([url]) => url).filter((url) => url.includes("/gate-runs/"));
    // Every run the page shows is read once, whenever its read starts: on a slow runner production's
    // lands after the cards, so the count is taken after the polls, not before.
    await vi.advanceTimersByTimeAsync(6000);
    expect(reads().length).toBeGreaterThan(1);
    expect(reads()).toEqual([...new Set(reads())]);
  });

  it("a finding whose column runs past its line still renders", async () => {
    const wide = { ...run, findings: [{ ...first, locations: [{ physicalLocation: { ...first.locations[0].physicalLocation, region: { ...first.locations[0].physicalLocation.region, startColumn: 999_999_999 } } }] }] };
    server({ ...reads(() => page.status), [RUN]: { body: { run: wide } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("article", { name: cardName(first) })).toBeDefined();
  });

  it("findings render from the run read", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    // Said once, in the headline: the verdict and "nothing was built" are no longer repeated under Review.
    expect(region("Where it stands").getByText(new RegExp(S.nothingBuilt))).toBeDefined();
    expect(region("Review").queryByText(S.verdictBlocked)).toBeNull();
    expect(region("Review").getByText("apps/web/src/lib/payments.ts:14")).toBeDefined();
    // What blocks first, under its own heading, then the rest: the run's order within each (the test cluster audit's sqli-blocked case).
    expect(region("Review").getAllByRole("article").map((card) => card.getAttribute("aria-label"))).toEqual(shown.map(cardName));
    expect([/^What blocks v1\.4/, /^Doesn't block/].map((name) => region("Review").getByRole("heading", { name }).tagName)).toEqual(["H3", "H3"]);
    expect(region("Review").getByRole("heading", { name: /^Doesn't block/ }).closest("details")?.hasAttribute("open")).toBe(false); // beside what blocks, behind a click
    // What doesn't block, by policy with its count, so many advisories of one kind read as one group.
    expect(region("Review").getAllByRole("heading", { level: 4 }).map((one) => one.textContent)).toEqual(["Every route needs sign-in · 2", "No unbounded refetch on user input · 1"]);
    for (const one of findings) {
      const card = within(screen.getByRole("article", { name: cardName(one) }));
      expect(card.queryByText("blocks") !== null).toBe(blocks(one));
    }
  });

  it("a failed run never reads clean", async () => {
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "error", blocking: 0, findings: [] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4's review didn't finish" });
    expect(region("Where it stands").getByText(/^Canyon itself failed\./)).toBeDefined();
    expect(region("Where it stands").queryByText(/passed review/)).toBeNull();
  });

  // The lifecycle colours its own lines for a terminal (the test cluster's "[31;1mERROR"): the page shows the words alone.
  it("shows a failed build's lines without the terminal's colour codes", async () => {
    const log = (line: string, id: number): TimelineRow => ({ id, kind: "build.log", data: { line } });
    const lines = ["\u001b[31;1mERROR: \u001b[0mfailed to build: exit status 1", "the build step exited 1"];
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "build", blocking: 0, findings: [] } } },
      [TIMELINE]: { body: { rows: lines.map(log), next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    const trail = within((await screen.findByText("The build's last lines")).closest("details") as HTMLElement);
    expect(trail.getByText("ERROR: failed to build: exit status 1")).toBeDefined();
  });

  // A build that failed after the review passed (the test cluster audit's build-fails case): the review is Passed,
  // the build is its own step that failed, in words, and the page shows what broke, the failing line first.
  it("says a build failed after the review passed, and shows what broke", async () => {
    const log = (line: string, id: number): TimelineRow => ({ id, kind: "build.log", data: { line } });
    const lines = ["> next build", "src/app/total/page.tsx(4,9): error TS2322: Type 'string' is not assignable to type 'number'.", "build failed: the package did not compile"];
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "build", blocking: 0, findings: [] } } },
      [TIMELINE]: { body: { rows: lines.map(log), next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 passed review, then didn't build" });
    const table = screen.getByRole("table", { name: "Versions" });
    const row = within(table).getByRole("rowheader", { name: /^v1\.4/ }).closest("[role=row]") as HTMLElement;
    expect([within(row).getByRole("cell", { name: "Review" }).textContent, within(row).getByRole("cell", { name: "Preview" }).textContent]).toEqual(["Passed", "Build failed"]);
    // The cause leads, in the headline; the lines sit behind a click, the failing one marked.
    expect(await region("Where it stands").findByText(lines[2]!)).toBeDefined();
    const trail = screen.getByText("The build's last lines").closest("details") as HTMLElement;
    expect(trail.hasAttribute("open")).toBe(true); // a build's last line is the executor's: its lines are open
    expect([...trail.querySelectorAll("pre > span")].map((one) => one.textContent)).toEqual(lines);
    expect(trail.querySelector("mark")?.textContent).toBe(lines[2]);
    // Copy for your agent: the headline, the failing line and the last lines, as a finding's card copies its own.
    const writeText = vi.fn(() => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    fireEvent.click(region("Where it stands").getByRole("button", { name: "Copy for your agent" }));
    expect(await region("Where it stands").findByText("Copied")).toBeDefined();
    expect(writeText).toHaveBeenCalledWith([
      "Fix why Canyon couldn't deploy claims-portal:",
      "v1.4 passed review, then didn't build",
      "Its last line says why. Fix it in the code, then deploy again. v1.3 keeps running in preview.",
      "",
      "failing line:",
      `  ${lines[2]}`,
      "",
      "last lines:",
      ...lines.map((one) => `  ${one}`),
      "",
      "Then run: canyon deploy",
      "It reviews and builds the code again. If it still fails after two full rounds of fixes,",
      "stop and show the user these lines. Do not keep looping.",
      "",
    ].join("\n"));
  });

  // A start that failed shows the failing line and the 20 before it, however many pages the log runs to;
  // a row of another kind is not a line.
  // A detect after the review passed: Review reads Passed, the step says the host can't build this kind
  // of app, and the verdict names what it builds. No log block: a detect's lines are each buildpack declining.
  it("says the host can't build this kind of app, its review passed", async () => {
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "detect", builds: ["paketo-buildpacks/nodejs"], blocking: 0, findings: [] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 couldn't be built on this host" });
    expect(region("Where it stands").getByText(/Canyon builds Node\.js apps\./)).toBeDefined();
    const row = within(screen.getByRole("table", { name: "Versions" })).getByRole("rowheader", { name: /^v1\.4/ }).closest("[role=row]") as HTMLElement;
    expect([within(row).getByRole("cell", { name: "Review" }).textContent, within(row).getByRole("cell", { name: "Preview" }).textContent]).toEqual(["Passed", "Can't build this kind of app"]);
    expect(screen.queryByRole("region", { name: /Why the/ })).toBeNull();
  });

  // A stock starter missing its one file: the page names the file, says why, and shows what goes in it,
  // each example whole (contracts/routes.md, What a starter is missing). A detect with one is never
  // "can't build this kind of app": the host builds it once the file is there.
  it("names the file a starter is missing and what goes in it", async () => {
    const procfile = { file: "Procfile", why: "The build gave the app no start command, so Python started with nothing to run and exited.",
      examples: [{ for: "FastAPI, with uvicorn in requirements.txt (fastapi[standard] brings it)", text: "web: uvicorn main:app --host 0.0.0.0 --port $PORT\n" }, { for: "Flask, with gunicorn in requirements.txt", text: "web: gunicorn app:app --bind 0.0.0.0:$PORT\n" }] };
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "start", blocking: 0, findings: [], fix: procfile } } }, [TIMELINE]: { body: { rows: [], next: null } } });
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    const missing = within(await screen.findByRole("region", { name: "The app is missing a Procfile" }));
    expect(missing.getAllByText(/./).map((one) => one.textContent)).toEqual(["The app is missing a Procfile", procfile.why,
      "Add a file named Procfile at the app's root, holding the lines for this app.", "For FastAPI, with uvicorn in requirements.txt (fastapi[standard] brings it):", "web: uvicorn main:app --host 0.0.0.0 --port $PORT\n",
      "For Flask, with gunicorn in requirements.txt:", "web: gunicorn app:app --bind 0.0.0.0:$PORT\n"]);
    unmount();
    const toml = { file: "project.toml", why: "No buildpack recognised the package: a static site needs a project.toml at its root that has nginx serve its files.",
      examples: [{ for: "a static site served from its root", text: "[_]\nschema-version = \"0.2\"\n" }] };
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "detect", builds: [], blocking: 0, findings: [], fix: toml } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 is missing a project.toml" });
    expect(screen.queryByText(/can't build this kind of app/)).toBeNull();
    expect(within(screen.getByRole("region", { name: "The app is missing a project.toml" })).getByText("For a static site served from its root:")).toBeTruthy();
  });

  it("says the app didn't stay up, with the last 21 lines of a long log", async () => {
    const log = (line: string, id: number): TimelineRow => ({ id, kind: "build.log", data: { line } });
    const first = Array.from({ length: 150 }, (_, at) => log(`build line ${at}`, at));
    // The app's own last lines as Node writes a crash: the error, its stack, then Node's version; the executor's reason last.
    const crash = ["Error: Cannot find module './config.json'", "    at Object.<anonymous> (/workspace/server.js:1:16)", "", "Node.js v22.23.2", "the app did not stay up: its container restarted (Error, exit 1)"];
    const second = [...Array.from({ length: 25 }, (_, at) => log(`app line ${at}`, 150 + at)), ...crash.map((line, at) => log(line, 175 + at)), { id: 999, kind: "run.state", data: { state: "failed" } }];
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "start", blocking: 0, findings: [] } } },
      [TIMELINE]: { body: { rows: first, next: 149 } }, "GET /api/v1/gate-runs/gr_8f2a/timeline?after=149": { body: { rows: second, next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 built, then stopped right after it started" });
    // No line of the app's is named its cause: Node ends every crash with its version, under the error (#2352).
    // The headline says the executor's reason, the last line; the last 21 lines are open, none marked.
    const where = region("Where it stands");
    expect(await where.findByText(crash[4]!)).toBeDefined();
    expect(where.queryByText(crash[3]!)).toBeNull();
    expect(where.getByText(/Its last lines say why\./)).toBeDefined();
    const trail = screen.getByText("The app's last lines").closest("details") as HTMLElement;
    expect(trail.hasAttribute("open")).toBe(true);
    const lines = [...trail.querySelectorAll("pre > span")].map((one) => one.textContent);
    expect([lines.length, lines[0], lines.includes(crash[0]!), lines.at(-1), trail.querySelector("mark")]).toEqual([21, "app line 9", true, crash[4], null]);
    // Copy for your agent names no failing line either: the last lines, whole.
    const writeText = vi.fn((_: string) => Promise.resolve());
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    fireEvent.click(where.getByRole("button", { name: "Copy for your agent" }));
    expect(await where.findByText("Copied")).toBeDefined();
    const copied = writeText.mock.calls[0]![0];
    expect([copied.includes("failing line:"), copied.includes("last lines:\n  app line 9\n"), copied.includes(`  ${crash[0]}\n`)]).toEqual([false, true, true]);
  });

  // A person's approval is never a pass: a version approved past its blocking findings whose build failed
  // reads Approved at review, and the verdict says so.
  it("says an approved version's build failed without calling it passed", async () => {
    const blocking = findings.find((one) => one.properties.canyon.blocks)!;
    const approval = { reason: "accepted for the demo", expires_at: "2026-11-01T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } };
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "failed", failure_reason: "build", blocking: 1, findings: [{ ...blocking, approval }] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 was approved past its review, then didn't build" });
    const row = within(screen.getByRole("table", { name: "Versions" })).getByRole("rowheader", { name: /^v1\.4/ }).closest("[role=row]") as HTMLElement;
    expect(within(row).getByRole("cell", { name: "Review" }).textContent).toBe("Approved past review");
  });

  // Every blocking finding of the newest version approved, and the version running in preview: its review
  // reads as the headline says it, never Blocked beside a version that runs.
  it("says a version approved past every blocking finding was approved past review", async () => {
    const blocking = findings.filter((one) => one.properties.canyon.blocks);
    const approval = { reason: "accepted for the demo", expires_at: "2026-11-01T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } };
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, findings: run.findings.map((one) => (blocking.includes(one) ? { ...one, approval } : one)) } } } });
    render(<App id="claims-portal" auth="oidc" />);
    const table = await screen.findByRole("table", { name: "Versions" });
    const row = within(table).getByRole("rowheader", { name: /^v1\.4/ }).closest("[role=row]") as HTMLElement;
    await waitFor(() => expect(within(row).getByRole("cell", { name: "Review" }).textContent).toBe("Approved past review"));
  });

  // History is four tabs, none open and none read until one is chosen; each says its rows in plain words.
  // Who can open it and the history are closed until asked for (the app-page redesign: "Advisories, access and
  // history follow, closed"); an owner with access requests waiting finds the card open, since they wait on them.
  it("keeps the history closed until asked for", async () => {
    server(reads(() => page.status));
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    const history = await screen.findByRole("region", { name: "History" });
    expect(history.querySelector("details")?.hasAttribute("open")).toBe(false);
    openHistory();
    expect(history.querySelector("details")?.hasAttribute("open")).toBe(true);
    unmount();
  });

  it("opens history a tab at a time, reads nothing until one is chosen, and says none of the machine's words", async () => {
    const sent = server({
      ...reads(() => page.status),
      [PACKAGES]: { body: pages[0] },
      "GET /api/v1/projects/claims-portal/gate-runs": { body: golden<{ gate_runs: unknown[] }>("app-page/claims-portal.json").gate_runs[0] },
      "GET /api/v1/projects/claims-portal/secrets": { body: secrets },
      "GET /api/v1/audit?project=claims-portal": { body: page.audit[0] },
    });
    const { container } = render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    const tabs = within(screen.getByRole("tablist", { name: "Versions, reviews and activity" })).getAllByRole("tab");
    expect(tabs.map((one) => [one.textContent, one.getAttribute("aria-selected")])).toEqual([["Versions", "false"], ["Reviews", "false"], ["Activity", "false"]]);
    // The run that cleared what preview runs is read for its review; nothing a tab holds is (the secrets are the Secrets section's).
    expect(sent().filter((request) => request !== RUN && request !== TIMELINE && request !== "GET /api/v1/gate-runs/gr_6c12" && request.includes("gate-runs")).concat(sent().filter((request) => /packages|audit/.test(request)))).toEqual([]);
    const tab = (name: string) => { fireEvent.click(screen.getByRole("tab", { name })); return within(screen.getByRole("tabpanel", { name })); };
    expect((await tab("Versions").findAllByText("v1.4")).length).toBeGreaterThan(0);
    expect((await tab("Reviews").findAllByText("v1.4")).length).toBeGreaterThan(0);
    // The trail's rows are sentences in the plain words strings.ts gives each action, never the route.
    const trail = tab("Activity");
    await trail.findByText("started a deploy of v1.4");
    expect(trail.getAllByRole("cell").map((cell) => cell.textContent)).toEqual(expect.arrayContaining(["reviewed v1.4: blocked, 1 blocking, 3 advisory", "uploaded v1.4", "deployed v1.3 to preview"]));
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i);
  });

  // A failed run's reason reads in words, never the reason's own name (contracts/routes.md, *A failure reason in words*).
  it("says why a run failed in words on the Reviews tab", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/gate-runs": { body: golden<{ gate_runs: unknown[] }>("app-page/claims-portal.json").gate_runs[1] } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const row = (await within(screen.getByRole("tabpanel", { name: "Reviews" })).findByText("v1.2")).closest("tr")!;
    expect(within(row).getAllByRole("cell")[1]!.textContent).toBe("Failed · the package did not build");
  });

  it("says a run no buildpack detected failed in words on the Reviews tab", async () => {
    const runs = golden<{ gate_runs: { runs: { failure_reason?: string }[] }[] }>("app-page/claims-portal.json").gate_runs[1]!;
    const detect = { ...runs, runs: runs.runs.map((row) => (row.failure_reason === "build" ? { ...row, failure_reason: "detect" } : row)) };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/gate-runs": { body: detect } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const row = (await within(screen.getByRole("tabpanel", { name: "Reviews" })).findByText("v1.2")).closest("tr")!;
    expect(within(row).getAllByRole("cell")[1]!.textContent).toBe("Failed · no buildpack on this host recognised the package");
  });

  // A pull request's runs name it; its newest one that passed offers the deploy to preview (#1947), an
  // older or blocked one does not, and a refusal reads in the host's words.
  it("offers a passing pull request's newest run the deploy to preview", async () => {
    const at = "2026-10-09T10:00:00Z";
    const row = (id: string, pkg: string, state: string, number: number) => ({ id, package: pkg, state, blocking: 0, warnings: 0, started_by: null, started_at: at, ended_at: at, pull_request: { number, branch: number === 12 ? "fix-login" : "\u202Eevil" } });
    const sent = server({
      ...reads(() => page.status),
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [row("gr_new", "pkg_pr12", "clean", 12), row("gr_old", "pkg_pr12", "clean", 12), row("gr_b", "pkg_pr13", "blocked", 13)], next: null } },
      "POST /api/v1/projects/claims-portal/deploys": { status: 409, body: { error: "review_not_passed", message: "Canyon's review of this pull request hasn't passed" } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const reviews = within(screen.getByRole("tabpanel", { name: "Reviews" }));
    expect((await reviews.findAllByText("PR #12 (fix-login)")).length).toBe(2);
    const buttons = reviews.getAllByRole("button", { name: S.prDeploy });
    expect(buttons.length).toBe(1);
    expect(buttons[0]!.closest("tr")!.textContent).toContain("PR #12 (fix-login)");
    expect(reviews.getByText("PR #13 (\u202Eevil)").tagName).toBe("BDI"); // a branch's own direction never reorders the line
    fireEvent.click(buttons[0]!);
    expect(await reviews.findByRole("status")).toHaveProperty("textContent", "Canyon's review of this pull request hasn't passed");
    expect(sent()).toContain('POST /api/v1/projects/claims-portal/deploys {"package":"pkg_pr12"}');
  });

  // A blocked pull request whose every blocking finding is approved passes as a deploy would (#1955):
  // its row reads approved past review and offers the deploy; one blocked and unapproved offers nothing.
  it("offers the deploy on a pull request approved past its review", async () => {
    const at = "2026-10-09T10:00:00Z";
    const row = (id: string, pkg: string, number: number, approved?: true) => ({ id, package: pkg, state: "blocked", blocking: 1, warnings: 0, approved, started_by: null, started_at: at, ended_at: at, pull_request: { number, branch: `b${number}` } });
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [row("gr_a", "pkg_pr14", 14, true), row("gr_b", "pkg_pr15", 15)], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const reviews = within(screen.getByRole("tabpanel", { name: "Reviews" }));
    const approved = (await reviews.findByText("PR #14 (b14)")).closest("tr")!;
    const blocked = reviews.getByText("PR #15 (b15)").closest("tr")!;
    expect(within(approved).getAllByRole("cell")[1]!.textContent).toBe("Approved past review");
    expect(within(approved).getByRole("button", { name: S.prDeploy })).toBeDefined();
    expect(within(blocked).getAllByRole("cell")[1]!.textContent).toBe("Blocked");
    expect(within(blocked).queryByRole("button", { name: S.prDeploy })).toBeNull();
  });

  // The deploy is followed to its end and the page read again, a second click sending nothing (#1947).
  it("follows a pull request's deploy to preview to its end", async () => {
    const at = "2026-10-09T10:00:00Z";
    let posts = 0;
    let polled = 0;
    server({
      ...reads(() => page.status),
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [{ id: "gr_new", package: "pkg_pr12", state: "clean", blocking: 0, warnings: 0, started_by: null, started_at: at, ended_at: at, pull_request: { number: 12, branch: "fix-login" } }], next: null } },
      "POST /api/v1/projects/claims-portal/deploys": () => { posts++; return { status: 202, body: { run: { id: "gr_dep", state: "pending" } } }; },
      "GET /api/v1/gate-runs/gr_dep": () => { polled++; return { body: { run: { id: "gr_dep", package: "pkg_pr12", state: polled < 2 ? "running" : "clean", blocking: 0, warnings: 0, findings: [] } } }; },
    });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const reviews = within(screen.getByRole("tabpanel", { name: "Reviews" }));
    const deploy = await reviews.findByRole("button", { name: S.prDeploy });
    fireEvent.click(deploy);
    fireEvent.click(deploy);
    await waitFor(() => expect(reviews.getByRole("status").textContent).toBe("PR #12 (fix-login) is on preview."), { timeout: 5000 });
    expect(posts).toBe(1);
  });

  // While preview runs a pull request's version, its card names the pull request and nothing promotes it (#1947).
  it("says preview runs a pull request, and offers no promotion of it", async () => {
    const carol = { kind: "person" as const, subject: "sub-carol", display: "Carol" };
    const pr = { ...page.status, envs: { ...page.status.envs, preview: { ...(page.status.envs.preview as Env), package: "pkg_pr12", label: undefined, deployed_by: carol, pull_request: { number: 12, branch: "fix-login" } } }, receipt: { ...page.status.receipt, blocked_by: [] } };
    server(reads(() => pr));
    render(<App id="claims-portal" auth="oidc" />);
    const preview = await screen.findByRole("region", { name: "Preview" });
    expect(await within(preview).findByText("PR #12 (fix-login)")).toBeDefined();
    // the versions on their way: the pull request is what runs in preview, and it waits on a merge, never a promotion
    const row = within(screen.getByRole("table", { name: "Versions" })).getByText("PR #12 (fix-login)").closest("[role=row]") as HTMLElement;
    expect(within(row).getByRole("cell", { name: "Preview" }).textContent).toContain("Running");
    expect(within(row).getByRole("cell", { name: "Production" }).textContent).toContain("Merge it first");
    expect(screen.queryByText("Ready to promote")).toBeNull();
    // the go-live panel is the pull request's: its name, why nothing promotes it, and none of main's checks
    const aside = screen.getByRole("complementary", { name: "Promote" });
    expect(within(aside).getByRole("heading").textContent).toBe("Go-live checks for PR #12 (fix-login)");
    expect(within(aside).getByRole("button", { name: "Promote PR #12 (fix-login) to production" }).hasAttribute("disabled")).toBe(true);
    expect(within(aside).getByText(S.prPreview)).toBeDefined();
    expect(aside.textContent).not.toMatch(/passed review/);
    expect(document.body.textContent).not.toMatch(/pr12/); // never the package's id, the headline's line included
  });

  // A check's run reviewed a tree that is no version of the app: its row says so, never a package's id.
  it("lists a check's review as from a check", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/gate-runs": { body: golden<{ gate_runs: unknown[] }>("app-page/claims-portal.json").gate_runs[1] } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const reviews = within(screen.getByRole("tabpanel", { name: "Reviews" }));
    await reviews.findByText("v1.2");
    expect(reviews.getAllByText(/^(v1\.2|2b17|From a check)$/).map((cell) => cell.textContent)).toEqual(["v1.2", "From a check"]);
  });

  it("Load more walks the cursor and lists no version twice", async () => {
    const cursor = pages[0]?.next ?? "";
    const sent = server({
      ...reads(() => page.status),
      [PACKAGES]: { body: pages[0] },
      [`${PACKAGES}?cursor=${cursor}`]: { body: { ...pages[1], packages: [...(pages[0]?.packages.slice(-1) ?? []), ...(pages[1]?.packages ?? [])] } },
      // each version's review, read alongside its page (#2696)
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [], next: null } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
    fireEvent.click(await screen.findByRole("button", { name: "Load more" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Load more" })).toBeNull());
    const versions = screen.getByRole("tabpanel", { name: "Versions" }); // the pipeline names v1.4 too
    expect(within(versions).getAllByText(/^v1\.[0-9]$/).map((cell) => cell.textContent)).toEqual(["v1.4", "v1.3", "v1.2"]);
    expect(sent()).toContain(`${PACKAGES}?cursor=${cursor}`);
  });

  it("never reviewed is said once", async () => {
    server({ ...reads(() => ({ ...page.status, last_run: null })), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, gate_state: "never_reviewed" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    expect(screen.getAllByText(S.neverReviewed)).toHaveLength(1);
  });

  // Run review: offered only when there is a package to look at; the run it starts is the next
  // status read's last_run, which the page's own poll then reads.
  const REVIEW = "POST /api/v1/projects/claims-portal/gate-runs";
  const unreviewed = (stage: AppRead["latest"]["stage"]) => ({ body: { project: { ...project, gate_state: "never_reviewed", latest: { ...project.latest, stage } } } });

  it("run review starts a run, and the page reads it", async () => {
    let started = false;
    const sent = server({
      ...reads(() => ({ ...page.status, last_run: started ? "gr_new" : null })),
      "GET /api/v1/projects/claims-portal": unreviewed("awaiting_review"),
      [REVIEW]: () => { started = true; return { status: 202, body: { run: { id: "gr_new", state: "pending" } } }; },
      "GET /api/v1/gate-runs/gr_new": { body: { run: { ...run, id: "gr_new", state: "running" } } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Review v1.4" }));
    await waitFor(() => expect(sent()).toContain("GET /api/v1/gate-runs/gr_new"));
    expect(sent().filter((one) => one === REVIEW)).toHaveLength(1);
  });

  // A version blocked only by checks that couldn't finish is reviewed again by deploying it again, which
  // reviews it first: the headline's one button, with approving as the quiet second.
  it("reviews a version again when only Canyon's checks blocked it", async () => {
    const unread = { ...first, ruleId: "gate.unreadable" };
    const sent = server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, findings: [unread] } } },
      "POST /api/v1/projects/claims-portal/deploys": { status: 202, body: { run: { id: "gr_again" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "v1.4 is blocked: 1 of Canyon's checks couldn't finish" });
    expect(within(screen.getByRole("region", { name: "Where it stands" })).getAllByRole("button").map((one) => one.textContent)).toEqual(["Hold production", "Approve with a reason", "Review v1.4 again"]); // an admin holds production from here too (#102)
    fireEvent.click(screen.getByRole("button", { name: "Review v1.4 again" }));
    await waitFor(() => expect(sent()).toContain(`POST /api/v1/projects/claims-portal/deploys {"package":"pkg_9e40"}`));
  });

  // A review going offers to cancel it, and nothing else.
  // A review going says where it stands, live: how many checks are done and which waits its turn, in the
  // headline, the pipeline and its own list (the redesign, "Reviewing, live"; contracts/events.md, progress).
  it("shows a review going, live", async () => {
    const going = [
      { id: 1, ts: "2026-10-05T12:00:00Z", kind: "progress", data: { step: "scan", tool: "gitleaks", status: "done", duration_ms: 3000 } },
      { id: 2, ts: "2026-10-05T12:00:01Z", kind: "progress", data: { step: "scan", tool: "trivy", status: "queued", position: 1 } },
    ];
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "running", blocking: 0, findings: [] } } }, [TIMELINE]: { body: { rows: going, next: 2 } },
      "GET /api/v1/gate-runs/gr_8f2a/timeline?after=2": { body: { rows: [], next: 2 } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await within(await screen.findByRole("region", { name: "Where it stands" })).findByText(/^1 of 2 checks done\. trivy is waiting its turn on this host\./)).toBeDefined();
    expect(await screen.findByRole("region", { name: "Progress" })).toBeDefined();
    const row = within(screen.getByRole("table", { name: "Versions" })).getByRole("rowheader", { name: /^v1\.4/ }).closest("[role=row]") as HTMLElement;
    expect(within(row).getByRole("cell", { name: "Review" }).textContent).toMatch(/1 of 2$/);
  });

  it("a review going can be cancelled from the headline", async () => {
    const sent = server({ ...reads(() => page.status), [RUN]: { body: { run: { ...run, state: "running", blocking: 0, findings: [] } } },
      "POST /api/v1/gate-runs/gr_8f2a/cancel": { body: { run: { id: "gr_8f2a", state: "failed" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Cancel review" }));
    await waitFor(() => expect(sent()).toContain("POST /api/v1/gate-runs/gr_8f2a/cancel"));
  });

  it("nothing uploaded offers no review", async () => {
    server({ ...reads(() => ({ ...page.status, last_run: null })), "GET /api/v1/projects/claims-portal": unreviewed("created") });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("heading", { name: "Nothing uploaded yet" });
    expect(screen.queryByRole("button", { name: /^Review/ })).toBeNull();
  });

  it("a review already running says the server's sentence", async () => {
    server({
      ...reads(() => ({ ...page.status, last_run: null })),
      "GET /api/v1/projects/claims-portal": unreviewed("awaiting_review"),
      [REVIEW]: { status: 409, body: { error: "run_in_progress", message: "a run of this project is running", run: { id: "gr_other", state: "running" } } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "Review v1.4" }));
    expect(await screen.findByText("a run of this project is running")).toBeDefined();
  });

  // Approved is read off each finding, never from the checklist: with two blocking findings and one
  // approval, the checklist still says blocked, and only the approved card may say approved.
  const card = async (title: string) => within(await within(await screen.findByRole("region", { name: "Review" })).findByRole("article", { name: title }));
  const [other, , , signed] = approved.run_approved.run.findings as [Finding, Finding, Finding, Finding]; // the reviewer's three, then gitleaks' key
  it("an approved finding says who approved it, until when and why, where its blocks chip was", async () => {
    const { run: at } = approved.run_approved;
    server({ ...reads(() => approved.status_approved), [RUN]: { body: { run: at } } });
    render(<App id="claims-portal" auth="oidc" />);
    const approval = signed.approval as NonNullable<Finding["approval"]>;
    const one = await card(cardName(signed));
    expect(one.getByText(`approved by ${approval.approved_by.display} until ${new Date(approval.expires_at).toLocaleDateString("en-GB", { day: "numeric", month: "short", timeZone: "UTC" })}`)).toBeTruthy();
    expect(one.getByText(approval.reason)).toBeTruthy();
    expect(one.queryByText("blocks")).toBeNull();
    expect((await card(other.properties.canyon.title)).getByText("advisory · does not block")).toBeTruthy();
  });

  // A finding advisory before preview that the run read says blocks before production: its card says both, never
  // "does not block" above the go-live refusal (the test cluster's case-sqli-blocked), and offers its approval (#2206).
  it("a finding that blocks before production says so on its card, and is approved there", async () => {
    server({ ...reads(() => approved.status_approved), [RUN]: { body: approved.run_placed } });
    render(<App id="claims-portal" auth="oidc" />);
    const one = await card(other.properties.canyon.title);
    expect(one.getByText("Doesn't stop preview. Stops production until fixed or approved.")).toBeTruthy();
    expect(one.queryByText("advisory · does not block")).toBeNull();
    expect(one.getByRole("button", { name: "Approve with a reason" })).toBeTruthy();
  });

  // The chip is the run read's answer, never the receipt's rule names: a finding of a rule placed blocking
  // before production that the read does not mark — a medium one, a Dockerfile check's — does not block (#2206).
  it("a finding of a placed rule the read does not mark reads advisory", async () => {
    const { run: at } = approved.run_approved;
    const placed = { id: "auth.unauthenticated-route", name: other.properties.canyon.policy_name!, blocks: true, passed: true, why: "v1.4 passed review: no blocking finding" };
    const status: Status = { ...approved.status_approved, receipt: { ...approved.status_approved.receipt, rules: [...approved.status_approved.receipt.rules, placed] } };
    server({ ...reads(() => status), [RUN]: { body: { run: at } } });
    render(<App id="claims-portal" auth="oidc" />);
    const one = await card(other.properties.canyon.title);
    expect(one.getByText("advisory · does not block")).toBeTruthy();
    expect(one.queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  // Someone the run read does not let approve — the team, or the admin or responder who started it — reads who
  // approves in place of the button, on the card and nowhere in the headline (#2204).
  it("says who approves to a reader who may not", async () => {
    const { approvable: _, ...at } = run;
    const lodash = golden<{ run: { run: RunRead } }>("app-page/birthday-list-dependency-approval.json").run.run.findings.filter(blocks);
    server({ ...reads(() => page.status), [RUN]: { body: { run: { ...at, findings: [...at.findings, ...lodash.map((one, i) => ({ ...one, entry: at.findings.length + i }))] } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await card(cardName(first));
    expect(screen.getAllByText(S.approvers)).toHaveLength(1); // once, above the findings: never on each card, a dependency's included
    expect(screen.queryByRole("button", { name: "Approve with a reason" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve with a reason: go to the findings" })).toBeNull();
  });

  it("with two blocking findings and one approved, only the approved one says so", async () => {
    const both: Finding = { ...other, properties: { canyon: { ...other.properties.canyon, blocks: true } } };
    const at: RunRead = { ...approved.run_approved.run, blocking: 2, findings: approved.run_approved.run.findings.map((one) => (one === other ? both : one)) };
    const status: Status = { ...approved.status_approved, receipt: { ...approved.status_approved.receipt, rules: approved.status_approved.receipt.rules.map((rule) => (rule.id === "prod.clean-gate" ? { ...rule, why: "gr_8f2a ended blocked: 1 blocking finding(s), no approval" } : rule)) } };
    server({ ...reads(() => status), [RUN]: { body: { run: at } } });
    render(<App id="claims-portal" auth="oidc" />);
    const one = await card(cardName(signed));
    expect(one.getByText(/^approved by /)).toBeTruthy();
    expect(one.queryByText("blocks")).toBeNull();
    const two = await card(cardName(both));
    expect(two.getByText("blocks")).toBeTruthy();
    expect(two.queryByText(/approved/)).toBeNull();
  });
});

// History's tabs, and what they say when a list is empty or cannot be read.
describe("the app's history", () => {
  it("says what each empty list would hold", async () => {
    server({
      ...reads(() => page.status),
      [PACKAGES]: { body: { packages: [], next: null } },
      "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [], next: null } },
      "GET /api/v1/projects/claims-portal/secrets": { body: { secrets: [] } },
      "GET /api/v1/audit?project=claims-portal": { body: { events: [], next: null } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    for (const [name, text] of [["Versions", "Nothing uploaded yet. Upload with canyon deploy."], ["Reviews", "Not reviewed yet."], ["Activity", "Nothing recorded for this app yet."]] as const) {
      fireEvent.click(screen.getByRole("tab", { name }));
      expect(await within(screen.getByRole("tabpanel", { name })).findByText(text)).toBeDefined();
    }
  });

  // A list that cannot be read says so in plain words, never the route, and can be asked again.
  it("says a list couldn't be read without the route's words, and tries again", async () => {
    let fail = true;
    server({ ...reads(() => page.status), [PACKAGES]: () => (fail ? { status: 500, body: null } : { body: pages[0] }), "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
    const panel = within(screen.getByRole("tabpanel", { name: "Versions" }));
    expect(await panel.findByText("This list couldn't be read.")).toBeDefined();
    expect(screen.getByRole("tabpanel", { name: "Versions" }).textContent).not.toMatch(/packages|\/api\/v1/);
    fail = false;
    fireEvent.click(panel.getByRole("button", { name: "Try again" }));
    expect((await panel.findAllByText(/^v1\.[0-9]$/)).length).toBeGreaterThan(0);
  });

  // One Tab stop for the tabs; the arrow keys move between them and choose.
  it("moves between tabs with the arrow keys", async () => {
    server({ ...reads(() => page.status), [PACKAGES]: { body: { packages: [], next: null } }, "GET /api/v1/projects/claims-portal/gate-runs": { body: { runs: [], next: null } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    const versions = screen.getByRole("tab", { name: "Versions" });
    expect(screen.getAllByRole("tab").map((one) => one.getAttribute("tabindex"))).toEqual(["0", "-1", "-1"]);
    fireEvent.click(versions);
    fireEvent.keyDown(versions, { key: "ArrowRight" });
    expect(screen.getByRole("tab", { name: "Reviews" }).getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "Reviews" }));
  });
});

// A hold of a production that ran nothing (the hold's `version` null) is said as held wherever the page reads the
// hold: the banner, production's card and Promote's reason alike, never stopped in one and held in another (#2696).
describe("a hold of a production that ran nothing", () => {
  it("says held, never stopped, in the banner and production's card, and Promote's reason in brief, to the app's owner", async () => {
    const by = { kind: "person" as const, subject: "rhea@meridian.example", display: "Rhea Okafor" };
    const hold = { by, at: "2026-10-11T09:40:00Z", reason: "Exports claimant SSNs unauthenticated (INC-2231)" };
    const status: Status = { ...page.status, hold: { ...hold, version: null }, envs: { preview: page.status.envs.preview }, receipt: { ...page.status.receipt, held: hold } };
    const owner: AppRead = { ...project, access: { ...project.access, level: "owner" } };
    server({ ...reads(() => status), "GET /api/v1/projects/claims-portal": { body: { project: owner } } });
    const { container } = render(<App id="claims-portal" auth="oidc" />);
    const held = `Production was held by Rhea Okafor on 2026-10-11: ${hold.reason}. It stays held until a responder or an admin lifts it.`;
    await waitFor(() => expect(screen.getAllByText(held).length).toBe(1)); // the banner alone: production's card says since when
    expect(within(screen.getByRole("region", { name: "Production" })).getByText("Held · since 2026-10-11")).toBeDefined();
    expect(within(screen.getByRole("complementary", { name: "Promote" })).getByText(S.heldNoPromote)).toBeDefined(); // Promote's reason, in brief (#2723)
    expect(container.textContent).not.toMatch(/Production was stopped by/);
  });
});

// The tour's story (#2696): a version that passed review after the one before it was blocked says so and
// links to that review, the run view names the version it shows, History links each review to its run and
// marks a blocked version, and a host with sign-in off names who acted as nobody signed in.
describe("the blocked-then-fixed story", () => {
  const RUNS = "GET /api/v1/projects/claims-portal/gate-runs";
  const POLICIES = "GET /api/v1/policies";
  const runPages = golden<{ gate_runs: { runs: unknown[]; next: string | null }[] }>("app-page/claims-portal.json").gate_runs;
  const anonymous = { kind: "person" as const, subject: "anonymous", display: "" }; // contracts/routes.md: person / anonymous, with no display
  const at = "2026-11-07T21:59:06Z";
  type Row = { id: string; package: string; state: "clean" | "blocked"; blocking: number; warnings: number; started_by: typeof anonymous; started_at: string; ended_at: string; label?: string; check?: true; approved?: true; pull_request?: { number: number; branch: string } };
  const listed = (id: string, pkg: string, state: "clean" | "blocked", blocking: number, more: Partial<Row> = {}): Row => ({ id, package: pkg, state, blocking, warnings: 0, started_by: anonymous, started_at: at, ended_at: at, ...more });
  const uploaded = (id: string): Package => ({ id, commit: "c0ffee1", source: { kind: "upload", ref: "", actor: "anonymous" }, created_at: at });
  // The golden's blocking finding as an imported tool's run read names it: gitleaks' rule under the policy secrets.client-bundle.
  const blocker: Finding = { ...first, tool: "sarif/gitleaks" };
  const RULE = { id: "secrets.client-bundle", name: "No secrets in the code", state: "blocking" };
  const fixed: AppRead = { ...project, gate_state: "clean", latest: { package: "pkg_d295", stage: "passed", since: at } };
  const clean: RunRead = { id: "gr_d295", package: "pkg_d295", state: "clean", blocking: 0, warnings: 0, findings: [] };
  const stoppedBy = (...findings: Finding[]): RunRead => ({ id: "gr_ef49", package: "pkg_ef49", state: "blocked", blocking: findings.length, warnings: 0, findings });
  // The tour, newest first: d295's deploy and its check, ef49 blocked twice, and the sample 1f4f before both.
  const tour = [listed("gr_d295", "pkg_d295", "clean", 0), listed("gr_d2c", "pkg_d295", "clean", 0), listed("gr_ef49", "pkg_ef49", "blocked", 1), listed("gr_ef48", "pkg_ef49", "blocked", 1), listed("gr_1f4f", "pkg_1f4f", "clean", 0)];
  type Story = { newest?: RunRead; runs?: Row[]; before?: RunRead; packages?: string[]; earlier?: Earlier[] | undefined; policies?: Answer; pinned?: Record<string, Answer> };
  // `earlier` given as undefined is a host that answers none (a host from before earlier[]), never the default.
  const story = (change: Story = {}) => storyOf({ ...change, earlier: "earlier" in change ? change.earlier : [] });
  const storyOf = ({ newest = clean, runs = tour, before = stoppedBy(blocker), packages = ["pkg_d295", "pkg_ef49", "pkg_1f4f"], earlier, policies = { body: { policies: [RULE] } }, pinned = {} }: Story) => ({
    "GET /api/v1/projects/claims-portal": { body: { project: fixed } },
    "GET /api/v1/projects/claims-portal/status": { body: { ...page.status, last_run: newest.id, earlier } },
    [`GET /api/v1/gate-runs/${newest.id}`]: { body: { run: newest } },
    [`GET /api/v1/gate-runs/${newest.id}/timeline?after=0`]: { body: { rows: [skipped], next: null } },
    "GET /api/v1/gate-runs/gr_ef49": { body: { run: before } },
    "GET /api/v1/gate-runs/gr_ef49/timeline?after=0": { body: { rows: [skipped], next: null } },
    [RUNS]: { body: { runs, next: null } },
    [PACKAGES]: { body: { packages: packages.map(uploaded), next: null } },
    [POLICIES]: policies,
    ...pinned,
  });
  const reviewOn = async () => within(await screen.findByRole("region", { name: "Review" }));

  // The tour's own case: ef49, the first deploy, was blocked; d295, uploaded just after it, passed without its finding.
  it("tells the tour's story: the version before was blocked, and this one fixed it", async () => {
    server(story());
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, fixed in d295")).toBeDefined();
    expect(review.getByText("ef49 had 1 blocking finding, and it isn't in d295's review.")).toBeDefined();
    expect(review.getByRole("link", { name: "See ef49's review →" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_ef49");
  });

  it("says none of several blocking findings is in the newest review", async () => {
    const second: Finding = { ...blocker, entry: 9, partialFingerprints: { "canyon/v1": "f".repeat(64) } };
    server(story({ before: stoppedBy(blocker, second), runs: tour.map((row) => (row.package === "pkg_ef49" ? { ...row, blocking: 2 } : row)) }));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, fixed in d295")).toBeDefined();
    expect(review.getByText("ef49 had 2 blocking findings, and d295's review has none of them.")).toBeDefined();
  });

  // An imported tool's result proves nothing by its absence unless Canyon runs that tool on every review: gitleaks, and a
  // tool whose sarif.<tool> rule it runs (`run`, and not Off). An attached report is the person's to attach again (#2696).
  it.each([
    ["an attached report's, which the newer version has none of", "deps.known-vuln.snyk.SNYK-JS-LODASH-1", "sarif/snyk", { id: "sarif.snyk", name: "Findings from snyk", state: "advisory" }],
    ["a Canyon-run tool's that Canyon doesn't run", "sql.string-concat.opengrep.sql-injection", "sarif/opengrep", { id: "sarif.opengrep", name: "Findings from opengrep", state: "advisory" }],
    ["a Canyon-run tool's whose rule is Off", "sql.string-concat.opengrep.sql-injection", "sarif/opengrep", { id: "sarif.opengrep", name: "Findings from opengrep", state: "off", run: true }],
  ])("never says fixed when the earlier blocking finding is %s", async (_, ruleId, tool, toolRule) => {
    const policies = { body: { policies: [RULE, { id: ruleId.split(`.${tool.slice("sarif/".length)}.`)[0], name: "its policy", state: "blocking" }, toolRule] } };
    server(story({ before: stoppedBy({ ...blocker, ruleId, tool }), policies }));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, passed in d295")).toBeDefined();
    expect(review.getByText("ef49 had 1 blocking finding. d295 passed review.")).toBeDefined();
  });

  it("says fixed when the earlier blocking finding is a tool's Canyon runs on every review", async () => {
    const policies = { body: { policies: [{ id: "sql.string-concat", name: "No hand-built SQL from user input", state: "blocking" }, { id: "sarif.opengrep", name: "Findings from opengrep", state: "advisory", run: true }] } };
    server(story({ before: stoppedBy({ ...blocker, ruleId: "sql.string-concat.opengrep.sql-injection", tool: "sarif/opengrep" }), policies }));
    render(<App id="claims-portal" auth="off" />);
    expect(await (await reviewOn()).findByText("Blocked earlier, fixed in d295")).toBeDefined();
  });

  // The earlier blocking finding is still in the newest review, advisory now: it passed, and nothing says it was fixed.
  it("says passed, never fixed, while the earlier blocking finding is still in the newest review", async () => {
    const still: RunRead = { ...clean, warnings: 1, findings: [{ ...blocker, properties: { canyon: { ...blocker.properties.canyon, blocks: false } } }] };
    server(story({ newest: still }));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, passed in d295")).toBeDefined();
    expect(review.getByText("ef49 had 1 blocking finding. d295 passed review.")).toBeDefined();
    expect(review.queryByText(/fixed in/)).toBeNull();
  });

  // A review that names something it did not check is never called passed, nor a fix (D5: never Passed).
  it("says nothing blocking in what was checked, never fixed or passed, when the newest review left something unchecked", async () => {
    server(story({ newest: { ...clean, not_checked: true } }));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, nothing blocking in d295")).toBeDefined();
    expect(review.getByText("ef49 had 1 blocking finding. d295 had nothing blocking in what was checked.")).toBeDefined();
    expect(review.queryByText(/fixed in|passed in/)).toBeNull();
  });

  // A finding whose absence proves nothing: Canyon's could-not-check, the AI review's, a custom rule's (by the model or by code).
  it.each([
    ["Canyon could not check", "gate.unreadable", "canyon/scanners"],
    ["an AI-review rule", "ai.sql-string-concat", "canyon-review"],
    ["the AI review's", "auth.unauthenticated-route", "canyon-review"],
    ["a custom rule checked by code", "custom.no-raw-sql", "custom-rules"],
  ])("never says fixed when an earlier blocking finding is %s", async (_, ruleId, tool) => {
    const policies = { body: { policies: [RULE, { id: ruleId, name: ruleId, state: "blocking" }] } };
    server(story({ before: stoppedBy({ ...blocker, ruleId, tool }), policies }));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByText("Blocked earlier, passed in d295")).toBeDefined();
    expect(review.getByText("ef49 had 1 blocking finding. d295 passed review.")).toBeDefined();
  });

  // Fixed only while its rule still blocks, as the dashboard reads the rule; a rule it can't read is never fixed.
  it.each([
    ["unreadable", { status: 500, body: null }],
    ["no longer blocking", { body: { policies: [{ ...RULE, state: "advisory" }] } }],
    ["not in the read", { body: { policies: [] } }],
  ])("never says fixed when the earlier blocking rule's state is %s", async (_, policies) => {
    server(story({ policies }));
    render(<App id="claims-portal" auth="off" />);
    expect(await (await reviewOn()).findByText("Blocked earlier, passed in d295")).toBeDefined();
  });

  // The app's own addition makes a rule stricter than the org's: the rule blocks this app, so its fix is a fix.
  it("reads the rule's state for this app, the stricter of the org's and the app's own", async () => {
    const policies = { body: { policies: [{ ...RULE, state: "advisory", additions: [{ project: "claims-portal", state: "blocking", set_by: anonymous, set_at: at }] }] } };
    server(story({ policies }));
    render(<App id="claims-portal" auth="off" />);
    expect(await (await reviewOn()).findByText("Blocked earlier, fixed in d295")).toBeDefined();
  });

  // A check's run or a pull request's between the two is another tree: the story is still ef49's.
  it.each([
    ["a check", listed("gr_chk", "pkg_c0de", "blocked", 1, { check: true })],
    ["a pull request's", listed("gr_pr7", "pkg_pr7", "blocked", 1, { pull_request: { number: 7, branch: "fix-sql" } })],
  ])("skips %s run between the two versions", async (_, between) => {
    const sent = server(story({ runs: [tour[0]!, tour[1]!, between, ...tour.slice(2)] }));
    render(<App id="claims-portal" auth="off" />);
    expect(await (await reviewOn()).findByText("Blocked earlier, fixed in d295")).toBeDefined();
    expect(sent()).not.toContain(`GET /api/v1/gate-runs/${between.id}`);
  });

  // Fail closed: a story only of the version uploaded just before this one, one production never ran — a rollback
  // review only reviews a version production ran, and the runs list does not say which runs are one.
  it.each([
    ["the version before passed its review", { runs: tour.map((row) => (row.package === "pkg_ef49" ? { ...row, state: "clean" as const, blocking: 0 } : row)) }],
    ["the review before was approved past", { runs: tour.map((row) => (row.id === "gr_ef49" ? { ...row, approved: true as const } : row)) }],
    ["the review before names no blocking finding", { runs: tour.map((row) => (row.id === "gr_ef49" ? { ...row, blocking: 0 } : row)) }],
    ["the run before is not of the version uploaded just before", { packages: ["pkg_d295", "pkg_1f4f", "pkg_ef49"] }],
    ["production ran the version before", { earlier: [{ package: "pkg_ef49", version: "ef49", image_digest: "sha256:0", deployed_at: at, promoted_by: null, kept: true }] }],
    ["the host says nothing of what production ran", { earlier: undefined }],
  ])("says nothing of an earlier block when %s", async (_, change) => {
    const sent = server(story(change));
    render(<App id="claims-portal" auth="off" />);
    const review = await reviewOn();
    await waitFor(() => expect(sent()).toContain(PACKAGES));
    await flush();
    expect(review.queryByText(/Blocked earlier/)).toBeNull();
    expect(sent()).not.toContain("GET /api/v1/gate-runs/gr_ef49");
  });

  // The link from the story, or from the audit's view →, lands on the blocked run: its heading is that run's version.
  it("heads a pinned run's review with the version it shows", async () => {
    server(story());
    render(<App id="claims-portal" run="gr_ef49" auth="off" />);
    const review = await reviewOn();
    expect(await review.findByRole("heading", { level: 3, name: /^What blocks ef49/ })).toBeDefined();
    expect(review.getByRole("heading", { level: 2 }).textContent).toBe("Review of ef49");
  });

  // Any version is named by its label, the newest or not; a check by what it is; a pull request's by its pull request.
  it.each([
    ["by its label", listed("gr_v12", "pkg_5c02", "blocked", 1, { label: "v1.2" }), "Review of v1.2"],
    ["as a check", listed("gr_chk", "pkg_c0de", "blocked", 1, { check: true }), "Review of a check"],
    ["by its pull request", listed("gr_pr7", "pkg_pr7", "blocked", 1, { pull_request: { number: 7, branch: "fix-sql" } }), "Review of PR #7 (fix-sql)"],
  ])("names a pinned run's version %s", async (_, row, heading) => {
    const read: RunRead = { ...stoppedBy(blocker), id: row.id, package: row.package };
    server(story({ runs: [...tour, row], pinned: { [`GET /api/v1/gate-runs/${row.id}`]: { body: { run: read } }, [`GET /api/v1/gate-runs/${row.id}/timeline?after=0`]: { body: { rows: [skipped], next: null } } } }));
    render(<App id="claims-portal" run={row.id} auth="off" />);
    const review = await reviewOn();
    await waitFor(() => expect(review.getByRole("heading", { level: 2 }).textContent).toBe(heading));
  });

  // A review that found nothing has its heading alone: what Canyon checked is its content, under it,
  // never an empty column beside the go-live checks.
  it("puts what Canyon checked under a review that found nothing", async () => {
    server(story());
    render(<App id="claims-portal" auth="off" />);
    expect(await (await reviewOn()).findByRole("region", { name: "What Canyon checked" })).toBeDefined();
  });

  it("links each review in History to its run", async () => {
    server({ ...reads(() => page.status), [RUNS]: { body: runPages[0] } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    const reviews = within(screen.getByRole("tabpanel", { name: "Reviews" }));
    expect((await reviews.findByRole("link", { name: "v1.4" })).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_8f2a");
    expect(reviews.getByRole("link", { name: "v1.3" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_6c12");
  });

  const versionsTab = async (routes: Record<string, Answer>) => {
    const sent = server({ ...reads(() => page.status), [PACKAGES]: { body: pages[0] }, ...routes });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(BLOCKED);
    openHistory();
    fireEvent.click(screen.getByRole("tab", { name: "Versions" }));
    const versions = within(screen.getByRole("tabpanel", { name: "Versions" }));
    await versions.findByText("v1.3");
    const review = (name: string) => within(versions.getByText(name).closest("tr")!).getAllByRole("cell")[1]!;
    return { sent, versions, review };
  };

  it("marks a blocked version as blocked in History's versions, with a link to its review", async () => {
    const { versions, review } = await versionsTab({ [RUNS]: { body: runPages[0] } });
    const blocked = await versions.findByRole("link", { name: "Blocked" });
    expect(blocked.getAttribute("href")).toBe("#/apps/claims-portal?run=gr_8f2a");
    expect(review("v1.4").textContent).toBe("Blocked");
    expect(versions.getByRole("link", { name: "Passed" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_6c12");
    expect(review("v1.3").textContent).toBe("Passed");
  });

  // Approved past every blocking finding is what the deploy went on by: never marked Blocked.
  it("never marks a version approved past its review as blocked", async () => {
    const runs = { ...(runPages[0] as { runs: Row[] }), runs: (runPages[0] as { runs: Row[] }).runs.map((row) => (row.id === "gr_8f2a" ? { ...row, approved: true as const } : row)) };
    const { versions } = await versionsTab({ [RUNS]: { body: runs } });
    const link = await versions.findByRole("link", { name: "Approved past review" });
    expect(link.className).not.toMatch(/critical/);
    expect(versions.queryByRole("link", { name: "Blocked" })).toBeNull();
  });

  // The runs are read on while a version of the page has none and they reach past no version's upload, and no further.
  it("reads the runs on until each version of the page has its review", async () => {
    const v13 = (runPages[0] as { runs: Row[] }).runs.find((row) => row.id === "gr_6c12")!;
    const v14 = (runPages[0] as { runs: Row[] }).runs.find((row) => row.id === "gr_8f2a")!;
    const { sent, review } = await versionsTab({ [RUNS]: { body: { runs: [v14], next: "more" } }, [`${RUNS}?cursor=more`]: { body: { runs: [v13], next: null } } });
    await waitFor(() => expect(review("v1.3").textContent).toBe("Passed"));
    expect(sent()).toContain(`${RUNS}?cursor=more`);
  });

  it("reads no further once the runs reach past the oldest version's upload", async () => {
    const v14 = (runPages[0] as { runs: Row[] }).runs.find((row) => row.id === "gr_8f2a")!;
    const older = listed("gr_old", "pkg_0ld0", "clean", 0, { started_at: "2026-08-01T00:00:00Z" }); // before v1.3 was uploaded
    const { sent, review } = await versionsTab({ [RUNS]: { body: { runs: [v14, older], next: "more" } } });
    expect(review("v1.4").textContent).toBe("Blocked");
    expect(review("v1.3").textContent).toBe("—");
    expect(sent()).not.toContain(`${RUNS}?cursor=more`);
  });

  // The versions are the list; their reviews only add to it: a runs read that fails leaves the list whole, each review a dash.
  it("still lists the versions when their reviews can't be read", async () => {
    const { versions, review } = await versionsTab({ [RUNS]: { status: 500, body: null } });
    expect(review("v1.4").textContent).toBe("—");
    expect(review("v1.3").textContent).toBe("—");
    expect(versions.queryByText("This list couldn't be read.")).toBeNull();
  });

  // On a host with sign-in off every caller is its anonymous admin (design/PERMISSIONS.md): nobody signed in,
  // so the page says that, and never shows the subject as if it were a name.
  it("names who acted on a host with sign-in off as someone not signed in, never anonymous", async () => {
    const preview = { ...(page.status.envs.preview as Env), deployed_by: anonymous };
    const prod = { ...page.promotion.promotion, deployed_by: anonymous, promoted_by: anonymous };
    server(reads(() => ({ ...page.status, envs: { preview, prod } })));
    const { container } = render(<App id="claims-portal" auth="off" />);
    await waitFor(() => expect(container.textContent).toContain("promoted by someone not signed in"));
    expect(container.textContent).toContain("· someone not signed in ·");
    expect(container.textContent).not.toMatch(/anonymous/);
  });
});

// Renaming is managing the app (design/specs/screens.md, Renaming it; #1909): only the display name
// changes, and the page reads the app again so the title follows.
describe("renaming the app", () => {
  const RENAME = "PUT /api/v1/projects/claims-portal/name";
  const askRename = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Rename…" }));
    return within(screen.getByRole("region", { name: "Rename this app" })).getByLabelText("Name") as HTMLInputElement;
  };

  it("saves the new name, says the id stays, and the title follows", async () => {
    let current = project;
    const sent = server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": () => ({ body: { project: current } }),
      [RENAME]: () => { current = { ...project, name: "Motor claims" }; return { body: { project: current } }; } });
    render(<App id="claims-portal" auth="oidc" />);
    const field = await askRename();
    expect(field.value).toBe(project.name);
    expect(screen.getByText("Only the name changes. Its address, canyon.toml and the CLI keep claims-portal.")).toBeDefined();
    fireEvent.change(field, { target: { value: "Motor claims" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.getByRole("heading", { level: 1 }).textContent).toContain("Motor claims"));
    expect(sent()).toContain(`${RENAME} {"name":"Motor claims"}`);
    expect(screen.queryByRole("region", { name: "Rename this app" })).toBeNull();
  });

  it("Enter saves, and a refusal stays in the panel with the name as typed", async () => {
    const sent = server({ ...reads(() => page.status), [RENAME]: { status: 400, body: { error: "invalid_name", message: "an app's name is one line of plain text, at most 100 characters" } } });
    render(<App id="claims-portal" auth="oidc" />);
    const field = await askRename();
    fireEvent.change(field, { target: { value: "   " } });
    fireEvent.keyDown(field, { key: "Enter" });
    expect(await screen.findByText("an app's name is one line of plain text, at most 100 characters")).toBeDefined();
    expect((within(screen.getByRole("region", { name: "Rename this app" })).getByLabelText("Name") as HTMLInputElement).value).toBe("   ");
    expect(sent()).toContain(`${RENAME} {"name":"   "}`);
  });

  it("Cancel closes it unsent, and Escape from the field it focuses does too", async () => {
    const sent = server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    await askRename();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("region", { name: "Rename this app" })).toBeNull();
    const field = await askRename();
    await waitFor(() => expect(document.activeElement).toBe(field));
    fireEvent.keyDown(document.activeElement as Element, { key: "Escape" });
    expect(screen.queryByRole("region", { name: "Rename this app" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "More actions" }));
    expect(sent().some((request) => request.startsWith("PUT"))).toBe(false);
  });

  it("offers Rename only to someone who manages the app", async () => {
    const contributor = { ...project, access: { level: "contributor" as const, everyone: false, people: 2, groups: 0 } };
    server({ ...reads(() => promoted), "GET /api/v1/projects/claims-portal": { body: { project: contributor } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    expect(screen.queryByRole("menuitem", { name: "Rename…" })).toBeNull();
  });
});

describe("taking an environment offline and deleting the app", () => {
  const OFFLINE = "DELETE /api/v1/projects/claims-portal/envs/prod";
  const DELETE = "DELETE /api/v1/projects/claims-portal";
  const CONFIRMED = `${DELETE}?confirm=claims-portal`;
  const resources = [{ name: "db", type: "postgres", env: "prod", state: "ready" }, { name: "reports", type: "postgres", env: "preview", state: "ready" }];
  const askOffline = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Take production offline" }));
  };
  const askDelete = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete app…" }));
    await screen.findByLabelText("Type claims-portal to enable this button.");
  };
  const type = (name: string) => fireEvent.change(screen.getByLabelText("Type claims-portal to enable this button."), { target: { value: name } });

  it("confirms inline, sends the undeploy, and production then shows nothing running", async () => {
    let status = promoted;
    const sent = server({ ...reads(() => status), [OFFLINE]: () => { status = open; return { status: 204, body: null }; } });
    render(<App id="claims-portal" auth="oidc" />);
    await askOffline();
    expect(screen.getByText("Take claims-portal's production offline? The link stops working. Its databases, data and secrets are kept. Promote again to bring it back.")).toBeDefined();
    expect(sent()).not.toContain(OFFLINE);
    fireEvent.click(screen.getByRole("button", { name: "Take production offline" }));
    await waitFor(() => expect(screen.queryByRole("link", { name: "Open v1.3 in production" })).toBeNull());
    expect(sent()).toContain(OFFLINE);
  });

  it("Escape closes the question, sends nothing, and gives the keyboard back to ⋯", async () => {
    const sent = server(reads(() => promoted));
    render(<App id="claims-portal" auth="oidc" />);
    await askOffline();
    fireEvent.keyDown(screen.getByRole("button", { name: "Take production offline" }), { key: "Escape" });
    expect(screen.queryByRole("button", { name: "Take production offline" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "More actions" }));
    expect(sent().some((request) => request.startsWith("DELETE"))).toBe(false);
  });

  it("sends nothing when the question is cancelled", async () => {
    const sent = server(reads(() => promoted));
    render(<App id="claims-portal" auth="oidc" />);
    await askOffline();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("button", { name: "Take production offline" })).toBeNull();
    expect(sent().some((request) => request.startsWith("DELETE"))).toBe(false);
  });

  // A run already switching the environment stops it once it lands (contracts/routes.md, The brake): the
  // panel closes as for a 204, and the polled page shows the environment leaving once the run has stopped it.
  it("closes on 202 stopping, and the page shows preview leaving once the run stops it", async () => {
    let status = promoted;
    const PREVIEW = "DELETE /api/v1/projects/claims-portal/envs/preview";
    server({ ...reads(() => status), [PREVIEW]: () => { status = { ...promoted, envs: { prod: promoted.envs.prod } }; return { status: 202, body: { stopping: { run: { id: "gr_switch", state: "running" } } } }; } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Take preview offline" }));
    fireEvent.click(screen.getByRole("button", { name: "Take preview offline" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Take preview offline" })).toBeNull());
    await waitFor(() => expect(screen.queryByRole("link", { name: /in preview$/ })).toBeNull());
  });

  // Deleting the app is managing it, its owners' and admins': a contributor's ⋯ offers no Delete.
  it("offers Delete only to someone who manages the app", async () => {
    const contributor = { ...project, access: { level: "contributor" as const, everyone: false, people: 2, groups: 0 } };
    server({ ...reads(() => promoted), "GET /api/v1/projects/claims-portal": { body: { project: contributor } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    expect(screen.getByRole("menuitem", { name: "Take production offline" })).toBeDefined();
    expect(screen.queryByRole("menuitem", { name: "Delete app…" })).toBeNull();
  });

  it("an owner with no requests waiting is still offered Delete", async () => {
    const owner = { ...project, access: { level: "owner" as const, everyone: false, people: 1, groups: 0, requests: 0 } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: owner } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    expect(screen.getByRole("menuitem", { name: "Delete app…" })).toBeDefined();
  });

  it("Cancel closes the delete question, sends no delete, and gives the keyboard back", async () => {
    const sent = server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources: [] } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    expect(screen.getByText("This permanently deletes claims-portal: every version, its review history and its secrets. It can't be undone.")).toBeDefined();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText("Type claims-portal to enable this button.")));
    type("claims-portal");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByLabelText("Type claims-portal to enable this button.")).toBeNull();
    expect(sent()).not.toContain(CONFIRMED);
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "More actions" }));
  });

  it("a listing that fails offers no delete, and Cancel still closes it", async () => {
    server({ ...reads(() => page.status), [DELETE]: { status: 500, body: { message: "the list could not be read" } } });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete app…" }));
    expect(await screen.findByText("the list could not be read")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Delete claims-portal" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("the list could not be read")).toBeNull();
  });

  // Deleting is a question over the page (#102), which says where production is live, in the CLI's words, when it is;
  // and taking production offline names the other way down, stopping it.
  it("asks to delete over the page, saying production goes offline with it, and offline names Stop production", async () => {
    server({ ...reads(() => promoted), [DELETE]: { status: 409, body: { error: "confirm_required", resources } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    const ask = screen.getByRole("dialog", { name: "Delete claims-portal" });
    expect(ask.getAttribute("aria-modal")).toBe("true");
    expect(within(ask).getByText("Production is live at http://claims-portal.prod.localhost:7777, and goes offline with it.")).toBeDefined();
    expect(document.activeElement).toBe(within(ask).getByLabelText("Type claims-portal to enable this button."));
    fireEvent.keyDown(ask, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
    await askOffline();
    expect(screen.getByText(S.offlineOrStop)).toBeDefined();
  });

  it("lists what the delete destroys and enables it only on the exact name", async () => {
    const sent = server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    expect(screen.getByText("This permanently deletes claims-portal: the production Postgres database (db), the preview Postgres database (reports), its preview and production, every version, its review history and its secrets. It can't be undone.")).toBeDefined();
    const button = () => screen.getByRole("button", { name: "Delete claims-portal" }) as HTMLButtonElement;
    for (const near of ["", "claims", "Claims-portal", "claims-portal "]) {
      type(near);
      expect(button().disabled).toBe(true);
    }
    type("claims-portal");
    expect(button().disabled).toBe(false);
    expect(sent()).not.toContain(CONFIRMED);
  });

  // What the org's platform provided is listed as the platform's: the delete asks it to delete it (#2432).
  it("lists what the platform provided as the platform's to delete", async () => {
    server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources: [resources[1], { name: "files", type: "s3", env: "preview", state: "provided" }] } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    expect(screen.getByText("This permanently deletes claims-portal: the preview Postgres database (reports), the preview s3 (files) (your platform deletes it), its preview and production, every version, its review history and its secrets. It can't be undone.")).toBeDefined();
  });

  it("sends the confirmed delete and goes to the fleet when it is done", async () => {
    window.location.hash = "#/apps/claims-portal";
    const sent = server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources } }, [CONFIRMED]: { status: 204, body: null } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    type("claims-portal");
    fireEvent.click(screen.getByRole("button", { name: "Delete claims-portal" }));
    await waitFor(() => expect(window.location.hash).toBe("#/fleet"));
    expect(sent()).toContain(CONFIRMED);
    window.location.hash = "";
  });

  it("names what is still deleting and sends the same delete again", async () => {
    const sent = server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources } }, [CONFIRMED]: { status: 202, body: { pending: [resources[0]] } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    type("claims-portal");
    fireEvent.click(screen.getByRole("button", { name: "Delete claims-portal" }));
    expect(await screen.findByText("Still deleting: the production Postgres database (db).")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Try again" }));
    await waitFor(() => expect(sent().filter((request) => request === CONFIRMED)).toHaveLength(2));
  });

  it("a teardown failure says the server's sentence", async () => {
    const message = "the postgres db in prod could not be deleted: its data is kept, and deleting again resumes";
    server({ ...reads(() => page.status), [DELETE]: { status: 409, body: { error: "confirm_required", resources } }, [CONFIRMED]: { status: 502, body: { error: "teardown_failed", message } } });
    render(<App id="claims-portal" auth="oidc" />);
    await askDelete();
    type("claims-portal");
    fireEvent.click(screen.getByRole("button", { name: "Delete claims-portal" }));
    expect(await screen.findByText(message)).toBeDefined();
  });
});

// The app page's Outbound (design/PERMISSIONS.md, Outbound): an owner's, a contributor's and an admin's;
// a read-only member and a responder with no grant never read the app's records nor see the card.
describe("the app page's Outbound", () => {
  const egress = golden<{ project_read: object; setting_member: object }>("egress/claims-portal.json");
  // the records answered to anyone who asks, so the page alone decides who reads them
  const outbound = { "GET /api/v1/projects/claims-portal/egress": { body: egress.project_read }, "GET /api/v1/egress": { body: egress.setting_member } };
  const reader = (level: AppRead["access"]["level"]) => ({ "GET /api/v1/projects/claims-portal": { body: { project: { ...project, access: { ...project.access, level } } } } });
  for (const [who, level] of [["an owner", "owner"], ["a contributor", "contributor"], ["an admin with no grant", "none"]] as const) {
    it(`is drawn for ${who}`, async () => {
      server({ ...reads(() => page.status), ...outbound, ...reader(level) });
      render(<App id="claims-portal" auth="oidc" />);
      expect(within(await screen.findByRole("region", { name: "Outbound" })).getByRole("region", { name: "Preview outbound" })).toBeDefined();
    });
  }

  it("is neither read nor drawn for a read-only member", async () => {
    const sent = server({ ...outbound, "GET /api/v1/projects/claims-portal": { body: { project: { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "viewer" } } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(`${S.readOnly} ${S.readOnlyUse}`);
    await flush();
    expect(screen.queryByRole("region", { name: "Outbound" })).toBeNull();
    expect(sent().filter((request) => request.includes("egress"))).toEqual([]);
  });

  it("is neither read nor drawn for a responder with no grant", async () => {
    const row = { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "none" } };
    const sent = server({ ...reads(() => page.status), ...outbound, "GET /api/v1/projects/claims-portal": { body: { project: row } },
      "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(S.responderAccess);
    await flush();
    expect(screen.queryByRole("region", { name: "Outbound" })).toBeNull();
    expect(sent().filter((request) => request.includes("egress"))).toEqual([]);
  });
});

// The app page's System libraries (#2079; design/PATCHING.md §8, §9): per environment, the image read in its own words,
// when its packages were listed and against which vulnerability data, each counted package as a card that says where it
// is in a developer's words, never its class, the notes, and what Canyon's patches fixed. A contributor's and up.
describe("the app page's System libraries", () => {
  const platform = golden<{ image_read: ImageRead }>("platform/claims-portal.json").image_read;
  const patched = golden<{ image_read: ImageRead }>("patch/claims-portal.json").image_read;
  const IMAGE = "GET /api/v1/projects/claims-portal/envs/preview/image";
  const PROD_IMAGE = "GET /api/v1/projects/claims-portal/envs/prod/image";

  it("draws each environment's read for a contributor: the packages where they are, the notes and Canyon's fixes", async () => {
    server({ ...reads(() => promoted), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, access: { ...project.access, level: "contributor" } } } },
      [IMAGE]: { body: platform }, [PROD_IMAGE]: { body: { ...patched, env: "prod" } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: LIBRARIES.title }));
    const preview = within(card.getByRole("region", { name: "Preview system libraries" }));
    expect(preview.getByText(`· ${platform.why}`)).toBeDefined();
    expect(preview.getByText(/^Packages listed .* · checked against vulnerability data from /)).toBeDefined();
    const libssl = within(preview.getByRole("article", { name: "libssl3 3.0.13-0ubuntu3.4: 1 known vulnerability" }));
    expect(libssl.getByText(LIBRARIES.inOs)).toBeDefined();
    expect(libssl.getByText("CVE-2024-6119 · fixed in 3.0.13-0ubuntu3.5")).toBeDefined();
    expect(within(preview.getByRole("article", { name: "cross-spawn 7.0.3: 1 known vulnerability" })).getByText(LIBRARIES.inTools)).toBeDefined();
    expect(within(preview.getByRole("article", { name: "stdlib v1.22.5: 2 known vulnerabilities" })).getByText(LIBRARIES.inTools)).toBeDefined();
    expect(preview.getByText("libc6 2.39-0ubuntu8.4 · no fix published · system libraries")).toBeDefined();
    expect(preview.getByText("certifi 2023.7.22 · Canyon can't tell what put it there")).toBeDefined();
    // the class is the contract's enum, never a word a person reads
    for (const raw of [/\bos\b/, /\bruntime\b/, /\blifecycle\b/, /run image/i, /operating system/i]) expect(card.queryAllByText(raw)).toEqual([]);
    const prod = within(card.getByRole("region", { name: "Production system libraries" }));
    expect(prod.getByText(`✓ ${patched.why}`)).toBeDefined();
    expect(prod.getByText("Fixed by Canyon · libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5 · CVE-2024-5535 · system libraries")).toBeDefined();
  });

  // A rebuild's fixes (#2631): one line per package, its vulnerabilities together, and where it was by its class — a
  // runtime package is in the build tools, never the system libraries.
  it("says what a rebuild fixed once per package, in the build tools", async () => {
    const tar = { name: "tar", from: "7.5.11", to: "7.5.20", class: "runtime" as const, run: "gr_r3b1d0", at: "2026-10-25T09:02:28Z" };
    server({ ...reads(() => promoted), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, access: { ...project.access, level: "contributor" } } } },
      [IMAGE]: { body: platform }, [PROD_IMAGE]: { body: { ...patched, env: "prod", patched: [{ ...tar, vulnerability: "CVE-2026-1" }, { ...tar, vulnerability: "CVE-2026-3" }] } } });
    render(<App id="claims-portal" auth="oidc" />);
    const prod = within(await screen.findByRole("region", { name: "Production system libraries" }));
    expect(prod.getAllByText(/^Fixed by Canyon/).map((one) => one.textContent)).toEqual(["Fixed by Canyon · tar 7.5.11 → 7.5.20 · CVE-2026-1, CVE-2026-3 · build tools"]);
  });

  // An imported tool's result Canyon can't place in the image (contracts/finding.md, An imported image result): its note in
  // the server's why's words, never the layer it lacks.
  it("says an imported result Canyon can't place in the image", async () => {
    const unplaced = { name: "zlib1g", version: "1.3", layer: null, class: "unknown" as const, why: "unplaced" as const };
    server({ ...reads(() => page.status), [IMAGE]: { body: { ...platform, notes: [...platform.notes, unplaced] } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: LIBRARIES.title }));
    expect(card.getByText("zlib1g 1.3 · Canyon can't tell which part of the image it's in")).toBeDefined();
  });

  it("says a read that could not check with the cross mark, never as nothing found", async () => {
    const why = "could not check: its last rescan did not finish";
    server({ ...reads(() => page.status), [IMAGE]: { body: { ...platform, state: "could_not_check", why, findings: [], notes: [] } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: LIBRARIES.title }));
    expect(card.getByText(`✗ ${why}`).className).toContain("text-critical");
  });

  it("draws no card while nothing is scanned, and says a read it can't read", async () => {
    server({ ...reads(() => page.status), [IMAGE]: { body: { ...platform, state: "not_scanned", why: "no image scan recorded", scanned_at: null, db_built_at: null, findings: [], notes: [] } } });
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    await flush();
    expect(screen.queryByRole("region", { name: LIBRARIES.title })).toBeNull();
    unmount();
    server({ ...reads(() => page.status), [IMAGE]: { body: { state: "checked" } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: LIBRARIES.title }));
    expect(card.getByText(`Canyon couldn't read what runs in preview: ${LIBRARIES.notWhole}`)).toBeDefined();
  });

  it("is neither read nor drawn for a read-only member", async () => {
    const sent = server({ [IMAGE]: { body: platform }, "GET /api/v1/projects/claims-portal": { body: { project: { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "viewer" },
      envs: { preview: { url: "http://claims-portal.preview.localhost:7777", state: "running" } } } } } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByText(`${S.readOnly} ${S.readOnlyUse}`);
    await flush();
    expect(screen.queryByRole("region", { name: LIBRARIES.title })).toBeNull();
    expect(sent().filter((request) => request.includes("/image"))).toEqual([]);
  });
});

// What the org's platform provided (design/HAND-OFF.md §7, #2432): the single read's resources[], the
// contract's own rows, drawn with their endpoints as text — the platform wrote them, so never a link.
describe("the app page's provided resources", () => {
  const listed = golden<{ resources: AppRead["resources"] }>("app-page/claims-portal.json").resources ?? [];
  const provided = listed.filter((one) => one.state === "provided");

  it("draws each provided resource and its endpoint as text, never a link", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: { ...project, resources: listed } } } });
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: "Provided by your platform" }));
    expect(provided.length).toBeGreaterThan(0);
    for (const one of provided) {
      expect(card.getByText(`${one.type} ${one.name} · ${one.env === "prod" ? "production" : "preview"}`)).toBeDefined();
      expect(card.getByText(one.endpoint as string).closest("a")).toBeNull();
    }
    expect(screen.queryAllByRole("link").filter((link) => provided.some((one) => link.textContent?.includes(one.endpoint as string)))).toEqual([]);
    expect(card.queryByText("reports", { exact: false })).toBeNull(); // Canyon's own records are not the platform's
  });

  it("draws nothing where the read carries none", async () => {
    server(reads(() => page.status));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Go-live checklist" });
    expect(screen.queryByRole("region", { name: "Provided by your platform" })).toBeNull();
  });
});

// What needs a person leads the app page (D15; contracts/routes.md, *What needs a person*): the status read's needs, most
// urgent first, each with its fix where the reader may act; the headline where it says what the top need says.
describe("what needs a person, on the app page", () => {
  const refused: Need = { kind: "refused", env: "prod", rank: 3, since: "2026-11-08T09:00:00Z", you: true, ask: [], destination: "api.stripe.com", refusals: 4,
    last_refused_at: new Date(Date.now() - 60_000).toISOString(), other_calls: 0, held: false };
  const secret: Need = { kind: "secret", env: null, rank: 4, since: null, you: true, ask: [], version: "v1.4" };
  // The fixture's v1.4 is blocked by one finding: a need the headline says (were it not the secret's alone).
  const blocked: Need = { kind: "blocked", env: null, rank: 6, since: null, you: true, ask: [], version: "v1.4", count: 1 };
  const unowned: Need = { kind: "unowned", env: null, rank: 10, since: null, you: true, ask: [] };
  const band = () => within(screen.getByRole("region", { name: "Needs you" }));
  const ALLOW = "PUT /api/v1/projects/claims-portal/egress/prod/destinations/api.stripe.com";

  it("lists the needs above the rest, its own steps on each, and says the headline in no second place", async () => {
    const sent = server({ ...reads(() => ({ ...page.status, needs: [refused, blocked] })), [ALLOW]: { status: 204, body: null } });
    render(<App id="claims-portal" auth="oidc" />);
    expect((await screen.findByRole("heading", { name: "Needs you · 2" }))).toBeDefined();
    expect(band().getAllByRole("listitem").map((line) => line.querySelector("b")?.textContent)).toEqual(["claims-portal can't reach api.stripe.com in production.", "claims-portal v1.4 is blocked by 1 finding."]);
    expect(screen.queryByRole("region", { name: "Where it stands" })).toBeNull(); // the blocked line says what it said, in its place
    expect(band().queryByRole("link", { name: /^Open review/ })).toBeNull(); // this page is the review
    fireEvent.click(band().getByRole("button", { name: "Allow api.stripe.com in production for claims-portal" }));
    expect(await band().findByText("Allowed api.stripe.com in production for claims-portal. Its next call goes through within seconds.")).toBeDefined();
    expect(sent()).toContain(ALLOW);
  });

  it("leads with the headline where it says what the top need says, the other needs under it", async () => {
    server(reads(() => ({ ...page.status, needs: [blocked, unowned] })));
    render(<App id="claims-portal" auth="oidc" />);
    const headed = await screen.findByRole("region", { name: "Where it stands" });
    expect(await within(headed).findByRole("heading", { name: BLOCKED })).toBeDefined();
    const listed = screen.getByRole("region", { name: "Needs you" });
    expect(within(listed).getAllByRole("listitem").map((line) => line.querySelector("b")?.textContent)).toEqual(["claims-portal has no owner."]);
    expect(headed.compareDocumentPosition(listed) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  // Where the receipt says no owner is needed (sign-in off, owner.required at its default), nobody needs to give the
  // app one: the unowned need is not shown, and the band's count leaves it out (the tour's final visitor run).
  it("shows no unowned need where the receipt has owner.required neutral", async () => {
    const neutral = { ...page.status.receipt, rules: page.status.receipt.rules.map((rule) => (rule.id === "owner.required"
      ? { ...rule, passed: false, unset: true as const, why: "no owner needed: sign-in is off on this Canyon" } : rule)) };
    server(reads(() => ({ ...page.status, needs: [refused, unowned], receipt: neutral })));
    render(<App id="claims-portal" auth="off" />);
    expect(await screen.findByRole("heading", { name: "Needs you · 1" })).toBeDefined();
    expect(band().queryByText("claims-portal has no owner.")).toBeNull();
    expect(screen.queryByText(/No owner needed/)).toBeNull();
  });

  // A secret in the blocked version is the blocked headline's own problem: said once, there, with "Rotate the key" and its
  // one filled button, the fix copied for the reader's agent (#102; it was a second red box above it).
  it("folds the blocked version's secret into the headline, which leads with the fix to copy for an agent", async () => {
    server(reads(() => ({ ...page.status, needs: [secret] })));
    const writeText = vi.fn(async (_text: string) => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    render(<App id="claims-portal" auth="oidc" />);
    const headed = await screen.findByRole("region", { name: "Where it stands" });
    expect(await within(headed).findByRole("heading", { name: BLOCKED })).toBeDefined();
    expect(within(headed).getByText(/^Rotate the key, then upload a fix\. Nothing was built/)).toBeDefined();
    expect(screen.queryByRole("region", { name: "Needs you" })).toBeNull();
    const copy = await within(headed).findByRole("button", { name: "Copy the fix for your agent" });
    expect(copy.className).toContain("bg-accent");
    fireEvent.click(copy);
    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    expect(writeText.mock.calls[0]![0]).toBe(findings.filter(blocks).map((one) => forAgent(one, "claims-portal")).join("\n---\n\n"));
  });

  // While a newer version is blocked, promoting what preview runs is no longer the page's main step: its button is plain (#102).
  it("draws Promote for what preview runs as a plain button while a newer version is blocked", async () => {
    server(reads(() => ({ ...page.status, needs: [] })));
    render(<App id="claims-portal" auth="oidc" />);
    const box = await screen.findByRole("complementary", { name: "Promote" });
    const promote = await within(box).findByRole("button", { name: /^Promote v1\.3/ });
    expect(promote.className).not.toContain("bg-accent");
  });

  // A secret of another version is no part of this headline: its line stays in the needs.
  it("keeps a secret of another version in the needs", async () => {
    server(reads(() => ({ ...page.status, needs: [{ ...secret, version: "v1.2" }] })));
    render(<App id="claims-portal" auth="oidc" />);
    const listed = await screen.findByRole("region", { name: "Needs you" });
    expect(within(listed).getByText(/Rotate the key, then upload a fix\./)).toBeDefined();
  });

  // The status read's two kinds, which the fleet never shows: what blocks production alone, and what production won't allow.
  it("says what blocks production alone, and allows a host name production won't, beside what only the code fixes", async () => {
    const placed: Need = { kind: "blocked_prod", env: "prod", rank: 6, since: null, you: true, ask: [], version: "v1.3", count: 2 };
    const refuses: Need = { kind: "will_refuse", env: "prod", rank: 6, since: null, you: true, ask: [], version: "v1.3", blocks: true,
      denied: [{ destination: "example.com", port: 443, allowable: true }, { destination: "1.1.1.1", port: 443, allowable: false }, { destination: "invalid", port: 443, allowable: false },
        { destination: "db.example", port: 5432, allowable: false }] };
    const PUT = "PUT /api/v1/projects/claims-portal/egress/prod/destinations/example.com";
    const sent = server({ ...reads(() => ({ ...page.status, needs: [placed, refuses] })), [PUT]: { status: 204, body: null } });
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Needs you" });
    expect(await band().findByText("claims-portal v1.3 can't go to production: 2 findings block production.")).toBeDefined();
    const line = band().getByText("claims-portal v1.3 can't go to production yet: production doesn't allow everything it connects to in preview.").closest("li")!;
    expect(line.textContent).toContain("Production doesn't allow example.com yet. Nobody can allow 1.1.1.1, an invalid address, db.example:5432: the code has to change.");
    expect(line.textContent).toContain(S.needAllowWho);
    fireEvent.click(within(line).getByRole("button", { name: "Allow example.com in production for claims-portal" }));
    await waitFor(() => expect(sent()).toContain(PUT));
  });

  // Production live on the newest version, two calls refused now (#2762's review): each its own line with its Allow, none
  // lost to the can't-reach headline, which stands in for no need; the link a refusal carries puts the keyboard on the first.
  it("keeps each refused call of a live production its own Allow, and focuses it from the refusal's link", async () => {
    const live = { ...project, latest: { ...project.latest, package: page.promotion.promotion.package, label: "v1.3", stage: "production" as const } };
    const older = { ...refused, destination: "api.stripe.com", since: "2026-11-08T08:00:00Z" };
    const newer = { ...refused, destination: "hooks.slack.com", since: "2026-11-08T09:00:00Z" };
    const base = egressOff as unknown as ProjectEgress;
    const requests = [newer, older].map((one) => ({ destination: one.destination, state: "pending", first_refused_at: one.since, last_refused_at: one.since, refusals: 4, refusing: true }));
    server({ ...reads(() => ({ ...page.status, envs: { prod: page.promotion.promotion }, needs: [older, newer] })), "GET /api/v1/projects/claims-portal": { body: { project: live } },
      "GET /api/v1/projects/claims-portal/egress": { body: { ...base, envs: { ...base.envs, prod: { ...base.envs.prod, requests } } } }, "GET /api/v1/egress": { body: { via: "off" } } });
    render(<App id="claims-portal" auth="oidc" outbound />);
    await screen.findByRole("region", { name: "Needs you" });
    const stripe = await band().findByRole("button", { name: "Allow api.stripe.com in production for claims-portal" });
    expect(band().getByRole("button", { name: "Allow hooks.slack.com in production for claims-portal" })).toBeDefined();
    await waitFor(() => expect(document.activeElement).toBe(stripe));
    // the can't-reach band would say the same twice: never drawn while the needs carry a refused call of production
    expect(screen.queryByRole("region", { name: "Where it stands" })).toBeNull();
    expect(screen.queryByText(/can't reach .* · and 1 more/)).toBeNull();
  });

  it("holds will_refuse's Allow while production is held, and offers none to a reader who may not", async () => {
    const refuses: Need = { kind: "will_refuse", env: "prod", rank: 7, since: null, you: true, ask: [], version: "v1.3", blocks: false, denied: [{ destination: "example.com", port: 443, allowable: true }] };
    const hold = { by: { kind: "person" as const, subject: "rhea", display: "Rhea Okafor" }, at: "2026-11-08T09:40:00Z", reason: "INC-1", version: "v1.3" };
    server(reads(() => ({ ...page.status, hold, receipt: { ...page.status.receipt, held: hold }, needs: [refuses] })));
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Needs you" });
    const held = await band().findByRole("button", { name: S.allowInProduction });
    expect((held as HTMLButtonElement).disabled).toBe(true);
    expect(held.nextElementSibling?.textContent).toBe(S.needAllowHeld);
    unmount();
    server(reads(() => ({ ...page.status, needs: [{ ...refuses, you: false }] })));
    render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Needs you" });
    expect(band().queryByRole("button", { name: /^Allow/ })).toBeNull();
  });

  // A Promote that what production won't allow stops says why from the need's destinations, never the rule's own line.
  it("says a stopped Promote's reason from what production won't allow", async () => {
    const egressRow = { id: "prod.egress-allowed", name: "Production allows everything preview connects to", blocks: true, passed: false, why: "production does not allow a line never read" };
    const refuses: Need = { kind: "will_refuse", env: "prod", rank: 6, since: null, you: true, ask: [], version: "v1.3", blocks: true, denied: [{ destination: "example.com", port: 443, allowable: true }] };
    server(reads(() => ({ ...open, receipt: { ...open.receipt, blocked_by: ["prod.egress-allowed"], rules: [...open.receipt.rules, egressRow] }, needs: [refuses] })));
    render(<App id="claims-portal" auth="oidc" />);
    const promote = await within(await screen.findByRole("complementary", { name: "Promote" })).findByRole("button", { name: /^Promote/ });
    expect((promote as HTMLButtonElement).disabled).toBe(true);
    expect(promote.nextElementSibling?.textContent).toBe("Production doesn't allow example.com yet.");
    expect(screen.queryByText(/a line never read/)).toBeNull();
  });

  // The link a refusal carries opens on the band, its Allow ready to press (#/apps/<app>/outbound).
  it("puts the keyboard on the refusal's Allow when opened from the refusal's link", async () => {
    server(reads(() => ({ ...page.status, needs: [refused] })));
    render(<App id="claims-portal" auth="oidc" outbound />);
    await screen.findByRole("region", { name: "Needs you" });
    const allow = await band().findByRole("button", { name: "Allow api.stripe.com in production for claims-portal" });
    await waitFor(() => expect(document.activeElement).toBe(allow));
  });
});

// The owner's decision 1 (2026-11-08) and D15 on the app page: who can open it under the name, with Share to whoever manages
// it; what each environment calls out to on its card; and a read-only member's lines.
describe("who can open it, what it calls out to, and who looks after it", () => {
  it("says under the name who can open it, and opens the card, at Share with for Share", async () => {
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/access": { body: access } });
    render(<App id="claims-portal" auth="oidc" />);
    const who = await screen.findByRole("button", { name: S.whoCanOpen });
    const card = await screen.findByRole("region", { name: "Access" });
    expect(who.closest("p")!.textContent).toMatch(/^(Shared with .*\.|Private: .*) · Who can open it · Share$/);
    fireEvent.click(screen.getByRole("button", { name: `${S.share} claims-portal` }));
    await waitFor(() => expect(document.activeElement).toBe(within(card).getByRole("combobox", { name: "Share with" })));
    expect((card.querySelector("details") as HTMLDetailsElement).open).toBe(true);
  });

  it("offers no Share to a contributor, who reads whom to ask", async () => {
    const contributor = { ...project, access: { ...project.access, level: "contributor" as const, requests: undefined } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: contributor } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("button", { name: S.whoCanOpen })).toBeDefined();
    expect(screen.queryByRole("button", { name: `${S.share} claims-portal` })).toBeNull();
  });

  it("says on each environment's card what it calls out to and how much Canyon refused", async () => {
    const calls = (n: number, refused: number) => Array.from({ length: n }, (_, at) => ({ destination: `d${at}.example`, port: 443, first_seen_at: "2026-11-01T00:00:00Z", last_seen_at: "2026-11-08T00:00:00Z",
      calls: 3, last_outcome: at < refused ? "refused" : "allowed", last_status_class: "2xx", last_protocol: "connect", last_response_flags: [] }));
    const base = egressOff as unknown as ProjectEgress;
    const read = { ...base, envs: { preview: { ...base.envs.preview, destinations: calls(5, 2) }, prod: { ...base.envs.prod, destinations: calls(1, 0) } } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/egress": { body: read }, "GET /api/v1/egress": { body: { via: "off" } } });
    render(<App id="claims-portal" auth="oidc" />);
    const line = await within(await screen.findByRole("region", { name: "Preview" })).findByRole("button", { name: "5 destinations in the last 30 days · 2 refused on their last call" });
    expect(line.className).not.toContain("text-critical"); // red only while a request is refused now
    expect(within(screen.getByRole("region", { name: "Production" })).queryByRole("button", { name: /destinations? in the last 30 days/ })).toBeNull(); // nothing runs there
  });

  // Only a running card says it; red while a request of its env is refused now (the refused tile's predicate).
  it("says what a stopped environment called out to nowhere, and turns red for a call refused now", async () => {
    const calls = [{ destination: "api.openai.com", port: 443, first_seen_at: "2026-11-01T00:00:00Z", last_seen_at: "2026-11-08T00:00:00Z", calls: 3, last_outcome: "refused", last_status_class: "none", last_protocol: "connect", last_response_flags: ["UAEX"] }];
    const base = egressOff as unknown as ProjectEgress;
    const refusing = [{ destination: "api.openai.com", state: "pending", first_refused_at: "2026-11-01T00:00:00Z", last_refused_at: "2026-11-08T00:00:00Z", refusals: 3, refusing: true }];
    const read = { ...base, envs: { ...base.envs, preview: { ...base.envs.preview, destinations: calls, requests: refusing } } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/egress": { body: read }, "GET /api/v1/egress": { body: { via: "off" } } });
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    expect((await within(await screen.findByRole("region", { name: "Preview" })).findByRole("button", { name: "1 destination in the last 30 days · 1 refused on their last call" })).className).toContain("text-critical");
    unmount();
    // A pending request the read doesn't mark refusing is nothing refused now: the line isn't red.
    const quiet = { ...read, envs: { ...read.envs, preview: { ...read.envs.preview, requests: [{ ...refusing[0]!, refusing: false }] } } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal/egress": { body: quiet }, "GET /api/v1/egress": { body: { via: "off" } } });
    const { unmount: calm } = render(<App id="claims-portal" auth="oidc" />);
    expect((await within(await screen.findByRole("region", { name: "Preview" })).findByRole("button", { name: /^1 destination in the last 30 days/ })).className).not.toContain("text-critical");
    calm();
    const stopped: Status = { ...page.status, envs: { ...page.status.envs, preview: { ...(page.status.envs.preview as Env), state: "stopped" } } };
    server({ ...reads(() => stopped), "GET /api/v1/projects/claims-portal/egress": { body: read }, "GET /api/v1/egress": { body: { via: "off" } } });
    render(<App id="claims-portal" auth="oidc" />);
    await within(await screen.findByRole("region", { name: "Preview" })).findByText(/^Stopped/);
    expect(within(screen.getByRole("region", { name: "Preview" })).queryByRole("button", { name: /in the last 30 days/ })).toBeNull();
  });

  // A read-only member never learns production is held (design/PERMISSIONS.md): its card never says nothing is there, and
  // the page says who looks after the app.
  it("tells a read-only member who looks after it, and never that nothing is in production", async () => {
    const nora = { kind: "person", subject: "nora@meridian.example", display: "Nora Lind" };
    server({
      "GET /api/v1/projects/pricing-lab": { body: { project: { id: "pricing-lab", name: "pricing-lab", owners: [nora], owner_groups: [], summary: "Tries new premium curves", access: { level: "viewer" } } } },
      "GET /api/v1/projects/pricing-lab/status": { status: 403, body: { error: "team_required" } },
    });
    render(<App id="pricing-lab" auth="oidc" />);
    expect(await screen.findByText(S.whoLooksAfterOwner.replace("<owner>", "Nora Lind"))).toBeDefined();
    expect(screen.getByText(`${S.readOnly} ${S.readOnlyUse}`)).toBeDefined();
    expect(within(screen.getByRole("region", { name: "Production" })).queryByText(/Nothing is in production/)).toBeNull();
  });

  // Who looks after it is the read-only member's: the team reads its owners in the head, and a responder, whose reduced
  // read carries the status read, reads no such section either.
  it("shows Who looks after it to a read-only member alone, never to the team or a responder", async () => {
    server(reads(() => page.status));
    const { unmount } = render(<App id="claims-portal" auth="oidc" />);
    await screen.findByRole("region", { name: "Preview" });
    expect(screen.queryByRole("region", { name: S.whoLooksAfter })).toBeNull();
    unmount();
    const row = { id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level: "none" } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: row } },
      "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByText(S.responderAccess)).toBeDefined();
    expect(screen.queryByRole("region", { name: S.whoLooksAfter })).toBeNull();
  });

  // A contributor on an app no single person owns asks the group that owns it to change the mode (#2766's review).
  it("hands the Outbound card the group that owns the app, for a contributor's mode line", async () => {
    const owned = { ...project, owners: [], owner_groups: ["claims-team"], access: { ...project.access, level: "contributor" as const, requests: undefined } };
    const base = egressOff as unknown as ProjectEgress;
    const through = { ...base, floor: "enforced" as const, envs: { preview: { ...base.envs.preview, via: "canyon" as const }, prod: { ...base.envs.prod, via: "canyon" as const } } };
    server({ ...reads(() => page.status), "GET /api/v1/projects/claims-portal": { body: { project: owned } },
      "GET /api/v1/projects/claims-portal/egress": { body: through }, "GET /api/v1/egress": { body: { via: "canyon", mode: "observe" } } });
    render(<RailContext value={{ org: null, viewer: { display: "Erin", role: "member", github_logins: [], github_link: false }, admin: false }}><App id="claims-portal" auth="oidc" /></RailContext>);
    expect(await within(await screen.findByRole("region", { name: "Outbound" })).findByText(`${S.modeOwner} Ask a member of the claims-team group, which owns it.`)).toBeDefined();
  });
});
