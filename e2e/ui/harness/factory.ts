// journey() makes one Playwright test from a list of steps, each photographed on every surface
// before and after, and failed on anything it changed that it did not declare.
import { test, expect, type Page } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { diff, onCluster, quiet, repo, snapshot, type Diff, type Snapshot } from "./surfaces";

type Count = number | "any";
// TypeScript requires every named property below to fit the index signature's value type, since
// they share one type: started joins added/changed/removed/actions so containers/pods/configMaps
// fit; stub becomes an object, like the rest, so it fits too, rather than loosening the index
// signature itself and losing the shape check on every real table entry.
export type Changes = {
  [table: string]: { added?: Count; changed?: Count; removed?: Count; started?: Count; actions?: string[] } | undefined;
} & {
  containers?: { started?: Count; removed?: Count; changed?: Count };
  appDatabases?: { created?: Count; dropped?: Count };
  stub?: { added?: Count };
  pods?: { started?: Count; removed?: Count; changed?: Count };
  configMaps?: { started?: Count; removed?: Count; changed?: Count };
};

export type Step<C> = {
  name: string;
  do: (page: Page, ctx: C) => Promise<void>;
  see?: (page: Page, ctx: C) => Promise<void>;
  changes?: Changes;
  settle?: number; // ms the surfaces may keep changing after the step, default 20 s
};

const reportDir = join(repo, "e2e/ui/.work/report");

// A server line that is a request answered 5xx, a panic, or the durable engine's warning or error.
const alarming = (line: string) => / 5\d\d \S+$|panic|level=(WARN|ERROR)/.test(line);

export function journey<C extends object>(name: string, j: { given?: (page: Page) => Promise<C>; steps: Step<C>[] }) {
  test(name, async ({ page, baseURL }, info) => {
    const browser: string[] = [];
    page.on("console", (m) => { if (m.type() === "error") browser.push(`console: ${m.text()}`); });
    page.on("pageerror", (e) => browser.push(`page error: ${e.message}`));
    page.on("response", (r) => { if (r.status() >= 500) browser.push(`${r.status()} ${r.request().method()} ${r.url()}`); });
    const ctx = j.given ? await j.given(page) : ({} as C); // on the test cluster, given signs in (signin.ts) before the first snapshot reads the trail
    const slug = `${name}-${info.project.name}`.toLowerCase().replace(/[^a-z0-9]+/g, "-");
    mkdirSync(reportDir, { recursive: true });
    const lines = [`# ${name} (${info.project.name})`, ""];
    try {
      for (const [i, s] of j.steps.entries()) {
        await test.step(s.name, async () => {
          browser.length = 0;
          const before = await snapshot();
          let seen: unknown;
          try {
            await s.do(page, ctx);
            if (s.see) await s.see(page, ctx);
          } catch (err) {
            seen = err; // the surfaces are still read and reported: a failed step is when they matter most
          }
          const d = await settled(before, s.settle ?? 20_000);
          const shot = `${slug}-${i + 1}.png`;
          // Never a picture of another origin's page: on the test cluster that is the identity provider's,
          // where passwords are typed.
          const here = new URL(page.url());
          const away = here.protocol.startsWith("http") && here.origin !== new URL(baseURL!).origin;
          if (!away) await page.screenshot({ path: join(reportDir, shot), fullPage: true });
          lines.push(`## ${i + 1}. ${s.name}`, "", away ? "screenshot skipped: off Canyon's origin" : `![](${shot})`, "", ...describe(d, browser), "");
          if (seen !== undefined) throw seen;
          // ui/Table.tsx's rule, at every width: the page never scrolls sideways; tables do.
          const [scroll, width] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
          if (scroll > width) browser.push(`the page is ${scroll}px wide in a ${width}px window: it scrolls sideways`);
          const wrong = [...undeclared(s.changes ?? {}, d), ...browser, ...d.logs.filter(alarming).map((l) => `server: ${l}`)];
          expect(wrong, `${s.name}: what happened that the step did not declare`).toEqual([]);
        });
      }
    } finally {
      writeFileSync(join(reportDir, `${slug}.md`), lines.join("\n"));
    }
  });
}

// settled waits until the surfaces stop changing (two reads a second apart agree) or the step's time
// is up, and answers everything that changed since before.
async function settled(before: Snapshot, within: number): Promise<Diff> {
  const end = Date.now() + within;
  let last = await snapshot();
  for (;;) {
    await new Promise((r) => setTimeout(r, 1000));
    const now = await snapshot();
    if (quiet(diff(last, now)) || Date.now() > end) return diff(before, now);
    last = now;
  }
}

