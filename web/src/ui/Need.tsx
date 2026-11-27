import { useEffect, useRef, useState } from "react";
import { allowDestination, disallowDestination, dismissEgressRequest, dismissRequest, said, share, unshare, type FleetRow, type Need, type Reduced, type Viewer } from "../api";
import { S } from "../strings";
import { pendingLine } from "./pending";
import { Assign, Claim } from "../screens/Claim";
import { actorName } from "./Actor";
import { ago } from "./Age";
import { Button } from "./Button";
import { heldLine, hostWords } from "./Headline";
import { Icon, type IconName } from "./Icon";
import { licenceDate } from "./Enterprise";

// What needs a person, as one line says it (design/EXPERIENCE.md D15; contracts/routes.md, *What needs a person*): what
// the app can't do, why, and the fix where the sentence is, or who can act when the reader can't. The server orders the
// needs and says who resolves each; this file only words them, so the fleet, the side pane and the app page say the same.

type App = FleetRow | Reduced;
const ENV = { prod: "production", preview: "preview" } as const;
const fill = (template: string, values: Record<string, string>) => template.replace(/<([a-z]+)>/g, (whole, key: string) => values[key] ?? whole);
const n = (count: number, one: string, many: string) => (count === 1 ? one : many.replace("<n>", String(count)));
const env = (need: Need) => (need.env === null ? "" : ENV[need.env]);
const version = (need: Need) => need.version ?? "";
const findings = (count: number) => n(count, "1 blocking finding", "<n> blocking findings");
const who = (need: Need) => (need.asked_by === undefined ? "Someone" : actorName(need.asked_by));

// Red when the people using the app hit it, or a version is stuck; amber when a person waits, or only preview is hit.
const TONE: Record<string, { icon: IconName; ink: string; edge: string }> = {
  red: { icon: "blocked", ink: "text-critical", edge: "border-l-critical" },
  amber: { icon: "alert", ink: "text-amber", edge: "border-l-amber" },
  neutral: { icon: "person", ink: "text-text-dim", edge: "border-l-border-strong" },
};
const tone = (need: Need) => {
  if (need.kind === "held" || need.kind === "secret" || need.kind === "blocked" || need.kind === "unchecked" || need.kind === "blocked_prod") return TONE.red!;
  if ((need.kind === "down" || need.kind === "refused" || need.kind === "refused_code") && need.env === "prod") return TONE.red!;
  if (need.kind === "will_refuse" && need.blocks === true) return TONE.red!;
  if (need.kind === "unowned" || need.kind === "access") return TONE.neutral!;
  return TONE.amber!;
};
export const needInk = (need: Need) => tone(need).ink;

// What production won't allow that no Allow can let through, as the code has to change for it (contracts/routes.md,
// will_refuse's denied[]): `invalid`, which the records keep for a name out of shape, as an invalid address; one on 443
// by its address alone; anything else with its port.
export const codeOnly = (denied: NonNullable<Need["denied"]>) =>
  denied.filter((one) => !one.allowable).map((one) => (one.destination === "invalid" ? S.invalidAddress : one.port === 443 ? one.destination : `${one.destination}:${one.port}`));

// The row's own short line, under the app's name in the fleet.
export function needShort(need: Need): string {
  const values = { env: env(need), destination: need.destination ?? "", version: version(need), who: who(need), findings: findings(need.count ?? 1), n: String(need.count ?? 1) };
  const short: Record<string, string> = {
    held: S.needRowHeld, down: need.state === "restarting" ? S.needRowRestarting : S.needRowDown, refused: S.needRowRefused, refused_code: S.needRowRefused,
    secret: S.needRowSecret, approval: S.needRowApproval, blocked: S.needRowBlocked, unchecked: (need.count ?? 1) > 1 ? S.needRowUncheckedMany : S.needRowUnchecked, patch: S.needRowPatch,
    access: S.needRowAccess, unowned: S.needRowUnowned, never_reviewed: S.needRowNeverReviewed,
    recreate: need.network === "unknown" ? S.needRowRecreateUnknown : need.network === "internal" ? S.needRowRecreateInternal : S.needRowRecreate,
    blocked_prod: S.needRowBlockedProd, will_refuse: S.needRowWillRefuse,
  };
  return fill(short[need.kind] ?? S.needRowUnknown, values);
}

