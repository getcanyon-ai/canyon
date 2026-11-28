import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { allowDestination, ApiError, caOf, getAuthConfig, getEgress, getFrontDoor, getPreferences, getProject, getProjectEgress, getScannerData, listProjectIds, putEgress, putFrontDoor, recreateNetwork, reduced, said, type AddressForm, type EgressDestination, type EgressCaps, type EgressMode, type EgressSetting, type FrontDoor, type FrontDoorEdit, type FrontDoorRead, type Preferences, type ScanDB } from "../api";
import { S } from "../strings";
import { Actor } from "../ui/Actor";
import { Age, age } from "../ui/Age";
import { AskFirst } from "../ui/AskFirst";
import { Button } from "../ui/Button";
import { Chip } from "../ui/Chip";
import { shownIntoView } from "../ui/media";
import { Rail } from "../ui/Rail";
import { GitHubSettings } from "./GitHub";
import { IntegrationsSettings } from "./Integrations";
import { HandOff } from "./HandOff";
import { ConnectAgents } from "./ConnectAgents";
import { isAdmin, OrganisationSettings, PeopleSettings, Sessions } from "./OrgSettings";
import { pendingLine } from "../ui/pending";
import { Saved } from "../ui/Saved";
import { useUnsaved } from "../ui/Unsaved";
import { AllowedLine, allowedLine, Refusals, wouldRefuse } from "./Outbound";
import { DatabasesSettings } from "./Databases";
import { OutboundKeys, subCAEnding } from "./OutboundKeys";
import { PatchingSettings } from "./Patching";
import { TelemetrySettings } from "./Telemetry";
import { LicenceSettings } from "./Licence";
import { EnterpriseLine, unlocks, useLicence } from "../ui/Enterprise";

const LABEL = "mt-3 mb-1 block text-xs tracking-caps text-text-faint uppercase";
const FIELD = "w-full max-w-[520px] rounded-lg border border-border bg-surface px-2.5 py-1.5 text-base";
const NOTE = "mt-1 text-md text-text-dim wrap-anywhere"; // a note can name an address longer than a narrow window is wide (#129)
const CARD = "mt-3 rounded-xl border border-border bg-surface-2 px-3.5 py-3";

// The load balancer's own session cookie, which a PUT leaves out (contracts/routes.md, the front-door row).
const ALB_COOKIE = "AWSELBAuthSessionCookie";
const SAMPLE = "claims-portal";
const CHOICES = [[1, S.gateway, S.gatewayNote], [2, S.proxy, S.proxyNote]] as const;
const GATEWAYS = [["oidc", "Standard OIDC gateway"], ["alb", "AWS load balancer sign-in"]] as const;
const FORMS: AddressForm[] = ["app.env.domain", "app-env.domain"];

// An app's address under each form, and the names the probe says its certificate must cover
// (contracts/executor.md, POST /probe).
const address = (form: AddressForm, app: string, env: string, domain: string) => (form === "app.env.domain" ? `${app}.${env}.${domain}` : `${app}-${env}.${domain}`);
const certificate = (form: AddressForm, domain: string) => S.certificate.replace("<names>", () => (form === "app.env.domain" ? `*.preview.${domain} and *.prod.${domain}` : `*.${domain}`));
const example = (form: AddressForm, domain: string) => `https://${address(form, SAMPLE, "preview", domain)}`;

// The form holds strings, one per field, seeded from what the server stored and never from the
// secret, which the read does not carry. `replacing` is an admin asking to type a new one.
function draftOf({ default_domain, front_door: door }: FrontDoorRead) {
  return {
    choice: door?.choice ?? null, gateway: door?.gateway ?? "oidc",
    issuer: door?.issuer ?? "", audience: door?.audience ?? "", jwks_url: door?.jwks_url ?? "",
    cookie: door?.cookie ?? (door?.gateway === "alb" ? ALB_COOKIE : ""), alb_arn: door?.alb_arn ?? "",
    signin_issuer: door?.signin?.issuer ?? "", client_id: door?.signin?.client_id ?? "", secret: "", replacing: false,
    domain: door?.domain ?? default_domain, form: door?.form ?? ("app.env.domain" as AddressForm),
  };
}
type Draft = ReturnType<typeof draftOf>;
type Text = "issuer" | "audience" | "jwks_url" | "cookie" | "alb_arn" | "signin_issuer" | "client_id" | "domain";

// The front door goes whole, with only the chosen way's keys. A domain equal to the default is the
// key left out, which the server reads as the default.
function bodyOf(draft: Draft, defaultDomain: string): FrontDoorEdit {
  const where = { ...(draft.domain === "" || draft.domain === defaultDomain ? {} : { domain: draft.domain }), form: draft.form };
  if (draft.choice === 2) {
    return { choice: 2, signin: { issuer: draft.signin_issuer, client_id: draft.client_id, ...(draft.secret === "" ? {} : { client_secret: draft.secret }) }, ...where };
  }
  if (draft.gateway === "alb") return { choice: 1, gateway: "alb", alb_arn: draft.alb_arn, ...(draft.cookie === ALB_COOKIE ? {} : { cookie: draft.cookie }), ...where };
  return { choice: 1, gateway: "oidc", issuer: draft.issuer, audience: draft.audience, jwks_url: draft.jwks_url, cookie: draft.cookie, ...where };
}

// The docs on closing an app's way to its own node, Host firewall (#118).
const FIREWALL_DOCS = "https://www.getcanyon.ai/docs/self-host/kubernetes/#host-firewall";

// The pages of Settings (the redesign): who the org is, who may sign in and who administers it, how
// people reach apps, whether AI review is on, and, for admins, the GitHub organizations. Each is #/settings/<page>; the bare route
// is the first.
const PAGES = [
  ["organisation", "Organization"], // the address keeps its old spelling, so links to it work (#118)
  ["people", "People & sign-in"],
  ["agents", "Agents"], // every member's: connecting an agent, and the sessions to disconnect one (#100's review)
  ["hosting", "App hosting"],
  ["ai", "AI review"],
  ["github", "GitHub"], // admins' alone (#1914)
  ["outbound", "Outbound"], // every member reads it, an admin changes it (design/EGRESS.md §8.3; #2422)
  ["integrations", "Integrations"], // admins' alone (design/INTEGRATIONS.md; #2725)
  ["telemetry", "Telemetry"], // admins' alone (#2807): a member's screen never asks its routes
  ["licence", "Licence"], // admins' alone (#2879): a member's screen never asks its routes
  ["databases", "Databases"], // every member reads it, an admin turns it on or off (#2835)
] as const;
type Page = (typeof PAGES)[number][0];
const pageOf = (hash: string): Page => PAGES.find(([key]) => hash.startsWith(`#/settings/${key}`))?.[0] ?? "organisation";


