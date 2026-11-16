import { useRef, useState } from "react";
import { said, type Finding as FindingRead } from "../api";
import { actorName } from "./Actor";
import { Button } from "./Button";
import { Chip } from "./Chip";
import { shortDate } from "./Verdict";

// A producer in the words a person reads, the CLI's words (cli.md §3.1: plain names): the reviewer
// is the AI review, the custom-rules agent is the custom rules, Canyon's own scanners say so, and a
// tool is the tool — trivy finds more than dependencies, so it is never renamed for one of them.
const PLAIN: Record<string, string> = { "canyon-review": "AI review", "custom-rules": "custom rules", "canyon/scanners": "Canyon's scanners" };
export const plainName = (producer: string) => PLAIN[producer] ?? (producer.startsWith("sarif/") ? producer.slice("sarif/".length) : producer);

const FIELD = "rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const LABEL = "mb-1.5 text-xs tracking-caps text-text-faint uppercase";
const day = (from: number) => new Date(Date.now() + from * 86_400_000).toISOString().slice(0, 10);

export type ApproveBody = { reason: string; expires_at: string };
// One dependency's blocking findings, approved together (design/specs/screens.md, The approval).
export type Group = { name: string; version: string; tool: string; titles: string[] };

// The approval of one blocking finding, or of one dependency's together, under its card, or of a package only the built
// app holds under its go-live check (design/specs/screens.md, The approval): a reason, an expiry that starts 30 days out,
// and who will see it. id names its fields apart from every other form's: a finding's fingerprint, or the package's.
export function ApproveForm({ id: of, group, org, send, close }: { id: string; group?: Group; org: string; send: (body: ApproveBody) => Promise<void>; close: () => void }) {
  const [reason, setReason] = useState("");
  const [expires, setExpires] = useState(day(30));
  const [failure, setFailure] = useState<string | null>(null);
  const sending = useRef(false);
  const submit = async () => {
    if (sending.current) return;
    sending.current = true;
    try {
      await send({ reason: reason.trim(), expires_at: `${expires}T00:00:00Z` });
    } catch (err) {
      setFailure(said(err));
    }
    sending.current = false;
  };
  const id = `${group === undefined ? "" : "all-"}${of}`;
  const approve = group === undefined ? "Approve" : `Approve all ${group.titles.length}`;
  return (
    <div className="mt-3 flex w-full flex-col gap-1.5">
      {group !== undefined && (
        <>
          <p className="text-base text-text-dim">{`Approves these ${group.titles.length} blocking findings of ${group.name} ${group.version}, found by ${plainName(group.tool)}:`}</p>
          <ul className="mb-1 list-disc pl-5 text-md text-text-dim">{group.titles.map((title, at) => <li key={at}>{title}</li>)}</ul>
        </>
      )}
      <label className={LABEL} htmlFor={`reason-${id}`}>Reason</label>
      <textarea id={`reason-${id}`} className={FIELD} maxLength={1024} rows={3} value={reason} onChange={(event) => setReason(event.target.value)} />
      <label className={LABEL} htmlFor={`expires-${id}`}>Approval expires</label>
      <input id={`expires-${id}`} className={FIELD} type="date" min={day(1)} max={day(90)} value={expires} onChange={(event) => setExpires(event.target.value)} />
      <p className="text-md text-text-dim">{`After that date the ${group === undefined ? "problem comes" : "problems come"} back in the checklist.`}</p>
      <p className="text-md text-text-dim">{`Attributed to you · in the audit, visible to every ${org} admin`}</p>
      {failure !== null && <p className="text-base text-critical">{failure}</p>}
      <span className="mt-1 flex flex-wrap items-start gap-2">
        {reason.trim() === "" ? <Button disabled reason="Needs a reason">{approve}</Button>
          : expires === "" ? <Button disabled reason="Needs an expiry date">{approve}</Button>
          : <Button variant="primary" onClick={() => void submit()}>{approve}</Button>}
        <Button variant="ghost" onClick={close}>Cancel</Button>
      </span>
    </div>
  );
}