// The line: its title and the sentences under it.
export function needWords(app: App, need: Need): { title: string; text: string[] } {
  const values = { app: app.name, env: env(need), destination: need.destination ?? "", version: version(need), who: who(need), port: String(need.port ?? "") };
  const t = (template: string) => fill(template, values);
  switch (need.kind) {
    case "held":
      return { title: t(S.needHeld), text: app.hold === undefined ? [] : [heldLine(app.hold)] };
    case "down":
      // Its last log lines are on its page, stopped or restarting (#2744, #188).
      return { title: t(need.state === "restarting" ? S.needRestarting : S.needDown), text: [need.env === "prod" ? S.needDownFixProd : S.needDownFixPreview] };
    case "refused": {
      const other = need.env === "prod" ? "Preview" : "Production";
      return {
        title: t(S.needRefused),
        text: [fill(S.needRefusedWhy, { ...values, calls: n(need.refusals ?? 1, "1 call", "<n> calls"), age: ago(need.last_refused_at ?? null) }),
          ...((need.other_calls ?? 0) > 0 ? [fill(S.needRefusedOther, { other, times: n(need.other_calls!, "once", "<n> times") })] : [])],
      };
    }
    case "refused_code":
      return { title: need.why === "port" ? fill(S.needCode, { ...values, destination: `${values.destination}:${values.port}` }) : t(S.needCode), text: [need.why === "address" ? S.needCodeAddress : t(S.needCodePort)] };
    case "secret":
      return { title: t(S.needSecret), text: [S.needSecretFix] };
    case "approval":
      return { title: t(S.needApproval), text: [fill(S.needApprovalWhy, { age: ago(need.since) })] };
    case "blocked":
      return { title: fill(S.needBlocked, { ...values, findings: n(need.count ?? 1, "1 finding", "<n> findings") }), text: [(need.count ?? 1) === 1 ? S.needBlockedFixOne : S.needBlockedFixMany] };
    case "unchecked": {
      const host = (need.checks ?? []).find((check) => check.fix === "host");
      const fix = host === undefined ? S.needUncheckedAgain : hostWords(host.tool, host.reason) ?? host.reason;
      return { title: fill(S.needUnchecked, { ...values, checks: n(need.count ?? 1, "1 of Canyon's checks", "<n> of Canyon's checks") }), text: [fix] };
    }
    case "patch":
      return { title: t(S.needPatch), text: [S.needPatchWhy] };
    case "access": // the asker's level when they asked (#2774): read-only asks to join the team; unrecorded claims neither
      return need.level === "viewer" ? { title: t(S.needJoin), text: [fill(S.needJoinWhy, { age: ago(need.since) })] }
        : { title: t(S.needAccess), text: [fill(need.level === "none" ? S.needAccessWhy : S.needAccessAsked, { age: ago(need.since) })] };
    case "unowned":
      return { title: t(S.needUnowned), text: [] };
    case "never_reviewed":
      return { title: t(S.needNeverReviewed), text: [S.needNeverReviewedFix] };
    case "blocked_prod": {
      const count = need.count ?? null;
      const title = count === null ? S.needBlockedProdUnread : count === 1 ? S.needBlockedProdOne : S.needBlockedProdMany;
      return { title: fill(title, { ...values, n: String(count) }), text: [count === 1 ? S.needBlockedFixOne : S.needBlockedFixMany] }; // unread, findings: the plural
    }
    case "will_refuse": {
      const denied = need.denied ?? [];
      const allowable = denied.filter((one) => one.allowable).map((one) => one.destination);
      const code = codeOnly(denied);
      return {
        title: t(need.blocks === true ? S.needWillRefuseBlocks : S.needWillRefuse),
        text: [...(allowable.length > 0 ? [fill(S.needWillRefuseAllowable, { destinations: allowable.join(", ") })] : []),
          ...(code.length > 0 ? [fill(S.needWillRefuseCode, { destinations: code.join(", ") })] : [])],
      };
    }
    case "recreate":
      return { title: t(need.network === "unknown" ? S.needRecreateUnknown : need.network === "internal" ? S.needRecreateInternal : S.needRecreateOpen), text: need.network === "unknown" ? [S.needRecreateUnknownStopped] : [] };
  }
  return { title: S.needUnknown, text: [] };
}

