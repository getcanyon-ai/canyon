import { type KeyboardEvent, useEffect, useRef, useState } from "react";
import {
  getPlatformRepo, getPlatformWebhook, pingPlatformWebhook, putPlatformWebhook, putPreferences, replaceHostToken, replaceWebhookSecret, said,
  type FrontDoorRead, type PlatformRepo, type PlatformWebhook, type Preferences,
} from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age, ago } from "../ui/Age";
import { Button } from "../ui/Button";
import { OVER, OVER_BOX } from "../ui/Modal";
import { RepoStep, RepoTeamCard, type RepoRevealed } from "./HandOffRepo";

const LABEL = "mt-3 mb-1 block text-xs tracking-caps text-text-faint uppercase";
const FIELD = "w-full max-w-[520px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-1 text-md text-text-dim";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// The values a save or a replace answered, once: held here and in the dialog alone, never stored, never logged.
type Revealed = { secret?: string; token?: string } & RepoRevealed;
type Asking = "on" | "off" | "token" | "secret";

// What a test delivery's status tells the admin (#2439): reachable, the secret to check, the redirect not followed, or no answer.
export function pinged(status: number): string {
  if (status === 0) return S.pingNone;
  const answered = S.pingAnswered.replace("<status>", String(status));
  const hint = status >= 200 && status < 300 ? S.pingReachable : status === 401 || status === 403 ? S.pingSecret : status >= 300 && status < 400 ? S.pingRedirect : "";
  return hint === "" ? answered : `${answered} ${hint}`;
}

