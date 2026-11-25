import { useEffect, useRef, useState, type FormEvent } from "react";
import { ApiError, dismissRequest, getAccess, said, share, unshare, type AccessRead, type Actor as ActorRef, type GrantKind, type Level, type TeamAccess } from "../api";
import { ACCESS_WORD, S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age } from "../ui/Age";
import { Button } from "../ui/Button";
import { EnterpriseLine, unlocks, useLicence } from "../ui/Enterprise";
import { useMedia } from "../ui/media";
import { CELL, Table, TableWrap } from "../ui/Table";

const H2 = "text-base font-semibold";
const LINE = "mt-2.5 text-base text-text-dim";
const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
// Screens say read-only for the API's viewer, and never the words grant or level (EXPERIENCE.md D6).
const WORD: Record<Level, string> = ACCESS_WORD;
const LEVELS = Object.entries(WORD) as [Level, string][];
const RANK: Record<Level, number> = { viewer: 0, contributor: 1, owner: 2 }; // a group's level, lowered without a licence, never raised
const COLUMNS = ["Who", "Access", "Roles", "Added by", "When", "Remove"].map((label, at) => ({ key: String(at), label }));

// How far an app is shared, as the shared line's parts (EXPERIENCE.md D5): its people and its groups,
// a zero part left out. Everyone is said instead of them, never beside them.
export function shareParts(access: TeamAccess): string[] {
  const part = (n: number, one: string, many: string) => (n === 0 ? [] : [`${n} ${n === 1 ? one : many}`]);
  return [...part(access.people, "person", "people"), ...part(access.groups, "group", "groups")];
}
// Its people and groups are besides its owners, whom the card's table lists too (EXPERIENCE.md D5; #2843).
const sharedWith = (access: TeamAccess, org: string) => {
  const parts = shareParts(access);
  return access.everyone ? `everyone at ${org}` : parts.length === 0 ? "" : `${parts.join(" and ")} besides its owners`;
};
// The shared line, as the card's summary and the page's head say it.
export const sharedLine = (access: TeamAccess, org: string) => { const words = sharedWith(access, org); return words === "" ? S.private : `Shared with ${words}.`; };
// A reader asking for the card from the page's head (asked: when, and whether to share): it opens, comes into view, and
// to whoever manages it, puts the keyboard in Share with.
export type Asked = { at: number; share: boolean } | null;
const useAsked = (asked: Asked, box?: React.RefObject<HTMLInputElement | null>) => {
  const section = useRef<HTMLElement>(null);
  useEffect(() => {
    if (asked === null || section.current === null) return;
    const details = section.current.querySelector("details");
    if (details !== null) details.open = true;
    section.current.scrollIntoView?.({ block: "start" });
    if (asked.share) box?.current?.focus();
  }, [asked, box]);
  return section;
};

// The add box takes what `canyon share` takes: an address, group:<name>, or everyone at the org.
function grantOf(typed: string, org: string, groups: boolean): { kind: GrantKind; grantee: string } {
  const text = typed.trim();
  if (["everyone", `everyone at ${org}`.toLowerCase()].includes(text.toLowerCase())) return { kind: "everyone", grantee: "everyone" };
  if (text.startsWith("group:")) return { kind: "group", grantee: text.slice("group:".length) };
  // An email address has an @ and a group's name never does, so where groups may be shared a name without one is a group's
  // (#102): nobody types a prefix. Where they may not, it is an address still being typed.
  return groups && !text.includes("@") ? { kind: "group", grantee: text } : { kind: "email", grantee: text };
}

