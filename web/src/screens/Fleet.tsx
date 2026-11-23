import { useEffect, useRef, useState, type KeyboardEvent, type MouseEvent, type ReactNode } from "react";
import { askForAccess, getFleet, reduced, said, version, waiting, type FleetPage, type FleetRow, type FleetStats, type Reduced, type TeamAccess } from "../api";
import { FLEET, S } from "../strings";
import { Button } from "../ui/Button";
import { actorName, anonymous } from "../ui/Actor";
import { Age } from "../ui/Age";
import { AppName, labels } from "../ui/AppName";
import { Avatar } from "../ui/Avatar";
import { checkIn, EnvOpen, EnvVersion, notRunning, shortPending } from "../ui/EnvLink";
import { Icon } from "../ui/Icon";
import { needInk, needShort } from "../ui/Need";
import { useMedia } from "../ui/media";
import { PAGE_SIZE, Pager } from "../ui/Pager";
import { Rail } from "../ui/Rail";
import { Review } from "../ui/Stage";
import { CELL, Table, TableWrap, WRAP_CELL } from "../ui/Table";
import { Patched } from "../ui/Patched";
import { Assign, Claim } from "./Claim";
import { POLL_MS, RecentActivity } from "./FleetActivity";
import { NeedsAttention } from "./FleetAttention";
import { FleetFilters } from "./FleetFilters";
import { HoldCell } from "./FleetHold";
import { HoldDialog, type Acting } from "./HoldDialog";
import { FleetPanel } from "./FleetPanel";
import { OwnerApps, ownerHref, ownerName } from "./Owner";
import { fleetHref, hashQuery } from "./query";
import { poll } from "../ui/poll";

// Each column answers one question (the redesign): which version, did it pass review, what runs in
// preview, what runs in production, who owns it, when it last moved. Access only where people sign
// in, the one place an app can be shared.
const NARROW_HIDDEN = "max-[1360px]:hidden"; // Updated gives way first: at 1280 the one-line Access column needs its width (the UX review)
const COLUMNS = [
  { key: "app", label: "App" },
  { key: "version", label: "Version" },
  { key: "review", label: "Review" },
  { key: "preview", label: "Preview" },
  { key: "prod", label: "Production" },
  { key: "owner", label: "Owner" },
  { key: "updated", label: "Updated", className: NARROW_HIDDEN },
];
const ACCESS_COLUMN = { key: "access", label: "Access" };
// With the side pane open beside it, the table folds away what the pane says for the selected row and
// keeps what is read down the list (the approved fleet redesign), so it fits beside the pane unscrolled.
const PEEKED = new Set(["version", "owner", "updated", "access"]); // Updated too: at 1280 the last column was cut off
// The wordy cells wrap, and under 1100px Updated gives way, so the table fits a laptop's width with
// Owner and its Claim in view.
const WRAPS = new Set(["app", "preview", "prod"]); // Access is one line, and the environments a line or two in the short words (the UX review: rows swelled to six)
// The row's own way into its panel for the keyboard; a click anywhere on the row does the same.
const DETAILS_COLUMN = { key: "details", label: "" };
const RESPONDER = new Set(["app", "prod", "owner"]);

const KBD = "rounded border border-b-2 border-border-strong bg-surface px-1.5 font-mono text-sm text-text";

const DASH = <span className="text-text-faint">—</span>;

// The Access column (EXPERIENCE.md D5), one line (the UX review: it wrapped to four): the reader's own place on a
// team row, then how far it is shared — Owner · shared with 2. An admin sees every row, and a row with no grant of
// theirs says only how far it is shared.
const YOU: Partial<Record<TeamAccess["level"], string>> = { owner: FLEET.owner, contributor: FLEET.contributor };
function shared(access: TeamAccess): string { // the table's Access cell, and a phone card's meta line
  const n = access.people + access.groups;
  const you = YOU[access.level];
  if (you === undefined) return access.everyone ? "Everyone" : n === 0 ? "Private" : FLEET.sharedWithAdmin.replace("<n>", String(n));
  return `${you} · ${access.everyone ? FLEET.sharedEveryone : n === 0 ? FLEET.private : FLEET.sharedWith.replace("<n>", String(n))}`;
}

