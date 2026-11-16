// The surfaces a journey watches: Canyon's database, the apps' Postgres, the containers, the stub's
// request log and the server's and executor's logs. snapshot() reads them all; diff() says what
// changed between two reads. Everything is read, nothing written.
//
// On the test cluster ($E2E_KUBECONFIG set, e2e/cluster/README.md) there is no stack and no access to
// Canyon's database: the surfaces are the pods and ConfigMaps in canyon-apps and the audit trail
// through the API, and nothing else is read.
import { execFileSync } from "node:child_process";
import { readFileSync, existsSync } from "node:fs";
import { join } from "node:path";

export const repo = join(import.meta.dirname, "../../..");
const stubLog = join(repo, "e2e/.work/log/requests.jsonl");
export const onCluster = Boolean(process.env.E2E_KUBECONFIG);

type Row = Record<string, unknown>;
type Pod = { uid: string; restarts: number; images: string[] }; // restarts: every container's, summed
export type Snapshot = {
  at: string;
  tables: Record<string, Map<string, Row>>; // by primary key
  appDatabases: string[];
  appRoles: string[];
  containers: Map<string, string>; // name → "id state"
  stubCalls: number;
  pods: Map<string, Pod>; // canyon-apps, by name
  configMaps: Map<string, Record<string, string>>; // canyon-apps, name → annotations
};

type Keyed = { started: string[]; removed: string[]; changed: string[] };
export type TableDiff = { added: Row[]; removed: Row[]; changed: { key: string; columns: string[]; after: Row }[] };
export type Diff = {
  tables: Record<string, TableDiff>;
  appDatabases: { created: string[]; dropped: string[] };
  appRoles: { created: string[]; dropped: string[] };
  containers: { started: string[]; removed: string[]; changed: string[] };
  stub: { method: string; path: string }[];
  logs: string[]; // every server and executor line in the window
  pods: Keyed;
  configMaps: Keyed;
};

function compose(...args: string[]): string {
  const files = ["-f", "docker-compose.yml", "-f", "e2e/compose.e2e.yml", ...(process.env.MODEL ? ["-f", "e2e/compose.model.yml"] : [])];
  return execFileSync("docker", ["compose", ...files, ...args], { cwd: repo, encoding: "utf8", maxBuffer: 256 << 20 });
}

// psql runs one statement on the stack's database, for a journey's given that seeds what no request
// on this stack can make (a custom rule checked by code needs a model to translate it).
export const psql = (query: string) => sql("postgres", query);

function sql(service: string, query: string): string {
  return compose("exec", "-T", service, "psql", "-U", "canyon", "-d", "canyon", "-AtX", "-v", "ON_ERROR_STOP=1", "-c", query).trim();
}

// The tables and their keys are read from the catalog each time, so a migration needs no edit here.
let shape: { table: string; key: string[]; drop: string[] }[] | undefined;
function tablesShape() {
  if (shape) return shape;
  const cols = JSON.parse(sql("postgres", `SELECT json_agg(json_build_object('t', table_name, 'c', column_name, 'bytea', data_type = 'bytea'))
    FROM information_schema.columns WHERE table_schema = 'public'`)) as { t: string; c: string; bytea: boolean }[];
  const keys = JSON.parse(sql("postgres", `SELECT coalesce(json_agg(json_build_object('t', tc.table_name, 'c', k.column_name)), '[]')
    FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage k USING (constraint_schema, constraint_name)
    WHERE tc.constraint_type = 'PRIMARY KEY' AND tc.table_schema = 'public'`)) as { t: string; c: string }[];
  // egress_receiver is one heartbeat row and nothing else: the receiver writes it on its first flush, 10 s
  // after the server starts and so after /ready, and moves it every 10 s whatever a step does; patch_plan_said is
  // the rescan tick's one row, which each tick writes on the clock (CANYON_PATCH_INTERVAL, unset every 6 h from
  // 00:00 UTC) whatever a step does, the first one adding it (#2719)
  const background = new Set(["egress_receiver", "patch_plan_said"]);
  const names = [...new Set(cols.map((c) => c.t))].filter((t) => !background.has(t)).sort();
  shape = names.map((table) => ({
    table,
    key: keys.filter((k) => k.t === table).map((k) => k.c),
    // a package's tree is bytes: its hash row says enough; a server's heartbeat (durable_executors.seen_at)
    // moves every 5 s whatever a step does, and so does egress-control's ask (egress_replicas: when it asked, the
    // version it holds and was served, and when that was current, the policy's own, which egress_policy shows),
    // while a row coming and going still counts
    drop: cols.filter((c) => c.t === table && (c.bytea || (table === "durable_executors" && c.c === "seen_at") ||
      (table === "egress_replicas" && c.c !== "replica"))).map((c) => c.c),
  }));
  return shape;
}

