import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { Page } from "@playwright/test";

// A host that answers with the contracts' goldens, so every menu and dialog can be opened with no state to make: the
// dashboard is the running server's own, its reads are these. An admin's, on a host where people sign in.
const golden = (path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8"));
const fleet = golden("fleet/page.json");
const app = golden("app-page/claims-portal.json");
const held = golden("app-page/claims-portal-held-status.json");
const prefs = golden("guardrails/preferences.json");
const webhook = golden("release/meridian.json");

fleet.banner = "none";
fleet.viewer = { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false };
const row = (id: string) => structuredClone(fleet.apps.find((one: { id: string }) => one.id === id));
// An app whose name is one unbroken word, in preview and in production, which needs all a golden app does.
const NEEDS = fleet.apps.flatMap((one: { needs: unknown[] }) => one.needs);
export const unbroken = (length: number) => "m".repeat(length);
for (const length of [30, 48]) fleet.apps.unshift({ ...row("premium-ledger"), id: unbroken(length), name: unbroken(length), envs: { ...row("claims-portal").envs, ...row("premium-ledger").envs }, needs: NEEDS });
// Its changes, among the golden's: the fleet's Recent activity and the audit trail tell them.
const audit = structuredClone(app.audit[0]);
for (const one of [...audit.events]) audit.events.push({ ...one, id: `${one.id}_long`, project: unbroken(48), ...(one.run === undefined ? {} : { run: `${one.run}_long` }) });
const status = (id: string) => ({ ...app.status, project: id, envs: row(id).envs, last_run: null, promote_run: null, needs: [] });

// An app whose review found something on a path far wider than a phone.
function blocked(rest: string) {
  const ok = (body: unknown) => ({ status: 200, body });
  if (rest === "") return ok({ project: { ...row("claims-portal"), id: "blocked" } });
  if (rest === "/status") return ok({ ...app.status, project: "blocked" });
  if (rest === "/access") return ok(golden("access/claims-portal.json").read);
  if (rest === "/packages") return ok(app.packages[0]);
  if (rest === "/gate-runs") return ok(app.gate_runs[0]);
  return { status: 404, body: { error: "not_found", message: "not found" } };
}

function answer(method: string, path: string, role: string): { status: number; body: unknown } {
  const ok = (body: unknown) => ({ status: 200, body });
  if (path === "/auth/config") return ok({ canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
  const at = path.replace("/api/v1", "");
  const [, project, rest = ""] = /^\/projects\/([^/]+)(\/.*)?$/.exec(at) ?? [];
  if (method === "DELETE" && project !== undefined && rest === "") {
    return { status: 409, body: { error: "confirm_required", message: "type the app's name to delete it", resources: [{ name: "db", type: "postgres", env: "preview", state: "ready" }, { name: "db", type: "postgres", env: "prod", state: "ready" }] } };
  }
  if (method === "POST" && at === "/platform-webhook/token") return ok({ token: "filler-host-token" });
  if (method !== "GET") return ok({});
  if (project !== undefined) {
    if (project === "blocked") return blocked(rest);
    const known = project === "held" ? { ...row("claims-portal"), id: "held", hold: held.hold } : row(project);
    if (known === undefined) return { status: 404, body: { error: "not_found", message: "not found" } };
    switch (rest) {
      case "": return ok({ project: known });
      case "/status": return ok(project === "held" ? { ...held, project } : status(project));
      case "/access": return ok(golden("access/claims-portal.json").read);
      case "/egress": return ok(golden("egress/claims-portal.json").project_read);
      case "/secrets": return ok({ secrets: [] });
      case "/packages": return ok(app.packages[0]);
      case "/gate-runs": return ok({ runs: [], next: null });
    }
  }
  if (/^\/gate-runs\/[^/]+$/.test(at)) return ok(app.run_placed);
  switch (at) {
    case "/audit": return ok(audit);
    case "/preferences": return ok(prefs.preferences_admin);
    case "/fleet": return ok({ ...fleet, viewer: { ...fleet.viewer, role } });
    case "/projects": return ok({ projects: fleet.apps });
    case "/policies": return ok(golden("policies/meridian.json").policies);
    case "/front-door": return ok(golden("front-door/meridian.json").get);
    case "/licence": return ok(golden("licence/licence.json").admin);
    case "/egress": return ok(golden("egress/claims-portal.json").setting_admin);
    case "/platform-webhook": return ok(webhook.webhook_get);
    case "/platform-repo": return ok(null);
    case "/github": return ok({ reachable: true, orgs: [{ account: "meridian-apps", app_slug: "canyon-meridian", keeps_code: true, imports_new: true, installed: true, last_event_at: "2026-09-02T08:00:00Z", links_accounts: true }] });
    case "/github/repositories": return ok({ unreachable: [], repositories: [
      { repo_id: 555, full_name: "meridian-apps/claims-portal", org: "meridian-apps", default_branch: "main", apps: [] },
      { repo_id: 81723, full_name: "meridian-apps/platform", org: "meridian-apps", default_branch: "main", apps: [{ folder: "apps/billing", id: "funny-snake-4k2", name: "billing", state: "linked" }] },
    ] });
    case "/db-sessions": return ok(golden("db-session/meridian.json").list);
  }
  return { status: 404, body: { error: "not_found", message: "not found" } };
}

export async function answered(page: Page, role = "admin") {
  await page.route((url) => /^\/(api|auth)\//.test(url.pathname), async (route) => {
    const { status: code, body } = answer(route.request().method(), new URL(route.request().url()).pathname, role);
    await route.fulfill({ status: code, contentType: "application/json", body: JSON.stringify(body) });
  });
}
