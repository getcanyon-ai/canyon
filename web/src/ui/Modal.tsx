import { useEffect, useRef, type KeyboardEvent, type ReactNode } from "react";

// A dialog's backdrop and its box, which every dialog over the page is drawn with.
export const OVER = "fixed inset-0 z-30 grid grid-rows-[minmax(0,1fr)] items-start bg-scrim px-4 pt-24 pb-4 [@media(max-height:560px)]:pt-4";
export const OVER_BOX = "mx-auto grid max-h-full w-full overflow-y-auto overscroll-contain rounded-[10px] border border-border-strong bg-surface p-4 shadow-overlay";

// A question over the page (#102): the page dimmed behind it, the keyboard kept inside it, Escape closing it unless it is
// busy, and the focus back where it was when it closes. label names it; first is what the keyboard lands on. sheet is a wide
// one for reading, the whole screen on a phone, its rows a head, a middle that scrolls and a foot.
export function Modal({ label, close, busy = false, sheet = false, children }: { label: string; close: () => void; busy?: boolean; sheet?: boolean; children: ReactNode }) {
  const box = useRef<HTMLDivElement>(null);
  // where the keyboard was before the question: read while rendering, before the question's own parts take it
  const was = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => {
    const before = was.current;
    // a question that put the keyboard somewhere already (its safe answer) keeps it there
    if (!box.current?.contains(document.activeElement)) box.current?.querySelector<HTMLElement>("textarea, input, select, button:not([disabled])")?.focus();
    return () => before?.focus?.();
  }, []);
  const keep = (event: KeyboardEvent) => {
    if (event.key === "Escape") {
      event.preventDefault();
      if (!busy) close();
      return;
    }
    if (event.key !== "Tab") return;
    const stops = [...(box.current?.querySelectorAll<HTMLElement>("a[href], textarea, input, select, button:not([disabled])") ?? [])];
    const at = stops.indexOf(document.activeElement as HTMLElement); // -1 on a line that took the keyboard and is no stop, as a refusal (#145)
    const ends = event.shiftKey ? at <= 0 : at === -1 || at === stops.length - 1;
    if (ends) { event.preventDefault(); stops[event.shiftKey ? stops.length - 1 : 0]?.focus(); }
  };
  return (
    // Never taller than the window, where it scrolls inside itself and its buttons are always reached (#126): a short window,
    // a phone held sideways, gives it the room above it too.
    <div className={sheet ? `${OVER} max-[620px]:p-0` : OVER}>
      <div ref={box} role="dialog" aria-modal="true" aria-label={label} onKeyDown={keep}
        className={`${OVER_BOX} gap-2.5 ${sheet ? "max-w-[960px] grid-rows-[auto_minmax(0,1fr)_auto] max-[620px]:h-full max-[620px]:rounded-none max-[620px]:border-0" : "max-w-[480px]"}`}>
        {children}
      </div>
    </div>
  );
}
