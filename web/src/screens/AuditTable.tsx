import { Fragment, useState } from "react";
import { version, type AuditEvent, type Via } from "../api";
import { ACCESS_WORD } from "../strings";
import { Actor, actorName, anonymous } from "../ui/Actor";
import { dependency, sentence } from "../ui/Audit";
import { Avatar } from "../ui/Avatar";
import { Icon } from "../ui/Icon";
import { Pill, type Tone } from "../ui/Pill";

// The audit page as the approved redesign draws it ("Audit: who did what, when, how, and what it
// changed"): fixed columns an auditor reads down — Time, Who, What, App, Result — the verb first and
// its specifics lighter after it, grouped by day, and each row opening on what it changed and its record.

const ENV: Record<string, string> = { preview: "preview", prod: "production" };
const text = (value: unknown) => (typeof value === "string" ? value : "");
const list = (value: unknown) => (Array.isArray(value) ? value.filter((one): one is string => typeof one === "string") : []);
const named = (event: AuditEvent) => (text(event.detail.package) === "" ? "" : version(text(event.detail.package), typeof event.detail.label === "string" ? event.detail.label : undefined));
const env = (event: AuditEvent) => ENV[text(event.detail.env)] ?? text(event.detail.env);
const access = (value: unknown) => (value === null || value === undefined ? "no access" : ACCESS_WORD[text(value) as keyof typeof ACCESS_WORD] ?? text(value));
const day = (at: string) => new Date(at).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" }); // the date stored, wherever it is read
const short = (digest: string) => digest.replace(/^(sha256:.{8}).*(.{4})$/, "$1…$2");
// Who a share names: an address, the word everyone, or a group as the Access card writes it.
const grantee = (d: Record<string, unknown>) => (d.kind === "group" ? `group:${text(d.grantee)}` : text(d.grantee));
const beforeAfter = (value: unknown): { before: unknown; after: unknown } | null =>
  value !== null && typeof value === "object" && !Array.isArray(value) && "after" in value ? (value as { before: unknown; after: unknown }) : null;
// What a list gained and lost, said the way the redesign says it: "added bob@…, removed carol@…".
function moved(change: { before: unknown; after: unknown }): string {
  const before = list(change.before);
  const after = list(change.after);
  return [...after.filter((one) => !before.includes(one)).map((one) => `added ${one}`), ...before.filter((one) => !after.includes(one)).map((one) => `removed ${one}`)].join(", ");
}

// Why a request or a sign-in was refused, in a few words (the error codes and reasons the server records).
const REFUSED: Record<string, string> = {
  admin_required: "admins only", owner_required: "the app's owners only", team_required: "the app's team only", person_required: "people only, not agents",
  dashboard_required: "in the dashboard only, not from the CLI",
  not_a_member: "not on the allowed domains or emails", setup: "Canyon is still being set up",
};
// What a refused request tried to do, by its route (the denied row's detail.action).
const TRIED: Record<string, string> = {
  "PUT /api/v1/preferences": "change the org's settings",
  "PUT /api/v1/policies/{rule}": "change a guardrail",
  "PUT /api/v1/custom-rules/{slug}": "change a custom rule",
  "PUT /api/v1/front-door": "change the front door",
  "PUT /api/v1/platform-webhook": "change the platform webhook",
  "POST /api/v1/platform-webhook/token": "rotate the platform token",
  "POST /api/v1/platform-webhook/secret": "replace the webhook secret",
  "POST /api/v1/platform-webhook/ping": "send a test delivery",
  "PUT /api/v1/platform-repo": "change the deploy repo",
  "POST /api/v1/platform-repo/check": "check the deploy repo",
  "POST /api/v1/platform-repo/token": "replace the deploy repo's token",
  "POST /api/v1/platform-repo/fetch-token": "replace the fetch-only token",
  "DELETE /api/v1/sessions": "sign someone out everywhere",
  "POST /api/v1/design-systems": "register a design system",
  "DELETE /api/v1/projects/{id}": "delete the app",
  "PUT /api/v1/projects/{id}/owner": "claim the app",
  "PUT /api/v1/projects/{id}/access/{kind}/{grantee}": "share the app",
  "DELETE /api/v1/projects/{id}/access/{kind}/{grantee}": "remove someone from the app",
  "DELETE /api/v1/projects/{id}/access-requests/{email}": "dismiss a request for access",
  "PUT /api/v1/projects/{id}/policies/{rule}": "change the app's guardrail",
  "DELETE /api/v1/projects/{id}/policies/{rule}": "remove the app's guardrail",
  "POST /api/v1/projects/{id}/packages": "upload a version",
  "POST /api/v1/projects/{id}/gate-runs": "start a review",
  "POST /api/v1/projects/{id}/deploys": "deploy a version",
  "POST /api/v1/projects/{id}/promotions": "promote a version to production",
  "POST /api/v1/promotion-approvals/{code}/approve": "approve a promotion to production",
  "POST /api/v1/promotion-approvals/{code}/deny": "deny a promotion to production",
  "POST /api/v1/gate-runs/{id}/cancel": "cancel a run",
  "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve": "approve a blocking problem",
  "POST /api/v1/gate-runs/{run}/findings/approve": "approve one dependency's blocking problems",
  "DELETE /api/v1/projects/{id}/envs/{env}": "take the app offline",
  "PUT /api/v1/projects/{id}/secrets/{name}": "set a secret",
  "DELETE /api/v1/projects/{id}/secrets/{name}": "remove a secret",
  // A credential Canyon adds and the roots its certificates are signed under (#2145).
  "PUT /api/v1/projects/{id}/egress/{env}/routes/{destination}": "set a credential",
  "DELETE /api/v1/projects/{id}/egress/{env}/routes/{destination}": "remove a credential",
  "POST /api/v1/projects/{id}/egress/{env}/routes/{destination}/from-secret": "move a secret into a credential",
  "POST /api/v1/egress/ca/rotate": "replace the root of Canyon's credential certificates",
};