// The app's Access card (design/PERMISSIONS.md). Its owners and admins manage the team here; a
// contributor reads one line naming whom to ask. Below contributor the page never draws it.
export function Access({ id, access, owners, groups, org, auth, asked = null }: { id: string; access: TeamAccess; owners: ActorRef[]; groups: string[]; org: string; auth: "oidc" | "off" | null; asked?: Asked }) {
  const section = useAsked(asked);
  // Nothing is shared where nobody signs in (contracts/routes.md, Access): the server routes none
  // of this group on a host with no sign-in, so the card asks nothing rather than asking and losing
  // the 404 to the console (canyonv2-028b). Also nothing while the caller doesn't know the mode yet
  // (auth === null): asking on a guess is exactly the race this card must not run.
  if (auth !== "oidc") return null;
  if (access.requests !== undefined) return <Manage id={id} access={access} org={org} asked={asked} />;
  const owner = owners[0]?.display;
  const group = groups[0];
  const line = owner !== undefined ? S.sharedAsk.replace("<owner>", () => owner) : group !== undefined ? S.sharedAskGroup.replace("<group>", () => group) : S.sharedAskNoOwner;
  return (
    <section ref={section} aria-label="Access">
      <details>
        <summary className="cursor-pointer"><h2 className={`${H2} inline`}>Who can open it</h2></summary>
        <p className={LINE}>{line.replace("…", () => sharedWith(access, org))}</p>
      </details>
    </section>
  );
}

type Row = { kind: GrantKind; grantee: string; display: string; level: Level; roles: string[]; granted_by?: ActorRef; granted_at?: string };

