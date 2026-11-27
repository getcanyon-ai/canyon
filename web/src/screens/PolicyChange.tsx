import { useRef, useState } from "react";
import { applyPolicy, previewPolicy, said, type Policy, type PolicyChange, type PolicyState, type Preview } from "../api";
import { POLICY_STATE as WORD } from "../strings";
import { Button } from "../ui/Button";

// The grace the tightening Save offers (design/specs/screens.md §A4b).
const GRACE = 14;
const RANK: Record<PolicyState, number> = { off: 0, advisory: 1, blocking: 2 };

// The one apply path: the save bar here and the setup chat's proposal card both send it. A grace of
// 0 goes as no grace_days, which the contract reads as now.
export const setPolicy = (rule: string, change: PolicyChange, grace: number) =>
  applyPolicy(rule, grace === 0 ? change : { ...change, grace_days: grace });

// A gate is named by the field that sets it (contracts/routes.md, the policies row): `state` before
// preview — a go-live rule's before production, its only gate — and `production` before production.
export type Gate = keyof PolicyChange;
const GATES: Gate[] = ["state", "production"];
// A rule's state in force in a gate now, and its edit waiting there: a rule not placed in a gate is Off there.
const now = (rule: Policy, gate: Gate) => (gate === "state" ? rule.state : rule.production ?? "off");
const waiting = (rule: Policy, gate: Gate) => (gate === "state" ? rule.scheduled : rule.scheduled_production);
const named = (change: PolicyChange) => GATES.filter((gate) => change[gate] !== undefined);

// One change asked of one rule: the server's preview of what it would block today, once it answers.
export interface Change { rule: Policy; change: PolicyChange; preview: Preview | null }
const tightens = ({ rule, change }: Change) => named(change).some((gate) => RANK[change[gate]!] > RANK[now(rule, gate)]);
const loosens = ({ rule, change }: Change) => named(change).some((gate) => RANK[change[gate]!] < RANK[now(rule, gate)]);
// Only a change that tightens and loosens nothing may wait a grace: one that moves both ways saves now.
const waits = (one: Change) => tightens(one) && !loosens(one);

// Every change on a page, held together until one Save (the redesign: "Changes collect in one save
// bar"). A control previews and never writes: only Save sends `apply`, rule by rule, and a refusal
// stops there with the rest still held. A rule's change holds every gate asked of it; asking a gate
// for what it already is drops it, unless a scheduled edit is waiting there, where keeping the state
// in force is the change, and asking a gate for nothing (undefined) drops it too. A preview answers
// only the change it was asked for: one replaced, discarded or saved meanwhile ignores it.
export function usePolicyChanges(applied: (policy: Policy) => void) {
  const [changes, setChanges] = useState<Change[]>([]);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false); // the last Save wrote every change: the bar goes, and the page says so
  const sending = useRef(false); // a second click in the same tick, before the bar says Saving…
  // The changes as last set, for the answers that arrive later to check against.
  const held = useRef<Change[]>([]);
  const hold = (next: (list: Change[]) => Change[]) => { held.current = next(held.current); setChanges(held.current); };

  const ask = async (rule: Policy, gates: PolicyChange) => {
    setRefusal(null);
    setSaved(false);
    const prior = held.current.find((one) => one.rule.id === rule.id);
    const asked: PolicyChange = { ...prior?.change, ...gates };
    const change: PolicyChange = Object.fromEntries(named(asked)
      .filter((gate) => asked[gate] !== now(rule, gate) || waiting(rule, gate) !== undefined).map((gate) => [gate, asked[gate]]));
    const same = named(change).length === 0;
    hold((list) => [...list.filter((one) => one.rule.id !== rule.id), ...(same ? [] : [{ rule, change, preview: null }])]);
    if (same) return;
    try {
      const preview = await previewPolicy(rule.id, change);
      hold((list) => list.map((one) => (one.change === change ? { ...one, preview } : one)));
    } catch (err) {
      // A refused preview takes its change back to what the rule held before it: the server would
      // refuse the apply the same way.
      if (!held.current.some((one) => one.change === change)) return;
      hold((list) => [...list.filter((one) => one.change !== change), ...(prior === undefined ? [] : [prior])]);
      setRefusal(`${rule.name}: ${said(err)}`);
    }
  };

  const save = async (grace: number) => {
    if (sending.current) return;
    sending.current = true;
    setSaving(true);
    setRefusal(null);
    let all = true;
    for (const one of held.current) {
      try {
        applied((await setPolicy(one.rule.id, one.change, waits(one) ? grace : 0)).policy);
        hold((list) => list.filter((left) => left !== one));
      } catch (err) {
        setRefusal(`${one.rule.name}: ${said(err)}`);
        all = false;
        break;
      }
    }
    setSaved(all);
    sending.current = false;
    setSaving(false);
  };

  const discard = () => { hold(() => []); setRefusal(null); setSaved(false); };
  const of = (id: string) => changes.find((one) => one.rule.id === id);
  // The state a rule's control in a gate shows: the one asked of it there, else its own.
  const state = (rule: Policy, gate: Gate = "state") => of(rule.id)?.change[gate] ?? now(rule, gate);
  return { changes, refusal, saving, saved, ask, save, discard, of, state };
}
export type Flow = ReturnType<typeof usePolicyChanges>;

