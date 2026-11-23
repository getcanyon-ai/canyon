import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AccessRead, Actor, TeamAccess } from "../api";
import { S } from "../strings";
import { Access } from "./Access";

// The contract's own access read, share and refusals: a change to their shape breaks this test.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/access/claims-portal.json"), "utf8")) as {
  read: AccessRead; share: { grant: { kind: string; grantee: string }; request: unknown; response: AccessRead };
  owner_required: { message: string }; last_owner: { message: string }; groups_unsupported: { message: string };
};
const dana: Actor = { kind: "person", subject: "dana@meridian.example", display: "Dana Whitfield" };
const ACCESS = "/api/v1/projects/claims-portal/access";
const REQUEST = "/api/v1/projects/claims-portal/access-requests/sam%40meridian.example";
// As the project read answers them: an owner (requests is answered to owners and admins only) and a contributor.
const owner: TeamAccess = { level: "owner", everyone: true, people: 0, groups: 1, requests: 1 };
const contributor: TeamAccess = { level: "contributor", everyone: false, people: 0, groups: 1 };

type Answer = { status?: number; body?: unknown };
// A licence that unlocks access by sign-in group, and one that lapsed (#2879).
const UNLOCKED = { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] };
const LAPSED = { state: "lapsed", allows: [] };
const LICENCE = "/api/v1/licence";
// The server answers from routes, the licence read from `licence` unless routes names it; every call but the licence
// read is recorded.
function server(routes: Record<string, Answer>, licence: unknown = UNLOCKED) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const { status = 200, body } = routes[`${init?.method} ${url}`] ?? (url === LICENCE ? { body: licence } : { status: 404, body: { error: "not_found", message: "not found" } });
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([url]) => url !== LICENCE).map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const show = (access: TeamAccess, owners: Actor[] = [dana], groups: string[] = []) => render(<Access id="claims-portal" access={access} owners={owners} groups={groups} org="Meridian" auth="oidc" />);
const row = (name: string) => within(screen.getByText(name).closest("tr")!);

afterEach(() => vi.unstubAllGlobals());

