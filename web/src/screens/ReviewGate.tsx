import { useEffect, useState } from "react";
import { entries, getTimeline, type Finding, type ProducerLog, type TimelineRow } from "../api";
import { plainName } from "../ui/Finding";
import { Icon, type IconName } from "../ui/Icon";

// A line of what nothing read carries its rule's policy and the reason apart (contracts/finding.md; #1668).
type Named = { policy_name: string; why: string };
type Said = { level?: string; message: { text: string }; properties?: { canyon?: Named } };
type VerdictRun = { tool: { driver: { name: string } }; automationDetails?: { id: string }; results?: unknown[]; invocations?: { toolExecutionNotifications?: Said[] }[] };
// at is where the row's run of the verdict's log stands among the run read's findings, which flatten that log in
// order: what the row counts there are entries (contracts/finding.md, One problem, one entry), as the CLI's tool rows count.
// status is how the row's check ended, for a tool that is one of the review's checks (contracts/events.md, check.completed).
interface Row { name: string; producer: string; results: number; notes: string[]; at?: [number, number]; status?: string }

const said = (run: VerdictRun, level?: string) => (run.invocations ?? []).flatMap((inv) => (inv.toolExecutionNotifications ?? []).filter((note) => level === undefined || note.level === level).map((note) => note.message.text));

// Every tool of an ended run, once: each producer row, and each tool the verdict's log names that wrote
// none — a scan that did not finish leaves no report, only its run there (contracts/finding.md, *Canyon-run
// tools*). What the run said of each tool is in the verdict's log, under its run, and its warnings join the
// row's notes once: a scan that did not finish says so over an earlier report, and an imported tool's row,
// its attach's summary with no log, gets them all; who attached it is not carried over.
async function rows(run: string): Promise<{ rows: Row[]; named: Map<string, Named> }> {
  const logs: ProducerLog[] = [];
  let verdict: VerdictRun[] = [];
  const status = new Map<string, string>();
  for (let after: number | null = 0; after !== null; ) {
    const page: { rows: TimelineRow[]; next: number | null } = await getTimeline(run, after);
    for (const row of page.rows) {
      if (row.kind === "producer.sarif") logs.push(row.data as ProducerLog);
      if (row.kind === "check.completed") { const done = row.data as { check: string; status: string }; status.set(done.check, done.status); }
      if (row.kind === "verdict") verdict = (row.data as { findings?: { runs?: VerdictRun[] } }).findings?.runs ?? [];
    }
    after = page.next;
  }
  const named = (producer: string, category?: string) => (category ? `${plainName(producer)} (${category})` : plainName(producer));
  const at = new Map<VerdictRun, [number, number]>(); // each verdict run's findings in the run read
  verdict.reduce((start, one) => { at.set(one, [start, start + (one.results?.length ?? 0)]); return start + (one.results?.length ?? 0); }, 0);
  const out: Row[] = logs.map((log) => {
    const runs = log.sarif?.runs ?? [];
    const its = verdict.find((one) => one.tool.driver.name === log.producer && (one.automationDetails?.id ?? "") === (log.category ?? ""));
    return {
      name: named(log.producer, log.category), producer: log.producer,
      results: runs.reduce((n, one) => n + one.results.length, log.results?.length ?? 0), at: its === undefined ? undefined : at.get(its),
      notes: [...new Set([...runs.flatMap((one) => said(one as VerdictRun)), ...(its === undefined ? [] : [...said(its, "warning"), ...said(its, "note").filter((text) => readNoFile(text) || noUI(text))])])],
    };
  });
  for (const one of verdict) {
    const producer = one.tool.driver.name;
    const category = one.automationDetails?.id ?? "";
    if (logs.some((log) => log.producer === producer && (log.category ?? "") === category) || out.some((row) => row.producer === producer && row.name === named(producer, category))) continue;
    out.push({ name: named(producer, category), producer, results: one.results?.length ?? 0, at: at.get(one), notes: [...said(one, "warning"), ...said(one, "note").filter((text) => readNoFile(text) || noUI(text))] });
  }
  const policies = new Map<string, Named>();
  for (const one of [...logs.flatMap((log) => (log.sarif?.runs ?? []) as VerdictRun[]), ...verdict]) {
    for (const note of (one.invocations ?? []).flatMap((inv) => inv.toolExecutionNotifications ?? [])) {
      if (note.properties?.canyon !== undefined) policies.set(note.message.text, note.properties.canyon);
    }
  }
  for (const row of out) row.status = status.get(row.producer);
  return { rows: out, named: policies };
}

