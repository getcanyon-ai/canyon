import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FleetPage, FleetRow } from "../api";
import { Stage, type StageKey } from "../ui/Stage";
import { Fleet } from "./Fleet";

const golden = JSON.parse(
  readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8"),
) as Omit<FleetPage, "apps"> & { apps: FleetRow[] }; // every row the golden carries is a team row

// The server is a function of the query it is sent; the returned reader lists every URL asked for.
function server(answer: (query: URLSearchParams) => unknown, ok = true, status = 200) {
  const fetch = vi.fn(async (url: string) =>
    ({ ok, status, json: async () => (url.startsWith("/api/v1/audit") ? { events: [], next: null } : answer(new URL(url, "http://canyon").searchParams)) }) as unknown as Response);
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url]) => url);
}

const rows = () => screen.getAllByRole("row").slice(1) as HTMLTableRowElement[];
const names = () => rows().map((row) => row.cells[0]?.querySelector("a")?.textContent);
// The page polls, so the same URL repeats; what a control did is the newest one asked for.
const latest = (urls: string[]) => urls.filter((url) => url.startsWith("/api/v1/fleet")).at(-1);
// The controls live in the Filter menu, which a person opens first.
const menu = () => {
  const button = screen.getByRole("button", { name: /^Filter/ });
  if (button.getAttribute("aria-expanded") !== "true") fireEvent.click(button);
};
const control = (name: string) => { menu(); return screen.getByRole("combobox", { name }); };
const signedIn = { banner: "none", viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false } };
const at = (hash: string) => { window.location.hash = hash; };

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the fleet's search and Filter menu", () => {
  // No Apply: a box sends what was typed once the typing settles, and one word is one request.
  it("sends the search as q once the typing settles, without an Apply button", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const box = screen.getByRole("searchbox", { name: "Search apps" }) as HTMLInputElement;
    expect(box.maxLength).toBe(64);
    for (const typed of ["c", "cl", "claims"]) fireEvent.change(box, { target: { value: typed } });
    expect(window.location.hash).toBe(""); // nothing is asked while the typing goes on
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?q=claims"));
    expect(screen.queryByRole("button", { name: "Apply" })).toBeNull();
  });

  it("sends the stack from the menu, encoded as typed", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    menu();
    fireEvent.change(screen.getByRole("textbox", { name: "Stack" }), { target: { value: "Next js & co" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stack=Next+js+%26+co"));
  });

  it("composes two changes made one after the other", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.change(control("Status"), { target: { value: "passed" } });
    fireEvent.change(control("Deployed"), { target: { value: "preview" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed&running=preview"));
    expect(screen.getByRole("button", { name: /^Filter/ }).textContent).toContain("(2)");
  });

  // Access is how far an app is shared, which only a host with sign-in has.
  it("offers Access where people sign in, asks for it, and offers to clear it", async () => {
    const sent = server(() => ({ ...golden, ...signedIn }));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(within(control("Access")).getAllByRole("option").map((option) => option.textContent)).toEqual(["Anyone", "Private", "Shared", "Everyone"]);
    fireEvent.change(control("Access"), { target: { value: "shared" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?access=shared"));
    expect(screen.getByRole("link", { name: "Clear filters" }).getAttribute("href")).toBe("#/fleet");
  });

  it("offers no Access filter on a host with sign-in off", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    menu();
    expect(screen.queryByRole("combobox", { name: "Access" })).toBeNull();
  });

  it("takes a menu back to its first option by dropping the key, not emptying it", async () => {
    const sent = server(() => golden);
    at("#/fleet?running=production");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.change(control("Deployed"), { target: { value: "" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet"));
  });

  it("asks the server for one of the ten words and shows exactly the rows it answers", async () => {
    const blocked = golden.apps.filter((app) => app.latest.stage === "blocked");
    const sent = server((query) => (query.get("stage") === "blocked" ? { ...golden, apps: blocked } : golden));
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.change(control("Status"), { target: { value: "blocked" } });
    await waitFor(() => expect(names()).toEqual(["claims-portal", "commission-calc"]));
    expect(sent()).toContain("/api/v1/fleet?stage=blocked");
  });

  // The rows, their order and the page are the server's answer. Here it answers the reverse of the
  // risk order and rows whose stack holds no "remix": the screen shows all of them, as they came.
  it("shows the server's page as it came, never re-sorted and never narrowed again", async () => {
    const backwards = [...golden.apps].reverse();
    server(() => ({ ...golden, apps: backwards }));
    at("#/fleet?stack=remix");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(names()).toEqual(backwards.map((app) => app.name));
  });

  it("composes the menu's controls with the search into one query", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.change(control("Status"), { target: { value: "passed" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed"));
    fireEvent.change(control("Deployed"), { target: { value: "preview" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed&running=preview"));
    menu();
    fireEvent.click(screen.getByRole("checkbox", { name: "Only apps that need you" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed&running=preview&needs_you=true"));
    menu();
    fireEvent.change(screen.getByRole("textbox", { name: "Stack" }), { target: { value: "Remix" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed&running=preview&needs_you=true&stack=Remix"));
    fireEvent.change(screen.getByRole("searchbox"), { target: { value: "hub" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?stage=passed&running=preview&needs_you=true&stack=Remix&q=hub"));
  });

  // Page 4 of one view is no page of another: a control goes back to the first page.
  it("drops the page when a control changes on a later page", async () => {
    const sent = server(() => golden);
    at("#/fleet?q=claims&page=4");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.change(control("Deployed"), { target: { value: "production" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?q=claims&running=production"));
  });

  // The server's three orders, and "Your apps" where people sign in; each goes back to the first page.
  // Codex after #1757: an order is no filter, so clearing the filters keeps it.
  it("keeps the order the rows are in when the filters are cleared", async () => {
    server(() => golden);
    at("#/fleet?sort=name&stage=blocked&page=2&limit=100");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(screen.getByRole("link", { name: "Clear filters" }).getAttribute("href")).toBe("#/fleet?sort=name&limit=100");
  });

  it("sorts by risk, change or name, and goes back to the first page", async () => {
    const sent = server(() => golden);
    at("#/fleet?q=claims&page=3");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const sort = screen.getByRole("combobox", { name: "Sort" }) as HTMLSelectElement;
    expect([...sort.options].map((one) => one.text)).toEqual(["Risk", "Recently changed", "Name"]);
    fireEvent.change(sort, { target: { value: "changed" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?q=claims&sort=changed"));
    fireEvent.change(sort, { target: { value: "risk" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?q=claims"));
  });

  it("narrows to your apps where people sign in, and back to all of them", async () => {
    const sent = server(() => ({ ...golden, ...signedIn }));
    at("#/fleet?page=2");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const group = within(screen.getByRole("group", { name: "Which apps" }));
    expect(group.getByRole("button", { name: "All apps" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(group.getByRole("button", { name: "Your apps" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?scope=team"));
    expect(await screen.findByText("The apps you own or work on. The counts are the whole org's.")).toBeDefined(); // an admin's counts are never narrowed
    expect(within(screen.getByRole("group", { name: "Which apps" })).getByRole("button", { name: "Your apps" }).getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(within(screen.getByRole("group", { name: "Which apps" })).getByRole("button", { name: "All apps" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet"));
  });

  it("offers your apps only where people sign in", async () => {
    server(() => golden); // the golden is a loopback host: nobody signs in, so nobody holds a grant
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(screen.queryByRole("group", { name: "Which apps" })).toBeNull();
  });

  it("clears one control without clearing the rest", async () => {
    const sent = server(() => golden);
    at("#/fleet?stage=blocked&q=claims");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect((screen.getByRole("searchbox") as HTMLInputElement).value).toBe("claims");
    fireEvent.change(control("Status"), { target: { value: "" } });
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?q=claims"));
  });

  it("turns the queue on with the one word the server accepts, and off again", async () => {
    const sent = server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    menu();
    fireEvent.click(screen.getByRole("checkbox", { name: "Only apps that need you" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?needs_you=true"));
    menu();
    await waitFor(() => expect((screen.getByRole("checkbox", { name: "Only apps that need you" }) as HTMLInputElement).checked).toBe(true));
    fireEvent.click(screen.getByRole("checkbox", { name: "Only apps that need you" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet"));
  });

  it("closes the menu on Escape", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    menu();
    fireEvent.keyDown(screen.getByRole("combobox", { name: "Status" }), { key: "Escape" });
    expect(screen.queryByRole("combobox", { name: "Status" })).toBeNull();
  });

  // A chip shows a count, so clicking it must show exactly the apps it counted — never that set
  // narrowed by whatever else was asked. The chips reset the query; the menu composes.
  it("drops the other filters when a chip is clicked", async () => {
    const sent = server(() => ({ ...golden, tiles: { ...golden.tiles, needs_you: 99 } })); // more needs than one read lists: the rest are the fleet's needs_you
    at("#/fleet?stage=passed&q=claims");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.click(await screen.findByRole("link", { name: "Show 94 more" }));
    await waitFor(() => expect(latest(sent())).toBe("/api/v1/fleet?needs_you=true"));
  });

  it("puts the way out beside an empty table", async () => {
    server(() => ({ ...golden, apps: [] }));
    at("#/fleet?stack=cobol");
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByText("No apps match")).toBeDefined();
    expect(screen.getByRole("link", { name: "Clear filters" }).getAttribute("href")).toBe("#/fleet");
  });

  // A hand-edited value the server refuses leaves nothing to click but the controls themselves.
  it("keeps the controls when the server refuses the query", async () => {
    server(() => ({ error: "invalid_stage" }), false, 400);
    at("#/fleet?stage=nope");
    render(<Fleet org="Meridian Insurance" />);
    expect(await screen.findByText("GET /api/v1/fleet answered 400")).toBeDefined();
    expect(document.body.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i); // not even in the failure line
    expect(screen.queryByRole("table")).toBeNull();
    expect(control("Status")).toBeDefined();
    expect(screen.getByRole("link", { name: "Clear filters" }).getAttribute("href")).toBe("#/fleet");
  });

  it("offers the ten words the table already uses, and none of the machine's", async () => {
    server(() => golden);
    const { container } = render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    menu();
    const options = within(document.getElementById("fleet-filter")!).getAllByRole("option").map((option) => option.textContent);
    expect(options).toHaveLength(15);
    expect(options).toContain("Nothing uploaded");
    expect(options).toContain("Awaiting review");
    expect(container.textContent).not.toMatch(/pkg_|package|receipt|stage|fingerprint/i);
  });

  // Pinned key by key: a second copy of the words with one of them wrong would pass the count above.
  it("labels each Status option with the word a stage pill shows for that key", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const options = within(control("Status")).getAllByRole("option").filter((option) => option.getAttribute("value") !== "");
    expect(options).toHaveLength(10);
    for (const option of options) {
      const cell = render(<Stage stage={option.getAttribute("value") as StageKey} />).container.textContent ?? "";
      expect(cell, option.getAttribute("value") ?? "").toBe(option.textContent); // the icon is drawn, not text
    }
  });

  it("closes the menu on a click outside it, and not on one inside it", async () => {
    server(() => golden);
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    fireEvent.mouseDown(control("Status"));
    expect(screen.queryByRole("combobox", { name: "Status" })).not.toBeNull();
    fireEvent.mouseDown(document.body);
    expect(screen.queryByRole("combobox", { name: "Status" })).toBeNull();
  });

  // The badge counts what the menu holds, never the search or a chip.
  it("counts the menu's filters on its button, not the search or a chip", async () => {
    server(() => golden);
    at("#/fleet?q=abc&stack=Remix&needs_you=true&tile=blocked");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    expect(screen.getByRole("button", { name: /^Filter/ }).textContent).toBe("Filter(2)");
  });

  // Clear filters, a chip or the back button change the query under the box: it shows what the
  // query holds, and a word still waiting to be sent is dropped rather than undoing the change.
  it("empties the search box when the query is cleared, and sends nothing typed before", async () => {
    const sent = server(() => golden);
    at("#/fleet?q=claims");
    render(<Fleet org="Meridian Insurance" />);
    await screen.findByText("premium-ledger");
    const box = screen.getByRole("searchbox") as HTMLInputElement;
    expect(box.value).toBe("claims");
    fireEvent.change(box, { target: { value: "claimsx" } });
    fireEvent.click(screen.getByRole("link", { name: "Clear filters" }));
    // A busy page (a loaded CI runner, #2857): nothing runs until the word has settled, so the
    // click's hashchange and the box's timer come due together, before the cleared query is drawn.
    const busy = Date.now() + 400;
    while (Date.now() < busy) { /* the main thread is held */ }
    await waitFor(() => expect(box.value).toBe(""));
    await new Promise((done) => setTimeout(done, 400));
    expect(latest(sent())).toBe("/api/v1/fleet");
  });
});
