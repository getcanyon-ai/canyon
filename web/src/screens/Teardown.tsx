import { useEffect, useRef, useState } from "react";
import { ApiError, deleteProject, said, takeOffline, type Resource } from "../api";
import { TEARDOWN as T } from "../strings";
import { Button } from "../ui/Button";

const WORD = { preview: "preview", prod: "production" } as const;
// One the org's platform provided is the platform's to delete, and says so (#2432).
const named = (resources: Resource[]) => resources.map((one) => {
  const it = `${one.type} ${one.name} in ${WORD[one.env]}`;
  return one.state === "provided" ? T.provided.replace("<resource>", () => it) : it;
}).join(", ");
const FIELD = "w-full rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-base";

// Taking one environment offline, asked before it is sent: the header's ⋯ opens it. `done` re-reads
// the page, which then shows nothing running there; `cancel` closes it unsent. A run already switching
// the environment stops it once it lands (202 {stopping}): the panel closes all the same, and the
// page, polling, shows the environment leaving when it has.
export function TakeOffline({ id, env, done, cancel }: { id: string; env: "preview" | "prod"; done: () => Promise<void>; cancel: () => void }) {
  const [failure, setFailure] = useState<string | null>(null);
  const confirming = useRef<HTMLButtonElement>(null);
  const sending = useRef(false);
  useEffect(() => { confirming.current?.focus(); }, []);

  const send = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      await takeOffline(id, env);
      await done();
    } catch (err) {
      setFailure(said(err));
    }
    sending.current = false;
  };

  return (
    <div className="flex flex-col items-start gap-2">
      <p className="text-md text-text-dim">{T.offlineConfirm.replace("<app>", () => id).replace("<env>", WORD[env]).replace("<back>", env === "prod" ? "Promote" : "Deploy")}</p>
      <span className="flex flex-wrap gap-2">
        <Button ref={confirming} variant="danger" onClick={() => void send()}>{T.takeOffline.replace("<env>", WORD[env])}</Button>
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </div>
  );
}

// Deleting the app, from the header's ⋯ (the redesign: risky actions are not in the reading flow).
// Opening it sends the call without confirm, so the server lists what would go and changes nothing;
// the typed name enables the confirmed call, which a 202 or a 502 sends again. Cancel sends nothing.
export function DeleteApp({ id, cancel }: { id: string; cancel: () => void }) {
  const [resources, setResources] = useState<Resource[] | null>(null);
  const [typed, setTyped] = useState("");
  const [pending, setPending] = useState<Resource[] | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const sending = useRef(false);
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => { if (resources !== null) field.current?.focus(); }, [resources]);

  useEffect(() => {
    let live = true;
    deleteProject(id).then(() => {}, (err: unknown) => {
      if (!live) return;
      const listing = err instanceof ApiError && err.status === 409 ? (err.body as { resources?: Resource[] } | null)?.resources : undefined;
      if (listing) { setResources(listing); setFailure(null); } else setFailure(said(err));
    });
    return () => { live = false; };
  }, [id]);

  const send = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      const answer = await deleteProject(id, id);
      if (answer === undefined) window.location.hash = "#/fleet";
      else { setPending(answer.pending); setFailure(null); }
    } catch (err) {
      setPending(null);
      setFailure(said(err)); // a 502 names what could not be deleted and that deleting again resumes
    }
    sending.current = false;
  };

  const name = T.deleteButton.replace("<app>", () => id);
  return (
    <section aria-label="Delete this app" className="flex flex-col items-start gap-2">
      <h3 className="text-base font-semibold text-critical">{name}?</h3>
      {resources === null && failure === null && <p className="text-md text-text-faint">Checking what this deletes…</p>}
      {resources !== null && (
        <>
          <p className="text-md text-text-dim">{resources.length === 0 ? T.destroysNothing.replace("<app>", () => id) : T.destroys.replace("<app>", () => id).replace("<resources>", () => named(resources))}</p>
          <label className="w-full text-md text-text-dim">
            {T.typeName.replace("<app>", () => id)}
            <input ref={field} className={`${FIELD} mt-1`} autoComplete="off" value={typed} onChange={(event) => setTyped(event.target.value)} />
          </label>
        </>
      )}
      <span className="flex flex-wrap items-start gap-2">
        {resources !== null && (typed === id ? <Button variant="danger" onClick={() => void send()}>{name}</Button> : <Button disabled reason="Type the name above first.">{name}</Button>)}
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
      {pending !== null && (
        <p className="flex flex-wrap items-center gap-2 text-base text-amber">
          {T.stillDeleting.replace("<resources>", () => named(pending))}
          <Button variant="ghost" size="sm" onClick={() => void send()}>Try again</Button>
        </p>
      )}
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </section>
  );
}
