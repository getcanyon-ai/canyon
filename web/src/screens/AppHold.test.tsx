import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { App as AppRead, FleetPage, RunRead, Status, Viewer } from "../api";
import { S } from "../strings";
import { RailContext } from "../ui/Rail";
import { App } from "./App";

// A held production on the app page (design/OPERATIONS.md §3.7, §3.8): the banner above production says
// who stopped it, when and why, and Promote is disabled with that sentence (D6). The status read is the
// contract's held one; who reads it is the matrix of design/PERMISSIONS.md.
const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
const held = golden<Status>("app-page/claims-portal-held-status.json");
const refusal = golden<{ message: string }>("app-page/claims-portal-held-refusal.json");
const project = golden<FleetPage>("fleet/page.json").apps.find((app) => app.id === "claims-portal") as AppRead;
// The sentence the 409 held carries is the one the page builds from the read: the same D5 line.
const SENTENCE = refusal.message;
const run: RunRead = { id: "gr_8f2a", package: "pkg_9e40", state: "blocked", blocking: 0, warnings: 0, findings: [] };

function server(routes: Record<string, unknown>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const key = `${init?.method ?? "GET"} ${url}`;
    const found = key in routes;
    return { ok: found, status: found ? 200 : 404, json: async () => (found ? routes[key] : { error: "not_found" }) } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([, init]) => init?.method !== undefined && init.method !== "GET").map(([url, init]) => `${init?.method} ${url}`);
}
// The page as a reader of a role sees it: the rail's viewer is who the fleet read named.
const as = (role: Viewer["role"]) => (
  <RailContext.Provider value={{ org: "Meridian", viewer: { display: "Rhea Okafor", role, github_logins: [], github_link: false }, admin: role === "admin" }}>
    <App id="claims-portal" auth="oidc" />
  </RailContext.Provider>
);
const team = (level: "owner" | "contributor" | "none") => ({
  "GET /api/v1/projects/claims-portal": { project: { ...project, access: { ...project.access, level } } },
  "GET /api/v1/projects/claims-portal/status": held,
  "GET /api/v1/gate-runs/gr_8f2a": { run },
  "GET /api/v1/gate-runs/gr_6c12": { run: { ...run, id: "gr_6c12", package: "pkg_7a1c", state: "clean" } },
  "GET /api/v1/projects/claims-portal/access": { everyone: null, entries: [], roles: [], requests: [] },
});
const reducedRow = (level: "viewer" | "none") => ({ id: "claims-portal", name: "claims-portal", owners: project.owners, owner_groups: [], summary: project.summary, claimable: false, access: { level } });
const band = () => within(screen.getByRole("region", { name: "Where it stands" }));
const flush = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("a held production on the app page", () => {
  // The team and an admin read the hold: the banner says it. Preview runs the version production ran, which the hold
  // stopped: no Promote, which would change nothing, but what lifting the hold does; and the version is on its way
  // nowhere, so it has no row in Versions on their way (#2723). A responder on the team reads the app by their grant, as
  // its team does (design/PERMISSIONS.md).
  it.each([["an owner", "owner"], ["a contributor", "contributor"], ["an admin, or a responder on the team", "none"]] as const)("%s reads the banner, and what lifting the hold starts", async (_who, level) => {
    server(team(level));
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("heading", { name: "Production is held" })).toBeDefined();
    expect(band().getByText(SENTENCE)).toBeDefined();
    const promote = within(screen.getByRole("complementary", { name: "Promote" }));
    expect(promote.queryByRole("button", { name: /^Promote/ })).toBeNull();
    expect(promote.getByText("v1.3 is production's version, stopped by the hold. Lifting the hold can start it again.")).toBeDefined();
    const versions = within(screen.getByRole("region", { name: "Versions on their way" }));
    expect(versions.getAllByRole("rowheader").map((one) => one.textContent?.slice(0, 4))).toEqual(["v1.4"]);
  });

  // Preview runs a newer version than the one the hold stopped: Promote stays, disabled, its reason the hold's in brief,
  // the banner above saying the rest (D6; #2723).
  it("disables a newer version's Promote while held, saying so briefly", async () => {
    const newer = { ...held, envs: { preview: { ...held.envs.preview!, package: "pkg_9e40", label: "v1.4", gate_run: "gr_8f2a" } } };
    server({ ...team("contributor"), "GET /api/v1/projects/claims-portal/status": newer });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("heading", { name: "Production is held" })).toBeDefined();
    const promote = within(screen.getByRole("complementary", { name: "Promote" }));
    expect((promote.getByRole("button", { name: "Promote v1.4 to production" }) as HTMLButtonElement).disabled).toBe(true);
    expect(promote.getByText("Production is held: nothing is promoted until a responder or an admin lifts the hold.")).toBeDefined();
    expect(promote.queryByText(SENTENCE)).toBeNull();
  });

  // The team's status read with no hold says none: the banner is the read's, never a guess.
  it("a contributor whose status read carries no hold reads no banner", async () => {
    const { hold: _hold, ...free } = held;
    server({ ...team("contributor"), "GET /api/v1/projects/claims-portal/status": { ...free, receipt: { org: "Meridian", blocked_by: [], rules: [] } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByRole("complementary", { name: "Promote" })).toBeDefined();
    expect(screen.queryByText("Production is held")).toBeNull();
    expect(screen.queryByText(SENTENCE)).toBeNull();
  });

  // A responder off the team reads the hold beside what they may approve, and is never offered Promote.
  // The test cluster pass (#2258): the held card said to promote, while Promote was disabled by the hold.
  // The hold's reason is said once, in the banner; production's card says since when (#2754's review).
  it("production's card says it is held and since when, never to promote", async () => {
    server(team("owner"));
    render(<App id="claims-portal" auth="oidc" />);
    const card = within(await screen.findByRole("region", { name: "Production" }));
    expect(card.getByText(S.heldSince.replace("<date>", held.hold!.at.slice(0, 10)))).toBeDefined();
    expect(card.queryByText(SENTENCE)).toBeNull();
    expect(card.queryByText(/Promote a version/)).toBeNull();
  });

  // The test cluster pass (#2258): a responder reads nothing of preview, and the page said nothing ran there.
  it("a responder with no grant is told preview isn't theirs to read, never that it runs nothing", async () => {
    const { envs: _envs, earlier: _earlier, ...rest } = held;
    server({
      "GET /api/v1/projects/claims-portal": { project: { ...reducedRow("none"), envs: {}, hold: held.hold } },
      "GET /api/v1/preferences": { viewer: { email: "rhea@meridian.example", role: "responder" } },
      "GET /api/v1/projects/claims-portal/status": { ...rest, envs: {} },
      "GET /api/v1/gate-runs/gr_8f2a": { run },
    });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByText(S.responderAccess)).toBeDefined();
    const preview = within(screen.getByRole("region", { name: "Preview" }));
    expect(preview.queryByText("Nothing running")).toBeNull();
    expect(preview.queryByText(/Nothing is in preview/)).toBeNull();
    expect(preview.getByText("Not shown to you")).toBeDefined();
    expect(screen.queryByRole("heading", { name: "Not running" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Not running in production" })).toBeDefined();
    const prod = within(screen.getByRole("region", { name: "Production" }));
    expect(prod.getByText(S.heldSince.replace("<date>", held.hold!.at.slice(0, 10)))).toBeDefined();
    expect(prod.queryByText(SENTENCE)).toBeNull();
  });

  it("a responder with no grant reads the banner and no Promote", async () => {
    window.location.hash = "#/apps/claims-portal";
    const { envs: _envs, earlier: _earlier, ...rest } = held;
    server({
      "GET /api/v1/projects/claims-portal": { project: { ...reducedRow("none"), hold: held.hold } },
      "GET /api/v1/preferences": { viewer: { email: "rhea@meridian.example", role: "responder" } },
      "GET /api/v1/projects/claims-portal/status": { ...rest, envs: {} },
      "GET /api/v1/gate-runs/gr_8f2a": { run },
    });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findAllByText(SENTENCE)).toHaveLength(1); // the banner alone: production's card says since when
    expect(screen.getByText(S.responderAccess)).toBeDefined();
    expect(screen.queryByRole("button", { name: /^Promote/ })).toBeNull();
    expect(window.location.hash).toBe("#/apps/claims-portal");
  });

  // A hold is lifted from the app page as from the fleet's row (#2723): the same Lift the hold, to the same people, an
  // admin or a responder off the app's team, asking the same question before it sends the lift of the hold it read.
  it("an admin lifts the hold from the banner, asked first, as the fleet's row asks", async () => {
    const sent = server({ ...team("none"), [`DELETE /api/v1/projects/claims-portal/envs/prod/hold?held_at=${encodeURIComponent(held.hold!.at)}`]: {} });
    render(as("admin"));
    expect(await screen.findByRole("heading", { name: "Production is held" })).toBeDefined();
    fireEvent.click(band().getByRole("button", { name: "Lift the hold on claims-portal" }));
    const ask = within(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" }));
    expect(ask.getByText(SENTENCE)).toBeDefined();
    expect(ask.getByRole("button", { name: "Lift the hold and start v1.3" })).toBeDefined();
    fireEvent.click(ask.getByRole("button", { name: "Lift the hold, keep it stopped" }));
    await waitFor(() => expect(sent()).toEqual([`DELETE /api/v1/projects/claims-portal/envs/prod/hold?held_at=${encodeURIComponent(held.hold!.at)}`]));
  });

  // Sign-in off: no viewer is read, and the host's one caller is the anonymous admin, as the rail says (Shell).
  it("the anonymous admin of a host with sign-in off lifts it from the banner", async () => {
    server(team("none"));
    render(<RailContext.Provider value={{ org: "Meridian", viewer: null, admin: true }}><App id="claims-portal" auth="off" /></RailContext.Provider>);
    expect(await screen.findByRole("button", { name: "Lift the hold on claims-portal" })).toBeDefined();
  });

  // A hold someone else lifts while the question is open is said, and nothing is offered on it, as the fleet's question
  // says it (#2723): the page's next read has no hold, and the question names the one it read.
  it("says the hold changed when someone else lifts it while the question is open", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const routes: Record<string, unknown> = team("none");
      server(routes);
      render(as("admin"));
      fireEvent.click(await screen.findByRole("button", { name: "Lift the hold on claims-portal" }));
      const ask = within(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" }));
      expect(ask.getByRole("button", { name: "Lift the hold, keep it stopped" })).toBeDefined();
      const { hold: _hold, ...lifted } = held;
      routes["GET /api/v1/projects/claims-portal/status"] = { ...lifted, receipt: { org: "Meridian", blocked_by: [], rules: [] } };
      await act(async () => { await vi.advanceTimersByTimeAsync(2500); });
      await waitFor(() => expect(ask.getByRole("alert").textContent).toBe(S.holdChanged));
      expect(ask.queryByRole("button", { name: "Lift the hold, keep it stopped" })).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });

  it("a responder off the team lifts it from the banner beside what they may approve", async () => {
    const { envs: _envs, earlier: _earlier, ...rest } = held;
    server({
      "GET /api/v1/projects/claims-portal": { project: { ...reducedRow("none"), hold: held.hold } },
      "GET /api/v1/preferences": { viewer: { email: "rhea@meridian.example", role: "responder" } },
      "GET /api/v1/projects/claims-portal/status": { ...rest, envs: {} },
      "GET /api/v1/gate-runs/gr_8f2a": { run },
    });
    render(as("responder"));
    expect(await screen.findByRole("button", { name: "Lift the hold on claims-portal" })).toBeDefined();
  });

  // The team's owner, and a responder on the team, are refused a lift by the server: neither is offered one.
  it.each([["its owner", "owner", "member"], ["a responder on its team", "contributor", "responder"]] as const)("%s is never offered Lift", async (_who, level, role) => {
    server(team(level));
    render(as(role));
    expect(await screen.findByRole("heading", { name: "Production is held" })).toBeDefined();
    await flush();
    expect(screen.queryByRole("button", { name: /^Lift the hold/ })).toBeNull();
  });

  // Read-only: no status read is made, and the reduced row carries no hold, so a held production reads
  // exactly as one with nothing deployed; no hold, no reason.
  it("a read-only member, read no status, is shown nothing of a hold", async () => {
    server({ "GET /api/v1/projects/claims-portal": { project: reducedRow("viewer") }, "GET /api/v1/preferences": { viewer: { email: "tomas@meridian.example", role: "member" } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await screen.findByText(`${S.readOnly} ${S.readOnlyUse}`)).toBeDefined();
    await flush();
    expect(screen.queryByText("Production is held")).toBeNull();
    expect(screen.queryByText(SENTENCE)).toBeNull();
  });

  // No access: the no-access screen, never the page, and never the hold.
  it("someone with no access is sent away before anything of the hold is shown", async () => {
    window.location.hash = "#/apps/claims-portal";
    server({ "GET /api/v1/projects/claims-portal": { project: reducedRow("none") }, "GET /api/v1/preferences": { viewer: { email: "tomas@meridian.example", role: "member" } } });
    render(<App id="claims-portal" auth="oidc" />);
    await waitFor(() => expect(window.location.hash).toBe("#/no-access/claims-portal"));
    expect(screen.queryByText(SENTENCE)).toBeNull();
  });

  // Stopping production is a responder's or an admin's, from the app page as from the fleet (#102): the headline's step,
  // and the ⋯ menu's, through the same question, over the page; a responder is never offered to ask for access.
  describe("stopping production from the app page", () => {
    const { hold: _hold, ...running } = held;
    const live = { ...running, envs: { ...held.envs, prod: { ...held.envs.preview!, url: "https://claims-portal.prod.meridian.example" } } };
    const HOLD = "POST /api/v1/projects/claims-portal/envs/prod/hold";

    it("a responder with no grant stops production from the headline, and isn't offered to ask for access", async () => {
      const sent = server({
        "GET /api/v1/projects/claims-portal": { project: { ...reducedRow("none"), envs: { prod: live.envs.prod } } },
        "GET /api/v1/preferences": { viewer: { email: "rhea@meridian.example", role: "responder" } },
        "GET /api/v1/projects/claims-portal/status": live,
        "GET /api/v1/gate-runs/gr_8f2a": { run },
        [HOLD]: {},
      });
      render(as("responder"));
      const stop = await screen.findByRole("button", { name: "Stop production of claims-portal" });
      expect(band().getByRole("button", { name: "Stop production of claims-portal" })).toBe(stop); // the headline's step
      expect(band().queryByRole("button", { name: S.askForAccess })).toBeNull();
      fireEvent.click(stop);
      const ask = screen.getByRole("dialog", { name: "Stop and hold claims-portal in production?" });
      expect(ask.getAttribute("aria-modal")).toBe("true");
      expect(document.activeElement).toBe(within(ask).getByRole("textbox"));
      expect(within(ask).getByText(S.stopOrOffline)).toBeDefined();
      fireEvent.change(within(ask).getByRole("textbox"), { target: { value: "INC-2240" } });
      fireEvent.click(within(ask).getByRole("button", { name: "Stop production" }));
      await waitFor(() => expect(sent()).toEqual([HOLD]));
    });

    it("an admin holds production that runs nothing from the ⋯ menu, and Escape closes the question", async () => {
      server({ ...team("none"), "GET /api/v1/projects/claims-portal/status": running });
      render(as("admin"));
      fireEvent.click(await screen.findByRole("button", { name: "More actions" }));
      fireEvent.click(screen.getByRole("menuitem", { name: "Hold production…" }));
      const ask = screen.getByRole("dialog", { name: "Hold claims-portal's production?" });
      expect(within(ask).queryByText(S.stopOrOffline)).toBeNull(); // nothing runs to take offline
      fireEvent.keyDown(ask, { key: "Escape" });
      expect(screen.queryByRole("dialog")).toBeNull();
    });

    it("an owner neither stops nor holds production", async () => {
      server({ ...team("owner"), "GET /api/v1/projects/claims-portal/status": live });
      render(as("member"));
      await screen.findByRole("region", { name: "Where it stands" });
      fireEvent.click(screen.getByRole("button", { name: "More actions" }));
      expect(screen.queryByRole("menuitem", { name: /production…$/ })).toBeNull();
      expect(screen.queryByRole("button", { name: /^Stop production|^Hold production/ })).toBeNull();
    });
  });
});

