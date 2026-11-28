import { useState } from "react";
import {
  checkPlatformRepo, putPlatformRepo, replaceFetchToken, replaceRepoHostToken, replaceRepoToken, said,
  type PlatformRepo, type PlatformRepoEdit, type RepoProblem,
} from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age, ago } from "../ui/Age";
import { Button } from "../ui/Button";
import { Saved } from "../ui/Saved";
import { useUnsaved } from "../ui/Unsaved";
import appProjects from "../../../deploy/gitops/argocd/appprojects.yaml?raw";
import services from "../../../deploy/gitops/admission/services.yaml?raw";
import applicationSets from "../../../deploy/gitops/argocd/applicationsets.yaml?raw";
import repository from "../../../deploy/gitops/argocd/repository.yaml?raw";
import namespaces from "../../../deploy/gitops/argocd/namespaces.yaml?raw";
import store from "../../../deploy/gitops/external-secrets/clustersecretstore.yaml?raw";
import chart from "../../../deploy/gitops/namespaces/Chart.yaml?raw";
import chartValues from "../../../deploy/gitops/namespaces/values.yaml?raw";
import chartNamespace from "../../../deploy/gitops/namespaces/templates/namespace.yaml?raw";

const LABEL = "mt-3 mb-1 block text-xs tracking-caps text-text-faint uppercase";
const FIELD = "w-full max-w-[520px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-1 text-md text-text-dim";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// What a save or a replace answered once, for the dialog alone (HandOff's Reveal).
export type RepoRevealed = { fetch?: string; token?: string };

type Form = { url: string; username: string; token: string; preview: string; prod: string; mode: "pull_request" | "push"; root: string; api: "github" | "gitlab" | "none"; apiURL: string; store: string; ca: string };

const blank: Form = { url: "", username: "x-access-token", token: "", preview: "preview", prod: "main", mode: "pull_request", root: "apps", api: "github", apiURL: "https://api.github.com", store: "canyon", ca: "" };

const formOf = (r: PlatformRepo): Form => ({
  url: r.url, username: r.username, token: "", preview: r.preview.branch, prod: r.prod.branch, mode: r.prod.mode, root: r.root, api: r.api, apiURL: r.api_url, store: r.store, ca: r.ca ?? "",
});

const editOf = (f: Form): PlatformRepoEdit => ({
  url: f.url.trim(), username: f.username.trim(), ...(f.token === "" ? {} : { token: f.token }), preview: { branch: f.preview.trim() },
  prod: { branch: f.prod.trim(), mode: f.api === "none" ? "push" : f.mode }, root: f.root.trim(), api: f.api,
  ...(f.apiURL.trim() === "" ? {} : { api_url: f.apiURL.trim() }), store: f.store.trim(), ca: f.ca.trim() === "" ? null : f.ca,
});

