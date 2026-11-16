// Seeding for a journey's `given`: the API and the fixtures, no browser.
import { execFileSync } from "node:child_process";
import { cpSync, existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Session } from "./signin";
import { onCluster, repo } from "./surfaces";

// The cluster project has no default target (playwright.config.ts's baseURL, deliberately); a run
// under it with the variable unset must fail loudly, not quietly talk to a local dev server.
if (onCluster && !process.env.E2E_CANYON_URL) throw new Error("the cluster project needs E2E_CANYON_URL");
export const host = process.env.E2E_CANYON_URL ?? "http://localhost:7777";

// api sends session's cookie, naming who makes the request -- explicitly, on every call, never
// carried from whoever signed in last (canyon#482 review). null is anonymous, which is every
// caller on Docker, where auth is off. extra are further request headers.
export async function api<T = unknown>(session: Session | null, method: string, path: string, body?: unknown, type = "application/json", extra: Record<string, string> = {}): Promise<T> {
  const raw = body instanceof Blob ? body : body === undefined ? undefined : JSON.stringify(body);
  const headers: Record<string, string> = raw === undefined ? { ...extra } : { ...extra, "Content-Type": type };
  if (session) headers.Cookie = session.cookie;
  const res = await fetch(host + path, { method, body: raw, headers });
  const text = await res.text();
  if (res.status >= 300) throw new Error(`${method} ${path}: ${res.status} ${text}`);
  return (text ? JSON.parse(text) : {}) as T;
}

// A fresh app id per run: a deleted project's id is never free again.
export const appId = (base: string) => `${base}-${Math.random().toString(16).slice(2, 8)}`;

// upload makes app from fixture (e2e/.work/fixtures/<fixture>, made by make e2e-up) and uploads it,
// as session (null on Docker).
export async function upload(app: string, fixture: string, session: Session | null = null): Promise<string> {
  await api(session, "POST", "/api/v1/projects", { id: app });
  return uploadVersion(app, fixture, {}, session);
}

// uploadVersion uploads another version of app, which upload made: fixture with files, by path, written into it.
// base is the package it follows (Canyon-Base-Package): a project with a current package refuses an upload without it.
export async function uploadVersion(app: string, fixture: string, files: Record<string, string>, session: Session | null = null, base?: string): Promise<string> {
  const src = join(repo, "e2e/.work/fixtures", fixture);
  if (!existsSync(src)) throw new Error(`no fixture ${fixture}: run make e2e-up, which generates them`);
  const dir = mkdtempSync(join(tmpdir(), "canyon-ux-"));
  cpSync(src, dir, { recursive: true });
  for (const [path, text] of Object.entries(files)) writeFileSync(join(dir, path), text);
  const toml = join(dir, "canyon.toml");
  const rest = existsSync(toml) ? readFileSync(toml, "utf8").replace(/^app = ".*"\n/m, "") : "";
  writeFileSync(toml, `app = "${app}"\n${rest}`);
  const tar = execFileSync("tar", ["-C", dir, "-cf", "-", "."], { maxBuffer: 256 << 20, env: { ...process.env, COPYFILE_DISABLE: "1" } }); // no macOS ._ files
  const { package: pkg } = await api<{ package: { id: string } }>(session, "POST", `/api/v1/projects/${app}/packages`, new Blob([new Uint8Array(tar)]), "application/x-tar", base === undefined ? {} : { "Canyon-Base-Package": base });
  return pkg.id;
}

type Run = { run: { id: string; state: string } };

// review starts a gate run of app's current package and waits for it to end, as session. The first
// review on a fresh host pulls the scanners' images and downloads trivy's and grype's databases
// (contracts/finding.md, Canyon-run tools): minutes on a CI runner, so ten of them.
export async function review(app: string, session: Session | null = null): Promise<string> {
  const { run } = await api<Run>(session, "POST", `/api/v1/projects/${app}/gate-runs`, {});
  return ended(run.id, 10 * 60_000, session);
}

// deploy deploys app's current package to preview, waits for the run to end (a build) and fails
// unless preview then runs it, as session.
export async function deploy(app: string, session: Session | null = null): Promise<string> {
  const { run } = await api<Run>(session, "POST", `/api/v1/projects/${app}/deploys`, {});
  const id = await ended(run.id, 15 * 60_000, session);
  const { project } = await api<{ project: { envs: Record<string, unknown> } }>(session, "GET", `/api/v1/projects/${app}`);
  if (!project.envs.preview) throw new Error(`deploy of ${app} (${id}) left nothing in preview: see its timeline`);
  return id;
}

// promote promotes app's run to production and waits for the promote run to end, failing unless it
// ended clean, as session.
export async function promote(app: string, gateRun: string, session: Session | null = null): Promise<string> {
  const { run } = await api<Run>(session, "POST", `/api/v1/projects/${app}/promotions`, { gate_run: gateRun });
  const id = await ended(run.id, 15 * 60_000, session);
  const { run: read } = await api<Run>(session, "GET", `/api/v1/gate-runs/${id}`);
  if (read.state !== "clean") throw new Error(`promotion of ${app} (${id}) ended ${read.state}: see its timeline`);
  return id;
}

async function ended(id: string, within: number, session: Session | null): Promise<string> {
  const end = Date.now() + within;
  while (Date.now() < end) {
    const { run } = await api<Run>(session, "GET", `/api/v1/gate-runs/${id}`);
    if (!["pending", "running"].includes(run.state)) return id;
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(`run ${id} did not end within ${within} ms`);
}
