// The shapes contracts/routes.md pins for the reads the dashboard makes, the calls it makes, and how a version is named on screen.
// The bundle carries no configuration, no org data and no credentials: it reads what it shows.
import type { StageKey } from "./ui/Stage";

export interface Actor { kind: "person" | "agent" | "canyon"; subject: string; display: string }
// The release an environment waits on (contracts/routes.md, envs.<env>.pending); `missing` what the platform's last
// deployed report on it left out (#2452); `via` how it reaches the platform, and `pull_request` the deploy repo's pull
// request it waits on (#2470), at contributor and above as the rest of pending is.
export interface Pending { release: string; event: "deploy" | "remove"; sent_at: string; state: "sent" | "accepted" | "undelivered" | "refused"; via: "webhook" | "repo"; missing?: string[]; pull_request?: { url: string; number: number } }
// `state` is whether it runs now, on the status read and the fleet row alone, and absent where the org runs it (contracts/routes.md, envs.<env>.state).
// The pull request a package came from (#1947): on a run of it, and on preview while preview runs it.
export interface PullRequest { number: number; branch: string }
// An environment's patch state (contracts/routes.md, envs.<env>.patch; patch.schema.json's env_patch), at contributor and
// above: wait and could_not_rescan on the status read alone; `approval` the open patch approval's code while asked, the
// denied one's while denied (#2601).
export type PatchState = "none" | "planned" | "wait" | "patched_preview" | "asked" | "patched" | "reverted" | "held" | "denied" | "could_not_rescan";
export interface EnvPatch { state: PatchState; since: string; identity?: string; run?: string; approval?: string; why?: string }
export interface Env { patch?: EnvPatch; pull_request?: PullRequest; url: string; package: string; label?: string; image_digest: string; deployed_at: string; gate_run: string; deployed_by: Actor | null; promoted_by?: Actor | null; run_by?: "canyon" | "org"; pending?: Pending; seen_at?: string; state?: "running" | "restarting" | "stopped" | "unknown"; stopped?: { exit_code: number; said: string }; restarting?: { restarts: number; exit_code?: number; said?: string }; answered?: { status: number; said: string } | { error: string } }
// An environment waiting on its first release (release.schema.json's env_pending): nothing runs there yet.
export interface Waiting { run_by: "org"; pending: Pending }
export const waiting = (env: Env | Waiting): env is Waiting => !("url" in env);
export interface Latest { package: string | null; label?: string; stage: StageKey; since: string }
// `access` on a whole row (contracts/routes.md, the project read): the caller's own level and how far
// the app is shared. `requests` is answered to its owners and admins only, so its presence is what
// says the caller manages the app's access.
export type Level = "owner" | "contributor" | "viewer";
// Production held by a responder or an admin (contracts/routes.md, The hold): who, when, why, and the
// version it ran then, null when nothing. Read by the team, responders and admins; never on a reduced row but a responder's.
export interface Hold { by: Actor; at: string; reason: string; version: string | null }
export interface TeamAccess { level: Level | "none"; everyone: boolean; people: number; groups: number; requests?: number }
export interface App { id: string; name: string; owners: Actor[]; owner_groups: string[]; summary: string; claimable: boolean; access: TeamAccess; repository?: RepoLink | null; auto_deploy?: boolean; resources?: ProjectResource[]; stack?: string; gate_state: "never_reviewed" | "pending" | "running" | "clean" | "blocked" | "failed"; envs: { preview?: Env | Waiting; prod?: Env | Waiting }; latest: Latest; hold?: Hold }
// Below contributor the project read, the list and each fleet row are this and nothing else
// (contracts/routes.md, App teams): the app's name, owners and summary, and the caller's own level —
// and at viewer `envs`, where the app runs and whether it does, so a read-only member can open it.
// Every full row carries `latest` and a reduced one never does, so that key tells the two apart.
// Both say whether the reader's claim would be accepted now (`claimable`): Claim is offered by it alone.
// `owners` are people; `owner_groups` the groups that own it, whom a reader asks where no person does.
// `seen_at` is when a pod the org runs last checked in (#2452): never beside `state`, which is Canyon's.
export interface ViewerEnv { url: string; state?: Env["state"]; seen_at?: string }
export interface Reduced { id: string; name: string; owners: Actor[]; owner_groups: string[]; summary: string; claimable: boolean; access: { level: "viewer" | "none" }; envs?: { preview?: ViewerEnv; prod?: ViewerEnv }; hold?: Hold; needs?: Need[] }
export const reduced = (row: App | Reduced): row is Reduced => !("latest" in row);
export type TileKey = "secrets_exposed" | "blocked" | "approved" | "never_reviewed" | "unowned" | "failed" | "refused";
// What needs a person (contracts/routes.md; fleet.schema.json's need): an app's, in the server's order. A kind this
// dashboard doesn't know is drawn as something that needs a person, never left out.
export type NeedKind = "held" | "down" | "refused" | "refused_code" | "secret" | "approval" | "blocked" | "unchecked" | "blocked_prod" | "will_refuse"
  | "patch" | "access" | "unowned" | "never_reviewed" | "recreate" | "notify_failing" | "licence" | "patching_unlicensed";