// The deploy repo's step (design/HAND-OFF.md §3b; #2470): the form while none is saved or an admin changes it, then the
// saved repo with who set it and when, its two replaces and its check. A member reads the saved repo and changes nothing.
export function RepoStep({ repo, admin, reveal, saved }: {
  repo: PlatformRepo | null; admin: boolean; reveal: (values: RepoRevealed) => void; saved: () => Promise<unknown>;
}) {
  const [editing, setEditing] = useState(false);
  const [form, setForm] = useState<Form>(blank);
  const [refusal, setRefusal] = useState<{ at: string; message: string } | null>(null);
  const [asking, setAsking] = useState<"token" | "fetch" | "host" | null>(null);
  const [newToken, setNewToken] = useState("");
  const [check, setCheck] = useState<{ passed: boolean; problems: RepoProblem[]; templates: string[] } | "checking" | null>(null);
  const [justSaved, setJustSaved] = useState(false);
  // Typed, so its Save shows only while the form differs from what is saved, and leaving asks first (D16); so does a
  // replacement token typed and not yet used.
  const changed = repo === null || editing ? JSON.stringify(form) !== JSON.stringify(repo === null ? blank : formOf(repo)) : false;
  useUnsaved(changed || newToken !== "");

  const set = (key: keyof Form) => (value: string) => setForm((f) => {
    if (key !== "api") return { ...f, [key]: value };
    // Each API its own address: GitHub's by default, GitLab's empty — the server's default on the repo's own host — so
    // neither's ever follows the other.
    const apiURL = value === "github" ? blank.apiURL : value === "gitlab" ? "" : f.apiURL;
    return { ...f, api: value as Form["api"], apiURL };
  });
  const host = (() => {
    try {
      return new URL(form.url.trim()).host || "<host>";
    } catch {
      return "<host>";
    }
  })();
  const fail = (at: string, err: unknown) => setRefusal({ at, message: said(err) });
  const refused = (at: string) => refusal?.at === at && <p role="alert" className="mt-1 text-base text-critical">{refusal.message}</p>;

  const save = async () => {
    try {
      const answer = await putPlatformRepo(editOf(form));
      setEditing(false);
      setRefusal(null);
      setForm(blank);
      setJustSaved(true);
      if (answer.fetch_token !== undefined || answer.token !== undefined) reveal({ fetch: answer.fetch_token, token: answer.token });
      await saved();
    } catch (err) {
      fail("save", err);
    }
  };
  const runCheck = async () => {
    if (check === "checking") return;
    setCheck("checking");
    try {
      setCheck(await checkPlatformRepo());
      await saved();
    } catch (err) {
      setCheck(null);
      fail("check", err);
    }
  };
  const replace = async (what: "token" | "fetch" | "host") => {
    try {
      if (what === "token") {
        await replaceRepoToken(newToken);
        setNewToken("");
      } else if (what === "host") {
        reveal({ token: (await replaceRepoHostToken()).token });
      } else {
        reveal({ fetch: (await replaceFetchToken()).fetch_token });
      }
      setAsking(null);
      setRefusal(null);
      await saved();
    } catch (err) {
      fail(what, err);
    }
  };

  const field = (id: string, label: string, key: keyof Form, note?: string, type = "text") => (
    <>
      <label className={LABEL} htmlFor={id}>{label}</label>
      <input id={id} type={type} className={FIELD} value={form[key]} autoComplete="off" spellCheck={false} onChange={(event) => set(key)(event.target.value)} />
      {note !== undefined && <p className={NOTE}>{note}</p>}
    </>
  );
  const choice = (name: string, label: string, key: "mode" | "api", options: [string, string][]) => (
    <fieldset className="mt-3">
      <legend className={LABEL}>{label}</legend>
      {options.map(([value, word]) => (
        <label key={value} className="mr-4 inline-flex items-center gap-2 text-base">
          <input type="radio" name={name} checked={form[key] === value} onChange={() => set(key)(value)} />
          {word}
        </label>
      ))}
    </fieldset>
  );
  const editor = (
    <>
      {field("repo-url", S.repoUrl, "url")}
      {field("repo-user", S.repoUser, "username")}
      {field("repo-token", S.repoToken, "token", repo === null ? S.repoTokenNote : S.repoTokenSet, "password")}
      {field("repo-preview", S.repoPreview, "preview")}
      {field("repo-prod", S.repoProd, "prod")}
      {form.api !== "none" && choice("repo-mode", S.repoProdMode, "mode", [["pull_request", S.repoPR], ["push", S.repoPush]])}
      {field("repo-root", S.repoRoot, "root", S.repoRootNote.replace("<path>", () => form.root.trim() || "apps"))}
      {choice("repo-api", S.repoAPI, "api", [["github", S.repoAPIGitHub], ["gitlab", S.repoAPIGitLab], ["none", S.repoAPINone]])}
      {form.api === "none" && <p role="note" className="mt-1 text-md text-amber">{S.repoNoReview}</p>}
      {form.api === "gitlab" && form.mode === "pull_request" && <p role="note" className={NOTE}>{S.repoGitLabNote.replace("<host>", () => host)}</p>}
      {form.api !== "none" && field("repo-api-url", S.repoAPIURL, "apiURL")}
      {field("repo-store", S.repoStore, "store")}
      <label className={LABEL} htmlFor="repo-ca">{S.repoCA}</label>
      <textarea id="repo-ca" className={`${FIELD} h-24 font-mono text-sm`} value={form.ca} spellCheck={false} onChange={(event) => set("ca")(event.target.value)} />
      <p className={NOTE}>{S.repoCANote}</p>
      <span className="mt-2 flex flex-wrap gap-2">
        {changed && <Button variant="primary" onClick={() => void save()}>{S.repoSave}</Button>}
        {editing && <Button variant="ghost" onClick={() => setEditing(false)}>Cancel</Button>}
      </span>
      {refused("save")}
    </>
  );
  if (repo === null) return admin ? editor : null;
  if (editing) return editor;

  const result = check !== null && check !== "checking" && (
    <div role="status" className="mt-2">
      {check.passed ? <p className="text-base text-ok">{S.checkPassed}</p> : (
        <ul className="list-disc pl-5">
          {check.problems.map((p) => <li key={p.code} className="text-base text-critical">{p.message}</li>)}
        </ul>
      )}
      <p className={NOTE}>{check.templates.length > 0 ? S.checkTemplates.replace("<list>", () => check.templates.join(", ")) : S.checkNoTemplates.replace("<path>", () => repo.root)}</p>
    </div>
  );
  return (
    <>
      <p className="mt-1 font-mono text-sm break-all">{repo.url}</p>
      <p className={NOTE}><Actor actor={repo.set_by} /> · <Age at={repo.set_at} /></p>
      {justSaved && <Saved />}
      <p className={NOTE}>{repo.preview.branch} · {repo.prod.branch} ({repo.prod.mode === "pull_request" ? S.repoPR : S.repoPush}) · {repo.root}</p>
      {repo.checked_at !== null && <p className={NOTE}>{S.checkedAt.replace("<age>", () => ago(repo.checked_at as string))}</p>}
      {admin && (
        <>
          <span className="mt-2 flex flex-wrap gap-2">
            <Button variant="secondary" onClick={() => void runCheck()}>{check === "checking" ? S.checkPending : S.check}</Button>
            <Button variant="ghost" onClick={() => { setForm(formOf(repo)); setEditing(true); setJustSaved(false); }}>Change</Button>
            <Button variant="ghost" onClick={() => { setRefusal(null); setAsking("token"); }}>{S.repoTokenReplace}</Button>
            <Button variant="ghost" onClick={() => { setRefusal(null); setAsking("host"); }}>{S.repoHostReplace}</Button>
            <Button variant="ghost" onClick={() => { setRefusal(null); setAsking("fetch"); }}>{S.fetchReplace}</Button>
          </span>
          {result}
          <p className={NOTE}>{S.checkCannot}</p>
          {refused("check")}
          {asking === "token" && (
            <div className={CARD}>
              <p className="text-base">{S.repoTokenAsk}</p>
              <label className={LABEL} htmlFor="repo-new-token">{S.repoToken}</label>
              <input id="repo-new-token" type="password" className={FIELD} value={newToken} autoComplete="off" onChange={(event) => setNewToken(event.target.value)} />
              <span className="mt-2 flex flex-wrap gap-2">
                <Button variant="primary" onClick={() => void replace("token")}>{S.repoTokenReplace}</Button>
                <Button variant="ghost" onClick={() => setAsking(null)}>Cancel</Button>
              </span>
              {refused("token")}
            </div>
          )}
          {([["host", S.repoHostAsk, S.repoHostReplace], ["fetch", S.fetchAsk, S.fetchReplace]] as const).map(([what, question, yes]) => asking === what && (
            <div key={what} className={CARD}>
              <p className="text-base">{question}</p>
              <span className="mt-2 flex flex-wrap gap-2">
                <Button variant="primary" onClick={() => void replace(what)}>{yes}</Button>
                <Button variant="ghost" onClick={() => setAsking(null)}>Cancel</Button>
              </span>
              {refused(what)}
            </div>
          ))}
        </>
      )}
    </>
  );
}

