import { useEffect, useRef, useState } from "react";
import { listSecrets, removeSecret, said, setSecret, type Secret } from "../api";
import { SECRETS as T } from "../strings";
import { Actor } from "../ui/Actor";
import { Age } from "../ui/Age";
import { Button } from "../ui/Button";
import { useMedia } from "../ui/media";
import { CELL, Table, TableWrap } from "../ui/Table";

const H2 = "text-base font-semibold";
const LABEL = "mb-1 block text-md font-medium";
const FIELD = "w-full rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "text-md text-text-dim";
type Env = Secret["env"];
type Where = Secret["where"];
const ENV: Record<Env, string> = { preview: T.preview, prod: T.prod };
const WHERE: Record<Where, string> = { runtime: T.runtime, build: T.build };
const named = (text: string, name: string) => text.replace("<name>", () => name);

// The app page's Secrets section (design/specs/screens.md, Secrets; design/specs/mcp.md §10.1): the app's secrets by name,
// and the dashboard's one form that sets one (PUT …/secrets/:name) or removes one. The app page draws it for a
// contributor and above alone, as the routes are theirs; below, the reduced page reads no secret. The value lives in its
// password field and nowhere else: never state, never a prop, never filled; it goes once in the PUT and the field is
// emptied when the PUT lands. Nothing the route checks is checked here: a refusal is the route's own message.
// A secret's name, as the server takes one (cmd/canyon-server/routes_secrets.go, validSecretName).
const SECRET_NAME = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;