type Item = {
  metadata: { name: string; uid: string; annotations?: Record<string, string> };
  spec?: { containers: { image: string }[] };
  status?: { containerStatuses?: { restartCount: number }[] };
};

function kubectl(kind: "pods" | "configmaps"): Item[] {
  const out = execFileSync("kubectl", ["--kubeconfig", process.env.E2E_KUBECONFIG!, "-n", "canyon-apps", "get", kind, "-o", "json"], { encoding: "utf8", maxBuffer: 256 << 20 });
  return JSON.parse(out).items;
}

// auditTrail is every row of the trail an admin may read, by id, page by page. snapshot() has no
// session in scope (it is not a step's action, so no "as whoever's running this step" exists), and
// an implicit "whoever signed in last" is exactly the footgun explicit sessions exist to avoid
// (canyon#482 review) -- so this is anonymous, which the test cluster refuses loudly rather than silently
// reading as the wrong person. A cluster case that needs the trail mid-journey reads it through
// seed.ts's api() with its own session instead; no case does yet (PRs 4-8).
async function auditTrail(): Promise<Map<string, Row>> {
  const rows = new Map<string, Row>();
  for (let cursor = ""; ; ) {
    const path = `/api/v1/audit?limit=200${cursor && `&cursor=${encodeURIComponent(cursor)}`}`;
    const res = await fetch(process.env.E2E_CANYON_URL + path);
    if (!res.ok) throw new Error(`GET ${path}: ${res.status} ${await res.text()}`);
    const page = (await res.json()) as { events: Row[]; next: string | null };
    for (const e of page.events) rows.set(String(e.id), e);
    if (!page.next) return rows;
    cursor = page.next;
  }
}

async function clusterSnapshot(): Promise<Snapshot> {
  const at = new Date().toISOString();
  const pods = new Map(kubectl("pods").map((p) => [p.metadata.name, {
    uid: p.metadata.uid,
    restarts: (p.status?.containerStatuses ?? []).reduce((n, c) => n + c.restartCount, 0),
    images: p.spec!.containers.map((c) => c.image),
  }]));
  const configMaps = new Map(kubectl("configmaps").map((c) => [c.metadata.name, c.metadata.annotations ?? {}]));
  return { at, tables: { audit_events: await auditTrail() }, appDatabases: [], appRoles: [], containers: new Map(), stubCalls: 0, pods, configMaps };
}

export async function snapshot(): Promise<Snapshot> {
  if (onCluster) return clusterSnapshot();
  const at = new Date().toISOString();
  const parts = tablesShape().map(({ table, drop }) =>
    `'${table}', (SELECT coalesce(json_agg(to_jsonb(t)${drop.map((d) => ` - '${d}'`).join("")}), '[]') FROM "${table}" t)`);
  // one object a table, joined in one statement: a function takes at most 100 arguments, so one object of every table stops at 50
  const rows = JSON.parse(sql("postgres", `SELECT ${parts.map((p) => `jsonb_build_object(${p})`).join(" || ")}`)) as Record<string, Row[]>;
  const tables: Snapshot["tables"] = {};
  for (const { table, key } of tablesShape()) {
    tables[table] = new Map(rows[table].map((r) => [key.length ? key.map((k) => String(r[k])).join("/") : JSON.stringify(r), r]));
  }
  const app = JSON.parse(sql("app-postgres", `SELECT json_build_object(
    'dbs', (SELECT json_agg(datname ORDER BY datname) FROM pg_database),
    'roles', (SELECT json_agg(rolname ORDER BY rolname) FROM pg_roles WHERE rolname !~ '^pg_'))`)) as { dbs: string[]; roles: string[] };
  const containers = new Map<string, string>();
  for (const line of execFileSync("docker", ["ps", "-a", "--format", "{{.Names}}\t{{.ID}} {{.State}}\t{{.Labels}}"], { encoding: "utf8" }).trim().split("\n")) {
    const [name, state, labels = ""] = line.split("\t");
    const scan = labels.split(",").some((label) => label.split("=")[0] === "canyon.scan") ? "true" : ""; // docker run --label canyon.scan: no value
    // an app's floor holder is part of the app's container, whose network namespace it keeps (contracts/executor.md,
    // An app reaches no private, link-local or host address): it starts and goes with it, so a step declares the app's alone.
    // A container labelled canyon.scan is Canyon's own, a scan or a database's refresh or reader, on the executor's
    // schedule rather than a step's: its refreshes run at start and every 6 hours, each read back after (#2743)
    if (name && scan !== "true" && !/^canyon-appfloor-[0-9a-f]{8}$/.test(name)) containers.set(name, state);
  }
  const stubCalls = existsSync(stubLog) ? readFileSync(stubLog, "utf8").split("\n").filter(Boolean).length : 0;
  return { at, tables, appDatabases: app.dbs, appRoles: app.roles, containers, stubCalls, pods: new Map(), configMaps: new Map() };
}