// A row the reader can't open whole says so once, in its Access cell, with the way to ask (the UX review: a dash in
// every column said nothing): read-only to a viewer, no access to anyone else, and Ask to join, the app's access request.
function Asking({ app }: { app: Reduced }) {
  const [asked, setAsked] = useState<boolean | string>(false);
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <span className="whitespace-nowrap">{app.access.level === "viewer" ? FLEET.readOnly : FLEET.noAccess}</span>
      {asked === true ? <span className="text-text-faint">{FLEET.asked}</span>
        : <Button variant="secondary" size="sm" label={`${FLEET.askToJoin} ${app.name}`} onClick={() => void askForAccess(app.id).then(() => setAsked(true), (err: unknown) => setAsked(said(err)))}>{FLEET.askToJoin}</Button>}
      {typeof asked === "string" && <span role="alert" className="w-full text-critical">{asked}</span>}
    </span>
  );
}

// An org with no apps yet reads how to add the first, either way (the UX review): the CLI, with the panel that connects
// a coding agent, and GitHub; an admin's links where only an admin can act on them.
function EmptyFleet({ admin }: { admin: boolean }) {
  const link = "font-medium text-link no-underline hover:underline";
  return (
    <section aria-label={FLEET.emptyTitle} className="grid gap-2 rounded-[10px] border border-border bg-surface px-4 py-3.5 text-base">
      <h2 className="text-lg font-semibold">{FLEET.emptyTitle}</h2>
      <p className="text-text-dim">{FLEET.emptyLead}</p>
      <ul className="grid list-disc gap-1.5 pl-5">
        <li>{FLEET.emptyCli.split("`").map((part, i) => (i % 2 === 1 ? <code key={i} className="font-mono text-sm">{part}</code> : part))} <a className={link} href="#/settings/agents">{FLEET.emptyCliLink}</a></li>
        <li>{admin ? <>{FLEET.emptyGitHubAdmin} <a className={link} href="#/settings/github">{FLEET.emptyGitHubLink}</a></> : FLEET.emptyGitHubMember}</li>
      </ul>
      {admin && <p className="text-text-dim">{FLEET.emptyRules} <a className={link} href="#/guardrails">{FLEET.emptyRulesLink}</a></p>}
    </section>
  );
}

// The two D5 sentences say this host has no authentication; the banner sits above everything. On a
// host bound to localhost it is a warning, reachable from the network it is critical.
const BANNER: Record<Exclude<FleetPage["banner"], "none">, { line: string; tone: string }> = {
  loopback: { line: S.bannerLoopback, tone: "bg-amber-dim text-amber" },
  exposed: { line: S.bannerExposed, tone: "bg-critical-dim text-critical" },
};

// Remembered in this browser only. A browser that refuses storage shows the strip again on the next
// load, which is the whole cost, so neither access is allowed to break the page.
const DISMISSED = "canyon.demo-strip.dismissed";
const remembered = () => { try { return localStorage.getItem(DISMISSED) === "1"; } catch { return false; } };