// What Canyon doesn't know, typed into the card: it fills the files in this browser alone, never sent, never stored. Until
// a field is filled, its files show the field's name. Argo CD's token has no field: the browser never holds it, and
// kubectl reads it from its input.
const TYPED = [
  ["READ_USER", S.teamReadUser], ["PLATFORM_REPO_URL", S.teamPlatformRepo],
  ["PLATFORM_REVISION", S.teamPlatformBranch], ["PLATFORM_PATH", S.teamPlatformPath],
] as const;
export type Typed = Partial<Record<(typeof TYPED)[number][0], string>>;
const typedOr = (typed: Typed, key: keyof Typed, label: string) => (typed[key] ?? "").trim() || `<${label}>`;

// shq is s as one shell word: single-quoted, each ' as '\'': nothing in it runs where the command is pasted.
export const shq = (s: string) => `'${s.replaceAll("'", "'\\''")}'`;

// filled is a reference file of deploy/gitops with this host's values and what was typed, never a token Canyon holds.
export function filled(text: string, repo: PlatformRepo, canyon: string, typed: Typed = {}): string {
  const values: Record<string, string> = {
    CANYON_URL: canyon, REPO_URL: repo.url, ROOT: repo.root, APP_SEGMENT: String(repo.root.split("/").length), PREVIEW_BRANCH: repo.preview.branch,
    PROD_BRANCH: repo.prod.branch, STORE: repo.store, REQUEUE_SECONDS: "180", CA_BUNDLE: repo.ca === null ? "" : `      caBundle: ${btoa(repo.ca)}`,
    READ_TOKEN: `<${S.teamReadToken}>`,
    ...Object.fromEntries(TYPED.map(([key, label]) => [key, typedOr(typed, key, label)])),
  };
  return text.replace(/\$\{([A-Z_]+)\}/g, (whole, name: string) => values[name] ?? whole);
}

