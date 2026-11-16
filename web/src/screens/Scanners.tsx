import { useEffect, useRef, useState } from "react";
import { applyScanner, getCredential, getPolicies, removeCredential, said, setCredential, type Credential, type Policy, type PolicyState, type ToolSettings } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age } from "../ui/Age";
import { Button } from "../ui/Button";
import { Dial } from "../ui/Dial";
import { Impact, SaveBar, Scheduled, usePolicyChanges, type Flow } from "./PolicyChange";
import { hashQuery } from "./query";

const LABEL = "mt-3 mb-1 block text-md font-semibold text-text-dim";
const FIELD = "max-w-[640px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-1 max-w-[720px] text-md text-text-dim";
const CARD = "mt-1 rounded-xl border border-border bg-surface-2 px-3.5 py-3";
// The Policies tab's row: what it is, its one control, and what a change would block today.
const ROW = "grid grid-cols-[minmax(0,1fr)_280px_minmax(0,220px)] items-start gap-x-6 gap-y-1.5 border-t border-border py-3.5 first:border-t-0 max-[860px]:grid-cols-1";

// COMMUNITY is SIG's maintained fork of the community Opengrep rules, at a commit pinned here, and the
// folders of the three languages it adds: a rules repo an organisation may choose, never one Canyon
// ships (DECISIONS 2026-10-01, Commons Clause). The folders keep the repo's statistics YAML out.
const COMMUNITY = { repo: "https://github.com/Software-Improvement-Group/opengrep-community-rules", commit: "fd85cc8cdb2156123607afbe51005463f692389a", paths: ["java", "php", "ruby"] };

// The five tools Canyon runs through its executor and the settings each takes (contracts/routes.md, the policies
// row). Their rules are configured on this tab, not on Policies, so each has one control.
type List = "skip_paths" | "frameworks" | "skip_checks";
const TOOLS: { id: string; name: string; about: string; lists: List[] }[] = [
  { id: "trivy", name: "Trivy", about: "Packages with known advisories, and misconfiguration", lists: ["skip_paths"] },
  { id: "opengrep", name: "Opengrep", about: "Injection, unsafe calls and other code patterns", lists: ["skip_paths"] },
  { id: "eslint", name: "ESLint", about: "Security rules for JavaScript and TypeScript", lists: ["skip_paths"] },
  { id: "grype", name: "Grype", about: "Packages with known advisories", lists: ["skip_paths"] },
  { id: "checkov", name: "Checkov", about: "Dockerfiles, Kubernetes and Terraform", lists: ["frameworks", "skip_checks", "skip_paths"] },
];
export const SCANNER_RULES = TOOLS.map((tool) => `sarif.${tool.id}`);
const LIST_LABEL: Record<List, string> = { skip_paths: "Paths to skip", frameworks: "Frameworks", skip_checks: "Checks to skip" };
const TRIVY_SCANNERS = [["vuln", "Vulnerabilities"], ["misconfig", "Misconfiguration"], ["secret", "Secrets"]] as const;

// A list is typed one per line; blank lines are dropped, and an empty list is no key at all.
const lines = (text: string) => text.split("\n").map((one) => one.trim()).filter((one) => one !== "");

