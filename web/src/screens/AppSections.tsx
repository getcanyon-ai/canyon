import { useEffect, useRef, useState, type ReactNode } from "react";
import { getRun, listAudit, listPackages, listRuns, pullRequestName, redeploy, said, version, waiting, type App as AppRead, type Package, type RunRow, type Status } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age } from "../ui/Age";
import { folded, sentence } from "../ui/Audit";
import { Button } from "../ui/Button";
import { exited, lastExited, restarts } from "../ui/EnvLink";
import { CELL, Table, TableWrap } from "../ui/Table";
import { failureWords } from "../ui/Verdict";

interface Page<Row> { rows: Row[]; next: string | null }

function Cells({ of }: { of: ReactNode[] }) {
  return <>{of.map((cell, at) => <td key={at} className={CELL}>{cell}</td>)}</>;
}

// One tab's list, read when the tab is first shown and never polled: the page's tick is the header,
// the versions and the review. Load more hands the server's cursor back untouched, and a row the list
// already carries is dropped — a page is not a snapshot. An empty list says what would be here.
function Listed<Row>({ columns, read, rowKey, cells, empty }: {
  columns: string[];
  read: (cursor: string | null) => Promise<Page<Row>>;
  rowKey: (row: Row) => string;
  cells: (row: Row) => ReactNode[];
  empty: string;
}) {
  const [page, setPage] = useState<Page<Row> | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const load = (cursor: string | null) =>
    read(cursor).then(
      (more) => {
        setPage((at) => ({ rows: [...(at?.rows ?? []), ...more.rows.filter((row) => at?.rows.every((seen) => rowKey(seen) !== rowKey(row)) ?? true)], next: more.next }));
        setFailure(null);
      },
      () => setFailure("This list couldn't be read."), // the route's own words are not the reader's
    );
  // Read when the tab is shown: its first page, once.
  useEffect(() => { void load(null); }, []);
  return (
    <>
      {page === null && failure === null && <p className="text-md text-text-faint">Reading…</p>}
      {failure !== null && (
        <p className="flex flex-wrap items-center gap-2 text-base text-critical">
          {failure}
          <Button variant="ghost" size="sm" onClick={() => { setFailure(null); void load(page?.next ?? null); }}>Try again</Button>
        </p>
      )}
      {page !== null && (
        <TableWrap minWidth={520}>
          <Table columns={columns.map((name) => ({ key: name, label: name }))} rows={page.rows} empty={empty} renderRow={(row) => <tr key={rowKey(row)}><Cells of={cells(row)} /></tr>} />
        </TableWrap>
      )}
      {page !== null && page.next !== null && <div><Button size="sm" onClick={() => void load(page.next)}>Load more</Button></div>}
    </>
  );
}

const FROM: Record<Package["source"]["kind"], string> = { upload: "CLI upload", repo: "Repository" };
const RESULT: Record<RunRow["state"], string> = { clean: "Passed", blocked: "Blocked", failed: "Failed", pending: "Reviewing", running: "Reviewing" };
// How a review ended, in the Reviews tab's words: an approval past every blocking finding is never Blocked.
const result = (row: RunRow) => (row.approved === true ? "Approved past review" : row.failure_reason === undefined ? RESULT[row.state] : `${RESULT[row.state]} · ${failureWords(row.failure_reason, row.failed_in)}`);
// A package row names its uploader by display, the subject where it has none (contracts/routes.md, the packages
// row): the anonymous admin of a host with sign-in off is nobody signed in, as ui/Actor.tsx says (#2696).
const uploader = (actor: string) => (actor === "anonymous" ? S.notSignedIn : actor);
const TABS = ["Versions", "Reviews", "Activity"] as const; // the secrets are the Secrets section's, beside its form
const FOLLOW_MS = 1000; // clients poll about once a second while a run is going (contracts/events.md)

