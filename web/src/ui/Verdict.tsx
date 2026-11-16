import { entries, type RunRead } from "../api";
import { S } from "../strings";
import { actorName } from "./Actor";

// The run's own verdict, in its own words. A state a screen cannot read is not a pass: every one
// of the five says what it is, and only "clean" reads clean. One copy for the whole dashboard —
// the app page prints it under Findings and the fleet's panel prints it for the same run.
export const TONE: Record<RunRead["state"], string> = { pending: "text-text-dim", running: "text-text-dim", clean: "text-ok", blocked: "text-critical", failed: "text-critical" };

// shortDate is an approval's expiry the way the verdict and the card say it: `7 Oct`.
export const shortDate = (at: string) => new Date(at).toLocaleDateString("en-GB", { day: "numeric", month: "short", timeZone: "UTC" });

// blockingEntries is how many problems block the run: its blocking findings' entries, as the CLI counts them
// (contracts/finding.md, One problem, one entry); the run read's own count is its results'.
export const blockingEntries = (run: RunRead) => entries(run.findings.filter((one) => one.properties.canyon.blocks));

// approved is D1's verdict when every blocking finding carries an approval (EXPERIENCE.md D5), or
// null: approved is not clean, and it is not blocked either.
export function approved(run: RunRead): string | null {
  const blocking = run.findings.filter((one) => one.properties.canyon.blocks);
  if (blocking.length === 0 || blocking.some((one) => one.approval === undefined)) return null;
  const [one] = blocking;
  if (entries(blocking) > 1 || one?.approval === undefined) return S.verdictApprovedEach.replace("<n>", String(entries(blocking)));
  const by = one.approval.approved_by;
  return S.verdictApproved.replace("<who>", () => actorName(by)).replace("<date>", shortDate(one.approval.expires_at)); // the demo's anonymous session has no display
}

// A failed run's reason in words (contracts/routes.md, *A failure reason in words*); one this list does
// not name reads as itself.
const FAILURE: Record<string, string> = {
  interrupted: "it was cancelled, or another run of this app replaced it",
  error: "Canyon itself failed",
  model: "the reviewer's model failed",
  build: "the package did not build",
  detect: "no buildpack on this host recognised the package",
  start: "the app did not stay up",
  no_room: "the cluster had no room to run the build",
  scan_no_room: "the cluster had no room to scan the package",
  image_gone: "the image production ran is no longer on this Canyon",
};
// The step a run Canyon itself failed in, as the reason's words name it; one this list does not name reads as itself.
const STEP: Record<string, string> = {
  scan: "scanning the package",
  review: "reviewing the package",
  resources: "making the app's resources",
  build: "building the package",
  deploy: "starting the app",
};
export const failureWords = (reason: string | undefined, step?: string) =>
  (reason === undefined ? "no reason was recorded" : FAILURE[reason] ?? reason) + (step === undefined ? "" : ` while ${STEP[step] ?? step}`);

// What a run Canyon itself failed in tells its owner to do: the server logged why under the run's id.
export const tryAgain = (run: RunRead) => (run.failed_in === undefined ? null : `Run it again. If it fails again, an admin can look up run ${run.id} in Canyon's server log.`);

// The reasons a run fails for after its review passed: the package did not build, no buildpack on the
// host recognised it, the app did not stay up, or the cluster had no room to build it. Its review is
// Passed, and what broke is that step's.
const AFTER_REVIEW: Record<string, string> = { build: "didn't build", detect: "couldn't be built on this host", start: "didn't stay up after it started", no_room: "couldn't be built: the cluster had no room" };
export const failedAfterReview = (run: RunRead) => run.state === "failed" && run.failure_reason !== undefined && AFTER_REVIEW[run.failure_reason] !== undefined;

// How a review that let the build go ahead ended, in the words the page uses for it: a person's approval
// is never a pass, and something nothing read is never called clean.
export const cleared = (run: RunRead) => (approved(run) !== null ? "was approved past its review" : run.not_checked ? "had nothing blocking in what was checked" : "passed review");

// `shown` names the version the run reviewed, where the screen knows it: a verdict without a version
// reads as the app's status, which it never is (the audit's C7).
export function verdict(run: RunRead, shown?: string): string {
  switch (run.state) {
    case "clean": return run.not_checked ? S.verdictNotChecked : `✓ ${shown === undefined ? "Passed review" : `${shown} passed review`}`; // something nothing read is never called clean
    case "blocked": {
      const n = blockingEntries(run);
      if (n === 0) return "Review result is missing. Run the review again."; // a block names what blocked it, or it says it cannot
      return approved(run) ?? (n === 1 ? S.verdictBlocked : `✗ Blocked — ${n} blocking findings`);
    }
    case "failed":
      if (run.failure_reason === "detect" && run.fix !== undefined) return `✕ ${shown ?? "This version"} is missing a ${run.fix.file}`; // the host builds it once the file is there
      if (run.failure_reason === "detect") { // nothing was compiled: the host does not build this kind of app
        const builds = run.builds ?? [];
        const said = `✕ ${S.undetected} ${builds.length > 0 ? S.undetectedBuilds.replace("<buildpacks>", () => builds.join(", ")) : S.undetectedNone}`;
        return run.errored ? `${said} ${S.undetectedErrored}` : said; // a buildpack erroring may mean a malformed manifest: never call the code fine
      }
      if (failedAfterReview(run)) return `✕ ${shown ?? "This version"} ${cleared(run)}, then ${AFTER_REVIEW[run.failure_reason!]}`;
      return `✕ The review couldn't finish: ${failureWords(run.failure_reason, run.failed_in)}`;
    case "pending": case "running": return "Reviewing…";
  }
}