// The words of a note that says what nothing read, and of one that says a tool's scan did not finish
// (contracts/finding.md, *What nothing read is named* and *Canyon-run tools*).
const notChecked = (text: string) => text.includes("not checked");
const unfinished = (text: string) => text.includes(" · could not check:");
// A reviewer with no model, and a rule nothing read for want of one, are said once, on the reviewer's row
// (cli.md §3.1's off row): never again under it, never in the group of what nothing read.
const noModel = (text: string) => text.endsWith(" · skipped (no model configured on this Canyon host)");
const unreadForNoModel = (text: string) => text.endsWith(": no model is configured on this Canyon host, so the AI review read nothing");
// A tool that had no file of the package to read says why (contracts/finding.md; #1742).
const NO_FILE = " · read no file: ";
const readNoFile = (text: string) => text.includes(NO_FILE);
// An app with no UI framework had no UI for the design-system rule to check: nothing went unread (contracts/finding.md).
const noUI = (text: string) => text === "design.org-system: no package.json in this app lists a UI framework, so there was no UI to check";

// What one tool's line says of it, with its mark: a scan that didn't finish, a review that was skipped, what
// it found, or nothing — never "passed": a tool that found nothing may have read little, and the group
// below names what nothing read.
function outcome(row: Row, findings: Finding[]): { word: string; icon: IconName; tone: string } {
  if (row.notes.some(unfinished)) return { word: "couldn't finish", icon: "blocked", tone: "text-critical" };
  if (row.notes.some(noModel)) return { word: "skipped, no AI model set up", icon: "never", tone: "text-text-faint" };
  if (row.notes.some((text) => text.includes(" · skipped"))) return { word: "skipped", icon: "never", tone: "text-text-faint" };
  const none = row.notes.find(readNoFile);
  if (none !== undefined) return { word: none.slice(none.indexOf(NO_FILE) + NO_FILE.length), icon: "never", tone: "text-text-faint" };
  if (row.notes.some((text) => text.endsWith(" · no report for this package"))) return { word: "no report", icon: "never", tone: "text-text-faint" };
  const found = row.at === undefined ? row.results : entries(findings.slice(...row.at));
  const counted = `${found} ${found === 1 ? "finding" : "findings"}`;
  // A check that ended skipped read nothing whatever else it found (contracts/finding.md, an agent that read
  // nothing): never a pass, and what it could not read is said under its line.
  if (row.status === "skipped") return { word: found > 0 ? `skipped · ${counted}` : "skipped", icon: "never", tone: "text-text-faint" };
  if (found > 0) return { word: counted, icon: "alert", tone: "text-amber" };
  // Never green while anything went unread.
  if (row.notes.some(notChecked)) return { word: "nothing found", icon: "never", tone: "text-text-faint" };
  return { word: "nothing found", icon: "passed", tone: "text-ok" };
}

