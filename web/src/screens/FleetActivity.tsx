import { useEffect, useState } from "react";
import { listAudit, version, type Actor as ActorRef, type AuditEvent } from "../api";
import { actorName, anonymous } from "../ui/Actor";
import { dependency, sentence } from "../ui/Audit";
import { Avatar } from "../ui/Avatar";
import { Icon } from "../ui/Icon";

// The fleet's Recent activity (the approved redesign, "what changed in my apps"): one line per run, the
// production changes, approvals with their reasons, shares, secrets, policy and admin changes (an admin's:
// a member is answered their team apps' rows, contracts/routes.md), and what the org's platform reported. Failures and image mismatches of the last day come first. Sign-ins, log
// reads and resource steps stay on the audit page: the server is asked for exactly these actions, so a
// page is never read and then thinned in the browser.
const UPLOAD = ["POST /api/v1/projects/{id}/packages", "package from github"];
const START = ["POST /api/v1/projects/{id}/gate-runs", "POST /api/v1/projects/{id}/deploys", "gate run from github"];
const FAILED: Record<string, string> = {
  "build failed": "didn't build",
  "start failed": "didn't stay up after it started",
  "detect failed": "can't be built on this host",
  "build had no room": "had no room to build",
};
const PROMOTE = ["POST /api/v1/projects/{id}/promotions", "deploy prod"];
const APPROVE = "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve";
const APPROVE_ALL = "POST /api/v1/gate-runs/{run}/findings/approve";
const RUN = new Set([...UPLOAD, ...START, "verdict", ...Object.keys(FAILED), "deploy preview"]);
export const ACTIVITY_ACTIONS = [
  ...RUN, ...PROMOTE, APPROVE, APPROVE_ALL,
  "share app", "unshare app",
  "PUT /api/v1/projects/{id}/secrets/{name}", "DELETE /api/v1/projects/{id}/secrets/{name}",
  "policy.set", "app_policy.set", "app_policy.remove", "custom_rule.set", "PUT /api/v1/preferences",
  "POST /api/v1/projects", "DELETE /api/v1/projects/{id}", "PUT /api/v1/projects/{id}/owner", "DELETE /api/v1/projects/{id}/envs/{env}",
  "platform reported deployed", "platform reported removed", "platform reported failed", "platform reported another digest",
];
const BURST_MS = 60 * 60 * 1000; // one person's runs on one app within an hour of each other are one burst
const DAY_MS = 24 * 60 * 60 * 1000;
const SHOWN = 12;
const ENV: Record<string, string> = { preview: "preview", prod: "production" };

const text = (value: unknown) => (typeof value === "string" ? value : "");
const named = (event: AuditEvent) => version(text(event.detail.package), typeof event.detail.label === "string" ? event.detail.label : undefined);
const short = (digest: string) => digest.replace(/^(sha256:.{6}).*$/, "$1…");

// One run, told by the furthest step it reached. `id` keys it: a version can be reviewed or deployed
// more than once, and each run is its own line.
interface Run { id: string; app: string; version: string; review: string; outcome: string; at: string; failed: boolean; github: boolean; who: ActorRef | null; deployed: boolean }
export interface Line { key: string; who: ActorRef | null; text: string; note?: string; at: string; red?: true; versions?: Run[] }

// appIn is the app a line names: "" for `here`, the pane's own, else its name in `names`, else its id.
const appIn = (project: string | undefined, here: string | undefined, names: Map<string, string> | undefined) =>
  project === undefined || project === here ? "" : names?.get(project) ?? project;

function runOf(id: string, rows: AuditEvent[], here: string | undefined, names: Map<string, string> | undefined): Run {
  const newest = rows[0]!;
  const app = appIn(newest.project, here, names);
  const v = named(rows.find((row) => typeof row.detail.label === "string") ?? newest);
  const failed = rows.find((row) => FAILED[row.action] !== undefined);
  const verdict = rows.find((row) => row.action === "verdict");
  const deployed = rows.some((row) => row.action === "deploy preview");
  const warnings = Number(verdict?.detail.warnings ?? 0);
  const review = verdict?.detail.state === "blocked" ? `Blocked · ${String(verdict.detail.blocking)} blocking`
    : verdict?.detail.state === "clean" ? `Passed${warnings > 0 ? ` · ${warnings} ${warnings === 1 ? "warning" : "warnings"}` : ""}` : "";
  // A deploy's review may sit on an older page of the trail: then the line says where it went and no more.
  const outcome = failed !== undefined ? FAILED[failed.action]! : deployed ? (review === "" ? "In preview" : `${review} · in preview`) : review || (rows.some((row) => START.includes(row.action)) ? "Started" : "Uploaded");
  // Who ran it: the person or agent behind its start, else its upload, never Canyon's own steps.
  const who = (rows.find((row) => START.includes(row.action)) ?? rows.find((row) => UPLOAD.includes(row.action)) ?? rows.find((row) => row.actor.kind !== "canyon"))?.actor ?? null;
  return { id, app, version: v, review, outcome, at: newest.ts, failed: failed !== undefined, github: rows.some((row) => row.action.includes("github")), who, deployed };
}

