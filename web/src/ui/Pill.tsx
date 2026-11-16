import type { ReactNode } from "react";
import { Icon, type IconName } from "./Icon";

// One colour per status, everywhere (the redesign, "The rules of the new look"): red for blocked, amber
// for waiting or approved with an exception, green for passed or live, grey for not checked; violet is
// Beta, which is no status (EXPERIENCE.md D5). A pill is always a word, with an icon when it is a status;
// the colour is never the only signal.
export type Tone = "critical" | "amber" | "ok" | "neutral";

const TONE: Record<Tone | "beta", string> = {
  critical: "bg-critical-dim text-critical",
  amber: "bg-amber-dim text-amber",
  ok: "bg-ok-dim text-ok",
  neutral: "bg-neutral-dim text-neutral",
  beta: "bg-beta-dim text-beta",
};

export function Pill({ tone, icon, title, children }: { tone: Tone | "beta"; icon?: IconName; title?: string; children: ReactNode }) {
  return (
    <span title={title} className={`inline-flex h-6 items-center gap-1.5 rounded-full text-sm font-medium whitespace-nowrap ${icon === undefined ? "px-2.5" : "pr-2.5 pl-2"} ${TONE[tone]}`}>
      {icon !== undefined && <Icon name={icon} size={14} />}
      {children}
    </span>
  );
}
