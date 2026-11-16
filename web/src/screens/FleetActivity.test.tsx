import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../api";
import { labels } from "../ui/AppName";
import { ACTIVITY_ACTIONS, activity, Row } from "./FleetActivity";

const NOW = Date.parse("2026-10-05T15:00:00Z");
const alice = { kind: "person" as const, subject: "alice@meridian.example", display: "Alice Moreau" };
const bob = { kind: "person" as const, subject: "bob@meridian.example", display: "Bob Lindqvist" };
const canyon = { kind: "canyon" as const, subject: "gate", display: "Canyon" };
let id = 0;
const row = (ts: string, actor: AuditEvent["actor"], action: string, detail: Record<string, unknown> = {}, project = "claims-portal", run?: string): AuditEvent =>
  ({ id: `ev_${++id}`, ts: `2026-10-05T${ts}:00Z`, actor, action, project, run, detail });
// One run as the trail writes it, newest row first: its start, verdict, then its deploy or failure,
// each naming the run; the upload before it names none.
const run = (at: [string, string, string, string], pkg: string, label: string, end: "deploy preview" | "verdict" | "start failed", who = alice, project = "claims-portal", upload = true) => {
  const gr = `gr_${pkg}_${at[1]}`;
  return [
    ...(end === "verdict" ? [] : [row(at[3], end === "deploy preview" ? who : canyon, end, { package: pkg, label }, project, gr)]),
    row(at[2], canyon, "verdict", { package: pkg, label, state: "clean", blocking: 0, warnings: 2 }, project, gr),
    row(at[1], who, "POST /api/v1/projects/{id}/deploys", { package: pkg, label }, project, gr),
    ...(upload ? [row(at[0], who, "POST /api/v1/projects/{id}/packages", { package: pkg, label }, project)] : []),
  ];
};
const said = (lines: { who: { display: string } | null; text: string; note?: string }[]) => lines.map((line) => `${line.who?.display ?? ""}|${line.text}|${line.note ?? ""}`);

