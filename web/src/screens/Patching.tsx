import { useEffect, useState } from "react";
import { approveIdentity, getIdentities, putPreferences, resumeIdentity, said, type ApiError, type Identities, type Identity, type PlanSaid, type Preferences } from "../api";
import { PATCHING, S } from "../strings";
import { Button } from "../ui/Button";
import { Chip } from "../ui/Chip";
import { ago } from "../ui/Age";
import { hex12 } from "../ui/Patched";
import { shortDate } from "../ui/Verdict";
import { EnterpriseLine, unlocks, useLicence } from "../ui/Enterprise";

const NOTE = "mt-1 text-md text-text-dim";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

const plural = (n: number, one: string, many: string) => (n === 1 ? one : many.replace("<n>", String(n)));

// An image by its 12 hex (#2582), the whole reference folded under it for whoever asks.
function Reference({ label, image }: { label: string; image: string }) {
  return (
    <details>
      <summary className="cursor-pointer font-mono text-sm text-text">{label}</summary>
      <p className="font-mono text-sm break-all text-text-dim">{image}</p>
    </details>
  );
}

// What an identity has left to do (contracts/routes.md, the identities row): its apps planned, patched in preview or
// asked. The fleet approval decides only patch approvals, filed once a preview's watch passed, so an identity with none
// of these left — nothing to patch, every app patched, reverted or held, or paused — gives it nothing to decide.
const left = (identity: Identity) => identity.apps.planned + identity.apps.patched_preview + identity.apps.asked;

// Settings › App hosting's patching, an admin's alone (design/PATCHING.md §8; design/PERMISSIONS.md, Platform patches): the
// org's switch, the system libraries image Canyon uses now and whether apps wait for a newer one, and each platform identity — paused
// or stalled with Resume, and Approve for every app — and what Canyon's last check said of each app. The page renders it for an admin only, so a member never reads the
// identities, which the server refuses them anyway. With sign-in off the identities are not served (a 404): only the
// switch shows, and it reads off there.
export function PatchingSettings({ record, adopt }: { record: Preferences; adopt: (row: Preferences) => void }) {
  // Turning patching or rebuilds to ask is Canyon Enterprise's (#2879): without a licence that unlocks it, ask is offered
  // only where it is already set, so an admin can turn it off, and the badge says why in its place.
  const licence = useLicence();
  const keys = unlocks(licence, "platform_patching");
  const offered = (stored: "off" | "ask") => ([value]: readonly ["off" | "ask", string]) => value === "off" || stored === "ask" || keys;
  const [read, setRead] = useState<Identities | "unserved" | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const load = () => getIdentities().then(setRead, (err: unknown) => {
    if ((err as ApiError).status === 404) setRead("unserved");
    else setFailure(said(err));
  });
  useEffect(() => { void load(); }, []);
  const choose = (edit: { patching: "off" | "ask" } | { rebuilds: "off" | "ask" }) => {
    setFailure(null);
    putPreferences(edit).then(adopt, (err: unknown) => setFailure(said(err)));
  };
  const identities = read === null || read === "unserved" ? null : read;
  // The system libraries image Canyon uses now and any identity with work left or a breaker to resume show; earlier ones with
  // nothing left fold under them (#2582).
  const shown = (identity: Identity) => identity.run_image === identities?.run_image?.image || left(identity) > 0
    || identity.paused !== undefined || identity.stalled !== undefined;
  const folded = identities?.identities.filter((identity) => !shown(identity)) ?? [];
  // What Canyon's last check said goes in the card of the identity it checked against, else under the image Canyon uses
  // now: a check of no app, or one onto an identity no card shows (#2588).
  const plan = identities?.plan ?? null;
  const beside = plan !== null && plan.why === undefined && identities!.identities.some((identity) => identity.id === plan.identity && shown(identity)) ? plan.identity : null;
  return (
    <section aria-labelledby="patching" className="mt-2">
      <h2 id="patching" className="flex items-center gap-2 text-base font-semibold">{PATCHING.title}<Chip tone="beta">{S.beta}</Chip></h2>
      <fieldset className="mt-2">
        <legend className="sr-only">{PATCHING.title}</legend>
        {([["off", PATCHING.off], ["ask", PATCHING.ask]] as const).filter(offered(record.patching)).map(([value, word]) => (
          <label key={value} className="mt-1 flex items-center gap-2 text-base">
            <input type="radio" name="patching" checked={record.patching === value} onChange={() => choose({ patching: value })} />
            {word}
          </label>
        ))}
        {record.patching === "off" && licence !== null && !keys && <div className="mt-1"><EnterpriseLine feature="platform_patching" /></div>}
      </fieldset>
      {record.patching === "ask" && (
        <fieldset className="mt-3">
          <legend className="text-base font-medium">{PATCHING.rebuildsTitle}</legend>
          {([["off", PATCHING.rebuildsOff], ["ask", PATCHING.rebuildsAsk]] as const).filter(offered(record.rebuilds)).map(([value, word]) => (
            <label key={value} className="mt-1 flex items-center gap-2 text-base">
              <input type="radio" name="rebuilds" checked={record.rebuilds === value} onChange={() => choose({ rebuilds: value })} />
              {word}
            </label>
          ))}
          {record.rebuilds === "off" && licence !== null && !keys && <div className="mt-1"><EnterpriseLine feature="platform_patching" /></div>}
        </fieldset>
      )}
      {failure !== null && <p className="mt-1 text-base text-critical">{failure}</p>}
      {identities !== null && <RunImage read={identities} />}
      {plan !== null && beside === null && <LastCheck plan={plan} />}
      {identities?.identities.filter(shown).map((identity) => (
        <IdentityCard key={identity.id} identity={identity} plan={identity.id === beside ? plan : null} reload={() => void load()} />
      ))}
      {folded.length > 0 && (
        <details className="mt-3">
          <summary className="cursor-pointer text-md text-text-dim">{plural(folded.length, PATCHING.earlierOne, PATCHING.earlier)}</summary>
          {folded.map((identity) => <IdentityCard key={identity.id} identity={identity} plan={null} reload={() => void load()} />)}
        </details>
      )}
    </section>
  );
}