describe("the Access card", () => {
  // Closed until asked for, its line on the summary (the app-page redesign); open while requests wait on its owners.
  it("is closed until asked for, unless access requests wait", async () => {
    server({ [`GET ${ACCESS}`]: { body: golden.read } });
    const card = async () => (await screen.findByRole("region", { name: "Access" })).querySelector("details")?.hasAttribute("open");
    show(owner);
    expect(await card()).toBe(true);
    cleanup();
    show({ ...owner, requests: 0 });
    expect(await card()).toBe(false);
    cleanup();
    show(contributor);
    expect(await card()).toBe(false);
  });

  // A fresh project's page can mount this card before the app's own id is resolved (a route param
  // read differently, or an effect that fires ahead of the id it's keyed on). The card must wait
  // for a real id rather than asking the server for "" and logging its 404 (canyonv2-028b).
  it("makes no request while the app's id is still unknown, then reads it once the id arrives", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read } });
    const { rerender } = render(<Access id="" access={owner} owners={[dana]} groups={[]} org="Meridian" auth="oidc" />);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(sent()).toEqual([]);
    rerender(<Access id="claims-portal" access={owner} owners={[dana]} groups={[]} org="Meridian" auth="oidc" />);
    expect(await screen.findByText("Dana Whitfield")).toBeDefined();
    expect(sent()).toEqual([`GET ${ACCESS}`]);
  });

  it("an owner sees every grant with its controls, and the requests to answer", async () => {
    server({ [`GET ${ACCESS}`]: { body: golden.read } });
    const { container } = show(owner);
    expect(await screen.findByText("Everyone at Meridian")).toBeDefined();
    expect(screen.getByText("Shared with everyone at Meridian.")).toBeDefined();
    expect(row("Everyone at Meridian").getAllByRole("cell").map((cell) => cell.textContent)).toEqual(["Everyone at Meridian", "read-only", "—", "—", "—", "Remove"]);
    expect((row("Dana Whitfield").getByRole("combobox", { name: "Access for Dana Whitfield" }) as HTMLSelectElement).value).toBe("owner");
    expect(row("Dana Whitfield").getByText("approver")).toBeDefined();
    expect((row("claims-engineering").getByRole("combobox") as HTMLSelectElement).value).toBe("contributor");
    expect(screen.getByRole("combobox", { name: "Share with" })).toBeDefined();
    expect(screen.getByText(S.roles)).toBeDefined();
    expect(screen.getByText("Reads every claim and changes none")).toBeDefined();
    expect(screen.getByRole("button", { name: "Add sam@meridian.example as read-only" })).toBeDefined();
    // The API's words stay off the screen (EXPERIENCE.md D6).
    expect(container.textContent).not.toMatch(/viewer|grant|level/i);
  });

  it("a contributor reads who it is shared with and whom to ask, and no control", () => {
    const sent = server({});
    const { container } = show(contributor);
    expect(screen.getByText("Shared with 1 group besides its owners · Ask Dana Whitfield or an admin to change it.")).toBeDefined();
    expect(container.querySelectorAll("input, select, button")).toHaveLength(0);
    expect(sent()).toEqual([]);
  });

  it("a contributor on an app with no owner is told to ask a Canyon admin", () => {
    server({});
    show({ ...contributor, everyone: true }, []);
    expect(screen.getByText("Shared with everyone at Meridian · Ask a Canyon admin to change it.")).toBeDefined();
  });

  it("a contributor on an app a group owns is told to ask that group", () => {
    server({});
    show({ ...contributor, everyone: true }, [], ["claims-team"]);
    expect(screen.getByText("Shared with everyone at Meridian · Ask a member of the claims-team group or an admin to change it.")).toBeDefined();
  });

  it("shares with the address typed, read-only unless another is chosen, with the roles ticked", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`PUT ${ACCESS}/email/sam%40meridian.example`]: { body: golden.share.response } });
    show(owner);
    fireEvent.change(await screen.findByRole("combobox", { name: "Share with" }), { target: { value: golden.share.grant.grantee } });
    fireEvent.click(screen.getByRole("checkbox", { name: /auditor/ }));
    fireEvent.click(screen.getByRole("button", { name: "Share" }));
    expect(await screen.findByText("Sam Ortiz")).toBeDefined();
    expect(sent()).toContain(`PUT ${ACCESS}/email/sam%40meridian.example ${JSON.stringify(golden.share.request)}`);
    expect(screen.queryByRole("button", { name: /as read-only/ })).toBeNull(); // the share ended the request
  });

  // A read-only member's request is to join the team (#2774): its Add makes them a contributor; anyone else's is read-only.
  it("adds a read-only member who asked to its team, as a contributor", async () => {
    const read = { ...golden.read, requests: [...golden.read.requests, { email: "frank@meridian.example", display: "Frank", requested_at: "2026-09-02T09:00:00Z", level: "viewer" as const }] };
    const sent = server({ [`GET ${ACCESS}`]: { body: read }, [`PUT ${ACCESS}/email/frank%40meridian.example`]: { body: golden.share.response } });
    show(owner);
    expect(await screen.findByRole("button", { name: "Add sam@meridian.example as read-only" })).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Add frank@meridian.example to its team" }));
    await screen.findByText("Sam Ortiz");
    expect(sent()).toContain(`PUT ${ACCESS}/email/frank%40meridian.example {"level":"contributor","roles":[]}`);
  });

  // The summary is the card's own read (#2560): a share or an unshare moves it at once, never the page's counts.
  it("the summary follows a share and an unshare at once", async () => {
    const alone = { ...golden.read, everyone: null, entries: golden.read.entries.filter((one) => one.level === "owner"), requests: [] };
    const shared = { ...alone, entries: [...alone.entries, golden.share.response.entries.at(-1)!] };
    server({ [`GET ${ACCESS}`]: { body: alone }, [`PUT ${ACCESS}/email/sam%40meridian.example`]: { body: shared }, [`DELETE ${ACCESS}/email/sam%40meridian.example`]: { status: 204 } });
    show({ level: "owner", everyone: false, people: 0, groups: 0, requests: 0 });
    fireEvent.change(await screen.findByRole("combobox", { name: "Share with" }), { target: { value: "sam@meridian.example" } });
    expect(screen.getByText(S.private)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Share" }));
    expect(await screen.findByText("Shared with 1 person besides its owners.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Remove Sam Ortiz" }));
    expect(await screen.findByText(S.private)).toBeDefined();
  });

  it("everyone is shared read-only, never at another level", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: { ...golden.read, everyone: null } }, [`PUT ${ACCESS}/everyone/everyone`]: { body: golden.read } });
    show({ ...owner, everyone: false });
    fireEvent.change(await screen.findByRole("combobox", { name: "Access" }), { target: { value: "owner" } });
    fireEvent.change(screen.getByRole("combobox", { name: "Share with" }), { target: { value: "Everyone at Meridian" } });
    expect(screen.queryByRole("combobox", { name: "Access" })).toBeNull();
    fireEvent.submit(screen.getByRole("combobox", { name: "Share with" }));
    await waitFor(() => expect(sent()).toContain(`PUT ${ACCESS}/everyone/everyone {"level":"viewer","roles":[]}`));
  });

  it("changes a grant in place, keeping its roles", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`PUT ${ACCESS}/email/dana%40meridian.example`]: { body: golden.read } });
    show(owner);
    fireEvent.change(await screen.findByRole("combobox", { name: "Access for Dana Whitfield" }), { target: { value: "contributor" } });
    await waitFor(() => expect(sent()).toContain(`PUT ${ACCESS}/email/dana%40meridian.example {"level":"contributor","roles":["approver"]}`));
  });

  // Each control that takes its row away hands the keyboard to the add box, never to the page.
  // A second click while the first is in flight sends nothing: the second DELETE would answer 404.
  for (const [control, request] of [
    ["Remove the group claims-engineering", `DELETE ${ACCESS}/group/claims-engineering`],
    ["Dismiss sam@meridian.example", `DELETE ${REQUEST}`],
    ["Add sam@meridian.example as read-only", `PUT ${ACCESS}/email/sam%40meridian.example {"level":"viewer","roles":[]}`],
  ] as const) {
    it(`${control.split(" ")[0]} is sent once and leaves the keyboard on the add box`, async () => {
      const [method, path] = request.split(" ");
      const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`${method} ${path}`]: { status: method === "PUT" ? 200 : 204, body: golden.read } });
      show(owner);
      fireEvent.change(await screen.findByRole("combobox", { name: "Access" }), { target: { value: "owner" } }); // the add box's choice is not the request's
      const button = screen.getByRole("button", { name: control });
      button.focus();
      fireEvent.click(button);
      fireEvent.click(button);
      await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("combobox", { name: "Share with" })));
      expect(sent().filter((one) => one.startsWith(`${method} ${path}`))).toEqual([request]);
    });
  }

  // Every refusal is the server's own sentence, whichever route said it.
  for (const [code, method, path, act] of [
    ["owner_required", "GET", ACCESS, () => undefined],
    ["last_owner", "DELETE", `${ACCESS}/email/dana%40meridian.example`, () => fireEvent.click(screen.getByRole("button", { name: "Remove Dana Whitfield" }))],
    ["groups_unsupported", "PUT", `${ACCESS}/group/ops`, () => {
      fireEvent.change(screen.getByRole("combobox", { name: "Share with" }), { target: { value: "group:ops" } });
      fireEvent.click(screen.getByRole("button", { name: "Share" }));
    }],
  ] as const) {
    it(`renders ${code} as the server says it`, async () => {
      const refusal = { status: code === "owner_required" ? 403 : 409, body: { error: code, ...golden[code] } };
      server({ [`GET ${ACCESS}`]: code === "owner_required" ? refusal : { body: golden.read }, [`${method} ${path}`]: refusal });
      show(owner);
      if (code !== "owner_required") await screen.findByText("Dana Whitfield");
      act();
      expect(await screen.findByText(golden[code].message)).toBeDefined();
    });
  }

  // Adding a group, or raising a group's level, is Canyon Enterprise's (#2879): with a lapsed licence an owner lowers
  // a group's level or removes it, and is told why in place of Share; a person is still shared with. Narrowing is never
  // gated: a paywall never stands between an owner and taking access away (#2881's review).
  it("without a licence, an owner lowers or removes a group but neither adds one nor raises its level", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`DELETE ${ACCESS}/group/claims-engineering`]: { status: 204 },
      [`PUT ${ACCESS}/group/claims-engineering`]: { body: golden.read } }, LAPSED);
    show(owner);
    await screen.findByText("Dana Whitfield");
    const level = () => row("claims-engineering").getByRole("combobox", { name: "Access for the group claims-engineering" }) as HTMLSelectElement;
    await waitFor(() => expect([...level().options].map((option) => option.value)).toEqual(["contributor", "viewer"]));
    expect(level().value).toBe("contributor");
    expect((row("Dana Whitfield").getByRole("combobox", { name: "Access for Dana Whitfield" }) as HTMLSelectElement).value).toBe("owner");
    fireEvent.change(level(), { target: { value: "viewer" } });
    await waitFor(() => expect(sent().some((call) => call.startsWith(`PUT ${ACCESS}/group/claims-engineering {"level":"viewer"`))).toBe(true));
    fireEvent.change(screen.getByRole("combobox", { name: "Share with" }), { target: { value: "group:ops" } });
    expect(screen.getByText(S.licenceRequiredGroups)).toBeDefined();
    expect(screen.queryByRole("button", { name: "Share" })).toBeNull();
    fireEvent.change(screen.getByRole("combobox", { name: "Share with" }), { target: { value: "sam@meridian.example" } });
    expect(screen.getByRole("button", { name: "Share" })).toBeDefined();
    expect(screen.queryByText(S.licenceRequiredGroups)).toBeNull();
    fireEvent.click(row("claims-engineering").getByRole("button", { name: "Remove the group claims-engineering" }));
    await waitFor(() => expect(sent()).toContain(`DELETE ${ACCESS}/group/claims-engineering`));
  });

  // A group is shared by its name: an email address has an @ and a group's name never does, so nobody types a prefix (#102).
  // Without the licence a name with no @ is an address still being typed, never a group to upsell.
  it("shares with a group by its name alone where groups may be shared, and takes a name with no @ as an address where not", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`PUT ${ACCESS}/group/ops`]: { body: golden.read } });
    show(owner);
    await screen.findByText("Dana Whitfield");
    const box = screen.getByRole("combobox", { name: "Share with" });
    expect(box.getAttribute("placeholder")).toBe("An email address, a group or Everyone at Meridian");
    fireEvent.change(box, { target: { value: "ops" } });
    fireEvent.click(screen.getByRole("button", { name: "Share" }));
    await waitFor(() => expect(sent().some((call) => call.startsWith(`PUT ${ACCESS}/group/ops`))).toBe(true));
    cleanup();
    server({ [`GET ${ACCESS}`]: { body: golden.read } }, LAPSED);
    show(owner);
    await screen.findByText("Dana Whitfield");
    expect(screen.getByRole("combobox", { name: "Share with" }).getAttribute("placeholder")).toBe("An email address or Everyone at Meridian");
    fireEvent.change(screen.getByRole("combobox", { name: "Share with" }), { target: { value: "sam" } });
    expect(screen.queryByText(S.licenceRequiredGroups)).toBeNull();
    expect(screen.getByRole("button", { name: "Share" })).toBeDefined();
  });

  // On a phone each grant is a card, its access and Remove inside it: the table's start past the screen's edge (#126).
  it("on a phone, reads each grant as a card with its access to change and its Remove", async () => {
    vi.stubGlobal("matchMedia", (media: string) => ({ matches: media === "(max-width: 719px)", addEventListener: () => {}, removeEventListener: () => {} }));
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read }, [`DELETE ${ACCESS}/group/claims-engineering`]: { status: 204 } });
    show(owner);
    const card = within((await screen.findByText("claims-engineering")).closest("li")!);
    expect(screen.queryByRole("table")).toBeNull();
    expect((card.getByRole("combobox", { name: "Access for the group claims-engineering" }) as HTMLSelectElement).value).toBe("contributor");
    for (const label of ["Access", "Roles", "Added by", "When"]) expect(card.getByText(label)).toBeDefined();
    fireEvent.click(card.getByRole("button", { name: "Remove the group claims-engineering" }));
    await waitFor(() => expect(sent()).toContain(`DELETE ${ACCESS}/group/claims-engineering`));
  });

  // A host with no sign-in routes none of the Access group (contracts/routes.md, Access): the card
  // must know this itself and ask nothing, rather than asking and losing the 404 to the console
  // (canyonv2-028b) — the auth-off e2e stack failed this way on every app page, not just fresh ones.
  // The same goes while the mode isn't known yet (Shell's own read of it still in flight): asking on
  // a guess is the race this card must not run, whichever way the guess would have landed.
  it("draws nothing and asks nothing while the sign-in mode is unknown, nor once it's known to have none; one request once it's known to have it", async () => {
    const sent = server({ [`GET ${ACCESS}`]: { body: golden.read } });
    const { container, rerender } = render(<Access id="claims-portal" access={owner} owners={[dana]} groups={[]} org="Meridian" auth={null} />);
    expect(container.textContent).toBe("");
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(sent()).toEqual([]);
    rerender(<Access id="claims-portal" access={owner} owners={[dana]} groups={[]} org="Meridian" auth="off" />);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(sent()).toEqual([]);
    rerender(<Access id="claims-portal" access={owner} owners={[dana]} groups={[]} org="Meridian" auth="oidc" />);
    expect(await screen.findByText("Dana Whitfield")).toBeDefined();
    expect(sent()).toEqual([`GET ${ACCESS}`]);
  });

  // The read fires again whenever `moved` changes (the page's poll), and a failure from an earlier
  // one must not outlive a later one that actually answered.
  it("clears a stale failure once a later read of the same card succeeds", async () => {
    let calls = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
      if (`${init?.method} ${url}` !== `GET ${ACCESS}`) return { ok: false, status: 404, json: async () => ({}) } as unknown as Response;
      calls++;
      if (calls === 1) return { ok: false, status: 500, json: async () => ({ error: "boom", message: "server exploded" }) } as unknown as Response;
      return { ok: true, status: 200, json: async () => golden.read } as unknown as Response;
    }));
    const { rerender } = render(<Access id="claims-portal" access={owner} owners={[dana]} groups={[]} org="Meridian" auth="oidc" />);
    expect(await screen.findByText("server exploded")).toBeDefined();
    rerender(<Access id="claims-portal" access={{ ...owner, people: 1 }} owners={[dana]} groups={[]} org="Meridian" auth="oidc" />); // moved changed: reads again
    expect(await screen.findByText("Dana Whitfield")).toBeDefined();
    expect(screen.queryByText("server exploded")).toBeNull();
  });
});
