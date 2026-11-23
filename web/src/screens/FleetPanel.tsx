import { useEffect, useRef, useState } from "react";
import { getProjectEgress, getRun, getStatus, listAudit, version, waiting, type AuditEvent, type Env, type FleetRow, type Reduced, type RunRead, type Status, type Viewer } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { AppName } from "../ui/AppName";
import { EnvCard } from "../ui/EnvCards";
import { Headline, headline, leads, type Act } from "../ui/Headline";
import { NeedList } from "../ui/Need";
import { Icon } from "../ui/Icon";
import { forAgent } from "../ui/Finding";
import { failedAfterReview } from "../ui/Verdict";
import { useBroke } from "./BuildFailure";
import { Claim } from "./Claim";
import { ACTIVITY_ACTIONS, activity, Row } from "./FleetActivity";
import { prodPending } from "./Outbound";
import { HoldCell, type Acting } from "./FleetHold";
import { ownerHref } from "./Owner";

const H3 = "text-sm font-semibold tracking-wide text-text-faint uppercase";
const running = (env: Env | { pending: unknown } | undefined): Env | null => (env !== undefined && !waiting(env as Env) ? (env as Env) : null);

// What the band's next step is called; each is done on the app page, which the step opens. Opening
// production is the one step done in place.
const STEP: Record<Exclude<Act, "open">, string> = { review: "Open review", approve: "Approve with a reason", cancel: "Open review", promote: "Promote", copy: "See what broke", outbound: S.openOutbound };
const LINK = "inline-flex h-[34px] items-center gap-1.5 rounded-[7px] px-3 text-base no-underline";

