import { version, waiting, type Actor as ActorRef, type Env, type Pending as PendingRelease, type ViewerEnv, type Waiting } from "../api";
import { Actor } from "./Actor";
import { Age, age, ago } from "./Age";
import { Icon } from "./Icon";
import { S } from "../strings";

// What is live in one environment: the host as the link, then the facts. An environment with
// nothing in it is a fact about the environment, not a stage, and the caller renders it —
// `EnvCell` below is the pair a fleet row and an owner card render; the app page renders its own,
// bigger, with the actor and the run.
interface EnvLinkProps {
  url: string | undefined; // undefined for an app that stopped: nothing answers there, so nothing links to it
  version: string;
  deployedAt: string;
  by?: ActorRef | null;
}

export function EnvLink({ url, version, deployedAt, by }: EnvLinkProps) {
  return (
    <span>
      {url !== undefined && (
        <a
          className="mr-1.5 text-md text-link no-underline hover:underline"
          href={url}
          target="_blank"
          rel="noreferrer"
        >
          {new URL(url).host} ↗
        </a>
      )}
      <span className="text-sm text-text-faint">
        {version} · <Age at={deployedAt} />
        {by !== undefined && <> · <Actor actor={by} /></>}
      </span>
    </span>
  );
}

// How the screen draws the release an environment waits on (contracts/routes.md, envs.<env>.pending).
const clock = (at: string) => new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });

export function pendingLine(p: PendingRelease): string {
  if (p.via === "repo") return repoLine(p);
  if (p.state === "undelivered") return "Your platform did not accept the release";
  if (p.state === "refused") return "Your platform refused the release";
  if (p.event === "remove") return S.removingPlatform;
  if (p.missing !== undefined && p.missing.length > 0) return `Your platform's report left out where ${p.missing.join(", ")} ${p.missing.length === 1 ? "is" : "are"}`;
  return S.pendingPlatform.replace("<time>", clock(p.sent_at));
}

// A delivery: repo release's line (design/EXPERIENCE.md, the environment's pending line under delivery: repo; #2470).
function repoLine(p: PendingRelease): string {
  if (p.state === "undelivered") return S.repoUndelivered;
  if (p.state === "refused") return S.repoRefused;
  if (p.event === "remove") return p.pull_request === undefined ? S.removingPlatform : S.removingMerge;
  return (p.pull_request === undefined ? S.pendingPod : S.pendingMerge).replace("<time>", clock(p.sent_at));
}

// The pull request a release waits on, in the deploy repo: a contributor's and above, as pending is.
export function PullLink({ p }: { p: PendingRelease }) {
  if (p.pull_request === undefined) return null;
  return (
    <a className="ml-1.5 text-sm text-link no-underline hover:underline" href={p.pull_request.url} target="_blank" rel="noreferrer">
      {S.pendingPR.replace("<n>", String(p.pull_request.number))} ↗
    </a>
  );
}

export function EnvCell({ env }: { env: Env | Waiting | undefined }) {
  if (env === undefined) return <span className="text-text-faint">Not deployed</span>;
  if (waiting(env)) return <span className="text-text-faint">{pendingLine(env.pending)}<PullLink p={env.pending} /></span>;
  return (
    <>
      <EnvLink url={opens(env)} version={version(env.package, env.label)} deployedAt={env.deployed_at} />
      <NotRunning env={env} />
      <NoCheckIn env={env} />
      <Pending env={env} />
    </>
  );
}

// The release an environment that runs something waits on, said under what runs (HAND-OFF §4: never
// silent); nothing while none is open.
export function Pending({ env }: { env: Env }) {
  return env.pending === undefined ? null : <span className="block text-sm text-text-faint">{pendingLine(env.pending)}<PullLink p={env.pending} /></span>;
}

// An environment that runs nothing to count on: it stopped, or it keeps restarting, up or not at this
// instant (contracts/routes.md, envs.<env>.state; #1722).
export const down = (env: ViewerEnv) => env.state === "stopped" || env.state === "restarting";

// What an environment deployed there is not doing, in plain words: stopped, restarting, or not known to
// run because Canyon could not ask (contracts/routes.md, envs.<env>.state); null while it runs, or where
// the org runs it.
export const notRunning = (env: ViewerEnv) => (env.state === "stopped" ? "Stopped" : env.state === "restarting" ? "Keeps restarting"
  : env.state === undefined || env.state === "running" ? null : "Couldn't check whether it runs"); // unknown, or a state this build does not know

// How often a restarting environment was started again since its deploy, as the status read says it
// (contracts/routes.md, envs.<env>.restarting): "7 times since it was deployed 2 hours ago"; null when
// the read carries no count.
export const restarts = (env: Env) => (env.restarting === undefined ? null
  : `${env.restarting.restarts === 1 ? "once" : `${env.restarting.restarts} times`} since it was deployed ${ago(env.deployed_at)}`);

const exit = (code: number, said: string) => `exited ${code}${said === "" ? "" : ` — ${said}`}`;

// Why a stopped environment stopped, as the status read says it (contracts/routes.md, envs.<env>.stopped):
// its last exit code and its last line, masked by the server; null when the executor did not say.
export const exited = (env: Env) => (env.stopped === undefined ? null : exit(env.stopped.exit_code, env.stopped.said));

// How a restarting environment last exited, as the status read says it (envs.<env>.restarting; #1597):
// "last exited 1 — <its last line>"; null when the executor did not say.
export const lastExited = (env: Env) => (env.restarting?.exit_code === undefined ? null : `last ${exit(env.restarting.exit_code, env.restarting.said ?? "")}`);

