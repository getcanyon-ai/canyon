import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FleetPage } from "../api";
import { Fleet } from "./Fleet";

// The golden is the demo host's page: a loopback banner and an anonymous viewer with no name.
const demo = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8")) as FleetPage;
const signedIn: FleetPage = { ...demo, banner: "none", viewer: { display: "Dana Whitfield", role: "member", github_logins: [], github_link: false } };
const CLAIM = "PUT /api/v1/projects/quote-engine/owner";

// The app's access list as an admin reads it before assigning: Priya already contributes, with a role.
const access = { everyone: null, roles: [], requests: [], entries: [{ kind: "email", grantee: "priya@meridian.example", display: "Priya Raman", level: "contributor", roles: ["adjuster"], granted_by: { kind: "person", subject: "dana", display: "Dana Whitfield" }, granted_at: "2026-09-01T00:00:00Z" }] };

// The fleet and the trail answer their reads; the claim answers `claimed`. The reader lists every
// claim sent, with its body.
function server(page: FleetPage, claimed: { status: number; body: unknown } = { status: 200, body: { owners: [] } }) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method === "PUT") return { ok: claimed.status < 300, status: claimed.status, json: async () => claimed.body } as unknown as Response;
    const status = { project: "quote-engine", envs: {}, last_run: null, promote_run: null, receipt: { org: "Meridian", blocked_by: [], rules: [] } }; // the side panel's read
    const body = url.startsWith("/api/v1/audit") ? { events: [], total: 0, page: 1, next: null } : url.endsWith("/access") ? access : url.endsWith("/status") ? status : page;
    return { ok: true, status: 200, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([, init]) => init?.method === "PUT").map(([url, init]) => `PUT ${url} ${init?.body}`);
}

const row = () => screen.getAllByRole("row").find((one) => one.textContent?.startsWith("quote-engine")) as HTMLElement;

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("claiming an app nobody owns", () => {
  it("claims it under the signed-in viewer's name from the row", async () => {
    const sent = server(signedIn);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Claim quote-engine" }));
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]).toBe(`${CLAIM} {"display":"Dana Whitfield"}`);
  });

  it("claims it from the side panel too", async () => {
    const sent = server(signedIn);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(screen.getByRole("button", { name: "Details for quote-engine" }));
    const panel = screen.getByRole("complementary", { name: "Details for quote-engine" });
    fireEvent.click(within(panel).getByRole("button", { name: "Claim quote-engine" }));
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]).toBe(`${CLAIM} {"display":"Dana Whitfield"}`);
  });

  // The demo host's viewer has no name, and the server refuses an empty one: ask, never send "".
  it("asks for a name on a host with no sign-in, and sends what was typed", async () => {
    const sent = server(demo);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Claim quote-engine" }));
    expect(sent()).toEqual([]);
    expect(within(row()).getByText("Enter your name to claim quote-engine.")).toBeDefined();
    fireEvent.change(within(row()).getByLabelText("Your name"), { target: { value: "  Sam Okafor " } });
    fireEvent.click(within(row()).getByRole("button", { name: "Claim" }));
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]).toBe(`${CLAIM} {"display":"Sam Okafor"}`);
  });

  // Claim is offered by the read's claimable alone: an app nobody owns may still hold a secret, a deploy,
  // a repository or a group owner, and the server refuses a member's claim on it.
  it("is not offered where the read says the server would refuse the claim", async () => {
    const refused: FleetPage = { ...signedIn, apps: signedIn.apps.map((app) => (app.id === "quote-engine" ? { ...app, claimable: false } : app)) };
    server(refused);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    expect(within(row()).getByText("No owner")).toBeDefined();
    expect(within(row()).queryByRole("button", { name: "Claim quote-engine" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Details for quote-engine" }));
    expect(within(screen.getByRole("complementary", { name: "Details for quote-engine" })).queryByRole("button", { name: "Claim quote-engine" })).toBeNull();
  });

  it("shows the server's own sentence when it refuses", async () => {
    server(signedIn, { status: 400, body: { error: "invalid_display", message: "a display name is 1 to 128 characters" } });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Claim quote-engine" }));
    expect(await within(row()).findByText("a display name is 1 to 128 characters")).toBeDefined();
  });
});

// Assign (the redesign): an admin names the owner on a host where people sign in, through the share route
// admins may call on any app; nobody else is offered it, and a demo host, where nothing is shared,
// offers only Claim.
describe("assigning an owner", () => {
  const admin: FleetPage = { ...signedIn, viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false } };
  const SHARE = "PUT /api/v1/projects/quote-engine/access/email/priya%40meridian.example";

  it("lets an admin make someone the owner of an app nobody owns", async () => {
    const sent = server(admin, { status: 200, body: { everyone: null, entries: [], roles: [], requests: [] } });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Assign an owner to quote-engine" }));
    fireEvent.change(within(row()).getByLabelText("Owner of quote-engine"), { target: { value: " priya@meridian.example " } });
    fireEvent.click(within(row()).getByRole("button", { name: "Assign" }));
    await waitFor(() => expect(sent()).toEqual([`${SHARE} {"level":"owner","roles":["adjuster"]}`])); // her role kept: the grant is written whole
  });

  it("assigns someone with no grant yet with no roles", async () => {
    const sent = server(admin, { status: 200, body: access });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Assign an owner to quote-engine" }));
    fireEvent.change(within(row()).getByLabelText("Owner of quote-engine"), { target: { value: "Marcus@Meridian.example" } });
    fireEvent.click(within(row()).getByRole("button", { name: "Assign" }));
    await waitFor(() => expect(sent()).toEqual([`PUT /api/v1/projects/quote-engine/access/email/marcus%40meridian.example {"level":"owner","roles":[]}`]));
  });

  it("is not offered to a member, who can still claim", async () => {
    server(signedIn);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    expect(within(row()).queryByRole("button", { name: "Assign an owner to quote-engine" })).toBeNull();
    expect(within(row()).getByRole("button", { name: "Claim quote-engine" })).toBeDefined();
  });

  it("is not offered on a host with sign-in off, where nothing is shared", async () => {
    server(demo);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    expect(within(row()).queryByRole("button", { name: "Assign an owner to quote-engine" })).toBeNull();
  });

  it("shows the server's own sentence when it refuses", async () => {
    server(admin, { status: 403, body: { error: "owner_required", message: "only an owner or an admin can share this app" } });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("quote-engine");
    fireEvent.click(within(row()).getByRole("button", { name: "Assign an owner to quote-engine" }));
    fireEvent.change(within(row()).getByLabelText("Owner of quote-engine"), { target: { value: "priya@meridian.example" } });
    fireEvent.click(within(row()).getByRole("button", { name: "Assign" }));
    expect(await within(row()).findByText("only an owner or an admin can share this app")).toBeDefined();
  });
});
