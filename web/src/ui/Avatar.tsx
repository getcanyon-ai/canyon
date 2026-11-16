// A person's initials in a circle: the rail's foot and the fleet's Owner column. Decorative — the
// name always sits beside it.
// Two letters at most, each a whole character (Array.from), not half of one.
export const initials = (name: string) => name.split(/\s+/).filter(Boolean).slice(0, 2).map((part) => Array.from(part)[0]!.toUpperCase()).join("");

export function Avatar({ name, size = "md" }: { name: string; size?: "sm" | "md" }) {
  return (
    <span aria-hidden="true" className={`grid flex-none place-items-center rounded-full border border-border bg-surface-2 font-semibold text-text-dim text-sm ${size === "sm" ? "size-6" : "size-7"}`}>
      {initials(name)}
    </span>
  );
}
