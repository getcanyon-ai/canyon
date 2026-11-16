import { useEffect, useState, type DragEvent, type ReactNode } from "react";
import { BETA, getPolicies, GO_LIVE, said, type Addition, type Policy, type PolicyState } from "../api";
import { S } from "../strings";
import { Chip } from "../ui/Chip";
import { Dial } from "../ui/Dial";
import { Icon } from "../ui/Icon";
import { POLICY_MEANING } from "./Policies";
import { SaveBar, Scheduled, usePolicyChanges, type Flow, type Gate } from "./PolicyChange";

// The three places a version can be, in order, and who moves it there; each sits before its gate.
const STAGES = [["Upload", "agent or person", "order-1"], ["Preview", "agents deploy", "order-3"], ["Production", "people promote", "order-5"]] as const;
const CARD = "relative flex min-w-0 flex-col gap-2 rounded-xl border border-border bg-surface px-3.5 py-3";
const ROW = "flex items-center gap-2.5 rounded-lg border px-3 py-2.5 max-[520px]:flex-wrap";
const TEXT = "grid min-w-0 flex-1 gap-0.5 max-[520px]:basis-[calc(100%-24px)]";

// One gate: its name, its line (D14), and its checks.
function Card({ title, line, children, place, drop }: {
  title: string; line: string; children: ReactNode; place: string; drop?: { over: (event: DragEvent) => void; drop: (event: DragEvent) => void };
}) {
  return (
    <section aria-label={title} className={`${CARD} ${place}`} onDragOver={drop?.over} onDrop={drop?.drop}>
      <h2 className="flex items-center gap-2 text-base font-semibold"><Icon name="guardrails" />{title}</h2>
      <p className="mb-1 text-md text-text-dim">{line}</p>
      {children}
    </section>
  );
}

// One org check in one gate: what it is, and its Dial there, named `label` (a rule in both gates is
// named for the gate in its second). An admin's check before preview also carries, under its Dial,
// the button that places it before production or takes it out again — the keyboard's and a touch
// screen's way to do what dragging it does — and, while it is not there, the grip it is dragged by.
function Check({ rule, gate, label, flow, place, carry }: {
  rule: Policy; gate: Gate; label: string; flow: Flow;
  place?: { placed: boolean; toggle: () => void }; carry?: (rule: Policy | null) => void;
}) {
  const grip = place !== undefined && !place.placed && carry !== undefined;
  const move = place?.placed ? "Take out of before production" : "Also before production";
  return (
    <section aria-label={label} className={`${ROW} border-border bg-surface`} draggable={grip}
      onDragStart={grip ? (event) => { event.dataTransfer.setData("text/plain", rule.id); event.dataTransfer.effectAllowed = "copy"; carry(rule); } : undefined}
      onDragEnd={grip ? () => carry(null) : undefined}>
      {grip && <span className="cursor-grab text-text-faint [@media(pointer:coarse)]:hidden"><Icon name="grip" size={14} /></span>}
      <span className={TEXT}>
        <span className="flex flex-wrap items-center gap-x-2 text-base font-medium">{rule.name}{BETA.includes(rule.id) && <Chip tone="beta">{S.beta}</Chip>}</span>
        {POLICY_MEANING[rule.id] !== undefined && <span className="text-md text-text-dim">{POLICY_MEANING[rule.id]}</span>}
        <Scheduled rule={rule} gate={gate} flow={flow} />
      </span>
      <span className="flex flex-col items-start gap-1">
        <Dial name={label} value={flow.state(rule, gate)} change={(state) => void flow.ask(rule, { [gate]: state })} />
        {place !== undefined && (
          <button type="button" aria-label={`${move}: ${rule.name}`} onClick={place.toggle}
            className={`text-left text-md ${place.placed ? "text-link hover:underline" : "text-text-dim hover:text-link focus-visible:text-link"}`}>
            {move}
          </button>
        )}
      </span>
    </section>
  );
}

// An app's addition, drawn dashed and read here: its owner adds and removes it on the app.
function Added({ rule, one, state }: { rule: Policy; one: Addition; state: PolicyState }) {
  const name = `${rule.name}, ${one.project} only`;
  return (
    <section aria-label={name} className={`${ROW} border-dashed border-border-strong bg-surface-2`}>
      <span className={TEXT}>
        <span className="text-base font-medium">{rule.name}</span>
        <span className="text-md text-text-dim">{`${one.project} only · added by ${one.set_by.display}`}</span>
      </span>
      <Dial name={name} value={state} />
    </section>
  );
}

