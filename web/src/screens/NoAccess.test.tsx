import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Actor, Preferences, Reduced } from "../api";
import { S } from "../strings";
import { NoAccess } from "./NoAccess";
import { Shell } from "./Shell";

const prefs = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences }).preferences_admin;
const dana: Actor = { kind: "person", subject: "dana@meridian.example", display: "Dana Whitfield" };
const app = (owners: Actor[], groups: string[] = []): Reduced => ({ id: "claims-portal", name: "claims-portal", owners, owner_groups: groups, summary: "File and track motor claims", claimable: false, access: { level: "none" } });
const ASK = "POST /api/v1/projects/claims-portal/access-requests";

// The shell's two setup reads, then the routes given; anything else is the contract's 404.
function server(routes: Record<string, { status?: number; body?: unknown }>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const key = `${init?.method ?? "GET"} ${url}`;
    const { status = 200, body } = url === "/auth/config" ? { body: { canyon: true, auth: "oidc", org: "Meridian" } }
      : key === "GET /api/v1/preferences" ? { body: prefs }
      : routes[key] ?? { status: 404, body: { error: "not_found" } };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`).filter((one) => !one.includes("/auth/config") && !one.endsWith("/preferences"));
}
const open = () => { window.location.hash = "#/no-access/claims-portal"; return render(<Shell />); };

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the no-access screen", () => {
  it("names the app and its owner, asks once, and says who will see it", async () => {
    const sent = server({ "GET /api/v1/projects/claims-portal": { body: { project: app([dana]) } }, [ASK]: { status: 201 } });
    const { container } = open();
    expect(await screen.findByRole("heading", { name: "You don't have access to claims-portal." })).toBeDefined();
    expect(screen.getByText("Dana Whitfield owns claims-portal. Ask them or a Canyon admin for access.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: S.askForAccess }));
    const answer = await screen.findByText("Asked. Dana Whitfield will see your request.");
    // The status text can land in the DOM (via its own MutationObserver-driven check) a tick
    // before the component's own effect calls .focus() on it — wait for focus, don't assume it.
    await waitFor(() => expect(document.activeElement).toBe(answer));
    expect(screen.queryByRole("button")).toBeNull();
    // The screen reads the app's name and owner and nothing else of it.
    expect(sent()).toEqual(["GET /api/v1/projects/claims-portal", ASK]);
    expect(container.textContent).not.toMatch(/File and track|Production|Preview|Findings|pkg_|stage/i);
  });

  it("an app with no owner sends the person to a Canyon admin", async () => {
    server({ "GET /api/v1/projects/claims-portal": { body: { project: app([]) } }, [ASK]: { status: 201 } });
    open();
    expect(await screen.findByText("Ask a Canyon admin.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: S.askForAccess }));
    expect(await screen.findByText("Asked. A Canyon admin will see your request.")).toBeDefined();
  });

  it("an app a group owns sends the person to that group, not to an admin", async () => {
    server({ "GET /api/v1/projects/claims-portal": { body: { project: app([], ["claims-team", "platform"]) } }, [ASK]: { status: 201 } });
    open();
    expect(await screen.findByText("Members of the claims-team group own claims-portal. Ask one of them or a Canyon admin for access.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: S.askForAccess }));
    expect(await screen.findByText("Asked. Members of the claims-team group will see your request.")).toBeDefined();
  });

  it("a refusal is the server's own sentence, and the button stays", async () => {
    server({ "GET /api/v1/projects/claims-portal": { body: { project: app([dana]) } }, [ASK]: { status: 409, body: { error: "already_on_team", message: "you can already change this app" } } });
    open();
    fireEvent.click(await screen.findByRole("button", { name: S.askForAccess }));
    expect(await screen.findByText("you can already change this app")).toBeDefined();
    await waitFor(() => expect(screen.getByRole("button", { name: S.askForAccess })).toBeDefined());
  });

  // A share reaches the dashboard at once and the app's own sign-in about a minute later (decision
  // D2), so a person who reloads this page after being shared with is told so, not asked again.
  it("tells a person who has access by now to open the app's link again, and asks nothing", async () => {
    const sent = server({ "GET /api/v1/projects/claims-portal": { body: { project: { ...app([dana]), access: { level: "viewer" } } } } });
    open();
    expect(await screen.findByText(S.accessNow.replace("<app>", "claims-portal"))).toBeDefined();
    expect(screen.getByRole("link", { name: "claims-portal" }).getAttribute("href")).toBe("#/apps/claims-portal");
    expect(screen.queryByRole("button", { name: S.askForAccess })).toBeNull();
    expect(screen.queryByText("You don't have access to claims-portal.")).toBeNull();
    expect(sent()).toEqual(["GET /api/v1/projects/claims-portal"]);
  });

  // With sign-in off everyone can open every app: there is nobody to ask, so the page is the app's.
  it("goes to the app on a host with sign-in off", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    window.location.hash = "#/no-access/claims-portal";
    render(<NoAccess id="claims-portal" auth="off" />);
    await waitFor(() => expect(window.location.hash).toBe("#/apps/claims-portal"));
    expect(fetch).not.toHaveBeenCalled(); // nothing asked, and no ask button drawn on the way
    expect(screen.queryByRole("button")).toBeNull();
  });
});
