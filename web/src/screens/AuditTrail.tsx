import { useEffect, useRef, useState } from "react";
import { getFleet, listAudit, reduced, said as sentenceOf, type AuditEvent, type AuditPage, type Via } from "../api";
import { labels } from "../ui/AppName";
import { AuditTable, VIA_FILTER, what } from "./AuditTable";
import { PAGE_SIZE, Pager } from "../ui/Pager";
import { Rail } from "../ui/Rail";

const ACTORS = [
  [null, "All"],
  ["person", "People"],
  ["agent", "Agents"],
  ["canyon", "Canyon"],
] as const;

// The range is the screen's own key: a link keeps "the last 24 hours", never the instant it was
// copied, so since= is worked out when the page is read.
const RANGES = [
  { key: "24h", label: "Last 24 hours", words: "24 hours", ms: 86_400_000 },
  { key: "7d", label: "Last 7 days", words: "7 days", ms: 7 * 86_400_000 },
  { key: "30d", label: "Last 30 days", words: "30 days", ms: 30 * 86_400_000 },
];

// The questions people come to the trail with (the approved redesign's "Common questions"): each a saved
// set of actions the server narrows to (contracts/routes.md, the audit read's action=). The org's own
// changes are an admin's to read, so `admin` questions are offered to an admin alone (screens.md, A4a).
const QUESTIONS: { key: string; label: string; actions: string[]; admin?: true }[] = [
  { key: "production", label: "Production changes", actions: ["deploy prod", "POST /api/v1/projects/{id}/promotions", "request promotion", "approve promotion", "deny promotion",
    "stop app", "lift hold", "DELETE /api/v1/projects/{id}/envs/{env}", "request undeploy", "send remove again",
    "patch prod", "revert patch prod", "request patch approval", "approve patch identity",
    "platform reported deployed", "platform reported removed", "platform reported failed", "platform reported another digest", "platform reported deployed without resources",
    "commit release", "release live by check-in", "release removed by commit", "release tree mismatch", "pull request closed", "pull request changed"] },
  { key: "admin", label: "Admin and sign-in changes", actions: ["PUT /api/v1/preferences", "sign in", "sign out", "revoke sessions", "complete setup", "set front door", "egress.set", "egress.networks.recreate", "set platform webhook", "rotate platform token", "scanner.credential.set", "scanner.credential.remove", "github app registered"], admin: true },
  { key: "guardrails", label: "Guardrail changes", actions: ["policy.set", "app_policy.set", "app_policy.remove", "custom_rule.set", "set playbook", "restore playbook", "remove playbook"], admin: true },
  { key: "access", label: "Access changes", actions: ["share app", "unshare app", "request access", "dismiss access request", "PUT /api/v1/projects/{id}/owner"] },
  { key: "approvals", label: "Approved blocking problems", actions: ["POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", "POST /api/v1/gate-runs/{run}/findings/approve"] },
  { key: "secrets", label: "Secrets", actions: ["PUT /api/v1/projects/{id}/secrets/{name}", "DELETE /api/v1/projects/{id}/secrets/{name}"] },
  { key: "deletions", label: "Deletions", actions: ["DELETE /api/v1/projects/{id}", "delete resource"] },
  { key: "refused", label: "Denied and failed", actions: ["denied", "sign in refused", "build failed", "start failed", "detect failed", "build had no room", "platform reported failed"] },
];
// The trail's three kinds of row (contracts/routes.md, Audit categories).
const CATEGORIES = [["", "Every kind"], ["governance", "Changes"], ["access", "Access"], ["outcome", "Outcomes"]] as const;

const FIELD = "rounded-lg border border-border bg-surface-2 px-2.5 py-1 text-base";
// The names a reader's zone may carry for UTC itself (the tz database's links to Etc/UTC and Etc/GMT): a reader in one
// already reads the times in UTC, so the line offers no hover for it (#141).
const UTC = new Set(["UTC", "Etc/UTC", "Etc/UCT", "Etc/Universal", "Etc/Zulu", "UCT", "Universal", "Zulu", "Etc/GMT", "Etc/GMT+0", "Etc/GMT-0", "Etc/GMT0", "Etc/Greenwich", "GMT", "GMT+0", "GMT-0", "GMT0", "Greenwich"]);
export const zoneLine = (zone: string) => (UTC.has(zone) ? "Times are in UTC." : `Times are in ${zone}; hover a time for UTC.`);
const ITEM = "rounded-[7px] px-2.5 py-2 text-left text-base hover:bg-surface-2 disabled:text-text-faint";