const UPLOAD = ["POST /api/v1/projects/{id}/packages", "package from github"];
const FAILED: Record<string, string> = { "build failed": "Build failed", "start failed": "Didn't stay up", "detect failed": "Can't build here", "build had no room": "No room to build" };

// What preview answered when a person promoted anyway (contracts/routes.md, the promotions row's
// detail.answered), "" when nothing was overridden.
function despite(d: Record<string, unknown>): string {
  const answered = d.answered as { status?: number; error?: string } | undefined;
  if (answered === undefined || answered === null) return "";
  return answered.error !== undefined ? ", despite preview giving no answer at /" : `, despite preview answering ${answered.status} at /`;
}

// The verb and its specifics. Every action the server writes has its own; one the bundle doesn't know
// yet is the trail's own sentence, or the action as written — odd, never blank.
export function what(event: AuditEvent): { verb: string; rest?: string } {
  const d = event.detail;
  const v = named(event);
  if (UPLOAD.includes(event.action)) return { verb: d.kind === "check" ? "Checked" : event.action === "package from github" ? "Pushed" : "Uploaded", rest: v };
  if (FAILED[event.action] !== undefined) return { verb: FAILED[event.action]!, rest: v };
  switch (event.action) {
    case "verdict": return { verb: "Reviewed", rest: v };
    case "deploy preview": return { verb: "Deployed to preview", rest: v };
    case "deploy prod": return { verb: "Promoted to production", rest: v };
    case "POST /api/v1/projects/{id}/gate-runs": case "gate run from github": return { verb: "Started a review of", rest: v };
    case "POST /api/v1/projects/{id}/deploys": return { verb: "Started a deploy of", rest: v };
    case "POST /api/v1/projects/{id}/promotions": return { verb: "Started a promotion of", rest: v + despite(d) };
    case "request promotion": return { verb: "Asked a person to approve", rest: v };
    case "approve promotion": return { verb: "Approved for production", rest: v + despite(d) };
    case "deny promotion": return { verb: "Denied for production", rest: v };
    case "POST /api/v1/gate-runs/{run}/findings/approve": {
      const until = text(d.expires_at);
      const n = Array.isArray(d.findings) ? d.findings.length : 0;
      return { verb: `Approved ${n} blocking problems`, rest: [`in ${dependency(d)}`, text(d.reason) === "" ? "" : `"${text(d.reason)}"`, until === "" ? "" : `until ${day(until)}`].filter(Boolean).join(", ") };
    }
    case "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve": {
      const until = text(d.expires_at);
      return { verb: "Approved a blocking problem", rest: [text(d.rule), text(d.reason) === "" ? "" : `"${text(d.reason)}"`, until === "" ? "" : `until ${day(until)}`].filter(Boolean).join(", ") };
    }
    case "share app": {
      const level = beforeAfter(d.level);
      return { verb: "Shared the app with", rest: `${grantee(d)}${level === null ? "" : `: ${access(level.before)} → ${access(level.after)}`}` };
    }
    case "unshare app": return { verb: "Removed from the app", rest: grantee(d) };
    case "PUT /api/v1/projects/{id}/secrets/{name}": return { verb: "Set a secret", rest: `${text(d.name)} for ${env(event)}` };
    case "DELETE /api/v1/projects/{id}/secrets/{name}": return { verb: "Removed a secret", rest: `${text(d.name)} from ${env(event)}` };
    // A policy edit can move its preview state, its production placement, its settings or its run, and can wait a grace:
    // the trail's own sentence says each of these, so it is said whole.
    case "policy.set": { const said = sentence(event); return { verb: said.charAt(0).toUpperCase() + said.slice(1) }; }
    case "app_policy.set": return { verb: "Changed the app's guardrail", rest: text(d.rule) };
    case "app_policy.remove": return { verb: "Removed the app's guardrail", rest: text(d.rule) };
    case "custom_rule.set": return { verb: "Saved a custom rule", rest: `${text(d.rule)}, version ${String(d.version ?? "")}` };
    case "set playbook": return { verb: "Saved a playbook", rest: `${text(d.slug)}, version ${String(d.version ?? "")}` };
    case "restore playbook": return { verb: "Restored a playbook", rest: `${text(d.slug)}, version ${String(d.from ?? "")} as version ${String(d.version ?? "")}` };
    case "remove playbook": return { verb: "Removed a playbook", rest: text(d.slug) };
    case "PUT /api/v1/preferences": {
      const admins = beforeAfter(d.admin_emails);
      if (admins !== null) return { verb: "Changed who is an admin", rest: moved(admins) };
      if (beforeAfter(d.admin_claim) !== null || beforeAfter(d.admin_value) !== null) return { verb: "Changed who is an admin", rest: "through the sign-in claim" };
      const signIn = [beforeAfter(d.allowed_domains), beforeAfter(d.allowed_emails)].filter((one) => one !== null);
      if (signIn.length > 0) return { verb: "Changed who can sign in", rest: signIn.map(moved).filter(Boolean).join(", ") };
      const runBy = beforeAfter(d.run_by);
      if (runBy !== null) return { verb: "Changed who runs the apps", rest: `${text(runBy.before) || "—"} → ${text(runBy.after)}` };
      const delivery = beforeAfter(d.delivery);
      if (delivery !== null) return { verb: "Changed how releases reach the platform", rest: `${text(delivery.before) || "—"} → ${text(delivery.after)}` };
      const grace = beforeAfter(d.access_grace);
      if (grace !== null) return { verb: "Changed how long access lasts after someone leaves", rest: `${String(grace.before ?? "—")} → ${String(grace.after ?? "—")}` };
      return { verb: "Changed the org's settings", rest: Object.keys(d).join(", ") };
    }
    case "sign in": return { verb: "Signed in", rest: d.flow === "device" ? "from the CLI" : d.flow === "browser" ? "in a browser" : undefined };
    // The two refusals the trail keeps (contracts/routes.md, the audit read): what was tried, and why not.
    case "denied": {
      const tried = text(d.action);
      const [method = "", path = ""] = tried.split(" ");
      // A route no phrase names yet reads as what it would have done to its path, never blank.
      const phrase = TRIED[tried] ?? `${method === "GET" ? "read" : "change"} ${path.replace(/^\/api\/v1/, "")}`;
      return { verb: "Tried to", rest: `${phrase} — ${REFUSED[text(d.error)] ?? text(d.error)}` };
    }
    case "sign in refused": return { verb: "Sign-in refused", rest: `${text(d.email)}${text(d.email) === "" ? "" : ": "}${REFUSED[text(d.reason)] ?? text(d.reason)}` };
    case "sign out": return { verb: "Signed out" };
    case "revoke sessions": return { verb: "Signed someone out everywhere", rest: text(d.subject) };
    case "POST /api/v1/projects": return { verb: "Created the app" };
    case "DELETE /api/v1/projects/{id}": return { verb: "Deleted the app" };
    case "PUT /api/v1/projects/{id}/owner": return { verb: "Claimed the app" };
    case "DELETE /api/v1/projects/{id}/envs/{env}": return { verb: "Took", rest: `${env(event)} offline, keeping its data` };
    case "declare resource": return { verb: "Declared a resource", rest: text(d.name) };
    case "ensure resource": return { verb: "Made a resource ready", rest: `${text(d.name)} in ${env(event)}` };
    case "reissue resource password": return { verb: "Gave a resource a new password", rest: `${text(d.name)} in ${env(event)}` };
    case "delete resource": return { verb: "Deleted a resource", rest: `${text(d.name)} in ${env(event)}, with its data` };
    case "attach report": return { verb: "Attached a report", rest: [text(d.tool), v].filter(Boolean).join(" for ") };
    case "GET /api/v1/projects/{id}/logs": return { verb: "Read the logs", rest: env(event) };
    case "platform reported deployed": return { verb: "Reported live", rest: `in ${env(event)}` };
    case "platform reported removed": return { verb: "Reported removed", rest: `from ${env(event)}` };
    case "platform reported failed": return { verb: "Reported a release failed", rest: `in ${env(event)}${text(d.reason) === "" ? "" : `: ${text(d.reason)}`}` };
    case "platform reported another digest": return { verb: "Reported a different image running", rest: `in ${env(event)}, not the reviewed one` };
    // The deploy repo's rows (#2470): Canyon's commits, and the check-ins that put a release live or refused it.
    case "commit release": return { verb: "Committed a release", rest: `to ${env(event)}${text(d.pull_request) === "" ? "" : ", as a pull request"}` };
    case "release live by check-in": return { verb: "Went live as a pod checked in", rest: `in ${env(event)}` };
    case "release removed by commit": return { verb: "Removed by a commit", rest: `from ${env(event)}` };
    case "release tree mismatch": return { verb: "Refused a release", rest: `in ${env(event)}: the deploy repo didn't hold what Canyon committed` };
    case "pull request closed": return { verb: "Refused a release", rest: `in ${env(event)}: its pull request was closed without merging` };
    case "pull request changed": return { verb: "Refused a release", rest: `in ${env(event)}: its pull request was changed after Canyon opened it` };
    case "set platform repo": return { verb: "Changed the deploy repo", rest: text(d.url) };
    case "replace platform repo token": return { verb: "Replaced the deploy repo's token" };
    case "rotate fetch token": return { verb: "Replaced the fetch-only token" };
    case "set front door": return { verb: "Changed the front door", rest: d.secret_replaced === true ? "and replaced its secret" : undefined };
    case "set platform webhook": return { verb: "Changed the platform webhook", rest: text(d.url) };
    case "egress.set": { // one row per PUT, naming what it changed (contracts/routes.md, the egress setting row)
      const after = (key: string) => (d[key] as { after?: unknown } | null | undefined)?.after;
      if (d.via !== undefined) return { verb: "Changed how apps call out", rest: text(after("via")) === "canyon" ? "through Canyon" : "directly, not through Canyon" };
      if (d.mode !== undefined) return { verb: "Changed what apps may call", rest: `to ${text(after("mode"))}` };
      return { verb: after("credential_routes") === true ? "Turned on adding credentials" : "Turned off adding credentials" };
    }
    case "egress.networks.recreate": {
      const left = Array.isArray(d.stopped) ? d.stopped.map(text).filter(Boolean) : [];
      return { verb: "Recreated the app's network", rest: `${d.internal === true ? "through Canyon" : "directly, not through Canyon"}${left.length > 0 ? `; ${left.join(" and ")} left stopped` : ""}` };
    }
    case "rotate platform token": return { verb: "Rotated the platform token" };
    case "rotate platform secret": return { verb: "Replaced the webhook secret" };
    case "scanner.credential.set": return { verb: "Set a scanner credential", rest: [text(d.tool), text(d.repo)].filter(Boolean).join(" · ") };
    case "scanner.credential.remove": return { verb: "Removed a scanner credential", rest: [text(d.tool), text(d.repo)].filter(Boolean).join(" · ") };
  }
  const said = sentence(event);
  return { verb: said.charAt(0).toUpperCase() + said.slice(1) };
}

