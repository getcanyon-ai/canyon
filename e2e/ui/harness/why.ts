// why is what a run that went wrong says of itself, for a seeding step's error: its state and failure reason, then
// its last timeline rows (contracts/events.md, the run scope), so a red names its cause rather than "see its
// timeline", which is gone with the stack. get reads a path of Canyon's API. It never throws: a run that can't be
// read says so, and the error it is part of still stands.
export type Get = <T>(path: string) => Promise<T>;

type Row = { id: number; kind: string; data: Record<string, unknown> };

export async function why(id: string, get: Get, last = 30): Promise<string> {
  try {
    const { run } = await get<{ run: { state: string; failure_reason?: string } }>(`/api/v1/gate-runs/${id}`);
    const rows: Row[] = [];
    for (let after = 0; ; ) {
      const page = await get<{ rows: Row[]; next: number | null }>(`/api/v1/gate-runs/${id}/timeline?after=${after}&limit=200`);
      rows.push(...page.rows);
      if (page.rows.length === 0 || page.next === null) break;
      after = page.next;
    }
    const tail = rows.slice(-last).map((r) => `  ${r.kind} ${said(r)}`);
    return `run ${id} ended ${run.state}${run.failure_reason ? ` (${run.failure_reason})` : ""}. Its last ${tail.length} timeline rows:\n${tail.join("\n")}`;
  } catch (err) {
    return `run ${id} could not be read: ${err}`;
  }
}

// said is a row in one line: a build.log row's line, else its data, cut to 200 characters (a verdict holds a whole SARIF log).
function said(r: Row): string {
  const text = r.kind === "build.log" && typeof r.data.line === "string" ? r.data.line : JSON.stringify(r.data);
  return text.length > 200 ? `${text.slice(0, 200)}…` : text;
}
