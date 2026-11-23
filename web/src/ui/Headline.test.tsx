import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Env, Finding, Need, Receipt, RunRead } from "../api";
import { S } from "../strings";
import { headline, Headline, heldLine, leads, viewerBand, type Facts } from "./Headline";

const env = (pkg: string, label: string, extra: Partial<Env> = {}): Env => ({ url: `https://${label}.example`, package: pkg, label, image_digest: "sha256:0", deployed_at: "2026-10-01T00:00:00Z", gate_run: "gr_x", deployed_by: null, state: "running", ...extra });
const ready: Receipt = { org: "Meridian", blocked_by: [], rules: [{ id: "prod.clean-gate", name: "Passed review", blocks: true, passed: true, why: "gr_x ended clean" }] };
// The owner check is advisory and unmet: nothing blocks, one advisory is open (the pipeline's "Met, with advisories").
const advisory: Receipt = { ...ready, rules: [...ready.rules, { id: "app.has-owner", name: "Every app has an owner", blocks: false, passed: false, why: "no owner has claimed this app" }] };
const unmet: Receipt = { ...ready, blocked_by: ["prod.human-approval"], rules: [{ id: "prod.human-approval", name: "A person promotes", blocks: true, passed: false, why: "A person promotes it" }] };
let next = 0; // each finding its own entry unless a test names one: one problem a finding
const finding = (ruleId: string, approval?: Finding["approval"], entry = next++): Finding => ({
  ruleId, entry, locations: [{ physicalLocation: { artifactLocation: { uri: "a.ts" }, region: { startLine: 1, startColumn: 1, snippet: { text: "" } } } }],
  partialFingerprints: { "canyon/v1": `f_${ruleId}` }, properties: { canyon: { severity: "high", blocks: true, title: ruleId, problem: "", fix: "" } }, ...(approval === undefined ? {} : { approval }),
});
const run = (extra: Partial<RunRead>): RunRead => ({ id: "gr_1", package: "pkg_14", state: "clean", blocking: 0, warnings: 0, findings: [], ...extra });
const v13 = env("pkg_13", "v1.3");
const facts = (extra: Partial<Facts>): Facts => ({
  latest: { package: "pkg_14", label: "v1.4", stage: "blocked", since: "2026-10-01T00:00:00Z" }, newest: "v1.4", neverReviewed: false,
  run: null, preview: v13, prod: null, previewName: "v1.3", prodName: null, receipt: ready, ...extra,
});
const at = (stage: Facts["latest"]["stage"]) => ({ latest: { package: "pkg_14", label: "v1.4", stage, since: "2026-10-01T00:00:00Z" } });

