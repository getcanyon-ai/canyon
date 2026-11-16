import { useEffect, useRef, useState } from "react";
import { caOf, rotateCA, said, type EgressRoot, type EgressSetting } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";

// Settings › Outbound's roots, an admin's (design/EGRESS.md §4.5, §8.3; contracts/routes.md, POST
// /api/v1/egress/ca/rotate; #2145): each root Canyon signs credentials' certificates under, its key dates, never a key;
// a rotation Canyon starts now; and an admin's own sub-CA in its place.
//
// The sub-CA's private key is never state: it is typed or pasted into an uncontrolled field, read from it once when the
// rotation sends it, and the field is emptied as soon as the server took it.

const FIELD = "w-full max-w-[520px] rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-md";
const LABEL = "mt-3 mb-1 block text-md font-medium";
const NOTE = "mt-1 text-md text-text-dim";
const DAY = 86_400_000;
// A date as stored, in UTC, wherever it is read: 11 Jan 2031.
const day = (at: string) => new Date(at).toLocaleDateString("en-GB", { day: "numeric", month: "short", year: "numeric", timeZone: "UTC" });

function Root({ root, signing }: { root: EgressRoot; signing: boolean }) {
  return (
    <li aria-label={root.byo ? S.keysByo : S.keysCanyon} className="grid gap-0.5 text-md">
      <span className="flex flex-wrap items-center gap-x-2">
        <span className="font-medium">{root.byo ? S.keysByo : S.keysCanyon}</span>
        <span className="font-mono text-text-dim" title={root.fingerprint}>{`SHA-256 ${root.fingerprint.slice(0, 16)}…`}</span>
        {signing && <span className="text-ok">{S.keysSigning}</span>}
      </span>
      <span className="text-text-dim">{S.keysDates.replace("<created>", () => day(root.created_at)).replace("<until>", () => day(root.not_after)).replace("<ends>", () => day(root.ends_at))}</span>
    </li>
  );
}

// `setting` is the admin's read Settings › Outbound holds, and `adopt` takes the read a rotation answers. `now` is when the
// page reads it: a sub-CA's end is warned of from 60 days before it.
export function OutboundKeys({ setting, adopt, now = Date.now() }: { setting: EgressSetting; adopt: (answer: EgressSetting) => void; now?: number }) {
  const ca = caOf(setting);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const cert = useRef<HTMLTextAreaElement>(null);
  const key = useRef<HTMLTextAreaElement>(null); // the sub-CA's key lives in the field alone
  const live = useRef(true);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  if (ca === null) return null; // a member's read, or a server from before credentials, names none
  const rotate = async (body: Record<string, never> | { cert: string; key: string }) => {
    if (busy) return;
    setBusy(true);
    setRefusal(null);
    setSaved(false);
    try {
      const answer = await rotateCA(body);
      if (key.current !== null) key.current.value = ""; // taken: the field holds it no longer
      if (live.current) { adopt(answer); setSaved(true); setAsking(false); }
    } catch (err) {
      if (live.current) setRefusal(said(err));
    }
    if (live.current) setBusy(false);
  };
  const newest = ca.roots[0];
  const byoEnding = newest?.byo === true && Date.parse(newest.ends_at) - now < 60 * DAY ? newest : null;
  return (
    <section aria-label={S.keysHeading} className="mt-6">
      <h2 className="text-base font-semibold">{S.keysHeading}</h2>
      <p className={NOTE}>{S.keysNote}</p>
      {ca.roots.length === 0 ? <p className={NOTE}>{S.keysNone}</p> : (
        <ul className="mt-2 grid gap-2">{ca.roots.map((root) => <Root key={root.fingerprint} root={root} signing={root.fingerprint === ca.signing} />)}</ul>
      )}
      {ca.rotates_at !== null && <p className={NOTE}>{S.keysRotatesAt.replace("<date>", () => day(ca.rotates_at ?? ""))}</p>}
      {byoEnding !== null && <p role="note" className="mt-1 text-md text-amber">{S.keysByoEnds.replace("<date>", () => day(byoEnding.ends_at))}</p>}
      {refusal !== null && <p role="alert" className="mt-2 text-md text-critical">{refusal}</p>}
      {saved && <p role="status" className="mt-2 text-md text-ok">{S.keysSaved}</p>}
      <div className="mt-2 flex flex-wrap items-center gap-2">
        {!asking ? <Button onClick={() => { setAsking(true); setSaved(false); }}>{S.keysRotate}</Button> : (<>
          <p className="basis-full text-base">{S.keysRotateAsk}</p>
          {busy ? <Button disabled reason="Replacing…">Replace</Button> : <Button variant="danger" onClick={() => void rotate({})}>Replace</Button>}
          <Button variant="ghost" onClick={() => setAsking(false)}>Cancel</Button>
        </>)}
      </div>
      <form aria-label={S.keysByoHeading} className="mt-6" onSubmit={(e) => { e.preventDefault(); }}>
        <h3 className="text-base font-semibold">{S.keysByoHeading}</h3>
        <p className={NOTE}>{S.keysByoNote}</p>
        <fieldset disabled={busy} className="contents">
          <label htmlFor="egress-ca-cert" className={LABEL}>Certificates: the sub-CA first, ending at your root</label>
          <textarea id="egress-ca-cert" ref={cert} rows={6} className={FIELD} spellCheck={false} autoComplete="off" />
          <label htmlFor="egress-ca-key" className={LABEL}>Private key (PKCS #8)</label>
          <textarea id="egress-ca-key" ref={key} rows={4} className={FIELD} spellCheck={false} autoComplete="off" aria-describedby="egress-ca-key-note" />
          <p id="egress-ca-key-note" className={NOTE}>{S.keysKeyNote}</p>
        </fieldset>
        <span className="mt-2 flex">
          {busy ? <Button disabled reason="Saving…">Use this sub-CA</Button>
            : <Button variant="primary" onClick={() => void rotate({ cert: cert.current?.value ?? "", key: key.current?.value ?? "" })}>Use this sub-CA</Button>}
        </span>
      </form>
    </section>
  );
}
