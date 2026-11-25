import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";
import { type ApiError, type Earlier, entries, getMaskedLogs, type Env, type Hold, type Latest, type Need, type Receipt, type RunRead } from "../api";
import { S } from "../strings";
import { actorName } from "./Actor";
import { down } from "./EnvLink";
import { Icon, type IconName } from "./Icon";
import { approved, buildKinds, cleared, failureWords, tryAgain } from "./Verdict";
import { oddAnswer } from "./answered";

// The app page's one sentence about the newest version and the one thing to do next (the app-page
// redesign, block 1). It is the only place the page says the state in words, from the server's facts
// alone: the project's latest, the newest run's read, what runs where and the go-live receipt.
export type Act = "review" | "approve" | "cancel" | "promote" | "open" | "copy" | "outbound";
// review: the band is about the newest version's review, which the fleet's side panel offers to open (#2723): never one
// about where versions run or a hold, which no review changes.
// covers: the needs that say what this band says (contracts/routes.md, *What needs a person*), so the two are never drawn
// together (D15; leads, below). repeats: the needs whose own lines say what this band says without its steps, so it isn't
// drawn while they are. tail: the environment that's down, whose last log lines the app page shows under it (#2744), and
// lined the text in place of text where they show. back: the earlier version a production that's down can be put back to (#188).
export interface Band { running?: true; tone: "red" | "amber" | "green" | "teal" | "neutral"; title: string; text: string; said?: string; primary?: Act; secondary?: Act; going?: true; review?: true; covers?: string[]; repeats?: string[]; tail?: "preview" | "prod"; lined?: string; back?: Earlier }

export interface Facts {
  latest: Latest; // the newest version
  newest: string | null; // its name, as the page names a version
  neverReviewed: boolean;
  run: RunRead | null; // the newest run's read, when it is the newest version's
  preview: Env | null;
  prod: Env | null;
  previewName: string | null;
  prodName: string | null;
  receipt: Receipt;
  hold?: Hold; // the status read's hold, which says whether production ran anything when it was held (#2696)
  broke?: { cause: string | null; reason: string | null }; // a failed build's or start's own lines, once read
  progress?: string | null; // a review going: how many of its checks are done, and which waits its turn
  refused?: string[]; // production's egress requests pending, the destinations Canyon refused that wait on a person (#2723)
  secret?: boolean; // the status read needs the newest version's secret rotated: the blocked band says it, in that need's place
  earlier?: Earlier[]; // production's earlier versions, newest first, where the reader's read carries them
}

// After a review that let the build go ahead, what broke: the review is said as it ended (approved is never passed).
const AFTER: Record<string, { title: string; text: string }> = {
  build: { title: ", then didn't build", text: "Its last line says why. Fix it in the code, then deploy again." },
  start: { title: "built, then stopped right after it started", text: "Its last lines say why. Fix that, then deploy again." },
};
const blockedBy = (receipt: Receipt) => receipt.waiting ?? (receipt.blocked_by.length === 0 ? null : receipt.rules.find((rule) => rule.id === receipt.blocked_by[0])?.why ?? "a go-live check isn't met");
// What a scan that couldn't finish waits on, by its reason (contracts/finding.md, Canyon-run tools: its `problem` reads
// exactly `could not check: <tool>: <reason>`), where a review again fails the same way (#2723): a database too old to
// read, no image for the tool, rules it couldn't load or an answer that isn't SARIF are the host's, whoever installed
// Canyon's to fix; an answer past what Canyon reads is the code's. Meanwhile an approver gets past it. Any other may finish
// on a second try: a database that couldn't be downloaded is downloaded again by the next scan (executor/scan.go).
const HOST: Record<string, string> = {
  "its vulnerability database is over 5 days old": S.reviewWontHelpDatabase,
  "this Canyon host has no image for it": S.reviewWontHelpImage,
  "could not load all of its rules": S.reviewWontHelpRules,
  "did not answer SARIF": S.reviewWontHelpTool,
  "answered more than Canyon reads": S.reviewWontHelpOverCap,
};
// hostWords is that line for a check the server read as the host's to fix (contracts/routes.md, *What needs a person*, unchecked).
export const hostWords = (tool: string, reason: string) => HOST[reason]?.replace(/<tool>/g, () => tool) ?? null;
const hostFix = (problem: string) => {
  const [, tool, reason] = /^could not check: ([^:]+): (.+)$/.exec(problem) ?? [];
  return tool === undefined || reason === undefined ? null : hostWords(tool, reason);
};

