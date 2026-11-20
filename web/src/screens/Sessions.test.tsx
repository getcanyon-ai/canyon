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

// Settings › People & sign-in's sessions list (contracts/routes.md, Sessions): how each signed in, an MCP client by its
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
    await waitFor(() => expect(sent()).toContain("DELETE /api/v1/sessions/ses_0000000000000001"));
  });

  it("shows an admin whose each session is", async () => {
    server([row("ses_0000000000000004", "browser", { person: { display: "Gina Ruiz", email: "gina@meridian.example" } })]);
    render(<Sessions admin />);
    const list = await screen.findByRole("region", { name: SESSIONS.title });
    expect(within(list).getByText(SESSIONS.person)).toBeDefined();
    expect(within(list).getByText("gina@meridian.example")).toBeDefined();
  });
});
