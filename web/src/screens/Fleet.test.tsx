import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AuditPage, FleetPage, FleetRow, Need } from "../api";
import { FLEET, S } from "../strings";
import { Fleet } from "./Fleet";

const golden = JSON.parse(
  readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8"),
) as FleetPage;
// The org's trail as the app page's golden carries it: the strip reads the same route with no project.
const audit = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { audit: AuditPage[] }).audit[0];

// The server is a function of the query it is sent (the trail read answers the golden's first page);
// the returned reader lists every URL asked for.
function server(answer: (query: URLSearchParams) => unknown, ok = true, status = 200) {
  const fetch = vi.fn(async (url: string) =>
    ({ ok, status, json: async () => (url.startsWith("/api/v1/audit") ? audit : answer(new URL(url, "http://canyon").searchParams)) }) as unknown as Response);
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url]) => url);
}

const rows = () => screen.getAllByRole("row").slice(1) as HTMLTableRowElement[];
// The first cell carries the name and, when nobody has reviewed the app, the mark beside it; the
// name is the link, so a row is found by what its link says and never by the whole cell.
const named = (row: HTMLTableRowElement) => row.cells[0]?.querySelector("a")?.textContent;
const names = () => rows().map(named);
const cell = (name: string, column: number) => rows().find((row) => named(row) === name)?.cells[column]?.textContent;

// Each test gets its own browser storage: Node 25's own localStorage global shadows jsdom's and
// answers nothing, so the screen is handed one that behaves the same on every Node.
function storage(refuse = false) {
  const held = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => { if (refuse) throw new Error("denied"); return held.get(key) ?? null; },
    setItem: (key: string, value: string) => { if (refuse) throw new Error("denied"); held.set(key, value); },
  });
}

