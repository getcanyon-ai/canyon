import { useEffect, useState } from "react";
import { getTimeline, type TimelineRow } from "../api";
import { S } from "../strings";
import { plainName } from "../ui/Finding";
import { Icon, type IconName } from "../ui/Icon";

const POLL_MS = 2000;

// One step of a run as it goes (contracts/events.md, `progress` and `check.*`): a tool Canyon runs, one of
// the review's agents, the build, or the start, with its status and what it found once done.
export interface Step {
  key: string;
  kind: "check" | "after";
  name: string;
  status: "queued" | "running" | "done" | "failed" | "skipped";
  position?: number; // its place in the host's line while it waits
  since?: string; // when it started running, from its row's timestamp
  ms?: number; // how long it ran, once done
  found?: number; // what it found, once done
  reused?: string; // the version whose image a deploy reused, in place of a build
  waited?: number; // the build's wait for Canyon's build image, before its first line (build.image), in ms
}

interface ProgressRow { step: "scan" | "build" | "start"; tool?: string; status: Step["status"]; position?: number; duration_ms?: number }
interface CheckRow { check: string; status?: string; findings?: number; duration_ms?: number }
const AGENT: Record<string, string> = { "ai-review": "AI review" };
// The one build.log row a deploy that reuses an image writes in place of a build (contracts/events.md; #1666).
const REUSING = /^Reusing the image built for (\S+) \(no build value changed\)$/;