export interface Need {
  kind: string; env: "preview" | "prod" | null; rank: number; since: string | null; you: boolean; ask: { display: string; as: "admin" }[];
  state?: "stopped" | "restarting"; destination?: string; port?: number; refusals?: number; last_refused_at?: string; other_calls?: number; held?: boolean;
  why?: "address" | "port"; calls?: number; last_seen_at?: string; version?: string; count?: number | null; checks?: { tool: string; reason: string; fix: "host" | "review" }[];
  asked_by?: Actor; email?: string; level?: "none" | "viewer" | null; code?: string; expires_at?: string; blocks?: boolean; denied?: { destination: string; port: number; allowable: boolean }[];
  network?: "internal" | "open" | "unknown"; via?: "canyon" | "off"; channel?: string; channel_id?: string;
  licence?: "grace" | "lapsed" | "invalid"; grace_until?: string;
}
// A fleet row is the project read plus the newest gate run's id — `null` when no run has looked at it.
export interface FleetRow extends App { last_run: string | null; warnings?: number; needs?: Need[] }
// github_logins are the GitHub accounts the reader linked, github_link whether this host can link one (#2228).
export interface Viewer { display: string; role: "member" | "responder" | "admin"; github_logins: string[]; github_link: boolean }
// banner is none on an authenticated host, where the rail shows the viewer instead of a D5 line.
// total counts every row the query selects across its pages; page is the page served, null on a read by cursor.
// stats count what the tiles count, every app for an admin and the caller's team apps otherwise; no filter narrows them.
export interface FleetStats { apps: number; owners: number; production: number; preview: number; passed: number; deploys_7d: number; production_7d: number }
export interface FleetPage { tiles: Record<TileKey, number> & { needs_you?: number }; stats: FleetStats; apps: (FleetRow | Reduced)[]; total: number; page: number | null; next: string | null; banner: "none" | "loopback" | "exposed"; viewer: Viewer; unchecked?: number; org_needs?: Need[] }
export type RuleId = "prod.clean-gate" | "prod.human-approval" | "stack.blessed" | "owner.required" | "platform.known-vuln" | "prod.egress-allowed";
// The go-live rules: never placed before preview, their `state` is their state before production (contracts/snapshot.md, *Two gates*).
export const GO_LIVE: readonly RuleId[] = ["prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required", "platform.known-vuln", "prod.egress-allowed"];
// The rules whose rows carry the Beta chip (design/EXPERIENCE.md D5, #2384).
export const BETA: readonly string[] = ["platform.known-vuln"];
// A receipt names the four go-live rules, platform.known-vuln and prod.egress-allowed while each is on, and after them the rules placed before production by their own ids.
// `ships` is on a rollback review's receipt alone, once the run recorded the image a promotion of it ships.
// `packages` is platform.known-vuln's alone: each package it counts, with every advisory at high or above and what fixes it;
// to a contributor and above also where it is, each layer by owner and class, and whether it is Go's standard library (#2579).
export type KnownVulnClass = "os" | "runtime" | "lifecycle";
export interface KnownVulnPackage { name: string; version: string; advisories: { id: string; fixed: string }[]; go_stdlib?: true; layers?: { owner: string; class: KnownVulnClass }[] }
// A package only the built app holds, as deps.known-vuln's row lists it (contracts/routes.md, The promote receipt; #2610):
// what it counts, then what approvals cover, with every fingerprint an approval of it names.
export interface BuiltAppPackage { name: string; version: string; advisories: { id: string; fixed: string }[]; fingerprints: string[]; approved: boolean }
export interface Receipt { org: string; blocked_by: string[]; rules: { id: string; name: string; blocks: boolean; passed: boolean; why: string; unset?: true; read_at?: string; notes?: number; packages?: KnownVulnPackage[]; built_app?: BuiltAppPackage[] }[]; waiting?: string; ships?: { version: string; image_digest: string }; held?: Omit<Hold, "version"> }
// One of production's earlier versions (contracts/routes.md, the status row's earlier[]): `kept` when Canyon still has its image.
// A host from before earlier[] answers none.
export interface Earlier { package: string; version: string; image_digest: string; deployed_at: string; promoted_by: Actor | null; patched?: true; kept: boolean }
export interface Status { project: string; envs: App["envs"]; last_run: string | null; promote_run: string | null; receipt: Receipt; earlier?: Earlier[]; hold?: Hold; needs?: Need[] }
// How many problems findings are: their entries, as the CLI and the server count them (contracts/finding.md,
// One problem, one entry).
export const entries = (findings: Finding[]) => new Set(findings.map((one) => one.entry)).size;
export interface Finding {
  ruleId: string;
  // The index of its entry's first finding in the run read: one problem, however many results (contracts/finding.md,
  // One problem, one entry). Every card and count joins by it.
  entry: number;
  locations: [{ physicalLocation: { artifactLocation: { uri: string }; region: { startLine: number; startColumn: number; snippet: { text: string } } } }];
  partialFingerprints: { "canyon/v1": string };
  properties: { canyon: { severity: "critical" | "high" | "medium" | "low"; blocks: boolean; title: string; problem: string; fix: string; policy_name?: string; required_end_state?: string; do_not?: string[]; shown?: { startColumn: number; endColumn: number }; carried_from?: string; playbook?: PlaybookRef } };
  // Present only while an unexpired approval covers the finding (contracts/routes.md, GET /gate-runs/:id).
  approval?: { reason: string; expires_at: string; approved_by: Actor };
  // The tool that found it, as the run read names it: absent on a finding read from a log itself.
  tool?: string;
  // The dependency it is in, as its tool wrote it (contracts/finding.md, A dependency's name and version).
  // vulnerability, fixed and link are trivy's own lines; fixed_in fixes every finding of its tool, dependency and blocks.
  dependency?: { name: string; version: string; vulnerability?: string; fixed?: string; link?: string; fixed_in?: string };
  // It blocks before production under the app's placed rules: the receipt's own answer, never worked out here (#2206).
  blocks_production?: true;
}
export interface Run { id: string; package: string; state: "pending" | "running" | "clean" | "blocked" | "failed"; failure_reason?: string; failed_in?: string; blocking: number; warnings: number }
// fix: the one file a stock starter is missing, on a failed detect or start (contracts/routes.md, What a starter is missing).
// approvable: the reader may approve the run's blocking findings — an admin or a responder who did not start it (#2204).
// link_github: they would, but the run is a push's, the org asks its approvers to link GitHub, and they linked none (#2342).
export interface RunRead extends Run { not_checked?: true; approvable?: true; link_github?: true; builds?: string[]; errored?: true; fix?: { file: string; why: string; examples: { for: string; text: string }[] }; findings: Finding[] }
// A producer.sarif row's data (contracts/events.md): who produced it and its log — or, for an imported
// tool, one row per report with its category and normalised results and no log — read for its result
// count and what it said about its own run.
export interface ProducerLog { producer: string; category?: string; results?: unknown[]; sarif?: { runs: { invocations?: { toolExecutionNotifications?: { message: { text: string } }[] }[]; results: unknown[] }[] } }
export interface TimelineRow { id: number; kind: string; data: unknown }
// approved: a blocked run every blocking finding of which an approval covers now (#1955).
export interface RunRow extends Run { label?: string; check?: true; approved?: true; pull_request?: PullRequest; started_by: Actor | null; started_at: string; ended_at: string | null }
export interface Package { id: string; label?: string; commit: string; source: { kind: "upload" | "repo"; ref: string; actor: string }; created_at: string }
// One row of the trail (contracts/routes.md, GET /audit): `action` is a route pattern or a step's
// name and `detail` is whatever that action recorded — ui/Audit.tsx turns the two into a sentence.
// How and from where it was done (contracts/routes.md, the audit read): each recorded from the provenance
// amendment on where it applies; `session` and `source` are answered to admins alone.
export type Via = "dashboard" | "cli" | "mcp" | "github" | "platform" | "canyon";
export interface AuditEvent { id: string; ts: string; actor: Actor; action: string; via?: Via; role?: "admin" | "member"; session?: string; source?: string; project?: string; run?: string; detail: Record<string, unknown> }
export interface AuditPage { events: AuditEvent[]; total: number; page: number | null; next: string | null }
export interface Secret { name: string; env: "preview" | "prod"; version: number; set_by: Actor; set_at: string; origin: string; where: "runtime" | "build" }
// The org's guardrails (contracts/schema/guardrails.schema.json). A resolved design system names a
// registry and carries no history; a row is the registry entry an admin picks from.
export interface Stack { language: string; frameworks: string[] }
export interface Resolved { id: string; name: string; namespace: string; registry_url: string; tokens_url?: string }
export interface DesignSystemRow { id: string; name: string; namespace: string; source_type: "shipped" | "registry"; registry_url: string; tokens_url?: string; created_at: string; created_by: string }
// Who is reading, as GET /preferences answers them: the address the session stores and the role,
// so the settings page can find the reader in the lists it draws. Absent on a demo host, whose
// sessions store no address. GET /fleet answers the same concept as `Viewer` above — {display,
// role} — and no consumer is handed both.
export interface SettingsViewer { email: string; role: "member" | "responder" | "admin" }
// The keys the environment sets, which the read answers in its stead and a PUT is refused.
export type Overridable = "org_name" | "allowed_domains" | "allowed_emails" | "admin_claim" | "admin_value" | "responder_claim" | "responder_value" | "groups_claim";
// `admin_claim` and `admin_value` are present together or absent together, and both are absent on a
// member's read whatever the row holds (routes.md row 219) — `overridden` still names the pair to a
// member, because a variable's name is not its value.
export interface Preferences {
  stacks: Stack[]; design_system: Resolved | null; setup_available: boolean; ai_review: boolean; viewer?: SettingsViewer;
  // The buildpack ids "Approved stacks only" approves, sorted by id, and every buildpack an app's deploy
  // ran, named where the builder names it: what the screen offers to approve.
  approved_buildpacks: string[]; buildpacks_seen: { id: string; name?: string }[];
  org_name: string; allowed_domains: string[]; allowed_emails: string[]; admin_emails: string[];
  // The admins CANYON_ADMINS grants, answered apart from admin_emails and never merged into it: a
  // screen drawing one list would let somebody remove a bootstrap address, see the PUT succeed and
  // have that person stay an admin. `overridden` does not name this key — the variable grants
  // without pinning — so this list is the only thing telling the screen the two groups differ.
  bootstrap_admins: string[];
  admin_claim?: string; admin_value?: string; onboarded_at: string | null; overridden: Overridable[];
  // The security responders, answered to an admin alone (contracts/routes.md, the preferences row).
  responder_emails?: string[]; responder_claim?: string; responder_value?: string;
  // The sign-in claim carrying a person's groups, "" while unset: no group is shared with until it is set.
  groups_claim: string;
  // Whether a promotion the CLI asks for waits for a person to approve it here (design/AUTH.md §4.5).
  prod_approval: boolean;
  // Whether a push's blocking finding is approved only by someone who linked a GitHub account (#2342).
  push_approvers_linked: boolean;
  // Where apps run, and how long a handed-off app keeps its last access list when Canyon is unreachable (design/HAND-OFF.md §1);
  // how releases reach the platform (§3b).
  run_by: "canyon" | "org"; access_grace: string; delivery: "webhook" | "repo";
  // Whether Canyon patches its run image under the apps: off, or to preview on its own and production asked (design/PATCHING.md §4.5).
  patching: "off" | "ask";
  // Whether Canyon also rebuilds apps on a newer builder, production always asked (design/PATCHING.md §5); off while patching is.
  rebuilds: "off" | "ask";
  // Whether people may open database sessions at all, off by default (design/DATABASE-SESSIONS.md §5; #2835).
  db_sessions: "off" | "on";
}
// A PUT names the keys it writes; an absent key is unchanged. `admin_claim` and `admin_value` are
// named together, both strings or both null to clear the pair; `onboarded_at` is set once, and the
// PUT that sets it is the write that ends the setup state.
export interface PreferencesEdit {
  stacks?: Stack[]; approved_buildpacks?: string[]; design_system_id?: string | null; org_name?: string;
  allowed_domains?: string[]; allowed_emails?: string[]; admin_emails?: string[];
  admin_claim?: string | null; admin_value?: string | null; onboarded_at?: string; groups_claim?: string; prod_approval?: boolean; push_approvers_linked?: boolean;
  responder_emails?: string[]; responder_claim?: string | null; responder_value?: string | null;
  run_by?: "canyon" | "org"; access_grace?: string; delivery?: "webhook" | "repo"; patching?: "off" | "ask"; rebuilds?: "off" | "ask"; db_sessions?: "off" | "on";
}
// The skill manifest an agent is handed (contracts/routes.md, GET /skill/manifest). `state` is the
// two-name union and not a string — the opposite call to `tool` below, for the opposite reason: the
// route drops every `off` rule server-side, so a third state cannot arrive and there is nothing here
// to filter. A manifest design system carries no `id`: an agent names a registry, not a row.
// `how` (a Canyon rule's line on how to pass it) and `instructions` (a custom rule's text, its admin's)
// arrive only on a rule that blocks in either gate (contracts/routes.md, #2622).
export interface ManifestPolicy { id: string; name: string; state: "off" | "advisory" | "blocking"; production?: "advisory" | "blocking"; how?: string; instructions?: string }
export interface ManifestDesignSystem { name: string; namespace: string; registry_url: string; tokens_url?: string }
// `prod_approval`: whether a person approves each promotion in the dashboard (design/AUTH.md §4.5).
export interface Manifest { host: string; policies: ManifestPolicy[]; component_types: string[]; stacks: Stack[]; design_system: ManifestDesignSystem | null; prod_approval: boolean; playbooks?: ManifestPlaybook[] }
// A playbook as an agent is handed it, never its body (contracts/routes.md, GET /skill/manifest).
export interface ManifestPlaybook { slug: string; name: string; kind: PlaybookKind; when: string; version: number }
// One setup turn. `tool` is a string and not the schema's two-name enum: the dispatcher grows tools
// the bundle was built before, and the panel has to echo one it has never heard of. `before` and
// `after` are typed JSON the panel prints as typed JSON — nothing here reads inside them.
export interface SetupMessage { role: "user" | "assistant"; content: string }
export interface SetupWrite { tool: string; before: unknown; after: unknown }
export interface SetupProposal { rule: string; state: "off" | "advisory" | "blocking"; grace_days?: number; would_block: { project: string; findings: number }[] }
export interface SetupAnswer { message: string; writes: SetupWrite[]; proposal?: SetupProposal }
// The org's policies (contracts/routes.md, the policies row). `state` is the effective state now
// before preview — a go-live rule's before production, its only gate — and `production` its state
// before production, present only when the rule is placed there; `scheduled` and
// `scheduled_production` are each gate's applied edit whose grace has not elapsed yet.
export type PolicyState = "off" | "advisory" | "blocking";
// A Canyon-run tool's rule (sarif.trivy, .opengrep, .eslint, .grype, .checkov) also carries `run` and its
// settings, each only when set: absent `run` is off and absent `settings` is none.
export interface ToolSettings { scanners?: string[]; rules?: { repo: string; commit: string; paths?: string[] }; frameworks?: string[]; skip_checks?: string[]; skip_paths?: string[] }
// An app's addition to a rule (the app-additions row): stricter than the org's in each gate it names.
export interface Addition { project: string; state?: PolicyState; production?: PolicyState; set_by: Actor; set_at: string }
export interface Policy {
  id: string; name: string; state: PolicyState; production?: PolicyState; grace_days?: number; scheduled?: { state: PolicyState; effective_at: string };
  scheduled_production?: { state: PolicyState; effective_at: string };
  set_by?: Actor; set_at?: string; run?: boolean; settings?: ToolSettings; additions?: Addition[];
}
export interface Blocked { project: string; findings: number }
// An edit names the gates it sets: `state` before preview (a go-live rule's before production, its
// only gate) and `production` before production, `off` taking the rule out of that gate.
export interface PolicyChange { state?: PolicyState; production?: PolicyState }
// A pending device grant as the approval screen reads it: the address the CLI ran on and when.
export interface DeviceGrant { requested_by: string; requested_at: string; expires_at: string; approver: { display: string; email: string } }
// A production approval as its page reads it (contracts/routes.md, Production approvals): who asked and
// from where, the version and what it replaces, what became of it, and the receipt for the reader.
export interface PromotionApproval {
  code: string; app: string; version: string; replaces: string | null;
  requested_by: Actor; requested_via: string; requested_at: string; expires_at: string;
  state: "pending" | "approved" | "denied" | "expired" | "outdated"; decided_by?: Actor; decided_at?: string; run?: string; held?: Omit<Hold, "version">;
  receipt?: Receipt; answered?: Env["answered"]; approver: { display: string; email: string };
  // A patch approval's alone (patch.schema.json's approval_read): Canyon asked, for a patch whose watch passed in preview.
  kind?: "patch"; patch_run?: string; identity?: string; replaces_image?: string; image?: string; fixes?: PatchFix[]; proof?: string; watch?: { until: string; result: "passed" };
  rebuild?: true; // a rebuild patch's (contracts/routes.md, A rebuild patch): its proof is the rebuild's own
}
export interface PatchFix { name: string; from: string; to: string; vulnerability: string; class: "os" | "runtime" | "lifecycle" }
// A platform identity (contracts/routes.md, the identities row): one run image Canyon patches onto, its wave, its apps by
// patch state, and whether its breaker paused or stalled it or an admin approved it for every app.
export interface Identity {
  id: string; kind: "rebase" | "rebuild"; run_image: string; builder?: string; planned_at: string; wave: number;
  apps: { planned: number; patched_preview: number; asked: number; patched: number; reverted: number; held: number; denied: number };
  paused?: { at: string; projects: string[] }; stalled?: { at: string; projects: string[] }; approved?: { by: Actor; at: string; reason: string };
}
// What the newest tick's patch plan said (contracts/routes.md, the identities row's plan; #2588): why it read no app, or
// the identity it planned onto and each app it read with its one cause. Each `why` is the contract's enum, read as a
// string so a cause this page does not know reads unknown.
export interface PlanSaid { at: string; identity: string | null; why?: string; apps: { project: string; why: string }[] }
// `run_image` the run image Canyon uses now, how many apps wait for a newer one and their ids; null when Canyon can't tell.
// `plan` null until a tick planned.
export interface Identities { identities: Identity[]; run_image: { image: string; behind: number; projects: string[] } | null; plan: PlanSaid | null }

