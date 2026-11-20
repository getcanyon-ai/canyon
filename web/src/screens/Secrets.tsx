import { useEffect, useRef, useState } from "react";
import { listSecrets, removeSecret, said, setSecret, type Secret } from "../api";
import { SECRETS as T } from "../strings";
import { Actor } from "../ui/Actor";
import { Age } from "../ui/Age";
import { Button } from "../ui/Button";
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
export function Secrets({ id }: { id: string }) {
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

  const [name, setName] = useState("");
  const [env, setEnv] = useState<Env>("preview");
  const [where, setWhere] = useState<Where>("runtime");
  const value = useRef<HTMLInputElement>(null);
  const [typed, setTyped] = useState(false); // whether Value holds anything: the value itself is never in state
  const [busy, setBusy] = useState(false);
  const [said_, setSaid] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [asking, setAsking] = useState<Secret | null>(null);

  const set = async () => {
    if (busy) return;
    setBusy(true);
    setSaid(null);
    setRefusal(null);
    try {
      await setSecret(id, name, { value: value.current?.value ?? "", env, where });
      if (value.current !== null) value.current.value = ""; // taken: the field holds it no longer
      setTyped(false);
      setSaid(named(env === "prod" ? T.setProd : T.setPreview, name));
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
  // Replace fills the form from its row, and the value is the person's to type: focus goes there.
  const replace = (row: Secret) => {
    setName(row.name);
    setEnv(row.env);
    setWhere(row.where);
    setSaid(null);
    setRefusal(null);
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
    <section aria-label={T.title} className="grid grid-cols-[minmax(0,1fr)] gap-3">
      <h2 className={H2}>{T.title}</h2>
      <p className={NOTE}>{T.about}</p>
      {unread !== null && <p role="alert" className="text-base text-critical">{unread}</p>}
      {rows !== null && (
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
      <form aria-label={T.form} className="grid max-w-[560px] gap-2 rounded-xl border border-border bg-surface px-3.5 py-3" onSubmit={(event) => { event.preventDefault(); void set(); }}>
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
        <span className="mt-1 flex flex-wrap items-center gap-3">
          {busy ? <Button disabled reason="Saving…">{T.set}</Button>
            : name === "" ? <Button disabled reason={T.nameFirst}>{T.set}</Button>
            : !typed ? <Button disabled reason={T.valueFirst}>{T.set}</Button>
            : <Button variant="primary" onClick={() => void set()}>{T.set}</Button>}
          <span className={NOTE}>{T.replaces}</span>
        </span>
        {refusal !== null && <p role="alert" className="text-md text-critical">{refusal}</p>}
        {said_ !== null && <p role="status" className="text-md text-ok">{said_}</p>}
      </form>
    </section>
  );
}
