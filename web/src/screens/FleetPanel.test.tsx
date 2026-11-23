import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AuditPage, Env, FleetPage, FleetRow, Receipt, RunRead } from "../api";
import { S } from "../strings";
import { Fleet } from "./Fleet";

const golden = JSON.parse(
  readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8"),
) as FleetPage;
// The app page's golden status read: its receipt is what the panel's headline weighs a promotion by.
const receipt = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: { receipt: Receipt } }).status.receipt;
// The trail as the app page's golden carries it: its first page is claims-portal's newest changes.
const trail = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { audit: AuditPage[] }).audit[0]!;
const run: RunRead = { id: "gr_8f2a", package: "pkg_9e40", state: "blocked", blocking: 1, warnings: 2, findings: [] };
const portal = golden.apps.find((app) => app.id === "claims-portal") as FleetRow;

// The fleet read is a function of its query; an app's status read answers what the fleet's row says
// runs there, and its hold; a run read answers `gateRun`; the trail answers the golden's newest page for one app;
// an app's outbound read answers what `refused` says production waits on, pending (#2723).
function server(fleet: (query: URLSearchParams) => FleetPage, gateRun: { ok?: boolean; status?: number; body?: unknown } = {}, refused: Record<string, string[]> = {}) {
  let last = golden;
  const fetch = vi.fn(async (url: string) => {
    const json = (body: unknown, status = 200) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;
    const at = new URL(url, "http://canyon");
    if (at.pathname === "/api/v1/fleet") return json((last = fleet(at.searchParams)));
    if (at.pathname === "/api/v1/audit") return json(at.searchParams.has("project") ? trail : { events: [], total: 0, page: 1, next: null });
    const status = at.pathname.match(/^\/api\/v1\/projects\/([^/]+)\/status$/);
    if (status !== null) {
      const row = last.apps.find((app) => app.id === decodeURIComponent(status[1]!)) as FleetRow;
      // The receipt's held carries no version (contracts/routes.md, The hold): only the status read's hold says what production ran.
      const held = row.hold === undefined ? undefined : { by: row.hold.by, at: row.hold.at, reason: row.hold.reason };
      return json({ project: row.id, envs: row.envs, last_run: row.last_run, promote_run: null, receipt: held === undefined ? receipt : { ...receipt, held }, ...(row.hold === undefined ? {} : { hold: row.hold }), needs: row.needs ?? [] });
    }
    const egress = at.pathname.match(/^\/api\/v1\/projects\/([^/]+)\/egress$/);
    if (egress !== null) {
      const requests = (refused[decodeURIComponent(egress[1]!)] ?? []).map((destination) => ({ destination, state: "pending", first_refused_at: "2026-11-08T09:00:00Z", last_refused_at: "2026-11-08T09:00:00Z", refusals: 3, refusing: true }));
      return json({ since: "2026-10-09T00:00:00Z", floor: "enforced", envs: { preview: { requests: [] }, prod: { requests } }, gaps: [], platform: [] });
    }
    // A run read answers that run: its own id, its app's newest package, and how its stage reads.
    const id = at.pathname.split("/").pop()!;
    const row = last.apps.find((app) => (app as FleetRow).last_run === id) as FleetRow | undefined;
    const own = row === undefined ? run : { ...run, id, package: row.latest.package!, state: row.latest.stage === "blocked" ? "blocked" as const : "clean" as const, blocking: row.latest.stage === "blocked" ? 1 : 0 };
    const { ok = true, status: code = 200, body = { run: own } } = gateRun;
    return { ok, status: code, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url]) => url as string);
}

const swap = (id: string, fields: Partial<FleetRow>) =>
  ({ ...golden, apps: golden.apps.map((app) => (app.id === id ? { ...app, ...fields } : app)) }) as FleetPage;
