import { useEffect, useRef, useState, type ReactNode } from "react";
import { holdProduction, liftHold, reduced, said, type App, type Reduced, type Viewer } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { heldLine } from "../ui/Headline";
import { Icon } from "../ui/Icon";

type Row = App | Reduced; // a fleet row, or the app page's own read of the app (#2723)
// A stop or a lift being asked: the row as it was read when asked, and which time the fleet asked one.
export type Acting = { app: Row; kind: "stop" | "lift"; asked: number };

// Who is offered what, from the read alone (design/PERMISSIONS.md, Security responders): stopping is a
// responder's or an admin's on any row not held; lifting an admin's, or a responder's off the app's team,
// which is a row the server reduces for them. The server decides both, a team on the hold's record too.
const stops = (role: Viewer["role"], app: Row) => (role === "responder" || role === "admin") && app.hold === undefined;
// With nothing in production nothing stops: the same hold stops every promotion, and is said as a hold (#2696).
const empty = (app: Row) => app.envs?.prod === undefined;
export const lifts = (role: Viewer["role"], app: Row) => app.hold !== undefined && (role === "admin" || (role === "responder" && reduced(app)));

// A row's production while held, and what the reader may do about it, beside what the cell shows otherwise.
export function HoldCell({ app, role, act, children }: { app: Row; role: Viewer["role"]; act: (acting: Omit<Acting, "asked">) => void; children: ReactNode }) {
  const stop = stops(role, app);
  const lift = lifts(role, app);
  if (app.hold === undefined && !stop) return <>{children}</>;
  return (
    <span className="flex flex-wrap items-center gap-x-2 gap-y-1">
      {app.hold !== undefined ? <span className="inline-flex items-center gap-1 font-medium text-critical"><Icon name="blocked" size={14} />{S.heldMark}</span> : children}
      {stop && <Button variant="ghost" size="sm" label={`${empty(app) ? S.holdProduction : S.stopProduction} of ${app.name}`} onClick={() => act({ app, kind: "stop" })}>{empty(app) ? S.holdProduction : S.stopProduction}</Button>}
      {lift && <Button variant="secondary" size="sm" label={`${S.liftHold} on ${app.name}`} onClick={() => act({ app, kind: "lift" })}>{S.liftHold}</Button>}
    </span>
  );
}

// The question a stop or a lift asks before it is sent, over the page: a row is too narrow to hold it, and
// a table that scrolls sideways would clip a popover. Escape and Cancel close it unsent, never while it is
// sending; its answer closes it — this question alone, never a newer one (`close` is scoped to it) — and the
// fleet's next read shows the row held or let go. A lift names the hold it read (held_at): a hold that
// changed under the question, on a poll or by the server's 409, is said, and nothing is offered on it.
// A refusal is the server's own sentence.
export function HoldDialog({ acting, now, close }: { acting: Acting; now: Row | undefined; close: () => void }) {
  const [reason, setReason] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => { box.current?.querySelector<HTMLElement>("textarea, button")?.focus(); }, []); // the keyboard follows the question
  const { app, kind } = acting;
  const send = async (call: () => Promise<unknown>) => {
    if (sending) return;
    setSending(true);
    try {
      await call();
      close();
    } catch (err) {
      setFailure(said(err));
    }
    setSending(false);
  };
  const changed = kind === "lift" && now !== undefined && now.hold?.at !== app.hold?.at;
  const holds = kind === "stop" && empty(app); // nothing runs in production: a hold, which stops nothing that runs
  const title = (kind === "stop" ? (holds ? S.holdAsk : S.stopAsk) : S.liftAsk).replace("<app>", () => app.name);
  const confirm = holds ? S.holdProduction : S.stopProduction;
  const cancel = sending ? null : <Button variant="ghost" onClick={close}>Cancel</Button>;
  const going = <p role="status" className="text-md text-text-dim">{kind === "stop" ? (holds ? "Holding production…" : "Stopping production…") : "Lifting the hold…"}</p>;
  return (
    <div ref={box} role="dialog" aria-modal="true" aria-label={title} onKeyDown={(event) => { if (event.key === "Escape" && !sending) close(); }}
      className="fixed inset-x-4 top-24 z-20 mx-auto grid max-w-[480px] gap-2.5 rounded-[10px] border border-border-strong bg-surface p-4 shadow-overlay">
      <h2 className="text-base font-semibold">{title}</h2>
      {kind === "stop" ? (
        <>
          <p className="text-md text-text-dim">{holds ? S.holdNothing : S.stopping.replace("<app>", () => app.name)}</p>
          <p className="text-md text-text-dim">{S.stopPreview}</p>
          <label className="grid gap-1 text-md font-medium text-text-dim">
            Reason
            <textarea maxLength={1024} rows={3} readOnly={sending} className="rounded-lg border border-border-strong bg-surface px-2.5 py-1.5 text-base text-text" value={reason} onChange={(event) => setReason(event.target.value)} />
          </label>
          <span className="flex flex-wrap items-start gap-2">
            {sending ? going : reason.trim() === "" ? <Button disabled reason="Type the reason above first.">{confirm}</Button>
              : <Button variant="danger" onClick={() => void send(() => holdProduction(app.id, reason))}>{confirm}</Button>}
            {cancel}
          </span>
        </>
      ) : app.hold !== undefined && (
        <>
          <p className="text-md text-text-dim">{heldLine(app.hold)}</p>
          {changed ? <p role="alert" className="text-base text-critical">{S.holdChanged}</p> : null}
          <span className="flex flex-wrap items-start gap-2">
            {sending ? going : !changed && (
              <>
                <Button variant="secondary" onClick={() => void send(() => liftHold(app.id, app.hold!.at, false))}>{S.leaveStopped}</Button>
                {app.hold.version !== null && <Button variant="primary" onClick={() => void send(() => liftHold(app.id, app.hold!.at, true))}>{S.startAgain.replace("<version>", () => app.hold!.version!)}</Button>}
              </>
            )}
            {cancel}
          </span>
        </>
      )}
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </div>
  );
}
