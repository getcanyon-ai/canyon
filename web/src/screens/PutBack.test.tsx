import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { App as AppRead, Earlier, Env, Finding, FleetPage, Receipt, RunRead, Status } from "../api";
import { APPROVAL, S } from "../strings";
import { shortDate } from "../ui/Verdict";
import { App } from "./App";

const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
// The contract's rollback status: production runs v1.4, and its earlier versions are v1.3, v1.4 patched and v1.2.
// Read with ?run= it is the rollback review's, its receipt carrying `ships`; read bare, it carries none.
const rollback = golden<Status>("app-page/claims-portal-rollback-status.json");
const { ships: _ships, ...atRest } = rollback.receipt;
const bare: Status = { ...rollback, receipt: atRest };
const admin = golden<FleetPage>("fleet/page.json").apps.find((app) => app.id === "claims-portal") as AppRead;
// The minimum level for putting a version back is contributor (design/OPERATIONS.md §1.8), as deploy and promote.
const at = (level: "owner" | "contributor"): AppRead => ({ ...admin, access: { level, everyone: false, people: 1, groups: 0 } });
const contributor = at("contributor");
const [v13, v14, v12] = rollback.earlier as [Earlier, Earlier, Earlier];
const REVIEW = "gr_b1c4";
const PUT_BACK = "POST /api/v1/projects/claims-portal/gate-runs";
const PROMOTE = "POST /api/v1/projects/claims-portal/promotions";
const RUN = `GET /api/v1/gate-runs/${REVIEW}`;
const reviewed = (state: RunRead["state"], failure_reason?: string, findings: Finding[] = []): RunRead => ({ id: REVIEW, package: v13.package, state, blocking: findings.length, warnings: 0, findings, ...(failure_reason === undefined ? {} : { failure_reason }) });
// One blocking finding of the gate's own log, numbered as the run read numbers it.
const blocker = golden<{ runs: { results: Finding[] }[] }>("finding/claims-portal.sarif.json").runs.flatMap((one) => one.results).map((one, entry) => ({ ...one, entry })).find((one) => one.properties.canyon.blocks) as Finding;
// The receipt with one go-live rule failing for why.
const failing = (id: string, why: string): Receipt => ({ ...rollback.receipt, blocked_by: [id], rules: rollback.receipt.rules.map((rule) => (rule.id === id ? { ...rule, passed: false, why } : rule)) });

type Answer = { status?: number; body: unknown };
// The server answers "<METHOD> <url>" from routes, read on every request so a test can change an answer
// mid-way; a function answers each time it is asked, a promise when it should not answer yet.
function server(routes: Routes) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status = 200, body } = typeof route === "function" ? await route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
// The app as a contributor reads it, the rollback review answering `run` and its receipt `receipt`.
type Routes = Record<string, Answer | (() => Answer | Promise<Answer>)>;
const reads = (run: RunRead, receipt: Receipt = rollback.receipt, more: Routes = {}): Routes => ({
  "GET /api/v1/projects/claims-portal": { body: { project: contributor } },
  "GET /api/v1/projects/claims-portal/status": { body: bare },
  [`GET /api/v1/projects/claims-portal/status?run=${REVIEW}`]: { body: { ...rollback, promote_run: REVIEW, receipt } },
  [PUT_BACK]: { status: 202, body: { run: { id: REVIEW, state: "pending" } } },
  [RUN]: { body: { run } },
  ...more,
});
const production = async () => within(await screen.findByRole("region", { name: "Production" }));
const putBack = async (version = "v1.3") => fireEvent.click((await production()).getByRole("button", { name: `Put back ${version}` }));
// Promote, then its question's Promote: the question before production changes (#157).
const promoteIt = async () => {
  fireEvent.click(await (await production()).findByRole("button", { name: "Promote v1.3" }));
  fireEvent.click((await production()).getByRole("button", { name: "Promote v1.3" }));
};
const posts = (sent: string[], route: string) => sent.filter((one) => one.startsWith(route)).length;
const reviewing = (version: string) => S.putBackReviewing.replace("<version>", () => version);
const flush = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("who sees Earlier versions", () => {
  // Contributor and above read the status read, so they see the card and its Put back (design/OPERATIONS.md §1.8).
  describe.each([["owner", at("owner")], ["contributor", contributor], ["admin", admin]])("a reader at %s", (_level, project) => {
    it("sees production's earlier versions, with Put back", async () => {
      server({ ...reads(reviewed("clean")), "GET /api/v1/projects/claims-portal": { body: { project } } });
      render(<App id="claims-portal" auth="oidc" />);
      expect(await (await production()).findByRole("button", { name: "Put back v1.3" })).toBeDefined();
    });
  });

  // Below contributor the status read is refused, so earlier[] is never read: no card, no Put back.
  describe.each([["read-only", "viewer"], ["no access", "none"]] as const)("a reader with %s", (_level, level) => {
    it("sees no earlier versions, and the status is never read", async () => {
      const sent = server({ "GET /api/v1/projects/claims-portal": { body: { project: { id: "claims-portal", name: "claims-portal", owners: [], summary: "x", access: { level }, envs: { prod: { url: (rollback.envs.prod as Env).url, state: "running" } } } } } });
      render(<App id="claims-portal" auth="oidc" />);
      await flush();
      await flush();
      expect(screen.queryByRole("region", { name: "Earlier versions" })).toBeNull();
      expect(screen.queryByRole("button", { name: /^Put back/ })).toBeNull();
      expect(sent()).toEqual(["GET /api/v1/projects/claims-portal", "GET /api/v1/preferences"]); // the app's row, and whether the reader is a responder
    });
  });
});

