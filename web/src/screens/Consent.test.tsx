import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AUTH, CONSENT, S } from "../strings";
import { leave } from "./Consent";
import { requestedAt } from "./Device";
import { Shell } from "./Shell";

const ID = "a".repeat(64);
const REQUEST = `/api/v1/oauth/requests/${ID}`;
const pending = (verified: boolean, scopes = ["read", "act"], name = "Claude Code") => ({
  client: { name, host: verified ? "claude.ai" : "127.0.0.1", verified }, redirect_host: "127.0.0.1", scopes, host: "canyon.meridian.example",
  requested_at: "2026-11-20T12:00:00Z", expires_at: "2026-11-20T12:10:00Z", account: { display: "Gina Ruiz", email: "gina@meridian.example" },
});
const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;

// The request read answers from `read`; an approval or a refusal answers the client's redirect.
function server(read: () => Response) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
    if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" });
    if (init?.method === "POST" && url === `${REQUEST}/approve`) return answer(200, { redirect: "http://127.0.0.1:41999/callback?code=c&iss=x&state=s" });
    if (init?.method === "POST" && url === `${REQUEST}/deny`) return answer(200, { redirect: "http://127.0.0.1:41999/callback?error=access_denied&iss=x&state=s" });
    if (url === REQUEST) return read();
    return answer(404, { error: "not_found", message: "no such route" });
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`).filter((line) => !line.endsWith("/auth/config") && !line.endsWith("/api/v1/preferences"));
}

function open(hash: string, read = () => answer(200, pending(false))) {
  const assign = vi.spyOn(leave, "to").mockImplementation(() => {});
  window.location.hash = hash;
  const sent = server(read);
  render(<Shell />);
  return { sent, assign };
}

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.location.hash = "";
  sessionStorage.clear();
});

describe("an MCP client's consent page", () => {
  it("names the client, the account, where it sends you back and each scope, and allows nothing until the click", async () => {
    const { sent, assign } = open(`#/oauth?request=${ID}`);
    expect(await screen.findByRole("heading", { name: "Allow Claude Code to use Canyon as you?" })).toBeDefined();
    expect(screen.getByText("Claude Code will act as Gina Ruiz (gina@meridian.example) on canyon.meridian.example. Then Canyon sends you back to 127.0.0.1.")).toBeDefined();
    expect(screen.getByText(CONSENT.onlyYours)).toBeDefined(); // a link someone else sent is how this page phishes
    expect(screen.getByText(CONSENT.unverified)).toBeDefined(); // a registration's name is its own word
    const asked = screen.getByRole("region", { name: "The request" });
    expect(within(asked).getByText(AUTH.deviceFrom).nextSibling?.textContent).toBe("127.0.0.1");
    expect(within(asked).getByText(AUTH.deviceWhen).nextSibling?.textContent).toBe(requestedAt("2026-11-20T12:00:00Z"));
    const may = screen.getByRole("region", { name: "What it may do" });
    expect(within(may).getByText(CONSENT.scope.read)).toBeDefined();
    expect(within(may).getByText(CONSENT.scope.act)).toBeDefined();
    expect(within(may).getByText(CONSENT.only)).toBeDefined();
    expect(sent()).toEqual([`GET ${REQUEST}`]);
    fireEvent.click(screen.getByRole("button", { name: CONSENT.allow }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("http://127.0.0.1:41999/callback?code=c&iss=x&state=s"));
    expect(sent()).toEqual([`GET ${REQUEST}`, `POST ${REQUEST}/approve`]);
  });

  it("shows a client document's host, read alone, and sends a refusal back", async () => {
    const { sent, assign } = open(`#/oauth?request=${ID}`, () => answer(200, pending(true, ["read"])));
    const asked = await screen.findByRole("region", { name: "The request" });
    expect(within(asked).getByText(AUTH.deviceFrom).nextSibling?.textContent).toBe("claude.ai");
    expect(screen.getByText(CONSENT.onlyYours)).toBeDefined();
    expect(screen.queryByText(CONSENT.unverified)).toBeNull();
    expect(screen.queryByText(CONSENT.scope.act)).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: CONSENT.refuse }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("http://127.0.0.1:41999/callback?error=access_denied&iss=x&state=s"));
    expect(sent()).toEqual([`GET ${REQUEST}`, `POST ${REQUEST}/deny`]);
  });

  it("fills a client's name in as it is, never as a placeholder or a replacement pattern", async () => {
    const name = "Evil <redirect host> $' $& <email>";
    open(`#/oauth?request=${ID}`, () => answer(200, pending(false, ["read"], name)));
    expect(await screen.findByRole("heading", { name: `Allow ${name} to use Canyon as you?` })).toBeDefined();
    expect(screen.getByText(`${name} will act as Gina Ruiz (gina@meridian.example) on canyon.meridian.example. Then Canyon sends you back to 127.0.0.1.`)).toBeDefined();
  });

  it("follows only an https redirect, or http on loopback", async () => {
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" });
      if (init?.method === "POST") return answer(200, { redirect: "javascript:alert(document.cookie)" });
      return answer(200, pending(false));
    });
    const assign = vi.spyOn(leave, "to").mockImplementation(() => {});
    window.location.hash = `#/oauth?request=${ID}`;
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    fireEvent.click(await screen.findByRole("button", { name: CONSENT.allow }));
    await waitFor(() => expect(fetch.mock.calls.some(([, init]) => init?.method === "POST")).toBe(true));
    expect(assign).not.toHaveBeenCalled();
  });

  it("follows a cursor:// redirect, Cursor's own scheme", async () => {
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" });
      if (init?.method === "POST") return answer(200, { redirect: "cursor://anysphere.cursor-retrieval/oauth/user-canyon/callback?code=c&iss=x" });
      return answer(200, { ...pending(false), redirect_host: "cursor" });
    });
    const assign = vi.spyOn(leave, "to").mockImplementation(() => {});
    window.location.hash = `#/oauth?request=${ID}`;
    vi.stubGlobal("fetch", fetch);
    render(<Shell />);
    expect(await screen.findByText(/Then Canyon sends you back to cursor\.$/)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: CONSENT.allow }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("cursor://anysphere.cursor-retrieval/oauth/user-canyon/callback?code=c&iss=x"));
  });

  it("says a request expired, and never follows a redirect it wasn't answered", async () => {
    const { assign } = open(`#/oauth?request=${ID}`, () => answer(404, { error: "not_found", message: "This request has expired." }));
    expect(await screen.findByRole("heading", { name: CONSENT.expiredNoClient })).toBeDefined();
    expect(screen.queryByRole("button", { name: CONSENT.allow })).toBeNull();
    expect(assign).not.toHaveBeenCalled();
  });

  it("names why a request isn't valid, and asks nothing", async () => {
    for (const invalid of ["client", "redirect", "response", "pkce", "resource", "scope", "state"] as const) {
      const { sent } = open(`#/oauth?invalid=${invalid}`);
      expect(await screen.findByRole("heading", { name: CONSENT.invalid.replace("<reason>", CONSENT.reason[invalid]) })).toBeDefined();
      expect(sent()).toEqual([]);
      cleanup();
    }
  });

  it("is a page a sign-in comes back to, its heading the agent's", async () => {
    open(`#/oauth?request=${ID}`, () => answer(401, { error: "unauthenticated", message: "sign in to Canyon" }));
    expect(await screen.findByRole("heading", { name: S.signInForAgent })).toBeDefined();
  });
});
