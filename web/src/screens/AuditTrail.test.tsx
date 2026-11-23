import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AuditPage } from "../api";
import { AUDIT_ACTIONS } from "../strings";
import { what } from "./AuditTable";
import { AuditTrail } from "./AuditTrail";

// The contract's trail in four pages: the first all agents, the second mostly people.
const pages = (JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { audit: AuditPage[] }).audit;
const NOW = new Date("2026-09-02T10:00:00Z");
const EMPTY: AuditPage = { events: [], total: 0, page: 1, next: null };

// The fleet the App filter offers: one app the reader can read the trail of, one they can't.
const FLEET = { tiles: {}, next: null, banner: "none", viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false }, apps: [{ id: "claims-portal", name: "Claims portal", latest: {} }, { id: "hr-portal", name: "hr-portal", owners: [], summary: "", access: { level: "none" } }, { id: "adjuster-notes", name: "Adjuster notes", latest: {} }] };

// The server answers the next trail page on each read of the trail; the returned reader lists the
// query of every trail read.
function server(...answers: AuditPage[]) {
  const fetch = vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => (url.startsWith("/api/v1/fleet") ? FLEET : answers.shift()) }) as unknown as Response);
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url]) => url).filter((url) => url.startsWith("/api/v1/audit")).map((url) => new URL(url, "http://canyon").searchParams);
}

