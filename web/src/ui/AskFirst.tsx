import { useEffect, useRef, type KeyboardEvent, type ReactNode } from "react";
import { SAVE } from "../strings";
import { Button } from "./Button";

// The question before a choice that changes what apps can do, or a removal that can't simply be put back (design/EXPERIENCE.md,
// D16): beside the control, what changes under it, and nothing saved until yes. No goes back to what is saved, as Escape does.
// The keyboard lands on yes, the answer the person was reaching for.
// `wait` holds yes back with its reason while what the question says is still being read.
export function AskFirst({ question, yes, onYes, onNo, busy = false, wait = null, danger = false, children }: {
  question: string; yes: string; onYes: () => void; onNo: () => void; busy?: boolean; wait?: string | null; danger?: boolean; children?: ReactNode;
}) {
  const first = useRef<HTMLButtonElement>(null);
  const ready = !busy && wait === null;
  useEffect(() => { if (ready) first.current?.focus(); }, [ready]);
  const key = (event: KeyboardEvent) => {
    if (event.key !== "Escape" || busy) return;
    event.preventDefault();
    onNo();
  };
  return (
    <div role="group" aria-label={question} onKeyDown={key} className="mt-3 grid grid-cols-[minmax(0,1fr)] gap-2 rounded-[10px] border border-border-strong bg-surface px-4 py-3">
      <p className="text-base font-semibold">{question}</p>
      {children}
      <span className="flex flex-wrap gap-2">
        {busy || wait !== null ? <Button disabled reason={busy ? "Saving…" : wait!}>{yes}</Button>
          : <Button variant={danger ? "danger" : "primary"} ref={first} onClick={onYes}>{yes}</Button>}
        {!busy && <Button variant="ghost" onClick={onNo}>{SAVE.cancel}</Button>}
      </span>
    </div>
  );
}
