import { useEffect, useId, useState } from "react";
import { dryRunCustomRule, getCustomRules, getPlaybooks, putCustomRule, said, type CustomRule, type DryRunApp, type PlaybookEntry, type Policy, type PolicyState } from "../api";
import { S } from "../strings";
import { shortDate } from "../ui/Verdict";
import { actorName } from "../ui/Actor";
import { Button } from "../ui/Button";
import { Dial } from "../ui/Dial";
import { Impact, Scheduled, type Flow } from "./PolicyChange";

const H3 = "mt-6 text-base font-semibold";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";
const NAME_MAX = 80;
const TEXT_MAX = 2000;

// A rule's slug from its name, fixed once the rule exists (contracts/snapshot.md, Custom rules): the
// name lowercased, each run of other characters one hyphen, letters and hyphens only, at most 40.
export const slugOf = (name: string) =>
  name.toLowerCase().replace(/[^a-z]+/g, "-").replace(/^-+/, "").slice(0, 40).replace(/-+$/, "");


// The org's own rules, written in plain words and checked on every review, by code where Canyon can and
// by its AI otherwise: each one installs Off and takes the policies' dial.
export function CustomRules({ policies, flow, saved }: { policies: Policy[]; flow: Flow; saved: () => void }) {
  const [rules, setRules] = useState<CustomRule[] | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [editing, setEditing] = useState<CustomRule | "new" | null>(null);
  const [reads, setReads] = useState(0);
  const [why, setWhy] = useState<{ rule: string; text: string } | null>(null); // why a save asked for code stays with the AI
  const [tried, setTried] = useState<{ rule: string; apps?: DryRunApp[]; refusal?: string } | null>(null); // the latest dry run, one rule's
  const tryOn = (rule: string) => {
    setTried({ rule });
    dryRunCustomRule(rule.slice("custom.".length)).then(
      (answer) => setTried((now) => (now?.rule === rule ? { rule, apps: answer.apps } : now)),
      (err: unknown) => setTried((now) => (now?.rule === rule ? { rule, refusal: said(err) } : now)),
    );
  };
  useEffect(() => {
    let live = true;
    getCustomRules().then(
      (answer) => { if (live) setRules(answer.custom_rules); },
      (err: unknown) => { if (live) setRefusal(said(err)); },
    );
    return () => { live = false; };
  }, [reads]);
  const done = (answer?: { custom_rule: CustomRule; why?: string }) => {
    setWhy(answer?.why ? { rule: answer.custom_rule.rule, text: answer.why } : null);
    setEditing(null); setReads((n) => n + 1); saved();
    if (answer?.custom_rule.check.by === "code") tryOn(answer.custom_rule.rule); // on save, which apps it would pass and fail
    else setTried(null);
  };
  return (
    <section aria-label="Custom rules">
      <h3 className={H3}>Custom rules</h3>
      <p className="mt-1 text-md text-text-dim">Your org's own rules, checked on every review: by code where what Canyon reads of an app decides the rule, and by Canyon's AI otherwise. A new rule starts Off: start in Advisory to see what it finds.</p>
      {refusal !== null && <p className="mt-2.5 text-base text-critical">{refusal}</p>}
      {rules?.map((rule) => {
        const policy = policies.find((one) => one.id === rule.rule);
        if (editing !== "new" && editing?.rule === rule.rule) return <Editor key={rule.rule} rule={rule} taken={[]} done={done} cancel={() => setEditing(null)} />;
        return (
          <section key={rule.rule} id={`rule-${rule.rule}`} aria-label={rule.name} className={CARD}>
            <span className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-base">{rule.name}</span>
              {policy !== undefined && <Dial name={rule.name} value={flow.state(policy)} change={(state: PolicyState) => void flow.ask(policy, { state })} />}
            </span>
            <p className="mt-2 text-md break-words whitespace-pre-wrap">{rule.instructions}</p>
            <p className="mt-1 text-md">{rule.check.by === "code" ? S.checkedByCode.replace("<says>", rule.check.says ?? "") : S.checkedByAI}</p>
            {rule.playbook !== null && <p className="mt-1 text-md">{S.rulePlaybookLine.replace("<slug>", rule.playbook)}</p>}
            {why?.rule === rule.rule && <p role="status" className="mt-1 text-sm text-text-dim">{why.text}</p>}
            {tried?.rule === rule.rule && <DryRun apps={tried.apps} refusal={tried.refusal} name={rule.name} />}
            <p className="mt-1 text-sm text-text-dim">{`Written by ${actorName(rule.set_by)} · version ${rule.version} · changed ${shortDate(rule.set_at)}`}</p>
            <span className="mt-1 flex flex-wrap gap-3">
              <button type="button" className="text-link" onClick={() => setEditing(rule)}>Edit</button>
              <Switch rule={rule} done={done} refused={setRefusal} />
              {rule.check.by === "code" && <button type="button" className="text-link" aria-label={`${S.dryRun}: ${rule.name}`} onClick={() => tryOn(rule.rule)}>{S.dryRun}</button>}
              <a className="text-link no-underline hover:underline" href={`#/audit?rule=${encodeURIComponent(rule.rule)}`}>History</a>
            </span>
            {policy !== undefined && <Scheduled rule={policy} flow={flow} />}
            {policy !== undefined && <Impact flow={flow} rule={policy} />}
          </section>
        );
      })}
      {rules !== null && (editing === "new"
        ? <Editor taken={rules.map((one) => one.rule)} done={done} cancel={() => setEditing(null)} />
        : <span className="mt-3 block"><Button onClick={() => setEditing("new")}>New rule</Button></span>)}
    </section>
  );
}

