import { useEffect, useRef, useState } from "react";
import { allowDestination, ApiError, disallowDestination, dismissEgressRequest, getEgress, getFrontDoor, getProjectEgress, putEgressMode, said, waiting, type EgressDestination, type EgressEnv, type EgressMode, type EgressRequest, type ProjectEgress, type Status } from "../api";
import { S } from "../strings";
import { pendingLine } from "../ui/pending";
import { Age, ago } from "../ui/Age";
import { Button } from "../ui/Button";
import { Chip } from "../ui/Chip";
import { useMedia } from "../ui/media";
import { CELL, Table, TableWrap } from "../ui/Table";
import { Routes } from "./Routes";
import { useLicence } from "../ui/Enterprise";

const OUTBOUND = {
  platform: "runs on your own platform",
  floorKube: "may skip Canyon until its next deploy or promotion", // an env's label on Kubernetes: no network to recreate
  deployAgain: "not yet through Canyon: deploy again",
} as const;

const ENVS = [["preview", "Preview"], ["prod", "Production"]] as const;
type EnvKey = (typeof ENVS)[number][0];

const failed = (row: EgressDestination) => row.last_response_flags.filter((flag) => flag !== "DC" && flag !== "UC");
// A refusal no allowlist can lift says why beside its row (#2723): an IP address, then a port other than 80 or 443.
const forGood = (row: EgressDestination) => row.last_outcome !== "refused" ? null
  : row.destination !== "invalid" && !named(row.destination) ? S.refusedAddress : row.port !== 80 && row.port !== 443 ? S.refusedPort.replace("<port>", () => String(row.port)) : null;
const result = (row: EgressDestination) =>
  row.last_outcome === "refused" ? forGood(row) ?? "refused"
    : row.last_protocol === "connect" && failed(row).length > 0 ? `tunnel failed: ${failed(row).join(",")}`
      : row.last_status_class === "none" ? "no answer" : row.last_status_class;

// What allowlist decides (contracts/routes.md, The check): an entry allows its name, `*.` and a name every
// name any depth below it and never the name itself; a platform destination is allowed for every app. A row
// the check refuses in every mode — `invalid`, a dotted IPv4 address (a name never ends in a number,
// egress.schema.json's `name`), a port other than 80 or 443 — is no allowlist's to allow.
const MODES = new Set<unknown>(["observe", "allowlist"]);
const named = (destination: string) => destination !== "invalid" && !/^[0-9]+$/.test(destination.slice(destination.lastIndexOf(".") + 1));
const allows = (allowed: string[], name: string) => allowed.some((entry) => (entry.startsWith("*.") ? name.endsWith(entry.slice(1)) : entry === name));
const allowable = (row: EgressDestination, env: EgressEnv, platform: string[]) =>
  named(row.destination) && (row.port === 80 || row.port === 443) && !allows(env.allowed, row.destination) && !platform.includes(row.destination);
// The rows allowlist would refuse that the env's mode lets through now: its last call went out.
export const wouldRefuse = (env: EgressEnv, platform: string[]) => env.destinations.filter((row) => row.last_outcome === "allowed" && allowable(row, env, platform));
// What preview connected to that production's allowlist doesn't hold, each name once and none it has a request of: a
// person's way to answer the go-live row, which names them, before production ever refused one.
const prodRefuses = (egress: ProjectEgress) => {
  const { preview, prod } = egress.envs;
  if (prod?.mode !== "allowlist" || !Array.isArray(prod.requests) || !Array.isArray(prod.allowed) || !Array.isArray(preview?.destinations)) return []; // an env the card draws is read above; this one may not be drawn
  const asked = new Set(prod.requests.map((one) => one.destination));
  return [...new Set(preview.destinations.filter((row) => allowable(row, prod, egress.platform) && !asked.has(row.destination)).map((row) => row.destination))].sort();
};
const modeWord = (mode: EgressMode) => (mode === "allowlist" ? S.modeAllowlist : S.modeObserve);
// Settings › Outbound in a line, a link to it for an admin, who recreates networks there (#2774).
const SETTINGS = "Settings › Outbound";
const linked = (line: string, admin: boolean) => {
  if (!admin || !line.includes(SETTINGS)) return line;
  const [before, after] = line.split(SETTINGS);
  return <>{before}<a className="text-link no-underline hover:underline" href="#/settings/outbound">{SETTINGS}</a>{after}</>;
};
const count = (n: number) => n.toLocaleString("en-US");

