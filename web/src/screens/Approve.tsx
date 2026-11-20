import { useEffect, useState } from "react";
import { approvePromotion, denyPromotion, getImage, getPromotionApproval, said, signOut, type ApiError, type ImageRead, type PatchFix, type PromotionApproval } from "../api";
import { APPROVAL, PATCHING, S } from "../strings";
import { ApprovingAs } from "../ui/ApprovingAs";
import { oddAnswer } from "../ui/answered";
import { AuthCard, AuthHead, AuthNote, Eyebrow } from "../ui/AuthCard";
import { heldLine } from "../ui/Headline";
import { Avatar } from "../ui/Avatar";
import { Button } from "../ui/Button";
import { Icon } from "../ui/Icon";
import { hex12 } from "../ui/Patched";
import { Receipt } from "../ui/Receipt";

// The production approval screen (design/specs/screens.md): the page `canyon deploy --env prod` sends a
// person to where the org has a person approve production (design/AUTH.md §4.5). The code is looked up,
// which is a read; only the click approves. Who asked and from where comes first; what goes live is the
// promote receipt, read for this reader; the account approving is named above the buttons. While the
// receipt blocks, Approve stays in its place, disabled, saying why (D6). Approving is the Promote
// button: the same promotion, followed on the app's page.

// "14:22 today", as the device page says it, or "tomorrow": an approval lasts a day. Further off names its date.
function when(at: string): string {
  const moment = new Date(at);
  const clock = moment.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  const tomorrow = new Date(Date.now() + 86_400_000).toDateString();
  if (moment.toDateString() === new Date().toDateString()) return `${clock} today`;
  return moment.toDateString() === tomorrow ? `${clock} tomorrow` : `${clock} on ${moment.toLocaleDateString()}`;
}

const fill = (line: string, values: Record<string, string>) => line.replace(/<(\w+)>/g, (whole, key: string) => values[key] ?? whole);

// The question the approver answers names what ships, and for a rollback what it replaces, as the CLI asks it (D5).
const question = (read: PromotionApproval) => {
  if (!read.rollback) return fill(APPROVAL.title, { app: read.app, version: read.version });
  if (read.replaces === null) return fill(APPROVAL.titlePutBackNothing, { app: read.app, version: read.version });
  return fill(APPROVAL.titlePutBack, { app: read.app, version: read.version, replaces: read.replaces });
};

// A patch approval's own part (design/PATCHING.md §8): what the patch changes, the proof that only Canyon's system
// libraries changed, and how preview's watch ended — then D5's confirm, as for any promotion, a rebuild's its own (#2631).
// The read files one only for a watch that passed, so its one result is said.
// A patch's fixes, one line per package it moved, its vulnerabilities together (#2579): the approval lists one per vulnerability.
function fixLines(fixes: PatchFix[]): string[] {
  const lines = new Map<string, { fix: PatchFix; vulnerabilities: string[] }>();
  for (const fix of fixes) {
    const key = `${fix.name} ${fix.from} ${fix.to}`;
    const line = lines.get(key) ?? { fix, vulnerabilities: [] };
    if (!line.vulnerabilities.includes(fix.vulnerability)) line.vulnerabilities.push(fix.vulnerability);
    lines.set(key, line);
  }
  return [...lines.values()].map(({ fix, vulnerabilities }) => fill(PATCHING.fix, { ...fix, vulnerability: vulnerabilities.join(", ") }));
}

// What the patch leaves (#2579): the checklist's known-vulnerable row is the patched image's, so what it still counts is
// what remains; and whether production, by its own image read, holds every one of those already. Anything Canyon could
// not read says it can't tell: never that production runs them.
function remainsLines(read: PromotionApproval, production: ImageRead | "unread" | null): string[] {
  const row = read.receipt?.rules.find((rule) => rule.id === "platform.known-vuln");
  if (row === undefined) return [];
  if (row.read_at === undefined) return [PATCHING.remainsUnchecked];
  const packages = row.packages ?? [];
  if (packages.length === 0) return [PATCHING.remainsNone];
  const single = packages.length === 1;
  const count = single ? PATCHING.remainsOne : fill(PATCHING.remainsSome, { n: String(packages.length) });
  if (production === null) return [count]; // still reading
  if (production === "unread" || production.state !== "checked") return [count, single ? PATCHING.productionUnknownOne : PATCHING.productionUnknown];
  const runs = packages.every((one) => one.advisories.every((advisory) => production.findings.some(({ dependency }) =>
    dependency.name === one.name && dependency.version === one.version && dependency.vulnerability === advisory.id)));
  if (single) return [count, runs ? PATCHING.inProductionOne : PATCHING.notInProductionOne];
  return [count, runs ? PATCHING.inProduction : PATCHING.notInProduction];
}