// Who acts, where the reader can't: admins by name, as the server gives them; to an admin, what they can do of the two.
function whoLine(need: Need, viewer: Viewer, signedIn: boolean, claimable: boolean): string | null {
  const admins = need.ask.length === 0 ? null : fill(S.needAdmins, { admins: need.ask.map((one) => one.display).join(", ") });
  switch (need.kind) {
    case "held":
      return need.you ? null : [S.needHeldWho, admins].filter(Boolean).join(" ");
    case "unchecked":
    case "blocked_prod":
      return need.you ? null : [S.approvers, admins].filter(Boolean).join(" ");
    case "refused":
      return need.you ? S.needAllowWho : S.needAllowWhoOther; // its <app> filled where the line is drawn
    case "will_refuse": {
      const hosts = (need.denied ?? []).filter((one) => one.allowable).length;
      if (hosts === 0) return null;
      return need.you ? (hosts === 1 ? S.needAllowWho : S.needAllowWhoMany) : hosts === 1 ? S.needAllowWhoOther : S.needAllowWhoOtherMany;
    }
    case "unowned":
      if (viewer.role === "admin" && claimable) return S.needUnownedClaimAdmin;
      if (viewer.role === "admin" && signedIn) return S.needUnownedAssign;
      return claimable ? S.needUnownedClaim : [S.needUnownedAsk, admins].filter(Boolean).join(" ");
  }
  return null;
}

const LINK = "inline-flex h-7 items-center rounded-[7px] border border-border-strong bg-surface px-2.5 text-md font-medium whitespace-nowrap text-text no-underline hover:bg-surface-2";
const appHref = (app: App) => `#/apps/${app.id}`;
const reviewHref = (app: App) => ("last_run" in app && app.last_run !== null ? `#/apps/${app.id}?run=${encodeURIComponent(app.last_run)}` : appHref(app));

// What a person did from a line, said in its place, with what undoes it.
export interface Done { line: string; again?: { label: string; run: () => Promise<unknown> } }

