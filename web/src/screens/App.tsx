import { useContext, useEffect, useRef, useState } from "react";
import { ApiError, approve, approveFindings, askForAccess, cancelRun, getPolicies, getPreferences, getProject, getRun, getStatus, listPackages, listRuns, promote, pullRequestName, redeploy, reduced, said, startReview, version, waiting, type App as AppRead, type Env, type FleetRow, type Need, type Policy, type ProjectEgress, type PolicyState, type Receipt as ReceiptRead, type Reduced, type Finding as FindingRead, type RunRead, type RunRow, type Status } from "../api";
import { AUTH, RENAME, S, TEARDOWN } from "../strings";
import { Actor, actorName } from "../ui/Actor";
import { Age } from "../ui/Age";
import { AppName } from "../ui/AppName";
import { AppSource } from "./AppSource";
import { Button } from "../ui/Button";
import { EnvCard } from "../ui/EnvCards";
import { down } from "../ui/EnvLink";
import { DependencyCard } from "../ui/DependencyCard";
import { Finding, type ApproveBody } from "../ui/Finding";
import { goLive, Headline, headline, heldBand, LastLines, leads, viewerBand, type Act } from "../ui/Headline";
import { NeedsBand } from "../ui/Need";
import { Icon } from "../ui/Icon";
import type { Tone } from "../ui/Pill";
import { Rail, RailContext } from "../ui/Rail";
import { Receipt } from "../ui/Receipt";
import { reviewOf } from "../ui/Stage";
import { approved, blockingEntries, failedAfterReview, TONE, verdict } from "../ui/Verdict";
import { Access, sharedLine, type Asked } from "./Access";
import { HoldDialog, lifts, type Acting } from "./FleetHold";
import { Outbound, prodPending } from "./Outbound";
import { Provided } from "./Provided";
import { SystemLibraries } from "./SystemLibraries";
import { AppSections } from "./AppSections";
import { BuildFailure, CopyForAgent, useBroke } from "./BuildFailure";
import { Progress, progressWords, useProgress } from "./Progress";
import { Missing } from "../ui/Missing";
import { ownerHref } from "./Owner";
import { Promote, promoteReason } from "./Promote";
import { EarlierVersions } from "./PutBack";
import { ReviewGate } from "./ReviewGate";
import { RenameApp } from "./Rename";
import { DeleteApp, TakeOffline } from "./Teardown";
import { Versions, type Cell, type Version } from "./Versions";

const POLL_MS = 2000;
const H2 = "text-base font-semibold";
const ended = (run: RunRead) => run.state !== "pending" && run.state !== "running";
const running = (env: Env | { pending: unknown } | undefined): Env | null => (env !== undefined && !waiting(env as Env) ? (env as Env) : null);
const MARK: Record<Tone, Cell["mark"]> = { ok: "pass", critical: "stop", amber: "wait", neutral: "none" };
const NONE: Cell = { mark: "none", text: "—" };
// The step that broke after a review passed, where preview would be. A detect has no log to show: its
// lines are each buildpack declining the app, so the verdict's "what this host builds" says it all.
const STEP: Record<string, string> = { build: "Build failed", detect: "Can't build this kind of app", start: "Didn't stay up", no_room: "No room to build" };

// The policy a finding takes its state from (contracts/finding.md, *From the tool into the profile*), as the gate reads
// it (gate/finding's canyonRule): sarif.<tool> for an imported result no policy covers, the policy a tool's result is
// mapped to (<policy>.<tool>.<its rule>), else the finding's own rule.
function policyOf(one: FindingRead): string {
  const dot = one.ruleId.indexOf(".");
  if (dot >= 0 && one.ruleId.slice(0, dot) === "sarif") {
    const rest = one.ruleId.slice(dot + 1);
    if (rest.includes(".")) return `sarif.${rest.slice(0, rest.indexOf("."))}`;
  }
  const tool = one.tool?.startsWith("sarif/") ? one.tool.slice("sarif/".length) : undefined;
  if (tool === undefined) return one.ruleId;
  const at = one.ruleId.indexOf(`.${tool}.`);
  return at < 0 ? one.ruleId : one.ruleId.slice(0, at);
}
// A blocking finding whose absence from a later review proves nothing (#2696): Canyon's own could-not-check, which says
// only that a tool did not finish (contracts/finding.md, gate.unreadable); the AI review's, whose model need not say it
// again; and a custom rule's, checked by the model or by code over facts Canyon may not read the same way twice.
const AGENTS = new Set(["canyon-review", "custom-rules"]);
const unprovable = (one: FindingRead) => one.ruleId === "gate.unreadable" || one.ruleId.startsWith("ai.") || one.ruleId.startsWith("custom.") || (one.tool !== undefined && AGENTS.has(one.tool));
const RANK: Record<PolicyState, number> = { off: 0, advisory: 1, blocking: 2 };
// A rule's state before preview for this app, the stricter of the org's and the app's own addition (contracts/routes.md,
// the app policies row); undefined where the read names no such rule.
function stateFor(policies: Policy[], rule: string, app: string): PolicyState | undefined {
  const one = policies.find((policy) => policy.id === rule);
  if (one === undefined) return undefined;
  const added = one.additions?.find((addition) => addition.project === app)?.state;
  return added !== undefined && RANK[added] > RANK[one.state] ? added : one.state;
}
// Whether a later review that lacks the finding read what found it: Canyon's own steps, every review; an imported
// tool's (sarif/<tool>) only where Canyon runs that tool on every review — gitleaks, in process, and a tool Canyon runs
// itself, its sarif.<tool> rule carrying `run` and not Off (D5: a tool whose rule is Off is not run, whatever run says).
// An attached report is the person's to attach again, and a finding that names no tool can't say what found it.
function readAgain(one: FindingRead, policies: Policy[]): boolean {
  if (one.tool === undefined) return false;
  if (!one.tool.startsWith("sarif/")) return true;
  const tool = one.tool.slice("sarif/".length);
  if (tool === "gitleaks") return true;
  const rule = policies.find((policy) => policy.id === `sarif.${tool}`);
  return rule?.run === true && rule.state !== "off";
}
// Fixed only where the later review proves it: every blocking finding of the earlier one is one whose absence proves
// something, found by what the later review ran again, of a rule that still blocks this app as the dashboard reads it,
// and not in the later review.
function provesFixed(earlier: FindingRead[], later: FindingRead[], policies: Policy[], app: string): boolean {
  const here = new Set(later.map((one) => one.partialFingerprints["canyon/v1"]));
  const blocking = earlier.filter((one) => one.properties.canyon.blocks);
  return blocking.length > 0 && blocking.every((one) => !unprovable(one) && readAgain(one, policies) && stateFor(policies, policyOf(one), app) === "blocking" && !here.has(one.partialFingerprints["canyon/v1"]));
}

// A deployed version's review, from the run that cleared it for where it runs: a person's approval
// is an exception, never a pass, and a run not read yet says nothing. A run whose build or start failed
// after its review is read by how that review ended.
function cleared(run: RunRead | Error | undefined): Cell {
  if (run === undefined || run instanceof Error) return NONE;
  const state = failedAfterReview(run) ? (approved(run) === null ? "clean" : "blocked") : run.state;
  if (state === "clean") return run.not_checked ? { mark: "wait", text: "Not fully checked" } : { mark: "pass", text: "Passed" };
  if (state === "blocked") return approved(run) === null ? { mark: "stop", text: "Blocked" } : { mark: "wait", text: "Approved past review" };
  if (state === "failed") return { mark: "stop", text: "Failed" };
  return { mark: "wait", text: "Reviewing" };
}

