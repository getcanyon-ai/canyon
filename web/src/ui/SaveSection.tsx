import { useEffect, useState } from "react";
import { said } from "../api";
import { Button } from "./Button";
import { Saved } from "./Saved";
import { useUnsaved } from "./Unsaved";

// The Save under one section of typed values (design/EXPERIENCE.md, D16): named for what it saves, shown only while the
// section differs from what is saved, and "Saved." once the server has it, until the next change. While it differs,
// leaving the page asks first. A refusal keeps what was typed, since it is what the person fixes, and is the server's own
// sentence. `save` sends the section's change and takes the answer in; `then` is what more a save says, and `pending`
// the line in place of "Saved." while some part of Canyon hasn't taken it yet.
export function SaveSection({ label, changed, save, then, pending = null }: {
  label: string; changed: boolean; save: () => Promise<unknown>; then?: string; pending?: string | null;
}) {
  const [refusal, setRefusal] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [sending, setSending] = useState(false);
  useUnsaved(changed);
  useEffect(() => { if (changed) setSaved(false); }, [changed]);
  const send = async () => {
    if (sending) return;
    setSending(true);
    try {
      await save();
      setRefusal(null);
      setSaved(true);
    } catch (err) {
      setSaved(false);
      setRefusal(said(err));
    } finally {
      setSending(false);
    }
  };
  return (
    <>
      {changed && (
        <span className="mt-3 flex">
          {sending ? <Button disabled reason="Saving…">{label}</Button> : <Button variant="primary" onClick={() => void send()}>{label}</Button>}
        </span>
      )}
      {refusal !== null && <p role="alert" className="mt-2 text-md text-critical">{refusal}</p>}
      {saved && !changed && <Saved then={then} pending={pending} />}
    </>
  );
}
