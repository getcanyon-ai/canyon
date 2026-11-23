import { createContext, useContext, useState } from "react";
import { signOut, type Viewer } from "../api";
import { AUTH, S } from "../strings";
import { Avatar } from "./Avatar";
import { Icon, Mark, Wordmark } from "./Icon";

// The rail carries the views that exist; the rest join it with their screens. The OS decides the
// theme. It reads no hash: main.tsx re-renders the whole tree on hashchange, and each screen says
// which one it is.
//
// Who is reading sits at its foot on every page (the redesign: "Who am I acting as?" belongs on every
// page). The shell learns it once and hands it down here, so no screen carries it: `viewer` is null
// until it is known and on a host with sign-in off, where nobody is signed in and there is nothing
// to sign out of. `admin` is whether the reader may open Guardrails, an admin's screen
// (design/specs/screens.md, A4b): the shell answers false until it knows, so a member is never offered
// it; the default is only a tree rendered without the shell's.
export const RailContext = createContext<{ org: string | null; viewer: Viewer | null; admin: boolean; needs?: number | "?" | "unread" }>({ org: null, viewer: null, admin: true });

// Canyon's source: the AGPL-3.0's offer to the people who use it over a network (LICENSE, section 13), on every host.
export const SOURCE = "https://github.com/getcanyon-ai/canyon";

const VIEWS = [
  ["fleet", "Fleet"],
  ["guardrails", "Guardrails"],
  ["audit", "Audit"],
  ["settings", "Settings"],
] as const;


// Under 860px the rail is a bar across the top, and the views and the foot fold behind a menu button.
export function Rail({ current }: { current: (typeof VIEWS)[number][0] }) {
  const { org, viewer, admin, needs = 0 } = useContext(RailContext);
  const [open, setOpen] = useState(false);
  return (
    <nav aria-label="Canyon" onKeyDown={(event) => { if (event.key === "Escape") setOpen(false); }} className="sticky top-0 flex h-screen w-[224px] min-w-[224px] flex-col gap-5 overflow-y-auto border-r border-border bg-surface px-3 py-4 max-[860px]:static max-[860px]:h-auto max-[860px]:w-auto max-[860px]:min-w-0 max-[860px]:gap-0 max-[860px]:border-r-0 max-[860px]:border-b max-[860px]:px-4 max-[860px]:py-3">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2.5 px-2 text-base font-semibold max-[860px]:px-0"><Mark /><Wordmark /></div>
        <button type="button" className="-mr-2.5 hidden size-11 place-items-center rounded-[7px] text-text-dim hover:bg-surface-2 max-[860px]:grid" aria-expanded={open} aria-controls="rail-menu" onClick={() => setOpen(!open)}>
          <Icon name={open ? "close" : "menu"} size={20} />
          <span className="sr-only">Menu</span>
        </button>
      </div>
      <div id="rail-menu" className={`flex flex-1 flex-col gap-5 max-[860px]:pt-4 ${open ? "" : "max-[860px]:hidden"}`}>
        {org !== null && (
          <div className="grid px-2 leading-snug">
            <span className="text-md font-semibold">{org}</span>
            <span className="text-sm text-text-faint">{window.location.host}</span>
          </div>
        )}
        <div className="grid gap-0.5">
          {VIEWS.filter(([view]) => admin || view !== "guardrails").map(([view, label]) => (
            <a
              key={view}
              href={`#/${view}`}
              aria-current={current === view ? "page" : undefined}
              className={`flex items-center gap-2.5 rounded-[7px] px-2.5 py-2 text-base font-medium no-underline max-[860px]:py-3 ${current === view ? "bg-accent-dim text-accent" : "text-text-dim hover:bg-surface-2"}`}
            >
              <Icon name={view} />
              {label}
              {view === "fleet" && needs !== 0 && (
                <span className="ml-auto rounded-full bg-critical-dim px-1.5 text-sm font-semibold text-critical tnum">
                  <span aria-hidden="true">{needs === "unread" ? "?" : needs}</span>
                  <span className="sr-only">{needs === "?" ? S.needsRailUnchecked : needs === "unread" ? S.needsRailUnread : needs === 1 ? S.needsCountOne : S.needsCount.replace("<n>", String(needs))}</span>
                </span>
              )}
            </a>
          ))}
        </div>
        {viewer !== null && (
          <div className="mt-auto flex items-center gap-2.5 border-t border-border px-2 pt-3">
            <Avatar name={viewer.display} />
            <div className="grid min-w-0 leading-snug">
              <span className="truncate text-md font-semibold">{viewer.display}</span>
              <span className="text-sm text-text-faint">
                {AUTH.role[viewer.role]} · <button type="button" className="-my-2 py-2 text-link hover:underline max-[860px]:-my-3 max-[860px]:py-3" onClick={() => void signOut()}>{AUTH.signOut}</button>
              </span>
              {viewer.github_logins.length > 0 ? (
                <span className="truncate text-sm text-text-faint">{AUTH.githubLinked.replace("<logins>", viewer.github_logins.join(", "))}</span>
              ) : viewer.github_link && (
                <a href="/api/v1/github/link" className="-my-2 py-2 text-sm text-link no-underline hover:underline max-[860px]:-my-3 max-[860px]:py-3">{AUTH.githubLink}</a>
              )}
            </div>
          </div>
        )}
        <a href={SOURCE} target="_blank" rel="noreferrer" className={`${viewer === null ? "mt-auto " : ""}-my-2 px-2 py-2 text-sm text-text-faint no-underline hover:underline max-[860px]:-my-3 max-[860px]:py-3`}>{S.sourceCode}</a>
      </div>
    </nav>
  );
}