// An upload is shown as its version: the label the read answers when it has one, else its id
// without the prefix (contracts/routes.md, *Naming a version*). Every read that names a package
// answers its label, so no screen works one out.
// A pull request's package by its pull request (#1947): "PR #12 (fix-login)".
export const pullRequestName = (pr: PullRequest) => `PR #${pr.number} (${pr.branch})`;

export function version(id: string, label?: string): string {
  return label ?? id.slice("pkg_".length);
}

// A non-2xx answer keeps the server's error body: a refused promotion carries its receipt there.
// The message names the route and not the query: a query carries the words the screens do not say
// (D6), and a hand-typed one would put them on screen through the failure line.
export class ApiError extends Error {
  constructor(method: string, path: string, readonly status: number, readonly body: unknown) {
    super(`${method} /api/v1${path.split("?")[0]} answered ${status}`);
  }
}

// What the server said when it said something, else the failure the call carries. Every screen that
// renders a refusal renders the server's own sentence and recognises none of them.
export const said = (err: unknown) => ((err as ApiError).body as { message?: string } | null | undefined)?.message ?? (err as Error).message;

// A 401 from any call is announced on the window: the shell replaces the whole app with the
// sign-in page (design/AUTH.md §8), whichever screen made the read.
export const UNAUTHENTICATED = "canyon:unauthenticated";

