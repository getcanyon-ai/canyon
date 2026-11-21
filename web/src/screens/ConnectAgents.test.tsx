import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CONNECT as T, S } from "../strings";
import { ConnectAgents } from "./ConnectAgents";

const METADATA = "GET /.well-known/oauth-protected-resource/mcp";
const ADDRESS = "https://canyon.example.com/mcp";

// The server answers "<METHOD> <url>" from a table and 404s anything else; the reader lists every request.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const { status = 200, body } = routes[`${init?.method ?? "GET"} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`);
}
const panel = () => within(screen.getByRole("region", { name: T.title }));
const term = (agent: string) => panel().getByText(agent, { selector: "dt" }).nextElementSibling as HTMLElement;
const link = (name: string) => panel().getByRole("link", { name }).getAttribute("href") ?? "";
const served = { status: 200, body: { resource: ADDRESS, authorization_servers: ["https://canyon.example.com"], scopes_supported: ["read", "act"] } };

afterEach(() => vi.unstubAllGlobals());

describe("Connect an agent", () => {
  it("gives a member the address to copy, and each agent's way to it, and no org-managed snippets", async () => {
    server({ [METADATA]: served });
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ConnectAgents signIn admin={false} />);
    expect(await panel().findByLabelText(T.address)).toHaveProperty("value", ADDRESS);
    fireEvent.click(panel().getByRole("button", { name: `${T.copy} ${T.address.toLowerCase()}` }));
    expect(await panel().findByText(T.copied)).toBeDefined();
    expect(writeText).toHaveBeenCalledWith(ADDRESS);
    expect(panel().getByText("canyon skill install")).toBeDefined();
    expect(term("Claude Code").textContent).toBe(`claude mcp add --scope user --transport http canyon ${ADDRESS}`);
    expect(term("Codex").textContent).toBe(`codex mcp add canyon --url ${ADDRESS}`);
    const cursor = new URL(link(T.addCursor));
    expect([cursor.protocol, cursor.searchParams.get("name"), JSON.parse(atob(cursor.searchParams.get("config") ?? ""))]).toEqual(["cursor:", "canyon", { url: ADDRESS }]);
    expect(JSON.parse(decodeURIComponent(link(T.addVSCode).replace("vscode:mcp/install?", "")))).toEqual({ name: "canyon", type: "http", url: ADDRESS });
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  it("gives an admin each agent's org-managed settings with the address", async () => {
    server({ [METADATA]: served });
    render(<ConnectAgents signIn admin />);
    const managed = within(await screen.findByRole("region", { name: T.managed }));
    const snippets = managed.getAllByText(/canyon/, { selector: "code" }).map((code) => code.textContent);
    expect(snippets).toEqual([
      JSON.stringify({ mcpServers: { canyon: { type: "http", url: ADDRESS } } }),
      `[mcp_servers.canyon]\nurl = "${ADDRESS}"`,
      JSON.stringify({ mcpServers: { canyon: { url: ADDRESS } } }),
    ]);
    expect(managed.getByText(`: ${T.managedClaude}`, { exact: false })).toBeDefined();
  });

  it("says why there is no address on a host that serves none, and gives canyon mcp for each agent", async () => {
    server({ [METADATA]: { status: 404, body: { error: "not_found" } } });
    render(<ConnectAgents signIn admin />);
    expect(await panel().findByText(T.noAddress)).toBeDefined();
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(term("Claude Code").textContent).toBe("claude mcp add --scope user canyon -- canyon mcp");
    expect(term("Codex").textContent).toBe("codex mcp add canyon -- canyon mcp");
    expect(JSON.parse(atob(new URL(link(T.addCursor)).searchParams.get("config") ?? ""))).toEqual({ command: "canyon", args: ["mcp"] });
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  it("with sign-in off asks for no address, says why there is none, and gives canyon mcp", () => {
    const asked = server({});
    render(<ConnectAgents signIn={false} admin />);
    expect(panel().getByText(T.signInOff)).toBeDefined();
    expect(asked()).toEqual([]);
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(term("Codex").textContent).toBe("codex mcp add canyon -- canyon mcp");
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  it("shows no address it can't trust: a failed read, or one that isn't https, says so and offers nothing to copy", async () => {
    for (const answer of [{ status: 500, body: { error: "internal", message: "Canyon couldn't read it." } }, { status: 200, body: { resource: "http://canyon.example.com/mcp" } }]) {
      server({ [METADATA]: answer });
      const { unmount } = render(<ConnectAgents signIn admin />);
      expect(await panel().findByText(/answered (500|200) without an address/)).toBeDefined();
      expect(panel().queryByLabelText(T.address)).toBeNull();
      expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
      unmount();
    }
  });

  it("says when the browser won't copy", async () => {
    server({ [METADATA]: served });
    vi.stubGlobal("navigator", { clipboard: { writeText: async () => { throw new Error("denied"); } } });
    render(<ConnectAgents signIn admin={false} />);
    fireEvent.click(await panel().findByRole("button", { name: `${T.copy} ${T.address.toLowerCase()}` }));
    expect(await panel().findByRole("alert")).toHaveProperty("textContent", S.copyFailed);
  });
});
