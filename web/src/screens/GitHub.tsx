import { useEffect, useState } from "react";
import { AskFirst } from "../ui/AskFirst";
import { useSaveChoice } from "../ui/Saved";
import { useUnsaved } from "../ui/Unsaved";
import { getGitHub, getGitHubRepositories, importRepository, linkRepository, said, setGitHubClientSecret, setGitHubOrg, unlinkRepository, type GitHubOrg, type GitHubRead, type GitHubRepository } from "../api";
import { S } from "../strings";
import { Age } from "../ui/Age";
import { Button } from "../ui/Button";
import { useMedia } from "../ui/media";
import { CELL, Table, TableWrap } from "../ui/Table";

const LABEL = "mt-3 mb-1 block text-xs tracking-caps text-text-faint uppercase";
const NOTE = "mt-1 text-md text-text-dim";
const COLUMNS = [
  { key: "org", label: "Organization" }, { key: "new", label: "New repositories" }, { key: "event", label: "Last event" }, { key: "state", label: "State" },
];

// Settings › GitHub (design/specs/screens.md, A0b; #1914): one App per GitHub organization, registered
// from here. The admin names the org and says what it holds; GitHub does the rest, and the register
// link goes there with the answers (contracts/routes.md, GET /api/v1/github/app/new).
export function GitHubSettings() {
  const [read, setRead] = useState<GitHubRead | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [org, setOrg] = useState("");
  const [imports, setImports] = useState<boolean | null>(null);
  const phone = useMedia("(max-width: 619px)"); // the table's 560px and the page's gutters
  const [keeps, setKeeps] = useState(false);
  useEffect(() => {
    let live = true;
    getGitHub().then((answer) => { if (live) setRead(answer); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, []);
  if (failure !== null) return <p className="text-base text-critical">{failure}</p>;
  if (read === null) return null;
  const keeper = read.orgs.find((one) => one.keeps_code);
  const named = org.trim();
  const register = S.githubRegister.replace("<org>", () => named || "the organization");
  const href = `/api/v1/github/app/new?${new URLSearchParams({ org: named, imports_new: String(imports), keeps_code: String(keeps) })}`;
  return (
    <section aria-label="GitHub" className="grid min-w-0 grid-cols-1 gap-6">
      <div className="min-w-0">
        <h2 className="text-base font-semibold">{S.githubConnect}</h2>
        <p className={NOTE}>{S.githubConnectNote}</p>
        <label className={LABEL} htmlFor="github-org">GitHub organization</label>
        <span className="flex w-full max-w-[520px] items-center rounded-lg border border-border bg-surface text-base">
          <span className="pl-2.5 text-text-faint">github.com/</span>
          <input id="github-org" className="min-w-0 flex-1 bg-transparent px-1 py-1.5" value={org} onChange={(event) => setOrg(event.target.value)} />
        </span>
        <fieldset className="mt-3">
          <legend className="sr-only">What the organization holds</legend>
          {([[true, S.githubOnly, S.githubOnlyNote], [false, S.githubOther, S.githubOtherNote]] as const).map(([value, word, note]) => (
            <div key={word} className="mt-2">
              <label className="flex items-center gap-2 text-base font-semibold">
                <input type="radio" name="github-imports" checked={imports === value} aria-describedby={`github-imports-${value}`} onChange={() => setImports(value)} />
                {word}
              </label>
              <p id={`github-imports-${value}`} className={`${NOTE} ml-6`}>{note}</p>
            </div>
          ))}
        </fieldset>
        <div className="mt-3">
          <label className="flex items-center gap-2 text-base font-semibold">
            <input type="checkbox" checked={keeps} disabled={keeper !== undefined} aria-describedby="github-keeps" onChange={(event) => setKeeps(event.target.checked)} />
            {S.githubKeep}
          </label>
          <p id="github-keeps" className={`${NOTE} ml-6`}>{keeper === undefined ? S.githubKeepNote : S.githubKeeper.replace("<org>", () => keeper.account)}</p>
        </div>
        {read.reachable
          ? (
            <div className="mt-4">
              {named === "" || imports === null
                ? <Button disabled reason="Name the organization and say what it holds first.">{register}</Button>
                : <a className="inline-flex h-[34px] items-center rounded-[7px] border border-accent bg-accent px-3 text-base font-medium text-accent-ink no-underline hover:brightness-110" href={href}>{register}</a>}
              <p className={NOTE}>{S.githubRegisterNote}</p>
            </div>
          )
          : <p className={`${NOTE} mt-4`}>{S.githubUnreachable}</p>}
      </div>
      {read.orgs.length > 0 && (
        <div className="min-w-0">
          <h2 className="text-base font-semibold">{S.githubConnected}</h2>
          {/* on a phone each organization is a card: the table's end starts past its edge (#126) */}
          {phone
            ? <ul className="mt-2.5 grid gap-2.5">{read.orgs.map((one) => <OrgCard key={one.app_slug} org={one} />)}</ul>
            : <TableWrap minWidth={560}><Table columns={COLUMNS} rows={read.orgs} renderRow={(one) => <Row key={one.app_slug} org={one} />} /></TableWrap>}
        </div>
      )}
      {read.orgs.filter((one) => !one.links_accounts && one.account !== "").map((one) => (
        <ClientSecret key={one.app_slug} org={one} saved={(org) => setRead({ ...read, orgs: read.orgs.map((was) => (was.app_slug === org.app_slug ? org : was)) })} />
      ))}
      {read.orgs.length > 0 && <Repositories />}
    </section>
  );
}

// An organization's columns, the same in the table and in a phone's card.
function partsOf(org: GitHubOrg) {
  const app = `https://github.com/apps/${org.app_slug}`;
  return [
    <>
      <a className="font-medium text-text no-underline hover:underline" href={app}>{org.account || org.app_slug}</a>
      {org.keeps_code && <span className="ml-2 rounded-full bg-surface-2 px-2 py-0.5 text-sm text-text-dim">Keeps code</span>}
    </>,
    org.account === "" ? "Picked by an admin" : <NewRepositories org={org} />,
    org.last_event_at === null ? <span className="text-sm text-text-faint">None yet</span> : <Age at={org.last_event_at} />,
    org.installed ? "Receiving events" : <>Not installed · <a className="text-link no-underline hover:underline" href={`${app}/installations/new`}>Install</a></>,
  ];
}

function Row({ org }: { org: GitHubOrg }) {
  return <tr>{partsOf(org).map((part, at) => <td key={COLUMNS[at]!.key} className={CELL}>{part}</td>)}</tr>;
}

function OrgCard({ org }: { org: GitHubOrg }) {
  const [name, ...rest] = partsOf(org);
  return (
    <li className="grid gap-2 rounded-xl border border-border bg-surface px-3.5 py-3">
      <span className="break-words">{name}</span>
      <dl className="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-md">
        {rest.map((part, at) => <div key={COLUMNS[at + 1]!.key} className="contents"><dt className="text-text-faint">{COLUMNS[at + 1]!.label}</dt><dd>{part}</dd></div>)}
      </dl>
    </li>
  );
}

// Repositories (#1917): what Canyon's Apps see, read from GitHub now, one row per repository and one
// more per further folder linked in it. An admin makes an app of a repository or a folder, links an
// existing app, or unlinks one; every change reads the list again.
function Repositories() {
  const [list, setList] = useState<{ repositories: GitHubRepository[]; unreachable: string[] } | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [form, setForm] = useState<{ repo: number; kind: "import" | "folder" | "link" } | null>(null);
  // Unlinking stops the app's pushes reaching it, so it asks first, saying what stops (D16).
  const [unlinking, setUnlinking] = useState<{ id: string; name: string; repo: string } | null>(null);
  const phone = useMedia("(max-width: 719px)"); // the table's 640px and the page's gutters
  const load = () => getGitHubRepositories().then((answer) => { setList(answer); setFailure(null); }, (err: unknown) => setFailure(said(err)));
  useEffect(() => { void load(); }, []);
  const act = (work: Promise<unknown>) => work.then(() => { setForm(null); setUnlinking(null); return load(); }, (err: unknown) => setFailure(said(err)));
  if (list === null) return failure === null ? null : <p className="text-base text-critical">{failure}</p>;
  const row = (repo: GitHubRepository): RepositoryProps => ({ repo, open: form?.repo === repo.repo_id ? form.kind : null, ask: (kind) => setForm(kind === null ? null : { repo: repo.repo_id, kind }), act,
    unlink: (app) => setUnlinking({ id: app.id, name: app.name, repo: repo.full_name }) });
  return (
    <div>
      <h2 className="text-base font-semibold">{S.githubRepositories}</h2>
      {list.unreachable.map((org) => <p key={org} className={NOTE}>{S.githubUnanswered.replace("<org>", () => org)}</p>)}
      {failure !== null && <p className="mt-1 text-base text-critical">{failure}</p>}
      {unlinking !== null && (
        <AskFirst question={S.githubUnlinkAsk.replace("<app>", () => unlinking.name).replace("<repo>", () => unlinking.repo)} yes={S.githubUnlinkYes} danger
          onYes={() => act(unlinkRepository(unlinking.id))} onNo={() => setUnlinking(null)}>
          <p className="text-md">{S.githubUnlinkMeans.replace("<app>", () => unlinking.name)}</p>
        </AskFirst>
      )}
      {/* on a phone each repository is a card: the table's buttons start past its edge (#126) */}
      {phone && list.repositories.length > 0
        ? <ul className="mt-2.5 grid gap-2.5">{list.repositories.map((repo) => <RepositoryCard key={repo.repo_id} {...row(repo)} />)}</ul>
        : (
          <TableWrap minWidth={phone ? 0 : 640}>
            <Table columns={[{ key: "repo", label: "Repository" }, { key: "folder", label: "Folder" }, { key: "app", label: "Canyon app" }]} rows={list.repositories} empty={NO_REPOSITORIES}
              renderRow={(repo) => <RepositoryRows key={repo.repo_id} {...row(repo)} />} />
          </TableWrap>
        )}
      <p className={NOTE}>{S.githubImportNote}</p>
    </div>
  );
}

const NO_REPOSITORIES = "None of Canyon's Apps sees a repository yet";

interface RepositoryProps {
  repo: GitHubRepository; open: "import" | "folder" | "link" | null; ask: (kind: "import" | "folder" | "link" | null) => void; act: (work: Promise<unknown>) => void;
  unlink: (app: GitHubRepository["apps"][number]) => void;
}

// A repository's parts, the same in the table and in a phone's card: its name, what an admin does with one no app is
// linked to, each app linked to it, and the form open on it.
function repositoryParts({ repo, open, ask, act, unlink }: RepositoryProps) {
  return {
    name: <a className="font-medium text-text no-underline hover:underline" href={`https://github.com/${repo.full_name}`}>{repo.full_name}</a>,
    unlinked: (
      <span className="flex flex-wrap gap-2">
        <Button size="sm" variant="secondary" onClick={() => ask("import")}>Make it an app</Button>
        <Button size="sm" variant="ghost" onClick={() => ask("link")}>Link to an app</Button>
      </span>
    ),
    apps: repo.apps.map((app, at) => ({
      folder: app.folder === "" ? "/" : app.folder,
      app: (
        <span className="flex flex-wrap items-center gap-2">
          <a className="text-link no-underline hover:underline" href={`#/apps/${app.id}`}>{app.name}</a>
          <Button size="sm" variant="ghost" label={`Unlink ${app.name}`} onClick={() => unlink(app)}>Unlink</Button>
          {at === repo.apps.length - 1 && <Button size="sm" variant="ghost" onClick={() => ask("folder")}>Add another folder</Button>}
        </span>
      ),
    })),
    form: open === null ? null : open === "link" ? <LinkForm repo={repo} cancel={() => ask(null)} act={act} /> : <ImportForm repo={repo} folder={open === "folder"} cancel={() => ask(null)} act={act} />,
  };
}

function RepositoryRows(props: RepositoryProps) {
  const { name, unlinked, apps, form } = repositoryParts(props);
  return (
    <>
      {apps.length === 0
        ? <tr><td className={CELL}>{name}</td><td className={CELL} /><td className={CELL}>{unlinked}</td></tr>
        : apps.map(({ folder, app }, at) => (
          <tr key={folder}><td className={CELL}>{at === 0 && name}</td><td className={`${CELL} font-mono`}>{folder}</td><td className={CELL}>{app}</td></tr>
        ))}
      {form !== null && <tr><td className={CELL} colSpan={3}>{form}</td></tr>}
    </>
  );
}

function RepositoryCard(props: RepositoryProps) {
  const { name, unlinked, apps, form } = repositoryParts(props);
  return (
    <li className="grid gap-2 rounded-xl border border-border bg-surface px-3.5 py-3">
      <span className="break-words">{name}</span>
      {apps.length === 0 ? unlinked : apps.map(({ folder, app }) => (
        <dl key={folder} className="grid grid-cols-[max-content_minmax(0,1fr)] items-center gap-x-3 gap-y-1 text-md">
          <dt className="text-text-faint">Folder</dt><dd className="font-mono break-all">{folder}</dd>
          <dt className="text-text-faint">Canyon app</dt><dd>{app}</dd>
        </dl>
      ))}
      {form}
    </li>
  );
}

const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";

// Make an app of the repository, or of a folder of it: the name defaults to the folder's last segment,
// or the repository's name, as the server picks it.
function ImportForm({ repo, folder, cancel, act }: { repo: GitHubRepository; folder: boolean; cancel: () => void; act: (work: Promise<unknown>) => void }) {
  const [path, setPath] = useState("");
  const [name, setName] = useState("");
  useUnsaved(path !== "" || name !== ""); // typed and not made yet: leaving asks first (D16)
  const fallback = path.trim() === "" ? repo.full_name.split("/")[1]! : path.trim().split("/").pop()!;
  return (
    <span className="flex flex-wrap items-end gap-2">
      <label className="grid gap-1 text-sm text-text-dim">Folder{folder ? "" : " (empty for the whole repository)"}
        <input className={FIELD} value={path} placeholder="apps/billing" onChange={(event) => setPath(event.target.value)} />
      </label>
      <label className="grid gap-1 text-sm text-text-dim">Name
        <input className={FIELD} value={name} placeholder={fallback} onChange={(event) => setName(event.target.value)} />
      </label>
      {folder && path.trim() === ""
        ? <Button size="sm" disabled reason="Name the folder: the whole repository is already an app.">Make the app</Button>
        : <Button size="sm" onClick={() => act(importRepository(repo.repo_id, path.trim(), name.trim() === "" ? undefined : name.trim()))}>Make the app</Button>}
      <Button size="sm" variant="ghost" onClick={cancel}>Cancel</Button>
    </span>
  );
}

// Link an existing app, typed by its id, to the repository's root or a folder.
function LinkForm({ repo, cancel, act }: { repo: GitHubRepository; cancel: () => void; act: (work: Promise<unknown>) => void }) {
  const [app, setApp] = useState("");
  const [path, setPath] = useState("");
  useUnsaved(app !== "" || path !== "");
  return (
    <span className="flex flex-wrap items-end gap-2">
      <label className="grid gap-1 text-sm text-text-dim">App (its id in Canyon)
        <input className={FIELD} value={app} placeholder="funny-snake-4k2" onChange={(event) => setApp(event.target.value)} />
      </label>
      <label className="grid gap-1 text-sm text-text-dim">Folder (empty for the whole repository)
        <input className={FIELD} value={path} onChange={(event) => setPath(event.target.value)} />
      </label>
      {app.trim() === ""
        ? <Button size="sm" disabled reason="Type the app's id: it is on its page, beside its name.">Link</Button>
        : <Button size="sm" onClick={() => act(linkRepository(app.trim(), repo.repo_id, path.trim()))}>Link</Button>}
      <Button size="sm" variant="ghost" onClick={cancel}>Cancel</Button>
    </span>
  );
}

// Whether the org's new repositories become apps (#1923): a switch saved as it is flipped, with why to
// be careful under it while it is on.
function NewRepositories({ org }: { org: GitHubOrg }) {
  const [on, setOn] = useState(org.imports_new);
  const choice = useSaveChoice(); // a choice: it saves as it is made, and says so (D16)
  const flip = (next: boolean) => choice.save(() => setGitHubOrg(org.account, next).then(({ org: saved }) => setOn(saved.imports_new)));
  return (
    <span className="grid gap-1 whitespace-normal">
      <select aria-label={`New repositories in ${org.account}`} className="w-fit rounded-md border border-border bg-surface px-1.5 py-0.5 text-base" value={on ? "apps" : "admin"} onChange={(event) => void flip(event.target.value === "apps")}>
        <option value="apps">Become apps</option>
        <option value="admin">Picked by an admin</option>
      </select>
      {on && <span className="max-w-[320px] text-sm text-text-dim">{S.githubImportsWarning.replaceAll("<org>", org.account)}</span>}
      {choice.line()}
    </span>
  );
}

// The client secret of an App registered before Canyon kept one (#2228): its owner generates a new one on
// GitHub and adds Canyon's callback, and an admin pastes the secret here. Once saved, the org's row links
// accounts and this goes away; the secret is never shown again.
function ClientSecret({ org, saved }: { org: GitHubOrg; saved: (org: GitHubOrg) => void }) {
  const [secret, setSecret] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  useUnsaved(secret.trim() !== ""); // typed: its Save shows once pasted, and leaving asks first (D16)
  const save = () => setGitHubClientSecret(org.account, secret.trim()).then(({ org: next }) => saved(next), (err: unknown) => setFailure(said(err)));
  const callback = `${window.location.origin}/api/v1/github/link/callback`;
  return (
    <div className="min-w-0">
      <h2 className="text-base font-semibold">{S.githubSecretTitle.replace("<org>", () => org.account)}</h2>
      <p className={NOTE}>{S.githubSecretNote.split("<callback>").flatMap((part, at) => (at === 0 ? [part] : [<code key={at} className="font-mono text-sm break-all">{callback}</code>, part]))}</p>
      <label className={LABEL} htmlFor={`github-secret-${org.app_slug}`}>Client secret</label>
      <span className="flex max-w-[520px] flex-wrap items-center gap-2">
        <input id={`github-secret-${org.app_slug}`} type="password" autoComplete="off" className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-base" value={secret} onChange={(event) => setSecret(event.target.value)} />
        {secret.trim() !== "" && <Button variant="primary" onClick={() => void save()}>{S.githubSecretSave}</Button>}
      </span>
      {failure !== null && <p className="mt-1 text-base text-critical">{failure}</p>}
    </div>
  );
}