// The editor: a name and what Canyon's AI should look for, each with its count. Save writes a new
// version — a new rule's first, installed Off — and the server says what it refuses, in its words. A
// new rule's id comes from its name and is shown; one that names a rule already written (taken) is
// refused here, as saving it would edit that rule.
// Switch saves the rule's next version checked the other way: by the AI, with no model call, or by code
// where Canyon can, which asks the model again and stays with the AI when it can't (contracts/routes.md).
// DryRun is a dry run's answer under its rule: each app would pass, would fail, or was not checked and why.
function DryRun({ apps, refusal, name }: { apps?: DryRunApp[]; refusal?: string; name: string }) {
  if (refusal !== undefined) return <p className="mt-1 text-sm text-critical">{refusal}</p>;
  if (apps === undefined) return <p role="status" className="mt-1 text-sm text-text-dim">{S.dryRunning}</p>;
  const line = (app: DryRunApp) => (app.result === "pass" ? S.dryRunPass : app.result === "fail" ? S.dryRunFail : S.dryRunNotChecked)
    .replace("<app>", app.project).replace("<why>", app.why ?? "");
  return (
    <ul aria-label={`${name} on your apps`} className="mt-1 text-sm">
      {apps.map((app) => <li key={app.project} className={app.result === "fail" ? "text-critical" : app.result === "pass" ? "" : "text-text-dim"}>{line(app)}</li>)}
    </ul>
  );
}

function Switch({ rule, done, refused }: { rule: CustomRule; done: (answer: { custom_rule: CustomRule; why?: string }) => void; refused: (message: string) => void }) {
  const [saving, setSaving] = useState(false);
  const to = rule.check.by === "code" ? "ai" : "code";
  const label = to === "ai" ? S.checkWithAI : S.checkByCode;
  const save = async () => {
    setSaving(true);
    try {
      done(await putCustomRule(rule.rule.slice("custom.".length), { name: rule.name, instructions: rule.instructions, check: to, playbook: rule.playbook }));
    } catch (err) {
      refused(said(err));
    }
    setSaving(false);
  };
  return <button type="button" className="text-link" disabled={saving} aria-label={`${label}: ${rule.name}`} onClick={() => void save()}>{saving ? "Saving…" : label}</button>;
}

