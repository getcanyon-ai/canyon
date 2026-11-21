import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Receipt } from "../api";
import { S } from "../strings";
import { Shell } from "./Shell";

// The receipt is the golden's, read for a person in the dashboard; nothing here writes a rule down.
const page = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")) as { status: { receipt: Receipt } };
const patch = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/patch/claims-portal.json"), "utf8")) as { approval: Record<string, unknown>; rebuild: { approval: Record<string, unknown> } };
const CODE = "K7QP-4RXM";
const APPROVAL = `/api/v1/promotion-approvals/${CODE}`;
// The screen says "today" and "tomorrow" by the clock, so each test sets it: at midday an approval asked a minute
// before expires tomorrow, where the real clock's first minute after midnight made that today (#2709).
const NOON = new Date(2026, 10, 8, 12, 0);
const at = new Date(NOON.getTime() - 60_000);
const pending = {
  code: CODE, app: "claims-portal", version: "v1.4", replaces: "v1.3", state: "pending",
  requested_by: { kind: "person", subject: "sub-carol", display: "Carol Diaz" }, requested_via: "cli",
  requested_at: at.toISOString(), expires_at: new Date(at.getTime() + 86_400_000).toISOString(),
  receipt: page.status.receipt, approver: { display: "Dana Whitfield", email: "dana@meridian.example" },
};
const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;

// The read answers from `read`, production's image read from `image`, approve 202s with the run, deny 204s, everything else 404s.
const IMAGE = "/api/v1/projects/claims-portal/envs/prod/image";
let image = () => answer(404, { error: "not_found", message: "nothing is deployed to prod" });
function server(read: () => Response) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === IMAGE) return image();
    if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
    if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" });
    if (init?.method === "POST" && url === `${APPROVAL}/approve`) return answer(202, { run: { id: "gr_9c41", state: "pending" } });
    if (init?.method === "POST" && url === `${APPROVAL}/deny`) return answer(204, null);
    if ((init?.method ?? "GET") === "GET" && url === APPROVAL) return read();
    return answer(404, { error: "not_found", message: "no approval has that code" });
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`).filter((line) => line.includes("promotion-approvals"));
}

const open = (read = () => answer(200, pending)) => {
  window.location.hash = `#/approve?code=${CODE}`;
  const sent = server(read);
  render(<Shell />);
  return sent;
};

