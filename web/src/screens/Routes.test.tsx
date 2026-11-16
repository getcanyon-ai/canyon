import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { EgressEnv, LicenceState, ProjectEgress, Status } from "../api";
import { S } from "../strings";
import { Outbound } from "./Outbound";
import { Routes } from "./Routes";

const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as { project_read: ProjectEgress };
const { preview, prod } = golden.project_read.envs;
// A credential's shape, never one (AGENTS.md, Fixtures), built here so no file holds it.
const VALUE = "sk_live_" + "e".repeat(24);

type Answer = { status: number; body?: unknown; pending?: string };
// The server: each call answered from `answers` by method and path, 404 otherwise; it returns the calls made.
function server(answers: Record<string, Answer> = {}) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const answer = answers[`${init?.method ?? "GET"} ${url}`] ?? { status: 404, body: { error: "not_found", message: "not found" } };
    return { ok: answer.status < 300, status: answer.status, json: async () => answer.body, headers: new Headers(answer.pending === undefined ? {} : { "Canyon-Egress-Pending": answer.pending }) } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([, init]) => init?.method !== undefined && init.method !== "GET").map(([url, init]) => [init?.method, url, init?.body === undefined ? undefined : JSON.parse(String(init.body)) as unknown]);
}
// A licence in force that unlocks credentials (#2879), and one that lapsed.
const UNLOCKED: LicenceState = { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] };
const LAPSED: LicenceState = { state: "lapsed", allows: [] };
const settle = () => act(() => new Promise((resolve) => setTimeout(resolve, 0)));
const card = (env: EgressEnv, level: "admin" | "owner" | "contributor" | "viewer" | "responder", saved = vi.fn(async () => {}), licence: LicenceState | null = UNLOCKED) => {
  render(<Routes id="claims-portal" env={env === prod ? "prod" : "preview"} name={env === prod ? "Production" : "Preview"} read={env} level={level} licence={licence} saved={saved} />);
  return saved;
};
const routeRow = (destination: string) => within(screen.getByRole("listitem", { name: destination }));
const PREVIEW_ROUTE = "/api/v1/projects/claims-portal/egress/preview/routes/api.stripe.com";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("an env's credentials", () => {
  // design/EGRESS.md §8.3: routes, never a value; each says what Canyon does with it and why it waits when it does.
  it("lists each route as the read says it, and why Canyon isn't adding one yet", () => {
    server();
    card(prod, "owner");
    const stripe = routeRow("api.stripe.com");
    expect(stripe.getByText("authorization · Bearer token")).toBeDefined();
    expect(stripe.getByText(S.routeFromSecret)).toBeDefined();
    expect(stripe.getByText("The app gets canyon-adds-the-real-key in STRIPE_SECRET_KEY.")).toBeDefined();
    expect(routeRow("hooks.slack.com").getByText("x-slack-token · As written")).toBeDefined();
    cleanup();
    card(preview, "contributor");
    expect(routeRow("api.stripe.com").getByText(S.routeWaitRedeploy)).toBeDefined();
    cleanup();
    card({ ...preview, routes: [] } as EgressEnv, "contributor");
    expect(screen.getByText(S.routesNone)).toBeDefined();
  });

  it("says nothing while the read names no routes, as a server from before them answers", () => {
    server();
    const { routes: _, ...before } = preview as EgressEnv & { routes?: unknown };
    card(before as EgressEnv, "admin");
    expect(screen.queryByRole("region")).toBeNull();
  });

  // #2145's acceptance: the route form never echoes a value. The value goes to the server once, in the PUT's body, and
  // is then nowhere: not in the page, not in a field, not in a log, and not in the form when it is opened again.
  it("never echoes a credential: it reaches the server and nothing else", async () => {
    const calls = server({ [`PUT ${PREVIEW_ROUTE}`]: { status: 204 } });
    const logged = (["log", "info", "warn", "error", "debug"] as const).map((level) => vi.spyOn(console, level));
    card({ ...preview, allowed: ["api.stripe.com", "*.example.com"] }, "contributor");
    fireEvent.click(screen.getByRole("button", { name: "Add a credential" }));
    const destination = screen.getByLabelText("Destination") as HTMLInputElement;
    // a typed host, the allowlist's own names suggested: a route names one host, never a *. entry
    expect([...document.getElementById(destination.getAttribute("list") ?? "")!.querySelectorAll("option")].map((one) => one.value)).toEqual(["api.stripe.com"]);
    expect(destination.value).toBe("api.stripe.com");
    const field = screen.getByLabelText("Credential");
    // no browser keeps it or offers one of its own: no password manager's save prompt, no generated password
    expect([field.getAttribute("autocomplete"), field.hasAttribute("data-1p-ignore"), field.getAttribute("data-lpignore")]).toEqual(["off", true, "true"]);
    fireEvent.change(field, { target: { value: VALUE } });
    expect(document.body.innerHTML).not.toContain(VALUE); // typed: in the field's value alone, never an attribute
    fireEvent.click(screen.getByRole("button", { name: "Save the credential" }));
    const saved = await screen.findByRole("status");
    expect(calls()).toEqual([["PUT", PREVIEW_ROUTE, { header: "Authorization", scheme: "bearer", value: VALUE }]]);
    expect(document.body.innerHTML).not.toContain(VALUE);
    expect(saved.textContent).toBe("Saved. Canyon adds it to Preview's calls to api.stripe.com.");
    for (const input of document.querySelectorAll("input, textarea")) expect((input as HTMLInputElement).value).not.toContain(VALUE);
    for (const spy of logged) expect(JSON.stringify(spy.mock.calls)).not.toContain(VALUE);
    fireEvent.click(within(screen.getByRole("listitem", { name: "api.stripe.com" })).getByRole("button", { name: "Replace the credential for api.stripe.com" }));
    expect((screen.getByLabelText("Credential") as HTMLInputElement).value).toBe(""); // the form holds none of it
  });

  // A refusal is said in the server's words and the fields stay to fix: a header typo shouldn't cost the person the key.
  it("says the server's refusal and keeps what was typed", async () => {
    server({ [`PUT ${PREVIEW_ROUTE}`]: { status: 400, body: { error: "invalid_route", field: "header", message: "the header is none Canyon may set" } } });
    card({ ...preview, allowed: ["api.stripe.com"] }, "contributor");
    fireEvent.click(screen.getByRole("button", { name: "Add a credential" }));
    fireEvent.change(screen.getByLabelText("Header"), { target: { value: "Host" } });
    fireEvent.change(screen.getByLabelText("Credential"), { target: { value: VALUE } });
    fireEvent.click(screen.getByRole("button", { name: "Save the credential" }));
    expect((await screen.findByRole("alert")).textContent).toBe("the header is none Canyon may set");
    expect((screen.getByLabelText("Header") as HTMLInputElement).value).toBe("Host");
    expect(document.body.innerHTML).not.toContain(VALUE);
  });

  // Which destinations a route may name is the server's to judge (contracts/routes.md, the routes row): any name under
  // observe, one a *. entry covers, a platform destination. Only an allowlist with nothing on it can take none.
  it("asks for an allowed name only where an allowlist holds none", () => {
    server();
    card({ ...prod, allowed: [], routes: [] } as EgressEnv, "contributor");
    fireEvent.click(screen.getByRole("button", { name: "Add a credential" }));
    expect(screen.getByText(S.routeNoDestination)).toBeDefined();
    expect(screen.queryByLabelText("Credential")).toBeNull();
  });

  it("takes a typed host the server judges: any under observe, one a *. entry covers, its refusal in its words", async () => {
    const calls = server({
      "PUT /api/v1/projects/claims-portal/egress/preview/routes/api.openai.com": { status: 204 },
      "PUT /api/v1/projects/claims-portal/egress/prod/routes/hooks.slack.com": { status: 204 },
      "PUT /api/v1/projects/claims-portal/egress/prod/routes/api.openai.com": { status: 409, body: { error: "destination_not_allowed", message: "allow api.openai.com before you add a credential for it" } },
    });
    const add = async (destination: string) => {
      fireEvent.click(screen.getByRole("button", { name: "Add a credential" }));
      fireEvent.change(screen.getByLabelText("Destination"), { target: { value: destination } });
      fireEvent.change(screen.getByLabelText("Credential"), { target: { value: VALUE } });
      fireEvent.click(screen.getByRole("button", { name: "Save the credential" }));
      await settle();
    };
    card({ ...preview, routes: [] } as EgressEnv, "contributor"); // observe, nothing on its allowlist
    await add("api.openai.com");
    cleanup();
    render(<Routes id="claims-portal" env="prod" name="Production" read={{ ...prod, routes: [] } as EgressEnv} level="contributor" licence={UNLOCKED} saved={async () => {}} />); // allowlist: *.slack.com and api.stripe.com
    await add("hooks.slack.com");
    fireEvent.click(screen.getByRole("button", { name: "Add a credential" }));
    fireEvent.change(screen.getByLabelText("Destination"), { target: { value: "api.openai.com" } });
    fireEvent.change(screen.getByLabelText("Credential"), { target: { value: VALUE } });
    fireEvent.click(screen.getByRole("button", { name: "Save the credential" }));
    expect((await screen.findByRole("alert")).textContent).toBe("allow api.openai.com before you add a credential for it");
    expect(calls().map(([method, url]) => `${method} ${url}`)).toEqual([
      "PUT /api/v1/projects/claims-portal/egress/preview/routes/api.openai.com",
      "PUT /api/v1/projects/claims-portal/egress/prod/routes/hooks.slack.com",
      "PUT /api/v1/projects/claims-portal/egress/prod/routes/api.openai.com",
    ]);
  });

  it("removes a route and says so, or what gateway hasn't taken it yet", async () => {
    const calls = server({ [`DELETE ${PREVIEW_ROUTE}`]: { status: 204, pending: "egress-0" } });
    const saved = card(preview, "contributor");
    fireEvent.click(screen.getByRole("button", { name: "Remove the credential for api.stripe.com" }));
    await screen.findByText(/one of Canyon's gateways \(egress-0\)/);
    expect(calls()).toEqual([["DELETE", PREVIEW_ROUTE, undefined]]);
    expect(saved).toHaveBeenCalled();
  });
});

describe("the Outbound card", () => {
  it("shows each env's credentials under its destinations", async () => {
    server({
      [`GET /api/v1/projects/claims-portal/egress`]: { status: 200, body: golden.project_read },
      "GET /api/v1/egress": { status: 200, body: { via: "canyon", mode: "observe" } },
      "GET /api/v1/front-door": { status: 200, body: { runtime: "docker", default_domain: "localhost", front_door: null, warning: "", hand_off_needs: [] } },
    });
    const page = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: Status; promotion: { promotion: NonNullable<Status["envs"]["prod"]> } };
    render(<Outbound id="claims-portal" envs={{ ...page.status.envs, prod: page.promotion.promotion }} level="contributor" />);
    await settle();
    expect(within(screen.getByRole("region", { name: "Preview credentials" })).getByRole("listitem", { name: "api.stripe.com" })).toBeDefined();
    expect(within(screen.getByRole("region", { name: "Production credentials" })).getAllByRole("listitem").map((one) => one.getAttribute("aria-label"))).toEqual(["api.stripe.com", "hooks.slack.com"]);
  });

  // contracts/routes.md, waiting: host holds while the host's via is off, its credential_routes false or the org runs the
  // apps. Every member reads credential_routes (the egress setting row), so the card says which: an admin's to turn on,
  // or apps that don't call out through Canyon.
  it("says why a route waits on the host: credentials off, or calls that skip Canyon", async () => {
    const page = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: Status; promotion: { promotion: NonNullable<Status["envs"]["prod"]> } };
    const waits: ProjectEgress = { ...golden.project_read, envs: { ...golden.project_read.envs, preview: { ...preview, routes: [{ destination: "api.stripe.com", header: "authorization", scheme: "bearer", placeholder_env: null, from_secret: false, waiting: { reason: "host", secret: null } }] } as EgressEnv } };
    for (const [credentials, said, not] of [
      [false, "Canyon isn't adding it: an admin turns on credentials in Settings › Outbound.", "Canyon isn't adding it: apps here don't call out through Canyon."],
      [true, "Canyon isn't adding it: apps here don't call out through Canyon.", "Canyon isn't adding it: an admin turns on credentials in Settings › Outbound."],
    ] as const) {
      server({
        [`GET /api/v1/projects/claims-portal/egress`]: { status: 200, body: waits },
        "GET /api/v1/egress": { status: 200, body: { via: "canyon", mode: "observe", credential_routes: credentials } },
        "GET /api/v1/front-door": { status: 200, body: { runtime: "docker", default_domain: "localhost", front_door: null, warning: "", hand_off_needs: [] } },
      });
      render(<Outbound id="claims-portal" envs={{ ...page.status.envs, prod: page.promotion.promotion }} level="contributor" />);
      await settle();
      const route = within(within(screen.getByRole("region", { name: "Preview credentials" })).getByRole("listitem", { name: "api.stripe.com" }));
      expect(route.getByText(said), `credential_routes ${credentials}`).toBeDefined();
      expect(route.queryByText(not), `credential_routes ${credentials}`).toBeNull();
      cleanup();
    }
  });
});

