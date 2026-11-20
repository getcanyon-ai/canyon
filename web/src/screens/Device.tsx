import { useEffect, useState, type FormEvent } from "react";
import { approveDevice, denyDevice, getDeviceGrant, said, signOut, type ApiError, type DeviceGrant } from "../api";
import { AUTH } from "../strings";
import { ApprovingAs } from "../ui/ApprovingAs";
import { AuthCard, AuthHead, AuthNote, CliStep } from "../ui/AuthCard";
import { Button } from "../ui/Button";

// The device approval screen (design/specs/screens.md): the page `canyon login` sends a person to.
// A code in ?code= fills the field and is looked up, which is a read; only the click approves
// (design/AUTH.md, device-code phishing: no single link may complete a sign-in). Once looked up, the
// field folds into the request: the code, the address and the time are the one fact that tells a
// person the code is not theirs, and the account named under them is whose access approving hands
// over: Switch account signs out, and the sign-in page that follows asks the provider which account
// and comes back to this code. A 404 says this screen's own line, never the server's: that sentence
// is written for the CLI's terminal.
const FIELD = "h-[52px] w-full rounded-lg border border-border-strong bg-surface text-center font-mono text-title tracking-[.18em] placeholder:text-text-faint focus:border-accent aria-invalid:border-critical";
export const ROW = "border-t border-border px-3.5 py-2 first-of-type:border-t-0";

// "14:22 today" as the mockup has it; a code lives ten minutes, so another day is only midnight.
export function requestedAt(at: string): string {
  const when = new Date(at);
  const clock = when.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return when.toDateString() === new Date().toDateString() ? `${clock} today` : `${clock} on ${when.toLocaleDateString()}`;
}

export function Device({ code: prefilled, org }: { code: string; org: string | null }) {
  const [code, setCode] = useState(prefilled);
  const [read, setRead] = useState<{ code: string; grant: DeviceGrant } | null>(null); // the grant with the code it was read for: a slow read landing after the field changed must not put another code's address above the button
  const grant = read !== null && read.code === code ? read.grant : null;
  const [missing, setMissing] = useState(false);
  const [changing, setChanging] = useState(false);
  const [outcome, setOutcome] = useState<"approved" | "denied" | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  // A 404 is the screen's line. Anything else is the call's own failure, as the fleet shows one; a
  // 401 has already replaced this screen with the sign-in page by the time it lands here.
  const gone = (err: unknown) => {
    setRead(null);
    if ((err as ApiError).status === 404) setMissing(true);
    else setFailure(said(err)); // the server's own line, never a developer string carrying the code
  };
  const lookUp = (value: string) => getDeviceGrant(value).then((found) => setRead({ code: value, grant: found }), gone);
  useEffect(() => { if (prefilled !== "") void lookUp(prefilled); }, [prefilled]);

  const submit = (event: FormEvent) => { event.preventDefault(); void lookUp(code); };
  const decide = (send: (code: string) => Promise<void>, result: "approved" | "denied") => () =>
    read === null ? Promise.resolve() : send(read.code).then(() => setOutcome(result), gone); // the code the shown address belongs to
  const failed = failure !== null && <p className="text-md text-critical">{failure}</p>;

  if (outcome !== null) {
    return (
      <AuthCard org={org}>
        {outcome === "approved"
          ? <AuthHead mark={{ tone: "ok", icon: "done" }} title={AUTH.deviceApproved} line={AUTH.deviceApprovedLine} />
          : <AuthHead mark={{ tone: "neutral", icon: "close" }} title={AUTH.deviceDenied} line={AUTH.deviceDeniedLine} />}
      </AuthCard>
    );
  }
  return (
    <AuthCard org={org}>
      <form onSubmit={submit} className="grid gap-5">
        <AuthHead eyebrow={<CliStep step={2} />} title={AUTH.deviceTitle} line={grant === null ? AUTH.deviceEnter : AUTH.deviceCheck} />
        {grant === null
          ? (
            <>
              <label className="grid gap-1.5">
                <span className="text-md font-medium">{AUTH.deviceCode}</span>
                <input className={FIELD} value={code} placeholder="K7QP-4RXM" autoFocus={prefilled === "" || changing} aria-invalid={missing}
                  onChange={(event) => { setCode(event.target.value); setRead(null); setMissing(false); setFailure(null); }} />
              </label>
              {missing && <AuthNote tone="critical" icon="alert" title={AUTH.deviceUnknown}>{AUTH.deviceUnknownLine}</AuthNote>}
              {failed}
              <Button variant="primary" size="block" onClick={() => void lookUp(code)}>{AUTH.deviceContinue}</Button>
            </>
          )
          : (
            <>
              <section aria-label="The request">
                <dl className="grid grid-cols-[auto_1fr] rounded-lg border border-border text-md [&>dd]:text-right [&>dd]:font-medium [&>dt]:text-text-dim">
                  <dt className={ROW}>{AUTH.deviceCode}</dt><dd className={`${ROW} font-mono`}>{code}</dd>
                  <dt className={ROW}>{AUTH.deviceFrom}</dt><dd className={`${ROW} font-mono`}>{grant.requested_by}</dd>
                  <dt className={ROW}>{AUTH.deviceWhen}</dt>
                  <dd className={ROW}>
                    <span>{requestedAt(grant.requested_at)}</span>{" · "}
                    <button type="button" className="font-medium text-link hover:underline" onClick={() => { setRead(null); setChanging(true); }}>{AUTH.deviceChange}</button>
                  </dd>
                </dl>
              </section>
              <AuthNote tone="amber" icon="alert" title={AUTH.deviceOnlyYours}>{AUTH.deviceHandsOver}</AuthNote>
              <ApprovingAs approver={grant.approver} onSwitch={() => void signOut().catch(gone)} />
              <div className="grid gap-2.5 sm:grid-cols-2">
                <Button variant="primary" size="block" onClick={() => void decide(approveDevice, "approved")()}>{AUTH.deviceApprove}</Button>
                <Button size="block" onClick={() => void decide(denyDevice, "denied")()}>{AUTH.deviceDeny}</Button>
              </div>
              {failed}
            </>
          )}
      </form>
    </AuthCard>
  );
}
