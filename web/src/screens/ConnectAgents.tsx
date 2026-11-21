import { useEffect, useState } from "react";
import { getAgentAddress, said } from "../api";
import { CONNECT as T, CONNECTOR_DOCS, S } from "../strings";
import { Button } from "../ui/Button";

const H2 = "mt-5 text-2xl font-semibold";
const H3 = "mt-4 text-lg font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const CODE = "mt-1 block overflow-x-auto rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 font-mono text-sm whitespace-pre";

// The CLI's install line, as the README and the docs give it.
const INSTALL = "curl -fsSL https://github.com/getcanyon-ai/canyon-releases/releases/latest/download/install.sh -o install.sh && sh install.sh";

// Connect an agent, on Settings › People & sign-in above the sessions list, every member's: on a laptop the CLI, for
// this host; in the cloud Canyon's address, where the host serves one, and each cloud agent's way to it. The address is
// never offered for a laptop, by a command, an install link or managed settings, all of which reach laptops: a laptop's
// agents use the server canyon skill install sets up (design/specs/mcp.md §2.4, §12).
export function ConnectAgents({ signIn }: { signIn: boolean }) {
  const [address, setAddress] = useState<string | null | undefined>(undefined); // undefined while it is read
  const [failure, setFailure] = useState<string | null>(null);
  const [copied, setCopied] = useState<boolean | null>(null);
  useEffect(() => {
    if (!signIn) return;
    let live = true;
    getAgentAddress().then((answer) => { if (live) setAddress(answer); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, [signIn]);
  const served = signIn && typeof address === "string" ? address : null;
  // navigator.clipboard is absent outside a secure context: that is a failure too, said in place.
  const copy = (text: string) => void Promise.resolve().then(() => navigator.clipboard.writeText(text)).then(() => setCopied(true), () => setCopied(false));
  const steps: [string, string][] = [[T.stepInstall, INSTALL], [T.stepLogin, `canyon login ${location.origin}`], [T.stepSkill, "canyon skill install"]];

  return (
    <section aria-label={T.title} className="mt-8">
      <h2 className={H2}>{T.title}</h2>
      <p className={SUB}>{T.about}</p>
      <section aria-label={T.laptop}>
        <h3 className={H3}>{T.laptop}</h3>
        <ol className="mt-1 list-decimal space-y-2 pl-5 text-md">
          {steps.map(([step, line]) => <li key={step}>{step}<code className={CODE}>{line}</code></li>)}
        </ol>
        <p className={SUB}>{T.laptopNote}</p>
      </section>
      <h3 className={H3}>{T.cloud}</h3>
      <p className={SUB}>{T.cloudNote}</p>
      {!signIn && <p className={SUB}>{T.signInOff}</p>}
      {failure !== null && <p className="mt-2 text-md text-critical">{failure}</p>}
      {signIn && address === null && <p className={SUB}>{T.noAddress}</p>}
      {served !== null && (
        <div className="mt-2">
          <label htmlFor="agent-address" className="mb-1 block text-md font-semibold text-text-dim">{T.address}</label>
          <span className="flex flex-wrap items-center gap-2">
            <input id="agent-address" readOnly value={served} spellCheck={false} autoComplete="off" className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-sm" />
            <Button variant="secondary" label={`${T.copy} ${T.address.toLowerCase()}`} onClick={() => copy(served)}>{T.copy}</Button>
          </span>
          <p className={SUB}>{T.addressNote}</p>
          <p role="status" className="text-md text-ok">{copied === true ? T.copied : ""}</p>
          {copied === false && <p role="alert" className="text-md text-critical">{S.copyFailed}</p>}
          <dl className="mt-1 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-4 gap-y-2 text-md">
            <dt className="font-semibold">{T.claudeWeb}</dt>
            <dd>{T.claudeWebHow} <a href={CONNECTOR_DOCS} target="_blank" rel="noreferrer">{T.claudeDocs}</a></dd>
            <dt className="font-semibold">{T.cursorCloud}</dt><dd>{T.cursorCloudHow}</dd>
            <dt className="font-semibold">Codex</dt><dd>{T.codexCloud}</dd>
          </dl>
        </div>
      )}
    </section>
  );
}
