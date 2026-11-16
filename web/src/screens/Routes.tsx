import { useEffect, useRef, useState } from "react";
import { listSecrets, putRoute, removeRoute, routeFromSecret, routesOf, said, type EgressEnv, type EgressRoute, type GatewayPending, type LicenceState, type RouteScheme } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { EnterpriseLine, unlocks } from "../ui/Enterprise";
import { pendingLine } from "../ui/pending";
import { EntryButton } from "./Outbound";

// An env's credentials on the Outbound card (design/EGRESS.md §4.1, §8.3; #2145): each value route, never its value,
// and the forms that add, replace, move and remove them.
//
// A route's value never comes back: no read carries it, and the form never holds it as state. It is typed into an
// uncontrolled password field, read from the field once when Save sends it, and the field is emptied as soon as the
// server took it, so neither the page nor React's state holds it after a save.

type Level = "admin" | "owner" | "contributor" | "viewer" | "responder";
type Env = "preview" | "prod";

// Who changes a credential (design/EGRESS.md §3.5; contracts/routes.md, the routes rows): a contributor and up adds,
// replaces and removes one; one moved from a secret, and a move itself, only an owner or an admin. Any other level
// changes nothing (fail closed); the server refuses whatever this misses.
const contributes = (level: Level) => level === "contributor" || level === "owner" || level === "admin";
const owns = (level: Level) => level === "owner" || level === "admin";

const SCHEMES: [RouteScheme, string][] = [["bearer", "Bearer token"], ["basic", "Basic, user:password"], ["raw", "As written"]];
const schemeWord = (scheme: RouteScheme) => SCHEMES.find(([key]) => key === scheme)?.[1] ?? scheme;
const FIELD = "w-full max-w-[520px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const LABEL = "mt-2 mb-1 block text-md font-medium";
const NOTE = "text-md text-text-dim";
const CARD = "grid grid-cols-[minmax(0,1fr)] gap-1 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// host is the host's: via off, credential routes off, or the org running the apps (contracts/routes.md, waiting). Every
// member reads credential_routes, so the line names it when it is the one; unread, the line about the calls.
const waitLine = (wait: EgressRoute["waiting"], credentials: boolean | undefined) =>
  wait === null ? null : wait.reason === "host" ? (credentials === false ? S.routeWaitCredentials : S.routeWaitHost) : wait.reason === "secret" ? S.routeWaitSecret.replace("<secret>", () => wait.secret ?? "") : S.routeWaitRedeploy;

type Form = { kind: "put"; route: EgressRoute | null } | { kind: "move" };

// `saved` reads the card again once a change landed. `level` is the caller's on the app. `licence` is the host's, null
// until read: adding and moving a credential is Canyon Enterprise's (#2879); replacing one, to rotate a leaked key, and
// removing one never are (#2881).
export function Routes({ id, env, name, read, level, licence, credentials, saved }: {
  id: string; env: Env; name: string; read: EgressEnv; level: Level; licence: LicenceState | null; credentials?: boolean; saved: () => Promise<void>;
}) {
  const keys = unlocks(licence, "outbound_keys");
  const [form, setForm] = useState<Form | null>(null);
  const [line, setLine] = useState<string | null>(null);
  const routes = routesOf(read);
  if (routes === null) return null; // a server from before credentials: nothing to list, nothing to add
  // The allowlist's own names, suggested: a route's certificate names one host, so never a *. entry. Which names a route
  // may take is the server's to judge (any under observe, one a *. entry covers, a platform destination); only an
  // allowlist with nothing on it can take none.
  const destinations = read.allowed.filter((entry) => !entry.startsWith("*."));
  const none = read.mode === "allowlist" && read.allowed.length === 0;
  const done = async (said: string) => { setForm(null); setLine(said); await saved(); };
  const remove = (route: EgressRoute) => async () => {
    const pending = await removeRoute(id, env, route.destination);
    await done(pendingLine(pending) ?? S.routeRemoved.replace("<destination>", () => route.destination));
  };
  return (
    <section aria-label={`${name} credentials`} className="grid grid-cols-[minmax(0,1fr)] gap-1.5">
      <h4 className="text-md font-medium">{S.routesHeading}</h4>
      {routes.length === 0 ? <p className={NOTE}>{S.routesNone}</p> : (
        <ul className="grid gap-1.5">
          {routes.map((route) => {
            const changes = route.from_secret ? owns(level) : contributes(level);
            const wait = waitLine(route.waiting, credentials);
            return (
              <li key={route.destination} aria-label={route.destination} className="grid gap-0.5">
                <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-md">
                  <span className="font-mono">{route.destination}</span>
                  <span className="text-text-dim">{`${route.header} · ${schemeWord(route.scheme)}`}</span>
                  {route.from_secret && <span className="text-text-faint">{S.routeFromSecret}</span>}
                  {changes && form === null && (<>
                    <Button size="sm" label={`Replace the credential for ${route.destination}`} onClick={() => { setLine(null); setForm({ kind: "put", route }); }}>Replace</Button>
                    <EntryButton word="Remove" busy="Removing…" label={`Remove the credential for ${route.destination}`} act={remove(route)} />
                  </>)}
                </span>
                {route.placeholder_env !== null && <span className={NOTE}>{S.routePlaceholder.replace("<variable>", () => route.placeholder_env ?? "")}</span>}
                {wait !== null && <span className="text-md text-amber">{wait}</span>}
                {route.from_secret && contributes(level) && !owns(level) && <span className="text-md text-text-faint">{S.routeOwnerOnly}</span>}
              </li>
            );
          })}
        </ul>
      )}
      {line !== null && <p role="status" className="text-md text-ok">{line}</p>}
      {form === null && contributes(level) && licence !== null && !keys && <EnterpriseLine feature="outbound_keys" />}
      {form === null && contributes(level) && keys && (
        <span className="flex flex-wrap gap-2">
          <Button size="sm" onClick={() => { setLine(null); setForm({ kind: "put", route: null }); }}>Add a credential</Button>
          {owns(level) && <Button size="sm" onClick={() => { setLine(null); setForm({ kind: "move" }); }}>Move a secret into a credential</Button>}
        </span>
      )}
      {form?.kind === "put" && <RouteForm id={id} env={env} name={name} route={form.route} destinations={destinations} none={none} done={done} cancel={() => setForm(null)} />}
      {form?.kind === "move" && <MoveForm id={id} env={env} name={name} destinations={destinations} none={none} done={done} cancel={() => setForm(null)} />}
    </section>
  );
}