// The demo strip (design/specs/screens.md, "How the guided task surfaces"): one line pointing a first
// visitor at the first blocked app in the server's order that is deployed somewhere — a version of an app
// that runs, held back by its review, which the visitor opens and their agent fixes. A blocked app nothing
// was ever deployed from is a seeded row on the demo, which no agent can pull (#2696): Needs attention
// lists it, and it is never where to start. Only on a host with no auth — on a company's own host it
// would be telling the people who run it where to look.
function DemoStrip({ page }: { page: FleetPage }) {
  const [dismissed, setDismissed] = useState(remembered);
  const blocked = page.apps.find((app): app is FleetRow => !reduced(app) && app.latest.stage === "blocked" && (app.envs.preview !== undefined || app.envs.prod !== undefined));
  if (dismissed || page.banner === "none" || blocked === undefined) return null;
  const dismiss = () => {
    try { localStorage.setItem(DISMISSED, "1"); } catch { /* dismissed for this visit only */ }
    setDismissed(true);
  };
  const shown = blocked.latest.package === null ? "" : ` ${version(blocked.latest.package, blocked.latest.label)}`;
  return (
    <div className="flex flex-wrap items-center gap-2.5 rounded-[9px] bg-critical-dim px-3.5 py-2.5 text-base">
      <span className="text-critical"><Icon name="blocked" /></span>
      <span className="min-w-0 flex-1 basis-[240px]"><b className="font-semibold">Start here:</b> {blocked.name}{shown} is blocked.</span>
      <a className="ml-auto font-medium whitespace-nowrap text-link no-underline hover:underline" href={"#/apps/" + blocked.id}>Open {blocked.name} →</a>
      <button type="button" aria-label="Dismiss" className="grid size-7 flex-none place-items-center rounded-[7px] text-text-dim hover:bg-surface" onClick={dismiss}><Icon name="close" size={14} /></button>
    </div>
  );
}


// How the fleet is doing, before what to do about it (the approved fleet top): the server's counts,
// the whole org for an admin and the reader's team apps for everyone else, never a page's rows.
const count = (n: number) => n.toLocaleString("en-US");
const plural = (n: number, one: string, many = `${one}s`) => `${count(n)} ${n === 1 ? one : many}`;
function Stats({ stats, admin }: { stats: FleetStats; admin: boolean }) {
  const cells = [
    { label: admin ? "Apps" : "Your apps", value: count(stats.apps), more: plural(stats.owners, "owner") },
    { label: "Deployed to production", value: count(stats.production), more: `${count(stats.preview)} deployed to preview` }, // a stopped one too: what was deployed
    { label: "Deploys, last 7 days", value: count(stats.deploys_7d), more: `${count(stats.production_7d)} to production` },
    { label: "Passed their latest review", value: count(stats.passed), of: count(stats.apps), more: "the newest upload of each app" },
  ];
  return (
    <dl className="grid grid-cols-4 rounded-[10px] border border-border bg-surface max-[860px]:grid-cols-2">
      {cells.map((cell) => (
        <div key={cell.label} className="grid content-start gap-0.5 border-l border-border px-4 py-3 first:border-l-0 max-[860px]:nth-[3]:border-l-0 max-[860px]:nth-[n+3]:border-t">
          <dt className="text-md font-medium text-text-dim">{cell.label}</dt>
          <dd className="tnum text-title font-semibold tracking-tight">{cell.value}{cell.of !== undefined && <small className="text-base font-medium text-text-dim"> of {cell.of}</small>}</dd>
          <dd className="text-md text-text-dim">{cell.more}</dd>
        </div>
      ))}
    </dl>
  );
}

// Who owns a row, or the two ways it gets an owner: an admin assigns one where people sign in,
// anyone with access claims it.
function Owner({ app, page }: { app: FleetRow | Reduced; page: FleetPage }) {
  const first = app.owners[0];
  if (first !== undefined) {
    return (
      <a className="inline-flex items-center gap-2 text-text no-underline hover:underline" href={ownerHref(first.subject)}>
        {!anonymous(first) && <Avatar name={actorName(first)} size="sm" />}{actorName(first)}
      </a>
    );
  }
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <span className="text-text-faint">No owner</span>
      <Takers app={app} page={page} />
    </span>
  );
}

// The two ways an ownerless app gets an owner: an admin assigns one where people sign in, anyone with
// access claims it.
function Takers({ app, page }: { app: FleetRow | Reduced; page: FleetPage }) {
  return (
    <>
      {page.banner === "none" && page.viewer.role === "admin" && <Assign app={app} />}
      <Claim app={app} display={page.viewer.display} />
    </>
  );
}

// Under 700px the table becomes a list of cards, and under 1100px the pane stacks above the table
// instead of beside it; jsdom has no matchMedia, so tests read the wide table.
const NARROW = "(max-width: 700px)";
const STACKED = "(max-width: 1100px)";