// Where apps run (design/HAND-OFF.md §1; #2439), above the front door on a Kubernetes host: Canyon runs them, or the org's
// platform does. An admin picks the platform and follows a checklist — the server's settings, the webhook, a front door
// at choice 1, then the switch — every step read from the server: the front door's read, the webhook's (null, none saved)
// and the preferences. The checklist only shows the way; the server decides, and a refusal is shown where it was asked.
// A member reads the choice, the webhook and the platform team's card, and changes nothing.
export function HandOff({ read, record, admin, adopt, reload, toFrontDoor }: {
  read: FrontDoorRead; record: Preferences; admin: boolean;
  adopt: (record: Preferences) => void; reload: () => Promise<unknown>; toFrontDoor: () => void;
}) {
  const [hook, setHook] = useState<PlatformWebhook | null | undefined>(undefined);
  const [repo, setRepo] = useState<PlatformRepo | null | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  const [picked, setPicked] = useState<"canyon" | "org" | null>(null);
  const [revealed, setRevealed] = useState<Revealed | null>(null);
  const [asking, setAsking] = useState<Asking | null>(null);
  const [refusal, setRefusal] = useState<{ at: Asking | "url" | "grace" | "delivery"; message: string } | null>(null);
  const [url, setUrl] = useState("");
  const [editing, setEditing] = useState(false);
  const [ping, setPing] = useState<string | "sending" | null>(null);
  const [grace, setGrace] = useState(record.access_grace);
  const heading = useRef<HTMLHeadingElement>(null);
  const tester = useRef<HTMLButtonElement>(null);
  const afterReveal = useRef<"heading" | "tester">("heading");
  const live = useRef(true);

  const readHook = () => getPlatformWebhook().then(
    (answer) => { if (live.current) { setHook(answer); setFailure(null); } },
    (err: unknown) => { if (live.current) setFailure(said(err)); },
  );
  const readRepo = () => getPlatformRepo().then(
    (answer) => { if (live.current) setRepo(answer); },
    (err: unknown) => { if (live.current) setFailure(said(err)); },
  );
  useEffect(() => { live.current = true; void readHook(); void readRepo(); return () => { live.current = false; }; }, []);
  useEffect(() => { setGrace(record.access_grace); }, [record.access_grace]);

  const org = record.run_by === "org";
  const viaRepo = record.delivery === "repo";
  const repoSaved = repo !== null && repo !== undefined;
  const choice = picked ?? (org ? "org" : "canyon"); // run_by's, never what is saved: the admin's choice reads as the member's line does (#2560)
  const steps = [read.hand_off_needs.length === 0, viaRepo ? repoSaved && repo.checked_at !== null : hook !== null && hook !== undefined,
    read.front_door?.choice === 1 && read.front_door.enforcing]; // a door not seen enforcing NetworkPolicy builds nothing (#2507)
  const current = steps.indexOf(false);

  const fail = (at: Asking | "url" | "grace" | "delivery", err: unknown) => setRefusal({ at, message: said(err) });
  const choose = (to: "canyon" | "org") => {
    setRefusal(null);
    if (to === "canyon" && org) setAsking("off");
    else { setPicked(to); setAsking(null); }
  };
  const switchTo = async (to: "canyon" | "org") => {
    try {
      adopt(await putPreferences({ run_by: to }));
      setAsking(null);
      setPicked(null);
      heading.current?.focus();
    } catch (err) {
      fail(to === "org" ? "on" : "off", err);
      await Promise.allSettled([reload(), readHook()]); // the server's state moved under the checklist: read every step again
    }
  };
  const saveUrl = async () => {
    try {
      const saved = await putPlatformWebhook(url);
      setEditing(false);
      setUrl("");
      setRefusal(null);
      if (saved.secret !== undefined || saved.token !== undefined) {
        afterReveal.current = "heading";
        setRevealed({ secret: saved.secret, token: saved.token });
      }
      await readHook();
    } catch (err) {
      fail("url", err);
    }
  };
  const replace = async (what: "token" | "secret") => {
    try {
      const answer = what === "token" ? { token: (await replaceHostToken()).token } : { secret: (await replaceWebhookSecret()).secret };
      setAsking(null);
      afterReveal.current = what === "secret" ? "tester" : "heading"; // a new secret is checked next: the platform refuses releases until it has it
      setRevealed(answer);
    } catch (err) {
      fail(what, err);
    }
  };
  const sendPing = async () => {
    if (ping === "sending") return;
    setPing("sending");
    try {
      setPing(pinged((await pingPlatformWebhook()).status));
    } catch (err) {
      setPing(said(err));
    }
  };
  const pickDelivery = async (to: "webhook" | "repo") => {
    try {
      adopt(await putPreferences({ delivery: to }));
      setRefusal(null);
    } catch (err) {
      fail("delivery", err);
      await reload();
    }
  };
  const saveGrace = async () => {
    try {
      adopt(await putPreferences({ access_grace: grace }));
      setRefusal(null);
    } catch (err) {
      fail("grace", err);
      await reload();
    }
  };
  const done = () => {
    setRevealed(null);
    requestAnimationFrame(() => (afterReveal.current === "tester" ? tester.current : heading.current)?.focus());
  };
  const refused = (at: Asking | "url" | "grace" | "delivery") => refusal?.at === at && <p role="alert" className="mt-1 text-base text-critical">{refusal.message}</p>;
  const confirm = (at: Asking, question: string, yes: string, go: () => void) => asking === at && (
    <div className={CARD}>
      <p className="text-base">{question}</p>
      <span className="mt-2 flex flex-wrap gap-2">
        <Button variant="primary" onClick={go}>{yes}</Button>
        <Button variant="ghost" onClick={() => setAsking(null)}>Cancel</Button>
      </span>
    </div>
  );

  const last = hook?.last_delivery;
  const delivered = last === null || last === undefined
    ? <p className={NOTE}>{org ? S.lastNoneOn : S.lastNone}</p>
    : <p className={NOTE}>{last.status === 0 ? S.lastNoAnswer : S.lastDelivery.replace("<status>", String(last.status))} · {ago(last.sent_at)}</p>;
  const urlForm = (label: string) => (
    <>
      <label className={LABEL} htmlFor="hand-off-url">Webhook URL</label>
      <input id="hand-off-url" className={FIELD} value={url} placeholder="https://" onChange={(event) => setUrl(event.target.value)} />
      <p className={NOTE}>{S.webhookNote}</p>
      <span className="mt-2 flex flex-wrap gap-2">
        <Button variant="primary" label={label} onClick={() => void saveUrl()}>Save</Button>
        {editing && <Button variant="ghost" onClick={() => setEditing(false)}>Cancel</Button>}
      </span>
      {refused("url")}
    </>
  );
  const webhookControls = hook && (
    <>
      <p className="mt-1 font-mono text-sm break-all">{hook.url}</p>
      <p className={NOTE}><Actor actor={hook.set_by} /> · <Age at={hook.set_at} /></p>
      {delivered}
      {admin && (
        editing ? urlForm("Save the new webhook URL") : (
          <>
            <span className="mt-2 flex flex-wrap gap-2">
              <button ref={tester} type="button" onClick={() => void sendPing()} className="inline-flex h-[34px] items-center rounded-[7px] border border-border-strong bg-surface px-3 text-base font-medium">
                {ping === "sending" ? "Sending…" : "Send a test delivery"}
              </button>
              <Button variant="ghost" onClick={() => { setEditing(true); setUrl(hook.url); }}>Change URL</Button>
              <Button variant="ghost" onClick={() => { setRefusal(null); setAsking("token"); }}>Replace the host token</Button>
              <Button variant="ghost" onClick={() => { setRefusal(null); setAsking("secret"); }}>Replace the webhook secret</Button>
            </span>
            <p role="status" className="mt-1 text-base">{ping !== null && ping !== "sending" ? ping : ""}</p>
            {confirm("token", S.tokenAsk, "Replace the host token", () => void replace("token"))}
            {refused("token")}
            {confirm("secret", S.webhookSecretAsk, "Replace the webhook secret", () => void replace("secret"))}
            {refused("secret")}
          </>
        )
      )}
    </>
  );
  const team = hook && (
    <section aria-labelledby="hand-off-team" className={CARD}>
      <h3 id="hand-off-team" className="text-base font-semibold">{S.teamTitle}</h3>
      <p className={NOTE}>{S.teamWebhook.replace("<url>", () => hook.url)}</p>
      <p className={NOTE}>{S.teamReport}</p>
      <p className={NOTE}>{S.teamDocs}</p>
    </section>
  );
  const deliveryChoice = admin ? (
    <fieldset className="mt-3">
      <legend className="text-base font-semibold">{S.deliveryLegend}</legend>
      {([["repo", S.deliveryRepo, S.deliveryRepoNote], ["webhook", S.deliveryWebhook, S.deliveryWebhookNote]] as const).map(([value, word, note]) => (
        <div key={value} className="mt-2">
          <label className="flex items-center gap-2 text-base">
            <input type="radio" name="delivery" checked={record.delivery === value} aria-describedby={`delivery-${value}-note`} onChange={() => void pickDelivery(value)} />
            {word}
          </label>
          <p id={`delivery-${value}-note`} className={`${NOTE} ml-6`}>{note}</p>
        </div>
      ))}
      {refused("delivery")}
    </fieldset>
  ) : <p className="mt-2 text-base">{viaRepo ? S.deliveryRepo : S.deliveryWebhook}</p>;
  const repoReveal = (values: RepoRevealed) => { afterReveal.current = "heading"; setRevealed(values); };
  const repoStep = repo !== undefined && <RepoStep repo={repo} admin={admin} reveal={repoReveal} saved={readRepo} />;
  const repoTeam = repoSaved && <RepoTeamCard repo={repo} />;
  const repoNext = repoSaved && <RepoTeamCard repo={repo} next />; // in the checklist, under the repo step: the platform team's part comes next
  const graceField = admin ? (
    <>
      <label className={LABEL} htmlFor="hand-off-grace">Access grace</label>
      <span className="flex flex-wrap items-center gap-2">
        <input id="hand-off-grace" className={`${FIELD} max-w-[160px]`} value={grace} onChange={(event) => setGrace(event.target.value)} />
        <Button variant="secondary" label="Save the access grace" onClick={() => void saveGrace()}>Save</Button>
      </span>
      <p className={NOTE}>{S.graceNote}</p>
      {refused("grace")}
    </>
  ) : <p className={NOTE}>Access grace: <span className="font-mono">{record.access_grace}</span></p>;

  return (
    <section aria-labelledby="run-where">
      <h2 id="run-where" ref={heading} tabIndex={-1} className="text-base font-semibold">{S.runWhere}</h2>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      {!admin && <p className={NOTE}>{S.runAdmin}</p>}
      {admin ? (
        <fieldset className="mt-2">
          <legend className="sr-only">{S.runWhere}</legend>
          {([["canyon", S.runCanyon, null], ["org", S.runOrg, S.runOrgNote]] as const).map(([value, word, note]) => (
            <div key={value} className="mt-2">
              <label className="flex items-center gap-2 text-base font-semibold">
                <input type="radio" name="run-where" checked={choice === value} aria-describedby={note === null ? undefined : "run-org-note"} onChange={() => choose(value)} />
                {word}
              </label>
              {note !== null && <p id="run-org-note" className={`${NOTE} ml-6`}>{note}</p>}
            </div>
          ))}
        </fieldset>
      ) : <p className="mt-2 text-base font-semibold">{org ? S.runOrg : S.runCanyon}</p>}
      {confirm("off", S.offAsk, "Run apps on this cluster", () => void switchTo("canyon"))}
      {refused("off")}

      {org && viaRepo ? (
        <>
          {deliveryChoice}
          {repoSaved && <p className="mt-3 text-base">{S.onRepoSummary.replace("<url>", () => repo.url)}</p>}
          {repo?.last_commit?.state === "accepted" && <p className={NOTE}>{S.onRepoWaiting}</p>}
          {repoStep}
          {graceField}
          {repoTeam}
        </>
      ) : org ? (
        <>
          {deliveryChoice}
          {hook && <p className="mt-3 text-base">{S.onSummary.replace("<url>", () => hook.url)}</p>}
          {webhookControls}
          {graceField}
          {team}
        </>
      ) : admin && choice === "org" ? (
        <>
          {deliveryChoice}
          <ol className="mt-3 grid gap-3">
            <li aria-current={current === 0 ? "step" : undefined} className={CARD}>
              <h3 className="text-base font-semibold">{steps[0] ? "✓ " : "1. "}{S.stepServer}</h3>
              {!steps[0] && (
                <section aria-labelledby="hand-off-needs">
                  <p id="hand-off-needs" className={NOTE}>{S.handOffNeeds}</p>
                  <ul className="mt-1 list-disc pl-5">
                    {read.hand_off_needs.map((need) => <li key={need.error} className={NOTE}>{need.message}</li>)}
                  </ul>
                  <p className={NOTE}>{S.reloadAfter}</p>
                </section>
              )}
            </li>
            <li aria-current={current === 1 ? "step" : undefined} className={CARD}>
              <h3 className="text-base font-semibold">{steps[1] ? "✓ " : "2. "}{viaRepo ? S.stepRepo : S.stepWebhook}</h3>
              {viaRepo ? <>{repoStep}{repoNext}</> : hook === null ? urlForm("Save the webhook") : webhookControls}
            </li>
            <li aria-current={current === 2 ? "step" : undefined} className={CARD}>
              <h3 className="text-base font-semibold">{steps[2] ? "✓ " : "3. "}{S.gateway}</h3>
              {steps[2]
                ? read.probe_note !== "" && <p role="note" className="mt-1 text-md text-amber">{read.probe_note}</p>
                : (
                  <>
                    {read.front_door?.choice === 1
                      ? <p role="note" className="mt-1 text-md text-amber">{read.probe_note}</p>
                      : <p className={NOTE}>{read.front_door === null ? S.stepDoorNone : S.stepDoorProxy}</p>}
                    <span className="mt-2 flex"><Button variant="secondary" onClick={toFrontDoor}>{S.toFrontDoor}</Button></span>
                  </>
                )}
            </li>
            <li aria-current={current === -1 ? "step" : undefined} className={CARD}>
              <h3 className="text-base font-semibold">4. {S.handOn}</h3>
              <span className="mt-2 flex">
                {current === -1
                  ? <Button variant="primary" onClick={() => { setRefusal(null); setAsking("on"); }}>{S.handOn}</Button>
                  : <Button disabled reason={S.handOnFirst}>{S.handOn}</Button>}
              </span>
              {confirm("on", S.onAsk, S.handOn, () => void switchTo("org"))}
              {refused("on")}
            </li>
          </ol>
          {graceField}
          {!viaRepo && team}
        </>
      ) : viaRepo ? repoSaved && (
        <>
          {repoStep}
          {repoTeam}
        </>
      ) : hook && (
        <>
          {webhookControls}
          {team}
        </>
      )}
      {revealed !== null && <Reveal values={revealed} done={done} />}
    </section>
  );
}

