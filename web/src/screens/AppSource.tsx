import { useEffect, useRef, useState } from "react";
import { getPreferences, handOver, said, setAutoDeploy, unlinkRepository, type RepoLink } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { isAdmin } from "./OrgSettings";

// Where the app's code lives (design/specs/screens.md, A2; #1917): one line under its name, from the
// project read's repository. Linking and unlinking are an admin's, so the actions are theirs alone.
export function AppSource({ id, link, autoDeploy, owner, changed }: { id: string; link: RepoLink | null; autoDeploy: boolean; owner: boolean; changed: () => void }) {
  const [admin, setAdmin] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [asking, setAsking] = useState(false); // the hand-over can't be undone: it asks first
  const confirming = useRef<HTMLButtonElement>(null);
  useEffect(() => { if (asking) confirming.current?.focus(); }, [asking]);
  const sending = useRef(false); // a second click while a call is out sends nothing
  useEffect(() => {
    let live = true;
    getPreferences().then((record) => { if (live) setAdmin(isAdmin(record)); }, () => undefined); // no actions without the answer: the line itself stands
    return () => { live = false; };
  }, []);
  // one call at a time, its failure said until the next one succeeds
  const run = (call: () => Promise<unknown>) => {
    if (sending.current) return Promise.resolve();
    sending.current = true;
    setBusy(true);
    return call().then(() => { setFailure(null); changed(); }, (err: unknown) => setFailure(said(err))).finally(() => { sending.current = false; setBusy(false); });
  };
  const unlink = () => run(() => unlinkRepository(id));
  const unlinkButton = admin && <> <Button size="sm" variant="ghost" onClick={() => void unlink()}>Unlink</Button></>;
  let line;
  if (link === null) {
    line = <>{S.sourceKept}{admin && <> · <a className="text-link no-underline hover:underline" href="#/settings/github">Link a repository</a></>}</>;
  } else if (link.state === "pending") {
    line = S.sourcePending;
  } else if (link.state === "linked") {
    line = (
      <>
        {link.keeps && <>{S.sourceKeptBy} </>}
        <a className="text-link no-underline hover:underline" href={`https://github.com/${link.full_name}`}>github.com/{link.full_name}</a>
        {link.folder !== "" && <> · <span className="font-mono">{link.folder}</span></>}
        {link.keeps && link.kept_commit && <> · commit <span className="font-mono">{link.kept_commit.slice(0, 7)}</span></>}
        {unlinkButton}
      </>
    );
  } else { // GitHub no longer feeds it: an admin unlinks it, to link it again or to another repository
    line = <>{({ deleted: S.sourceDeleted, archived: S.sourceArchived, removed: S.sourceRemoved } as const)[link.state].replace("<repo>", () => link.full_name)}{unlinkButton}</>;
  }
  const flip = (on: boolean) => run(() => setAutoDeploy(id, on));
  const hand = () => run(() => handOver(id)).then(() => setAsking(false));
  const kept = link !== null && (link.state === "pending" || link.keeps === true);
  return (
    <div className="grid gap-1">
      <p className="flex flex-wrap items-center gap-x-1.5 text-md text-text-dim">
        <span className="text-text-faint">Source</span> {line}
        {failure !== null && <span role="alert" className="text-critical">{failure}</span>}
      </p>
      {kept && link.kept_error && <p className="text-md text-text-dim">{link.kept_error}</p>}
      {/* the kept repository is its owners' to hand to developers (#1938) */}
      {kept && link.state === "linked" && (owner || admin) && (
        asking
          ? (
            <div className="flex flex-col items-start gap-1.5 text-md">
              <p>{S.handOverConfirm.replace("<repo>", () => link.full_name)}</p>
              <span className="flex flex-wrap gap-2">
                <Button ref={confirming} size="sm" variant="primary" onClick={() => void hand()}>{S.handOverYes}</Button>
                <Button size="sm" variant="ghost" onClick={() => setAsking(false)}>Cancel</Button>
              </span>
            </div>
          )
          : (
            <div className="flex flex-wrap items-center gap-x-2 text-md">
              <Button size="sm" onClick={() => setAsking(true)}>{S.handOver}</Button>
              <span className="text-text-dim">{S.handOverNote}</span>
            </div>
          )
      )}
      {/* the switch is a linked app's, and its owners' and admins' to flip (#1923); the rest of the team reads it */}
      {link !== null && link.state === "linked" && (owner || admin
        ? (
          <label className="flex items-start gap-2 text-md">
            <input type="checkbox" className="mt-1" checked={autoDeploy} disabled={busy} onChange={(event) => void flip(event.target.checked)} />
            <span><span className="font-medium">{S.autoDeploy}</span><span className="block text-text-dim">{S.autoDeployNote}</span></span>
          </label>
        )
        : autoDeploy && <p className="text-md text-text-dim">{S.autoDeployOn}</p>)}
    </div>
  );
}
