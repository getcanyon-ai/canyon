import { version, type Actor as ActorRef, type AuditEvent } from "../api";
import { ACCESS_WORD, AUDIT_ACTIONS, AUDIT_CA_BYO, AUDIT_CHECK, AUDIT_GRACE, AUDIT_HELD_RUNNING, AUDIT_LIFT_STARTED, AUDIT_PRODUCTION, AUDIT_RECREATE_FAILED, AUDIT_ROUTE_MOVED, PATCHING, S } from "../strings";
import { Actor, actorName } from "./Actor";
import { Age } from "./Age";
import { Icon } from "./Icon";
import { hex12 } from "./Patched";

const ENV: Record<string, string> = { preview: "preview", prod: "production" };
const text = (value: unknown) => (typeof value === "string" ? value : "");
// Who an access row names: an address, the word everyone, or a group as the Access card writes it.
// A rule as the Guardrails page names it; a row written before the name was recorded reads its id.
const ruleName = (detail: Record<string, unknown>) => (typeof detail.name === "string" && detail.name !== "" ? detail.name : text(detail.rule));
// One dependency's findings approved together name it as its tool wrote it (contracts/routes.md).
export const dependency = (detail: Record<string, unknown>) => {
  const { name, version } = (detail.dependency ?? {}) as { name?: unknown; version?: unknown };
  return `${text(name)} ${text(version)}`.trim();
};
// An app env's mode as egress.mode.set's row has it: null follows the org's (#2563).
const mode = (detail: Record<string, unknown>) => { const after = (detail.app_mode as { after?: unknown } | null | undefined)?.after; return after === null ? "the org's" : text(after); };
const grantee = (detail: Record<string, unknown>) => (detail.kind === "group" ? `group:${text(detail.grantee)}` : text(detail.grantee));

// The row as a sentence: the action's plain words with the detail's values in their places. An
// action the map does not know is shown as the server wrote it — a route the bundle was built
// before reads odd, never blank. A forced upload says so: the override is on the trail (routes.md); a check says it checked.
export function sentence({ action, detail }: AuditEvent): string {
  if (action === "policy.set") return policySet(detail);
  return worded(action, detail);
}

// A policy change, and when it is in force when it waits a grace.
function policySet(detail: Record<string, unknown>): string {
  const now = detail.production !== null && typeof detail.production === "object" ? placement(detail) : worded("policy.set", detail);
  const days = typeof detail.grace_days === "number" ? detail.grace_days : 0;
  return days <= 0 ? now : now + (days === 1 ? AUDIT_GRACE.one : AUDIT_GRACE.many.replace("<days>", String(days)));
}

function worded(action: string, detail: Record<string, unknown>): string {
  const words = detail.kind === "check" ? AUDIT_CHECK : action === "lift hold" && detail.started === true && typeof detail.package === "string" ? AUDIT_LIFT_STARTED
    : action === "stop app" && detail.stopped === false ? AUDIT_HELD_RUNNING
    : action === "egress.networks.recreate" && detail.failed !== null && detail.failed !== undefined ? AUDIT_RECREATE_FAILED
    : action === "egress.route.set" && typeof detail.secret === "string" ? AUDIT_ROUTE_MOVED
    : action === "egress.ca.rotate" && detail.byo === true ? AUDIT_CA_BYO : AUDIT_ACTIONS[action];
  if (words === undefined) return action;
  const said = words
    .replace("<version>", () => version(text(detail.package), typeof detail.label === "string" ? detail.label : undefined))
    .replace("<name>", () => text(detail.name))
    .replace("<env>", () => ENV[text(detail.env)] ?? text(detail.env))
    .replace("<rule>", () => ruleName(detail))
    .replace("<number>", () => String(detail.version ?? ""))
    .replace("<state>", () => text(detail.after))
    .replace("<subject>", () => text(detail.subject))
    .replace("<verdict>", () => text(detail.state))
    .replace("<blocking>", () => String(detail.blocking ?? ""))
    .replace("<warnings>", () => String(detail.warnings ?? ""))
    .replace("<grantee>", () => grantee(detail))
    .replace("<access>", () => ACCESS_WORD[text((detail.level as { after?: unknown } | null | undefined)?.after) as keyof typeof ACCESS_WORD] ?? "")
    .replace("<email>", () => text(detail.email))
    .replace("<destination>", () => text(detail.destination))
    .replace("<secret>", () => text(detail.secret))
    .replace("<mode>", () => mode(detail))
    .replace("<count>", () => String(Array.isArray(detail.findings) ? detail.findings.length : ""))
    .replace("<dependency>", () => dependency(detail))
    .replace("<run>", () => (detail.kind === "promote" ? "the promotion" : "the run"))
    .replace("<started_by>", () => { const by = detail.started_by as ActorRef | null | undefined; return by === null || by === undefined ? "someone" : actorName(by); })
    .replace("<image>", () => PATCHING.liveImage.replace("<hex>", () => hex12(text(detail.identity))))
    .replace("<slug>", () => text(detail.slug))
    .replace("<from>", () => String(detail.from ?? ""))
    .replace("<reason>", () => text(detail.reason)); // last: a person's words, never read for a word to fill
  return detail.forced === true ? `${said}${S.auditForced}` : said;
}

