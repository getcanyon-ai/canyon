import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { EgressSetting, FrontDoorRead, Preferences, ProjectEgress } from "../api";
import { S } from "../strings";
import { Settings } from "./Settings";

// The contract's own front door: its PUT is what the form sends, its GET what the server answers.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/front-door/meridian.json"), "utf8")) as {
  put: unknown; refusal: { error: string; message: string }; get: FrontDoorRead; get_unprobed: FrontDoorRead;
};
const prefs = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences; preferences_member: Preferences };
const unset: FrontDoorRead = { ...golden.get, front_door: null, warning: "" };
const ARN = "arn:aws:elasticloadbalancing:eu-central-1:123456789012:loadbalancer/app/canyon/0123456789abcdef";
// The probe's switch sentence (contracts/executor.md, POST /probe) and D5's probe_unavailable message.
const NETWORK_POLICY = 'This cluster does not enforce NetworkPolicy, so an app\'s port could be reached without signing in. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on) and save again.';
const UNAVAILABLE = "Canyon could not finish checking this cluster. Try again in a minute; if it keeps failing, Canyon's log says why.";
// D5's lines on whether a grant to a person applies through the front door, which the server answers (#2299).
const OTHER_ISSUER = "People sign in at this front door through another issuer than Canyon's, so an app shared with a person won't let them in. Sharing with a group or with everyone still does.";
const ALB_ONLY = "An app shared with a person lets them in only if the load balancer signs people in through Canyon's own sign-in. Sharing with a group or with everyone works either way.";
// D5's gateway-sign-in lines (design/EXPERIENCE.md; #2316): the front door's egress_note, and PUT canyon's 409 there.
const DISCOVERY_NOTE = "Canyon couldn't read your sign-in provider's discovery document yet and keeps trying every minute; until it does, apps whose outbound mode is allowlist can't sign people in.";
const GATEWAY_NOTE = "Apps routed through Canyon can't reach a sign-in served through this cluster's own gateway, so nobody can sign in to them. Use another sign-in provider here, or turn off Through Canyon in Settings › Outbound.";
const GATEWAY_REFUSED = "Apps routed through Canyon can't reach a sign-in served through this cluster's own gateway, so nobody could sign in to them. Set the front door to another sign-in provider, or leave this off.";

// The server answers the reads from what the test hands it and every PUT from `put`, which a test
// holds open to look at the screen while the probe runs. It returns the PUT bodies sent.
type Answer = { status?: number; body?: unknown; pending?: string }; // pending: the answer's Canyon-Egress-Pending (#2789)
function server(door: FrontDoorRead, put: Answer | (() => Promise<Answer>) = { status: 404 }, record = prefs.preferences_admin, auth: "oidc" | "off" = "oidc") {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const { status = 200, body } = url === "/auth/config" ? { body: { canyon: true, auth } } : url === "/api/v1/preferences" ? { body: record } : url === "/api/v1/platform-webhook" || url === "/api/v1/platform-repo" ? { body: null }
      : url === "/api/v1/patching/identities" ? { status: 404, body: { error: "not_found" } } : init?.method === "PUT" ? (typeof put === "function" ? await put() : put) : { body: door };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([url, init]) => url === "/api/v1/front-door" && init?.method === "PUT").map(([, init]) => JSON.parse(String(init?.body)) as unknown);
}
const save = () => screen.getByRole("button", { name: "Save the front door" });
const type = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });

afterEach(() => { vi.unstubAllGlobals(); window.location.hash = ""; });

