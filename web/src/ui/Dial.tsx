import { useId } from "react";
import type { PolicyState } from "../api";
import { POLICY_STATE as WORD } from "../strings";

const STATES: PolicyState[] = ["off", "advisory", "blocking"];
const ON: Record<PolicyState, string> = { off: "bg-surface-2 text-text shadow-[inset_0_0_0_1px_var(--color-border-strong)]", advisory: "bg-amber-dim text-amber", blocking: "bg-critical-dim text-critical" };

// A rule's one control (the redesign: "one row per policy, one control each"): Off, Advisory or
// Blocking as one segmented choice, a radio group under the rule's name. A reader who cannot change
// it sees the same three words with the one in force marked, and nothing to press.
export function Dial({ name, value, change }: { name: string; value: PolicyState; change?: (state: PolicyState) => void }) {
  const group = useId(); // two rules may share a name, never a radio group
  return (
    <span role="radiogroup" aria-label={name} className="inline-flex flex-none self-start justify-self-start rounded-[7px] border border-border-strong bg-surface p-0.5">
      {STATES.map((state) => (
        <label key={state} className={`rounded-[5px] px-2.5 py-1 text-sm has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-accent max-[860px]:py-2 ${state === value ? `font-semibold ${ON[state]}` : `font-medium text-text-faint ${change === undefined ? "" : "hover:text-text"}`} ${change === undefined ? "cursor-default" : "cursor-pointer"}`}>
          <input type="radio" className="sr-only" name={group} value={state} checked={state === value} disabled={change === undefined} onChange={() => change?.(state)} />
          {WORD[state]}
        </label>
      ))}
    </span>
  );
}
