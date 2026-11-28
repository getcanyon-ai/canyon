import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { EgressDestination, ProjectEgress, Status } from "../api";
import { S } from "../strings";
import { Outbound, prodPending } from "./Outbound";

const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
const egress = golden<{ project_read: ProjectEgress }>("egress/claims-portal.json").project_read;
const page = golden<{ status: Status; promotion: { promotion: NonNullable<Status["envs"]["prod"]> } }>("app-page/claims-portal.json");
const envs: Status["envs"] = { ...page.status.envs, prod: page.promotion.promotion };
// The card's own line under its heading, the app its subject; the env's label keeps the fragment canyon status says (#2723).
const FLOOR = "This app's calls may skip Canyon, so Canyon may not record or refuse them. An admin can recreate its network in Settings › Outbound.";
const FLOOR_KUBE = "This app's calls may skip Canyon, so Canyon may not record or refuse them. Its next deploy or promotion fixes this.";
const FLOOR_UNREAD = "This app's calls may skip Canyon, so Canyon may not record or refuse them.";
const AGAIN = "not yet through Canyon: deploy again";

type Answer = { status: number; body: unknown } | "dropped";
// The app's records, the host's via and its runtime (the front door's read), each its own answer; a function
// answers each read anew.
function server(read: ProjectEgress | Answer | (() => ProjectEgress), via: "off" | "canyon" | Answer = "canyon", runtime: "docker" | "kubernetes" = "docker") {
  const fetch = vi.fn(async (url: string) => {
    const answer: Answer = url === "/api/v1/front-door" ? { status: 200, body: { runtime, default_domain: "localhost", front_door: null, warning: "", hand_off_needs: [] } }
      : url === "/api/v1/egress" ? (typeof via === "string" && via !== "dropped" ? { status: 200, body: { via, mode: "observe" } } : via)
      : typeof read === "function" ? { status: 200, body: read() } : "since" in (read as object) ? { status: 200, body: read } : read as Answer;
    if (answer === "dropped") throw new TypeError("Failed to fetch");
    return { ok: answer.status < 300, status: answer.status, json: async () => answer.body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}
// Every read answered and every state set before anything is asserted.
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));
const env = (name: string) => within(screen.getByRole("region", { name: `${name} outbound` }));
const rows = (name: string) => env(name).getAllByRole("row").slice(1).map((row) => [...row.querySelectorAll("td")].map((cell, at) => (at === 1 ? "" : cell.textContent)));
const row = (over: Partial<EgressDestination>): EgressDestination => ({ ...egress.envs.prod.destinations[0]!, ...over });
const withProd = (destinations: EgressDestination[]): ProjectEgress => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, destinations } } });

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("the Outbound card", () => {
  // design/EGRESS.md §8.3: per env, each destination called in the last 30 days, the latest seen first —
  // last seen, calls, last result — from the egress contract's app read, in its order.
  it("lists each env's destinations as the records say them", async () => {
    server(egress);
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    // every env's table keeps the Allow column, so stacked tables line up; production's allowlist allows both: no Allow
    expect(rows("Preview")).toEqual([["a call with no valid host name, port 443", "", "1", "refused", ""], ["registry.npmjs.org:80", "", "1", "redirected (3xx)", ""]]);
    expect(rows("Production")).toEqual([["api.stripe.com:443", "", "1,840", "answered (2xx)", ""], ["hooks.slack.com:443", "", "12", "tunnel failed: UF,URX", ""]]);
    expect(screen.queryByText(FLOOR)).toBeNull();
    expect(screen.queryByText(AGAIN)).toBeNull();
  });

  // design/EXPERIENCE.md D5 (#2384): outbound is beta in v0.2.0, the card marked beside its heading.
  it("marks the card Beta beside its heading, with what beta means", async () => {
    server(egress);
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    const heading = screen.getByRole("heading", { level: 2 });
    expect(within(heading).getByText(S.beta).textContent).toBe(`${S.beta}: ${S.betaWhy}`);
  });

  // A refused call reads refused; a plain request's flags are no tunnel's, so its class shows; a call that
  // got no answer says so, never a bare "none". A tunnel's DC or UC is how it ends (#2349): only its other
  // flags say it failed.
  it("says each last result in its words", async () => {
    server(withProd([
      row({ destination: "a.example", last_outcome: "refused", last_status_class: "4xx" }),
      row({ destination: "b.example", last_protocol: "http", last_status_class: "5xx", last_response_flags: ["DC"] }),
      row({ destination: "c.example", last_status_class: "none" }),
      row({ destination: "d.example", last_status_class: "2xx", last_response_flags: ["DC"] }),
      row({ destination: "e.example", last_status_class: "5xx", last_response_flags: ["DC", "UF"] }),
      row({ destination: "f.example", last_status_class: "3xx", last_response_flags: ["UC"] }),
    ]));
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(rows("Production").map((one) => one[3])).toEqual(["refused", "failed at the destination (5xx)", "no answer", "answered (2xx)", "tunnel failed: UF", "redirected (3xx)"]);
  });

  // A call no allowlist can let through says why beside its row (#2723): an IP address, or a port other than 80 or 443,
  // which every mode refuses (egress.schema.json's mode). A name the allowlist refused is Allow's, and reads refused.
  it("says why nobody can allow a refused call: an IP address, or a port", async () => {
    server(withProd([
      row({ destination: "1.1.1.1", port: 443, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["UAEX"] }),
      row({ destination: "example.com", port: 8443, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["NR"] }),
      row({ destination: "10.0.0.1", port: 8443, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["NR"] }),
      row({ destination: "api.openai.com", port: 443, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["UAEX"] }),
      row({ destination: "invalid", port: 443, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["UAEX"] }),
    ]));
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(rows("Production").map((one) => one[3])).toEqual(["refused: an IP address", "refused: port 8443", "refused: an IP address", "refused", "refused"]);
    // Under the table, what fixes those: prod.egress-allowed's own how-to-fix (gate/policy/canonical.go), byte for byte.
    expect(env("Production").getByText("A person allows each host name in Canyon's dashboard; anything else has to change in the code.")).toBeDefined();
    expect(env("Preview").queryByText(/anything else has to change in the code/)).toBeNull(); // its one refused row is invalid, a name no allowlist holds
    cleanup();
    // An env the org's own platform runs lists none of its calls, so says nothing of what fixes them.
    render(<Outbound id="claims-portal" envs={{ ...envs, prod: { ...envs.prod!, run_by: "org" } } as Status["envs"]} />);
    await settle();
    expect(env("Production").getByText("runs on your own platform")).toBeDefined();
    expect(env("Production").queryByText(/anything else has to change in the code/)).toBeNull();
  });

  // What the app headline names: production's requests the server says it refuses now (refusing, the refused tile's
  // predicate; #2723). A pending one production no longer refuses, and a dismissed one, are no need; a read out of shape
  // names none.
  it("names for the headline only production's requests it refuses now", () => {
    const req = (destination: string, state: "pending" | "dismissed", refusing: boolean) => ({ destination, state, first_refused_at: "2026-11-08T09:00:00Z", last_refused_at: "2026-11-08T09:00:00Z", refusals: 2, refusing });
    const read = (requests: unknown[]) => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, requests } } }) as ProjectEgress;
    expect(prodPending(read([req("api.stripe.com", "pending", true), req("api.openai.com", "pending", false), req("hooks.zapier.com", "dismissed", true)]))).toEqual(["api.stripe.com"]);
    expect(prodPending(read([{ destination: "api.stripe.com", state: "pending" }]))).toEqual([]); // no refusing: the server never said so
    expect(prodPending({ ...egress, envs: {} } as unknown as ProjectEgress)).toEqual([]);
  });

  // Fail closed (#2291): an answer without its envs, without an env the status read names, or without its
  // gaps is not read — never a thrown page.
  it("says an answer out of shape couldn't be read", async () => {
    const { prod: _, ...noProd } = egress.envs;
    for (const shape of [
      { ...egress, envs: undefined }, { ...egress, envs: noProd }, { ...egress, envs: { ...egress.envs, prod: null } },
      { ...egress, envs: { ...egress.envs, prod: { via: "canyon" } } }, { ...egress, gaps: undefined }, { ...egress, gaps: [null] },
      withProd([{ ...row({}), last_response_flags: undefined } as unknown as EgressDestination]),
    ]) {
      server(shape as unknown as ProjectEgress);
      render(<Outbound id="claims-portal" envs={envs} />);
      await settle();
      expect(screen.getByText("Outbound couldn't be read.")).toBeDefined();
      cleanup();
    }
  });

  it("says an env through Canyon has recorded no calls", async () => {
    server(withProd([]));
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(env("Production").getByText("No calls out recorded.")).toBeDefined();
  });

  // §8.1's values, and their order: the org's platform first, whatever the floor; then a network not read
  // as enforced, said once above the envs, never as deploy again; then a version from before via canyon.
  it("says what is not through Canyon, the org's platform before the floor before deploy again", async () => {
    const orgPreview = { ...envs, preview: { ...envs.preview!, run_by: "org" } } as Status["envs"];
    server({ ...egress, floor: "not_enforced", envs: { ...egress.envs, prod: { ...egress.envs.prod, via: "off" } } });
    const { unmount } = render(<Outbound id="claims-portal" envs={orgPreview} />);
    await settle();
    expect(screen.getByRole("note").textContent).toBe(FLOOR);
    expect(env("Preview").getByText("runs on your own platform")).toBeDefined();
    expect(env("Preview").queryByRole("table")).toBeNull();
    expect(env("Production").queryByText("not yet through Canyon: deploy again")).toBeNull();
    unmount();
    server({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, via: "off" } } });
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(env("Production").getByText("not yet through Canyon: deploy again")).toBeDefined();
    expect(env("Preview").queryByText("not yet through Canyon: deploy again")).toBeNull();
    expect(screen.queryByRole("note")).toBeNull();
  });

  // Under Off, an app whose network still sends its calls through Canyon says so, as Settings › Outbound does, and the way
  // out is recreating its network: a link for an admin, ask an admin for anyone else; never deploy again, which doesn't
  // change the network. The 30 days' destinations say Outbound is Off (#2774).
  it("says under Off that the app still goes through Canyon until its network is recreated", async () => {
    const stuck: ProjectEgress = { ...egress, floor: "enforced", envs: { ...egress.envs, prod: { ...egress.envs.prod, via: "off" } } };
    for (const [level, line] of [
      ["contributor", "Outbound is Off, but this app's calls still go through Canyon until its network is recreated. Ask an admin to recreate it."],
      ["owner", "Outbound is Off, but this app's calls still go through Canyon until its network is recreated. Ask an admin to recreate it."],
      ["admin", "Outbound is Off, but this app's calls still go through Canyon until you recreate its network in Settings › Outbound."],
    ] as const) {
      server(stuck, "off");
      const { unmount } = render(<Outbound id="claims-portal" envs={envs} level={level} />);
      await settle();
      expect(screen.getByRole("note").textContent).toBe(line);
      expect(screen.queryByRole("link", { name: "Settings › Outbound" })?.getAttribute("href") ?? null).toBe(level === "admin" ? "#/settings/outbound" : null);
      expect(screen.queryByText(AGAIN)).toBeNull();
      expect(screen.getByRole("heading", { level: 2 }).textContent).toContain("Destinations called in the last 30 days · Outbound is Off");
      unmount();
    }
    // through Canyon, the same env from before via canyon still says deploy again, and the note is not there
    server(stuck, "canyon");
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(env("Production").getByText(AGAIN)).toBeDefined();
    expect(screen.getByRole("heading", { level: 2 }).textContent).not.toContain("Outbound is Off");
  });

  // A recreate is named only where the runtime is read as Docker: on Kubernetes, which has no network to recreate, and
  // while the runtime read is pending or failed, the card names none and never says deploy again under Off. A floor not
  // enforced under Off says nothing goes through Canyon of it (#2774).
  it("names a recreate under Off only on a runtime read as Docker", async () => {
    const stuck: ProjectEgress = { ...egress, floor: "enforced", envs: { ...egress.envs, prod: { ...egress.envs.prod, via: "off" } } };
    const door = (answer: "pending" | "failed") => {
      const inner = vi.mocked(fetch);
      vi.stubGlobal("fetch", vi.fn((url: string, init?: RequestInit) => url !== "/api/v1/front-door" ? inner(url, init)
        : answer === "pending" ? new Promise<Response>(() => {}) : Promise.resolve({ ok: false, status: 500, json: async () => ({ error: "internal" }) } as unknown as Response)));
    };
    for (const runtime of ["kubernetes", "pending", "failed"] as const) {
      for (const level of ["admin", "contributor"] as const) {
        server(stuck, "off", runtime === "kubernetes" ? "kubernetes" : "docker");
        if (runtime !== "kubernetes") door(runtime);
        const { unmount } = render(<Outbound id="claims-portal" envs={envs} level={level} />);
        await settle();
        const card = screen.getByRole("region", { name: "Outbound" });
        expect(card.textContent, `${runtime}, ${level}`).not.toMatch(/recreat/i);
        expect(screen.queryByText(AGAIN), `${runtime}, ${level}`).toBeNull();
        unmount();
      }
    }
    server({ ...stuck, floor: "not_enforced" }, "off");
    render(<Outbound id="claims-portal" envs={envs} level="admin" />);
    await settle();
    expect(screen.getByRole("region", { name: "Outbound" }).textContent).not.toMatch(/still go(es)? through Canyon/);
  });

  // To an admin, who recreates it, the floor's line links to Settings › Outbound; anyone else reads who can (#2774).
  it("links an admin to recreate a network that isn't enforced", async () => {
    server({ ...egress, floor: "not_enforced" });
    const { unmount } = render(<Outbound id="claims-portal" envs={envs} level="admin" />);
    await settle();
    expect(screen.getByRole("note").textContent).toBe("This app's calls may skip Canyon, so Canyon may not record or refuse them. Recreate its network in Settings › Outbound.");
    expect(screen.getByRole("link", { name: "Settings › Outbound" }).getAttribute("href")).toBe("#/settings/outbound");
    unmount();
    server({ ...egress, floor: "not_enforced" });
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    expect(screen.getByRole("note").textContent).toBe(FLOOR);
    expect(screen.queryByRole("link")).toBeNull();
  });

  // Fail closed: a floor this build does not know, or none at all under via canyon, is not enforced.
  it("reads a floor that is not enforced as not enforced", async () => {
    for (const floor of ["unknown", null]) {
      server({ ...egress, floor } as unknown as ProjectEgress);
      const { unmount } = render(<Outbound id="claims-portal" envs={envs} />);
      await settle();
      expect(screen.getByRole("note").textContent).toBe(FLOOR);
      unmount();
    }
  });

  // On Kubernetes (design/EGRESS.md §8.1, #2286) the floor is per env and there is no network to recreate: under
  // not_enforced only an env not run under canyon says its next deploy or promotion floors it, never an admin's
  // recreation; a floor this build does not know says so of the app, fail closed.
  it("says on Kubernetes that the next deploy or promotion enforces the floor", async () => {
    const KUBE = "may skip Canyon until its next deploy or promotion";
    server({ ...egress, floor: "not_enforced", envs: { ...egress.envs, prod: { ...egress.envs.prod, via: "off" } } }, "canyon", "kubernetes");
    const { unmount } = render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(screen.queryByRole("note")).toBeNull();
    expect(env("Production").getByText(KUBE)).toBeDefined();
    expect(env("Preview").queryByText(KUBE)).toBeNull();
    expect(screen.queryByText(FLOOR)).toBeNull();
    unmount();
    server({ ...egress, floor: "unknown" } as unknown as ProjectEgress, "canyon", "kubernetes");
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(screen.getByRole("note").textContent).toBe(FLOOR_KUBE);
  });

  // Until the host's runtime is read the card names no remedy — never Docker's recreation on a Kubernetes host
  // (#2317) — and it asks again at its next reread.
  it("names no remedy until it reads the runtime, and asks again", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let door: Answer = { status: 500, body: { error: "internal", message: "internal error" } };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const answer: Answer = url === "/api/v1/front-door" ? door : url === "/api/v1/egress" ? { status: 200, body: { via: "canyon", mode: "observe" } }
        : { status: 200, body: { ...egress, floor: "not_enforced" } };
      if (answer === "dropped") throw new TypeError("Failed to fetch");
      return { ok: answer.status < 300, status: answer.status, json: async () => answer.body } as unknown as Response;
    }));
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(screen.getByRole("note").textContent).toBe(FLOOR_UNREAD);
    expect(screen.queryByText(FLOOR)).toBeNull();
    door = { status: 200, body: { runtime: "docker" } };
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(screen.getByRole("note").textContent).toBe(FLOOR);
  });

  // The calls past the cap, the env's own over_cap gap, said with a real plural.
  it("says the calls past the cap of each env", async () => {
    const gap = (calls: number, env: "preview" | "prod") => ({ from: "2026-10-11T10:40:50Z", to: "2026-10-11T10:41:20Z", reason: "over_cap", calls, app: "claims-portal", env });
    server({ ...egress, gaps: [...egress.gaps, gap(37, "prod"), gap(1, "preview")] });
    render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(env("Production").getByText("37 calls to destinations past 500, not listed")).toBeDefined();
    expect(env("Preview").getByText("1 call to destinations past 500, not listed")).toBeDefined();
  });

  // Nothing goes through Canyon (via off, the network open): one line in place of the card, where the /outbound
  // link a refusal carries lands (#2561). Via off on an internal network still routes the app through Canyon:
  // the card. Nothing deployed in an env: no section for it.
  it("says in one line that nothing goes through Canyon, and draws no env with nothing in it", async () => {
    server({ ...egress, floor: null }, "off");
    const { unmount } = render(<Outbound id="claims-portal" envs={envs} shown />);
    await settle();
    const card = screen.getByRole("region", { name: "Outbound" });
    expect(within(card).getByRole("heading", { level: 2 }).textContent).toMatch(/^Outbound/);
    // floor null under Off is an open network or one Canyon can't tell (a recreation under way or failed): the line
    // claims nothing about the network, never that the app calls out directly (#2774)
    expect(within(card).getAllByRole("paragraph").map((one) => one.textContent)).toEqual(["Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound."]);
    expect(card.textContent).not.toMatch(/directly/);
    expect(within(card).queryByRole("region")).toBeNull();
    expect(within(card).queryByRole("button")).toBeNull();
    unmount();
    server(egress, "off");
    const second = render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(rows("Production")).toHaveLength(2);
    second.unmount();
    server({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, mode: "observe" } } });
    render(<Outbound id="claims-portal" envs={{ preview: envs.preview }} />);
    await settle();
    expect(screen.getByRole("region", { name: "Outbound" })).toBeDefined();
    expect(screen.queryByRole("region", { name: "Production outbound" })).toBeNull();
  });

  // Below contributor the records answer team_required (design/PERMISSIONS.md, Outbound): no card and no
  // refusal drawn. Any other failure of either read is said — another 403 included.
  it("draws nothing on team_required, and says every other failure", async () => {
    server({ status: 403, body: { error: "team_required", message: "Ask Dana Whitfield to add you to claims-portal" } });
    const { container, unmount } = render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(container.textContent).toBe("");
    unmount();
    for (const [read, via] of [
      [{ status: 500, body: { error: "internal", message: "internal error" } }, "canyon"],
      [{ status: 403, body: { error: "person_required", message: "a person reads this" } }, "canyon"],
      ["dropped", "canyon"],
      [egress, { status: 500, body: { error: "internal", message: "internal error" } }],
      [egress, "dropped"],
    ] as [ProjectEgress | Answer, "canyon" | Answer][]) {
      server(read, via);
      const shown = render(<Outbound id="claims-portal" envs={envs} />);
      await settle();
      expect(shown.container.textContent).toBe("Outbound couldn't be read.");
      shown.unmount();
    }
  });

  // A recreation changes nothing the status read says: the card reads again on its own, and when an
  // env's version or deploy changes.
  it("reads again, so a recreated network shows", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let floor: ProjectEgress["floor"] = "not_enforced";
    const fetch = server(() => ({ ...egress, floor }));
    const { rerender } = render(<Outbound id="claims-portal" envs={envs} />);
    await settle();
    expect(screen.getByRole("note")).toBeDefined();
    floor = "enforced";
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(screen.queryByRole("note")).toBeNull();
    const asked = fetch.mock.calls.length;
    floor = "not_enforced";
    rerender(<Outbound id="claims-portal" envs={{ ...envs, prod: { ...envs.prod!, deployed_at: "2026-09-02T11:00:00Z" } as Status["envs"]["prod"] }} />);
    await settle();
    expect(fetch.mock.calls.length).toBe(asked + 2);
    expect(screen.getByRole("note")).toBeDefined();
  });
});