// The row's outcome in one word, coloured by what it asks of the reader: red for what went wrong, amber
// for an exception or a privilege change, green for a pass or a deploy. Most rows have none.
export function result(event: AuditEvent): { word: string; tone: Tone } | null {
  if (FAILED[event.action] !== undefined || event.action === "platform reported failed") return { word: "Failed", tone: "critical" };
  switch (event.action) {
    case "platform reported another digest": return { word: "Image mismatch", tone: "critical" };
    case "release tree mismatch": case "pull request closed": case "pull request changed": return { word: "Refused", tone: "critical" };
    case "denied": return { word: "Denied", tone: "neutral" };
    case "sign in refused": return { word: "Refused", tone: "neutral" };
    case "verdict": {
      const warnings = Number(event.detail.warnings ?? 0);
      return event.detail.state === "blocked" ? { word: "Blocked", tone: "critical" } : event.detail.state === "clean" ? { word: `Passed${warnings > 0 ? ` · ${warnings} ${warnings === 1 ? "warning" : "warnings"}` : ""}`, tone: "ok" } : null;
    }
    case "deploy preview": case "deploy prod": case "platform reported deployed": case "release live by check-in": return { word: "Live", tone: "ok" };
    case "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve": case "POST /api/v1/gate-runs/{run}/findings/approve": return { word: "Risk accepted", tone: "amber" };
    case "PUT /api/v1/preferences": case "complete setup":
      return beforeAfter(event.detail.admin_emails) !== null || beforeAfter(event.detail.admin_claim) !== null || beforeAfter(event.detail.admin_value) !== null ? { word: "Admin change", tone: "amber" } : null;
  }
  return null;
}

