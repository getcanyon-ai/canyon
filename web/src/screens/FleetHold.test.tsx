import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FleetPage, FleetRow, Hold, Reduced } from "../api";
import { S } from "../strings";
import { Fleet } from "./Fleet";

// Stopping and lifting from the fleet (design/OPERATIONS.md §3.8; design/PERMISSIONS.md, Security
// responders): the row says Held; a responder and an admin get Stop production on a row not held; Lift
// the hold is offered to an admin, and to a responder off the app's team — whose row the server reduces —
// never to anyone on it. The server decides each; the screen offers only what the read says it may.
const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
const page = golden<FleetPage>("fleet/page.json");
const responder = golden<FleetPage>("fleet/responder.json");
const refusal = golden<{ message: string }>("app-page/claims-portal-held-refusal.json");
const hold = (responder.apps.find((app) => app.id === "claims-portal") as Reduced & { hold: Hold }).hold;
const SENTENCE = refusal.message; // the same hold, the same D5 sentence
const team = page.apps.find((app) => app.id === "claims-portal") as FleetRow;
const other = page.apps.find((app) => app.id === "claims-archive") as FleetRow; // production runs v3.1
const empty = page.apps.find((app) => app.id === "commission-calc") as FleetRow; // nothing deployed anywhere
const as = (role: "member" | "responder" | "admin", apps: (FleetRow | Reduced)[]): FleetPage => ({ ...page, banner: "none", viewer: { display: "Rhea Okafor", role, github_logins: [], github_link: false }, apps });
const teamRow = (level: "owner" | "contributor", held: boolean): FleetRow => ({ ...team, access: { ...team.access, level }, ...(held ? { hold } : {}) });
const LIFT = `DELETE /api/v1/projects/claims-portal/envs/prod/hold?held_at=${encodeURIComponent(hold.at)}`;
const stopOf = (app: string) => ({ name: `Stop production of ${app}` });
const holdOf = (app: string) => ({ name: `Hold production of ${app}` });
const liftOf = (app: string) => ({ name: `Lift the hold on ${app}` });

