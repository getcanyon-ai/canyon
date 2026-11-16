import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { S } from "../strings";
import { AppSource } from "./AppSource";

function server(role: "admin" | "member") {
  const sent: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === "DELETE") sent.push(`DELETE ${url}`);
    return { ok: true, status: url === "/api/v1/preferences" ? 200 : 204, json: async () => ({ viewer: { role } }) } as unknown as Response;
  }));
  return sent;
}
afterEach(() => vi.unstubAllGlobals());
const link = { repo_id: 81723, full_name: "acme/platform", folder: "apps/billing", state: "linked" as const };

// Where the app's code lives (design/specs/screens.md, A2; #1917).
describe("AppSource", () => {
  it("names the repository and folder, and an admin unlinks it", async () => {
    const sent = server("admin");
    const changed = vi.fn();
    render(<AppSource id="funny-snake-4k2" link={link} autoDeploy={false} owner={false} changed={changed} />);
    expect(screen.getByRole("link", { name: "github.com/acme/platform" }).getAttribute("href")).toBe("https://github.com/acme/platform");
    expect(screen.getByText("apps/billing")).toBeDefined();
    fireEvent.click(await screen.findByRole("button", { name: "Unlink" }));
    await waitFor(() => expect(changed).toHaveBeenCalled());
    expect(sent).toEqual(["DELETE /api/v1/projects/funny-snake-4k2/repository"]);
  });

  it("offers a member nothing to change", async () => {
    server("member");
    render(<AppSource id="funny-snake-4k2" link={null} autoDeploy={false} owner={false} changed={() => undefined} />);
    expect(screen.getByText(S.sourceKept)).toBeDefined();
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByRole("link", { name: "Link a repository" })).toBeNull();
  });

  it("says why GitHub no longer feeds the app, and an admin can unlink it; no switch there", async () => {
    server("admin");
    for (const [state, line] of [["deleted", S.sourceDeleted], ["archived", S.sourceArchived], ["removed", S.sourceRemoved]] as const) {
      const { unmount } = render(<AppSource id="a" link={{ ...link, state }} autoDeploy={false} owner changed={() => undefined} />);
      expect(screen.getByText(line.replace("<repo>", "acme/platform"), { exact: false })).toBeDefined();
      expect(await screen.findByRole("button", { name: "Unlink" })).toBeDefined();
      expect(screen.queryByRole("checkbox")).toBeNull();
      unmount();
    }
  });

  it("offers a contributor no unlink on a linked app, and says a refused call in the host's words", async () => {
    vi.stubGlobal("fetch", vi.fn(async (url: string) => (url === "/api/v1/preferences"
      ? { ok: true, status: 200, json: async () => ({ viewer: { role: "member" } }) }
      : { ok: false, status: 409, json: async () => ({ error: "x", message: "this app is not linked to a repository" }) }) as unknown as Response));
    const changed = vi.fn();
    render(<AppSource id="a" link={{ ...link, keeps: true }} autoDeploy={false} owner changed={changed} />);
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Unlink" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: S.handOver }));
    fireEvent.click(screen.getByRole("button", { name: S.handOverYes }));
    expect((await screen.findByRole("alert")).textContent).toBe("this app is not linked to a repository");
    expect(changed).not.toHaveBeenCalled();
  });

  // Auto-deploy pushes to preview (#1923): the owners' switch on a linked app; the team reads it.
  it("lets an owner turn auto-deploy on, and tells a contributor it is on", async () => {
    const sent: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (init?.method === "PUT") sent.push(`PUT ${url} ${String(init.body)}`);
      return { ok: true, status: 200, json: async () => (url === "/api/v1/preferences" ? { viewer: { role: "member" } } : { auto_deploy: true }) } as unknown as Response;
    }));
    const changed = vi.fn();
    const { unmount } = render(<AppSource id="funny-snake-4k2" link={link} autoDeploy={false} owner changed={changed} />);
    fireEvent.click(screen.getByRole("checkbox", { name: new RegExp(S.autoDeploy) }));
    await waitFor(() => expect(changed).toHaveBeenCalled());
    expect(sent).toEqual(['PUT /api/v1/projects/funny-snake-4k2/auto-deploy {"auto_deploy":true}']);
    unmount();
    render(<AppSource id="funny-snake-4k2" link={link} autoDeploy owner={false} changed={() => undefined} />);
    await waitFor(() => expect(screen.getByText(S.autoDeployOn)).toBeDefined());
    expect(screen.queryByRole("checkbox")).toBeNull();
  });

  // Kept in GitHub (#1926): the repository Canyon keeps, its last commit and why the last version isn't
  // there; its owners hand it to developers (#1938). While it isn't made yet, there is nothing to switch.
  it("says Canyon keeps the code, and an owner hands it to developers", async () => {
    const sent: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (init?.method === "PUT") sent.push(`PUT ${url} ${String(init.body)}`);
      return { ok: true, status: 200, json: async () => (url === "/api/v1/preferences" ? { viewer: { role: "member" } } : {}) } as unknown as Response;
    }));
    const kept = { repo_id: 555, full_name: "acme-canyon/claims-app-k33p", folder: "", state: "linked" as const, keeps: true, kept_commit: "3f2a1c9d0e", kept_error: "GitHub refused v1.5: remote: error: GH006: Protected branch update failed." };
    const changed = vi.fn();
    const { container, unmount } = render(<AppSource id="claims-app" link={kept} autoDeploy={false} owner changed={changed} />);
    expect(container.textContent).toContain(`Source ${S.sourceKeptBy} github.com/acme-canyon/claims-app-k33p · commit 3f2a1c9`);
    expect(screen.getByText(kept.kept_error)).toBeDefined();
    // it can't be undone, so it asks first: one click sends nothing, Cancel sends nothing, the yes sends it
    fireEvent.click(screen.getByRole("button", { name: S.handOver }));
    expect(screen.getByText(S.handOverConfirm.replace("<repo>", kept.full_name))).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(sent).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: S.handOver }));
    expect(sent).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: S.handOverYes }));
    await waitFor(() => expect(changed).toHaveBeenCalled());
    expect(sent).toEqual(['PUT /api/v1/projects/claims-app/repository/keeps {"keeps":false}']);
    unmount();
    const others = render(<AppSource id="claims-app" link={kept} autoDeploy={false} owner={false} changed={() => undefined} />);
    await waitFor(() => expect(vi.mocked(fetch)).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: S.handOver })).toBeNull(); // a contributor reads it
    others.unmount();
    render(<AppSource id="claims-app" link={{ repo_id: null, full_name: "", folder: "", state: "pending", keeps: true, kept_commit: null, kept_error: "" }} autoDeploy={false} owner changed={() => undefined} />);
    expect(screen.getByText(S.sourcePending)).toBeDefined();
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(screen.queryByRole("button", { name: S.handOver })).toBeNull();
  });
});