// toFrontDoor takes the checklist's step to the front door below it: scrolled to and focused, never a link, since
// Settings routes on the hash and #front-door is no page of it.
const toFrontDoor = () => {
  const heading = document.getElementById("front-door");
  heading?.scrollIntoView?.({ block: "start" });
  heading?.focus();
};

// Settings. The org's row and the front door are read once, with whether people sign in here: no
// poll, these pages are forms. Who is an admin is the preferences read's, as everywhere.
export function Settings() {
  const [record, setRecord] = useState<Preferences | null>(null);
  const [read, setRead] = useState<FrontDoorRead | null>(null);
  const [auth, setAuth] = useState<"oidc" | "off" | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  const live = useRef(true);
  // Each read lands on its own: a front door that fails to read leaves the organisation's pages
  // standing, and the failure line names the one that failed.
  const load = () => Promise.allSettled([getFrontDoor(), getPreferences(), getAuthConfig()]).then(([door, row, config]) => {
    if (!live.current) return;
    if (door.status === "fulfilled") setRead(door.value);
    if (row.status === "fulfilled") setRecord(row.value);
    if (config.status === "fulfilled") setAuth(config.value.auth);
    const failed = [door, row, config].find((one) => one.status === "rejected");
    setFailure(failed === undefined ? null : said(failed.reason));
  });
  useEffect(() => { live.current = true; void load(); return () => { live.current = false; }; }, []);
  const page = pageOf(window.location.hash);
  const admin = record !== null && isAdmin(record);
  // The tab shown, centred in its strip once the strip holds every tab it will (an admin's are more), and again for each
  // tab shown: never on another re-render, which would yank the page back up (#100's review).
  useLayoutEffect(() => { shownIntoView("Settings"); }, [page, admin]);

  return (
    <div className="flex min-h-screen items-start max-[860px]:block">
      <Rail current="settings" />
      <main className="mx-auto grid max-w-[1280px] min-w-0 flex-1 grid-cols-[minmax(0,1fr)] content-start gap-4 px-8 pt-7 pb-24 max-[860px]:px-4 max-[860px]:pt-4">
        <h1 className="text-title font-semibold tracking-tight">Settings</h1>
        <nav aria-label="Settings" className="flex gap-5 overflow-x-auto border-b border-border max-[860px]:-mx-4 max-[860px]:px-4 max-[520px]:gap-4">
          {PAGES.filter(([key]) => (key !== "github" && key !== "integrations" && key !== "telemetry" && key !== "licence") || admin).map(([key, label]) => (
            <a key={key} href={`#/settings/${key}`} aria-current={page === key ? "page" : undefined} aria-describedby={key === "outbound" ? "settings-outbound-beta" : undefined}
              className={`flex items-center gap-1.5 py-2.5 text-base font-medium whitespace-nowrap no-underline ${page === key ? "text-text shadow-[inset_0_-2px_0_var(--color-accent)]" : "text-text-faint hover:text-text"}`}>
              {label}
              {key === "outbound" && <>{" "}<Chip tone="beta" why="settings-outbound-beta">{S.beta}</Chip></>}
            </a>
          ))}
        </nav>
        {failure !== null && (
          <span className="flex flex-wrap items-center gap-2">
            <span className="text-base text-critical">{failure}</span>
            <Button variant="ghost" size="sm" onClick={() => void load()}>Retry</Button>
          </span>
        )}
        {record !== null && page === "organisation" && <OrganisationSettings record={record} adopt={setRecord} />}
        {record !== null && page === "people" && (
          <section aria-label="People & sign-in">
            {/* With sign-in off there is nobody to admit and every caller is an admin: forms here
                would do nothing (the audit's C9). */}
            {auth === "off" ? <p className={NOTE}>{S.signInOff}</p> : <PeopleSettings record={record} adopt={setRecord} />}
          </section>
        )}
        {/* Connect an agent directly above Sessions, so an agent is connected and disconnected in one place, on a tab
            every member finds by name (screens.md; #100's review). */}
        {page === "agents" && auth !== null && (
          <section aria-label="Agents" className="grid gap-2">
            <ConnectAgents signIn={auth === "oidc"} />
            {auth !== "off" && <Sessions admin={admin} />}
          </section>
        )}
        {read !== null && record !== null && page === "hosting" && read.runtime === "kubernetes" && (
          <HandOff read={read} record={record} admin={admin} adopt={setRecord} reload={load} toFrontDoor={toFrontDoor} />
        )}
        {read !== null && page === "hosting" && (
          <section aria-label="How people reach apps">
            <h2 id="front-door" tabIndex={-1} className="text-base font-semibold">How people reach apps</h2>
            {read.runtime === "docker"
              ? <p className={NOTE}>{S.frontDoorDocker}</p>
              : (
                <>
                  <p className={NOTE}>{S.frontDoor}</p>
                  {!admin && <p className={NOTE}>{S.frontDoorAdmin}</p>}
                  {read.front_door === null
                    ? <p className={NOTE}>{S.frontDoorNone}</p>
                    : <p className={NOTE}><Actor actor={read.front_door.set_by} /> · <Age at={read.front_door.set_at} /></p>}
                  {/* The probe's warning in plain words, the docs a link away (#118): its whole text, ports and all, is canyon status's. */}
                  {read.warning !== "" && <p role="note" className="text-md text-amber">{S.reachesNode} <a className="text-link no-underline hover:underline" href={FIREWALL_DOCS} target="_blank" rel="noreferrer">{S.reachesNodeHow}</a></p>}
                  {read.grants_note !== "" && <p role="note" className="text-md text-amber">{read.grants_note}</p>}
                  {read.egress_note !== "" && <p role="note" className="text-md text-amber">{read.egress_note}</p>}
                  {read.discovery_note !== "" && <p role="note" className="text-md text-amber">{read.discovery_note}</p>}
                  {read.probe_note !== "" && <p role="note" className="text-md text-amber">{read.probe_note}</p>}
                  {admin ? <Form read={read} adopt={setRead} /> : read.front_door !== null && <Stored door={read.front_door} />}
                </>
              )}
          </section>
        )}
        {/* Patching is an admin's to read and change (design/PERMISSIONS.md, Platform patches): a member sees none of it. */}
        {record !== null && page === "hosting" && admin && <ScannerData />}
        {record !== null && page === "hosting" && admin && <PatchingSettings record={record} adopt={setRecord} />}
        {page === "github" && (admin ? <GitHubSettings /> : record !== null && <p className={NOTE}>{S.githubAdmin}</p>)}
        {page === "outbound" && record !== null && (admin ? <OutboundSettings /> : <OutboundRead />)}
        {page === "integrations" && (admin ? <IntegrationsSettings /> : record !== null && <p className={NOTE}>{S.integrationsAdmin}</p>)}
        {page === "telemetry" && (admin ? <TelemetrySettings /> : record !== null && <p className={NOTE}>{S.telemetryAdmin}</p>)}
        {page === "licence" && (admin ? <LicenceSettings /> : record !== null && <p className={NOTE}>{S.licenceAdmin}</p>)}
        {page === "databases" && record !== null && <DatabasesSettings record={record} admin={admin} adopt={setRecord} />}
        {record !== null && page === "ai" && (
          <section aria-label="AI review">
            <p className={NOTE}>{record.ai_review ? S.aiOn : S.aiOff}</p>
          </section>
        )}
      </main>
    </div>
  );
}