const openPanel = (name: string) => fireEvent.click(screen.getByRole("button", { name: `Details for ${name}` }));
const panel = (name: string) => screen.getByRole("complementary", { name: `Details for ${name}` });
const inPanel = (name: string) => within(panel(name));
const band = async (name: string) => (await inPanel(name).findByRole("region", { name: "Where it stands" })).textContent;
const bandNow = (name: string) => inPanel(name).getByRole("region", { name: "Where it stands" }).textContent; // under fake timers, which findBy would wait on forever
const runReads = (urls: string[]) => urls.filter((url) => url.startsWith("/api/v1/gate-runs/"));
const statusReads = (urls: string[]) => urls.filter((url) => url.endsWith("/status"));
const fleetReads = (urls: string[]) => urls.filter((url) => url.startsWith("/api/v1/fleet"));

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the fleet's side panel", () => {
  // The approved redesign: the pane is the app page's top block, so it says what the row can't —
  // where the newest version stands in a sentence, and the next step — never the row again.
  it("leads with the app page's headline and its next step, which opens the app", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await band("claims-portal")).toMatch(/^v1\.4 is blocked at review/);
    openPanel("quote-engine");
    expect(await band("quote-engine")).toContain("2b6d hasn't been reviewed");
    expect(inPanel("quote-engine").getByRole("link", { name: "Open review" }).getAttribute("href")).toBe("#/apps/quote-engine");
    expect(inPanel("quote-engine").getByRole("link", { name: "Open app →" }).getAttribute("href")).toBe("#/apps/quote-engine");
  });

  // A failed build's band offers Copy for your agent on the app page: the pane's step opens it there.
  it("sends a failed build to the app page to see what broke", async () => {
    server(() => golden, { body: { run: { ...run, id: portal.last_run, package: portal.latest.package, state: "failed", failure_reason: "build", blocking: 0 } } });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await band("claims-portal")).toMatch(/then didn't build/);
    expect(inPanel("claims-portal").getByRole("link", { name: "See what broke" }).getAttribute("href")).toBe("#/apps/claims-portal"); // the newest run, never pinned: its headline carries the copy
  });

  // The pane shows the most important info and the actions. A blocked version names its first open
  // blocking problem with its fix, and offers the review and the agent's copy, as the app page does.
  it("offers a blocked version's review and its agent's copy, naming the problem and its fix", async () => {
    const finding = {
      ruleId: "secrets.client-bundle",
      locations: [{ physicalLocation: { artifactLocation: { uri: "src/lib/stripe.ts" }, region: { startLine: 12, startColumn: 1, snippet: { text: "const key = 'sk_live'" } } } }],
      partialFingerprints: { "canyon/v1": "fp1" },
      properties: { canyon: { severity: "critical", blocks: true, title: "A Stripe secret key in browser code", problem: "The live key ships to every visitor.", fix: "Move the call to a server route." } },
    };
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    server(() => golden, { body: { run: { ...run, findings: [finding] } } });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    await band("claims-portal");
    expect(inPanel("claims-portal").getByRole("link", { name: "Open review" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_8f2a");
    expect(inPanel("claims-portal").getByText("A Stripe secret key in browser code").closest("p")!.textContent).toBe("A Stripe secret key in browser code · src/lib/stripe.ts:12. Move the call to a server route.");
    fireEvent.click(inPanel("claims-portal").getByRole("button", { name: "Copy for your agent" }));
    expect(await inPanel("claims-portal").findByText("Copied for your agent.")).toBeDefined();
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining("src/lib/stripe.ts"));
  });

  it("offers a blocked version's review even with no finding to name", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await band("claims-portal")).toMatch(/^v1\.4 is blocked at review/);
    expect(inPanel("claims-portal").getByRole("link", { name: "Open review" })).toBeDefined();
  });

  it("says a promotion is ready where the newest version runs in preview and meets the go-live checks", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("endorsement-hub");
    expect(await band("endorsement-hub")).toMatch(/^v1\.0 is ready for production.*It passed review/);
    expect(inPanel("endorsement-hub").getByRole("link", { name: "Promote" }).getAttribute("href")).toBe("#/apps/endorsement-hub");
  });

  // A version that meets its checks but answered 400 at / keeps Promote first: a review can't change the answer.
  it("keeps Promote first where the newest version meets its checks but answered 400 at /", async () => {
    const hub = golden.apps.find((app) => app.id === "endorsement-hub") as FleetRow;
    server(() => swap("endorsement-hub", { envs: { ...hub.envs, preview: { ...hub.envs.preview!, answered: { status: 400, said: "Bad Request" } } as Env } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("endorsement-hub");
    expect(await band("endorsement-hub")).toMatch(/^v1\.0 meets .*, but answered 400 at \//);
    expect(inPanel("endorsement-hub").getAllByRole("link").map((one) => one.textContent).filter((text) => text === "Promote" || text === "Open review")).toEqual(["Promote"]);
  });

  // The pane offers only steps that fit what its band says (#2723): a version that keeps restarting or a held
  // production has no review to open, and Approve with a reason is offered only to a reader who may approve.
  it("offers only the steps that fit the band: no review for a restart or a hold, no Approve to a reader who can't", async () => {
    const hub = golden.apps.find((app) => app.id === "endorsement-hub") as FleetRow;
    const hold = { by: { kind: "person" as const, subject: "rhea@meridian.example", display: "Rhea Okafor" }, at: "2026-11-08T09:00:00Z", reason: "Leaking claim PDFs; stop it while we look.", version: "v5.0" };
    server(() => ({ ...swap("endorsement-hub", { envs: { ...hub.envs, preview: { ...hub.envs.preview!, state: "restarting", restarting: { restarts: 3 } } as Env } }),
      apps: swap("endorsement-hub", { envs: { ...hub.envs, preview: { ...hub.envs.preview!, state: "restarting", restarting: { restarts: 3 } } as Env } }).apps.map((app) => (app.id === "premium-ledger" ? { ...app, hold } : app)) }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("endorsement-hub");
    expect(await band("endorsement-hub")).toMatch(/^v1\.0 restarted 3 times since it was deployed to preview/);
    expect(inPanel("endorsement-hub").queryByRole("link", { name: "Open review" })).toBeNull();
    openPanel("premium-ledger");
    expect(await band("premium-ledger")).toMatch(/^Production is held/);
    expect(inPanel("premium-ledger").queryByRole("link", { name: "Open review" })).toBeNull();
    openPanel("claims-portal");
    expect(await band("claims-portal")).toMatch(/^v1\.4 is blocked at review/);
    expect(inPanel("claims-portal").getByRole("link", { name: "Open review" })).toBeDefined(); // a review to open is still offered
  });

  it("offers Approve with a reason only to a reader the run lets approve", async () => {
    const finding = {
      ruleId: "secrets.client-bundle", entry: 0,
      locations: [{ physicalLocation: { artifactLocation: { uri: "src/lib/stripe.ts" }, region: { startLine: 12, startColumn: 1, snippet: { text: "const key = 'sk_live'" } } } }],
      partialFingerprints: { "canyon/v1": "fp1" },
      properties: { canyon: { severity: "critical", blocks: true, title: "A Stripe secret key in browser code", problem: "The live key ships to every visitor.", fix: "Move the call to a server route." } },
    };
    for (const [approvable, offered] of [[undefined, false], [true, true]] as const) {
      server(() => golden, { body: { run: { ...run, id: portal.last_run, package: portal.latest.package, findings: [finding], ...(approvable === undefined ? {} : { approvable }) } } });
      const { unmount } = render(<Fleet org="Meridian Insurance" />);
      await screen.findByText("premium-ledger");
      openPanel("claims-portal");
      expect(await band("claims-portal")).toMatch(/^v1\.4 is blocked: 1 blocking finding/);
      expect(inPanel("claims-portal").queryByRole("link", { name: "Approve with a reason" }) !== null).toBe(offered);
      unmount();
    }
  });

  // A held production: the pane's band says the hold from the status read's own, held where production ran nothing,
  // its reason ended once; production's card says Held and since when, never "Nothing is in production" and never the
  // band's sentence again (#2723).
  it("says a held production in its band and, briefly, on its card", async () => {
    const hold = { by: { kind: "person" as const, subject: "rhea@meridian.example", display: "Rhea Okafor" }, at: "2026-11-08T09:00:00Z", reason: "Leaking claim PDFs; stop it while we look.", version: null };
    server(() => swap("claims-portal", { hold }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await band("claims-portal")).toBe("Production is heldProduction was held by Rhea Okafor on 2026-11-08: Leaking claim PDFs; stop it while we look. It stays held until a responder or an admin lifts it.");
    const prod = inPanel("claims-portal").getByRole("region", { name: "Production" });
    expect(prod.textContent).toBe("ProductionHeld · since 2026-11-08");
  });

  // Live is never said in green while production can't reach a destination nobody has allowed (#2723): the pane says
  // it as the app page does, and its step opens the Outbound card.
  it("says what production can't reach in place of live, with the way to Allow", async () => {
    server(() => golden, {}, { "premium-ledger": ["api.stripe.com"] });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("premium-ledger");
    await waitFor(() => expect(bandNow("premium-ledger")).toMatch(/^v5\.0 can't reach api\.stripe\.com in productionNobody has allowed api\.stripe\.com in production yet/));
    expect(inPanel("premium-ledger").getByRole("link", { name: "Open Outbound" }).getAttribute("href")).toBe("#/apps/premium-ledger/outbound");
  });

  // The golden's claims-archive prod has stopped: production is down, its card says so and opens nothing.
  it("says a stopped environment stopped", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("claims-archive");
    openPanel("claims-archive");
    const prod = within(await inPanel("claims-archive").findByRole("region", { name: "Production" }));
    expect(prod.getByText("Stopped")).toBeDefined();
    expect(prod.queryByRole("link")).toBeNull();
    expect(await band("claims-archive")).toMatch(/^Production is down/);
  });

  it("opens the row whose button was pressed, with that row's own facts", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(inPanel("claims-portal").getByText(/Express · SQLite/)).toBeDefined();
    expect(inPanel("claims-portal").getByRole("link", { name: "Dana W." })).toBeDefined();
    expect(screen.queryByRole("complementary", { name: "Details for commission-calc" })).toBeNull();
  });

  it("draws an environment waiting on its first release by the release it waits on", async () => {
    const pending = { via: "webhook" as const, release: "rel_8f2a", event: "deploy" as const, sent_at: "2026-09-27T10:00:00Z", state: "refused" as const };
    server(() => swap("claims-portal", { envs: { preview: { run_by: "org", pending } } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(within(await inPanel("claims-portal").findByRole("region", { name: "Preview" })).getByText("Your platform refused the release")).toBeDefined();
  });

  it("puts each environment in its own card, and says so where nothing runs", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    const preview = within(await inPanel("claims-portal").findByRole("region", { name: "Preview" }));
    expect(preview.getByRole("link", { name: "Open v1.3 in preview" }).getAttribute("href")).toBe((portal.envs.preview as Env).url); // what preview runs, not v1.4
    expect(inPanel("claims-portal").getByRole("region", { name: "Production" }).textContent).toBe("ProductionNothing runningNothing is in production.");
  });

  it("never shows one app's headline under another app's name", async () => {
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      const json = (body: unknown) => Promise.resolve(({ ok: true, status: 200, json: async () => body }) as unknown as Response);
      if (url.startsWith("/api/v1/fleet")) return json(golden);
      if (url.startsWith("/api/v1/audit")) return json({ events: [], total: 0, page: 1, next: null });
      if (url.includes("claims-portal/status")) return json({ project: "claims-portal", envs: portal.envs, last_run: portal.last_run, promote_run: null, receipt });
      if (url.includes(portal.last_run as string)) return json({ run });
      return new Promise<Response>(() => {}); // the second app's reads never come back
    }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await band("claims-portal")).toContain("v1.4");
    openPanel("premium-ledger");
    expect(inPanel("premium-ledger").queryByRole("region", { name: "Where it stands" })).toBeNull();
  });

  it("hands the keyboard to the panel, closes on Escape, and gives it back to the row", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(document.activeElement).toBe(panel("claims-portal"));
    fireEvent.keyDown(panel("claims-portal"), { key: "Escape" });
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Details for claims-portal" }));
  });

  it("reads the row's newest run and its status, and no run for an app no run has looked at", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    await band("claims-portal");
    expect(runReads(sent())).toEqual(["/api/v1/gate-runs/gr_8f2a"]);
    expect(statusReads(sent())).toEqual(["/api/v1/projects/claims-portal/status"]);
    openPanel("quote-engine");
    await band("quote-engine");
    expect(runReads(sent())).toHaveLength(1);
    expect(screen.getAllByText(S.neverReviewed)).toHaveLength(1);
  });

  // Fails closed: with the newest run unreadable, the status alone never says ready, live or what to do.
  it("says why the run read failed, and no headline in its place", async () => {
    server(() => golden, { ok: false, status: 503, body: null });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("endorsement-hub");
    expect(await inPanel("endorsement-hub").findByText("GET /api/v1/gate-runs/gr_7e31 answered 503")).toBeDefined();
    await inPanel("endorsement-hub").findByRole("region", { name: "Preview" });
    expect(inPanel("endorsement-hub").queryByRole("region", { name: "Where it stands" })).toBeNull();
    expect(inPanel("endorsement-hub").queryByRole("link", { name: "Promote" })).toBeNull();
  });

  it("opens production where the newest version is live there", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("premium-ledger");
    await band("premium-ledger");
    const prod = golden.apps.find((app) => app.id === "premium-ledger") as FleetRow;
    expect(inPanel("premium-ledger").getByRole("link", { name: "Open production" }).getAttribute("href")).toBe((prod.envs.prod as Env).url);
  });

  // A new run of the same version: until it is read, the last run's verdict is never said in its place.
  it("says nothing of a run it hasn't read yet, whatever the last run said", async () => {
    vi.useFakeTimers();
    let last = portal.last_run as string;
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      const json = (body: unknown) => Promise.resolve(({ ok: true, status: 200, json: async () => body }) as unknown as Response);
      if (url.startsWith("/api/v1/fleet")) return json(swap("claims-portal", { last_run: last }));
      if (url.startsWith("/api/v1/audit")) return json({ events: [], total: 0, page: 1, next: null });
      if (url.endsWith("/status")) return json({ project: "claims-portal", envs: portal.envs, last_run: last, promote_run: null, receipt });
      if (url.endsWith(portal.last_run as string)) return json({ run });
      return new Promise<Response>(() => {}); // the new run's read never comes back
    }));
    render(<Fleet org="Meridian Insurance" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    openPanel("claims-portal");
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(bandNow("claims-portal")).toMatch(/blocked/);
    last = "gr_again";
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // the fleet's next poll
    expect(inPanel("claims-portal").queryByRole("region", { name: "Where it stands" })).toBeNull();
  });

  it("says why the app's changes couldn't be read", async () => {
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const json = (body: unknown, status = 200) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;
      if (url.startsWith("/api/v1/fleet")) return json(golden);
      if (url.startsWith("/api/v1/audit?project")) return json(null, 503);
      if (url.startsWith("/api/v1/audit")) return json({ events: [], total: 0, page: 1, next: null });
      if (url.endsWith("/status")) return json({ project: "claims-portal", envs: portal.envs, last_run: portal.last_run, promote_run: null, receipt });
      return json({ run });
    }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(await inPanel("claims-portal").findByText("GET /api/v1/audit answered 503")).toBeDefined();
  });

  // A review that ends keeps its run's id and moves its version's stage: the panel reads the run again.
  it("says how a review ended once the row moves, never Reviewing still", async () => {
    vi.useFakeTimers();
    let state: RunRead["state"] = "running";
    let latest = { ...portal.latest, stage: "reviewing" as const, since: "2026-09-02T09:30:00Z" } as FleetRow["latest"];
    server(() => swap("claims-portal", { latest }), { get body() { return { run: { ...run, state } }; } });
    render(<Fleet org="Meridian Insurance" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    openPanel("claims-portal");
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(bandNow("claims-portal")).toMatch(/^Reviewing v1\.4/);
    state = "blocked";
    latest = { ...portal.latest };
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // the fleet's next poll
    expect(bandNow("claims-portal")).toMatch(/^v1\.4 is blocked/);
  });

  // An app that stops changes no run: the panel reads the status again when the row says so.
  // Codex after #1765: when what runs where moves, the last status read says nothing until the new one comes.
  it("says nothing of what runs while the moved row's status is read again", async () => {
    vi.useFakeTimers();
    const live = golden.apps.find((app) => app.id === "premium-ledger") as FleetRow;
    let prod = { ...(live.envs.prod as Env), state: "running" as const } as Env;
    let reads = 0;
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      const json = (body: unknown) => Promise.resolve(({ ok: true, status: 200, json: async () => body }) as unknown as Response);
      if (url.startsWith("/api/v1/fleet")) return json(swap("premium-ledger", { envs: { prod } }));
      if (url.startsWith("/api/v1/audit")) return json({ events: [], total: 0, page: 1, next: null });
      if (url.endsWith("/status")) return ++reads === 1 ? json({ project: "premium-ledger", envs: { prod }, last_run: live.last_run, promote_run: null, receipt }) : new Promise<Response>(() => {});
      return json({ run: { ...run, id: live.last_run!, package: live.latest.package!, state: "clean", blocking: 0 } });
    }));
    render(<Fleet org="Meridian Insurance" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    openPanel("premium-ledger");
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(bandNow("premium-ledger")).toMatch(/^v5\.0 is live in production/);
    prod = { ...prod, state: "stopped" };
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // the fleet's next poll
    expect(inPanel("premium-ledger").queryByRole("region", { name: "Where it stands" })).toBeNull(); // never "live" from the last read
  });

  it("says production is down once the row says it stopped", async () => {
    vi.useFakeTimers();
    const live = golden.apps.find((app) => app.id === "premium-ledger") as FleetRow;
    let prod = { ...(live.envs.prod as Env), state: "running" as const } as Env;
    server(() => swap("premium-ledger", { envs: { prod } }), { body: { run: { ...run, id: "gr_4b60", package: "pkg_6f05", state: "clean", blocking: 0 } } });
    render(<Fleet org="Meridian Insurance" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    openPanel("premium-ledger");
    await act(() => vi.advanceTimersByTimeAsync(0));
    expect(bandNow("premium-ledger")).toMatch(/^v5\.0 is live in production/);
    prod = { ...prod, state: "stopped" };
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // the fleet's next poll
    expect(bandNow("premium-ledger")).toMatch(/^Production is down/);
  });

  // Beside the pane the table keeps what is read down the list; the pane says the rest for its row.
  it("folds the version, owner, updated and access columns away while the pane is open", async () => {
    server(() => ({ ...golden, banner: "none", viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const heads = () => screen.getAllByRole("columnheader").map((one) => one.textContent);
    expect(heads()).toEqual(["App", "Version", "Review", "Preview", "Production", "Owner", "Updated", "Access", ""]);
    openPanel("claims-portal");
    expect(heads()).toEqual(["App", "Review", "Preview", "Production", ""]); // Updated too: at 1280 the row's way into its pane was cut off (the UX review)
    fireEvent.click(inPanel("claims-portal").getByRole("button", { name: "Close" }));
    expect(heads()).toHaveLength(9);
  });

  it("lists the app's last few changes in plain words, and links its whole trail", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    const changes = within(await inPanel("claims-portal").findByRole("region", { name: "Recent changes" }));
    // One line per run, as the fleet's Recent activity tells it, and the app's own pane never names the app again.
    expect(changes.getAllByRole("listitem").map((row) => [...row.querySelectorAll("p")].map((one) => one.textContent).join(" — "))).toEqual([
      "Dana's agent sent v1.4 for review. — Blocked · 1 blocking",
      "Dana's agent deployed v1.3 to preview.",
    ]);
    for (const row of changes.getAllByRole("listitem")) expect(row.querySelector("p")!.textContent).not.toMatch(/^\s*[·|]/);
    expect(changes.getByRole("link", { name: "All activity" }).getAttribute("href")).toBe("#/audit?project=claims-portal");
    const asked = new URL(sent().find((url) => url.startsWith("/api/v1/audit?project="))!, "http://canyon").searchParams;
    expect([asked.get("project"), asked.get("limit")]).toEqual(["claims-portal", "20"]); // enough rows to tell five runs whole
    expect(asked.get("action")?.split(",")).not.toContain("sign in"); // the server narrows, never the browser
  });

  // The panel is a view of the row the server keeps re-answering: a new run is read again, an unchanged one never.
  it("reads a review once while it stays the newest, and again when the row's newest one changes", async () => {
    vi.useFakeTimers();
    let last = portal.last_run as string;
    const sent = server(() => swap("claims-portal", { last_run: last }));
    render(<Fleet org="Meridian Insurance" />);
    await act(() => vi.advanceTimersByTimeAsync(0));
    openPanel("claims-portal");
    await act(() => vi.advanceTimersByTimeAsync(6000));
    expect(runReads(sent())).toHaveLength(1);
    expect(statusReads(sent())).toHaveLength(1);
    last = "gr_newer";
    await act(() => vi.advanceTimersByTimeAsync(10_000)); // the fleet's next poll
    expect(runReads(sent())).toContain("/api/v1/gate-runs/gr_newer");
    expect(statusReads(sent())).toHaveLength(2);
  });

  it("leaves no panel when the server's next answer no longer carries that app", async () => {
    server((query) => (query.get("tile") === "unowned" ? { ...golden, apps: golden.apps.filter((app) => app.id === "quote-engine") } : golden));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(panel("claims-portal")).toBeDefined();
    window.location.hash = "#/fleet?tile=unowned";
    fireEvent(window, new HashChangeEvent("hashchange"));
    await waitFor(() => expect(screen.queryByRole("complementary")).toBeNull());
  });

  // Selection is React state: the hash is the fleet read's query, and a key the server does not
  // know would be sent to it.
  it("opens and closes without asking the fleet again or touching the address bar", async () => {
    const sent = server(() => golden);
    window.location.hash = "#/fleet?stage=blocked";
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("claims-portal");
    const before = fleetReads(sent()).length;
    openPanel("claims-portal");
    expect(window.location.hash).toBe("#/fleet?stage=blocked");
    fireEvent.click(inPanel("claims-portal").getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(window.location.hash).toBe("#/fleet?stage=blocked");
    expect(fleetReads(sent())).toHaveLength(before);
  });

  // Two people under one display name: the link can only be right if it is built from the subject.
  it("links every owner of the row, by subject", async () => {
    server(() => swap("claims-portal", { owners: [
      { kind: "person", subject: "dana@meridian.example", display: "Dana Whitfield" },
      { kind: "person", subject: "dana.whitfield@meridian.example", display: "Dana Whitfield" },
    ] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    expect(inPanel("claims-portal").getAllByRole("link", { name: "Dana W." }).map((link) => link.getAttribute("href")))
      .toEqual(["#/fleet?owner=dana%40meridian.example", "#/fleet?owner=dana.whitfield%40meridian.example"]);
  });

  // The approved redesign's triage keys: j and k move the pane down and up the rows that open one, Space
  // opens or closes it where the keys are, Enter opens that app, Esc closes — only with the keyboard in
  // the table or its pane (WCAG 2.1.4), never while typing.
  const keysOn = () => screen.getByRole("group", { name: "Apps" });
  const key = (name: string, on: Element = document.activeElement ?? document.body) => fireEvent.keyDown(on, { key: name });
  it("moves the pane down and up the list with j and k, peeks with Space and opens the app with Enter", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const team = golden.apps.map((app) => app.name);
    keysOn().focus();
    key("j");
    expect(panel(team[0]!)).toBeDefined();
    key("j");
    expect(panel(team[1]!)).toBeDefined();
    key("k");
    expect(panel(team[0]!)).toBeDefined();
    expect(key(" ")).toBe(false); // the page doesn't scroll under it
    expect(screen.queryByRole("complementary")).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: `Details for ${team[0]}` })); // the keyboard back on its row
    key(" ", keysOn());
    expect(panel(team[0]!)).toBeDefined(); // back on the row the keys were on
    key("Escape", keysOn()); // the table's own Escape, not the pane's
    expect(screen.queryByRole("complementary")).toBeNull();
    key("j", keysOn());
    key("Enter");
    expect(window.location.hash).toBe(`#/apps/${golden.apps[1]!.id}`);
  });

  it("gives the keyboard back to a row opened by a click when the keys close its pane", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const row = screen.getAllByRole("row").find((one) => one.textContent?.startsWith(golden.apps[0]!.name)) as HTMLTableRowElement;
    fireEvent.click(row.cells[2]!);
    expect(panel(golden.apps[0]!.name)).toBeDefined();
    key("Escape", keysOn());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: `Details for ${golden.apps[0]!.name}` }));
  });

  it("passes over a row that opens no pane", async () => {
    const hidden = { id: "hr-portal", name: "hr-portal", owners: [], summary: "", access: { level: "none" } };
    server(() => ({ ...golden, apps: [golden.apps[0]!, hidden, golden.apps[1]!] } as FleetPage));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("hr-portal");
    keysOn().focus();
    key("j");
    key("j");
    expect(panel(golden.apps[1]!.name)).toBeDefined();
  });

  it("leaves the keys to a box being typed in, to the page outside the table, and Enter to a focused link", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    key("j", screen.getByRole("searchbox"));
    key("j", document.body);
    expect(screen.queryByRole("complementary")).toBeNull();
    keysOn().focus();
    key("j");
    const link = within(panel(golden.apps[0]!.name)).getByRole("link", { name: "Open app →" });
    key("Enter", link);
    expect(window.location.hash).toBe(""); // the link's own Enter, not the table's
    expect(screen.getByText(/move · /).textContent).toBe("j k move · Space peek · Enter open the app · Esc close");
    expect(keysOn().getAttribute("aria-describedby")).toBe("fleet-keys");
    // a box inside the table's pane: typing a j there types a j
    openPanel("quote-engine");
    fireEvent.click(inPanel("quote-engine").getByRole("button", { name: "Claim quote-engine" }));
    const name = inPanel("quote-engine").getByLabelText("Your name");
    expect(key("j", name)).toBe(true);
    expect(panel("quote-engine")).toBeDefined();
  });

  it("never puts the machine's words on screen with the panel open", async () => {
    server(() => golden);
    const { container } = render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    openPanel("claims-portal");
    await band("claims-portal");
    await inPanel("claims-portal").findByRole("region", { name: "Recent changes" });
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i);
  });
});

