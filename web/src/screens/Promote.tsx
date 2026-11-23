import { useEffect, useRef, useState } from "react";
import { cancelRun, getTimeline, listSecrets, said, type ApiError, type Env, type Hold, type Need, type Receipt as ReceiptRead } from "../api";
import { S } from "../strings";
import { oddAnswer } from "../ui/answered";
import { Button } from "../ui/Button";
import { heldLine } from "../ui/Headline";
import { codeOnly } from "../ui/Need";

// The TS twin of --ds-motion-delay-unlock. The checklist row flips first and the button fills
// this long after, so cause visibly precedes effect (D6). App.test.tsx holds the two equal.
export const UNLOCK_MS = 160;

// Why Promote is disabled: a held production first, whose receipt is not evaluated (D6). The hold is said from the
// page's own read of it where the page has one (`hold`, the status read's), which says whether production ran anything
// when it was held; the receipt's carries no version, and says stopped (#2696).
export const blockedBy = (receipt: ReceiptRead, hold?: Hold): string | null =>
  receipt.held !== undefined ? heldLine(hold ?? receipt.held) : receipt.waiting ?? (receipt.blocked_by.length === 0 ? null : receipt.rules.find((rule) => rule.id === receipt.blocked_by[0])?.why ?? "");

// Why Promote is disabled, as its button says it: a pull request's version, a held production in brief (the page's banner
// says it whole, #2723), what production doesn't allow of what preview calls, else blockedBy. The go-live box says a
// waiting receipt's line once, through this (#2774).
export const promoteReason = (receipt: ReceiptRead, hold: Hold | undefined, refuses: Need | undefined, pullRequest: boolean): string | null => {
  const egress = receipt.blocked_by[0] === "prod.egress-allowed" && refuses !== undefined ? refusing(refuses, S.needWillRefuseAllowable).join(" ") : "";
  return pullRequest ? S.prPreview : receipt.held !== undefined ? S.heldNoPromote : egress !== "" ? egress : blockedBy(receipt, hold);
};

// What production will refuse, from the status read's will_refuse need (contracts/routes.md, *What needs a person*), never
// from the rule's own line: the host names a person can allow, then what only a change to the code fixes. `allowable` is
// said of the host names (Production doesn't allow …, or will refuse … once it goes live); [] where the need names none.
export const refusing = (need: Need | undefined, allowable: string): string[] => {
  const denied = need?.denied ?? [];
  const hosts = denied.filter((one) => one.allowable).map((one) => one.destination);
  const code = codeOnly(denied);
  return [...(hosts.length > 0 ? [allowable.replace("<destinations>", () => hosts.join(", "))] : []), ...(code.length > 0 ? [S.needWillRefuseCode.replace("<destinations>", () => code.join(", "))] : [])];
};

// What a cancel says: at once for a pending run, after the step in flight for a running one, and
// never once production is switching. What a cancelled run had made stays: the next promotion keeps
// a ready resource and asks again, under the same name, for one still being made (runs/pipeline.go).
export const CANCEL = {
  done: "Cancelled. Nothing was changed.",
  cancelling: (step?: string) => `Cancelling after the current step${step === undefined ? "" : ` (${step})`}…`,
  tooLate: "Too late to cancel: production is already switching to this version.",
  kept: "Cancelled. Production was not changed. What was already made stays, and the next promotion uses it.",
};

