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
const laptop = () => within(panel().getByRole("region", { name: T.laptop }));
const steps = () => laptop().getAllByRole("listitem").map((step) => step.textContent);
const INSTALL = "curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh";
// The laptop's three steps, with this host filled in: the CLI, a sign-in to this Canyon, and every agent on the machine.
const threeSteps = () => [`${T.stepInstall}${INSTALL}`, `${T.stepLogin}canyon login ${location.origin}`, `${T.stepSkill}canyon skill install`];
const link = (name: string) => panel().getByRole("link", { name }).getAttribute("href") ?? "";
const served = { status: 200, body: { resource: ADDRESS, authorization_servers: ["https://canyon.example.com"], scopes_supported: ["read", "act"] } };

afterEach(() => vi.unstubAllGlobals());

describe("Connect an agent", () => {
  it("gives a member the laptop's three steps for this host, the address to copy, and each agent's way to it", async () => {
    server({ [METADATA]: served });
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ConnectAgents signIn admin={false} />);
    expect(steps()).toEqual(threeSteps());
    expect(await panel().findByLabelText(T.address)).toHaveProperty("value", ADDRESS);
    fireEvent.click(panel().getByRole("button", { name: `${T.copy} ${T.address.toLowerCase()}` }));
    expect(await panel().findByText(T.copied)).toBeDefined();
    expect(writeText).toHaveBeenCalledWith(ADDRESS);
    expect(term("Claude Code").textContent).toBe(`claude mcp add --scope user --transport http canyon ${ADDRESS}`);
    expect(term("Codex").textContent).toBe(`codex mcp add canyon --url ${ADDRESS}\ncodex mcp login canyon`);
    const cursor = new URL(link(T.addCursor));
    expect([cursor.protocol, cursor.searchParams.get("name"), JSON.parse(atob(cursor.searchParams.get("config") ?? ""))]).toEqual(["cursor:", "canyon", { url: ADDRESS }]);
    expect(JSON.parse(decodeURIComponent(link(T.addVSCode).replace("vscode:mcp/install?", "")))).toEqual({ name: "canyon", type: "http", url: ADDRESS });
    expect(panel().queryByText(/canyon mcp$/)).toBeNull(); // no agent is pointed at canyon mcp: canyon skill install sets that up
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  // Claude Code's managed-mcp.json takes exclusive control: every other MCP server on the machine stops loading
  // (code.claude.com/docs/en/managed-mcp). managedMcpServers adds Canyon beside them; Codex's managed configuration can
  // only allow or block a server, and Cursor's team marketplace is set in its dashboard, so each is a line, not a snippet.
  it("gives an admin Claude Code's managedMcpServers, never managed-mcp.json, and a line each for Codex and Cursor", async () => {
    server({ [METADATA]: served });
    render(<ConnectAgents signIn admin />);
    const managed = within(await screen.findByRole("region", { name: T.managed }));
    expect(managed.getAllByText(/canyon/, { selector: "code" }).map((code) => code.textContent)).toEqual([
      JSON.stringify({ managedMcpServers: { canyon: { type: "http", url: ADDRESS } } }),
    ]);
    expect(managed.queryByText(/managed-mcp\.json|mcpServers"/)).toBeNull();
    for (const line of [T.managedClaude, T.managedCodex, T.managedCursor]) expect(managed.getByText(line, { exact: false })).toBeDefined();
  });

  it("rolls nothing out by an address that isn't https: Claude Code refuses one", async () => {
    server({ [METADATA]: { status: 200, body: { resource: "http://localhost:7777/mcp" } } });
    render(<ConnectAgents signIn admin />);
    expect(await panel().findByLabelText(T.address)).toHaveProperty("value", "http://localhost:7777/mcp");
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  it("says why there is no address on a host that serves none, and keeps the laptop's steps", async () => {
    server({ [METADATA]: { status: 404, body: { error: "not_found" } } });
    render(<ConnectAgents signIn admin />);
    expect(await panel().findByText(T.noAddress)).toBeDefined();
    expect(steps()).toEqual(threeSteps());
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(panel().queryByText("Claude Code", { selector: "dt" })).toBeNull();
    expect(screen.queryByRole("region", { name: T.managed })).toBeNull();
  });

  it("with sign-in off asks for no address, says why there is none, and gives the laptop's steps", () => {
    const asked = server({});
    render(<ConnectAgents signIn={false} admin />);
    expect(panel().getByText(T.signInOff)).toBeDefined();
    expect(asked()).toEqual([]);
    expect(steps()).toEqual(threeSteps());
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(panel().queryByText("Codex", { selector: "dt" })).toBeNull();
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
