import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { type ApiError, getMaskedLogs } from "../api";
import { S } from "../strings";
import { Button } from "./Button";
import { unread } from "./Headline";
import { Modal } from "./Modal";

// An environment's logs on its card (A of incidents and logs): its last 200 lines, masked by the server, read when opened
// and again on Refresh, never followed or polled. team: the reader is on the app's team, whose canyon logs reads them raw;
// a security responder's read is production's, masked, alone (design/PERMISSIONS.md).
export function Logs({ app, name, env, team }: { app: string; name: string; env: "preview" | "prod"; team: boolean }) {
  const [open, setOpen] = useState(false);
  const opener = useRef<HTMLButtonElement>(null);
  const was = useRef(false);
  // The keyboard comes back to the button when the sheet closes: a click doesn't focus a button in every browser.
  useEffect(() => { if (open) was.current = true; else if (was.current) { was.current = false; opener.current?.focus(); } }, [open]);
  const where = env === "prod" ? "production" : "preview";
  return (
    <>
      <span className="col-start-1 mt-1"><Button ref={opener} variant="ghost" size="sm" label={`Show ${where}'s logs`} onClick={() => setOpen(true)}>{S.showLogs}</Button></span>
      {open && <Sheet app={app} name={name} env={env} where={where} team={team} close={() => setOpen(false)} />}
    </>
  );
}

function Sheet({ app, name, env, where, team, close }: { app: string; name: string; env: "preview" | "prod"; where: string; team: boolean; close: () => void }) {
  const [read, setRead] = useState<{ lines: string } | { failed: string } | null>(null);
  const [asked, setAsked] = useState(0); // each Refresh
  const [copied, setCopied] = useState<boolean | null>(null);
  const box = useRef<HTMLPreElement>(null);
  useEffect(() => {
    let live = true;
    setRead(null);
    setCopied(null);
    getMaskedLogs(app, env, 200).then((lines) => { if (live) setRead({ lines: lines.trimEnd() }); },
      (err) => { if (live) setRead({ failed: unread((err as ApiError).status, env, team) }); });
    return () => { live = false; };
  }, [app, env, team, asked]);
  // The newest line is the last, in view as the lines are drawn, as the band's are.
  useLayoutEffect(() => { if (box.current !== null) box.current.scrollTop = box.current.scrollHeight; }, [read]);
  const lines = read !== null && "lines" in read ? read.lines : null;
  const copy = () => {
    if (lines === null) return;
    Promise.resolve().then(() => navigator.clipboard.writeText(lines)).then(() => setCopied(true), () => setCopied(false));
  };
  const title = S.logsOf.replace("<app>", () => name).replace("<env>", where);
  return (
    <Modal label={title} close={close} sheet>
      <div className="grid gap-1">
        <h2 className="text-lg font-semibold">{title}</h2>
        <p className="text-sm text-text-dim">{S.logsLines}</p>
      </div>
      {read === null ? <p role="status" className="text-md text-text-dim">{S.logsReading}</p>
        : "failed" in read ? <p className="text-md text-critical">{read.failed}</p>
        : <pre ref={box} className="min-h-0 overflow-auto rounded-[7px] border border-border bg-bg px-3 py-2 font-mono text-md break-words whitespace-pre-wrap max-[620px]:h-full sm:max-h-[60vh]">{lines === "" ? S.logsNone : lines}</pre>}
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="secondary" onClick={() => setAsked((n) => n + 1)}>Refresh</Button>
        {lines !== null && lines !== "" && <Button variant="secondary" onClick={copy}>Copy</Button>}
        <Button variant="ghost" onClick={close}>Close</Button>
        <span role="status" className={`text-md ${copied === false ? "text-critical" : "text-text-dim"}`}>{copied === null ? "" : copied ? S.logsCopied : S.logsUncopied}</span>
      </div>
    </Modal>
  );
}