// The run's steps, from its timeline: each tool's and the build's and the start's newest progress row, each
// agent's check rows, and what each tool's report found as it lands. Read once, then every 2 s while it goes.
export function useProgress(run: string | null, going: boolean): Step[] {
  const [steps, setSteps] = useState<{ run: string; list: Step[] } | null>(null);
  useEffect(() => {
    if (run === null) return;
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const seen = new Map<string, Step>();
    const found = new Map<string, number>(); // a tool's report, by tool name, once it lands
    let waited: number | undefined; // the run's wait for Canyon's images (contracts/events.md, build.image; #2749)
    const take = (row: TimelineRow & { ts?: string }) => {
      if (row.kind === "progress") {
        const data = row.data as ProgressRow;
        const key = `${data.step}:${data.tool ?? ""}`;
        const was = seen.get(key);
        seen.set(key, { key, kind: data.step === "scan" ? "check" : "after", name: data.step === "scan" ? plainName(data.tool ?? "") : data.step === "build" ? "Build" : "Start in preview",
          status: data.status, position: data.position, ms: data.duration_ms, since: data.status === "running" ? row.ts : was?.since });
      }
      if (row.kind === "check.started" || row.kind === "check.completed") {
        const data = row.data as CheckRow;
        const key = `check:${data.check}`;
        const status = row.kind === "check.started" ? "running" : data.status === "failed" ? "failed" : data.status === "skipped" ? "skipped" : "done";
        seen.set(key, { key, kind: "check", name: AGENT[data.check] ?? data.check, status, ms: data.duration_ms, found: data.findings, since: row.kind === "check.started" ? row.ts : seen.get(key)?.since });
      }
      const reusing = row.kind === "build.log" ? REUSING.exec((row.data as { line: string }).line) : null;
      if (reusing !== null) seen.set("build:", { key: "build:", kind: "after", name: "Build", status: "done", reused: reusing[1] });
      if (row.kind === "build.image") waited = (row.data as { waited_ms: number }).waited_ms;
      if (row.kind === "producer.sarif") {
        const data = row.data as { producer: string; tool?: string; results?: unknown[]; sarif?: { runs: { results: unknown[] }[] } };
        found.set(data.tool ?? data.producer.replace(/^sarif\//, ""), (data.sarif?.runs ?? []).reduce((n, one) => n + one.results.length, data.results?.length ?? 0));
      }
    };
    const read = async (after: number) => {
      try {
        const page = await getTimeline(run, after);
        if (!live) return;
        page.rows.forEach(take);
        const list = [...seen.values()].map((step) => (step.found === undefined && step.key.startsWith("scan:") && found.has(step.key.slice(5)) ? { ...step, found: found.get(step.key.slice(5)) }
          : step.key === "build:" && waited !== undefined ? { ...step, waited } : step));
        setSteps({ run, list });
        if (page.next !== null && going) timer = setTimeout(() => void read(page.next!), page.rows.length > 0 ? 0 : POLL_MS);
      } catch { /* a read that fails is tried again on the next poll; the run read says how it ended */
        if (live && going) timer = setTimeout(() => void read(after), POLL_MS);
      }
    };
    void read(0);
    return () => { live = false; clearTimeout(timer); };
  }, [run, going]);
  return steps === null || steps.run !== run ? [] : steps.list;
}

// Whole seconds first, so 119.6 s reads "2 min 0 s", never "1 min 60 s".
const seconds = (ms: number) => {
  const s = Math.max(1, Math.round(ms / 1000));
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${s % 60} s`;
};
const ordinal = (n: number) => `${n}${n % 100 >= 11 && n % 100 <= 13 ? "th" : ["th", "st", "nd", "rd"][n % 10] ?? "th"}`;

// One step's line: its mark, its name, and where it stands in words.
function line(step: Step, now: number): { icon: IconName; tone: string; said: string; at?: string } {
  switch (step.status) {
    case "queued": return { icon: "waiting", tone: "text-accent", said: step.position === undefined ? "Waiting its turn on this host" : `Waiting its turn: ${ordinal(step.position)} in line on this host`, at: "queued" };
    case "running": return { icon: "working", tone: "text-accent", said: "running", at: step.since === undefined ? undefined : seconds(now - new Date(step.since).getTime()) };
    case "failed": return { icon: "blocked", tone: "text-critical", said: "couldn't finish", at: step.ms === undefined ? undefined : seconds(step.ms) };
    case "skipped": return { icon: "never", tone: "text-text-faint", said: "skipped" };
    case "done": {
      // A build that waited for Canyon's build image first: the wait came before its first line, so its own clock leaves it
      // out; the step counts it and names it, in the CLI's words (D5; #2749).
      const waited = step.reused === undefined && step.ms !== undefined ? step.waited : undefined;
      return {
        icon: step.found !== undefined && step.found > 0 ? "alert" : "passed", tone: step.found !== undefined && step.found > 0 ? "text-amber" : "text-ok",
        said: step.reused !== undefined ? `reused the image built for ${step.reused}` : waited !== undefined ? `done, ${S.buildPrepared.replace("<wait>", () => seconds(waited))}`
          : step.kind === "after" ? "done" : step.found === undefined ? "done" : step.found === 0 ? "nothing found" : `${step.found} ${step.found === 1 ? "finding" : "findings"}`,
        at: step.ms === undefined ? undefined : seconds(step.ms + (waited ?? 0)),
      };
    }
  }
}

// The run as it goes (the app-page redesign, "Reviewing, live"): each check with its state, its time and,
// while it waits, its place in line; then the build and the start, once the run has them.
export function Progress({ steps }: { steps: Step[] }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  if (steps.length === 0) return null;
  // Each group its own list under its name, so a screen reader never counts the name as a step.
  const group = (name: string, list: Step[]) => list.length === 0 ? null : (
    <>
      <p className="pt-1 text-xs font-semibold tracking-caps text-text-faint uppercase" aria-hidden="true">{name}</p>
      <ul aria-label={name} className="grid gap-1.5">
      {list.map((step) => {
        const { icon, tone, said, at } = line(step, now);
        return (
          <li key={step.key} className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-center gap-2 text-md">
            <span className={tone}><Icon name={icon} size={16} /></span>
            <span><span className="font-medium">{step.name}</span> <span className="text-text-dim">{said}</span></span>
            <span className="text-sm text-text-faint tnum">{at ?? ""}</span>
          </li>
        );
      })}
      </ul>
    </>
  );
  return (
    <section aria-label="Progress" className="grid gap-2">
      <h3 className="text-base font-semibold">Progress <span className="text-md font-normal text-text-faint">updates as it happens</span></h3>
      <div className="grid gap-1.5 rounded-[10px] border border-border bg-surface px-4 py-3">
        {group("Review", steps.filter((step) => step.kind === "check"))}
        {group("After review", steps.filter((step) => step.kind === "after"))}
      </div>
    </section>
  );
}

// "4 of 6 checks done", and which one waits its turn: the headline's words for a review going.
export function progressWords(steps: Step[]): string | null {
  const checks = steps.filter((step) => step.kind === "check");
  if (checks.length === 0) return null;
  const done = checks.filter((step) => step.status === "done" || step.status === "failed" || step.status === "skipped").length;
  const waiting = checks.filter((step) => step.status === "queued").map((step) => step.name);
  return `${done} of ${checks.length} checks done.${waiting.length === 0 ? "" : ` ${waiting.join(" and ")} ${waiting.length === 1 ? "is" : "are"} waiting ${waiting.length === 1 ? "its" : "their"} turn on this host.`}`;
}
