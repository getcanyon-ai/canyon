import { fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CONNECT as T, CONNECTOR_DOCS, S } from "../strings";
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
const served = { status: 200, body: { resource: ADDRESS, authorization_servers: ["https://canyon.example.com"], scopes_supported: ["read", "act"] } };

afterEach(() => vi.unstubAllGlobals());

describe("Connect an agent", () => {
  it("gives the laptop's three steps for this host, and the address with each cloud agent's way to it", async () => {
    server({ [METADATA]: served });
    const writeText = vi.fn(async () => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    render(<ConnectAgents signIn />);
    expect(steps()).toEqual(threeSteps());
    expect(await panel().findByLabelText(T.address)).toHaveProperty("value", ADDRESS);
    fireEvent.click(panel().getByRole("button", { name: `${T.copy} ${T.address.toLowerCase()}` }));
    expect(await panel().findByText(T.copied)).toBeDefined();
    expect(writeText).toHaveBeenCalledWith(ADDRESS);
    expect(term(T.claudeWeb).textContent).toBe(`${T.claudeWebHow} ${T.claudeDocs}`);
    expect(within(term(T.claudeWeb)).getByRole("link", { name: T.claudeDocs }).getAttribute("href")).toBe(CONNECTOR_DOCS);
    expect(within(term(T.claudeWeb)).getByRole("link", { name: T.claudeDocs }).className).toMatch(/\btext-link\b/); // drawn as a link, never as plain text
    expect(term(T.cursorCloud).textContent).toBe(T.cursorCloudHow);
    expect(term("Codex").textContent).toBe(T.codexCloud);
  });

  // The address is for cloud agents: a laptop's agents get the CLI's own server. Nothing here installs the address on a
  // laptop, neither an agent's command nor an editor's install link, nor Claude Code's managed settings, which reach
  // laptops too (managedMcpServers, managed-mcp.json).
  it("offers no way to put the address on a laptop", async () => {
    server({ [METADATA]: served });
    render(<ConnectAgents signIn />);
    await panel().findByLabelText(T.address);
    const text = screen.getByRole("region", { name: T.title }).textContent ?? "";
    for (const laptop of ["claude mcp add", "codex mcp add", "managedMcpServers", "managed-mcp.json", "mcpServers"]) expect(text).not.toContain(laptop);
    expect(panel().queryAllByRole("link").map((a) => a.getAttribute("href"))).toEqual([CONNECTOR_DOCS]);
  });

  it("says why there is no address on a host that serves none, and keeps the laptop's steps", async () => {
    server({ [METADATA]: { status: 404, body: { error: "not_found" } } });
    render(<ConnectAgents signIn />);
    expect(await panel().findByText(T.noAddress)).toBeDefined();
    expect(steps()).toEqual(threeSteps());
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(panel().queryByText(T.claudeWeb, { selector: "dt" })).toBeNull();
  });

  it("with sign-in off asks for no address, says why there is none, and gives the laptop's steps", () => {
    const asked = server({});
    render(<ConnectAgents signIn={false} />);
    expect(panel().getByText(T.signInOff)).toBeDefined();
    expect(asked()).toEqual([]);
    expect(steps()).toEqual(threeSteps());
    expect(panel().queryByLabelText(T.address)).toBeNull();
    expect(panel().queryByText(T.claudeWeb, { selector: "dt" })).toBeNull();
  });

  it("shows no address it can't trust: a failed read, or one that isn't https, says so and offers nothing to copy", async () => {
    for (const answer of [{ status: 500, body: { error: "internal", message: "Canyon couldn't read it." } }, { status: 200, body: { resource: "http://canyon.example.com/mcp" } }]) {
      server({ [METADATA]: answer });
      const { unmount } = render(<ConnectAgents signIn />);
      expect(await panel().findByText(/answered (500|200) without an address/)).toBeDefined();
      expect(panel().queryByLabelText(T.address)).toBeNull();
      unmount();
    }
  });

  it("says when the browser won't copy", async () => {
    server({ [METADATA]: served });
    vi.stubGlobal("navigator", { clipboard: { writeText: async () => { throw new Error("denied"); } } });
    render(<ConnectAgents signIn />);
    fireEvent.click(await panel().findByRole("button", { name: `${T.copy} ${T.address.toLowerCase()}` }));
    expect(await panel().findByRole("alert")).toHaveProperty("textContent", S.copyFailed);
  });
});