async function send(method: "GET" | "POST" | "PUT" | "DELETE", path: string, body?: unknown): Promise<Response> {
  const res = await fetch(`/api/v1${path}`, { method, headers: { accept: "application/json", "content-type": "application/json" }, body: JSON.stringify(body) });
  if (res.status === 401) window.dispatchEvent(new Event(UNAUTHENTICATED));
  if (!res.ok) throw new ApiError(method, path, res.status, await res.json().catch(() => null));
  return res;
}

// `bodiless` is a success answered without a body besides a 204: POST …/access-requests' 201.
async function call<T>(method: "GET" | "POST" | "PUT" | "DELETE", path: string, body?: unknown, bodiless = false): Promise<T> {
  const res = await send(method, path, body);
  return (bodiless || res.status === 204 ? undefined : await res.json()) as T;
}

// GatewayPending is the egress gateway replica that hadn't taken an outbound rule change when the server stopped waiting for
// it, "unread" when it couldn't tell, else null: the change stands either way (contracts/routes.md,
// Canyon-Egress-Pending; #2789).
export type GatewayPending = string | null;

// egressWrite is one of the writes that change the egress policy: its answer, and what was still pending.
async function egressWrite<T>(method: "PUT" | "POST" | "DELETE", path: string, body?: unknown): Promise<{ answer: T; pending: GatewayPending }> {
  const res = await send(method, path, body);
  return { answer: (res.status === 204 ? undefined : await res.json()) as T, pending: res.headers?.get("Canyon-Egress-Pending") ?? null }; // headers, as every fetch has; a test's stand-in answer may carry none
}

// An environment's last log lines, masked (contracts/routes.md, the logs row's redaction=masked; #2744). A body the
// server doesn't say it masked is never returned (fail closed).
export async function getMaskedLogs(app: string, env: "preview" | "prod", lines: number): Promise<string> {
  const path = `/projects/${encodeURIComponent(app)}/logs?env=${env}&lines=${lines}&redaction=masked`;
  const res = await fetch(`/api/v1${path}`, { method: "GET", headers: { accept: "text/plain" } });
  if (res.status === 401) window.dispatchEvent(new Event(UNAUTHENTICATED));
  if (!res.ok) throw new ApiError("GET", path, res.status, await res.json().catch(() => null));
  if (res.headers.get("Canyon-Log-Redaction") !== "masked") throw new Error(`GET /api/v1${path.split("?")[0]} wasn't masked`);
  return res.text();
}

// The Open route the sign-in page reads the org's name from; the bundle carries no configuration.
// `auth` is what tells the shell whether a null `onboarded_at` means a host nobody has set up or a
// demo host with no authentication at all, where the row's setup marker is null and stays null.
export const getAuthConfig = async () => (await fetch("/auth/config")).json() as Promise<{ org: string; auth: "oidc" | "off" }>;
// Revokes the caller's session, then the app shows the sign-in page as it would on any 401.
// The sign-in page this lands on says the company account is still signed in: only a chosen sign-out does.
export const signOut = () => call<void>("DELETE", "/sessions").then(() => window.dispatchEvent(new CustomEvent(UNAUTHENTICATED, { detail: "signed-out" })));

// The code goes to the server as typed: it ignores dashes and case, and answers 404 for a code that
// is not pending, so the screen normalises nothing.
const grantPath = (code: string) => `/device-grants/${encodeURIComponent(code)}`;
export const getDeviceGrant = (code: string) => call<DeviceGrant>("GET", grantPath(code));
export const approveDevice = (code: string) => call<void>("POST", `${grantPath(code)}/approve`);
export const denyDevice = (code: string) => call<void>("POST", `${grantPath(code)}/deny`);
// An MCP client's consent (contracts/routes.md, Sessions; design/AUTH.md §4.7): the request it waits on, and the
// redirect the browser follows once the person allows or refuses it.
export interface OAuthRequest { client: { name: string; host: string; verified: boolean }; redirect_host: string; scopes: ("read" | "act")[]; host: string; requested_at: string; expires_at: string; account: { display: string; email: string } }
const oauthPath = (id: string) => `/oauth/requests/${encodeURIComponent(id)}`;
export const getOAuthRequest = (id: string) => call<OAuthRequest>("GET", oauthPath(id));
export const approveOAuthRequest = (id: string) => call<{ redirect: string }>("POST", `${oauthPath(id)}/approve`);
export const denyOAuthRequest = (id: string) => call<{ redirect: string }>("POST", `${oauthPath(id)}/deny`);
// The reader's live sessions, and everyone's to an admin, each by its digest; ending one is a browser's.
export interface ListedSession { id: string; flow: "browser" | "device" | "demo" | "oauth"; client?: string; created_at: string; last_seen_at: string | null; expires_at: string; current: boolean; person?: { display: string; email: string } }
export const listSessions = () => call<{ sessions: ListedSession[] }>("GET", "/sessions");
export const endSession = (id: string) => call<void>("DELETE", `/sessions/${encodeURIComponent(id)}`);
const approvalPath = (code: string) => `/promotion-approvals/${encodeURIComponent(code)}`;
export const getPromotionApproval = (code: string) => call<PromotionApproval>("GET", approvalPath(code));
// What the image an environment runs holds of Canyon's (contracts/routes.md, the image read), as far as a page reads it:
// whether it checked and its own words for it, when its packages were listed and against which vulnerability database,
// each counted finding with its class and package, the notes, and what Canyon's patches fixed in it. Contributor and above.
export interface ImageFinding { class: KnownVulnClass; properties: { canyon: { severity: "critical" | "high" | "medium" | "low"; fix: string } }; dependency: { name: string; version: string; vulnerability: string; fixed: string } }
export interface ImageRead {
  state: "checked" | "could_not_check" | "not_scanned"; why: string; scanned_at: string | null; db_built_at: string | null;
  findings: ImageFinding[]; notes: { name: string; version: string; class: KnownVulnClass | "unknown"; why: "no_fix" | "unknown" | "not_checked" | "unplaced" }[];
  patched?: { name: string; from: string; to: string; vulnerability: string; class: KnownVulnClass }[];
}
export const getImage = (app: string, env: "preview" | "prod") => call<ImageRead>("GET", `/projects/${encodeURIComponent(app)}/envs/${env}/image`);
export const approvePromotion = (code: string, anyway: boolean) => call<{ run: { id: string; state: string } }>("POST", `${approvalPath(code)}/approve`, anyway ? { promote_anyway: true } : undefined);
export const denyPromotion = (code: string) => call<void>("POST", `${approvalPath(code)}/deny`);
// An admin's, served only while people sign in (a 404 otherwise); each change from a browser session.
const identityPath = (id: string) => `/patching/identities/${encodeURIComponent(id)}`;
export const getIdentities = () => call<Identities>("GET", "/patching/identities");
export const approveIdentity = (id: string, reason: string) => call<{ identity: Identity }>("POST", `${identityPath(id)}/approve`, { reason });
export const resumeIdentity = (id: string) => call<{ identity: Identity }>("POST", `${identityPath(id)}/resume`);

