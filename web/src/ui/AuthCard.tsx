import type { ReactNode } from "react";
import { AUTH } from "../strings";
import { initials } from "./Avatar";
import { Icon, Mark, Wordmark, type IconName } from "./Icon";
import type { Tone } from "./Pill";

// The card the sign-in, CLI approval and production approval pages share (design/specs/screens.md, A0):
// the mark above it, and on top of it the org with the address in the browser under it, read from the
// browser and never the server, so a person with two Canyon hosts open knows which one is asking. On a
// phone the card is the page: its edges drop away.
export function AuthCard({ org, wide = false, children }: { org: string | null; wide?: boolean; children: ReactNode }) {
  const width = wide ? "sm:max-w-[540px]" : "sm:max-w-[400px]";
  return (
    <main className="flex min-h-screen flex-col items-center max-sm:bg-surface sm:justify-center sm:gap-4 sm:px-4 sm:py-8">
      <div className={`flex w-full items-center gap-2 text-base font-semibold max-sm:px-5 max-sm:pt-4 ${width}`}><Mark /><Wordmark /></div>
      <div className={`grid w-full gap-5 bg-surface px-5 pt-4 pb-7 sm:rounded-2xl sm:border sm:border-border sm:p-7 sm:shadow-overlay ${width}`}>
        <div className="flex items-center gap-3 border-b border-border pb-5">
          {org !== null && <span aria-hidden="true" className="grid size-10 flex-none place-items-center rounded-xl bg-accent-dim font-semibold text-accent">{initials(org)}</span>}
          <div className="min-w-0">
            {org !== null && <p className="leading-snug font-semibold">{org}</p>}
            <p className="flex items-center gap-1.5 font-mono text-sm text-text-faint">{window.location.protocol === "https:" && <Icon name="locked" size={14} />}{window.location.host}</p>
          </div>
        </div>
        {children}
      </div>
    </main>
  );
}

const TINT: Record<Tone, string> = { critical: "bg-critical-dim", amber: "bg-amber-dim", ok: "bg-ok-dim", neutral: "bg-neutral-dim" };
const INK: Record<Tone, string> = { critical: "text-critical", amber: "text-amber", ok: "text-ok", neutral: "text-neutral" };

// A page's title and its line. A page part-way through something has an eyebrow over the title (the
// CLI's step, "Production approval"); a request that has ended has its icon over it instead.
export function AuthHead({ eyebrow, mark, title, line }: { eyebrow?: ReactNode; mark?: { tone: Tone; icon: IconName }; title: string; line?: ReactNode }) {
  return (
    <div className="grid justify-items-start gap-3.5">
      {mark !== undefined && <span className={`grid size-11 place-items-center rounded-xl ${TINT[mark.tone]} ${INK[mark.tone]}`}><Icon name={mark.icon} size={20} /></span>}
      <div className="grid gap-1.5">
        {eyebrow}
        <h1 className="text-title leading-tight font-semibold tracking-tight text-balance">{title}</h1>
        {line !== undefined && <p className="text-base text-text-dim">{line}</p>}
      </div>
    </div>
  );
}

export function Eyebrow({ icon, children }: { icon: IconName; children: ReactNode }) {
  return <p className="flex items-center gap-1.5 text-sm font-medium text-accent"><Icon name={icon} size={14} />{children}</p>;
}

// Signing in for the CLI is two pages: the sign-in, then the device page it comes back to.
export function CliStep({ step }: { step: 1 | 2 }) {
  return (
    <>
      <Eyebrow icon="terminal">{AUTH.cliStep.replace("<n>", String(step))}</Eyebrow>
      <div aria-hidden="true" className="mt-0.5 mb-1 flex gap-1">
        <i className="h-[3px] flex-1 rounded-xs bg-accent" />
        <i className={`h-[3px] flex-1 rounded-xs ${step === 2 ? "bg-accent" : "bg-border"}`} />
      </div>
    </>
  );
}

// Why a person is here, or what a click hands over: a title and a line in a tinted box, above the button.
export function AuthNote({ tone, icon, title, children }: { tone: Tone; icon: IconName; title: string; children?: ReactNode }) {
  return (
    <div className={`grid grid-cols-[16px_1fr] gap-2.5 rounded-lg px-3.5 py-3 text-md leading-snug ${TINT[tone]}`}>
      <span className={`mt-px ${INK[tone]}`}><Icon name={icon} /></span>
      <div>
        <strong className="block font-semibold text-text">{title}</strong>
        {children !== undefined && <span className="text-text-dim">{children}</span>}
      </div>
    </div>
  );
}