// The files the platform team applies, then the namespaces chart for its own repo, each named as deploy/gitops names it.
const APPLY = [
  ["argocd/applicationsets.yaml", applicationSets], ["argocd/appprojects.yaml", appProjects], ["admission/services.yaml", services],
  ["argocd/namespaces.yaml", namespaces],
  ["external-secrets/clustersecretstore.yaml", store], ["argocd/repository.yaml", repository],
] as const;
const CHART = [["Chart.yaml", chart], ["values.yaml", chartValues], ["templates/namespace.yaml", chartNamespace]] as const;

// `next` is the checklist's card, under the repo step: its first line says the platform team's part comes next.
export function RepoTeamCard({ repo, next = false }: { repo: PlatformRepo; next?: boolean }) {
  const [typed, setTyped] = useState<Typed>({});
  const [copied, setCopied] = useState<{ which: string; ok: boolean } | null>(null);
  const canyon = window.location.origin;
  const under = typedOr(typed, "PLATFORM_PATH", S.teamPlatformPath);
  const files = [
    ...APPLY.map(([name, text]) => ({ name, path: name, text: filled(text, repo, canyon, typed) })),
    ...CHART.map(([name, text]) => ({ name, path: `${under}/${name}`, text: filled(text, repo, canyon, typed) })),
  ];
  // navigator.clipboard is absent outside a secure context, a plain-http host: that is a failure too, said in place.
  const copy = (which: string, value: string) => {
    Promise.resolve().then(() => navigator.clipboard.writeText(value)).then(() => setCopied({ which, ok: true }), () => setCopied({ which, ok: false }));
  };
  const all = files.map(({ path, text }) => `# ${path}\n${text.endsWith("\n") ? text : `${text}\n`}`).join("---\n");
  // The repository credential's token goes in with kubectl, from its input: never in a file, never on a command line. One
  // line, joined by &&: pasted without bracketed paste, a second line would be read as the token.
  const kubectl = [
    `kubectl -n argocd create secret generic canyon-deploy-repo --from-literal=type=git --from-literal=url=${shq(repo.url)} --from-literal=username=${shq(typedOr(typed, "READ_USER", S.teamReadUser))} --from-file=password=/dev/stdin`,
    "kubectl -n argocd label secret canyon-deploy-repo argocd.argoproj.io/secret-type=repository",
  ].join(" && ");
  const block = ({ name, text }: { name: string; text: string }) => (
    <figure key={name} aria-labelledby={`team-file-${name}`} className="mt-3">
      <span className="flex flex-wrap items-center justify-between gap-2">
        <span id={`team-file-${name}`} className="font-mono text-sm">{name}</span>
        <Button variant="secondary" size="sm" label={`Copy ${name}`} onClick={() => copy(name, text)}>{S.teamCopy}</Button>
      </span>
      <pre className="mt-1 max-h-72 overflow-auto rounded-lg border border-border bg-surface p-2 font-mono text-xs">{text}</pre>
      {name === "argocd/repository.yaml" && (
        <>
          <p className={NOTE}>{S.teamTokenKubectl}</p>
          <pre className="mt-1 overflow-auto rounded-lg border border-border bg-surface p-2 font-mono text-xs">{kubectl}</pre>
        </>
      )}
    </figure>
  );
  return (
    <section aria-labelledby="hand-off-team" className={CARD}>
      {next && <p className="mb-2 text-base">{S.teamNext}</p>}
      <h3 id="hand-off-team" className="text-base font-semibold">{S.teamTitle}</h3>
      <p className={NOTE}>{S.teamRepo.replace("<path>", () => repo.root).replace("<url>", () => repo.url)}</p>
      <p className={NOTE}>{S.teamFetch}</p>
      <p className={NOTE}>{S.teamRepoDocs}</p>
      {TYPED.map(([key, label]) => (
        <div key={key}>
          <label className={LABEL} htmlFor={`team-${key}`}>{label}</label>
          <input id={`team-${key}`} type="text" className={FIELD} value={typed[key] ?? ""} autoComplete="off" spellCheck={false}
            onChange={(event) => { const value = event.target.value; setTyped((t) => ({ ...t, [key]: value })); }} />
        </div>
      ))}
      <p className={NOTE}>{S.teamFieldsNote}</p>
      <span className="mt-3 flex">
        <Button variant="secondary" onClick={() => copy(files.map(({ path }) => path).join(", "), all)}>{S.teamCopyAll}</Button>
      </span>
      <p role="status" className="mt-1 text-md text-ok">{copied?.ok === true ? S.teamCopied.replace("<file>", () => copied.which) : ""}</p>
      {copied?.ok === false && <p role="alert" className="text-md text-critical">{S.copyFailed}</p>}
      {files.slice(0, APPLY.length).map(block)}
      <h4 className="mt-4 text-base font-semibold">{S.teamChart.replace("<path>", () => under)}</h4>
      {files.slice(APPLY.length).map(block)}
    </section>
  );
}
