// The cluster journeys' shared pieces (e2e/cluster/CASES.md, e2e/cluster/README.md): the test cluster's
// variables, the apps' addresses, the cluster read through $E2E_KUBECONFIG, a deadline poll that
// names its last observation, the fixture's pages parsed, and the CLI run as a signed-in person.
// The pure parts (podSummary, waitMessage, parseWhoami, sessionId) have
// cluster.test.ts, which node runs without a cluster -- so this module imports no other harness
// module (node resolves no extensionless "./seed"), only types.
import { execFileSync, spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Page } from "@playwright/test";
import type { Session } from "./signin";

const repo = join(import.meta.dirname, "../../..");

// bed is one of README's variables, and fails naming it when the test cluster did not set it.
export function bed(name: string): string {
  const value = process.env[name];
  if (!value) throw new Error(`the cluster project needs ${name} (e2e/cluster/README.md, What the test cluster provides)`);
  return value;
}

const host = () => bed("E2E_CANYON_URL"); // seed.ts's host, read when a request is made, not at load

export const email = (user: "admin" | "alice" | "bob") => bed(`E2E_${user.toUpperCase()}_EMAIL`);

// appUrl is an environment's address under the front door's domain, form app.env.domain (A2).
export const appUrl = (app: string, env: "preview" | "prod") => `https://${app}.${env}.${bed("E2E_APPS_DOMAIN")}`;