// One sentence about the newest version and the one thing to do, for each state the redesign draws,
// from the server's facts alone (the app-page redesign, block 1).
describe("the headline", () => {
  it.each<[string, Partial<Facts>, string | RegExp, string | RegExp, string | undefined, string | undefined]>([
    ["production stopped outranks the rest", { prod: env("pkg_13", "v1.3", { state: "stopped", stopped: { exit_code: 1, said: "connection refused" } }), prodName: "v1.3", run: run({ state: "blocked", blocking: 1, findings: [finding("x")] }) },
      "Production is down: v1.3 stopped with exit code 1", /^Preview runs the same version and is still up\. The app's last line is below/, undefined, undefined],
    ["a review going", { run: run({ state: "running" }), ...at("reviewing") }, "Reviewing v1.4", /Nothing for you to do\.$/, undefined, "cancel"],
    // Who approves names the roles, never the team (#2204).
    ["blocked by findings", { run: run({ state: "blocked", blocking: 2, findings: [finding("a"), finding("b")] }) }, "v1.4 is blocked: 2 blocking findings", /^Fix them in the code and deploy again, or ask an admin or a security responder to approve each with a reason\. .* v1\.3 keeps running in preview\.$/, undefined, "approve"],
    // Two rules on one line are one problem, as the CLI says (contracts/finding.md, One problem, one entry; #2013).
    ["blocked by one rule on one line", { run: run({ state: "blocked", blocking: 1, findings: [finding("a", undefined, 90), finding("b", undefined, 90)] }) }, "v1.4 is blocked: 1 blocking finding", /^Fix it in the code and deploy again, or ask an admin or a security responder to approve it with a reason\. /, undefined, "approve"],
    ["blocked by checks that couldn't finish", { run: run({ state: "blocked", blocking: 2, findings: [finding("gate.unreadable"), finding("gate.unreadable")] }) },
      "v1.4 is blocked: 2 of Canyon's checks couldn't finish", /^A check that didn't run can't vouch for the code/, "review", "approve"],
    ["every blocker approved", { run: run({ state: "blocked", blocking: 1, findings: [finding("a", { reason: "r", expires_at: "2099-01-01T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } })] }), ...at("approved") },
      "v1.4 was approved past its review", /^Approved — 1 blocking finding, approved by Dana until 1 Jan\. Approved is not passed/, undefined, undefined],
    ["a build that failed after review", { run: run({ state: "failed", failure_reason: "build" }), ...at("failed") }, "v1.4 passed review, then didn't build", /^Its last line says why\./, undefined, "copy"],
    ["an app that didn't stay up", { run: run({ state: "failed", failure_reason: "start" }), ...at("failed") }, "v1.4 built, then stopped right after it started", /^It passed review and built\. Its last lines say why\./, undefined, "copy"],
    ["a starter missing its file", { run: run({ state: "failed", failure_reason: "start", fix: { file: "Procfile", why: "No start command.", examples: [] } }), ...at("failed") }, "v1.4 is missing a Procfile", /^No start command\./, undefined, undefined],
    ["a host that builds no such app", { run: run({ state: "failed", failure_reason: "detect", builds: ["Paketo Buildpack for Go"] }), ...at("failed") }, "v1.4 couldn't be built on this host", /^Canyon builds Go apps\. Nothing was compiled/, undefined, undefined],
    ["Canyon's own failure", { run: run({ state: "failed", failure_reason: "error", failed_in: "scan" }), ...at("failed") }, "v1.4's review didn't finish", /^Canyon itself failed while scanning the package\. Run it again/, "review", undefined],
    ["never reviewed", { neverReviewed: true, ...at("awaiting_review") }, "v1.4 hasn't been reviewed", S.neverReviewed, "review", undefined],
    ["nothing uploaded", { latest: { package: null, stage: "created", since: "2026-10-01T00:00:00Z" }, newest: null }, "Nothing uploaded yet", /canyon deploy/, undefined, undefined],
    ["live", { ...at("production"), run: run({}), preview: env("pkg_14", "v1.4"), prod: env("pkg_14", "v1.4"), previewName: "v1.4", prodName: "v1.4" }, "v1.4 is live in production", "Preview runs the same version.", "open", undefined],
    ["live, with something not checked", { ...at("production"), run: run({ not_checked: true }), preview: null, prod: env("pkg_14", "v1.4"), previewName: null, prodName: "v1.4" }, "v1.4 is live in production", /^Nothing blocking in what was checked; some things weren't checked\.$/, "open", undefined],
    ["in production, unchecked", { ...at("production"), run: run({}), preview: null, prod: env("pkg_14", "v1.4", { state: "unknown" }), previewName: null, prodName: "v1.4" }, "v1.4 is in production, but Canyon couldn't check whether it runs", /^canyon logs --env prod/, "open", undefined],
    ["stopped in preview", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { state: "stopped", stopped: { exit_code: 1, said: "boom" } }), previewName: "v1.4" }, "v1.4 stopped in preview with exit code 1", /^The app's last line is below/, undefined, undefined],
    ["ready to promote", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4" }, "v1.4 is ready for production", /nothing is rebuilt\.$/, "promote", undefined],
    // What preview answered at / is said beside the checks it meets: a 400 never reads ready (the test cluster's stock Django).
    ["meets the checks, but answered 400", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { status: 400, said: "Bad Request" } }), previewName: "v1.4" },
      "v1.4 meets the go-live check, but answered 400 at /", /^Check the app before promoting: the preview card shows what it said\./, "promote", undefined],
    ["meets the checks, but gave no answer", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { error: "connection refused" } }), previewName: "v1.4" },
      "v1.4 meets the go-live check, but gave no answer at /", /^Check the app before promoting/, "promote", undefined],
    // A 401 or a 404 at / can be right (contracts/routes.md, answered: never a judgement): an app's own
    // sign-in, or an API with no page at its root.
    ["a 401 at / is an answer", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { status: 401, said: "" } }), previewName: "v1.4" }, "v1.4 is ready for production", /nothing is rebuilt\.$/, "promote", undefined],
    ["a 404 at / is an answer", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { status: 404, said: "" } }), previewName: "v1.4" }, "v1.4 is ready for production", /nothing is rebuilt\.$/, "promote", undefined],
    // One count with the pipeline: an unmet advisory is never "all met" (the test cluster: "meets all 3" beside "2 of 3").
    ["ready, with an advisory open", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4", receipt: advisory }, "v1.4 is ready for production", /meets 1 of 2 go-live checks, with 1 advisory open\./, "promote", undefined],
    ["ready, with advisories open", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4", receipt: { ...advisory, rules: [...advisory.rules, { id: "app.described", name: "Every app is described", blocks: false, passed: false, why: "no description" }] } },
      "v1.4 is ready for production", /meets 1 of 3 go-live checks, with 2 advisories open\./, "promote", undefined],
    ["answered 400, with an advisory open", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { status: 400, said: "" } }), previewName: "v1.4", receipt: advisory },
      "v1.4 meets 1 of 2 go-live checks, with 1 advisory open, but answered 400 at /", /^Check the app/, "promote", undefined],
    ["a redirect at / is an answer", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { answered: { status: 302, said: "" } }), previewName: "v1.4" }, "v1.4 is ready for production", /nothing is rebuilt\.$/, "promote", undefined],
    // case-dies-later in one of its up-windows: started again since its deploy, so never ready (#1722)
    ["restarting in preview", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 7 } }), previewName: "v1.4" },
      "v1.4 restarted 7 times since it was deployed to preview", /^Deploy again once it's fixed: the build is reused\.( |$)/, undefined, undefined],
    ["restarting in production", { ...at("production"), run: run({}), preview: null, prod: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 1 } }), previewName: null, prodName: "v1.4" },
      "v1.4 restarted once since it was deployed to production", "Deploy again once it's fixed: the build is reused.", undefined, undefined],
    // how it last exited, when the status read says it: the test cluster's case-dies-later never reads stopped on Kubernetes (#1597)
    ["restarting in preview, its last exit said", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 7, exit_code: 1, said: "lost the queue: exiting" } }), previewName: "v1.4" },
      "v1.4 restarted 7 times since it was deployed to preview", /^It last exited with code 1\. The app's last line is below\. Deploy again once it's fixed: the build is reused\. canyon logs --env preview has the rest\./, undefined, undefined],
    ["restarting in production, its last exit said", { ...at("production"), run: run({}), preview: null, prod: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 1, exit_code: 137, said: "" } }), previewName: null, prodName: "v1.4" },
      "v1.4 restarted once since it was deployed to production", "It last exited with code 137. Deploy again once it's fixed: the build is reused. canyon logs --env prod has the rest.", undefined, undefined],
    ["approved, and in preview", { ...at("preview"), run: run({ state: "blocked", blocking: 1, findings: [finding("a", { reason: "r", expires_at: "2099-01-01T00:00:00Z", approved_by: { kind: "person", subject: "dana", display: "Dana" } })] }), preview: env("pkg_14", "v1.4"), previewName: "v1.4" },
      "v1.4 is ready for production", /nothing is rebuilt\.$/, "promote", undefined],
    ["a go-live check not met", { ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4", receipt: unmet }, "v1.4 isn't ready for production", "A person promotes it", undefined, undefined],
    ["passed and deployed nowhere", { ...at("passed"), run: run({}) }, "v1.4 passed review", /^It isn't running anywhere yet\./, undefined, undefined],
  ])("%s", (_, given, title, text, primary, secondary) => {
    const band = headline(facts(given));
    if (typeof title === "string") expect(band.title).toBe(title);
    else expect(band.title).toMatch(title);
    if (typeof text === "string") expect(band.text).toBe(text);
    else expect(band.text).toMatch(text);
    expect([band.primary, band.secondary]).toEqual([primary, secondary]);
  });

  // A check that couldn't finish offers Review again only where a second try can finish it (#2723): a scanner's database
  // over five days old, a tool this host has no image for or one that couldn't load its rules fail the same way again,
  // so the headline says an admin fixes the host, and approving stays the way past it.
  it("offers Review again only where it can help, and otherwise says who fixes the host and who approves meanwhile", () => {
    const unread = (problem: string) => ({ ...finding("gate.unreadable"), properties: { canyon: { severity: "high" as const, blocks: true, title: "Canyon could not check this package", problem, fix: "" } } });
    const blocked = (...problems: string[]) => headline(facts({ run: run({ state: "blocked", blocking: problems.length, findings: problems.map(unread) }) }));
    const lead = "A check that didn't run can't vouch for the code, so it blocks. Nothing found in the code blocks it. ";
    const until = " Until then, an admin or a security responder who didn't start this review can approve it with a reason. v1.3 keeps running in preview.";
    const stale = blocked("could not check: trivy: its vulnerability database is over 5 days old");
    expect([stale.title, stale.text, stale.primary, stale.secondary]).toEqual(["v1.4 is blocked: 1 of Canyon's checks couldn't finish",
      `${lead}Reviewing it again won't help until whoever installed Canyon updates trivy's vulnerability database.${until}`, undefined, "approve"]);
    for (const [reason, says] of [
      ["this Canyon host has no image for it", "Reviewing it again won't help until whoever installed Canyon gives this host opengrep's image, or an admin turns opengrep off in Guardrails → Scanners."],
      ["could not load all of its rules", "Reviewing it again won't help until whoever installed Canyon fixes opengrep's rules on this host."],
      ["did not answer SARIF", "Reviewing it again won't help until whoever installed Canyon fixes opengrep on this host."],
    ]) {
      const host = blocked(`could not check: opengrep: ${reason}`);
      expect([host.text, host.primary, host.secondary]).toEqual([`${lead}${says}${until}`, undefined, "approve"]);
    }
    const big = blocked("could not check: opengrep: answered more than Canyon reads");
    expect([big.text, big.primary, big.secondary]).toEqual([`${lead}Reviewing it again won't help: opengrep says more about this code than Canyon reads. An admin or a security responder who didn't start this review can approve it with a reason. v1.3 keeps running in preview.`, undefined, "approve"]);
    const mixed = blocked("could not check: checkov: did not finish in time", "could not check: grype: its vulnerability database is over 5 days old");
    expect([mixed.primary, mixed.text]).toEqual([undefined, expect.stringContaining("until whoever installed Canyon updates grype's vulnerability database.")]);
    // A database that couldn't be downloaded is downloaded again by the next scan (executor/scan.go): a review again can help.
    for (const reason of ["did not finish in time", "stopped before it finished", "its vulnerability database could not be downloaded"]) {
      const again = blocked(`could not check: trivy: ${reason}`);
      expect([again.text, again.primary, again.secondary]).toEqual([expect.stringMatching(/Review it again: the checks wait their turn instead of failing\./), "review", "approve"]);
    }
  });

  // A version live in production while Canyon refuses a call of it that waits on a person is never green and live
  // (#2723): it says what production can't reach and why, and its step opens the Outbound card's Allow.
  it("never says live in green while production waits on a person to allow a call", () => {
    const live = { ...at("production"), run: run({}), preview: env("pkg_14", "v1.4"), prod: env("pkg_14", "v1.4"), previewName: "v1.4", prodName: "v1.4" };
    const one = headline(facts({ ...live, refused: ["api.stripe.com"] }));
    expect([one.tone, one.title, one.text, one.primary]).toEqual(["amber", "v1.4 can't reach api.stripe.com in production",
      "Nobody has allowed api.stripe.com in production yet, so Canyon refuses the app's calls to it.", "outbound"]);
    expect(headline(facts({ ...live, refused: ["api.stripe.com", "hooks.slack.com", "api.openai.com"] })).title).toBe("v1.4 can't reach api.stripe.com in production · and 2 more");
    expect(headline(facts({ ...live, refused: [] })).title).toBe("v1.4 is live in production");
  });

  // Production's own last line rides under the sentence; a stopped environment never "keeps running".
  it("carries production's last line, and never says a stopped preview keeps running", () => {
    expect(headline(facts({ prod: env("pkg_13", "v1.3", { state: "stopped", stopped: { exit_code: 137, said: "" } }), prodName: "v1.3" })).said).toBeUndefined();
    expect(headline(facts({ prod: env("pkg_13", "v1.3", { state: "stopped", stopped: { exit_code: 1, said: "boom" } }), prodName: "v1.3" })).said).toBe("boom");
    expect(headline(facts({ ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 7, exit_code: 1, said: "boom" } }), previewName: "v1.4" })).said).toBe("boom");
    const stopped = headline(facts({ run: run({ state: "blocked", blocking: 1, findings: [finding("a")] }), preview: env("pkg_13", "v1.3", { state: "stopped" }) }));
    expect(stopped.text).not.toContain("keeps running");
  });

  // A read-only member is told where the app runs, never "Running" of one that stopped or wasn't checked.
  it.each<[string, Parameters<typeof viewerBand>[0], string]>([
    ["production running", { prod: { state: "running" }, preview: { state: "running" } }, "Running in production"],
    ["production stopped, preview running", { prod: { state: "stopped" }, preview: { state: "running" } }, "Running in preview"],
    ["both stopped", { prod: { state: "stopped" }, preview: { state: "stopped" } }, "Not running"],
    ["production not checked", { prod: { state: "unknown" } }, "Canyon couldn't check whether it runs"],
    ["preview restarting", { preview: { state: "restarting" } }, "Keeps restarting"],
    ["nowhere", undefined, "Not running"],
  ])("a read-only member: %s", (_, envs, title) => {
    expect(viewerBand(envs, "read-only").title).toBe(title);
  });

  // What broke leads in its own words: a build's last line; a start's the executor's reason, no line of the app's (#2352).
  it("carries a build's cause, and a start's reason", () => {
    const build = headline(facts({ run: run({ state: "failed", failure_reason: "build" }), ...at("failed"), broke: { cause: "error TS2322", reason: null } }));
    const start = headline(facts({ run: run({ state: "failed", failure_reason: "start" }), ...at("failed"), broke: { cause: null, reason: "restarted (Error, exit 1)" } }));
    expect([build.said, start.said]).toEqual(["error TS2322", "restarted (Error, exit 1)"]);
  });
});