// The four pages of Settings, one at a time, each its own address: the bare route is Organisation.
describe("Settings' pages", () => {
  // A member who opens Settings › Integrations by its address reads whose it is, and Canyon asks nothing of the channels.
  it("tells a member Integrations is an admin's", async () => {
    window.location.hash = "#/settings/integrations";
    server(golden.get, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.integrationsAdmin)).toBeDefined();
    expect(vi.mocked(fetch).mock.calls.map(([url]) => String(url))).not.toContain("/api/v1/integrations/channels");
  });

  it("opens on Organisation and names the pages, the one shown marked, GitHub an admin's alone", async () => {
    window.location.hash = "#/settings";
    server(golden.get);
    const { unmount } = render(<Settings />);
    expect(await screen.findByLabelText("Organisation name")).toBeDefined();
    const pages = within(screen.getByRole("navigation", { name: "Settings" })).getAllByRole("link");
    expect(pages.map((one) => [one.firstChild?.textContent, one.getAttribute("href")])).toEqual([
      ["Organisation", "#/settings/organisation"], ["People & sign-in", "#/settings/people"], ["Agents", "#/settings/agents"], ["App hosting", "#/settings/hosting"], ["AI review", "#/settings/ai"],
      ["GitHub", "#/settings/github"], ["Outbound", "#/settings/outbound"], ["Integrations", "#/settings/integrations"], ["Telemetry", "#/settings/telemetry"],
      ["Licence", "#/settings/licence"], ["Databases", "#/settings/databases"],
    ]);
    expect(pages[0]!.getAttribute("aria-current")).toBe("page");
    unmount();
    server(golden.get, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    await screen.findByText("Organisation name");
    expect(within(screen.getByRole("navigation", { name: "Settings" })).queryByRole("link", { name: "GitHub" })).toBeNull();
    expect(within(screen.getByRole("navigation", { name: "Settings" })).queryByRole("link", { name: "Integrations" })).toBeNull(); // an admin's alone (#2725)
    expect(within(screen.getByRole("navigation", { name: "Settings" })).queryByRole("link", { name: "Licence" })).toBeNull(); // an admin's alone (#2879)
    // every member reads Outbound (#2422): the host's settings are theirs to read (design/PERMISSIONS.md, Outbound)
    expect(within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: /^Outbound/ }).getAttribute("href")).toBe("#/settings/outbound");
    // and Databases (#2835): whether database sessions are on is theirs to read
    expect(within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Databases" }).getAttribute("href")).toBe("#/settings/databases");
  });

  // On a phone the tabs scroll sideways: the page shown is scrolled into view, never left off-screen.
  it("scrolls the page shown into view among the tabs", async () => {
    const scrolled = vi.fn(); // jsdom has no scrollIntoView; the stub is removed below
    Element.prototype.scrollIntoView = scrolled;
    try {
      window.location.hash = "#/settings/ai";
      server(golden.get);
      render(<Settings />);
      expect(await screen.findByRole("region", { name: "AI review" })).toBeDefined(); // the reads have landed, and Settings drew again
      const shown = screen.getByRole("link", { name: "AI review", current: "page" });
      expect(scrolled.mock.contexts).toEqual([shown]); // once: a re-render as the reads land never scrolls again
      expect(scrolled).toHaveBeenCalledWith({ inline: "nearest", block: "nearest" });
    } finally {
      delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
    }
  });

  it("People & sign-in shows who may sign in and who administers Canyon", async () => {
    window.location.hash = "#/settings/people";
    server(golden.get);
    render(<Settings />);
    expect(await screen.findByRole("region", { name: "Who may sign in" })).toBeDefined();
    expect(screen.getByRole("region", { name: "Who administers Canyon" })).toBeDefined();
    expect(screen.getByRole("button", { name: "Save people and sign-in" })).toBeDefined();
  });

  // Agents is every member's tab (#100's review): Connect an agent, then Sessions, found by name; People & sign-in keeps
  // the admin's forms alone.
  it("Agents holds Connect an agent and Sessions, and People & sign-in neither", async () => {
    window.location.hash = "#/settings/agents";
    server(golden.get, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    const connect = await screen.findByRole("region", { name: "Connect an agent" });
    expect(connect.nextElementSibling).toBe(await screen.findByRole("region", { name: "Sessions" }));
    expect(within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: "Agents", current: "page" }).getAttribute("href")).toBe("#/settings/agents");
    cleanup();
    window.location.hash = "#/settings/people";
    server(golden.get);
    render(<Settings />);
    await screen.findByRole("region", { name: "Who may sign in" });
    expect(screen.queryByRole("region", { name: "Connect an agent" })).toBeNull();
    expect(screen.queryByRole("region", { name: "Sessions" })).toBeNull();
  });



  // With sign-in off there is nobody to admit and every caller is an admin: forms would do nothing.
  it("People & sign-in on a host with sign-in off says so, and draws no form", async () => {
    window.location.hash = "#/settings/people";
    server(golden.get, { status: 404 }, prefs.preferences_admin, "off");
    render(<Settings />);
    expect(await screen.findByText(S.signInOff)).toBeDefined();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(within(screen.getByRole("main")).queryByRole("button", { name: /^Save/ })).toBeNull();
  });

  it.each([[true, S.aiOn], [false, S.aiOff]])("AI review says whether a model is set up (%s)", async (on, line) => {
    window.location.hash = "#/settings/ai";
    server(golden.get, { status: 404 }, { ...prefs.preferences_admin, ai_review: on });
    render(<Settings />);
    expect(await screen.findByText(line)).toBeDefined();
  });

  // An OpenAI host reviews with a model though it offers no setup chat (#2003).
  it("AI review is on where the host reviews without the setup chat", async () => {
    window.location.hash = "#/settings/ai";
    server(golden.get, { status: 404 }, { ...prefs.preferences_admin, setup_available: false, ai_review: true });
    render(<Settings />);
    expect(await screen.findByText(S.aiOn)).toBeDefined();
  });

  // A front door that fails to read takes nothing else with it.
  it("keeps the organisation's pages when the front door fails to read", async () => {
    window.location.hash = "#/settings/organisation";
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url === "/api/v1/front-door") return { ok: false, status: 500, json: async () => ({ error: "internal", message: "internal error" }) } as unknown as Response;
      const body = url === "/auth/config" ? { canyon: true, auth: "oidc" } : prefs.preferences_admin;
      return { ok: true, status: 200, json: async () => body } as unknown as Response;
    }));
    render(<Settings />);
    expect(await screen.findByLabelText("Organisation name")).toBeDefined();
    expect(screen.getByText("internal error")).toBeDefined();
  });

  // A member who follows a link to the GitHub page is told whose it is, never handed an empty page.
  it("a member on GitHub reads that only an admin changes it", async () => {
    window.location.hash = "#/settings/github";
    const fetch = vi.fn(async (url: string) => {
      const body = url === "/auth/config" ? { canyon: true, auth: "oidc" } : url === "/api/v1/preferences" ? prefs.preferences_member : golden.get;
      return { ok: true, status: 200, json: async () => body } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    render(<Settings />);
    expect(await screen.findByText("Only an admin can change how Canyon connects to GitHub.")).toBeDefined();
    expect(fetch.mock.calls.map(([url]) => url)).not.toContain("/api/v1/github"); // the member is not sent to read it
  });

  // The same for Settings › Telemetry (#2807): a member who follows a link reads whose it is, and the routes, which
  // would answer 403, are never asked.
  it("a member on Telemetry reads that only an admin sees and changes it", async () => {
    window.location.hash = "#/settings/telemetry";
    const fetch = vi.fn(async (url: string) => {
      const body = url === "/auth/config" ? { canyon: true, auth: "oidc" } : url === "/api/v1/preferences" ? prefs.preferences_member : golden.get;
      return { ok: true, status: 200, json: async () => body } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    render(<Settings />);
    expect(await screen.findByText(S.telemetryAdmin)).toBeDefined();
    expect(fetch.mock.calls.map(([url]) => url).filter((u) => u.startsWith("/api/v1/telemetry"))).toEqual([]);
  });

  // Settings › Outbound is an admin's (design/EGRESS.md §8.3): a member who follows a link is told so,
  // and the host's egress setting is never read for them.
  it("a member reads Organisation and changes nothing", async () => {
    window.location.hash = "#/settings/organisation";
    server(golden.get, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText("An admin changes these settings.")).toBeDefined();
    expect(within(screen.getByRole("main")).queryByRole("button", { name: /^Save/ })).toBeNull();
  });
});

describe("Settings › App hosting", () => {
  beforeEach(() => { window.location.hash = "#/settings/hosting"; });

  // The tab says what a front door is where it first names one, and its first line is no second heading
  // (#2174: "How people reach apps", then "How people sign in to apps on this Canyon.").
  it("explains the front door under its heading, in other words than the heading's", async () => {
    server(unset);
    render(<Settings />);
    const heading = await screen.findByRole("heading", { name: "How people reach apps" });
    const first = heading.nextElementSibling?.textContent ?? "";
    expect(first).toContain("The front door is");
    expect(first.startsWith("How people")).toBe(false);
  });

  it("an admin sets Canyon's sign-in: the check shows while it runs, the form stays, and the keyboard stays on Save", async () => {
    let answer: (value: Answer) => void = () => {};
    const sent = server(unset, () => new Promise((resolve) => { answer = resolve; }));
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorNone)).toBeDefined();
    expect(screen.getByText(S.frontDoor)).toBeDefined();
    fireEvent.click(screen.getByRole("radio", { name: S.proxy }));
    type("Issuer", "https://login.meridian.example");
    type("Client ID", "canyon-apps");
    type("Client secret", "redacted");
    expect(screen.getByText(S.callbacks.replace("<address>", "<app>.<env>.canyon.meridian.example"))).toBeDefined();
    save().focus();
    fireEvent.click(save());
    expect(await screen.findByText(S.checking)).toBeDefined();
    expect(screen.getByRole("group", { name: "Who signs people in" })).toHaveProperty("disabled", true);
    expect(screen.getByLabelText("Client ID")).toHaveProperty("value", "canyon-apps");
    expect(document.activeElement).toBe(save());
    await act(async () => answer({ body: golden.get }));
    expect(screen.getByRole("status").textContent).toBe(S.frontDoorSaved);
    expect(sent()).toEqual([golden.put]);
    expect(screen.getByText(S.secretSet.replace("<client_id>", "canyon-apps"))).toBeDefined();
    expect(screen.queryByLabelText("Client secret")).toBeNull();
    expect(document.activeElement).toBe(save());
  });

  // Under hand-off the save runs the probe too, and may wait on it up to 150 s (#2438): the page says it is checking
  // meanwhile, its form held, never a frozen button.
  it("under hand-off an admin's save says it is checking this cluster while the probe runs", async () => {
    let answer: (value: Answer) => void = () => {};
    server(golden.get, () => new Promise((resolve) => { answer = resolve; }), { ...prefs.preferences_admin, run_by: "org" });
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.gateway }));
    type("Issuer", "https://sso.meridian.example");
    type("Audience", "canyon");
    type("JWKS URL", "https://sso.meridian.example/jwks");
    type("Session cookie", "_pomerium");
    fireEvent.click(save());
    expect(await screen.findByText(S.checking)).toBeDefined();
    expect(screen.getByRole("group", { name: "Who signs people in" })).toHaveProperty("disabled", true);
    await act(async () => answer({ body: golden.get_unprobed }));
    expect(screen.getAllByRole("status").map((one) => one.textContent)).toContain(S.frontDoorSaved);
  });

  it("an admin's gateway: an OIDC gateway's four fields, or a load balancer's ARN with its cookie filled in and left out", async () => {
    const sent = server(unset, { body: golden.get });
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.gateway }));
    expect(screen.getByRole("radio", { name: "Standard OIDC gateway" })).toHaveProperty("checked", true);
    expect(screen.getByText(S.cookieNote)).toBeDefined();
    type("Issuer", "https://sso.meridian.example");
    type("Audience", "canyon");
    type("JWKS URL", "https://sso.meridian.example/jwks");
    type("Session cookie", "_pomerium");
    type("Domain", "apps.meridian.example");
    fireEvent.click(save());
    await screen.findByText(S.frontDoorSaved);
    fireEvent.click(screen.getByRole("radio", { name: S.gateway }));
    fireEvent.click(screen.getByRole("radio", { name: "AWS load balancer sign-in" }));
    expect(screen.queryByLabelText("JWKS URL")).toBeNull();
    expect(screen.getByLabelText("Session cookie")).toHaveProperty("value", "AWSELBAuthSessionCookie");
    type("Load balancer ARN", ARN);
    fireEvent.click(save());
    await screen.findByText(S.frontDoorSaved);
    expect(sent()).toEqual([
      { choice: 1, gateway: "oidc", issuer: "https://sso.meridian.example", audience: "canyon", jwks_url: "https://sso.meridian.example/jwks", cookie: "_pomerium", domain: "apps.meridian.example", form: "app.env.domain" },
      { choice: 1, gateway: "alb", alb_arn: ARN, form: "app.env.domain" },
    ]);
  });

  // A load balancer sends each person to the provider with redirect_uri https://<app address>/oauth2/idpresponse, which
  // AWS fixes per host: the provider must accept it for every app, or opening a new app stops there (#2945).
  it("under a load balancer's sign-in, says the provider must accept each app's /oauth2/idpresponse; an OIDC gateway's says nothing of it", async () => {
    server(unset);
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.gateway }));
    expect(screen.queryByText(/oauth2\/idpresponse/)).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "AWS load balancer sign-in" }));
    expect(screen.getByText(S.idpResponse.replace("<app address>", "<app>.<env>.canyon.meridian.example"))).toBeDefined();
    type("Domain", "apps.meridian.example");
    fireEvent.click(screen.getByRole("radio", { name: "https://claims-portal-preview.apps.meridian.example" }));
    expect(screen.getByText(S.idpResponse.replace("<app address>", "<app>-<env>.apps.meridian.example"))).toBeDefined();
    fireEvent.click(screen.getByRole("radio", { name: "Standard OIDC gateway" }));
    expect(screen.queryByText(/oauth2\/idpresponse/)).toBeNull();
  });

  it("shows an app's address under each form and the certificate it needs, as the domain is typed", async () => {
    server(unset);
    render(<Settings />);
    expect(await screen.findByRole("radio", { name: "https://claims-portal.preview.canyon.meridian.example" })).toHaveProperty("checked", true);
    expect(screen.getByText(S.certificate.replace("<names>", "*.preview.canyon.meridian.example and *.prod.canyon.meridian.example"))).toBeDefined();
    type("Domain", "apps.meridian.example");
    fireEvent.click(screen.getByRole("radio", { name: "https://claims-portal-preview.apps.meridian.example" }));
    expect(screen.getByText(S.certificate.replace("<names>", "*.apps.meridian.example"))).toBeDefined();
  });

  it("never fills the secret in: a stored one is named and kept, and replacing it starts from an empty field", async () => {
    const sent = server(golden.get, { body: golden.get });
    render(<Settings />);
    expect(await screen.findByText(S.secretSet.replace("<client_id>", "canyon-apps"))).toBeDefined();
    expect(screen.queryByLabelText("Client secret")).toBeNull();
    fireEvent.click(save());
    await screen.findByText(S.frontDoorSaved);
    expect(sent()).toEqual([{ choice: 2, signin: { issuer: "https://login.meridian.example", client_id: "canyon-apps" }, form: "app.env.domain" }]);
    fireEvent.click(screen.getByRole("button", { name: "Replace the secret" }));
    const secret = screen.getByLabelText("Client secret");
    expect(secret).toHaveProperty("value", "");
    expect(secret).toHaveProperty("type", "password");
    expect(document.activeElement).toBe(secret);
    expect(screen.getByText(S.secretAsk)).toBeDefined();
  });

  it("asks for a secret again when the client changes", async () => {
    server(golden.get);
    render(<Settings />);
    await screen.findByText(S.secretSet.replace("<client_id>", "canyon-apps"));
    type("Client ID", "canyon-apps-2");
    expect(screen.queryByText(S.secretSet.replace("<client_id>", "canyon-apps"))).toBeNull();
    expect(screen.getByLabelText("Client secret")).toHaveProperty("value", "");
  });

  const refusals: [string, Answer][] = [
    ["an uncovered certificate", { status: 409, body: golden.refusal }],
    ["NetworkPolicy not enforced", { status: 409, body: { error: "probe_failed", message: NETWORK_POLICY, network_policy: "not_enforced", certificate: "ok" } }],
    ["a probe that could not tell", { status: 502, body: { error: "probe_unavailable", message: UNAVAILABLE } }],
    ["a value /run refuses", { status: 400, body: { error: "invalid_front_door", message: "signin.issuer is an https URL" } }],
    ["a domain that is not one", { status: 400, body: { error: "invalid_domain", message: "a domain is a lower-case DNS name of two labels or more" } }],
    ["a Docker host", { status: 409, body: { error: "not_kubernetes", message: S.frontDoorDocker } }],
  ];
  for (const [what, refused] of refusals) {
    it(`refused for ${what}: the server's own sentence, and the form keeps what was typed`, async () => {
      server(golden.get, refused);
      render(<Settings />);
      fireEvent.click(await screen.findByRole("button", { name: "Replace the secret" }));
      type("Client secret", "typed-again");
      fireEvent.click(save());
      expect((await screen.findByRole("alert")).textContent).toBe((refused.body as { message: string }).message);
      expect(screen.getByRole("status").textContent).toBe("");
      expect(screen.getByLabelText("Client secret")).toHaveProperty("value", "typed-again");
    });
  }

  it("a member reads the front door and changes nothing", async () => {
    const sent = server(golden.get, { status: 403, body: { error: "admin_required", message: "only an admin can do this: ask dana@meridian.example" } }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorAdmin)).toBeDefined();
    expect(screen.getByText(S.proxy)).toBeDefined();
    expect(screen.getByText("Client ID: canyon-apps")).toBeDefined();
    expect(screen.getByText("https://claims-portal.preview.canyon.meridian.example")).toBeDefined();
    expect(screen.getByText("Dana W.")).toBeDefined();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("radio")).toBeNull();
    expect(within(screen.getByRole("main")).queryByRole("button")).toBeNull(); // the rail's menu button is not the page's
    expect(sent()).toEqual([]);
  });

  // A pod that reaches its own node never refuses the save: the probe's warning
  // shows beside the door as an amber line, saying where the host rule is, to admin and member alike.
  it("a front door saved with a warning shows it as an amber line, with where the rule is", async () => {
    server(golden.get, { status: 403, body: { error: "admin_required", message: "only an admin can do this: ask dana@meridian.example" } }, prefs.preferences_member);
    render(<Settings />);
    const note = await screen.findByRole("note");
    expect(note.textContent).toBe(`${golden.get.warning} ${S.hostFirewall}`);
    expect(note.className).toContain("text-amber");
  });

  // A sign-in on another issuer than Canyon's never refuses the save: once it answers, the line saying
  // person grants won't apply there shows beside the door, amber; on Canyon's own issuer none does (#2299).
  it("an admin's save on another issuer shows that person grants won't apply there, and one on Canyon's shows nothing", async () => {
    server(unset, { body: { ...golden.get, warning: "", grants_note: OTHER_ISSUER } });
    const { unmount } = render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.proxy }));
    type("Issuer", "https://login.contoso.example");
    type("Client ID", "canyon-apps");
    type("Client secret", "redacted");
    fireEvent.click(save());
    const note = await screen.findByText(OTHER_ISSUER);
    expect(note.getAttribute("role")).toBe("note");
    expect(note.className).toContain("text-amber");
    expect(screen.getByRole("status").textContent).toBe(S.frontDoorSaved);
    unmount();
    server(unset, { body: { ...golden.get, warning: "" } });
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.proxy }));
    type("Issuer", "https://login.meridian.example");
    type("Client ID", "canyon-apps");
    type("Client secret", "redacted");
    fireEvent.click(save());
    expect(await screen.findByText(S.frontDoorSaved)).toBeDefined();
    expect(screen.queryByRole("note")).toBeNull();
  });

  // A front door at a sign-in served through the cluster's own gateway is saved under via canyon all the same — refusing it
  // could lock admins out — and once it answers, the line saying routed apps can't reach it shows beside the
  // door, amber (#2316).
  it("an admin's save at a sign-in behind the cluster's gateway under via canyon shows that routed apps can't reach it", async () => {
    server(unset, { body: { ...golden.get, warning: "", egress_note: GATEWAY_NOTE } });
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.proxy }));
    type("Issuer", "https://id.canyon.meridian.example");
    type("Client ID", "canyon-apps");
    type("Client secret", "redacted");
    fireEvent.click(save());
    const note = await screen.findByText(GATEWAY_NOTE);
    expect(note.getAttribute("role")).toBe("note");
    expect(note.className).toContain("text-amber");
    expect(screen.getByRole("status").textContent).toBe(S.frontDoorSaved);
  });

  // A choice 2 door whose provider's discovery Canyon couldn't read yet is saved all the same — the save never waits
  // on the provider — and once it answers, the line saying Canyon keeps trying shows beside the door, amber, to a
  // member reading it too; a read once the server records it carries none ("no warning, no amber line", below; #2344).
  it("a choice 2 door whose discovery isn't read yet says so beside the door until it is", async () => {
    server(unset, { body: { ...golden.get, warning: "", discovery_note: DISCOVERY_NOTE } });
    render(<Settings />);
    fireEvent.click(await screen.findByRole("radio", { name: S.proxy }));
    type("Issuer", "https://login.meridian.example");
    type("Client ID", "canyon-apps");
    type("Client secret", "redacted");
    fireEvent.click(save());
    const note = await screen.findByText(DISCOVERY_NOTE);
    expect(note.getAttribute("role")).toBe("note");
    expect(note.className).toContain("text-amber");
    cleanup();
    server({ ...golden.get, warning: "", discovery_note: DISCOVERY_NOTE }, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorAdmin)).toBeDefined();
    expect((await screen.findByRole("note")).textContent).toBe(DISCOVERY_NOTE);
  });

  // An alb's issuer is known only from a request it signs: its door always says when person grants apply,
  // to a member too, who still reads that only an admin changes it.
  it("a load balancer's door says when person grants apply, to a member as to an admin", async () => {
    server({ ...golden.get, warning: "", grants_note: ALB_ONLY, front_door: { choice: 1, gateway: "alb", alb_arn: ARN, domain: "canyon.meridian.example", form: "app.env.domain", set_by: golden.get.front_door!.set_by, set_at: golden.get.front_door!.set_at, probed: true, enforcing: true } }, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorAdmin)).toBeDefined();
    expect((await screen.findByRole("note")).textContent).toBe(ALB_ONLY);
  });

  it("no warning, no amber line", async () => {
    server({ ...golden.get, warning: "" });
    render(<Settings />);
    expect(await screen.findByText("Dana W.")).toBeDefined();
    expect(screen.queryByRole("note")).toBeNull();
  });

  // A door saved not probed says so beside it, to a member as to an admin: Canyon runs no app behind it (#2438).
  it("a door saved without passing the probe says so beside it", async () => {
    const note = "Not checked on this cluster: your platform can run apps behind this front door, but Canyon won't until a save passes the check. " + NETWORK_POLICY;
    server({ ...golden.get, warning: "", probe_note: note }, { status: 404 }, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorAdmin)).toBeDefined();
    expect((await screen.findByRole("note")).textContent).toBe(note);
  });

  it("on a Docker host says there is no front door to set, and nothing else", async () => {
    server({ runtime: "docker", default_domain: "localhost", front_door: null, warning: "", grants_note: "", egress_note: "", discovery_note: "", hand_off_needs: [], probe_note: "" });
    render(<Settings />);
    expect(await screen.findByText(S.frontDoorDocker)).toBeDefined();
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    expect(screen.queryByText(S.frontDoor)).toBeNull();
    expect(screen.queryByText(S.frontDoorNone)).toBeNull();
    expect(within(screen.getByRole("region", { name: "How people reach apps" })).queryByRole("radio")).toBeNull(); // patching, below, is an admin's on Docker too
  });
});

