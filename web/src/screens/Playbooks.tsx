import { useEffect, useId, useState } from "react";
import {
  getPlaybook, getPlaybooks, getPlaybookVersions, putPlaybook, removePlaybook, restorePlaybook, said,
  type PlaybookEntry, type PlaybookKind, type PlaybookText, type PlaybookVersion,
} from "../api";
import { S } from "../strings";
import { actorName } from "../ui/Actor";
import { Button } from "../ui/Button";
import { Chip } from "../ui/Chip";
import { shortDate } from "../ui/Verdict";
import { Markdown } from "../ui/Markdown";
import { lineDiff } from "./lineDiff";
import { skillOf } from "./skillFile";

const H3 = "mt-6 text-base font-semibold";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";
const NAME_MAX = 80;
const WHEN_MAX = 200;
const BODY_MAX = 64 << 10;

// A playbook's slug from its name, fixed once the playbook exists (contracts/routes.md, the playbooks row): the name
// lowercased, each run of other characters one hyphen, a-z, 0-9 and hyphens only, a letter first, at most 40.
export const playbookSlugOf = (name: string) =>
  name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+/, "").slice(0, 40).replace(/-+$/, "");

type Open = { mode: "edit"; slug: string } | { mode: "new"; draft?: PlaybookText & { slug: string } } | { mode: "history"; slug: string } | { mode: "import" } | null;

// Guardrails › Playbooks (design/specs/screens.md; design/specs/mcp.md §3.2): the org's own skills and notes on how
// it builds, which every coding agent is handed by name and reads by slug. Canyon never checks one. An admin's
// screen, as all of Guardrails is: a save, a restore and a removal are refused in the server's own words.
export function Playbooks() {
  const [list, setList] = useState<PlaybookEntry[] | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [open, setOpen] = useState<Open>(null);
  const [reads, setReads] = useState(0);
  const [removing, setRemoving] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    getPlaybooks().then(
      (answer) => { if (live) { setList(answer.playbooks); setRefusal(null); } },
      (err: unknown) => { if (live) setRefusal(said(err)); },
    );
    return () => { live = false; };
  }, [reads]);
  const done = () => { setOpen(null); setReads((n) => n + 1); };
  const remove = async (slug: string) => {
    try {
      await removePlaybook(slug);
      setRemoving(null);
      done();
    } catch (err) {
      setRefusal(said(err));
    }
  };
  return (
    <section aria-label="Playbooks">
      <h3 className={H3}>Playbooks</h3>
      <p className="mt-1 text-md text-text-dim">{S.playbooksIntro}</p>
      {/* What the two kinds are, before anything about the counts (#118). */}
      <ul className="mt-1.5 grid gap-0.5 text-md text-text-dim">
        <li><span className="font-medium text-text">{S.playbookSkill}:</span> {S.playbookSkillMeans}</li>
        <li><span className="font-medium text-text">{S.playbookNote}:</span> {S.playbookNoteMeans}</li>
      </ul>
      {refusal !== null && <p className="mt-2.5 text-base text-critical">{refusal}</p>}
      {list !== null && list.length === 0 && open === null && <p className="mt-2.5 text-base text-text-dim">{S.playbooksNone}</p>}
      {list?.map((entry) => {
        if (open?.mode === "edit" && open.slug === entry.slug) return <Editor key={entry.slug} slug={entry.slug} done={done} cancel={() => setOpen(null)} />;
        if (open?.mode === "history" && open.slug === entry.slug) return <History key={entry.slug} entry={entry} done={done} close={() => setOpen(null)} />;
        return (
          <section key={entry.slug} aria-label={entry.name} className={CARD}>
            <span className="flex flex-wrap items-center gap-2">
              <span className="text-base">{entry.name}</span>
              <Chip tone="advisory">{entry.kind === "skill" ? S.playbookSkill : S.playbookNote}</Chip>
            </span>
            <p className="mt-1 text-md">{entry.when}</p>
            <p className="mt-1 text-sm text-text-dim">{`${entry.slug} · version ${entry.version} · ${actorName(entry.updated_by)} · ${shortDate(entry.updated_at)}`}</p>
            <p className="mt-0.5 text-sm text-text-dim">
              {[S.playbookReads.replace("<n>", entry.reads.toLocaleString("en")), entry.machines !== undefined ? S.playbookMachines.replace("<n>", entry.machines.toLocaleString("en")) : ""].filter(Boolean).join(" · ")}
            </p>
            {removing === entry.slug
              ? (
                <span className="mt-2 flex flex-wrap items-center gap-2">
                  <span className="text-md">{S.playbookRemoveAsk.replace("<name>", entry.name)}</span>
                  <Button variant="danger" size="sm" label={`Remove ${entry.name}`} onClick={() => void remove(entry.slug)}>Remove</Button>
                  <Button variant="ghost" size="sm" onClick={() => setRemoving(null)}>Keep it</Button>
                </span>
              )
              : (
                <span className="mt-1 flex flex-wrap gap-3">
                  <button type="button" className="text-link" aria-label={`Edit ${entry.name}`} onClick={() => setOpen({ mode: "edit", slug: entry.slug })}>Edit</button>
                  <button type="button" className="text-link" aria-label={`History of ${entry.name}`} onClick={() => setOpen({ mode: "history", slug: entry.slug })}>History</button>
                  <button type="button" className="text-link" aria-label={`Remove ${entry.name}`} onClick={() => setRemoving(entry.slug)}>Remove</button>
                </span>
              )}
          </section>
        );
      })}
      {/* Beside the counts it explains, under the list (#118). */}
      {list?.some((one) => one.kind === "skill") === true && <p className="mt-2 text-sm text-text-faint">{S.playbookCountsMean}</p>}
      {list !== null && open?.mode === "new" && <Editor draft={open.draft} taken={list.map((one) => one.slug)} done={done} cancel={() => setOpen(null)} />}
      {list !== null && open?.mode === "import" && <Import use={(draft) => setOpen({ mode: "new", draft })} cancel={() => setOpen(null)} />}
      {list !== null && open?.mode !== "new" && open?.mode !== "import" && (
        <span className="mt-3 flex flex-wrap gap-2">
          <Button onClick={() => setOpen({ mode: "new" })}>New playbook</Button>
          <Button variant="ghost" onClick={() => setOpen({ mode: "import" })}>{S.playbookImport}</Button>
        </span>
      )}
    </section>
  );
}