// Guardrails › Pipeline (D14): the two gates every version passes, Upload → Before preview → Preview
// → Before production → Production, each check one row at its state in that gate, and the apps'
// additions dashed beside them. An admin places a check before production by dragging it there or
// with its button, sets it in either gate with its Dial, and saves through the one save bar, which
// says what each change does and what it would keep from production today. Everyone else reads it.
export function Pipeline() {
  const [rows, setRows] = useState<Policy[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [carried, setCarried] = useState<Policy | null>(null);
  const flow = usePolicyChanges((policy) => setRows((list) => list && list.map((one) => (one.id === policy.id ? policy : one))));
  useEffect(() => {
    let live = true;
    getPolicies().then((answer) => { if (live) setRows(answer.policies); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, []);

  const goLive = (rule: Policy) => GO_LIVE.some((id) => id === rule.id);
  // Saved before production: in force there, or scheduled to be.
  const saved = (rule: Policy) => rule.production !== undefined || rule.scheduled_production !== undefined;
  // Before production lists the go-live rules and every rule saved or asked there, Off included, so a
  // change taking one out is drawn where it happens.
  const listed = (rule: Policy) => goLive(rule) || saved(rule) || flow.of(rule.id)?.change.production !== undefined;
  // In the gate as the screen shows it: what is asked of it there, else what is saved.
  const there = (rule: Policy) => {
    const asked = flow.of(rule.id)?.change.production;
    return asked === undefined ? saved(rule) : asked !== "off";
  };
  // Taking a check out sets it Off there, withdrawing an edit scheduled there too. Putting back one
  // that was saved there asks for what is saved, so the change drops to nothing; one never placed
  // goes there at its state before preview, Advisory where that is Off, and its Dial there sets it.
  const toggle = (rule: Policy) => {
    setCarried(null);
    const preview = flow.state(rule);
    void flow.ask(rule, { production: there(rule) ? "off" : saved(rule) ? rule.production : preview === "off" ? "advisory" : preview });
  };
  const drop = {
    over: (event: DragEvent) => { if (carried !== null) event.preventDefault(); },
    drop: (event: DragEvent) => { event.preventDefault(); if (carried !== null) toggle(carried); },
  };
  // The additions in a gate, each at its state there: on a go-live rule an addition's `state` is
  // before production, as the rule's is.
  const added = (list: Policy[], production: boolean) => list.flatMap((rule) => (rule.additions ?? []).flatMap((one) => {
    const state = goLive(rule) ? (production ? one.state : undefined) : production ? one.production : one.state;
    return state === undefined ? [] : [<Added key={`${rule.id} ${one.project}`} rule={rule} one={one} state={state} />];
  }));

  return (
    <section aria-label="Pipeline" className="grid gap-3">
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      {rows !== null && (
        <div className="relative grid grid-cols-[auto_minmax(0,1fr)_auto_minmax(0,1fr)_auto] items-start gap-3 max-[1100px]:grid-cols-1">
          {/* The line the stages sit on: across above 1100px, down below it. Every stage and gate is
              positioned after it, so each is drawn over it. */}
          <span aria-hidden="true" className="absolute inset-x-0 top-[27px] h-px bg-border-strong max-[1100px]:inset-x-auto max-[1100px]:inset-y-0 max-[1100px]:left-8 max-[1100px]:h-auto max-[1100px]:w-px" />
          <ol aria-label="Pipeline stages" className="contents">
            {STAGES.map(([stage, who, place]) => (
              <li key={stage} className={`${place} relative mt-3 flex flex-col items-center gap-1 max-[1100px]:mt-0 max-[1100px]:flex-row max-[1100px]:gap-2`}>
                <span className="rounded-full border border-border-strong bg-surface px-3 py-1 text-base font-semibold">{stage}</span>
                <span className="text-sm text-text-dim">{who}</span>
              </li>
            ))}
          </ol>
          <Card title="Before preview" line={S.beforePreview} place="order-2">
            {rows.filter((rule) => !goLive(rule)).map((rule) => (
              <Check key={rule.id} rule={rule} gate="state" label={rule.name} flow={flow}
                place={{ placed: there(rule), toggle: () => toggle(rule) }} carry={setCarried} />
            ))}
            {added(rows, false)}
          </Card>
          <Card title="Before production" line={S.beforeProduction} place="order-4 min-[1100px]:sticky min-[1100px]:top-4" drop={drop}>
            {carried !== null && (
              <p className="rounded-lg border border-dashed border-accent bg-accent-dim px-3 py-2.5 text-center text-md text-accent">{`Drop here: ${carried.name}, before production`}</p>
            )}
            {rows.filter(listed).map((rule) => (
              <Check key={rule.id} rule={rule} gate={goLive(rule) ? "state" : "production"} label={goLive(rule) ? rule.name : `${rule.name}, before production`} flow={flow} />
            ))}
            {added(rows, true)}
          </Card>
        </div>
      )}
      <SaveBar flow={flow} affects />
    </section>
  );
}
