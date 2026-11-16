import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { App, Env, FleetPage, FleetRow } from "../api";
import { S } from "../strings";
import { Fleet } from "./Fleet";
import { ownerHref } from "./Owner";

const golden = JSON.parse(
  readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8"),
) as Omit<FleetPage, "apps"> & { apps: FleetRow[] }; // every row the golden carries is a team row

const PRIYA = "priya@meridian.example";

// The fleet read as the route answers it: the owner is a subject matched exactly, never a display
// name, and the rows come back in the server's order. `shape` bends the matching rows for one case.
function server(shape: (app: App) => App = (app) => app) {
  const fetch = vi.fn(async (url: string) => {
    const subject = new URL(url, "http://canyon").searchParams.get("owner");
    const asked = subject === null ? golden.apps : golden.apps.filter((app) => app.owners.some((one) => one.subject === subject));
    const apps = asked.map(shape);
    return ({ ok: true, status: 200, json: async () => ({ ...golden, apps }) }) as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url]) => url);
}

const viewing = (subject: string) => { window.location.hash = `#/fleet?owner=${encodeURIComponent(subject)}`; };
const cards = () => screen.getAllByRole("listitem");

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the owner view", () => {
  it("keeps a plus sign or a space in an account name out of the link's meaning", () => {
    expect(ownerHref("priya+test@meridian.example")).toBe("#/fleet?owner=priya%2Btest%40meridian.example");
    expect(new URLSearchParams(ownerHref("a b@meridian.example").split("?")[1]).get("owner")).toBe("a b@meridian.example");
  });

  // The golden's claims-archive prod has stopped: its card says so and links nowhere.
  it("says a stopped app stopped on its card, with no link to it", async () => {
    server();
    viewing("tomas@meridian.example");
    render(<Fleet org="Meridian" />);
    const card = (await screen.findAllByRole("link", { name: "claims-archive" }))[0]!.closest("li") as HTMLElement;
    expect(within(card).getByText("Stopped")).toBeDefined();
    expect(within(card).queryByRole("link", { name: /claims-archive\.prod/ })).toBeNull();
  });

  it("shows the apps the server answers for one owner, as cards in its order", async () => {
    server();
    viewing(PRIYA);
    const { container } = render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("heading", { name: "Priya Raman", level: 1 });
    expect(cards().map((card) => within(card).getByRole("heading", { level: 2 }).textContent))
      .toEqual(["commission-calcReact + Vite", "endorsement-hubRemix · Postgres"]);
    const [blocked, live] = cards() as [HTMLElement, HTMLElement];
    expect(within(blocked).getByRole("link", { name: "commission-calc" }).getAttribute("href")).toBe("#/apps/commission-calc");
    expect(blocked.textContent).toContain("v0.9Blocked");
    expect(within(blocked).getAllByText("Not deployed")).toHaveLength(2);
    expect(within(live).getByRole("link", { name: /endorsement-hub\.preview/ }).getAttribute("href"))
      .toBe("http://endorsement-hub.preview.localhost:7777");
    expect(screen.queryByRole("table")).toBeNull();
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i);
  });

  it("never shows the whole fleet as one person's while their page is still coming", async () => {
    vi.stubGlobal("fetch", vi.fn((url: string) =>
      url.includes("owner=")
        ? new Promise<Response>(() => {}) // still in flight
        : Promise.resolve(({ ok: true, status: 200, json: async () => (url.startsWith("/api/v1/audit") ? { events: [], next: null } : golden) }) as unknown as Response)));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("table");
    viewing(PRIYA);
    await waitFor(() => expect(screen.queryByRole("table")).toBeNull());
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
  });

  it("asks the server for the subject in the hash and asks for nothing else", async () => {
    const sent = server();
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect([...new Set(sent())]).toEqual(["/api/v1/fleet?owner=priya%40meridian.example"]);
  });

  it("keeps the org's four boxes and its search box off one person's view", async () => {
    server();
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    for (const box of [/apps? with secrets in code/, /never reviewed/, /no owner/, /apps? blocked/]) {
      expect(screen.queryByRole("link", { name: box })).toBeNull();
    }
    expect(screen.queryByRole("searchbox")).toBeNull();
    expect(screen.queryAllByRole("combobox")).toHaveLength(0); // no filter bar either: the view is one person's apps
    expect(screen.queryByRole("button", { name: /needs you/i })).toBeNull();
  });

  it("says Never reviewed once, not on every card", async () => {
    server((app) => ({ ...app, gate_state: "never_reviewed" }));
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(cards()).toHaveLength(2);
    expect(screen.getAllByText(S.neverReviewed)).toHaveLength(1);
  });

  it("keeps the owner when it asks for the next page", async () => {
    const fetch = vi.fn(async (url: string) => {
      const asked = new URL(url, "http://canyon").searchParams;
      const apps = golden.apps.filter((app) => app.owners.some((one) => one.subject === asked.get("owner")));
      return ({ ok: true, status: 200, json: async () => ({ ...golden, apps, total: 60, page: Number(asked.get("page") ?? 1) }) }) as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    fireEvent.click(await screen.findByRole("button", { name: "Next" }));
    await waitFor(() => expect(fetch.mock.calls.map(([url]) => url)).toContain("/api/v1/fleet?owner=priya%40meridian.example&page=2"));
    expect((await screen.findByText(/of 60 apps/)).textContent).toBe("51–60 of 60 apps · Page 2 of 2");
  });

  it("marks the card of an app nobody has reviewed", async () => {
    server((app) => ({ ...app, gate_state: "never_reviewed" as const }));
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.getAllByText(S.neverReviewedMark)).toHaveLength(cards().length);
    expect(screen.getAllByText(S.neverReviewed)).toHaveLength(1);
  });

  it("names the version an environment runs, not the newest upload", async () => {
    server();
    viewing("dana@meridian.example");
    render(<Fleet org="Meridian Insurance" />);
    const card = await screen.findByText("claims-portal");
    expect((card.closest("li") as HTMLElement).textContent).toContain("v1.3");
  });

  it("draws an environment waiting on its first release by the release it waits on", async () => {
    const pending = { via: "webhook" as const, release: "rel_8f2a", event: "deploy" as const, sent_at: "2026-09-27T10:00:00Z", state: "refused" as const };
    server((app) => (app.id === "claims-portal" ? { ...app, envs: { preview: { run_by: "org", pending } } } : app));
    viewing("dana@meridian.example");
    render(<Fleet org="Meridian Insurance" />);
    const card = await screen.findByText("claims-portal");
    expect((card.closest("li") as HTMLElement).textContent).toContain("Your platform refused the release");
  });

  it("says the release a running environment waits on beside what runs there", async () => {
    const pending = { via: "webhook" as const, release: "rel_8f2a", event: "deploy" as const, sent_at: "2026-09-27T10:00:00Z", state: "undelivered" as const };
    server((app) => (app.id === "claims-portal" ? { ...app, envs: { preview: { ...(app.envs.preview as Env), run_by: "org", pending } } } : app));
    viewing("dana@meridian.example");
    render(<Fleet org="Meridian Insurance" />);
    const card = (await screen.findByText("claims-portal")).closest("li") as HTMLElement;
    expect(card.textContent).toContain("v1.3");
    expect(card.textContent).toContain("Your platform did not accept the release");
  });

  it("names the owner it was asked for, not the one listed first", async () => {
    server((app) => ({ ...app, owners: [{ kind: "person" as const, subject: "zara@meridian.example", display: "Zara Qureshi" }, ...app.owners] }));
    viewing(PRIYA);
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByRole("heading", { name: "Priya Raman", level: 1 })).toBeDefined();
  });

  it("names the subject when it owns nothing here", async () => {
    server();
    viewing("nobody@meridian.example");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("No apps — nobody owns anything here under that name.");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("nobody@meridian.example");
  });
});
