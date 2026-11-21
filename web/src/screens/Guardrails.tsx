import { useEffect, useRef, useState } from "react";
import { createDesignSystem, getDesignSystems, getEgress, getPreferences, getSkillManifest, putPreferences, said, type DesignSystemRow, type Manifest, type Preferences, type Stack } from "../api";
import { Button } from "../ui/Button";
import { Rail } from "../ui/Rail";
import { isAdmin } from "./OrgSettings";
import { Pipeline } from "./Pipeline";
import { Playbooks } from "./Playbooks";
import { Policies } from "./Policies";
import { hashQuery } from "./query";
import { SCANNER_RULES, Scanners } from "./Scanners";
import { SetupPanel } from "./SetupPanel";
import { SkillSection } from "./SkillSection";

const H2 = "mt-5 text-base font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const LABEL = "mb-1 block text-md font-semibold text-text-dim";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// Registering adds a row to the org's registry. It does not make it the org's default — that is the
// Save beside the picker, a different route and a different audit row.
function Register({ done, close }: { done: (row: DesignSystemRow) => void; close: () => void }) {
  const [form, setForm] = useState({ name: "", namespace: "", registry_url: "", tokens_url: "" });
  const [refusal, setRefusal] = useState<string | null>(null);
  const sending = useRef(false);

  const send = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      // An empty tokens URL is the key left out, never "" and never null: the server reads an absent
      // key as unset and refuses a null.
      const answer = await createDesignSystem({ ...form, tokens_url: form.tokens_url === "" ? undefined : form.tokens_url });
      done(answer.design_system);
    } catch (err) {
      setRefusal(said(err));
    } finally {
      sending.current = false;
    }
  };

  const field = (key: keyof typeof form, label: string) => (
    <>
      <label className={LABEL} htmlFor={`ds-${key}`}>{label}</label>
      <input id={`ds-${key}`} className={FIELD} value={form[key]} onChange={(event) => setForm({ ...form, [key]: event.target.value })} />
    </>
  );

  return (
    <div className="mt-3 flex max-w-[520px] flex-col gap-1.5">
      {field("name", "Name")}
      {field("namespace", "Namespace")}
      {field("registry_url", "Registry URL")}
      {field("tokens_url", "Tokens URL (optional)")}
      {refusal !== null && <p className="text-base text-critical">{refusal}</p>}
      <span className="mt-1 flex flex-wrap gap-2">
        <Button variant="primary" onClick={() => void send()}>Register</Button>
        <Button variant="ghost" onClick={close}>Cancel</Button>
      </span>
    </div>
  );
}

// Guardrails' four pages (the redesign: "Only the rules live here now"): the pipeline, the policies,
// the scanners, and what coding agents are told. Each is #/guardrails/<page>; the bare route is the
// first, Pipeline, and a link to a rule's row (?rule=) is the tab that rule's row is on.
const PAGES = [["pipeline", "Pipeline"], ["policies", "Policies"], ["scanners", "Scanners"], ["playbooks", "Playbooks"], ["agents", "What agents are told"]] as const;
const pageOf = (hash: string) => {
  const rule = new URLSearchParams(hashQuery()).get("rule");
  if (rule !== null) return SCANNER_RULES.includes(rule) ? "scanners" : "policies";
  return PAGES.find(([key]) => hash.startsWith(`#/guardrails/${key}`))?.[0] ?? "pipeline";
};