// One app: what runs where, the button that moves preview to production, and the checklist that
// decides it. The checklist, its verdicts and the stage are the server's, never worked out here.
// `run` pins the findings to that run; the promote button still sends the newest.
export function App({ id, run: pinned = null, auth, outbound = false }: { id: string; run?: string | null; auth: "oidc" | "off" | null; outbound?: boolean }) {
  const { viewer, admin } = useContext(RailContext);
  const [project, setProject] = useState<AppRead | Reduced | null>(null);
  const [status, setStatus] = useState<Status | null>(null);
  // An app with no current package has no review of its own; its newest check's run is shown, read-only, as a check.
  const [checked, setChecked] = useState<RunRow | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const [run, setRun] = useState<RunRead | Error | null>(null);
  const [seen, setSeen] = useState<RunRead | null>(null);
  // The runs that cleared what preview and production run, read once each: a deployed version's
  // review is its own run's, never the app's newest.
  const [envRuns, setEnvRuns] = useState<Record<string, RunRead | Error>>({});
  // Bumped when an approval may have changed what those runs say, so they are read again.
  const [approvals, setApprovals] = useState(0);
  const [reviewFailure, setReviewFailure] = useState<string | null>(null);
  // A lift of production's hold being asked, as the fleet's row asks it (#2723).
  const [lifting, setLifting] = useState<Acting | null>(null);
  // What production can't reach that waits on a person, as the Outbound card last read it (#2723).
  const [refused, setRefused] = useState<string[]>([]);
  // The Outbound card's last read, for each environment card's line on what it calls out to.
  const [egress, setEgress] = useState<ProjectEgress | null>(null);
  // The Access card asked for from the page's head (the owner's decision 1, 2026-11-08): who can open it, or Share.
  const [accessAsked, setAccessAsked] = useState<Asked>(null);
  // A read-only member's request for more access (#1706): asked, or the server's sentence when it refused.
  const [asked, setAsked] = useState<true | string | null>(null);
  const tick = useRef(async () => {});
  // Whether the reader is a security responder, read once: one reads what approving needs of any app (#2204).
  const responder = useRef<Promise<boolean> | null>(null);
  const promoting = useRef(false);
  const starting = useRef(false);

  useEffect(() => {
    let live = true;
    tick.current = async () => {
      try {
        const { project: p } = await getProject(id);
        // A responder below contributor reads what approving needs: the status read and the runs (design/PERMISSIONS.md).
        if (reduced(p)) responder.current ??= getPreferences().then((record) => record.viewer?.role === "responder", () => false);
        const reviewer = reduced(p) && responder.current !== null && await responder.current;
        // Someone with no access is on no team of it: the no-access screen is theirs, never this page
        // (contracts/routes.md, App surfaces). Replaced, so Back does not land here again.
        if (reduced(p) && p.access.level === "none" && !reviewer) {
          clearInterval(timer);
          if (live) window.location.replace(`#/no-access/${encodeURIComponent(id)}`);
          return;
        }
        // Below contributor every other read of the app is refused (contracts/routes.md, App teams), so none is made.
        const s = reduced(p) && !reviewer ? null : await getStatus(id);
        const c = reduced(p) || p.latest.package !== null ? null : (await listRuns(id, null)).runs.find((row) => row.check === true) ?? null;
        if (live) { setProject(p); setStatus(s); setChecked(c); setFailure(null); }
      } catch (err) {
        if (!live) return;
        const gone = err instanceof ApiError && err.status === 404;
        if (gone) { clearInterval(timer); setProject(null); setStatus(null); }
        setFailure(gone ? `No app ${id}` : (err as Error).message);
      }
    };
    const timer = setInterval(() => void tick.current(), POLL_MS);
    void tick.current();
    return () => { live = false; clearInterval(timer); };
  }, [id]);

  // The findings are the newest run's, read when that run changes and again until it ends. Neither
  // run read speaks for the app: a 404 here is this run's failure line, never "No app".
  const fromCheck = pinned === null && (status?.last_run ?? null) === null ? checked : null;
  const lastRun = pinned ?? status?.last_run ?? fromCheck?.id ?? null;
  // The findings of the run before this one stay on the page, dimmed, until this one answers:
  // they are the last thing anyone knows about the app, and clearing them says less than that.
  // A reader below contributor reads nothing of the app but its project row, a pinned run included — but a
  // responder, who reads its runs to approve them (the status read answered them).
  const team = project !== null && !reduced(project);
  const reads = team || status !== null;
  useEffect(() => {
    if (lastRun === null || !reads) return;
    let live = true;
    const read = () => getRun(lastRun).then(
      ({ run: at }) => {
        if (!live) return;
        setRun(at);
        if (ended(at)) { setSeen(at); clearInterval(timer); }
      },
      (err: Error) => { if (live) setRun(err); },
    );
    const timer = setInterval(() => void read(), POLL_MS);
    void read();
    return () => { live = false; clearInterval(timer); };
  }, [id, lastRun, reads]);

  const prod = running(status?.envs.prod);
  const preview = running(status?.envs.preview);
  // The review the receipt is read for, as its own run read answers this reader: whether they may approve its blocking
  // findings, a package only its built app holds among them (#2610). Read here only when no other read of the page is of
  // it — the review shown, or what preview or production runs — so every run the page shows is read once.
  const promoteRun = status?.promote_run ?? null;
  const [promoteRead, setPromoteRead] = useState<{ id: string; approvable: boolean } | null>(null);
  const gateRuns = [preview?.gate_run, prod?.gate_run].filter((one): one is string => one !== undefined && one !== "").join(" ");
  useEffect(() => {
    if (gateRuns === "") return;
    let live = true;
    for (const one of new Set(gateRuns.split(" "))) {
      void getRun(one).then(({ run: at }) => { if (live) setEnvRuns((was) => ({ ...was, [one]: at })); }, (err: Error) => { if (live) setEnvRuns((was) => ({ ...was, [one]: err })); });
    }
    return () => { live = false; };
  }, [id, gateRuns, approvals]);
  const promoteElsewhere = promoteRun === null || promoteRun === lastRun || gateRuns.split(" ").includes(promoteRun);
  useEffect(() => {
    if (promoteRun === null || promoteElsewhere || !reads) return;
    let live = true;
    void getRun(promoteRun).then(({ run: at }) => { if (live) setPromoteRead({ id: at.id, approvable: at.approvable === true }); }, () => { if (live) setPromoteRead(null); });
    return () => { live = false; };
  }, [id, promoteRun, promoteElsewhere, reads]);

  const send = async (gateRun: string | null, anyway: boolean) => {
    if (promoting.current) return null;
    promoting.current = true;
    let landed: string | null = null;
    try {
      landed = (await promote(id, gateRun, anyway)).run?.id ?? "";
      await tick.current();
    } catch (err) {
      const refusal = err instanceof ApiError && err.status === 409 ? (err.body as { error?: string; receipt?: ReceiptRead }) : undefined;
      const receipt = refusal?.receipt;
      if (receipt) setStatus((at) => at && { ...at, receipt });
      else if (refusal?.error === "preview_answered") await tick.current(); // preview was deployed again: the page reads its answer, and the question offers the override
      else setFailure(said(err)); // run_in_progress, deleting: the server's sentence, never a receipt

    }
    promoting.current = false;
    return landed;
  };

  // The new run reaches the page the way every run does: the status read names it and the poll reads it.
  const review = async (again: string | null) => {
    if (starting.current) return;
    starting.current = true;
    try {
      if (again === null) await startReview(id);
      else await redeploy(id, again); // a version reviewed before is deployed again, which reviews it first
      setReviewFailure(null);
      await tick.current();
    } catch (err) {
      setReviewFailure(said(err));
    }
    starting.current = false;
  };

  const reading = run !== null && !(run instanceof Error) && run.id === lastRun ? run : null;
  const settled = reading !== null && ended(reading);
  const dimmed = lastRun !== null && !settled;
  const cards = lastRun === null ? [] : seen?.findings ?? []; // a settled read is already `seen`; none once no run is shown, as a check's once a version is current
  // A card is an entry (contracts/finding.md, One problem, one entry; screens.md): the findings the run read numbers
  // one entry are one card where the first of them stands, its approved rows kept in it, and every count counts entries.
  const byEntry = new Map<number, FindingRead[]>(); // each entry's findings, built once a render
  for (const one of cards) byEntry.set(one.entry, [...(byEntry.get(one.entry) ?? []), one]);
  const membersOf = (one: FindingRead) => byEntry.get(one.entry) ?? [one];
  // What stops the version first, under its own heading: a blocking finding nobody approved, a dependency's
  // card while any of its findings is. The rest, approved ones and advisories, after it, each group in the run's own order.
  const fix = cards.filter((one) => one.properties.canyon.blocks && membersOf(one).some((other) => other.approval === undefined));
  const rest = cards.filter((one) => !fix.includes(one));
  // The rest by policy, each group where its first finding stands, so seven advisories of one kind read
  // as one group with its count rather than seven cards in a row.
  const policyOf = (one: FindingRead) => one.properties.canyon.policy_name ?? one.properties.canyon.title;
  const groups = [...new Set(rest.map(policyOf))].map((name) => ({ name, cards: rest.filter((one) => policyOf(one) === name) }));
  // Each card once: an entry's where its first finding stands.
  const shown = (list: FindingRead[]) => list.filter((one) => membersOf(one)[0] === one);
  // Approving acts on the settled run shown — the newest, or a pinned one (#2242) — never a check's, a finding at a
  // time; the run is read again after, so the card says approved and the verdict counts it.
  // A finding blocking before production is the run read's own answer, as the receipt counts it (#2206).
  const atProduction = (one: FindingRead) => one.blocks_production === true;
  const reread = async (id: string) => {
    const { run: at } = await getRun(id);
    setRun(at);
    setSeen(at);
    setApprovals((n) => n + 1);
    await tick.current();
  };
  // A blocking finding nobody approved, on the settled run shown: approved there, by whoever its run read says may (#2204).
  const unapproved = (one: FindingRead) => fromCheck === null && settled && reading !== null && (one.properties.canyon.blocks || atProduction(one)) && one.approval === undefined;
  // Who approves, said once above the findings, to a reader the run read does not let approve them.
  // A push's, where the org asks its approvers to link GitHub and the reader linked none, says so and links to linking (#2342) —
  // only where the host can link one: elsewhere the route answers 409, so the line names where an admin sets it up, and
  // says neither until the viewer is read (#2351).
  const settings = admin ? <a className="text-link no-underline hover:underline" href="#/settings/github">Settings › GitHub</a> : "Settings › GitHub";
  const [cantLink, afterSettings] = S.pushApproversCantLink.split("<settings>");
  const whoApproves = reading?.approvable === true || !cards.some(unapproved) ? null
    : reading?.link_github === true
      ? <p className="text-md text-text-dim">{S.pushApproversLink.replace("<org>", () => status?.receipt.org ?? "Your org")} {viewer === null ? null : viewer.github_link
        ? <a className="text-link no-underline hover:underline" href="/api/v1/github/link">{AUTH.githubLink}</a>
        : <>{cantLink}{settings}{afterSettings}</>}</p>
      : <p className="text-md text-text-dim">{S.approvers}</p>;
  const approvable = (one: FindingRead) => reading === null || reading.approvable !== true || !unapproved(one) ? undefined : {
    org: status?.receipt.org ?? "",
    send: async (body: ApproveBody) => {
      await approve(reading.id, one.partialFingerprints["canyon/v1"], body);
      await reread(reading.id);
    },
  };
  // An entry's card approves each of its findings nobody has approved, with one reason: one rule on one line is one approval.
  const approvesEntry = (members: FindingRead[]) => {
    const open = members.filter((other) => approvable(other) !== undefined);
    if (open.length === 0 || reading === null) return undefined;
    return {
      org: status?.receipt.org ?? "",
      send: async (body: ApproveBody) => {
        try {
          for (const other of open) await approve(reading.id, other.partialFingerprints["canyon/v1"], body);
        } finally {
          await reread(reading.id); // what went through shows, though a later one failed
        }
      },
    };
  };
  // A dependency's card approves its findings nobody has approved together, two up to the route's 200.
  const together = (members: FindingRead[]) => {
    const group = members.filter((other) => approvable(other) !== undefined);
    const [one] = group;
    if (one === undefined || group.length < 2 || group.length > 200 || reading === null) return undefined;
    return {
      org: status?.receipt.org ?? "",
      group: { name: one.dependency!.name, version: one.dependency!.version, tool: one.tool ?? "", titles: group.map((other) => other.properties.canyon.title) },
      send: async (body: ApproveBody) => {
        await approveFindings(reading.id, { fingerprints: group.map((other) => other.partialFingerprints["canyon/v1"]), ...body });
        await reread(reading.id);
      },
    };
  };
  // A package only the built app holds is approved on the review the receipt is read for (contracts/routes.md, the approve
  // rows; #2610): its promote_run, read again after, so its go-live check says approved. Offered only where that run's read
  // lets this reader approve, as a finding's card is, so nobody sees an Approve the route refuses.
  const promoteAt = promoteRun === null ? undefined : promoteRun === lastRun ? reading ?? undefined : envRuns[promoteRun];
  const promoteApprovable = promoteRun !== null && (promoteAt !== undefined ? !(promoteAt instanceof Error) && promoteAt.approvable === true
    : promoteRead?.id === promoteRun && promoteRead.approvable);
  const approveBuiltApp = status === null || promoteRun === null || !promoteApprovable ? undefined : {
    org: status.receipt.org,
    send: async (fingerprints: string[], body: ApproveBody) => {
      await approveFindings(promoteRun, { fingerprints, ...body });
      setApprovals((n) => n + 1);
      await tick.current();
    },
  };
  const card = (one: FindingRead) => (one.dependency === undefined
    ? <Finding key={one.partialFingerprints["canyon/v1"]} finding={one} also={membersOf(one).slice(1)} app={id} approve={approvesEntry(membersOf(one))} atProduction={atProduction(one)} />
    : <DependencyCard key={one.partialFingerprints["canyon/v1"]} findings={membersOf(one)} app={id} approve={approvable} approveAll={together(membersOf(one))} atProduction={atProduction(one)} />);
  // The versions that matter, newest first, each once: the newest, then what preview runs, then what
  // production runs. A version's place on the four stages is the server's facts, never inferred.
  const rows = (app: AppRead, at: Status): Version[] => {
    const latest = app.latest;
    const seen = new Set<string>();
    const out: Version[] = [];
    const checks = (): Cell => {
      const { set, met } = goLive(at.receipt);
      if (at.receipt.waiting !== undefined || set === 0) return NONE; // nothing checked is never "all met"
      const sub = `${met} of ${set} checks`;
      if (at.receipt.blocked_by.length > 0) return { mark: "stop", text: "Not met", sub };
      return met === set ? { mark: "pass", text: "All met", sub } : { mark: "wait", text: "Met, with advisories", sub };
    };
    // What runs there now, never what the last deploy recorded (contracts/routes.md, envs.<env>.state), as
    // a label: why it stopped, where it answers and who put it there are its card's, above.
    const live = (env: Env): Cell => (env.state === "stopped" ? { mark: "stop", text: "Stopped" } : env.state === "restarting" ? { mark: "stop", text: "Keeps restarting" } : env.state === "unknown" ? { mark: "wait", text: "Couldn't check" } : { mark: "live", text: "Running" });
    // Only a version on its way: what production runs is its card's, and has no row here.
    const add = (pkg: string, label: string | undefined, who: Version["who"], review: Cell, broke?: Cell) => {
      // Nor the version a hold stopped, which production ran: lifting the hold starts it again, it is on its way nowhere (#2723).
      if (seen.has(pkg) || prod?.package === pkg || at.hold?.version === version(pkg, label)) return;
      seen.add(pkg);
      const inPreview = preview?.package === pkg;
      const pr = inPreview ? preview.pull_request : undefined; // a pull request's version: never promoted (#1947)
      const stopped = review.mark === "stop";
      out.push({
        key: pkg,
        name: pr === undefined ? version(pkg, label) : pullRequestName(pr),
        who,
        review,
        preview: inPreview ? live(preview) : broke !== undefined ? broke : stopped ? { mark: "none", text: "Never reached preview" } : NONE,
        checks: inPreview && pr === undefined ? checks() : NONE,
        production: !inPreview ? NONE : pr !== undefined ? { mark: "none", text: "Merge it first" } : { mark: "none", text: at.receipt.waiting === undefined && at.receipt.blocked_by.length === 0 ? "Ready to promote" : "Not yet" },
      });
    };
    if (latest.package !== null) {
      const { word, tone } = reviewOf(latest.stage, app.gate_state);
      const ours = pinned === null && reading !== null && reading.package === latest.package;
      const checks = steps.filter((step) => step.kind === "check");
      const count = ours && reading.state === "blocked" && blockingEntries(reading) > 0 ? `${blockingEntries(reading)} blocking ${blockingEntries(reading) === 1 ? "finding" : "findings"}`
        : ours && !ended(reading) && checks.length > 0 ? `${checks.filter((step) => step.status !== "queued" && step.status !== "running").length} of ${checks.length}` : undefined;
      // A build or a start that failed after the review passed: the review is Passed, and the step that
      // broke says so where preview would be, in words.
      const after = pinned === null && reading !== null && reading.package === latest.package && failedAfterReview(reading) ? reading : null;
      // Every blocking finding approved: approved past review, as the headline says, never Blocked.
      const past = ours && reading.state === "blocked" && approved(reading) !== null ? cleared(reading) : null;
      add(latest.package, latest.label, <Age at={latest.since} long />, after !== null ? cleared(after) : past ?? { mark: MARK[tone], text: word, sub: count },
        after === null ? undefined : { mark: "stop", text: STEP[after.failure_reason!] ?? "Build failed" });
    }
    if (preview !== null) add(preview.package, preview.label, <><Actor actor={preview.deployed_by} /> · <Age at={preview.deployed_at} long /></>, cleared(envRuns[preview.gate_run]));
    return out;
  };
  // The newest version, named, for the review section's heading and verdict.
  const newest = project !== null && !reduced(project) && project.latest.package !== null ? version(project.latest.package, project.latest.label) : null;
  const stopped = project !== null && !reduced(project) && project.latest.stage === "blocked";
  const named = (pkg: string) => (project !== null && !reduced(project) && pkg === project.latest.package ? project.latest.label : pkg === preview?.package ? preview.label : pkg === prod?.package ? prod.label : undefined);
  // A pinned run, or a check's, is not the newest version's: its own verdict says how it ended, under Review.
  const other = reading !== null && (pinned !== null || fromCheck !== null) ? reading : null;
  // The run a link pins, as the runs list has it (#2696): its label, and whether it is a check's or a pull request's,
  // none of which its run read carries. Read from the newest page on until it is found; unread, it is named by its package.
  const [pinnedRow, setPinnedRow] = useState<RunRow | null>(null);
  useEffect(() => {
    if (pinned === null || !reads) return;
    let live = true;
    void (async () => {
      let cursor: string | null = null;
      do {
        const page: { runs: RunRow[]; next: string | null } = await listRuns(id, cursor);
        const row = page.runs.find((one) => one.id === pinned);
        if (row !== undefined) { if (live) setPinnedRow(row); return; }
        cursor = page.next;
      } while (cursor !== null);
    })().catch(() => {});
    return () => { live = false; };
  }, [id, pinned, reads]);
  const pinnedOwn = pinnedRow !== null && pinnedRow.id === pinned ? pinnedRow : null;
  // A pinned run's version by its name: a pull request's by its pull request, any other by its label, else its id.
  const pinnedName = pinnedOwn === null || pinnedOwn.check === true ? null : pinnedOwn.pull_request !== undefined ? pullRequestName(pinnedOwn.pull_request) : version(pinnedOwn.package, pinnedOwn.label);
  // The version the review section shows: a pinned run's own once it is read — a check's as a check — else the newest (#2696).
  const titled = pinned === null ? newest : pinnedOwn?.check === true ? "a check" : pinnedName ?? (reading === null ? null : version(reading.package, named(reading.package)));
  // The story of a fix (#2696): once the newest version's own review passed, the run before this version's runs — never
  // a check's or a pull request's — when it was blocked, and whether this version fixed it. Told only of the version
  // uploaded just before this one, and one production never ran: a rollback review reviews only a version production ran
  // (contracts/routes.md, rollback_to) and the runs list does not say which runs are one, so where one might be, nothing
  // is said. Fixed only where this review proves it (provesFixed), never where it left something unchecked.
  const passed = pinned === null && fromCheck === null && settled && reading.state === "clean" && project !== null && !reduced(project) && reading.package === project.latest.package ? reading : null;
  const ranThere = useRef<Status["earlier"]>(undefined); // what production ran before, as the newest status read says
  ranThere.current = status?.earlier;
  const [earlier, setEarlier] = useState<{ after: string; run: RunRow; fixed: boolean } | null>(null);
  useEffect(() => {
    if (passed === null) return;
    let live = true;
    void (async () => {
      const [{ runs }, { packages }] = await Promise.all([listRuns(id, null), listPackages(id, null)]);
      const at = runs.findIndex((row) => row.id === passed.id);
      const before = at < 0 ? undefined : runs.slice(at + 1).find((row) => row.package !== passed.package && row.check !== true && row.pull_request === undefined);
      const previous = packages[0]?.id === passed.package ? packages[1]?.id : undefined;
      const ran = ranThere.current;
      if (before === undefined || before.package !== previous || ran === undefined || ran.some((one) => one.package === before.package)) return;
      if (before.state !== "blocked" || before.approved === true || before.blocking === 0) return;
      let fixed = false;
      if (passed.not_checked !== true) {
        try {
          const [{ run: then }, { policies }] = await Promise.all([getRun(before.id), getPolicies()]);
          fixed = provesFixed(then.findings, passed.findings, policies, id);
        } catch { /* its findings or its rules unread: the block is said, never that this version fixed it */ }
      }
      if (live) setEarlier({ after: passed.id, run: before, fixed });
    })().catch(() => {}); // the runs or the versions unread: the review says the rest
    return () => { live = false; };
  }, [id, passed?.id]); // keyed by the run's id: read once a passing review, never again for a poll's new object of it
  const story = earlier !== null && passed !== null && earlier.after === passed.id && newest !== null ? earlier : null;
  // What broke after a review passed, read once: a build's cause, or a start's reason, leads the headline; its lines are open below.
  // The newest run as it goes: its checks, their places in line and their times (the redesign, "Reviewing, live").
  const going = reading !== null && !ended(reading);
  const steps = useProgress(reading === null || pinned !== null || !going || !team ? null : reading.id, going); // an ended run's story is what Canyon checked; its timeline is the team's
  const broke = useBroke(team && settled && reading !== null && failedAfterReview(reading) && reading.failure_reason !== "detect" ? reading : null);
  // The headline speaks of the newest version: a pinned run, or a check's, is shown below and never there.
  const band = project === null || reduced(project) || status === null ? null : headline({
    latest: project.latest, newest, neverReviewed: project.gate_state === "never_reviewed",
    run: pinned === null && fromCheck === null && reading !== null && reading.package === project.latest.package ? reading : null,
    preview, prod, previewName: preview === null ? null : preview.pull_request ? pullRequestName(preview.pull_request) : version(preview.package, preview.label), prodName: prod === null ? null : version(prod.package, prod.label), receipt: status.receipt, hold: status.hold,
    broke: broke === null || broke instanceof Error ? undefined : broke,
    progress: progressWords(steps), refused,
  });
  // What an empty preview says: where the newest version stopped, else how something gets there.
  const newestRun = reading !== null && project !== null && !reduced(project) && reading.package === project.latest.package ? reading : null;
  const emptyPreview = newest === null || newestRun === null ? "Deploy a version to run it here."
    : newestRun.state === "blocked" && approved(newestRun) === null ? `${newest} goes here once it passes review.`
    : newestRun.failure_reason === "build" ? `${newest} didn't build.` : newestRun.failure_reason === "start" ? `${newest} didn't stay up.` : "Deploy a version to run it here.";
  // Production carries how its review ended on its face (D5): a pass, or approved problems; nothing while unread.
  const prodRun = prod === null ? undefined : envRuns[prod.gate_run];
  const prodNote = prodRun === undefined || prodRun instanceof Error ? undefined
    : (prodRun.state === "clean" && !prodRun.not_checked ? S.provenance : prodRun.state === "blocked" && approved(prodRun) !== null ? S.provenanceApproved : "").replace("<version>", () => version(prod!.package, prod!.label)) || undefined;
  // What blocks, named by the version it blocks (the redesign: "What blocks 07128a"): the findings'
  // own run, which is the one before while a new review goes.
  const shownFix = seen === null ? "What blocks" : `What blocks ${pinnedName !== null && seen.package === pinnedOwn?.package ? pinnedName : version(seen.package, named(seen.package))}`;
  // What production will refuse of what preview calls, as the status read's need lists it: the go-live box and Promote say it
  // from its destinations, host names apart from what only a change to the code fixes, never from the rule's own line.
  const refuses = status?.needs?.find((need) => need.kind === "will_refuse");
  // Approving is on each finding's card: the headline's button brings the reader to the first one.
  const toApprove = () => {
    const button = [...document.querySelectorAll<HTMLButtonElement>('section[aria-label="Review"] button')].find((one) => one.textContent === "Approve with a reason");
    button?.scrollIntoView({ block: "center" });
    button?.focus();
  };
  const act = (one: Act | undefined, primary: boolean) => {
    if (one === undefined || project === null || reduced(project) || status === null) return null;
    const again = band?.primary === "review" && project.latest.stage !== "awaiting_review" && project.gate_state !== "never_reviewed" ? project.latest.package : null;
    switch (one) {
      case "review": return <Button key={one} variant={primary ? "primary" : "secondary"} onClick={() => void review(again)}>{`Review ${newest ?? "it"}${again === null ? "" : " again"}`}</Button>;
      case "approve": return reading?.approvable !== true ? null : <Button key={one} variant="ghost" label="Approve with a reason: go to the findings" onClick={toApprove}>Approve with a reason</Button>;
      case "cancel": return <Button key={one} variant="ghost" onClick={() => { if (reading !== null) void cancelRun(reading.id).then(() => tick.current(), (err) => setReviewFailure(said(err))); }}>Cancel review</Button>;
      case "open": return prod === null ? null : <a key={one} className="inline-flex h-[34px] items-center gap-1.5 rounded-[7px] bg-accent px-3 text-base font-semibold text-accent-ink no-underline hover:brightness-110" href={prod.url} target="_blank" rel="noreferrer">Open production<Icon name="external" size={14} /></a>;
      case "copy": return broke === null || broke instanceof Error || band === null ? null : <CopyForAgent key={one} app={id} title={band.title} text={band.text} broke={broke} />;
      case "outbound": return <a key={one} className="inline-flex h-[34px] items-center gap-1.5 rounded-[7px] bg-accent px-3 text-base font-semibold text-accent-ink no-underline hover:brightness-110" href={`#/apps/${id}/outbound`}>{S.openOutbound}</a>;
      case "promote": return <Promote key={one} id={id} pullRequest={preview?.pull_request !== undefined} shown={preview === null ? null : preview.pull_request ? pullRequestName(preview.pull_request) : version(preview.package, preview.label)} live={false} receipt={status.receipt} hold={status.hold} answered={preview?.answered} gateRun={status.promote_run} refuses={refuses} send={send} ended={() => void tick.current()} />;
    }
  };
  // Lift the hold, on the banner that says it, to whoever the fleet's row offers it (#2723): an admin, or a responder off
  // the app's team, whose read of it is reduced. The question is the row's, over the page, and the page reads again after.
  const held = project === null || status?.hold === undefined ? null : { ...project, hold: status.hold };
  // The reader's role as the rail knows it: on a host with sign-in off no viewer is read, and its one caller is an admin.
  const role = viewer?.role ?? (admin ? "admin" : null);
  const lift = held !== null && role !== null && lifts(role, held)
    ? <Button variant="secondary" label={`${S.liftHold} on ${held.name}`} onClick={() => setLifting({ app: held, kind: "lift", asked: Date.now() })}>{S.liftHold}</Button> : null;
  // What waits on a person leads the page (D15): the server's needs, most urgent first, with the headline where Headline's
  // leads puts it.
  // An unowned app whose receipt has owner.required neutral (sign-in off, the rule at its default) needs nobody to own
  // it: that need is not shown here, nor counted (design/EXPERIENCE.md). The fleet's tile counts it still.
  const ownerless = status?.receipt.rules.some((rule) => rule.id === "owner.required" && rule.unset === true) === true;
  const needs: Need[] = (status?.needs ?? []).filter((need) => !(ownerless && need.kind === "unowned"));
  const top = leads(band, needs);
  // A restarting environment the band doesn't show the lines of shows them on its card, to whoever may read its logs
  // (#2744), so the need's "Its last log lines, on its page" holds wherever the band stands.
  const untailed = (key: "preview" | "prod") => {
    const at = status?.envs[key];
    const mayRead = project !== null && !reduced(project) && (admin || project.access.level === "owner" || project.access.level === "contributor");
    return mayRead && at !== undefined && !waiting(at) && at.state === "restarting" && top.band?.tail !== key;
  };
  // What a running environment calls out to, on its card (D15): how many destinations, how many refused on their last call,
  // the Outbound card a click away; red while one of its requests is refused now (the refused tile's predicate).
  const calls = (key: "preview" | "prod") => {
    const at = status?.envs[key];
    const read = egress?.envs?.[key];
    const rows = read?.destinations;
    if (at === undefined || waiting(at) || at.state !== "running" || !Array.isArray(rows) || rows.length === 0) return undefined;
    const refusedLast = rows.filter((row) => row.last_outcome === "refused").length;
    const now = Array.isArray(read?.requests) && read.requests.some((one) => one.state === "pending" && one.refusing === true);
    const line = (rows.length === 1 ? S.callsOutOne : S.callsOut.replace("<n>", () => String(rows.length)))
      + (refusedLast > 0 ? S.callsOutRefused.replace("<n>", () => String(refusedLast)) : S.callsOutNoneRefused);
    return <button type="button" className={`text-left hover:underline ${now ? "text-critical" : "text-link"}`} onClick={() => document.querySelector<HTMLElement>('section[aria-label="Outbound"]')?.scrollIntoView?.({ block: "start" })}>{line}</button>;
  };
  const me = viewer ?? { display: "", role: admin ? "admin" as const : "member" as const, github_logins: [], github_link: false };
  // The link a refusal carries (#/apps/<app>/outbound) opens on the band with its Allow ready, where there is one to press.
  const allowHere = outbound && needs.some((need) => (need.kind === "refused" || need.kind === "will_refuse") && need.you && need.held !== true);
  // What Canyon checked: beside the go-live checks while the review has findings to show, and under the review's
  // heading when it has none — there it is the review's content, never an empty column beside the checks (#2696).
  const gate = settled && reading !== null ? <ReviewGate run={reading.id} findings={reading.findings} /> : null;
  const quiet = cards.length === 0;
  const beside = preview !== null || (gate !== null && !quiet);
  // The go-live box's Promote, drawn for what preview runs when it isn't the newest version or production is held, but
  // for the version a hold stopped; a waiting receipt's line is then its disabled reason, said once (#2774).
  const boxPromote = preview !== null && status !== null && project !== null && !reduced(project) && preview.pull_request === undefined
    && !(status.hold !== undefined && status.hold.version === version(preview.package, preview.label))
    && (preview.package !== project.latest.package || status.receipt.held !== undefined);
  const saidByPromote = boxPromote && status!.receipt.waiting !== undefined && promoteReason(status!.receipt, status!.hold, refuses, false) === status!.receipt.waiting;
  return (
    <div className="flex min-h-screen items-start max-[860px]:block">
      <Rail current="fleet" />
      <main className="mx-auto grid max-w-[1280px] min-w-0 flex-1 grid-cols-[minmax(0,1fr)] content-start gap-6 px-8 pt-7 pb-24 max-[860px]:px-4 max-[860px]:pt-4">
        <header className="grid gap-1.5">
          <nav aria-label="Breadcrumb" className="flex items-center gap-1.5 text-md text-text-faint">
            <a className="text-text-faint no-underline hover:underline" href="#/fleet">Fleet</a>
            <Icon name="chevron" size={14} />
            <span aria-current="page">{project?.name ?? id}</span>
          </nav>
          {/* the actions stay top-right whatever grows under the title; only a phone stacks them */}
          <div className="flex items-start justify-between gap-3 max-[860px]:flex-wrap">
            <div className="grid min-w-0 flex-1 gap-1">
              <h1 className="text-title font-semibold tracking-tight"><AppName app={project ?? { id, name: id }} /></h1>
              {project !== null && (
                <p className="text-md text-text-dim">
                  {[project.summary || S.noDescription, reduced(project) ? null : project.stack].filter(Boolean).join(" · ")}
                  {" · "}
                  {project.owners.length === 0 ? <span className="text-text-faint">No owner</span> : (
                    <>Owner {project.owners.map((owner, at) => (
                      <span key={owner.subject}>{at > 0 && ", "}<a className="font-medium text-text no-underline hover:underline" href={ownerHref(owner.subject)}>{actorName(owner)}</a></span>
                    ))}</>
                  )}
                </p>
              )}
              {/* Who can open it, under the name (the owner's decision 1, 2026-11-08): the card's line, its card a click away, and
                  Share to whoever manages it. Nothing is shared where nobody signs in. */}
              {project !== null && !reduced(project) && auth === "oidc" && status !== null && (
                <p className="text-md text-text-dim">
                  {sharedLine(project.access, status.receipt.org)}{" · "}
                  <button type="button" className="text-link hover:underline" onClick={() => setAccessAsked({ at: Date.now(), share: false })}>{S.whoCanOpen}</button>
                  {project.access.requests !== undefined && <>{" · "}<button type="button" className="text-link hover:underline" aria-label={`${S.share} ${project.name}`} onClick={() => setAccessAsked({ at: Date.now(), share: true })}>{S.share}</button></>}
                </p>
              )}
              {project !== null && !reduced(project) && project.repository !== undefined && <AppSource id={id} link={project.repository} autoDeploy={project.auto_deploy ?? false} owner={project.access.level === "owner"} changed={() => tick.current()} />}
            </div>
            {project !== null && !reduced(project) && (
              <div className="flex shrink-0 flex-wrap items-center gap-2">
                {preview !== null && !down(preview) && <a className="inline-flex h-[34px] items-center gap-1.5 rounded-[7px] border border-border-strong bg-surface px-3 text-base font-medium text-text no-underline hover:bg-surface-2" href={preview.url} target="_blank" rel="noreferrer">Open preview<Icon name="external" size={14} /></a>}
                {/* Delete is managing the app, its owners' and admins' (PERMISSIONS.md): the read answers
                    `requests` to them alone, so its presence is what says the reader may. */}
                <More id={id} name={project.name} preview={preview !== null} prod={prod !== null} manage={project.access.requests !== undefined} done={() => tick.current()} />
              </div>
            )}
          </div>
        </header>
        {failure !== null && <p className="text-base text-critical">{failure}</p>}
        {/* The question names the hold it read; the page's newest read, held or not, says when someone else changed it (#2723). */}
        {lifting !== null && <HoldDialog acting={lifting} now={project === null ? undefined : { ...project, hold: status?.hold }} close={() => { setLifting(null); void tick.current(); }} />}
        {/* The body waits on the app's row and its status read, one after the other: until they answer, it says so (#2561). */}
        {project === null && failure === null && <p role="status" className="text-base text-text-dim">{S.appReading}</p>}
        {project !== null && reduced(project) && (
          <>
            <Headline band={viewerBand(project.envs, status === null ? `${S.readOnly} ${S.readOnlyUse}` : S.responderAccess, project.access.level !== "none")}>
              {asked === null ? <Button variant="secondary" onClick={() => void askForAccess(id).then(() => setAsked(true), (err: unknown) => setAsked(said(err)))}>{project.access.level === "viewer" ? S.askToJoin : S.askForAccess}</Button>
                : asked === true ? <p role="status" className="text-md text-ok">{project.owners[0] !== undefined ? S.asked.replace("<owner>", () => actorName(project.owners[0]!))
                  : project.owner_groups[0] !== undefined ? S.askedGroup.replace("<group>", () => project.owner_groups[0]!) : S.askedNoOwner}</p>
                : <p className="text-md text-critical">{asked}</p>}
            </Headline>
            {/* A responder reads the hold above production; a read-only member's read carries none. */}
            {status?.hold !== undefined && <Headline band={heldBand(status.hold)}>{lift ?? undefined}</Headline>}
            <div className="grid grid-cols-2 gap-3 max-[620px]:grid-cols-1">
              {/* A responder with no grant reads nothing of preview: the card says so, never that it is empty. */}
              {project.access.level === "none" ? <EnvCard name="Preview" env={undefined} blank="Not shown to you" empty="A security responder reads an app's production, never its preview." />
                : <EnvCard name="Preview" env={project.envs?.preview} empty="Nothing is in preview." />}
              {/* A read-only member's read never says production is held (design/PERMISSIONS.md): its card says nothing runs, which
                  is true either way, never that nothing is there. */}
              <EnvCard name="Production" env={project.envs?.prod} {...(status?.hold !== undefined ? { blank: S.heldSince.replace("<date>", () => status.hold!.at.slice(0, 10)), empty: "" }
                : status === null ? { empty: "" } : { empty: "Nothing is in production." })} />
            </div>
            {status === null && (
              <section aria-label={S.whoLooksAfter} className="grid gap-1">
                <h2 className={H2}>{S.whoLooksAfter}</h2>
                <p className="text-base text-text-dim">{project.owners[0] !== undefined ? S.whoLooksAfterOwner.replace("<owner>", () => actorName(project.owners[0]!))
                  : project.owner_groups?.[0] !== undefined ? S.whoLooksAfterGroup.replace("<group>", () => project.owner_groups[0]!) : S.whoLooksAfterNone}</p>
              </section>
            )}
            {/* A responder's: the newest review's findings, to approve them, and what stops production (#2204). */}
            {status !== null && (
              <div className="grid grid-cols-[minmax(0,1fr)_360px] items-start gap-7 max-[1100px]:grid-cols-1">
                <section aria-label="Review" className="grid min-w-0 gap-2.5">
                  <h2 className={H2}>Review</h2>
                  {run instanceof Error && <p className="text-base text-critical">{run.message}</p>}
                  {fix.length > 0 && <h3 className={H2}>{shownFix}<span className="ml-2 font-normal text-text-faint">{shown(fix).length}</span></h3>}
                  {whoApproves}
                  {shown(fix).map(card)}
                  {shown(rest).map(card)}
                </section>
                {/* A responder with no grant reads no preview, but the go-live checks, to approve what only the built app holds (#2610). */}
                {(preview !== null ? preview.pull_request === undefined : status.receipt.rules.some((rule) => rule.built_app !== undefined)) && (
                  <aside aria-label="Promote" className="grid gap-2.5 rounded-[10px] border border-border bg-surface p-4">
                    <Receipt receipt={status.receipt} patch={preview?.patch} approveBuiltApp={approveBuiltApp} />
                  </aside>
                )}
              </div>
            )}
          </>
        )}
        {project !== null && !reduced(project) && status !== null && (
          <>
            {(() => {
              const headed = top.band === null ? null : (
                <Headline key="headline" band={top.band} app={admin || project.access.level === "owner" || project.access.level === "contributor" ? id : undefined}>
                  {top.band.primary === undefined && top.band.secondary === undefined && (lift === null || status.receipt.held === undefined) ? undefined
                    : <>{status.receipt.held !== undefined && lift}{act(top.band.secondary, false)}{act(top.band.primary, true)}</>}
                </Headline>
              );
              const listed = top.needs.length === 0 ? null : (
                <NeedsBand key="needs" app={{ ...project, last_run: status.last_run, needs, hold: status.hold } as FleetRow} needs={top.needs} viewer={me} signedIn={auth === "oidc"} here focus={allowHere}
                  lift={() => { if (held !== null) setLifting({ app: held, kind: "lift", asked: Date.now() }); }} changed={() => void tick.current()} />
              );
              return top.first ? [headed, listed] : [listed, headed]; // keyed: a band that moves is the same band, never drawn anew
            })()}
            {reviewFailure !== null && <p className="text-base text-critical">{reviewFailure}</p>}
            {/* What runs where, at a glance; a release your platform has not taken yet is said on its card (HAND-OFF §4: never silent). */}
            <div className="grid grid-cols-2 gap-3 max-[620px]:grid-cols-1">
              <EnvCard name="Preview" env={status.envs.preview} empty={emptyPreview} calls={calls("preview")} who={preview?.deployed_by === undefined || preview.deployed_by === null ? undefined : <Actor actor={preview.deployed_by} />}>
                {untailed("preview") && <LastLines app={id} env="preview" />}
              </EnvCard>
              {/* Held, it says who held it and who lets it run again: Promote is disabled, so it never says to promote. */}
              <EnvCard name="Production" env={status.envs.prod} note={prodNote} preview={status.envs.preview} calls={calls("prod")}
                {...(status.hold === undefined ? { empty: "Promote a version from preview to run it here." } : { blank: S.heldSince.replace("<date>", () => status.hold!.at.slice(0, 10)), empty: "" })}
                who={prod?.promoted_by === undefined || prod.promoted_by === null ? undefined : <>promoted by <Actor actor={prod.promoted_by} /></>}>
                {untailed("prod") && <LastLines app={id} env="prod" />}
                {/* a host from before earlier[] answers none */}
                {status.earlier !== undefined && status.earlier.length > 0 && <EarlierVersions id={id} earlier={status.earlier} prod={prod} hold={status.hold} ended={() => void tick.current()} />}
              </EnvCard>
            </div>
            {rows(project, status).length > 0 && <Versions rows={rows(project, status)} />}
            {/* Why the newest version stopped, beside what it takes to ship what preview runs: the
                builder sees what to fix and the approver whether it can ship, together (spec A2). */}
            <div className={`grid items-start gap-7 max-[1100px]:grid-cols-1 ${beside ? "grid-cols-[minmax(0,1fr)_360px]" : "grid-cols-[minmax(0,1fr)]"}`}>
            <section aria-label="Review" aria-busy={dimmed} className="grid min-w-0 gap-2.5">
              {!(stopped && fix.length > 0) && (
                <h2 className={H2}>
                  {titled === null ? "Review" : `Review of ${titled}`}
                  {/* A review with nothing blocking says so on its heading, so an empty section never reads as missing. */}
                  {/* A finding that blocks production only is never "nothing blocking": said apart, the advisories after it (#2723). */}
                  {settled && reading.state === "clean" && (() => {
                    const atProd = shown(rest).filter((one) => membersOf(one).some((other) => atProduction(other) && other.approval === undefined)).length;
                    const others = shown(rest).length - atProd;
                    const advisories = `${others} ${others === 1 ? "advisory" : "advisories"}`;
                    // Beside what blocks production, "no advisories" is no news: it's said only with nothing blocking.
                    const blocks = S.blocksProduction.replace("<n>", () => String(atProd));
                    return <span className="ml-2 text-md font-normal text-text-faint">{atProd > 0 ? (others === 0 ? blocks : `${blocks} · ${advisories}`) : `nothing blocking · ${others === 0 ? "no advisories" : advisories}`}</span>;
                  })()}
                </h2>
              )}
              {pinned !== null && <p className="text-base text-text-dim">{`Run ${pinned} · `}<a className="text-link no-underline hover:underline" href={`#/apps/${id}`}>the newest run →</a></p>}
              {/* The version before this one was blocked: said with its link, fixed only where this review proves it, and
                  never passed where this review left something unchecked (D5). */}
              {story !== null && newest !== null && passed !== null && (() => {
                const unchecked = passed.not_checked === true;
                const fill = (words: string) => words.replace("<earlier>", () => version(story.run.package, story.run.label)).replace("<version>", () => newest).replace("<n>", () => String(story.run.blocking));
                const one = story.run.blocking === 1;
                const body = story.fixed ? (one ? S.earlierFixedOne : S.earlierFixedMany) : `${one ? S.earlierOne : S.earlierMany} ${unchecked ? S.earlierNotCheckedTail : S.earlierPassedTail}`;
                return (
                  <div className="grid grid-cols-[18px_minmax(0,1fr)] gap-x-2 gap-y-0.5 text-md">
                    <span className={`mt-0.5 ${story.fixed ? "text-ok" : "text-text-faint"}`}><Icon name={unchecked ? "never" : "passed"} size={16} /></span>
                    <p className="font-medium">{fill(story.fixed ? S.earlierFixed : unchecked ? S.earlierNotChecked : S.earlierPassed)}</p>
                    <p className="col-start-2 text-text-dim">
                      {fill(body)}
                      {" "}<a className="text-link no-underline hover:underline" href={`#/apps/${id}?run=${encodeURIComponent(story.run.id)}`}>{fill(S.earlierSee)}</a>
                    </p>
                  </div>
                );
              })()}
              {run instanceof Error && <p className="text-base text-critical">{run.message}</p>}
              {going && pinned === null && <Progress steps={steps} />}
              {reading !== null && (
                <>
                  {fromCheck !== null && <p className="text-md text-text-dim">From a check — not deployed yet · <Age at={fromCheck.started_at} long /></p>}
                  {other !== null && <p className={`text-base font-semibold ${TONE[other.state]}`}>{verdict(other, pinnedName ?? version(other.package, named(other.package)))}</p>}
                  {settled && reading.fix !== undefined && <Missing run={reading.id} fix={reading.fix} />}
                  {broke !== null && <BuildFailure run={reading} broke={broke} />}
                </>
              )}
              <div className={`grid grid-cols-[minmax(0,1fr)] gap-3 transition-opacity duration-(--ds-motion-duration-fast) ${dimmed ? "opacity-45" : ""}`}>
                {fix.length > 0 && <h3 className={H2}>{shownFix}<span className="ml-2 font-normal text-text-faint">{shown(fix).length}</span></h3>}
                {whoApproves}
                {shown(fix).map(card)}
                {/* Beside what blocks, what doesn't waits behind a click, its count on the summary; alone, it is the review's content. */}
                {(() => {
                  const advisories = groups.map((group) => (
                    <section key={group.name} aria-label={group.name} className="grid grid-cols-[minmax(0,1fr)] gap-3">
                      {shown(rest).length > 1 && <h4 className="text-md font-medium text-text-dim">{`${group.name} · ${shown(group.cards).length}`}</h4>}
                      {shown(group.cards).map(card)}
                    </section>
                  ));
                  return fix.length > 0 && rest.length > 0 ? (
                    <details className="mt-2 grid gap-3">
                      <summary className="cursor-pointer"><h3 className={`${H2} inline`}>Doesn't block<span className="ml-2 text-md font-normal text-text-faint">{`${shown(rest).length} ${shown(rest).length === 1 ? "finding" : "findings"}`}</span></h3></summary>
                      {advisories}
                    </details>
                  ) : advisories;
                })()}
              </div>
              {quiet && gate}
            </section>
              {beside && <div className="grid content-start gap-4">
                {/* The go-live checks while preview runs something: a promotion that lands keeps its lines here to the end. */}
                {preview !== null && (
                  <aside id="promote" tabIndex={-1} aria-label="Promote" className="grid gap-2.5 rounded-[10px] border border-border bg-surface p-4">
                    <h2 className={H2}>{`Go-live checks for ${preview.pull_request ? pullRequestName(preview.pull_request) : version(preview.package, preview.label)}`}</h2>
                    {/* a pull request's version is never promoted (#1947): the receipt is main's, and says nothing of it */}
                    {preview.pull_request === undefined && !saidByPromote && <Receipt receipt={status.receipt} patch={preview.patch} approveBuiltApp={approveBuiltApp} refuses={refuses} />}
                    {/* What preview runs, when it isn't the newest version: the headline's promote is the newest's alone.
                        While production is held the headline is the hold's, so Promote is here, disabled with it (D6);
                        but for the version the hold stopped, which lifting it starts again: that is said instead (#2723). */}
                    {status.hold !== undefined && status.hold.version === version(preview.package, preview.label)
                      ? <p className="text-md text-text-dim">{S.heldVersion.replace("<version>", () => status.hold!.version!)}</p>
                      : (preview.package !== project.latest.package || status.receipt.held !== undefined) && (
                      <Promote id={id} pullRequest={preview.pull_request !== undefined} shown={preview.pull_request ? pullRequestName(preview.pull_request) : version(preview.package, preview.label)} live={prod?.package === preview.package && !down(prod)} receipt={status.receipt} hold={status.hold} answered={preview?.answered} gateRun={status.promote_run} refuses={refuses} send={send} ended={() => void tick.current()} />
                    )}
                  </aside>
                )}
                {!quiet && gate}
              </div>}
            </div>
            <Access id={id} access={project.access} owners={project.owners} groups={project.owner_groups} org={status.receipt.org} auth={auth} asked={accessAsked} />
            {/* What the org's platform provided: the single read's resources[], a team member's alone (#2432). */}
            <Provided resources={project.resources} />
            {/* What the app calls out to: a full read is a contributor's and up (design/PERMISSIONS.md, Outbound). */}
            <Outbound id={id} envs={status.envs} level={admin ? "admin" : project.access.level === "owner" ? "owner" : "contributor"} shown={outbound && !allowHere}
              owner={project.owners[0] !== undefined ? { name: actorName(project.owners[0]) } : project.owner_groups?.[0] !== undefined ? { group: project.owner_groups[0] } : undefined}
              told={(read) => { const now = prodPending(read); setRefused((was) => (was.join(" ") === now.join(" ") ? was : now)); setEgress(read); }} />
            {/* What Canyon adds under the app, as its known-vulnerable check reads it: a contributor's and up (design/PERMISSIONS.md). */}
            <SystemLibraries id={id} envs={status.envs} />
            <AppSections project={project} envs={status.envs} refreshed={() => void tick.current()} />
          </>
        )}
      </main>
    </div>
  );
}