// design/EGRESS.md §3.5 and contracts/routes.md's routes rows: one allowed and one refused caller at each boundary.
describe("who changes a credential", () => {
  // Adding, replacing and removing a route: a contributor and up. A read-only member's card has no form.
  it("lets a contributor add and remove a route, and a read-only member neither", () => {
    server();
    card(preview, "contributor");
    expect(screen.getByRole("button", { name: "Add a credential" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Remove the credential for api.stripe.com" })).toBeDefined();
    cleanup();
    card(preview, "viewer");
    expect(screen.queryByRole("button")).toBeNull();
    expect(routeRow("api.stripe.com").getByText("authorization · Bearer token")).toBeDefined();
  });

  // A route a secret placed: its owner's or an admin's to replace or remove; a contributor is told who.
  it("lets an owner change a route a secret placed, and tells a contributor only an owner can", () => {
    server();
    card(prod, "owner");
    expect(routeRow("api.stripe.com").getByRole("button", { name: "Replace the credential for api.stripe.com" })).toBeDefined();
    expect(routeRow("api.stripe.com").queryByText(S.routeOwnerOnly)).toBeNull();
    cleanup();
    card(prod, "contributor");
    expect(routeRow("api.stripe.com").queryByRole("button")).toBeNull();
    expect(routeRow("api.stripe.com").getByText(S.routeOwnerOnly)).toBeDefined();
    expect(routeRow("hooks.slack.com").getByRole("button", { name: "Replace the credential for hooks.slack.com" })).toBeDefined(); // one no secret placed
  });

  // Moving a secret into a route: an owner's or an admin's.
  it("offers an owner to move a secret, by its name, and a contributor not", async () => {
    const calls = server({
      "GET /api/v1/projects/claims-portal/secrets": { status: 200, body: { secrets: [
        { name: "STRIPE_SECRET_KEY", env: "preview", version: 1, set_by: { kind: "person", id: "u" }, set_at: "2026-11-01T00:00:00Z", origin: "cli", where: "runtime" },
        { name: "NPM_TOKEN", env: "preview", version: 1, set_by: { kind: "person", id: "u" }, set_at: "2026-11-01T00:00:00Z", origin: "cli", where: "build" },
        { name: "PROD_ONLY", env: "prod", version: 1, set_by: { kind: "person", id: "u" }, set_at: "2026-11-01T00:00:00Z", origin: "cli", where: "runtime" },
      ] } },
      [`POST ${PREVIEW_ROUTE}/from-secret`]: { status: 204 },
    });
    card({ ...preview, allowed: ["api.stripe.com"] }, "owner");
    fireEvent.click(screen.getByRole("button", { name: "Move a secret into a credential" }));
    await settle();
    expect([...screen.getByLabelText("Secret").querySelectorAll("option")].map((one) => one.textContent)).toEqual(["STRIPE_SECRET_KEY"]); // preview's runtime secrets alone
    fireEvent.click(screen.getByRole("button", { name: "Move the secret" }));
    await screen.findByText("Moved STRIPE_SECRET_KEY into a credential for api.stripe.com.");
    expect(calls()).toEqual([["POST", `${PREVIEW_ROUTE}/from-secret`, { secret: "STRIPE_SECRET_KEY", header: "Authorization", scheme: "bearer" }]]);
    cleanup();
    card({ ...preview, allowed: ["api.stripe.com"] }, "contributor");
    expect(screen.queryByRole("button", { name: "Move a secret into a credential" })).toBeNull();
  });
});

// Adding or changing a credential is Canyon Enterprise's (contracts/routes.md, Licence; #2879): without a licence that
// unlocks it, the badge and the server's words stand in place of Add, Replace and Move; Remove stays, and what is set is
// still listed.
describe("a credential without Canyon Enterprise", () => {
  it("offers no add or move, says why, and keeps Replace and Remove: a leaked key can always be rotated", () => {
    server();
    card(prod, "owner", vi.fn(async () => {}), LAPSED);
    expect(screen.getByText(S.licenceRequiredOutbound)).toBeDefined();
    expect(screen.getByText(S.enterprise)).toBeDefined();
    expect(screen.queryByRole("button", { name: "Add a credential" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Move a secret into a credential" })).toBeNull();
    expect(routeRow("api.stripe.com").getByRole("button", { name: "Replace the credential for api.stripe.com" })).toBeDefined();
    expect(routeRow("api.stripe.com").getByRole("button", { name: "Remove the credential for api.stripe.com" })).toBeDefined();
    expect(routeRow("api.stripe.com").getByText("authorization · Bearer token")).toBeDefined();
    cleanup();
    card(prod, "owner");
    expect(screen.queryByText(S.licenceRequiredOutbound)).toBeNull();
    expect(screen.getByRole("button", { name: "Add a credential" })).toBeDefined();
  });

  it("offers nothing to add before the licence is read, and says nothing either", () => {
    server();
    card(prod, "owner", vi.fn(async () => {}), null);
    expect(screen.queryByRole("button", { name: "Add a credential" })).toBeNull();
    expect(screen.queryByText(S.licenceRequiredOutbound)).toBeNull();
  });
});