// A version as a line names it: its app and its name, or its name alone inside its own app's pane.
const of = (app: string, version: string) => (app === "" ? version : `${app} ${version}`);
const sameWho = (a: ActorRef | null, b: ActorRef | null) => a !== null && b !== null && a.kind === b.kind && a.subject === b.subject;

function runLine(runs: Run[]): Line {
  const top = runs[0]!;
  const from = top.github ? " from GitHub" : "";
  if (top.failed) return { key: top.id, who: top.who, text: `${of(top.app, top.version)} ${top.outcome}.`, note: top.who === null ? undefined : `Run by ${actorName(top.who)}${from}`, at: top.at, red: true };
  if (runs.length > 1) {
    const live = runs.find((run) => run.deployed);
    return { key: `burst ${top.id}`, who: top.who, at: top.at, versions: runs,
      text: `ran ${runs.length} versions${top.app === "" ? "" : ` of ${top.app}`}${from}.${live === undefined ? "" : ` ${live.version} went to preview.`}` };
  }
  // Only a version that passed shipped anywhere: a blocked one was sent for review and stopped there.
  const verb = top.deployed ? "deployed" : top.review.startsWith("Passed") ? "shipped" : top.review.startsWith("Blocked") ? "sent for review" : top.outcome === "Started" ? "started a review of" : "uploaded";
  const said = verb === "sent for review" ? `sent ${of(top.app, top.version)} for review` : `${verb} ${of(top.app, top.version)}${top.deployed ? " to preview" : ""}`;
  return { key: top.id, who: top.who, text: `${said}${from}.`, note: top.review || undefined, at: top.at };
}

// A row that is not a run's, in the trail's words with its app named where the words say "the app" —
// unless it is `here`, the one app a pane shows, which never names itself again.
function oneLine(event: AuditEvent, org: string, here: string | undefined, names: Map<string, string> | undefined): Line {
  const base = { key: event.id, who: event.actor, at: event.ts };
  const app = appIn(event.project, here, names);
  const its = app === "" ? "it" : app;
  const env = ENV[text(event.detail.env)] ?? text(event.detail.env);
  switch (event.action) {
    case "platform reported another digest":
      return { ...base, who: null, red: true, text: `${org}'s platform is running an image Canyon didn't review${app === "" ? "" : ` for ${app}`} in ${env}.`, note: `Reviewed ${short(text(event.detail.expected))} · running ${short(text(event.detail.reported))}` };
    case "platform reported failed":
      return { ...base, who: null, red: true, text: `${org}'s platform couldn't put ${its} live in ${env}.`, note: text(event.detail.reason) || undefined };
    case "platform reported deployed": return { ...base, text: `reported ${its} live in ${env}.` };
    case "platform reported removed": return { ...base, text: `reported ${its} removed from ${env}.` };
    case APPROVE_ALL: {
      const until = text(event.detail.expires_at);
      const reason = text(event.detail.reason);
      const n = Array.isArray(event.detail.findings) ? event.detail.findings.length : 0;
      return { ...base, text: `approved ${n} blocking problems in ${dependency(event.detail)}${app === "" ? "" : ` in ${app}`}.`, note: [reason === "" ? "" : `"${reason}"`, until === "" ? "" : `until ${new Date(until).toLocaleDateString("en-GB", { day: "numeric", month: "short" })}`].filter(Boolean).join(" · ") };
    }
    case APPROVE: {
      const until = text(event.detail.expires_at);
      const reason = text(event.detail.reason);
      return { ...base, text: `approved a blocking problem${app === "" ? "" : ` in ${app}`}.`, note: [text(event.detail.rule), reason === "" ? "" : `"${reason}"`, until === "" ? "" : `until ${new Date(until).toLocaleDateString("en-GB", { day: "numeric", month: "short" })}`].filter(Boolean).join(" · ") };
    }
    case "PUT /api/v1/preferences": {
      const admins = event.detail.admin_emails as { before?: string[] | null; after?: string[] | null } | undefined;
      if (admins === undefined) break;
      const before = admins.before ?? [];
      const after = admins.after ?? [];
      const change = [...after.filter((one) => !before.includes(one)).map((one) => `+${one}`), ...before.filter((one) => !after.includes(one)).map((one) => `−${one}`)];
      if (change.length === 0) break;
      return { ...base, text: `changed who is an admin: ${change.join(", ")}.`, note: "Admin change" };
    }
  }
  const said = sentence(event);
  const where = app === "" ? said : /\bthe app\b/.test(said) ? said.replace(/\bthe app\b/, () => app) : `${said} in ${app}`;
  return { ...base, text: `${where}.` };
}