const plural = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;
const turn = (was: PolicyState, to: PolicyState) => (to === was ? `keep ${WORD[was]}` : `${WORD[was]} → ${WORD[to]}`);
const DOES: Record<PolicyState, string> = { off: "is Off", advisory: "warns", blocking: "blocks" };
// What one change asks for, in the row's words. A change before production says what the rule does
// there, and what it is before preview beside it.
const asked = ({ rule, change }: Change) => {
  const { state, production } = change;
  if (production === undefined) return `${rule.name}, ${turn(rule.state, state!)}`;
  const was = now(rule, "production");
  const preview = state ?? rule.state;
  const there = production === "off" && was !== "off" ? `${rule.name} comes out of before production.`
    : was === "off" && production !== "off" ? `${rule.name} ${preview === "off" ? "" : "also "}${DOES[production]} before production.`
    : `${rule.name}, ${turn(was, production)} before production.`;
  if (state !== undefined) return `${there} It is ${WORD[state]} before preview.`;
  return rule.state === "off" ? there : `${there} It stays ${WORD[rule.state]} before preview.`;
};

// What a rule blocks today, beside its control: the server's preview of the change asked of it, else of
// the rule at its own state (`today`: absent until read, a sentence when the read failed), never a guess.
// Judged when an app is promoted, from the server's own state: no review holds a finding of one, so the preview, which
// reads reviews, names no app for it (contracts/routes.md, the policies row). The other two go-live rules it reads now.
const AT_PROMOTION = ["prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required"];
// What an empty preview means. It counts apps a gate at blocking would hold, so it says nothing of a looser rule;
// before production it names an app whose review had the rule off, and before preview the server names those apart.
function nobody(rule: Policy, one: Change | undefined): string {
  const to = one?.change ?? { state: rule.state, production: rule.production };
  if (!named(to).some((gate) => to[gate] === "blocking")) return one === undefined ? "No app is affected today." : loosens(one) ? "Canyon doesn't check which apps this lets through." : "Advisory blocks no app.";
  if (AT_PROMOTION.includes(rule.id)) return "Canyon checks this rule when an app is promoted, so it can't say which apps this affects.";
  return "No app is affected today.";
}

// The apps whose latest review ran with the rule off (the preview's unchecked, #155): what the rule would block of them
// is not known until each is reviewed again, so they are counted apart from the apps it would hold.
const unknown = (apps: string[], more: boolean) =>
  `Canyon can't tell yet whether this affects ${plural(apps.length, more ? "more app" : "app")}: ${apps.length === 1 ? "its" : "their"} latest review ran while this rule was Off.`;