// Settings › Outbound (design/EGRESS.md §8.3), an admin's: the host's via, and each network to recreate
// with its Recreate, asked first, which calls the route (contracts/routes.md, POST /egress/networks/recreate).
describe("Settings › Outbound", () => {
  const admin = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as { setting_admin: EgressSetting }).setting_admin;
  type Route = Answer | (() => Answer | Promise<Answer>);
  // GET /api/v1/egress answers from `reads` in turn, the last one again; every other route from `routes`.
  function host(reads: Route[], routes: Record<string, Route> = {}, record = prefs.preferences_admin) {
    let read = 0;
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      const key = `${init?.method} ${url}`;
      const route = url === "/auth/config" ? { body: { canyon: true, auth: "oidc" } } : url === "/api/v1/preferences" ? { body: record }
        : key === "GET /api/v1/egress" ? reads[Math.min(read++, reads.length - 1)]! : routes[key] ?? (key === "GET /api/v1/licence" ? { body: { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] } } : key.startsWith("GET /api/v1/fleet?needs_you=true") ? { body: { tiles: {}, apps: [], next: null } } : { status: 404, body: { error: "not_found" } });
      const { status = 200, body, pending } = typeof route === "function" ? await route() : route;
      if (status === 0) throw new TypeError("Failed to fetch");
      return { ok: status < 300, status, json: async () => body, headers: new Headers(pending === undefined ? {} : { "Canyon-Egress-Pending": pending }) } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    return {
      sent: () => fetch.mock.calls.filter(([, init]) => init?.method !== undefined && init.method !== "GET").map(([url, init]) => `${init?.method} ${url} ${String(init?.body)}`),
      reads: () => fetch.mock.calls.filter(([url, init]) => url === "/api/v1/egress" && init?.method === "GET").length,
    };
  }
  const open = async () => {
    window.location.hash = "#/settings/outbound";
    render(<Settings />);
    return screen.findByRole("region", { name: "Outbound" });
  };
  const ask = async (app: string) => fireEvent.click(await screen.findByRole("button", { name: `Recreate ${app}'s network` }));
  const ASK = "Recreate intake-forms's network? Its production and preview restart on the network this setting gives it. If that fails, they stay stopped until you recreate it again.";

  it("an admin reaches it through Settings: via, and the networks to recreate", async () => {
    host([{ body: admin }]);
    const page = within(await open());
    expect((await page.findByRole("radio", { name: "Through Canyon" }) as HTMLInputElement).checked).toBe(true);
    expect(page.getByText("Apps call out through Canyon's egress gateway, which records each destination.")).toBeDefined();
    expect(page.getByText("intake-forms")).toBeDefined();
    expect(page.getByText("These apps still run on their old network. Recreating one restarts it, production included, on the network this setting gives it. If that fails, the app stays stopped until you recreate it again.")).toBeDefined();
  });

  // Off says apps call out directly; an app whose network predates the change still goes through Canyon until an admin
  // recreates it, so Off names those apps beside its line, as the list below names them (#2723).
  it("says under Off which apps still go through Canyon until their network is recreated", async () => {
    const off = (apps: string[], networks: Record<string, string> | null = Object.fromEntries(apps.map((app) => [app, "internal"]))) =>
      ({ ...admin, via: "off", networks_to_recreate: apps, ...(networks === null ? {} : { networks }) });
    host([{ body: off(["ux-crash", "ux-outbound"]) }]);
    const page = within(await open());
    expect((await page.findByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    expect(await page.findByText("ux-crash and ux-outbound still go through Canyon until you recreate their networks below.")).toBeDefined();
    cleanup();
    host([{ body: off(["a", "b", "ux-crash"]) }]);
    expect(await within(await open()).findByText("a, b and ux-crash still go through Canyon until you recreate their networks below.")).toBeDefined();
    cleanup();
    host([{ body: off(["ux-crash"]) }]);
    expect(await within(await open()).findByText("ux-crash still goes through Canyon until you recreate its network below.")).toBeDefined();
    for (const read of [{ ...admin, via: "off", networks_to_recreate: [], networks: {} }, { ...admin, via: "canyon", networks_to_recreate: ["ux-crash"], networks: { "ux-crash": "open" } },
      { ...admin, via: "canyon", networks_to_recreate: null, networks: null }]) {
      cleanup();
      host([{ body: read }]);
      await within(await open()).findByRole("radio", { name: "Off" });
      expect(screen.queryByText(/still go(es)? through/)).toBeNull(); // none listed, or through Canyon already: nothing goes around what Off says
    }
    // Each app's network comes in the settings read itself, one call (#2767): one Canyon can't tell, its recreation under
    // way or failed, is said so, never that it goes through Canyon (D15), and the fleet's needs are never paged for it.
    cleanup();
    const { reads: fetched } = host([{ body: off(["ux-crash", "ux-outbound"], { "ux-crash": "unknown", "ux-outbound": "internal" }) }]);
    const sorted = within(await open());
    expect(await sorted.findByText(`${S.needRecreateUnknown.replace("<app>", "ux-crash")} ${S.needRecreateUnknownStopped}`)).toBeDefined();
    expect(sorted.getByText("ux-outbound still goes through Canyon until you recreate its network below.")).toBeDefined();
    expect(fetched()).toBe(1);
    expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).startsWith("/api/v1/fleet"))).toBe(false);
    // A read that names no network for an app it lists sorts no app, and says so (fail closed).
    for (const networks of [null, { "ux-outbound": "internal" }]) { // none named, or one listed app missing
      cleanup();
      host([{ body: off(["ux-crash", "ux-outbound"], networks) }]);
      const unread = within(await open());
      expect(await unread.findByText("Canyon couldn't read these apps' networks: ux-crash and ux-outbound.")).toBeDefined();
      expect(unread.queryByText(/still go(es)? through Canyon/)).toBeNull();
    }
    // An app id is any name, an object's own key among them: one called constructor that the read doesn't name is missing
    // too, never found on the object's prototype (#2767's review).
    cleanup();
    host([{ body: off(["constructor"], {}) }]);
    expect(await within(await open()).findByText("Canyon couldn't read these apps' networks: constructor.")).toBeDefined();
    // Kubernetes has no network to recreate (networks_to_recreate null): an app keeps its policy until its next deploy.
    cleanup();
    host([{ body: { ...admin, via: "off", networks_to_recreate: null, networks: null } }]);
    expect(await within(await open()).findByText("Apps deployed while this was Through Canyon still go through it until their next deploy.")).toBeDefined();
  });

  // design/EXPERIENCE.md D5 (#2384): outbound is beta in v0.2.0, its tab and its choice marked.
  it("marks Outbound Beta, on its tab and beside how apps call out, with what beta means", async () => {
    host([{ body: admin }]);
    const page = within(await open());
    // the sentence is the tab's and the choice's description, so neither name grows by it
    expect(within(screen.getByRole("navigation", { name: "Settings" })).getByRole("link", { name: `Outbound ${S.beta}`, description: S.betaWhy })).toBeDefined();
    expect(await page.findByRole("group", { name: `${S.via} ${S.beta}`, description: S.betaWhy })).toBeDefined();
  });

  // Recreate asks first, naming the app and only the environments Canyon runs of it, which the recreation starts
  // again (the app's read); unread, it names none (#2774). Cancel sends nothing.
  it("asks before recreating, naming the environments it restarts, and Cancel sends nothing", async () => {
    const PREVIEW = "Recreate intake-forms's network? Its preview restarts on the network this setting gives it. If that fails, it stays stopped until you recreate it again.";
    const UNREAD = "Recreate intake-forms's network? Each of its environments that Canyon runs restarts on the network this setting gives it. If that fails, they stay stopped until you recreate it again.";
    for (const [envs, line] of [
      [{ preview: { run_by: "canyon" } }, PREVIEW],
      [{ prod: { run_by: "canyon" } }, "Recreate intake-forms's network? Its production restarts on the network this setting gives it. If that fails, it stays stopped until you recreate it again."],
      [{ preview: { run_by: "canyon" }, prod: { run_by: "canyon" } }, ASK],
      [{ preview: { run_by: "canyon" }, prod: { run_by: "org" } }, PREVIEW], // production on the org's platform: no network of Canyon's
      [null, UNREAD],
    ] as const) {
      cleanup();
      const { sent } = host([{ body: admin }], envs === null ? {} : { "GET /api/v1/projects/intake-forms": { body: { project: { id: "intake-forms", latest: {}, envs } } } });
      await open();
      await ask("intake-forms");
      expect(await screen.findByText(line)).toBeDefined();
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      expect(screen.queryByText(line)).toBeNull();
      expect(sent()).toEqual([]);
    }
  });

  // One change at a time: a double click sends one POST, Save and every Recreate are off while it runs,
  // and the list is read again once it answers.
  it("recreates once, everything off meanwhile, and reads the list again", async () => {
    let answer: (value: Answer) => void = () => {};
    const { sent, reads } = host([{ body: { ...admin, networks_to_recreate: ["intake-forms", "case-files"] } }, { body: { ...admin, networks_to_recreate: ["case-files"] } }],
      { "POST /api/v1/egress/networks/recreate": () => new Promise<Answer>((resolve) => { answer = resolve; }) });
    await open();
    await ask("intake-forms");
    fireEvent.click(screen.getByRole("radio", { name: "Off" }));
    const yes = screen.getByRole("button", { name: "Recreate the network" });
    fireEvent.click(yes);
    fireEvent.click(yes);
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Recreating intake-forms's network…");
    for (const button of [screen.getByRole("button", { name: "Save how apps call out" }), screen.getByRole("button", { name: "Recreate case-files's network" })]) {
      expect((button.closest("fieldset[disabled]"))).not.toBeNull();
    }
    await act(async () => { answer({ body: { ...admin, networks_to_recreate: ["case-files"] } }); });
    expect(await screen.findByRole("button", { name: "Recreate case-files's network" })).toBeDefined();
    expect(screen.queryByText("intake-forms")).toBeNull();
    expect(screen.queryByRole("status")).toBeNull();
    expect(sent()).toEqual(['POST /api/v1/egress/networks/recreate {"app":"intake-forms"}']);
    expect(reads()).toBe(2);
  });

  // Every refusal of a recreation is said in the server's words; the change is over, and the list is read
  // again, the app still on it.
  for (const [status, error, message] of [
    [409, "held", "production is held"],
    [409, "busy", "another recreation is running on this host"],
    [409, "deleting", "claims-portal is being deleted"],
    [409, "run_in_progress", "a run of intake-forms is under way"],
    [502, "recreate_failed", "intake-forms's network is recreated. Preview can't start again: the image it ran is no longer on this Canyon, so intake-forms needs a new deploy."],
    [404, "not_found", "intake-forms isn't on this Canyon yet. Deploy it first: canyon deploy"],
  ] as const) {
    it(`a recreation answered ${status} ${error}: its words, the change over, the app still listed`, async () => {
      const { sent, reads } = host([{ body: admin }], { "POST /api/v1/egress/networks/recreate": { status, body: { error, message } } });
      await open();
      await ask("intake-forms");
      fireEvent.click(screen.getByRole("button", { name: "Recreate the network" }));
      expect((await screen.findByRole("alert")).textContent).toBe(message);
      expect(screen.queryByRole("status")).toBeNull();
      expect(screen.getByRole("button", { name: "Recreate intake-forms's network" }).closest("fieldset[disabled]")).toBeNull();
      expect(sent()).toHaveLength(1);
      expect(reads()).toBe(2);
    });
  }

  // A page left while its recreation runs reads nothing once it answers.
  it("reads nothing once the page is gone", async () => {
    let answer: (value: Answer) => void = () => {};
    const { reads } = host([{ body: admin }], { "POST /api/v1/egress/networks/recreate": () => new Promise<Answer>((resolve) => { answer = resolve; }) });
    await open();
    await ask("intake-forms");
    fireEvent.click(screen.getByRole("button", { name: "Recreate the network" }));
    cleanup();
    await act(async () => { answer({ status: 502, body: { error: "recreate_failed", message: "left stopped" } }); });
    await act(() => new Promise((resolve) => setTimeout(resolve, 0)));
    expect(reads()).toBe(1);
  });

  it("sets via: the answer adopted, the list as it answers, Save gone", async () => {
    const { sent } = host([{ body: admin }], { "PUT /api/v1/egress": { body: { ...admin, via: "off", networks_to_recreate: ["case-files"] } } });
    await open();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    fireEvent.click(screen.getByRole("button", { name: "Save how apps call out" }));
    expect(await screen.findByText("case-files")).toBeDefined();
    expect((screen.getByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByRole("button", { name: "Save how apps call out" })).toBeNull();
    expect(sent()).toEqual(['PUT /api/v1/egress {"via":"off"}']);
  });

  // The limits, an admin's (#2691; design/EXPERIENCE.md, Settings › Outbound): each field the host's, its range under
  // it; Save sends only what moved, and never a value outside its range or no whole number; the server's refusal in its
  // own words; a server from before them names none, and none shows.
  it("sets the limits: what moved alone, each in its range", async () => {
    const changed = { ...admin, caps: { destinations: 500, new_names: 60, pending: 50 } };
    const refusal = { error: "invalid_caps", message: "caps is an object of destinations (50 to 5000), new_names (1 to 600) and pending (1 to 500), each a whole number, one key at least" };
    let put = 0;
    const { sent } = host([{ body: admin }], { "PUT /api/v1/egress": () => (put++ === 0 ? { status: 400, body: refusal } : { body: changed }) });
    const page = within(await open());
    const limits = within(await page.findByRole("region", { name: S.capsHeading }));
    const field = (label: string) => limits.getByRole("spinbutton", { name: label }) as HTMLInputElement;
    expect([field(S.capNewNames).value, field(S.capDestinations).value, field(S.capPending).value]).toEqual(["30", "500", "50"]);
    expect(limits.getByText(`${S.capNewNamesNote} 1 to 600.`)).toBeDefined();
    expect(limits.getByText(`${S.capDestinationsNote} 50 to 5,000.`)).toBeDefined();
    expect(limits.queryByRole("button", { name: "Save limits" })).toBeNull();
    for (const bad of ["0", "601", "30.5", "", "-1"]) {
      fireEvent.change(field(S.capNewNames), { target: { value: bad } });
      expect(limits.queryByRole("button", { name: "Save limits" }), `new names ${JSON.stringify(bad)}`).toBeNull();
    }
    fireEvent.change(field(S.capNewNames), { target: { value: "60" } });
    fireEvent.click(limits.getByRole("button", { name: "Save limits" }));
    expect((await screen.findByRole("alert")).textContent).toBe(refusal.message);
    fireEvent.change(field(S.capNewNames), { target: { value: "60" } });
    fireEvent.click(limits.getByRole("button", { name: "Save limits" }));
    expect(await limits.findByText(S.capsSaved)).toBeDefined();
    expect(field(S.capNewNames).value).toBe("60");
    expect(sent()).toEqual(['PUT /api/v1/egress {"caps":{"new_names":60}}', 'PUT /api/v1/egress {"caps":{"new_names":60}}']);
  });

  // #2789: limits a gateway replica hadn't taken when the server stopped waiting are saved, and say so in place of
  // Saved, naming the replica.
  it("says saved limits a gateway replica hasn't taken yet, naming it", async () => {
    const replica = "canyon-egress-7d9f6c5b8-k2x4q";
    host([{ body: admin }], { "PUT /api/v1/egress": { body: { ...admin, caps: { destinations: 500, new_names: 60, pending: 50 } }, pending: replica } });
    const limits = within(await within(await open()).findByRole("region", { name: S.capsHeading }));
    fireEvent.change(limits.getByRole("spinbutton", { name: S.capNewNames }), { target: { value: "60" } });
    fireEvent.click(limits.getByRole("button", { name: "Save limits" }));
    expect((await limits.findByText(S.gatewayPending.replace("<replica>", replica))).getAttribute("role")).toBe("status");
    expect(limits.queryByText(S.capsSaved)).toBeNull();
  });

  // #2789: the limits' save waits for the gateway, up to 30 s: while its answer is out the page says Saving… and the
  // fields can't be changed or saved again.
  it("says Saving… while the limits' answer is out", async () => {
    let answer: (a: Answer) => void = () => {};
    host([{ body: admin }], { "PUT /api/v1/egress": () => new Promise<Answer>((resolve) => { answer = resolve; }) });
    const page = within(await open());
    const limits = within(await page.findByRole("region", { name: S.capsHeading }));
    fireEvent.change(limits.getByRole("spinbutton", { name: S.capNewNames }), { target: { value: "60" } });
    fireEvent.click(limits.getByRole("button", { name: "Save limits" }));
    expect((await page.findByText("Saving…")).getAttribute("role")).toBe("status");
    expect((limits.getByRole("spinbutton", { name: S.capNewNames }) as HTMLInputElement).closest("fieldset[disabled]")).not.toBeNull();
    await act(async () => { answer({ body: { ...admin, caps: { destinations: 500, new_names: 60, pending: 50 } } }); });
    expect(await limits.findByText(S.capsSaved)).toBeDefined();
  });

  it("shows no limits when the server names none", async () => {
    const { caps: _, ...before } = admin;
    host([{ body: before }]);
    await open();
    expect(await screen.findByRole("radio", { name: "Through Canyon" })).toBeDefined();
    expect(screen.queryByRole("region", { name: S.capsHeading })).toBeNull();
  });

  it("sets via, and says the server's refusal in its own words", async () => {
    const refusal = { error: "egress_not_ready", message: "Canyon can't send apps' calls out through its egress gateway: this server has no CANYON_EGRESS_PROXY. Leave this off until it does." };
    const { sent } = host([{ body: { ...admin, via: "off", networks_to_recreate: [] } }], { "PUT /api/v1/egress": { status: 409, body: refusal } });
    await open();
    expect(await screen.findByText("None.")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Save how apps call out" })).toBeNull();
    fireEvent.click(screen.getByRole("radio", { name: "Through Canyon" }));
    fireEvent.click(screen.getByRole("button", { name: "Save how apps call out" }));
    expect((await screen.findByRole("alert")).textContent).toBe(refusal.message);
    expect(sent()).toEqual(['PUT /api/v1/egress {"via":"canyon"}']);
    // the form is the host's again, never the refused choice with Save still showing (#2300)
    expect((screen.getByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByRole("button", { name: "Save how apps call out" })).toBeNull();
  });

  // A save whose answer was lost may still have been taken: the form shows what the host reads after it,
  // never the choice from before the save (#2300's review).
  it("reads the setting again after a save that failed, and shows the host's", async () => {
    host([{ body: { ...admin, via: "canyon" } }, { body: { ...admin, via: "off" } }], { "PUT /api/v1/egress": { status: 0, body: null } });
    await open();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    fireEvent.click(screen.getByRole("button", { name: "Save how apps call out" }));
    expect(await screen.findByRole("alert")).toBeDefined();
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect((screen.getByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByRole("button", { name: "Save how apps call out" })).toBeNull();
  });

  // Fail closed: when the read after a failed save fails too, no choice is shown as the host's.
  it("shows no choice when the setting can't be read again", async () => {
    host([{ body: admin }, { status: 500, body: { error: "internal", message: "internal error" } }], { "PUT /api/v1/egress": { status: 409, body: { error: "egress_not_ready", message: "refused" } } });
    await open();
    fireEvent.click(await screen.findByRole("radio", { name: "Off" }));
    fireEvent.click(screen.getByRole("button", { name: "Save how apps call out" }));
    await waitFor(() => expect(screen.queryByRole("radio")).toBeNull());
    expect(screen.getByRole("alert").textContent).toBe("refused");
  });

  // A refused save on Kubernetes ends as the page reads there: the reason once, above the choice, Off,
  // Through Canyon off, and no networks to recreate (#2302's review).
  it("after a save Kubernetes refused, shows the host as Kubernetes reads it", async () => {
    const refused = "Canyon can't send apps' calls out through its egress gateway on Kubernetes yet. Leave this off for now.";
    host([{ body: { ...admin, via: "off", canyon_refused: null, networks_to_recreate: [] } }, { body: { ...admin, via: "off", canyon_refused: refused, networks_to_recreate: null } }],
      { "PUT /api/v1/egress": { status: 409, body: { error: "egress_not_ready", message: refused } } });
    const page = within(await open());
    fireEvent.click(await page.findByRole("radio", { name: "Through Canyon" }));
    fireEvent.click(page.getByRole("button", { name: "Save how apps call out" }));
    await waitFor(() => expect((page.getByRole("radio", { name: "Through Canyon" }) as HTMLInputElement).disabled).toBe(true));
    expect(page.queryByText("Networks to recreate")).toBeNull();
    expect(page.queryByText("None.")).toBeNull();
    expect((page.getByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    expect(page.getAllByText(refused)).toHaveLength(1); // the reason once, above the choice
    expect(page.getByRole("alert").textContent).toBe(refused); // and announced, as the save's refusal
    expect(page.queryByRole("button", { name: "Save how apps call out" })).toBeNull();
  });

  // A host that would refuse Through Canyon says so before the choice, which is off (#2300): the admin
  // never learns it on Save.
  it("says before the choice why Through Canyon can't be had, and turns it off", async () => {
    const refused = "Canyon can't send apps' calls out through its egress gateway on Kubernetes yet. Leave this off for now.";
    const { sent } = host([{ body: { ...admin, via: "off", canyon_refused: refused, networks_to_recreate: null } }]);
    const page = within(await open());
    const said = await page.findByText(refused);
    const canyon = page.getByRole("radio", { name: "Through Canyon" }) as HTMLInputElement;
    expect(canyon.disabled).toBe(true);
    expect(said.compareDocumentPosition(canyon) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect((page.getByRole("radio", { name: "Off" }) as HTMLInputElement).disabled).toBe(false);
    expect(sent()).toEqual([]);
  });

  // A host already through Canyon whose front door moved to a sign-in behind the cluster's gateway says so above
  // the choice, and Through Canyon stays the host's, so Off is still one save away (#2316).
  it("says above the choice that routed apps can't reach a sign-in behind the cluster's gateway, on a host already through Canyon", async () => {
    const { sent } = host([{ body: { ...admin, via: "canyon", canyon_refused: GATEWAY_REFUSED, networks_to_recreate: null } }]);
    const page = within(await open());
    const said = await page.findByText(GATEWAY_REFUSED);
    expect(said.getAttribute("role")).toBe("note");
    expect(said.className).toContain("text-amber");
    const canyon = page.getByRole("radio", { name: "Through Canyon" }) as HTMLInputElement;
    expect(canyon.checked && !canyon.disabled).toBe(true);
    expect(said.compareDocumentPosition(canyon) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(sent()).toEqual([]);
  });

  // A host with no app network to recreate, Kubernetes, shows none of it (#2300).
  it("shows no networks to recreate where the host has none", async () => {
    host([{ body: { ...admin, networks_to_recreate: null } }]);
    const page = within(await open());
    expect(await page.findByRole("radio", { name: "Off" })).toBeDefined();
    expect(page.queryByText("Networks to recreate")).toBeNull();
    expect(page.queryByText("The networks to recreate couldn't be read.")).toBeNull();
  });

  // Fail closed: a read with no list never says there is nothing to recreate; a read that fails says so.
  it("never says None. of a list it did not read", async () => {
    const { networks_to_recreate: _, ...member } = admin;
    host([{ body: member }]);
    await open();
    expect(await screen.findByText("The networks to recreate couldn't be read.")).toBeDefined();
    expect(screen.queryByText("None.")).toBeNull();
    cleanup();
    host([{ status: 500, body: { error: "internal", message: "internal error" } }]);
    await open();
    expect((await screen.findByRole("alert")).textContent).toBe("internal error");
    expect(screen.queryByRole("radio")).toBeNull();
  });
});

// Settings › Outbound's mode (#2422; contracts/routes.md, PUT /api/v1/egress {mode}): the org's, an admin's, off
// while the host's via is off. A switch to allowlist lists, before anything is sent, every destination it would
// refuse — an app env that follows the org's mode and runs through Canyon, its last call allowed, neither its
// allowlist nor the platform's allowing it — each with Allow, and asks in the page; back to observe saves.
describe("Settings › Outbound's replicas", () => {
  const admin = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as { setting_admin: EgressSetting }).setting_admin;
  const answering = (body: EgressSetting) => {
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      const answer = url === "/auth/config" ? { canyon: true, auth: "oidc" } : url === "/api/v1/preferences" ? prefs.preferences_admin : url === "/api/v1/egress" ? body : { error: "not_found" };
      return { ok: url !== "/nope", status: 200, json: async () => answer } as unknown as Response;
    }));
  };
  const open = async () => {
    window.location.hash = "#/settings/outbound";
    render(<Settings />);
    return screen.findByRole("region", { name: "Outbound" });
  };

  // design/EGRESS.md §3.2: an admin reads each replica's policy age, now less current_at; an old one alarms, and a
  // replica that never held the current policy says so. An old policy reads as old however it got there.
  it("says each replica's policy age to an admin", async () => {
    const now = Date.now();
    const at = (ago: number) => new Date(now - ago).toISOString();
    answering({ ...admin, replicas: [
      { replica: "canyon-egress-a", have: "e:42", seen_at: at(0), current_at: at(2_000) },
      { replica: "canyon-egress-b", have: "e:39", seen_at: at(0), current_at: at(47 * 60_000) },
      { replica: "canyon-egress-c", have: null, seen_at: at(0), current_at: null },
    ] });
    const page = within(await open());
    expect(await page.findByText(S.replicasHeading)).not.toBeNull();
    const line = (name: string) => page.getByText(name).parentElement?.textContent;
    expect(line("canyon-egress-a")).toBe("canyon-egress-apolicy up to date");
    expect(line("canyon-egress-b")).toBe("canyon-egress-bpolicy 47m old");
    expect(line("canyon-egress-c")).toBe("canyon-egress-cno policy yet");
  });

  it("draws no heading with no replica", async () => {
    answering({ ...admin, replicas: [] });
    const page = within(await open());
    await page.findByText(S.via);
    expect(page.queryByText(S.replicasHeading)).toBeNull();
  });
});

describe("Settings › Outbound's mode", () => {
  const contract = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/egress/claims-portal.json"), "utf8")) as {
    setting_admin: EgressSetting; setting_member: EgressSetting; project_read: ProjectEgress;
  };
  const admin = contract.setting_admin;
  const golden = contract.project_read;
  // Both envs follow the org's observe: preview's registry.npmjs.org:80 would be refused, its invalid row is refused
  // in every mode; production allows api.stripe.com and calls the platform's login.example-idp.com, so only
  // hooks.slack.com would be refused there.
  const platformRow = { ...golden.envs.prod.destinations[0]!, destination: "login.example-idp.com" };
  const following = (allowed = ["api.stripe.com"]): ProjectEgress => ({
    ...golden,
    envs: { preview: golden.envs.preview, prod: { ...golden.envs.prod, mode: "observe", app_mode: null, allowed, destinations: [...golden.envs.prod.destinations, platformRow] } },
  });
  type Route = Answer | (() => Answer | Promise<Answer>);
  function host(reads: Route[], routes: Record<string, Route> = {}, record = prefs.preferences_admin) {
    let read = 0;
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      const key = `${init?.method} ${url}`;
      const route = url === "/auth/config" ? { body: { canyon: true, auth: "oidc" } } : url === "/api/v1/preferences" ? { body: record }
        : key === "GET /api/v1/egress" ? reads[Math.min(read++, reads.length - 1)]! : routes[key] ?? (key === "GET /api/v1/licence" ? { body: { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] } } : { status: 404, body: { error: "not_found" } });
      const { status = 200, body, pending } = typeof route === "function" ? await route() : route;
      return { ok: status < 300, status, json: async () => body, headers: new Headers(pending === undefined ? {} : { "Canyon-Egress-Pending": pending }) } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    return {
      sent: () => fetch.mock.calls.filter(([, init]) => init?.method === "PUT").map(([url, init]) => `PUT ${url}${init?.body === undefined ? "" : ` ${String(init.body)}`}`),
      asked: (url: string) => fetch.mock.calls.filter(([one]) => one === url).length,
    };
  }
  // intake-bot runs production on its own allowlist: it changes nothing. case-files runs nothing anywhere.
  const intakeBot: ProjectEgress = { ...golden, envs: { preview: { ...golden.envs.preview, via: null, destinations: [] }, prod: golden.envs.prod } };
  const running = { preview: { url: "http://x.preview.localhost:7777" } };
  const reads = (project: () => ProjectEgress = () => following()) => ({
    "GET /api/v1/projects": { body: { projects: [{ id: "case-files", envs: {} }, { id: "claims-portal", envs: running }, { id: "intake-bot", envs: running }] } },
    "GET /api/v1/projects/claims-portal/egress": () => ({ body: project() }),
    "GET /api/v1/projects/intake-bot/egress": { body: intakeBot },
  });
  // Save, once the list is read: while it is read, Save is off.
  const listed = (name = "Save what apps may call") => waitFor(() => {
    const save = changes().getByRole("button", { name }) as HTMLButtonElement;
    expect(save.disabled).toBe(false);
    return save;
  });
  const open = async () => {
    window.location.hash = "#/settings/outbound";
    render(<Settings />);
    return within(await screen.findByRole("region", { name: "Outbound" }));
  };
  const changes = () => within(screen.getByRole("group", { name: "What changes" }));

  // The boundary (design/PERMISSIONS.md, Outbound, Host settings): an admin changes the org's mode.
  it("an admin switches to allowlist: what it would refuse, each with Allow, the question, then {mode}", async () => {
    const { sent } = host([{ body: admin }], { ...reads(), "PUT /api/v1/egress": { body: { ...admin, mode: "allowlist" } } });
    const page = await open();
    expect((await page.findByRole("radio", { name: "Observe" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(page.getByRole("radio", { name: "Allowlist" }));
    expect((await changes().findByText("Allowlist would refuse 2 destinations apps called in the last 30 days:")).closest('[aria-live="polite"]')).not.toBeNull();
    expect(changes().getByText(S.modeNow)).toBeDefined();
    expect(changes().getAllByRole("button", { name: /^Allow / }).map((one) => one.getAttribute("aria-label"))).toEqual(["Allow registry.npmjs.org", "Allow hooks.slack.com"]);
    expect(within(changes().getByRole("region", { name: "claims-portal Production" })).getByText("hooks.slack.com:443")).toBeDefined();
    fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
    expect(sent()).toEqual([]);
    expect(changes().getByText("Refuse 2 destinations now? Apps that call them get a 403 from Canyon until they're allowed or you switch back to Observe.")).toBeDefined();
    expect(document.activeElement).toBe(changes().getByRole("button", { name: S.modeYes })); // the question takes the focus Save had
    fireEvent.click(changes().getByRole("button", { name: S.modeYes }));
    expect(await page.findByText(S.modeSaved)).toBeDefined();
    expect(sent()).toEqual(['PUT /api/v1/egress {"mode":"allowlist"}']);
    expect((page.getByRole("radio", { name: "Allowlist" }) as HTMLInputElement).checked).toBe(true);
  });

  it("Cancel sends nothing", async () => {
    const { sent } = host([{ body: admin }], reads());
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Allowlist" }));
    fireEvent.click(await listed());
    fireEvent.click(changes().getByRole("button", { name: "Cancel" }));
    expect(changes().queryByRole("button", { name: S.modeYes })).toBeNull();
    expect(sent()).toEqual([]);
  });

  // Allow before switching: one entry on that app env's allowlist, and the list is read again without it.
  it("allows a destination from the list, which then refuses one fewer", async () => {
    let allowed = ["api.stripe.com"];
    const { sent } = host([{ body: admin }], { ...reads(() => following(allowed)), "PUT /api/v1/projects/claims-portal/egress/prod/destinations/hooks.slack.com": () => { allowed = [...allowed, "*.slack.com"]; return { status: 204 }; } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Allowlist" }));
    fireEvent.click(await changes().findByRole("button", { name: "Allow hooks.slack.com" }));
    expect(await changes().findByText("Allowlist would refuse 1 destination apps called in the last 30 days:")).toBeDefined();
    expect(sent()).toEqual(["PUT /api/v1/projects/claims-portal/egress/prod/destinations/hooks.slack.com"]);
    expect(changes().queryByRole("region", { name: "claims-portal Production" })).toBeNull();
    // the row leaves the list: said, politely, so it never just vanishes
    expect(changes().getByText("Allowed hooks.slack.com").closest('[aria-live="polite"]')).not.toBeNull();
  });

  // An env with its own mode keeps it, and one not through Canyon decides nothing: neither is counted.
  it("counts no env with its own mode, and says when nothing would be refused", async () => {
    host([{ body: admin }], reads(() => ({ ...golden, envs: { preview: { ...golden.envs.preview, via: "off" }, prod: golden.envs.prod } })));
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Allowlist" }));
    expect(await changes().findByText(S.modeRefusesNone)).toBeDefined();
    fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
    expect(changes().getByText(S.modeAskNone)).toBeDefined();
  });

  // Through Canyon saved while the org's mode is allowlist starts refusing too (#2422's review: the server keeps the
  // mode while via is off). It lists what Canyon recorded before, counting every env that follows the org's mode
  // whatever its via — none goes through Canyon yet — says it can't know all of it, and asks first. Fail closed.
  it("asks before sending calls through Canyon while the org's mode is allowlist", async () => {
    const off = { ...golden, envs: { preview: { ...golden.envs.preview, via: "off" as const }, prod: { ...following().envs.prod, via: "off" as const } } };
    const { sent } = host([{ body: { ...admin, via: "off", mode: "allowlist" } }], { ...reads(() => off), "PUT /api/v1/egress": { body: { ...admin, via: "canyon", mode: "allowlist" } } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Through Canyon" }));
    fireEvent.click(page.getByRole("button", { name: "Save how apps call out" }));
    expect(sent()).toEqual([]);
    expect(await changes().findByText(S.viaUnderAllowlist)).toBeDefined();
    expect(changes().getByText(S.viaUnseen)).toBeDefined();
    expect(await changes().findByText("Allowlist would refuse 2 destinations apps called in the last 30 days:")).toBeDefined();
    expect(changes().getAllByRole("button", { name: /^Allow / }).map((one) => one.getAttribute("aria-label"))).toEqual(["Allow registry.npmjs.org", "Allow hooks.slack.com"]);
    fireEvent.click(await listed("Save how apps call out"));
    expect(changes().getByText(S.viaAsk)).toBeDefined();
    expect(document.activeElement).toBe(changes().getByRole("button", { name: S.viaYes }));
    fireEvent.click(changes().getByRole("button", { name: S.viaYes }));
    await waitFor(() => expect(sent()).toEqual(['PUT /api/v1/egress {"via":"canyon"}']));
  });

  it("never says nothing would be refused when sending calls through Canyon under allowlist", async () => {
    host([{ body: { ...admin, via: "off", mode: "allowlist" } }], { ...reads(() => ({ ...golden, envs: { preview: { ...golden.envs.preview, via: "off", destinations: [] }, prod: { ...golden.envs.prod, destinations: [] } } })) });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Through Canyon" }));
    fireEvent.click(page.getByRole("button", { name: "Save how apps call out" }));
    expect(await changes().findByText(S.viaUnseen)).toBeDefined();
    fireEvent.click(await listed("Save how apps call out"));
    expect(changes().getByText(S.viaAsk)).toBeDefined();
    expect(changes().queryByText(S.modeRefusesNone)).toBeNull();
  });

  // An app that runs nothing anywhere refuses nothing: it is not read. Every other app the list holds is.
  it("reads every app that runs something, and none that runs nothing", async () => {
    const { asked } = host([{ body: admin }], reads());
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Allowlist" }));
    expect(await changes().findByText("Allowlist would refuse 2 destinations apps called in the last 30 days:")).toBeDefined();
    expect(["case-files", "claims-portal", "intake-bot"].map((app) => asked(`/api/v1/projects/${app}/egress`))).toEqual([0, 1, 1]);
  });

  // Fail closed: a read the list needs that fails never reads as nothing refused, and the question is still asked.
  for (const [what, routes] of [
    ["the list of apps", { ...reads(), "GET /api/v1/projects": { status: 500, body: { error: "internal", message: "internal error" } } }],
    ["an app's read", { ...reads(), "GET /api/v1/projects/claims-portal/egress": { status: 500, body: { error: "internal", message: "internal error" } } }],
    ["an app's read out of shape", { ...reads(), "GET /api/v1/projects/claims-portal/egress": { body: { ...golden, envs: { ...golden.envs, prod: { ...golden.envs.prod, allowed: undefined } } } } }],
  ] as const) {
    it(`says it can't say what allowlist would refuse when ${what} fails, and still asks`, async () => {
      const { sent } = host([{ body: admin }], routes as Record<string, Route>);
      const page = await open();
      fireEvent.click(await page.findByRole("radio", { name: "Allowlist" }));
      expect(await changes().findByText(S.modeUnread)).toBeDefined();
      expect(changes().queryByText(S.modeRefusesNone)).toBeNull();
      fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
      expect(changes().getByText(S.modeAskNone)).toBeDefined();
      expect(sent()).toEqual([]);
    });
  }

  // Back to observe widens: it refuses nothing, so it is saved with no question and no list read.
  it("switches back to observe with no question", async () => {
    const { sent, asked } = host([{ body: { ...admin, mode: "allowlist" } }], { "PUT /api/v1/egress": { body: { ...admin, mode: "observe" } } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Observe" }));
    expect(changes().getByText(S.modeWiden)).toBeDefined();
    fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
    expect(await page.findByText(S.modeSaved)).toBeDefined();
    expect(sent()).toEqual(['PUT /api/v1/egress {"mode":"observe"}']);
    expect(asked("/api/v1/projects")).toBe(0);
  });

  // #2789: a change a gateway replica hadn't taken when the server stopped waiting is saved, and says so in place of its
  // confirmation, naming the replica, or that Canyon couldn't confirm every replica took it.
  for (const [pending, line] of [["canyon-egress-7d9f6c5b8-k2x4q", S.gatewayPending.replace("<replica>", "canyon-egress-7d9f6c5b8-k2x4q")], ["unread", S.gatewaysUnread]] as const) {
    it(`says a saved mode no gateway replica had all taken: ${pending}`, async () => {
      host([{ body: { ...admin, mode: "allowlist" } }], { "PUT /api/v1/egress": { body: { ...admin, mode: "observe" }, pending } });
      const page = await open();
      fireEvent.click(await page.findByRole("radio", { name: "Observe" }));
      fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
      expect((await page.findByText(line)).getAttribute("role")).toBe("status");
      expect(page.queryByText(S.modeSaved)).toBeNull();
    });
  }

  // A widening a held production refuses (contracts/routes.md, PUT /api/v1/egress's 409 held) says it wasn't saved,
  // why, and what lets it through, then the server's line, which names the app and its hold (#2746).
  it("says a mode a hold refused wasn't saved, why, and what lets it through, then the server's line", async () => {
    const message = "claims-portal: Production was stopped by Dana Whitfield on 2026-11-08: Stopped while we rotate the Stripe key. It stays stopped until a responder or an admin lets it run again.";
    host([{ body: { ...admin, mode: "allowlist" } }], { "PUT /api/v1/egress": { status: 409, body: { error: "held", message, hold: { by: { kind: "person", subject: "dana", display: "Dana Whitfield" }, at: "2026-11-08T21:19:27Z", reason: "Stopped while we rotate the Stripe key" }, admins: [] } } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Observe" }));
    fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
    const alert = await page.findByRole("alert");
    expect([...alert.querySelectorAll("p")].map((line) => line.textContent)).toEqual([
      "Not saved. A held production can't be given more than it may call now: lift its hold, or give that app's production a mode of its own, then save again.",
      message,
    ]);
    await waitFor(() => expect((page.getByRole("radio", { name: "Allowlist" }) as HTMLInputElement).checked).toBe(true));
    expect(screen.queryByRole("group", { name: "What changes" })).toBeNull();
    expect(page.queryByText(S.modeSaved)).toBeNull();
  });

  it("says calling out directly a hold refused wasn't saved, why, and what lets it through, then the server's line", async () => {
    const message = "claims-portal: Production was stopped by Dana Whitfield on 2026-11-08: Stopped while we rotate the Stripe key. It stays stopped until a responder or an admin lets it run again.";
    host([{ body: { ...admin, via: "canyon" } }], { "PUT /api/v1/egress": { status: 409, body: { error: "held", message, hold: { by: { kind: "person", subject: "dana", display: "Dana Whitfield" }, at: "2026-11-08T21:19:27Z", reason: "Stopped while we rotate the Stripe key" }, admins: [] } } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Off" }));
    fireEvent.click(page.getByRole("button", { name: "Save how apps call out" }));
    const alert = await page.findByRole("alert");
    expect([...alert.querySelectorAll("p")].map((line) => line.textContent)).toEqual([
      "Not saved. A held production can't start calling out directly: lift its hold, then save again.",
      message,
    ]);
    await waitFor(() => expect((page.getByRole("radio", { name: "Through Canyon" }) as HTMLInputElement).checked).toBe(true));
  });

  // Any other refusal is the server's line alone, as before.
  it("says a refused mode in the server's words alone when no hold refused it", async () => {
    const message = "the org's mode could not be saved";
    host([{ body: { ...admin, mode: "allowlist" } }], { "PUT /api/v1/egress": { status: 409, body: { error: "conflict", message } } });
    const page = await open();
    fireEvent.click(await page.findByRole("radio", { name: "Observe" }));
    fireEvent.click(changes().getByRole("button", { name: "Save what apps may call" }));
    expect((await page.findByRole("alert")).textContent).toBe(message);
  });

  // The mode decides only calls through Canyon: while the host's via is off it is off, and says why.
  it("is off while the host's via is off", async () => {
    host([{ body: { ...admin, via: "off" } }]);
    const page = await open();
    expect(await page.findByText(S.modeViaOff)).toBeDefined();
    for (const name of ["Observe", "Allowlist"]) expect(page.getByRole("radio", { name }).closest("fieldset[disabled]")).not.toBeNull();
  });

  // design/EXPERIENCE.md D5 (#2384): outbound is beta, its mode marked as its via is.
  it("marks what apps may call Beta, with what beta means", async () => {
    host([{ body: admin }]);
    const page = await open();
    expect(await page.findByRole("group", { name: `${S.mode} ${S.beta}`, description: S.betaWhy })).toBeDefined();
  });

  // Fail closed: a mode this build does not know is no choice.
  it("shows no mode it does not know", async () => {
    host([{ body: { ...admin, mode: "open" } }]);
    const page = await open();
    expect(await page.findByText("What apps may call couldn't be read.")).toBeDefined();
    expect(page.queryByRole("radio", { name: "Observe" })).toBeNull();
  });

  // Credentials, the API's credential_routes (#2143; contracts/routes.md, the egress setting row): an admin's to turn, {credential_routes}
  // alone, and turning them off says apps keep Canyon's certificate until their next deploy.
  it("an admin turns adding credentials off, told what stays, then on again", async () => {
    const { sent } = host([{ body: admin }], { "PUT /api/v1/egress": { body: { ...admin, credential_routes: false } } });
    const page = await open();
    const box = await page.findByRole("checkbox", { name: S.routesAdd }) as HTMLInputElement;
    expect(box.checked).toBe(true);
    expect(page.queryByText(S.routesTurningOff)).toBeNull();
    fireEvent.click(box);
    expect(page.getByRole("note").textContent).toBe(S.routesTurningOff);
    fireEvent.click(page.getByRole("button", { name: "Save whether Canyon adds credentials" }));
    expect(await page.findByText(S.routesSaved)).toBeDefined();
    expect(sent()).toEqual(['PUT /api/v1/egress {"credential_routes":false}']);
    expect(box.checked).toBe(false);
    fireEvent.click(box);
    expect(page.queryByText(S.routesTurningOff)).toBeNull();
    expect(page.getByRole("button", { name: "Save whether Canyon adds credentials" })).toBeDefined();
  });

  // Turning credentials on is Canyon Enterprise's (#2879): with a lapsed licence an admin turns them off, never on, and is
  // told why in place of the checkbox; what is on stays on.
  it("offers turning credentials off but not on without a licence that unlocks them", async () => {
    const lapsed = { "GET /api/v1/licence": { body: { state: "lapsed", allows: [] } } };
    host([{ body: admin }], { ...lapsed, "PUT /api/v1/egress": { body: { ...admin, credential_routes: false } } });
    const page = await open();
    const box = await page.findByRole("checkbox", { name: S.routesAdd });
    fireEvent.click(box);
    fireEvent.click(page.getByRole("button", { name: "Save whether Canyon adds credentials" }));
    expect(await page.findByText(S.licenceRequiredOutbound)).toBeDefined();
    expect(page.queryByRole("checkbox", { name: S.routesAdd })).toBeNull();
    expect(page.getByText(S.enterprise)).toBeDefined();
  });

  // Fail closed: a read without credential_routes offers no checkbox.
  it("offers no Credentials it could not read", async () => {
    const { credential_routes: _, ...without } = admin;
    host([{ body: without }]);
    const page = await open();
    expect(await page.findByText(S.routesUnread)).toBeDefined();
    expect(page.queryByRole("checkbox", { name: S.routesAdd })).toBeNull();
  });

  // The boundary's other side: a member reads the host's via and the org's mode, changes neither, and reads nothing
  // an admin's read alone answers (design/PERMISSIONS.md, Outbound, Host settings).
  it("a member reads how apps call out and what they may call, read-only", async () => {
    const { sent, asked } = host([{ body: contract.setting_member }], {}, prefs.preferences_member);
    const page = await open();
    expect(page.getByText(S.outboundAdmin)).toBeDefined();
    expect(await page.findByText(S.viaCanyonNote)).toBeDefined();
    expect(page.getByRole("group", { name: `${S.via} ${S.beta}` })).toBeDefined();
    expect(within(page.getByRole("group", { name: `${S.mode} ${S.beta}` })).getByText(S.modeObserve)).toBeDefined();
    expect(page.getByText(S.modeObserveNote)).toBeDefined();
    expect(within(page.getByRole("group", { name: `${S.routes} ${S.beta}` })).getByText(S.routesOn)).toBeDefined();
    expect(page.getByText(S.routesOnNote)).toBeDefined();
    expect(page.queryAllByRole("radio")).toEqual([]);
    expect(page.queryAllByRole("checkbox")).toEqual([]);
    expect(page.queryAllByRole("button")).toEqual([]);
    expect(asked("/api/v1/projects")).toBe(0);
    expect(sent()).toEqual([]);
  });

  it("a member reads no mode while calls don't go through Canyon, and nothing it does not know", async () => {
    host([{ body: { via: "off", mode: "observe", credential_routes: false } }], {}, prefs.preferences_member);
    let page = await open();
    expect(await page.findByText(S.viaOffNote)).toBeDefined();
    expect(page.queryByText(S.mode, { exact: false })).toBeNull();
    cleanup();
    host([{ body: { via: "canyon", mode: "open" } }], {}, prefs.preferences_member);
    page = await open();
    expect((await page.findByRole("alert")).textContent).toBe("Outbound couldn't be read.");
    expect(page.queryByText(S.viaCanyonNote)).toBeNull();
  });
});

// Settings › App hosting's patching (design/PATCHING.md §8; design/PERMISSIONS.md, Platform patches): an admin's alone.
describe("Settings › App hosting › Patching", () => {
  beforeEach(() => { window.location.hash = "#/settings/hosting"; });
  const patchGolden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/patch/claims-portal.json"), "utf8")) as
    { identities: { identities: Record<string, unknown>[] }; kubernetes: { identity: Record<string, unknown> } };
  const identity = patchGolden.identities.identities[0]!;
  const RUN = String(identity.run_image);
  const ID = String(identity.id);
  const SHORT = `system libraries image ${ID.slice(7, 19)}`; // a person reads an image by 12 hex, only here (#2605)
  const paused = { ...identity, paused: { at: "2026-10-19T10:00:00Z", projects: ["claims-portal", "quote-engine"] } };

  // The identities read answers `read`; resume, approve and the preferences PUT answer 200; the rest is the front door.
  function patching(record: Preferences, read: { status: number; body: unknown }, licence: unknown = { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] }) {
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? "GET";
      const { status = 200, body } = url === "/auth/config" ? { body: { canyon: true, auth: "oidc" } }
        : url === "/api/v1/preferences" ? { body: method === "PUT" ? { ...record, ...JSON.parse(String(init?.body)) } : record }
        : url === "/api/v1/platform-webhook" || url === "/api/v1/platform-repo" ? { body: null }
        : url === "/api/v1/patching/identities" ? read
        : url.startsWith("/api/v1/patching/identities/") ? { body: { identity } }
        : url === "/api/v1/licence" ? { body: licence }
        : { body: unset };
      return { ok: status < 300, status, json: async () => body } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    return (path: string) => fetch.mock.calls.filter(([url]) => String(url).startsWith(path)).map(([url, init]) => `${init?.method ?? "GET"} ${url} ${init?.body ?? ""}`.trim());
  }
  const admin = { ...prefs.preferences_admin, patching: "ask" as const };

  // Turning patching or rebuilds on is Canyon Enterprise's (#2879): with a lapsed licence what is on stays on and can be
  // turned off; what is off can't be turned on, and the badge says why in its place.
  it("without a licence, offers turning patching off and never on", async () => {
    const lapsed = { state: "lapsed", allows: [] };
    const sent = patching({ ...admin, rebuilds: "off" }, { status: 200, body: { identities: [], run_image: null } }, lapsed);
    render(<Settings />);
    const section = within(await screen.findByRole("region", { name: /Patching/ }));
    expect(await section.findByText(S.licenceRequiredPatching)).toBeDefined(); // rebuilds are off: no turning them on
    expect(section.queryByRole("radio", { name: "Canyon rebuilds previews on newer Node.js, Python, Java and Go releases and asks before production" })).toBeNull();
    fireEvent.click(section.getByRole("radio", { name: "Canyon doesn't patch apps" }));
    await waitFor(() => expect(sent("/api/v1/preferences").filter((call) => call.startsWith("PUT"))).toEqual(['PUT /api/v1/preferences {"patching":"off"}']));
    expect(await section.findByText(S.licenceRequiredPatching)).toBeDefined();
    expect(section.queryByRole("radio", { name: "Canyon patches previews and asks before production" })).toBeNull();
    cleanup();
    patching({ ...admin, patching: "off" }, { status: 200, body: { identities: [], run_image: null } });
    render(<Settings />);
    expect(await screen.findByRole("radio", { name: "Canyon patches previews and asks before production" })).toBeDefined();
    expect(screen.queryByText(S.licenceRequiredPatching)).toBeNull();
  });

  it("an admin sees the switch, the system libraries image and the apps behind it, and a paused identity they resume", async () => {
    const sent = patching(admin, { status: 200, body: { identities: [paused], run_image: { image: RUN, behind: 3, projects: ["claims-portal", "ledger", "quote-engine"] } } });
    render(<Settings />);
    const section = within(await screen.findByRole("region", { name: /Patching/ }));
    expect(section.getByText("Beta")).toBeDefined();
    expect((section.getByRole("radio", { name: "Canyon patches previews and asks before production" }) as HTMLInputElement).checked).toBe(true);
    expect(section.getByRole("radio", { name: "Canyon doesn't patch apps" })).toBeDefined();
    const behind = (await section.findByText(/3 apps wait for newer system libraries/)).closest("p")!;
    expect(behind.textContent).toBe("3 apps wait for newer system libraries: claims-portal, ledger, quote-engine");
    expect(within(behind).getAllByRole("link").map((link) => link.getAttribute("href"))).toEqual(["#/apps/claims-portal", "#/apps/ledger", "#/apps/quote-engine"]);
    expect(section.getByText(`Canyon paused patching with ${SHORT}: 2 apps reverted after the patch.`)).toBeDefined();
    fireEvent.click(section.getByRole("button", { name: `Resume ${SHORT}` }));
    await waitFor(() => expect(sent("/api/v1/patching/identities/")).toEqual([`POST /api/v1/patching/identities/${encodeURIComponent(ID)}/resume`]));
  });

  it("an admin turns patching off, and the PUT names patching alone", async () => {
    const sent = patching(admin, { status: 200, body: { identities: [], run_image: { image: RUN, behind: 0, projects: [] } } });
    render(<Settings />);
    expect(await screen.findByText("No app waits for newer system libraries")).toBeDefined();
    fireEvent.click(screen.getByRole("radio", { name: "Canyon doesn't patch apps" }));
    await waitFor(() => expect(sent("/api/v1/preferences").filter((call) => call.startsWith("PUT"))).toEqual(['PUT /api/v1/preferences {"patching":"off"}']));
  });

  // #2117 — a rebuild is always its team's to approve
  it("an admin turns rebuilds on, and a rebuild's identity reads as build tools with no approval for every app", async () => {
    const rebuild = (patchGolden as unknown as { rebuild: { identity: Record<string, unknown> } }).rebuild.identity;
    patching({ ...admin, rebuilds: "off" }, { status: 200, body: { identities: [rebuild], run_image: { image: RUN, behind: 0, projects: [] } } });
    render(<Settings />);
    const card = within(await screen.findByRole("group", { name: `build tools ${String(rebuild.id).slice(7, 19)}` }));
    expect(card.queryByRole("button", { name: "Approve for every app" })).toBeNull();
    cleanup();
    patching({ ...admin, rebuilds: "ask" }, { status: 200, body: { identities: [{ ...rebuild, apps: { planned: 0, patched_preview: 0, asked: 0, patched: 0, reverted: 0, held: 0, denied: 0 } }], run_image: { image: RUN, behind: 0, projects: [] } } });
    render(<Settings />);
    expect(await screen.findByText("No app to rebuild with these build tools")).toBeDefined();
    cleanup();
    const sent = patching({ ...admin, rebuilds: "off" }, { status: 200, body: { identities: [rebuild], run_image: { image: RUN, behind: 0, projects: [] } } });
    render(<Settings />);
    await screen.findByRole("group", { name: `build tools ${String(rebuild.id).slice(7, 19)}` });
    expect((screen.getByRole("radio", { name: "Canyon doesn't rebuild apps" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(screen.getByRole("radio", { name: "Canyon rebuilds previews on newer Node.js, Python, Java and Go releases and asks before production" }));
    await waitFor(() => expect(sent("/api/v1/preferences").filter((call) => call.startsWith("PUT"))).toEqual(['PUT /api/v1/preferences {"rebuilds":"ask"}']));
  });

  it("an admin approves an identity for every app with a reason, and sees who approved it once approved", async () => {
    const sent = patching(admin, { status: 200, body: { identities: [identity], run_image: { image: RUN, behind: 0, projects: [] } } });
    const { unmount } = render(<Settings />);
    fireEvent.click(await screen.findByRole("button", { name: "Approve for every app" }));
    expect(screen.getByText(`Approve ${SHORT} for production on every app?`)).toBeDefined();
    expect(screen.getByText("Type the reason above first.")).toBeDefined(); // no reason, no approval (D6: the disabled button says why)
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "USN-7001: libssl3 for the whole fleet" } });
    fireEvent.click(screen.getAllByRole("button", { name: "Approve for every app" }).at(-1)!);
    await waitFor(() => expect(sent("/api/v1/patching/identities/")).toEqual([`POST /api/v1/patching/identities/${encodeURIComponent(ID)}/approve {"reason":"USN-7001: libssl3 for the whole fleet"}`]));
    unmount();
    patching(admin, { status: 200, body: { identities: [{ ...identity, approved: { by: { kind: "person", subject: "sub-dana", display: "Dana Whitfield" }, at: "2026-10-20T09:00:00Z", reason: "USN-7001" } }], run_image: null } });
    render(<Settings />);
    expect(await screen.findByText("Approved for every app by Dana Whitfield, 20 Oct: USN-7001")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve for every app" })).toBeNull();
    expect(screen.getByText("Canyon can't tell which system libraries it uses now, or whether apps wait for newer ones.")).toBeDefined();
  });

  // The test cluster at 611ca7f2 (#2582): the system libraries image Canyon uses now with no app to patch, one patched, one whose canary is
  // held, and one under way. Each reads by 12 hex with its reference folded under it; the wave and Approve for every
  // app only where work is left; the two with none left fold under one line.
  it("an admin reads each system libraries image short, a wave only where one runs, Approve only where it decides, and earlier ones folded", async () => {
    const kube = patchGolden.kubernetes.identity;
    const named = String(kube.run_image); // a Kubernetes run image, its repository and digest
    const none = { planned: 0, patched_preview: 0, asked: 0, patched: 0, reverted: 0, held: 0, denied: 0 };
    const at = (hex: string) => ({ id: `sha256:${hex.repeat(32)}`, run_image: `registry.example/canyon-builder@sha256:${hex.repeat(32)}`, planned_at: "2026-10-18T10:00:00Z", wave: 0 });
    const current = { ...kube, apps: none };
    const underWay = { ...at("3c"), wave: 1, apps: { ...none, planned: 4, patched: 1 } };
    const patched = { ...at("2b"), wave: 3, apps: { ...none, patched: 2, denied: 1 } }; // one team declined it (#2601)
    const held = { ...at("1a"), apps: { ...none, held: 1 } };
    patching(admin, { status: 200, body: { identities: [current, underWay, patched, held], run_image: { image: named, behind: 0, projects: [] } } });
    render(<Settings />);
    const section = within(await screen.findByRole("region", { name: /Patching/ }));
    const reference = (await section.findByText(`System libraries image ${String(kube.id).slice(7, 19)}`)).closest("details")!;
    expect(reference.open).toBe(false);
    expect(within(reference).getByText(named)).toBeDefined(); // the whole reference, on demand
    const card = (hex: string) => within(section.getByRole("group", { name: `system libraries image ${hex.repeat(6)}` }));
    const now = card(String(kube.id).slice(7, 9));
    expect(now.getByText(named)).toBeDefined();
    expect(now.getByText("No app to patch with these system libraries")).toBeDefined();
    expect(now.queryByRole("button", { name: "Approve for every app" })).toBeNull();
    expect(card("3c").getByText("next 5 apps · 4 planned · 1 patched in production")).toBeDefined();
    expect(card("3c").getByRole("button", { name: "Approve for every app" })).toBeDefined();
    expect(section.getAllByRole("button", { name: "Approve for every app" })).toHaveLength(1);
    expect(card("2b").getByText("2 patched in production · 1 turned down for production")).toBeDefined();
    expect(card("1a").getByText("1 held")).toBeDefined();
    expect(section.queryByText(/first app/)).toBeNull();
    const earlier = section.getByText("2 earlier fixes, nothing left to do").closest("details")!;
    expect(earlier.open).toBe(false);
    expect([...earlier.querySelectorAll('[role="group"]')].map((group) => group.getAttribute("aria-label"))).toEqual([`system libraries image ${"2b".repeat(6)}`, `system libraries image ${"1a".repeat(6)}`]);
  });

  // #2588: the identities read's plan, what Canyon's last check said, in the card of the identity it checked against: each
  // cause once with its apps, linked, the causes in strings.ts's order, one the page does not know read as unknown; a
  // check of no app says why under the image Canyon uses now.
  it("an admin reads why each app got no patch at Canyon's last check, beside the system libraries it checked against", async () => {
    const golden = (patchGolden.identities as unknown as { plan: { at: string; identity: string; apps: { project: string; why: string }[] } }).plan;
    const plan = { ...golden, apps: [...golden.apps, { project: "ledger", why: "wave" }, { project: "quote-engine", why: "sideways" }, { project: "zeta", why: "wave" }, { project: "proto", why: "constructor" }] };
    patching(admin, { status: 200, body: { identities: [identity], run_image: { image: RUN, behind: 0, projects: [] }, plan } });
    const { unmount } = render(<Settings />);
    const card = within(await screen.findByRole("group", { name: SHORT }));
    expect(card.getByText(/^Canyon's last check, \d+ \w+ ago$/)).toBeDefined();
    const causes = card.getAllByRole("listitem");
    expect(causes.map((cause) => cause.textContent)).toEqual([
      "Patched in preview · claims-portal",
      "Waiting for its wave · ledger, zeta",
      "No fix from Canyon for this app's findings yet · intake-forms",
      "Not patched, for a reason Canyon can't name · quote-engine, proto",
    ]);
    expect(within(causes[2]!).getByRole("link", { name: "intake-forms" }).getAttribute("href")).toBe("#/apps/intake-forms");
    unmount();
    patching(admin, { status: 200, body: { identities: [identity], run_image: null, plan: { at: golden.at, identity: null, why: "org_off", apps: [] } } });
    render(<Settings />);
    const section = within(await screen.findByRole("region", { name: /Patching/ }));
    expect(await section.findByText("Patching is off for this organisation")).toBeDefined();
    expect(within(section.getByRole("group", { name: SHORT })).queryByText(/Canyon's last check/)).toBeNull();
    cleanup();
    const off = (patchGolden as unknown as { rule_off: { plan: unknown } }).rule_off; // the tick after the rule went off on every app
    patching(admin, { status: 200, body: { identities: [identity], run_image: null, plan: off.plan } });
    render(<Settings />);
    expect(await screen.findByText("The known-vulnerable packages check is off on every app")).toBeDefined();
    expect(screen.queryByText(/No fix from Canyon/)).toBeNull();
  });

  it("with sign-in off the identities are not served: the switch alone", async () => {
    patching({ ...admin, patching: "off" }, { status: 404, body: { error: "not_found" } });
    render(<Settings />);
    expect(await screen.findByRole("radio", { name: "Canyon doesn't patch apps" })).toBeDefined();
    await waitFor(() => expect(screen.queryByText(/system libraries/)).toBeNull());
  });

  it("a member sees none of it, and the identities are never asked for", async () => {
    const sent = patching({ ...prefs.preferences_member, patching: "ask" }, { status: 403, body: { error: "admin_required" } });
    render(<Settings />);
    await screen.findByRole("heading", { name: "How people reach apps" });
    expect(screen.queryByRole("region", { name: /Patching/ })).toBeNull();
    expect(screen.queryByText("Canyon patches previews and asks before production")).toBeNull();
    expect(sent("/api/v1/patching")).toEqual([]);
  });
});

// Settings › App hosting warns an admin of each scanner's data about to turn stale, or stale, in canyon status's words
// (D5; contracts/routes.md, GET /api/v1/scanners/data; #2743): a database more than 5 days old makes every review
// "could not check" with its tool. A member is never sent the read, which answers them 403.
describe("Settings › App hosting › scanner data", () => {
  beforeEach(() => { window.location.hash = "#/settings/hosting"; });
  const now = Date.now();
  const at = (hours: number) => new Date(now + hours * 3_600_000).toISOString().replace(/\.\d+Z$/, "Z");
  const databases = [
    { tool: "grype", built_at: at(-102), stale_at: at(18.2) },
    { tool: "trivy", built_at: at(-48), stale_at: at(72) },
    { tool: "trivy-java", built_at: at(-121), stale_at: at(-1) },
  ];
  function reads(record: Preferences) {
    const fetch = vi.fn(async (url: string) => {
      const { status = 200, body } = url === "/auth/config" ? { body: { canyon: true, auth: "oidc" } }
        : url === "/api/v1/preferences" ? { body: record }
        : url === "/api/v1/platform-webhook" || url === "/api/v1/platform-repo" ? { body: null }
        : url === "/api/v1/patching/identities" ? { status: 404, body: { error: "not_found" } }
        : url === "/api/v1/scanners/data" ? { body: { databases } }
        : { body: unset };
      return { ok: status < 300, status, json: async () => body } as unknown as Response;
    });
    vi.stubGlobal("fetch", fetch);
    return () => fetch.mock.calls.filter(([url]) => url === "/api/v1/scanners/data").length;
  }

  it("an admin reads each database within a day of turning stale, or stale, and nothing of one with longer left", async () => {
    reads(prefs.preferences_admin);
    render(<Settings />);
    const section = within(await screen.findByRole("region", { name: "Scanner data" }));
    const lines = section.getAllByText(/./);
    expect(lines.map((line) => line.textContent)).toEqual([
      "grype's vulnerability data goes stale in 18h, and reviews can't check with grype after that. Canyon couldn't refresh it: its logs say why.",
      "trivy's Java data is over 5 days old, so reviews can't check with trivy until Canyon refreshes it. Canyon's logs say why it couldn't.",
    ]);
    expect(lines[0]!.className).toContain("text-amber");
  });

  it("a member is never sent the read", async () => {
    const asked = reads(prefs.preferences_member);
    render(<Settings />);
    await screen.findByText(S.frontDoorAdmin);
    expect(screen.queryByRole("region", { name: "Scanner data" })).toBeNull();
    expect(asked()).toBe(0);
  });
});