// What Canyon checked (the app-page redesign, block 5): every tool once, with its outcome, then what nothing
// read in its own grey group, never counted as passing. Canyon's own run is no tool: what it found is the
// tools that could not finish, each on its own line, and what nothing read, in the group.
export function ReviewGate({ run, findings }: { run: string; findings: Finding[] }) {
  const [read, setRead] = useState<Awaited<ReturnType<typeof rows>> | Error | null>(null);
  useEffect(() => {
    let live = true;
    rows(run).then((got) => { if (live) setRead(got); }, (err: Error) => { if (live) setRead(err); });
    return () => { live = false; };
  }, [run]);
  if (read === null) return null;
  if (read instanceof Error) return <p className="mt-2 text-base text-critical">{read.message}</p>;
  // A skipped check's own lines of what it could not read are said under its line, once (#2294).
  const unread = read.rows.filter((row) => row.status !== "skipped").flatMap((row) => row.notes.filter((text) => notChecked(text) && !unreadForNoModel(text)));
  // Where no scanner rule reads a language, what the AI review read in its place (contracts/finding.md).
  const byAI = read.rows.flatMap((row) => row.notes.filter((text) => text.startsWith("checked by the AI review: ")));
  const nothing = [...new Set(read.rows.flatMap((row) => row.notes.filter(noUI)))];
  // A group of coverage lines, each by its policy's name and its reason when the line carries them (#1668).
  const group = (name: string, lines: string[], icon: IconName) => lines.length === 0 ? null : (
    <section aria-label={name} className="grid gap-1 border-t border-border pt-2">
      <p className="text-xs font-semibold tracking-caps text-text-faint uppercase">{`${name} · ${lines.length}`}</p>
      <ul className="grid gap-1 text-md text-text-dim">
        {lines.map((text, at) => {
          const policy = read.named.get(text);
          return (
            <li key={at} className="grid grid-cols-[18px_minmax(0,1fr)] gap-2">
              <span className="text-text-faint"><Icon name={icon} size={16} /></span>
              <span>{policy === undefined ? text : <><span className="font-medium text-text">{policy.policy_name}</span>{`: ${policy.why}`}</>}</span>
            </li>
          );
        })}
      </ul>
    </section>
  );
  const tools = read.rows.filter((row) => row.producer !== "canyon/scanners");
  return (
    <section aria-label="What Canyon checked" className="grid gap-2 rounded-[10px] border border-border bg-surface px-4 py-3">
      <h3 className="text-md font-semibold">What Canyon checked</h3>
      <ul aria-label="Review gate" className="grid gap-1.5 text-md">
        {tools.map((row) => {
          const { word, icon, tone } = outcome(row, findings);
          // What a tool said of its own run, under its line, unless it is the reason already in its word.
          // "No report" is the line's word, or the why that outranks it: never said again under it.
          const skipped = row.status === "skipped";
          const notes = row.notes.filter((text) => (skipped || !notChecked(text)) && !unfinished(text) && !readNoFile(text) && !noModel(text) && !text.endsWith(" · no report for this package"));
          const missed = skipped ? 0 : row.notes.filter(notChecked).length;
          // under a check's own row its lines drop its name, as the CLI's do (cli.md §3.1); in the group they keep it
          const named = `${row.name.toLowerCase()} · `;
          const own = (text: string) => (row.status !== undefined && text.startsWith(named) ? text.slice(named.length) : text);
          return (
            <li key={row.name}>
              <p className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-center gap-2">
                <span className={tone}><Icon name={icon} size={16} /></span>
                <span className="font-medium">{row.name}</span>
                <span className="text-text-dim">{missed > 0 ? `${word} · ${missed} not checked` : word}</span>
              </p>
              {/* what a skipped check could not read leads, as the group would say it */}
              {skipped && notes.filter(notChecked).map((text, at) => <p key={`unread-${at}`} className="pl-[26px] text-sm text-text-dim">{own(text)}</p>)}
              {notes.filter((text) => !skipped || !notChecked(text)).map((text, at) => <p key={at} className="pl-[26px] text-sm text-text-faint">{own(text)}</p>)}
            </li>
          );
        })}
      </ul>
      {group("Checked by the AI review", byAI, "passed")}
      {group("Didn't apply", nothing, "passed")}
      {group("Not checked", unread, "never")}
    </section>
  );
}
