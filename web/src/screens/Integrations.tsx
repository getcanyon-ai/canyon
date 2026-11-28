import { useEffect, useState } from "react";
import { useUnsaved } from "../ui/Unsaved";
import { getChannels, putChannel, removeChannel, said, testChannel, type Channel } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age, ago } from "../ui/Age";
import { Button, buttonClass } from "../ui/Button";

const LABEL = "mt-3 mb-1 block text-md font-semibold text-text-dim";
const FIELD = "w-full max-w-[640px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-1 max-w-[720px] text-md text-text-dim";
const MAX = 5; // the server's own cap: a sixth is its 409
const KINDS = [["slack", "Slack"], ["teams", "Teams"]] as const;

// idFor is the id a new channel is saved under: its name in the id's alphabet, never one already taken.
export function idFor(name: string, taken: string[]): string {
  const base = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^[^a-z]+|-+$/g, "").slice(0, 36).replace(/-+$/, "") || "channel";
  let id = base;
  for (let n = 2; taken.includes(id); n++) id = `${base}-${n}`;
  return id;
}

// Settings › Integrations (design/INTEGRATIONS.md; #2725): the org's chat channels, an admin's alone. Each says where it
// posts by host and the URL's last four characters, never the URL; a save posts the summary first, and a failing one says so.
export function IntegrationsSettings() {
  const [channels, setChannels] = useState<Channel[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [status, setStatus] = useState<string | null>(null);
  const [editing, setEditing] = useState<Channel | "new" | null>(null);
  const load = () => getChannels().then((answer) => { setChannels(answer.channels); setFailure(null); }, (err: unknown) => setFailure(said(err)));
  useEffect(() => { void load(); }, []);
  if (channels === null) return failure === null ? null : <p className="text-base text-critical">{failure}</p>;
  const done = (line: string) => { setStatus(line); setEditing(null); void load(); };
  return (
    <section aria-label="Integrations" className="grid min-w-0 gap-3">
      <p className={NOTE}>{S.integrationsNote}</p>
      {failure !== null && <p role="alert" className="text-base text-critical">{failure}</p>}
      {status !== null && <p role="status" className="text-base text-text">{status}</p>}
      {channels.length === 0 ? <p className="text-base text-text-dim">{S.integrationsNone}</p> : (
        <ul className="max-w-[880px] rounded-[10px] border border-border bg-surface">
          {channels.map((one) => editing !== "new" && editing?.id === one.id
            ? <li key={one.id} className="border-b border-border px-4 py-3 last:border-b-0"><ChannelForm channel={one} taken={[]} done={done} cancel={() => setEditing(null)} /></li>
            : <ChannelRow key={one.id} channel={one} done={done} change={() => { setStatus(null); setEditing(one); }} />)}
        </ul>
      )}
      {editing === "new" ? <ChannelForm taken={channels.map((one) => one.id)} done={done} cancel={() => setEditing(null)} />
        : channels.length >= MAX ? <p className={NOTE}>{S.integrationsFull}</p>
        : <span><Button variant="secondary" onClick={() => { setStatus(null); setEditing("new"); }}>{S.integrationsAdd}</Button></span>}
    </section>
  );
}

function ChannelRow({ channel, done, change }: { channel: Channel; done: (line: string) => void; change: () => void }) {
  const [failure, setFailure] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const act = async (call: () => Promise<unknown>, line: string) => {
    if (sending) return;
    setSending(true);
    setFailure(null);
    try {
      await call();
      done(line.replace("<channel>", channel.name));
    } catch (err) {
      setFailure(said(err));
    }
    setSending(false);
  };
  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-x-4 gap-y-2 border-b border-border px-4 py-3 last:border-b-0 max-[620px]:grid-cols-1">
      <div className="grid min-w-0 gap-0.5 text-base text-text-dim">
        <p><b className="font-semibold text-text">{channel.name}</b> · {channel.kind === "slack" ? "Slack" : "Teams"}</p>
        <p className="text-md break-all">{channel.host}/…{channel.tail}</p>
        <p className="text-md">Set by <Actor actor={channel.set_by} /> <Age at={channel.set_at} long /></p>
        {channel.failing !== null && (
          <p className="text-md text-critical">{S.integrationsFailing.replace("<age>", ago(channel.failing.since)).replace("<status>", String(channel.failing.status))}</p>
        )}
        {failure !== null && <p role="alert" className="text-md text-critical">{failure}</p>}
      </div>
      <span className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant="secondary" label={`${S.integrationsTest}: ${channel.name}`} onClick={() => void act(() => testChannel(channel.id), S.integrationsTested)}>{S.integrationsTest}</Button>
        <Button size="sm" variant="ghost" label={`Change ${channel.name}`} onClick={change}>Change</Button>
        <Button size="sm" variant="ghost" label={`Remove ${channel.name}`} onClick={() => void act(() => removeChannel(channel.id), S.integrationsRemoved)}>Remove</Button>
      </span>
    </li>
  );
}