// The screen's address with one key changed; a default (the last day, 50 rows) is the key left out.
// Any change but the page's own goes back to the first page; a page chosen in the footer is named, the
// first one too, so the screen it opens knows to hand the keyboard to its heading.
const DEFAULTS: Record<string, string> = { range: "24h", limit: String(PAGE_SIZE) };
function auditHref(query: string, set: Record<string, string | null>): string {
  const params = new URLSearchParams(query);
  if (!("page" in set)) { params.delete("page"); params.delete("until"); } // a later page's walk ends with the view it walked
  for (const [key, value] of Object.entries(set)) {
    if (value === null || DEFAULTS[key] === value) params.delete(key);
    else params.set(key, value);
  }
  const next = params.toString();
  return next === "" ? "#/audit" : `#/audit?${next}`;
}

// A cell as a spreadsheet reads it safely: quoted when it holds a separator, and never taken for a
// formula — a sentence an agent wrote may begin with "=".
const cell = (value: string) => {
  const safe = /^[\s=+\-@]/.test(value) ? `'${value}` : value;
  return /[",\n\r]/.test(safe) ? `"${safe.replaceAll('"', '""')}"` : safe;
};
// The row as the screen says it, the verb and its specifics.
const words = (event: AuditEvent) => { const said = what(event); return said.rest === undefined || said.rest === "" ? said.verb : `${said.verb} ${said.rest}`; };
// Every field an auditor needs reaches the spreadsheet: the detail too, with its reasons and its before and after values.
const csv = (events: AuditEvent[]) =>
  [["When", "Kind", "Who", "What", "App", "Run", "Action", "Via", "Role", "Session", "From", "Detail"],
    ...events.map((one) => [one.ts, one.actor.kind, one.actor.display || one.actor.subject, words(one), one.project ?? "", one.run ?? "", one.action, one.via ?? "", one.role ?? "", one.session ?? "", one.source ?? "", JSON.stringify(one.detail)])]
    .map((row) => row.map(cell).join(",")).join("\r\n");

// The file goes to the browser's own download; nothing is sent anywhere.
function download(name: string, type: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url)); // revoked at once, some browsers cancel the download
}