// query is the fleet read's query string exactly as the URL hash carries it: the server filters,
// searches, sorts and pages, and answers 400 for a value it does not accept.
export const getFleet = (query: string) => call<FleetPage>("GET", query === "" ? "/fleet" : `/fleet?${query}`);
export const getProject = (id: string) => call<{ project: App | Reduced }>("GET", `/projects/${encodeURIComponent(id)}`);
// The owner set gains whoever asks, under the display sent; the server refuses an empty one.
export const claim = (id: string, display: string) => call<{ owners: Actor[] }>("PUT", `/projects/${encodeURIComponent(id)}/owner`, { display });
// With run, the receipt is that gate run's, as a promotion naming it would read it (a rollback review's).
export const getStatus = (id: string, run?: string) => call<Status>("GET", `/projects/${encodeURIComponent(id)}/status${run === undefined ? "" : `?run=${encodeURIComponent(run)}`}`);
// Undeploy: stops the env's app and keeps its data, never refused for a run: 204 stopped, 202
// {stopping} when a run already switching the env stops it once it lands, 202 {release} when the org's
// platform removes it (contracts/routes.md, The brake).
export const takeOffline = (id: string, env: "preview" | "prod") =>
  call<{ stopping?: { run: { id: string; state: string } }; release?: string } | undefined>("DELETE", `/projects/${encodeURIComponent(id)}/envs/${env}`);
// Without confirm nothing changes: the 409 confirm_required lists what the confirmed call destroys.
// Confirmed, 204 is gone and 202 names what a driver is still deleting; the same call again resumes.
// Stopping production and holding it, and lifting the hold, a responder's or an admin's (contracts/routes.md, The hold).
const holdPath = (id: string) => `/projects/${encodeURIComponent(id)}/envs/prod/hold`;
export const holdProduction = (id: string, reason: string) => call<unknown>("POST", holdPath(id), { reason });
// A lift names the hold it read, held_at its at: a hold placed since is 409 hold_changed, and nothing is lifted.
export const liftHold = (id: string, heldAt: string, start: boolean) => call<unknown>("DELETE", `${holdPath(id)}?held_at=${encodeURIComponent(heldAt)}${start ? "&start=true" : ""}`);
// A delete's listing: Canyon's records, then what the org's platform provided, state "provided" (#2432).
export interface Resource { name: string; type: string; env: "preview" | "prod"; state: string }
// The single read's resources[], a team member's alone (contracts/routes.md): a record of Canyon's, a
// declaration no one has made yet (no env), or one the org's platform provided, with where it is.
export interface ProjectResource { name: string; type: string; env?: "preview" | "prod"; state: "declared" | "pending" | "ready" | "failed" | "deleting" | "provided"; endpoint?: string }
// Only the display name changes: the id stays the app's address and every route's (#1909).
export const renameProject = (id: string, name: string) => call<{ project: App }>("PUT", `/projects/${encodeURIComponent(id)}/name`, { name });
export const deleteProject = (id: string, confirm?: string) =>
  call<{ pending: Resource[] } | undefined>("DELETE", `/projects/${encodeURIComponent(id)}${confirm === undefined ? "" : `?confirm=${encodeURIComponent(confirm)}`}`);
// anyway is the person's override of an error preview answered (contracts/routes.md, preview_answered).
export const promote = (id: string, gateRun: string | null, anyway: boolean) => call<{ run?: { id: string } }>("POST", `/projects/${encodeURIComponent(id)}/promotions`, anyway ? { gate_run: gateRun, promote_anyway: true } : { gate_run: gateRun });
// Starts a review of the current package; a run already going is the server's 409, said as it said it.
export const startReview = (id: string) => call<{ run: { id: string; state: string } }>("POST", `/projects/${encodeURIComponent(id)}/gate-runs`);
// A rollback review (design/OPERATIONS.md §1): the package production ran, reviewed under today's policy, preview untouched.
export const putBack = (id: string, pkg: string, digest: string) => call<{ run: { id: string; state: string } }>("POST", `/projects/${encodeURIComponent(id)}/gate-runs`, { package: pkg, rollback_to: digest });
// Deploys a package again, which reviews it first (contracts/routes.md, the deploys row): "review it again".
export const redeploy = (id: string, pkg: string) => call<{ run: { id: string } }>("POST", `/projects/${encodeURIComponent(id)}/deploys`, { package: pkg });
export const getRun = (run: string) => call<{ run: RunRead }>("GET", `/gate-runs/${encodeURIComponent(run)}`);
export const cancelRun = (run: string) => call<{ run: { id: string; state: string } }>("POST", `/gate-runs/${encodeURIComponent(run)}/cancel`);
export const getTimeline = (run: string, after: number) => call<{ rows: TimelineRow[]; next: number | null }>("GET", `/gate-runs/${encodeURIComponent(run)}/timeline?after=${after}`);
// fingerprint is the finding's whole canyon/v1 value: the approval follows the finding across runs.
export const approve = (run: string, fingerprint: string, body: { reason: string; expires_at: string }) =>
  call<unknown>("POST", `/gate-runs/${encodeURIComponent(run)}/findings/${encodeURIComponent(fingerprint)}/approve`, body);
// One dependency's findings approved together, one reason and expiry for all (contracts/routes.md).
export const approveFindings = (run: string, body: { fingerprints: string[]; reason: string; expires_at: string }) =>
  call<unknown>("POST", `/gate-runs/${encodeURIComponent(run)}/findings/approve`, body);
// A cursor is the server's, handed back exactly as it came; no limit is sent, so a page is the server's default.
const paged = (path: string, cursor: string | null) => (cursor === null ? path : `${path}?cursor=${encodeURIComponent(cursor)}`);
export const listPackages = (id: string, cursor: string | null) => call<{ packages: Package[]; next: string | null }>("GET", paged(`/projects/${encodeURIComponent(id)}/packages`, cursor));
export const listRuns = (id: string, cursor: string | null) => call<{ runs: RunRow[]; next: string | null }>("GET", paged(`/projects/${encodeURIComponent(id)}/gate-runs`, cursor));
// query is the trail read's query string as the caller builds it (project, cursor, limit); the server filters and pages.
export const listAudit = (query: string) => call<AuditPage>("GET", query === "" ? "/audit" : `/audit?${query}`);
export const listSecrets = (id: string) => call<{ secrets: Secret[] }>("GET", `/projects/${encodeURIComponent(id)}/secrets`);
// A secret set or replaced, and removed (contracts/routes.md, the secrets rows): the value travels in this PUT alone, and no
// route ever answers one back. env and where are sent as chosen, never left for the server to default.
export const setSecret = (id: string, name: string, body: { value: string; env: "preview" | "prod"; where: "runtime" | "build" }) =>
  call<void>("PUT", `/projects/${encodeURIComponent(id)}/secrets/${encodeURIComponent(name)}`, body);
export const removeSecret = (id: string, name: string, env: "preview" | "prod") =>
  call<void>("DELETE", `/projects/${encodeURIComponent(id)}/secrets/${encodeURIComponent(name)}?env=${env}`);