// The app's history (the redesign: "history as tabs", Versions · Reviews · Activity; its secrets are the Secrets
// section's, design/specs/screens.md): what it uploaded, what each review said, and what was done to it. Rarely needed, so it
// comes last; a tab reads only once it is chosen. Who answers for the app is the Access card's.
export function AppSections({ project, envs, refreshed }: { project: AppRead; envs: Status["envs"]; refreshed: () => void }) {
  // No tab is open until one is chosen: nothing here is read until somebody asks for it.
  const [tab, setTab] = useState<(typeof TABS)[number] | null>(null);
  // the reviews newest first, page after page: the first of each package is its newest run
  const reviewed = useRef(new Set<string>());
  // each version's newest run, for the Versions tab, and the runs' next cursor: undefined before the first page, null after the last
  const newestRuns = useRef<{ of: Map<string, RunRow>; next: string | null | undefined }>({ of: new Map(), next: undefined });
  // a review's own page: the app's, pinned to the run (#2696)
  const reviewOf = (row: RunRow) => `#/apps/${project.id}?run=${encodeURIComponent(row.id)}`;
  const [deploying, setDeploying] = useState<{ run: string; said: string } | null>(null);
  const sending = useRef(false); // one deploy at a time: a second click sends nothing
  const live = useRef(true);
  useEffect(() => () => { live.current = false; }, []);
  // The pull request's deploy is followed to its end, as any deploy is, and the page read again then.
  const deployPR = async (row: RunRow) => {
    if (sending.current) return;
    sending.current = true;
    const name = pullRequestName(row.pull_request!);
    const say = (words: string) => { if (live.current) setDeploying({ run: row.id, said: words }); };
    try {
      const { run } = await redeploy(project.id, row.package);
      say(`Deploying ${name} to preview…`);
      for (;;) {
        await new Promise((done) => setTimeout(done, FOLLOW_MS));
        const { run: read } = await getRun(run.id);
        if (read.state === "pending" || read.state === "running") continue;
        say(read.state === "failed" ? `The deploy of ${name} didn't finish: ${failureWords(read.failure_reason ?? "error", read.failed_in)}.` : read.state === "blocked" && read.blocking > 0 && read.findings.some((one) => one.properties.canyon.blocks && one.approval === undefined) ? `Canyon's review blocked ${name}, so nothing was deployed.` : `${name} is on preview.`);
        refreshed();
        break;
      }
    } catch (err) {
      say(said(err));
    }
    sending.current = false;
  };
  // The tabs pattern: one Tab stop for the list, the arrow keys move between tabs and choose one.
  const step = (from: number, by: number) => {
    const next = TABS[(from + by + TABS.length) % TABS.length]!;
    setTab(next);
    document.getElementById(`history-tab-${next}`)?.focus();
  };
  // where a version is deployed, said as the cells say it: only one that runs is named alone
  // (contracts/routes.md, envs.<env>.state); one waiting on its first release runs nothing
  const at = (name: string, env: Status["envs"]["preview"], pkg: string) =>
    env === undefined || waiting(env) || env.package !== pkg ? null : env.state === "stopped" ? `${name} · stopped${exited(env) === null ? "" : `: ${exited(env)}`}` : env.state === "restarting" ? `${name} · keeps restarting${restarts(env) === null ? "" : `: ${restarts(env)}`}${lastExited(env) === null ? "" : ` · ${lastExited(env)}`}` : env.state === "unknown" ? `${name} · couldn't check` : name;
  const where = (pkg: string) => [at("Preview", envs.preview, pkg), at("Production", envs.prod, pkg)].filter(Boolean).join(" · ") || "—";
  return (
    <section aria-label="History">
      {/* Closed until asked for (the app-page redesign): its tabs read nothing until one is chosen anyway. */}
      <details className="grid grid-cols-[minmax(0,1fr)] gap-3">
      <summary className="cursor-pointer"><h2 className="inline text-base font-semibold">History<span className="ml-2 text-md font-regular text-text-faint">Versions, reviews, activity</span></h2></summary>
      <div role="tablist" aria-label="Versions, reviews and activity" className="flex gap-5 overflow-x-auto border-b border-border">
        {TABS.map((name, at) => (
          <button key={name} id={`history-tab-${name}`} type="button" role="tab" aria-selected={tab === name} aria-controls="history-panel"
            tabIndex={tab === name || (tab === null && at === 0) ? 0 : -1} onClick={() => setTab(name)}
            onKeyDown={(event) => { if (event.key === "ArrowRight") step(at, 1); else if (event.key === "ArrowLeft") step(at, -1); }}
            className={`-mb-px py-2 text-base font-medium whitespace-nowrap ${tab === name ? "text-text shadow-[inset_0_-2px_0_var(--color-accent)]" : "text-text-dim hover:text-text"}`}>
            {name}
          </button>
        ))}
      </div>
      {tab !== null && <div id="history-panel" role="tabpanel" aria-labelledby={`history-tab-${tab}`} aria-label={tab} tabIndex={0}>
        {tab === "Versions" && (
          <Listed
            columns={["Version", "Review", "Commit", "From", "Uploaded", "Running in"]}
            read={async (cursor) => {
              const read = await listPackages(project.id, cursor);
              // Each version's review is its newest run, linked to it, a blocked one marked (#2696). The runs, newest
              // first, are read on until every version of the page has one, or they run out or reach past its oldest upload.
              // The versions are the list and their reviews only add to it: runs that can't be read leave each review a dash.
              if (cursor === null) newestRuns.current = { of: new Map(), next: undefined };
              const runs = newestRuns.current;
              const oldest = read.packages.reduce((first, one) => (one.created_at < first ? one.created_at : first), read.packages[0]?.created_at ?? "");
              try {
                while (runs.next !== null && read.packages.some((one) => !runs.of.has(one.id))) {
                  const page = await listRuns(project.id, runs.next ?? null);
                  for (const one of page.runs) if (!runs.of.has(one.package)) runs.of.set(one.package, one);
                  runs.next = page.next;
                  if ((page.runs.at(-1)?.started_at ?? "") < oldest) break;
                }
              } catch {
                runs.next = null; // read no further: what was read stands, the rest a dash
              }
              return { rows: read.packages.map((one) => ({ ...one, review: runs.of.get(one.id) })), next: read.next };
            }}
            rowKey={(row) => row.id}
            cells={(row) => [
              version(row.id, row.label),
              row.review === undefined ? "—" : <a className={`no-underline hover:underline ${row.review.state === "blocked" && row.review.approved !== true ? "font-medium text-critical" : "text-link"}`} href={reviewOf(row.review)}>{result(row.review)}</a>,
              <span className="font-mono text-md">{row.commit.slice(0, 7)}</span>, [FROM[row.source.kind], uploader(row.source.actor)].filter(Boolean).join(" · "), <Age at={row.created_at} long />, where(row.id)]}
            empty="Nothing uploaded yet. Upload with canyon deploy."
          />
        )}
        {tab === "Reviews" && (
          <Listed
            columns={["Version", "Result", "Blocking", "Advisory", "Started by", "When"]}
            read={(cursor) => {
              if (cursor === null) reviewed.current.clear(); // the list read again from its start
              return listRuns(project.id, cursor).then((read) => ({
                rows: read.runs.map((row) => {
                  const newest = !reviewed.current.has(row.package);
                  reviewed.current.add(row.package);
                  return { ...row, newest };
                }),
                next: read.next,
              }));
            }}
            rowKey={(row) => row.id}
            cells={(row) => [row.check ? <a className="text-link no-underline hover:underline" href={reviewOf(row)}>From a check</a> : row.pull_request ? (
              // a person deploys a pull request's version to preview once its newest review passed (#1947)
              <span className="grid justify-items-start gap-1">
                <a className="text-link no-underline hover:underline" href={reviewOf(row)}><bdi>{pullRequestName(row.pull_request)}</bdi></a>
                {row.newest && (row.state === "clean" || row.approved === true) && <Button size="sm" onClick={() => void deployPR(row)}>{S.prDeploy}</Button>}
                {deploying?.run === row.id && <span role="status" className="text-sm text-text-dim">{deploying.said}</span>}
              </span>
            ) : <a className="text-link no-underline hover:underline" href={reviewOf(row)}>{version(row.package, row.label)}</a>, result(row), row.blocking, row.warnings, row.started_by === null ? "Canyon (automatic)" : <Actor actor={row.started_by} />, <Age at={row.started_at} long />]}
            empty="Not reviewed yet."
          />
        )}
        {tab === "Activity" && (
          <Listed
            columns={["When", "Who", "What"]}
            read={(cursor) => listAudit(`project=${encodeURIComponent(project.id)}${cursor === null ? "" : `&cursor=${encodeURIComponent(cursor)}`}`).then((read) => ({ rows: read.events.filter((event) => !folded(event)), next: read.next }))}
            rowKey={(row) => row.id}
            cells={(row) => [<Age at={row.ts} long />, <Actor actor={row.actor} />, sentence(row)]}
            empty="Nothing recorded for this app yet."
          />
        )}
      </div>}
      </details>
    </section>
  );
}
