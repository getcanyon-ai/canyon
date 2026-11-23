import { describe, expect, it } from "vitest";
import type { Finding, RunRead } from "../api";
import { S } from "../strings";
import { tryAgain, verdict } from "./Verdict";

const blocker = { ruleId: "a", entry: 0, properties: { canyon: { blocks: true } } } as unknown as Finding;
const run = (over: Partial<RunRead>): RunRead => ({ id: "gr_1", package: "pkg_1", state: "clean", blocking: 0, warnings: 0, findings: [], ...over });

describe("the verdict", () => {
  // A verdict names the version it ruled on: without it, it reads as the app's status.
  it("names the version a clean review passed, where the screen knows it", () => {
    expect(verdict(run({}), "v1.0")).toBe("✓ v1.0 passed review");
    expect(verdict(run({}))).toBe("✓ Passed review");
    expect(verdict(run({ not_checked: true }), "v1.0")).toBe(S.verdictNotChecked);
  });

  // A block names what blocked it; a block with nothing named is a result that went missing, never
  // "Blocked — 0 blocking findings" (the audit's C4).
  it("never says Blocked with no blocking finding", () => {
    expect(verdict(run({ state: "blocked", blocking: 0 }))).toBe("Review result is missing. Run the review again.");
    expect(verdict(run({ state: "blocked", blocking: 1, findings: [blocker] }))).toBe(S.verdictBlocked);
  });

  // The count is the run's entries, as the CLI's (contracts/finding.md, One problem, one entry; #2013):
  // the read's own count stays its two results, and two rules on one line say one.
  it("counts entries, two rules on one line one", () => {
    expect(verdict(run({ state: "blocked", blocking: 2, findings: [blocker, { ...blocker, ruleId: "b" }] }))).toBe(S.verdictBlocked);
  });

  it("says why a review could not finish, in words", () => {
    expect(verdict(run({ state: "failed", failure_reason: "error" }))).toBe("✕ The review couldn't finish: Canyon itself failed");
    expect(verdict(run({ state: "failed", failure_reason: "weird" }))).toBe("✕ The review couldn't finish: weird");
    expect(verdict(run({ state: "failed" }))).toBe("✕ The review couldn't finish: no reason was recorded");
  });

  // contracts/routes.md, A failure reason in words: the step, then what to do and where an admin looks.
  it("names the step a run Canyon itself failed in, and what to do next", () => {
    const failed = run({ id: "gr_7c1e", state: "failed", failure_reason: "error", failed_in: "scan" });
    expect(verdict(failed)).toBe("✕ The review couldn't finish: Canyon itself failed while scanning the package");
    expect(tryAgain(failed)).toBe("Run it again. If it fails again, an admin can look up run gr_7c1e in Canyon's server log.");
    expect(tryAgain(run({ state: "failed", failure_reason: "error" }))).toBeNull(); // no step named: its own lines say why
  });

  // A build or a start that failed came after the review passed: the verdict says both, never "the
  // review couldn't finish".
  it("says a version passed review before its build or its start failed", () => {
    expect(verdict(run({ state: "failed", failure_reason: "build" }), "v1.4")).toBe("✕ v1.4 passed review, then didn't build");
    expect(verdict(run({ state: "failed", failure_reason: "start" }), "v1.4")).toBe("✕ v1.4 passed review, then didn't stay up after it started");
    // the cluster's capacity, never the package's fault (#1637)
    expect(verdict(run({ state: "failed", failure_reason: "no_room" }), "v1.4")).toBe("✕ v1.4 passed review, then couldn't be built: the cluster had no room");
  });

  // A scan the cluster had no room for ends the run before its review: never a pass, a block, nor Canyon failing.
  it("says the cluster had no room to scan a package nothing reviewed", () => {
    expect(verdict(run({ state: "failed", failure_reason: "scan_no_room" }), "v1.4")).toBe("✕ The review couldn't finish: the cluster had no room to scan the package");
  });

  // A package no buildpack on the host detects was never compiled: the verdict says the host can't
  // build this kind of app and names what it builds, never that the review couldn't finish.
  it("says what the host builds when no buildpack detected the package", () => {
    const undetected = run({ state: "failed", failure_reason: "detect", builds: ["Paketo Buildpack for Ruby", "Paketo Buildpack for Node.js"] });
    expect(verdict(undetected)).toBe("✕ This host can't build this kind of app. Canyon builds Ruby and Node.js apps.");
    expect(verdict({ ...undetected, builds: [] })).toBe("✕ This host can't build this kind of app. Its builder doesn't say what it builds.");
    // a buildpack named by its id, as a builder without names gives it, is the same kind; one the table lacks keeps its own name
    expect(verdict({ ...undetected, builds: ["paketo-buildpacks/web-servers", "paketo-buildpacks/go", "Paketo Buildpack for Procfile", "acme/elixir"] }))
      .toBe("✕ This host can't build this kind of app. Canyon builds Go apps, static sites, apps with a Procfile and acme/elixir.");
    // a buildpack that errored while detecting may mean a malformed manifest: the code is never called fine
    expect(verdict({ ...undetected, errored: true })).toBe("✕ This host can't build this kind of app. Canyon builds Ruby and Node.js apps. " +
      "A buildpack also errored while checking. If this app is meant to be one of those stacks, check that its manifest is valid (for example package.json).");
  });
});
