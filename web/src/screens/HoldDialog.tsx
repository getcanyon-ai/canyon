import { useCallback, useState } from "react";
import { holdProduction, liftHold, reduced, said, type App, type Reduced, type Viewer } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { heldLine } from "../ui/Headline";
import { Modal } from "../ui/Modal";

type Row = App | Reduced; // a fleet row, or the app page's own read of the app (#2723)
export type Acting = { app: Row; kind: "stop" | "lift"; asked: number };

// Who stops or holds an app's production: a security responder or an admin, while it isn't held (design/PERMISSIONS.md,
// The hold). Who lifts a hold: an admin, or a responder off the app's team, whose read of it is reduced.
export const stops = (role: Viewer["role"], app: Row) => (role === "responder" || role === "admin") && app.hold === undefined;
export const lifts = (role: Viewer["role"], app: Row) => app.hold !== undefined && (role === "admin" || (role === "responder" && reduced(app)));
// Nothing runs in production: a hold, which stops nothing that runs.
export const empty = (app: Row) => app.envs?.prod === undefined;

// The question that stops or holds production, or lifts the hold, over the page (#102): Escape closes it unless it is
// sending, and the keyboard stays in it.
// offline: the reader may also take production offline (an owner or an admin), so the stop question names that way too.
export function HoldDialog({ acting, now, close, offline = false }: { acting: Acting; now: Row | undefined; close: () => void; offline?: boolean }) {
  const [reason, setReason] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const { app, kind } = acting;
  const send = async (call: () => Promise<unknown>) => {
    if (sending) return;
    setSending(true);
    try {
      await call();
      close();
    } catch (err) {
      // fetch rejects with a TypeError when no answer came. Asking again is safe: the server resumes a stop it began, and
      // answers 409 held to one it finished (contracts/routes.md, The hold).
      setFailure(err instanceof TypeError && kind === "stop" && !empty(app) ? S.stopUnknown : said(err));
    }
    setSending(false);
  };
  const changed = kind === "lift" && now !== undefined && now.hold?.at !== app.hold?.at;
  const holds = kind === "stop" && empty(app);
  const title = (kind === "stop" ? (holds ? S.holdAsk : S.stopAsk) : S.liftAsk).replace("<app>", () => app.name);
  const confirm = holds ? S.holdProduction : S.stopProduction;
  // A lift's keyboard lands on Cancel, its safe answer: Enter on what opened the question, and Enter again, lifts nothing.
  // A stop's lands on its reason. Placed as the button mounts, which Modal then keeps.
  const safe = useCallback((button: HTMLButtonElement | null) => { if (kind === "lift") button?.focus(); }, [kind]);
  const cancel = sending ? null : <Button ref={safe} variant="ghost" onClick={close}>Cancel</Button>;
  const going = <p role="status" className="text-md text-text-dim">{kind === "stop" ? (holds ? "Holding production…" : "Stopping production…") : "Lifting the hold…"}</p>;
  return (
    <Modal label={title} close={close} busy={sending}>
      <h2 className="text-base font-semibold">{title}</h2>
      {kind === "stop" ? (
        <>
          <p className="text-md text-text-dim">{holds ? S.holdNothing : S.stopping.replace("<app>", () => app.name)}</p>
          <p className="text-md text-text-dim">{S.stopPreview}</p>
          {/* the other way to take production down, for whoever wanted that one (#102) */}
          {!holds && offline && <p className="text-md text-text-dim">{S.stopOrOffline}</p>}
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
                <Button variant="secondary" onClick={() => void send(() => liftHold(app.id, app.hold!.at, false))}>{S.liftKeepStopped}</Button>
                {app.hold.version !== null && <Button variant="secondary" onClick={() => void send(() => liftHold(app.id, app.hold!.at, true))}>{S.liftAndStart.replace("<version>", () => app.hold!.version!)}</Button>}
              </>
            )}
            {cancel}
          </span>
        </>
      )}
      {failure !== null && <p role="alert" className="text-base text-critical">{failure}</p>}
    </Modal>
  );
}