// The organisation's trail, newest first. Everything after "#/audit?" is the view, so a link
// reproduces it; the Shell keys this screen on the hash, so a new filter is a fresh read and an
// answer to the old one lands nowhere. The filters go to the server as they stand: filtering here
// would narrow one page and page through the unfiltered trail.
export function AuditTrail({ query }: { query: string }) {
  const params = new URLSearchParams(query);
  const actor = params.get("actor");
  const app = params.get("project");
  // One rule's history (a custom rule's card links here): every change to it, whatever the range.
  const rule = params.get("rule") || null;
  const range = RANGES.find((one) => one.key === params.get("range")) ?? RANGES[0]!;
  const question = QUESTIONS.find((one) => one.key === params.get("question")) ?? null;
  const category = params.get("category");
  const via = params.get("via");
  const [since] = useState(() => new Date(Date.now() - range.ms).toISOString());
  // When the first page was read: the later pages of one walk are read up to it (until=), so rows that
  // arrive meanwhile wait in the bar above page 1 instead of pushing rows across the pages.
  const [opened] = useState(() => new Date().toISOString());
  const until = params.get("until");
  // The rows a page holds as the server served them: what the address asks for, past 200 clamped to 200.
  const limit = Math.min(Number(params.get("limit")) || PAGE_SIZE, 200);
  const [page, setPage] = useState<AuditPage | null>(null);
  const heading = useRef<HTMLHeadingElement>(null);
  const [failure, setFailure] = useState<string | null>(null);
  // The apps a reader can narrow to: the ones they can read the trail of. ponytail: the fleet's
  // first 200; a bigger org reaches the rest from an app's Activity link.
  const [apps, setApps] = useState<{ id: string; name: string }[]>([]);
  const [admin, setAdmin] = useState<boolean | null>(null); // who reads, from the fleet read's viewer, unknown until it answers: a session and an address are an admin's to see
  const [said, setSaid] = useState<{ line: string; failed?: true } | null>(null);
  const [exporting, setExporting] = useState(false);

  const view = () => {
    const ask = new URLSearchParams(rule === null ? { since } : { rule });
    if (actor !== null) ask.set("actor", actor);
    if (app !== null && rule === null) ask.set("project", app); // a rule's rows are the org's, never an app's
    if (question !== null) ask.set("action", question.actions.join(","));
    if (category !== null && category !== "") ask.set("category", category);
    if (via !== null && via !== "") ask.set("via", via);
    if (until !== null) ask.set("until", until);
    return ask;
  };
  // The first page is read again every 2 s, and what came in since waits in a bar above the table
  // until it is asked for, so the rows never move under the reader. A later page holds still while it is
  // read; since is worked out again as each page is opened, so a page edge can move by what aged out between them.
  const [waiting, setWaiting] = useState<AuditPage | null>(null);
  const sent = useRef(0); // an answer older than the newest asked for never lands over it
  const table = useRef<HTMLDivElement>(null);
  const read = () => {
    const ask = view();
    ask.set("limit", String(limit));
    const asked = params.get("page");
    if (asked !== null) ask.set("page", asked);
    const mine = ++sent.current;
    return listAudit(ask.toString()).then(
      (answer) => {
        if (mine !== sent.current) return;
        setPage((shown) => (shown === null ? answer : shown));
        setWaiting(answer);
        setFailure(null);
      },
      (err: unknown) => { if (mine === sent.current) setFailure(sentenceOf(err)); },
    );
  };
  // How many came in: the totals of one view at one since, so a burst past a page is counted whole.
  const arrived = waiting === null || page === null ? 0 : Math.max(0, waiting.total - page.total);
  const show = () => {
    setPage(waiting);
    setTimeout(() => table.current?.querySelector<HTMLButtonElement>("button[aria-expanded]")?.focus()); // the keyboard to the newest row
  };
  useEffect(() => {
    void read();
    void getFleet("limit=200").then(
      (page) => {
        setApps(page.apps.filter((row) => !reduced(row)).map(({ id, name }) => ({ id, name })).sort((a, b) => a.name.localeCompare(b.name)));
        setAdmin(page.viewer.role === "admin");
      },
      () => setApps([]), // no choices: the trail still reads, and a link can still name an app
    );
    // A page chosen in the footer, the first one included, gives the keyboard to the heading; only the first page is read again.
    if (params.has("page")) heading.current?.focus();
    if ((params.get("page") ?? "1") !== "1") return;
    const timer = setInterval(() => void read(), 2000);
    return () => clearInterval(timer);
  }, []);

  // Export holds every row the view selects, read page by page from the same route.
  // A file is handed over only once every page has answered: a failure downloads nothing.
  const exported = async (format: "csv" | "json") => {
    setExporting(true);
    setSaid({ line: "Reading the trail…" });
    try {
      const rows: AuditEvent[] = [];
      let cursor: string | null = null;
      do {
        const ask = view();
        ask.set("limit", "200");
        if (cursor !== null) ask.set("cursor", cursor);
        const answer = await listAudit(ask.toString());
        rows.push(...answer.events);
        cursor = answer.next;
      } while (cursor !== null);
      const name = rule !== null ? `canyon-audit-${rule}` : `canyon-audit-${app ?? "all"}-${range.key}`;
      if (format === "csv") download(`${name}.csv`, "text/csv", csv(rows));
      else download(`${name}.json`, "application/json", JSON.stringify(rows, null, 2));
      setSaid({ line: `Exported ${rows.length} ${rows.length === 1 ? "row" : "rows"}.` });
    } catch (err) {
      setSaid({ line: sentenceOf(err), failed: true });
    } finally {
      setExporting(false);
    }
  };
  // No clipboard outside a secure page (a host reached over plain http): said, never thrown.
  const copy = () =>
    Promise.resolve().then(() => navigator.clipboard.writeText(window.location.href))
      .then(() => setSaid({ line: "Link copied." }), () => setSaid({ line: "The link couldn't be copied.", failed: true }));
  // A choice closes the menu, as Escape does.
  const close = (event: { currentTarget: Element }) => { const menu = event.currentTarget.closest("details"); if (menu !== null) menu.open = false; };
  // Each filter writes its own key into the address, so the others stay put.
  const go = (set: Record<string, string | null>) => { window.location.hash = auditHref(query, set); };

  return (
    <div className="flex min-h-screen items-start max-[860px]:block">
      <Rail current="audit" />
      <main className="mx-auto max-w-[1280px] min-w-0 flex-1 px-8 pt-3.5 pb-24 max-[860px]:px-4">
        <h1 ref={heading} tabIndex={-1} className="text-title font-semibold tracking-tight outline-none">Audit</h1>
        <p className="mt-0.5 text-md text-text-dim">{admin === false ? "Your apps' changes and your own sign-ins, newest first." : "Every change, newest first."} {zoneLine(Intl.DateTimeFormat().resolvedOptions().timeZone)}</p>
        <div className="mt-2.5 flex flex-wrap items-center gap-2.5">
          <nav aria-label="Who" className="flex flex-wrap gap-1.5">
            {ACTORS.map(([kind, label]) => (
              <a
                key={label}
                href={auditHref(query, { actor: kind })}
                aria-current={actor === kind || undefined}
                className={`rounded-lg border bg-surface px-3 py-1 text-base text-text no-underline ${actor === kind ? "border-accent ring-1 ring-accent ring-inset" : "border-border hover:border-border-strong"}`}
              >
                {label}
              </a>
            ))}
          </nav>
          <span className="ml-auto flex max-w-full flex-wrap items-center gap-2.5 max-[520px]:ml-0">
            {rule === null && <select aria-label="App" className={`${FIELD} max-w-full`} value={app ?? ""} onChange={(event) => go({ project: event.target.value === "" ? null : event.target.value })}>
              <option value="">All apps</option>
              {app !== null && !apps.some((one) => one.id === app) && <option value={app}>{app}</option>}
              {apps.map((one) => <option key={one.id} value={one.id}>{one.name}</option>)}
            </select>}
            <select aria-label="Via" className={FIELD} value={via ?? ""} onChange={(event) => go({ via: event.target.value === "" ? null : event.target.value })}>
              <option value="">From anywhere</option>
              {(Object.entries(VIA_FILTER) as [Via, string][]).map(([value, word]) => <option key={value} value={value}>{word}</option>)}
            </select>
            <select aria-label="Kind" className={FIELD} value={category ?? ""} onChange={(event) => go({ category: event.target.value === "" ? null : event.target.value })}>
              {CATEGORIES.map(([value, word]) => <option key={value} value={value}>{word}</option>)}
            </select>
            {rule === null && (
              <select aria-label="Time range" className={FIELD} value={range.key} onChange={(event) => go({ range: event.target.value })}>
                {RANGES.map((one) => <option key={one.key} value={one.key}>{one.label}</option>)}
              </select>
            )}
            <details className="relative max-[860px]:static" onKeyDown={(event) => { if (event.key === "Escape") { close(event); event.currentTarget.querySelector("summary")?.focus(); } }}>
              <summary className={`${FIELD} cursor-pointer list-none`}>Export ▾</summary>
              <div className="absolute right-0 z-10 mt-1.5 grid w-[200px] rounded-[10px] border border-border bg-surface p-1.5 shadow-overlay max-[860px]:inset-x-4 max-[860px]:w-auto">
                <button type="button" disabled={exporting} className={ITEM} onClick={(event) => { close(event); void exported("csv"); }}>CSV</button>
                <button type="button" disabled={exporting} className={ITEM} onClick={(event) => { close(event); void exported("json"); }}>JSON</button>
                <button type="button" className={ITEM} onClick={(event) => { close(event); void copy(); }}>Copy link to this view</button>
              </div>
            </details>
          </span>
        </div>
        {rule !== null && (
          <p className="mt-2.5 flex flex-wrap items-center gap-2 text-base">
            <span>Changes to <span className="font-mono text-md">{rule}</span></span>
            <a className="text-md text-link no-underline hover:underline" href={auditHref(query, { rule: null })}>Show every change</a>
          </p>
        )}
        {said !== null && <p role="status" className={`mt-2.5 text-md ${said.failed ? "text-critical" : "text-text-dim"}`}>{said.line}</p>}
        {rule === null && (
          <nav aria-label="Common questions" className="mt-2.5 flex flex-wrap items-center gap-1.5 text-md">
            <span className="text-text-dim">Common questions:</span>
            {QUESTIONS.filter((one) => admin === true || one.admin === undefined).map((one) => (
              <a key={one.key} href={auditHref(query, { question: question === one ? null : one.key })} aria-current={question === one || undefined}
                className={`rounded-full border px-2.5 py-0.5 no-underline ${question === one ? "border-accent bg-accent-dim text-text" : "border-border text-text-dim hover:border-border-strong"}`}>
                {one.label}
              </a>
            ))}
          </nav>
        )}
        {failure !== null && <p className="mt-2.5 text-base text-critical">{failure}</p>}
        <div role="status">
          {arrived > 0 && (
            <button type="button" className="mt-2.5 w-full rounded-lg border border-accent bg-accent-dim px-3 py-1.5 text-md font-medium text-text" onClick={show}>
              {arrived === 1 ? "1 new event" : `${arrived} new events`} · Show
            </button>
          )}
        </div>
        {page !== null && (page.events.length === 0
          ? <p className="mt-2.5 text-base text-text-faint">{rule !== null ? "Nothing has changed this rule yet." : `Nothing happened in the last ${range.words}.${range === RANGES.at(-1) ? "" : " Try a wider range."}`}</p>
          : <div ref={table}><AuditTable events={page.events} now={Date.now()} admin={admin} names={labels(apps)} /></div>)}
        {page !== null && page.page !== null && (
          <div className="mt-3">
            <Pager label="Audit pages" noun={["event", "events"]} total={page.total} page={page.page} limit={limit}
              go={(to) => { window.location.hash = auditHref(query, { page: String(to.page), limit: String(to.limit), until: to.page === 1 ? null : until ?? opened }); }} />
          </div>
        )}
      </main>
    </div>
  );
}
