import { useEffect, useRef, useState } from "react";
import { askForAccess, getProject, said, type App, type Reduced } from "../api";
import { S } from "../strings";
import { Button } from "../ui/Button";
import { Brand } from "../ui/Icon";

// #/no-access/<app>: where an app surface sends a signed-in person with no access to it
// (contracts/routes.md, App surfaces). The app's name, its owner and one button that asks; the
// project read answers every member those, and nothing else of the app is read or shown. A share
// reaches this read at once and the app's own sign-in about a minute later, so a person the read
// already counts in is told to open the app's link again, and asked nothing.
export function NoAccess({ id, auth }: { id: string; auth: "oidc" | "off" | null }) {
  const [project, setProject] = useState<App | Reduced | null>(null);
  const [asked, setAsked] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const answer = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    // With sign-in off everyone can open every app, so there is nothing to ask for: go to the app.
    if (auth === "off") { window.location.replace(`#/apps/${encodeURIComponent(id)}`); return; }
    let live = true;
    getProject(id).then(({ project: read }) => { if (live) setProject(read); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, [id, auth]);
  // The button gives way to the answer, which takes the keyboard with it.
  useEffect(() => { if (asked) answer.current?.focus(); }, [asked]);

  const ask = () => askForAccess(id).then(() => setAsked(true), (err: unknown) => setFailure(said(err)));
  // Whom to ask: the first person who owns it, else the first group that does, else an admin.
  const owner = project?.owners[0]?.display;
  const group = project?.owner_groups[0];
  const fill = (line: string) => line.replace("<owner>", () => owner ?? "").replace("<group>", () => group ?? "").replace("<app>", () => project?.name ?? "");
  return (
    <main className="flex min-h-screen flex-col items-center justify-center gap-4 px-4 text-center">
      <Brand />
      {project !== null && project.access.level !== "none" && (
        <p role="status" className="max-w-[440px] text-md">
          {S.accessNow.replace("<app>", () => project.name)}{" "}
          <a className="text-link no-underline hover:underline" href={`#/apps/${id}`}>{project.name}</a>
        </p>
      )}
      {project !== null && project.access.level === "none" && (
        <>
          <h1 className="text-title font-semibold">{S.noAccess.replace("<app>", () => project.name)}</h1>
          <p className="max-w-[440px] text-md text-text-dim">{fill(owner !== undefined ? S.noAccessOwner : group !== undefined ? S.noAccessGroup : S.noAccessNoOwner)}</p>
          {asked
            ? <p ref={answer} tabIndex={-1} role="status" className="text-base text-ok">{fill(owner !== undefined ? S.asked : group !== undefined ? S.askedGroup : S.askedNoOwner)}</p>
            : <Button variant="primary" onClick={() => void ask()}>{S.askForAccess}</Button>}
        </>
      )}
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
    </main>
  );
}