// imported is the tool and its own rule of a finding no Canyon policy covers, whose rule id is
// sarif.<tool>.<its rule> (contracts/finding.md, In: external SARIF); undefined for any other.
function imported(ruleId: string): { tool: string; rule: string } | undefined {
  const [space, tool, ...rule] = ruleId.split(".");
  return space === "sarif" && tool !== undefined && rule.length > 0 ? { tool, rule: rule.join(".") } : undefined;
}

// toolRule is a tool's finding's tool, its own rule and the policy its result is mapped to, <policy>.<tool>.<its
// rule>, with no policy where none covers it, sarif.<tool>.<its rule> (contracts/snapshot.md); undefined for Canyon's own.
function toolRule(finding: FindingRead): { tool: string; rule: string; policy?: string } | undefined {
  const from = imported(finding.ruleId);
  if (from !== undefined) return from;
  const tool = finding.tool?.startsWith("sarif/") ? finding.tool.slice("sarif/".length) : undefined;
  const at = tool === undefined ? -1 : finding.ruleId.indexOf(`.${tool}.`);
  return tool === undefined || at < 0 ? undefined : { tool, rule: finding.ruleId.slice(at + tool.length + 2), policy: finding.ruleId.slice(0, at) };
}
const rulesOf = (entry: FindingRead[]) => entry.map(toolRule).filter((one) => one !== undefined);

// named is an entry as --ai's head names it (cli.md §5.3): the Canyon rule its results are mapped to, or its
// title when no policy covers it, then every tool's rule that found it under found by, once each. Canyon's own
// finding is its rule.
function named(entry: FindingRead[]): string {
  const [first] = entry as [FindingRead];
  const from = toolRule(first);
  if (from === undefined) return first.ruleId;
  const credits = [...new Set(rulesOf(entry).map(({ tool, rule }) => `${tool}'s rule ${rule}`))];
  return `${from.policy ?? first.properties.canyon.title}\nfound by: ${credits.join(", ")}`;
}

// foundBy is an entry's found-by line when several of its tools' rules found it (cli.md §3.3): the tools in order,
// and how many of their rules agreed — `Found by opengrep (2 rules)`, `Found by eslint and opengrep (3 rules)`.
function foundBy(entry: FindingRead[]): string {
  const rules = rulesOf(entry);
  const tools = [...new Set(rules.map((one) => one.tool))];
  const agreed = new Set(rules.map((one) => `${one.tool}\u0000${one.rule}`)).size;
  return `Found by ${tools.length > 1 ? `${tools.slice(0, -1).join(", ")} and ${tools.at(-1)}` : tools[0]}${agreed > 1 ? ` (${agreed} rules)` : ""}`;
}

// The block a person pastes into their coding agent (design/specs/screens.md, "Copy for your
// agent"). Whether it blocks, the end state, the wrong moves and the steps after the fix are
// printed as `canyon check --ai` prints them (cli/ai.go), so an agent gets one instruction and
// never less from the browser than from the command line for the same finding.
export function forAgent(finding: FindingRead, app: string, also: FindingRead[] = []): string {
  const { blocks, problem, fix, required_end_state, do_not } = finding.properties.canyon;
  const severity = highest([finding, ...also]);
  const { artifactLocation, region } = finding.locations[0].physicalLocation;
  const parts = [
    `Fix this finding from Canyon's review of ${app}:`,
    `${severity.toUpperCase()} ${named([finding, ...also])}\nblocks: ${blocks ? "yes" : "no"}\n${artifactLocation.uri}:${region.startLine} — ${problem}`,
    fix,
  ];
  const end = blocks ? required_end_state : "Not required. This severity does not block deployment.";
  if (end !== undefined) parts.push(`required end state:\n  ${end.split("\n").join("\n  ")}`);
  if (blocks && do_not !== undefined) parts.push(`do not:\n${do_not.map((move) => `  - ${move}\n`).join("")}  Each of these produces the same finding on the next run.`);
  parts.push(AFTER);
  return `${parts.join("\n\n")}\n`;
}

export const AFTER = "Then run: canyon check\n" +
  "This runs the identical gate without deploying. It takes about 15 seconds.\n" +
  "If it prints RESULT: PASSED, run: canyon deploy\n" +
  "\n" +
  "If canyon check still prints RESULT: BLOCKED after two full rounds of fixes,\n" +
  "stop and show the user the remaining findings. Do not keep looping.";