function PatchProof({ read, production }: { read: PromotionApproval; production: ImageRead | "unread" | null }) {
  const remains = remainsLines(read, production);
  return (
    <section aria-label="What the patch fixes" className="grid gap-2 rounded-lg border border-border px-4 py-3">
      <span className="text-sm text-text-faint">{PATCHING.fixes}</span>
      <ul className="grid gap-1">
        {fixLines(read.fixes ?? []).map((line) => <li key={line} className="font-mono text-md">{line}</li>)}
      </ul>
      {remains.length > 0 && (
        <>
          <span className="text-sm text-text-faint">{PATCHING.remains}</span>
          <p className="text-base">{remains.join(" ")}</p>
        </>
      )}
      <p className="flex items-start gap-2 text-base"><span className="mt-px text-ok"><Icon name="passed" size={16} /></span>{read.proof}</p>
      <span className="text-sm text-text-faint">{PATCHING.watch}</span>
      <p className="flex items-start gap-2 text-base"><span className="mt-px text-ok"><Icon name="passed" size={16} /></span>{PATCHING.watchPassed}</p>
    </section>
  );
}

export function Approve({ code, org }: { code: string; org: string | null }) {
  const [read, setRead] = useState<PromotionApproval | null>(null);
  const [missing, setMissing] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<"approved" | "denied" | null>(null);
  const [sending, setSending] = useState(false);

  // A 404 is the screen's line; anything else is the server's own sentence (a team refusal is its ask).
  const gone = (err: unknown) => {
    if ((err as ApiError).status === 404) setMissing(true);
    else setFailure(said(err));
  };
  useEffect(() => { getPromotionApproval(code).then(setRead, gone); }, [code]);
  // A patch's: production's image read, which what the patch leaves is compared with; a read that fails can't tell.
  const [production, setProduction] = useState<ImageRead | "unread" | null>(null);
  const asksPatch = read?.kind === "patch" && read.state === "pending" ? read.app : null;
  useEffect(() => {
    if (asksPatch !== null) getImage(asksPatch, "prod").then(setProduction, () => setProduction("unread"));
  }, [asksPatch]);

  const decide = (send: (code: string) => Promise<unknown>, result: "approved" | "denied") => {
    if (sending || read === null) return;
    setSending(true);
    send(read.code).then(() => setOutcome(result), (err: unknown) => { setFailure(said(err)); setSending(false); });
  };
  const failed = failure !== null && <p className="text-md text-critical">{failure}</p>;
  const deny = <Button size="block" onClick={() => decide(denyPromotion, "denied")}>{APPROVAL.deny}</Button>;

  const body = () => {
    if (missing) return <AuthHead mark={{ tone: "neutral", icon: "alert" }} title={APPROVAL.unknown} />;
    if (read === null) return failure === null ? null : <><AuthHead title={APPROVAL.titleUnread} />{failed}</>;
    if (outcome === "approved") {
      return (
        <AuthHead mark={{ tone: "ok", icon: "done" }} title={APPROVAL.approved} line={
          <>{fill(APPROVAL.approvedLine, { app: read.app, version: read.version })}<a className="font-medium text-link hover:underline" href={`#/apps/${encodeURIComponent(read.app)}`}>{APPROVAL.approvedLink}</a></>
        } />
      );
    }
    if (outcome === "denied") return <AuthHead mark={{ tone: "neutral", icon: "close" }} title={APPROVAL.denied} line={APPROVAL.deniedLine} />;
    const decider = read.decided_by?.display ?? "";
    const patch = read.kind === "patch";
    switch (read.state) {
      case "approved": return <AuthHead mark={{ tone: "ok", icon: "done" }} title={fill(APPROVAL.wasApproved, { name: decider })} line={APPROVAL.wasDecidedLine} />;
      case "denied": return <AuthHead mark={{ tone: "neutral", icon: "close" }} title={fill(APPROVAL.wasDenied, { name: decider })} line={APPROVAL.wasDecidedLine} />;
      // Canyon asked for a patch, so nobody asks again from the CLI: Canyon does, while the patch still applies.
      case "expired": return <AuthHead mark={{ tone: "neutral", icon: "waiting" }} title={APPROVAL.expired} line={patch ? PATCHING.asksAgain : APPROVAL.expiredLine} />;
      // A hold closed it (contracts/routes.md, Production approvals): the held sentence, and nothing to deny.
      case "outdated": return read.held !== undefined
        ? <AuthHead mark={{ tone: "critical", icon: "blocked" }} title={S.heldTitle} line={heldLine(read.held)} />
        : <><AuthHead mark={{ tone: "amber", icon: "alert" }} title={patch ? PATCHING.outdated : APPROVAL.outdated} line={patch ? PATCHING.asksAgain : APPROVAL.outdatedLine} />{deny}{failed}</>;
    }
    const receipt = read.receipt;
    const clear = receipt !== undefined && receipt.waiting === undefined && receipt.blocked_by.length === 0;
    const odd = oddAnswer(read.answered); // the person's to override: Approve sends it
    const blocking = receipt?.rules.filter((rule) => receipt.blocked_by.includes(rule.id)) ?? [];
    const [before, after] = (read.requested_via === "cli" ? APPROVAL.asked : APPROVAL.asked_elsewhere).split("<name>") as [string, string];
    return (
      <>
        <div className="grid gap-3">
          <AuthHead eyebrow={<Eyebrow icon="production">{APPROVAL.eyebrow}</Eyebrow>} title={patch ? fill(PATCHING.approvalTitle, { app: read.app }) : question(read)} />
          <div className="flex items-center gap-2">
            <Avatar name={read.requested_by.display} size="sm" />
            <p className="text-md text-text-dim">{before}<strong className="font-semibold text-text">{read.requested_by.display}</strong>{fill(after, { time: when(read.requested_at) })}</p>
          </div>
        </div>
        {!clear && (
          // Nothing in preview blocks with no rule named: the checklist's own line below is the reason.
          <AuthNote tone="critical" icon="blocked" title={APPROVAL.blocked}>
            {blocking.length === 0 ? undefined : (
              <>
                {blocking.map((rule) => `${rule.name}. `).join("")}
                {receipt?.blocked_by.includes("prod.clean-gate") && <a className="font-medium whitespace-nowrap text-link hover:underline" href={`#/apps/${encodeURIComponent(read.app)}`}>{APPROVAL.openReview}</a>}
              </>
            )}
          </AuthNote>
        )}
        <section aria-label="What changes" className="grid grid-cols-[1fr_auto_1fr] items-center gap-3 rounded-lg border border-border px-4 py-3">
          <div>
            <span className="block text-sm text-text-faint">{APPROVAL.now}</span><span className={read.replaces === null ? "text-base text-text-dim" : "font-mono text-lg font-medium"}>{read.replaces ?? APPROVAL.nothingYet}</span>
            {patch && read.replaces_image !== undefined && <span className="block font-mono text-sm text-text-faint">{fill(PATCHING.image, { hex: hex12(read.replaces_image) })}</span>}
          </div>
          <span className="text-text-faint"><Icon name="arrow" /></span>
          <div>
            <span className="block text-sm text-text-faint">{APPROVAL.live}</span><span className="font-mono text-lg font-medium text-accent">{read.version}</span>
            {/* A patch keeps the version: what tells the two apart is the image, like with like (#2593). Canyon's system
                libraries image is Settings › Patching's alone (#2605). */}
            {patch && read.image !== undefined && <span className="block font-mono text-sm text-text-faint">{fill(PATCHING.image, { hex: hex12(read.image) })}</span>}
          </div>
        </section>
        {patch && <PatchProof read={read} production={production} />}
        <p className="text-base text-text-dim">{read.rebuild === true ? PATCHING.rebuildConfirm : read.rollback ? S.putBackCodeBare : S.promoteConfirm /* a rollback puts back production's own earlier image, never preview's */}</p>
        {odd !== null && <AuthNote tone="amber" icon="alert" title={`${read.version} ${odd} on preview. Check the app before approving it.`} />}
        {receipt !== undefined && <Receipt receipt={receipt} framed />}
        <ApprovingAs approver={read.approver} onSwitch={() => void signOut().catch(gone)} />
        <div className="grid items-start gap-2.5 sm:grid-cols-2">
          {clear
            ? <Button variant="primary" size="block" onClick={() => decide((code) => approvePromotion(code, odd !== null), "approved")}>{APPROVAL.approve}</Button>
            : <Button size="block" disabled reason={APPROVAL.blockedReason}>{APPROVAL.approve}</Button>}
          {deny}
        </div>
        {failed}
        <div className="flex flex-wrap justify-between gap-x-3 gap-y-1 border-t border-border pt-3.5 text-md text-text-faint">
          <span>{fill(APPROVAL.expires, { time: when(read.expires_at) })}</span>
          <span>{APPROVAL.code}<span className="font-mono">{read.code}</span></span>
        </div>
      </>
    );
  };

  return <AuthCard org={org} wide>{body()}</AuthCard>;
}