// ChannelForm adds a channel, or changes one: a change that leaves the URL empty keeps the one saved.
function ChannelForm({ channel, taken, done, cancel }: { channel?: Channel; taken: string[]; done: (line: string) => void; cancel: () => void }) {
  const [name, setName] = useState(channel?.name ?? "");
  const [kind, setKind] = useState<Channel["kind"]>(channel?.kind ?? "slack");
  const [url, setURL] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const id = channel?.id ?? idFor(name, taken);
  // Typed, so its Save shows only while the form differs from the channel saved, and leaving asks first (D16).
  const changed = url.trim() !== "" || name.trim() !== (channel?.name ?? "") || kind !== (channel?.kind ?? "slack");
  useUnsaved(changed);
  const save = async () => {
    if (saving) return;
    setSaving(true);
    setFailure(null);
    try {
      const answer = await putChannel(id, { name: name.trim(), kind, ...(url.trim() === "" ? {} : { url: url.trim() }) });
      done(S.integrationsSaved.replace("<channel>", answer.channel.name));
    } catch (err) {
      setFailure(said(err));
    }
    setSaving(false);
  };
  const field = channel?.id ?? "new";
  return (
    <form aria-label={channel === undefined ? S.integrationsAdd : `Change ${channel.name}`} className="grid min-w-0 max-w-[880px]" onSubmit={(event) => { event.preventDefault(); void save(); }}>
      <label className={LABEL} htmlFor={`channel-name-${field}`}>Name</label>
      <input id={`channel-name-${field}`} className={FIELD} required value={name} maxLength={80} placeholder="#platform-alerts" onChange={(event) => setName(event.target.value)} />
      <fieldset className="mt-3">
        <legend className="mb-1 text-md font-semibold text-text-dim">Chat app</legend>
        <span className="flex gap-4">
          {KINDS.map(([value, word]) => (
            <label key={value} className="flex items-center gap-2 text-base">
              <input type="radio" name={`channel-kind-${field}`} checked={kind === value} onChange={() => setKind(value)} />
              {word}
            </label>
          ))}
        </span>
      </fieldset>
      <label className={LABEL} htmlFor={`channel-url-${field}`}>Webhook URL</label>
      <input id={`channel-url-${field}`} className={FIELD} type="url" required={channel === undefined} autoComplete="off" spellCheck={false} value={url} aria-describedby={`channel-url-how-${field}`}
        placeholder={channel === undefined ? "https://" : `${channel.host}/…${channel.tail}`} onChange={(event) => setURL(event.target.value)} />
      <p id={`channel-url-how-${field}`} className={NOTE}>
        {kind === "slack" ? S.integrationsSlackHow : S.integrationsTeamsHow}{channel !== undefined && ` ${S.integrationsKeep}`}
      </p>
      <p className={`${NOTE} text-amber`}>{S.integrationsEveryone}</p>
      <p className={NOTE}>{S.integrationsSaveNote}</p>
      {failure !== null && <p role="alert" className="mt-2 text-base text-critical">{failure}</p>}
      <span className="mt-3 flex gap-2">
        {changed && <button type="submit" className={buttonClass("primary", "md")}>{S.integrationsSave}</button>}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
    </form>
  );
}