// The promote run's build.log lines as the server writes them, read until the run ends (next is null),
// when ended re-reads production: they end on `live at <url>` or on the reason it failed. While the
// run goes it can be cancelled.
export function PromotionProgress({ run, ended }: { run: string; ended: () => void }) {
  const [lines, setLines] = useState<string[]>([]);
  const [failure, setFailure] = useState<string | null>(null);
  const [going, setGoing] = useState(true);
  const [asked, setAsked] = useState(false);
  const [note, setNote] = useState<string | null>(null);
  const [waiting, setWaiting] = useState(false); // a running run was asked to end: its interrupted end is the cancel landing
  const [interrupted, setInterrupted] = useState(false);
  const end = useRef(ended); // the page's latest refresh, without restarting the read on every render
  useEffect(() => { end.current = ended; }, [ended]);
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout>;
    const read = async (after: number) => {
      try {
        const page = await getTimeline(run, after);
        if (!live) return;
        const logged = page.rows.filter((row) => row.kind === "build.log").map((row) => (row.data as { line: string }).line);
        setLines((at) => [...at, ...logged]);
        if (page.rows.some((row) => row.kind === "run.state" && (row.data as { failure_reason?: string }).failure_reason === "interrupted")) setInterrupted(true);
        if (page.next === null) { setGoing(false); end.current(); }
        else timer = setTimeout(() => void read(page.next!), page.rows.length > 0 ? 0 : PROGRESS_MS); // a page with rows means the next may be there already
      } catch (err) { if (live) setFailure((err as Error).message); }
    };
    void read(0);
    return () => { live = false; clearTimeout(timer); };
  }, [run]);
  const cancel = async () => {
    setAsked(true);
    try {
      const answer = await cancelRun(run);
      setWaiting(answer.run.state !== "failed");
      setNote(answer.run.state === "failed" ? CANCEL.done : CANCEL.cancelling(lines.at(-1)));
    } catch (err) {
      const code = ((err as ApiError).body as { error?: string } | null)?.error;
      if (code !== "ended") setNote(code === "too_late" ? CANCEL.tooLate : said(err)); // ended: its last lines say how
    }
  };
  return (
    <>
      <ul aria-label="Promotion" className="mt-2 font-mono text-md text-text-dim">{lines.map((line, at) => <li key={at}>{line}</li>)}</ul>
      {going && !asked && <span className="mt-2 inline-flex"><Button variant="ghost" size="sm" onClick={() => void cancel()}>Cancel promotion</Button></span>}
      {note !== null && <p aria-live="polite" className="mt-2 text-base text-text-dim">{waiting && interrupted ? CANCEL.kept : note}</p>}
      {failure !== null && <p className="mt-2 text-base text-critical">{failure}</p>}
    </>
  );
}

const PROGRESS_MS = 1000; // clients poll about once a second while a run is going (contracts/events.md)