const NOT = <span className="text-text-faint">not recorded</span>;
// How it was done, in the words the redesign uses under a name.
export const VIA_WORD: Record<Via, string> = { dashboard: "dashboard", cli: "CLI", mcp: "MCP", github: "GitHub", platform: "platform", canyon: "Canyon" };
const VIA_RECORD: Record<Via, string> = { dashboard: "The dashboard", cli: "The CLI", mcp: "An MCP client", github: "GitHub", platform: "The org's platform", canyon: "Canyon" };
// The Via filter's words for each way of doing.
export const VIA_FILTER: Record<Via, string> = { dashboard: "From the dashboard", cli: "From the CLI", mcp: "From an MCP client", github: "From GitHub", platform: "From the platform", canyon: "Canyon itself" };
const DT = "text-text-dim";
const DD = "m-0 min-w-0 break-words";

// What a row changed: each detail value stored as a before and an after, side by side; an image the
// platform reported against the one Canyon reviewed. Rows that changed nothing of the kind show none.
function Changed({ event }: { event: AuditEvent }) {
  if (event.action === "platform reported another digest") {
    return (
      <section aria-label="What was reported" className="grid content-start gap-2">
        <h2 className="text-sm font-semibold tracking-wide text-text-faint uppercase">What was reported</h2>
        <dl className="grid grid-cols-[180px_minmax(0,1fr)] gap-x-3 gap-y-1 text-md max-[620px]:grid-cols-1">
          <dt className={DT}>Reviewed and released</dt><dd className={`${DD} font-mono`}>{short(text(event.detail.expected))}</dd>
          <dt className={DT}>Running in {env(event)}</dt><dd className={`${DD} font-mono text-critical`}>{short(text(event.detail.reported))}</dd>
        </dl>
        <p className="text-md text-text-dim">Canyon didn't build this image. Find out who deployed it outside Canyon, then promote a reviewed version.</p>
      </section>
    );
  }
  // A policy's own preview state is stored flat, as detail.before and detail.after; every other change nests its own.
  const flat = event.action === "policy.set" && "after" in event.detail ? [["state", { before: event.detail.before, after: event.detail.after }] as const] : [];
  const one = (value: unknown) => (typeof value === "string" ? value : JSON.stringify(value));
  const said = (value: unknown) => (Array.isArray(value) ? (value.length === 0 ? "—" : value.map(one).join(", ")) : value === null || value === undefined || value === "" ? "—" : one(value));
  const changes = [...flat, ...Object.entries(event.detail).map(([key, value]) => [key, beforeAfter(value)] as const)]
    .filter((row): row is readonly [string, { before: unknown; after: unknown }] => row[1] !== null && said(row[1].before) !== said(row[1].after)); // what stayed is no change
  if (changes.length === 0) return null;
  return (
    <section aria-label="What changed" className="grid content-start gap-2">
      <h2 className="text-sm font-semibold tracking-wide text-text-faint uppercase">What changed</h2>
      <table className="w-full table-fixed border-collapse text-md">
        <thead><tr className="text-left text-text-dim"><th className="w-1/4 py-1 font-medium">Setting</th><th className="py-1 font-medium">Before</th><th className="py-1 font-medium">After</th></tr></thead>
        <tbody>
          {changes.map(([key, change]) => (
            <tr key={key} className="border-t border-border align-top">
              <td className="py-1 pr-2 font-mono break-words">{key}</td>
              <td className="py-1 pr-2 break-words text-text-dim">{said(change.before)}</td>
              <td className="py-1 break-words">{said(change.after)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}

// The record behind a row, as the trail stores it, never guessed: a row written before the provenance
// record carries none of it. `admin` says who reads (null until known): a session and an address are
// answered to admins alone, so to a member a row a session wrote says whose they are.
function Record({ event, admin }: { event: AuditEvent; admin: boolean | null }) {
  // Canyon's own step has no person behind it: no role, session or address to record.
  const own = event.via === "canyon" ? <span className="text-text-faint">none, Canyon's own step</span> : null;
  const bySession = event.via === "dashboard" || event.via === "cli";
  const hidden = own ?? (admin === false && bySession ? <span className="text-text-faint">shown to admins</span> : NOT);
  const mono = (value: string | undefined) => (value === undefined ? hidden : <span className="font-mono">{value}</span>);
  const [copied, setCopied] = useState<string | null>(null);
  const copy = () => Promise.resolve().then(() => navigator.clipboard.writeText(JSON.stringify(event, null, 2)))
    .then(() => setCopied("Copied."), () => setCopied("The record couldn't be copied."));
  return (
    <section aria-label="Record" className="grid content-start gap-2">
      <h2 className="text-sm font-semibold tracking-wide text-text-faint uppercase">Record</h2>
      <dl className="grid grid-cols-[120px_minmax(0,1fr)] gap-x-3 gap-y-1 text-md">
        <dt className={DT}>When</dt><dd className={`${DD} tnum`}>{event.ts.replace("T", " ").replace("Z", " UTC")}</dd>
        <dt className={DT}>Who</dt><dd className={DD}>{event.actor.subject} · {event.actor.kind}</dd>
        <dt className={DT}>Role</dt><dd className={DD}>{event.role ?? own ?? NOT}</dd>
        <dt className={DT}>Via</dt><dd className={DD}>{event.via === undefined ? NOT : VIA_RECORD[event.via]}</dd>
        <dt className={DT}>Session</dt><dd className={DD}>{mono(event.session)}</dd>
        <dt className={DT}>From</dt><dd className={DD}>{mono(event.source)}</dd>
        <dt className={DT}>Action</dt><dd className={`${DD} font-mono`}>{event.action}</dd>
        <dt className={DT}>Event</dt><dd className={`${DD} font-mono`}>{event.id}</dd>
      </dl>
      <p className="flex flex-wrap items-center gap-3 text-md">
        {event.project !== undefined && event.run !== undefined && <a className="text-link no-underline hover:underline" href={`#/apps/${event.project}?run=${encodeURIComponent(event.run)}`}>Open the run</a>}
        <button type="button" className="text-link" onClick={() => void copy()}>Copy JSON</button>
        {copied !== null && <span role="status" className="text-text-dim">{copied}</span>}
      </p>
    </section>
  );
}

const HEAD = "border-b border-border px-3 py-2 text-left text-sm font-semibold text-text-faint";
// Under 700px a row reads as a card, its cells one under another, rather than a table scrolled sideways.
const CELL = "border-b border-border px-3 py-2 align-top max-[700px]:block max-[700px]:border-0 max-[700px]:py-0.5";
const clock = (at: string) => new Date(at).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
function dayHeading(at: string, now: number): string {
  const start = (time: number) => new Date(time).setHours(0, 0, 0, 0);
  const days = Math.round((start(now) - start(Date.parse(at))) / 86_400_000);
  const date = new Date(at).toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long" }).toUpperCase();
  return days === 0 ? `TODAY · ${date}` : days === 1 ? `YESTERDAY · ${date}` : date;
}

// One page of the trail, newest first, a body per day. A row opens in place on what it changed and its
// record; the table scrolls sideways inside its frame on a narrow screen, never the page, and an opened
// row's detail stays in view at the frame's left edge.
export function AuditTable({ events, now, admin, names }: { events: AuditEvent[]; now: number; admin: boolean | null; names?: Map<string, string> }) {
  const [open, setOpen] = useState<string | null>(null);
  const days: { heading: string; events: AuditEvent[] }[] = [];
  for (const event of events) {
    const heading = dayHeading(event.ts, now);
    if (days.at(-1)?.heading !== heading) days.push({ heading, events: [] });
    days.at(-1)!.events.push(event);
  }
  return (
    <div className="mt-2.5 overflow-x-auto rounded-xl border border-border bg-surface">
      <table className="w-full min-w-[760px] border-collapse text-base max-[700px]:block max-[700px]:min-w-0">
        <thead className="max-[700px]:hidden"><tr><th className={`${HEAD} w-[96px]`}>Time</th><th className={`${HEAD} w-[200px]`}>Who</th><th className={HEAD}>What</th><th className={`${HEAD} w-[150px]`}>App</th><th className={`${HEAD} w-[140px]`}>Result</th></tr></thead>
        {days.map((group) => (
          <tbody key={group.heading} className="max-[700px]:block">
            <tr className="max-[700px]:block"><th colSpan={5} scope="rowgroup" className="border-b border-border bg-surface-2 px-3 py-1.5 max-[700px]:block text-left text-sm font-semibold tracking-wide text-text-faint">{group.heading}</th></tr>
            {group.events.map((event) => {
              const said = what(event);
              const outcome = result(event);
              const opened = open === event.id;
              const name = actorName(event.actor);
              return (
                <Fragment key={event.id}>
                  <tr className={`max-[700px]:block max-[700px]:border-b max-[700px]:border-border max-[700px]:py-2 ${opened ? "bg-surface-2" : "hover:bg-surface-2"}`}>
                    <td className={`${CELL} tnum text-text-dim`}><time dateTime={event.ts} title={event.ts.replace("T", " ").replace("Z", " UTC")}>{clock(event.ts)}</time></td>
                    <td className={CELL}><span className="flex items-center gap-2">{event.actor.kind === "canyon" || anonymous(event.actor) ? <span className="size-6 flex-none" /> : <Avatar name={name} size="sm" />}<span className="grid leading-snug"><Actor actor={event.actor} />{event.via !== undefined && event.via !== "canyon" && <span className="text-sm text-text-faint"><span className="sr-only">, via </span>{VIA_WORD[event.via]}</span>}</span></span></td>
                    <td className={CELL}>
                      <button type="button" aria-expanded={opened} aria-controls={opened ? `detail-${event.id}` : undefined} onClick={() => setOpen(opened ? null : event.id)} className="flex w-full items-start gap-1.5 text-left">
                        <span className={`mt-0.5 text-text-faint transition-transform ${opened ? "rotate-90" : ""}`}><Icon name="chevron" size={14} /></span>
                        <span className="min-w-0"><span className="font-medium text-text">{said.verb}</span>{said.rest !== undefined && said.rest !== "" && <span className="text-text-dim"> {said.rest}</span>}</span>
                      </button>
                    </td>
                    <td className={CELL}>{!event.project ? <span className="text-text-dim">{event.action === "policy.set" ? "Every app" : "Org"}</span> : <a className="text-link no-underline hover:underline" href={`#/apps/${event.project}`}>{names?.get(event.project) ?? event.project}</a>}</td>
                    <td className={CELL}>{outcome !== null && <Pill tone={outcome.tone}>{outcome.word}</Pill>}</td>
                  </tr>
                  {opened && (
                    <tr id={`detail-${event.id}`} className="max-[700px]:block">
                      <td colSpan={5} className="border-b border-border bg-surface-2 py-3 max-[700px]:block">
                        <div className="sticky left-0 grid max-w-[calc(100vw-2rem)] grid-cols-2 gap-6 px-3 max-[900px]:grid-cols-1">
                          <Changed event={event} />
                          <Record event={event} admin={admin} />
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        ))}
      </table>
    </div>
  );
}