// Reads can overlap: an older answer arriving after a newer one never replaces it.
describe("the Outbound card's reads", () => {
  it("keeps the newest read when an older one answers last", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let first: (body: ProjectEgress) => void = () => {};
    let reads = 0;
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const body = url === "/api/v1/front-door" ? { runtime: "docker" } : url === "/api/v1/egress" ? { via: "canyon", mode: "observe" }
        : reads++ === 0 ? await new Promise<ProjectEgress>((resolve) => { first = resolve; }) : { ...egress, floor: "not_enforced" };
      return { ok: true, status: 200, json: async () => body } as unknown as Response;
    }));
    render(<Outbound id="claims-portal" envs={envs} />);
    await act(async () => { await vi.advanceTimersByTimeAsync(30_000); });
    expect(screen.getByRole("note").textContent).toBe(FLOOR);
    await act(async () => { first({ ...egress, floor: "enforced" }); });
    await settle();
    expect(screen.getByRole("note").textContent).toBe(FLOOR);
  });
});

// An env's mode and its allowlist on the card (#2422; contracts/routes.md, the egress rows): an owner sets an env
// stricter than the org's or back to it, an admin either way, a contributor reads it; a contributor and up
// allows a destination the env's mode refuses. A switch that starts refusing lists what it would refuse, each
// with Allow, and asks in the page before anything is sent.
describe("the Outbound card's mode and Allow", () => {
  type Route = { status?: number; body?: unknown; pending?: string }; // pending: the answer's Canyon-Egress-Pending (#2789)
  // The reads answer `reads()` (a function, so a test moves what the app's read says); every PUT from `puts`.
  function host(reads: () => ProjectEgress, puts: Record<string, Route | (() => Route | Promise<Route>)> = {}, orgMode: "observe" | "allowlist" = "observe") {
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      const put = puts[url];
      const answer: Route = init?.method === "PUT" || init?.method === "DELETE" || init?.method === "POST" ? (await (typeof put === "function" ? put() : put)) ?? { status: 204 }
        : url === "/api/v1/front-door" ? { body: { runtime: "docker" } } : url === "/api/v1/egress" ? { body: { via: "canyon", mode: orgMode } } : { body: reads() };
      const status = answer.status ?? 200;
      return { ok: status < 300, status, json: async () => answer.body, headers: new Headers(answer.pending === undefined ? {} : { "Canyon-Egress-Pending": answer.pending }) } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    return () => fetch.mock.calls.filter(([, init]) => init?.method === "PUT" || init?.method === "DELETE" || init?.method === "POST").map(([url, init]) => `${init?.method} ${url}${init?.body === undefined ? "" : ` ${String(init.body)}`}`);
  }
  // In the golden, preview follows the org's observe and called registry.npmjs.org; production is its own allowlist.
  const radios = (name: string) => env(name).queryAllByRole("radio").map((one) => [one.parentElement?.textContent, (one as HTMLInputElement).checked]);

  it("says each env's mode and whose it is, and the env's allowlist", async () => {
    host(() => egress);
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Preview").getByText("Observe, as the org sets it")).toBeDefined();
    expect(env("Production").getByText("Allowlist, set for this app")).toBeDefined();
    expect(env("Production").getByText(/^Allowlist:/).textContent).toBe("Allowlist: *.slack.com Remove, api.stripe.com Remove");
    expect(env("Preview").queryByText(/^Allowlist:/)).toBeNull();
  });

  // The boundary (design/PERMISSIONS.md, Outbound, Mode): a contributor reads the mode and changes none.
  it("a contributor reads the mode, with no choice", async () => {
    const sent = host(() => egress);
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(screen.queryAllByRole("radio")).toEqual([]);
    expect(env("Preview").getByText(S.modeOwner)).toBeDefined();
    expect(env("Production").getByText(S.modeOwner)).toBeDefined();
    expect(sent()).toEqual([]);
  });

  // An owner's choices are the org's and allowlist, never observe, which is never stricter than the org's.
  it("an owner sets an env to allowlist: what it would refuse, each with Allow, the question, then {mode}", async () => {
    let read = egress; // the server takes the mode, and the card's read after the save says it
    const sent = host(() => read, { "/api/v1/projects/claims-portal/egress/preview/mode": () => {
      read = { ...egress, envs: { ...egress.envs, preview: { ...egress.envs.preview, mode: "allowlist", app_mode: "allowlist" } } };
      return { status: 204 };
    } });
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    expect(radios("Preview")).toEqual([["Same as the org: Observe", true], ["Allowlist", false]]);
    fireEvent.click(env("Preview").getByRole("radio", { name: "Allowlist" }));
    expect(env("Preview").getByText(S.modeNow)).toBeDefined();
    expect(env("Preview").getByRole("button", { name: "Allow registry.npmjs.org" })).toBeDefined();
    expect(env("Preview").queryByRole("button", { name: "Allow invalid" })).toBeNull(); // refused in every mode: no allowlist's to allow
    fireEvent.click(env("Preview").getByRole("button", { name: "Save Preview's mode" }));
    expect(sent()).toEqual([]);
    expect(env("Preview").getByText("Refuse 1 destination in Preview now? Canyon refuses its calls to it until it's allowed.")).toBeDefined();
    expect(document.activeElement).toBe(env("Preview").getByRole("button", { name: S.modeYes }));
    fireEvent.click(env("Preview").getByRole("button", { name: S.modeYes }));
    await settle();
    expect(sent()).toEqual(['PUT /api/v1/projects/claims-portal/egress/preview/mode {"mode":"allowlist"}']);
    // the card reads again once it is saved: the env's own mode, chosen, and nothing left to save
    expect(env("Preview").getByText("Allowlist, set for this app")).toBeDefined();
    expect(radios("Preview")).toEqual([["Same as the org: Observe", false], ["Allowlist", true]]);
    expect(env("Preview").queryByRole("button", { name: "Save Preview's mode" })).toBeNull();
    expect(env("Preview").queryByRole("button", { name: S.modeYes })).toBeNull();
  });

  it("Cancel asks nothing more and sends nothing", async () => {
    const sent = host(() => egress);
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    fireEvent.click(env("Preview").getByRole("radio", { name: "Allowlist" }));
    fireEvent.click(env("Preview").getByRole("button", { name: "Save Preview's mode" }));
    fireEvent.click(env("Preview").getByRole("button", { name: "Cancel" }));
    expect(env("Preview").queryByRole("button", { name: S.modeYes })).toBeNull();
    expect(sent()).toEqual([]);
  });

  // Back to the org's from a stricter mode refuses nothing more: saved without a question, as null.
  it("an owner sets an env back to the org's, with no question", async () => {
    const sent = host(() => egress);
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    expect(radios("Production")).toEqual([["Same as the org: Observe", false], ["Allowlist", true]]);
    fireEvent.click(env("Production").getByRole("radio", { name: "Same as the org: Observe" }));
    fireEvent.click(env("Production").getByRole("button", { name: "Save Production's mode" }));
    await settle();
    expect(sent()).toEqual(['PUT /api/v1/projects/claims-portal/egress/prod/mode {"mode":null}']);
  });

  // #2789: a save waits for the gateway, up to 30 s: while its answer is out, Save says Saving… and can't be clicked again.
  it("says Saving… while a mode's answer is out", async () => {
    let answer: (route: Route) => void = () => {};
    host(() => egress, { "/api/v1/projects/claims-portal/egress/prod/mode": () => new Promise<Route>((resolve) => { answer = resolve; }) });
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    fireEvent.click(env("Production").getByRole("radio", { name: "Same as the org: Observe" }));
    fireEvent.click(env("Production").getByRole("button", { name: "Save Production's mode" }));
    await settle();
    expect((env("Production").getByRole("button", { name: "Save Production's mode" }) as HTMLButtonElement).disabled).toBe(true);
    expect(env("Production").getByText("Saving…")).toBeDefined();
    await act(async () => { answer({ status: 204 }); });
    await settle();
    expect(env("Production").queryByText("Saving…")).toBeNull();
  });

  // An admin sets either way: observe too, looser than the org's allowlist; following the org's then refuses, so it asks.
  it("an admin gets observe while the org's is allowlist, and following it asks first", async () => {
    const own = { ...egress, envs: { ...egress.envs, preview: { ...egress.envs.preview, app_mode: "observe" as const } } };
    const sent = host(() => own, {}, "allowlist");
    render(<Outbound id="claims-portal" envs={envs} level="admin" />);
    await settle();
    expect(radios("Preview")).toEqual([["Same as the org: Allowlist", false], ["Allowlist", false], ["Observe", true]]);
    fireEvent.click(env("Preview").getByRole("radio", { name: "Same as the org: Allowlist" }));
    fireEvent.click(env("Preview").getByRole("button", { name: "Save Preview's mode" }));
    expect(sent()).toEqual([]);
    fireEvent.click(env("Preview").getByRole("button", { name: S.modeYes }));
    await settle();
    expect(sent()).toEqual(['PUT /api/v1/projects/claims-portal/egress/preview/mode {"mode":null}']);
  });

  // An env an admin set to observe shows it checked to its owner, who can't choose it: never a group with nothing checked.
  it("shows an owner the env's own observe, checked and off", async () => {
    const own = { ...egress, envs: { ...egress.envs, preview: { ...egress.envs.preview, app_mode: "observe" as const } } };
    host(() => own);
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    expect(radios("Preview")).toEqual([["Same as the org: Observe", false], ["Allowlist", false], ["Observe", true]]);
    expect((env("Preview").getByRole("radio", { name: "Observe" }) as HTMLInputElement).disabled).toBe(true);
    expect((env("Preview").getByRole("radio", { name: "Allowlist" }) as HTMLInputElement).disabled).toBe(false);
  });

  it("says a refused mode in the server's words, and the choice is the env's again", async () => {
    const message = "production is held: Stopped while we rotate the Stripe key";
    host(() => egress, { "/api/v1/projects/claims-portal/egress/prod/mode": { status: 409, body: { error: "held", message } } });
    render(<Outbound id="claims-portal" envs={envs} level="owner" />);
    await settle();
    fireEvent.click(env("Production").getByRole("radio", { name: "Same as the org: Observe" }));
    fireEvent.click(env("Production").getByRole("button", { name: "Save Production's mode" }));
    await settle();
    expect(env("Production").getByRole("alert").textContent).toBe(message);
    expect(radios("Production")).toEqual([["Same as the org: Observe", false], ["Allowlist", true]]);
  });

  // The mode decides only calls through Canyon: no choice for an env whose network doesn't send them through it.
  it("offers no choice while the floor is not enforced", async () => {
    host(() => ({ ...egress, floor: "not_enforced" }));
    render(<Outbound id="claims-portal" envs={envs} level="admin" />);
    await settle();
    expect(screen.queryAllByRole("radio")).toEqual([]);
  });

  // A contributor allows a destination the env's allowlist refuses: one entry, its name, and the card reads again.
  it("a contributor allows what production's allowlist refused, and the card reads again", async () => {
    let allowed = egress.envs.prod.allowed;
    const refusedRow = row({ destination: "api.openai.com", calls: 14, last_outcome: "refused", last_status_class: "4xx", last_response_flags: ["UAEX"] });
    const sent = host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, allowed, requests: [], destinations: [refusedRow, ...egress.envs.prod.destinations] } }, platform: [...egress.platform, "registry.npmjs.org"] }));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").getAllByRole("button", { name: /^Allow / }).map((one) => one.getAttribute("aria-label"))).toEqual(["Allow api.openai.com"]);
    allowed = [...allowed, "api.openai.com"];
    fireEvent.click(env("Production").getByRole("button", { name: "Allow api.openai.com" }));
    await settle();
    expect(sent()).toEqual(["PUT /api/v1/projects/claims-portal/egress/prod/destinations/api.openai.com"]);
    expect(env("Production").queryByRole("button", { name: "Allow api.openai.com" })).toBeNull();
    expect(env("Production").getByText("Allowed api.openai.com").closest('[aria-live="polite"]')).not.toBeNull();
  });

  // #2789: a Remove a gateway replica hadn't taken when the server stopped waiting is saved, and says so in place of
  // Removed <destination>, naming the replica.
  it("says a Remove a gateway replica hasn't taken yet, naming it", async () => {
    const path = "/api/v1/projects/claims-portal/egress/prod/destinations/api.stripe.com";
    host(() => egress, { [path]: { status: 204, pending: "canyon-egress-7d9f6c5b8-k2x4q" } });
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Remove api.stripe.com" }));
    await settle();
    expect(env("Production").getByText(S.gatewayPending.replace("<replica>", "canyon-egress-7d9f6c5b8-k2x4q"))).toBeDefined();
    expect(env("Production").queryByText("Removed api.stripe.com")).toBeNull();
  });

  // design/EGRESS.md §3.4, §8.3; D15: what Canyon refuses now (`refusing`) opens the env under Needs a person, with Allow
  // and Dismiss, before its allowlist and mode; dismissed requests follow under Dismissed, with Allow alone, a refusal
  // count each; Allow beside a request is the one Allow of that destination.
  it("opens an env with what needs a person, and lists the dismissed requests after, with Allow", async () => {
    host(() => egress);
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    const rows = (heading: string) => within(env("Production").getByText(heading).parentElement!).getAllByRole("listitem")
      .map((item) => [item.querySelector(".font-mono")?.textContent, ...within(item).getAllByRole("button").map((one) => one.textContent)]);
    expect(rows(S.needsAPerson)).toEqual([["api.openai.com", "Allow", "Dismiss"]]);
    expect(rows(S.dismissedHeading)).toEqual([["hooks.zapier.com", "Allow"]]);
    expect(env("Production").getByText(S.needsAPerson).parentElement!.textContent).toMatch(/Not allowed 14 times, last \d+ (day|month)s? ago/);
    expect(env("Production").getByText(S.dismissedHeading).parentElement!.textContent).toMatch(/Not allowed 2 times, last \d+ (day|month)s? ago · Dismissed/);
    expect(env("Production").getByText(S.needsAPerson).compareDocumentPosition(env("Production").getByText(S.modeOwner)) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(env("Production").getAllByRole("button", { name: "Allow api.openai.com" })).toHaveLength(1);
    expect(env("Preview").queryByText(S.needsAPerson)).toBeNull();
  });

  // A pending request the read doesn't mark refusing is no need: never in the group, its destination's row still listed.
  it("lists only what Canyon refuses now under Needs a person", async () => {
    const quiet = { ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod,
      requests: egress.envs.prod.requests.map((one) => (one.destination === "api.openai.com" ? { ...one, refusing: false } : one)),
      destinations: [...egress.envs.prod.destinations, { ...egress.envs.prod.destinations[0]!, destination: "api.openai.com", last_outcome: "allowed" as const }] } } };
    host(() => quiet);
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").queryByText(S.needsAPerson)).toBeNull();
    expect(env("Production").getAllByText(/^api\.openai\.com:443$/).length).toBeGreaterThan(0);
    // shown in no group, its row keeps the Allow every other row has (#2762's review)
    expect(env("Production").getByRole("button", { name: "Allow api.openai.com" })).toBeDefined();
  });

  // Who changes an env's mode (design/PERMISSIONS.md, Outbound): a contributor is told whom to ask, by name; an owner's own
  // allowlist under the org's observe says who changes it and what going back would let through.
  it("names the owner a contributor asks, and says what going back to the org's lets through", async () => {
    const own = { ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, mode: "allowlist" as const, app_mode: "allowlist" as const } } };
    host(() => own);
    const { unmount } = render(<Outbound id="claims-portal" envs={envs} level="contributor" owner={{ name: "Dana Whitfield" }} />);
    await settle();
    expect(env("Production").getByText("Only an owner or an admin can change this environment's mode. Ask Dana Whitfield, its owner.")).toBeDefined();
    expect(env("Production").getByText(S.modeSwitchBack)).toBeDefined(); // the mode line under it says who changes it
    expect(env("Preview").queryByText(S.modeSwitchBack)).toBeNull();
    unmount();
    // No single owner: the group that owns it, as Who looks after it says; an owner reads only what switching back does.
    host(() => own);
    const { unmount: gone } = render(<Outbound id="claims-portal" envs={envs} level="contributor" owner={{ group: "claims-team" }} />);
    await settle();
    expect(env("Production").getByText(`${S.modeOwner} Ask a member of the claims-team group, which owns it.`)).toBeDefined();
    gone();
    // Whoever can change the mode (an owner, an admin: the mode controls' own check) reads the switching line alone, its
    // choices under it; only one who can't is told who can, in the mode line.
    for (const level of ["owner", "admin"] as const) {
      host(() => own);
      const { unmount: done } = render(<Outbound id="claims-portal" envs={envs} level={level} owner={{ name: "Dana Whitfield" }} />);
      await settle();
      expect(env("Production").getByText(S.modeSwitchBack)).toBeDefined();
      expect(env("Production").queryByText(/Only an owner or an admin can change/)).toBeNull();
      expect(env("Production").getByRole("radio", { name: S.modeAllowlist })).toBeDefined();
      done();
    }
    // Fail closed: a reader below contributor, who never reaches the card today, would get no controls, only the mode line.
    for (const level of ["viewer", "responder"] as const) {
      host(() => own);
      const { unmount: done } = render(<Outbound id="claims-portal" envs={envs} level={level} owner={{ name: "Dana Whitfield" }} />);
      await settle();
      expect(env("Production").queryByRole("radio")).toBeNull();
      expect(env("Production").getByText(`${S.modeOwner} Ask Dana Whitfield, its owner.`)).toBeDefined();
      done();
    }
  });

  it("says one refusal once", async () => {
    const one = { destination: "api.openai.com", state: "pending" as const, first_refused_at: "2026-10-11T10:00:00Z", last_refused_at: "2026-10-11T10:00:00Z", refusals: 1, refusing: true };
    host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, requests: [one] } } }));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").getByText(/^Not allowed once, /)).not.toBeNull();
  });

  // Dismiss is a person's, from any session (design/PERMISSIONS.md, Outbound): sent with no question, the card reads
  // again and says it politely, and the destination stays, dismissed, with its Allow.
  it("a contributor dismisses a request, and the card reads again", async () => {
    let state: "pending" | "dismissed" = "pending";
    const read = () => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, requests: [{ ...egress.envs.prod.requests[0]!, state }] } } });
    const sent = host(read);
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    state = "dismissed";
    fireEvent.click(env("Production").getByRole("button", { name: "Dismiss api.openai.com" }));
    await settle();
    expect(sent()).toEqual(["POST /api/v1/projects/claims-portal/egress/prod/requests/api.openai.com/dismiss"]);
    expect(env("Production").queryByRole("button", { name: "Dismiss api.openai.com" })).toBeNull();
    expect(env("Production").getByRole("button", { name: "Allow api.openai.com" })).not.toBeNull();
    expect(env("Production").getByText("Dismissed api.openai.com").closest('[aria-live="polite"]')).not.toBeNull();
  });

  it("says a refused Dismiss in the server's words, and keeps the request", async () => {
    const path = "/api/v1/projects/claims-portal/egress/prod/requests/api.openai.com/dismiss";
    host(() => egress, { [path]: { status: 403, body: { error: "person_required", message: "a person changes how apps call out" } } });
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Dismiss api.openai.com" }));
    await settle();
    expect(env("Production").getByRole("alert").textContent).toBe("a person changes how apps call out");
    expect(env("Production").getByRole("button", { name: "Dismiss api.openai.com" })).not.toBeNull();
  });

  // A held production refuses a widening: an Allow beside a request says so in the server's words.
  it("says a held production's refusal of an Allow", async () => {
    const path = "/api/v1/projects/claims-portal/egress/prod/destinations/api.openai.com";
    host(() => egress, { [path]: { status: 409, body: { error: "held", message: "Production is held: the incident" } } });
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Allow api.openai.com" }));
    await settle();
    expect(env("Production").getByRole("alert").textContent).toBe("Production is held: the incident");
  });

  // The receipt's production does not allow <destinations> needs an answer before production ever refused one: under
  // production's allowlist the card lists what preview connected to that it doesn't hold, a name once, each with Allow.
  // Never what no entry can allow, what production allows already or has a request of, and nothing under observe.
  it("lists what preview connects to that production doesn't allow, with Allow", async () => {
    const preview = ["api.stripe.com", "api.openai.com", "new.example.com", "login.example-idp.com", "invalid", "10.0.4.7"].map((destination) => row({ destination }));
    const read = (mode: "observe" | "allowlist") => ({ ...egress, envs: { ...egress.envs, preview: { ...egress.envs.preview, destinations: [...preview, row({ destination: "new.example.com", port: 80 }), row({ destination: "elsewhere.example.com", port: 8443 })] }, prod: { ...egress.envs.prod, mode } } });
    let mode: "observe" | "allowlist" = "allowlist";
    const sent = host(() => read(mode));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    const list = within(env("Production").getByText(S.prodRefuses).parentElement!);
    expect(list.getAllByRole("listitem").map((item) => item.querySelector(".font-mono")?.textContent)).toEqual(["new.example.com"]);
    fireEvent.click(list.getByRole("button", { name: "Allow new.example.com in production" }));
    await settle();
    expect(sent()).toEqual(["PUT /api/v1/projects/claims-portal/egress/prod/destinations/new.example.com"]);
    cleanup();
    mode = "observe";
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").queryByText(S.prodRefuses)).toBeNull();
  });

  // A first promotion meets the same line with nothing in production yet: the section is drawn for it, and for nothing else.
  it("lists it before anything runs in production", async () => {
    const { prod: _, ...noProd } = envs;
    const sent = host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, destinations: [], requests: [] } } }));
    render(<Outbound id="claims-portal" envs={noProd} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Allow registry.npmjs.org in production" }));
    await settle();
    expect(sent()).toEqual(["PUT /api/v1/projects/claims-portal/egress/prod/destinations/registry.npmjs.org"]);
    cleanup();
    host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, mode: "observe" } } }));
    render(<Outbound id="claims-portal" envs={noProd} level="contributor" />);
    await settle();
    expect(screen.queryByRole("region", { name: "Production outbound" })).toBeNull();
  });

  // Remove, a narrowing (design/PERMISSIONS.md, Outbound: a contributor's and up, from any session): each entry of
  // an env's allowlist has one, sent with no question; the card reads again and says it politely (#2561). Below
  // contributor the records answer team_required and there is no card, so no Remove (above).
  it("a contributor removes an allowlist entry, and the card reads again", async () => {
    let allowed = egress.envs.prod.allowed;
    const sent = host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, allowed } } }));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").getAllByRole("button", { name: /^Remove (?!the credential)/ }).map((one) => one.getAttribute("aria-label"))).toEqual(["Remove *.slack.com", "Remove api.stripe.com"]);
    expect(env("Preview").queryByRole("button", { name: /^Remove (?!the credential)/ })).toBeNull();
    allowed = ["*.slack.com"];
    fireEvent.click(env("Production").getByRole("button", { name: "Remove api.stripe.com" }));
    await settle();
    expect(sent()).toEqual(["DELETE /api/v1/projects/claims-portal/egress/prod/destinations/api.stripe.com"]);
    expect(env("Production").getAllByRole("button", { name: /^Remove (?!the credential)/ }).map((one) => one.getAttribute("aria-label"))).toEqual(["Remove *.slack.com"]);
    expect(env("Production").getByText("Removed api.stripe.com").closest('[aria-live="polite"]')).not.toBeNull();
  });

  it("says a refused Remove in the server's words", async () => {
    host(() => egress, { "/api/v1/projects/claims-portal/egress/prod/destinations/api.stripe.com": { status: 500, body: { error: "internal", message: "internal error" } } });
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Remove api.stripe.com" }));
    await settle();
    expect(env("Production").getByRole("alert").textContent).toBe("internal error");
  });

  // On a phone there is no table to scroll sideways: each destination is a small stack, nothing clipped —
  // the destination, then when it was last seen, its calls and its last result, with Allow.
  it("on a phone lists each destination as a stack, Allow in it, nothing off to the side", async () => {
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: () => {}, removeEventListener: () => {} }));
    const refusedRow = row({ destination: "api.openai.com", last_outcome: "refused" });
    host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, requests: [], destinations: [refusedRow] } } }));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(env("Production").queryByRole("table")).toBeNull();
    const item = env("Production").getByRole("button", { name: "Allow api.openai.com" }).closest("li") as HTMLLIElement;
    expect(item.textContent).toMatch(/^api\.openai\.com:443.+ ago · 1,840 calls · refused/);
  });

  it("says a refused Allow in the server's words", async () => {
    const message = "production is held: Stopped while we rotate the Stripe key";
    const refusedRow = row({ destination: "api.openai.com", last_outcome: "refused" });
    host(() => ({ ...egress, envs: { ...egress.envs, prod: { ...egress.envs.prod, destinations: [refusedRow] } } }),
      { "/api/v1/projects/claims-portal/egress/prod/destinations/api.openai.com": { status: 409, body: { error: "held", message } } });
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    fireEvent.click(env("Production").getByRole("button", { name: "Allow api.openai.com" }));
    await settle();
    expect(env("Production").getByRole("alert").textContent).toBe(message);
  });

  // Fail closed (#2110): a mode this build does not know, or none, is not read — never observe.
  it("says requests out of shape couldn't be read", async () => {
    const { requests: _, ...prod } = egress.envs.prod;
    host(() => ({ ...egress, envs: { ...egress.envs, prod: prod as ProjectEgress["envs"]["prod"] } }));
    render(<Outbound id="claims-portal" envs={envs} level="contributor" />);
    await settle();
    expect(screen.getByText("Outbound couldn't be read.")).not.toBeNull();
  });

  it("says a mode out of shape couldn't be read", async () => {
    for (const prod of [{ ...egress.envs.prod, mode: "open" }, { ...egress.envs.prod, mode: undefined }, { ...egress.envs.prod, allowed: undefined }, { ...egress.envs.prod, app_mode: "off" }]) {
      host(() => ({ ...egress, envs: { ...egress.envs, prod } }) as unknown as ProjectEgress);
      render(<Outbound id="claims-portal" envs={envs} level="owner" />);
      await settle();
      expect(screen.getByText("Outbound couldn't be read.")).toBeDefined();
      cleanup();
    }
  });
});