// The values Canyon shows once, over the page until the admin says they are stored: a dialog the keyboard stays in, Escape
// answered with why it stays, and leaving the page asked first. No form and no field name, so nothing submits them anywhere.
function Reveal({ values, done }: { values: Revealed; done: () => void }) {
  const [stored, setStored] = useState(false);
  const [leaving, setLeaving] = useState(false);
  const [copied, setCopied] = useState<{ which: string; ok: boolean } | null>(null);
  const box = useRef<HTMLDivElement>(null);
  const both = [values.secret, values.token, values.fetch].filter((v) => v !== undefined).length > 1;
  useEffect(() => {
    box.current?.querySelector<HTMLElement>("input")?.focus();
    const ask = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    window.addEventListener("beforeunload", ask);
    return () => window.removeEventListener("beforeunload", ask);
  }, []);
  const keep = (event: KeyboardEvent) => {
    if (event.key === "Escape") {
      event.preventDefault();
      setLeaving(true);
      return;
    }
    if (event.key !== "Tab") return;
    const stops = [...(box.current?.querySelectorAll<HTMLElement>("input, button:not([disabled])") ?? [])];
    const [first, last] = [stops[0], stops[stops.length - 1]];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
  };
  // navigator.clipboard is absent outside a secure context, a plain-http host: that is a failure too, said in place.
  const copy = (which: string, value: string) => {
    Promise.resolve().then(() => navigator.clipboard.writeText(value)).then(() => setCopied({ which, ok: true }), () => setCopied({ which, ok: false }));
  };
  const field = (id: string, label: string, value: string, note: string) => (
    <div>
      <label className={LABEL} htmlFor={id}>{label}</label>
      <span className="flex flex-wrap items-center gap-2">
        <input id={id} readOnly value={value} spellCheck={false} autoComplete="off" className={`${FIELD} font-mono text-sm`} />
        <Button variant="secondary" label={`Copy the ${label.toLowerCase()}`} onClick={() => copy(label, value)}>Copy</Button>
      </span>
      <p className={NOTE}>{note}</p>
    </div>
  );
  return (
    <div className={OVER}>
      <div ref={box} role="dialog" aria-modal="true" aria-labelledby="reveal-title" aria-describedby="reveal-ask" onKeyDown={keep}
        className={`${OVER_BOX} max-w-[560px] gap-2`}>
        <h2 id="reveal-title" className="text-base font-semibold">{S.revealTitle}</h2>
        <p id="reveal-ask" className="text-md text-text-dim">{both ? S.revealAsk : S.revealAskOne}</p>
        {values.secret !== undefined && field("reveal-secret", "Webhook secret", values.secret, S.secretNote)}
        {values.fetch !== undefined && field("reveal-fetch", S.fetchLabel, values.fetch, S.fetchNote)}
        {values.token !== undefined && field("reveal-token", "Host token", values.token, S.tokenNote)}
        <p role="status" className="text-md text-ok">{copied?.ok === true ? `${copied.which} copied` : ""}</p>
        {copied?.ok === false && <p role="alert" className="text-md text-critical">{S.copyFailed}</p>}
        <label className="mt-1 flex items-center gap-2 text-base">
          <input type="checkbox" checked={stored} onChange={(event) => setStored(event.target.checked)} />
          {both ? S.revealAck : S.revealAckOne}
        </label>
        <p className="text-md text-text-dim">{S.revealLost}</p>
        {leaving && <p role="alert" className="text-md text-amber">{S.revealLeave}</p>}
        <span className="flex">
          {stored ? <Button variant="primary" onClick={done}>Done</Button> : <Button disabled reason={both ? S.revealTickBoth : S.revealTickOne}>Done</Button>}
        </span>
      </div>
    </div>
  );
}