export function Promote({ id, shown, live, receipt, hold, answered, gateRun, send, ended, pullRequest = false, refuses, quiet = false, setSecret, secretsSet = 0 }: {
  id: string;
  // The page's own read of a hold (the status read's), which says whether production ran anything when it was held.
  hold?: Hold;
  // The version preview runs, which is what a promotion ships: the button names it.
  shown: string | null;
  // Production already runs it: a promotion would change nothing, so the panel says so in its place.
  live: boolean;
  receipt: ReceiptRead;
  // What preview answered at /: an error is said in the question, whose button is then the override.
  answered: Env["answered"];
  gateRun: string | null;
  send: (gateRun: string | null, anyway: boolean) => Promise<string | null>;
  ended: () => void;
  pullRequest?: boolean; // preview runs a pull request's version, which is never promoted (#1947)
  refuses?: Need; // the status read's will_refuse: what production doesn't allow of what preview calls
  quiet?: boolean; // a newer version is blocked: fixing it is the page's step, so this one isn't drawn as the main one (#102)
  // Sets a runtime secret production lacks, on this page, the question kept open; secretsSet moves when one was set (#102).
  setSecret?: (name: string) => void;
  secretsSet?: number;
}) {
  const blocked = promoteReason(receipt, hold, refuses, pullRequest);
  const [was, setWas] = useState(blocked);
  const [held, setHeld] = useState<string | null>(null);
  const [asking, setAsking] = useState(false);
  const [landed, setLanded] = useState(false);
  const [following, setFollowing] = useState<string | null>(null);
  // What production lacks, read when the question is asked (#1698): runtime values set for preview and not
  // for production, which runs preview's image with its own. A resource's value the promotion makes.
  // null while it is read: the question is not answered before what it names has been said.
  const [lacks, setLacks] = useState<string[] | Error | null>(null);
  const goLive = useRef<HTMLButtonElement>(null);
  const confirming = useRef<HTMLButtonElement>(null);
  // What production lacks, read when the question is asked and again whenever a secret is set beside it.
  const readLacks = () => {
    setLacks(null);
    listSecrets(id).then(({ secrets }) => {
      const prod = new Set(secrets.filter((one) => one.env === "prod" && one.where === "runtime").map((one) => one.name));
      setLacks(secrets.filter((one) => one.env === "preview" && one.where === "runtime" && one.origin === "user" && !prod.has(one.name)).map((one) => one.name));
    }, (err: Error) => setLacks(err));
  };
  const askingNow = useRef(false);
  askingNow.current = asking;
  useEffect(() => { if (askingNow.current && secretsSet > 0) readLacks(); }, [secretsSet]); // a set secret, read again while the question is open

  // Adjusted during render, never in an effect: StrictMode's double invoke would spend the hold
  // before the browser painted the disabled button, and the tests would still pass.
  if (was !== blocked) {
    setWas(blocked);
    setHeld(blocked === null ? was : null);
    if (blocked !== null) setAsking(false);
  }
  useEffect(() => {
    if (held === null) return;
    const timer = setTimeout(() => setHeld(null), UNLOCK_MS);
    return () => clearTimeout(timer);
  }, [held]);

  // The question replaces the button that asked it, so the keyboard follows the question, and
  // comes back to the button when the question closes. Without this the focus falls to the
  // document body and the next Enter does nothing. `asked` keeps a first render from taking it.
  const asked = useRef(false);
  useEffect(() => {
    if (asking) { asked.current = true; confirming.current?.focus(); } // again once the confirm appears, after the secrets read
    else if (asked.current) { asked.current = false; goLive.current?.focus(); }
  }, [asking, lacks === null]);

  const label = shown === null ? "Promote to production" : `Promote ${shown} to production`;
  const odd = oddAnswer(answered);
  // Holding the reason that has just gone is why the line doesn't blink blank for 160ms.
  const why = blocked ?? held;
  const ask = async () => {
    const run = await send(gateRun, odd !== null);
    setAsking(false);
    if (run !== null) setLanded(true);
    if (run) setFollowing(run);
  };

  return (
    <>
      {live && shown !== null
        ? <p className="text-md text-text-dim">{S.inProduction.replace("<version>", () => shown)}</p>
        : why !== null
        ? <Button disabled reason={why}>{label}</Button>
        : asking
          ? (
            <div className="flex flex-col items-start gap-1.5">
              <p className="text-md text-text-dim">{S.promoteConfirm}</p>
              {odd !== null && <p className="text-md text-amber">{`${shown ?? "This version"} ${odd} on preview. Check the app before promoting it.`}</p>}
              {refusing(refuses, S.willRefuse.replace("<version>", () => shown ?? "this version")).map((line) => <p key={line} className="text-md text-amber">{line}</p>)}
              {lacks instanceof Error ? <p className="text-md text-amber">{`Couldn't check production's secrets: ${lacks.message}`}</p>
                : (lacks ?? []).map((name) => (
                  <p key={name} className="text-md text-amber">
                    {`${S.prodLacks.replace("<name>", () => name)} ${S.prodLacksWhy.replace("<version>", () => shown ?? "this version")} `}
                    {setSecret === undefined
                      ? <a className="text-link no-underline hover:underline" href={`#/apps/${encodeURIComponent(id)}/secrets?name=${encodeURIComponent(name)}&env=prod`}>{S.prodLacksSet}</a>
                      : <button type="button" className="text-link hover:underline" onClick={() => setSecret(name)}>{S.prodLacksSet}</button>}
                  </p>
                ))}
              <span className="flex flex-wrap gap-2">
                {lacks === null ? <span className="text-md text-text-dim">Checking production's secrets…</span>
                  : <Button ref={confirming} variant={odd !== null ? "secondary" : "primary"} onClick={() => void ask()}>{odd !== null ? "Promote anyway" : shown === null ? "Promote" : `Promote ${shown}`}</Button>}
                <Button variant="ghost" onClick={() => setAsking(false)}>Cancel</Button>
              </span>
            </div>
          )
          : <Button ref={goLive} variant={quiet ? "secondary" : "primary"} onClick={() => {
            setAsking(true);
            readLacks();
          }}>{label}</Button>}
      {following !== null && <PromotionProgress key={following} run={following} ended={ended} />}
      {landed && (
        <p role="status" className="fixed right-4 bottom-4 flex items-center gap-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3 text-base">
          {S.promoted.replace("<app>", () => id)}
          <Button variant="ghost" size="sm" onClick={() => { setLanded(false); goLive.current?.focus(); }}>Dismiss</Button>
        </p>
      )}
    </>
  );
}