// raw is one request as session (null: anonymous), answering the status and the body whatever the
// status: for a refusal a case asserts, where seed.ts's api() would throw.
export async function raw(session: Session | null, method: string, path: string, body?: unknown): Promise<{ status: number; text: string; json: unknown }> {
  const headers: Record<string, string> = body === undefined ? {} : { "Content-Type": "application/json" };
  if (session) headers.Cookie = session.cookie;
  const res = await fetch(host() + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await res.text();
  let json: unknown = null;
  try { json = text ? JSON.parse(text) : null; } catch { json = null; } // a refusal is JSON; an HTML error page is not, and the text says so
  return { status: res.status, text, json };
}

// portalSession is the claims portal's own sign-in, satisfied the way Docker's appPage satisfies it
// (e2e/deploy_test.go: `Cookie: cp_session=x`): the fixture's middleware sends /db and /secret to
// its /login without that cookie, and exempts /whoami, /probe and /fetch alone (e2e/fixtures/gen.sh). Set
// on the app's origin, so the proxy's own cookie and Canyon's are untouched.
export async function portalSession(page: Page, app: string, env: "preview" | "prod"): Promise<void> {
  await page.context().addCookies([{ name: "cp_session", value: "x", url: appUrl(app, env) }]);
}

// Every row of the trail session may read, newest first, page by page (contracts/routes.md, GET /audit).
export type AuditEvent = { id: string; ts: string; actor: { kind: string; subject: string; display: string }; action: string; project?: string; detail: Record<string, unknown> };
export async function audit(session: Session, query = ""): Promise<AuditEvent[]> {
  const rows: AuditEvent[] = [];
  for (let cursor = ""; ; ) {
    const { status, text, json } = await raw(session, "GET", `/api/v1/audit?limit=200${query && `&${query}`}${cursor && `&cursor=${encodeURIComponent(cursor)}`}`);
    if (status !== 200) throw new Error(`GET /api/v1/audit: ${status} ${text}`);
    const page = json as { events: AuditEvent[]; next: string | null };
    rows.push(...page.events);
    if (!page.next) return rows;
    cursor = page.next;
  }
}

// The cluster, read through kubectl as surfaces.ts reads it: never a Secret (README, The read-only kubeconfig).
type Item = {
  metadata: { name: string; uid: string; labels?: Record<string, string>; annotations?: Record<string, string> };
  spec?: { containers?: { name: string; image: string }[]; initContainers?: { name: string; image: string }[] };
  status?: { phase?: string; containerStatuses?: { restartCount: number }[]; initContainerStatuses?: { restartCount: number }[] };
};

export function kube(namespace: string, kinds: string, selector = ""): Item[] {
  const args = ["--kubeconfig", bed("E2E_KUBECONFIG"), "-n", namespace, "get", kinds, "-o", "json", ...(selector ? ["-l", selector] : [])];
  // A kind lists {items: [...]}; one named object (configmap/<name>, service/<name>) is the object itself.
  const out = JSON.parse(execFileSync("kubectl", args, { encoding: "utf8", maxBuffer: 256 << 20 })) as { items?: Item[] } & Item;
  return out.items ?? [out];
}

export type PodSummary = { name: string; uid: string; phase: string; restarts: number; images: Record<string, string>; initImages: Record<string, string> };

// podSummary is what the cases compare a pod by: its UID and every container's restarts summed (a
// restarted pod keeps its UID, a replaced one does not: both are checked), and each container's
// image by name, init containers apart.
export function podSummary(pod: Item): PodSummary {
  const restarts = [...(pod.status?.containerStatuses ?? []), ...(pod.status?.initContainerStatuses ?? [])].reduce((n, c) => n + c.restartCount, 0);
  const byName = (list: { name: string; image: string }[] | undefined) => Object.fromEntries((list ?? []).map((c) => [c.name, c.image]));
  return { name: pod.metadata.name, uid: pod.metadata.uid, phase: pod.status?.phase ?? "", restarts, images: byName(pod.spec?.containers), initImages: byName(pod.spec?.initContainers) };
}

// podNamed is the one pod in namespace whose name is prefix plus a Deployment's generated suffix
// (never a hard-coded pod name), or undefined when there is none.
export function podNamed(namespace: string, prefix: string): PodSummary | undefined {
  const found = kube(namespace, "pods").filter((p) => p.metadata.name.startsWith(prefix + "-"));
  if (found.length > 1) throw new Error(`${found.length} pods named ${prefix}-* in ${namespace}: ${found.map((p) => p.metadata.name).join(", ")}`);
  return found[0] && podSummary(found[0]);
}

// configMapAnnotations is a ConfigMap's annotations, {} when it has none.
export function configMapAnnotations(namespace: string, name: string): Record<string, string> {
  const [found] = kube(namespace, `configmap/${name}`);
  return found?.metadata.annotations ?? {};
}

// until polls every 5 s until poll says done, and answers what it saw. Past within it fails
// naming what it waited for and the last observation (README, Waits).
export async function until(within: number, what: string, poll: () => Promise<{ done: boolean; saw: string }>): Promise<string> {
  const end = Date.now() + within;
  for (;;) {
    const { done, saw } = await poll();
    if (done) return saw;
    if (Date.now() > end) throw new Error(waitMessage(within, what, saw, new Date()));
    await new Promise((r) => setTimeout(r, 5000));
  }
}

export const waitMessage = (within: number, what: string, last: string, at: Date) =>
  `waited ${within / 1000}s for ${what}: last answer ${last} at ${at.toTimeString().slice(0, 8)}`;

// The fixture's /whoami (e2e/fixtures/gen.sh, f-cluster) as it prints: four lines, each `key: value`,
// roles "-" for none (and a fifth, door:, M8 reads by its id). Anything else — the provider's page, an oauth2-proxy error, a 502 — is a
// failure naming the text, never a whoami with empty fields.
export type WhoAmI = { email: string; roles: string; aud: string; verified: string };
export function parseWhoami(text: string): WhoAmI {
  const lines = Object.fromEntries(text.split("\n").map((l) => l.trim()).filter((l) => l.includes(": ")).map((l) => l.split(/: (.*)/s).slice(0, 2)));
  for (const key of ["email", "roles", "aud", "verified"]) if (!(key in lines)) throw new Error(`not the fixture's /whoami: no "${key}:" line in ${JSON.stringify(text.slice(0, 300))}`);
  return { email: lines.email!, roles: lines.roles!, aud: lines.aud!, verified: lines.verified! };
}


// The CLI (cli/), built once per process into a temp dir as e2e/cluster/main_test.go builds it.
let built: string | undefined;
export function cli(): string {
  if (!built) {
    built = join(mkdtempSync(join(tmpdir(), "canyon-e2e-ui-")), "canyon");
    execFileSync("go", ["build", "-o", built, "./cmd/canyon"], { cwd: repo, stdio: "inherit" });
  }
  return built;
}

// sessionId is the session row's id out of a browser session's Cookie header: the value of the
// session cookie (cmd/canyon-server/session.go sessionCookieOf: __Host-canyon_session under https,
// canyon_session under http), whatever else the browser holds for Canyon's origin.
export function sessionId(session: Session): string {
  const found = session.cookie.split("; ").map((c) => c.split(/=(.*)/s)).find(([name]) => name === "__Host-canyon_session" || name === "canyon_session");
  if (!found) throw new Error(`no Canyon session cookie among ${session.cookie.split("; ").map((c) => c.split("=")[0]).join(", ")}`);
  return found[1]!;
}

// cliHome is a config home of its own for one person: the CLI's config.json naming the host and
// the session the browser signed in with (cli/config.go's shape) -- the device-flow session and a
// browser session are the same session row, so the CLI acts as that person. null is nobody.
export function cliHome(session: Session | null): string {
  const home = mkdtempSync(join(tmpdir(), "canyon-e2e-home-"));
  if (session) {
    mkdirSync(join(home, "canyon"));
    writeFileSync(join(home, "canyon", "config.json"), JSON.stringify({ host: host(), session: sessionId(session) }), { mode: 0o600 });
  }
  return home;
}

// cliEnv is the CLI's environment with its config in home (e2e/cluster/session_test.go's cliEnv):
// no agent marker, no colour, this machine's own config nowhere in reach.
export function cliEnv(home: string): NodeJS.ProcessEnv {
  const env = { ...process.env };
  for (const name of ["CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CODEX_SANDBOX", "CANYON_FORMAT"]) delete env[name];
  return { ...env, HOME: home, XDG_CONFIG_HOME: home, CANYON_HOST: host(), CANYON_NO_UPDATE_NOTICE: "1", NO_COLOR: "1" };
}

// canyon runs the CLI as home's person and answers what it printed and its exit code.
export function canyon(home: string, args: string[]): { stdout: string; stderr: string; code: number } {
  const run = spawnSync(cli(), args, { env: cliEnv(home), encoding: "utf8", cwd: home, timeout: 15 * 60_000 });
  if (run.error) throw run.error;
  return { stdout: run.stdout, stderr: run.stderr, code: run.status ?? -1 };
}