// SCAN_DATA is each vulnerability database by the name a person knows it by, and the tool a review names (D5).
const SCAN_DATA: Record<string, [string, string]> = {
  "trivy": ["trivy's vulnerability data", "trivy"],
  "trivy-java": ["trivy's Java data", "trivy"],
  "grype": ["grype's vulnerability data", "grype"],
};

// staleWords is what an admin is told of a database that turns stale within a day of now, or is stale, as canyon
// status says it (D5; #2743): null for any other, one with no age and a tool this screen doesn't know.
export function staleWords(db: ScanDB, now: number): string | null {
  const names = SCAN_DATA[db.tool];
  const stale = db.stale_at === undefined ? NaN : Date.parse(db.stale_at);
  if (names === undefined || Number.isNaN(stale) || stale - now > DAY) return null;
  const said = (line: string) => line.replace("<data>", names[0]).replace("<tool>", names[1]);
  if (now >= stale) return said(S.scannerStale);
  const minutes = Math.round((stale - now) / 60_000);
  return said(S.scannerStaleSoon).replace("<left>", minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h`);
}
const DAY = 24 * 60 * 60 * 1000;

// ScannerData warns an admin, first on App hosting, of each scanner's data about to turn stale or stale: a database
// more than 5 days old makes every review "could not check" with its tool (#2743). A read that fails says nothing,
// as an older host's does: the reviews themselves still say why they couldn't check.
function ScannerData() {
  const [dbs, setDbs] = useState<ScanDB[]>([]);
  useEffect(() => {
    let live = true;
    getScannerData().then((read) => { if (live) setDbs(read.databases ?? []); }, () => {});
    return () => { live = false; };
  }, []);
  const lines = dbs.map((db) => staleWords(db, Date.now())).filter((line): line is string => line !== null);
  if (lines.length === 0) return null;
  return (
    <section aria-label="Scanner data">
      {lines.map((line) => <p key={line} className="mt-1 text-md text-amber">{line}</p>)}
    </section>
  );
}

// A member's read: what is set, and no control, since the PUT answers them 403.
function Stored({ door }: { door: FrontDoor }) {
  const rows = door.choice === 2
    ? [["Issuer", door.signin?.issuer], ["Client ID", door.signin?.client_id]]
    : [["Gateway", GATEWAYS.find(([one]) => one === door.gateway)?.[1]], ["Issuer", door.issuer], ["Audience", door.audience], ["JWKS URL", door.jwks_url],
      ["Load balancer ARN", door.alb_arn], ["Session cookie", door.cookie ?? (door.gateway === "alb" ? ALB_COOKIE : undefined)]];
  return (
    <div className={CARD}>
      <p className="text-base font-semibold">{door.choice === 2 ? S.proxy : S.gateway}</p>
      {rows.filter(([, value]) => value !== undefined).map(([label, value]) => <p key={label} className="mt-1 font-mono text-sm">{`${label}: ${value}`}</p>)}
      <p className="mt-1 font-mono text-sm">{example(door.form, door.domain)}</p>
      <p className={NOTE}>{certificate(door.form, door.domain)}</p>
    </div>
  );
}

// The admin's form. Saving runs the executor's probe, which can take a minute: the fields stay on
// screen, locked, under the line saying what is being checked, and the keyboard stays on Save. A
// refusal is the server's sentence — for a failed probe, the fix it names — and the form keeps what
// was typed, the secret included, so the admin fixes one thing and saves again.
function Form({ read, adopt }: { read: FrontDoorRead; adopt: (answer: FrontDoorRead) => void }) {
  const [draft, setDraft] = useState(() => draftOf(read));
  const [status, setStatus] = useState<null | "checking" | "saved" | { refusal: string }>(null);
  const checking = status === "checking";
  const domain = draft.domain === "" ? read.default_domain : draft.domain;
  const stored = read.front_door?.signin;
  const secretSet = stored !== undefined && !draft.replacing && stored.issuer === draft.signin_issuer && stored.client_id === draft.client_id;
  // Typed, so it has its own Save, shown only while the form differs from what is saved, and leaving asks first (D16).
  const changed = JSON.stringify(draft) !== JSON.stringify(draftOf(read));
  useUnsaved(changed);

  // Any edit makes "Saved." stale; a refusal stays until the next Save answers.
  const edit = (patch: Partial<Draft>) => {
    setDraft((was) => ({ ...was, ...patch }));
    setStatus((was) => (was === "saved" ? null : was));
  };
  // The load balancer's cookie is filled in for it and taken out again for an OIDC gateway, unless
  // the admin typed another.
  const gateway = (to: "oidc" | "alb") => edit({ gateway: to, ...(draft.cookie === "" || draft.cookie === ALB_COOKIE ? { cookie: to === "alb" ? ALB_COOKIE : "" } : {}) });
  const save = async () => {
    if (checking) return;
    setStatus("checking");
    try {
      const answer = await putFrontDoor(bodyOf(draft, read.default_domain));
      adopt(answer);
      setDraft(draftOf(answer));
      setStatus("saved");
    } catch (err) {
      setStatus({ refusal: said(err) });
    }
  };
  const field = (key: Text, label: string) => (
    <>
      <label className={LABEL} htmlFor={`front-door-${key}`}>{label}</label>
      <input id={`front-door-${key}`} className={FIELD} value={draft[key]} onChange={(event) => edit({ [key]: event.target.value })} />
    </>
  );

  return (
    <>
      <fieldset disabled={checking} className="mt-3 min-w-0">{/* a fieldset is as wide as its content by default: at 320px it widened the page (#129) */}
        <legend className={LABEL}>Who signs people in</legend>
        {CHOICES.map(([value, word, note]) => (
          <div key={value} className="mt-2">
            <label className="flex items-center gap-2 text-base font-semibold">
              <input type="radio" name="front-door-choice" checked={draft.choice === value} aria-describedby={`front-door-choice-${value}`} onChange={() => edit({ choice: value })} />
              {word}
            </label>
            <p id={`front-door-choice-${value}`} className={`${NOTE} ml-6`}>{note}</p>
          </div>
        ))}
        {draft.choice === 1 && (
          <div className={CARD}>
            <fieldset>
              <legend className={LABEL}>Your gateway</legend>
              <span className="flex flex-wrap gap-4">
                {GATEWAYS.map(([value, word]) => (
                  <label key={value} className="flex items-center gap-1.5 text-base">
                    <input type="radio" name="front-door-gateway" checked={draft.gateway === value} onChange={() => gateway(value)} />
                    {word}
                  </label>
                ))}
              </span>
            </fieldset>
            {draft.gateway === "oidc"
              ? <>{field("issuer", "Issuer")}{field("audience", "Audience")}{field("jwks_url", "JWKS URL")}</>
              : field("alb_arn", "Load balancer ARN")}
            {field("cookie", "Session cookie")}
            <p className={NOTE}>{S.cookieNote}</p>
            {draft.gateway === "alb" && <p className={NOTE}>{S.idpResponse.replace("<app address>", () => address(draft.form, "<app>", "<env>", domain))}</p>}
          </div>
        )}
        {draft.choice === 2 && (
          <div className={CARD}>
            {field("signin_issuer", "Issuer")}
            {field("client_id", "Client ID")}
            {secretSet
              ? (
                <span className="mt-3 flex flex-wrap items-center gap-2 text-base">
                  {S.secretSet.replace("<client_id>", () => stored.client_id)}
                  <Button variant="ghost" size="sm" onClick={() => edit({ replacing: true })}>Replace the secret</Button>
                </span>
              )
              : (
                <>
                  <label className={LABEL} htmlFor="front-door-secret">Client secret</label>
                  {/* Never seeded: the read carries no secret, and an admin replacing one starts typing here. */}
                  <input id="front-door-secret" type="password" autoComplete="new-password" autoFocus={draft.replacing} className={FIELD}
                    value={draft.secret} onChange={(event) => edit({ secret: event.target.value })} />
                  <p className={NOTE}>{S.secretAsk}</p>
                </>
              )}
            <p className={NOTE}>{S.callbacks.replace("<address>", () => address(draft.form, "<app>", "<env>", domain))}</p>
          </div>
        )}
        {field("domain", "Domain")}
        <fieldset className="mt-3 min-w-0">
          <legend className={LABEL}>App addresses</legend>
          {FORMS.map((form) => (
            <label key={form} className="mt-1 flex items-center gap-2 font-mono text-base">
              <input type="radio" name="front-door-form" checked={draft.form === form} onChange={() => edit({ form })} />
              <span className="min-w-0 wrap-anywhere">{example(form, domain)}</span>
            </label>
          ))}
        </fieldset>
        <p className={NOTE}>{certificate(draft.form, domain)}</p>
      </fieldset>
      {/* The check is said as it runs, then "Saved." in its place: one status line at a time, announced as it changes. */}
      {draft.choice !== null && changed && (checking ? <p role="status" className="mt-4 text-base text-text-dim">{S.checking}</p> : (
        <span className="mt-4 flex"><Button variant="primary" onClick={() => void save()}>{S.frontDoorSave}</Button></span>
      ))}
      {typeof status === "object" && status !== null && <p role="alert" className="mt-2 text-base text-critical">{status.refusal}</p>}
      {status === "saved" && !changed && <Saved then={S.frontDoorSavedThen} />}
    </>
  );
}

const VIA = [["off", S.viaOff, S.viaOffNote], ["canyon", S.viaCanyon, S.viaCanyonNote]] as const;
// The apps a host's via off still sends through Canyon, their network from before the change: each named, the last two
// joined by "and" (#2723).
const joined = (apps: string[]) => (apps.length === 1 ? apps[0]! : `${apps.slice(0, -1).join(", ")} and ${apps.at(-1)!}`);
const stillThrough = (apps: string[]) => apps.length === 1 ? S.viaOffStillOne.replace("<app>", () => apps[0]!)
  : S.viaOffStill.replace("<apps>", () => joined(apps));
const MODES = [["observe", S.modeObserve, S.modeObserveNote], ["allowlist", S.modeAllowlist, S.modeAllowlistNote]] as const;
// Credentials' two states as a member reads them (#2143), off first, as a host starts.
const ROUTES = [[false, S.routesOff, S.routesOffNote], [true, S.routesOn, S.routesOnNote]] as const;
const known = (mode: unknown): mode is EgressMode => MODES.some(([value]) => value === mode);
const ENV_NAMES = [["preview", "Preview"], ["prod", "Production"]] as const;

// What allowlist would refuse: each app env that follows the org's mode and runs through Canyon, with the rows
// allowlist would refuse there (Outbound's wouldRefuse). Each app's own read holds its envs' destinations, modes
// and allowlists and the platform's; every app the list holds is read but one that runs nothing anywhere (its
// row's envs empty), which refuses nothing. The fleet's destinations read (GET /api/v1/egress/destinations) is
// not served yet (#2345). `anyVia` counts every env that follows the org's mode whatever its via: while the host's
// via is off none runs through Canyon, and each will at its next deploy (#2422's review).
// Fail closed: a read that fails or answers out of shape throws, and the page says it can't say.
type Refused = { app: string; env: "preview" | "prod"; name: string; rows: EgressDestination[] };
async function refusals(anyVia: boolean): Promise<Refused[]> {
  const list = await listProjectIds();
  if (!Array.isArray(list?.projects)) throw new Error("the apps are out of shape");
  const apps = list.projects.filter((project) => project.envs === undefined || Object.keys(project.envs).length > 0).map((project) => project.id).sort();
  const reads = await Promise.all(apps.map(async (app) => [app, await getProjectEgress(app)] as const));
  return reads.flatMap(([app, egress]) => {
    if (!Array.isArray(egress?.platform)) throw new Error(`${app}'s outbound is out of shape`);
    return ENV_NAMES.flatMap(([key, name]) => {
      const env = egress.envs?.[key];
      if (!Array.isArray(env?.destinations) || !Array.isArray(env.allowed) || !(env.app_mode === null || known(env.app_mode))) throw new Error(`${app}'s outbound is out of shape`);
      const rows = env.app_mode === null && (anyVia || env.via === "canyon") ? wouldRefuse(env, egress.platform) : [];
      return rows.length === 0 ? [] : [{ app, env: key, name, rows }];
    });
  });
}