function Editor({ rule, taken, done, cancel }: { rule?: CustomRule; taken: string[]; done: (answer: { custom_rule: CustomRule; why?: string }) => void; cancel: () => void }) {
  const [name, setName] = useState(rule?.name ?? "");
  const [text, setText] = useState(rule?.instructions ?? "");
  const [playbook, setPlaybook] = useState(rule?.playbook ?? "");
  const [playbooks, setPlaybooks] = useState<PlaybookEntry[] | null>(null); // the save waits for them: it sends the field whole
  const [refusal, setRefusal] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    getPlaybooks().then((answer) => { if (live) setPlaybooks(answer.playbooks); }, (err: unknown) => { if (live) setRefusal(said(err)); });
    return () => { live = false; };
  }, []);
  const [saving, setSaving] = useState(false);
  const id = useId();
  const slug = rule ? rule.rule.slice("custom.".length) : slugOf(name);
  const save = async () => {
    if (slug === "") { setRefusal("The name needs at least one letter a–z: it becomes the rule's id."); return; }
    if (taken.includes(`custom.${slug}`)) { setRefusal("A rule with this id already exists: edit that one instead."); return; }
    setSaving(true);
    try {
      done(await putCustomRule(slug, { name, instructions: text, playbook: playbook === "" ? null : playbook }));
    } catch (err) {
      setRefusal(said(err));
      setSaving(false);
    }
  };
  return (
    <section aria-label={rule ? `Edit ${rule.name}` : "New rule"} className={CARD}>
      <label className="text-md text-text-dim" htmlFor={`${id}-name`}>Name</label>
      <input id={`${id}-name`} aria-describedby={`${id}-name-count`} className={`${FIELD} mt-1 w-full`} maxLength={NAME_MAX} value={name} onChange={(event) => setName(event.target.value)} />
      <span id={`${id}-name-count`} className="text-sm text-text-faint">{`${name.length} / ${NAME_MAX}${rule || slug === "" ? "" : ` · its id: custom.${slug}`}`}</span>
      <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-text`}>What should Canyon check?</label>
      <textarea id={`${id}-text`} aria-describedby={`${id}-text-count`} className={`${FIELD} mt-1 w-full`} rows={5} maxLength={TEXT_MAX} value={text} onChange={(event) => setText(event.target.value)}
        placeholder="Describe one problem in one app's code. Say what counts and what doesn't. Example: Flag code that writes a customer's email address to a database column or file without encrypting it. Hashing for lookup is fine." />
      <span id={`${id}-text-count`} className="text-sm text-text-faint">{`${text.length.toLocaleString("en")} / ${TEXT_MAX.toLocaleString("en")}`}</span>
      <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-playbook`}>{S.rulePlaybook}</label>
      <select id={`${id}-playbook`} aria-describedby={`${id}-playbook-means`} className={`${FIELD} mt-1`} value={playbook} onChange={(event) => setPlaybook(event.target.value)}>
        <option value="">None</option>
        {playbooks?.map((one) => <option key={one.slug} value={one.slug}>{one.name}</option>)}
        {playbook !== "" && playbooks?.some((one) => one.slug === playbook) === false && <option value={playbook}>{playbook}</option>}
      </select>
      <span id={`${id}-playbook-means`} className="block text-sm text-text-faint">{S.rulePlaybookMeans}</span>
      {refusal !== null && <p className="mt-2 text-base text-critical">{refusal}</p>}
      <span className="mt-2 flex gap-2">
        {saving || playbooks === null
          ? <Button disabled reason={saving ? "Saving: Canyon is working out how to check it, which can take a few seconds." : "Reading the org's playbooks."}>Save</Button>
          : <Button variant="primary" label={rule ? `Save ${rule.name}` : "Save the new rule"} onClick={() => void save()}>Save</Button>}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </section>
  );
}