// One finding as the gate wrote it: its severity, whether it stops production, where it is, and
// the two paragraphs. Whether it blocks is the finding's own field — read, never worked out here
// from the severity (D6: the display cannot fail open). Approved is the finding's own field too: it
// stays on screen with a name, a date and a reason, and is never the word for resolved (screens.md).
// approve is given for a blocking finding of the newest run a person may still approve: its card
// carries the way to approve this entry and no other. also is the rest of its entry, the same Canyon rule on
// the same line found by other tools' rules (contracts/finding.md, One problem, one entry): one card, approved
// once every finding of it is. A dependency's findings are one DependencyCard instead.
// atProduction: the finding blocks before production (the run read's blocks_production), so a finding that
// does not block preview still blocks the promotion, and its card says both.
export function Finding({ finding, also = [], app, approve, atProduction = false }: {
  finding: FindingRead; also?: FindingRead[]; app: string; approve?: { org: string; send: (body: ApproveBody) => Promise<void> }; atProduction?: boolean;
}) {
  const { copy, copied } = useCopy(() => forAgent(finding, app, also));
  const [approving, setApproving] = useState(false);
  const { blocks, title, problem, fix } = finding.properties.canyon;
  const severity = highest([finding, ...also]); // an entry's is the highest of its findings', as the CLI's (cli.md §3.3)
  const approval = also.every((other) => other.approval !== undefined) ? finding.approval : undefined;
  const { artifactLocation, region } = finding.locations[0].physicalLocation;
  const from = imported(finding.ruleId); // its snippet is the mask
  const [line = "", ...rest] = region.snippet.text.split("\n");
  // A masked or windowed snippet carries its own caret column (contracts/finding.md, canyon.shown).
  const start = finding.properties.canyon.shown?.startColumn ?? region.startColumn;
  const caret = Math.max(0, Math.min(start - 1, line.length));
  // A check that couldn't finish is no finding in the code (contracts/finding.md, gate.unreadable): it has
  // no severity to weigh and no line to point at, so it is a short row, its digest behind Details.
  const unread = finding.ruleId === "gate.unreadable";
  return (
    <article aria-label={title} className={`mt-2.5 rounded-xl border border-border bg-surface px-3.5 py-3 ${unread ? "border-l-[3px] border-l-critical" : ""}`}>
      <div className="flex flex-wrap items-baseline gap-2">
        {approval !== undefined
          ? <Chip tone="approved">{`approved by ${actorName(approval.approved_by)} until ${shortDate(approval.expires_at)}`}</Chip>
          : unread ? <Chip tone="blocks">couldn't finish</Chip>
          : blocks ? <Chip tone="blocks">blocks</Chip> : atProduction ? <Chip tone="blocks">advisory before preview · blocks before production</Chip> : <Chip tone="advisory">advisory · does not block</Chip>}
        {!unread && <Chip tone={severity}>{capital(severity)}</Chip>}
        <span className="text-lg font-semibold">{title}</span>
      </div>
      {approval !== undefined && <p className="mt-1.5 text-base text-text-dim">{approval.reason}</p>}
      {also.length > 0 ? <p className="mt-1.5 text-base text-text-dim">{foundBy([finding, ...also])}</p>
        : from !== undefined ? <p className="mt-1.5 text-base text-text-dim">{`Found by ${from.tool} · rule ${from.rule}`}</p>
        : finding.tool !== undefined && <p className="mt-1.5 text-base text-text-dim">{`Found by ${plainName(finding.tool)}`}</p>}
      {!unread && (
        <>
          <p className="mt-1.5 font-mono text-sm text-text-faint">{artifactLocation.uri}:{region.startLine}</p>
          <pre className="mt-1.5 overflow-x-auto rounded-lg bg-surface-2 px-3 py-2 font-mono text-sm">
            {[line, `${" ".repeat(caret)}^`, ...rest].join("\n")}
          </pre>
        </>
      )}
      <dl className="mt-2.5 grid grid-cols-[48px_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-base">
        <dt className="text-text-faint">Why</dt><dd className="text-text-dim">{problem}</dd>
        <dt className="text-text-faint">Fix</dt><dd className="text-text-dim">{fix}</dd>
      </dl>
      {unread && (
        <details className="mt-2">
          <summary className="cursor-pointer text-md text-text-faint">Details</summary>
          <p className="mt-1 font-mono text-sm break-all text-text-faint">{`${artifactLocation.uri} · ${region.snippet.text}`}</p>
        </details>
      )}
      <div className="mt-2.5 flex flex-wrap items-center gap-2.5">
        <Button size="sm" onClick={copy}>Copy for your agent</Button>
        {approve !== undefined && (blocks || atProduction) && approval === undefined && !approving && <Button variant="ghost" size="sm" onClick={() => setApproving(true)}>Approve with a reason</Button>}
        {copied}
      </div>
      {approve !== undefined && approving && approval === undefined && (
        <ApproveForm id={finding.partialFingerprints["canyon/v1"]} org={approve.org} send={async (body) => { await approve.send(body); setApproving(false); }} close={() => setApproving(false)} />
      )}
    </article>
  );
}

