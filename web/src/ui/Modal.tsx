import { useEffect, useRef, type KeyboardEvent, type ReactNode } from "react";

// A question over the page (#102): the page dimmed behind it, the keyboard kept inside it, Escape closing it unless it is
// busy, and the focus back where it was when it closes. label names it; first is what the keyboard lands on.
export function Modal({ label, close, busy = false, children }: { label: string; close: () => void; busy?: boolean; children: ReactNode }) {
  const box = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const before = document.activeElement as HTMLElement | null;
    box.current?.querySelector<HTMLElement>("textarea, input, select, button:not([disabled])")?.focus();
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
    const [first, last] = [stops[0], stops[stops.length - 1]];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
  };
  return (
    <div className="fixed inset-0 z-30 grid place-items-start bg-scrim px-4 pt-24">
      <div ref={box} role="dialog" aria-modal="true" aria-label={label} onKeyDown={keep}
        className="mx-auto grid w-full max-w-[480px] gap-2.5 rounded-[10px] border border-border-strong bg-surface p-4 shadow-overlay">
        {children}
      </div>
    </div>
  );
}
