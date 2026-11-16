import type { ReactNode } from "react";
import { Icon, type IconName } from "../ui/Icon";

// One stage of one version on its way to production: how far it got, in a word, with a mark that
// says passed, stopped, running, waiting, or never got there. The word always carries the meaning.
export interface Cell { mark: "pass" | "stop" | "live" | "wait" | "none"; text: ReactNode; sub?: ReactNode }
export interface Version { key: string; name: string; who: ReactNode; review: Cell; preview: Cell; checks: Cell; production: Cell }

const MARK: Record<Cell["mark"], { icon: IconName; tone: string }> = {
  pass: { icon: "passed", tone: "text-ok" },
  stop: { icon: "blocked", tone: "text-critical" },
  live: { icon: "live", tone: "text-ok" },
  wait: { icon: "waiting", tone: "text-amber" },
  none: { icon: "never", tone: "text-text-faint" },
};
const STAGES = ["Review", "Preview", "Go-live checks", "Production"] as const;

// The lines between a version's stages: solid between two it reached, dashed on from where it
// stopped, and none across a gap it simply has no part in (a version in production, gone from preview).
function links(cells: Cell[]) {
  const stop = cells.findIndex((one) => one.mark === "stop");
  return cells.slice(0, -1).map((one, at) =>
    stop !== -1 && at >= stop ? "dashed" as const : one.mark !== "none" && cells[at + 1]!.mark !== "none" ? "solid" as const : undefined);
}

function Stage({ cell, name, link }: { cell: Cell; name: string; link?: "dashed" | "solid" }) {
  const { icon, tone } = MARK[cell.mark];
  return (
    <div role="cell" aria-label={name} className="flex min-w-0 items-start gap-2 text-md max-[860px]:pl-1 max-[860px]:before:w-[104px] max-[860px]:before:shrink-0 max-[860px]:before:text-text-faint max-[860px]:before:content-[attr(aria-label)]">
      <span className={`mt-0.5 ${tone}`}><Icon name={icon} /></span>
      <span className="grid min-w-0 leading-snug">
        <span className={cell.mark === "none" ? "text-text-faint" : cell.mark === "stop" ? "font-medium text-critical" : "font-medium text-text"}>{cell.text}</span>
        {cell.sub !== undefined && <span className="text-sm text-text-faint [overflow-wrap:anywhere]">{cell.sub}</span>}
      </span>
      {link !== undefined && <span aria-hidden="true" className={`mt-2.5 min-w-4 flex-1 border-t max-[860px]:hidden ${link === "dashed" ? "border-dashed border-border-strong" : "border-ok"}`} />}
    </div>
  );
}

// The versions on their way to production — the newest, and what preview runs, when production runs
// neither — on the same four stages, each stopping where it stopped; its cells are labels, never
// sentences (the app-page redesign, block 3). The page shows it only while a version is on its way:
// on a live app the cards above already say everything.
export function Versions({ rows }: { rows: Version[] }) {
  return (
    <section aria-label="Versions on their way" className="grid gap-3">
      <h2 className="text-base font-semibold">Versions on their way</h2>
      <div role="table" aria-label="Versions" className="overflow-hidden rounded-[10px] border border-border bg-surface">
        <div role="row" className="grid grid-cols-[170px_repeat(4,minmax(0,1fr))] gap-3 bg-surface-2 px-4 py-2.5 text-sm font-semibold text-text-faint max-[860px]:hidden">
          <span role="columnheader">Version</span>
          {STAGES.map((stage) => <span key={stage} role="columnheader">{stage}</span>)}
        </div>
        {rows.map((row) => (
          <div key={row.key} role="row" className="grid grid-cols-[170px_repeat(4,minmax(0,1fr))] items-start gap-3 border-t border-border px-4 py-3.5 first:border-t-0 max-[860px]:grid-cols-1 max-[860px]:gap-2">
            <div role="rowheader" className="grid leading-snug">
              <span className="tnum text-base font-semibold">{row.name}</span>
              <span className="text-sm text-text-faint">{row.who}</span>
            </div>
            {(() => {
              const [toPreview, toChecks, toProduction] = links([row.review, row.preview, row.checks, row.production]);
              return (
                <>
                  <Stage cell={row.review} name="Review" link={toPreview} />
                  <Stage cell={row.preview} name="Preview" link={toChecks} />
                  <Stage cell={row.checks} name="Go-live checks" link={toProduction} />
                  <Stage cell={row.production} name="Production" />
                </>
              );
            })()}
          </div>
        ))}
      </div>
    </section>
  );
}