// The org's guardrails: the rules every app must pass, the stacks every agent is handed, and the
// registry their components come from. Nothing on this form is checked here — the bounds and the refusals are the server's, and the
// admin reads them in the server's own sentence.
export function Guardrails() {
  const [record, setRecord] = useState<Preferences | null>(null);
  const [rows, setRows] = useState<DesignSystemRow[] | null>(null);
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const [outbound, setOutbound] = useState(false);
  const [stacks, setStacks] = useState<Stack[]>([]);
  const [approved, setApproved] = useState<string[]>([]);
  const [chosen, setChosen] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [registering, setRegistering] = useState(false);
  const sending = useRef(false);
  // The panel's box, held here: the parent owns the button that moves focus to it.
  const box = useRef<HTMLTextAreaElement>(null);
  // Keyed on the drafts, not the record, so the line goes the moment Add a stack is clicked and
  // never claims "nothing" while a design system is set.
  const nothing = stacks.length === 0 && chosen === "";
  const admin = record !== null && (isAdmin(record) || true); // FAULT (red first): every member opens Guardrails
  const page = pageOf(window.location.hash);

  // The answer is the record. The read on mount and the answer to a save both land here, so the form
  // holds what the server stored and never what was typed at it.
  const adopt = (answer: Preferences) => {
    setRecord(answer);
    setStacks(answer.stacks);
    setApproved(answer.approved_buildpacks);
    setChosen(answer.design_system?.id ?? "");
  };

  // Read once. No poll: this screen is a form, and an interval would overwrite the row being typed.
  // Retry beside a failed read is the same four reads, run again by hand. A read that lands after
  // the screen is gone sets nothing. The host's via says whether agents are told of outbound, as
  // canyon manifest reads it: a via read that fails tells them nothing of it, and fails nothing here.
  const live = useRef(true);
  const load = () => Promise.all([getPreferences(), getDesignSystems(), getSkillManifest(), getEgress().then((host) => host.via === "canyon", () => false)]).then(
    ([answer, registry, handed, through]) => { if (live.current) { adopt(answer); setRows(registry.design_systems); setManifest(handed); setOutbound(through); setFailure(null); } },
    (err: unknown) => { if (live.current) setFailure(said(err)); }, // the server's words, as a re-read says them
  );

  // The two re-reads, each owning its own read's failure so neither can throw the other's answer
  // away. The manifest is derived from the row the form writes, so it is stale on screen until the
  // row is read back.
  const pullPreferences = () => getPreferences().then(adopt, (err: unknown) => setFailure(said(err)));
  const pullManifest = () => getSkillManifest().then(setManifest, (err: unknown) => setFailure(said(err)));

  // StrictMode runs setup, cleanup, setup on one instance in dev, and the ref outlives the cleanup:
  // the setup that follows has to turn it back on or every read lands on a screen that is "gone".
  useEffect(() => { live.current = true; void load(); return () => { live.current = false; }; }, []);

  const put = (at: number, next: Stack) => {
    setStacks((list) => list.map((one, i) => (i === at ? next : one)));
    setSaved(false);
  };
  const drop = (at: number) => { setStacks((list) => list.filter((_, i) => i !== at)); setSaved(false); };

  // One Save for the whole form, both keys every time. A failed save keeps what was typed: you need
  // it to fix what the server refused.
  const save = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      adopt(await putPreferences({ stacks, approved_buildpacks: approved, design_system_id: chosen === "" ? null : chosen }));
      setRefusal(null);
      setFailure(null); // a read that failed before this save is stale: the manifest is read again below
      // The PUT's answer is the record, so the form needs no read; the manifest is built from the
      // row that PUT just wrote, so it does. It cannot reject: a manifest that fails leaves its
      // sentence above a form that still says Saved., because the save did land.
      await pullManifest();
      setSaved(true);
    } catch (err) {
      setSaved(false); // a refusal and "Saved." never share the line
      setRefusal(said(err));
    } finally {
      sending.current = false;
    }
  };

  // After every setup turn, whatever it answered: the form is the record, and a turn that failed can
  // still have written. It replaces the form's draft — unsaved typing is lost when a turn lands, and
  // the person just asked the chat to change the same row. A failed re-read says so above a form
  // that stays.
  const reread = () => {
    setSaved(false);
    setRefusal(null); // the form's refusal described input the re-read replaces
    setFailure(null); // cleared here, so neither read clears it and the failing one's sentence stays
    return Promise.all([pullPreferences(), pullManifest()]).then(() => undefined);
  };

  // An admin's screen (design/specs/screens.md, A4b): a member who opens it is told so, and shown nothing of it.
  if (record !== null && !admin) {
    return (
      <div className="flex min-h-screen items-start max-[860px]:block">
        <Rail current="guardrails" />
        <main className="mx-auto max-w-[1280px] min-w-0 flex-1 px-8 pt-3.5 pb-24 max-[860px]:px-4">
          <h1 className="text-title font-semibold tracking-tight">Guardrails</h1>
          <p className="mt-3 text-base">Only an admin can open Guardrails.</p>
          <p className={SUB}>What an app must pass is on that app's page.</p>
          <a className="mt-3 inline-block text-base text-link no-underline hover:underline" href="#/fleet">Back to the fleet</a>
        </main>
      </div>
    );
  }
  return (
    <div className="flex min-h-screen items-start max-[860px]:block">
      <Rail current="guardrails" />
      <main className="mx-auto max-w-[1280px] min-w-0 flex-1 px-8 pt-3.5 pb-24 max-[860px]:px-4">
        <h1 className="text-title font-semibold tracking-tight">Guardrails</h1>
        <p className={SUB}>What every app must pass before it reaches production, and what coding agents are told.</p>
        {record !== null && <nav aria-label="Guardrails" className="mt-4 flex gap-5 overflow-x-auto border-b border-border max-[520px]:gap-4">
          {PAGES.map(([key, label]) => (
            <a key={key} href={`#/guardrails/${key}`} aria-current={page === key ? "page" : undefined}
              className={`py-2.5 text-base font-medium whitespace-nowrap no-underline ${page === key ? "text-text shadow-[inset_0_-2px_0_var(--color-accent)]" : "text-text-dim hover:text-text"}`}>
              {label}
            </a>
          ))}
        </nav>}
        {failure !== null && (
          <span className="mt-2.5 flex flex-wrap items-center gap-2">
            <span className="text-base text-critical">{failure}</span>
            <Button variant="ghost" size="sm" onClick={() => void load()}>Retry</Button>
          </span>
        )}
        {record !== null && page === "pipeline" && <div className="mt-4"><Pipeline /></div>}
        {record !== null && page === "policies" && <div className="mt-4"><Policies /></div>}
        {record !== null && page === "scanners" && <div className="mt-4"><Scanners /></div>}
        {record !== null && page === "playbooks" && <div className="mt-4"><Playbooks /></div>}
        {record !== null && rows !== null && page === "agents" && (
          <>
            <section aria-label="Approved stacks">
              <h2 className={H2}>Approved stacks</h2>
              <p className={SUB}>Agents are told to use these stacks. This list guides agents and blocks nothing: Approved stacks only checks the buildpacks below.</p>
              {nothing && <p className="mt-2.5 text-base text-text-dim">Nothing configured yet.</p>}
              {/* Ordinal labels, so a row keeps its name while its language is being typed. */}
              {stacks.map((one, i) => (
                <section key={i} aria-label={`Stack ${i + 1}`} className={CARD}>
                  <label className={LABEL} htmlFor={`stack-${i}-language`}>Language</label>
                  <input id={`stack-${i}-language`} className={FIELD} value={one.language} onChange={(event) => put(i, { ...one, language: event.target.value })} />
                  <p className={`${LABEL} mt-3`}>Frameworks</p>
                  <div className="flex flex-wrap items-center gap-2">
                    {one.frameworks.map((framework, j) => (
                      <span key={j} className="flex items-center gap-1">
                        <input aria-label={`Framework ${j + 1}`} className={FIELD} value={framework}
                          onChange={(event) => put(i, { ...one, frameworks: one.frameworks.map((was, at) => (at === j ? event.target.value : was)) })} />
                        <button type="button" className="text-base text-link" aria-label={`Remove framework ${j + 1}`}
                          onClick={() => put(i, { ...one, frameworks: one.frameworks.filter((_, at) => at !== j) })}>×</button>
                      </span>
                    ))}
                  </div>
                  <span className="mt-2.5 flex flex-wrap gap-2">
                    <Button variant="ghost" size="sm" onClick={() => put(i, { ...one, frameworks: [...one.frameworks, ""] })}>Add a framework</Button>
                    <Button variant="ghost" size="sm" onClick={() => drop(i)}>Remove stack</Button>
                  </span>
                </section>
              ))}
              <span className="mt-3 flex flex-wrap gap-2">
                {/* The same words as the panel's heading, on purpose: the button says where it goes. */}
                {nothing && record.setup_available && <Button variant="primary" onClick={() => box.current?.focus()}>Set up with Canyon</Button>}
                <Button onClick={() => { setStacks((list) => [...list, { language: "", frameworks: [""] }]); setSaved(false); }}>Add a stack</Button>
              </span>
            </section>
            <Buildpacks record={record} approved={approved} choose={(next) => { setApproved(next); setSaved(false); }} />
            <section aria-label="Design system">
              <h2 className={H2}>Design system</h2>
              <p className={SUB}>Components come from this registry. Each review checks that an app's package.json lists it (its components.json, for shadcn/ui and Canyon); an app that doesn't gets an advisory finding, unless Policies makes the rule blocking.</p>
              <label className={`${LABEL} mt-3`} htmlFor="org-default">Default design system</label>
              <select id="org-default" className={FIELD} value={chosen} onChange={(event) => { setChosen(event.target.value); setSaved(false); }}>
                <option value="">None</option>
                {rows.map((row) => <option key={row.id} value={row.id}>{`${row.name} · ${row.namespace}`}</option>)}
              </select>
              {/* The server's resolved row, not a lookup of the draft: these are the URLs an agent is
                  handed, and they change when the save lands, not when the picker moves. */}
              {record.design_system !== null && (
                <p className="mt-2 font-mono text-sm text-text-faint">
                  {record.design_system.registry_url}
                  {record.design_system.tokens_url !== undefined && ` · ${record.design_system.tokens_url}`}
                </p>
              )}
              {registering
                ? <Register close={() => setRegistering(false)} done={(row) => { setRows([...rows, row]); setChosen(row.id); setSaved(false); setRegistering(false); }} />
                : <button type="button" className="mt-3 block text-base text-link" onClick={() => setRegistering(true)}>Add a design system</button>}
            </section>
            <span className="mt-5 flex flex-wrap items-center gap-3">
              <Button variant="primary" onClick={() => void save()}>Save</Button>
              {saved && <span role="status" className="text-base text-ok">Saved.</span>}
              {refusal !== null && <span className="text-base text-critical">{refusal}</span>}
            </span>
            {/* Above the panel: the form, then what the form produces, then the chat that edits the
                form — the transcript grows without bound and would push this off the screen. */}
            {manifest !== null && <SkillSection manifest={manifest} org={record.org_name} outbound={outbound} />}
            {record.setup_available && <SetupPanel box={box} refresh={reread} />}
          </>
        )}
      </main>
    </div>
  );
}