// The trail, newest first, as lines (`here` the one app a pane reads them for, never named in them): a run's rows become one line — its upload, which names no run,
// joins the newest run of its version — a promotion's two rows one, and one person's runs on one app
// within the hour of the newest a burst. Failures and mismatches of the last day are pinned, at most five.
type Item = { kind: "run" | "promote"; key: string } | { kind: "event"; event: AuditEvent };
export function activity(events: AuditEvent[], org: string, now: number, here?: string, names?: Map<string, string>): { pinned: Line[]; lines: Line[] } {
  const groups = new Map<string, AuditEvent[]>();
  const order: Item[] = [];
  const newestRunOf = new Map<string, string>(); // a version's newest run, for its upload to join
  for (const event of events) {
    const version = `${event.project ?? ""}|${text(event.detail.package)}`;
    const promote = PROMOTE.includes(event.action);
    if (!RUN.has(event.action) && !promote) { order.push({ kind: "event", event }); continue; }
    const key = promote ? `promote ${event.run ?? version}` : event.run !== undefined ? `run ${event.run}` : newestRunOf.get(version) ?? `upload ${version} ${event.id}`;
    if (!promote && event.run !== undefined && !newestRunOf.has(version)) newestRunOf.set(version, key);
    if (!groups.has(key)) { groups.set(key, []); order.push({ kind: promote ? "promote" : "run", key }); }
    groups.get(key)!.push(event);
  }
  const lines: Line[] = [];
  let burst: Run[] = [];
  const flush = () => { if (burst.length > 0) lines.push(runLine(burst)); burst = []; };
  for (const item of order) {
    if (item.kind === "run") {
      const run = runOf(item.key, groups.get(item.key)!, here, names);
      const first = burst[0];
      if (first !== undefined && (run.failed || first.failed || run.app !== first.app || !sameWho(run.who, first.who) || Date.parse(first.at) - Date.parse(run.at) > BURST_MS)) flush();
      burst.push(run);
      continue;
    }
    flush();
    if (item.kind === "promote") {
      const rows = groups.get(item.key)!;
      const done = rows.some((row) => row.action === "deploy prod");
      const who = rows.find((row) => row.action !== "deploy prod")?.actor ?? rows[0]!.actor;
      lines.push({ key: item.key, who, at: rows[0]!.ts, text: `${done ? "promoted" : "started promoting"} ${of(appIn(rows[0]!.project, here, names), named(rows[0]!))} to production.` });
    } else if (item.kind === "event") lines.push(oneLine(item.event, org, here, names));
  }
  flush();
  const pinned = lines.filter((line) => line.red === true && now - Date.parse(line.at) < DAY_MS).slice(0, 5);
  return { pinned, lines: lines.filter((line) => !pinned.includes(line)).slice(0, SHOWN) };
}

const clock = (at: string) => new Date(at).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
function day(at: string, now: number): string {
  const start = (time: number) => new Date(time).setHours(0, 0, 0, 0);
  const days = Math.round((start(now) - start(Date.parse(at))) / DAY_MS);
  return days === 0 ? "Today" : days === 1 ? "Yesterday" : new Date(at).toLocaleDateString("en-GB", { day: "numeric", month: "short" });
}

const capital = (said: string) => said.charAt(0).toUpperCase() + said.slice(1);