// One app as a phone reads it: the name and its review, then what runs and who owns it.
function Card({ app, page, act }: { app: FleetRow | Reduced; page: FleetPage; act: (acting: Omit<Acting, "asked">) => void }) {
  const owner = app.owners[0] === undefined ? "no owner" : actorName(app.owners[0]);
  if (reduced(app)) {
    return (
      <li className="grid gap-1 border-b border-border px-3.5 py-3 last:border-b-0">
        <a className="font-semibold text-text no-underline" href={"#/apps/" + app.id}><AppName app={app} /></a>
        <small className="text-sm text-text-faint">{app.summary || S.noDescription} · {owner}</small>
        {(app.envs?.preview ?? app.envs?.prod) !== undefined && (
          <span className="flex flex-wrap gap-3 text-sm">
            {app.envs?.preview !== undefined && <EnvOpen env={app.envs.preview} name="preview" />}
            {app.envs?.prod !== undefined && <EnvOpen env={app.envs.prod} name="production" />}
          </span>
        )}
        {page.banner === "none" && <span className="text-sm text-text-dim"><Asking app={app} /></span>}{/* the reader's level, as the table's Access column says it, where people sign in */}
        <HoldCell app={app} role={page.viewer.role} act={act}>{null}</HoldCell>
        {app.owners.length === 0 && <span className="flex flex-wrap gap-2"><Takers app={app} page={page} /></span>}
      </li>
    );
  }
  // what each environment runs now, in the table's short words (the UX review): a stopped app, or one the platform
  // hasn't reported running, is never said to run
  const lower = (line: string) => (line.startsWith("Canyon") ? line : line[0]!.toLowerCase() + line.slice(1));
  const running = (place: string, env: FleetRow["envs"]["preview"]) => {
    if (env === undefined) return null;
    if (waiting(env)) return `${place}: ${lower(shortPending(env.pending))}`;
    const pods = checkIn(env);
    const notes = [notRunning(env), env.pending === undefined ? null : shortPending(env.pending), pods?.silent ? pods.short : null].filter((one) => one !== null);
    return notes.length === 0 ? `${place} runs ${version(env.package, env.label)}` : `${place} ${version(env.package, env.label)}: ${notes.map(lower).join(", ")}`;
  };
  const where = [running("Production", app.envs.prod), running("Preview", app.envs.preview)].filter(Boolean).join(" · ") || "not deployed";
  return (
    <li className="grid gap-1.5 border-b border-border px-3.5 py-3 last:border-b-0">
      <span className="flex items-center justify-between gap-2">
        <a className="font-semibold text-text no-underline" href={"#/apps/" + app.id}><AppName app={app} /></a>
        <Review stage={app.latest.stage} gate={app.gate_state} warnings={app.warnings} />
      </span>
      <small className="text-sm text-text-faint">
        {app.latest.package !== null && `${version(app.latest.package, app.latest.label)} · `}{where} · {owner}{page.banner === "none" && ` · ${shared(app.access)}`}
      </small>
      {app.hold !== undefined && <span className="inline-flex items-center gap-1 text-sm font-medium text-critical"><Icon name="blocked" size={14} />{S.heldMark}</span>}{/* stopping and lifting are the app page's (#102), never a card's (the UX review) */}
      {app.owners.length === 0 && <span className="flex flex-wrap gap-2"><Takers app={app} page={page} /></span>}
    </li>
  );
}

