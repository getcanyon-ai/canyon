import { readFileSync } from "node:fs";
import { join } from "node:path";
import { render, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { AuditEvent } from "../api";
import { AUDIT_ACTIONS, AUDIT_CA_BYO, AUDIT_RECREATE_FAILED, AUDIT_ROUTE_MOVED } from "../strings";
import { AuditRow, folded, sentence } from "./Audit";

// The contract's trail: every action a row on it carries has to have plain words here.
const pages = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/claims-portal.json"), "utf8")).audit as { events: AuditEvent[] }[];
const dana = { kind: "person" as const, subject: "dana@meridian.example", display: "Dana Whitfield" };
const event = (action: string, detail: Record<string, unknown> = {}): AuditEvent => ({ id: "ev_1", ts: "2026-09-02T09:30:40Z", actor: dana, action, project: "claims-portal", detail });

afterEach(() => vi.useRealTimers());

// One dependency's findings approved together read as one row naming it (screens.md, The approval).
it("an approval of one dependency's findings names the dependency and how many", () => {
  const row = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/app-page/birthday-list-dependency-approval.json"), "utf8")).audit as AuditEvent;
  expect(sentence(row)).toBe("approved 5 problems in lodash 4.17.4 with a reason");
});

// A report Canyon attached itself is part of its review, ~5 a deploy: the app's Activity and the fleet's
// recent activity fold it, the audit page keeps it (the test cluster audit). A person's attach is theirs to see.
describe("which rows a review folds", () => {
  it("folds Canyon's own report attaches and nothing else", () => {
    const canyon = { kind: "canyon" as const, subject: "scan", display: "Canyon" };
    expect(folded({ ...event("attach report", { tool: "trivy" }), actor: canyon })).toBe(true);
    expect(folded(event("attach report", { tool: "codeql" }))).toBe(false);
    expect(folded({ ...event("verdict", { state: "clean" }), actor: canyon })).toBe(false);
  });
});

describe("an audit row's sentence", () => {
  it("has plain words for every action on the contract's trail, and none of the machine's", () => {
    for (const one of pages.flatMap((page) => page.events)) {
      expect(Object.keys(AUDIT_ACTIONS), one.action).toContain(one.action);
      expect(sentence(one)).not.toMatch(/api|pkg_|package|[<>{}]/);
    }
  });

  it("fills every word the map asks for", () => {
    const detail = { package: "pkg_9e40", label: "v1.4", name: "STRIPE_KEY", env: "prod", rule: "prod.clean-gate", after: "advisory", subject: "dana@meridian.example" };
    for (const action of Object.keys(AUDIT_ACTIONS)) expect(sentence(event(action, detail)), action).not.toMatch(/[<>]/);
  });

  for (const [action, detail, said] of [
    ["POST /api/v1/projects/{id}/packages", { package: "pkg_9e40", label: "v1.4" }, "uploaded v1.4"],
    // Forced past stale_base: the copy recorded another base, or none (contracts/routes.md), so it replaced a version it didn't start from (#2696).
    ["POST /api/v1/projects/{id}/packages", { package: "pkg_9e40", forced: true }, "uploaded 9e40, replacing a version it didn't start from"],
    ["POST /api/v1/projects/{id}/packages", { package: "pkg_2b17", kind: "check" }, "checked 2b17"],
    ["POST /api/v1/projects/{id}/promotions", { package: "pkg_9e40", gate_run: "gr_8f2a" }, "started a promotion of 9e40"],
    ["deploy prod", { package: "pkg_9e40", image_digest: "sha256:0", url: "https://x" }, "promoted 9e40 to production"],
    // Where a person approves production, the CLI asks and a person in the dashboard answers (design/AUTH.md §4.5).
    ["request promotion", { package: "pkg_9e40", label: "v1.4", gate_run: "gr_8f2a", code: "K7QP-4RXM" }, "asked a person to approve v1.4 for production"],
    ["approve promotion", { package: "pkg_9e40", label: "v1.4", gate_run: "gr_8f2a", code: "K7QP-4RXM" }, "approved v1.4 for production"],
    ["deny promotion", { package: "pkg_9e40", label: "v1.4", gate_run: "gr_8f2a", code: "K7QP-4RXM" }, "denied v1.4 for production"],
    ["build failed", { package: "pkg_9e40", label: "v1.4" }, "the build of v1.4 failed"],
    ["start failed", { package: "pkg_9e40" }, "9e40 didn't stay up after it started"],
    ["detect failed", { package: "pkg_9e40", label: "v1.4" }, "this host can't build v1.4's kind of app"],
    ["build had no room", { package: "pkg_9e40", label: "v1.4" }, "the cluster had no room to build v1.4"],
    // Canyon's roll of the sign-in sidecar after an upgrade names the env; the image stays in the detail (#2305).
    ["update sidecar", { env: "prod", image: "ghcr.io/getcanyon-ai/canyon-identity@sha256:fe", roll: "4f1c0b9a" }, "updated the sign-in sidecar in production after an upgrade"],
    ["PUT /api/v1/projects/{id}/secrets/{name}", { name: "STRIPE_KEY", env: "prod", where: "runtime" }, "set the secret STRIPE_KEY for production"],
    ["GET /api/v1/projects/{id}/logs", { env: "preview" }, "read the preview logs"],
    ["revoke sessions", { subject: "tomas@meridian.example" }, "signed tomas@meridian.example out everywhere"],
    ["policy.set", { rule: "prod.clean-gate", before: "blocking", after: "advisory" }, "set the policy prod.clean-gate to advisory"],
    // A change with a grace says when it bites: it was made now, and is in force only after the days.
    ["policy.set", { rule: "prod.clean-gate", before: "advisory", after: "blocking", grace_days: 14 }, "set the policy prod.clean-gate to blocking, in force in 14 days"],
    ["policy.set", { rule: "secrets.client-bundle", before: "blocking", after: "blocking", grace_days: 1, production: { before: "off", after: "blocking" } }, "put the policy secrets.client-bundle before production, blocking, in force in 1 day"],
    // The trail names the rule as the Guardrails page does; a row written before the name was recorded reads its id.
    ["policy.set", { rule: "secrets.client-bundle", name: "No secrets in the code", before: "advisory", after: "blocking" }, "set the policy No secrets in the code to blocking"],
    ["policy.set", { rule: "secrets.client-bundle", name: "No secrets in the code", before: "blocking", after: "blocking", production: { before: "blocking", after: "off" } }, "took the policy No secrets in the code out of before production"],
    // A before-production change says so, and placing reads apart from removing.
    ["policy.set", { rule: "secrets.client-bundle", before: "blocking", after: "blocking", production: { before: "off", after: "blocking" } }, "put the policy secrets.client-bundle before production, blocking"],
    ["policy.set", { rule: "secrets.client-bundle", before: "blocking", after: "blocking", production: { before: "blocking", after: "off" } }, "took the policy secrets.client-bundle out of before production"],
    ["policy.set", { rule: "secrets.client-bundle", before: "blocking", after: "blocking", production: { before: "blocking", after: "advisory" } }, "set the policy secrets.client-bundle before production to advisory"],
    ["policy.set", { rule: "secrets.client-bundle", before: "advisory", after: "blocking", production: { before: "off", after: "blocking" } }, "set the policy secrets.client-bundle to blocking, and put it before production, blocking"],
    ["custom_rule.set", { rule: "custom.no-pii", version: 2 }, "saved version 2 of the custom rule custom.no-pii"],
    ["DELETE /api/v1/projects/{id}/envs/{env}", { env: "preview", package: "pkg_9e40" }, "took preview offline, keeping its data"],
    ["delete resource", { name: "db", type: "postgres", env: "prod" }, "deleted the resource db in production, with its data"],
    ["gate run from github", { package: "pkg_9e40" }, "started a review of 9e40 from GitHub"],
    ["POST /api/v1/gate-runs/{id}/cancel", { package: "pkg_9e40", kind: "promote", started_by: { kind: "agent", subject: "claude-code:dana@meridian.example", display: "Claude Code" } }, "cancelled the promotion of 9e40 · started by Claude Code"],
    ["POST /api/v1/gate-runs/{id}/cancel", { package: "pkg_9e40", kind: "gate", started_by: null }, "cancelled the run of 9e40 · started by someone"],
    // The hold's two rows (design/OPERATIONS.md §3.9, #2244): a lift that started production again names the version.
    ["stop app", { env: "prod", package: "pkg_9e40", image_digest: "sha256:0", reason: "Exports claimant SSNs (INC-2231)", cancelled: [] }, "held production: Exports claimant SSNs (INC-2231)"], // Canyon stopped it
    // Canyon could not stop it, or the org's platform runs it and was sent a remove: neither says it stopped (contracts/routes.md, The hold)
    ["stop app", { env: "prod", package: "pkg_9e40", image_digest: "sha256:0", reason: "Exports claimant SSNs (INC-2231)", cancelled: ["gr_1"], stopped: false }, "held production, but Canyon couldn't stop it: Exports claimant SSNs (INC-2231)"],
    ["stop app", { env: "prod", reason: "Exports claimant SSNs (INC-2231)", cancelled: [] }, "held production: Exports claimant SSNs (INC-2231)"],
    // A row that says what happened to what ran (contracts/routes.md, the hold's and the undeploy's stop; #156).
    ["stop app", { env: "prod", reason: "INC-2231", cancelled: [], stop: "stopped" }, "stopped and held production: INC-2231"],
    ["stop app", { env: "prod", reason: "INC-2231", cancelled: [], stop: "remove_sent" }, "held production and asked your platform to remove it: INC-2231"],
    ["stop app", { env: "prod", reason: "INC-2231", cancelled: [], stop: "nothing" }, "held production, where nothing ran: INC-2231"],
    ["stop app", { env: "prod", reason: "INC-2231", cancelled: ["gr_1"], stop: "failed", stopped: false }, "held production, but Canyon couldn't stop it: INC-2231"],
    ["DELETE /api/v1/projects/{id}/envs/{env}", { env: "prod", package: "pkg_9e40", stop: "stopped" }, "took production offline, keeping its data"],
    ["DELETE /api/v1/projects/{id}/envs/{env}", { env: "prod", package: "pkg_9e40", stop: "remove_sent" }, "asked your platform to take production offline, keeping its data"],
    ["DELETE /api/v1/projects/{id}/envs/{env}", { env: "preview", package: null, run: "gr_1", stop: "nothing" }, "stopped the deploy to preview; nothing ran there"], // it ran nothing (contracts/routes.md: image_digest absent)
    ["lift hold", { held_by: dana, held_at: "2026-10-11T09:40:00Z", reason: "Exports claimant SSNs (INC-2231)", started: false }, "lifted the hold without starting production"],
    ["lift hold", { held_by: dana, held_at: "2026-10-11T09:40:00Z", reason: "Exports claimant SSNs (INC-2231)", started: true, package: "pkg_9e40", label: "v1.4" }, "lifted the hold and started v1.4"],
    ["POST /api/v1/nothing/{id}", {}, "POST /api/v1/nothing/{id}"], // a route the bundle was built before: shown as written, never blank
  ] as const) {
    it(`says "${said}"`, () => expect(sentence(event(action, detail))).toBe(said));
  }

  // A custom rule's saved version folds its text away, before and after; version 1 has only an after.
  it("shows a custom rule's text before and after, folded", () => {
    const { container, getByText } = render(<ul><AuditRow event={event("custom_rule.set", { rule: "custom.no-pii", version: 2, instructions: { before: "Old words.", after: "New words." } })} /></ul>);
    expect(container.querySelector("details")!.open).toBe(false);
    expect(getByText("Old words.")).toBeDefined();
    expect(getByText("New words.")).toBeDefined();
    const first = render(<ul><AuditRow event={event("custom_rule.set", { rule: "custom.no-pii", version: 1, instructions: { after: "First words." } })} /></ul>);
    expect(within(first.container).queryByText("Before")).toBeNull();
  });

  it("folds a custom rule's text on the fleet's strip too", () => {
    const { container } = render(<ul><AuditRow event={event("custom_rule.set", { rule: "custom.no-pii", version: 1, instructions: { after: "First words." } })} stacked /></ul>);
    expect(container.querySelector("details")).not.toBeNull();
  });

  // The Access group's rows, from its contract sample, in plain words — and those words are D5's.
  const access = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/access/claims-portal.json"), "utf8")).audit as AuditEvent[];
  it("says the Access group's rows in plain words, each of them D5's", () => {
    expect(access.map(sentence)).toEqual([
      "shared the app with sam@meridian.example as read-only",
      "asked for access to the app",
      "dismissed kim@meridian.example's request for access",
      "removed lee@meridian.example from the app",
    ]);
    expect(sentence(event("share app", { kind: "group", grantee: "claims-engineering", level: { before: null, after: "owner" } }))).toBe("shared the app with group:claims-engineering as owner");
    const experience = readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8");
    for (const one of ["share app", "unshare app", "request access", "dismiss access request"]) expect(experience).toContain(AUDIT_ACTIONS[one]);
  });

  // An app environment's allowlist rows (design/EGRESS.md §3.3, #2560), in D5's words.
  it("says the allowlist rows in plain words, each of them D5's", () => {
    expect(sentence(event("egress.allow", { env: "prod", destination: "api.stripe.com" }))).toBe("added api.stripe.com to the production allowlist");
    expect(sentence(event("egress.disallow", { env: "preview", destination: "*.example.com" }))).toBe("removed *.example.com from the preview allowlist");
    const experience = readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8");
    for (const one of ["egress.allow", "egress.disallow"]) expect(experience).toContain(AUDIT_ACTIONS[one]);
  });

  // The five other egress kinds the server writes (#2563), each as its row's detail is written.
  it("says the other egress rows in plain words, each of them D5's", () => {
    expect(sentence(event("egress.set", { via: { before: "off", after: "canyon" } }))).toBe("changed the org's outbound settings");
    expect(sentence(event("egress.mode.set", { env: "prod", app_mode: { before: null, after: "allowlist" } }))).toBe("set the production outbound mode to allowlist");
    expect(sentence(event("egress.mode.set", { env: "preview", app_mode: { before: "allowlist", after: null } }))).toBe("set the preview outbound mode to the org's");
    expect(sentence(event("egress.request", { env: "prod", destination: "api.stripe.com" }))).toBe("refused api.stripe.com in production and asked a person to allow it");
    expect(sentence(event("egress.request.dismiss", { env: "preview", destination: "api.stripe.com" }))).toBe("dismissed the request to allow api.stripe.com in preview");
    expect(sentence(event("egress.networks.recreate", { internal: true, started: ["prod", "preview"], stopped: [], failed: null }))).toBe("recreated the app's network");
    for (const step of ["network", "record", "start"]) {
      expect(sentence(event("egress.networks.recreate", { internal: true, started: [], stopped: ["prod"], failed: step }))).toBe("tried to recreate the app's network, but it failed");
    }
    const experience = readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8");
    for (const one of ["egress.set", "egress.mode.set", "egress.request", "egress.request.dismiss", "egress.networks.recreate"]) expect(experience).toContain(AUDIT_ACTIONS[one]);
    expect(experience).toContain(AUDIT_RECREATE_FAILED);
  });

  // The credentials' rows and the roots' (#2145), each as the server writes its detail (contracts/routes.md, the routes rows
  // and POST /api/v1/egress/ca/rotate), on the Audit page and the fleet's Recent activity alike: plain words, never the
  // action's name, never a value or a key, and each of them D5's.
  it("says the credentials' and the roots' rows in plain words, each of them D5's", () => {
    const canyon = { kind: "canyon" as const, subject: "egress", display: "Canyon" };
    const previous = [{ root: "d".repeat(64), ends_at: "2026-11-30T09:00:00Z" }];
    for (const [action, actor, detail, said] of [
      ["egress.route.set", dana, { env: "prod", destination: "api.stripe.com" }, "Dana W. set the credential for api.stripe.com in production"],
      ["egress.route.set", dana, { env: "preview", destination: "api.stripe.com", secret: "STRIPE_SECRET_KEY" }, "Dana W. moved the secret STRIPE_SECRET_KEY into the credential for api.stripe.com in preview"],
      ["egress.route.remove", dana, { env: "prod", destination: "hooks.slack.com" }, "Dana W. removed the credential for hooks.slack.com in production"],
      ["egress.ca.rotate", dana, { root: "f".repeat(64), byo: false, previous }, "Dana W. replaced the root of Canyon's credential certificates"],
      ["egress.ca.rotate", canyon, { root: "f".repeat(64), byo: false, previous }, "Canyon replaced the root of Canyon's credential certificates"],
      ["egress.ca.rotate", dana, { root: "e".repeat(64), byo: true, previous }, "Dana W. made the org's own sub-CA the root of Canyon's credential certificates"],
    ] as const) {
      for (const stacked of [false, true]) {
        const { container, unmount } = render(<ul><AuditRow event={{ ...event(action, detail), actor }} stacked={stacked} /></ul>);
        expect(container.textContent, `${action}${stacked ? " on the fleet" : ""}`).toContain(said);
        expect(container.textContent).not.toContain(action);
        unmount();
      }
    }
    const experience = readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8");
    for (const one of ["egress.route.set", "egress.route.remove", "egress.ca.rotate"]) expect(experience, one).toContain(AUDIT_ACTIONS[one]);
    for (const one of [AUDIT_ROUTE_MOVED, AUDIT_CA_BYO]) expect(experience).toContain(one);
  });

  // Canyon's patches and an app's own guardrail (#2603), each row as the server writes its detail, rendered as the Audit
  // page and the fleet's Recent activity draw it: plain words, never the action's name, and each of them D5's.
  it("says patching's rows and an app's guardrail in plain words, each of them D5's", () => {
    const canyon = { kind: "canyon" as const, subject: "patch", display: "Canyon" };
    const identity = "sha256:" + "4dc4df20c917".repeat(5) + "abcd";
    const patched = { package: "pkg_9e40", run: "gr_9c4e1a", replaces: "sha256:aa", image: "sha256:bb", proof: "layers", fixed: [{ name: "libssl3", from: "3.0.13-0ubuntu3.4", to: "3.0.13-0ubuntu3.5", vulnerability: "CVE-2024-5535" }] };
    for (const [action, actor, detail, said] of [
      ["patch preview", canyon, patched, "Canyon patched 9e40 in preview"],
      ["patch prod", dana, { package: "pkg_9e40", image_digest: "sha256:bb", url: "https://x", replaces: "sha256:aa" }, "Dana W. promoted Canyon's patch of 9e40 to production"],
      ["revert patch preview", canyon, patched, "Canyon reverted its patch of 9e40 in preview"],
      ["revert patch prod", canyon, patched, "Canyon reverted its patch of 9e40 in production"],
      ["request patch approval", canyon, { package: "pkg_9e40", code: "K7QF-2M9X", image: "sha256:bb" }, "Canyon asked a person to approve its patch of 9e40 for production"],
      ["approve patch identity", dana, { identity, run_image: "canyon-run:noble", reason: "USN-7001" }, "Dana W. approved system libraries image 4dc4df20c917 for production on every app: USN-7001"],
      ["pause patching", canyon, { identity, run_image: "canyon-run:noble", projects: ["r1-exits-b", "r1-exits-a"] }, "Canyon paused patching with system libraries image 4dc4df20c917"],
      ["resume patching", dana, { identity, run_image: "canyon-run:noble" }, "Dana W. resumed patching with system libraries image 4dc4df20c917"],
      ["app_policy.set", dana, { rule: "platform.known-vuln", state: { before: "off", after: "advisory" } }, "Dana W. changed the app's guardrail platform.known-vuln"],
      ["app_policy.remove", dana, { rule: "platform.known-vuln" }, "Dana W. removed the app's guardrail platform.known-vuln"],
    ] as const) {
      for (const stacked of [false, true]) {
        const { container, unmount } = render(<ul><AuditRow event={{ ...event(action, detail), actor }} stacked={stacked} /></ul>);
        expect(container.textContent, `${action}${stacked ? " on the fleet" : ""}`).toContain(said);
        unmount();
      }
    }
    const experience = readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8");
    for (const one of ["patch preview", "patch prod", "revert patch preview", "revert patch prod", "request patch approval", "approve patch identity", "pause patching", "resume patching", "app_policy.set", "app_policy.remove"]) {
      expect(experience, one).toContain(AUDIT_ACTIONS[one]);
    }
  });

  it("reads when, who and what, and names the app only on the fleet", () => {
    vi.setSystemTime(new Date("2026-09-02T10:00:40Z"));
    const one = event("POST /api/v1/projects/{id}/packages", { package: "pkg_9e40", label: "v1.4" });
    expect(render(<ul><AuditRow event={one} app /></ul>).container.textContent).toBe("30mclaims-portalDana W. uploaded v1.4");
    expect(render(<ul><AuditRow event={one} /></ul>).container.textContent).toBe("30mDana W. uploaded v1.4");
  });
});