// A hold's reason as the held sentence ends it: a full stop after it, unless it already ends a sentence, which read
// "while we look.. It stays stopped" (#2723).
const ended = (reason: string) => (/[.?!]$/.test(reason) ? reason : `${reason}.`);
// A held production in D5's words (contracts/routes.md, The hold): who held it, the day in UTC, and why — held, never
// stopped, where production ran nothing when it was held (its `version` null; #2696), and stopped where a read carries
// no version to say otherwise (the receipt's `held`). One pass, so a reason or a name that holds `<date>` is said as
// written, never filled in.
export const heldLine = (hold: Omit<Hold, "version"> & { version?: Hold["version"] }) => {
  const filled: Record<string, string> = { "<who>": actorName(hold.by), "<date>": hold.at.slice(0, 10), "<reason>": hold.reason, "<reason>.": ended(hold.reason) };
  return (hold.version === null ? S.heldNothing : S.held).replace(/<who>|<date>|<reason>\.?/g, (key) => filled[key]!);
};
// The banner above production while it is held: it outranks everything else on the page, as a stopped production does.
export const heldBand = (hold: Omit<Hold, "version"> & { version?: Hold["version"] }): Band => ({ tone: "red", title: S.heldTitle, text: heldLine(hold), covers: ["held"] });
const findings = (n: number) => `${n} blocking ${n === 1 ? "finding" : "findings"}`;

// How often a restarting environment was started again since its deploy, in its headline: " 7 times",
// " once"; "" when the read carries no count.
const times = (env: Env) => (env.restarting === undefined ? "" : env.restarting.restarts === 1 ? " once" : ` ${env.restarting.restarts} times`);
// What a restarting environment's headline says under it: how it last exited when the read says it, with
// the app's last line below, else where its reason is (#1597); after is what the band says next.
const restarted = (env: Env, at: "preview" | "prod", after: string) => {
  const code = env.restarting?.exit_code;
  const said = env.restarting?.said || undefined;
  const text = (code === undefined ? S.restartedWayOut : S.restartedLastExit.replace("<code>", String(code)).replace("<below>", said === undefined ? "" : " The app's last line is below.")).replace("<env>", at) + after;
  return { text: text + (code === undefined ? S.restartedHasWhy.replace("<env>", at) : ""), lined: text, said, tail: at };
};
// Where a production that's down can go back to: the newest earlier version Canyon kept, else, where the read carries
// earlier[], why none can (#188).
const back = (f: Facts) => {
  const row = f.earlier?.find((one) => one.kept);
  return { back: row, none: f.earlier !== undefined && row === undefined ? S.putBackNone : "" };
};
const sentences = (...parts: string[]) => parts.filter((one) => one !== "").join(" ");

const READY_COVERS: Record<string, string[]> = { "prod.clean-gate": ["blocked_prod"], "prod.egress-allowed": ["unchecked:prod"] };