// asked is what a link to the section names (#/apps/<app>/secrets?name=&env=, design/specs/mcp.md §2.3, canyon_secret_link):
// the form opens on that secret, scrolled to with its value's field ready. Only a name a secret may have and an
// environment there is are taken; a value never comes from a link.
// back is the way back to what sent the reader here, offered once the secret is set (#102: promote's missing secret);
// saved is told each time one is set. live: production runs, so a value it had is the one it keeps until a promotion (#145).
export function Secrets({ id, asked, back, saved, live = false }: { id: string; asked?: { name: string | null; env: string | null }; back?: { label: string; go: () => void }; saved?: () => void; live?: boolean }) {
  const [rows, setRows] = useState<Secret[] | null>(null);
  const [unread, setUnread] = useState<string | null>(null);
  const [reads, setReads] = useState(0);
  useEffect(() => {
    let live = true;
    listSecrets(id).then(
      ({ secrets }) => { // a read of a shape this section does not know is not read, never shown as no secrets
        if (!live) return;
        if (Array.isArray(secrets)) { setRows(secrets); setUnread(null); } else setUnread(T.unread);
      },
      (err: unknown) => { if (live) setUnread(said(err)); },
    );
    return () => { live = false; };
  }, [id, reads]);

  const [name, setName] = useState(asked?.name != null && SECRET_NAME.test(asked.name) ? asked.name : "");
  const [env, setEnv] = useState<Env>(asked?.env === "prod" ? "prod" : "preview");
  const here = useRef<HTMLElement>(null);
  const [where, setWhere] = useState<Where>("runtime");
  const value = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState(false); // whether Value holds anything: the value itself is never in state
  const [busy, setBusy] = useState(false);
  const [said_, setSaid] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [asking, setAsking] = useState<Secret | null>(null);
  // The form opens from its button, from Replace, or from a link that names a secret to set (#102).
  const [open, setOpen] = useState(asked !== undefined);
  // A phone reads each secret as a card: the table's seven columns overflow it (#102).
  const phone = useMedia("(max-width: 619px)");

  const set = async () => {
    if (busy) return;
    setBusy(true);
    setSaid(null);
    setRefusal(null);
    const had = live && env === "prod" && rows?.some((row) => row.name === name && row.env === "prod") === true;
    try {
      await setSecret(id, name, { value: value.current?.value ?? "", env, where });
      if (value.current !== null) value.current.value = ""; // taken: the field holds it no longer
      setTyped(false);
      setName(""); // a name left in the field would invite the next value to overwrite the same secret
      setSaid(`${named(env === "prod" ? T.setProd : T.setPreview, name)}${had ? ` ${T.keepsOld}` : ""}`);
      setOpen(false);
      saved?.();
      setReads((n) => n + 1);
    } catch (err) {
      setRefusal(said(err));
    }
    setBusy(false);
  };
  const remove = async (row: Secret) => {
    if (busy) return;
    setBusy(true);
    setSaid(null);
    setRefusal(null);
    try {
      await removeSecret(id, row.name, row.env);
      setSaid(named(row.env === "prod" ? T.removedProd : T.removedPreview, row.name));
      setAsking(null);
      setReads((n) => n + 1);
    } catch (err) {
      setRefusal(said(err));
    }
    setBusy(false);
  };
  useEffect(() => {
    if (asked === undefined) return;
    if (asked.name != null && SECRET_NAME.test(asked.name)) setName(asked.name);
    if (asked.env === "prod" || asked.env === "preview") setEnv(asked.env);
    setSaid(null);
    setOpen(true);
    value.current?.focus(); // already open, the open effect doesn't run again
    here.current?.scrollIntoView?.({ block: "start" });
  }, [asked]);
  // The value is the person's to type: focus goes there each time the form opens.
  useEffect(() => { if (open) value.current?.focus(); }, [open]);

  // Replace fills the form from its row, and the value is the person's to type: focus goes there.
  const replace = (row: Secret) => {
    setName(row.name);
    setEnv(row.env);
    setWhere(row.where);
    setSaid(null);
    setRefusal(null);
    setOpen(true);
    value.current?.focus();
  };

  const actions = (row: Secret) => {
    if (row.origin !== "user") return <span className="text-md text-text-faint">{T.canyons}</span>;
    if (asking !== null && asking.name === row.name && asking.env === row.env) {
      return (
        <span className="flex flex-wrap items-center gap-2">
          <span className="text-md">{named(row.env === "prod" ? T.askProd : T.askPreview, row.name)}</span>
          <Button size="sm" variant="danger" label={`${T.remove} ${row.name} from ${ENV[row.env]}`} onClick={() => void remove(row)}>{T.remove}</Button>
          <Button size="sm" variant="ghost" onClick={() => setAsking(null)}>{T.cancel}</Button>
        </span>
      );
    }
    return (
      <span className="flex flex-wrap gap-2">
        <Button size="sm" label={`${T.replace} ${row.name} in ${ENV[row.env]}`} onClick={() => replace(row)}>{T.replace}</Button>
        <Button size="sm" variant="ghost" label={`${T.remove} ${row.name} from ${ENV[row.env]}`} onClick={() => { setAsking(row); setSaid(null); setRefusal(null); }}>{T.remove}</Button>
      </span>
    );
  };

  return (
    <section ref={here} aria-label={T.title} className="grid grid-cols-[minmax(0,1fr)] gap-3">
      <h2 className={H2}>{T.title}</h2>
      <p className={NOTE}>{T.about}</p>
      {unread !== null && <p role="alert" className="text-base text-critical">{unread}</p>}
      {rows !== null && phone && (rows.length === 0 ? <p className={NOTE}>{T.none}</p> : (
        <ul className="grid gap-2.5">
          {rows.map((row) => (
            // a list item read whole: its name as text first, then its fields and its actions, each button named for its row
            <li key={`${row.env}/${row.name}`} className="grid gap-2 rounded-xl border border-border bg-surface px-3.5 py-3">
              <span className="font-mono text-md break-all">{row.name}</span>
              <dl className="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1 text-md">
                <dt className="text-text-faint">{T.env}</dt><dd>{ENV[row.env]}</dd>
                <dt className="text-text-faint">{T.where}</dt><dd>{WHERE[row.where]}</dd>
                <dt className="text-text-faint">Version</dt><dd>{row.version}</dd>
                <dt className="text-text-faint">Set by</dt><dd><Actor actor={row.set_by} /></dd>
                <dt className="text-text-faint">Set</dt><dd><Age at={row.set_at} long /></dd>
              </dl>
              {actions(row)}
            </li>
          ))}
        </ul>
      ))}
      {rows !== null && !phone && (
        <TableWrap minWidth={620}>
          <Table columns={[T.name, T.env, T.where, "Version", "Set by", "Set", ""].map((label, at) => ({ key: String(at), label }))} rows={rows} empty={T.none}
            renderRow={(row) => (
              <tr key={`${row.env}/${row.name}`}>
                <td className={CELL}><span className="font-mono text-md">{row.name}</span></td>
                <td className={CELL}>{ENV[row.env]}</td>
                <td className={CELL}>{WHERE[row.where]}</td>
                <td className={CELL}>{row.version}</td>
                <td className={CELL}><Actor actor={row.set_by} /></td>
                <td className={CELL}><Age at={row.set_at} long /></td>
                <td className={CELL}>{actions(row)}</td>
              </tr>
            )} />
        </TableWrap>
      )}
      {!open && <span><Button onClick={() => { setOpen(true); setSaid(null); setRefusal(null); }}>{T.form}</Button></span>}
      {open && <form aria-label={T.form} className="grid max-w-[560px] gap-2 rounded-xl border border-border bg-surface px-3.5 py-3" onSubmit={(event) => { event.preventDefault(); void set(); }}>
        <h3 className="text-md font-semibold">{T.form}</h3>
        <fieldset disabled={busy} className="contents">
          <label htmlFor="secret-name" className={LABEL}>{T.name}</label>
          <input id="secret-name" className={`${FIELD} font-mono`} value={name} autoComplete="off" spellCheck={false} onChange={(event) => setName(event.target.value)} />
          <label htmlFor="secret-value" className={LABEL}>{T.value}</label>
          <input id="secret-value" ref={value} type="password" className={FIELD} autoComplete="off" data-1p-ignore data-lpignore="true" spellCheck={false} onChange={(e) => setTyped(e.currentTarget.value !== "")} />
          <span className="grid grid-cols-2 gap-3 max-[520px]:grid-cols-1">
            <span>
              <label htmlFor="secret-env" className={LABEL}>{T.env}</label>
              <select id="secret-env" className={FIELD} value={env} onChange={(event) => setEnv(event.target.value as Env)}>
                <option value="preview">{T.preview}</option>
                <option value="prod">{T.prod}</option>
              </select>
            </span>
            <span>
              <label htmlFor="secret-where" className={LABEL}>{T.where}</label>
              <select id="secret-where" className={FIELD} value={where} onChange={(event) => setWhere(event.target.value as Where)}>
                <option value="runtime">{T.runtime}</option>
                <option value="build">{T.build}</option>
              </select>
            </span>
          </span>
        </fieldset>
        <span className="mt-1 flex flex-wrap items-start gap-2">
          {busy ? <Button disabled reason="Saving…">{T.set}</Button>
            : name === "" ? <Button disabled reason={T.nameFirst}>{T.set}</Button>
            : !typed ? <Button disabled reason={T.valueFirst}>{T.set}</Button>
            : <Button variant="primary" onClick={() => void set()}>{T.set}</Button>}
          <Button variant="ghost" onClick={() => { setOpen(false); setRefusal(null); }}>{T.cancel}</Button>
        </span>
        <p className={NOTE}>{T.replaces}</p>
      </form>}
      {refusal !== null && <p role="alert" className="text-md text-critical">{refusal}</p>}
      {said_ !== null && <p role="status" className="text-md text-ok">{said_}</p>}
      {said_ !== null && back !== undefined && <span><Button variant="secondary" onClick={back.go}>{back.label}</Button></span>}
    </section>
  );
}