// Settings › Outbound (design/EGRESS.md §8.3), an admin's: the host's via, the org's mode, and the networks
// to recreate, each with its Recreate, asked first — it stops the app. One change at a time (a ref, so a
// second click sends nothing), every Save and Recreate off meanwhile; after any recreation's answer the list
// is read again, as a failed one may have changed what is listed, and so is the setting after a Save that
// failed. Where the host would refuse Through Canyon its reason is said before the choice, which is off
// (#2300). The mode is off while the host's via is off (409 floor_not_enforced). Whatever starts refusing calls
// — the mode to allowlist, or Through Canyon while the org's mode is allowlist, which the server keeps while via
// is off — lists what it would refuse, each with Allow, and asks in the page before anything is sent (#2422);
// the question takes the focus Save had.
// Fail closed: a read again that fails shows no choice at all.
function OutboundSettings() {
  const licence = useLicence(); // turning credentials on is Canyon Enterprise's (#2879); turning them off never needs it
  const [read, setRead] = useState<EgressSetting | null>(null);
  const [via, setVia] = useState<EgressSetting["via"]>("off"); // the choice, the host's until changed
  const [mode, setMode] = useState<EgressMode>("observe"); // the same, of the org's mode
  const [routes, setRoutes] = useState(false); // the same, of whether Canyon adds credentials
  const [routesSaved, setRoutesSaved] = useState(false);
  const [routesPending, setRoutesPending] = useState<string | null>(null); // the same, of credential routes (#2789)
  const [impact, setImpact] = useState<null | "reading" | "unread" | Refused[]>(null);
  const [modeSaved, setModeSaved] = useState(false);
  const [viaSaved, setViaSaved] = useState(false);
  // a gateway replica that hadn't taken a saved change, said in place of its confirmation (#2789): via's and the mode's
  const [viaPending, setViaPending] = useState<string | null>(null);
  const [modePending, setModePending] = useState<string | null>(null);
  const [allowed, setAllowed] = useState(""); // Allowed <destination>, once an Allow in the list is saved
  const [asking, setAsking] = useState<string | null>(null); // the app whose Recreate asks first
  // The envs Canyon runs of the app asked about, which the recreation starts again (the app's read, #2774): the question
  // names them, and none while the read is unanswered or failed.
  const [restarts, setRestarts] = useState<{ app: string; envs: ("prod" | "preview")[] } | null>(null);
  const askAbout = (app: string) => {
    setAsking(app);
    getProject(app).then(({ project }) => {
      if (!live.current || reduced(project)) return;
      setRestarts({ app, envs: (["prod", "preview"] as const).filter((key) => project.envs[key] !== undefined && project.envs[key]!.run_by !== "org") });
    }, () => {});
  };
  const askLine = (app: string) => {
    const envs = restarts?.app === app ? restarts.envs : [];
    return (envs.length === 2 ? S.recreateAsk : envs[0] === "prod" ? S.recreateAskProd : envs[0] === "preview" ? S.recreateAskPreview : S.recreateAskUnread).replace("<app>", () => app);
  };
  const [busy, setBusy] = useState<string | null>(null); // the line while a change runs
  const [refusal, setRefusal] = useState<string | null>(null);
  const [held, setHeld] = useState<string | null>(null); // the line before the server's when a hold refused the save (409 held)
  const [caps, setCaps] = useState<Record<keyof EgressCaps, string> | null>(null); // the limits as typed, the host's until changed
  const [capsSaved, setCapsSaved] = useState(false);
  const [capsPending, setCapsPending] = useState<string | null>(null); // the same, of the limits (#2789)
  const sending = useRef(false);
  const live = useRef(true);
  const impactRead = useRef(0); // reads overlap: only the newest one's answer is drawn
  // Advanced (#118) opens by itself when a read lands that needs a person, judged as it lands and never again as the page
  // redraws; nothing here closes it. `landed` is when, so a replica's age says what the read said.
  const [advanced, setAdvanced] = useState(false);
  const capsTyped = useRef(false); // limits typed since they were last saved or read
  const [landed, setLanded] = useState(0);
  const adopt = (answer: EgressSetting) => {
    if (!live.current) return;
    const now = Date.now();
    setLanded(now);
    if (needsPerson(answer, now)) setAdvanced(true);
    setRead(answer); setVia(answer.via); setMode(answer.mode); setRoutes(answer.credential_routes);
    // Limits typed and not saved stay as typed: another section's save, or a read again, never takes them (D16).
    if (!capsTyped.current) setCaps(answer.caps === undefined ? null : { new_names: String(answer.caps.new_names), destinations: String(answer.caps.destinations), pending: String(answer.caps.pending) });
  };
  // Each listed app's network, internal, open, or one Canyon can't tell (a recreation under way or one that didn't finish),
  // from this same read (contracts/routes.md, GET /api/v1/egress's networks; #2767). A read that names none, or an
  // app it lists without one, sorts no app (null): fail closed.
  const named = read?.networks;
  const networks = named !== null && named !== undefined && Array.isArray(read?.networks_to_recreate) && read.networks_to_recreate.every((app) => Object.hasOwn(named, app)) ? named : null; // own keys: an app may be called constructor
  const reread = () => getEgress().then(adopt, (err: unknown) => { if (live.current) { setRead(null); setRefusal((was) => was ?? said(err)); } });
  useEffect(() => { live.current = true; void reread(); return () => { live.current = false; }; }, []);
  const change = async (line: string, send: () => Promise<EgressSetting>, after?: () => Promise<void>, heldLine?: string) => {
    if (sending.current) return false;
    sending.current = true;
    setBusy(line);
    setRefusal(null);
    setHeld(null);
    let taken = false;
    try {
      adopt(await send());
      taken = true;
    } catch (err) {
      if (live.current) {
        setRefusal(said(err));
        if (err instanceof ApiError && err.status === 409 && (err.body as { error?: string } | null)?.error === "held") setHeld(heldLine ?? null);
      }
    }
    if (live.current) await after?.(); // a page left meanwhile reads nothing more
    sending.current = false;
    if (live.current) { setBusy(null); setAsking(null); }
    return taken;
  };
  const save = () => putEgress({ via }).then(({ answer, pending }) => { if (live.current) setViaPending(pendingLine(pending)); return answer; })
    .catch(async (err: unknown) => { await reread(); throw err; });
  const saveVia = async () => {
    const taken = await change("Saving…", save, undefined, S.viaHeld);
    if (taken && live.current) setViaSaved(true);
  };
  const recreate = (app: string) => void change(S.recreating.replace("<app>", () => app), () => recreateNetwork(app), reread);
  // Through Canyon under an org on allowlist starts refusing at each app's next deploy: weighed and asked as a mode switch is.
  const viaRefuses = read !== null && read.via === "off" && via === "canyon" && read.mode === "allowlist";
  const weigh = async (anyVia: boolean, quiet = false) => {
    const asked = ++impactRead.current;
    if (!quiet) setImpact("reading");
    const answer = await refusals(anyVia).catch(() => "unread" as const);
    if (live.current && asked === impactRead.current) setImpact(answer);
  };
  const chooseVia = (to: EgressSetting["via"]) => {
    setVia(to);
    setViaSaved(false);
    setAllowed("");
    setViaPending(null);
    if (to === "canyon" && read?.via === "off" && read.mode === "allowlist") void weigh(true);
  };
  const choose = (to: EgressMode) => {
    setMode(to);
    setModeSaved(false);
    setModePending(null);
    setAllowed("");
    if (to === "allowlist" && read?.mode !== "allowlist") void weigh(false);
  };
  const allow = (app: string, env: "preview" | "prod") => async (name: string) => {
    const pending = await allowDestination(app, env, name);
    if (live.current) setAllowed(pendingLine(pending) ?? allowedLine(name));
    await weigh(viaRefuses, true);
  };
  const saveRoutes = async () => {
    const taken = await change("Saving…", () => putEgress({ credential_routes: routes }).then(({ answer, pending }) => { if (live.current) setRoutesPending(pendingLine(pending)); return answer; })
      .catch(async (err: unknown) => { await reread(); throw err; }));
    if (taken && live.current) setRoutesSaved(true);
  };
  const saveMode = async () => {
    const taken = await change("Saving…", () => putEgress({ mode }).then(({ answer, pending }) => { if (live.current) setModePending(pendingLine(pending)); return answer; })
      .catch(async (err: unknown) => { await reread(); throw err; }), undefined, S.modeHeld);
    if (taken && live.current) setModeSaved(true);
  };
  // The limits that moved, each a whole number in its range, else none: a value out of it is never sent.
  const capsMoved = (): Partial<EgressCaps> | null => {
    if (caps === null || read?.caps === undefined) return null;
    const moved: Partial<EgressCaps> = {};
    for (const [key, , , [min, max]] of CAPS) {
      if (!/^[0-9]+$/.test(caps[key])) return null;
      const n = Number(caps[key]);
      if (n < min || n > max) return null;
      if (n !== read.caps[key]) moved[key] = n;
    }
    return Object.keys(moved).length > 0 ? moved : null;
  };
  // A limit typed and not saved, one Save won't send included: the folded Advanced says so (#139).
  const capsUnsaved = () => {
    const saved = read?.caps;
    return caps !== null && saved !== undefined && CAPS.some(([key]) => !/^[0-9]+$/.test(caps[key]) || Number(caps[key]) !== saved[key]);
  };
  useUnsaved(capsUnsaved());
  const saveCaps = async (moved: Partial<EgressCaps>) => {
    const taken = await change("Saving…", () => putEgress({ caps: moved }).then(({ answer, pending }) => { capsTyped.current = false; if (live.current) setCapsPending(pendingLine(pending)); return answer; })
      .catch(async (err: unknown) => { await reread(); throw err; }));
    if (taken && live.current) setCapsSaved(true);
  };
  const listed = read?.networks_to_recreate; // null on Kubernetes, which has no app network to recreate
  const refused = typeof read?.canyon_refused === "string";
  const through = read?.via === "canyon";
  const count = Array.isArray(impact) ? impact.reduce((sum, group) => sum + group.rows.length, 0) : 0;
  const ask = !Array.isArray(impact) || count === 0 ? S.modeAskNone : count === 1 ? S.modeAskOne : S.modeAsk.replace("<n>", () => String(count));
  // What would be refused, said as it is read (polite: the count changes as Allow narrows it). Under a via change
  // nothing was recorded while via was off, so it never says nothing would be refused: only what it knows.
  const weighed = (unseen: boolean) => (
    <>
      <div aria-live="polite" className="grid gap-2">
        {impact === "reading" || impact === null ? <p className={NOTE}>Reading what apps called…</p>
          : impact === "unread" ? <p role="note" className="text-md text-amber">{S.modeUnread}</p>
            : count === 0 ? !unseen && <p className={NOTE}>{S.modeRefusesNone}</p>
              : <p className="text-base">{count === 1 ? S.modeRefusesOne : S.modeRefuses.replace("<n>", () => String(count))}</p>}
      </div>
      {Array.isArray(impact) && impact.map((group) => (
        <section key={`${group.app}:${group.env}`} aria-label={`${group.app} ${group.name}`} className="mt-1">
          <h3 className="text-md font-medium"><span className="font-mono">{group.app}</span> · {group.name}</h3>
          <Refusals rows={group.rows} allow={allow(group.app, group.env)} />
        </section>
      ))}
      <AllowedLine line={allowed} />
    </>
  );
  const reading = impact === "reading" || impact === null;
  return (
    <section aria-label="Outbound">
      {refusal !== null && held !== null && <div role="alert" className="grid gap-1 text-base text-critical"><p>{held}</p><p>{refusal}</p></div>}
      {refusal !== null && held === null && refusal !== read?.canyon_refused && <p role="alert" className="text-base text-critical">{refusal}</p>}
      {read !== null && (
        <fieldset disabled={busy !== null} className="min-w-0">
          <fieldset aria-describedby="egress-via-beta">
            <legend className="text-base font-semibold">{S.via} <Chip tone="beta" why="egress-via-beta">{S.beta}</Chip></legend>
            {refused && <p role={refusal === read.canyon_refused ? "alert" : "note"} className="text-md text-amber">{read.canyon_refused}</p>}
            {VIA.map(([value, word, note]) => (
              <div key={value} className="mt-2">
                <label className="flex items-center gap-2 text-base font-semibold has-disabled:text-text-faint">
                  <input type="radio" name="egress-via" checked={via === value} disabled={refused && value === "canyon" && read.via !== "canyon"} aria-describedby={`egress-via-${value}`} onChange={() => chooseVia(value)} />
                  {word}
                </label>
                <p id={`egress-via-${value}`} className={`${NOTE} ml-6`}>{note}</p>
                {/* Off while apps on their old network still go through Canyon: named, as the list below names them (#2723), and
                    once the map is read, one Canyon can't tell the network of said so, never that it goes through Canyon (D15);
                    while it is read, every listed app as the list says it, never "Apps call out directly." alone. */}
                {value === "off" && read.via === "off" && Array.isArray(listed) && listed.length > 0 && networks === null && <p role="note" className="ml-6 text-md text-amber">{S.networksUnread.replace("<apps>", () => joined(listed))}</p>}
                {value === "off" && read.via === "off" && Array.isArray(listed) && networks !== null && listed.some((app) => networks?.[app] !== "unknown") && <p role="note" className="ml-6 text-md text-amber">{stillThrough(listed.filter((app) => networks?.[app] !== "unknown"))}</p>}
                {value === "off" && read.via === "off" && Array.isArray(listed) && networks !== null && listed.filter((app) => networks?.[app] === "unknown").map((app) => (
                  <p key={app} role="note" className="ml-6 text-md text-amber">{`${S.needRecreateUnknown.replace("<app>", () => app)} ${S.needRecreateUnknownStopped}`}</p>
                ))}
                {/* On Kubernetes (networks_to_recreate null) an app keeps the policy it was deployed under until its next deploy. */}
                {value === "off" && read.via === "off" && listed === null && <p role="note" className="ml-6 text-md text-amber">{S.viaOffKube}</p>}
              </div>
            ))}
          </fieldset>
          {/* A choice that changes what apps can do asks first, saying what changes (D16): nothing is sent before yes. */}
          {via !== read.via && !viaRefuses && (
            <AskFirst question={via === "canyon" ? S.viaAskOn : S.viaAskOff} yes={via === "canyon" ? S.viaYes : S.viaYesOff} onYes={() => void saveVia()} onNo={() => chooseVia(read.via)}>
              <p className="text-md">{via === "canyon" ? S.viaCanyonNote : S.viaOffNote}</p>
            </AskFirst>
          )}
          {via === read.via && viaSaved && <Saved pending={viaPending} />}
          {viaRefuses && (
            <AskFirst question={S.viaAsk} yes={S.viaYes} wait={reading ? S.readingCalls : null} onYes={() => void saveVia()} onNo={() => chooseVia(read.via)}>
              <p className="text-base">{S.viaUnderAllowlist}</p>
              <p role="note" className="text-md text-amber">{S.viaUnseen}</p>
              {weighed(true)}
            </AskFirst>
          )}
          {!known(read.mode) ? <p className="mt-6 text-md text-critical">What apps may call couldn't be read.</p> : (<>
            <fieldset aria-describedby="egress-mode-beta" disabled={!through} className="mt-6">
              <legend className="text-base font-semibold">{S.mode} <Chip tone="beta" why="egress-mode-beta">{S.beta}</Chip></legend>
              {!through && <p role="note" className={NOTE}>{S.modeViaOff}</p>}
              {MODES.map(([value, word, note]) => (
                <div key={value} className="mt-2">
                  <label className="flex items-center gap-2 text-base font-semibold has-disabled:text-text-faint">
                    <input type="radio" name="egress-mode" checked={mode === value} aria-describedby={`egress-mode-${value}`} onChange={() => choose(value)} />
                    {word}
                  </label>
                  <p id={`egress-mode-${value}`} className={`${NOTE} ml-6`}>{note}</p>
                </div>
              ))}
              <p className={`mt-3 ${NOTE}`}>{S.modeFollowed}</p>
            </fieldset>
            {through && mode !== read.mode && (mode === "observe"
              ? (
                <AskFirst question={S.modeAskObserve} yes={S.modeYesObserve} onYes={() => void saveMode()} onNo={() => choose(read.mode)}>
                  <p className="text-md">{S.modeWiden}</p>
                </AskFirst>
              ) : (
                <AskFirst question={ask} yes={S.modeYes} danger wait={reading ? S.readingCalls : null} onYes={() => void saveMode()} onNo={() => choose(read.mode)}>
                  <p className="text-md">{S.modeNow}</p>
                  {weighed(false)}
                </AskFirst>
              ))}
            {modeSaved && mode === read.mode && <Saved then={S.modeSavedThen} pending={modePending} />}
          </>)}
          {typeof read.credential_routes !== "boolean" ? <p className="mt-6 text-md text-critical">{S.routesUnread}</p> : (<>
            <fieldset aria-describedby="egress-routes-beta" className="mt-6">
              <legend className="text-base font-semibold">{S.routes} <Chip tone="beta" why="egress-routes-beta">{S.beta}</Chip></legend>
              {!read.credential_routes && !unlocks(licence, "outbound_keys") ? licence !== null && <div className="mt-2"><EnterpriseLine feature="outbound_keys" /></div> : <label className="mt-2 flex items-center gap-2 text-base font-semibold">
                <input type="checkbox" checked={routes} aria-describedby="egress-routes-note" onChange={(e) => { setRoutes(e.target.checked); setRoutesSaved(false); setRoutesPending(null); }} />
                {S.routesAdd}
              </label>}
              <p id="egress-routes-note" className={`${NOTE} ml-6`}>{S.routesOnNote}</p>
            </fieldset>
            {routes !== read.credential_routes && (
              <AskFirst question={routes ? S.routesAskOn : S.routesAskOff} yes={routes ? S.routesYesOn : S.routesYesOff} danger={!routes}
                onYes={() => void saveRoutes()} onNo={() => { setRoutes(read.credential_routes); setRoutesSaved(false); }}>
                <p className={routes ? "text-md" : "text-md text-amber"}>{routes ? S.routesOnNote : S.routesTurningOff}</p>
              </AskFirst>
            )}
            {routesSaved && routes === read.credential_routes && <Saved pending={routesPending} />}
          </>)}
          {listed !== null && (<>
            <h2 className="mt-6 text-base font-semibold">{S.recreateHeading}</h2>
            <p className={NOTE}>{S.recreateNote}</p>
            {listed === undefined ? <p className="mt-1 text-md text-critical">The networks to recreate couldn't be read.</p>
              : listed.length === 0 ? <p className={NOTE}>None.</p> : (
                <ul className="mt-2 grid gap-2">
                  {listed.map((app) => (
                    <li key={app} className="grid gap-1.5">
                      <span className="flex flex-wrap items-center gap-3">
                        <span className="font-mono text-base">{app}</span>
                        {asking !== app && <Button size="sm" label={`Recreate ${app}'s network`} onClick={() => askAbout(app)}>Recreate</Button>}
                      </span>
                      {asking === app && (
                        <div className={`${CARD} grid gap-2`}>
                          <p className="text-base">{askLine(app)}</p>
                          <span className="flex flex-wrap gap-2">
                            <Button variant="danger" onClick={() => recreate(app)}>{S.recreateYes}</Button>
                            <Button variant="ghost" onClick={() => setAsking(null)}>Cancel</Button>
                          </span>
                        </div>
                      )}
                    </li>
                  ))}
                </ul>
              )}
          </>)}
        </fieldset>
      )}
      {busy !== null && <p role="status" className={NOTE}>{busy}</p>}
      {/* Advanced (#118): the limits, the gateway replicas and the certificates, an admin's, folded away until a read needs
          a person, so a warning is never folded by itself. */}
      {read !== null && ((caps !== null && read.caps !== undefined) || (read.replicas?.length ?? 0) > 0 || caOf(read) !== null) && (
        <details className="mt-6 rounded-[10px] border border-border bg-surface px-4 py-3" open={advanced} onToggle={(event) => setAdvanced(event.currentTarget.open)}>
          <summary className="cursor-pointer text-base font-semibold">{S.advanced}<span className="ml-2 font-regular text-text-dim">{S.advancedWhat}</span>
            {!advanced && capsUnsaved() && <span className="ml-2 font-regular text-amber">{S.capsUnsaved}</span>}</summary>
          <fieldset disabled={busy !== null} className="min-w-0">
            {/* The limits, an admin's (#2691): a server from before them names none, and then none is shown. */}
            {caps !== null && read.caps !== undefined && (() => {
              const moved = capsMoved();
              return (
                <section aria-label={S.capsHeading} className="mt-6">
                  <h2 className="text-base font-semibold">{S.capsHeading}</h2>
                  <p className={NOTE}>{S.capsNote}</p>
                  {CAPS.map(([key, label, note, [min, max]]) => (
                    <div key={key} className="mt-2 grid gap-1">
                      <label htmlFor={`egress-cap-${key}`} className="text-base font-semibold">{label}</label>
                      <input id={`egress-cap-${key}`} type="number" inputMode="numeric" min={min} max={max} step={1} value={caps[key]} aria-describedby={`egress-cap-${key}-note`}
                        className="w-32 rounded border border-border bg-surface px-2 py-1 text-base"
                        onChange={(e) => { capsTyped.current = true; setCaps({ ...caps, [key]: e.target.value }); setCapsSaved(false); setCapsPending(null); }} />
                      <p id={`egress-cap-${key}-note`} className={NOTE}>{note} {S.capRange.replace("<min>", () => min.toLocaleString("en")).replace("<max>", () => max.toLocaleString("en"))}.</p>
                    </div>
                  ))}
                  {moved !== null && <span className="mt-3 flex"><Button variant="primary" onClick={() => void saveCaps(moved)}>{S.capsSave}</Button></span>}
                  {capsSaved && moved === null && <Saved pending={capsPending} />}
                </section>
              );
            })()}
            {(read.replicas?.length ?? 0) > 0 && (<>
              <h2 className="mt-6 text-base font-semibold">{S.replicasHeading}</h2>
              <p className={NOTE}>{S.replicasNote}</p>
              <ul className="mt-2 grid gap-1">
                {read.replicas?.map((one) => {
                  const line = policyAge(one.current_at, landed);
                  return (
                    <li key={one.replica} className="flex flex-wrap items-baseline gap-x-3">
                      <span className="font-mono text-base break-all">{one.replica}</span>
                      <span className={line === S.replicaCurrent ? NOTE : "text-md text-amber"}>{line}</span>
                    </li>
                  );
                })}
              </ul>
            </>)}
          </fieldset>
          <OutboundKeys setting={read} adopt={adopt} />
        </details>
      )}
    </section>
  );
}