const sameSet = (a: string[], b: string[]) => ({ created: b.filter((x) => !a.includes(x)), dropped: a.filter((x) => !b.includes(x)) });

function keyed<V>(before: Map<string, V>, after: Map<string, V>): Keyed {
  const d: Keyed = { started: [], removed: [], changed: [] };
  for (const n of new Set([...before.keys(), ...after.keys()])) {
    const [a, b] = [JSON.stringify(before.get(n)), JSON.stringify(after.get(n))];
    if (!before.has(n)) d.started.push(n);
    else if (!after.has(n)) d.removed.push(n);
    else if (a !== b) d.changed.push(`${n}: ${a} → ${b}`);
  }
  return d;
}

export function diff(before: Snapshot, after: Snapshot): Diff {
  const tables: Diff["tables"] = {};
  for (const [table, rows] of Object.entries(after.tables)) {
    const was = before.tables[table] ?? new Map();
    const d: TableDiff = { added: [], removed: [], changed: [] };
    for (const [key, row] of rows) {
      const old = was.get(key);
      if (!old) d.added.push(row);
      else {
        const columns = Object.keys(row).filter((c) => JSON.stringify(row[c]) !== JSON.stringify(old[c]));
        if (columns.length) d.changed.push({ key, columns, after: row });
      }
    }
    for (const [key, row] of was) if (!rows.has(key)) d.removed.push(row);
    if (d.added.length || d.removed.length || d.changed.length) tables[table] = d;
  }
  const names = new Set([...before.containers.keys(), ...after.containers.keys()]);
  const containers: Diff["containers"] = { started: [], removed: [], changed: [] };
  for (const n of names) {
    const [a, b] = [before.containers.get(n), after.containers.get(n)];
    if (!a) containers.started.push(n);
    else if (!b) containers.removed.push(n);
    else if (a !== b) containers.changed.push(`${n}: ${a} → ${b}`);
  }
  const stub = !onCluster && existsSync(stubLog)
    ? readFileSync(stubLog, "utf8").split("\n").filter(Boolean).slice(before.stubCalls).map((l) => {
        const { method, path } = JSON.parse(l) as { method: string; path: string };
        return { method, path };
      })
    : [];
  const logs = onCluster ? [] : compose("logs", "--no-color", "--no-log-prefix", "--since", before.at, "server", "executor").split("\n").filter(Boolean);
  return {
    tables,
    appDatabases: sameSet(before.appDatabases, after.appDatabases),
    appRoles: sameSet(before.appRoles, after.appRoles),
    containers,
    stub,
    logs,
    pods: keyed(before.pods, after.pods),
    configMaps: keyed(before.configMaps, after.configMaps),
  };
}

const still = (k: Keyed) => !k.started.length && !k.removed.length && !k.changed.length;

export const quiet = (d: Diff) =>
  !Object.keys(d.tables).length && !d.appDatabases.created.length && !d.appDatabases.dropped.length &&
  !d.containers.started.length && !d.containers.removed.length && !d.containers.changed.length && !d.stub.length &&
  still(d.pods) && still(d.configMaps);
