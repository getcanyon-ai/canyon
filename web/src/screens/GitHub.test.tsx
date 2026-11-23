import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { GitHubRead } from "../api";
import { S } from "../strings";
import { GitHubSettings } from "./GitHub";

const acme = { account: "acme-canyon", app_slug: "canyon-acme-canyon", keeps_code: true, imports_new: true, installed: true, last_event_at: new Date(Date.now() - 120_000).toISOString(), links_accounts: true };
const labs = { account: "acme-labs", app_slug: "canyon-acme-labs", keeps_code: false, imports_new: false, installed: false, last_event_at: null, links_accounts: false };

const repositories = {
  repositories: [
    { repo_id: 555, full_name: "acme/claims-portal", org: "acme", default_branch: "main", apps: [] },
    { repo_id: 81723, full_name: "acme/platform", org: "acme", default_branch: "main", apps: [{ folder: "apps/billing", id: "funny-snake-4k2", name: "billing", state: "linked" }] },
  ],
  unreachable: ["acme-labs"],
};

// The server answers the two reads, and every write with 200, recording "METHOD url body".
function server(read: GitHubRead) {
  const sent: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method !== undefined && init.method !== "GET") sent.push(`${init.method} ${url} ${String(init.body ?? "")}`.trim());
    const body = url === "/api/v1/github/repositories" ? repositories : url === "/api/v1/github" ? read
      : url.endsWith("/client-secret") ? { org: { ...labs, links_accounts: true } }
      : url.startsWith("/api/v1/github/orgs/") ? { org: { ...read.orgs[0], imports_new: true } } : { project: { id: "calm-fox-1ab" } };
    return { ok: true, status: 200, json: async () => body } as unknown as Response;
  }));
  return sent;
}
afterEach(() => vi.unstubAllGlobals());