// The hold's sentence fills its three places in one pass: a reason quoting a placeholder is said as written.
// A restarting app's band says where its reason is only where the app's last lines aren't beside it: the fleet's panel
// draws no lines, the app page does, its newest line last and in view, across the whole band on a phone (#2843).
describe("a restarting band", () => {
  afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
  const band = headline(facts({ ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4", { state: "restarting", restarting: { restarts: 3 } }), previewName: "v1.4" }));
  const lines = Array.from({ length: 20 }, (_, i) => `line ${i + 1}`).join("\n");
  it("says canyon logs has why where no lines are drawn", () => {
    render(<Headline band={band} />);
    expect(screen.getByText("Deploy again once it's fixed: the build is reused. canyon logs --env preview has why.")).toBeDefined();
  });
  it("leaves it out beside the lines, which end on the newest, in view, at the band's full width on a phone", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, headers: new Headers({ "Canyon-Log-Redaction": "masked" }), text: async () => lines } as unknown as Response)));
    const tall = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockReturnValue(640);
    // Where the lines stand the moment they are drawn, read as the DOM changes, as findByText's own observer reads it:
    // on a busy runner that is all a test sees before it asserts (#2907).
    let drawn: number | undefined;
    const watch = new MutationObserver(() => { const shown = document.querySelector("pre"); if (drawn === undefined && shown !== null) drawn = shown.scrollTop; });
    watch.observe(document.body, { childList: true, subtree: true });
    render(<Headline band={band} app="claims-portal" />);
    const region = within(screen.getByRole("region", { name: "Where it stands" }));
    const pre = (await region.findByText((_, node) => node?.tagName === "PRE")) as HTMLPreElement;
    watch.disconnect();
    expect(pre.textContent?.endsWith("line 20")).toBe(true);
    expect(drawn).toBe(640);
    expect(pre.closest("figure")?.className).toMatch(/max-\[620px\]:col-start-1/);
    expect(region.getByText("Deploy again once it's fixed: the build is reused.")).toBeDefined();
    expect(document.body.textContent).not.toContain("has why");
    tall.mockRestore();
  });
});

