import { useEffect, useState } from "react";
import { getTelemetry, putTelemetry, said, type TelemetryState } from "../api";
import { S } from "../strings";
import { ago } from "../ui/Age";
import { Button } from "../ui/Button";

// Telemetry (#2807; contracts/routes.md, Telemetry): Settings › Telemetry, an admin's alone: a member's screen never asks
// the routes, which would answer 403. It is on by default, and no screen marks it seen (the owner, 2026-11-12).

const FIELDS = "https://www.getcanyon.ai/docs/org/usage-reports/";
const CARD = "rounded-xl border border-border bg-surface-2 px-3.5 py-3";

const OFF: Record<NonNullable<TelemetryState["off_because"]>, string> = {
  env: S.telemetryOffEnv, admin: S.telemetryOffAdmin, unreachable: S.telemetryOffUnreachable,
};

const day = (at: string, more: Intl.DateTimeFormatOptions = {}) => new Date(at).toLocaleString("en-GB", { day: "numeric", month: "short", ...more });

// retrying is the line while no report has reached the collector: the next try and the last day, in the reader's time.
const retrying = (next: string, until: string) =>
  S.telemetryRetrying.replace("<when>", () => day(next, { hour: "2-digit", minute: "2-digit" })).replace("<day>", () => day(until));

export const statusLine = (s: TelemetryState) => (s.enabled ? (s.next_try_at && s.gives_up_at ? retrying(s.next_try_at, s.gives_up_at) : S.telemetryOn) : s.off_because === undefined ? S.telemetryOffDone : OFF[s.off_because]);

// Report is a report exactly as the host's string holds the send's bytes, as canyon telemetry show prints it.
function Report({ sent }: { sent: string }) {
  return <pre className="mt-1 overflow-x-auto rounded-lg bg-surface px-2.5 py-2 font-mono text-sm whitespace-pre-wrap break-all">{sent}</pre>;
}

// TelemetrySettings is Settings › Telemetry: on or off and why, the last report sent and when, Turn off or Turn on,
// and the next report exactly as it is sent.
export function TelemetrySettings() {
  const [state, setState] = useState<TelemetryState | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const load = () => getTelemetry().then(setState, (err: unknown) => setFailure(said(err)));
  useEffect(() => { void load(); }, []);
  if (state === null) return failure === null ? null : <p className="text-base text-critical">{failure}</p>;
  const set = (enabled: boolean) => putTelemetry(enabled).then((next) => { setState(next); setFailure(null); }, (err: unknown) => setFailure(said(err)));
  const canTurnOn = !state.enabled && (state.off_because === "admin" || state.off_because === "unreachable");
  return (
    <section aria-label="Telemetry" className="grid gap-3">
      <p className="text-base">{statusLine(state)}</p>
      <div className="flex flex-wrap gap-2">
        {state.enabled && <Button variant="ghost" size="sm" onClick={() => void set(false)}>{S.telemetryTurnOff}</Button>}
        {canTurnOn && <Button variant="ghost" size="sm" onClick={() => void set(true)}>{S.telemetryTurnOn}</Button>}
      </div>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      <div className={CARD}>
        <p className="text-md text-text-dim">{S.telemetryNext}</p>
        <Report sent={state.report} />
      </div>
      <div className={CARD}>
        {state.last_sent_at === null || state.last_report === null
          ? <p className="text-md text-text-dim">{S.telemetryNotSent}</p>
          : <><p className="text-md text-text-dim">{S.telemetryLast.replace("<when>", () => ago(state.last_sent_at))}</p><Report sent={state.last_report} /></>}
      </div>
      <p className="text-md text-text-dim"><a className="text-link" href={FIELDS}>{S.telemetryFields}</a></p>
    </section>
  );
}
