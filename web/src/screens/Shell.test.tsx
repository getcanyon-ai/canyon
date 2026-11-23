import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UNAUTHENTICATED, type FleetPage, type Preferences } from "../api";
import { AUTH, S } from "../strings";
import { Shell } from "./Shell";

const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8")) as FleetPage;
const signedIn: FleetPage = { ...golden, banner: "none", viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false } };
const prefs = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences }).preferences_admin;
const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;

// The server answers the fleet from a queue, one page per read, and the rest as the routes say.
// `row` is what the preferences read answers the shell's setup gate; the golden is an onboarded host.
function server(row: Preferences, ...fleet: (() => Response)[]) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
    if (url === "/api/v1/preferences" && init?.method !== "PUT") return answer(200, row);
    if (init?.method === "DELETE") return answer(204, null);
    if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
    return (fleet.length > 1 ? fleet.shift() : fleet[0])?.() ?? answer(404, null);
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`);
}

const signInPage = async (start = "/auth/start", button = "Continue to company sign-in") => {
  expect(await screen.findByText("Meridian Insurance")).toBeDefined();
  expect(screen.getByRole("heading", { name: "Sign in to Canyon" })).toBeDefined();
  expect(screen.getByRole("link", { name: button }).getAttribute("href")).toBe(start);
  expect(screen.queryByRole("table")).toBeNull();
  expect(screen.queryByText("commission-calc")).toBeNull();
};

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
  sessionStorage.clear();
});

describe("the shell", () => {
  it("names the org again each time the fleet is shown, so a rename is the heading on the way back", async () => {
    window.location.hash = "#/fleet";
    const names = ["Meridian Insurance", "Meridian UX"];
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "off", org: names.length > 1 ? names.shift() : names[0], device: false });
      if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
      if (url.startsWith("/api/v1/fleet")) return answer(200, golden);
      return answer(200, prefs);
    }));
    render(<Shell />);
    expect(await screen.findByRole("heading", { name: "Meridian Insurance" })).toBeDefined();
    window.location.hash = "#/audit"; // away from the fleet (the rename itself is under Guardrails)
    fireEvent(window, new HashChangeEvent("hashchange"));
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Meridian Insurance" })).toBeNull());
    window.location.hash = "#/fleet";
    fireEvent(window, new HashChangeEvent("hashchange"));
    expect(await screen.findByRole("heading", { name: "Meridian UX" })).toBeDefined();
  });

  it("replaces the fleet with the sign-in page on a 401, with no rows left behind it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true }); // findByText polls on real time; the fleet's 2s poll is jumped
    window.location.hash = "#/fleet";
    // Two pages before the 401: the rail's one read of who is signed in, and the fleet's own first read.
    const sent = server(prefs, () => answer(200, signedIn), () => answer(200, signedIn), () => answer(401, { error: "unauthenticated", message: "sign in to Canyon" }));
    render(<Shell />);
    expect(await screen.findByText("commission-calc")).toBeDefined();
    await vi.advanceTimersByTimeAsync(2000); // the poll: the session has expired
    await signInPage();
    expect(screen.getByText("Your session ended")).toBeDefined(); // a read in this tab was answered, so this one ended
    expect(screen.queryByText("You're signed out of Canyon")).toBeNull(); // nobody chose to sign out
    const before = sent().length;
    await vi.advanceTimersByTimeAsync(10000);
    expect(sent().length).toBe(before); // the screen behind it is gone, so nothing keeps polling
  });

  // The server answers the same 401 for no session and for one that ended: only a tab whose read was
  // answered had one to end (design/specs/screens.md, A0).
  it("says nothing about a session ending when the tab's first read answers 401", async () => {
    window.location.hash = "#/fleet";
    vi.stubGlobal("fetch", vi.fn(async (url: string) => url === "/auth/config"
      ? answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true })
      : answer(401, { error: "unauthenticated", message: "sign in to Canyon" })));
    render(<Shell />);
    await signInPage();
    expect(screen.queryByText("Your session ended")).toBeNull();
    expect(screen.queryByText("You're signed out of Canyon")).toBeNull();
  });

  // The run an audit row's view → names is the app page's to read; the app's id stops at the "?".
  it("opens #/apps/<id>?run= on that app, reading that run", async () => {
    window.location.hash = "#/apps/claims-portal?run=gr_6c12";
    // The app page reads its run only for a reader on the app's team, so the project read answers a team row.
    const status = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: unknown }).status;
    const egress = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as { project_read: unknown; setting_member: unknown };
    const sent = server(prefs, () => answer(200, signedIn));
    const routed = vi.mocked(fetch);
    const base = routed.getMockImplementation()!;
    routed.mockImplementation(async (url: string | URL | Request, init?: RequestInit) => {
      if (url === "/api/v1/projects/claims-portal") return answer(200, { project: golden.apps.find((app) => app.id === "claims-portal") });
      if (url === "/api/v1/projects/claims-portal/status") return answer(200, status);
      if (url === "/api/v1/projects/claims-portal/access") return answer(200, { everyone: null, entries: [], roles: [], requests: [] });
      // The Outbound card's two reads, the contract's: answered by the fleet's fallback, they crashed the card after the test
      if (url === "/api/v1/projects/claims-portal/egress") return answer(200, egress.project_read);
      if (url === "/api/v1/egress") return answer(200, egress.setting_member);
      if (String(url).startsWith("/api/v1/gate-runs/")) return answer(200, { run: { id: "gr_6c12", package: "pkg_7a1c", state: "clean", blocking: 0, warnings: 0, findings: [] } });
      return base(url, init);
    });
    render(<Shell />);
    await waitFor(() => expect(sent()).toContain("GET /api/v1/gate-runs/gr_6c12"));
    expect(sent()).toContain("GET /api/v1/projects/claims-portal");
  });

  // The link an egress refusal carries (contracts/routes.md, The check): the app's page, scrolled to its Outbound card.
  it("opens #/apps/<id>/outbound on that app, at its Outbound card", async () => {
    window.location.hash = "#/apps/claims-portal/outbound";
    const status = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: unknown }).status;
    const egress = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as { project_read: unknown; setting_member: unknown };
    const sent = server(prefs, () => answer(200, signedIn));
    const routed = vi.mocked(fetch);
    const base = routed.getMockImplementation()!;
    routed.mockImplementation(async (url: string | URL | Request, init?: RequestInit) => {
      if (url === "/api/v1/projects/claims-portal") return answer(200, { project: golden.apps.find((app) => app.id === "claims-portal") });
      if (url === "/api/v1/projects/claims-portal/status") return answer(200, status);
      if (url === "/api/v1/projects/claims-portal/access") return answer(200, { everyone: null, entries: [], roles: [], requests: [] });
      if (url === "/api/v1/projects/claims-portal/egress") return answer(200, egress.project_read);
      if (url === "/api/v1/egress") return answer(200, egress.setting_member);
      if (String(url).startsWith("/api/v1/gate-runs/")) return answer(200, { run: { id: "gr_6c12", package: "pkg_7a1c", state: "clean", blocking: 0, warnings: 0, findings: [] } });
      return base(url, init);
    });
    const scrolled = vi.fn(); // jsdom has no scrollIntoView; the stub is removed below
    Element.prototype.scrollIntoView = scrolled;
    try {
      render(<Shell />);
      const card = await screen.findByRole("region", { name: "Outbound" });
      expect(sent()).toContain("GET /api/v1/projects/claims-portal");
      expect(sent().some((line) => line.includes("/projects/claims-portal/outbound"))).toBe(false);
      expect(scrolled.mock.contexts).toEqual([card]);
    } finally {
      delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    }
  });

  // A failed /auth/config read must never be read as "oidc" — that guess asks, and 404s, on a host
  // with no sign-in: `auth` stays null, so the app page's Access card asks nothing, exactly as it
  // would while the read is merely still in flight (canyonv2-028b).
  it("a failed /auth/config read leaves the app page's Access card silent, not guessing oidc", async () => {
    window.location.hash = "#/apps/claims-portal";
    const project = {
      id: "claims-portal", name: "claims-portal", owners: [],
      access: { level: "owner", everyone: false, people: 0, groups: 0, requests: 0 },
      gate_state: "clean", envs: {}, latest: { package: "pkg_1", stage: "created", since: new Date().toISOString() },
    };
    const status = { project: project.id, envs: {}, last_run: null, receipt: { org: "Meridian", blocked_by: [], rules: [] }, earlier: [] };
    const sent: string[] = [];
    vi.stubGlobal("fetch", vi.fn((url: string) => {
      sent.push(url);
      if (url === "/auth/config") return Promise.reject(new Error("network down"));
      if (url === "/api/v1/projects/claims-portal") return Promise.resolve(answer(200, { project }));
      if (url === "/api/v1/projects/claims-portal/status") return Promise.resolve(answer(200, status));
      return Promise.resolve(answer(404, null));
    }));
    render(<Shell />);
    await screen.findByRole("heading", { level: 1, name: "claims-portal" });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(sent.some((url) => url.includes("/access"))).toBe(false);
  });

  // "#/apps/" and "#/no-access/" with no app named are not routes either screen can serve: the
  // fleet renders instead, exactly as it would for any other hash neither screen recognises, and
  // nothing shaped like a project request goes out for the id neither of them has (canyonv2-028b).
  it.each(["#/apps/", "#/no-access/"])("%s with no id falls through to the fleet, asking nothing project-shaped", async (route) => {
    window.location.hash = route;
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    expect(await screen.findByRole("heading", { level: 1, name: "Meridian Insurance" })).toBeDefined();
    expect(sent().some((request) => request.includes("/api/v1/projects/"))).toBe(false);
  });

  it("opens #/settings on the host's front door", async () => {
    window.location.hash = "#/settings";
    const sent = server(prefs);
    render(<Shell />);
    await waitFor(() => expect(sent()).toContain("GET /api/v1/front-door"));
  });

  // The org's name comes from the config read the shell already makes: no second request for it.
  it("heads the fleet with the org /auth/config names", async () => {
    window.location.hash = "#/fleet";
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    expect(await screen.findByRole("heading", { level: 1, name: "Meridian Insurance" })).toBeDefined();
    expect(sent().filter((request) => request === "GET /auth/config")).toHaveLength(1);
  });

  // Where the callback's two refusals land. Only the callback navigates to these routes, so
  // arriving on one is the whole message: there is no marker in the URL and nothing is read back.
  it("resolves #/signin to the sign-in page carrying its failure line", async () => {
    window.location.hash = "#/signin";
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    expect(await screen.findByText("Sign-in didn't finish")).toBeDefined();
    await signInPage("/auth/start", "Try again");
    expect(sent()).toEqual(["GET /auth/config"]); // the fleet is never read: nobody is signed in
  });

  it("resolves #/setup-required to one sentence with nothing to click", async () => {
    const sent = server(prefs, () => answer(200, signedIn));
    window.location.hash = "#/setup-required";
    render(<Shell />);
    expect(await screen.findByText(AUTH.setupRequired)).toBeDefined();
    expect(screen.queryByRole("link")).toBeNull(); // no way on: "try again" is the one instruction that cannot work here
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.queryByText("Sign in to Canyon")).toBeNull();
    expect(screen.queryByText("Sign-in didn't finish")).toBeNull();
    expect(sent()).toEqual([]); // the screen renders from nothing: an unset-up host has nothing to tell it
  });

  // The dashboard asks two questions before it draws anything: is this host authenticated, and has
  // anybody finished the form. A demo host answers the second one the same way a fresh install does
  // — the marker is null and stays null — so the mode is what separates them, and onboarding must
  // not appear in front of a `docker compose up`.
  it.each([
    ["oidc", true],
    ["off", false],
  ])("a host with a null setup marker under %s renders onboarding: %s", async (auth, onboarding) => {
    window.location.hash = "#/fleet";
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth, org: "Meridian Insurance", device: auth === "oidc" });
      if (url === "/api/v1/preferences") return answer(200, { ...prefs, onboarded_at: null });
      if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
      return answer(200, signedIn);
    }));
    render(<Shell />);
    if (onboarding) expect(await screen.findByText("Set up Canyon")).toBeDefined();
    else expect(await screen.findByText("commission-calc")).toBeDefined();
    expect(screen.queryByText(onboarding ? "commission-calc" : "Set up Canyon")).toBeNull();
  });

  it("signs out through DELETE /api/v1/sessions and then shows the sign-in page", async () => {
    window.location.hash = "#/fleet";
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    await screen.findByRole("button", { name: AUTH.signOut });
    fireEvent.click(screen.getByRole("button", { name: AUTH.signOut }));
    await waitFor(() => expect(sent()).toContain("DELETE /api/v1/sessions"));
    await signInPage("/auth/start?prompt=select_account"); // the provider's session outlives Canyon's: the next sign-in asks which account
    expect(screen.getByText("You're signed out of Canyon")).toBeDefined(); // only a sign-out the person chose says it
    // A poll already on its way answers 401 after the sign-out: still a sign-out the person chose.
    act(() => { window.dispatchEvent(new Event(UNAUTHENTICATED)); });
    expect(screen.getByText("You're signed out of Canyon")).toBeDefined();
    expect(screen.getByRole("link", { name: "Continue to company sign-in" }).getAttribute("href")).toBe("/auth/start?prompt=select_account");
  });

  // Signing out was chosen, and the next person at this browser may be another: the address the first one read is out of the
  // bar, so a reload, or Back from the provider, keeps no page for them (#145).
  it("a chosen sign-out leaves no page to come back to, reloaded or not", async () => {
    window.location.hash = "#/audit?range=7d";
    server(prefs, () => answer(200, signedIn));
    const { unmount } = render(<Shell />);
    fireEvent.click(await screen.findByRole("button", { name: AUTH.signOut }));
    expect(await screen.findByText("You're signed out of Canyon")).toBeDefined();
    expect(window.location.hash).toBe("#/fleet");
    unmount(); // a reload: nothing in memory is left
    vi.stubGlobal("fetch", vi.fn(async (url: string) => (url === "/auth/config" ? answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true }) : answer(401, { error: "unauthenticated" }))));
    render(<Shell />);
    fireEvent.click(await screen.findByRole("link", { name: "Continue to company sign-in" }));
    expect(sessionStorage.getItem("canyon_return")).toBeNull();
  });

  it("a sign-out on a step of signing in stays on it, to be come back to", async () => {
    window.location.hash = "#/device?code=K7QP-4RXM";
    server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    await waitFor(() => expect(document.title).not.toBe(""));
    act(() => { window.dispatchEvent(new CustomEvent(UNAUTHENTICATED, { detail: "signed-out" })); });
    expect(window.location.hash).toBe("#/device?code=K7QP-4RXM");
  });

  // The rail names who is signed in on every page, from one read of the fleet; a host with sign-in
  // off has nobody to name, so it never asks.
  it("names the signed-in person at the foot of the rail on a page that is not the fleet", async () => {
    window.location.hash = "#/audit";
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    expect(await screen.findByText("Dana Whitfield")).toBeDefined();
    expect(screen.getByRole("button", { name: AUTH.signOut })).toBeDefined();
    expect(sent().filter((line) => line.startsWith("GET /api/v1/fleet?limit=1"))).toEqual(["GET /api/v1/fleet?limit=1"]); // the audit's App filter reads its own
  });

  // Guardrails is an admin's screen (design/specs/screens.md, A4b): the rail offers it once the reader is
  // known to be an admin, and never to a member.
  it("offers Guardrails in the rail to an admin, and never to a member", async () => {
    window.location.hash = "#/audit";
    server(prefs, () => answer(200, { ...signedIn, viewer: { display: "Sam Reyes", role: "member", github_logins: [], github_link: false } }));
    const { unmount } = render(<Shell />);
    expect(await screen.findByText("Sam Reyes")).toBeDefined();
    expect(screen.queryByRole("link", { name: "Guardrails" })).toBeNull();
    unmount();
    server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    expect(await screen.findByRole("link", { name: "Guardrails" })).toBeDefined();
  });

  // The preferences read the setup gate already makes names the reader's role, so an admin's rail offers Guardrails on its
  // first paint, before the fleet's read answers (the v0.11.0 role walk caught the rail without it), and a member's never.
  it("offers Guardrails to an admin from the first paint, before the fleet's read answers", async () => {
    window.location.hash = "#/audit";
    server(prefs, () => new Promise<Response>(() => {}) as unknown as Response); // the fleet's read never answers
    const { unmount } = render(<Shell />);
    expect(await screen.findByRole("link", { name: "Guardrails" })).toBeDefined();
    unmount();
    server({ ...prefs, viewer: { ...prefs.viewer!, role: "member" } }, () => new Promise<Response>(() => {}) as unknown as Response);
    render(<Shell />);
    expect(await screen.findByRole("link", { name: "Audit" })).toBeDefined();
    expect(screen.queryByRole("link", { name: "Guardrails" })).toBeNull();
  });

  // A host in setup answers every read but preferences with 409, so the name is asked for only once
  // onboarding is done; the person who finishes it sees themselves at the foot of the rail.
  it("names the person who finishes onboarding, asking only after it is done", async () => {
    window.location.hash = "#/fleet";
    let onboarded = false;
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (url === "/api/v1/preferences") {
        if (init?.method === "PUT") onboarded = true;
        return answer(200, { ...prefs, onboarded_at: onboarded ? "2026-10-02T00:00:00Z" : null });
      }
      if (!onboarded) return answer(409, { error: "setup_required", message: "Canyon is being set up" });
      if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
      return answer(200, signedIn);
    });
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    expect(await screen.findByText("Set up Canyon")).toBeDefined();
    expect(fetch.mock.calls.filter(([url]) => url.startsWith("/api/v1/fleet"))).toEqual([]);
    for (let step = 0; step < 3; step++) fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
    expect(await screen.findByRole("button", { name: AUTH.signOut })).toBeDefined(); // the rail's foot, beside the name
  });

  // The CLI's approval page, opened at #/device as the CLI's link names it (the rail no longer offers it: the UX
  // review), with an empty code field, and nothing on the way lists or asks for a pending request (design/AUTH.md,
  // device-code phishing).
  it("opens the CLI approval page at its address, its code field empty, asking for no request", async () => {
    window.location.hash = "#/audit";
    const sent = server(prefs, () => answer(200, signedIn));
    render(<Shell />);
    await screen.findByRole("navigation", { name: "Canyon" });
    expect(screen.queryByRole("link", { name: "Approve a CLI sign-in" })).toBeNull();
    window.location.hash = "#/device";
    window.dispatchEvent(new HashChangeEvent("hashchange"));
    expect(await screen.findByRole("textbox", { name: AUTH.deviceCode })).toHaveProperty("value", "");
    expect(screen.getByText(AUTH.deviceEnter)).toBeDefined();
    expect(sent().filter((one) => one.includes("device"))).toEqual([]);
  });

  // A read still on its way when the person moves to the device page is not the end of the foot.
  it("asks again on the next page when the first read never came back", async () => {
    window.location.hash = "#/audit";
    let first = true;
    const fetch = vi.fn(async (url: string) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (url === "/api/v1/preferences") return answer(200, prefs);
      if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
      if (url === "/api/v1/fleet?limit=1" && first) { first = false; return answer(500, null); }
      return answer(200, signedIn);
    });
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    await waitFor(() => expect(fetch.mock.calls.map(([url]) => url)).toContain("/api/v1/fleet?limit=1"));
    expect(screen.queryByText("Dana Whitfield")).toBeNull();
    window.location.hash = "#/device";
    fireEvent(window, new HashChangeEvent("hashchange"));
    window.location.hash = "#/audit";
    fireEvent(window, new HashChangeEvent("hashchange"));
    expect(await screen.findByText("Dana Whitfield")).toBeDefined();
  });

  // The rail's Fleet and the tab carry what needs the reader, read again every 30 seconds (D15): the count, one in the
  // singular, nothing at none; ? while nothing is counted and some deployments couldn't be checked; and ? while the read
  // fails, never the last number, nor nothing, which reads as all clear.
  it("keeps the rail's count and the tab to what the last read said", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    window.location.hash = "#/audit";
    const read = (needs_you: number, unchecked = 0) => () => answer(200, { ...signedIn, tiles: { ...signedIn.tiles, needs_you }, unchecked });
    const queue = [read(3), read(1), read(0), read(0, 2), read(2), () => answer(503, { error: "unavailable", message: "the database didn't answer" })];
    const fetch = vi.fn(async (url: string) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (url === "/api/v1/preferences") return answer(200, prefs);
      if (url.startsWith("/api/v1/audit")) return answer(200, { events: [], total: 0, page: 1, next: null });
      if (url === "/api/v1/fleet?limit=1") return (queue.length > 1 ? queue.shift()! : queue[0]!)();
      return answer(200, signedIn); // the audit's App filter
    });
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    const fleet = () => screen.getByRole("link", { name: /^Fleet/ });
    const next = () => act(() => vi.advanceTimersByTimeAsync(30_000));
    await waitFor(() => expect(fleet().textContent).toBe("Fleet33 things need you"));
    await waitFor(() => expect(document.title).toBe("(3) Meridian Insurance · Canyon"));
    await next();
    await waitFor(() => expect(fleet().textContent).toBe("Fleet11 thing needs you"));
    await waitFor(() => expect(document.title).toBe("(1) Meridian Insurance · Canyon"));
    await next();
    await waitFor(() => expect(fleet().textContent).toBe("Fleet"));
    await waitFor(() => expect(document.title).toBe("Meridian Insurance · Canyon"));
    await next();
    await waitFor(() => expect(fleet().textContent).toBe(`Fleet?${S.needsRailUnchecked}`));
    await waitFor(() => expect(document.title).toBe("(?) Meridian Insurance · Canyon"));
    await next();
    await waitFor(() => expect(fleet().textContent).toBe("Fleet22 things need you"));
    await next();
    await waitFor(() => expect(fleet().textContent).toBe(`Fleet?${S.needsRailUnread}`));
    await waitFor(() => expect(document.title).toBe("(?) Meridian Insurance · Canyon"));
  });

  // The rail's count is everyone's, a demo's anonymous admin's too (D15); the name at its foot is a signed-in person's alone.
  it("asks nobody's name on a host with sign-in off, only what needs them", async () => {
    window.location.hash = "#/audit";
    const fetch = vi.fn(async (url: string) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "off", org: "Meridian Insurance", device: false });
      if (url.startsWith("/api/v1/fleet")) return answer(200, signedIn); // the audit's App filter
      return answer(200, { events: [], total: 0, page: 1, next: null });
    });
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    expect(await screen.findByRole("link", { name: "Audit" })).toBeDefined();
    expect(await screen.findByRole("link", { name: "Guardrails" })).toBeDefined(); // everyone is the anonymous admin
    await waitFor(() => expect(fetch.mock.calls.map(([url]) => url).filter((url) => url === "/api/v1/fleet?limit=1")).toEqual(["/api/v1/fleet?limit=1"]));
    expect(screen.queryByRole("button", { name: AUTH.signOut })).toBeNull();
    expect(screen.queryByText("Dana Whitfield")).toBeNull();
  });

  // The callback lands on /, and a sign-in begun on the device page or an app's no-access page comes
  // back to it, once: the page kept is cleared as it is read, and nothing else is ever restored.
  it("comes back from a sign-in to the page it was begun on, once", async () => {
    server(prefs, () => answer(200, signedIn));
    sessionStorage.setItem("canyon_return", "#/device?code=K7QP-4RXM");
    render(<Shell />);
    await waitFor(() => expect(window.location.hash).toBe("#/device?code=K7QP-4RXM"));
    expect(sessionStorage.getItem("canyon_return")).toBeNull();
  });

  it("keeps the page through a refused sign-in, which lands on #/signin, not /", async () => {
    server(prefs, () => answer(200, signedIn));
    sessionStorage.setItem("canyon_return", "#/no-access/claims-portal");
    window.location.hash = "#/signin";
    render(<Shell />);
    expect(await screen.findByText("Sign-in didn't finish")).toBeDefined();
    expect(sessionStorage.getItem("canyon_return")).toBe("#/no-access/claims-portal");
  });

  it("comes back to the page a link named", async () => {
    server(prefs, () => answer(200, signedIn));
    sessionStorage.setItem("canyon_return", "#/audit?range=7d");
    render(<Shell />);
    await waitFor(() => expect(window.location.hash).toBe("#/audit?range=7d"));
    expect(sessionStorage.getItem("canyon_return")).toBeNull();
  });

  it("restores nothing that is not one of those pages", async () => {
    server(prefs, () => answer(200, signedIn));
    sessionStorage.setItem("canyon_return", "#/apps/claims-portal/../../evil");
    render(<Shell />);
    expect(await screen.findByRole("heading", { name: "Meridian Insurance" })).toBeDefined();
    expect(window.location.hash).toBe("#/fleet"); // where every other sign-in lands
    expect(sessionStorage.getItem("canyon_return")).toBeNull();
  });
});