function Manage({ id, access, org, asked }: { id: string; access: TeamAccess; org: string; asked: Asked }) {
  const [read, setRead] = useState<AccessRead | null>(null);
  // A host with no sign-in routes no Access group (contracts/routes.md, Access): nothing to share there.
  const [unrouted, setUnrouted] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const [who, setWho] = useState("");
  const [level, setLevel] = useState<Level>("viewer");
  const [roles, setRoles] = useState<string[]>([]);
  const box = useRef<HTMLInputElement>(null);
  const section = useAsked(asked, box);
  // Adding a group, or raising a group's level, is Canyon Enterprise's (#2879); lowering or removing one never is (the
  // lead, #2881), and a group already added keeps its access whatever the licence.
  const licence = useLicence();
  const groupsUnlocked = unlocks(licence, "group_sync");
  const phone = useMedia("(max-width: 719px)"); // the table's 640px and the page's gutters
  const sending = useRef(false);
  // The page's poll carries the counts; when one moves, a grant or a request changed elsewhere.
  const moved = `${access.everyone} ${access.people} ${access.groups} ${access.requests}`;
  useEffect(() => {
    if (!id) return; // a backstop: the router never hands this card an id-less route, but nothing reads before a real id either way
    let live = true;
    getAccess(id).then(
      (answer) => { if (live) { setRead(answer); setFailure(null); } },
      (err: unknown) => { if (live) { if (err instanceof ApiError && err.status === 404) setUnrouted(true); else setFailure(said(err)); } },
    );
    return () => { live = false; };
  }, [id, moved]);

  // A share answers the read; an unshare or a dismissal answers 204, so the read follows it. A control
  // that leaves the card hands the keyboard to the add box rather than dropping it on the page.
  // One change at a time: a second click on Remove would be a second DELETE, answered 404.
  const change = async (send: () => Promise<AccessRead | void>, gone = false) => {
    if (sending.current) return false;
    sending.current = true;
    try {
      setRead((await send()) ?? (await getAccess(id)));
      setFailure(null);
      if (gone) box.current?.focus();
      return true;
    } catch (err) {
      setFailure(said(err));
      return false;
    } finally {
      sending.current = false;
    }
  };

  // Nothing until the read answers: a host with no sign-in answers 404, and a card drawn before that would flash.
  if (unrouted || (read === null && failure === null)) return null;
  const typed = grantOf(who, org, groupsUnlocked);
  // The one text box submits on Enter as the button does.
  const add = async (event?: FormEvent) => {
    event?.preventDefault();
    if (who.trim() === "") return;
    const body = { level: typed.kind === "everyone" ? "viewer" : level, roles } as const;
    if (await change(() => share(id, typed.kind, typed.grantee, body))) { setWho(""); setLevel("viewer"); setRoles([]); }
  };
  // The card's own read once it has one, counted as the project read counts (contracts/routes.md): a share or an
  // unshare answers it at once, while the page's counts wait for its next poll (#2560).
  const words = sharedWith(read === null ? access : {
    ...access,
    everyone: read.everyone !== null,
    people: read.entries.filter((one) => one.kind === "email" && one.level !== "owner").length,
    groups: read.entries.filter((one) => one.kind === "group").length,
  }, org);
  const rows: Row[] = read === null ? [] : [
    ...(read.everyone === null ? [] : [{ kind: "everyone" as const, grantee: "everyone", display: `Everyone at ${org}`, level: "viewer" as const, roles: read.everyone.roles }]),
    ...read.entries,
  ];
  // A grant's parts, the same in the table and in a phone's card: who, the access to change, and its Remove.
  const nameOf = (row: Row) => (row.kind === "group" ? S.groupNamed.replace("<group>", () => row.grantee) : row.display);
  const whoOf = (row: Row) => (
    <>{row.kind === "group" ? <span>{row.grantee}</span> : row.display}{row.kind === "group" && <span className="ml-2 text-sm text-text-faint">{S.group}</span>}{row.kind === "email" && row.display !== row.grantee && <span className="ml-2 font-mono text-sm break-all text-text-faint">{row.grantee}</span>}</>
  );
  const levelOf = (row: Row) => (row.kind === "everyone" ? WORD[row.level] : (
    <select aria-label={`Access for ${nameOf(row)}`} className={FIELD} value={row.level}
      onChange={(event) => void change(() => share(id, row.kind, row.grantee, { level: event.target.value as Level, roles: row.roles }))}>
      {LEVELS.filter(([key]) => row.kind !== "group" || groupsUnlocked || RANK[key] <= RANK[row.level]).map(([key, word]) => <option key={key} value={key}>{word}</option>)}
    </select>
  ));
  const remover = (row: Row) => <button type="button" className="text-base text-link" aria-label={`Remove ${nameOf(row)}`} onClick={() => void change(() => unshare(id, row.kind, row.grantee), true)}>Remove</button>;
  return (
    <section ref={section} aria-label="Access">
      {/* Closed until asked for, its line on the summary; open while access requests wait on its owners. */}
      <details open={(access.requests ?? 0) > 0}>
      <summary className="cursor-pointer"><h2 className={`${H2} inline`}>Who can open it<span className="ml-2 text-md font-regular text-text-faint">{words === "" ? S.private : `Shared with ${words}.`}</span></h2></summary>
      {failure !== null && <p className="mt-2 text-base text-critical">{failure}</p>}
      {read !== null && (
        <>
          {/* on a phone each grant is a card, as each secret is: the table's selects and Remove start past its edge (#126) */}
          {phone ? (
            <ul className="mt-2.5 grid gap-2.5">
              {rows.map((row) => (
                <li key={`${row.kind}/${row.grantee}`} className="grid gap-2 rounded-xl border border-border bg-surface px-3.5 py-3">
                  <span className="break-words">{whoOf(row)}</span>
                  <dl className="grid grid-cols-[max-content_minmax(0,1fr)] items-center gap-x-3 gap-y-1 text-md">
                    <dt className="text-text-faint">Access</dt><dd>{levelOf(row)}</dd>
                    <dt className="text-text-faint">Roles</dt><dd>{row.roles.join(", ") || "—"}</dd>
                    <dt className="text-text-faint">Added by</dt><dd><Actor actor={row.granted_by ?? null} /></dd>
                    <dt className="text-text-faint">When</dt><dd><Age at={row.granted_at ?? null} /></dd>
                  </dl>
                  <span>{remover(row)}</span>
                </li>
              ))}
            </ul>
          ) : (
            <TableWrap minWidth={640}>
              <Table columns={COLUMNS} rows={rows} renderRow={(row) => (
                <tr key={`${row.kind}/${row.grantee}`}>
                  <td className={CELL}>{whoOf(row)}</td>
                  <td className={CELL}>{levelOf(row)}</td>
                  <td className={CELL}>{row.roles.join(", ") || "—"}</td>
                  <td className={CELL}><Actor actor={row.granted_by ?? null} /></td>
                  <td className={CELL}><Age at={row.granted_at ?? null} /></td>
                  <td className={CELL}>{remover(row)}</td>
                </tr>
              )} />
            </TableWrap>
          )}
          {/* the box on its own line, and the access and Share under it, so the button's reason sits under it and never floats (#102) */}
          <form onSubmit={(event) => void add(event)} className="mt-3 grid max-w-[640px] gap-2">
            <input ref={box} aria-label="Share with" list="share-with" className={`${FIELD} min-w-0`} value={who}
              placeholder={(groupsUnlocked ? S.shareWithHint : S.shareWithHintNoGroups).replace("<org>", () => org)} onChange={(event) => setWho(event.target.value)} />
            <datalist id="share-with"><option value={`Everyone at ${org}`} /></datalist>
            <span className="flex flex-wrap items-start gap-2">
            {typed.kind === "everyone" ? <span className="px-2.5 py-1.5 text-base">{WORD.viewer}</span> : (
              <select aria-label="Access" className={FIELD} value={level} onChange={(event) => setLevel(event.target.value as Level)}>
                {LEVELS.map(([key, word]) => <option key={key} value={key}>{word}</option>)}
              </select>
            )}
            {typed.kind === "group" && !groupsUnlocked ? null
              : who.trim() === "" ? <Button disabled reason="Needs someone to share with">Share</Button> : <Button variant="primary" onClick={() => void add()}>Share</Button>}
            </span>
          </form>
          {typed.kind === "group" && !groupsUnlocked && licence !== null && <div className="mt-2"><EnterpriseLine feature="group_sync" /></div>}
          {read.roles.length > 0 && (
            <fieldset className="mt-2.5">
              <legend className="text-md text-text-dim">{S.roles}</legend>
              {read.roles.map((role) => (
                <label key={role.name} className="mt-1 flex items-baseline gap-2 text-base">
                  <input type="checkbox" checked={roles.includes(role.name)}
                    onChange={(event) => setRoles(event.target.checked ? [...roles, role.name] : roles.filter((one) => one !== role.name))} />
                  <span className="font-mono">{role.name}</span><span className="text-text-dim">{role.description}</span>
                </label>
              ))}
            </fieldset>
          )}
          {read.requests.length > 0 && (
            <>
              <h3 className="mt-4 text-xs tracking-caps text-text-faint uppercase">Requests</h3>
              <ul className="mt-1">
                {read.requests.map((one) => (
                  <li key={one.email} className="flex flex-wrap items-center gap-2 py-1 text-base">
                    <span>{one.display || one.email}</span>
                    {one.display !== one.email && <span className="font-mono text-sm text-text-faint">{one.email}</span>}
                    <Age at={one.requested_at} />
                    {/* a read-only member asked to join the team: Add makes them a contributor (#2774) */}
                    {one.level === "viewer"
                      ? <Button size="sm" label={`Add ${one.email} to its team`} onClick={() => void change(() => share(id, "email", one.email, { level: "contributor", roles: [] }), true)}>{S.addToTeam}</Button>
                      : <Button size="sm" label={`Add ${one.email} as read-only`} onClick={() => void change(() => share(id, "email", one.email, { level: "viewer", roles: [] }), true)}>{S.addReadOnly}</Button>}
                    <Button size="sm" variant="ghost" label={`Dismiss ${one.email}`} onClick={() => void change(() => dismissRequest(id, one.email), true)}>Dismiss</Button>
                  </li>
                ))}
              </ul>
            </>
          )}
        </>
      )}
      </details>
    </section>
  );
}