const answering = (page: unknown) => ({ ok: true, status: 200, json: async () => page }) as unknown as Response;

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the fleet screen", () => {
  it("shows the server's page in the server's order, a blocked app first", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(names()).toEqual(golden.apps.map((app) => app.name));
    expect([cell("commission-calc", 1), cell("commission-calc", 2)]).toEqual(["v0.9", "Blocked"]);
    expect(cell("claims-portal", 4)).toBe("—"); // nothing runs; holding it is the row's peek's, never every row's (the UX review)
    expect(cell("claims-portal", 3)).toBe("v1.3"); // the version preview runs, named as the server names it
    expect(cell("quote-engine", 5)).toBe("No ownerClaim");
    expect(screen.getAllByRole("columnheader").map((head) => head.textContent)).toEqual(["App", "Version", "Review", "Preview", "Production", "Owner", "Updated", ""]); // the last is the row's way into its panel
  });

  // Two people can carry one name, so the link to a person's apps carries the subject: here the
  // second Priya Raman is somebody else, and her apps are not the first one's.
  it("links an owner to their view by subject, never by their display name", async () => {
    const namesake = { kind: "person" as const, subject: "priya.raman@meridian.example", display: "Priya Raman" };
    server(() => ({ ...golden, apps: golden.apps.map((app) => (app.id === "endorsement-hub" ? { ...app, owners: [namesake] } : app)) }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.getAllByRole("link", { name: "Priya Raman" }).map((link) => link.getAttribute("href")))
      .toEqual(["#/fleet?owner=priya%40meridian.example", "#/fleet?owner=priya.raman%40meridian.example"]);
  });


  // A never-reviewed app's newest upload is awaiting review, but the Review cell says what matters:
  // no run has ever looked at it. One word for one state (the audit's C5).
  it("says Never reviewed in the Review cell of an app nobody has reviewed, and nowhere else", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(cell("quote-engine", 2)).toBe("Never reviewed");
    expect(screen.getAllByText("Never reviewed")).toHaveLength(1);
  });

  // The Review column is the review's word, not where the version went next: once a version runs,
  // the app's newest run says what its review said, and a block is never hidden behind preview.
  it("reads a running version's review from the app's newest run, never from where it runs", async () => {
    const hub = golden.apps.find((app) => app.id === "endorsement-hub") as FleetRow;
    server(() => ({ ...golden, apps: [...golden.apps.filter((app) => app.id !== "endorsement-hub"), { ...hub, gate_state: "blocked" }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(["premium-ledger", "fnol-intake", "endorsement-hub"].map((name) => cell(name, 2))).toEqual(["Passed", "Passed", "Blocked"]);
    expect(cell("claims-archive", 2)).toBe("Awaiting review");
  });

  // A passed review with advisories says how many beside its pill, so it never looks like a clean one
  // (the test cluster audit: case-vuln-dep's 7 CVEs looked like case-next-live); a blocked one's pill says enough.
  it("says how many warnings a passed review carries, and no more", async () => {
    server(() => ({ ...golden, apps: golden.apps.map((app) => (app.id === "premium-ledger" || app.id === "claims-portal" ? { ...app, warnings: 2 } : app)) }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(cell("premium-ledger", 2)).toBe("Passed2 warnings");
    expect(cell("fnol-intake", 2)).toBe("Passed"); // none
    expect(cell("claims-portal", 2)).not.toMatch(/warning/); // blocked: the block is what it says
  });

  // Below contributor the server answers an app as its name, owners and summary and nothing else,
  // after every row of the viewer's own teams (contracts/routes.md, App teams).
  const nora = { kind: "person", subject: "nora@meridian.example", display: "Nora Lind" };
  const reduced = { id: "pricing-lab", name: "pricing-lab", owners: [nora], owner_groups: [], summary: "Tries new premium curves", claimable: false, access: { level: "none" } };
  const member = { banner: "none", viewer: { display: "Tomas Berg", role: "member", github_logins: [], github_link: false } };

  it("shows an app off the viewer's teams as its name, owner and summary, and a team's row as before", async () => {
    server(() => ({ ...golden, ...member, apps: [...golden.apps, reduced] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("pricing-lab");
    expect(names()).toEqual([...golden.apps.map((app) => app.name), "pricing-lab"]);
    const row = rows().find((one) => named(one) === "pricing-lab") as HTMLTableRowElement;
    // what the reader may do about it, said once in its Access cell, never a dash in every column (the UX review)
    expect([...row.cells].map((one) => one.textContent)).toEqual(["pricing-labTries new premium curves", "", "", "—", "—", "NLNora Lind", "", FLEET.noAccess + FLEET.askToJoin, ""]);
    expect(cell("commission-calc", 2)).toBe("Blocked");
    expect(screen.queryByRole("button", { name: "Details for pricing-lab" })).toBeNull();
  });

  // The Access column, from the contract's own member page: how far each team row is shared and
  // the member's own place on it; a reduced row says nothing of either.
  const memberPage = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/member.json"), "utf8")) as FleetPage;
  it("says who can open each team row, and to a member where they stand on it", async () => {
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    // one line each: the member's place, then how far it is shared (the UX review: it wrapped to four lines)
    expect(["claims-archive", "fnol-intake", "claims-portal", "quote-engine"].map((name) => cell(name, 7)))
      .toEqual(["Owner · shared with 2", "Contributor · shared with 2", FLEET.readOnly + FLEET.askToJoin, FLEET.noAccess + FLEET.askToJoin]);
  });

  // A row the reader can't open whole asks from where it says so: Ask to join sends the app's access request, and says Asked.
  it("asks to join an app from its row, once", async () => {
    const sent = server(() => memberPage);
    render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    const row = rows().find((one) => named(one) === "quote-engine")!;
    fireEvent.click(within(row).getByRole("button", { name: `${FLEET.askToJoin} quote-engine` }));
    expect(await within(row).findByText(FLEET.asked)).toBeDefined();
    expect(sent()).toContain("/api/v1/projects/quote-engine/access-requests");
    expect(within(row).queryByRole("button", { name: `${FLEET.askToJoin} quote-engine` })).toBeNull();
  });

  // A member's counts are their own apps while the table lists the whole org: the line under the title says both.
  // A responder off every team (the UX review: their fleet was mostly "—"): no tiles, which would all be 0, and the table
  // is what they read and act on — the app, its production with its hold, its owner.
  it("gives a responder off every team no tiles and only the columns they read", async () => {
    const responder = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/responder.json"), "utf8")) as FleetPage;
    server(() => responder);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("claims-archive");
    expect(document.querySelector("dl")).toBeNull(); // no team, so no counts: zeros help no one
    expect(screen.getByText("Every app in the org")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Your apps" })).toBeNull(); // none of theirs: no scope to choose
    expect(screen.getAllByRole("columnheader").map((head) => head.textContent)).toEqual(["App", "Production", "Owner"]);
    expect(rows().find((row) => named(row) === "claims-archive")!.cells[1]!.textContent).toBe(`${S.running}Open production${S.stopProduction}`);
    expect(within(rows().find((row) => named(row) === "claims-archive")!).getByRole("button", { name: "Stop production of claims-archive" })).toBeDefined();
    expect(within(rows().find((row) => named(row) === "claims-portal")!).getByRole("button", { name: "Lift the hold on claims-portal" })).toBeDefined();
  });

  it("says a member's counts are their apps' while the table is every app in the org", async () => {
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    expect(await screen.findByText(FLEET.allScopeMember)).toBeDefined();
  });

  // claims-portal is shared with the member read-only: its row opens the app where it runs, and says
  // so in the team's words where it does not — one that stopped opens nothing (contracts/routes.md).
  it("opens a read-only member's app from its row where it runs, on a table and on a phone", async () => {
    server(() => memberPage);
    const { unmount } = render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    const row = rows().find((one) => named(one) === "claims-portal")!;
    expect(within(row.cells[3]!).getByRole("link", { name: "Open preview" }).getAttribute("href")).toBe("http://claims-portal.preview.localhost:7777");
    expect(cell("claims-portal", 4)).toBe("—");
    unmount();
    const stopped = { ...memberPage, apps: memberPage.apps.map((app) => (app.id === "claims-portal" ? { ...app, envs: { preview: { url: "http://claims-portal.preview.localhost:7777", state: "stopped" } } } : app)) } as FleetPage;
    server(() => stopped);
    const again = render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    expect(cell("claims-portal", 3)).toBe("Open previewStopped");
    expect(within(rows().find((one) => named(one) === "claims-portal")!.cells[3]!).queryByRole("link")).toBeNull();
    again.unmount();
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    const card = (await screen.findAllByRole("link", { name: "claims-portal" }))[0]!.closest("li") as HTMLElement;
    expect(within(card).getByRole("link", { name: "Open preview" }).getAttribute("href")).toBe("http://claims-portal.preview.localhost:7777");
  });

  // claims-archive's prod runs v3.1 while its removal waits on the org: both are said.
  it("says the release a running environment waits on beside what runs there", async () => {
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    expect(cell("claims-archive", 4)).toContain("v3.1");
    expect(cell("claims-archive", 4)).toContain(S.removingShort); // the short form: the table's cells stay a line or two (the UX review)
    expect(cell("claims-archive", 4)).not.toContain(S.removingPlatform);
    expect(within(rows().find((row) => named(row) === "claims-archive")!.cells[4]!).getByRole("link").getAttribute("href")).toBe("http://claims-archive.prod.localhost:7777");
  });

  // broker-portal, the contract's app waiting on its first release: its preview says what it waits on, not a link.
  it("draws a row whose preview waits on its first release by the release it waits on", async () => {
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    await screen.findByText("broker-portal");
    const clock = new Date("2026-09-02T10:00:00Z").toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    expect([cell("broker-portal", 4), cell("broker-portal", 3)]).toEqual(["—", `Sent ${clock} · not running yet`]);
  });

  it("an admin's row on no team of theirs says only who can open it: Private, or Everyone before any count", async () => {
    const row = memberPage.apps[0]!;
    const shared = (everyone: boolean, people: number) => ({ ...row, id: `app-${people}${everyone}`, name: `app-${people}${everyone}`, access: { level: "none", everyone, people, groups: 0, requests: 0 } });
    server(() => ({ ...memberPage, viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false }, apps: [shared(false, 0), shared(true, 3)] }));
    render(<Fleet org="Meridian" />);
    await screen.findByText("app-0false");
    expect([cell("app-0false", 7), cell("app-3true", 7)]).toEqual(["Private", "Everyone"]);
  });

  it("draws a fleet of apps the viewer is on no team of, a missing summary said so", async () => {
    server(() => ({ ...golden, ...member, apps: [{ ...reduced, summary: "" }] }));
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByText(S.noDescription)).toBeDefined();
    expect(names()).toEqual(["pricing-lab"]);
  });

  it("shows an owner's app off the viewer's teams as its name and summary in the owner view", async () => {
    window.location.hash = "#/fleet?owner=nora%40meridian.example";
    server(() => ({ ...golden, ...member, apps: [reduced] }));
    render(<Fleet org="Meridian Insurance" />);
    const card = within((await screen.findByRole("link", { name: "pricing-lab" })).closest("li") as HTMLElement);
    expect(card.getByText("Tries new premium curves")).toBeDefined();
    expect(card.queryByText("Production")).toBeNull();
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("Nora Lind");
  });

  // The strip is the org's trail, newest first, in the plain words strings.ts gives each action:
  // who did what, and to which app, with no route pattern and no version id in sight.
  // The fleet top (the approved redesign): the server's counts, whatever page or filter is shown —
  // counted from one page in the browser, the line was wrong past 200 apps and gone on page two.
  const statsCells = () => screen.getAllByRole("definition").map((one) => one.textContent);
  it("counts the fleet from the server's stats, on any page and under any filter", async () => {
    const stats = { apps: 1284, owners: 212, production: 409, preview: 977, passed: 1101, deploys_7d: 3150, production_7d: 288 };
    server(() => ({ ...golden, stats, total: 3, page: 2 }));
    window.location.hash = "#/fleet?stage=blocked&page=2";
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(screen.getAllByRole("term").map((one) => one.textContent)).toEqual(["Apps", "Deployed to production", "Deploys, last 7 days", "Passed their latest review"]);
    expect(statsCells()).toEqual(["1,284", "212 owners", "409", "977 deployed to preview", "3,150", "288 to production", "1,101 of 1,284", "the newest upload of each app"]);
    cleanup();
    server(() => ({ ...golden, ...member, stats: { ...stats, apps: 1, owners: 1 } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.getAllByRole("term")[0]!.textContent).toBe("Your apps"); // a member's counts are their team apps'
    expect(statsCells().slice(0, 2)).toEqual(["1", "1 owner"]);
  });

  // The approved redesign: one line per run under a day, "You" for the reader, asked of the server by
  // the actions it shows — never 40 rows read and thinned in the browser.
  it("tells each run in one line under its day, asking the server for exactly what it shows", async () => {
    vi.setSystemTime(new Date("2026-09-02T10:00:00Z"));
    const sent = server(() => ({ ...golden, banner: "none", viewer: { display: "Dana's agent", role: "admin", github_logins: [], github_link: false } }));
    render(<Fleet org="Meridian Insurance" />);
    const strip = within(await screen.findByRole("region", { name: "Recent activity" }));
    expect(strip.getAllByRole("heading", { level: 3 }).map((one) => one.textContent)).toEqual(["Today", "Yesterday"]);
    expect(strip.getAllByRole("listitem").map((row) => [...row.querySelectorAll("p")].map((one) => one.textContent).join(" — "))).toEqual([
      "Dana's agent sent claims-portal v1.4 for review. — Blocked · 1 blocking",
      "Dana's agent deployed claims-portal v1.3 to preview.",
    ]);
    expect(strip.getAllByRole("listitem")[0]!.textContent).toMatch(/\d\d:\d\d$/); // a clock time on the right
    expect(strip.getByText(/^Sign-ins, log reads and resource steps are on the audit page/)).toBeDefined();
    const asked = new URL(sent().find((url) => url.startsWith("/api/v1/audit"))!, "http://canyon").searchParams;
    expect(asked.get("action")?.split(",")).toContain("verdict");
    expect(asked.get("action")?.split(",")).not.toContain("sign in");
    expect(asked.get("limit")).toBe("100");
  });


  // Pagination once showed no total, no page count and no page size.
  it("says where the page sits in the server's total and asks for exactly the page pressed", async () => {
    const sent = server((query) => ({ ...golden, total: 1284, page: Number(query.get("page") ?? 1) }));
    window.location.hash = "#/fleet?stage=blocked";
    render(<Fleet org="Meridian Insurance" />);
    const pages = within(await screen.findByRole("navigation", { name: "Fleet pages" }));
    expect(pages.getByText(/of 1,284 apps/).textContent).toBe("1–50 of 1,284 apps · Page 1 of 26");
    fireEvent.click(pages.getByRole("button", { name: "Page 26" }));
    await waitFor(() => expect(sent()).toContain("/api/v1/fleet?stage=blocked&page=26"));
    expect((await screen.findByText(/of 1,284 apps/)).textContent).toBe("1,251–1,284 of 1,284 apps · Page 26 of 26");
    expect(document.activeElement).toBe(screen.getByRole("heading", { level: 1 })); // the keyboard goes to the top of the new page
    fireEvent.change(screen.getByRole("combobox", { name: "Rows per page" }), { target: { value: "200" } });
    await waitFor(() => expect(sent()).toContain("/api/v1/fleet?stage=blocked&page=7&limit=200")); // row 1,251 is on page 7 at 200 a page
    await screen.findByText(/^1,201–1,284 of 1,284 apps/);
    fireEvent.click(screen.getByRole("button", { name: "First" }));
    expect(window.location.hash).toBe("#/fleet?stage=blocked&limit=200"); // the first page is the key left out
  });

  // The page already shown asks nothing, so the next poll must not pull the keyboard to the heading.
  it("leaves the keyboard where it is when the page already shown is pressed", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    server(() => ({ ...golden, total: 1284, page: 1 }));
    render(<Fleet org="Meridian Insurance" />);
    const pages = within(await screen.findByRole("navigation", { name: "Fleet pages" }));
    fireEvent.click(pages.getByRole("button", { name: "Page 1" }));
    const search = screen.getByRole("searchbox");
    search.focus();
    await vi.advanceTimersByTimeAsync(2500);
    expect(document.activeElement).toBe(search);
  });

  it("reads an older link's cursor as the first page, with its footer", async () => {
    const sent = server((query) => ({ ...golden, total: 120, page: query.has("cursor") ? null : 1 }));
    window.location.hash = "#/fleet?stage=blocked&cursor=c_after_premium";
    render(<Fleet org="Meridian Insurance" />);
    expect((await screen.findByText(/of 120 apps/)).textContent).toBe("1–50 of 120 apps · Page 1 of 3");
    expect(sent()).toContain("/api/v1/fleet?stage=blocked");
  });

  it("reads the page the server served, never the one the address asked for", async () => {
    server(() => ({ ...golden, total: 120, page: 3 }));
    window.location.hash = "#/fleet?page=40";
    render(<Fleet org="Meridian Insurance" />);
    expect((await screen.findByText(/of 120 apps/)).textContent).toBe("101–120 of 120 apps · Page 3 of 3");
  });



  it("reads an app whose every blocker is approved as approved, never blocked (D1)", async () => {
    const claims = golden.apps.find((app) => app.id === "claims-portal") as FleetRow;
    server(() => ({ ...golden, apps: [{ ...claims, latest: { ...claims.latest, stage: "approved" } }] }));
    render(<Fleet org="Meridian Insurance" />);
    await waitFor(() => expect(names()).toEqual(["claims-portal"]));
    const row = rows()[0]!;
    expect(within(row).getByText("Approved")).toBeDefined();
    expect(within(row).queryByText("Blocked")).toBeNull();
  });

  it("drops an answer to a query the screen has already left", async () => {
    let release: (page: unknown) => void = () => {};
    const blocked = { ...golden, apps: golden.apps.filter((app) => app.id === "claims-portal") };
    vi.stubGlobal("fetch", vi.fn((url: string) => url.includes("q=slow")
      ? new Promise((resolve) => { release = (page) => resolve(answering(page)); })
      : Promise.resolve(answering(url.startsWith("/api/v1/audit") ? audit : blocked))));
    window.location.hash = "#/fleet?q=slow";
    render(<Fleet org="Meridian Insurance" />);
    window.location.hash = "#/fleet?tile=blocked";
    await waitFor(() => expect(names()).toEqual(["claims-portal"]));
    await act(async () => { release(golden); await new Promise((done) => setTimeout(done, 0)); });
    expect(names()).toEqual(["claims-portal"]);
  });

  // The fleet read costs seconds on a large fleet (#87): the table and its activity are read again every ten seconds,
  // never two seconds after each answer, which kept one read always in flight; none while the tab is hidden; and the
  // trail's last day, for what is pinned, once a minute.
  it("reads the fleet and its activity again every ten seconds, and not while the tab is hidden", async () => {
    vi.useFakeTimers();
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    const table = () => sent().filter((url) => url.startsWith("/api/v1/fleet") && !url.includes("needs_you")).length;
    const trail = () => sent().filter((url) => url.startsWith("/api/v1/audit")).length;
    await vi.advanceTimersByTimeAsync(100);
    const [first, firstTrail] = [table(), trail()];
    await vi.advanceTimersByTimeAsync(9_800);
    expect([table(), trail()]).toEqual([first, firstTrail]); // nothing again inside ten seconds
    await vi.advanceTimersByTimeAsync(20_200);
    expect([table(), trail()]).toEqual([first + 3, firstTrail + 3]); // at 10, 20 and 30 seconds: the trail's newest rows, its last day's once a minute
    const hidden = vi.spyOn(document, "visibilityState", "get").mockReturnValue("hidden");
    await vi.advanceTimersByTimeAsync(30_000);
    expect([table(), trail()]).toEqual([first + 3, firstTrail + 3]);
    hidden.mockReturnValue("visible");
    document.dispatchEvent(new Event("visibilitychange"));
    await vi.advanceTimersByTimeAsync(100);
    expect([table(), trail()]).toEqual([first + 4, firstTrail + 5]); // shown again, read at once, the last day's too a minute on
  });

  it("stops asking the server once the screen is gone", async () => {
    vi.useFakeTimers();
    const sent = server(() => golden);
    const { unmount } = render(<Fleet org="Meridian Insurance" />);
    await vi.advanceTimersByTimeAsync(10_000);
    expect(sent()).toHaveLength(6); // the fleet and the trail's two reads at once, the fleet and the trail's newest ten seconds on, and what needs a person once
    unmount();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(sent()).toHaveLength(6);
  });

  for (const [banner, shown, hidden] of [
    ["loopback", S.bannerLoopback, S.bannerExposed],
    ["exposed", S.bannerExposed, S.bannerLoopback],
  ] as const) {
    it(`shows the ${banner} banner when the server says ${banner}`, async () => {
      server(() => ({ ...golden, banner }));
      render(<Fleet org="Meridian Insurance" />);
      expect(await screen.findByText(shown)).toBeDefined();
      expect(screen.queryByText(hidden)).toBeNull();
    });
  }

  // The two D5 lines say the host has no auth. On one that has, neither shows; the rail names the
  // person (ui/Rail.test.tsx).
  it("shows no demo line when the server says none", async () => {
    server(() => ({ ...golden, banner: "none", viewer: { display: "Dana Whitfield", role: "member", github_logins: [], github_link: false } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.queryByText(S.bannerLoopback)).toBeNull();
    expect(screen.queryByText(S.bannerExposed)).toBeNull();
  });

  // The no-auth sentence is a banner above everything, with the way out beside it.
  it("puts the no-auth sentence in a banner above the page, with the way to turn sign-in on", async () => {
    server(() => ({ ...golden, banner: "exposed" }));
    render(<Fleet org="Meridian Insurance" />);
    const banner = (await screen.findByText(S.bannerExposed)).parentElement as HTMLElement;
    expect(banner.className).toContain("text-critical");
    expect(within(banner).getByRole("link", { name: "Turn on sign-in →" }).getAttribute("href")).toBe("#/settings");
  });






  it("adds the Access column only where people sign in", async () => {
    server(() => ({ ...golden, ...member }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.getAllByRole("columnheader").at(-2)?.textContent).toBe("Access");
  });

  // On a phone each app is a two-line card: its name and review, then what runs and who owns it.
  // What runs now, never what the last deploy recorded: the golden's claims-archive prod has
  // stopped, so its cell says so and opens nothing, its card never says it runs, and it is not counted in production.
  it("says a stopped app stopped wherever the fleet shows it, and never counts it as running", async () => {
    server(() => golden);
    const { unmount } = render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("claims-archive");
    expect(cell("claims-archive", 4)).toBe("v3.1Stopped");
    expect(within(rows().find((row) => named(row) === "claims-archive")!.cells[4]!).queryByRole("link")).toBeNull();
    expect(cell("premium-ledger", 4)).toBe("v5.0");
    unmount();
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    render(<Fleet org="Meridian Insurance" />);
    const card = (await screen.findAllByRole("link", { name: "claims-archive" }))[0]!.closest("li") as HTMLElement;
    expect(card.textContent).toContain("Production v3.1: stopped");
  });

  // One Canyon could not check may well run: it keeps its way to open it, says it could not check,
  // and stays in the production count, which only a stopped app leaves.
  it("keeps an app Canyon could not check openable and in production", async () => {
    const unchecked = { ...golden, apps: golden.apps.map((app) => (app.id === "premium-ledger" ? { ...app, envs: { prod: { ...(app as FleetRow).envs.prod, state: "unknown" } } } : app)) } as FleetPage;
    server(() => unchecked);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(cell("premium-ledger", 4)).toBe("v5.0Couldn't check whether it runs");
    expect(within(rows().find((row) => named(row) === "premium-ledger")!.cells[4]!).getByRole("link").getAttribute("href")).toBe("http://premium-ledger.prod.localhost:7777");
  });

  it("draws each app as a card on a phone", async () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    const card = (await screen.findAllByRole("link", { name: "claims-portal" }))[0]!.closest("li") as HTMLElement; // the first: the activity below names it too
    expect(screen.queryByRole("table")).toBeNull();
    expect(card.textContent).toBe("claims-portalBlockedv1.4 · Preview runs v1.3 · Dana Whitfield"); // an admin's card: holding is the app page's, never the card's (the UX review)
  });

  // A phone's card says what the table says (the UX review): the platform's state in the short form, never that
  // production runs what the platform hasn't reported, and a reduced card the reader's level with Ask to join.
  it("says on a phone's card what the table says of the platform, and the reader's level on a reduced one", async () => {
    vi.setSystemTime(new Date("2026-09-02T12:00:00Z"));
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    server(() => memberPage);
    render(<Fleet org="Meridian" />);
    const card = async (name: string) => (await screen.findAllByRole("link", { name }))[0]!.closest("li") as HTMLElement;
    const clock = (at: string) => new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    const archive = (await card("claims-archive")).textContent!;
    expect(archive).toContain(`Production v3.1: being removed, not reported since ${clock("2026-09-02T11:29:12Z")}`);
    expect(archive).not.toContain("Production runs");
    expect(archive).toContain("Tomas Neri · Owner · shared with 2"); // the reader's place, as the table's Access column
    expect((await card("fnol-intake")).textContent).toContain("Marcus Bell · Contributor · shared with 2");
    expect((await card("broker-portal")).textContent).toContain(`Preview: sent ${clock("2026-09-02T10:00:00Z")} · not running yet`);
    expect(within(await card("claims-portal")).getByText(FLEET.readOnly)).toBeDefined();
    expect(within(await card("claims-portal")).getByRole("button", { name: `${FLEET.askToJoin} claims-portal` })).toBeDefined();
    expect(within(await card("quote-engine")).getByText(FLEET.noAccess)).toBeDefined();
  });

  // A click on a row opens its panel; a click on what the row links or acts on does only that.
  it("opens the panel from a click on the row, never from a click on its links", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    const portal = rows().find((row) => named(row) === "claims-portal")!;
    fireEvent.click(within(portal).getByRole("link", { name: "claims-portal" }));
    expect(screen.queryByRole("complementary", { name: "Details for claims-portal" })).toBeNull();
    fireEvent.click(portal.cells[1]!);
    expect(screen.getByRole("complementary", { name: "Details for claims-portal" })).toBeDefined();
  });

  // Below contributor a row is name, owners and summary, whatever else arrives with it; a phone card
  // says the same and no more.
  it("draws a reduced row as a card with its summary and owner and nothing of the team's", async () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    server(() => ({ ...golden, ...member, apps: [reduced] }));
    render(<Fleet org="Meridian Insurance" />);
    const card = (await screen.findByRole("link", { name: "pricing-lab" })).closest("li") as HTMLElement;
    expect(card.textContent).toBe(`pricing-labTries new premium curves · Nora Lind${FLEET.noAccess}${FLEET.askToJoin}`); // the reader's level, as the table's Access column
  });

  it("offers an admin Assign on an ownerless row off their teams, and a member only Claim", async () => {
    const orphan = { ...reduced, owners: [], claimable: true };
    for (const [role, assign] of [["member", false], ["admin", true]] as const) {
      server(() => ({ ...golden, banner: "none", viewer: { display: "Tomas Berg", role, github_logins: [], github_link: false }, apps: [orphan], tiles: { secrets_exposed: 0, blocked: 0, approved: 0, never_reviewed: 0, unowned: 0, failed: 0, refused: 0 } }));
      const { unmount } = render(<Fleet org="Meridian Insurance" />);
      await screen.findByText("pricing-lab");
      expect(screen.getByRole("button", { name: "Claim pricing-lab" })).toBeDefined();
      expect(screen.queryByRole("button", { name: "Assign an owner to pricing-lab" }) !== null).toBe(assign);
      unmount();
    }
  });

  it("heads the fleet with the org's name", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    expect((await screen.findByRole("heading", { level: 1 })).textContent).toBe("Meridian Insurance");
  });

  // The golden's first blocked row is the spec's claims-portal, the story's hero: the strip names
  // whatever the server put first, and never sorts for itself.
  for (const banner of ["loopback", "exposed"] as const) {
    it(`points a visitor on a ${banner} host at the server's first blocked app`, async () => {
      server(() => ({ ...golden, banner }));
      render(<Fleet org="Meridian Insurance" />);
      const link = await screen.findByRole("link", { name: "Open claims-portal →" });
      expect(link.getAttribute("href")).toBe("#/apps/claims-portal");
      expect(link.parentElement?.textContent).toContain("Start here: claims-portal v1.4 is blocked.");
    });
  }

  // The tour (#2696): commission-calc, blocked on a seeded row nothing was ever deployed from, comes first by
  // risk, but the visitor can't pull it or fix it. The strip names the first blocked app that is deployed.
  it("points past a blocked app nothing was deployed from, at the first blocked app that is deployed", async () => {
    const portal = golden.apps.find((app) => app.id === "claims-portal")!;
    const calc = golden.apps.find((app) => app.id === "commission-calc")!;
    server(() => ({ ...golden, banner: "loopback", apps: [calc, portal, ...golden.apps.filter((app) => app !== calc && app !== portal)] }));
    render(<Fleet org="Meridian Insurance" />);
    const link = await screen.findByRole("link", { name: "Open claims-portal →" });
    expect(link.parentElement?.textContent).toContain("Start here: claims-portal v1.4 is blocked.");
  });

  // Once the tour's fix passed, the only blocked app is one nothing was deployed from: no strip, and
  // Needs attention still lists it.
  it("shows no strip when no blocked app is deployed", async () => {
    const passed = golden.apps.map((app) => (app.id === "claims-portal" && "latest" in app ? { ...app, latest: { ...app.latest, stage: "passed" as const } } : app));
    server(() => ({ ...golden, banner: "loopback", apps: passed }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.queryByText(/Start here/)).toBeNull();
  });

  it("shows no demo strip to someone signed in to their company's host", async () => {
    server(() => ({ ...golden, banner: "none", viewer: { display: "Dana Whitfield", role: "member", github_logins: [], github_link: false } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.queryByText(/Start here/)).toBeNull();
  });

  it("remembers a dismissed strip", async () => {
    storage();
    server(() => golden);
    const { unmount } = render(<Fleet org="Meridian Insurance" />);
    fireEvent.click(await screen.findByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/Start here/)).toBeNull();
    unmount();
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(screen.queryByText(/Start here/)).toBeNull();
  });

  it("still draws the strip and dismisses it when storage refuses", async () => {
    storage(true);
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    fireEvent.click(await screen.findByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText(/Start here/)).toBeNull();
  });

  it("never puts the machine's words on screen", async () => {
    server(() => golden);
    const { container } = render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i);
  });

  // An org with no apps reads the two ways to add one, never a bare "No apps yet" (the UX review): the CLI, with the
  // panel that connects an agent, and GitHub; an admin is pointed at connecting GitHub and at the rules, a member is not.
  it("says how to add the first app when there is none, the admin's links where they are an admin's", async () => {
    server(() => ({ ...golden, apps: [], total: 0 }));
    const { unmount } = render(<Fleet org="Meridian Insurance" />);
    const empty = within(await screen.findByRole("region", { name: FLEET.emptyTitle }));
    expect(empty.getByRole("link", { name: FLEET.emptyCliLink }).getAttribute("href")).toBe("#/settings/agents");
    expect(document.querySelector("dl")).toBeNull(); // no tiles and no Needs panel: No apps yet comes first (the UX review)
    expect(screen.queryByText(/^Nothing needs you/)).toBeNull();
    expect(screen.queryByRole("searchbox")).toBeNull(); // nothing to search, filter or sort yet
    expect(screen.queryByRole("button", { name: /Filter/ })).toBeNull();
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(empty.getByRole("link", { name: FLEET.emptyGitHubLink }).getAttribute("href")).toBe("#/settings/github");
    expect(empty.getByRole("link", { name: FLEET.emptyRulesLink }).getAttribute("href")).toBe("#/guardrails");
    unmount();
    server(() => ({ ...golden, ...member, apps: [], total: 0 }));
    render(<Fleet org="Meridian Insurance" />);
    const theirs = within(await screen.findByRole("region", { name: FLEET.emptyTitle }));
    expect(theirs.getByText(FLEET.emptyGitHubMember)).toBeDefined();
    expect(theirs.queryByRole("link", { name: FLEET.emptyGitHubLink })).toBeNull();
    expect(theirs.queryByRole("link", { name: FLEET.emptyRulesLink })).toBeNull();
  });

  // The org's own needs, which no app carries, still show over an org with no apps.
  it("keeps the org's own needs above No apps yet", async () => {
    server(() => ({ ...golden, apps: [], total: 0, org_needs: [{ kind: "licence", env: null, rank: 2, since: "2026-11-01T00:00:00Z", you: true, ask: [], licence: "lapsed" }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: FLEET.emptyTitle });
    expect(screen.getByRole("heading", { name: /^Needs (attention|you)/ })).toBeDefined();
    expect(screen.queryByText(/^Nothing needs you/)).toBeNull();
  });

  it("says no app matches a filter that matches none, without the first-app guide", async () => {
    window.location.hash = "#/fleet?stage=blocked";
    server(() => ({ ...golden, apps: [], total: 0 }));
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByText("No apps match")).toBeDefined();
    expect(screen.queryByRole("region", { name: FLEET.emptyTitle })).toBeNull();
  });

  it("says why when the read fails, and shows no fleet it cannot vouch for", async () => {
    server(() => null, false, 503);
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByText("GET /api/v1/fleet answered 503")).toBeDefined();
    expect(screen.queryByRole("table")).toBeNull();
  });

  // Who reads the pods' check-in on the fleet (design/PERMISSIONS.md; #2455): claims-archive's owner reads it on
  // the team row, its pods last seen at 11:29, half an hour before noon; a read-only member reads it where the
  // row carries seen_at; a member with no access to quote-engine reads its name, owner and summary and no environment.
  it("says where the org's pods stopped checking in to whoever reads the environment, and to nobody else", async () => {
    vi.setSystemTime(new Date("2026-09-02T12:00:00Z"));
    const seen = "2026-09-02T11:29:12Z";
    const clock = new Date(seen).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    const line = `not reported since ${clock}`; // the table's short form; the pane and the app page say it whole
    const page = { ...memberPage, apps: memberPage.apps.map((app) => (app.id === "claims-portal" ? { ...app, envs: { preview: { url: "http://claims-portal.preview.localhost:7777", seen_at: seen } } } : app)) } as FleetPage;
    server(() => page);
    render(<Fleet org="Meridian" />);
    await screen.findByText("claims-archive");
    const row = (name: string) => rows().find((one) => named(one) === name)!;
    expect(within(row("claims-archive")).getByText(line).closest("span")?.className).toContain("text-amber");
    expect(row("claims-archive").cells[4]!.textContent).toContain(`v3.1 · ${line}`);
    expect(row("claims-archive").cells[4]!.querySelector(".rounded-full")?.className).toContain("bg-amber"); // never the running dot on what the platform hasn't reported
    expect(row("claims-portal").cells[3]!.textContent).toBe(`Open preview · ${line}`);
    expect(row("quote-engine").textContent).not.toContain("checked in");
  });

  // design/PATCHING.md §8: no new column. A row read whole says under the version when Canyon's patch reached
  // the environment, or that a fix waits for the reader, linked to its approval; a reduced row carries no patch.
  it("says a patch under the version on a team row, and nothing on a reduced row", async () => {
    const patch = (state: string, extra = {}) => ({ state, since: "2026-10-10T09:00:00Z", identity: "sha256:" + "5f".repeat(32), ...extra });
    const page = {
      ...golden,
      apps: golden.apps.map((app) => {
        if (app.id === "claims-portal") return { ...app, envs: { preview: { ...(app as FleetRow).envs.preview, patch: patch("patched_preview") } } };
        if (app.id === "claims-archive") return { ...app, envs: { prod: { ...(app as FleetRow).envs.prod, patch: patch("patched") } } };
        if (app.id === "premium-ledger") return { ...app, envs: { prod: { ...(app as FleetRow).envs.prod, patch: patch("asked", { approval: "K7QF-2M9X" }) } } };
        if (app.id === "endorsement-hub") return { id: app.id, name: app.name, owners: app.owners, owner_groups: [], summary: app.summary, claimable: false, access: { level: "viewer" },
          envs: { preview: { url: "http://endorsement-hub.preview.localhost:7777", state: "running", patch: patch("patched_preview") } } };
        return app;
      }),
    } as FleetPage;
    server(() => page);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("commission-calc");
    expect(cell("claims-portal", 3)).toBe("v1.3patched by Canyon · 10 Oct");
    expect(cell("claims-archive", 4)).toContain("patched by Canyon · 10 Oct");
    const waiting = within(rows().find((row) => named(row) === "premium-ledger")!).getByRole("link", { name: "fix ready for production" });
    expect(waiting.getAttribute("href")).toBe("#/approve?code=K7QF-2M9X");
    expect(waiting.className).toContain("text-amber");
    expect(rows().find((row) => named(row) === "endorsement-hub")!.textContent).not.toContain("patched by Canyon");
    expect(cell("commission-calc", 3)).toBe("—"); // nothing deployed: no line
  });
});

// What needs a person (design/EXPERIENCE.md D15; contracts/routes.md, *What needs a person*; #2724): one line a need of the
// rows needs_you selects, in the server's order, each its fix where the reader may act and who acts where they can't.
describe("what needs a person, on the fleet", () => {
  const none = { secrets_exposed: 0, blocked: 0, approved: 0, never_reviewed: 0, unowned: 0, failed: 0, refused: 0, needs_you: 0 };
  const box = () => within(screen.getByRole("region", { name: /^Needs (attention|you)$/ }));
  const items = () => box().queryAllByRole("listitem").map((one) => one.textContent);
  const app = (id: string, needs: Need[], extra: Partial<FleetRow> = {}) => ({ ...(golden.apps[0] as FleetRow), id, name: id, last_run: `gr_${id}`, needs, ...extra });
  const refused = (over: Partial<Need> = {}): Need => ({ kind: "refused", env: "prod", rank: 3, since: "2026-11-08T09:00:00Z", you: true, ask: [], destination: "api.stripe.com", refusals: 14,
    last_refused_at: new Date(Date.now() - 2 * 60_000).toISOString(), other_calls: 212, held: false, ...over });
  // A server that answers the reads with page, records every call with its method, and answers the rest 204.
  // pending: a write's Canyon-Egress-Pending, by its method and URL (#2789)
  // held: a write whose answer waits for the test, by its method and URL (#2789)
  function host(page: (query: URLSearchParams) => unknown, pending: Record<string, string> = {}, held: Record<string, Promise<void>> = {}) {
    const calls: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      calls.push(`${method} ${url}`);
      await held[`${method} ${url}`];
      const left = pending[`${method} ${url}`];
      if (method !== "GET") return { ok: true, status: 204, json: async () => null, headers: new Headers(left === undefined ? {} : { "Canyon-Egress-Pending": left }) } as unknown as Response;
      const out = url.startsWith("/api/v1/audit") ? audit : page(new URL(url, "http://canyon").searchParams);
      return out instanceof Promise || (out !== null && typeof out === "object" && "ok" in out) ? out as Response : answering(out);
    }));
    return calls;
  }

  // A version its secret blocks is the secret's line alone: the server lists no blocked need beside it.
  it("lists each need of the rows that need someone, most urgent first", async () => {
    const calls = host(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    await waitFor(() => expect(items()).toEqual([
      "claims-archive is down in production. canyon logs --env prod says why. Fix the cause, deploy to preview, then promote.Open the app",
      "claims-portal has a secret in its code. Rotate the key, then upload a fix.Open review",
      "commission-calc has a secret in its code. Rotate the key, then upload a fix.Open review",
      "quote-engine has never been reviewed. Deploy or check it so Canyon reviews it.Open the app",
      "quote-engine has no owner.Claim it.Claim",
    ]));
    expect(calls).toContain("GET /api/v1/fleet?needs_you=true&limit=50");
    expect(box().getByRole("link", { name: "Open review: claims-portal" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_8f2a");
  });

  // A restarting environment's last log lines are on its page (#2744), so its need points there; a stopped one's, and
  // anything else down, point at canyon logs, which the page doesn't show for them (#2774).
  it("points a restarting environment at its page's log lines, and a stopped one at canyon logs", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 4 }, apps: [
      app("alpha", [{ kind: "down", env: "preview", rank: 7, since: null, you: true, ask: [], state: "restarting" }]),
      app("beta", [{ kind: "down", env: "prod", rank: 2, since: null, you: true, ask: [], state: "restarting" }]),
      app("gamma", [{ kind: "down", env: "preview", rank: 7, since: null, you: true, ask: [], state: "stopped" }]),
      app("delta", [{ kind: "down", env: "prod", rank: 2, since: null, you: true, ask: [], state: "stopped" }]),
    ] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    const line = (title: string) => box().getByText(title).closest("li")!.textContent;
    expect((await box().findByText("alpha keeps restarting in preview.")).closest("li")!.textContent).toContain("Its last log lines, on its page, say why. Fix the cause, then deploy again.");
    expect(line("beta keeps restarting in production.")).toContain("Its last log lines, on its page, say why. Fix the cause, deploy to preview, then promote.");
    expect(line("gamma is down in preview.")).toContain("canyon logs --env preview says why. Fix the cause, then deploy again.");
    expect(line("delta is down in production.")).toContain("canyon logs --env prod says why. Fix the cause, deploy to preview, then promote.");
  });

  // Each kind's fix where it is: preview's own, a port named with its host, the network a recreation leaves, and the row's
  // short line for each.
  it("words each kind as where it stands says it", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 5 }, apps: [
      app("alpha", [{ kind: "down", env: "preview", rank: 7, since: null, you: true, ask: [], state: "stopped" }]),
      app("beta", [{ kind: "refused_code", env: "prod", rank: 3, since: null, you: true, ask: [], destination: "db.internal", port: 5432, why: "port", calls: 3 }]),
      app("gamma", [{ kind: "recreate", env: null, rank: 11, since: null, you: true, ask: [], network: "unknown", via: "canyon" }]),
      app("delta", [{ kind: "unchecked", env: null, rank: 6, since: null, you: true, ask: [], version: "v2", count: 2, checks: [] }]),
      app("epsilon", [{ kind: "recreate", env: null, rank: 11, since: null, you: true, ask: [], network: "internal", via: "off" }]),
    ] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    // what is true today: the page shows no log lines for a stopped app, so the need points at canyon logs (#2774)
    expect((await box().findByText("alpha is down in preview.")).closest("li")!.textContent).toContain("canyon logs --env preview says why. Fix the cause, then deploy again.");
    expect(box().getByText("beta can't reach db.internal:5432 in production, and nobody can allow it.")).toBeDefined();
    expect(box().getByText(/what network gamma is on/).closest("li")!.textContent).toContain(S.needRecreateUnknownStopped);
    expect(box().getByRole("link", { name: `${S.openSettingsOutbound}: gamma` }).getAttribute("href")).toBe("#/settings/outbound");
    for (const line of ["Network unknown", "Still goes through Canyon", "v2 can't deploy: 2 checks couldn't finish"]) expect(screen.getByText(line)).toBeDefined();
  });

  it("orders every line by its rank across apps, then the oldest first", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 4 }, apps: [
      app("alpha", [refused(), { kind: "unowned", env: null, rank: 10, since: null, you: true, ask: [] }]),
      app("beta", [{ kind: "held", env: "prod", rank: 1, since: "2026-11-08T09:40:00Z", you: false, ask: [] }], { hold: { by: { kind: "person", subject: "r", display: "Rhea Okafor" }, at: "2026-11-08T09:40:00Z", reason: "INC-1", version: "v1" } }),
      app("gamma", [refused({ destination: "hooks.slack.com", since: "2026-11-08T08:00:00Z" })]), // the same rank as alpha's, an hour older
    ] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    await waitFor(() => expect(items().map((line) => line?.split(".")[0])).toEqual(["beta's production is held", "gamma can't reach hooks", "alpha can't reach api", "alpha has no owner"]));
  });

  // The org's own needs (the fleet read's org_needs; #2725): a failing chat channel ranks among the apps' needs, with its
  // link to Settings, and a kind this dashboard doesn't know still says something needs a person.
  it("lists the org's own needs among the apps', by rank", async () => {
    const failing: Need = { kind: "notify_failing", env: null, rank: 11, since: new Date(Date.now() - 2 * 3_600_000).toISOString(), you: true, ask: [], channel: "Risk team", channel_id: "risk-team" };
    host(() => ({ ...golden, viewer: { ...golden.viewer, role: "admin" }, tiles: { ...none, needs_you: 3 }, apps: [app("alpha", [refused()])],
      org_needs: [failing, { kind: "stuck", env: null, rank: 11, since: null, you: true, ask: [] }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: "Needs attention" });
    await waitFor(() => expect(items()).toEqual([
      expect.stringMatching(/^alpha can't reach api\.stripe\.com/),
      `Canyon can't post to Risk team. Its webhook stopped working 2 hours ago, so the channel gets nothing from Canyon until an admin sets a new webhook URL.${S.openSettingsIntegrations}`,
      S.needUnknown,
    ]));
    expect(box().getByRole("link", { name: `${S.openSettingsIntegrations}: Risk team` }).getAttribute("href")).toBe("#/settings/integrations");
  });

  // The licence in grace, lapsed or invalid is an admin's need (the fleet read's org_needs, licence; #2879): what it means
  // for Enterprise features, and the link to Settings › Licence.
  it("says a lapsed licence, one in grace, and an invalid one, each with Settings › Licence", async () => {
    const licence = (state: "grace" | "lapsed" | "invalid", extra: Partial<Need> = {}): Need => ({ kind: "licence", env: null, rank: 11, since: state === "invalid" ? null : "2027-11-14T00:00:00Z", you: true, ask: [], licence: state, ...extra });
    host(() => ({ ...golden, viewer: { ...golden.viewer, role: "admin" }, tiles: { ...none, needs_you: 3 }, apps: [],
      org_needs: [licence("grace", { grace_until: "2027-12-14T00:00:00Z" }), licence("lapsed"), licence("invalid")] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: "Needs attention" });
    await waitFor(() => expect(items()).toEqual([
      `Canyon's Enterprise licence ended on 14 Nov 2027. Its features stay unlocked until 14 Dec 2027.${S.licenceNeedOpen}`,
      `${S.licenceNeedLapsed}${S.licenceNeedOpen}`,
      `${S.licenceNeedInvalid}${S.licenceNeedOpen}`,
    ]));
    expect(box().getAllByRole("link", { name: S.licenceNeedOpen }).map((link) => link.getAttribute("href"))).toEqual(["#/settings/licence", "#/settings/licence", "#/settings/licence"]);
  });

  // Patching the server is set to do that no licence unlocks is an admin's need too (patching_unlicensed; #2881).
  it("says Canyon patches nothing when the server is set to patch and no licence unlocks it", async () => {
    host(() => ({ ...golden, viewer: { ...golden.viewer, role: "admin" }, tiles: { ...none, needs_you: 1 }, apps: [],
      org_needs: [{ kind: "patching_unlicensed", env: null, rank: 11, since: null, you: true, ask: [] }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: "Needs attention" });
    await waitFor(() => expect(items()).toEqual([`${S.licenceNeedPatching}${S.licenceNeedOpen}`]));
    expect(box().getByRole("link", { name: S.licenceNeedOpen }).getAttribute("href")).toBe("#/settings/licence");
  });

  it("says the first need under a row's name, and an environment's refused calls beside what it runs", async () => {
    host(() => ({ ...golden, apps: [app("ux-outbound", [refused(), refused({ destination: "hooks.slack.com" })], { envs: (golden.apps.find((one) => one.id === "premium-ledger") as FleetRow).envs })] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect(await screen.findByText("Can't reach api.stripe.com in production · and 1 more")).toBeDefined();
    expect(cell("ux-outbound", 4)).toContain("2 refused");
  });

  // Red is for what the people using an app hit, or a version stuck: no owner and a refusal in preview are not.
  it("colours a row's first need as its line is coloured", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 3 }, apps: [
      app("alpha", [{ kind: "unowned", env: null, rank: 10, since: null, you: true, ask: [] }]),
      app("beta", [refused({ env: "preview", rank: 7 })]),
      app("gamma", [refused()]),
    ] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect((await screen.findByText("No owner")).className).not.toContain("text-critical");
    expect(screen.getByText("Can't reach api.stripe.com in preview").className).toContain("text-amber");
    expect(screen.getByText("Can't reach api.stripe.com in production").className).toContain("text-critical");
  });

  it("says one line when nothing needs you, and nothing of a zero", async () => {
    host(() => ({ ...golden, tiles: none, stats: { ...golden.stats, apps: 24, passed: 24 } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect((await box().findByText(/^Nothing needs you/)).textContent).toBe("Nothing needs you. All 24 apps passed their latest review.");
    expect(box().queryAllByRole("listitem")).toEqual([]);
  });

  // The fleet read couldn't tell whether some deployments run: a down need may hide among them, so never all clear.
  it("never says nothing needs you while Canyon couldn't check whether deployments run", async () => {
    host(() => ({ ...golden, tiles: none, unchecked: 3 }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect(await box().findByText(S.needsUnchecked.replace("<n>", "3"))).toBeDefined();
    expect(box().queryByText(/Nothing needs you/)).toBeNull();
  });

  // A refused call's Allow and Dismiss act in place, from the routes and with the permission the Outbound card uses, and
  // the line then says what was done, with what undoes it.
  it("allows a refused call from the list, says so, and undoes it", async () => {
    const calls = host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused()])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    const line = await box().findByText("ux-outbound can't reach api.stripe.com in production.");
    expect(line.closest("li")?.textContent).toContain("Nobody has allowed it for production yet, so Canyon refused 14 calls, the last 2 minutes ago. Preview tried to reach it 212 times in the last 30 days.");
    expect(line.closest("li")?.textContent).toContain(S.needAllowWho);
    fireEvent.click(box().getByRole("button", { name: "Allow api.stripe.com in production for ux-outbound" }));
    expect(await box().findByText("Allowed api.stripe.com in production for ux-outbound. Its next call goes through within seconds.")).toBeDefined();
    expect(calls).toContain("PUT /api/v1/projects/ux-outbound/egress/prod/destinations/api.stripe.com");
    fireEvent.click(box().getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(calls).toContain("DELETE /api/v1/projects/ux-outbound/egress/prod/destinations/api.stripe.com"));
  });

  // #2789: an Allow, or its Undo, a gateway replica hadn't taken when the server stopped waiting is saved, and says so in
  // place of what was done, naming the replica; the Undo's stays rather than clearing the line.
  it("says an Allow and its Undo a gateway replica hasn't taken yet, naming it", async () => {
    const at = "/api/v1/projects/ux-outbound/egress/prod/destinations/api.stripe.com";
    const replica = "canyon-egress-7d9f6c5b8-k2x4q";
    const calls = host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused()])] }), { [`PUT ${at}`]: replica, [`DELETE ${at}`]: replica });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Allow api.stripe.com in production for ux-outbound" }));
    const line = S.gatewayPending.replace("<replica>", replica);
    expect(await box().findByText(line)).toBeDefined();
    expect(box().queryByText("Allowed api.stripe.com in production for ux-outbound. Its next call goes through within seconds.")).toBeNull();
    fireEvent.click(box().getByRole("button", { name: "Undo" }));
    await waitFor(() => expect(calls).toContain(`DELETE ${at}`));
    await act(() => new Promise((resolve) => setTimeout(resolve, 0)));
    expect(box().getAllByText(line).length).toBe(1);
  });

  // #2789: an Allow, and its Undo, wait for the gateway, up to 30 s: while each answer is out the line says Saving…
  // and offers nothing to click again.
  it("says Saving… while an Allow's answer, and its Undo's, is out", async () => {
    const at = "/api/v1/projects/ux-outbound/egress/prod/destinations/api.stripe.com";
    let allowed: () => void = () => {};
    let undone: () => void = () => {};
    host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused()])] }), {},
      { [`PUT ${at}`]: new Promise<void>((resolve) => { allowed = resolve; }), [`DELETE ${at}`]: new Promise<void>((resolve) => { undone = resolve; }) });
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Allow api.stripe.com in production for ux-outbound" }));
    expect(await box().findByText("Saving…")).toBeDefined();
    expect(box().queryByRole("button", { name: "Allow api.stripe.com in production for ux-outbound" })).toBeNull();
    await act(async () => { allowed(); });
    fireEvent.click(await box().findByRole("button", { name: "Undo" }));
    expect(await box().findByText("Saving…")).toBeDefined();
    expect((box().getByRole("button", { name: "Undo" }) as HTMLButtonElement).disabled).toBe(true);
    await act(async () => { undone(); });
  });

  it("dismisses a refused call from the list, and offers to allow it instead", async () => {
    const calls = host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused()])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Dismiss api.stripe.com for ux-outbound" }));
    expect(await box().findByText("Dismissed api.stripe.com in production for ux-outbound. Canyon keeps refusing it there and won't ask about it again.")).toBeDefined();
    expect(calls).toContain("POST /api/v1/projects/ux-outbound/egress/prod/requests/api.stripe.com/dismiss");
    fireEvent.click(box().getByRole("button", { name: "Allow it" }));
    await waitFor(() => expect(calls).toContain("PUT /api/v1/projects/ux-outbound/egress/prod/destinations/api.stripe.com"));
  });

  // While production is held an Allow there is refused (409 held): the button stays, disabled, its reason in view.
  it("keeps Allow on a held production disabled with its reason, and Dismiss working", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused({ held: true })])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect(await box().findByText(S.needAllowHeld)).toBeDefined();
    expect((box().getByRole("button", { name: "Allow" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(box().getByRole("button", { name: "Dismiss api.stripe.com for ux-outbound" }));
    expect(await box().findByText(/^Dismissed api\.stripe\.com in production/)).toBeDefined();
    expect(box().queryByRole("button", { name: S.needAllowInstead })).toBeNull(); // an Allow there would be refused
  });

  // A line acted on is gone at once: its done line stands in its place until the list is read again.
  it("drops a line acted on while the list is read again", async () => {
    let reads = 0;
    host((query) => (query.get("needs_you") === "true" && ++reads > 1 ? new Promise(() => {}) : { ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [refused()])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Allow api.stripe.com in production for ux-outbound" }));
    expect(await box().findByText(/^Allowed api\.stripe\.com/)).toBeDefined();
    expect(box().queryByText("ux-outbound can't reach api.stripe.com in production.")).toBeNull();
  });

  // The count stands while the read of its lines fails, and the line says why, above where the rest are.
  it("says why it couldn't read the lines of what needs you", async () => {
    host((query) => (query.get("needs_you") === "true"
      ? { ok: false, status: 503, json: async () => ({ error: "unavailable", message: "the database didn't answer" }) }
      : { ...golden, tiles: { ...none, needs_you: 3 } }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect(await box().findByText("Canyon couldn't read what needs you: the database didn't answer.")).toBeDefined();
    expect(items()).toEqual(["Canyon couldn't read what needs you: the database didn't answer.Show 3 more"]);
  });

  // Only whoever may act gets the buttons: a need the server says the reader can't resolve shows who can, never a button.
  it("offers no Allow to a reader the need says can't, and shows who lifts a hold and who approves", async () => {
    const hold = { by: { kind: "person" as const, subject: "rhea", display: "Rhea Okafor" }, at: "2026-11-08T09:40:00Z", reason: "Exports claimant SSNs", version: "v1.4" };
    host(() => ({ ...golden, tiles: { ...none, needs_you: 4 }, apps: [app("ux-outbound", [
      { kind: "held", env: "prod", rank: 1, since: hold.at, you: false, ask: [{ display: "Elena Vasquez", as: "admin" }] }, refused({ you: false }),
      { kind: "blocked", env: null, rank: 6, since: hold.at, you: false, ask: [{ display: "Elena Vasquez", as: "admin" }], version: "v1.4", count: 1 },
      { kind: "unchecked", env: null, rank: 6, since: hold.at, you: false, ask: [{ display: "Elena Vasquez", as: "admin" }], version: "v1.5", count: 1, checks: [] },
    ], { hold })] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    const held = (await box().findByText("ux-outbound's production is held.")).closest("li")!;
    expect(held.textContent).toContain(`${S.needHeldWho} Admins: Elena Vasquez.`);
    // A blocked version's fix says who approves; a check that couldn't finish names them to whoever can't.
    expect((await box().findByText("ux-outbound v1.4 is blocked by 1 finding.")).closest("li")!.textContent).toBe(`ux-outbound v1.4 is blocked by 1 finding. ${S.needBlockedFixOne}Open review`);
    expect(box().getByText(/^ux-outbound v1\.5 is blocked/).closest("li")!.textContent).toContain(`${S.approvers} Admins: Elena Vasquez.`);
    expect(within(held).queryByRole("button", { name: /^Lift the hold/ })).toBeNull();
    expect(box().queryByRole("button", { name: /^Allow/ })).toBeNull();
  });

  it("lifts a hold from the list for whoever the need says lifts it", async () => {
    const hold = { by: { kind: "person" as const, subject: "rhea", display: "Rhea Okafor" }, at: "2026-11-08T09:40:00Z", reason: "Exports claimant SSNs", version: "v1.4" };
    host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [{ kind: "held", env: "prod", rank: 1, since: hold.at, you: true, ask: [] }], { hold })] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Lift the hold on ux-outbound" }));
    expect(screen.getByRole("dialog", { name: S.liftAsk.replace("<app>", "ux-outbound") })).toBeDefined();
  });

  // A request for access is its owners' and admins' to answer, from the list: Add, with Undo, or Dismiss.
  it("adds who asked for access as read-only, and dismisses another", async () => {
    const access = (email: string, display: string): Need => ({ kind: "access", env: null, rank: 9, since: "2026-11-08T09:00:00Z", you: true, ask: [], asked_by: { kind: "person", subject: `sub-${display}`, display }, email });
    const calls = host(() => ({ ...golden, tiles: { ...none, needs_you: 2 }, apps: [app("premium-ledger", [access("tomas@meridian.example", "Tomas Neri"), access("gus@meridian.example", "Gus")])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await box().findByRole("button", { name: "Add as read-only: Tomas Neri on premium-ledger" }));
    expect(await box().findByText("Added Tomas Neri as read-only. They can open it within about a minute.")).toBeDefined();
    expect(calls).toContain("PUT /api/v1/projects/premium-ledger/access/email/tomas%40meridian.example");
    fireEvent.click(await box().findByRole("button", { name: "Dismiss Gus's request for premium-ledger" })); // once the list is read again
    expect(await box().findByText("Dismissed Gus's request. They can ask again.")).toBeDefined();
    expect(calls).toContain("DELETE /api/v1/projects/premium-ledger/access-requests/gus%40meridian.example");
  });

  // A read-only member's request is to join the team (#2774): said so, never that they can't open the app, and its Add makes
  // them a contributor, with no Undo, which would take away the read-only access they had. A request from before the level
  // was recorded claims neither.
  it("words a read-only member's request as joining the team, and adds them to it", async () => {
    const access = (email: string, display: string, level: Need["level"]): Need => ({ kind: "access", env: null, rank: 9, since: "2026-11-08T09:00:00Z", you: true, ask: [], asked_by: { kind: "person", subject: `sub-${display}`, display }, email, level });
    const calls = host(() => ({ ...golden, tiles: { ...none, needs_you: 2 }, apps: [app("premium-ledger", [access("frank@meridian.example", "Frank", "viewer"), access("gus@meridian.example", "Gus", null)])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect((await box().findByText("Frank asked to join premium-ledger's team.")).closest("li")!.textContent).toContain("When they asked, they could open it read-only. Asked ");
    expect(box().getByText("Gus asked for access to premium-ledger.").closest("li")!.textContent).not.toContain("can't open it");
    fireEvent.click(box().getByRole("button", { name: "Add to its team: Frank on premium-ledger" }));
    expect(await box().findByText("Added Frank to its team. They can change it now.")).toBeDefined();
    expect(box().queryByRole("button", { name: "Undo" })).toBeNull();
    expect(calls).toContain("PUT /api/v1/projects/premium-ledger/access/email/frank%40meridian.example");
    expect(vi.mocked(fetch).mock.calls.find(([, init]) => init?.method === "PUT")?.[1]?.body).toBe('{"level":"contributor","roles":[]}');
  });

  it("offers a request for access no Add or Dismiss where the need says the reader can't answer it", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("premium-ledger", [{ kind: "access", env: null, rank: 9, since: "2026-11-08T09:00:00Z", you: false, ask: [],
      asked_by: { kind: "person", subject: "sub-gus", display: "Gus" }, email: "gus@meridian.example" }])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect(await box().findByText("Gus asked for access to premium-ledger.")).toBeDefined();
    expect(box().queryByRole("button", { name: /^Add as read-only/ })).toBeNull();
    expect(box().queryByRole("button", { name: /^Dismiss/ })).toBeNull();
  });

  it("offers the one ownerless app its Assign and Claim, under a member's own heading", async () => {
    const orphan = { ...(golden.apps.find((one) => one.id === "quote-engine") as FleetRow), name: "quote-engine" };
    for (const [role, title, assign, line] of [["member", "Needs you", false, S.needUnownedClaim], ["admin", "Needs attention", true, S.needUnownedClaimAdmin]] as const) {
      host(() => ({ ...golden, banner: "none", viewer: { display: "Tomas Berg", role, github_logins: [], github_link: false }, tiles: { ...none, needs_you: 1 },
        apps: [{ ...orphan, needs: [{ kind: "unowned", env: null, rank: 10, since: null, you: true, ask: [] }] }] }));
      const { unmount } = render(<Fleet org="Meridian Insurance" />);
      const it = within(await screen.findByRole("region", { name: title }));
      expect((await it.findByText("quote-engine has no owner.")).closest("li")!.textContent).toContain(line);
      expect(it.getByRole("button", { name: "Claim quote-engine" })).toBeDefined();
      expect(it.queryByRole("button", { name: "Assign an owner to quote-engine" }) !== null).toBe(assign);
      unmount();
    }
  });

  it("tells an admin who can't claim it to assign one", async () => {
    const held = { ...(golden.apps.find((one) => one.id === "quote-engine") as FleetRow), name: "quote-engine", claimable: false };
    host(() => ({ ...golden, banner: "none", viewer: { display: "Elena Vasquez", role: "admin", github_logins: [], github_link: false }, tiles: { ...none, needs_you: 1 },
      apps: [{ ...held, needs: [{ kind: "unowned", env: null, rank: 10, since: null, you: false, ask: [] }] }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect((await box().findByText("quote-engine has no owner.")).closest("li")!.textContent).toContain(S.needUnownedAssign);
  });

  it("tells a member to ask an admin, with no Claim, where the server would refuse their claim", async () => {
    const held = { ...(golden.apps.find((one) => one.id === "quote-engine") as FleetRow), name: "quote-engine", claimable: false };
    host(() => ({ ...golden, banner: "none", viewer: { display: "Tomas Berg", role: "member", github_logins: [], github_link: false }, tiles: { ...none, needs_you: 1 },
      apps: [{ ...held, needs: [{ kind: "unowned", env: null, rank: 10, since: null, you: false, ask: [{ display: "Elena Vasquez", as: "admin" }] }] }] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    const line = (await box().findByText("quote-engine has no owner.")).closest("li")!;
    expect(line.textContent).toContain("Ask an admin to assign an owner. Admins: Elena Vasquez.");
    expect(within(line).queryByRole("button", { name: "Claim quote-engine" })).toBeNull();
  });

  // A kind this dashboard doesn't know is something that needs a person, with the app's page: never left out (fail closed).
  it("says a kind it doesn't know needs a person, never nothing", async () => {
    host(() => ({ ...golden, tiles: { ...none, needs_you: 1 }, apps: [app("ux-outbound", [{ kind: "quota", env: null, rank: 6, since: null, you: true, ask: [] }])] }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    expect((await box().findByText(S.needUnknown)).closest("li")?.textContent).toBe(`${S.needUnknown}Open the app`);
    expect(screen.getByText("Needs a person")).toBeDefined();
  });

  // Past what one read lists, the rest are the fleet's own needs_you filter, a fresh view that keeps the page's rows.
  it("sends the rest past one read to the fleet's needs_you, keeping the rows a page holds", async () => {
    host(() => ({ ...golden, tiles: { ...golden.tiles, needs_you: 99 }, apps: [...golden.apps, app("zeta", [refused(), refused({ destination: "hooks.slack.com" })])] }));
    window.location.hash = "#/fleet?limit=100&stage=passed&page=3";
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByRole("region", { name: /^Needs (attention|you)$/ });
    fireEvent.click(await screen.findByRole("button", { name: "Show 94 more" }));
    expect(screen.getByRole("link", { name: "Show 92 more" }).getAttribute("href")).toBe("#/fleet?limit=100&needs_you=true");
  });
});
