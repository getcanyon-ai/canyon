import type { ReactNode, Ref } from "react";

interface Base {
  variant?: "primary" | "secondary" | "ghost" | "danger";
  // block is the sign-in and approval pages' one action: the card's width, thumb height on a phone.
  size?: "sm" | "md" | "block";
  // The accessible name, when the visible word is not enough on its own: a page carrying one Save
  // per section has four buttons reading "Save", and which one this is belongs in the name.
  label?: string;
  // A caller that has to move the keyboard onto this button — a confirmation taking the focus the
  // button it replaced was holding — reaches the element through here.
  ref?: Ref<HTMLButtonElement>;
  children: ReactNode;
}
// reason is not optional on a disabled button: the type makes a silent disabled button
// unrepresentable, which is D6 — the explanation is always visible and never a tooltip.
export type ButtonProps =
  | (Base & { disabled?: false; onClick?: () => void })
  | (Omit<Base, "variant"> & { disabled: true; reason: string; variant?: never });

// Weight belongs to the variant, never to the base: Tailwind resolves two utilities for one
// property by their order in the stylesheet, not by their order in the class attribute, so a base
// font-semibold would silently outrank ghost's font-medium.
const VARIANT = {
  primary: "border border-accent bg-accent text-accent-ink font-medium hover:brightness-110",
  secondary: "border border-border-strong bg-surface text-text font-medium hover:bg-surface-2",
  ghost: "border border-transparent text-text-dim font-medium hover:bg-surface-2",
  danger: "border border-critical bg-critical-dim text-critical font-medium",
};

const SIZE = { sm: "h-7 px-2.5 text-md", md: "h-[34px] px-3 text-base", block: "h-10 w-full px-4 text-base max-sm:h-[46px]" };
const base = (size: keyof typeof SIZE) => `inline-flex items-center justify-center gap-1.5 rounded-[7px] whitespace-nowrap ${SIZE[size]}`;

// A link that looks like the button: the sign-in page's one action is a navigation, not a click handler.
export const buttonClass = (variant: keyof typeof VARIANT, size: keyof typeof SIZE) => `${base(size)} ${VARIANT[variant]} no-underline`;

export function Button(props: ButtonProps) {
  const { variant = "secondary", size = "md", ref, label, children } = props;
  if (props.disabled === true) {
    return (
      <span className={`${size === "block" ? "flex w-full" : "inline-flex"} flex-col items-start gap-1`}>
        <button type="button" disabled ref={ref} aria-label={label} className={`${base(size)} cursor-not-allowed border border-border bg-surface-2 font-medium text-text-faint`}>
          {children}
        </button>
        <span className="text-md text-text-dim">{props.reason}</span>
      </span>
    );
  }
  return (
    <button type="button" onClick={props.onClick} ref={ref} aria-label={label} className={`${base(size)} ${VARIANT[variant]}`}>
      {children}
    </button>
  );
}