// The ⋯ in the header: taking an environment offline is rare and risky, so it is not in the reading
// flow (the redesign: "Take offline and Delete live in the ⋯ menu"). Each item asks before it acts.
// Renaming and deleting are managing the app, its owners' and admins' (manage).
function More({ id, name, preview, prod, manage, done }: { id: string; name: string; preview: boolean; prod: boolean; manage: boolean; done: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  const [env, setEnv] = useState<"preview" | "prod" | "rename" | "delete" | null>(null);
  const button = useRef<HTMLButtonElement>(null);
  // An environment gone from under an open question takes the question with it.
  if ((env === "preview" && !preview) || (env === "prod" && !prod) || ((env === "rename" || env === "delete") && !manage)) setEnv(null);
  if (!preview && !prod && !manage) return null;
  const close = () => { setOpen(false); setEnv(null); button.current?.focus(); };
  return (
    <div className="relative" onKeyDown={(event) => { if (event.key === "Escape") close(); }}>
      <button type="button" ref={button} aria-label="More actions" aria-expanded={open} onClick={() => { setOpen(!open); setEnv(null); }}
        className={`grid size-[34px] place-items-center rounded-[7px] border border-border-strong hover:bg-surface-2 ${open || env !== null ? "bg-surface-2 text-text" : "bg-surface text-text-dim"}`}>
        <Icon name="more" />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 z-10 mt-1.5 grid w-[220px] max-[520px]:right-auto max-[520px]:left-0 rounded-[10px] border border-border-strong bg-surface p-1.5 shadow-overlay">
          {preview && <button type="button" role="menuitem" className="rounded-[7px] px-2.5 py-2 text-left text-base hover:bg-surface-2" onClick={() => { setEnv("preview"); setOpen(false); }}>Take preview offline</button>}
          {prod && <button type="button" role="menuitem" className="rounded-[7px] px-2.5 py-2 text-left text-base hover:bg-surface-2" onClick={() => { setEnv("prod"); setOpen(false); }}>Take production offline</button>}
          {(preview || prod) && manage && <div role="separator" className="my-1 h-px bg-border" />}
          {manage && <button type="button" role="menuitem" className="rounded-[7px] px-2.5 py-2 text-left text-base hover:bg-surface-2" onClick={() => { setEnv("rename"); setOpen(false); }}>{RENAME.title}</button>}
          {manage && <button type="button" role="menuitem" className="rounded-[7px] px-2.5 py-2 text-left text-base text-critical hover:bg-surface-2" onClick={() => { setEnv("delete"); setOpen(false); }}>{TEARDOWN.deleteTitle}</button>}
        </div>
      )}
      {env !== null && (
        <div className="absolute right-0 z-10 mt-1.5 w-[360px] rounded-[10px] border border-border-strong bg-surface p-3.5 shadow-overlay max-[520px]:fixed max-[520px]:inset-x-4 max-[520px]:w-auto">
          {env === "delete" ? <DeleteApp id={id} cancel={close} />
            : env === "rename" ? <RenameApp id={id} name={name} done={async () => { await done(); close(); }} cancel={close} />
            : <TakeOffline key={env} id={id} env={env} done={async () => { await done(); setEnv(null); }} cancel={close} />}
        </div>
      )}
    </div>
  );
}