// The Access group (contracts/routes.md): the read, and a share that writes one grant whole and
// answers the read again. Every route but asking is its owners' and admins'.
export type GrantKind = "email" | "group" | "everyone";
export interface Grant { kind: "email" | "group"; grantee: string; display: string; level: Level; roles: string[]; granted_by: Actor; granted_at: string }
export interface AccessRead { everyone: { roles: string[] } | null; entries: Grant[]; roles: { name: string; description: string }[]; requests: { email: string; display: string; requested_at: string; level: "none" | "viewer" | null }[] }
const accessPath = (id: string) => `/projects/${encodeURIComponent(id)}/access`;
const sharePath = (id: string, kind: GrantKind, grantee: string) => `${accessPath(id)}/${kind}/${encodeURIComponent(grantee)}`;
export const getAccess = (id: string) => call<AccessRead>("GET", accessPath(id));
export const share = (id: string, kind: GrantKind, grantee: string, body: { level: Level; roles: string[] }) => call<AccessRead>("PUT", sharePath(id, kind, grantee), body);
export const unshare = (id: string, kind: GrantKind, grantee: string) => call<void>("DELETE", sharePath(id, kind, grantee));
export const askForAccess = (id: string) => call<void>("POST", `/projects/${encodeURIComponent(id)}/access-requests`, undefined, true);
export const dismissRequest = (id: string, email: string) => call<void>("DELETE", `/projects/${encodeURIComponent(id)}/access-requests/${encodeURIComponent(email)}`);
// One org per host, one row. The PUT writes only the keys it names and answers the GET shape, so its
// answer is the re-read: a key left out is unchanged, and a null design_system_id clears the default.
export const getPreferences = () => call<Preferences>("GET", "/preferences");
export const putPreferences = (body: PreferencesEdit) => call<Preferences>("PUT", "/preferences", body);
export const getDesignSystems = () => call<{ design_systems: DesignSystemRow[] }>("GET", "/design-systems");
export const createDesignSystem = (body: { name: string; namespace: string; registry_url: string; tokens_url?: string }) =>
  call<{ design_system: DesignSystemRow }>("POST", "/design-systems", body);
// Derived fresh on every request from the snapshot and the org's row, so it is re-read wherever
// those can have moved — the mount, a Save, a setup turn — and never polled.
export const getSkillManifest = () => call<Manifest>("GET", "/skill/manifest");
// A PUT without apply is the preview and writes nothing; only `apply` writes the edit.
export const getPolicies = () => call<{ policies: Policy[] }>("GET", "/policies");
export const previewPolicy = (rule: string, change: PolicyChange) => call<{ would_block: Blocked[] }>("PUT", `/policies/${encodeURIComponent(rule)}`, change);
export const applyPolicy = (rule: string, body: PolicyChange & { grace_days?: number }) =>
  call<{ policy: Policy; would_block: Blocked[] }>("PUT", `/policies/${encodeURIComponent(rule)}?apply=true`, body);
// An org's custom rules (contracts/routes.md, the custom-rules row): every member reads them, an
// admin saves a rule's next version; its state is the policies row's, like any rule's.
// Its check is how its version is checked: by code, with the condition in plain words, or by AI (#2639).
export interface CustomCheck { by: "code" | "ai"; expr?: string; says?: string; facts_version: number }
// `playbook` is the slug of the org's playbook the version points at, null for none: a PUT that leaves it out points at none.
export interface CustomRule { rule: string; name: string; instructions: string; check: CustomCheck; playbook: string | null; version: number; set_by: Actor; set_at: string }
export const getCustomRules = () => call<{ custom_rules: CustomRule[] }>("GET", "/custom-rules");
export const putCustomRule = (slug: string, body: { name: string; instructions: string; check?: "code" | "ai"; playbook?: string | null }) =>
  call<{ custom_rule: CustomRule; why?: string }>("PUT", `/custom-rules/${encodeURIComponent(slug)}`, body);
// The org's playbooks (contracts/routes.md, the playbooks row; design/specs/mcp.md §3): how the org builds, which
// agents read and Canyon never checks. Every member reads one that stands; an admin, from a browser, saves, removes
// and restores them, and reads every version, a removed playbook's too.
export type PlaybookKind = "skill" | "note";
export interface PlaybookRef { slug: string; name: string }
export interface PlaybookText { name: string; kind: PlaybookKind; when: string; body: string }
export interface PlaybookEntry { slug: string; name: string; kind: PlaybookKind; when: string; version: number; updated_by: Actor; updated_at: string }
export interface Playbook extends PlaybookEntry { body: string }
export interface PlaybookVersion extends PlaybookText { version: number; set_by: Actor; set_at: string }
export const getPlaybooks = () => call<{ playbooks: PlaybookEntry[] }>("GET", "/playbooks");
export const getPlaybook = (slug: string) => call<{ playbook: Playbook }>("GET", `/playbooks/${encodeURIComponent(slug)}`);
export const putPlaybook = (slug: string, body: PlaybookText) => call<{ playbook: Playbook }>("PUT", `/playbooks/${encodeURIComponent(slug)}`, body);
export const removePlaybook = (slug: string) => call<void>("DELETE", `/playbooks/${encodeURIComponent(slug)}`);
export const getPlaybookVersions = (slug: string) => call<{ slug: string; removed: boolean; versions: PlaybookVersion[] }>("GET", `/playbooks/${encodeURIComponent(slug)}/versions`);
export const restorePlaybook = (slug: string, version: number) => call<{ playbook: Playbook }>("POST", `/playbooks/${encodeURIComponent(slug)}/restore`, { version });
// A dry run of a rule checked by code: its current version judged on every app's latest review, an
// admin's alone (contracts/routes.md, the dry-run row; #2640). Nothing is written.
export interface DryRunApp { project: string; result: "pass" | "fail" | "not_checked"; why?: string }
export const dryRunCustomRule = (slug: string) =>
  call<{ rule: string; version: number; apps: DryRunApp[] }>("POST", `/custom-rules/${encodeURIComponent(slug)}/dry-run`);
// A Canyon-run tool's run and settings, both in effect at once; `state` rides only when the apply
// adds the tool. An added tool's state changes through the policies' preview (PolicyChange.tsx).
export const applyScanner = (rule: string, body: { state?: PolicyState; run: boolean; settings: ToolSettings }) =>
  call<{ policy: Policy; would_block: Blocked[] }>("PUT", `/policies/${encodeURIComponent(rule)}?apply=true`, body);
