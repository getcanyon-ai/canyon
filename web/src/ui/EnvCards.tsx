import type { ReactNode } from "react";
import { pullRequestName, version, waiting, type Env, type ViewerEnv, type Waiting } from "../api";
import { Age } from "./Age";
import { checkIn, down, exited, lastExited, notRunning, pendingLine, PullLink, restarts } from "./EnvLink";
import { Icon } from "./Icon";
import { Patched, PatchLine } from "./Patched";

// What one environment runs right now, at a glance (the app-page redesign, block 2): its state, the
// version, where it answers, who put it there and when. An empty one is dashed and says so; a stopped
// one is red and says why. Only a running app, or one Canyon couldn't check, has an Open link.
export function EnvCard({ name, env, empty, blank = "Nothing running", who, note, preview, calls, lined, children }: {
  name: "Preview" | "Production";
  env: Env | Waiting | ViewerEnv | undefined;
  empty: string; // what an empty one says under its blank
  blank?: string; // an empty one's state: "Nothing running", else why the card has nothing to show
  who?: ReactNode; // who put it there, for the team's read
  note?: string; // what production carries on its face, D5's line: "<version> · passed review"
  preview?: Env | Waiting | ViewerEnv; // production's card: what preview runs, which a patch held behind it names
  calls?: ReactNode; // what it calls out to, and how much of it Canyon refused, the Outbound card a click away
  lined?: boolean; // the headline shows its last lines: canyon logs is no longer where the rest is (#188)
  children?: ReactNode; // what the card gains under its facts: production's earlier versions
}) {
  const label = <span className="col-span-2 text-sm font-semibold tracking-wide text-text-faint uppercase">{name}</span>;
  if (env === undefined) {
    return (
      <section aria-label={name} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-3 gap-y-0.5 rounded-[10px] border border-dashed border-border-strong px-4 py-3">
        {label}
        <p className="text-base text-text-dim">{blank}</p>
        <p className="col-start-1 text-sm text-text-faint">{empty}</p>
        {children}
      </section>
    );
  }
  if ("pending" in env && waiting(env as Env)) {
    return (
      <section aria-label={name} className="grid gap-y-0.5 rounded-[10px] border border-border bg-surface px-4 py-3">
        {label}
        <p className="text-base font-semibold">Waiting on your platform</p>
        <p className="text-sm text-text-faint">{pendingLine((env as Waiting).pending)}<PullLink p={(env as Waiting).pending} /></p>
        {children}
      </section>
    );
  }
  const at = env as ViewerEnv; // the waiting case is above: what is left runs something
  const full = "package" in env ? (env as Env) : null;
  const stopped = down(at);
  const pods = checkIn(at);
  // Only the executor's word is "running": where the org runs it Canyon has no state, and says only that it was deployed.
  const state = notRunning(at) ?? (at.state === "running" ? "Running" : "Deployed");
  const shown = full === null ? null : full.pull_request ? pullRequestName(full.pull_request) : version(full.package, full.label);
  // Under the state: why it stopped and where the rest is, or where it answers, its review, who and when.
  // A read-only member's stopped card says no more: the reduced read names no reason, and logs are the team's.
  const why = full === null ? [] : (at.state === "restarting" ? [restarts(full) === null ? null : `started again ${restarts(full)}`, lastExited(full)] : [exited(full)]).filter((one) => one !== null);
  const facts: ReactNode[] = (stopped ? (full === null ? [] : (why.length > 0 ? [...why, lined ? null : "canyon logs has the rest"] : ["canyon logs says why."]))
    : [new URL(at.url).host, who, full === null ? null : <Age at={full.deployed_at} long />]).filter((one) => one !== null && one !== undefined);
  return (
    <section aria-label={name} className={`grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-0.5 rounded-[10px] border bg-surface px-4 py-3 ${stopped ? "border-critical/40" : "border-border"}`}>
      {label}
      <p className="flex flex-wrap items-center gap-2 text-base font-semibold">
        <span aria-hidden="true" className={`size-2 shrink-0 rounded-full ${stopped ? "bg-critical" : at.state === "unknown" || pods?.warn ? "bg-amber" : at.state === "running" ? "bg-ok" : "bg-border-strong"}`} />
        {state}
        {shown !== null && <span className="tnum font-medium"><bdi>{shown}</bdi></span>}
      </p>
      {!stopped && at.state !== "starting" && ( // starting: nothing answers there yet (#230)
        <a className="row-span-2 inline-flex items-center gap-1 text-base text-link no-underline hover:underline" href={at.url} target="_blank" rel="noreferrer" aria-label={shown === null ? `Open ${name.toLowerCase()}` : `Open ${shown} in ${name.toLowerCase()}`}>
          Open<Icon name="external" size={14} />
        </a>
      )}
      <p className={`col-start-1 text-sm break-words ${stopped ? "text-critical" : "text-text-faint"}`}>
        {facts.map((one, at) => <span key={at}>{at > 0 && " · "}{one}</span>)}
      </p>
      {pods !== null && <p className={`col-start-1 text-sm ${pods.warn ? "text-amber" : "text-text-faint"}`}><time dateTime={at.seen_at}>{pods.line}</time></p>}
      {full?.answered !== undefined && !stopped && <Answered answered={full.answered} />}
      {note !== undefined && !stopped && <p className="col-start-1 text-sm text-text-dim">{note}</p>}
      {calls !== undefined && <p className="col-start-1 text-sm">{calls}</p>}
      <Patched env={env} className="col-start-1" />{/* the team's read alone carries a patch */}
      <PatchLine env={env} name={name === "Preview" ? "preview" : "production"} preview={preview} className="col-start-1" />
      {full?.pending !== undefined && <p className="col-start-1 text-sm text-text-faint">{pendingLine(full.pending)}<PullLink p={full.pending} /></p>}
      {children}
    </section>
  );
}

// What the app answered Canyon's one GET / after its deploy (contracts/routes.md, envs.<env>.answered): a
// fact, never a verdict, since a 401 or a 302 at / can be right, so it is never red; the body's first line under it.
function Answered({ answered }: { answered: NonNullable<Env["answered"]> }) {
  const said = "error" in answered ? `No answer at /: ${answered.error}` : `Answered ${answered.status} at /`;
  return (
    <p className="col-start-1 text-sm text-text-faint break-words">
      {said}
      {"said" in answered && answered.said !== "" && <span className="block font-mono text-xs">{answered.said}</span>}
    </p>
  );
}