// The system libraries image Canyon uses now and how many apps wait for a newer one; Canyon can't tell is said, never "none behind".
function RunImage({ read }: { read: Identities }) {
  const run = read.run_image;
  if (run === null) return <p className={NOTE}>{PATCHING.unknown}</p>;
  const [before, after] = plural(run.behind, PATCHING.behindOne, PATCHING.behind).split("<apps>");
  return (
    <div className={NOTE}>
      <Reference label={`${PATCHING.runImage} ${hex12(run.image)}`} image={run.image} />
      {run.behind === 0 ? <p>{PATCHING.current}</p> : (
        <p className="text-amber">
          {before}
          {run.projects.map((project, i) => (
            <span key={project}>{i > 0 && ", "}<a className="text-link no-underline hover:underline" href={`#/apps/${encodeURIComponent(project)}`}>{project}</a></span>
          ))}
          {after}
        </p>
      )}
    </div>
  );
}

// What Canyon's last check said (#2588; contracts/routes.md, the identities row's plan): why it checked no app, or each
// cause with the apps it names, linked, in PATCHING.why's order; a cause the page does not know reads unknown.
function LastCheck({ plan }: { plan: PlanSaid }) {
  const by = new Map<string, string[]>();
  for (const app of plan.apps) {
    const why = Object.hasOwn(PATCHING.why, app.why) ? app.why : "unknown";
    by.set(why, [...(by.get(why) ?? []), app.project]);
  }
  if (plan.why === undefined && by.size === 0) return null;
  return (
    <div className={NOTE}>
      <p>{PATCHING.lastCheck.replace("<ago>", () => ago(plan.at))}</p>
      {plan.why !== undefined ? <p>{Object.hasOwn(PATCHING.checkedNone, plan.why) ? PATCHING.checkedNone[plan.why] : PATCHING.why.unknown}</p> : (
        <ul>
          {Object.keys(PATCHING.why).filter((why) => by.has(why)).map((why) => (
            <li key={why}>
              {PATCHING.why[why]} ·{" "}
              {by.get(why)!.map((project, i) => (
                <span key={project}>{i > 0 && ", "}<a className="text-link no-underline hover:underline" href={`#/apps/${encodeURIComponent(project)}`}>{project}</a></span>
              ))}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// One platform identity: its system libraries image by 12 hex, its wave while it has work left, and its apps by patch state; Resume
// while its breaker paused or stalled it; Approve for every app while it has work left and no admin has, with a reason;
// then who approved it, when and why; and what Canyon's last check said of its apps, when it checked against this one.
function IdentityCard({ identity, plan, reload }: { identity: Identity; plan: PlanSaid | null; reload: () => void }) {
  const [asking, setAsking] = useState(false);
  const [reason, setReason] = useState("");
  const [sending, setSending] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const send = (call: () => Promise<unknown>) => {
    if (sending) return;
    setSending(true);
    setFailure(null);
    call().then(() => { setAsking(false); reload(); }, (err: unknown) => setFailure(said(err))).finally(() => setSending(false));
  };
  const image = (identity.kind === "rebuild" ? PATCHING.builderImage : PATCHING.liveImage).replace("<hex>", () => hex12(identity.id));
  const counts = (Object.keys(PATCHING.counts) as (keyof typeof PATCHING.counts)[])
    .filter((key) => identity.apps[key] > 0).map((key) => `${identity.apps[key]} ${PATCHING.counts[key]}`);
  const reverted = identity.paused?.projects.length ?? 0;
  // A wave is said only while the identity has work left (#2582): one with no app ran none, and one whose apps all
  // ended is past its waves.
  const wave = left(identity) > 0 ? PATCHING.wave[identity.wave] ?? "" : "";
  return (
    <div className={CARD} aria-label={image} role="group">
      <Reference label={image} image={identity.builder ?? identity.run_image} />
      <p className={NOTE}>{counts.length === 0 ? (identity.kind === "rebuild" ? PATCHING.noRebuildApps : PATCHING.noApps) : [wave, ...counts].filter(Boolean).join(" · ")}</p>
      {identity.paused !== undefined && (
        <p role="note" className="mt-1 text-md text-amber">{plural(reverted, PATCHING.pausedOne, PATCHING.paused).replace("<image>", () => image)}</p>
      )}
      {identity.paused === undefined && identity.stalled !== undefined && (
        <p role="note" className="mt-1 text-md text-amber">{PATCHING.stalled.replace("<image>", () => image).replace("<app>", () => identity.stalled!.projects.join(", "))}</p>
      )}
      {identity.approved !== undefined && (
        <p className={NOTE}>{PATCHING.approved.replace("<name>", () => identity.approved!.by.display).replace("<date>", () => shortDate(identity.approved!.at)).replace("<reason>", () => identity.approved!.reason)}</p>
      )}
      {plan !== null && <LastCheck plan={plan} />}
      <span className="mt-2 flex flex-wrap items-start gap-2">
        {(identity.paused !== undefined || identity.stalled !== undefined) && (
          <Button variant="secondary" size="sm" label={`${PATCHING.resume} ${image}`} onClick={() => send(() => resumeIdentity(identity.id))}>{PATCHING.resume}</Button>
        )}
        {identity.kind !== "rebuild" && identity.approved === undefined && left(identity) > 0 && !asking && <Button variant="secondary" size="sm" onClick={() => setAsking(true)}>{PATCHING.approveAll}</Button>}
      </span>
      {asking && (
        <div className="mt-2 grid max-w-[520px] gap-2">
          <p className="text-base font-medium">{PATCHING.approveAllAsk.replace("<image>", () => image)}</p>
          <label className="grid gap-1 text-md font-medium text-text-dim">
            {PATCHING.reason}
            <textarea maxLength={1024} rows={3} readOnly={sending} className="rounded-lg border border-border-strong bg-surface px-2.5 py-1.5 text-base text-text" value={reason} onChange={(event) => setReason(event.target.value)} />
          </label>
          <span className="flex flex-wrap items-start gap-2">
            {reason.trim() === "" ? <Button disabled reason={PATCHING.reasonFirst}>{PATCHING.approveAll}</Button>
              : <Button variant="primary" onClick={() => send(() => approveIdentity(identity.id, reason))}>{PATCHING.approveAll}</Button>}
            {!sending && <Button variant="ghost" onClick={() => setAsking(false)}>Cancel</Button>}
          </span>
        </div>
      )}
      {failure !== null && <p className="mt-1 text-base text-critical">{failure}</p>}
    </div>
  );
}