// Who is named as the trail names them: the fleet read's viewer carries a display name and no subject,
// and two people can share a name, so nobody is said as "You". A host with sign-in off has nobody signed
// in to name, and no initials to draw (ui/Actor.tsx).
export function Row({ line }: { line: Line }) {
  const name = line.who === null ? "" : anonymous(line.who) ? capital(actorName(line.who)) : actorName(line.who); // it leads the line
  const lead = line.who !== null && !line.red;
  return (
    <li className="grid grid-cols-[28px_minmax(0,1fr)_auto] items-start gap-x-3 py-2.5">
      {line.red ? <span className="mt-1 text-critical"><Icon name="alert" /></span> : !lead || line.who!.kind === "canyon" || anonymous(line.who!) ? <span /> : <Avatar name={name} size="sm" />}
      <div className="grid min-w-0 gap-0.5">
        <p className="text-base text-text">{lead ? <><b className="font-semibold">{name}</b> {line.text}</> : line.red ? line.text : capital(line.text)}</p>
        {line.note !== undefined && <p className={`text-md ${line.red ? "text-critical" : "text-text-dim"}`}>{line.note}</p>}
        {line.versions !== undefined && (
          <details className="text-md text-text-dim">
            <summary className="cursor-pointer text-link">Show {line.versions.length} versions</summary>
            <ul className="mt-1 grid gap-0.5">
              {line.versions.map((run) => <li key={run.id} className="flex gap-3"><span className="tnum w-16 shrink-0 text-text">{run.version}</span><span className="min-w-0 flex-1">{run.outcome}</span><span className="tnum">{clock(run.at)}</span></li>)}
            </ul>
          </details>
        )}
      </div>
      <span className="tnum text-md text-text-dim">{clock(line.at)}</span>
    </li>
  );
}

const GROUP = "pt-3 text-sm font-semibold tracking-wide text-text-faint uppercase";

// Polled like the table above it; `org` names whose platform reported. Two reads: the newest 100 rows
// for the timeline, and the last day's for what is pinned, so a failure 20 hours old in a busy org is
// pinned however far down the timeline's page it sits.
export function RecentActivity({ org, names }: { org: string; names?: Map<string, string> }) {
  const [events, setEvents] = useState<{ newest: AuditEvent[]; day: AuditEvent[] } | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const read = async () => {
      const ask = (more: Record<string, string>) => listAudit(new URLSearchParams({ action: ACTIVITY_ACTIONS.join(","), ...more }).toString());
      try {
        const [newest, day] = await Promise.all([ask({ limit: "100" }), ask({ since: new Date(Date.now() - DAY_MS).toISOString(), limit: "200" })]);
        if (live) { setEvents({ newest: newest.events, day: day.events }); setFailure(null); }
      } catch (err) {
        if (live) setFailure((err as Error).message);
      }
      if (live) timer = setTimeout(read, 2000);
    };
    void read();
    return () => { live = false; clearTimeout(timer); };
  }, []);

  const now = Date.now();
  const { pinned } = activity(events?.day ?? [], org, now, undefined, names);
  const lines = activity(events?.newest ?? [], org, now, undefined, names).lines.filter((line) => !pinned.some((one) => one.key === line.key));
  const days: { label: string; lines: Line[] }[] = [];
  for (const line of lines) {
    const label = day(line.at, now);
    if (days.at(-1)?.label !== label) days.push({ label, lines: [] });
    days.at(-1)!.lines.push(line);
  }
  return (
    <section aria-label="Recent activity">
      <h2 className="flex items-baseline justify-between text-base font-semibold">
        Recent activity
        <a className="text-md font-medium text-link no-underline hover:underline" href="#/audit">Open audit</a>
      </h2>
      {failure !== null && <p className="mt-1.5 text-base text-critical">{failure}</p>}
      {events !== null && pinned.length + lines.length === 0 && <p className="mt-1.5 text-base text-text-faint">Nothing recorded yet. Deploys, reviews and promotions appear here.</p>}
      {pinned.length > 0 && (
        <>
          <h3 className={`${GROUP} text-critical`}>Failures and mismatches · last 24 hours</h3>
          <ul className="divide-y divide-border">{pinned.map((line) => <Row key={line.key} line={line} />)}</ul>
        </>
      )}
      {days.map((group) => (
        <div key={group.label}>
          <h3 className={GROUP}>{group.label}</h3>
          <ul className="divide-y divide-border">{group.lines.map((line) => <Row key={line.key} line={line} />)}</ul>
        </div>
      ))}
      {events !== null && <p className="mt-3 text-md text-text-dim">Sign-ins, log reads and resource steps are on the audit page. <a className="text-link no-underline hover:underline" href="#/audit">Open audit</a></p>}
    </section>
  );
}