describe("Earlier versions on the production card", () => {
  it("lists production's earlier versions, newest first", async () => {
    server(reads(reviewed("clean")));
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: "Earlier versions" }));
    expect(card.getAllByRole("listitem").map((row) => row.querySelector("bdi")?.textContent)).toEqual([
      `v1.3 · ${shortDate(v13.deployed_at)}`,
      S.patched.replace("<version>", "v1.4").replace("<date>", () => shortDate(v14.deployed_at)), // 30 Sep, as this runtime spells it
      `v1.2 · ${shortDate(v12.deployed_at)}`,
    ]);
    expect(card.getAllByRole("listitem")[1]!.textContent).toContain("promoted by Dana W.");
  });

  it("shows no card when production ran nothing before", async () => {
    server({ ...reads(reviewed("clean")), "GET /api/v1/projects/claims-portal/status": { body: { ...bare, earlier: [] } } });
    render(<App id="claims-portal" auth="oidc" />);
    await production();
    expect(screen.queryByRole("region", { name: "Earlier versions" })).toBeNull();
  });

  // A host from before earlier[] answers none: the page stands, without the card.
  it("shows the production card without earlier versions when the server answers none", async () => {
    const { earlier: _earlier, ...older } = bare;
    server({ ...reads(reviewed("clean")), "GET /api/v1/projects/claims-portal/status": { body: older } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await (await production()).findByText("v1.4")).toBeDefined();
    expect(screen.queryByRole("region", { name: "Earlier versions" })).toBeNull();
  });

  // An image Canyon no longer keeps can't be put back as it ran: no review is spent finding that out.
  it("disables Put back on a version whose image is gone, and sends nothing", async () => {
    const sent = server(reads(reviewed("clean")));
    render(<App id="claims-portal" auth="oidc" />);
    const button = (await production()).getByRole("button", { name: "Put back v1.2" });
    expect((button as HTMLButtonElement).disabled).toBe(true);
    expect((await production()).getByText(S.imageGone.replaceAll("<version>", "v1.2"))).toBeDefined();
    fireEvent.click(button);
    await flush();
    expect(posts(sent(), PUT_BACK)).toBe(0);
  });
});

