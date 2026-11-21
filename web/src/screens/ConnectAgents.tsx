import { useEffect, useState } from "react";
import { getAgentAddress, said } from "../api";
import { CONNECT as T, S } from "../strings";
import { Button } from "../ui/Button";

const H2 = "mt-5 text-2xl font-semibold";
const H3 = "mt-4 text-lg font-semibold";
const SUB = "mt-1 text-md text-text-dim";
const CODE = "mt-1 block overflow-x-auto rounded-lg border border-border bg-surface-2 px-2.5 py-1.5 font-mono text-sm whitespace-pre";

// Each agent's way to add Canyon's server: by its address where the host serves one, else canyon mcp, the CLI's own
// (design/specs/mcp.md §8, §12). The links carry their configuration as each editor's install link asks for it.
function ways(address: string | null) {
  const cursor = address === null ? { command: "canyon", args: ["mcp"] } : { url: address };
  const vscode = address === null ? { name: "canyon", command: "canyon", args: ["mcp"] } : { name: "canyon", type: "http", url: address };
  return {
    claude: address === null ? "claude mcp add --scope user canyon -- canyon mcp" : `claude mcp add --scope user --transport http canyon ${address}`,
    codex: address === null ? "codex mcp add canyon -- canyon mcp" : `codex mcp add canyon --url ${address}`,
    cursor: `cursor://anysphere.cursor-deeplink/mcp/install?name=canyon&config=${btoa(JSON.stringify(cursor))}`,
    vscode: `vscode:mcp/install?${encodeURIComponent(JSON.stringify(vscode))}`,
  };
}

// Each agent's org-managed settings with the address (design/specs/mcp.md §2.4): an admin's, to roll Canyon out to everyone.
function managed(address: string) {
  return [
    { agent: "Claude Code", where: T.managedClaude, text: JSON.stringify({ mcpServers: { canyon: { type: "http", url: address } } }) },
    { agent: "Codex", where: T.managedCodex, text: `[mcp_servers.canyon]\nurl = ${JSON.stringify(address)}` },
    { agent: "Cursor", where: T.managedCursor, text: JSON.stringify({ mcpServers: { canyon: { url: address } } }) },
  ];
}

// Connect an agent, on Settings › People & sign-in above the sessions list: every member's, the org-managed snippets
// an admin's alone. With sign-in off the host serves no address, and the panel says why and gives canyon mcp.
export function ConnectAgents({ signIn, admin }: { signIn: boolean; admin: boolean }) {
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
  const way = ways(served);
  // navigator.clipboard is absent outside a secure context: that is a failure too, said in place.
  const copy = (text: string) => void Promise.resolve().then(() => navigator.clipboard.writeText(text)).then(() => setCopied(true), () => setCopied(false));

  return (
    <section aria-label={T.title} className="mt-8">
      <h2 className={H2}>{T.title}</h2>
      <p className={SUB}>{T.about}</p>
      {!signIn && <p className={SUB}>{T.signInOff}</p>}
      {failure !== null && <p className="mt-2 text-md text-critical">{failure}</p>}
      {signIn && address === null && <p className={SUB}>{T.noAddress}</p>}
      {served !== null && (
        <div className="mt-3">
          <label htmlFor="agent-address" className="mb-1 block text-md font-semibold text-text-dim">{T.address}</label>
          <span className="flex flex-wrap items-center gap-2">
            <input id="agent-address" readOnly value={served} spellCheck={false} autoComplete="off" className="min-w-0 flex-1 rounded-lg border border-border bg-surface px-2.5 py-1.5 font-mono text-sm" />
            <Button variant="secondary" label={`${T.copy} ${T.address.toLowerCase()}`} onClick={() => copy(served)}>{T.copy}</Button>
          </span>
          <p className={SUB}>{T.addressNote}</p>
          <p role="status" className="text-md text-ok">{copied === true ? T.copied : ""}</p>
          {copied === false && <p role="alert" className="text-md text-critical">{S.copyFailed}</p>}
        </div>
      )}
      <h3 className={H3}>{T.cli}</h3>
      <code className={CODE}>canyon skill install</code>
      <p className={SUB}>{T.cliNote}</p>
      <h3 className={H3}>{T.each}</h3>
      <dl className="mt-1 grid grid-cols-[max-content_minmax(0,1fr)] items-center gap-x-4 gap-y-2 text-md">
        <dt>Claude Code</dt><dd><code className={CODE}>{way.claude}</code></dd>
        <dt>Codex</dt><dd><code className={CODE}>{way.codex}</code></dd>
        <dt>Cursor</dt><dd><a href={way.cursor}>{T.addCursor}</a></dd>
        <dt>VS Code</dt><dd><a href={way.vscode}>{T.addVSCode}</a></dd>
      </dl>
      {admin && served !== null && (
        <section aria-label={T.managed}>
          <h3 className={H3}>{T.managed}</h3>
          <p className={SUB}>{T.managedNote}</p>
          {managed(served).map((one) => (
            <div key={one.agent} className="mt-2">
              <p className="text-md"><span className="font-semibold">{one.agent}</span>: {one.where}</p>
              <code className={CODE}>{one.text}</code>
            </div>
          ))}
        </section>
      )}
    </section>
  );
}
