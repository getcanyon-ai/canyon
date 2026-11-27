import { useEffect, useState } from "react";
import { BETA, getAuthConfig, getPolicies, getPreferences, previewPolicy, putPreferences, said, type Policy, type Preferences, type Preview } from "../api";
import { S } from "../strings";
import { Chip } from "../ui/Chip";
import { Dial } from "../ui/Dial";
import { Icon } from "../ui/Icon";
import { CustomRules } from "./CustomRules";
import { Impact, SaveBar, Scheduled, usePolicyChanges } from "./PolicyChange";
import { hashQuery } from "./query";
import { SCANNER_RULES } from "./Scanners";

// One sentence of what each shipped rule is about, beside its name on Guardrails › Policies. A rule
// with no line here (a newer server's) shows its name alone.
export const POLICY_MEANING: Record<string, string> = {
  "secrets.client-bundle": "A live key in code that people download or that ships in the image.",
  "sql.string-concat": "Queries made by pasting user input into SQL.",
  "prod.clean-gate": "Only versions that passed review reach production.",
  "prod.human-approval": "A person promotes, or their agent from the CLI. Settings can have a person approve each promotion in the dashboard.",
  "auth.unauthenticated-route": "App routes anyone can reach without signing in.",
  "deps.known-vuln": "Packages with published security advisories.",
  "stack.blessed": "Apps built on a stack the org approved.",
  "owner.required": "Someone who gets asked when it breaks.",
  "perf.unbounded-refetch": "Lists that read everything again on every keystroke.",
  "design.org-system": "Apps use components from the org's design system.",
  "code.injection": "User input that reaches commands, files, pages or URLs.",
  "iac.misconfig": "Dockerfiles, Kubernetes and Terraform that fail their benchmark checks.",
  "sarif.gitleaks": "Keys, tokens and passwords written into the code.",
};

// The rows this tab draws: the scanners are their own tab's, and custom rules their own section's.
const shown = (rule: Policy) => !SCANNER_RULES.includes(rule.id) && !rule.id.startsWith("custom.");

const COLUMNS = "grid grid-cols-[minmax(0,1fr)_280px_minmax(0,220px)] gap-x-6 max-[1100px]:grid-cols-1";
const ROW = "grid grid-cols-[minmax(0,1fr)_280px_minmax(0,220px)] items-start gap-x-6 gap-y-1.5 border-t border-border py-3.5 first:border-t-0 max-[1100px]:grid-cols-1";

// The row a link names (?rule=): marked, so the reader sees which of the rows they were sent to.
const LINKED = "-mx-3 rounded-[8px] bg-accent-dim px-3 shadow-[inset_3px_0_0_var(--color-accent)]"; // -mx-3 keeps its columns on the other rows