// The trail's rows, each its cells' text apart: a day's heading is a row of its own and no event.
const events = () => screen.getAllByRole("row").filter((row) => row.querySelector("button[aria-expanded]") !== null);
const rows = () => events().map((row) => [...row.querySelectorAll("td")].map((cell) => cell.textContent).join(" | "));

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the audit screen", () => {
  // Found by the UX journeys: the screen read the trail once, so a change made while it was open
  // showed only after a reload. The page is read again every 2 s and what is new goes on top when asked for.
  it("puts a change made while it is open on top, without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(NOW);
    const first = pages[0]!;
    const newer = { ...first.events[0]!, id: "ev_new", action: "policy.set", project: undefined, detail: { rule: "sql.string-concat", name: "No hand-built SQL from user input", before: "blocking", after: "advisory" } }; // as the server writes it
    const asked = server(first, { ...first, events: [newer, ...first.events], total: first.total + 1 });
    render(<AuditTrail query="" />);
    await screen.findByText("Deployed to preview");
    await vi.advanceTimersByTimeAsync(2000);
    // The approved redesign: what came in waits in a bar, so the rows never move under the reader.
    expect(rows()).toHaveLength(first.events.length);
    fireEvent.click(await screen.findByRole("button", { name: "1 new event · Show" }));
    expect(await screen.findByText("Set the policy No hand-built SQL from user input to advisory")).toBeDefined();
    expect(rows()).toHaveLength(first.events.length + 1);
    expect(rows()[0]).toMatch(/Set the policy No hand-built SQL from user input to advisory \| Every app/);
    expect(document.activeElement?.textContent).toMatch(/^Set the policy/); // the keyboard on the newest row
    expect([...asked()[1]!]).toEqual([["since", "2026-09-01T10:00:00.000Z"], ["limit", "50"]]); // the same view, its first page
  });

  it("shows the server's page in the server's order, as sentences, over the last 24 hours", async () => {
    vi.setSystemTime(NOW);
    const asked = server(pages[0]!);
    render(<AuditTrail query="" />);
    await screen.findByText("Deployed to preview");
    expect(rows()).toHaveLength(pages[0]!.events.length);
    // Time | Who | What, the verb first | App | Result
    expect(rows()[0]).toMatch(/^\d\d:\d\d:\d\d \| Canyon \| Reviewed v1.4 \| Claims portal \| Blocked$/);
    expect(rows()[1]).toMatch(/^\d\d:\d\d:\d\d \| DA.+ \| Started a deploy of v1.4 \| Claims portal \| $/);
    expect([...asked()[0]!]).toEqual([["since", "2026-09-01T10:00:00.000Z"], ["limit", "50"]]);
    expect(screen.getByRole("link", { name: "All" }).getAttribute("aria-current")).toBe("true");
  });

  // The server narrows the trail. A page it answers is shown whole: narrowing it here would show a
  // partial page and page through the wrong set.
  it("sends the actor to the server and shows every row it answers", async () => {
    const asked = server(pages[1]!);
    render(<AuditTrail query="actor=agent" />);
    await screen.findAllByRole("table");
    expect(asked()[0]!.get("actor")).toBe("agent");
    expect(rows()).toHaveLength(pages[1]!.events.length);
    expect(screen.getByRole("link", { name: "Agents" }).getAttribute("aria-current")).toBe("true");
  });

  // The questions people come with are saved sets of actions, asked of the server: never a page narrowed here.
  it("asks the server for a common question's actions, and the kind of row chosen", async () => {
    window.location.hash = "#/audit?range=7d";
    const asked = server(EMPTY, EMPTY);
    render(<AuditTrail query="range=7d" />);
    const chip = await screen.findByRole("link", { name: "Approved blocking problems" });
    expect(chip.getAttribute("href")).toBe("#/audit?range=7d&question=approvals");
    cleanup();
    render(<AuditTrail query="question=approvals&category=governance" />);
    await screen.findByText(/^Nothing happened/);
    expect(asked()[1]!.get("action")).toBe("POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve,POST /api/v1/gate-runs/{run}/findings/approve");
    expect(asked()[1]!.get("category")).toBe("governance");
    expect(screen.getByRole("link", { name: "Approved blocking problems" }).getAttribute("aria-current")).toBe("true");
    expect(screen.getByRole("link", { name: "Approved blocking problems" }).getAttribute("href")).toBe("#/audit?category=governance"); // pressed again, it lets go
    fireEvent.change(screen.getByRole("combobox", { name: "Kind" }), { target: { value: "access" } });
    expect(window.location.hash).toBe("#/audit?question=approvals&category=access");
  });

  // A row opens in place on its record: the stored time, who as the trail stores them, the action key and
  // the event's id, a link to its run; how it was done, the session, its address and the role aren't on
  // the trail yet, and say so rather than guess.
  it("opens a row on its record, saying what the trail doesn't record yet", async () => {
    server(pages[0]!);
    render(<AuditTrail query="" />);
    fireEvent.click((await screen.findByText("Reviewed")).closest("button")!); // a role query by name over the whole table outruns findBy's second
    const record = within(screen.getByRole("region", { name: "Record" }));
    const field = (name: string) => record.getByText(name).nextElementSibling?.textContent;
    expect(field("When")).toBe("2026-09-02 09:31:02 UTC");
    expect(field("Who")).toBe("gate · canyon");
    expect(field("Action")).toBe("verdict");
    expect(field("Event")).toBe("ev_c8a1");
    expect(field("Via")).toBe("Canyon");
    for (const none of ["Role", "Session", "From"]) expect(field(none)).toBe("none, Canyon's own step");
    expect(record.getByRole("link", { name: "Open the run" }).getAttribute("href")).toBe("#/apps/claims-portal?run=gr_8f2a");
    expect(screen.getByText("Reviewed").closest("button")!.getAttribute("aria-expanded")).toBe("true");
  });

  // Served from the provenance amendment on: how it was done under the name, and the record's role,
  // channel, session and address — the last two an admin's alone, so a member is told whose they are.
  it("says how a row was done, and to an admin its session and address", async () => {
    const done = { ...pages[0]!.events[1]!, id: "ev_pv", via: "cli" as const, role: "member" as const, session: "ses_4b1a9c0e77d2f3a1", source: "10.2.14.7" };
    server({ ...pages[0]!, events: [done] });
    render(<AuditTrail query="" />);
    await screen.findByText("Started a deploy of");
    expect(rows()[0]).toMatch(/Dana's agent, via CLI \| Started a deploy of v1.4/);
    fireEvent.click(screen.getByText("Started a deploy of").closest("button")!);
    const record = within(screen.getByRole("region", { name: "Record" }));
    const field = (name: string) => record.getByText(name).nextElementSibling?.textContent;
    await waitFor(() => expect(field("Session")).toBe("ses_4b1a9c0e77d2f3a1"));
    expect([field("Role"), field("Via"), field("From")]).toEqual(["member", "The CLI", "10.2.14.7"]);
  });

  // An admin is answered every session: a row a session wrote that carries none was written before the record.
  it("tells an admin a row with no session wasn't recorded, never that it is someone else's to see", async () => {
    const old = { ...pages[0]!.events[1]!, id: "ev_old", via: "cli" as const, session: undefined, source: undefined };
    server({ ...pages[0]!, events: [old] });
    render(<AuditTrail query="" />);
    await screen.findByRole("option", { name: "Claims portal" }); // the fleet read said who reads
    fireEvent.click(screen.getByText("Started a deploy of").closest("button")!);
    expect(within(screen.getByRole("region", { name: "Record" })).getByText("Session").nextElementSibling?.textContent).toBe("not recorded");
  });

  it("tells a member a session and an address are shown to admins, and asks the server for one way of doing", async () => {
    const member = { ...FLEET, viewer: { display: "Tomas Berg", role: "member", github_logins: [], github_link: false } };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => (url.startsWith("/api/v1/fleet") ? member : { ...pages[0]!, events: [{ ...pages[0]!.events[1]!, via: "dashboard", role: "member", session: undefined, source: undefined }] }) }) as unknown as Response)); // a member is answered neither
    render(<AuditTrail query="via=dashboard" />);
    await screen.findByRole("option", { name: "Claims portal" }); // the fleet read said who reads
    fireEvent.click((await screen.findByText("Started a deploy of")).closest("button")!);
    const record = within(screen.getByRole("region", { name: "Record" }));
    await waitFor(() => expect(record.getByText("Session").nextElementSibling?.textContent).toBe("shown to admins"));
    expect((fetch as unknown as { mock: { calls: string[][] } }).mock.calls.map(([url]) => url!).find((url) => url.startsWith("/api/v1/audit"))).toContain("via=dashboard");
    expect((screen.getByRole("combobox", { name: "Via" }) as HTMLSelectElement).value).toBe("dashboard");
    // the way of doing's empty choice says it (#118): from anywhere, never "Any way"
    expect(within(screen.getByRole("combobox", { name: "Via" })).getByRole("option", { name: "From anywhere" }).getAttribute("value")).toBe("");
  });

  // A member's trail is their own work (design/specs/screens.md, A4a): the server answers it, and the
  // screen offers no question about the org's admin, sign-in or guardrail changes; an admin's is as ever.
  it("offers a member no question about the org's own changes, and says whose trail it is", async () => {
    const member = { ...FLEET, viewer: { display: "Tomas Berg", role: "member", github_logins: [], github_link: false } };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => (url.startsWith("/api/v1/fleet") ? member : EMPTY) }) as unknown as Response));
    render(<AuditTrail query="" />);
    expect(await screen.findByText("Your apps' changes and your own sign-ins, newest first.", { exact: false })).toBeDefined();
    const questions = within(screen.getByRole("navigation", { name: "Common questions" }));
    expect(questions.getAllByRole("link").map((one) => one.textContent)).toEqual(["Production changes", "Access changes", "Approved blocking problems", "Secrets", "Deletions", "Denied and failed"]);
    cleanup();
    server(EMPTY);
    render(<AuditTrail query="" />);
    expect(await screen.findByText("Every change, newest first.", { exact: false })).toBeDefined();
    await waitFor(() => expect(within(screen.getByRole("navigation", { name: "Common questions" })).getAllByRole("link")).toHaveLength(8));
  });

  it("tells a member a row written before the record has none of it, not that it is hidden", async () => {
    const member = { ...FLEET, viewer: { display: "Tomas Berg", role: "member", github_logins: [], github_link: false } };
    const old = { ...pages[0]!.events[1]!, via: undefined, role: undefined, session: undefined, source: undefined };
    vi.stubGlobal("fetch", vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => (url.startsWith("/api/v1/fleet") ? member : { ...pages[0]!, events: [old] }) }) as unknown as Response));
    render(<AuditTrail query="" />);
    await screen.findByRole("option", { name: "Claims portal" });
    fireEvent.click((await screen.findByText("Started a deploy of")).closest("button")!);
    const record = within(screen.getByRole("region", { name: "Record" }));
    for (const field of ["Role", "Via", "Session", "From"]) expect(record.getByText(field).nextElementSibling?.textContent).toBe("not recorded");
  });

  // The two refusals the trail keeps (contracts/routes.md, the audit read): what was tried and why not,
  // greyed as Denied and Refused, and one common question asks for them with the runs that failed.
  it("says what a refused request tried and why, and asks for the denied and failed together", async () => {
    const bob = { kind: "person" as const, subject: "bob@meridian.example", display: "Bob Lindqvist" };
    const denied = { ...pages[0]!.events[0]!, id: "ev_dn", action: "denied", actor: bob, project: undefined, run: undefined, detail: { action: "PUT /api/v1/preferences", error: "admin_required" } };
    const refused = { ...pages[0]!.events[0]!, id: "ev_rf", action: "sign in refused", actor: { kind: "person" as const, subject: "mallory@gmail.example", display: "" }, project: undefined, run: undefined, detail: { flow: "browser", email: "mallory@gmail.example", reason: "not_a_member" } };
    const at = (id: string, action: string, error: string, project?: string) => ({ ...denied, id, project, detail: { action, error } });
    const asked = server({ ...pages[0]!, events: [denied, refused,
      at("ev_pr", "POST /api/v1/projects/{id}/promotions", "team_required", "claims-portal"),
      at("ev_cn", "POST /api/v1/gate-runs/{id}/cancel", "person_required", "claims-portal"),
      at("ev_rd", "GET /api/v1/projects/{id}/status", "team_required", "claims-portal")] });
    render(<AuditTrail query="question=refused" />);
    await screen.findByText("Sign-in refused");
    expect(rows()[0]).toMatch(/Bob L.+ \| Tried to change the org's settings — admins only \| Org \| Denied$/); // Bob tried; Canyon refused
    expect(rows()[1]).toMatch(/Sign-in refused mallory@gmail.example: not on the allowed domains or emails \| Org \| Refused$/);
    expect(rows().slice(2).map((row) => row.split(" | ")[2])).toEqual([
      "Tried to promote a version to production — the app's team only",
      "Tried to cancel a run — people only, not agents",
      "Tried to read /projects/{id}/status — the app's team only", // a route with no phrase yet, never blank
    ]);
    expect(asked()[0]!.get("action")?.split(",")).toEqual(["denied", "sign in refused", "build failed", "start failed", "detect failed", "build had no room", "platform reported failed"]);
    expect(screen.getByRole("link", { name: "Denied and failed" }).getAttribute("aria-current")).toBe("true");
  });

  // Production changes (#145): what went to production, was taken from it or held, and who was asked and said what. Every
  // action the bundle words is sorted here, so a new one is production's or isn't before it ships; the server has no one
  // list of the actions it records, so one it writes and the bundle has no words for is not seen here. The org's platform's
  // rows are worded by the table and each names its environment.
  const PRODUCTION = [
    "deploy prod", "POST /api/v1/projects/{id}/promotions", "request promotion", "approve promotion", "deny promotion",
    "stop app", "lift hold", "DELETE /api/v1/projects/{id}/envs/{env}", "request undeploy", "send remove again",
    "patch prod", "revert patch prod", "request patch approval", "approve patch identity",
  ];
  const PLATFORM = [
    "platform reported deployed", "platform reported removed", "platform reported failed", "platform reported another digest",
    "commit release", "release live by check-in", "release removed by commit", "release tree mismatch", "pull request closed", "pull request changed",
  ];
  const NOT_PRODUCTION = [
    "sign in", "sign out", "revoke sessions", "complete setup", "PUT /api/v1/preferences", "POST /api/v1/guardrails/setup",
    "deploy preview", "verdict", "build failed", "start failed", "detect failed", "build had no room",
    "policy.set", "custom_rule.set", "set playbook", "restore playbook", "remove playbook", "app_policy.set", "app_policy.remove",
    "POST /api/v1/projects", "PUT /api/v1/projects/{id}/owner", "DELETE /api/v1/projects/{id}",
    "declare resource", "ensure resource", "update sidecar", "reissue resource password", "delete resource",
    "package from github", "gate run from github", "github app registered", "POST /api/v1/projects/{id}/packages",
    "POST /api/v1/projects/{id}/packages/from-repository", "POST /api/v1/projects/{id}/gate-runs", "POST /api/v1/projects/{id}/deploys",
    "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", "POST /api/v1/gate-runs/{run}/findings/approve", "POST /api/v1/gate-runs/{id}/cancel",
    "PUT /api/v1/projects/{id}/secrets/{name}", "DELETE /api/v1/projects/{id}/secrets/{name}", "GET /api/v1/projects/{id}/logs",
    "POST /api/v1/design-systems", "share app", "unshare app", "request access", "dismiss access request",
    "egress.allow", "egress.disallow", "egress.set", "egress.mode.set", "egress.request", "egress.request.dismiss", "egress.networks.recreate",
    "egress.route.set", "egress.route.remove", "egress.ca.rotate",
    "patch preview", "revert patch preview", "pause patching", "resume patching", "patching off at start",
  ];
  it("Production changes asks for every action that is one, and every worded action is sorted", async () => {
    expect([...PRODUCTION, ...NOT_PRODUCTION].sort()).toEqual(Object.keys(AUDIT_ACTIONS).sort());
    const asked = server(EMPTY);
    render(<AuditTrail query="question=production" />);
    await screen.findByText(/^Nothing happened/);
    const actions = asked()[0]!.get("action")!.split(",");
    expect([...actions].sort()).toEqual([...PRODUCTION, ...PLATFORM].sort());
    expect(actions.length).toBeLessThanOrEqual(64); // what the read takes (contracts/routes.md)
  });
  it("says an environment taken offline as a sentence", () => {
    const { verb, rest } = what({ ...pages[0]!.events[0]!, action: "DELETE /api/v1/projects/{id}/envs/{env}", detail: { env: "preview" } });
    expect([verb, rest]).toEqual(["Took", "preview offline, keeping its data"]);
  });
  it("each of them that can be preview's names its environment", () => {
    const either = ["DELETE /api/v1/projects/{id}/envs/{env}", "request undeploy", "send remove again", ...PLATFORM];
    for (const action of either) for (const [env, word] of [["prod", "production"], ["preview", "preview"]] as const) {
      const { verb, rest } = what({ ...pages[0]!.events[0]!, action, detail: { env, package: "pkg_1" } });
      expect(`${verb} ${rest ?? ""}`, action).toContain(word);
    }
  });

  // A refused change to a credential or the roots (#2145) says what was tried in words, never the route's path.
  it("says a refused change to a credential or the roots in words", async () => {
    const bob = { kind: "person" as const, subject: "bob@meridian.example", display: "Bob Lindqvist" };
    const at = (id: string, action: string, error: string, project?: string) => ({ ...pages[0]!.events[0]!, id, action: "denied", actor: bob, project, run: undefined, detail: { action, error } });
    server({ ...pages[0]!, events: [
      at("ev_rs", "PUT /api/v1/projects/{id}/egress/{env}/routes/{destination}", "dashboard_required", "claims-portal"),
      at("ev_rr", "DELETE /api/v1/projects/{id}/egress/{env}/routes/{destination}", "owner_required", "claims-portal"),
      at("ev_rm", "POST /api/v1/projects/{id}/egress/{env}/routes/{destination}/from-secret", "owner_required", "claims-portal"),
      at("ev_ca", "POST /api/v1/egress/ca/rotate", "admin_required")] });
    render(<AuditTrail query="question=refused" />);
    await screen.findByText(/set a credential/);
    expect(rows().map((row) => row.split(" | ")[2])).toEqual([
      "Tried to set a credential — in the dashboard only, not from the CLI",
      "Tried to remove a credential — the app's owners only",
      "Tried to move a secret into a credential — the app's owners only",
      "Tried to replace the root of Canyon's credential certificates — admins only",
    ]);
  });

  // An image Canyon didn't review, running where a reviewed one was released: the row says so at alert
  // weight and opens on the two digests side by side.
  it("shows an image mismatch at alert weight, with the reviewed and the running image", async () => {
    const mismatch = { ...pages[0]!.events[0]!, id: "ev_mm", action: "platform reported another digest", actor: { kind: "agent" as const, subject: "platform", display: "Meridian's platform" }, detail: { env: "prod", expected: "sha256:9f1c2e8a00000000d41b", reported: "sha256:47aa0b1300000000e902" } };
    server({ ...pages[0]!, events: [mismatch] });
    render(<AuditTrail query="" />);
    await screen.findByText("Reported a different image running");
    expect(rows()[0]).toMatch(/Reported a different image running in production, not the reviewed one \| Claims portal \| Image mismatch$/);
    fireEvent.click(screen.getByText("Reported a different image running").closest("button")!);
    const reported = screen.getByRole("region", { name: "What was reported" });
    expect(reported.textContent).toContain("sha256:9f1c2e8a…d41b");
    expect(reported.textContent).toContain("sha256:47aa0b13…e902");
  });

  // Privilege changes read as privilege changes: the admins before and after, never "changed the guardrails".
  it("reads a change to the admins as one, and opens on the list before and after", async () => {
    const admins = { ...pages[0]!.events[0]!, id: "ev_ad", action: "PUT /api/v1/preferences", project: undefined, actor: { kind: "person" as const, subject: "dana@meridian.example", display: "Dana Whitfield" }, detail: { admin_emails: { before: ["dana@meridian.example"], after: ["dana@meridian.example", "bob@meridian.example"] } } };
    server({ ...pages[0]!, events: [admins] });
    render(<AuditTrail query="" />);
    await screen.findByText("Changed who is an admin");
    expect(rows()[0]).toMatch(/Changed who is an admin added bob@meridian.example \| Org \| Admin change$/);
    fireEvent.click(screen.getByText("Changed who is an admin").closest("button")!);
    expect(within(screen.getByRole("region", { name: "What changed" })).getAllByRole("row").map((row) => row.textContent)).toEqual([
      "SettingBeforeAfter", "admin_emailsdana@meridian.exampledana@meridian.example, bob@meridian.example",
    ]);
  });

  // An approval says what was accepted: the rule, the reason and the expiry the row stores.
  it("says what an approval accepted, with its reason and expiry", async () => {
    const approval = { ...pages[0]!.events[0]!, id: "ev_ap", action: "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", actor: { kind: "person" as const, subject: "bob@meridian.example", display: "Bob Lindqvist" }, detail: { rule: "secrets.client-bundle", reason: "Test key, rotated on 3 Oct", expires_at: "2026-11-03T12:00:00Z" } };
    server({ ...pages[0]!, events: [approval] });
    render(<AuditTrail query="" />);
    await screen.findByText("Approved a blocking problem");
    expect(rows()[0]).toMatch(/Approved a blocking problem secrets.client-bundle, "Test key, rotated on 3 Oct", until 3 Nov 2026 \| Claims portal \| Risk accepted$/);
  });

  it("keeps the filters in the address, each link changing one of them", () => {
    server(EMPTY);
    window.location.hash = "#/audit?actor=agent&range=7d";
    render(<AuditTrail query="actor=agent&range=7d" />);
    const href = (name: string) => screen.getByRole("link", { name }).getAttribute("href");
    expect(href("All")).toBe("#/audit?range=7d");
    expect(href("People")).toBe("#/audit?actor=person&range=7d");
    expect(href("Canyon")).toBe("#/audit?actor=canyon&range=7d");
    fireEvent.change(screen.getByRole("combobox", { name: "Time range" }), { target: { value: "24h" } });
    expect(window.location.hash).toBe("#/audit?actor=agent");
    fireEvent.change(screen.getByRole("combobox", { name: "Time range" }), { target: { value: "30d" } });
    expect(window.location.hash).toBe("#/audit?actor=agent&range=30d");
  });

  it("sends the range as since=, and says so in the spec's words when nothing happened", async () => {
    vi.setSystemTime(NOW);
    const asked = server(EMPTY);
    render(<AuditTrail query="actor=canyon&range=7d" />);
    await screen.findByText("Nothing happened in the last 7 days. Try a wider range.");
    expect(asked()[0]!.get("since")).toBe("2026-08-26T10:00:00.000Z");
    expect(asked()[0]!.get("actor")).toBe("canyon");
  });

  it("uses the spec's empty line word for word over the default range", async () => {
    server(EMPTY);
    render(<AuditTrail query="" />);
    await screen.findByText("Nothing happened in the last 24 hours. Try a wider range.");
  });

  // Load more once said neither how many rows there were nor where a page sat in them.
  it("says where the page sits in the server's total and goes to the page pressed, under the same filters", async () => {
    server({ ...pages[0]!, total: 15, page: 1 });
    window.location.hash = "#/audit?actor=person&limit=4";
    render(<AuditTrail query="actor=person&limit=4" />);
    expect((await screen.findByText(/of 15 events/)).textContent).toBe("1–4 of 15 events · Page 1 of 4");
    fireEvent.click(screen.getByRole("button", { name: "Page 2" }));
    expect(window.location.hash).toMatch(/^#\/audit\?actor=person&limit=4&page=2&until=2\d{3}-[^&]+Z$/); // a walk is read up to when it began
    cleanup();
    const asked = server({ ...pages[1]!, total: 15, page: 2 });
    render(<AuditTrail query="actor=person&limit=4&page=2" />);
    expect((await screen.findByText(/of 15 events/)).textContent).toBe("5–8 of 15 events · Page 2 of 4");
    expect(screen.getByRole("button", { name: "Page 2" }).getAttribute("aria-current")).toBe("page");
    expect([...asked()[0]!].filter(([key]) => key !== "since")).toEqual([["actor", "person"], ["limit", "4"], ["page", "2"]]);
    expect(screen.getByRole("link", { name: "Agents" }).getAttribute("href")).toBe("#/audit?actor=agent&limit=4"); // a filter goes back to the first page
    expect(document.activeElement).toBe(screen.getByRole("heading", { level: 1 })); // the keyboard starts at the top of the page opened
    fireEvent.change(screen.getByRole("combobox", { name: "Rows per page" }), { target: { value: "50" } });
    expect(window.location.hash).toBe("#/audit?actor=person&page=1"); // rows 5 to 54: the first page, at the default size, named
  });

  // Back to the first page from the footer: the keyboard goes to the heading there too, and the first page polls.
  it("hands the keyboard to the heading on a first page chosen in the footer, and keeps reading it", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const asked = server({ ...pages[0]!, total: 15, page: 1 }, { ...pages[0]!, total: 15, page: 1 });
    render(<AuditTrail query="limit=4&page=1" />);
    await screen.findByText(/of 15 events/);
    expect(document.activeElement).toBe(screen.getByRole("heading", { level: 1 }));
    await vi.advanceTimersByTimeAsync(2000);
    expect(asked().length).toBeGreaterThan(1);
  });

  // A later page holds still while it is read: a new row on the first page would move every row under the reader.
  it("reads a later page once, and only the first page again every 2 s", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const asked = server({ ...pages[1]!, total: 15, page: 2 }, { ...pages[1]!, total: 15, page: 2 });
    render(<AuditTrail query="limit=4&page=2" />);
    await screen.findByText(/of 15 events/);
    await vi.advanceTimersByTimeAsync(4000);
    expect(asked()).toHaveLength(1);
  });

  // Found in the audit of the redesign: People, then 30 days, came back as All.
  it("keeps People when the range changes to 30 days", async () => {
    server(EMPTY, EMPTY);
    window.location.hash = "#/audit";
    const { unmount } = render(<AuditTrail query="" />);
    window.location.hash = screen.getByRole("link", { name: "People" }).getAttribute("href")!;
    unmount();
    render(<AuditTrail query="actor=person" />);
    fireEvent.change(screen.getByRole("combobox", { name: "Time range" }), { target: { value: "30d" } });
    expect(window.location.hash).toBe("#/audit?actor=person&range=30d");
  });

  it("drops the suggestion at the widest range", async () => {
    server(EMPTY);
    render(<AuditTrail query="range=30d" />);
    expect(await screen.findByText("Nothing happened in the last 30 days.")).toBeDefined();
  });

  // The server narrows to one app; the choices are the apps whose trail the reader may read.
  it("narrows to one app, offering the apps the reader can read the trail of", async () => {
    const asked = server(EMPTY);
    window.location.hash = "#/audit?actor=agent";
    render(<AuditTrail query="actor=agent" />);
    const pick = screen.getByRole("combobox", { name: "App" }) as HTMLSelectElement;
    await waitFor(() => expect([...pick.options].map((one) => [one.value, one.text])).toEqual([["", "All apps"], ["adjuster-notes", "Adjuster notes"], ["claims-portal", "Claims portal"]]));
    fireEvent.change(pick, { target: { value: "claims-portal" } });
    expect(window.location.hash).toBe("#/audit?actor=agent&project=claims-portal");
    cleanup();
    const again = server(EMPTY);
    render(<AuditTrail query="actor=agent&project=claims-portal" />);
    await screen.findByText(/^Nothing happened/);
    expect(again()[0]!.get("project")).toBe("claims-portal");
    expect(asked()).toHaveLength(1);
  });

  // An app below the reader's contributor level is the server's 403, said in the server's words.
  it("says the server's sentence when the trail is refused", async () => {
    vi.stubGlobal("fetch", vi.fn(async (url: string) => (url.startsWith("/api/v1/fleet")
      ? { ok: true, status: 200, json: async () => FLEET }
      : { ok: false, status: 403, json: async () => ({ error: "team_required", message: "Ask Dana Whitfield to add you to hr-portal" }) }) as unknown as Response));
    render(<AuditTrail query="project=hr-portal" />);
    expect(await screen.findByText("Ask Dana Whitfield to add you to hr-portal")).toBeDefined();
  });

  // A custom rule's History: every change to that rule, whatever the range, and a way back.
  it("shows one rule's changes over all time, with a way back to every change", async () => {
    const asked = server(EMPTY);
    render(<AuditTrail query="rule=custom.no-pii&range=7d&project=claims-portal" />);
    expect(await screen.findByText("Nothing has changed this rule yet.")).toBeDefined();
    expect([...asked()[0]!]).toEqual([["rule", "custom.no-pii"], ["limit", "50"]]); // a rule's rows are the org's: no app, no range
    expect(screen.queryByRole("combobox", { name: "Time range" })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "App" })).toBeNull();
    expect(screen.getByRole("link", { name: "Show every change" }).getAttribute("href")).toBe("#/audit?range=7d&project=claims-portal");
  });

  describe("Export", () => {
    // The file the browser is handed, read back from the Blob the screen made.
    const saved = () => {
      const files: { name: string; blob: Blob }[] = [];
      vi.stubGlobal("URL", Object.assign(URL, { createObjectURL: (blob: Blob) => { files.push({ name: "", blob }); return "blob:x"; }, revokeObjectURL: () => {} }));
      vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) { files.at(-1)!.name = this.download; });
      return files;
    };

    it("downloads every row the filters select as CSV, across the server's pages", async () => {
      const files = saved();
      const asked = server(pages[0]!, ...pages);
      render(<AuditTrail query="actor=person&range=7d" />);
      await screen.findByText("Deployed to preview");
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      const total = pages.flatMap((page) => page.events).length;
      expect(await screen.findByText(`Exported ${total} rows.`)).toBeDefined();
      expect(files[0]!.name).toBe("canyon-audit-all-7d.csv");
      const lines = (await files[0]!.blob.text()).split("\r\n");
      expect(lines[0]).toBe("When,Kind,Who,What,App,Run,Action,Via,Role,Session,From,Detail");
    expect(lines.find((line) => line.includes(",verdict,"))).toContain('""blocking"":1'); // the detail reaches the spreadsheet
      expect(lines).toHaveLength(total + 1);
      const reads = asked().slice(1);
      expect(reads.map((one) => one.get("cursor"))).toEqual([null, ...pages.slice(0, -1).map((page) => page.next)]);
      for (const one of reads) { expect(one.get("actor")).toBe("person"); expect(one.get("limit")).toBe("200"); }
    });

    it("names a rule's export after the rule, and reads every change to it", async () => {
      const files = saved();
      const asked = server(EMPTY, { ...pages[0]!, next: null });
      render(<AuditTrail query="rule=custom.no-pii&range=7d" />);
      await screen.findByText("Nothing has changed this rule yet.");
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      await screen.findByText(/^Exported/);
      expect(files[0]!.name).toBe("canyon-audit-custom.no-pii.csv");
      expect([...asked()[1]!]).toEqual([["rule", "custom.no-pii"], ["limit", "200"]]);
    });

    it("downloads JSON as the server's rows", async () => {
      const files = saved();
      server(EMPTY, { ...pages[0]!, next: null });
      render(<AuditTrail query="" />);
      await screen.findByText(/^Nothing happened/);
      fireEvent.click(screen.getByRole("button", { name: "JSON" }));
      await screen.findByText(/^Exported/);
      expect(files[0]!.name).toBe("canyon-audit-all-24h.json");
      expect(JSON.parse(await files[0]!.blob.text())).toEqual(pages[0]!.events);
    });

    // A spreadsheet runs a cell that begins with = as a formula: an exported sentence never does.
    it("never hands a spreadsheet a formula", async () => {
      const files = saved();
      const evil = { ...pages[0]!.events[0]!, id: "ev_evil", actor: { kind: "agent", subject: "a", display: "=HYPERLINK(\"http://x\")" } };
      server(EMPTY, { ...EMPTY, events: [evil as AuditPage["events"][number]] });
      render(<AuditTrail query="" />);
      await screen.findByText(/^Nothing happened/);
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      await screen.findByText("Exported 1 row.");
      expect((await files[0]!.blob.text()).split("\r\n")[1]).toContain(`,"'=HYPERLINK(""http://x"")",`);
    });

    it("copies a link to this view", async () => {
      const writeText = vi.fn(async () => {});
      vi.stubGlobal("navigator", { clipboard: { writeText } });
      server(EMPTY);
      window.location.hash = "#/audit?actor=agent&range=7d";
      render(<AuditTrail query="actor=agent&range=7d" />);
      fireEvent.click(screen.getByRole("button", { name: "Copy link to this view" }));
      expect(await screen.findByText("Link copied.")).toBeDefined();
      expect(writeText).toHaveBeenCalledWith(window.location.href);
    });

    it("downloads nothing when a page fails, and says the server's sentence", async () => {
      const files = saved();
      const fetch = vi.fn(async (url: string) => {
        if (url.startsWith("/api/v1/fleet")) return { ok: true, status: 200, json: async () => FLEET } as unknown as Response;
        if (url.includes("cursor=")) return { ok: false, status: 500, json: async () => ({ error: "internal", message: "the trail could not be read" }) } as unknown as Response;
        return { ok: true, status: 200, json: async () => (url.includes("limit=200") ? pages[0] : EMPTY) } as unknown as Response;
      });
      vi.stubGlobal("fetch", fetch);
      render(<AuditTrail query="" />);
      await screen.findByText(/^Nothing happened/);
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      expect((await screen.findByText("the trail could not be read")).className).toContain("text-critical");
      expect(files).toHaveLength(0);
    });

    it("runs one export at a time, and closes the menu on a choice", async () => {
      const files = saved();
      server(EMPTY, { ...pages[0]!, next: null }, { ...pages[0]!, next: null });
      render(<AuditTrail query="" />);
      await screen.findByText(/^Nothing happened/);
      const menu = screen.getByText("Export ▾").closest("details")!;
      menu.open = true;
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      fireEvent.click(screen.getByRole("button", { name: "CSV" }));
      expect(menu.open).toBe(false);
      await screen.findByText(/^Exported/);
      expect(files).toHaveLength(1);
    });

    it("says so when there is no clipboard to copy to", async () => {
      vi.stubGlobal("navigator", {});
      server(EMPTY);
      render(<AuditTrail query="" />);
      fireEvent.click(screen.getByRole("button", { name: "Copy link to this view" }));
      expect(await screen.findByText("The link couldn't be copied.")).toBeDefined();
    });
  });
});