beforeEach(() => { vi.setSystemTime(NOON); });
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("the production approval screen", () => {
  it("says who asked and from where, what goes live, the checks and who is approving, and approves nothing until the click", async () => {
    const sent = open();
    expect(await screen.findByRole("heading", { name: "Promote claims-portal v1.4 to production?" })).toBeDefined();
    expect(screen.getByText("Production approval")).toBeDefined();
    expect(screen.getByText((_, node) => node?.tagName === "P" && (node.textContent ?? "").startsWith("Carol Diaz asked from the CLI,"))).toBeDefined();
    const versions = within(screen.getByRole("region", { name: "What changes" }));
    expect(versions.getByText("In production now").nextElementSibling?.textContent).toBe("v1.3");
    expect(versions.getByText("Goes live").nextElementSibling?.textContent).toBe("v1.4");
    expect(screen.getByText("Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.")).toBeDefined();
    const checks = within(screen.getByRole("region", { name: "Go-live checklist" }));
    expect(checks.getByText("Go-live checklist")).toBeDefined(); // a visible heading over the bordered list
    for (const rule of page.status.receipt.rules) expect(checks.getByText(rule.why)).toBeDefined();
    const strip = screen.getByRole("region", { name: "Approving as" });
    expect(strip.textContent).toContain("Approving as Dana Whitfield");
    expect(within(strip).getByText("dana@meridian.example")).toBeDefined();
    expect(within(strip).getByText("Dana Whitfield").tagName).toBe("STRONG");
    expect(strip.nextElementSibling?.contains(screen.getByRole("button", { name: "Approve" }))).toBe(true);
    expect(screen.getByText(/^Expires \S+( \S+)? tomorrow\.$/)).toBeDefined(); // a day on: said as tomorrow, not a date
    expect(screen.getByText(CODE).parentElement?.textContent).toBe(`Code ${CODE}`); // to match with what the asker pasted
    expect(sent()).toEqual([`GET ${APPROVAL}`]);
  });

  it("says an approval asked a minute before midnight expires today, not tomorrow", async () => {
    vi.setSystemTime(new Date(2026, 10, 8, 0, 0, 30));
    const asked = new Date(2026, 10, 7, 23, 59, 30);
    const expires = new Date(asked.getTime() + 86_400_000);
    open(() => answer(200, { ...pending, requested_at: asked.toISOString(), expires_at: expires.toISOString() }));
    expect(await screen.findByText(`Expires ${expires.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} today.`)).toBeDefined();
  });

  it("approving starts the promotion and sends the person to the app's page", async () => {
    const sent = open();
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    expect(await screen.findByRole("heading", { name: "Promotion started" })).toBeDefined();
    expect(screen.getByText((_, node) => node?.tagName === "P" && node.textContent === "claims-portal v1.4 is going live. Follow it on its page →")).toBeDefined();
    expect(screen.getByRole("link", { name: "Follow it on its page →" }).getAttribute("href")).toBe("#/apps/claims-portal");
    expect(sent()).toEqual([`GET ${APPROVAL}`, `POST ${APPROVAL}/approve`]);
  });

  // The version asked for answered an error on preview: the page says so, and Approve is the person's
  // override, sent as promote_anyway (contracts/routes.md, Production approvals).
  it("says what preview answered when it is an error, and approving sends the override", async () => {
    open(() => answer(200, { ...pending, answered: { error: "refused" } }));
    expect(await screen.findByText("v1.4 gave no answer at / on preview. Check the app before approving it.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    await screen.findByRole("heading", { name: "Promotion started" });
    const calls = vi.mocked(globalThis.fetch).mock.calls.filter(([url, init]) => url === `${APPROVAL}/approve` && init?.method === "POST");
    expect(calls.map(([, init]) => init?.body)).toEqual([JSON.stringify({ promote_anyway: true })]);
  });

  it("an approval whose preview answered nothing odd sends no override", async () => {
    open(() => answer(200, { ...pending, answered: { status: 404, said: "" } }));
    fireEvent.click(await screen.findByRole("button", { name: "Approve" }));
    await screen.findByRole("heading", { name: "Promotion started" });
    expect(screen.queryByText(/on preview\. Check the app/)).toBeNull();
    const calls = vi.mocked(globalThis.fetch).mock.calls.filter(([url, init]) => url === `${APPROVAL}/approve` && init?.method === "POST");
    expect(calls.map(([, init]) => init?.body)).toEqual([undefined]);
  });

  it("denying promotes nothing", async () => {
    const sent = open();
    fireEvent.click(await screen.findByRole("button", { name: "Deny" }));
    expect(await screen.findByRole("heading", { name: "Denied" })).toBeDefined();
    expect(screen.getByText("Nothing was promoted.")).toBeDefined();
    expect(sent()).toEqual([`GET ${APPROVAL}`, `POST ${APPROVAL}/deny`]);
  });

  // Approve used to vanish: a disabled button always says why (D6), and here the checklist is why.
  it("keeps Approve in its place, disabled with its reason, while the checks stop the promotion", async () => {
    const receipt = { ...page.status.receipt, blocked_by: ["prod.clean-gate"], rules: page.status.receipt.rules.map((rule) => rule.id === "prod.clean-gate" ? { ...rule, passed: false, blocks: true } : rule) };
    const sent = open(() => answer(200, { ...pending, receipt }));
    const box = (await screen.findByText("This can't go to production now")).parentElement!;
    const review = receipt.rules.find((rule) => rule.id === "prod.clean-gate")!;
    expect(within(box).getByText((_, node) => node?.tagName === "SPAN" && (node.textContent ?? "").startsWith(`${review.name}.`))).toBeDefined();
    expect(within(box).getByRole("link", { name: "Open the review →" }).getAttribute("href")).toBe("#/apps/claims-portal");
    const approve = screen.getByRole("button", { name: "Approve" }) as HTMLButtonElement;
    expect(approve.disabled).toBe(true);
    expect(screen.getByText("Blocked by the go-live checklist.")).toBeDefined();
    fireEvent.click(approve);
    expect(screen.getByRole("button", { name: "Deny" })).toBeDefined();
    expect(sent()).toEqual([`GET ${APPROVAL}`]);
  });

  it("a first release reads nothing yet on the production side", async () => {
    open(() => answer(200, { ...pending, replaces: null }));
    const versions = within(await screen.findByRole("region", { name: "What changes" }));
    expect(versions.getByText("In production now").nextElementSibling?.textContent).toBe("Nothing yet");
    expect(versions.getByText("Goes live").nextElementSibling?.textContent).toBe("v1.4");
  });

  it.each([
    [{ state: "approved", decided_by: { kind: "person", subject: "sub-priya", display: "Priya Raman" } }, "Already approved by Priya Raman", "Nothing more to do here."],
    [{ state: "denied", decided_by: { kind: "person", subject: "sub-priya", display: "Priya Raman" } }, "Already denied by Priya Raman", "Nothing more to do here."],
    [{ state: "expired" }, "This approval expired", "Ask for a new one from the CLI."],
  ])("a decided or expired approval says so, says what now, and offers nothing: %o", async (over, title, line) => {
    open(() => answer(200, { ...pending, ...over }));
    expect(await screen.findByRole("heading", { name: title })).toBeDefined();
    expect(screen.getByText(line)).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
  });

  it("an approval preview outdated says preview moved on, and can still be denied", async () => {
    open(() => answer(200, { ...pending, state: "outdated" }));
    expect(await screen.findByRole("heading", { name: "Preview runs another version now" })).toBeDefined();
    expect(screen.getByText("Ask for a new approval from the CLI.")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.getByRole("button", { name: "Deny" })).toBeDefined();
  });

  // A hold turned it outdated (#2243): the screen says production is held, who held it, when and why, in
  // D5's held sentence — never that preview moved — and offers nothing, since the hold closed it.
  it("an approval a hold outdated says the hold, and offers nothing", async () => {
    const held = { by: { kind: "person", subject: "sub-rhea", display: "Rhea Okafor" }, at: "2026-10-14T09:12:00Z", reason: "Exports claimant SSNs unauthenticated (INC-2231)" };
    open(() => answer(200, { ...pending, state: "outdated", held }));
    expect(await screen.findByRole("heading", { name: "Production is held" })).toBeDefined();
    expect(screen.getByText("Production was stopped by Rhea Okafor on 2026-10-14: Exports claimant SSNs unauthenticated (INC-2231). It stays stopped until a responder or an admin lets it run again.")).toBeDefined();
    expect(screen.queryByText("Preview runs another version now")).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Deny" })).toBeNull();
  });

  it("below contributor, the server's ask is the screen", async () => {
    open(() => answer(403, { error: "team_required", message: "Ask Dana Whitfield to add you to claims-portal", owners: [] }));
    expect(await screen.findByText("Ask Dana Whitfield to add you to claims-portal")).toBeDefined();
  });

  // A patch approval (contracts/routes.md, Production approvals; design/PATCHING.md §8), Canyon's own ask, as the patch
  // golden files it: what changes, the proof, preview's watch result, then D5's confirm.
  const patched = { ...patch.approval, code: CODE, requested_at: at.toISOString(), expires_at: new Date(at.getTime() + 86_400_000).toISOString(), receipt: page.status.receipt };

  it("a patch approval says its proof: what changes, that only Canyon's system libraries changed, preview's watch, then D5's confirm", async () => {
    open(() => answer(200, patched));
    expect(await screen.findByRole("heading", { name: "Promote Canyon's patch of claims-portal to production?" })).toBeDefined();
    expect(screen.getByText((_, node) => node?.tagName === "P" && (node.textContent ?? "").startsWith("Canyon asked,"))).toBeDefined();
    // One version on both sides: the image tells them apart, like with like (#2593): production's image now and the
    // patched image that goes live, and no image of Canyon's system libraries, which only Settings › Patching names (#2605).
    const versions = within(screen.getByRole("region", { name: "What changes" }));
    expect(versions.getByText("In production now").nextElementSibling?.textContent).toBe("v1.4");
    expect(versions.getByText("Goes live").nextElementSibling?.textContent).toBe("v1.4");
    const side = (label: string) => [...(versions.getByText(label).parentElement?.querySelectorAll("span") ?? [])].slice(2).map((line) => line.textContent);
    expect(side("In production now")).toEqual(["image 4f1c0b9a7e2a"]);
    expect(side("Goes live")).toEqual(["image 6a6a6a6a6a6a"]);
    const fixes = within(screen.getByRole("region", { name: "What the patch fixes" }));
    expect(fixes.getByText("libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5 (CVE-2024-5535)")).toBeDefined();
    expect(fixes.getByText("The app's own files are byte for byte the ones in production.")).toBeDefined();
    expect(fixes.getByText("The watch passed: the patched app neither exited nor restarted")).toBeDefined();
    const confirm = screen.getByText("Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.");
    expect(screen.getByRole("region", { name: "What the patch fixes" }).compareDocumentPosition(confirm) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy(); // the confirm comes last
    expect(screen.getByRole("button", { name: "Approve" })).toBeDefined();
  });

  // A rebuild's approval says the rebuild's own proof, as its read gives it, never that the app's layers are the same (#2117).
  it("a rebuild approval says it was rebuilt, and that a person on the team decides", async () => {
    open(() => answer(200, { ...patch.rebuild.approval, code: CODE, requested_at: at.toISOString(), expires_at: new Date(at.getTime() + 86_400_000).toISOString(), receipt: page.status.receipt }));
    const fixes = within(await screen.findByRole("region", { name: "What the patch fixes" }));
    expect(fixes.getByText("cross-spawn 7.0.3 → 7.0.5 (CVE-2024-21538)")).toBeDefined();
    expect(fixes.getByText("Rebuilt from the same code and the same dependencies with a newer Node.js. A rebuild is a new program, not a copy of what production runs, so a person on your team decides.")).toBeDefined();
    expect(fixes.queryByText("The app's own files are byte for byte the ones in production.")).toBeNull();
    // #2631: its confirm never says nothing is rebuilt beside a proof that says it was
    expect(screen.getByText("Production gets the image preview ran and Canyon watched, with production's own secrets. Approving doesn't build it again.")).toBeDefined();
    expect(screen.queryByText(/Nothing is rebuilt/)).toBeNull();
  });

  // #2579: the checklist's known-vulnerable row is the patched image's, so the patch's part says what remains of it and
  // whether production already runs all of that, by production's own image read; anything unread can't tell.
  const remaining = { id: "platform.known-vuln", name: "No known-vulnerable system libraries or build tools", blocks: false, passed: false, read_at: "2026-10-30T09:00:00Z", notes: 14,
    why: "2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 14 notes: no fix published, or Canyon can't tell what put it there",
    packages: [
      { name: "libbrotli1", version: "1.1.0-2build2", advisories: [{ id: "CVE-2025-6176", fixed: "1.1.0-2ubuntu0.1" }], layers: [{ owner: "run-image", class: "os" as const }] },
      { name: "stdlib", version: "v1.26.5", go_stdlib: true as const, advisories: [{ id: "CVE-2026-33818", fixed: "1.26.9, 1.27.2" }], layers: [{ owner: "lifecycle", class: "lifecycle" as const }] },
    ] };
  const leaves = { ...patched, receipt: { ...page.status.receipt, rules: [...page.status.receipt.rules, remaining] },
    fixes: [{ name: "libssl3", from: "3.0.13-0ubuntu3.4", to: "3.0.13-0ubuntu3.5", vulnerability: "CVE-2024-5535", class: "os" }, { name: "libssl3", from: "3.0.13-0ubuntu3.4", to: "3.0.13-0ubuntu3.5", vulnerability: "CVE-2024-6119", class: "os" }] };
  const production = (vulnerabilities: [string, string, string][], state = "checked") => () => answer(200, { state, findings: vulnerabilities.map(([name, version, vulnerability]) => ({ dependency: { name, version, vulnerability } })) });
  const both: [string, string, string][] = [["libbrotli1", "1.1.0-2build2", "CVE-2025-6176"], ["stdlib", "v1.26.5", "CVE-2026-33818"], ["libssl3", "3.0.13-0ubuntu3.4", "CVE-2024-5535"]];
  afterEach(() => { image = () => answer(404, { error: "not_found", message: "nothing is deployed to prod" }); });

  it.each([
    ["production runs every one", production(both), "Production already runs them, so approving adds none."],
    ["production lacks one", production(both.slice(1)), "Production doesn't run all of them: approving adds some."],
    ["production's read could not check", production(both, "could_not_check"), "Canyon can't tell whether production already runs them."],
    ["production's read is refused", () => answer(403, { error: "team_required", message: "Ask Dana Whitfield to add you to claims-portal" }), "Canyon can't tell whether production already runs them."],
  ])("a patch approval says what it fixes, one line a package, and what remains: %s", async (_, read, line) => {
    image = read;
    open(() => answer(200, leaves));
    const part = within(await screen.findByRole("region", { name: "What the patch fixes" }));
    expect(part.getByText("libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5 (CVE-2024-5535, CVE-2024-6119)")).toBeDefined();
    expect(part.getByText("Still there after this patch")).toBeDefined();
    expect(await part.findByText(`2 known-vulnerable packages, listed in the checklist below. ${line}`)).toBeDefined();
  });

  // One package left reads in the singular throughout (#2591): never "1 known-vulnerable package … runs them".
  const one = { ...remaining, why: "1 known-vulnerable package with a published fix, in the system libraries and build tools Canyon adds; 14 notes: no fix published, or Canyon can't tell what put it there",
    packages: remaining.packages.slice(0, 1) };
  it.each([
    ["production runs it", production(both), "Production already runs it, so approving adds nothing."],
    ["production lacks it", production(both.slice(1)), "Production doesn't run it yet: approving adds it."],
    ["production's read could not check", production(both, "could_not_check"), "Canyon can't tell whether production already runs it."],
  ])("a patch approval with one package left says it in the singular: %s", async (_, read, line) => {
    image = read;
    open(() => answer(200, { ...leaves, receipt: { ...leaves.receipt, rules: [...page.status.receipt.rules, one] } }));
    const part = within(await screen.findByRole("region", { name: "What the patch fixes" }));
    expect(await part.findByText(`1 known-vulnerable package, listed in the checklist below. ${line}`)).toBeDefined();
  });

  it("a patch approval says when nothing remains, or that Canyon couldn't check what does", async () => {
    image = production(both);
    open(() => answer(200, { ...leaves, receipt: { ...leaves.receipt, rules: [...page.status.receipt.rules, { ...remaining, passed: true, why: "no known-vulnerable package with a published fix, in the system libraries and build tools Canyon adds", packages: undefined }] } }));
    expect(await screen.findByText("Nothing else known-vulnerable with a published fix.")).toBeDefined();
    cleanup();
    open(() => answer(200, { ...leaves, receipt: { ...leaves.receipt, rules: [...page.status.receipt.rules, { id: remaining.id, name: remaining.name, blocks: false, passed: false, why: "could not check: the image has not been matched yet" }] } }));
    expect(await screen.findByText("Canyon couldn't check what remains: the checklist below says why.")).toBeDefined();
    expect(screen.queryByText(/production already runs/i)).toBeNull();
  });

  // The approver reads what ships, as the CLI's question names it (D5): a rollback's names the version going back and
  // what it replaces, or none where production runs nothing.
  it.each([
    [{ rollback: true, version: "v1.2", replaces: "v1.4" }, "Put claims-portal v1.2 back in production, replacing v1.4?"],
    [{ rollback: true, version: "v1.2", replaces: null }, "Put claims-portal v1.2 back in production?"],
  ])("a rollback's question names both versions: %j", async (change, title) => {
    open(() => answer(200, { ...pending, ...change }));
    expect(await screen.findByRole("heading", { name: title })).toBeDefined();
    expect(screen.getByText(S.putBackCodeBare)).toBeDefined(); // production's own earlier image, never preview's
    expect(screen.queryByText(S.promoteConfirm)).toBeNull();
  });

  // Nobody approves without seeing what ships: until the read names the version, there is nothing to approve, while it
  // is on its way and when it fails alike.
  it("offers no Approve until the read names the version", async () => {
    let answerRead: (r: Response) => void = () => {};
    const sent = open(() => new Promise<Response>((resolve) => { answerRead = resolve; }) as unknown as Response);
    await waitFor(() => expect(sent()).toContain(`GET ${APPROVAL}`));
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    answerRead(answer(502, { error: "bad_gateway", message: "bad gateway" }));
    expect(await screen.findByRole("heading", { name: "Promote this app to production?" })).toBeDefined();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
    expect(sent()).toEqual([`GET ${APPROVAL}`]);
  });

  it("a person's approval carries no patch part", async () => {
    open();
    await screen.findByRole("heading", { name: "Promote claims-portal v1.4 to production?" });
    expect(screen.queryByRole("region", { name: "What the patch fixes" })).toBeNull();
    expect(screen.queryByText("The app's own files are byte for byte the ones in production.")).toBeNull();
    expect(screen.queryByText(/^(run )?image /)).toBeNull();
  });

  it.each([
    [{ state: "expired" }, "This approval expired"],
    [{ state: "outdated" }, "This approval is out of date"],
  ])("a patch approval that expired or is out of date says Canyon asks again, never the CLI: %o", async (over, title) => {
    open(() => answer(200, { ...patched, ...over }));
    expect(await screen.findByRole("heading", { name: title })).toBeDefined();
    expect(screen.getByText("Canyon asks again if the patch still applies.")).toBeDefined();
    expect(screen.queryByText(/from the CLI/)).toBeNull();
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull();
  });

  it("a code no approval has says so in the screen's own words", async () => {
    open(() => answer(404, { error: "not_found", message: "no approval has that code" }));
    expect(await screen.findByText("That code isn't valid, or a newer request replaced it.")).toBeDefined();
    await waitFor(() => expect(screen.queryByText("no approval has that code")).toBeNull());
  });
});
