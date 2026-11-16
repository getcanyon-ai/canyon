import { useEffect, useState } from "react";
import { getTimeline, type RunRead, type TimelineRow } from "../api";
import { Button } from "../ui/Button";

// How many of the build log's last lines the page shows: the failing line, then the ones before it.
const LINES = 20;

// A terminal's control sequences (ESC [ … letter): the lifecycle colours its own lines, and the page shows the words.
const CONTROL = /\u001b\[[0-9;?]*[A-Za-z]/g;

// The run's last build.log lines, oldest first: the build's output, or the app's own output before it
// stopped, with the executor's reason last (contracts/events.md, build.log).
async function lastLines(run: string) {
  const lines: string[] = [];
  for (let after: number | null = 0; after !== null; ) {
    const page: { rows: TimelineRow[]; next: number | null } = await getTimeline(run, after);
    for (const row of page.rows) if (row.kind === "build.log") lines.push((row.data as { line: string }).line.replace(CONTROL, ""));
    lines.splice(0, Math.max(0, lines.length - LINES - 1));
    after = page.next;
  }
  return lines;
}

// What broke after a review passed, read once per run: the lines, and which of them says why. A start
// that failed writes the app's last lines, then the executor's reason (contracts/events.md, build.log):
// no one line of the app's is its cause for every runtime — Node ends each crash with its version, under
// the error (#2352) — so a start names the reason alone and its lines say why; a build's last line is its message.
export interface Broke { lines: string[]; cause: string | null; at: number; reason: string | null }
export function useBroke(run: RunRead | null): Broke | Error | null {
  // Each read keeps the run it was of, an error too: a read of another run is never this one's answer.
  const [read, setRead] = useState<{ run: string; lines: string[] | Error } | null>(null);
  const id = run?.id ?? null;
  useEffect(() => {
    if (id === null) return;
    let live = true;
    lastLines(id).then((lines) => { if (live) setRead({ run: id, lines }); }, (err: Error) => { if (live) setRead({ run: id, lines: err }); });
    return () => { live = false; };
  }, [id]);
  if (read === null || run === null || read.run !== run.id) return null;
  const { lines } = read;
  if (lines instanceof Error) return lines;
  if (run.failure_reason === "start") return { lines, cause: null, at: -1, reason: lines.at(-1) ?? null };
  return { lines, cause: lines.at(-1) ?? null, at: lines.length - 1, reason: null };
}

// The lines, open: a build's last line, marked, is the executor's message, which rarely says what the
// compiler said, and a start's say why above the executor's reason, no line of them marked (#2352).
export function BuildFailure({ run, broke }: { run: RunRead; broke: Broke | Error }) {
  if (broke instanceof Error) return <p className="text-base text-critical">{broke.message}</p>;
  if (broke.lines.length === 0) return <p className="text-md text-text-dim">The run recorded no output.</p>;
  const start = run.failure_reason === "start";
  return (
    <details open className="group grid gap-1.5">
      <summary className="cursor-pointer text-md font-semibold text-text-dim">
        {start ? "The app's last lines" : "The build's last lines"}
        <span className="font-normal text-text-faint">{start ? " · build output, then the app's own" : ""}</span>
      </summary>
      <pre className="mt-1.5 overflow-x-auto rounded-lg bg-surface-2 px-3 py-2 font-mono text-sm whitespace-pre-wrap text-text">
        {broke.lines.map((line, at) => (
          <span key={at} className="block">{at === broke.at ? <mark className="rounded-sm bg-critical-dim px-0.5 text-critical">{line}</mark> : line}</span>
        ))}
      </pre>
    </details>
  );
}

// Copy for your agent on a failure (the app-page redesign): the headline, the failing line and the last
// lines, then the one step after the fix, as a finding's card copies its own (ui/Finding.tsx, forAgent).
export function CopyForAgent({ app, title, text, broke }: { app: string; title: string; text: string; broke: Broke }) {
  const [copied, setCopied] = useState<"yes" | "no" | null>(null);
  const indent = (lines: string[]) => lines.map((line) => `  ${line}`).join("\n");
  const words = [`Fix why Canyon couldn't deploy ${app}:\n${title}\n${text}`, ...(broke.cause === null ? [] : [`failing line:\n${indent([broke.cause])}`]), `last lines:\n${indent(broke.lines)}`,
    "Then run: canyon deploy\nIt reviews and builds the code again. If it still fails after two full rounds of fixes,\nstop and show the user these lines. Do not keep looping."].join("\n\n") + "\n";
  // navigator.clipboard is absent outside a secure context, a plain-http host: that is a failure too.
  const copy = () => {
    Promise.resolve().then(() => navigator.clipboard.writeText(words)).then(
      () => { setCopied("yes"); setTimeout(() => setCopied(null), 2000); },
      () => setCopied("no"),
    );
  };
  return (
    <>
      {copied === "yes" && <span role="status" className="text-md text-ok">Copied</span>}
      {copied === "no" && <span role="alert" className="text-md text-critical">Couldn't copy. The browser did not allow it.</span>}
      <Button variant="secondary" onClick={copy}>Copy for your agent</Button>
    </>
  );
}
