import type { IconName } from "./Icon";
import { Pill, type Tone } from "./Pill";

// The one renderer for where a package sits in the machine. The fleet row, the inspector, the app
// header and the owner card all call it: a second one would mean this is wrong. A stage is never
// "clean" and never "received" — the gate's verdict lives on the run.
export type StageKey =
  | "created" | "awaiting_review" | "reviewing" | "blocked" | "approved"
  | "passed" | "building" | "preview" | "production" | "failed";

const STAGES: Record<StageKey, { icon: IconName; word: string; tone: Tone }> = {
  created: { icon: "never", word: "Nothing uploaded", tone: "neutral" },
  awaiting_review: { icon: "waiting", word: "Awaiting review", tone: "amber" },
  reviewing: { icon: "working", word: "Reviewing", tone: "neutral" },
  blocked: { icon: "blocked", word: "Blocked", tone: "critical" },
  approved: { icon: "alert", word: "Approved", tone: "amber" }, // D1: every blocker approved, never blocked and never passed
  passed: { icon: "passed", word: "Passed", tone: "ok" },
  building: { icon: "working", word: "Building", tone: "neutral" },
  preview: { icon: "live", word: "Preview", tone: "ok" },
  production: { icon: "live", word: "Production", tone: "ok" },
  failed: { icon: "blocked", word: "Failed", tone: "critical" },
};

// The ten keys with the word each one shows, in the order declared above. The fleet's Latest
// filter lists its options from here, so an option and the cell it selects cannot drift apart.
export const STAGE_OPTIONS = Object.entries(STAGES).map(([key, { word }]) => [key as StageKey, word] as const);

export function Stage({ stage, version }: { stage: StageKey; version?: string }) {
  const { icon, word, tone } = STAGES[stage];
  return (
    <span className="inline-flex items-center gap-2 whitespace-nowrap">
      {version !== undefined && <span className="tnum text-md text-text-dim">{version}</span>}
      <Pill tone={tone} icon={icon}>{word}</Pill>
    </span>
  );
}

// The fleet's Review column: what the newest version's review said, and nothing about where it runs
// (the redesign: "Version is just the version. Review is its own column."). Once that version builds or
// runs, its stage says where it is and not what its review said, so the review is the app's newest
// run (`gate_state`): a deploy is not a pass, and a run that blocked a version already running is
// never hidden behind where it runs (contracts/routes.md, the package's stage). An app no run has
// ever looked at says so, whatever its stage.
type GateState = "never_reviewed" | "pending" | "running" | "clean" | "blocked" | "failed";
const RUN: Record<Exclude<GateState, "never_reviewed">, StageKey> = { pending: "reviewing", running: "reviewing", clean: "passed", blocked: "blocked", failed: "failed" };
const PLACED: StageKey[] = ["building", "preview", "production"];
export function reviewOf(stage: StageKey, gate: GateState): { icon: IconName; word: string; tone: Tone } {
  if (gate === "never_reviewed") return { icon: "never", word: "Never reviewed", tone: "amber" };
  return STAGES[PLACED.includes(stage) ? RUN[gate] : stage];
}
// warnings is the last run's advisory count where the read carries it (the fleet's): a passed review with
// advisories says so beside its pill, so it never looks like a clean one.
export function Review({ stage, gate, warnings = 0 }: { stage: StageKey; gate: GateState; warnings?: number }) {
  const { icon, word, tone } = reviewOf(stage, gate);
  if (tone !== "ok" || warnings === 0) return <Pill tone={tone} icon={icon}>{word}</Pill>;
  return <span className="inline-flex items-center gap-1.5 whitespace-nowrap"><Pill tone={tone} icon={icon}>{word}</Pill><span className="text-md text-amber">{`${warnings} ${warnings === 1 ? "warning" : "warnings"}`}</span></span>;
}