function undeclared(want: Changes, diffed: Diff): string[] {
  let d = diffed;
  const out: string[] = [];
  const check = (what: string, got: number, allowed: Count | undefined) => {
    if (allowed !== "any" && got !== (allowed ?? 0)) out.push(`${what}: ${got}, declared ${allowed ?? 0}`);
  };
  const { containers, appDatabases, stub, pods, configMaps, ...tables } = want;
  // Every audit row naming a project is announced once on the timeline as audit.appended
  // (events.AuditTx): the announcement is checked here, not declared by each step. The test cluster's
  // timeline is not read, so there it is not checked.
  const audited = (d.tables.audit_events?.added ?? []).filter((r) => r.project_id != null).map((r) => String(r.id));
  const timeline = d.tables.timeline_rows;
  if (!onCluster && (timeline || audited.length)) {
    const announced = (timeline?.added ?? []).filter((r) => r.kind === "audit.appended").map((r) => String((r.data as { event_id?: string }).event_id));
    if (JSON.stringify([...announced].sort()) !== JSON.stringify([...audited].sort())) out.push(`audit rows ${JSON.stringify(audited)} announced as ${JSON.stringify(announced)}`);
    if (timeline) d = { ...d, tables: { ...d.tables, timeline_rows: { ...timeline, added: timeline.added.filter((r) => r.kind !== "audit.appended") } } };
  }
  for (const t of new Set([...Object.keys(d.tables), ...Object.keys(tables)])) {
    const got = d.tables[t] ?? { added: [], removed: [], changed: [] };
    if (!got.added.length && !got.changed.length && !got.removed.length && !(t in tables)) continue;
    const w = tables[t] ?? {};
    check(`${t} rows added`, got.added.length, w.added);
    check(`${t} rows changed`, got.changed.length, w.changed);
    check(`${t} rows removed`, got.removed.length, w.removed);
    if (w.actions) {
      const actions = got.added.map((r) => String(r.action)).sort();
      if (JSON.stringify(actions) !== JSON.stringify([...w.actions].sort())) out.push(`${t} actions ${JSON.stringify(actions)}, declared ${JSON.stringify(w.actions)}`);
    }
  }
  check("containers started", d.containers.started.length, containers?.started);
  check("containers removed", d.containers.removed.length, containers?.removed);
  check("containers changed", d.containers.changed.length, containers?.changed);
  check("app databases created", d.appDatabases.created.length, appDatabases?.created);
  check("app databases dropped", d.appDatabases.dropped.length, appDatabases?.dropped);
  check("stub calls", d.stub.length, stub?.added);
  check("pods started", d.pods.started.length, pods?.started);
  check("pods removed", d.pods.removed.length, pods?.removed);
  check("pods changed", d.pods.changed.length, pods?.changed);
  check("config maps started", d.configMaps.started.length, configMaps?.started);
  check("config maps removed", d.configMaps.removed.length, configMaps?.removed);
  check("config maps changed", d.configMaps.changed.length, configMaps?.changed);
  return out;
}

function describe(d: Diff, browser: string[]): string[] {
  const out: string[] = [];
  for (const [t, x] of Object.entries(d.tables)) {
    out.push(`- **${t}**: +${x.added.length} ~${x.changed.length} -${x.removed.length}`);
    for (const r of x.added) out.push(`  - added \`${JSON.stringify(r).slice(0, 300)}\``);
    for (const c of x.changed) out.push(`  - changed ${c.key}: ${c.columns.join(", ")}`);
    for (const r of x.removed) out.push(`  - removed \`${JSON.stringify(r).slice(0, 200)}\``);
  }
  const { started, removed, changed } = d.containers;
  if (started.length || removed.length || changed.length) out.push(`- **containers**: started ${started.join(", ") || "none"}; removed ${removed.join(", ") || "none"}; ${changed.join("; ")}`);
  if (d.appDatabases.created.length || d.appDatabases.dropped.length) out.push(`- **app databases**: created ${d.appDatabases.created.join(", ") || "none"}; dropped ${d.appDatabases.dropped.join(", ") || "none"}`);
  if (d.stub.length) out.push(`- **stub**: ${d.stub.map((s) => `${s.method} ${s.path}`).join(", ")}`);
  for (const [what, k] of [["pods", d.pods], ["config maps", d.configMaps]] as const) {
    if (k.started.length || k.removed.length || k.changed.length) out.push(`- **${what}**: started ${k.started.join(", ") || "none"}; removed ${k.removed.join(", ") || "none"}; ${k.changed.join("; ")}`);
  }
  const requests = d.logs.filter((l) => / \/api\//.test(l));
  if (requests.length) out.push(`- **server requests**: ${requests.map((l) => l.replace(/^\S+ \S+ /, "")).join(" · ")}`);
  for (const b of browser) out.push(`- **browser**: ${b}`);
  if (!out.length) out.push("- nothing changed");
  return out;
}