// folded is a row the app's Activity and the fleet's recent activity leave out: a report Canyon attached
// itself, which its review row already speaks for. Canyon no longer writes these; the ones it wrote
// before stay on the trail. The audit page lists them all.
export const folded = (event: AuditEvent) => event.action === "attach report" && event.actor.kind === "canyon";

// A before-production change: what it did there, after the preview state when that changed too.
function placement(detail: Record<string, unknown>): string {
  const { before, after } = detail.production as { before?: unknown; after?: unknown };
  const changed = detail.before !== detail.after;
  const it = changed ? "it" : `the policy ${ruleName(detail)}`;
  const words = after === "off" ? AUDIT_PRODUCTION.removed : before === "off" ? AUDIT_PRODUCTION.placed : AUDIT_PRODUCTION.set;
  const said = words.replace("<it>", it).replace("<state>", () => text(after));
  const set = (AUDIT_ACTIONS["policy.set"] ?? "").replace("<rule>", () => ruleName(detail)).replace("<state>", () => text(detail.after));
  return changed ? `${set}, and ${said}` : said;
}

// When, who and what; `app` adds which, for the fleet's strip, where the rows are the whole org's.
// Every Canyon row links to its evidence: the run it ruled on, on its app's page (screens.md, A4a).
// `stacked` is the fleet's strip as the approved mockup draws it: what happened on one line, under a
// review's mark, and when muted under it.
export function AuditRow({ event, app = false, stacked = false }: { event: AuditEvent; app?: boolean; stacked?: boolean }) {
  const view = event.actor.kind === "canyon" && event.project !== undefined && event.run !== undefined && (
    <a className="ml-auto text-link no-underline hover:underline" href={`#/apps/${event.project}?run=${encodeURIComponent(event.run)}`}>view →</a>
  );
  if (stacked) {
    // A review's row carries its result's mark, and a build or a start that failed after it the blocked one.
    const reviewed = event.action === "verdict" ? event.detail.state : ["build failed", "detect failed", "start failed", "build had no room"].includes(event.action) ? "failed" : undefined;
    return (
      <li className="grid grid-cols-[16px_minmax(0,1fr)_auto] gap-x-2.5 py-1.5 text-base">
        <span className={`mt-0.5 ${reviewed === "clean" ? "text-ok" : reviewed === undefined ? "" : "text-critical"}`}>
          {reviewed !== undefined && <Icon name={reviewed === "clean" ? "passed" : "blocked"} size={14} />}
        </span>
        <span className="min-w-0">
          {event.project !== undefined && <><a className="font-semibold text-text no-underline hover:underline" href={`#/apps/${event.project}`}>{event.project}</a>{" · "}</>}
          <Actor actor={event.actor} /> {sentence(event)}
        </span>
        {view || <span />}
        <span className="col-start-2 text-md text-text-faint"><Age at={event.ts} long /></span>
        {event.action === "custom_rule.set" && <span className="col-start-2"><RuleText detail={event.detail} /></span>}
      </li>
    );
  }
  return (
    <li className="flex flex-wrap items-baseline gap-2 py-1 text-base">
      <Age at={event.ts} />
      {app && event.project !== undefined && <a className="text-link no-underline hover:underline" href={`#/apps/${event.project}`}>{event.project}</a>}
      <span><Actor actor={event.actor} /> {sentence(event)}</span>
      {event.action === "custom_rule.set" && <RuleText detail={event.detail} />}
      {view}
    </li>
  );
}

// A custom rule's saved version shows what its text was and became (the History a rule's card links
// to): folded, since the text runs to 2,000 characters. Version 1 has no before.
function RuleText({ detail }: { detail: Record<string, unknown> }) {
  const said = detail.instructions as { before?: string; after?: string } | undefined;
  if (said?.after === undefined) return null;
  return (
    <details className="basis-full pl-[42px] text-md">
      <summary className="cursor-pointer text-link">The rule's text</summary>
      {said.before !== undefined && <><p className="mt-1.5 text-sm font-semibold text-text-dim">Before</p><p className="whitespace-pre-wrap text-text-dim">{said.before}</p></>}
      <p className="mt-1.5 text-sm font-semibold text-text-dim">After</p>
      <p className="whitespace-pre-wrap">{said.after}</p>
    </details>
  );
}