// The buildpacks "Approved stacks only" approves: every one an app's deploy ran, and any approved that
// none runs now, each by its builder's name where it has one, else its id, ticked when approved. The
// list is saved with the form.
function Buildpacks({ record, approved, choose }: { record: Preferences; approved: string[]; choose: (next: string[]) => void }) {
  const named = new Map(record.buildpacks_seen.map((bp) => [bp.id, bp.name ?? bp.id]));
  const offered = [...new Set([...record.buildpacks_seen.map((bp) => bp.id), ...approved])].sort();
  const label = (id: string) => named.get(id) ?? id;
  return (
    <section aria-label="Approved buildpacks">
      <h2 className={H2}>Approved buildpacks</h2>
      <p className={SUB}>Approved stacks only passes when every buildpack an app's build ran is approved here. These are the buildpacks Canyon has seen apps build with.</p>
      {offered.length === 0 && <p className="mt-2.5 text-base text-text-dim">No app has been built yet.</p>}
      <ul className="mt-2.5 grid gap-1.5">
          {offered.map((id) => (
            <li key={id}>
              <label className="flex items-center gap-2 text-base">
                <input type="checkbox" checked={approved.includes(id)}
                  onChange={(event) => choose(event.target.checked ? [...approved, id].sort() : approved.filter((one) => one !== id))} />
                {label(id)}
              </label>
            </li>
          ))}
      </ul>
    </section>
  );
}