// The editor: a name, a skill or a note, one line on when to read it, and the Markdown body with its preview. A new
// playbook's slug comes from its name, or from the SKILL.md imported, and is shown; one that names a playbook that
// stands is refused here, as saving it would edit that one. Save writes the next version; the server says what it
// refuses, in its words.
function Editor({ slug: editing, draft, taken = [], done, cancel }: {
  slug?: string; draft?: PlaybookText & { slug: string }; taken?: string[]; done: () => void; cancel: () => void;
}) {
  const [loaded, setLoaded] = useState(editing === undefined);
  const [name, setName] = useState(draft?.name ?? "");
  const [slug, setSlug] = useState(draft?.slug ?? "");
  const [kind, setKind] = useState<PlaybookKind>(draft?.kind ?? "skill");
  const [when, setWhen] = useState(draft?.when ?? "");
  const [body, setBody] = useState(draft?.body ?? "");
  const [preview, setPreview] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const id = useId();
  useEffect(() => {
    if (editing === undefined) return;
    let live = true;
    getPlaybook(editing).then(
      ({ playbook }) => { if (live) { setName(playbook.name); setKind(playbook.kind); setWhen(playbook.when); setBody(playbook.body); setLoaded(true); } },
      (err: unknown) => { if (live) setRefusal(said(err)); },
    );
    return () => { live = false; };
  }, [editing]);
  const at = editing ?? (draft !== undefined ? slug : playbookSlugOf(name)); // an import's slug is its own, and editable
  const save = async () => {
    if (at === "") { setRefusal("The name needs a letter a–z: it becomes the playbook's slug."); return; }
    if (editing === undefined && taken.includes(at)) { setRefusal("A playbook with this slug already exists: edit that one instead."); return; }
    setSaving(true);
    try {
      await putPlaybook(at, { name, kind, when, body });
      done();
    } catch (err) {
      setRefusal(said(err));
      setSaving(false);
    }
  };
  if (!loaded) return <section aria-label={`Edit ${editing}`} className={CARD}>{refusal !== null ? <p className="text-base text-critical">{refusal}</p> : <p role="status" className="text-md text-text-dim">Reading it…</p>}</section>;
  const bytes = new TextEncoder().encode(body).length;
  return (
    <section aria-label={editing !== undefined ? `Edit ${name}` : "New playbook"} className={CARD}>
      <label className="text-md text-text-dim" htmlFor={`${id}-name`}>Name</label>
      <input id={`${id}-name`} className={`${FIELD} mt-1 w-full`} maxLength={NAME_MAX} value={name} onChange={(event) => setName(event.target.value)} />
      <span className="text-sm text-text-faint">{`${name.length} / ${NAME_MAX}${at === "" || draft !== undefined ? "" : ` · its slug: ${at}`}`}</span>
      {editing === undefined && draft !== undefined && (
        <>
          <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-slug`}>Slug</label>
          <input id={`${id}-slug`} className={`${FIELD} mt-1 w-full font-mono`} maxLength={40} value={slug} onChange={(event) => setSlug(event.target.value)} />
        </>
      )}
      <fieldset className="mt-2">
        <legend className="text-md text-text-dim">Kind</legend>
        <span className="mt-1 flex gap-4">
          {(["skill", "note"] as const).map((one) => (
            <label key={one} className="flex items-center gap-1.5 text-base">
              <input type="radio" name={`${id}-kind`} checked={kind === one} onChange={() => setKind(one)} />
              {one === "skill" ? S.playbookSkill : S.playbookNote}
            </label>
          ))}
        </span>
        <span className="text-sm text-text-faint">{kind === "skill" ? S.playbookSkillMeans : S.playbookNoteMeans}</span>
      </fieldset>
      <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-when`}>{S.playbookWhen}</label>
      <input id={`${id}-when`} className={`${FIELD} mt-1 w-full`} maxLength={WHEN_MAX} value={when} onChange={(event) => setWhen(event.target.value)}
        placeholder="Read before you add a route or a query to a service." />
      <span className="text-sm text-text-faint">{`${when.length} / ${WHEN_MAX}`}</span>
      <span className="mt-2 flex items-center gap-3">
        <span className="text-md text-text-dim">Body</span>
        <span role="tablist" aria-label="Write or preview the body" className="flex gap-2">
          <button type="button" role="tab" aria-selected={!preview} className={!preview ? "text-text" : "text-link"} onClick={() => setPreview(false)}>Write</button>
          <button type="button" role="tab" aria-selected={preview} className={preview ? "text-text" : "text-link"} onClick={() => setPreview(true)}>Preview</button>
        </span>
      </span>
      {preview
        ? <div aria-label="Preview" className="mt-1 rounded-lg border border-border bg-surface px-3 py-2"><Markdown text={body} /></div>
        : <textarea aria-label="Body" className={`${FIELD} mt-1 w-full font-mono`} rows={14} value={body} onChange={(event) => setBody(event.target.value)} />}
      <span className="text-sm text-text-faint">{`${bytes.toLocaleString("en")} / ${BODY_MAX.toLocaleString("en")} bytes`}</span>
      {refusal !== null && <p className="mt-2 text-base text-critical">{refusal}</p>}
      <span className="mt-2 flex gap-2">
        {saving
          ? <Button disabled reason="Saving.">Save</Button>
          : <Button variant="primary" label={editing !== undefined ? `Save ${name}` : "Save the new playbook"} onClick={() => void save()}>Save</Button>}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </section>
  );
}

// Import: a SKILL.md pasted or chosen from disk (design/specs/mcp.md §3.2). Its frontmatter's name fills the slug and
// the name, its description the when line, and the rest is the body, as a skill; the editor opens on it, unsaved.
function Import({ use, cancel }: { use: (draft: PlaybookText & { slug: string }) => void; cancel: () => void }) {
  const [text, setText] = useState("");
  const [refusal, setRefusal] = useState<string | null>(null);
  const id = useId();
  const read = (raw: string) => {
    const skill = skillOf(raw);
    if (typeof skill === "string") { setRefusal(skill); return; }
    use({ slug: playbookSlugOf(skill.name), name: skill.name, kind: "skill", when: skill.description, body: skill.body });
  };
  return (
    <section aria-label={S.playbookImport} className={CARD}>
      <p className="text-md">{S.playbookImportHow}</p>
      <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-file`}>A SKILL.md file</label>
      <input id={`${id}-file`} type="file" accept=".md,text/markdown" className="mt-1 text-base" onChange={(event) => {
        const file = event.target.files?.[0];
        if (file !== undefined) void file.text().then(read);
      }} />
      <label className="mt-2 block text-md text-text-dim" htmlFor={`${id}-text`}>Or paste it</label>
      <textarea id={`${id}-text`} className={`${FIELD} mt-1 w-full font-mono`} rows={10} value={text} onChange={(event) => setText(event.target.value)} />
      {refusal !== null && <p className="mt-2 text-base text-critical">{refusal}</p>}
      <span className="mt-2 flex gap-2">
        <Button variant="primary" label="Read the SKILL.md" onClick={() => read(text)}>Read it</Button>
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </section>
  );
}

// History: every version, newest first, a diff between any two, and a restore of an older one, which saves its text
// as the next version (contracts/routes.md, the playbooks row). The newest is what agents read.
function History({ entry, done, close }: { entry: PlaybookEntry; done: () => void; close: () => void }) {
  const [versions, setVersions] = useState<PlaybookVersion[] | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [from, setFrom] = useState<number | null>(null);
  const [to, setTo] = useState<number | null>(null);
  useEffect(() => {
    let live = true;
    getPlaybookVersions(entry.slug).then(
      (answer) => {
        if (!live) return;
        setVersions(answer.versions);
        setTo(answer.versions[0]?.version ?? null);
        setFrom(answer.versions[1]?.version ?? null);
      },
      (err: unknown) => { if (live) setRefusal(said(err)); },
    );
    return () => { live = false; };
  }, [entry.slug]);
  const restore = async (version: number) => {
    try {
      await restorePlaybook(entry.slug, version);
      done();
    } catch (err) {
      setRefusal(said(err));
    }
  };
  const a = versions?.find((one) => one.version === from);
  const b = versions?.find((one) => one.version === to);
  return (
    <section aria-label={`History of ${entry.name}`} className={CARD}>
      <span className="flex flex-wrap items-center justify-between gap-2">
        <span className="text-base">{`History of ${entry.name}`}</span>
        <Button variant="ghost" size="sm" onClick={close}>Close</Button>
      </span>
      {refusal !== null && <p className="mt-2 text-base text-critical">{refusal}</p>}
      {versions !== null && (
        <>
          <ol aria-label="Versions" className="mt-2 text-md">
            {versions.map((one, i) => (
              <li key={one.version} className="flex flex-wrap items-center gap-2 py-0.5">
                <span>{`Version ${one.version} · ${actorName(one.set_by)} · ${shortDate(one.set_at)}${i === 0 ? " · what agents read" : ""}`}</span>
                {i > 0 && <button type="button" className="text-link" aria-label={`Restore version ${one.version}`} onClick={() => void restore(one.version)}>{S.playbookRestore}</button>}
              </li>
            ))}
          </ol>
          {versions.length > 1 && (
            <span className="mt-2 flex flex-wrap items-center gap-2 text-md">
              <label htmlFor={`diff-from-${entry.slug}`}>Compare version</label>
              <select id={`diff-from-${entry.slug}`} className={FIELD} value={from ?? ""} onChange={(event) => setFrom(Number(event.target.value))}>
                {versions.map((one) => <option key={one.version} value={one.version}>{one.version}</option>)}
              </select>
              <label htmlFor={`diff-to-${entry.slug}`}>with version</label>
              <select id={`diff-to-${entry.slug}`} className={FIELD} value={to ?? ""} onChange={(event) => setTo(Number(event.target.value))}>
                {versions.map((one) => <option key={one.version} value={one.version}>{one.version}</option>)}
              </select>
            </span>
          )}
          {a !== undefined && b !== undefined && a.version !== b.version && <Diff a={a} b={b} />}
        </>
      )}
    </section>
  );
}

// Diff is two versions side by side as lines: each field that changed, and the body's lines removed and added.
function Diff({ a, b }: { a: PlaybookVersion; b: PlaybookVersion }) {
  const fields = (["name", "kind", "when"] as const).filter((key) => a[key] !== b[key]);
  const lines = lineDiff(a.body, b.body);
  return (
    <div aria-label={`Version ${a.version} to version ${b.version}`} className="mt-2">
      {fields.map((key) => <p key={key} className="text-md">{`${key}: ${a[key]} → ${b[key]}`}</p>)}
      {lines.some((one) => one.op !== " ")
        ? (
          <pre className="mt-1 overflow-x-auto rounded-lg bg-surface px-3 py-2 font-mono text-sm">
            {lines.map((one, i) => (
              <span key={i} className={`block ${one.op === "+" ? "text-ok" : one.op === "-" ? "text-critical" : "text-text-dim"}`}>{`${one.op} ${one.line}`}</span>
            ))}
          </pre>
        )
        : <p className="mt-1 text-md text-text-dim">The body is the same.</p>}
    </div>
  );
}
