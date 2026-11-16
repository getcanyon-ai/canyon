import { useEffect, useState } from "react";
import { getFleet, said, type FleetPage, type FleetRow, type Need, type Reduced } from "../api";
import { S } from "../strings";
import { DoneLine, NeedLine, OrgNeedLine, type Done } from "../ui/Need";
import { fleetHref, freshQuery } from "./query";

// What needs a person, one line a need (design/EXPERIENCE.md D15; contracts/routes.md, *What needs a person*): the needs
// of the rows the server's needs_you selects, by the rank each carries, then since. The count is tiles.needs_you; five lines,
// then the rest on a click. A line acts in place where the reader may, and says what it did with what undoes it. While the
// read couldn't check whether some deployments run, it says so, and never that nothing needs the reader.
const SHOWN = 5;
const LISTED = 50; // the rows one read lists: past them, the fleet's own needs_you filter

type App = FleetRow | Reduced;
const count = (n: number) => n.toLocaleString("en-US");

export function NeedsAttention({ page, query, signedIn, lift }: { page: FleetPage; query: string; signedIn: boolean; lift: (app: App) => void }) {
  const total = page.tiles.needs_you ?? 0;
  const unchecked = page.unchecked ?? 0;
  const [asked, setAsked] = useState(0); // read again after a click, whatever the count says
  const [rows, setRows] = useState<{ key: string; apps: App[]; org: Need[] } | { key: string; failed: string } | null>(null);
  const [done, setDone] = useState<Done[]>([]);
  const [all, setAll] = useState(false);
  const key = `${JSON.stringify(page.tiles)}|${asked}`;
  useEffect(() => {
    if (total === 0) return;
    let live = true;
    void getFleet(new URLSearchParams({ needs_you: "true", limit: String(LISTED) }).toString()).then(
      (answer) => { if (live) setRows({ key, apps: answer.apps, org: answer.org_needs ?? [] }); },
      (err: unknown) => { if (live) setRows({ key, failed: said(err).replace(/\.$/, "") }); }, // the count stands, and the line says why
    );
    return () => { live = false; };
  }, [key, total]);
  // Most urgent first across apps: the server's rank, then the oldest first, none last; a tie keeps the server's order.
  // Only this read's rows: a line just acted on is gone until the list is read again, its done line in its place.
  const read = total === 0 || rows === null || rows.key !== key ? null : rows;
  const failed = read !== null && "failed" in read ? read.failed : null;
  // the org's own needs (org_needs, an admin's) rank among the apps' by the same rank and since
  const lines = (read === null || "failed" in read ? [] : [...read.org.map((need) => ({ app: null as App | null, need })), ...read.apps.flatMap((app) => (app.needs ?? []).map((need) => ({ app: app as App | null, need })))])
    .sort((a, b) => a.need.rank - b.need.rank || (a.need.since === null ? 1 : 0) - (b.need.since === null ? 1 : 0) || (a.need.since ?? "").localeCompare(b.need.since ?? ""));
  const shown = all ? lines : lines.slice(0, SHOWN);
  const more = Math.max(total, lines.length) - shown.length;
  const title = page.viewer.role === "admin" ? "Needs attention" : "Needs you";
  const did = (what: Done) => { setDone((was) => [what, ...was]); setAsked((n) => n + 1); };
  return (
    <section aria-label={title} className="rounded-[10px] border border-border bg-surface">
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 border-b border-border px-4 py-2.5">
        <h2 className="text-base font-semibold">{title}</h2>
        {total > 0 && <span className="text-md text-text-dim">Most urgent first</span>}
      </div>
      {unchecked > 0 && (
        <p role="status" className="border-b border-border px-4 py-3 text-base text-amber">
          {unchecked === 1 ? S.needsUncheckedOne : S.needsUnchecked.replace("<n>", count(unchecked))}
        </p>
      )}
      {total === 0 && done.length === 0 ? (
        unchecked === 0 && (
          <p className="px-4 py-3 text-base text-text-dim">
            Nothing needs you.{page.stats.apps > 0 && page.stats.passed === page.stats.apps && ` All ${count(page.stats.apps)} ${page.stats.apps === 1 ? "app" : "apps"} passed their latest review.`}
          </p>
        )
      ) : (
        <ul>
          {done.map((what, i) => <DoneLine key={`done-${done.length - i}`} what={what} clear={() => { setDone((was) => was.filter((one) => one !== what)); setAsked((n) => n + 1); }} />)}
          {shown.map(({ app, need }, i) => app === null
            ? <OrgNeedLine key={`org-${need.kind}-${need.channel_id ?? i}`} need={need} />
            : <NeedLine key={`${app.id}-${need.kind}-${need.env ?? ""}-${need.destination ?? need.email ?? i}`} app={app} need={need} viewer={page.viewer} signedIn={signedIn} lift={lift} done={did} />)}
          {more > 0 && (
            <li className="px-4 py-2.5">
              {failed !== null && <p role="alert" className="mb-1 text-md text-critical">{S.needsUnread.replace("<error>", failed)}</p>}
              {!all && lines.length > SHOWN ? (
                <button type="button" className="text-md font-medium text-link hover:underline" onClick={() => setAll(true)}>{S.needsMore.replace("<n>", count(more))}</button>
              ) : (
                <a className="text-md font-medium text-link no-underline hover:underline" href={fleetHref(freshQuery(query), { needs_you: "true" })}>{S.needsMore.replace("<n>", count(more))}</a>
              )}
            </li>
          )}
        </ul>
      )}
    </section>
  );
}