describe("Put back", () => {
  // What production refuses of what a person dismissed stops nothing, and is said before the person promotes.
  it("says what production refuses of what a person dismissed, and Promote stays", async () => {
    const line = "this version may need hooks.slack.com, which production does not allow. A person can change that on the Outbound card.";
    const row = { id: "prod.egress-allowed", name: "Production allows everything preview connects to", blocks: false, passed: false, why: line };
    server(reads(reviewed("clean"), { ...rollback.receipt, rules: [...rollback.receipt.rules, row] as Receipt["rules"] }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await screen.findByText(line)).toBeDefined();
    expect((screen.getByRole("button", { name: "Promote v1.3" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("reviews the exact image production ran, preview untouched, and links the run in a new tab", async () => {
    const sent = server(reads(reviewed("running")));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await screen.findByText(reviewing("v1.3"))).toBeDefined();
    expect(sent()).toContain(`${PUT_BACK} ${JSON.stringify({ package: v13.package, rollback_to: v13.image_digest })}`);
    const link = screen.getByRole("link", { name: "the run →" });
    expect([link.getAttribute("href"), link.getAttribute("target"), link.getAttribute("rel")]).toEqual([`#/apps/claims-portal?run=${REVIEW}`, "_blank", "noopener"]);
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
  });

  it.each([
    [409, { error: "not_earlier", message: "v1.3 isn't one of production's earlier versions" }],
    [409, { error: "run_in_progress", run: { id: "gr_other", state: "running" } }],
    [400, { error: "bad_request" }],
    [429, { error: "rate_limited", message: "Too many reviews this hour: try again in 20 minutes.", retry_after_s: 1200 }],
  ])("says a %i refusal as the server said it, links no run, and can be asked again", async (status, body) => {
    const routes = reads(reviewed("running"), rollback.receipt, { [PUT_BACK]: { status, body } });
    const sent = server(routes);
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await screen.findByText("message" in body ? body.message : `POST /api/v1/projects/claims-portal/gate-runs answered ${status}`)).toBeDefined();
    expect(screen.queryByRole("link", { name: "the run →" })).toBeNull();
    routes[PUT_BACK] = { status: 202, body: { run: { id: REVIEW, state: "pending" } } };
    await putBack();
    expect(await screen.findByText(reviewing("v1.3"))).toBeDefined();
    expect(posts(sent(), PUT_BACK)).toBe(2);
  });

  it("asks once for a double click, and says it is asking", async () => {
    let answer: (one: Answer) => void = () => {};
    const sent = server(reads(reviewed("running"), rollback.receipt, { [PUT_BACK]: () => new Promise<Answer>((resolve) => { answer = resolve; }) }));
    render(<App id="claims-portal" auth="oidc" />);
    const button = (await production()).getByRole("button", { name: "Put back v1.3" });
    fireEvent.click(button);
    fireEvent.click(button);
    expect(await screen.findByText("Putting back…")).toBeDefined();
    await act(async () => answer({ status: 202, body: { run: { id: REVIEW, state: "pending" } } }));
    expect(await screen.findByText(reviewing("v1.3"))).toBeDefined();
    expect(posts(sent(), PUT_BACK)).toBe(1);
  });

  it("cancels the review while it runs", async () => {
    const sent = server(reads(reviewed("running"), rollback.receipt, { [`POST /api/v1/gate-runs/${REVIEW}/cancel`]: { status: 202, body: { run: { id: REVIEW, state: "running" } } } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    fireEvent.click(await screen.findByRole("button", { name: "Cancel review" }));
    await flush();
    expect(sent()).toContain(`POST /api/v1/gate-runs/${REVIEW}/cancel`);
    expect(screen.queryByText(reviewing("v1.3"))).toBeNull();
  });

  // The first review's read answers late, after the second Put back: it is the first's, and changes nothing.
  it("cancels once for a double click", async () => {
    const sent = server(reads(reviewed("running"), rollback.receipt, { [`POST /api/v1/gate-runs/${REVIEW}/cancel`]: () => new Promise<Answer>((resolve) => setTimeout(() => resolve({ status: 202, body: { run: { id: REVIEW, state: "running" } } }), 50)) }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const cancel = await screen.findByRole("button", { name: "Cancel review" });
    fireEvent.click(cancel);
    fireEvent.click(cancel);
    await act(() => new Promise((resolve) => setTimeout(resolve, 100)));
    expect(posts(sent(), `POST /api/v1/gate-runs/${REVIEW}/cancel`)).toBe(1);
  });

  // The review ended between the reading and the click: the cancel finds nothing to cancel, and the card closes quietly.
  it("closes quietly when the review had already ended", async () => {
    server(reads(reviewed("running"), rollback.receipt, { [`POST /api/v1/gate-runs/${REVIEW}/cancel`]: { status: 409, body: { error: "ended" } } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    fireEvent.click(await screen.findByRole("button", { name: "Cancel review" }));
    await flush();
    expect(screen.queryByText(reviewing("v1.3"))).toBeNull();
    expect(screen.queryByText(`POST /api/v1/gate-runs/${REVIEW}/cancel answered 409`)).toBeNull();
  });

  // The second Put back is answered and, in the same tick, the first review's answer lands before the screen has redrawn: it is v1.3's, not v1.2's.
  it.each([
    ["the review's state", { body: { run: reviewed("clean") } }, RUN],
    ["the review's receipt", { body: { ...rollback, promote_run: REVIEW, receipt: rollback.receipt } }, `GET /api/v1/projects/claims-portal/status?run=${REVIEW}`],
    ["a failed read", { status: 503, body: null }, RUN],
  ] as const)("starts over on another row's Put back mid-review, and drops the first review's late answer: %s", async (_case, answer, held) => {
    const both = { ...bare, earlier: [v13, { ...v12, kept: true }] };
    let late: (one: Answer) => void = () => {};
    let started: (one: Answer) => void = () => {};
    const routes = reads(reviewed(held === RUN ? "running" : "clean"), rollback.receipt, { "GET /api/v1/projects/claims-portal/status": { body: both }, [held]: () => new Promise<Answer>((resolve) => { late = resolve; }) });
    const sent = server(routes);
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await vi.waitFor(() => expect(sent()).toContain(held));
    routes[PUT_BACK] = () => new Promise<Answer>((resolve) => { started = resolve; });
    routes["GET /api/v1/gate-runs/gr_c2d5"] = () => new Promise<Answer>(() => {}); // v1.2's review is still being read
    await putBack("v1.2");
    await screen.findByText("Putting back…");
    await act(async () => {
      started({ status: 202, body: { run: { id: "gr_c2d5", state: "pending" } } });
      late(answer);
    });
    await flush();
    expect(screen.getByText(reviewing("v1.2"))).toBeDefined();
    expect(screen.queryByText(reviewing("v1.3"))).toBeNull();
    expect(screen.queryByText(S.putBackNotThat.replace("<version>", "v1.2"))).toBeNull();
    expect(screen.queryByText(`GET /api/v1/gate-runs/${REVIEW} answered 503`)).toBeNull();
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
    expect(sent()).toContain(`${PUT_BACK} ${JSON.stringify({ package: v12.package, rollback_to: v12.image_digest })}`);
  });

  it("keeps reading a review through a failed read, and clears the failure once one answers", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let reads503 = 1;
    server(reads(reviewed("clean"), rollback.receipt, { [RUN]: () => (reads503-- > 0 ? { status: 503, body: null } : { body: { run: reviewed("clean") } }) }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await screen.findByText(`GET /api/v1/gate-runs/${REVIEW} answered 503`)).toBeDefined();
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(await (await production()).findByRole("button", { name: "Promote v1.3" })).toBeDefined();
    expect(screen.queryByText(`GET /api/v1/gate-runs/${REVIEW} answered 503`)).toBeNull();
  });

  it("reads no more once the page is gone, though a read answers after", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let late: (one: Answer) => void = () => {};
    const sent = server(reads(reviewed("running"), rollback.receipt, { [RUN]: () => new Promise<Answer>((resolve) => { late = resolve; }) }));
    const page = render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await vi.waitFor(() => expect(sent()).toContain(RUN));
    page.unmount();
    const before = sent().length;
    await act(async () => late({ body: { run: reviewed("running") } }));
    await vi.advanceTimersByTimeAsync(10_000);
    expect(sent().length).toBe(before);
  });

  it("asks nothing more, and says nothing, when the page is gone before Put back is answered", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const errors = vi.spyOn(console, "error");
    let started: (one: Answer) => void = () => {};
    const sent = server(reads(reviewed("running"), rollback.receipt, { [PUT_BACK]: () => new Promise<Answer>((resolve) => { started = resolve; }) }));
    const page = render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await screen.findByText("Putting back…");
    page.unmount();
    const before = sent().length;
    await act(async () => started({ status: 202, body: { run: { id: REVIEW, state: "pending" } } }));
    await vi.advanceTimersByTimeAsync(10_000);
    expect(sent().length).toBe(before);
    expect(errors).not.toHaveBeenCalled();
  });

  it("stops reading the review once the page is gone", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const sent = server(reads(reviewed("running")));
    const page = render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await screen.findByText(reviewing("v1.3"));
    page.unmount();
    const before = sent().length;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(sent().length).toBe(before);
  });
});

describe("after the review", () => {
  // Like every production promote since #157: a question naming the app, the version and what it replaces, the keyboard on
  // Cancel, so Enter twice promotes nothing (design/EXPERIENCE.md, the question before production changes).
  it("asks before promoting, Cancel selected, naming the app, the version and what it replaces", async () => {
    const sent = server(reads(reviewed("clean"), rollback.receipt, {
      [PROMOTE]: { status: 202, body: { run: { id: "gr_p7", state: "pending" } } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=0": () => new Promise<Answer>(() => {}),
    }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    fireEvent.click(await card.findByRole("button", { name: "Promote v1.3" }));
    expect(card.getByText(APPROVAL.titlePutBack.replace("<app>", "claims-portal").replace("<version>", "v1.3").replace("<replaces>", "v1.4"))).toBeDefined();
    expect(document.activeElement).toBe(card.getByRole("button", { name: "Cancel" }));
    expect(posts(sent(), PROMOTE)).toBe(0);
    fireEvent.click(card.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(card.getByRole("button", { name: "Promote v1.3" }));
    expect(posts(sent(), PROMOTE)).toBe(0);
    fireEvent.click(card.getByRole("button", { name: "Promote v1.3" }));
    fireEvent.click(card.getByRole("button", { name: "Promote v1.3" }));
    await flush();
    expect(posts(sent(), PROMOTE)).toBe(1);
  });

  it("offers Promote v1.3 with the confirm's lines once the review passes, and promotes that run", async () => {
    const sent = server(reads(reviewed("clean"), rollback.receipt, {
      [PROMOTE]: { status: 202, body: { run: { id: "gr_p7", state: "pending" } } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=0": { body: { rows: [{ id: 1, kind: "build.log", data: { line: "deploying to production" } }], next: 1 } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=1": () => new Promise<Answer>(() => {}), // the promotion goes on
    }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    const promote = await card.findByRole("button", { name: "Promote v1.3" });
    expect(card.getByText(`${S.putBack.replace("<version>", "v1.3")} ${S.putBackReplaces.replace("<version>", "v1.4")}`)).toBeDefined();
    expect(card.getByText(S.putBackImage.replace("<date>", "2 Oct"))).toBeDefined();
    expect(card.getByText(S.putBackCode.replace("<version>", "v1.3").replace("<version>", "v1.4"))).toBeDefined();
    fireEvent.click(promote);
    const yes = card.getByRole("button", { name: "Promote v1.3" });
    fireEvent.click(yes);
    fireEvent.click(yes);
    expect(await card.findByText("deploying to production")).toBeDefined();
    expect(sent().filter((one) => one.startsWith(PROMOTE))).toEqual([`${PROMOTE} ${JSON.stringify({ gate_run: REVIEW })}`]);
    expect(document.activeElement).toBe(card.getByRole("list", { name: "Promotion" }).parentElement);
    expect(card.queryByRole("button", { name: "Close" })).toBeNull(); // the promotion has its own cancel
  });

  it("leaves the promotion's lines and drops the review once the promotion ends", async () => {
    server(reads(reviewed("clean"), rollback.receipt, {
      [PROMOTE]: { status: 202, body: { run: { id: "gr_p7", state: "pending" } } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=0": { body: { rows: [{ id: 1, kind: "build.log", data: { line: "live at http://claims-portal.prod.localhost:7777" } }], next: null } },
    }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    expect(await screen.findByText("live at http://claims-portal.prod.localhost:7777")).toBeDefined();
    expect(screen.queryByRole("link", { name: "the run →" })).toBeNull();
  });

  it("keeps a new Put back when the promotion before it ends", async () => {
    const both = { ...bare, earlier: [v13, { ...v12, kept: true }] };
    let done: (one: Answer) => void = () => {};
    let started: (one: Answer) => void = () => {};
    const routes = reads(reviewed("clean"), rollback.receipt, {
      "GET /api/v1/projects/claims-portal/status": { body: both },
      [PROMOTE]: { status: 202, body: { run: { id: "gr_p7", state: "pending" } } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=0": () => new Promise<Answer>((resolve) => { done = resolve; }),
    });
    server(routes);
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    await screen.findByRole("button", { name: "Cancel promotion" });
    routes[PUT_BACK] = () => new Promise<Answer>((resolve) => { started = resolve; });
    routes["GET /api/v1/gate-runs/gr_c2d5"] = () => new Promise<Answer>(() => {});
    await putBack("v1.2");
    await screen.findByText("Putting back…");
    await act(async () => {
      started({ status: 202, body: { run: { id: "gr_c2d5", state: "pending" } } });
      done({ body: { rows: [], next: null } });
    });
    await flush();
    expect(screen.getByText(reviewing("v1.2"))).toBeDefined();
    expect(screen.getByRole("link", { name: "the run →" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_c2d5");
  });

  it("says nothing is replaced when nothing runs in production", async () => {
    const { prod: _prod, ...preview } = bare.envs;
    server({ ...reads(reviewed("clean")), "GET /api/v1/projects/claims-portal/status": { body: { ...bare, envs: preview } } });
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    expect(await card.findByText(`${S.putBack.replace("<version>", "v1.3")} ${S.putBackNothing}`)).toBeDefined();
    expect(card.getByText(S.putBackCodeBare)).toBeDefined();
  });

  // The receipt carries ships here: only the run's own image_gone keeps Promote away.
  it("says why an image that is gone can't be put back, and offers no Promote", async () => {
    server(reads(reviewed("failed", "image_gone")));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await (await production()).findByText(S.imageGone.replaceAll("<version>", "v1.3"))).toBeDefined();
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
    expect(screen.queryByText(S.putBack.replace("<version>", "v1.3"), { exact: false })).toBeNull();
  });

  it("shows today's verdict and a disabled Promote with the receipt's reason when today's review blocks", async () => {
    const why = "v1.3's review found 1 blocking finding(s), not approved";
    server(reads(reviewed("blocked", undefined, [blocker]), failing("prod.clean-gate", why)));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    expect(await card.findByText(S.verdictBlocked)).toBeDefined();
    expect((card.getByRole("button", { name: "Promote v1.3" }) as HTMLButtonElement).disabled).toBe(true);
    expect(card.getByText(why)).toBeDefined();
    fireEvent.click(await card.findByRole("button", { name: "Close" }));
    expect(card.queryByText(S.verdictBlocked)).toBeNull();
  });

  // A review that blocks is approved on its run's own page, the run → (#2198): the card reads its receipt
  // again until it clears, then offers Promote for that run.
  it("offers Promote for the blocked review once its findings are approved on its run's page", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const why = "v1.3's review found 1 blocking finding(s), not approved";
    let receipt = failing("prod.clean-gate", why);
    const sent = server(reads(reviewed("blocked", undefined, [blocker]), rollback.receipt, {
      [`GET /api/v1/projects/claims-portal/status?run=${REVIEW}`]: () => ({ body: { ...rollback, promote_run: REVIEW, receipt } }),
      [PROMOTE]: { status: 202, body: { run: { id: "gr_p7", state: "pending" } } },
      "GET /api/v1/gate-runs/gr_p7/timeline?after=0": () => new Promise<Answer>(() => {}),
    }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    expect(await card.findByText(why)).toBeDefined();
    receipt = rollback.receipt; // a responder approved its blocking finding on the run's page
    await act(async () => { await vi.advanceTimersByTimeAsync(2000); });
    expect(await card.findByText(S.putBackImage.replace("<date>", "2 Oct"))).toBeDefined();
    fireEvent.click(card.getByRole("button", { name: "Promote v1.3" }));
    fireEvent.click(card.getByRole("button", { name: "Promote v1.3" }));
    await act(async () => { await vi.advanceTimersByTimeAsync(0); });
    expect(sent().filter((one) => one.startsWith(PROMOTE))).toEqual([`${PROMOTE} ${JSON.stringify({ gate_run: REVIEW })}`]);
  });

  // A hold of a production that ran nothing (its version null) reads held, never stopped, in Put back's disabled
  // Promote too: the page's own read of the hold, as its banner and card say it (#2696; D5's hold row).
  it("says held, never stopped, in its disabled Promote when production was held running nothing", async () => {
    const by = { kind: "person" as const, subject: "rhea@meridian.example", display: "Rhea Okafor" };
    const hold = { by, at: "2026-10-11T09:40:00Z", reason: "Exports claimant SSNs unauthenticated (INC-2231)" };
    const { prod: _prod, ...preview } = bare.envs;
    server({
      ...reads(reviewed("clean"), { ...rollback.receipt, held: hold }),
      "GET /api/v1/projects/claims-portal/status": { body: { ...bare, envs: preview, hold: { ...hold, version: null }, receipt: { ...bare.receipt, held: hold } } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const promote = await (await production()).findByRole("button", { name: "Promote v1.3" });
    expect((promote as HTMLButtonElement).disabled).toBe(true);
    expect(promote.nextElementSibling?.textContent).toBe(`Production was held by Rhea Okafor on 2026-10-11: ${hold.reason}. It stays held until a responder or an admin lifts it.`);
  });

  it("disables Promote with the rule's words while a person must promote", async () => {
    const why = "a person promotes, or an agent they ask, signed in as them";
    server(reads(reviewed("clean"), failing("prod.human-approval", why)));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    const card = await production();
    expect(await card.findByText(why)).toBeDefined();
    expect((card.getByRole("button", { name: "Promote v1.3" }) as HTMLButtonElement).disabled).toBe(true);
  });

  // The CLI's rule (design/specs/cli.md §6.3): a receipt that ships another image than the row production ran, or none, promotes nothing.
  it.each([
    ["another image", { ...rollback.receipt, ships: { version: "v1.3", image_digest: v14.image_digest } }],
    ["no image", atRest],
  ])("offers no Promote when the review ships %s", async (_case, receipt) => {
    server(reads(reviewed("clean"), receipt));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    expect(await (await production()).findByText(S.putBackNotThat.replace("<version>", "v1.3"))).toBeDefined();
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
  });

  it.each([
    ["outdated", "production runs another version now: review it again"],
    ["nothing_to_ship", "v1.3's review recorded no image to put back; review it again"],
  ])("says a %s refusal and takes the confirm away: it is reviewed again", async (error, message) => {
    server(reads(reviewed("clean"), rollback.receipt, { [PROMOTE]: { status: 409, body: { error, message } } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    expect(await screen.findByText(message)).toBeDefined();
    expect(screen.queryByRole("button", { name: "Promote v1.3" })).toBeNull();
  });

  it("says a run_in_progress refusal and keeps the confirm to ask again", async () => {
    const message = "another run of claims-portal is under way";
    const sent = server(reads(reviewed("clean"), rollback.receipt, { [PROMOTE]: { status: 409, body: { error: "run_in_progress", message, run: { id: "gr_x", state: "running" } } } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    expect(await screen.findByText(message)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Promote v1.3" }));
    await flush();
    expect(posts(sent(), PROMOTE)).toBe(2);
  });

  it("disables Promote with the reason of a refusal's receipt", async () => {
    const why = "v1.3 has a newer review than the one named";
    server(reads(reviewed("clean"), rollback.receipt, { [PROMOTE]: { status: 409, body: { error: "gate_not_clean", receipt: failing("prod.clean-gate", why) } } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    expect(await screen.findByText(why)).toBeDefined();
    expect((screen.getByRole("button", { name: "Promote v1.3" }) as HTMLButtonElement).disabled).toBe(true);
  });

  // A refusal whose body is not JSON leaves the card working: its line is said and the button asks again.
  it("survives a refusal with no body", async () => {
    const sent = server(reads(reviewed("clean"), rollback.receipt, { [PROMOTE]: { status: 409, body: null } }));
    render(<App id="claims-portal" auth="oidc" />);
    await putBack();
    await promoteIt();
    expect(await screen.findByText("POST /api/v1/projects/claims-portal/promotions answered 409")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Promote v1.3" }));
    await flush();
    expect(posts(sent(), PROMOTE)).toBe(2);
  });
});

// Production down leads with its last lines and the newest kept version to put back (#188): the button does what
// that version's Put back on the card does, and the card follows it.
describe("the headline of a production that's down", () => {
  const down = (prod: Partial<Env>, earlier = rollback.earlier): Status => ({ ...bare, envs: { ...bare.envs, prod: { ...(bare.envs.prod as Env), ...prod } }, earlier });
  const stopped: Partial<Env> = { state: "stopped", stopped: { exit_code: 1, said: "boom" } };
  const restarting: Partial<Env> = { state: "restarting", restarting: { restarts: 3, exit_code: 1, said: "boom" } };
  // The masked read of the environment's last lines answers text, as the server does.
  const logs = (env: "preview" | "prod") => {
    const LOGS = `GET /api/v1/projects/claims-portal/logs?env=${env}&lines=20&redaction=masked`;
    const routed = vi.mocked(fetch);
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => (`${init?.method} ${url}` !== LOGS ? routed(url, init)
      : { ok: true, status: 200, headers: new Headers({ "Canyon-Log-Redaction": "masked" }), text: async () => "dialing <DATABASE_URL>\nboom\n" } as unknown as Response)));
  };
  const band = async () => within(await screen.findByRole("region", { name: "Where it stands" }));

  it.each([["stopped", stopped], ["restarting", restarting]])("shows a %s production's last lines and puts the newest kept version back", async (_state, prod) => {
    const sent = server({ ...reads(reviewed("pending")), "GET /api/v1/projects/claims-portal/status": { body: down(prod) } });
    logs("prod");
    render(<App id="claims-portal" auth="oidc" />);
    expect(await (await band()).findByText(S.lastLines)).toBeDefined();
    expect((await band()).getByText((_, node) => node?.tagName === "PRE").textContent).toBe("dialing <DATABASE_URL>\nboom");
    if (prod === stopped) expect((await band()).queryByText(/canyon logs --env prod has the rest/)).toBeNull(); // the lines say why
    expect((await production()).getByText(/^exited 1|last exited 1/).closest("p")?.textContent).not.toContain("canyon logs has the rest"); // nor the card's
    fireEvent.click((await band()).getByRole("button", { name: "Put v1.3 back" })); // v1.4 patched and v1.2 aren't kept
    expect(await (await production()).findByText(reviewing("v1.3"))).toBeDefined();
    expect(posts(sent(), PUT_BACK)).toBe(1);
    expect(sent().find((one) => one.startsWith(PUT_BACK))).toContain(v13.package);
    expect((screen.getByRole("region", { name: "Earlier versions" }).closest("details") as HTMLDetailsElement).open).toBe(true);
  });

  it("says no earlier version can be put back where none is kept, with no button", async () => {
    server({ ...reads(reviewed("pending")), "GET /api/v1/projects/claims-portal/status": { body: down(stopped, rollback.earlier!.map((one) => ({ ...one, kept: false }))) } });
    logs("prod");
    render(<App id="claims-portal" auth="oidc" />);
    expect(await (await band()).findByText(new RegExp(S.putBackNone.replace(/[.:]/g, "\\$&")))).toBeDefined();
    expect(screen.queryByRole("button", { name: /^Put .* back$/ })).toBeNull();
  });

  // Production up says nothing of putting back.
  it("offers no headline Put back while production runs", async () => {
    server(reads(reviewed("clean")));
    render(<App id="claims-portal" auth="oidc" />);
    await (await production()).findByRole("button", { name: "Put back v1.3" });
    expect(screen.queryByRole("button", { name: /^Put .* back$/ })).toBeNull();
    expect(screen.queryByText(S.putBackNone)).toBeNull();
  });

  // A stopped preview shows its last lines too, with nothing to put back.
  it("shows a stopped preview's last lines", async () => {
    server({ ...reads(reviewed("clean")), "GET /api/v1/projects/claims-portal/status": { body: { ...bare, envs: { ...bare.envs, preview: { ...(bare.envs.preview as Env), ...stopped } } } } });
    logs("preview");
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByText(S.lastLines)).toBeDefined();
    expect(screen.queryByRole("button", { name: /^Put .* back$/ })).toBeNull();
  });
});