// Allow, a widening, and Remove, a narrowing: one entry each, the destination's name. The server's refusal is
// said beside it in its words.
export function EntryButton({ word, busy, label, act }: { word: string; busy: string; label: string; act: () => Promise<void> }) {
  const [state, setState] = useState<null | "sending" | { refusal: string }>(null);
  const live = useRef(true);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  const send = async () => {
    if (state === "sending") return;
    setState("sending");
    try {
      await act();
      if (live.current) setState(null);
    } catch (err) {
      if (live.current) setState({ refusal: said(err) });
    }
  };
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      {state === "sending" ? <Button size="sm" disabled reason={busy} label={label}>{word}</Button>
        : <Button size="sm" label={label} onClick={() => void send()}>{word}</Button>}
      {typeof state === "object" && state !== null && <span role="alert" className="text-md whitespace-normal text-critical">{state.refusal}</span>}
    </span>
  );
}

// Destinations as the card and the impact lists draw them: last seen, calls (grouped, right-aligned, tabular),
// the last result where it is the card's, and a column Allow keeps whether or not a row has one, at fixed widths
// past the destination, so tables stacked one under another line up. A phone gets a list instead (below).
const refusedLine = (row: EgressRequest) => (row.refusals === 1 ? S.refusedOnce : S.refusedMany.replace("<n>", () => count(row.refusals))).replace("<age>", () => ago(row.last_refused_at));

const STACK = "divide-y divide-border rounded-xl border border-border bg-surface";