// The credential for a private Opengrep rules repo: the read says one is set and never what it is.
export interface Credential { repo: string; set_at: string; set_by: Actor }
const CREDENTIAL = "/scanners/opengrep/credential";
export const getCredential = () => call<{ credential: Credential | null }>("GET", CREDENTIAL);
export const setCredential = (body: { repo: string; username: string; token: string }) => call<void>("PUT", CREDENTIAL, body);
export const removeCredential = () => call<void>("DELETE", CREDENTIAL);
// The org's chat channels (contracts/routes.md, the channels row; #2725), an admin's alone: no read carries a webhook URL,
// only its host and last four characters. A PUT posts the summary first and saves only once the channel takes it; a
// change that leaves `url` out keeps the one saved.
export interface Channel { id: string; name: string; kind: "slack" | "teams"; host: string; tail: string; set_by: Actor; set_at: string; failing: { since: string; status: number } | null }
const channelPath = (id: string) => `/integrations/channels/${encodeURIComponent(id)}`;
export const getChannels = () => call<{ channels: Channel[] }>("GET", "/integrations/channels");
export const putChannel = (id: string, body: { name: string; kind: Channel["kind"]; url?: string }) => call<{ channel: Channel }>("PUT", channelPath(id), body);
export const removeChannel = (id: string) => call<void>("DELETE", channelPath(id));
export const testChannel = (id: string) => call<void>("POST", `${channelPath(id)}/test`);
// The host's one front door (contracts/routes.md, /api/v1/front-door): how people reach apps on a
// Kubernetes host. The read never carries the client secret. A PUT replaces it whole, is written
// only once the executor's probe passes the cluster and the certificate, and answers the read.
export type AddressForm = "app.env.domain" | "app-env.domain";
export interface FrontDoor {
  choice: 1 | 2; gateway?: "oidc" | "alb"; issuer?: string; audience?: string; jwks_url?: string; cookie?: string; alb_arn?: string;
  signin?: { issuer: string; client_id: string }; domain: string; form: AddressForm; set_by: Actor; set_at: string;
  probed: boolean; // the save that stored it passed the probe: Canyon runs apps behind no other door (#2438)
  enforcing: boolean; // the save that stored it found NetworkPolicy enforced: nothing builds behind any other door, under either run_by (#2507)
}
// `warning` is the probe's at the save that stored the door, "" without one: it never refused the save.
// `grants_note` is the server's line on whether a grant to a person applies through the door, "" when it does (#2299).
// `egress_note` is the server's line, under via canyon, that routed apps can't reach the door's sign-in, "" when they can (#2316).
// `discovery_note` is the server's line that it hasn't read a choice 2 provider's discovery yet (#2344), or couldn't in its daily check (#2389), "" otherwise.
// `hand_off_needs` is what the server's own settings lack to serve hand-off, each with the sentence that says how to set it (#2010).
// `probe_note` is the server's line, and why, while the stored door is not enforcing (#2507) or has not passed the probe (#2438), "" once it is both.
export interface FrontDoorRead {
  runtime: "docker" | "kubernetes"; default_domain: string; front_door: FrontDoor | null; warning: string; grants_note: string; egress_note: string;
  discovery_note: string; hand_off_needs: { error: "identity_image_required" | "https_required"; message: string }[]; probe_note: string;
}
// `client_secret` left out keeps the stored one while `issuer` and `client_id` are unchanged; `domain` left out is `default_domain`.
export type FrontDoorEdit = Omit<FrontDoor, "signin" | "domain" | "set_by" | "set_at" | "probed" | "enforcing"> & { signin?: { issuer: string; client_id: string; client_secret?: string }; domain?: string };
// Where releases go under run_by: org (contracts/routes.md, the platform-webhook row). The read never carries the secret or
// the host token: the first save answers both once, and each replace answers its one, once (#2439).
export interface PlatformWebhook { url: string; set_by: Actor; set_at: string; last_delivery: { delivery: string; release: string; sent_at: string; status: number } | null }
export interface WebhookSaved { url: string; set_by: Actor; set_at: string; secret?: string; token?: string }
// null before one is saved (#2560).
export const getPlatformWebhook = () => call<PlatformWebhook | null>("GET", "/platform-webhook");
export const putPlatformWebhook = (url: string) => call<WebhookSaved>("PUT", "/platform-webhook", { url });
export const replaceHostToken = () => call<{ token: string }>("POST", "/platform-webhook/token");
export const replaceWebhookSecret = () => call<{ secret: string }>("POST", "/platform-webhook/secret");
// A test delivery to the saved address: the status it answered, 0 for none.
export const pingPlatformWebhook = () => call<{ delivery: string; sent_at: string; status: number }>("POST", "/platform-webhook/ping");
// Where releases go under delivery: repo (contracts/routes.md, the platform-repo row; #2470). The read never carries the repo's
// token; the first save answers the fetch-only token and the host token, both kept on this row, once.
export interface PlatformRepo {
  url: string; username: string; preview: { branch: string }; prod: { branch: string; mode: "pull_request" | "push" };
  root: string; api: "github" | "gitlab" | "none"; api_url: string; store: string; ca: string | null; set_by: Actor; set_at: string; checked_at: string | null;
  last_commit: { release: string; env: "preview" | "prod"; state: Pending["state"] | "deployed" | "failed" | "removed"; at: string; commit?: string; pull_request?: { url: string; number: number }; problem?: string } | null;
}
export interface PlatformRepoEdit {
  url: string; username?: string; token?: string; preview: { branch: string }; prod: { branch: string; mode: "pull_request" | "push" };
  root?: string; api?: "github" | "gitlab" | "none"; api_url?: string; store?: string; ca?: string | null;
}
export interface RepoProblem { code: string; message: string }
export const getPlatformRepo = () => call<PlatformRepo | null>("GET", "/platform-repo"); // null before one is saved (#2560)
export const putPlatformRepo = (edit: PlatformRepoEdit) => call<PlatformRepo & { fetch_token?: string; token?: string }>("PUT", "/platform-repo", edit);
export const checkPlatformRepo = () => call<{ passed: boolean; problems: RepoProblem[]; templates: string[] }>("POST", "/platform-repo/check");
export const replaceRepoToken = (token: string) => call<{ set_by: Actor; set_at: string }>("POST", "/platform-repo/token", { token });
export const replaceRepoHostToken = () => call<{ token: string }>("POST", "/platform-repo/host-token");
export const replaceFetchToken = () => call<{ fetch_token: string }>("POST", "/platform-repo/fetch-token");
// The GitHub organizations Canyon's Apps are on, one App per org (contracts/routes.md, GET /api/v1/github; #1914).
// links_accounts: Canyon holds the App's client secret, which linking a person's GitHub account needs (#2228).
export interface GitHubOrg { account: string; app_slug: string; keeps_code: boolean; imports_new: boolean; installed: boolean; last_event_at: string | null; links_accounts: boolean }
export interface GitHubRead { reachable: boolean; orgs: GitHubOrg[] }
export const getGitHub = () => call<GitHubRead>("GET", "/github");
export const setGitHubClientSecret = (account: string, secret: string) => call<{ org: GitHubOrg }>("PUT", `/github/orgs/${encodeURIComponent(account)}/client-secret`, { client_secret: secret });
export const setGitHubOrg = (account: string, importsNew: boolean) => call<{ org: GitHubOrg }>("PUT", `/github/orgs/${encodeURIComponent(account)}`, { imports_new: importsNew });
export const handOver = (app: string) => call<{ repository: RepoLink }>("PUT", `/projects/${encodeURIComponent(app)}/repository/keeps`, { keeps: false });
export const setAutoDeploy = (app: string, on: boolean) => call<{ auto_deploy: boolean }>("PUT", `/projects/${encodeURIComponent(app)}/auto-deploy`, { auto_deploy: on });
// The repositories Canyon's Apps see and the apps linked to them, read from GitHub on each request (#1917).
export interface LinkedApp { folder: string; id: string; name: string; state: RepoLink["state"] }
export interface GitHubRepository { repo_id: number; full_name: string; org: string; default_branch: string; apps: LinkedApp[] }
// keeps: Canyon commits the app's versions there (#1926), and pending is that repository not made yet, with no repo_id.
export interface RepoLink { repo_id: number | null; full_name: string; folder: string; state: "linked" | "deleted" | "archived" | "removed" | "pending"; keeps?: boolean; kept_commit?: string | null; kept_error?: string }
export const getGitHubRepositories = () => call<{ repositories: GitHubRepository[]; unreachable: string[] }>("GET", "/github/repositories");
export const importRepository = (repo: number, folder: string, name?: string) =>
  call<{ project: App }>("POST", `/github/repositories/${repo}/import`, name === undefined ? { folder } : { folder, name });
