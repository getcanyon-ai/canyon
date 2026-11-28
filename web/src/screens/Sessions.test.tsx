import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SESSIONS } from "../strings";
import { Sessions } from "./OrgSettings";

const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;
const row = (id: string, flow: string, extra: Record<string, unknown> = {}) => ({ id, flow, created_at: "2026-11-20T09:00:00Z", last_seen_at: null, expires_at: "2026-11-20T21:00:00Z", current: false, ...extra });

function server(sessions: unknown[]) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/v1/sessions") return answer(200, { sessions });
    if (init?.method === "DELETE" && url.startsWith("/api/v1/sessions/")) return answer(204, null);
    return answer(404, { error: "not_found", message: "no such route" });
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`);
}

afterEach(() => { vi.unstubAllGlobals(); });

// Settings › Agents' sessions list (contracts/routes.md, Sessions): how each signed in, an MCP client by its
// name, and Disconnect on each but the reader's own browser session, which keeps Sign out; Person is an admin's column.
describe("the sessions list", () => {
  it("names each session's way in, marks the reader's own, and disconnects one by its digest", async () => {
    const sent = server([row("ses_0000000000000001", "oauth", { client: "Claude Code" }), row("ses_0000000000000002", "device"), row("ses_0000000000000003", "browser", { current: true })]);
    render(<Sessions admin={false} />);
    const list = await screen.findByRole("region", { name: SESSIONS.title });
    expect(within(list).getByText(SESSIONS.line)).toBeDefined();
    expect(within(list).getByText("Claude Code (MCP)")).toBeDefined();
    expect(within(list).getByText(SESSIONS.flow.device)).toBeDefined();
    expect(within(list).getByText(SESSIONS.current)).toBeDefined();
    expect(within(list).queryByText(SESSIONS.person)).toBeNull();
    expect(within(list).getAllByRole("button", { name: /^Disconnect/ })).toHaveLength(2);
    fireEvent.click(within(list).getByRole("button", { name: "Disconnect ses_0000000000000001" }));
    // A disconnect ends someone's access, so it asks first, saying what stops, and Cancel sends nothing (D16).
    const ask = within(within(list).getByRole("group", { name: "Disconnect this Claude Code (MCP) session?" }));
    expect(ask.getByText(SESSIONS.disconnectMeans)).toBeDefined();
    fireEvent.click(ask.getByRole("button", { name: "Cancel" }));
    expect(sent()).not.toContain("DELETE /api/v1/sessions/ses_0000000000000001");
    fireEvent.click(within(list).getByRole("button", { name: "Disconnect ses_0000000000000001" }));
    fireEvent.click(within(within(list).getByRole("group", { name: "Disconnect this Claude Code (MCP) session?" })).getByRole("button", { name: SESSIONS.disconnect }));
    await waitFor(() => expect(sent()).toContain("DELETE /api/v1/sessions/ses_0000000000000001"));
  });

  // On a phone a row is two lines (#100's review): how and whether it is this one, over when it began, was last used and
  // ends, with Disconnect beside them; the table's columns are a wide screen's.
  it("says each session in two lines for a narrow screen", async () => {
    server([row("ses_0000000000000003", "browser", { current: true, last_seen_at: "2026-11-20T10:00:00Z" })]);
    render(<Sessions admin={false} />);
    const list = await screen.findByRole("region", { name: SESSIONS.title });
    const when = (at: string) => new Date(at).toLocaleString([], { dateStyle: "medium", timeStyle: "short" });
    const said = (text: string) => [...list.querySelectorAll("span")].some((one) => one.textContent === text);
    expect(said(`${SESSIONS.flow.browser} · ${SESSIONS.current.toLowerCase()}`)).toBe(true);
    expect(within(list).getByText(`${SESSIONS.since} ${when("2026-11-20T09:00:00Z")} · ${SESSIONS.lastUsed.toLowerCase()} ${when("2026-11-20T10:00:00Z")} · ${SESSIONS.expires.toLowerCase()} ${when("2026-11-20T21:00:00Z")}`)).toBeDefined();
  });

  it("shows an admin whose each session is", async () => {
    server([row("ses_0000000000000004", "browser", { person: { display: "Gina Ruiz", email: "gina@meridian.example" } })]);
    render(<Sessions admin />);
    const list = await screen.findByRole("region", { name: SESSIONS.title });
    expect(within(list).getByText(SESSIONS.person)).toBeDefined();
    expect(within(list).getByText("gina@meridian.example")).toBeDefined();
  });
});