describe("the held sentence", () => {
  const by = { kind: "person" as const, subject: "rhea@meridian.example", display: "Rhea Okafor" };
  it("names who, the day in UTC and why", () => {
    expect(heldLine({ by, at: "2026-10-11T23:40:00Z", reason: "INC-2231" })).toBe("Production was stopped by Rhea Okafor on 2026-10-11: INC-2231. It stays stopped until a responder or an admin lets it run again.");
  });
  it("leaves a placeholder in the reason or the name as written", () => {
    expect(heldLine({ by: { ...by, display: "<reason>" }, at: "2026-10-11T09:40:00Z", reason: "leaks <date> and <who>" }))
      .toBe("Production was stopped by <reason> on 2026-10-11: leaks <date> and <who>. It stays stopped until a responder or an admin lets it run again.");
  });
  it("ends a reason once, whatever ends its sentence", () => { // #2723
    expect(heldLine({ by, at: "2026-11-08T09:00:00Z", reason: "stop it while we look." })).toBe("Production was stopped by Rhea Okafor on 2026-11-08: stop it while we look. It stays stopped until a responder or an admin lets it run again.");
    expect(heldLine({ by, at: "2026-11-08T09:00:00Z", reason: "leaking?", version: null })).toBe("Production was held by Rhea Okafor on 2026-11-08: leaking? It stays held until a responder or an admin lifts it.");
  });
  it("names the subject when the holder has no display name", () => {
    expect(heldLine({ by: { ...by, display: "" }, at: "2026-10-11T09:40:00Z", reason: "x" })).toContain("stopped by rhea@meridian.example on");
  });
});