// Guardrails › Scanners: each tool's findings (its rule's state, changed through the page's one save
// bar), whether Canyon runs it on this host, and its settings, saved together. The shapes and
// refusals are the server's.
export function Scanners() {
  const [rules, setRules] = useState<Policy[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const saved = (policy: Policy) => setRules((list) => list && (list.some((one) => one.id === policy.id) ? list.map((one) => (one.id === policy.id ? policy : one)) : [...list, policy]));
  const flow = usePolicyChanges(saved);
  useEffect(() => {
    let live = true;
    getPolicies().then((answer) => { if (live) setRules(answer.policies.filter((rule) => SCANNER_RULES.includes(rule.id))); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, []);
  // A checklist row links a tool's rule as #/guardrails?rule=sarif.<tool>, and lands on its row once.
  const ready = rules !== null;
  useEffect(() => {
    const rule = new URLSearchParams(hashQuery()).get("rule");
    if (ready && rule !== null) document.getElementById(`rule-${rule}`)?.scrollIntoView();
  }, [ready]);
  return (
    <section aria-label="Scanners" className="grid gap-3">
      <p className={NOTE}>{S.scanners}</p>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      {rules !== null && TOOLS.map((tool) => {
        const rule = rules.find((one) => one.id === `sarif.${tool.id}`);
        return <Tool key={tool.id} tool={tool} rule={rule} saved={saved} flow={flow} />;
      })}
      <SaveBar flow={flow} />
    </section>
  );
}

function Tool({ tool, rule, saved, flow }: { tool: (typeof TOOLS)[number]; rule?: Policy; saved: (policy: Policy) => void; flow: Flow }) {
  const settings = rule?.settings ?? {};
  // An added tool's findings change as any policy's does: previewed, then saved through the page's
  // save bar. A tool not yet added has nothing to preview (the server answers no app), so its state
  // rides with its own Save.
  const [refusal, setRefusal] = useState<string | null>(null);
  const named = rule && { ...rule, name: tool.name };
  const [adding, setAdding] = useState<PolicyState>("off");
  const [run, setRun] = useState(rule?.run === true);
  const [scanners, setScanners] = useState<string[]>(settings.scanners ?? []);
  const [repo, setRepo] = useState(settings.rules?.repo ?? "");
  const [commit, setCommit] = useState(settings.rules?.commit ?? "");
  const [folders, setFolders] = useState((settings.rules?.paths ?? []).join("\n"));
  const [lists, setLists] = useState<Record<List, string>>({ skip_paths: "", frameworks: "", skip_checks: "", ...Object.fromEntries(tool.lists.map((key) => [key, (settings[key] ?? []).join("\n")])) });
  const [done, setDone] = useState(false);
  // A tool not yet added opens on its settings: there is nothing else to show.
  const [configuring, setConfiguring] = useState(rule === undefined);
  const sending = useRef(false);
  const id = (field: string) => `${tool.id}-${field}`;

  // Settings go whole: what the card holds replaces what was set, and nothing at all clears it.
  const body = (): ToolSettings => {
    const out: ToolSettings = {};
    if (tool.id === "trivy" && scanners.length > 0) out.scanners = scanners;
    if (tool.id === "opengrep" && (repo.trim() !== "" || commit.trim() !== "")) {
      out.rules = { repo: repo.trim(), commit: commit.trim(), ...(lines(folders).length > 0 ? { paths: lines(folders) } : {}) };
    }
    for (const key of tool.lists) if (lines(lists[key]).length > 0) out[key] = lines(lists[key]);
    return out;
  };
  // The answer is the rule as stored, and the card is redrawn from it.
  const save = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      saved((await applyScanner(`sarif.${tool.id}`, { ...(rule === undefined ? { state: adding } : {}), run, settings: body() })).policy);
      setRefusal(null);
      setDone(true);
    } catch (err) {
      setDone(false); // a refusal and "Saved." never share the line
      setRefusal(said(err));
    } finally {
      sending.current = false;
    }
  };

  // Any edit makes "Saved." stale, so it goes until the next Save answers. The settings stay folded
  // until asked for (the redesign: "Settings go behind Configure").
  return (
    <section id={`rule-sarif.${tool.id}`} aria-label={tool.name} className={ROW} onChange={() => setDone(false)}>
      <span className="grid gap-0.5">
        <span className="text-base font-medium">{tool.name}</span>
        <span className="text-md text-text-dim">{tool.about}</span>
        {named !== undefined && <Scheduled rule={named} flow={flow} />}
      </span>
      <span className="flex items-center gap-3 self-start">
        <Dial name={`Findings from ${tool.name}`} value={named === undefined ? adding : flow.state(named)}
          change={(state: PolicyState) => (named === undefined ? setAdding(state) : void flow.ask(named, { state }))} />
        <button type="button" aria-expanded={configuring} className="text-md font-medium text-link hover:underline aria-expanded:underline" onClick={() => setConfiguring(!configuring)}>Configure</button>
      </span>
      {named === undefined ? <span /> : <Impact flow={flow} rule={named} />}
      {configuring && <div className={`${CARD} col-span-full`}>
      <label className="flex items-center gap-2 text-base">
        <input type="checkbox" checked={run} onChange={(event) => setRun(event.target.checked)} />
        {`Canyon runs ${tool.name}`}
      </label>
      <p className={NOTE}>{S.runsWhile}</p>
      {tool.id === "trivy" && (
        <fieldset className="mt-3">
          <legend className="mb-1 block text-md font-semibold text-text-dim">Check for</legend>
          <span className="flex flex-wrap gap-3">
            {TRIVY_SCANNERS.map(([key, word]) => (
              <label key={key} className="flex items-center gap-1.5 text-base">
                <input type="checkbox" checked={scanners.includes(key)} onChange={(event) => setScanners(event.target.checked ? [...scanners, key] : scanners.filter((one) => one !== key))} />
                {word}
              </label>
            ))}
          </span>
          <p className={NOTE}>{S.trivyDefault}</p>
        </fieldset>
      )}
      {tool.id === "opengrep" && (
        <>
          <p className={NOTE}>{S.bundledRules}</p>
          <label className={LABEL} htmlFor={id("repo")}>Rules repo</label>
          <input id={id("repo")} className={`${FIELD} w-full`} value={repo} placeholder="https://github.com/your-org/rules" onChange={(event) => setRepo(event.target.value)} />
          <label className={LABEL} htmlFor={id("commit")}>Commit</label>
          <input id={id("commit")} className={`${FIELD} w-full font-mono`} value={commit} onChange={(event) => setCommit(event.target.value)} />
          <label className={LABEL} htmlFor={id("folders")}>Rules folders, one per line</label>
          <textarea id={id("folders")} className={`${FIELD} w-full font-mono`} rows={2} value={folders} onChange={(event) => setFolders(event.target.value)} />
          <p className={NOTE}>{S.rulesFolders}</p>
          <p className={NOTE}>{S.communityRules}</p>
          <Button size="sm" onClick={() => { setRepo(COMMUNITY.repo); setCommit(COMMUNITY.commit); setFolders(COMMUNITY.paths.join("\n")); }}>Use the community rules</Button>
        </>
      )}
      {tool.lists.map((key) => (
        <div key={key}>
          <label className={LABEL} htmlFor={id(key)}>{`${LIST_LABEL[key]} (one per line)`}</label>
          <textarea id={id(key)} className={`${FIELD} w-full font-mono`} rows={2} value={lists[key]} onChange={(event) => setLists({ ...lists, [key]: event.target.value })} />
          {key === "skip_paths" && <p className={NOTE}>{S.skipBlinds}</p>}
        </div>
      ))}
      <span className="mt-3 flex flex-wrap items-center gap-3">
        <Button variant="primary" label={`Save ${tool.name}`} onClick={() => void save()}>Save</Button>
        {done && <span role="status" className="text-base text-ok">Saved.</span>}
        {refusal !== null && <span className="text-base text-critical">{refusal}</span>}
      </span>
      {tool.id === "opengrep" && <OpengrepCredential repo={settings.rules?.repo} />}
      </div>}
    </section>
  );
}

// The credential for the saved rules repo, read on its own route: it says one is set, for which
// repo, by whom and when, never the username or the token. Read only while a repo is saved, and
// again whenever it changes, since an apply that changes the repo removes it.
function OpengrepCredential({ repo }: { repo?: string }) {
  const [credential, setRead] = useState<Credential | null | undefined>(undefined);
  const [failure, setFailure] = useState<string | null>(null);
  // A refused set or remove keeps the form and the button: it is said beside them, and cleared by the next try.
  const [refusal, setRefusal] = useState<string | null>(null);
  const [username, setUsername] = useState("");
  const [token, setToken] = useState("");
  const sending = useRef(false);
  // The control that replaces the one just clicked takes the keyboard once the read lands.
  const moved = useRef(false);
  const remove = useRef<HTMLButtonElement>(null);
  const user = useRef<HTMLInputElement>(null);
  const read = () => getCredential().then(({ credential: answer }) => { setRead(answer); setFailure(null); }, (err: unknown) => setFailure(said(err)));
  useEffect(() => { if (repo !== undefined) void read(); }, [repo]);
  useEffect(() => {
    if (!moved.current) return;
    moved.current = false;
    (credential ? remove : user).current?.focus();
  }, [credential]);

  const send = async (write: () => Promise<void>) => {
    if (sending.current) return;
    sending.current = true;
    setRefusal(null);
    try {
      await write();
      setUsername("");
      setToken("");
      moved.current = true;
      await read();
    } catch (err) {
      setRefusal(said(err));
    } finally {
      sending.current = false;
    }
  };

  if (repo === undefined) return null;
  if (failure !== null) return <p className="mt-3 text-base text-critical">{failure}</p>;
  if (credential === undefined) return null;
  const refused = refusal !== null && <span className="text-base text-critical">{refusal}</span>;
  if (credential !== null) {
    return (
      <div className="mt-3 flex flex-wrap items-center gap-2 text-base">
        <span>{S.credentialSet.replace("<repo>", () => credential.repo)}</span>
        <span className="text-text-dim"><Actor actor={credential.set_by} /> · <Age at={credential.set_at} /></span>
        <Button ref={remove} variant="ghost" size="sm" label="Remove the credential" onClick={() => void send(removeCredential)}>Remove</Button>
        {refused}
      </div>
    );
  }
  return (
    <div className="mt-3">
      <p className={NOTE}>{S.credentialAsk}</p>
      <span className="mt-1.5 flex flex-wrap items-center gap-2">
        <input ref={user} aria-label="Username" className={FIELD} autoComplete="off" value={username} onChange={(event) => setUsername(event.target.value)} />
        <input aria-label="Token" type="password" className={FIELD} autoComplete="new-password" value={token} onChange={(event) => setToken(event.target.value)} />
        <Button size="sm" onClick={() => void send(() => setCredential({ repo: repo ?? "", username, token }))}>Save credential</Button>
        {refused}
      </span>
    </div>
  );
}
