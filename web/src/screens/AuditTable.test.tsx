import { describe, expect, it } from "vitest";
import type { AuditEvent } from "../api";
import { what } from "./AuditTable";

const dana = { kind: "person" as const, subject: "sub-dana", display: "Dana Whitfield" };
const row = (action: string, detail: Record<string, unknown>): AuditEvent => ({ id: "ev_1", ts: "2026-10-08T12:00:00Z", actor: dana, action, project: "claims-portal", detail });

// A person who promoted a version whose preview answered an error is on the trail as having done so
// despite it (contracts/routes.md, the promotions row and Production approvals: detail.answered).
describe("a promotion despite what preview answered", () => {
  it("says the answer it overrode, from the Promote button and from an approval", () => {
    expect(what(row("POST /api/v1/projects/{id}/promotions", { package: "pkg_7a1c", label: "v1.3", gate_run: "gr_1", answered: { status: 400, said: "" } })))
      .toEqual({ verb: "Started a promotion of", rest: "v1.3, despite preview answering 400 at /" });
    expect(what(row("approve promotion", { package: "pkg_7a1c", label: "v1.3", gate_run: "gr_1", code: "K7QP-4RXM", answered: { error: "refused" } })))
      .toEqual({ verb: "Approved for production", rest: "v1.3, despite preview giving no answer at /" });
    expect(what(row("POST /api/v1/projects/{id}/promotions", { package: "pkg_7a1c", label: "v1.3", gate_run: "gr_1" })))
      .toEqual({ verb: "Started a promotion of", rest: "v1.3" });
  });
});

// The Audit page says Canyon's patches in the trail's own words, never the action's name (#2603).
describe("Canyon's patches", () => {
  it("are said in plain words on the Audit page", () => {
    const identity = "sha256:" + "4dc4df20c917".repeat(5) + "abcd";
    expect(what(row("patch prod", { package: "pkg_7a1c", label: "v1.3", image_digest: "sha256:bb", url: "https://x", replaces: "sha256:aa" }))).toEqual({ verb: "Promoted Canyon's patch of v1.3 to production" });
    expect(what(row("revert patch preview", { package: "pkg_7a1c", label: "v1.3" }))).toEqual({ verb: "Reverted its patch of v1.3 in preview" });
    expect(what(row("pause patching", { identity, run_image: "canyon-run:noble", projects: ["a", "b"] }))).toEqual({ verb: "Paused patching with system libraries image 4dc4df20c917" });
  });
});

describe("the host's egress setting", () => {
  it("says which way apps call out now", () => {
    expect(what(row("egress.set", { via: { before: "off", after: "canyon" } }))).toEqual({ verb: "Changed how apps call out", rest: "through Canyon" });
    expect(what(row("egress.set", { via: { before: "canyon", after: "off" } }))).toEqual({ verb: "Changed how apps call out", rest: "directly, not through Canyon" });
  });
  // #2143: credential_routes in the audit trail.
  it("says what else an admin changed: the org's mode or adding credentials", () => {
    expect(what(row("egress.set", { mode: { before: "observe", after: "allowlist" } }))).toEqual({ verb: "Changed what apps may call", rest: "to allowlist" });
    expect(what(row("egress.set", { credential_routes: { before: false, after: true } }))).toEqual({ verb: "Turned on adding credentials" });
    expect(what(row("egress.set", { credential_routes: { before: true, after: false } }))).toEqual({ verb: "Turned off adding credentials" });
  });
  it("says which way an app's recreated network sends its calls, and what was left stopped", () => {
    expect(what(row("egress.networks.recreate", { internal: true, started: ["preview", "prod"], stopped: [] }))).toEqual({ verb: "Recreated the app's network", rest: "through Canyon" });
    expect(what(row("egress.networks.recreate", { internal: false, started: ["preview"], stopped: ["prod"] }))).toEqual({ verb: "Recreated the app's network", rest: "directly, not through Canyon; prod left stopped" });
  });
});

// access_grace is how long a handed-off app keeps letting people in on its last access list when it can't reach Canyon
// (design/HAND-OFF.md §1), and the trail says so in the dashboard's own words for the field (#270).
describe("the access grace's row", () => {
  it("says what the grace is, before and after", () => {
    expect(what(row("PUT /api/v1/preferences", { access_grace: { before: "1h", after: "30m" } })))
      .toEqual({ verb: "Changed how long apps keep letting people in when they can't reach Canyon", rest: "1h → 30m" });
  });
});
