import { useEffect, useRef, useState } from "react";
import { S } from "../strings";
import { Icon } from "../ui/Icon";
import { useInWindow } from "../ui/media";
import { STAGE_OPTIONS } from "../ui/Stage";
import { fleetHref, freshQuery, hashQuery } from "./query";

// Every key the search and the menu can leave in the query, beside the chips' `tile`. Clear filters
// takes all of them, the search included, which is what the words say.
const FILTERS = ["tile", "stage", "running", "stack", "q", "needs_you", "access", "held"];

// Where an app is deployed, which is not where its newest upload got to (contracts/routes.md, the
// fleet read): an app can be blocked and still be deployed to production from an older one. The
// filter reads the deploy, never whether the app runs now: a stopped app matches it too.
const RUNNING = [
  ["production", "In production"],
  ["preview", "In preview"],
  ["nothing", "Not deployed"],
] as const;

// Who can open an app, in the Access column's words (contracts/routes.md, the fleet read's `access`).
const ACCESS = [
  ["private", "Private"],
  ["shared", "Shared"],
  ["everyone", "Everyone"],
] as const;

// The server's three orders (contracts/routes.md, the fleet read's sort); risk is the key left out.
const SORTS = [
  ["risk", "Risk"],
  ["changed", "Recently changed"],
  ["name", "Name"],
] as const;

// "Your apps" is the apps the reader owns or works on, by their own grants (scope=team); all apps is the key left out.
const SCOPES = [
  ["team", "Your apps"],
  [null, "All apps"],
] as const;

const FIELD = "h-[34px] min-w-0 rounded-[7px] border border-border-strong bg-surface px-2.5 text-base";
const LABEL = "grid gap-1 text-md font-medium text-text-dim";
// A typed box waits this long after the last key before it asks, so a word is one request, not five.
const SETTLE_MS = 300;

// A box that sends what is typed once the typing settles, and shows what the query holds when the
// query changes under it (Clear filters, a chip, the back button). An emptied box deletes its key,
// `name`, which the box also reads straight from the address bar.
function Typed({ name, sent, send, type, placeholder, label, className }: { name: string; sent: string; send: (value: string | null) => void; type: "search" | "text"; placeholder: string; label?: string; className: string }) {
  const [text, setText] = useState(sent);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  // The key's value the box last knew in the address bar: the one it was handed, or the one it sent.
  const known = useRef(sent);
  // A query changed under the box drops what was still waiting to be sent, or it would undo it.
  useEffect(() => { clearTimeout(timer.current); known.current = sent; setText(sent); }, [sent]);
  useEffect(() => () => clearTimeout(timer.current), []);
  const change = (value: string) => {
    setText(value);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      // The query can change before React draws it, on a busy page in the same turn as this timer
      // (#2857), so the address bar decides: a key no longer what the box knew drops the word.
      if ((new URLSearchParams(hashQuery()).get(name) ?? "") !== known.current) return;
      const next = value.trim() === "" ? null : value.trim();
      known.current = next ?? "";
      send(next);
    }, SETTLE_MS);
  };
  return <input type={type} maxLength={64} value={text} placeholder={placeholder} aria-label={label} className={className} onChange={(event) => change(event.target.value)} />;
}