export const linkRepository = (app: string, repo: number, folder: string) => call<{ repository: RepoLink }>("PUT", `/projects/${encodeURIComponent(app)}/repository`, { repo_id: repo, folder });
export const unlinkRepository = (app: string) => call<void>("DELETE", `/projects/${encodeURIComponent(app)}/repository`, undefined, true);
export const getFrontDoor = () => call<FrontDoorRead>("GET", "/front-door");
// GET /scanners/data (contracts/routes.md; #2743): how old each vulnerability database is, an admin's read.
export interface ScanDB { tool: string; built_at?: string; stale_at?: string }
export const getScannerData = () => call<{ databases: ScanDB[] }>("GET", "/scanners/data");
// The daily report of counts (contracts/schema/telemetry.schema.json, state; contracts/routes.md, Telemetry): an admin's.
// `report` is the next send's body; `last_report` what the last send carried, null before one.
// Each report is a string of the send's exact bytes, shown as it is and never parsed to be re-written: what is sent is
// the promise (contracts/routes.md, Telemetry).
export interface TelemetryState { enabled: boolean; off_because?: "env" | "admin" | "unreachable"; report: string; last_sent_at: string | null; last_report: string | null }
export const getTelemetry = () => call<TelemetryState>("GET", "/telemetry");
export const putTelemetry = (enabled: boolean) => call<TelemetryState>("PUT", "/telemetry", { enabled });
// The Enterprise licence (contracts/schema/licence.schema.json, state; contracts/routes.md, Licence; #2879): every member
// reads {state, allows}, an admin also where it came from and what it says. `allows` is what this Canyon unlocks now.
export type LicenceFeature = "outbound_keys" | "platform_patching" | "group_sync";
export interface LicenceState {
  state: "none" | "valid" | "grace" | "lapsed" | "invalid"; allows: LicenceFeature[]; source?: "env" | "dashboard";
  customer?: string; seats?: number; issued?: string; expires?: string; grace_until?: string; features?: string[]; problem?: "unreadable" | "unsigned";
}
export const getLicence = () => call<LicenceState>("GET", "/licence");
export const putLicence = (licence: string) => call<LicenceState>("PUT", "/licence", { licence });
export const putFrontDoor = (body: FrontDoorEdit) => call<FrontDoorRead>("PUT", "/front-door", body);
// The conversation goes back whole every turn: the server stores none of it. Never streamed, and
// the route is not registered at all on a host with no model, which answers 404.
export const guardrailsSetup = (messages: SetupMessage[]) => call<SetupAnswer>("POST", "/guardrails/setup", { messages });
// What an app called out to (contracts/routes.md, Egress control; egress.schema.json's project_read):
// per env the egress spec its running version was deployed under, null where nothing is, and each
// destination's row; `floor` the app's network. Contributor and up: below it 403 team_required.
export interface EgressDestination { destination: string; port: number; first_seen_at: string; last_seen_at: string; calls: number; last_outcome: "allowed" | "refused"; last_status_class: string; last_protocol: "connect" | "http" | "https" | "intercepted"; last_response_flags: string[] }
export interface EgressGap { from: string; to: string; reason: string; calls: number | null; app: string | null; env: "preview" | "prod" | null }
export type EgressMode = "observe" | "allowlist";
// From layer 2 (#2104) each env also carries the mode that applies, its own (null: it follows the
// org's), its allowlist and its egress requests; the read, the platform destinations every app may call.
// refusing: the env refuses it now, as the fleet's refused tile counts it (contracts/routes.md; #2723).
export interface EgressRequest { destination: string; state: "pending" | "dismissed"; first_refused_at: string; last_refused_at: string; refusals: number; refusing?: boolean }
export interface EgressEnv { via: "off" | "canyon" | null; destinations: EgressDestination[]; mode: EgressMode; app_mode: EgressMode | null; allowed: string[]; requests: EgressRequest[] }
export interface ProjectEgress { since: string; floor: "enforced" | "not_enforced" | null; envs: { preview: EgressEnv; prod: EgressEnv }; gaps: EgressGap[]; platform: string[] }
// The host's egress setting: every member reads via and the org's mode; an admin also the rest (setting_admin).
export interface EgressReplica { replica: string; have: string | null; seen_at: string; current_at: string | null }
export interface EgressSetting { via: "off" | "canyon"; mode: EgressMode; credential_routes: boolean; proxy?: string | null; receiver?: boolean; streams?: number; last_entry_at?: string | null; networks_to_recreate?: string[] | null; networks?: Record<string, "internal" | "open" | "unknown"> | null; canyon_refused?: string | null; replicas?: EgressReplica[]; caps?: EgressCaps }
// The admin's egress caps (#2691), each in its range.
export interface EgressCaps { destinations: number; new_names: number; pending: number }
export const getProjectEgress = (id: string) => call<ProjectEgress>("GET", `/projects/${encodeURIComponent(id)}/egress`);
// A person's, from any session; the destination files no new request (contracts/routes.md, Egress control).
export const dismissEgressRequest = (id: string, env: "preview" | "prod", destination: string) => call<void>("POST", `/projects/${encodeURIComponent(id)}/egress/${env}/requests/${encodeURIComponent(destination)}/dismiss`);
export const getEgress = () => call<EgressSetting>("GET", "/egress");
// The apps on this Canyon (GET /api/v1/projects), by id, with where each runs: `envs` {} when nowhere.
export const listProjectIds = () => call<{ projects: { id: string; envs?: Record<string, unknown> }[] }>("GET", "/projects");
// {via}, {mode}, {caps} or {credential_routes}: a key left out keeps its value. Each egress write below answers once
// every gateway replica holds it, or says which doesn't (GatewayPending).
export const putEgress = (body: { via: EgressSetting["via"] } | { mode: EgressMode } | { caps: Partial<EgressCaps> } | { credential_routes: boolean }) => egressWrite<EgressSetting>("PUT", "/egress", body);
const envEgress = (id: string, env: "preview" | "prod") => `/projects/${encodeURIComponent(id)}/egress/${env}`;
// An app env's own mode, or null to follow the org's: 204.
export const putEgressMode = (id: string, env: "preview" | "prod", mode: EgressMode | null) => egressWrite<void>("PUT", `${envEgress(id, env)}/mode`, { mode }).then((r) => r.pending);
// One allowlist entry, the destination's name: no body, 204.
export const allowDestination = (id: string, env: "preview" | "prod", destination: string) => egressWrite<void>("PUT", `${envEgress(id, env)}/destinations/${encodeURIComponent(destination)}`).then((r) => r.pending);
// Remove, a narrowing: the entry off the env's allowlist (#2561).
export const disallowDestination = (id: string, env: "preview" | "prod", destination: string) => egressWrite<void>("DELETE", `${envEgress(id, env)}/destinations/${encodeURIComponent(destination)}`).then((r) => r.pending);
export const recreateNetwork = (app: string) => call<EgressSetting>("POST", "/egress/networks/recreate", { app });

// Layer 3's screens (#2145). An env's value routes as its read lists them (egress.schema.json's route_read), never a
// value, to any level: `from_secret` one only an owner or an admin replaces or removes, `waiting` why Canyon isn't
// adding it yet, null while it is. A read without `routes` (a server from before them) is null: no list, no form.
export type RouteScheme = "bearer" | "basic" | "raw";
export interface RouteWait { reason: "host" | "secret" | "redeploy"; secret: string | null }
export interface EgressRoute { destination: string; header: string; scheme: RouteScheme; placeholder_env: string | null; from_secret: boolean; waiting: RouteWait | null }
export const routesOf = (env: EgressEnv): EgressRoute[] | null => { const routes = (env as { routes?: unknown }).routes; return Array.isArray(routes) ? (routes as EgressRoute[]) : null; };
const route = (id: string, env: "preview" | "prod", destination: string) => `${envEgress(id, env)}/routes/${encodeURIComponent(destination)}`;
// A route set whole, its value entered with it (route_put): contributor and up, a person in a browser. The value goes in
// this body and nowhere else: no read answers it back.
export const putRoute = (id: string, env: "preview" | "prod", destination: string, body: { header: string; scheme: RouteScheme; value: string; placeholder_env?: string }) =>
  egressWrite<void>("PUT", route(id, env, destination), body).then((r) => r.pending);
// A narrowing, from any session; an owner's for a route a secret placed.
export const removeRoute = (id: string, env: "preview" | "prod", destination: string) => egressWrite<void>("DELETE", route(id, env, destination)).then((r) => r.pending);
// A runtime secret of the env moved into a route by its name (route_from_secret), an owner's: `placeholder_env` left out is
// the secret's own name.
export const routeFromSecret = (id: string, env: "preview" | "prod", destination: string, body: { secret: string; header: string; scheme: RouteScheme; placeholder_env?: string }) =>
  egressWrite<void>("POST", `${route(id, env, destination)}/from-secret`, body).then((r) => r.pending);
// The interception roots' key dates (ca_read), an admin's read of the setting alone, never a key.
export interface EgressRoot { fingerprint: string; byo: boolean; created_at: string; not_after: string; ends_at: string }
export interface EgressCA { signing: string | null; roots: EgressRoot[]; rotates_at: string | null }
export const caOf = (setting: EgressSetting): EgressCA | null => (setting as { ca?: EgressCA }).ca ?? null;
// A rotation (ca_rotate): {} for a root Canyon makes, {cert, key} for an admin's sub-CA. 200, the admin's read.
export const rotateCA = (body: Record<string, never> | { cert: string; key: string }) => call<EgressSetting>("POST", "/egress/ca/rotate", body);