// Settings › GitHub (design/specs/screens.md, A0b; #1914): an org's login, its two answers, then GitHub.
describe("GitHubSettings", () => {
  it("registers on the org typed, with its two answers, once both are given", async () => {
    server({ reachable: true, orgs: [] });
    render(<GitHubSettings />);
    fireEvent.change(await screen.findByLabelText("GitHub organization"), { target: { value: "acme-canyon" } });
    expect((screen.getByRole("button", { name: "Register Canyon on acme-canyon" }) as HTMLButtonElement).disabled).toBe(true); // which kind of org it is, not yet said
    fireEvent.click(screen.getByRole("radio", { name: S.githubOnly }));
    fireEvent.click(screen.getByRole("checkbox", { name: S.githubKeep }));
    expect(screen.getByRole("link", { name: "Register Canyon on acme-canyon" }).getAttribute("href")).toBe("/api/v1/github/app/new?org=acme-canyon&imports_new=true&keeps_code=true");
    fireEvent.click(screen.getByRole("radio", { name: S.githubOther }));
    fireEvent.click(screen.getByRole("checkbox", { name: S.githubKeep }));
    expect(screen.getByRole("link", { name: "Register Canyon on acme-canyon" }).getAttribute("href")).toBe("/api/v1/github/app/new?org=acme-canyon&imports_new=false&keeps_code=false");
    expect(screen.getByText(S.githubRegisterNote)).toBeDefined();
    expect(screen.queryByRole("table")).toBeNull(); // no org connected, no table
  });

  it("says GitHub can't reach a localhost Canyon in place of the button", async () => {
    server({ reachable: false, orgs: [] });
    render(<GitHubSettings />);
    expect(await screen.findByText(S.githubUnreachable)).toBeDefined();
    expect(screen.queryByRole("link", { name: /^Register Canyon/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Register Canyon/ })).toBeNull();
  });

  it("lists each connected org, and offers keeping code only where none keeps it", async () => {
    server({ reachable: true, orgs: [acme, labs] });
    render(<GitHubSettings />);
    const table = (await screen.findAllByRole("table"))[0]!;
    const [first, second] = within(table).getAllByRole("row").slice(1);
    expect(within(first!).getByRole("link", { name: "acme-canyon" }).getAttribute("href")).toBe("https://github.com/apps/canyon-acme-canyon");
    expect(first!.textContent).toMatch(/^acme-canyonKeeps codeBecome apps.*2m.*Receiving events/); // the chip is the org's, beside its name
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Organization", "New repositories", "Last event", "State"]);
    expect(second!.textContent).toMatch(/Picked by an admin.*None yet.*Not installed/);
    expect(within(second!).getByRole("link", { name: "Install" }).getAttribute("href")).toBe("https://github.com/apps/canyon-acme-labs/installations/new");
    const keep = screen.getByRole("checkbox", { name: S.githubKeep }) as HTMLInputElement;
    expect(keep.disabled).toBe(true);
    expect(screen.getByText(S.githubKeeper.replace("<org>", "acme-canyon"))).toBeDefined();
  });

  // On a phone each organization is a card, what it does with new repositories inside it (#126).
  it("on a phone, reads each connected org as a card", async () => {
    vi.stubGlobal("matchMedia", (media: string) => ({ matches: media === "(max-width: 619px)", addEventListener: () => {}, removeEventListener: () => {} }));
    server({ reachable: true, orgs: [acme, labs] });
    render(<GitHubSettings />);
    const card = within((await screen.findByRole("link", { name: "acme-canyon" })).closest("li")!);
    expect(card.getByRole("combobox", { name: "New repositories in acme-canyon" })).toBeDefined();
    for (const words of ["New repositories", "Last event", "State", "Receiving events"]) expect(card.getByText(words)).toBeDefined();
    expect(screen.queryByRole("columnheader", { name: "New repositories" })).toBeNull();
  });

  // Repositories (#1917): what the Apps see, the apps linked to each, and the three things an admin does.
  it("lists the repositories, makes an app of one, links and unlinks", async () => {
    const sent = server({ reachable: true, orgs: [acme] });
    render(<GitHubSettings />);
    expect(await screen.findByText(S.githubUnanswered.replace("<org>", "acme-labs"))).toBeDefined();
    const table = within(screen.getAllByRole("table")[1]!);
    const [portal, platform] = table.getAllByRole("row").slice(1);
    expect(platform!.textContent).toMatch(/acme\/platform.*apps\/billing.*billing/);
    fireEvent.click(within(portal!).getByRole("button", { name: "Make it an app" }));
    fireEvent.click(screen.getByRole("button", { name: "Make the app" }));
    await waitFor(() => expect(sent).toEqual(['POST /api/v1/github/repositories/555/import {"folder":""}']));
    fireEvent.click(within(table.getAllByRole("row")[1]!).getByRole("button", { name: "Link to an app" }));
    fireEvent.change(screen.getByLabelText("App id"), { target: { value: "quote-engine" } });
    fireEvent.click(screen.getByRole("button", { name: "Link" }));
    await waitFor(() => expect(sent[1]).toBe('PUT /api/v1/projects/quote-engine/repository {"repo_id":555,"folder":""}'));
    fireEvent.click(screen.getByRole("button", { name: "Unlink billing" }));
    await waitFor(() => expect(sent[2]).toBe("DELETE /api/v1/projects/funny-snake-4k2/repository"));
    expect(screen.getByText(S.githubImportNote)).toBeDefined();
  });

  // Whether an org's new repositories become apps is switched on its row, saved as it is flipped (#1923).
  it("switches an org between making apps of new repositories and an admin picking", async () => {
    const sent = server({ reachable: true, orgs: [labs] });
    render(<GitHubSettings />);
    fireEvent.change(await screen.findByRole("combobox", { name: "New repositories in acme-labs" }), { target: { value: "apps" } });
    await waitFor(() => expect(sent[0]).toBe('PUT /api/v1/github/orgs/acme-labs {"imports_new":true}'));
  });
});

// An App registered before Canyon kept its client secret links no GitHub accounts (#2228): the admin is
// told where GitHub generates one and which callback to add, pastes it, and the ask goes away once saved.
describe("an App's client secret", () => {
  it("is asked for an App without one, saved trimmed, and the ask goes away", async () => {
    const sent = server({ reachable: true, orgs: [acme, labs] });
    render(<GitHubSettings />);
    const ask = await screen.findByRole("heading", { name: S.githubSecretTitle.replace("<org>", "acme-labs") });
    expect(screen.queryByRole("heading", { name: S.githubSecretTitle.replace("<org>", "acme-canyon") })).toBeNull();
    expect(ask.parentElement?.textContent).toContain(`${window.location.origin}/api/v1/github/link/callback`);
    expect((screen.getByRole("button", { name: S.githubSecretSave }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Client secret"), { target: { value: " 0123abcd " } });
    fireEvent.click(screen.getByRole("button", { name: S.githubSecretSave }));
    await waitFor(() => expect(screen.queryByRole("heading", { name: S.githubSecretTitle.replace("<org>", "acme-labs") })).toBeNull());
    expect(sent).toContain(`PUT /api/v1/github/orgs/acme-labs/client-secret {"client_secret":"0123abcd"}`);
  });
});