// The pane says what needs a person as the app page does (D15): production's green never beside a need of production.
describe("what needs a person, in the side pane", () => {
  it("lists a refused call of a live production, with its Allow, and never says live", async () => {
    const live = golden.apps.find((app) => app.id === "premium-ledger") as FleetRow;
    const refused = { kind: "refused", env: "prod" as const, rank: 3, since: "2026-11-08T09:00:00Z", you: true, ask: [], destination: "api.stripe.com", refusals: 4,
      last_refused_at: "2026-11-08T09:00:00Z", other_calls: 0, held: false };
    server(() => swap("premium-ledger", { needs: [refused], envs: { prod: { ...(live.envs.prod as Env), state: "running" } } }), { body: { run: { ...run, id: "gr_4b60", package: "pkg_6f05", state: "clean", blocking: 0 } } });
    render(<Fleet org="Meridian Insurance" />);
    openPanel(await screen.findByText("premium-ledger").then(() => "premium-ledger"));
    const needs = within(await inPanel("premium-ledger").findByRole("region", { name: "Needs you" }));
    expect(await needs.findByText("premium-ledger can't reach api.stripe.com in production.")).toBeDefined();
    expect(needs.getByRole("button", { name: "Allow api.stripe.com in production for premium-ledger" })).toBeDefined();
    expect(inPanel("premium-ledger").queryByText(/is live in production/)).toBeNull();
  });

  // An ownerless app's Claim is offered once: its need's line carries it, and the pane's head no second one.
  it("offers an ownerless app's Claim once", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    openPanel(await screen.findByText("quote-engine").then(() => "quote-engine"));
    await inPanel("quote-engine").findByText("quote-engine has no owner.");
    expect(inPanel("quote-engine").getAllByRole("button", { name: "Claim quote-engine" })).toHaveLength(1);
  });
});