// useCopy copies what text gives to the clipboard on copy, and copied says how that went for two
// seconds. navigator.clipboard is absent outside a secure context, a plain-http host: that is a failure too.
export function useCopy(text: () => string) {
  const [state, setState] = useState<"yes" | "no" | null>(null);
  const copy = () => {
    Promise.resolve().then(() => navigator.clipboard.writeText(text())).then(
      () => { setState("yes"); setTimeout(() => setState(null), 2000); },
      () => setState("no"),
    );
  };
  const copied = state === "yes" ? <span role="status" className="text-md text-ok">Copied</span>
    : state === "no" ? <span role="alert" className="text-md text-critical">Couldn't copy. The browser did not allow it.</span> : null;
  return { copy, copied };
}

// where is a dependency finding's one line in place of its snippet, a lockfile entry hidden whole
// (design/specs/screens.md, The approval): its file and line, and the dependency.
export const where = (finding: FindingRead) => {
  const { artifactLocation, region } = finding.locations[0].physicalLocation;
  return `${artifactLocation.uri}:${region.startLine} · ${finding.dependency!.name} ${finding.dependency!.version}`;
};

// Advisory is a dependency finding's row in trivy's fields: its vulnerability, linked when the run read
// carries its https link, its severity and the versions that fix it — never trivy's message as a paragraph.
export function Advisory({ finding }: { finding: FindingRead }) {
  const { vulnerability, link, fixed } = finding.dependency!;
  const id = vulnerability ?? finding.ruleId;
  const { severity } = finding.properties.canyon;
  return (
    <>
      {link === undefined ? id : <a className="text-link no-underline hover:underline" href={link} target="_blank" rel="noopener noreferrer">{id}</a>}
      {` · ${capital(severity)} · ${fixed === undefined ? "no fixed version yet" : `fixed in ${fixed}`}`}
    </>
  );
}

export const capital = (word: string) => word[0]!.toUpperCase() + word.slice(1);
const SEVERITY = ["low", "medium", "high", "critical"];
// highest is the highest severity among an entry's findings: the one its card and its copy carry.
export const highest = (findings: FindingRead[]) => findings.map((one) => one.properties.canyon.severity).reduce((top, one) => (SEVERITY.indexOf(one) > SEVERITY.indexOf(top) ? one : top));
export const fileOf = (one: FindingRead) => one.locations[0].physicalLocation.artifactLocation.uri;

// The card's fix names the version that fixes them all, from the run read's fixed_in (cli.md §5.3's
// rule), and how many it fixes when some name no fix; with none, the first finding's own fix.
export function upgrade(findings: FindingRead[]): string {
  const [first] = findings as [FindingRead];
  const { name, fixed_in } = first.dependency!;
  if (fixed_in === undefined) return first.properties.canyon.fix;
  const n = findings.length;
  const k = findings.filter((one) => one.dependency?.fixed !== undefined).length;
  const fixes = n < 2 ? "" : k === n ? ` (fixes all ${n})` : ` (fixes ${k} of ${n}; ${n - k} with no fixed version yet)`;
  return `Upgrade ${name} to ${fixed_in}${fixes} and commit ${[...new Set(findings.map(fileOf))].join(", ")}.`;
}