export function headline(f: Facts): Band {
  if (f.receipt.held !== undefined) return heldBand(f.hold ?? f.receipt.held); // the page's read of the hold where it has one
  const v = f.newest ?? "This version";
  // What preview and production keep running, said wherever the newest version went nowhere: never of
  // one that stopped, which runs nothing.
  const other = (env: Env | null) => env !== null && !down(env) && env.package !== f.latest.package;
  const keeps = other(f.preview) && other(f.prod) && f.preview!.package === f.prod!.package ? ` ${f.prodName} keeps running in preview and production.`
    : `${other(f.preview) ? ` ${f.previewName} keeps running in preview.` : ""}${other(f.prod) ? ` ${f.prodName} keeps running in production.` : ""}`;
  // An environment that ran and stopped outranks everything else on the page.
  if (f.prod?.state === "stopped") {
    const code = f.prod.stopped === undefined ? "" : ` with exit code ${f.prod.stopped.exit_code}`;
    const twin = f.preview !== null && f.preview.package === f.prod.package && f.preview.state === "running" ? "Preview runs the same version and is still up." : "";
    const { back: row, none } = back(f);
    return { tone: "red", title: `Production is down: ${f.prodName} stopped${code}`, text: sentences(twin, `${f.prod.stopped?.said ? "The app's last line is below; " : ""}canyon logs --env prod has the rest.`, none),
      lined: sentences(twin, none), said: f.prod.stopped?.said || undefined, tail: "prod", back: row, covers: ["down:prod"] };
  }
  // One started again since its deploy is not running, up or not at this instant, and the way out is a new deploy (#1722).
  if (f.prod?.state === "restarting") {
    const { back: row, none } = back(f);
    return { tone: "red", title: `${f.prodName} restarted${times(f.prod)} since it was deployed to production`, ...restarted(f.prod, "prod", none === "" ? "" : ` ${none}`), back: row, covers: ["down:prod"] };
  }
  const run = f.run;
  if (run !== null && (run.state === "pending" || run.state === "running")) {
    return { tone: "teal", going: true, title: `Reviewing ${v}`, text: `${f.progress ?? "This page follows it as it goes."}${f.prod !== null ? ` Production stays on ${f.prodName} until a person promotes.` : ""} Nothing for you to do.`, secondary: "cancel" };
  }
  if (f.latest.package === null || f.latest.stage === "created") return { tone: "neutral", title: "Nothing uploaded yet", text: "Deploy it from its directory with canyon deploy." };
  // A version awaiting review has no run of its own yet, whatever run the app read last.
  if (f.neverReviewed || f.latest.stage === "awaiting_review") return { tone: "amber", title: `${v} hasn't been reviewed`, text: S.neverReviewed, primary: "review", covers: ["never_reviewed"] };
  if (run !== null && run.state === "failed") {
    if (run.fix !== undefined) return { tone: "red", title: `${v} is missing a ${run.fix.file}`, text: run.fix.why + keeps, review: true };
    const after = AFTER[run.failure_reason ?? ""];
    // A build's cause leads, in its own words; a start's is the executor's reason, its lines saying why (#2352).
    // A start's title is what broke; how its review ended leads the sentence under it (the mockup's "64e97a built, then stopped").
    if (after !== undefined) {
      const start = run.failure_reason === "start";
      return { tone: "red", title: start ? `${v} ${after.title}` : `${v} ${cleared(run)}${after.title}`, text: (start ? `It ${cleared(run)} and built. ${after.text}` : after.text) + keeps,
        said: f.broke?.cause ?? f.broke?.reason ?? undefined, secondary: "copy" };
    }
    if (run.failure_reason === "detect") {
      const builds = run.builds ?? [];
      return { tone: "red", title: `${v} couldn't be built on this host`, text: `${builds.length > 0 ? S.undetectedBuilds.replace("<kinds>", () => buildKinds(builds)) : S.undetectedNone} ${run.errored ? S.undetectedErrored : "Nothing was compiled, so there is nothing in the code to fix."}${keeps}`, review: true };
    }
    const words = failureWords(run.failure_reason, run.failed_in);
    return { tone: "red", title: `${v}'s review didn't finish`, text: `${words[0]!.toUpperCase()}${words.slice(1)}. ${tryAgain(run) ?? "Review it again."}${keeps}`, primary: "review" };
  }
  const open = run === null || run.state !== "blocked" ? [] : run.findings.filter((one) => one.properties.canyon.blocks && one.approval === undefined);
  if (run !== null && run.state === "blocked" && run.blocking > 0 && open.length > 0) {
    const n = entries(open);
    const unfinished = entries(open.filter((one) => one.ruleId === "gate.unreadable"));
    if (unfinished === n) {
      const title = `${v} is blocked: ${unfinished === 1 ? "1 of Canyon's checks" : `${unfinished} of Canyon's checks`} couldn't finish`;
      const host = open.map((one) => hostFix(one.properties.canyon.problem)).find((fix) => fix !== null);
      if (host !== undefined) return { tone: "red", title, text: `A check that didn't run can't vouch for the code, so it blocks. Nothing found in the code blocks it. ${host}${keeps}`, secondary: "approve", review: true, covers: ["unchecked"] };
      return { tone: "red", title, text: `A check that didn't run can't vouch for the code, so it blocks. Nothing found in the code blocks it. Review it again: the checks wait their turn instead of failing.${keeps}`, primary: "review", secondary: "approve", covers: ["unchecked"] };
    }
    // A secret the review found is the same problem the needs' secret line names: said here once, with its step (#102).
    const fix = f.secret === true ? S.needSecretFix : n === 1 ? S.needBlockedFixOne : S.needBlockedFixMany;
    return { tone: "red", title: `${v} is blocked: ${findings(n)}`, text: `${fix} ${S.nothingBuilt}${keeps}`, secondary: "approve", review: true, covers: f.secret === true ? ["blocked", "secret"] : ["blocked"] };
  }
  // Only a running environment is live: one Canyon couldn't check is never called live (routes.md, envs.<env>.state).
  if (f.prod !== null && f.prod.package === f.latest.package && f.prod.state === "unknown") {
    return { tone: "amber", title: `${v} is in production, but Canyon couldn't check whether it runs`, text: "canyon logs --env prod shows what it is doing.", primary: "open" };
  }
  // Live is never said in green while production can't reach a destination nobody has allowed yet (#2723): the band
  // names it, and its step is the Outbound card's Allow.
  const [refused, ...more] = f.refused ?? [];
  if (f.prod !== null && f.prod.package === f.latest.package && refused !== undefined) {
    return { tone: "amber", title: `${S.cantReach.replace("<version>", () => v).replace("<destination>", () => refused)}${more.length > 0 ? S.cantReachMore.replace("<n>", () => String(more.length)) : ""}`,
      text: S.cantReachWhy.replace("<destination>", () => refused), primary: "outbound", repeats: ["refused:prod"] }; // stands in for no need: each refused line keeps its Allow
  }
  if (f.prod !== null && f.prod.package === f.latest.package) {
    const twin = f.preview !== null && f.preview.package === f.prod.package ? "Preview runs the same version. " : "";
    const partly = run?.not_checked ? "Nothing blocking in what was checked; some things weren't checked." : "";
    return { tone: "green", title: `${v} is live in production`, text: `${twin}${partly}`.trim(), primary: "open" };
  }
  if (f.preview !== null && f.preview.package === f.latest.package && f.preview.state === "restarting") {
    return { tone: "red", title: `${v} restarted${times(f.preview)} since it was deployed to preview`, ...restarted(f.preview, "preview", keeps), covers: ["down:preview"] };
  }
  if (f.preview !== null && f.preview.package === f.latest.package && f.preview.state === "stopped") {
    const code = f.preview.stopped === undefined ? "" : ` with exit code ${f.preview.stopped.exit_code}`;
    return { tone: "red", title: `${v} stopped in preview${code}`, text: `${f.preview.stopped?.said ? "The app's last line is below; " : ""}canyon logs --env preview has the rest.${keeps}`,
      lined: keeps.trim(), said: f.preview.stopped?.said || undefined, tail: "preview", covers: ["down:preview"] };
  }
  if (f.preview !== null && f.preview.package === f.latest.package && f.preview.state === "unknown") {
    return { tone: "amber", title: `${v} is in preview, but Canyon couldn't check whether it runs`, text: `canyon logs --env preview shows what it is doing.${keeps}` };
  }
  if (f.preview !== null && f.preview.package === f.latest.package) {
    const why = blockedBy(f.receipt);
    // It stands in for the one need it says: blocked_by[0]'s, findings that block production alone, or the go-live rule
    // that couldn't check; never what production won't allow, whose Allow it doesn't offer.
    if (why !== null) return { tone: "amber", title: `${v} isn't ready for production`, text: `${why}${keeps}`, review: true, covers: READY_COVERS[f.receipt.blocked_by[0] ?? ""] ?? [] };
    const { set, met } = goLive(f.receipt);
    const open = set - met; // nothing blocks here, so a check not met is an advisory
    const checks = open > 0 ? `${met} of ${set} go-live checks, with ${open} ${open === 1 ? "advisory" : "advisories"} open` : set === 1 ? "the go-live check" : `all ${set} go-live checks`;
    // What preview answered at / is said beside the checks when it is an answer to say beside readiness:
    // promoting it anyway is the person's call, and the CLI's never.
    const odd = oddAnswer(f.preview.answered);
    // A production approval asked says the same with its own step, Review and approve: Promote would ask again, so the
    // band gives way to it (repeats; the v0.11.0 role walk drew both).
    if (odd !== null) return { tone: "amber", title: `${v} meets ${checks}, but ${odd}`, text: "Check the app before promoting: the preview card shows what it said. Production runs the same image.", primary: "promote", repeats: ["approval"] };
    return { tone: "teal", title: `${v} is ready for production`, text: `It ${run === null ? "passed review" : cleared(run)}, runs in preview, and meets ${checks}. Production runs the same image, with production's own secrets: nothing is rebuilt.`, primary: "promote", repeats: ["approval"] };
  }
  // Every blocker approved and the version gone nowhere yet: where it runs, the states above say it.
  if (run !== null && run.state === "blocked" && approved(run) !== null) return { tone: "amber", title: `${v} was approved past its review`, text: `${approved(run)!.replace(/^! /, "")}. Approved is not passed: the findings stand.${keeps}`, review: true };
  if (run !== null && run.state === "clean") {
    return { tone: "neutral", title: `${v} ${cleared(run)}`, text: f.latest.stage === "building" ? `It starts in preview when the build is done.${keeps}` : `It isn't running anywhere yet. Deploy it to run it in preview.${keeps}` };
  }
  // No read of the newest run to say more: the stage alone, as the fleet says it.
  switch (f.latest.stage) {
    case "blocked": return { tone: "red", title: `${v} is blocked at review`, text: `${S.nothingBuilt}${keeps}`, review: true, covers: ["blocked", "unchecked"] };
    case "approved": return { tone: "amber", title: `${v} was approved past its review`, text: `A person approved what blocked it, with a reason.${keeps}`, review: true };
    case "failed": return { tone: "red", title: `${v} didn't reach preview`, text: `Its run failed.${keeps}`, review: true };
    case "reviewing": return { tone: "teal", going: true, title: `Reviewing ${v}`, text: "Nothing for you to do." };
    case "building": return { tone: "teal", going: true, title: `Building ${v}`, text: `It passed review and starts in preview when the build is done.${keeps}` };
    default: return { tone: "neutral", title: `${v} passed review`, text: `It isn't running anywhere yet. Deploy it to run it in preview.${keeps}` };
  }
}

// The band beside what needs a person (D15), most urgent first. Where the band says what the top need says, it leads in
// that need's place, in its own fuller words and with its steps, and the other needs follow it. Where it says what a need
// further down says, the needs alone are drawn, that one in its place. Production's green gives way to any need that ranks
// 6 or above, production's or the app's; beside needs that all rank 7 and below it leads, the needs under it. Any other
// band is drawn under the needs.
export function leads(band: Band | null, needs: Need[]): { band: Band | null; first: boolean; needs: Need[] } {
  const says = (list: string[] | undefined) => (need: Need) => list?.some((cover) => cover === need.kind || cover === `${need.kind}:${need.env}`) === true;
  const covers = says(band?.covers);
  if (band === null || needs.length === 0) return { band, first: true, needs };
  if (covers(needs[0]!)) return { band, first: true, needs: needs.slice(1) };
  if (needs.some(covers) || needs.some(says(band.repeats)) || (band.tone === "green" && needs.some((need) => need.rank <= 6))) return { band: null, first: false, needs };
  if (band.tone === "green" && needs.every((need) => need.rank >= 7)) return { band, first: true, needs };
  return { band, first: false, needs };
}

// A read-only member's headline: where the app runs, from the reduced read's environments alone. A
// responder with no grant reads nothing of preview, so theirs speaks of production alone (#2258).
export function viewerBand(envs: { preview?: { state?: Env["state"] }; prod?: { state?: Env["state"] } } | undefined, readOnly: string, preview = true): Band {
  const runs = (env: { state?: Env["state"] } | undefined) => env !== undefined && env.state !== "stopped" && env.state !== "restarting" && env.state !== "unknown";
  const any = (state: Env["state"]) => (preview ? [envs?.prod, envs?.preview] : [envs?.prod]).some((env) => env?.state === state);
  const title = runs(envs?.prod) ? "Running in production" : preview && runs(envs?.preview) ? "Running in preview"
    : any("unknown") ? "Canyon couldn't check whether it runs" : any("restarting") ? "Keeps restarting" : preview ? "Not running" : "Not running in production";
  return { tone: "neutral", title, text: readOnly, ...(runs(envs?.prod) || (preview && runs(envs?.preview)) ? { running: true as const } : {}) };
}

const TONE: Record<Band["tone"], { box: string; icon: IconName; ink: string }> = {
  red: { box: "border-l-critical bg-critical-dim", icon: "blocked", ink: "text-critical" },
  amber: { box: "border-l-amber bg-amber-dim", icon: "alert", ink: "text-amber" },
  green: { box: "border-l-ok bg-surface", icon: "passed", ink: "text-ok" },
  teal: { box: "border-l-accent bg-accent-dim", icon: "passed", ink: "text-accent" },
  neutral: { box: "border-l-border-strong bg-surface", icon: "never", ink: "text-text-faint" },
};

// The go-live checks a receipt sets and how many it met: counted once, for the headline and the pipeline's
// cell, so the two never say different numbers.
export function goLive(receipt: Receipt): { set: number; met: number } {
  const set = receipt.rules.filter((rule) => rule.unset !== true);
  return { set: set.length, met: set.filter((rule) => rule.passed).length };
}

// What a failed masked read of an environment's lines says: a log it can't mask (409), one too long to (422), else unread;
// to the app's team, canyon logs after it, which reads it raw. A responder's read is masked alone, so theirs names no CLI.
export const unread = (err: unknown, env: "preview" | "prod", team: boolean) => {
  const status = (err as ApiError).status;
  // a responder's read alone is refused where a value is too short to hide by its name (contracts/routes.md, the logs row)
  if (status === 409 && ((err as ApiError).body as { error?: string } | null)?.error === "too_short_to_mask") return S.logsTooShort;
  return (status === 409 ? S.lastLinesUnmasked : status === 422 ? S.lastLinesTooLong : S.lastLinesUnread) + (team ? S.lastLinesCli.replaceAll("<env>", env) : "");
};

// A down environment's last log lines, masked by the server (#2744, #188), for a contributor and above, who may read
// its logs: the caller never draws it for a reader below. They stand in for said, the one line the status read
// carries. Until they come, or when none can be shown, said stays, with why the rest can't; a 403 adds nothing, since
// it's the reader's access, not a fault. place is where it sits in its parent's grid. The band draws it for the
// environment it speaks of, and an environment's card for one the band doesn't, so the need's "on its page" holds.
export function LastLines({ app, env, said, place = "" }: { app: string; env: "preview" | "prod"; said?: string; place?: string }) {
  const [read, setRead] = useState<{ lines: string } | { failed: string | null } | null>(null);
  const newest = useRef<HTMLPreElement>(null);
  // The newest line is the last, in view as the lines are drawn: a layout effect runs before anything reads or paints
  // them, where a passive one let the top show first (#2907).
  useLayoutEffect(() => { if (newest.current !== null) newest.current.scrollTop = newest.current.scrollHeight; }, [read]);
  useEffect(() => {
    let live = true;
    setRead(null);
    getMaskedLogs(app, env, 20).then((lines) => { if (live) setRead({ lines: lines.trimEnd() }); },
      (err) => { const status = (err as ApiError).status; if (live) setRead({ failed: status === 403 ? null : unread(err, env, true) }); });
    return () => { live = false; };
  }, [app, env, said]);
  const box = "mt-2 rounded-[7px] border border-border bg-surface px-3 py-2 font-mono text-md break-words";
  if (read !== null && "lines" in read && read.lines !== "") {
    return (
      <figure className={`${place} mt-2 grid gap-1`}>
        <figcaption className="text-sm text-text-dim">{S.lastLines}</figcaption>
        <pre ref={newest} className={`${box} mt-0 max-h-80 overflow-auto whitespace-pre-wrap max-[620px]:max-h-48`}>{read.lines}</pre>
      </figure>
    );
  }
  return (
    <>
      {said !== undefined && <p className={`${place} ${box}`}>{said}</p>}
      {read !== null && "failed" in read && read.failed !== null && <p className={`${place} text-sm text-text-dim`}>{read.failed}</p>}
    </>
  );
}

// The band's own last line, or, for the environment it speaks of, its last log lines where the page may read them.
function Said({ band, app }: { band: Band; app?: string }) {
  const place = "col-span-2 col-start-2 max-[620px]:col-start-1";
  if (app === undefined || band.tail === undefined) {
    return band.said === undefined ? null : <p className={`${place} mt-2 rounded-[7px] border border-border bg-surface px-3 py-2 font-mono text-md break-words`}>{band.said}</p>;
  }
  return <LastLines app={app} env={band.tail} said={band.said} place={place} />;
}

// The band: the state's colour on its left edge, the sentence, the reason under it, the app's own last
// line when one says why, and the actions on the right. The headline is the page's status.
export function Headline({ band, app, children }: { band: Band; app?: string; children?: ReactNode }) {
  const tone = TONE[band.tone];
  const text = app !== undefined && band.tail !== undefined ? band.lined ?? band.text : band.text; // the lines say why where they're shown
  return (
    <section aria-label="Where it stands" className={`grid grid-cols-[22px_minmax(0,1fr)_auto] items-start gap-x-3.5 gap-y-1 rounded-[10px] border border-border border-l-4 px-[18px] py-4 max-[620px]:grid-cols-[22px_minmax(0,1fr)] ${tone.box}`}>
      {/* running, the environment cards' green dot: the empty circle said nothing ran (#102) */}
      {band.running ? <span className="mt-1.5 grid size-5 place-items-center"><span aria-hidden="true" className="size-2.5 rounded-full bg-ok" /></span>
        : <span className={`mt-0.5 ${tone.ink}`}><Icon name={band.going ? "working" : tone.icon} size={20} /></span>}
      <div role="status" className="grid gap-1">
        <h2 className="text-[17px] font-semibold tracking-tight">{band.title}</h2>
        {text !== "" && <p className="max-w-[720px] text-base text-text-dim">{text}</p>}
      </div>
      {children !== undefined && <div className="flex flex-wrap items-center justify-end gap-2 max-[620px]:col-start-2 max-[620px]:justify-start">{children}</div>}
      <Said band={band} app={app} />
    </section>
  );
}