// An env's egress requests under their heading: what Canyon refuses now, with Allow and Dismiss, opens the env; the
// dismissed ones, with Allow, which file no new request, follow its allowlist (design/EGRESS.md §3.4; D15).
function Requests({ heading, rows, allow, dismiss }: { heading: string; rows: EgressRequest[]; allow: (name: string) => Promise<void>; dismiss: (name: string) => Promise<void> }) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
      <h4 className="text-md font-medium">{heading}</h4>
      <ul className={STACK}>
        {rows.map((row) => (
          <li key={row.destination} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2.5">
            <span className="grid min-w-0 gap-0.5">
              <span className="font-mono text-md break-all">{row.destination}</span>
              <span className="text-sm text-text-dim">{refusedLine(row)}{row.state === "dismissed" && ` · ${S.dismissedMark}`}</span>
            </span>
            <span className="flex flex-wrap items-center gap-2">
              <EntryButton word={S.allow} busy="Allowing…" label={`Allow ${row.destination}`} act={() => allow(row.destination)} />
              {row.state === "pending" && <EntryButton word={S.dismiss} busy="Dismissing…" label={`Dismiss ${row.destination}`} act={() => dismiss(row.destination)} />}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function ProdRefuses({ names, allow }: { names: string[]; allow: (name: string) => Promise<void> }) {
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-1">
      <p className="text-md text-text-dim">{S.prodRefuses}</p>
      <ul className={STACK}>
        {names.map((name) => (
          <li key={name} className="flex flex-wrap items-center justify-between gap-2 px-3 py-2.5">
            <span className="min-w-0 font-mono text-md break-all">{name}</span>
            <EntryButton word={S.allow} busy="Allowing…" label={`Allow ${name} in production`} act={() => allow(name)} />
          </li>
        ))}
      </ul>
    </div>
  );
}

const PHONE = "(max-width: 479px)";
function Destinations({ rows, last = false, allowFor, allow, empty }: {
  rows: EgressDestination[]; last?: boolean; allowFor: (row: EgressDestination) => boolean; allow: (name: string) => Promise<void>; empty?: string;
}) {
  const phone = useMedia(PHONE);
  const button = (row: EgressDestination) => allowFor(row) && <EntryButton word={S.allow} busy="Allowing…" label={`Allow ${row.destination}`} act={() => allow(row.destination)} />;
  if (phone) {
    // A phone gets no table to scroll: each destination a small stack, nothing clipped — the destination, then
    // when it was last seen and its calls (and its last result, on the card), Allow at that line's end.
    return (
      <ul className="mt-2.5 divide-y divide-border rounded-xl border border-border bg-surface">
        {rows.length === 0 ? <li className="px-3 py-4 text-text-faint">{empty ?? "No rows"}</li> : rows.map((row) => (
          <li key={`${row.destination}:${row.port}`} className="grid gap-1 px-3 py-2.5">
            <span className="font-mono text-md break-all">{`${row.destination}:${row.port}`}</span>
            <span className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-sm text-text-dim">
                <Age at={row.last_seen_at} long />
                {" · "}<span className="tnum">{`${count(row.calls)} ${row.calls === 1 ? "call" : "calls"}`}</span>
                {last && ` · ${result(row)}`}
              </span>
              {button(row)}
            </span>
          </li>
        ))}
      </ul>
    );
  }
  return (
    <TableWrap minWidth={520}>
      <Table columns={[
        { key: "destination", label: "Destination" }, { key: "seen", label: "Last seen", className: "w-[170px]" }, { key: "calls", label: "Calls", className: "w-[110px] text-right!" },
        ...(last ? [{ key: "result", label: "Last result", className: "w-[200px]" }] : []), { key: "allow", label: "", className: "w-[120px]" },
      ]}
        rows={rows} empty={empty}
        renderRow={(row) => (
          <tr key={`${row.destination}:${row.port}`}>
            <td className={`${CELL} font-mono text-md`}>{`${row.destination}:${row.port}`}</td>
            <td className={CELL}><Age at={row.last_seen_at} long /></td>
            <td className={`${CELL} tnum text-right`}>{count(row.calls)}</td>
            {last && <td className={CELL}>{result(row)}</td>}
            <td className={CELL}>{button(row)}</td>
          </tr>
        )} />
    </TableWrap>
  );
}

// The destinations a switch to allowlist would refuse, as the card lists destinations, each with its Allow.
export function Refusals({ rows, allow }: { rows: EgressDestination[]; allow: (name: string) => Promise<void> }) {
  return <Destinations rows={rows} allowFor={() => true} allow={allow} />;
}

// What production can't reach that waits on a person: its pending requests the server says it refuses now, in the read's
// order (`refusing`, the fleet's refused tile's predicate; #2723). A read out of shape says none, as the card says it
// couldn't be read.
export const prodPending = (egress: ProjectEgress) => {
  const requests: unknown = egress.envs?.prod?.requests;
  return Array.isArray(requests) ? (requests as EgressRequest[]).filter((one) => one.state === "pending" && one.refusing === true).map((one) => one.destination) : [];
};

// `Allowed <destination>`: the row leaves its list once the entry is saved, so it is said, politely, never gone unseen.
export const AllowedLine = ({ line }: { line: string }) => <p aria-live="polite" className="text-md text-ok">{line}</p>;
export const allowedLine = (name: string) => S.allowed.replace("<destination>", () => name);

const REREAD_MS = 30_000; // a recreation changes nothing the status read says: the card reads again on its own

// Who changes an env's mode (design/PERMISSIONS.md, Outbound): an owner stricter than the org's or back to
// it, an admin either way; a contributor reads it.
type ModeLevel = "admin" | "owner" | "contributor" | "viewer" | "responder"; // the last two never reach the card today

// `shown` is the page opened at the card (#/apps/<id>/outbound, the link a refusal carries): it scrolls into view
// once, when first drawn. `told` hears each read, so the page's headline says what production can't reach (#2723).
// `owner` is whom a contributor asks to change an env's mode: its owner by name, else the group that owns it.
export function Outbound({ id, envs, level = "contributor", shown = false, told, owner }: {
  id: string; envs: Status["envs"]; level?: ModeLevel; shown?: boolean; told?: (egress: ProjectEgress) => void; owner?: { name: string } | { group: string };
}) {
  const licence = useLicence(); // an env's credentials are changed only while it unlocks them (#2879)
  const ask = owner === undefined ? null : "name" in owner ? S.modeOwnerAsk.replace("<owner>", () => owner.name) : S.modeOwnerAskGroup.replace("<group>", () => owner.group);
  const tell = useRef(told);
  tell.current = told;
  const scrolled = useRef(false);
  const intoView = (card: HTMLElement | null) => {
    if (card === null || !shown || scrolled.current) return;
    scrolled.current = true;
    card.scrollIntoView?.({ block: "start" });
  };
  const [read, setRead] = useState<{ egress: ProjectEgress; via: string; mode: unknown; credentials: boolean | undefined } | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [said, setSaid] = useState<Partial<Record<EnvKey, string>>>({}); // each env's Allowed or Removed <destination>
  const latest = useRef(0); // reads overlap: only the newest one's answer is drawn
  const [runtime, setRuntime] = useState<string | null>(null);
  const runtimeRead = useRef<string | null>(null);
  const loader = useRef<() => Promise<void>>(async () => {});
  const deployed = ENVS.map(([key]) => { const env = envs[key]; return env === undefined || waiting(env) ? "" : `${env.package}@${env.deployed_at}`; }).join(" ");
  useEffect(() => {
    let live = true;
    const load = () => {
      if (runtimeRead.current === null) {
        getFrontDoor().then((door) => { if (live) { runtimeRead.current = door.runtime; setRuntime(door.runtime); } }, () => {});
      }
      const asked = ++latest.current;
      return Promise.all([getProjectEgress(id), getEgress()]).then(
        ([egress, host]) => { if (live && asked === latest.current) { setRead({ egress, via: host.via, mode: host.mode, credentials: host.credential_routes }); setFailure(null); tell.current?.(egress); } },
        (err: unknown) => {
          if (!live || asked !== latest.current) return;
          if (err instanceof ApiError && err.status === 403 && (err.body as { error?: string } | null)?.error === "team_required") setRead(null);
          else setFailure("Outbound couldn't be read.");
        },
      );
    };
    loader.current = load;
    void load();
    const every = setInterval(() => void load(), REREAD_MS);
    return () => { live = false; clearInterval(every); };
  }, [id, deployed]);
  const record = (value: unknown): value is object => typeof value === "object" && value !== null;
  // Fail closed: layer 2's fields are required (#2110); absent or unknown is not read, never observe.
  const unread = read !== null && (!record(read.egress?.envs) || !Array.isArray(read.egress.gaps) || !read.egress.gaps.every(record)
    || !Array.isArray(read.egress.platform) || !MODES.has(read.mode)
    || ENVS.some(([key]) => {
      const env: unknown = read.egress.envs[key];
      return envs[key] !== undefined && !(record(env) && "destinations" in env && Array.isArray(env.destinations)
        && env.destinations.every((row: unknown) => record(row) && "last_response_flags" in row && Array.isArray(row.last_response_flags))
        && "mode" in env && MODES.has(env.mode) && "app_mode" in env && (env.app_mode === null || MODES.has(env.app_mode)) && "allowed" in env && Array.isArray(env.allowed)
        && "requests" in env && Array.isArray(env.requests));
    }));
  if (failure !== null || unread) return <p className="text-base text-critical">{failure ?? "Outbound couldn't be read."}</p>;
  if (read === null) return null;
  // Nothing of the app goes through Canyon: one line in place of the card, where the /outbound link a refusal carries lands (#2561).
  // floor null under Off is an open network or one Canyon can't tell (a recreation under way or failed), so the line says the
  // setting, never that the app calls out directly (#2774).
  if (read.via !== "canyon" && read.egress.floor === null) {
    return (
      <section ref={intoView} aria-label="Outbound" className="grid grid-cols-[minmax(0,1fr)] gap-2.5">
        <h2 className="flex flex-wrap items-center gap-x-2 text-base font-semibold">Outbound<Chip tone="beta">{S.beta}</Chip></h2>
        <p className="text-md text-text-dim">{linked(S.outboundOff, level === "admin")}</p>
      </section>
    );
  }
  const { egress } = read;
  const orgMode = read.mode as EgressMode;
  const org = (key: EnvKey) => { const env = envs[key]; return env !== undefined && (waiting(env) || env.run_by === "org"); };
  const open = egress.floor !== "enforced" && ENVS.some(([key]) => envs[key] !== undefined && !org(key));
  const perEnv = runtime === "kubernetes" && egress.floor === "not_enforced";
  // Off, on a network that still sends the app's calls through Canyon: Settings lists it to recreate, and so does this card,
  // never a deploy, which doesn't change the network (#2774). Only on a runtime read as Docker: Kubernetes has no network to
  // recreate, and a runtime unread names no remedy (fail closed).
  const off = read.via !== "canyon";
  const stuck = off && egress.floor === "enforced" && runtime === "docker";
  const admin = level === "admin";
  const reload = () => loader.current();
  const allow = (key: EnvKey) => async (name: string) => {
    const pending = await allowDestination(id, key, name);
    setSaid((was) => ({ ...was, [key]: pendingLine(pending) ?? allowedLine(name) }));
    await reload();
  };
  const dismiss = (key: EnvKey) => async (name: string) => {
    await dismissEgressRequest(id, key, name);
    setSaid((was) => ({ ...was, [key]: S.dismissedOne.replace("<destination>", () => name) }));
    await reload();
  };
  const remove = (key: EnvKey, name: string) => async () => {
    const pending = await disallowDestination(id, key, name);
    setSaid((was) => ({ ...was, [key]: pendingLine(pending) ?? S.removed.replace("<destination>", () => name) }));
    await reload();
  };
  const [listed, afterList] = S.allowlist.split("<entries>");
  return (
    <section ref={intoView} aria-label="Outbound" className="grid grid-cols-[minmax(0,1fr)] gap-2.5">
      <h2 className="flex flex-wrap items-center gap-x-2 text-base font-semibold">Outbound<Chip tone="beta">{S.beta}</Chip><span className="text-md font-normal text-text-faint">Destinations called in the last 30 days{off && ` · ${S.outboundOffNote}`}</span></h2>
      {/* The card's own line, the app its subject; an env's label keeps the fragment canyon status says (#2723). */}
      {open && !perEnv && <p role="note" className="text-md text-amber">{runtime === null ? S.floorCardUnread : runtime === "kubernetes" ? S.floorCardKube : linked(admin ? S.floorCardAdmin : S.floorCard, admin)}</p>}
      {stuck && <p role="note" className="text-md text-amber">{linked(admin ? S.offStillThrough : S.offStillThroughAsk, admin)}</p>}
      {ENVS.map(([key, name]) => {
        if (envs[key] === undefined) { // the first promotion answers the same line: its env has nothing else to say yet
          return key === "prod" && prodRefuses(egress).length > 0 && (
            <section key={key} aria-label={`${name} outbound`} className="grid grid-cols-[minmax(0,1fr)] gap-1.5">
              <h3 className="text-md font-medium">{name}</h3>
              <AllowedLine line={said[key] ?? ""} />
              <ProdRefuses names={prodRefuses(egress)} allow={allow(key)} />
            </section>
          );
        }
        const env = egress.envs[key];
        const past = egress.gaps.filter((gap) => gap.reason === "over_cap" && gap.env === key).reduce((sum, gap) => sum + (gap.calls ?? 0), 0);
        // The mode decides only calls that go through Canyon: an env whose network or version doesn't has no control.
        const through = !org(key) && egress.floor === "enforced" && env.via === "canyon";
        const refusing = env.mode === "allowlist";
        const changes = level === "owner" || level === "admin"; // they alone change an env's mode (design/PERMISSIONS.md, Outbound): fails closed
        // What needs a person is what Canyon refuses now (`refusing`, the refused tile's predicate); another pending request
        // is no need, its destination's row in the table below.
        const pending = env.requests.filter((one) => one.state === "pending" && one.refusing === true);
        const dismissed = env.requests.filter((one) => one.state === "dismissed");
        return (
          <section key={key} aria-label={`${name} outbound`} className="grid grid-cols-[minmax(0,1fr)] gap-1.5">
            <h3 className="text-md font-medium">
              {name}
              {org(key) ? <span className="ml-2 font-normal text-text-faint">{OUTBOUND.platform}</span>
                : perEnv && env.via !== "canyon" ? <span className="ml-2 font-normal text-amber">{OUTBOUND.floorKube}</span>
                  : stuck ? null
                  : !open && !off && env.via !== "canyon" ? <span className="ml-2 font-normal text-text-faint">{OUTBOUND.deployAgain}</span>
                    : <span className="ml-2 font-normal text-text-dim">{(env.app_mode === null ? S.modeOrgs : S.modeOwn).replace("<mode>", () => modeWord(env.mode))}</span>}
            </h3>
            {/* What waits on a person opens the env (D15), before its allowlist and its calls. */}
            {!org(key) && pending.length > 0 && <Requests heading={S.needsAPerson} rows={pending} allow={allow(key)} dismiss={dismiss(key)} />}
            {/* each entry with its Remove, a narrowing: a contributor's and up, as the card is (design/PERMISSIONS.md, Outbound) */}
            {!org(key) && env.allowed.length > 0 && (
              <p className="text-md text-text-dim">
                {listed}
                {env.allowed.map((entry, at) => (
                  <span key={entry}>{at > 0 && ", "}{entry} <EntryButton word={S.remove} busy="Removing…" label={`Remove ${entry}`} act={remove(key, entry)} /></span>
                ))}
                {afterList}
              </p>
            )}
            {/* An owner's own allowlist, under the org's observe: what switching back would let through, to everyone; who
                changes it is said once, in the mode line, to whoever can't (changes, the controls' own check). */}
            {through && env.app_mode === "allowlist" && orgMode === "observe" && <p className="text-md text-text-dim">{S.modeSwitchBack}</p>}
            {through && (!changes
              ? <p className="text-md text-text-faint">{ask === null ? S.modeOwner : `${S.modeOwner} ${ask}`}</p>
              : <EnvMode id={id} env={key} name={name} read={env} orgMode={orgMode} platform={egress.platform} admin={level === "admin"} saved={reload} allow={allow(key)} />)}
            {!org(key) && <AllowedLine line={said[key] ?? ""} />}
            {!org(key) && dismissed.length > 0 && <Requests heading={S.dismissedHeading} rows={dismissed} allow={allow(key)} dismiss={dismiss(key)} />}
            {key === "prod" && !org(key) && prodRefuses(egress).length > 0 && <ProdRefuses names={prodRefuses(egress)} allow={allow(key)} />}
            {!org(key) && (
              <Destinations rows={env.destinations} last empty="No calls out recorded." allow={allow(key)}
                allowFor={(row) => refusing && allowable(row, env, egress.platform) && !env.requests.some((one) => one.destination === row.destination && (one.refusing === true || one.state === "dismissed"))} />
            )}
            {/* What fixes a row no Allow can: prod.egress-allowed's own how-to-fix (gate/policy/canonical.go; #2723). */}
            {!org(key) && env.destinations.some((row) => forGood(row) !== null) && <p className="text-md text-text-dim">{S.egressHowToFix}</p>}
            {past > 0 && <p className="text-md text-text-dim">{`${past} ${past === 1 ? "call" : "calls"} to destinations past 500, not listed`}</p>}
            {!org(key) && <Routes id={id} env={key} name={name} read={env} level={level} licence={licence} credentials={read.credentials} saved={reload} />}
          </section>
        );
      })}
    </section>
  );
}

const NOTE = "text-md text-text-dim";
const CARD = "grid grid-cols-[minmax(0,1fr)] gap-2 rounded-xl border border-border bg-surface-2 px-3.5 py-3";
type Choice = "org" | EgressMode;

// An env's own mode (contracts/routes.md, PUT /api/v1/projects/:id/egress/:env/mode): the org's, null, or
// allowlist; an admin's observe too while the org's is allowlist, a widening. An env already on its own observe
// shows it checked to anyone, an owner unable to choose it again, so the group never has nothing checked. A
// choice that would start refusing calls lists what it would refuse, each with Allow, and asks in the page,
// the question taking the focus Save had, before anything is sent.
function EnvMode({ id, env, name, read, orgMode, platform, admin, saved, allow }: {
  id: string; env: EnvKey; name: string; read: EgressEnv; orgMode: EgressMode; platform: string[]; admin: boolean; saved: () => Promise<void>; allow: (name: string) => Promise<void>;
}) {
  const current: Choice = read.app_mode ?? "org";
  const [choice, setChoice] = useState<Choice>(current);
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<string | null>(null);
  const [pending, setPending] = useState<string | null>(null); // a gateway replica that hadn't taken the saved mode (#2789)
  const live = useRef(true);
  const yes = useRef<HTMLButtonElement>(null);
  useEffect(() => { live.current = true; return () => { live.current = false; }; }, []);
  useEffect(() => { setChoice(current); setAsking(false); }, [current]);
  useEffect(() => { if (asking) yes.current?.focus(); }, [asking]);
  const choices: [Choice, string][] = [["org", S.modeFollow.replace("<mode>", () => modeWord(orgMode))], ["allowlist", S.modeAllowlist]];
  if (current === "observe" || (admin && orgMode === "allowlist")) choices.push(["observe", S.modeObserve]);
  const after = choice === "org" ? orgMode : choice;
  const narrows = after === "allowlist" && read.mode !== "allowlist";
  const refused = narrows ? wouldRefuse(read, platform) : [];
  const save = async () => {
    if (busy) return;
    setBusy(true);
    setRefusal(null);
    setPending(null);
    try {
      const left = await putEgressMode(id, env, choice === "org" ? null : choice);
      if (live.current) setPending(pendingLine(left));
    } catch (err) {
      if (live.current) { setRefusal(said(err)); setChoice(current); }
    }
    await saved();
    if (live.current) { setBusy(false); setAsking(false); }
  };
  const ask = refused.length === 0 ? S.envModeAskNone : refused.length === 1 ? S.envModeAskOne : S.envModeAsk.replace("<n>", () => String(refused.length));
  return (
    <div className="grid grid-cols-[minmax(0,1fr)] gap-2">
      <fieldset disabled={busy} className="flex flex-wrap items-center gap-x-4 gap-y-1">
        <legend className="sr-only">{`${name}'s mode`}</legend>
        {choices.map(([value, word]) => (
          <label key={value} className="flex items-center gap-1.5 text-md has-disabled:text-text-faint">
            <input type="radio" name={`egress-mode-${env}`} checked={choice === value} disabled={value === "observe" && !admin}
              onChange={() => { setChoice(value); setAsking(false); setRefusal(null); }} />
            {word}
          </label>
        ))}
      </fieldset>
      {refusal !== null && <p role="alert" className="text-md text-critical">{refusal}</p>}
      {pending !== null && <p role="status" className="text-md text-amber">{pending}</p>}
      {choice !== current && (
        <div className={CARD}>
          {narrows && <p className={NOTE}>{S.modeNow}</p>}
          {narrows && refused.length > 0 && <Refusals rows={refused} allow={allow} />}
          {asking
            ? (
              <>
                <p className="text-base">{ask.replace("<env>", () => name)}</p>
                <span className="flex flex-wrap gap-2">
                  {busy ? <Button disabled reason="Saving…">{S.modeYes}</Button> : <Button variant="danger" ref={yes} onClick={() => void save()}>{S.modeYes}</Button>}
                  <Button variant="ghost" onClick={() => setAsking(false)}>Cancel</Button>
                </span>
              </>
            )
            : <span className="flex">{busy ? <Button disabled reason="Saving…" label={`Save ${name}'s mode`}>Save</Button>
              : <Button variant="primary" label={`Save ${name}'s mode`} onClick={() => (narrows ? setAsking(true) : void save())}>Save</Button>}</span>}
        </div>
      )}
    </div>
  );
}