describe("the fleet's recent activity", () => {
  // The approved redesign: a run's upload, start, verdict and deploy are one line, told by the furthest step.
  it("tells a run in one line by the furthest step it reached", () => {
    const { lines } = activity(run(["09:00", "09:01", "09:02", "09:03"], "pkg_a", "v1.4", "deploy preview"), "Meridian", NOW);
    expect(said(lines)).toEqual(["Alice Moreau|deployed claims-portal v1.4 to preview.|Passed · 2 warnings"]);
  });

  // Inside one app's pane the same lines leave the app out: it is the pane's own.
  it("never names the app a pane is about", () => {
    const events = [
      ...run(["09:00", "09:01", "09:02", "09:03"], "pkg_a", "v1.4", "deploy preview"),
      row("08:00", bob, "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", { rule: "secrets.client-bundle" }),
    ];
    expect(said(activity(events, "Meridian", NOW, "claims-portal").lines)).toEqual([
      "Alice Moreau|deployed v1.4 to preview.|Passed · 2 warnings",
      "Bob Lindqvist|approved a blocking problem.|secrets.client-bundle",
    ]);
  });

  // A host with sign-in off has one caller, its anonymous admin (contracts/routes.md, person / anonymous, no display):
  // the line says nobody signed in, and draws no initials for a name it doesn't have (#2696).
  it("names the anonymous admin as someone not signed in, with no initials", () => {
    const anonymous = { kind: "person" as const, subject: "anonymous", display: "" };
    const events = [...run(["09:20", "09:21", "09:22", "09:23"], "pkg_d", "d295", "deploy preview", anonymous), ...run(["09:00", "09:01", "09:02", "09:03"], "pkg_e", "ef49", "verdict", anonymous)];
    const { container } = render(<ul><Row line={activity(events, "Meridian", NOW).lines[0]!} /></ul>);
    const line = container.querySelector("li")!; // the line, not the versions folded under it
    expect(line.querySelector("p")!.textContent).toBe("Someone not signed in ran 2 versions of claims-portal. d295 went to preview.");
    expect(line.firstElementChild!.textContent).toBe("");
  });

  it("says a blocked version was sent for review, never shipped", () => {
    const events = [row("09:02", canyon, "verdict", { package: "pkg_b", label: "v2", state: "blocked", blocking: 1 }, "claims-portal", "gr_b"), row("09:01", alice, "POST /api/v1/projects/{id}/gate-runs", { package: "pkg_b", label: "v2" }, "claims-portal", "gr_b")];
    expect(said(activity(events, "Meridian", NOW).lines)).toEqual(["Alice Moreau|sent claims-portal v2 for review.|Blocked · 1 blocking"]);
  });

  // A version can run more than once: a failed start and its retry are two runs, the retry's its own.
  it("tells each run of one version apart, and never pins a failure its retry fixed under the retry", () => {
    const events = [
      ...run(["", "14:10", "14:11", "14:12"], "pkg_a", "v1", "deploy preview", bob, "claims-portal", false),
      ...run(["13:00", "13:01", "13:02", "13:10"], "pkg_a", "v1", "start failed"),
    ];
    const { pinned, lines } = activity(events, "Meridian", NOW);
    expect(said(lines)).toEqual(["Bob Lindqvist|deployed claims-portal v1 to preview.|Passed · 2 warnings"]); // the retry is Bob's, not the uploader's
    expect(said(pinned)).toEqual(["Alice Moreau|claims-portal v1 didn't stay up after it started.|Run by Alice Moreau"]);
  });

  // GitHub's "pushed 3 commits": one person's runs on one app within the hour of the newest are one burst.
  it("groups one person's runs on one app within the hour, and opens to each version", () => {
    const events = [
      ...run(["14:10", "14:11", "14:12", "14:13"], "pkg_c", "v1.4.2", "deploy preview"),
      ...run(["13:50", "13:51", "13:52", "13:53"], "pkg_b", "v1.4.1", "verdict"),
      ...run(["13:30", "13:31", "13:32", "13:33"], "pkg_a", "v1.4.0", "verdict"),
      ...run(["12:50", "12:51", "12:52", "12:53"], "pkg_y", "v1.3.9", "verdict"), // within an hour of the one before, over an hour from the newest
    ];
    const { lines } = activity(events, "Meridian", NOW);
    expect(said(lines)).toEqual([
      "Alice Moreau|ran 3 versions of claims-portal. v1.4.2 went to preview.|",
      "Alice Moreau|shipped claims-portal v1.3.9.|Passed · 2 warnings",
    ]);
    expect(lines[0]!.versions!.map((one) => `${one.version} ${one.outcome}`)).toEqual(["v1.4.2 Passed · 2 warnings · in preview", "v1.4.1 Passed · 2 warnings", "v1.4.0 Passed · 2 warnings"]);
  });

  it("never folds another person's run, another app's, a failure or a run past another change into a burst", () => {
    const one = (who: typeof alice, project: string) => [...run(["14:10", "14:11", "14:12", "14:13"], "pkg_b", "v2", "verdict", who, project), ...run(["14:00", "14:01", "14:02", "14:03"], "pkg_a", "v1", "verdict")];
    expect(activity(one(bob, "claims-portal"), "Meridian", NOW).lines).toHaveLength(2);
    expect(activity(one(alice, "quote-engine"), "Meridian", NOW).lines).toHaveLength(2);
    expect(activity([...run(["14:10", "14:11", "14:12", "14:13"], "pkg_b", "v2", "start failed"), ...run(["14:00", "14:01", "14:02", "14:03"], "pkg_a", "v1", "verdict")], "Meridian", NOW).lines).toHaveLength(1); // the failure is pinned, the rest stands alone
    const between = [...run(["14:10", "14:11", "14:12", "14:13"], "pkg_b", "v2", "verdict"), row("14:05", bob, "share app", { kind: "email", grantee: "c@m.example", level: { before: null, after: "viewer" } }), ...run(["14:00", "14:01", "14:02", "14:03"], "pkg_a", "v1", "verdict")];
    expect(activity(between, "Meridian", NOW).lines).toHaveLength(3);
  });

  // Failures and image mismatches of the last day are pinned above the timeline; older ones stay in it.
  it("pins the last day's failures and image mismatches above everything else", () => {
    const events = [
      row("14:41", { kind: "agent", subject: "platform", display: "Meridian's platform" }, "platform reported another digest", { env: "prod", expected: "sha256:9f1c2e0011", reported: "sha256:47aa0b2233" }, "premium-ledger"),
      ...run(["13:10", "13:11", "13:12", "13:13"], "pkg_f", "v2.3", "start failed", alice, "fnol-intake"),
      row("12:40", bob, "PUT /api/v1/projects/{id}/secrets/{name}", { name: "STRIPE_SECRET_KEY", env: "prod" }, "premium-ledger"),
    ];
    const { pinned, lines } = activity(events, "Meridian", NOW);
    expect(said(pinned)).toEqual([
      "|Meridian's platform is running an image Canyon didn't review for premium-ledger in production.|Reviewed sha256:9f1c2e… · running sha256:47aa0b…",
      "Alice Moreau|fnol-intake v2.3 didn't stay up after it started.|Run by Alice Moreau",
    ]);
    expect(said(lines)).toEqual(["Bob Lindqvist|set the secret STRIPE_SECRET_KEY for production in premium-ledger.|"]);
    const later = activity(events, "Meridian", NOW + 2 * 86_400_000);
    expect(later.pinned).toEqual([]);
    expect(later.lines).toHaveLength(3); // an old failure stays on the timeline
  });

  it("tells a promotion's two rows as one, each promotion its own, and an approval with its rule, reason and expiry", () => {
    const events = [
      row("14:11", bob, "deploy prod", { package: "pkg_l", label: "v5.1" }, "premium-ledger", "gr_p2"),
      row("14:10", bob, "POST /api/v1/projects/{id}/promotions", { package: "pkg_l", label: "v5.1" }, "premium-ledger", "gr_p2"),
      row("13:06", bob, "deploy prod", { package: "pkg_l", label: "v5.1" }, "premium-ledger", "gr_p1"),
      row("13:05", bob, "POST /api/v1/projects/{id}/promotions", { package: "pkg_l", label: "v5.1" }, "premium-ledger", "gr_p1"),
      row("13:00", bob, "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", { rule: "secrets.client-bundle", reason: "Test key, rotated on 3 Oct", expires_at: "2026-11-03T12:00:00Z" }, "premium-ledger"),
    ];
    expect(said(activity(events, "Meridian", NOW).lines)).toEqual([
      "Bob Lindqvist|promoted premium-ledger v5.1 to production.|",
      "Bob Lindqvist|promoted premium-ledger v5.1 to production.|",
      'Bob Lindqvist|approved a blocking problem in premium-ledger.|secrets.client-bundle · "Test key, rotated on 3 Oct" · until 3 Nov',
    ]);
  });

  it("reads a change to who is an admin as one, and names the app a share is of", () => {
    const admins = row("14:32", alice, "PUT /api/v1/preferences", { admin_emails: { before: ["dana@meridian.example"], after: ["dana@meridian.example", "bob@meridian.example"] } });
    admins.project = undefined;
    const reordered = row("14:30", alice, "PUT /api/v1/preferences", { admin_emails: { before: ["a@m.example", "b@m.example"], after: ["b@m.example", "a@m.example"] } });
    reordered.project = undefined;
    const events = [admins, reordered, row("13:58", alice, "share app", { kind: "email", grantee: "bob@meridian.example", level: { before: null, after: "viewer" } })];
    expect(said(activity(events, "Meridian", NOW).lines)).toEqual([
      "Alice Moreau|changed who is an admin: +bob@meridian.example.|Admin change",
      "Alice Moreau|changed the guardrails.|", // the same admins in another order changed nobody's role
      "Alice Moreau|shared claims-portal with bob@meridian.example as read-only.|",
    ]);
  });

  // An app is named by its display name, and by its id beside it where two apps share the name (#1911).
  it("names apps by their names, and two alike by their ids too", () => {
    const names = labels([{ id: "snappy-otter-0fz", name: "claims-app" }, { id: "tidy-finch-euk", name: "claims-app" }, { id: "calm-fox-1ab", name: "Expense tracker" }]);
    const events = [row("14:00", alice, "POST /api/v1/projects", {}, "snappy-otter-0fz"), row("13:00", alice, "POST /api/v1/projects", {}, "calm-fox-1ab"), row("12:00", alice, "POST /api/v1/projects", {}, "gone-app-7xq")];
    expect(said(activity(events, "Meridian", NOW, undefined, names).lines)).toEqual([
      "Alice Moreau|created claims-app (snappy-otter-0fz).|",
      "Alice Moreau|created Expense tracker.|",
      "Alice Moreau|created gone-app-7xq.|", // an app the fleet no longer lists keeps its id
    ]);
  });

  // Asked of the server by name: what the feed leaves out never reaches it.
  it("asks for no sign-in, log read or resource step", () => {
    for (const left of ["sign in", "sign out", "GET /api/v1/projects/{id}/logs", "ensure resource", "declare resource", "attach report"]) {
      expect(ACTIVITY_ACTIONS).not.toContain(left);
    }
    expect(ACTIVITY_ACTIONS.length).toBeLessThanOrEqual(64); // the audit read's most (contracts/routes.md)
  });
});