// The search and the Filter menu over the fleet. Every control is a request: it writes one key into
// the hash, and the rows, their order and the page are the server's answer. Nothing here sorts,
// filters or counts. Every change goes back to the first page: page 4 of one view is no page of
// another. There is no Apply: a choice is sent as it is made. `access` and "Your apps" are offered
// only where people sign in, the one place an app is shared and a reader holds grants of their own.
export function FleetFilters({ query, signedIn }: { query: string; signedIn: boolean }) {
  const params = new URLSearchParams(query);
  const [open, setOpen] = useState(false);
  const menu = useRef<HTMLDivElement>(null);
  const box = useRef<HTMLDivElement>(null);
  useInWindow(box, open);
  const toggle = useRef<HTMLButtonElement>(null);
  // The query is read from the address bar as the change happens, not from the render that drew the
  // control: two changes in quick succession must compose, not overwrite each other.
  const go = (set: Record<string, string | null>) => {
    window.location.hash = fleetHref(hashQuery(), { ...set, page: null, cursor: null });
  };
  // The menu closes on Escape, giving the keyboard back to its button, and on a click anywhere else.
  // Closed, it stays in the page and hidden, so a Stack still settling is still sent.
  useEffect(() => {
    if (!open) return;
    const away = (event: MouseEvent) => { if (!menu.current?.contains(event.target as Node)) setOpen(false); };
    document.addEventListener("mousedown", away);
    return () => document.removeEventListener("mousedown", away);
  }, [open]);
  const chosen = ["stage", "running", "stack", "needs_you", "access", "held"].filter((key) => params.has(key)).length;
  const select = (key: string, label: string, any: string, options: readonly (readonly [string, string])[]) => (
    <label className={LABEL}>
      {label}
      <select className={FIELD} value={params.get(key) ?? ""} onChange={(event) => go({ [key]: event.target.value === "" ? null : event.target.value })}>
        <option value="">{any}</option>
        {options.map(([value, word]) => <option key={value} value={value}>{word}</option>)}
      </select>
    </label>
  );

  return (
    <div className="flex flex-wrap items-center gap-2">
      {signedIn && (
        <div role="group" aria-label="Which apps" className="inline-flex h-[34px] rounded-[7px] border border-border-strong bg-surface p-0.5">
          {SCOPES.map(([value, word]) => (
            <button key={word} type="button" aria-pressed={params.get("scope") === value} onClick={() => go({ scope: value })}
              className={`rounded-[5px] px-2.5 text-base ${params.get("scope") === value ? "bg-surface-2 font-semibold text-text shadow-sm" : "font-medium text-text-dim hover:text-text"}`}>
              {word}
            </button>
          ))}
        </div>
      )}
      <label className="relative flex min-w-0 grow items-center sm:w-[260px] sm:grow-0">
        <span className="pointer-events-none absolute left-2.5 text-text-faint"><Icon name="search" /></span>
        <Typed name="q" sent={params.get("q") ?? ""} send={(value) => go({ q: value })} type="search" placeholder="Search apps" label="Search apps" className={`${FIELD} w-full pl-8`} />
      </label>
      <div className="relative" ref={menu} onKeyDown={(event) => { if (event.key === "Escape" && open) { setOpen(false); toggle.current?.focus(); } }}>
        <button type="button" ref={toggle} aria-expanded={open} aria-controls="fleet-filter" onClick={() => setOpen(!open)}
          className="inline-flex h-[34px] items-center gap-1.5 rounded-[7px] border border-border-strong bg-surface px-3 text-base font-medium hover:bg-surface-2">
          <Icon name="filter" />Filter{chosen > 0 && <span className="tnum text-text-dim">({chosen})</span>}
        </button>
        <div id="fleet-filter" ref={box} hidden={!open} className="absolute right-0 z-10 mt-1.5 grid w-[280px] gap-3 overflow-y-auto rounded-[10px] border border-border bg-surface p-3.5 shadow-overlay max-[520px]:w-[calc(100vw-2rem)]">
          {select("stage", "Status", "Any status", STAGE_OPTIONS)}
          {select("running", "Deployed", "Anywhere", RUNNING)}
          {signedIn && select("access", "Access", "Anyone", ACCESS)}
          <label className={LABEL}>
            Stack
            <Typed name="stack" sent={params.get("stack") ?? ""} send={(value) => go({ stack: value })} type="text" placeholder="Next.js" className={FIELD} />
          </label>
          {/* The server computes this queue — blocked, never reviewed, or nobody to page. Only the
              exact `true` turns it on, so the key is written or it is gone. */}
          <label className="flex items-center gap-2 text-base">
            <input type="checkbox" checked={params.get("needs_you") === "true"} onChange={(event) => go({ needs_you: event.target.checked ? "true" : null })} />
            Only apps that need you
          </label>
          {/* The held apps among the rows whose hold the reader reads (contracts/routes.md, the fleet read's
              `held`): it shows nobody a hold their rows do not. Only the exact `true` turns it on. */}
          <label className="flex items-center gap-2 text-base">
            <input type="checkbox" checked={params.get("held") === "true"} onChange={(event) => go({ held: event.target.checked ? "true" : null })} />
            {S.heldFilter}
          </label>
        </div>
      </div>
      <label className="flex items-center gap-2 text-base text-text-dim">
        Sort
        <select className={FIELD} value={params.get("sort") ?? "risk"} onChange={(event) => go({ sort: event.target.value === "risk" ? null : event.target.value })}>
          {SORTS.map(([value, word]) => <option key={value} value={value}>{word}</option>)}
        </select>
      </label>
      {FILTERS.some((key) => params.has(key)) && <a className="text-base text-link" href={fleetHref(freshQuery(query), {})}>Clear filters</a>}
    </div>
  );
}