// The fields both forms share: where, which header and how; the placeholder optional.
function Shape({ prefix, destination, destinations, header, scheme, placeholder, set }: {
  prefix: string; destination: string; destinations: string[] | null; header: string; scheme: RouteScheme; placeholder: string;
  set: (change: Partial<{ destination: string; header: string; scheme: RouteScheme; placeholder: string }>) => void;
}) {
  return (<>
    {destinations === null ? <p className="text-md">{`Destination: ${destination}`}</p> : (<>
      <label htmlFor={`${prefix}-destination`} className={LABEL}>Destination</label>
      <input id={`${prefix}-destination`} className={FIELD} value={destination} list={`${prefix}-destinations`} autoComplete="off" spellCheck={false}
        onChange={(e) => set({ destination: e.target.value.trim() })} />
      <datalist id={`${prefix}-destinations`}>{destinations.map((one) => <option key={one} value={one} />)}</datalist>
    </>)}
    <label htmlFor={`${prefix}-header`} className={LABEL}>Header</label>
    <input id={`${prefix}-header`} className={FIELD} value={header} autoComplete="off" spellCheck={false} onChange={(e) => set({ header: e.target.value })} />
    <label htmlFor={`${prefix}-scheme`} className={LABEL}>How Canyon sends it</label>
    <select id={`${prefix}-scheme`} className={FIELD} value={scheme} onChange={(e) => set({ scheme: e.target.value as RouteScheme })}>
      {SCHEMES.map(([key, word]) => <option key={key} value={key}>{word}</option>)}
    </select>
    <label htmlFor={`${prefix}-placeholder`} className={LABEL}>Placeholder variable</label>
    <input id={`${prefix}-placeholder`} className={FIELD} value={placeholder} autoComplete="off" spellCheck={false} aria-describedby={`${prefix}-placeholder-note`} onChange={(e) => set({ placeholder: e.target.value })} />
    <p id={`${prefix}-placeholder-note`} className={NOTE}>{S.routePlaceholderNote}</p>
  </>);
}

// Sending: Save turns to Saving… and the server's refusal is said in its words, the fields kept to fix.
function useSend(done: (said: string) => Promise<void>) {
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const live = useRef(true);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  const send = async (call: () => Promise<GatewayPending>, after: () => void, line: string) => {
    if (busy) return;
    setBusy(true);
    setRefusal(null);
    try {
      const pending = await call();
      after();
      await done(pendingLine(pending) ?? line);
    } catch (err) {
      if (live.current) setRefusal(said(err));
    }
    if (live.current) setBusy(false);
  };
  return { busy, refusal, send };
}

