import { useEffect, useState } from "react";
import { getPromotionApproval, getTimeline, version, type Env, type TimelineRow, type ViewerEnv, type Waiting } from "../api";
import { PATCHING } from "../strings";
import { shortDate } from "./Verdict";

// A digest's first 12 hex characters, as a person reads an image apart: sha256:4f1c0b9a7e2a…, or a reference by it
// (registry.example/canyon-builder@sha256:4f1c0b9a7e2a…), is 4f1c0b9a7e2a. A reference by no digest is said whole.
export function hex12(image: string): string {
  const at = image.lastIndexOf("sha256:");
  return at < 0 ? image : image.slice(at + 7, at + 19);
}

// An environment's patch line (design/PATCHING.md §8), the fleet's cells and the app page's cards alike: the date
// Canyon's patch reached it, or, while a patch approval of production is open, the way to it. Only a read at
// contributor and above carries envs.<env>.patch: a reduced row, a viewer's, says nothing.
export function Patched({ env, className = "" }: { env: Env | Waiting | ViewerEnv | undefined; className?: string }) {
  const patch = env !== undefined && "package" in env ? env.patch : undefined; // what runs something, read whole
  if (patch?.state === "patched_preview" || patch?.state === "patched") {
    return <span className={`block text-sm text-text-dim ${className}`}>{PATCHING.patchedBy.replace("<date>", () => shortDate(patch.since))}</span>;
  }
  if (patch?.state === "asked" && patch.approval !== undefined) {
    return <a className={`block text-sm font-medium text-amber no-underline hover:underline ${className}`} href={`#/approve?code=${encodeURIComponent(patch.approval)}`}>{PATCHING.waiting}</a>;
  }
  return null;
}

// The held whys a patch run's end does not give: canyon status says these in its own words, and so do the cards.
const HELD_BY = ["blocked", "production_held", "paused", "preview_ahead"];

// The last build.log line of a run: a patch run's end, or the revert its watch wrote (contracts/routes.md, Patch runs).
async function lastLine(run: string): Promise<string> {
  let line = "";
  for (let after: number | null = 0; after !== null; ) {
    const page: { rows: TimelineRow[]; next: number | null } = await getTimeline(run, after);
    for (const row of page.rows) if (row.kind === "build.log") line = (row.data as { line: string }).line;
    after = page.next;
  }
  return line;
}

// Each package the denied approval fixes, once, as production still runs it: the approval lists one fix per vulnerability.
async function kept(code: string): Promise<string> {
  const fixes = (await getPromotionApproval(code)).fixes ?? [];
  return [...new Set(fixes.map((fix) => `${fix.name} ${fix.from}`))].join(", ");
}

// What the app page's card says of an environment's patch when it went wrong or was declined (#2602, #2601), as
// canyon status's platform row says it: a revert in red, a hold or a denial in amber. A revert and a patch run's end
// are their run's own last line, read once; a denial names what production still runs from the approval's read.
export function PatchLine({ env, name, preview, className = "" }: { env: Env | Waiting | ViewerEnv | undefined; name: "preview" | "production"; preview?: Env | Waiting | ViewerEnv; className?: string }) {
  const patch = env !== undefined && "package" in env ? env.patch : undefined;
  const named = (one: Env | Waiting | ViewerEnv | undefined) => (one !== undefined && "package" in one ? version(one.package, one.label) : "it");
  // what the line is read from: the run whose last line it is, or the denied approval whose fixes it names
  const run = patch?.state === "reverted" || (patch?.state === "held" && !HELD_BY.includes(patch.why ?? "")) ? patch.run : undefined;
  const code = patch?.state === "denied" ? patch.approval : undefined;
  const key = run !== undefined ? `run:${run}` : code !== undefined ? `code:${code}` : "";
  const [said, setSaid] = useState<{ key: string; text: string | null } | null>(null);
  useEffect(() => {
    if (key === "") return;
    let live = true;
    const answer = (text: string | null) => { if (live) setSaid({ key, text }); };
    (run !== undefined ? lastLine(run) : kept(code!)).then((text) => answer(text === "" ? null : text), () => answer(null));
    return () => { live = false; };
  }, [key]); // run and code are key's: a new one reads again, a re-render does not
  if (patch === undefined || (key !== "" && said?.key !== key)) return null; // nothing to say, or still reading it
  const fetched = key === "" ? null : said!.text; // null when there was nothing to read, or the read had nothing
  const why = patch.why ?? "";
  const text = patch.state === "reverted" ? fetched ?? PATCHING.reverted.replace("<env>", name)
    : patch.state === "held" && HELD_BY.includes(why) ? (PATCHING.held[why] ?? "").replace("<version>", () => named(why === "preview_ahead" ? preview : env))
    : patch.state === "held" ? PATCHING.heldLine.replace("<line>", () => fetched ?? PATCHING.unfinished.replace("<version>", () => named(env)))
    : patch.state === "denied" ? (fetched === null ? PATCHING.declinedUnread : PATCHING.declined.replace("<packages>", () => fetched))
    : null;
  if (text === null) return null;
  return <p className={`block text-sm ${patch.state === "reverted" ? "text-critical" : "text-amber"} ${className}`}>{text}</p>;
}