// The server answers the fleet with `fleet` and the hold routes with `answer`; the reader lists the
// requests the screen made to them, with their bodies.
type Answer = { status: number; body?: unknown };
function server(fleet: FleetPage | ((query: URLSearchParams) => FleetPage), answer: Answer | (() => Promise<Answer>) = { status: 204 }) {
  const fetch = vi.fn(async (url: string, _init?: RequestInit) => {
    const at = new URL(url, "http://canyon");
    if (at.pathname === "/api/v1/fleet") return { ok: true, status: 200, json: async () => (typeof fleet === "function" ? fleet(at.searchParams) : fleet) } as unknown as Response;
    if (at.pathname.endsWith("/envs/prod/hold")) {
      const { status, body } = typeof answer === "function" ? await answer() : answer;
      return { ok: status < 300, status, json: async () => body } as unknown as Response;
    }
    return { ok: true, status: 200, json: async () => ({ events: [], total: 0, page: 1, next: null }) } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([url]) => String(url).includes("/hold")).map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
// An answer the test lets go of when it chooses.
function held() {
  let release: (answer: Answer) => void = () => {};
  const pending = () => new Promise<Answer>((resolve) => { release = resolve; });
  return { pending, release: (answer: Answer = { status: 204 }) => act(async () => release(answer)) };
}
const row = (name: string) => screen.getAllByRole("row").find((one) => one.querySelector("td a")?.textContent === name) as HTMLElement;
const card = (name: string) => screen.getAllByRole("link", { name })[0]!.closest("li") as HTMLElement;
const show = async () => {
  render(<Fleet org="Meridian Insurance" />);
  await screen.findAllByRole("link", { name: "claims-portal" });
};
const narrow = () => vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
const stopDialog = async (app: string) => {
  fireEvent.click(screen.getByRole("button", stopOf(app)));
  return within(screen.getByRole("dialog", { name: `Stop and hold ${app} in production?` }));
};

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the fleet's held rows, by who reads them", () => {
  // THE RED (#2069). A responder off the app's team: Stop production on a row that is not held, Lift
  // the hold on one that is, and the row still opens the app, whose runs they read to approve (#2204).
  it("a responder's row shows Stop production, a held one Lift the hold, and each still opens the app", async () => {
    server(responder);
    await show();
    expect(within(row("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
    expect(within(row("claims-portal")).getByText("Held")).toBeDefined();
    expect(within(row("claims-portal")).getByRole("button", liftOf("claims-portal"))).toBeDefined();
    expect(within(row("claims-portal")).queryByRole("button", stopOf("claims-portal"))).toBeNull();
    expect(within(row("claims-portal")).getByRole("link", { name: "claims-portal" }).getAttribute("href")).toBe("#/apps/claims-portal");
  });

  // On a phone each row is a card, with the same offers.
  it("a responder's cards on a phone offer the same: Lift on the held one, Stop on the other", async () => {
    narrow();
    server(responder);
    await show();
    expect(within(card("claims-portal")).getByText("Held")).toBeDefined();
    expect(within(card("claims-portal")).getByRole("button", liftOf("claims-portal"))).toBeDefined();
    expect(within(card("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
    expect(within(card("claims-archive")).queryByRole("button", liftOf("claims-archive"))).toBeNull();
  });

  // A viewer grant puts nobody on the team: the responder's row is reduced, and the lift is offered.
  it("a responder with a read-only grant is offered the lift", async () => {
    const fleet = as("responder", [{ ...(responder.apps[1] as Reduced), access: { level: "viewer" } }]);
    server(fleet);
    await show();
    expect(within(row("claims-portal")).getByRole("button", liftOf("claims-portal"))).toBeDefined();
  });

  // On the team, at owner or contributor, a responder acts with their grant: they may stop it, and never lift its hold.
  it.each(["owner", "contributor"] as const)("a responder on the app's team as %s may stop it and is offered no lift", async (level) => {
    const fleet = as("responder", [teamRow(level, true), { ...other, access: { ...other.access, level } }]);
    server(fleet);
    await show();
    expect(within(row("claims-portal")).getByText("Held")).toBeDefined();
    expect(within(row("claims-portal")).queryByRole("button", liftOf("claims-portal"))).toBeNull();
    expect(within(row("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
  });

  it("an admin may stop any row and lift any hold, their own team's too", async () => {
    const fleet = as("admin", [teamRow("owner", true), other]);
    server(fleet);
    await show();
    expect(within(row("claims-portal")).getByRole("button", liftOf("claims-portal"))).toBeDefined();
    expect(within(row("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
  });

  // Nothing runs in production, so nothing stops (#2696): the row still offers the hold, which stops every promotion,
  // and says it as a hold — on a phone's card too.
  it("offers Hold production, never Stop, on an app with nothing in production", async () => {
    server(as("admin", [other, empty]));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findAllByRole("link", { name: "commission-calc" });
    expect(within(row("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
    expect(within(row("commission-calc")).getByRole("button", holdOf("commission-calc")).textContent).toBe("Hold production");
    expect(within(row("commission-calc")).queryByRole("button", stopOf("commission-calc"))).toBeNull();
  });

  it("offers Hold production on a phone's card of an app with nothing in production", async () => {
    narrow();
    server(as("responder", [other, empty]));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findAllByRole("link", { name: "commission-calc" });
    expect(within(card("claims-archive")).getByRole("button", stopOf("claims-archive"))).toBeDefined();
    expect(within(card("commission-calc")).getByRole("button", holdOf("commission-calc"))).toBeDefined();
  });

  // The team reads the hold and does nothing with it: neither stopping nor lifting is a level's.
  it.each(["owner", "contributor"] as const)("an app's %s reads Held and is offered neither", async (level) => {
    const fleet = as("member", [teamRow(level, true), { ...other, access: { ...other.access, level } }]);
    server(fleet);
    await show();
    expect(within(row("claims-portal")).getByText("Held")).toBeDefined();
    expect(screen.queryByRole("button", { name: /^Lift the hold/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Stop production/ })).toBeNull();
  });

  // Read-only and no access: the server sends their reduced row with no hold, and the screen offers nothing on it.
  it.each(["viewer", "none"] as const)("a member's reduced row (%s), which carries no hold, shows none and offers nothing", async (level) => {
    const fleet = as("member", [{ ...(responder.apps[1] as Reduced), hold: undefined, needs: undefined, access: { level } }]); // a member's reduced row carries no needs
    server(fleet);
    await show();
    expect(screen.queryByText("Held")).toBeNull();
    expect(screen.queryByRole("button", { name: /^Lift the hold/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Stop production/ })).toBeNull();
  });
});

describe("stopping and lifting from the fleet", () => {
  // A hold of a production that runs nothing asks as a hold and says what it does: it stops every promotion (#2696).
  it("asks to hold a production that runs nothing, says what the hold does, and holds it with a reason", async () => {
    const sent = server(as("admin", [other, empty]));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findAllByRole("link", { name: "commission-calc" });
    fireEvent.click(screen.getByRole("button", holdOf("commission-calc")));
    const dialog = within(screen.getByRole("dialog", { name: "Hold commission-calc's production?" }));
    expect(dialog.getByText("Nothing runs in production. A hold stops every promotion until a responder or an admin lifts it.")).toBeDefined();
    expect(dialog.getByText("Its team can still deploy to preview.")).toBeDefined();
    expect(dialog.queryByText(/^Stopping /)).toBeNull();
    expect((dialog.getByRole("button", { name: "Hold production" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("Reason"), { target: { value: hold.reason } });
    fireEvent.click(dialog.getByRole("button", { name: "Hold production" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent()).toEqual([`POST /api/v1/projects/commission-calc/envs/prod/hold ${JSON.stringify({ reason: hold.reason })}`]);
  });

  // A hold of a production that ran nothing says held, never stopped: nothing was stopped.
  it("says production was held, never stopped, when it ran nothing", async () => {
    server(as("admin", [{ ...empty, hold: { ...hold, version: null } }]));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findAllByRole("link", { name: "commission-calc" });
    fireEvent.click(screen.getByRole("button", liftOf("commission-calc")));
    const dialog = within(screen.getByRole("dialog", { name: "Lift the hold on commission-calc?" }));
    expect(dialog.getByText(`Production was held by Rhea Okafor on 2026-10-11: ${hold.reason}. It stays held until a responder or an admin lifts it.`)).toBeDefined();
  });

  it("stops with a reason, saying first what it does, and sends only once a reason is given", async () => {
    const sent = server(responder);
    await show();
    const dialog = await stopDialog("claims-archive");
    expect(dialog.getByText("Stopping claims-archive in production and holding it: nobody can promote it until you or an admin let it run again.")).toBeDefined();
    expect(dialog.getByText("Its team can still deploy to preview.")).toBeDefined();
    expect((dialog.getByRole("button", { name: "Stop production" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("Reason"), { target: { value: "  \n " }}); // only spaces is no reason
    expect((dialog.getByRole("button", { name: "Stop production" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(dialog.getByLabelText("Reason"), { target: { value: hold.reason } });
    fireEvent.click(dialog.getByRole("button", { name: "Stop production" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent()).toEqual([`POST /api/v1/projects/claims-archive/envs/prod/hold ${JSON.stringify({ reason: hold.reason })}`]);
  });

  // While a stop is sending it is sent once, and neither Cancel nor Escape closes the question under it.
  it("sends once, and keeps the question open until it is answered", async () => {
    const answer = held();
    const sent = server(responder, answer.pending);
    await show();
    const dialog = await stopDialog("claims-archive");
    fireEvent.change(dialog.getByLabelText("Reason"), { target: { value: hold.reason } });
    fireEvent.click(dialog.getByRole("button", { name: "Stop production" }));
    expect(dialog.queryByRole("button", { name: "Stop production" })).toBeNull();
    expect(dialog.queryByRole("button", { name: "Cancel" })).toBeNull();
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
    expect(screen.getByRole("dialog")).toBeDefined();
    await answer.release();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent()).toHaveLength(1);
  });

  // An answer closes the question that asked it, never one asked since.
  it("an earlier question's answer leaves a newer question open", async () => {
    const answer = held();
    server(responder, answer.pending);
    await show();
    const first = await stopDialog("claims-archive");
    fireEvent.change(first.getByLabelText("Reason"), { target: { value: hold.reason } });
    fireEvent.click(first.getByRole("button", { name: "Stop production" }));
    fireEvent.click(within(row("claims-portal")).getByRole("button", liftOf("claims-portal")));
    expect(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" })).toBeDefined();
    await answer.release();
    expect(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" })).toBeDefined();
  });

  it("says the server's refusal and keeps the reason typed", async () => {
    const message = "only a security responder or an admin can hold production. Its team takes it offline with canyon undeploy.";
    server(responder, { status: 403, body: { error: "responder_required", message, admins: [] } });
    await show();
    const dialog = await stopDialog("claims-archive");
    fireEvent.change(dialog.getByLabelText("Reason"), { target: { value: hold.reason } });
    fireEvent.click(dialog.getByRole("button", { name: "Stop production" }));
    expect(await dialog.findByText(message)).toBeDefined();
    expect(dialog.getByLabelText("Reason")).toHaveProperty("value", hold.reason);
  });

  // The lift's two choices: leave production stopped, or start what it ran again — each naming the hold it read.
  it.each([
    ["Leave production stopped", LIFT],
    [`Start ${hold.version} again`, `${LIFT}&start=true`],
  ])("lifts with %s, naming the hold it read", async (choice, request) => {
    const sent = server(responder);
    await show();
    fireEvent.click(within(row("claims-portal")).getByRole("button", liftOf("claims-portal")));
    const dialog = within(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" }));
    expect(dialog.getByText(SENTENCE)).toBeDefined();
    fireEvent.click(dialog.getByRole("button", { name: choice }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(sent()).toEqual([request]);
  });

  it("offers no start when production ran nothing when it was held", async () => {
    const fleet = { ...responder, apps: responder.apps.map((app) => (app.id === "claims-portal" ? { ...app, hold: { ...hold, version: null } } : app)) };
    server(fleet);
    await show();
    fireEvent.click(within(row("claims-portal")).getByRole("button", liftOf("claims-portal")));
    const dialog = within(screen.getByRole("dialog"));
    expect(dialog.getByRole("button", { name: "Leave production stopped" })).toBeDefined();
    expect(dialog.queryByRole("button", { name: /^Start / })).toBeNull();
  });

  // A hold placed again while the question is open — read on the fleet's next read — is said, and the
  // question offers nothing on it: its choices were the old hold's.
  it("a hold that changes under the lift's question offers nothing more", async () => {
    let again = false;
    const sent = server(() => (again ? { ...responder, apps: responder.apps.map((app) => (app.id === "claims-portal" ? { ...app, hold: { ...hold, at: "2026-10-12T08:00:00Z", version: "v1.5" } } : app)) } : responder));
    await show();
    fireEvent.click(within(row("claims-portal")).getByRole("button", liftOf("claims-portal")));
    again = true;
    window.dispatchEvent(new Event("focus")); // the fleet's next read: at once on focus, as every ten seconds
    const dialog = within(screen.getByRole("dialog", { name: "Lift the hold on claims-portal?" }));
    expect(await dialog.findByText(S.holdChanged)).toBeDefined();
    expect(dialog.queryByRole("button", { name: "Leave production stopped" })).toBeNull();
    expect(dialog.queryByRole("button", { name: /^Start / })).toBeNull();
    expect(sent()).toEqual([]);
  });
});

describe("the Held filter", () => {
  it("asks the server for the held apps alone, and counts on the Filter button", async () => {
    window.location.hash = "#/fleet";
    const asked: string[] = [];
    server((query) => { asked.push(query.toString()); return responder; });
    await show();
    fireEvent.click(screen.getByRole("button", { name: /^Filter/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Only held apps" }));
    await waitFor(() => expect(asked).toContain("held=true"));
    expect(screen.getByRole("button", { name: /^Filter/ }).textContent).toBe("Filter(1)");
  });

  it("is one of the filters Clear filters takes away", async () => {
    window.location.hash = "#/fleet?held=true&sort=name";
    server(responder);
    await show();
    expect(screen.getByRole("link", { name: "Clear filters" }).getAttribute("href")).toBe("#/fleet?sort=name");
  });
});