// One need's line, with its fix where the reader may act: Allow and Dismiss on a refused call, Add and Dismiss on a request
// for access, Lift on a hold, and a link to the page that resolves the rest. done reports what a click did. On the app's
// own page (here) a link to that page is no step; focus puts the keyboard on the line's Allow, for the link a refusal
// carries (#/apps/<app>/outbound).
export function NeedLine({ app, need, viewer, signedIn, lift, done, here = false, focus = false }: {
  app: App; need: Need; viewer: Viewer; signedIn: boolean; lift: (app: App) => void; done: (what: Done) => void;
  here?: boolean; focus?: boolean;
}) {
  const allowing = useRef<HTMLButtonElement>(null);
  useEffect(() => { if (focus) allowing.current?.focus(); }, [focus]);
  const [failure, setFailure] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const { title, text } = needWords(app, need);
  const look = tone(need);
  const ask = whoLine(need, viewer, signedIn, app.claimable)?.replace("<app>", () => app.name) ?? null;
  const act = async (call: () => Promise<unknown>, after: Done) => {
    if (sending) return;
    setSending(true);
    setFailure(null);
    try {
      // an egress write answers what was still pending (#2789): said in place of the line
      const out = await call();
      const pending = typeof out === "string" ? pendingLine(out) : null;
      done(pending === null ? after : { ...after, line: pending });
    } catch (err) {
      setFailure(said(err));
    }
    setSending(false);
  };
  const values = { app: app.name, env: env(need), destination: need.destination ?? "", who: who(need) };
  const actions = (() => {
    if (need.kind === "refused" && need.you && need.env !== null && need.destination !== undefined) {
      const [at, dest] = [need.env, need.destination];
      const allow = () => act(() => allowDestination(app.id, at, dest), { line: fill(S.needAllowed, values), again: { label: S.undo, run: () => disallowDestination(app.id, at, dest) } });
      return (
        <>
          {need.held === true ? <Button size="sm" disabled reason={S.needAllowHeld}>{S.allow}</Button>
            : <Button ref={allowing} size="sm" label={`${S.allow} ${dest} in ${env(need)} for ${app.name}`} onClick={() => void allow()}>{S.allow}</Button>}
          <Button size="sm" variant="ghost" label={`${S.dismiss} ${dest} for ${app.name}`}
            onClick={() => void act(() => dismissEgressRequest(app.id, at, dest), { line: fill(S.needDismissed, values), again: need.held === true ? undefined : { label: S.needAllowInstead, run: () => allowDestination(app.id, at, dest) } })}>{S.dismiss}</Button>
        </>
      );
    }
    if (need.kind === "access" && need.you && need.email !== undefined) {
      const email = need.email;
      return (
        <>
          {need.level === "viewer"
            ? <Button size="sm" label={`${S.addToTeam}: ${who(need)} on ${app.name}`}
              onClick={() => void act(() => share(app.id, "email", email, { level: "contributor", roles: [] }), { line: fill(S.needAddedTeam, values) })}>{S.addToTeam}</Button>
            : <Button size="sm" label={`${S.addReadOnly}: ${who(need)} on ${app.name}`}
            onClick={() => void act(() => share(app.id, "email", email, { level: "viewer", roles: [] }), { line: fill(S.needAdded, values), again: { label: S.undo, run: () => unshare(app.id, "email", email) } })}>{S.addReadOnly}</Button>}
          <Button size="sm" variant="ghost" label={`${S.dismiss} ${who(need)}'s request for ${app.name}`}
            onClick={() => void act(() => dismissRequest(app.id, email), { line: fill(S.needRequestDismissed, values) })}>{S.dismiss}</Button>
        </>
      );
    }
    if (need.kind === "will_refuse" && need.you) {
      const hosts = (need.denied ?? []).filter((one) => one.allowable).map((one) => one.destination);
      return hosts.length === 0 ? null : (
        <>
          {hosts.map((dest, i) => app.hold !== undefined ? ( // while production is held an Allow there is refused (409 held)
            <Button key={dest} size="sm" disabled reason={S.needAllowHeld}>{hosts.length === 1 ? S.allowInProduction : `${S.allow} ${dest}`}</Button>
          ) : (
            <Button key={dest} ref={i === 0 ? allowing : undefined} size="sm" label={`${S.allow} ${dest} in production for ${app.name}`}
              onClick={() => void act(() => allowDestination(app.id, "prod", dest), { line: fill(S.needAllowed, { ...values, env: "production", destination: dest }), again: { label: S.undo, run: () => disallowDestination(app.id, "prod", dest) } })}>
              {hosts.length === 1 ? S.allowInProduction : `${S.allow} ${dest}`}
            </Button>
          ))}
        </>
      );
    }
    if (need.kind === "held") return need.you ? <Button size="sm" variant="secondary" label={`${S.liftHold} on ${app.name}`} onClick={() => lift(app)}>{S.liftHold}</Button> : null;
    if (need.kind === "unowned") return <>{viewer.role === "admin" && signedIn && <Assign app={app} />}<Claim app={app} display={viewer.display} /></>;
    if ((need.kind === "approval" || need.kind === "patch") && need.code !== undefined) return <a className={LINK} href={`#/approve?code=${encodeURIComponent(need.code)}`} aria-label={`${S.reviewAndApprove}: ${app.name}`}>{S.reviewAndApprove}</a>;
    if (need.kind === "refused_code") return <a className={LINK} href={`#/apps/${app.id}/outbound`} aria-label={`${S.openOutbound}: ${app.name}`}>{S.openOutbound}</a>;
    if (need.kind === "recreate") return <a className={LINK} href="#/settings/outbound" aria-label={`${S.openSettingsOutbound}: ${app.name}`}>{S.openSettingsOutbound}</a>;
    if (here) return null; // the review and the app are this page
    if (need.kind === "secret" || need.kind === "blocked" || need.kind === "unchecked" || need.kind === "blocked_prod") return <a className={LINK} href={reviewHref(app)} aria-label={`Open review: ${app.name}`}>Open review</a>;
    return <a className={LINK} href={appHref(app)} aria-label={`Open the app: ${app.name}`}>Open the app</a>;
  })();
  return (
    <li className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-start gap-x-3 gap-y-2 border-b border-border px-4 py-3 last:border-b-0 max-[620px]:grid-cols-[18px_minmax(0,1fr)]">
      <span className={`mt-0.5 ${look.ink}`}><Icon name={look.icon} /></span>
      <div className="grid min-w-0 gap-0.5 text-base wrap-anywhere text-text-dim">
        <p><b className="font-semibold text-text">{title}</b>{text.length > 0 && ` ${text.join(" ")}`}</p>
        {ask !== null && <p className="text-md">{ask}</p>}
        {failure !== null && <p role="alert" className="text-md text-critical">{failure}</p>}
      </div>
      {actions !== null && <span className="flex flex-wrap items-center gap-2 max-[620px]:col-start-2">
        {sending ? <span role="status" className="text-md text-text-dim">Saving…</span> : actions}
      </span>}
    </li>
  );
}

