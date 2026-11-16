// why (why.ts), run without a stack: `cd e2e/ui && npm run unit`.
import assert from "node:assert/strict";
import { test } from "node:test";
import { why } from "./why.ts";

type Row = { id: number; kind: string; data: Record<string, unknown> };

// host answers the run read and the run's timeline, paged as the server pages it: limit rows after the cursor,
// next the last id sent, and an empty page once the run has ended.
function host(state: string, reason: string | undefined, rows: Row[], limit = 200) {
  return async <T>(path: string): Promise<T> => {
    const url = new URL(path, "http://canyon.test");
    if (url.pathname === "/api/v1/gate-runs/gr_1") return { run: { id: "gr_1", state, failure_reason: reason } } as T;
    if (url.pathname === "/api/v1/gate-runs/gr_1/timeline") {
      const after = Number(url.searchParams.get("after") ?? 0);
      const page = rows.filter((r) => r.id > after).slice(0, limit);
      return { rows: page, next: page.length ? page[page.length - 1].id : null } as T;
    }
    throw new Error(`GET ${path}: 404`);
  };
}

const lines = (n: number): Row[] => Array.from({ length: n }, (_, i) => ({ id: i + 1, kind: "build.log", data: { line: `line ${i + 1}` } }));

test("why names the run's state and reason, then its last 30 timeline rows, across pages", async () => {
  const said = await why("gr_1", host("failed", "build", lines(250), 100));
  assert.match(said, /^run gr_1 ended failed \(build\)\. Its last 30 timeline rows:\n/);
  assert.ok(said.includes("  build.log line 221\n") && said.endsWith("  build.log line 250"), said);
  assert.ok(!said.includes("line 220\n"), "a row before the last 30 is left out");
});

test("why writes a row that isn't a log line as its kind and data, cut short", async () => {
  const said = await why("gr_1", host("blocked", undefined, [{ id: 1, kind: "verdict", data: { state: "blocked", findings: "x".repeat(500) } }]));
  assert.match(said, /^run gr_1 ended blocked\. Its last 1 timeline rows:\n  verdict \{"state":"blocked","findings":"x+…$/);
  assert.ok(said.length < 300, said);
});

test("why says when the run can't be read, and never throws", async () => {
  const said = await why("gr_1", async () => { throw new Error("GET /api/v1/gate-runs/gr_1: 502 bad gateway"); });
  assert.equal(said, "run gr_1 could not be read: Error: GET /api/v1/gate-runs/gr_1: 502 bad gateway");
});