// Where an environment opens: nowhere once it stopped or while it keeps restarting, its address
// otherwise — one Canyon could not check may well answer.
export const opens = (env: ViewerEnv) => (down(env) ? undefined : env.url);

// That line, said under the environment: red when it stopped or keeps restarting, amber when Canyon could not ask.
export function NotRunning({ env }: { env: ViewerEnv }) {
  const not = notRunning(env);
  return not === null ? null : <span className={`block text-sm ${down(env) ? "text-critical" : "text-amber"}`}>{not}</span>;
}

// The fleet's Preview and Production cells: the version that runs there, which opens the app, or a
// dash. A release the platform has not taken yet is said under it, as everywhere; an app that is not
// running says so under its version: one that stopped opens nothing, one Canyon could not check still opens.
export function EnvVersion({ env, name }: { env: Env | Waiting | undefined; name: string }) {
  if (env === undefined) return <span className="text-text-faint">—</span>;
  if (waiting(env)) return <span className="text-sm text-text-faint">{pendingLine(env.pending)}</span>;
  const shown = version(env.package, env.label);
  const url = opens(env);
  return (
    <>
      {url === undefined ? <span className="tnum inline-flex items-center gap-1.5 text-text-dim">{shown}</span> : (
        <a className="tnum inline-flex items-center gap-1.5 text-text no-underline hover:underline" href={url} target="_blank" rel="noreferrer" aria-label={`${shown}, open ${name}`}>
          <span aria-hidden="true" className={`size-2 rounded-full ${env.state === "unknown" ? "bg-amber" : "bg-ok"}`} />{shown}<span className="text-text-faint"><Icon name="external" size={14} /></span>
        </a>
      )}
      <NotRunning env={env} />
      <NoCheckIn env={env} />
      <Pending env={env} />
    </>
  );
}

// A read-only member's way into an environment (contracts/routes.md, the project read): `Open preview` or
// `Open production`, and the team's words when it does not run — one that stopped opens nothing.
export function EnvOpen({ env, name }: { env: ViewerEnv; name: "preview" | "production" }) {
  const url = opens(env);
  return (
    <span>
      {url === undefined ? <span className="text-text-dim">{`Open ${name}`}</span> : <a className="text-link no-underline hover:underline" href={url} target="_blank" rel="noreferrer">{`Open ${name}`}</a>}
      <NotRunning env={env} />
      <NoCheckIn env={env} />
    </span>
  );
}

// Every surface reads a pod's check-in by one rule (contracts/routes.md, envs.<env>.seen_at; #2452): fresh within
// five minutes, and none at all said once ten have passed since the report.
const STALE_MS = 5 * 60_000;
const NEVER_MS = 10 * 60_000;

// When a check-in that is not fresh was, as the reader's clock says it: the time today, the day too before today.
function since(at: Date, now: Date): string {
  const today = at.toDateString() === now.toDateString();
  return today ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : at.toLocaleString([], { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
}

// What an environment the org runs says of its pods (EXPERIENCE.md D5): a pod checked in, none since a time, or
// none at all once a pod should be up — the two last a warning; null for one Canyon runs, within the first ten
// minutes, and on a read-only member's card with no check-in, which carries no deploy time. It is the pods'
// sign-in sidecar checking in, never the app: never "Running", never green. A check-in ahead of the clock is now.
// While the environment waits on a remove (an undeploy's, a hold's or a delete's) its pods pull a list that admits
// nobody, so a fresh check-in is a warning: a pod the platform has not taken down yet (#2450). silent is the two
// lines the fleet says too: the pods stopped checking in, or never began (#2455).
export function checkIn(env: ViewerEnv | Env, now = Date.now()): { line: string; warn: boolean; silent: boolean } | null {
  const full = "package" in env ? (env as Env) : null;
  if (env.state !== undefined || (full !== null && full.run_by !== "org")) return null;
  if (env.seen_at !== undefined) {
    const seen = new Date(env.seen_at);
    if (Number.isNaN(seen.getTime())) return null;
    const still = full?.pending?.event === "remove";
    const said = (ago: string) => (still ? S.reportedStill : S.reported).replace("<ago>", ago);
    if (now - seen.getTime() < 60_000) return { line: said("just now"), warn: still, silent: false }; // as canyon status says the first minute
    if (now - seen.getTime() <= STALE_MS) return { line: said(`${age(env.seen_at)} ago`), warn: still, silent: false };
    return { line: S.notReportedSince.replace("<version>", full === null ? "it" : version(full.package, full.label)).replace("<time>", since(seen, new Date(now))), warn: true, silent: true };
  }
  if (full !== null && now - new Date(full.deployed_at).getTime() > NEVER_MS) return { line: S.reportedNotRunning.replace("<version>", version(full.package, full.label)), warn: true, silent: true };
  return null;
}

// The fleet's word on an environment the org runs whose pods stopped checking in, or never began (EXPERIENCE.md
// D5; #2455): the app page's amber line, under the version, on every row that carries the environment; nothing
// while they check in, nor for a pod still pulling while its environment is removed, which the app page says.
export function NoCheckIn({ env }: { env: ViewerEnv | Env }) {
  const pods = checkIn(env);
  return pods === null || !pods.silent ? null : <span className="block text-sm text-amber"><time dateTime={env.seen_at}>{pods.line}</time></span>;
}