// A triage peek beside the table (the approved fleet redesign): the app page's top block — the headline
// that says where the newest version stands and what to do, what runs in preview and production — and
// its last few changes, without leaving the query that got you here. The app page is the workbench: the
// name and every step open it. `display` is the viewer's name, which a claim of an ownerless app is made under.
// viewer, signedIn and lift are the fleet's, for the needs' lines, which act here as they do in its Needs you.
export function FleetPanel({ app, display, org, close, viewer, signedIn, lift, act }: {
  app: FleetRow; display: string; org: string; close: () => void; viewer: Viewer; signedIn: boolean; lift: (app: FleetRow | Reduced) => void;
  act: (acting: Omit<Acting, "asked">) => void; // stopping, holding or lifting production, which the row no longer offers (the UX review)
}) {
  const [run, setRun] = useState<RunRead | Error | null>(null);
  const [status, setStatus] = useState<Status | Error | null>(null);
  const [changes, setChanges] = useState<AuditEvent[] | Error | null>(null);
  const [refused, setRefused] = useState<string[]>([]);
  const [again, setAgain] = useState(0); // a need's line acted on: read the app again // what production can't reach that waits on a person (#2723)
  const panel = useRef<HTMLElement>(null);
  const lastRun = app.last_run;
  const since = app.latest.since; // a run that ends moves its version's stage, and with it since
  // What runs where and who owns it, as the row says them: an app that stops, a release refused, an
  // owner claimed — each changes the status read without a new run.
  const facts = JSON.stringify([app.envs, app.owners]);

  // No timer of its own: the fleet's poll moves the row, and this reads again when it does. The panel
  // is keyed on the app, so a read in flight for the row you just left never lands on this one.
  useEffect(() => setRun(null), [lastRun]); // a new run's verdict is unknown until read: never the last run's
  useEffect(() => {
    let live = true;
    if (lastRun !== null) void getRun(lastRun).then(({ run: at }) => { if (live) setRun(at); }, (err: Error) => { if (live) setRun(err); });
    return () => { live = false; };
  }, [lastRun, since]);
  useEffect(() => {
    let live = true;
    setStatus(null); // fails closed: what runs where changed, so the last read says nothing until the new one comes
    void getStatus(app.id).then((at) => { if (live) setStatus(at); }, (err: Error) => { if (live) setStatus(err); });
    // The row is a team row, so its outbound reads: what production refused that waits on a person, as the app page's Outbound card says it.
    void getProjectEgress(app.id).then((egress) => { if (live) setRefused(prodPending(egress)); }, () => { if (live) setRefused([]); });
    // The app's last changes, asked of the server by the actions the fleet's activity shows: never a page read and thinned here.
    void listAudit(new URLSearchParams({ project: app.id, action: ACTIVITY_ACTIONS.join(","), limit: "20" }).toString()).then(
      (page) => { if (live) setChanges(page.events); },
      (err: Error) => { if (live) setChanges(err); },
    );
    return () => { live = false; };
  }, [app.id, lastRun, since, facts, again]);

  // Below 1100px the panel opens above a table the reader has scrolled past, so the keyboard goes
  // to it rather than staying where the row was.
  useEffect(() => { panel.current?.focus(); }, [app.id]);

  const href = "#/apps/" + app.id;
  const told = changes === null || changes instanceof Error ? null : activity(changes, org, Date.now(), app.id);
  const recent = told === null ? [] : [...told.pinned, ...told.lines].sort((a, b) => Date.parse(b.at) - Date.parse(a.at)).slice(0, 5);
  const newest = app.latest.package === null ? null : version(app.latest.package, app.latest.label);
  const st = status instanceof Error || status === null ? null : status;
  const preview = running(st?.envs.preview);
  const prod = running(st?.envs.prod);
  const read = run === null || run instanceof Error || run.id !== lastRun ? null : run;
  const newestRun = read !== null && read.package === app.latest.package ? read : null;
  // A build or a start that broke after its review: its own last line, as the app page says it.
  const broke = useBroke(newestRun !== null && failedAfterReview(newestRun) && newestRun.failure_reason !== "detect" ? newestRun : null);
  // Fails closed: with the newest run unread, or unreadable, the status alone never says ready or live.
  const band = st === null || (lastRun !== null && read === null) ? null : headline({
    latest: app.latest, newest, neverReviewed: app.gate_state === "never_reviewed", run: newestRun,
    preview, prod, previewName: preview === null ? null : version(preview.package, preview.label), prodName: prod === null ? null : version(prod.package, prod.label), receipt: st.receipt, hold: st.hold,
    broke: broke === null || broke instanceof Error ? undefined : broke, refused,
  });
  // The pane is a peek at what matters and what to do, and offers only the steps that fit it (#2723): a band about the
  // newest version's review offers it, where the app page decides it, never one about a restart or a hold; and
  // Approve with a reason only to a reader the run lets approve, as the app page offers it.
  // What waits on a person leads, as on the app page (D15), with the band where Headline's leads puts it.
  const needs = st?.needs ?? [];
  const top = leads(band, needs);
  const shownBand = top.band;
  const own = band === null ? [] : [band.primary, band.secondary].filter((one): one is Act => one !== undefined && (one !== "approve" || newestRun?.approvable === true));
  const steps = band?.review === true && !own.includes("review") ? ["review" as const, ...own] : own;
  const review = newestRun === null ? href : `${href}?run=${encodeURIComponent(newestRun.id)}`;
  // The first blocking problem nobody approved: what it is, where, and the fix — the row can't say it.
  const blocking = newestRun?.state === "blocked" ? newestRun.findings.find((one) => one.properties.canyon.blocks && one.approval === undefined) : undefined;
  const [copied, setCopied] = useState<"yes" | "no" | null>(null);
  const copy = () => {
    if (blocking === undefined) return;
    const entry = newestRun!.findings.filter((one) => one.entry === blocking.entry); // its entry's, as the app page copies it
    Promise.resolve().then(() => navigator.clipboard.writeText(forAgent(entry[0]!, app.id, entry.slice(1)))).then(() => setCopied("yes"), () => setCopied("no"));
  };
  const step = (one: Act, first: boolean) => {
    const look = first ? "bg-accent font-semibold text-accent-ink hover:brightness-110" : "border border-border-strong bg-surface font-medium text-text hover:bg-surface-2";
    if (one !== "open") return <a key={one} href={one === "outbound" ? `${href}/outbound` : one === "promote" || one === "copy" ? href : review} className={`${LINK} ${look}`}>{STEP[one]}</a>;
    return prod === null ? null : <a key={one} href={prod.url} target="_blank" rel="noreferrer" className={`${LINK} ${look}`}>Open production<Icon name="external" size={14} /></a>;
  };

  return (
    <aside
      ref={panel}
      tabIndex={-1}
      aria-label={"Details for " + app.name}
      onKeyDown={(event) => { if (event.key === "Escape") close(); }}
      className="sticky top-4 grid max-h-[calc(100vh-2rem)] w-[380px] shrink-0 content-start gap-3 overflow-y-auto rounded-[10px] border border-border bg-surface px-4 py-4 focus-visible:ring-2 focus-visible:ring-accent max-[1100px]:static max-[1100px]:max-h-none max-[1100px]:w-auto"
    >
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <h2 className="text-lg font-semibold"><AppName app={app} /></h2>
        <a className="text-base font-medium text-link no-underline hover:underline" href={href}>Open app →</a>
        <button type="button" className="ml-auto text-base text-link" onClick={close}>Close</button>
        <p className="w-full text-md text-text-dim">
          {[app.summary || S.noDescription, app.stack].filter(Boolean).join(" · ")}
          {" · "}
          {app.owners.length === 0 ? <span className="text-amber">No owner</span>
            : app.owners.map((owner, at) => (
              <span key={owner.subject}>{at > 0 && ", "}<a className="text-link no-underline hover:underline" href={ownerHref(owner.subject)}><Actor actor={owner} /></a></span>
            ))}
        </p>
        {app.owners.length === 0 && !needs.some((need) => need.kind === "unowned") && <Claim app={app} display={display} />}{/* else its need's line carries it */}
        {/* Stop production, Hold production or Lift the hold, where the reader may: the row's peek holds them, never every row */}
        <span className="w-full"><HoldCell app={app} role={viewer.role} act={act}>{null}</HoldCell></span>
      </div>
      {status instanceof Error && <p className="text-base text-critical">{status.message}</p>}
      {run instanceof Error && <p className="text-base text-critical">{run.message}</p>}
      {!top.first && top.needs.length > 0 && (
        <section aria-label="Needs you" className="rounded-[10px] border border-border">
          <NeedList app={{ ...app, needs }} needs={top.needs} viewer={viewer} signedIn={signedIn} lift={lift} changed={() => setAgain((n) => n + 1)} />
        </section>
      )}
      {shownBand !== null && (
        <Headline band={shownBand} />
      )}
      {/* The band's next steps under it, not beside it: 380px leaves the sentence no room beside its buttons. */}
      {shownBand !== null && (steps.length > 0 || blocking !== undefined) && (
        <div className="flex flex-wrap items-center gap-2">
          {steps.map((one, at) => step(one, at === 0))}
          {blocking !== undefined && <button type="button" className={`${LINK} border border-border-strong bg-surface font-medium text-text hover:bg-surface-2`} onClick={copy}>Copy for your agent</button>}
        </div>
      )}
      {shownBand !== null && blocking !== undefined && (
        <p className="text-md text-text-dim">
          <b className="font-semibold text-text">{blocking.properties.canyon.title}</b>
          {" · "}<span className="font-mono text-sm">{blocking.locations[0].physicalLocation.artifactLocation.uri}:{blocking.locations[0].physicalLocation.region.startLine}</span>
          {". "}{blocking.properties.canyon.fix}
        </p>
      )}
      {top.first && top.needs.length > 0 && (
        <section aria-label="Needs you" className="rounded-[10px] border border-border">
          <NeedList app={{ ...app, needs }} needs={top.needs} viewer={viewer} signedIn={signedIn} lift={lift} changed={() => setAgain((n) => n + 1)} />
        </section>
      )}
      {copied === "yes" && <p role="status" className="text-md text-ok">Copied for your agent.</p>}
      {copied === "no" && <p role="alert" className="text-md text-critical">Couldn't copy. The browser did not allow it.</p>}
      {st !== null && (
        <>
          <EnvCard name="Preview" env={st.envs.preview} empty="Nothing is in preview." who={preview?.deployed_by === undefined || preview.deployed_by === null ? undefined : <Actor actor={preview.deployed_by} />} />
          {/* Held, it says so and since when; the band above says who held it and who lets it run again (#2723). */}
          <EnvCard name="Production" env={st.envs.prod} preview={st.envs.preview} {...(st.hold === undefined ? { empty: "Nothing is in production." } : { blank: S.heldSince.replace("<date>", () => st.hold!.at.slice(0, 10)), empty: "" })}
            who={prod?.promoted_by === undefined || prod.promoted_by === null ? undefined : <>promoted by <Actor actor={prod.promoted_by} /></>} />
        </>
      )}
      {changes instanceof Error && <p className="text-base text-critical">{changes.message}</p>}
      {recent.length > 0 && (
        <section aria-label="Recent changes">
          <div className="flex items-baseline justify-between">
            <h3 className={H3}>Recent changes</h3>
            <a className="text-md font-medium text-link no-underline hover:underline" href={`#/audit?project=${encodeURIComponent(app.id)}`}>All activity</a>
          </div>
          {/* One app's changes as the fleet's activity tells them, one line per run, never naming the app again. */}
          <ul className="divide-y divide-border">{recent.map((line) => <Row key={line.key} line={line} />)}</ul>
        </section>
      )}
    </aside>
  );
}