// Guardrails › Policies: the org's rules, one row each — the rule, what it is about, its one control,
// and what a change to it would block today — and one save bar for the page.
export function Policies() {
  const [rows, setRows] = useState<Policy[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  // A rule saved here replaces its row.
  const stored = (policy: Policy) => {
    setRows((list) => list && (list.some((one) => one.id === policy.id) ? list.map((one) => (one.id === policy.id ? policy : one)) : [...list, policy]));
  };
  const flow = usePolicyChanges(stored);

  // Read on mount, and again once a custom rule is saved: a new one joins the policies, Off.
  const [reads, setReads] = useState(0);
  useEffect(() => {
    let live = true;
    getPolicies().then(
      (answer) => {
        if (!live) return;
        setRows(answer.policies);
        setFailure(null);
      },
      (err: unknown) => { if (live) setFailure(said(err)); },
    );
    return () => { live = false; };
  }, [reads]);

  // Whether the org has picked no default design system, so design.org-system checks nothing
  // (design/specs/screens.md). The line is a pointer, not the rule: a read that fails draws no line.
  const [noDesignSystem, setNoDesignSystem] = useState(false);
  // The buildpacks Approved stacks only accepts, set on its row (#99): the record they are read from and saved to.
  const [prefs, setPrefs] = useState<Preferences | null>(null);
  useEffect(() => {
    let live = true;
    getPreferences().then((record) => { if (live) { setNoDesignSystem(record.design_system === null); setPrefs(record); } }, () => undefined);
    return () => { live = false; };
  }, []);

  // What each rule blocks today: the server's preview of the rule at its own state, one
  // per row, read again whenever the rows are A preview that fails says why in its row.
  // ponytail: one preview per rule per read; a count on GET /api/v1/policies if the rules grow many.
  const [today, setToday] = useState<Record<string, Preview | string>>({});
  useEffect(() => {
    if (rows === null) return;
    let live = true;
    setToday({});
    for (const rule of rows.filter(shown)) {
      previewPolicy(rule.id, rule.production === undefined ? { state: rule.state } : { state: rule.state, production: rule.production }).then(
        (answer) => { if (live) setToday((all) => ({ ...all, [rule.id]: answer })); },
        (err: unknown) => { if (live) setToday((all) => ({ ...all, [rule.id]: said(err) })); },
      );
    }
    return () => { live = false; };
  }, [rows]);

  // A checklist row on an app links here as #/guardrails?rule=<id>, and lands on that rule's row
  // once the rows are in — once, not again on every save.
  const ready = rows !== null;
  const linked = new URLSearchParams(hashQuery()).get("rule");
  useEffect(() => {
    if (ready && linked !== null) document.getElementById(`rule-${linked}`)?.scrollIntoView({ block: "center" });
  }, [ready]);

  // "Affects today" is drawn once it has something to say (#118): a rule's preview names an app or failed, or a change
  // is asked. A column of "No app is affected today." on every row is noise. Its track stays, so the dials hold their place.
  // Row by row: a line on every row, for one row's change, moves the control just tapped.
  const speaks = (rule: Policy) => {
    const now = today[rule.id];
    return flow.of(rule.id) !== undefined || typeof now === "string" || (now !== undefined && now.would_block.length > 0);
  };
  const affects = rows?.some(speaks) ?? false;
  return (
    <section aria-label="Policies" className="grid gap-3">
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      <div className={`${COLUMNS} text-sm font-semibold text-text-faint max-[1100px]:hidden`}>
        <span>Policy</span><span>Mode</span>{affects && <span>Affects today</span>}
      </div>
      <div>
        {rows?.filter(shown).map((rule) => {
          return (
            <section key={rule.id} id={`rule-${rule.id}`} aria-label={rule.name} aria-current={rule.id === linked ? "true" : undefined} className={rule.id === linked ? `${ROW} ${LINKED}` : ROW}>
              <span className="grid gap-0.5">
                <span className="flex flex-wrap items-center gap-x-2 text-base font-medium">{rule.name}{BETA.includes(rule.id) && <Chip tone="beta">{S.beta}</Chip>}</span>
                {POLICY_MEANING[rule.id] !== undefined && <span className="text-md text-text-dim">{POLICY_MEANING[rule.id]}</span>}
                {rule.id === "design.org-system" && noDesignSystem && (
                  <span className="text-md text-text-dim">Checks nothing until you <a className="text-link" href="#/guardrails/agents">pick a default design system</a>.</span>
                )}
                {rule.id === "stack.blessed" && prefs !== null && <Buildpacks record={prefs} adopt={setPrefs} />}
                <Scheduled rule={rule} flow={flow} />
              </span>
              <Dial name={rule.name} value={flow.state(rule)} change={(state) => void flow.ask(rule, { state })} />
              {speaks(rule) && <Impact flow={flow} rule={rule} today={today[rule.id]} />}
            </section>
          );
        })}
        {/* Not a policy: what Canyon does when a scanner can't open a file, and no setting changes it
            (contracts/finding.md: gate.unreadable is the gate's own rule, never a policy). Shown, locked, beside the rules. */}
        {rows !== null && (
          <section aria-label="Files Canyon can't read" className={ROW}>
            <span className="grid gap-0.5">
              <span className="text-base font-medium">Files Canyon can't read</span>
              <span className="text-md text-text-dim">A file the scanners can't open is never waved through.</span>
            </span>
            <span className="flex items-center gap-1.5 text-md text-text-dim"><Icon name="locked" size={14} />Always blocks until a person approves</span>
          </section>
        )}
        {rows !== null && <PushApprovers />}
      </div>
      {rows !== null && <CustomRules policies={rows} flow={flow} saved={() => setReads((n) => n + 1)} />}
      <SaveBar flow={flow} />
    </section>
  );
}

// The buildpacks Approved stacks only accepts (#99): every one an app's build ran, and any accepted that none runs now,
// each by its builder's name where it has one, else its id, ticked when accepted. Saved as it is ticked, sorted by id;
// a refused save says the server's words and keeps the record as it was.
function Buildpacks({ record, adopt }: { record: Preferences; adopt: (next: Preferences) => void }) {
  const [failure, setFailure] = useState<string | null>(null);
  const [saved, setSaved] = useState(false); // said for a moment after a tick lands (#100's review)
  useEffect(() => {
    if (!saved) return;
    const t = setTimeout(() => setSaved(false), 2000);
    return () => clearTimeout(t);
  }, [saved]);
  const approved = record.approved_buildpacks ?? [];
  const named = new Map((record.buildpacks_seen ?? []).map((bp) => [bp.id, bp.name ?? bp.id]));
  const offered = [...new Set([...(record.buildpacks_seen ?? []).map((bp) => bp.id), ...approved])].sort();
  const choose = (next: string[]) => putPreferences({ approved_buildpacks: next }).then((answer) => { adopt(answer); setFailure(null); setSaved(true); }, (err: unknown) => { setSaved(false); setFailure(said(err)); });
  return (
    <span className="mt-2 grid gap-1">
      <span className="text-md text-text-dim">Buildpacks this rule accepts. Canyon lists the ones it has seen apps build with.</span>
      {offered.length === 0 && <span className="text-md text-text-faint">No app has been built yet.</span>}
      {offered.map((id) => (
        <label key={id} className="flex items-center gap-2 text-md">
          <input type="checkbox" checked={approved.includes(id)} onChange={(event) => void choose(event.target.checked ? [...approved, id].sort() : approved.filter((one) => one !== id))} />
          {named.get(id) ?? id}
        </label>
      ))}
      <span role="status" className="text-md text-ok">{saved ? "Saved" : ""}</span>
      {failure !== null && <span className="text-md text-critical">{failure}</span>}
    </span>
  );
}

// Not a policy either: who approves a GitHub push's blocking finding (#2342). Off by default; saved as it is
// changed, an admin's from a browser, and never turned on with sign-in off, where nobody links an account.
function PushApprovers() {
  const [on, setOn] = useState<boolean | null>(null);
  const [signIn, setSignIn] = useState(true);
  const [failure, setFailure] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    Promise.all([getPreferences(), getAuthConfig()]).then(([record, config]) => {
      if (live) { setOn(record.push_approvers_linked); setSignIn(config.auth === "oidc"); }
    }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, []);
  const choose = (next: boolean) => putPreferences({ push_approvers_linked: next }).then(
    (record) => { setOn(record.push_approvers_linked); setFailure(null); },
    (err: unknown) => setFailure(said(err)),
  );
  return (
    <section aria-label={S.pushApproversTitle} className={ROW}>
      <span className="grid gap-0.5">
        <span className="text-base font-medium">{S.pushApproversTitle}</span>
        <span className="text-md text-text-dim">{S.pushApproversMeaning}</span>
        {failure !== null && <span className="text-md text-critical">{failure}</span>}
      </span>
      {on === null ? <span /> : !signIn && !on ? <span className="text-md text-text-dim">Needs sign-in</span> : (
        // Off or On, drawn as a rule's Dial is drawn: one segmented choice, a radio group under the row's name
        <span role="radiogroup" aria-label={S.pushApproversTitle} className="inline-flex flex-none self-start justify-self-start rounded-[7px] border border-border-strong bg-surface p-0.5">
          {([false, true] as const).map((state) => (
            <label key={String(state)} className={`cursor-pointer rounded-[5px] px-2.5 py-1 text-sm has-[:focus-visible]:ring-2 has-[:focus-visible]:ring-accent max-[860px]:py-2 ${state === on ? `font-semibold ${state ? "bg-accent-dim text-accent" : "bg-surface-2 text-text shadow-[inset_0_0_0_1px_var(--color-border-strong)]"}` : "font-medium text-text-faint hover:text-text"}`}>
              <input type="radio" className="sr-only" name="push-approvers-linked" value={state ? "on" : "off"} checked={state === on} onChange={() => void choose(state)} />
              {state ? "On" : "Off"}
            </label>
          ))}
        </span>
      )}
      <span />
    </section>
  );
}
