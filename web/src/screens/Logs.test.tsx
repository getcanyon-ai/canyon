import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { App as AppRead, FleetPage, Status } from "../api";
import { S } from "../strings";
import { RailContext } from "../ui/Rail";
import { App } from "./App";

const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
// Preview and production both run, as the contract's rollback status has them.
const status = golden<Status>("app-page/claims-portal-rollback-status.json");
const admin = golden<FleetPage>("fleet/page.json").apps.find((app) => app.id === "claims-portal") as AppRead;
const contributor: AppRead = { ...admin, access: { level: "contributor", everyone: false, people: 1, groups: 0 } };
// The reduced row a reader below the team gets: production's address and state.
const row = (level: "viewer" | "none") => ({ id: "claims-portal", name: "claims-portal", owners: [], owner_groups: [], summary: "x", claimable: false, access: { level },
  envs: { prod: { url: "https://claims-portal.example", state: "running" }, preview: { url: "https://claims-portal-preview.example", state: "running" } } });
// A responder's status read is trimmed to production and the receipt (contracts/routes.md, the status row).
const RESPONDER = { "GET /api/v1/preferences": { body: { viewer: { email: "rhea@meridian.example", role: "responder" } } },
  "GET /api/v1/projects/claims-portal/status": { body: { project: "claims-portal", envs: { prod: { url: "https://claims-portal.example", state: "running" } }, last_run: null, promote_run: null, receipt: status.receipt, needs: [] } } };
const logs = (env: "preview" | "prod") => `GET /api/v1/projects/claims-portal/logs?env=${env}&lines=200&redaction=masked`;

type Answer = { status?: number; body: unknown; text?: string };
// The server answers "<METHOD> <url>" from routes, JSON, or text for a masked log read; anything else is 404.
function server(routes: Record<string, Answer | (() => Answer)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status: code = 200, body, text } = typeof route === "function" ? route() : route;
    return { ok: code < 300, status: code, headers: new Headers(text === undefined ? {} : { "Canyon-Log-Redaction": "masked" }), text: async () => text ?? "", json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method} ${url}`);
}
// The shell's read of who signs in: a security responder.
const asResponder = () => <RailContext.Provider value={{ org: "Meridian", viewer: { display: "Rhea Okafor", role: "responder", github_logins: [], github_link: false }, admin: false }}><App id="claims-portal" auth="oidc" /></RailContext.Provider>;
const card = async (name: "Preview" | "Production") => within(await screen.findByRole("region", { name }));
const flush = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("an environment's logs on its card", () => {
  it("shows a contributor each environment's last 200 lines, masked, read again on Refresh, and copied", async () => {
    let n = 0;
    const sent = server({
      "GET /api/v1/projects/claims-portal": { body: { project: contributor } },
      "GET /api/v1/projects/claims-portal/status": { body: status },
      [logs("prod")]: () => ({ body: null, text: `GET /claims 200 41ms\nread ${++n} at <API_TOKEN>\n` }),
      [logs("preview")]: { body: null, text: "preview's own\n" },
    });
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    render(<App id="claims-portal" auth="oidc" />);
    const opener = (await card("Production")).getByRole("button", { name: "Show production's logs" });
    expect(sent()).not.toContain(logs("prod")); // read only when asked
    fireEvent.click(opener);
    const dialog = within(await screen.findByRole("dialog", { name: "claims-portal's logs in production" }));
    expect(dialog.getByText(S.logsLines)).toBeDefined();
    expect((await dialog.findByText(/read 1 at/)).textContent).toBe("GET /claims 200 41ms\nread 1 at <API_TOKEN>");
    fireEvent.click(dialog.getByRole("button", { name: "Refresh" }));
    expect(await dialog.findByText(/read 2 at/)).toBeDefined();
    fireEvent.click(dialog.getByRole("button", { name: "Copy" }));
    expect(await dialog.findByText(S.logsCopied)).toBeDefined();
    expect(writeText).toHaveBeenCalledWith("GET /claims 200 41ms\nread 2 at <API_TOKEN>");
    fireEvent.click(dialog.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.activeElement).toBe(opener);
    expect(sent().filter((one) => one === logs("prod"))).toHaveLength(2); // never followed, never polled
    fireEvent.click((await card("Preview")).getByRole("button", { name: "Show preview's logs" }));
    expect(await within(await screen.findByRole("dialog", { name: "claims-portal's logs in preview" })).findByText("preview's own")).toBeDefined();
  });

  it("says why it can't show them, and where the team reads them raw", async () => {
    server({
      "GET /api/v1/projects/claims-portal": { body: { project: contributor } },
      "GET /api/v1/projects/claims-portal/status": { body: status },
      [logs("prod")]: { status: 409, body: { error: "not_maskable" } },
    });
    render(<App id="claims-portal" auth="oidc" />);
    fireEvent.click((await card("Production")).getByRole("button", { name: "Show production's logs" }));
    expect(await screen.findByText(S.lastLinesUnmasked + S.lastLinesCli.replace("<env>", "prod"))).toBeDefined();
  });

  // A security responder reads production's, masked, whatever their access to the app (design/PERMISSIONS.md, Security
  // responders; the owner, 2026-11-25), never preview's; and is never pointed at canyon logs, whose raw read is the team's.
  it("shows a responder with no grant production's logs alone", async () => {
    const sent = server({
      "GET /api/v1/projects/claims-portal": { body: { project: row("none") } },
      ...RESPONDER,
      [logs("prod")]: { body: null, text: "pool: reconnecting to <DATABASE_URL>\n" },
    });
    render(asResponder());
    expect(screen.queryByRole("button", { name: "Show preview's logs" })).toBeNull();
    fireEvent.click(await (await card("Production")).findByRole("button", { name: "Show production's logs" }));
    expect(await within(await screen.findByRole("dialog")).findByText("pool: reconnecting to <DATABASE_URL>")).toBeDefined();
    expect(sent()).not.toContain(logs("preview"));
    cleanup();
    server({ "GET /api/v1/projects/claims-portal": { body: { project: row("none") } }, ...RESPONDER, [logs("prod")]: { status: 409, body: { error: "not_maskable" } } });
    render(asResponder());
    fireEvent.click(await (await card("Production")).findByRole("button", { name: "Show production's logs" }));
    expect(await screen.findByText(S.lastLinesUnmasked)).toBeDefined();
  });

  // Below contributor, a member who isn't a responder reads no logs: the card says who does, and nothing is asked.
  it("tells a read-only member who sees the logs, and reads none", async () => {
    const sent = server({ "GET /api/v1/projects/claims-portal": { body: { project: row("viewer") } } });
    render(<App id="claims-portal" auth="oidc" />);
    expect(await (await card("Production")).findByText(S.logsTeamOnly)).toBeDefined();
    expect((await card("Preview")).getByText(S.logsTeamOnly)).toBeDefined();
    await flush();
    expect(screen.queryByRole("button", { name: /^Show .* logs$/ })).toBeNull();
    expect(sent().some((one) => one.includes("/logs?"))).toBe(false);
  });
});