export function Impact({ flow, rule, today }: { flow: Flow; rule: Policy; today?: Preview | string }) {
  const one = flow.of(rule.id);
  const read = one === undefined ? today : one.preview;
  if (read === undefined) return null;
  if (typeof read === "string") return <span className="text-md text-text-faint">{`Couldn't check today's apps: ${read}`}</span>;
  if (read === null) return <span className="text-md text-text-faint">Checking…</span>;
  const blocked = read.would_block;
  const unchecked = read.unchecked ?? [];
  const names = unchecked.length === 0 ? undefined : `Not reviewed with this rule on yet: ${unchecked.join(", ")}`;
  if (blocked.length === 0) {
    return unchecked.length === 0 ? <span className="text-md text-text-dim">{nobody(rule, one)}</span>
      : <span className="text-md text-text-dim" title={names}>{`${unknown(unchecked, false)} No other app is affected today.`}</span>;
  }
  const held = `${plural(blocked.length, "app")} could not promote today: ${blocked.map((app) => app.project).join(", ")}`;
  return (
    <span className="text-md font-medium text-critical" title={[...blocked.map((app) => `${app.project}: ${plural(app.findings, "finding")}`), ...(names === undefined ? [] : [names])].join("\n")}>
      {unchecked.length === 0 ? held : `${held}. ${unknown(unchecked, true)}`}
    </span>
  );
}

// The one save bar: what changes, what that means for apps, and the only buttons that write it.
// Tightening keeps running apps running and may wait 14 days; loosening lets findings through. A
// page whose rows have no room for a change's impact (`affects`) says it here, change by change.
export function SaveBar({ flow, affects = false }: { flow: Flow; affects?: boolean }) {
  if (flow.changes.length === 0 && flow.refusal === null) return flow.saved ? <p role="status" className="mt-4 text-base text-ok">Saved.</p> : null;
  const tightening = flow.changes.some(waits);
  // Nothing is saved before its preview says what it blocks: that preview is the reason for the page.
  const checking = flow.changes.some((one) => one.preview === null);
  return (
    <div role="region" aria-label="Unsaved changes" className="sticky bottom-4 z-10 mt-4 flex flex-wrap items-center gap-3 rounded-[10px] border border-border-strong bg-surface px-4 py-3 text-base text-text shadow-overlay">
      <span aria-live="polite" className="min-w-0 flex-1 basis-[320px]">
        {flow.changes.length > 0 && <b className="font-semibold">{`${plural(flow.changes.length, "change")}: `}</b>}
        {affects
          ? flow.changes.map((one) => <span key={one.rule.id} className="block">{`${asked(one)} `}<Impact flow={flow} rule={one.rule} /></span>)
          : flow.changes.map(asked).join(" · ")}
        {flow.changes.some(loosens) && <span className="block text-md text-text-dim">Apps with these findings will be able to reach production.</span>}
        {tightening && <span className="block text-md text-text-dim">Apps already in production keep running: policies gate promotion, never running apps.</span>}
        {flow.refusal !== null && <span role="alert" className="block text-md font-medium text-critical">{flow.refusal}</span>}
      </span>
      {flow.saving && <span className="text-md text-text-dim">Saving…</span>}
      {flow.changes.length > 0 && !flow.saving && (
        <span className="flex flex-wrap items-center justify-end gap-2 max-[520px]:w-full">
          <Button variant="ghost" onClick={flow.discard}>Discard</Button>
          {checking ? <span className="text-md text-text-dim">Checking what this blocks…</span> : (
            <>
              {tightening && <Button onClick={() => void flow.save(GRACE)}>{`Save, tightening in ${GRACE} days`}</Button>}
              <Button variant="primary" onClick={() => void flow.save(0)}>Save</Button>
            </>
          )}
        </span>
      )}
    </div>
  );
}

// A change scheduled ahead in a gate says what it becomes and when. An admin can keep the state in
// force instead: saving it now supersedes the scheduled edit (contracts/routes.md, the policies row).
export function Scheduled({ rule, flow, gate = "state" }: { rule: Policy; flow: Flow; gate?: Gate }) {
  const scheduled = waiting(rule, gate);
  if (scheduled === undefined) return null;
  const kept = now(rule, gate);
  return (
    <p className="mt-1 flex flex-wrap items-center gap-2 text-md text-text-dim">
      {`Becomes ${WORD[scheduled.state]} on ${new Date(scheduled.effective_at).toLocaleDateString()}`}
      <button type="button" className="text-link" aria-label={`Keep ${WORD[kept]}: ${rule.name}`} onClick={() => void flow.ask(rule, { [gate]: kept })}>{`Keep ${WORD[kept]}`}</button>
    </p>
  );
}