// Everything after "#/fleet?" is the fleet read's query, so a link reproduces the view: every
// filter, search and page is a request, and the screen never narrows or reorders rows itself.
// The heading is the org's name, from the /auth/config read the shell makes before drawing anything.
export function Fleet({ org }: { org: string | null }) {
  const [query, setQuery] = useState(hashQuery);
  const [page, setPage] = useState<FleetPage | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  // Whether people sign in here, kept across queries: the page goes while a new query is asked, and
  // the Access filter must not go with it.
  const [banner, setBanner] = useState<FleetPage["banner"] | null>(null);
  // The last answer, kept across queries for what no query narrows — the counts and what needs doing —
  // so a sort or a page never blanks the top of the screen or asks it again.
  const [top, setTop] = useState<FleetPage | null>(null);
  // Which row the panel is looking at. Not in the hash: the hash IS this screen's query, and an
  // unknown key would be posted to the server — so the panel is not linkable and a link is the app page.
  const [selected, setSelected] = useState<string | null>(null);
  // A stop or a lift being asked, and the button that asked it, where the keyboard goes back after. Each
  // question is counted, so an answer to one closes that one alone, never a newer one asked meanwhile.
  const [acting, setActing] = useState<Acting | null>(null);
  const asker = useRef<Element | null>(null);
  const asked = useRef(0);
  const act = (next: Omit<Acting, "asked">) => { asker.current = document.activeElement; asked.current += 1; setActing({ ...next, asked: asked.current }); };
  const answered = (one: Acting) => () => {
    if (asked.current !== one.asked) return;
    setActing(null);
    (asker.current as HTMLElement | null)?.focus?.();
  };
  // The button that opened the panel, so closing puts the keyboard back where it was.
  const opener = useRef<HTMLButtonElement | null>(null);
  // A page chosen in the footer gives the keyboard to the heading once that page has come, so the
  // next row read is the new page's first, never the top of the document.
  const heading = useRef<HTMLHeadingElement>(null);
  const paged = useRef(false);
  const narrow = useMedia(NARROW);
  const stacked = useMedia(STACKED);

  useEffect(() => {
    const follow = () => setQuery(hashQuery());
    window.addEventListener("hashchange", follow);
    return () => window.removeEventListener("hashchange", follow);
  }, []);

  // The current page is read again every ten seconds while the tab shows, one read at a time (poll; #87). An answer to an
  // older query is dropped, and the page it answered goes with it, so one query's rows are never shown under another.
  useEffect(() => {
    setPage(null);
    let live = true;
    const { stop } = poll(async () => {
      try {
        // A cursor from an older link is never sent: the screen pages by number (contracts/routes.md, the fleet read).
        const sent = new URLSearchParams(query);
        sent.delete("cursor");
        const answer = await getFleet(sent.toString());
        if (live) { setPage(answer); setBanner(answer.banner); setTop(answer); setFailure(null); }
      } catch (err) {
        if (live) { setPage(null); setFailure((err as Error).message); }
      }
    }, POLL_MS);
    return () => { live = false; stop(); };
  }, [query]);

  useEffect(() => {
    if (page === null || !paged.current) return;
    paged.current = false;
    heading.current?.focus();
  }, [page]);

  const params = new URLSearchParams(query);
  // No filter, only how the list is ordered and paged: an empty answer is an empty fleet, not one that matches nothing.
  const unfiltered = [...params.keys()].every((key) => ["page", "limit", "cursor", "sort"].includes(key));
  // One person's apps is this same read, asked with ?owner=<subject>: the rows are the server's
  // answer either way, and only the presentation of them changes.
  const owner = params.get("owner");
  // The rows a page holds as the server served them: what the address asks for, past 200 clamped to 200.
  const limit = Math.min(Number(params.get("limit")) || PAGE_SIZE, 200);
  // Found in the page the server last answered, never a copy taken at click time: a poll that moves
  // the row moves the panel with it, and an answer without that id leaves no panel.
  const open = page?.apps.find((app) => app.id === selected);
  const signedIn = banner === "none";
  const peeking = open !== undefined && !reduced(open) && !stacked; // folded only where the pane sits beside the table
  // A responder off every team reads an app's name, owner and production, and stops or lifts it there (the UX review:
  // their fleet was mostly "—"); a page with a team row on it keeps every column.
  const offTeams = page !== null && page.viewer.role === "responder" && page.apps.length > 0 && page.apps.every(reduced);
  // An org with no apps: No apps yet comes first, with no tiles of zeros and no empty Needs panel above it (the UX review);
  // the org's own needs, an admin's, still show.
  const none = page !== null && unfiltered && page.apps.length === 0;
  const columns = offTeams ? COLUMNS.filter(({ key }) => RESPONDER.has(key))
    : (signedIn ? [...COLUMNS, ACCESS_COLUMN, DETAILS_COLUMN] : [...COLUMNS, DETAILS_COLUMN]).filter(({ key }) => !peeking || !PEEKED.has(key));
  // The fleet as a triage list (the approved redesign, "Clicks and keys"): j and k move the pane down and
  // up the rows that open one, Space opens or closes it on the row the keys are on, Enter opens that app
  // and Esc closes. Only while the keyboard is in the table or its pane (WCAG 2.1.4: a one-letter key
  // works where it is meant), only where the pane sits beside the table, and never while typing; Space
  // and Enter leave a focused link or button to do its own job.
  const onRow = useRef<string | null>(null); // the row the keys are on, kept when the pane closes so Space reopens it
  useEffect(() => { if (selected !== null) onRow.current = selected; }, [selected]);
  const ids = page?.apps.filter((app) => !reduced(app)).map((app) => app.id) ?? [];
  const keyed = !stacked && owner === null && ids.length > 0;
  const shown = open !== undefined && !reduced(open);
  const keys = (event: KeyboardEvent<HTMLDivElement>) => {
    if (!keyed || event.metaKey || event.ctrlKey || event.altKey || event.shiftKey || event.repeat) return;
    const target = event.target as HTMLElement;
    if (target.closest("input, select, textarea, [contenteditable]") !== null) return;
    const control = target.closest("a, button, summary") !== null;
    const row = (id: string) => [...document.querySelectorAll<HTMLElement>("tr[data-app]")].find((one) => one.dataset.app === id);
    const at = ids.indexOf(selected ?? onRow.current ?? "");
    const go = (id: string) => {
      event.preventDefault();
      opener.current = row(id)?.querySelector<HTMLButtonElement>("button[aria-label^='Details for']") ?? null; // where the keyboard goes back on close
      setSelected(id);
      row(id)?.scrollIntoView?.({ block: "nearest" });
    };
    const close = () => { event.preventDefault(); setSelected(null); opener.current?.focus(); };
    if (event.key === "j") go(ids[Math.min(at + 1, ids.length - 1)]!);
    else if (event.key === "k") go(ids[Math.max(at - 1, 0)]!);
    else if (event.key === "Escape" && shown) close();
    else if (control) return;
    else if (event.key === " ") { if (shown) close(); else go(ids[Math.max(at, 0)]!); }
    else if (event.key === "Enter" && shown) { event.preventDefault(); window.location.hash = "#/apps/" + open.id; }
  };
  // A click anywhere on a row opens its panel, except on what the row already links or acts on.
  const peek = (app: FleetRow) => (event: MouseEvent<HTMLTableRowElement>) => {
    if ((event.target as HTMLElement).closest("a, button, input, form") !== null) return;
    opener.current = event.currentTarget.querySelector<HTMLButtonElement>("button[aria-label^='Details for']"); // the row's own way in takes the keyboard back on close
    setSelected(app.id);
  };

  return (
    <div className="flex min-h-screen items-start max-[860px]:block">
      <Rail current="fleet" />
      <main className="mx-auto grid max-w-[1280px] min-w-0 flex-1 grid-cols-[minmax(0,1fr)] content-start gap-5 px-8 pt-7 pb-24 max-[860px]:px-4 max-[860px]:pt-4">
        {page !== null && page.banner !== "none" && (
          <div className={`flex flex-wrap items-center gap-2.5 rounded-[9px] px-3.5 py-2.5 text-base font-medium ${BANNER[page.banner].tone}`}>
            <Icon name="alert" />
            <span>{BANNER[page.banner].line}</span>
            <a className="ml-auto text-link no-underline hover:underline" href="#/settings">Turn on sign-in →</a>
          </div>
        )}
        <div className="flex flex-wrap items-end justify-between gap-4">
          <div className="grid gap-1">
            <h1 ref={heading} tabIndex={-1} className="text-title font-semibold tracking-tight outline-none">{owner === null ? org : ownerName(page?.apps ?? [], owner)}</h1>
            {owner === null && <p className="text-md text-text-dim">{params.get("scope") !== "team" ? (page !== null && page.viewer.role !== "admin" && !offTeams ? FLEET.allScopeMember : "Every app in the org") : page?.viewer.role === "admin" ? "The apps you own or work on. The counts are the whole org's." : "The apps you own or work on"}</p>}
          </div>
          {owner === null && !none && <FleetFilters query={query} signedIn={signedIn} scoped={signedIn && !offTeams} />}
        </div>
        {failure !== null && <p className="text-base text-critical">{failure}</p>}
        {/* What needs doing is the whole fleet's: org counts no filter narrows would read as this
            person's over their three apps, so one person's view leaves it out. */}
        {owner !== null ? (page !== null && <OwnerApps apps={page.apps} />) : (
          <>
            {top !== null && !offTeams && !none && <Stats stats={top.stats} admin={top.viewer.role === "admin"} />}{/* a responder off every team has no team counts: zeros help no one */}
            {page !== null && <DemoStrip page={page} />}
            {top !== null && !(none && (top.org_needs ?? []).length === 0) && <NeedsAttention page={top} query={query} signedIn={signedIn} lift={(app) => act({ app, kind: "lift" })} />}
            {page !== null && unfiltered && page.apps.length === 0 && <EmptyFleet admin={page.viewer.role === "admin"} />}
            {page !== null && narrow && !(unfiltered && page.apps.length === 0) && (
              <ul aria-label="Apps" className="rounded-[10px] border border-border bg-surface">
                {page.apps.length === 0 ? <li className="px-3.5 py-3 text-text-faint">{unfiltered ? "No apps yet" : "No apps match"}</li>
                  : page.apps.map((app) => <Card key={app.id} app={app} page={page} act={act} />)}
              </ul>
            )}
            {page !== null && !narrow && !(unfiltered && page.apps.length === 0) && (
              <div role="group" aria-label="Apps" aria-describedby={keyed ? "fleet-keys" : undefined} tabIndex={keyed ? 0 : undefined} onKeyDown={keys}
                className="flex items-start gap-3 rounded-[10px] outline-none focus-visible:ring-2 focus-visible:ring-accent max-[1100px]:flex-col-reverse max-[1100px]:items-stretch">
                <div className="min-w-0 flex-1">
                  <TableWrap minWidth={peeking ? 620 : 720}>
                    <Table
                      columns={columns}
                      rows={page.apps}
                      empty={unfiltered ? "No apps yet" : "No apps match"}
                      renderRow={(app) => {
                        // Under the name, the app's first need where it has one, in place of its summary (D15).
                        const needs = app.needs ?? [];
                        const name = (
                          <span className="grid leading-snug">
                            <a className="font-semibold whitespace-nowrap text-text no-underline hover:underline" href={"#/apps/" + app.id}><AppName app={app} /></a>
                            {needs.length > 0 ? (
                              <span className={`text-md font-medium ${needInk(needs[0]!)}`}>{needShort(needs[0]!)}{needs.length > 1 && S.needRowMore.replace("<n>", String(needs.length - 1))}</span>
                            ) : <span className="text-md text-text-faint">{app.summary || S.noDescription}</span>}
                          </span>
                        );
                        // An environment's refused calls, in a word beside what it runs, so colour is never the only signal.
                        const refused = (env: "preview" | "prod") => {
                          const refusing = needs.filter((need) => need.kind === "refused" && need.env === env);
                          return refusing.length === 0 ? null : <span className={`block text-md font-medium ${needInk(refusing[0]!)}`}>{S.needEnvRefused.replace("<n>", String(refusing.length))}</span>;
                        };
                        // An app off the viewer's teams is its name, owners and summary, and where a read-only
                        // member opens it: the team's other columns stay blank.
                        const cells = (row: Record<string, ReactNode>) => columns.map(({ key }) => <td key={key} className={`${WRAPS.has(key) ? WRAP_CELL : CELL} ${key === "version" ? "tnum text-text-dim" : key === "access" ? "text-md text-text-dim" : key === "updated" ? NARROW_HIDDEN : ""}`}>{row[key]}</td>);
                        if (reduced(app)) {
                          return (
                            <tr key={app.id}>
                              {cells({
                                app: name, version: null, review: null, owner: <Owner app={app} page={page} />, updated: null, access: <Asking app={app} />, details: null,
                                preview: app.envs?.preview === undefined ? DASH : <EnvOpen env={app.envs.preview} name="preview" />,
                                prod: <HoldCell app={app} role={page.viewer.role} act={act}>{app.envs?.prod === undefined ? DASH : <EnvOpen env={app.envs.prod} name="production" />}</HoldCell>,
                              })}
                            </tr>
                          );
                        }
                        return (
                          <tr key={app.id} data-app={app.id} onClick={peek(app)} className={`cursor-pointer hover:bg-surface-2 ${selected === app.id ? "bg-surface-2" : ""}`}>
                            {cells({
                              app: name,
                              version: app.latest.package === null ? DASH : version(app.latest.package, app.latest.label),
                              review: <Review stage={app.latest.stage} gate={app.gate_state} warnings={app.warnings} />,
                              preview: <><EnvVersion env={app.envs.preview} name="preview" /><Patched env={app.envs.preview} />{refused("preview")}</>,
                              // Held, the mark alone: stopping and lifting are the row's side pane's, never every row's (the UX review)
                              prod: app.hold !== undefined ? <span className="inline-flex items-center gap-1 font-medium text-critical"><Icon name="blocked" size={14} />{S.heldMark}</span>
                                : <span><EnvVersion env={app.envs.prod} name="production" /><Patched env={app.envs.prod} />{refused("prod")}</span>,
                              owner: <Owner app={app} page={page} />,
                              updated: <Age at={app.latest.since} long />,
                              access: shared(app.access),
                              details: (
                                <button type="button" className="grid size-7 place-items-center rounded-[7px] text-text-faint hover:bg-surface" aria-label={"Details for " + app.name} onClick={(event) => { opener.current = event.currentTarget; setSelected(app.id); }}>
                                  <Icon name="chevron" />
                                </button>
                              ),
                            })}
                          </tr>
                        );
                      }}
                    />
                  </TableWrap>
                  {keyed && <p id="fleet-keys" className="mt-2 text-md text-text-dim">
                    <kbd className={KBD}>j</kbd> <kbd className={KBD}>k</kbd> move · <kbd className={KBD}>Space</kbd> peek · <kbd className={KBD}>Enter</kbd> open the app · <kbd className={KBD}>Esc</kbd> close
                  </p>}
                </div>
                {open !== undefined && !reduced(open) && <FleetPanel key={open.id} app={open} display={page.viewer.display} org={org ?? "Your org"} close={() => { setSelected(null); opener.current?.focus(); }}
                  viewer={page.viewer} signedIn={signedIn} lift={(app) => act({ app, kind: "lift" })} act={act} />}
              </div>
            )}
          </>
        )}
        {page !== null && page.page !== null && (
          <Pager label="Fleet pages" noun={["app", "apps"]} total={page.total} page={page.page} limit={limit}
            go={(to) => {
              const href = fleetHref(hashQuery(), { page: to.page === 1 ? null : String(to.page), limit: to.limit === PAGE_SIZE ? null : String(to.limit), cursor: null });
              if (href === fleetHref(hashQuery(), {})) return; // the page already shown: nothing comes, so nothing takes the keyboard
              paged.current = true;
              window.location.hash = href;
            }} />
        )}
        {acting !== null && <HoldDialog key={acting.asked} acting={acting} now={page?.apps.find((app) => app.id === acting.app.id)} close={answered(acting)} />}
        {owner === null && <RecentActivity org={org ?? "Your org"} names={page === null ? undefined : labels(page.apps)} />}
      </main>
    </div>
  );
}
