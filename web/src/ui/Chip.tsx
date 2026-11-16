import type { ReactNode } from "react";
import { S } from "../strings";
import { Pill, type Tone } from "./Pill";

// A finding's severity and whether it blocks, as a pill with no icon. Critical and high both block,
// so both are red and the word tells them apart; medium is amber; low and advisory are grey — low
// has no colour of its own. A blocking finding someone approved is an exception: amber. blocks is
// rendered from the finding, never derived, so display cannot fail open. Beta marks a feature, not a
// finding: its own tone, and what it means wherever it is drawn (EXPERIENCE.md D5) — the title for a
// pointer, and read after the word by assistive tech. Given `why`, the sentence is hidden under that id
// instead, for the link or group around it to take as its description rather than into its name.
const TONE: Record<"critical" | "high" | "medium" | "low" | "advisory" | "blocks" | "approved" | "beta", Tone | "beta"> = {
  critical: "critical",
  high: "critical",
  medium: "amber",
  low: "neutral",
  advisory: "neutral",
  blocks: "critical",
  approved: "amber",
  beta: "beta",
};

export function Chip({ tone, why, children }: { tone: keyof typeof TONE; why?: string; children: ReactNode }) {
  return (
    <Pill tone={TONE[tone]} title={tone === "beta" ? S.betaWhy : undefined}>
      {children}
      {tone === "beta" && (why === undefined ? <span className="sr-only">: {S.betaWhy}</span> : <span id={why} hidden>{S.betaWhy}</span>)}
    </Pill>
  );
}
