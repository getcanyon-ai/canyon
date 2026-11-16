import { useEffect, useRef, useState } from "react";
import { renameProject, said } from "../api";
import { RENAME as R } from "../strings";
import { Button } from "../ui/Button";

// Renaming the app from the header's ⋯, its owners' and admins' (design/specs/screens.md, Renaming it;
// #1909): only the display name changes. `done` re-reads the page, whose title and breadcrumb then
// follow; a refusal stays here in the server's words, with the field as it was typed.
export function RenameApp({ id, name, done, cancel }: { id: string; name: string; done: () => Promise<void>; cancel: () => void }) {
  const [typed, setTyped] = useState(name);
  const [failure, setFailure] = useState<string | null>(null);
  const sending = useRef(false);
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => { field.current?.focus(); field.current?.select(); }, []); // the menu item that opened it is gone: the keyboard comes here, so Escape reaches the ⋯

  const send = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      await renameProject(id, typed);
      await done();
    } catch (err) {
      setFailure(said(err));
    }
    sending.current = false;
  };

  return (
    <section aria-label="Rename this app" className="flex flex-col items-start gap-2">
      <label className="w-full text-md text-text-dim">
        {R.name}
        <input ref={field} className="mt-1 w-full rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base text-text" autoComplete="off" value={typed}
          onChange={(event) => setTyped(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter") void send(); }} />
      </label>
      <p className="text-md text-text-faint">{R.idStays.replace("<id>", () => id)}</p>
      <span className="flex flex-wrap gap-2">
        <Button variant="primary" onClick={() => void send()}>Save</Button>
        <Button variant="ghost" onClick={cancel}>Cancel</Button>
      </span>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </section>
  );
}
