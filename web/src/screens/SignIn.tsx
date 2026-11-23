import { useEffect, useState } from "react";
import { getAuthConfig } from "../api";
import { AUTH, S } from "../strings";
import { AuthCard, AuthHead, AuthNote, CliStep } from "../ui/AuthCard";
import { buttonClass } from "../ui/Button";
import { Icon } from "../ui/Icon";

// The pages a sign-in comes back to (design/specs/screens.md, A0): the CLI's device page, an MCP client's consent page and the
// production approval page, each with its code or request, an app's no-access page, and what a link may name: an app, the
// trail, a Guardrails or Settings page, with its query. Kept in this tab for the length of the sign-in, and only ever a
// hash of this dashboard, so a kept value can send the browser nowhere else: each page is spelt out, and a link's query
// is the characters a URL's query has and no longer than a link of ours.
const RETURN = "canyon_return";
const APP = "[a-z]([a-z0-9-]*[a-z0-9])?";
const QUERY = "(\\?[A-Za-z0-9._~%&=+:,@/*-]{0,512})?";
const step = new RegExp(`^#/((device|approve|oauth)(\\?[^#]*)?|no-access/${APP})$`);
const linked = new RegExp(`^#/(apps/${APP}(/(outbound|secrets))?|audit|(guardrails|settings)(/[a-z-]{1,40})?)${QUERY}$`);
const returnable = (hash: string) => step.test(hash) || linked.test(hash);

// A browser that refuses storage (a private window) signs in all the same and lands on the fleet. A sign-in begun on any
// other page forgets what an earlier one kept; trying a refused one again doesn't. After a sign-out the person chose, a
// step of signing in is still come back to (the device page's Switch account), a page someone was reading is not: the
// next person to sign in here may be another.
function keep(hash: string, reason: Reason | undefined) {
  try {
    if (step.test(hash) || (reason !== "signed-out" && linked.test(hash))) sessionStorage.setItem(RETURN, hash);
    else if (reason !== "failed") sessionStorage.removeItem(RETURN);
  } catch { /* the fleet is where a sign-in lands without it */ }
}

// takeReturn is the page a sign-in began on, read once and cleared, or null.
export function takeReturn(): string | null {
  try {
    const hash = sessionStorage.getItem(RETURN);
    sessionStorage.removeItem(RETURN);
    return hash !== null && returnable(hash) ? hash : null;
  } catch {
    return null; // nothing could be kept, so there is nothing to come back to
  }
}

// A0 (design/specs/screens.md): the org's card, one button to /auth/start that never names the
// provider, and a box saying why the person is here when there is a reason. A refused sign-in is
// redirected here by the callback, and reaching #/signin as a route is the whole of what the screen is
// told: the redirect carries no query and no detail (contracts/routes.md, the callback), so the box
// names no reason. After a sign-out the person chose, the box says the company account is still signed
// in and the button asks the provider which account, since the provider's own session outlives
// Canyon's (design/AUTH.md §4.1).
type Reason = "failed" | "signed-out" | "ended";
const NOTES = {
  failed: { tone: "critical", icon: "alert", title: AUTH.signInFailed, line: AUTH.signInFailedLine },
  "signed-out": { tone: "neutral", icon: "info", title: AUTH.signedOut, line: AUTH.signedOutLine },
  ended: { tone: "neutral", icon: "waiting", title: AUTH.sessionEnded, line: AUTH.sessionEndedLine },
} as const;

export function SignIn({ reason }: { reason?: Reason }) {
  const [org, setOrg] = useState<string | null>(null);
  const [opening, setOpening] = useState(false);
  useEffect(() => {
    let live = true;
    void getAuthConfig().then((config) => { if (live) setOrg(config.org); });
    // Back from the provider restores this page from the browser's cache: it is not opening anything.
    const back = () => setOpening(false);
    window.addEventListener("pageshow", back);
    return () => { live = false; window.removeEventListener("pageshow", back); };
  }, []);
  const cli = window.location.hash.startsWith("#/device");
  const agent = window.location.hash.startsWith("#/oauth");
  const note = reason === undefined ? null : NOTES[reason];
  const line = cli ? S.signInForCliLine : agent ? S.signInForAgentLine : AUTH.signInLine.replace("<org> ", () => (org === null ? "" : `${org} `));
  return (
    <AuthCard org={org}>
      <AuthHead eyebrow={cli ? <CliStep step={1} /> : undefined} title={cli ? S.signInForCli : agent ? S.signInForAgent : AUTH.signIn} line={note === null ? line : undefined} />
      {note !== null && <AuthNote tone={note.tone} icon={note.icon} title={note.title}>{note.line}</AuthNote>}
      <a className={buttonClass("primary", "block")} href={reason === "signed-out" ? "/auth/start?prompt=select_account" : "/auth/start"} onClick={() => { keep(window.location.hash, reason); setOpening(true); }}>
        {opening ? <><span className="animate-spin"><Icon name="working" /></span>{AUTH.signingIn}</> : reason === "failed" ? AUTH.tryAgain : AUTH.signInGo}
      </a>
      {reason !== "signed-out" && <p className="flex items-center gap-1.5 text-md text-text-faint">{window.location.protocol === "https:" && <Icon name="locked" size={14} />}{AUTH.noPassword}</p>}
    </AuthCard>
  );
}