// The limits Settings › Outbound shows, in order, each with its line and its range, 0130's (#2691).
const CAPS: [keyof EgressCaps, string, string, [number, number]][] = [
  ["new_names", S.capNewNames, S.capNewNamesNote, [1, 600]],
  ["destinations", S.capDestinations, S.capDestinationsNote, [50, 5000]],
  ["pending", S.capPending, S.capPendingNote, [1, 500]],
];

// What in a read needs a person, and opens Settings › Outbound's Advanced (#118): a replica that doesn't hold the current
// policy, or the org's own sub-CA ending.
const needsPerson = (read: EgressSetting, now: number) => (read.replicas ?? []).some((one) => policyAge(one.current_at, now) !== S.replicaCurrent) || subCAEnding(read, now) !== null;

// A replica's policy age, now less the last time it held the current one (design/EGRESS.md §3.2): an old one is
// cut off from Canyon, which is why it alarms. Under a minute it asked within its 5 s polls.
const policyAge = (currentAt: string | null, now: number) => {
  if (currentAt === null) return S.replicaNone;
  return now - new Date(currentAt).getTime() < 60_000 ? S.replicaCurrent : S.replicaAge.replace("<age>", () => age(currentAt));
};

// Settings › Outbound for a member (design/PERMISSIONS.md, Outbound, Host settings): the host's via and the
// org's mode, read-only (setting_member), under who changes them. The mode only while calls go through Canyon,
// where it applies. Fail closed: a value this build does not know is not read.
function OutboundRead() {
  const [read, setRead] = useState<EgressSetting | null>(null);
  const [failure, setFailure] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    getEgress().then((answer) => { if (live) setRead(answer); }, (err: unknown) => { if (live) setFailure(said(err)); });
    return () => { live = false; };
  }, []);
  const via = VIA.find(([value]) => value === read?.via);
  const mode = MODES.find(([value]) => value === read?.mode);
  const routes = ROUTES.find(([value]) => value === read?.credential_routes);
  const unread = read !== null && (via === undefined || mode === undefined || routes === undefined);
  const shown = (id: string, legend: string, [, word, note]: readonly [unknown, string, string]) => (
    <div role="group" aria-labelledby={`${id}-legend`} aria-describedby={`${id}-beta`} className="mt-4">
      <p id={`${id}-legend`} className="text-base font-semibold">{legend} <Chip tone="beta" why={`${id}-beta`}>{S.beta}</Chip></p>
      <p className="mt-1.5 text-base font-medium">{word}</p>
      <p className={NOTE}>{note}</p>
    </div>
  );
  return (
    <section aria-label="Outbound">
      <p className={NOTE}>{S.outboundAdmin}</p>
      {(failure !== null || unread) && <p role="alert" className="mt-2 text-base text-critical">{failure ?? "Outbound couldn't be read."}</p>}
      {!unread && via !== undefined && shown("egress-via-read", S.via, via)}
      {!unread && via?.[0] === "canyon" && mode !== undefined && shown("egress-mode-read", S.mode, mode)}
      {!unread && routes !== undefined && shown("egress-routes-read", S.routes, routes)}
    </section>
  );
}