// A click's result, where its line was: what was done, and what undoes it.
// OrgNeedLine is one of the org's own needs, which no app carries (the fleet read's org_needs): a chat channel failing, an
// admin's to change. A kind this dashboard doesn't know still says something needs a person.
export function OrgNeedLine({ need }: { need: Need }) {
  if (need.kind === "licence" || need.kind === "patching_unlicensed") return <LicenceNeedLine need={need} />;
  const known = need.kind === "notify_failing";
  return (
    <li className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-start gap-x-3 gap-y-2 border-b border-border px-4 py-3 last:border-b-0 max-[620px]:grid-cols-[18px_minmax(0,1fr)]">
      <span className={`mt-0.5 ${TONE.amber!.ink}`}><Icon name={TONE.amber!.icon} /></span>
      <p className="text-base text-text-dim">
        <b className="font-semibold text-text">{known ? fill(S.needNotifyFailing, { channel: need.channel ?? "" }) : S.needUnknown}</b>
        {known && ` ${fill(S.needNotifyFailingWhy, { age: ago(need.since) })}`}
      </p>
      {known && <span className="max-[620px]:col-start-2"><a className={LINK} href="#/settings/integrations" aria-label={`${S.openSettingsIntegrations}: ${need.channel ?? ""}`}>{S.openSettingsIntegrations}</a></span>}
    </li>
  );
}

// LicenceNeedLine is the licence in grace, lapsed or invalid (the fleet read's org_needs, licence; #2879), or patching the
// server is set to do that no licence unlocks (patching_unlicensed; #2881), an admin's: what it means for the Enterprise
// features, and the link to Settings › Licence. A state this dashboard doesn't know still
// says something needs a person.
function LicenceNeedLine({ need }: { need: Need }) {
  const line = need.kind === "patching_unlicensed" ? S.licenceNeedPatching : need.licence === "grace" ? fill(S.licenceNeedGrace, { date: need.since === null ? "" : licenceDate(need.since), grace: need.grace_until === undefined ? "" : licenceDate(need.grace_until) })
    : need.licence === "lapsed" ? S.licenceNeedLapsed : need.licence === "invalid" ? S.licenceNeedInvalid : S.needUnknown;
  return (
    <li className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-start gap-x-3 gap-y-2 border-b border-border px-4 py-3 last:border-b-0 max-[620px]:grid-cols-[18px_minmax(0,1fr)]">
      <span className={`mt-0.5 ${TONE.amber!.ink}`}><Icon name={TONE.amber!.icon} /></span>
      <p className="text-base"><b className="font-semibold text-text">{line}</b></p>
      <span className="max-[620px]:col-start-2"><a className={LINK} href="#/settings/licence">{S.licenceNeedOpen}</a></span>
    </li>
  );
}

