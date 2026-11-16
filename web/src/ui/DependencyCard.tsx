import { useState } from "react";
import type { Finding as FindingRead } from "../api";
import { actorName } from "./Actor";
import { Button } from "./Button";
import { Chip } from "./Chip";
import { AFTER, Advisory, ApproveForm, capital, fileOf, highest, plainName, upgrade, useCopy, where, type ApproveBody, type Group } from "./Finding";
import { shortDate } from "./Verdict";


// The card as `canyon check --ai` prints a package's CVEs (cli.md §5.2), for a person to paste into their
// agent. Its rule is the Canyon rule, the finding's before trivy's own id (sarif.trivy where no policy covers
// it), then found by trivy: only a sarif/trivy finding carries a dependency.
function cardForAgent(findings: FindingRead[], head: string, severity: string, blocks: boolean, app: string): string {
  const [first] = findings as [FindingRead];
  const { region } = first.locations[0].physicalLocation;
  const policy = first.ruleId.slice(0, first.ruleId.indexOf(".trivy."));
  const rule = policy === "sarif" ? "sarif.trivy" : policy;
  const ends = [...new Set(findings.flatMap((one) => (one.properties.canyon.required_end_state ?? "").split("\n")).filter(Boolean))];
  const moves = [...new Set(findings.flatMap((one) => one.properties.canyon.do_not ?? []))];
  const parts = [
    `Fix these findings from Canyon's review of ${app}:`,
    `title: ${head}\nseverity: ${severity}\nblocks: ${blocks ? "yes" : "no"}\nrule: ${rule}\nfound by: trivy\nfile: ${fileOf(first)}\nline: ${region.startLine}\nfindings: ${findings.length}, one per CVE:\n` +
      findings.map((one) => `  - ${one.dependency!.vulnerability ?? one.ruleId} (${one.properties.canyon.severity}): ${one.properties.canyon.title} · ${one.dependency!.fixed === undefined ? "no fixed version yet" : `fixed in ${one.dependency!.fixed}`}`).join("\n"),
  ];
  if (ends.length > 0) parts.push(`required end state:\n  ${ends.join("\n  ")}`);
  parts.push(`suggested fix:\n  ${upgrade(findings)}`);
  if (moves.length > 0) parts.push(`do not:\n${moves.map((move) => `  - ${move}\n`).join("")}  Each of these produces the same finding on the next run.`);
  parts.push(AFTER);
  return `${parts.join("\n\n")}\n`;
}

// One dependency's findings from one tool, one entry (contracts/finding.md, One problem, one entry), as one card
// (design/specs/screens.md, The approval):
// the highest severity and how many, one line in place of the hidden lockfile entry, a row per finding
// in trivy's fields, each still its own to approve, the version that fixes them all, and the approval of
// those nobody has approved together. approve is the single approval of a finding a person may approve;
// approveAll is given for two to the route's 200 of them. An advisory entry says so, and so does one whose rule
// blocks before production (atProduction), as a finding's card does.
export function DependencyCard({ findings, app, approve, approveAll, atProduction = false }: {
  findings: FindingRead[]; app: string; approve: (one: FindingRead) => { org: string; send: (body: ApproveBody) => Promise<void> } | undefined;
  approveAll?: { org: string; group: Group; send: (body: ApproveBody) => Promise<void> }; atProduction?: boolean;
}) {
  const [first] = findings as [FindingRead];
  const { name, version } = first.dependency!;
  const severity = highest(findings);
  const head = `${name} ${version}: ${findings.length} known ${findings.length === 1 ? "vulnerability" : "vulnerabilities"}`;
  const { blocks } = first.properties.canyon;
  const { copy, copied } = useCopy(() => cardForAgent(findings, head, severity, blocks, app));
  // The open form: one finding's, by its fingerprint, or the group's; it closes once what it approves goes.
  const [approving, setApproving] = useState<string | null>(null);
  const one = findings.find((other) => other.partialFingerprints["canyon/v1"] === approving);
  const single = one === undefined ? undefined : approve(one);
  const action = approving === "all" ? approveAll : single;
  const open = findings.some((other) => other.approval === undefined);
  return (
    <article aria-label={head} className="mt-2.5 rounded-xl border border-border bg-surface px-3.5 py-3">
      <div className="flex flex-wrap items-baseline gap-2">
        {!blocks ? <Chip tone={atProduction ? "blocks" : "advisory"}>{atProduction ? "advisory before preview · blocks before production" : "advisory · does not block"}</Chip>
          : open ? <Chip tone="blocks">blocks</Chip> : <Chip tone="approved">approved</Chip>}
        <Chip tone={severity}>{capital(severity)}</Chip>
        <span className="text-lg font-semibold">{head}</span>
      </div>
      {first.tool !== undefined && <p className="mt-1.5 text-base text-text-dim">{`Found by ${plainName(first.tool)}`}</p>}
      {[...new Set(findings.map(fileOf))].map((file) => <p key={file} className="mt-1.5 font-mono text-sm text-text-faint">{where(findings.find((other) => fileOf(other) === file)!)}</p>)}
      <ul className="mt-2.5 grid gap-2">
        {findings.map((other) => (
          <li key={other.partialFingerprints["canyon/v1"]} className="grid gap-0.5">
            <span className="text-base">{other.properties.canyon.title}</span>
            <span className="flex flex-wrap items-baseline gap-x-2.5 text-md text-text-dim">
              <span><Advisory finding={other} /></span>
              {other.approval !== undefined
                ? <span className="text-amber">{`approved by ${actorName(other.approval.approved_by)} until ${shortDate(other.approval.expires_at)} · ${other.approval.reason}`}</span>
                : approve(other) !== undefined && action === undefined && <Button variant="ghost" size="sm" onClick={() => setApproving(other.partialFingerprints["canyon/v1"])}>Approve with a reason</Button>}
            </span>
          </li>
        ))}
      </ul>
      <dl className="mt-2.5 grid grid-cols-[48px_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-base">
        <dt className="text-text-faint">Fix</dt><dd className="text-text-dim">{upgrade(findings)}</dd>
      </dl>
      <div className="mt-2.5 flex flex-wrap items-center gap-2.5">
        <Button size="sm" onClick={copy}>Copy for your agent</Button>
        {approveAll !== undefined && action === undefined && (
          <Button variant="ghost" size="sm" onClick={() => setApproving("all")}>{`Approve all ${approveAll.group.titles.length} for ${name} ${version}`}</Button>
        )}
        {copied}
      </div>
      {action !== undefined && (
        <ApproveForm id={(one ?? first).partialFingerprints["canyon/v1"]} group={approving === "all" ? approveAll?.group : undefined} org={action.org}
          send={async (body) => { await action.send(body); setApproving(null); }} close={() => setApproving(null)} />
      )}
    </article>
  );
}