// The band beside what needs a person (D15): it leads in its need's place where it says what the top one says, gives way to
// the needs where it says what one further down says, and production's green never stands beside a need of production.
describe("the band beside the needs", () => {
  const need = (kind: string, env: Need["env"], rank: number): Need => ({ kind, env, rank, since: null, you: true, ask: [] });
  const [secret, blockedNeed, refused, unowned, advisory] = [need("secret", null, 4), need("blocked", null, 6), need("refused", "prod", 3), need("unowned", null, 10), need("will_refuse", "prod", 7)];
  const live = { ...at("production"), run: run({}), preview: env("pkg_14", "v1.4"), prod: env("pkg_14", "v1.4"), previewName: "v1.4", prodName: "v1.4" };
  const blocked = headline(facts({ ...at("blocked"), run: run({ state: "blocked", blocking: 1, findings: [{ properties: { canyon: { blocks: true } }, entry: 0 } as unknown as Finding] }) }));

  it("leads in place of the top need it says, the others after it", () => {
    expect(leads(blocked, [blockedNeed, unowned])).toEqual({ band: blocked, first: true, needs: [unowned] });
  });
  it("gives way where a need further down says it, which keeps its place", () => {
    expect(leads(blocked, [refused, blockedNeed])).toEqual({ band: null, first: false, needs: [refused, blockedNeed] });
  });
  // A band covers a need only where it says that need's words and offers its steps (#2754's review): a secret still
  // needs "Rotate the key", and what production won't allow its Allow.
  it("never stands in for a secret, or for what production won't allow", () => {
    expect(leads(blocked, [secret])).toEqual({ band: blocked, first: false, needs: [secret] });
    const stopped: Receipt = { org: "Meridian", blocked_by: ["prod.egress-allowed"], rules: [{ id: "prod.egress-allowed", name: "x", blocks: true, passed: false, why: "production does not allow example.com" }] };
    const ready = headline(facts({ ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4", receipt: stopped }));
    expect(ready.title).toBe("v1.4 isn't ready for production");
    const refuses = { ...need("will_refuse", "prod", 6), blocks: true };
    expect(leads(ready, [refuses])).toEqual({ band: ready, first: false, needs: [refuses] });
  });
  // A person asked for production, so its approval is the one thing to do: the ready band's Promote would ask again for
  // what is already asked (the v0.11.0 role walk drew both), so the band gives way to the approval's Review and approve.
  it("gives way to a production approval asked, so Promote is never beside Review and approve", () => {
    const ready = headline(facts({ ...at("preview"), run: run({}), preview: env("pkg_14", "v1.4"), previewName: "v1.4" }));
    expect(ready.primary).toBe("promote");
    const asked = need("approval", "prod", 5);
    expect(leads(ready, [asked])).toEqual({ band: null, first: false, needs: [asked] });
    expect(leads(ready, [unowned]).band).toBe(ready);
  });
  // Production's green gives way to a need of production that ranks 6 or above; below that it stays, the needs under it.
  it("keeps production's green over needs that rank 7 and below, and draws them under it", () => {
    const green = headline(facts(live));
    expect(green.tone).toBe("green");
    expect(leads(green, [refused]).band).toBeNull();
    expect(leads(green, [secret]).band).toBeNull(); // a secret in a promoted version, not production's own need, is never beside green
    // production's amber can't-reach band names the refused calls the needs list: never drawn beside them
    const reach = headline(facts({ ...live, refused: ["api.stripe.com"] }));
    expect(reach.tone).toBe("amber");
    expect(leads(reach, [refused])).toEqual({ band: null, first: false, needs: [refused] });
    expect(leads(reach, [unowned]).band).toBe(reach);
    expect(leads(green, [advisory, unowned])).toEqual({ band: green, first: true, needs: [advisory, unowned] });
  });
});