export function DoneLine({ what, clear }: { what: Done; clear: () => void }) {
  const [failure, setFailure] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null); // an undo that changed the egress policy, still pending (#2789)
  const [undoing, setUndoing] = useState(false); // its write out: it can wait up to 30 s for the gateway
  return (
    <li role="status" className="grid grid-cols-[18px_minmax(0,1fr)_auto] items-start gap-x-3 border-b border-border bg-surface-2 px-4 py-3 last:border-b-0 max-[620px]:grid-cols-[18px_minmax(0,1fr)]">
      <span className="mt-0.5 text-ok"><Icon name="done" /></span>
      <p className="text-base wrap-anywhere text-text">{pending ?? what.line}{failure !== null && <span className="block text-md text-critical">{failure}</span>}</p>
      {what.again !== undefined && (
        <span className="max-[620px]:col-start-2">
          {undoing ? <Button size="sm" disabled reason="Saving…">{what.again.label}</Button>
            : <Button size="sm" variant="ghost" onClick={() => { setUndoing(true); void what.again!.run().then((out) => { const line = typeof out === "string" ? pendingLine(out) : null; setUndoing(false); if (line === null) clear(); else setPending(line); }, (err: unknown) => { setUndoing(false); setFailure(said(err)); }); }}>{what.again.label}</Button>}
        </span>
      )}
    </li>
  );
}

const keyOf = (need: Need) => `${need.kind}-${need.env ?? ""}-${need.destination ?? need.email ?? ""}`;

// The needs of one app as one list (D15): the app page's band and the fleet's side pane. A click's result stands where its
// line was, and the line is gone until the app is read again (changed), so nothing acted on stays live under it.
export function NeedList({ app, needs, viewer, signedIn, lift, changed, here = false, focus = false }: {
  app: App; needs: Need[]; viewer: Viewer; signedIn: boolean; lift: (app: App) => void; changed: () => void; here?: boolean; focus?: boolean;
}) {
  const [done, setDone] = useState<Done[]>([]);
  const [acted, setActed] = useState<{ needs: Need[]; keys: string[] }>({ needs, keys: [] });
  const shown = needs.filter((need) => acted.needs !== needs || !acted.keys.includes(keyOf(need)));
  const focusAt = focus ? shown.findIndex((need) => (need.kind === "refused" || need.kind === "will_refuse") && need.you && need.held !== true) : -1;
  return (
    <ul>
      {done.map((what, i) => <DoneLine key={`done-${done.length - i}`} what={what} clear={() => { setDone((was) => was.filter((one) => one !== what)); changed(); }} />)}
      {shown.map((need, i) => (
        <NeedLine key={keyOf(need)} app={app} need={need} viewer={viewer} signedIn={signedIn} lift={lift} here={here} focus={i === focusAt}
          done={(what) => { setDone((was) => [what, ...was]); setActed((was) => ({ needs, keys: [...(was.needs === needs ? was.keys : []), keyOf(need)] })); changed(); }} />
      ))}
    </ul>
  );
}

// The app page's band while something on the app waits on a person (D15): its needs, most urgent first.
export function NeedsBand(props: Parameters<typeof NeedList>[0]) {
  return (
    <section aria-label="Needs you" className={`rounded-[10px] border border-border border-l-4 bg-surface ${props.needs[0] === undefined ? TONE.neutral!.edge : tone(props.needs[0]).edge}`}>
      <h2 className="border-b border-border px-4 py-2.5 text-base font-semibold">{S.needsBand.replace("<n>", String(props.needs.length))}</h2>
      <NeedList {...props} />
    </section>
  );
}