// Add a credential, or replace one: a PUT sets it whole, so its value is typed again every time.
function RouteForm({ id, env, name, route, destinations, none, done, cancel }: {
  id: string; env: Env; name: string; route: EgressRoute | null; destinations: string[]; none: boolean; done: (said: string) => Promise<void>; cancel: () => void;
}) {
  const [shape, setShape] = useState({ destination: route?.destination ?? destinations[0] ?? "", header: route?.header ?? "Authorization", scheme: route?.scheme ?? "bearer" as RouteScheme, placeholder: route?.placeholder_env ?? "" });
  const value = useRef<HTMLInputElement>(null); // the credential lives in the field alone: never state, never a prop
  const { busy, refusal, send } = useSend(done);
  if (route === null && none) {
    return <div className={CARD}><p className={NOTE}>{S.routeNoDestination}</p><span className="flex"><Button size="sm" variant="ghost" onClick={cancel}>Cancel</Button></span></div>;
  }
  const save = () => send(
    () => putRoute(id, env, shape.destination, { header: shape.header, scheme: shape.scheme, value: value.current?.value ?? "", ...(shape.placeholder === "" ? {} : { placeholder_env: shape.placeholder }) }),
    () => { if (value.current !== null) value.current.value = ""; }, // taken: the field holds it no longer
    S.routeSaved.replace("<env>", () => name).replace("<destination>", () => shape.destination),
  );
  const prefix = `route-${env}`;
  return (
    <form aria-label={route === null ? `Add a credential to ${name}` : `Replace the credential for ${route.destination}`} className={CARD} onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <fieldset disabled={busy} className="contents">
        <Shape prefix={prefix} destination={shape.destination} destinations={route === null ? destinations : null} header={shape.header} scheme={shape.scheme} placeholder={shape.placeholder}
          set={(change) => setShape((was) => ({ ...was, ...change }))} />
        <label htmlFor={`${prefix}-value`} className={LABEL}>Credential</label>
        <input id={`${prefix}-value`} ref={value} type="password" className={FIELD} autoComplete="off" data-1p-ignore data-lpignore="true" spellCheck={false} aria-describedby={`${prefix}-value-note`} />
        <p id={`${prefix}-value-note`} className={NOTE}>{S.routeValueNote}</p>
      </fieldset>
      {refusal !== null && <p role="alert" className="text-md text-critical">{refusal}</p>}
      <span className="mt-2 flex flex-wrap gap-2">
        {busy ? <Button disabled reason="Saving…">Save</Button> : <Button variant="primary" label="Save the credential" onClick={() => void save()}>Save</Button>}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </form>
  );
}

// Move a runtime secret of the env into a credential, an owner's: by its name, never its value, which the server moves.
function MoveForm({ id, env, name, destinations, none, done, cancel }: {
  id: string; env: Env; name: string; destinations: string[]; none: boolean; done: (said: string) => Promise<void>; cancel: () => void;
}) {
  const [secrets, setSecrets] = useState<string[] | null>(null);
  const [unread, setUnread] = useState<string | null>(null);
  const [secret, setSecret] = useState("");
  const [shape, setShape] = useState({ destination: destinations[0] ?? "", header: "Authorization", scheme: "bearer" as RouteScheme, placeholder: "" });
  const { busy, refusal, send } = useSend(done);
  useEffect(() => {
    let live = true;
    listSecrets(id).then(
      ({ secrets: all }) => { if (!live) return; const names = all.filter((one) => one.env === env && one.where === "runtime").map((one) => one.name).sort(); setSecrets(names); setSecret(names[0] ?? ""); },
      (err: unknown) => { if (live) setUnread(said(err)); },
    );
    return () => { live = false; };
  }, [id, env]);
  const close = <span className="flex"><Button size="sm" variant="ghost" onClick={cancel}>Cancel</Button></span>;
  if (unread !== null) return <div className={CARD}><p role="alert" className="text-md text-critical">{unread}</p>{close}</div>;
  if (secrets === null) return null;
  if (none) return <div className={CARD}><p className={NOTE}>{S.routeNoDestination}</p>{close}</div>;
  if (secrets.length === 0) return <div className={CARD}><p className={NOTE}>{S.routeMoveNone}</p>{close}</div>;
  const save = () => send(
    () => routeFromSecret(id, env, shape.destination, { secret, header: shape.header, scheme: shape.scheme, ...(shape.placeholder === "" ? {} : { placeholder_env: shape.placeholder }) }),
    () => {},
    S.routeMoved.replace("<secret>", () => secret).replace("<destination>", () => shape.destination),
  );
  const prefix = `move-${env}`;
  return (
    <form aria-label={`Move a secret into a credential for ${name}`} className={CARD} onSubmit={(e) => { e.preventDefault(); void save(); }}>
      <fieldset disabled={busy} className="contents">
        <label htmlFor={`${prefix}-secret`} className={LABEL}>Secret</label>
        <select id={`${prefix}-secret`} className={FIELD} value={secret} aria-describedby={`${prefix}-secret-note`} onChange={(e) => setSecret(e.target.value)}>
          {secrets.map((one) => <option key={one} value={one}>{one}</option>)}
        </select>
        <p id={`${prefix}-secret-note`} className={NOTE}>{S.routeMoveNote}</p>
        <Shape prefix={prefix} destination={shape.destination} destinations={destinations} header={shape.header} scheme={shape.scheme} placeholder={shape.placeholder}
          set={(change) => setShape((was) => ({ ...was, ...change }))} />
      </fieldset>
      {refusal !== null && <p role="alert" className="text-md text-critical">{refusal}</p>}
      <span className="mt-2 flex flex-wrap gap-2">
        {busy ? <Button disabled reason="Moving…">Move</Button> : <Button variant="primary" label="Move the secret" onClick={() => void save()}>Move</Button>}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </form>
  );
}
