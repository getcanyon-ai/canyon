import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { S } from "../strings";
import { Progress, progressWords, useProgress, type Step } from "./Progress";

const TIMELINE = "/api/v1/gate-runs/gr_live/timeline";
const at = (s: number) => new Date(Date.UTC(2026, 9, 5, 12, 0, s)).toISOString().replace(".000", "");
// A review going, as its timeline says it (contracts/events.md, progress and check.*): gitleaks done with its
// report, trivy waiting second in line, opengrep running, the AI review done with one finding, the build queued.
const rows = [
  { id: 1, ts: at(0), kind: "progress", data: { step: "scan", tool: "gitleaks", status: "running" } },
  { id: 2, ts: at(3), kind: "producer.sarif", data: { producer: "sarif/gitleaks", sarif: { runs: [{ results: [] }] } } },
  { id: 3, ts: at(3), kind: "progress", data: { step: "scan", tool: "gitleaks", status: "done", duration_ms: 3000 } },
  { id: 4, ts: at(4), kind: "progress", data: { step: "scan", tool: "trivy", status: "queued", position: 2 } },
  { id: 5, ts: at(5), kind: "progress", data: { step: "scan", tool: "opengrep", status: "running" } },
  { id: 6, ts: at(6), kind: "check.started", data: { check: "ai-review" } },
  { id: 7, ts: at(57), kind: "check.completed", data: { check: "ai-review", status: "findings", findings: 1, duration_ms: 119600 } },
  { id: 8, ts: at(58), kind: "progress", data: { step: "build", status: "queued" } },
];

function Harness({ going }: { going: boolean }) {
  const steps = useProgress("gr_live", going);
  return <><p>{progressWords(steps)}</p><Progress steps={steps} /></>;
}
// Each group is its own list, under its name: a group's name is never one of its steps.
const steps = (group: string) => within(screen.getByRole("list", { name: group })).getAllByRole("listitem").map((one) => one.textContent);

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("a review as it goes", () => {
  it("says each check's state, its place in line and its time, then the build", async () => {
    vi.useFakeTimers({ now: new Date(at(27)) });
    vi.stubGlobal("fetch", vi.fn(async (url: string) => ({ ok: true, status: 200, json: async () => (url === `${TIMELINE}?after=0` ? { rows, next: 8 } : { rows: [], next: 8 }) }) as unknown as Response));
    render(<Harness going />);
    await vi.waitFor(() => expect(screen.getByRole("list", { name: "Review" })).toBeDefined());
    expect(steps("Review")).toEqual([
      "gitleaks nothing found3 s",
      "trivy Waiting its turn: 2nd in line on this hostqueued",
      "opengrep running22 s",
      "AI review 1 finding2 min 0 s", // whole seconds, never "1 min 60 s"
    ]);
    expect(steps("After review")).toEqual(["Build Waiting its turn on this hostqueued"]);
    expect(screen.getByText("2 of 4 checks done. trivy is waiting its turn on this host.")).toBeDefined();
  });

  // A deploy that reuses an image runs no build: its one build.log row says so (contracts/events.md,
  // build.log), and the build step says it reused the image, never nothing at all (#1666).
  it("says the build reused an image, when the run built none", async () => {
    const reused = [
      { id: 1, ts: at(0), kind: "build.log", data: { line: "Reusing the image built for 22bc90 (no build value changed)" } },
      { id: 2, ts: at(1), kind: "progress", data: { step: "start", status: "running" } },
    ];
    vi.useFakeTimers({ now: new Date(at(2)) });
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ rows: reused, next: null }) }) as unknown as Response));
    render(<Harness going />);
    await vi.waitFor(() => expect(screen.getByRole("list", { name: "After review" })).toBeDefined());
    expect(steps("After review")).toEqual(["Build reused the image built for 22bc90", "Start in preview running1 s"]);
  });

  it("says the real build once the reused image turns out missing", async () => {
    const rebuilt = [
      { id: 1, ts: at(0), kind: "build.log", data: { line: "Reusing the image built for 22bc90 (no build value changed)" } },
      { id: 2, ts: at(1), kind: "progress", data: { step: "build", status: "running" } },
    ];
    vi.useFakeTimers({ now: new Date(at(3)) });
    vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ rows: rebuilt, next: null }) }) as unknown as Response));
    render(<Harness going />);
    await vi.waitFor(() => expect(screen.getByRole("list", { name: "After review" })).toBeDefined());
    expect(steps("After review")).toEqual(["Build running2 s"]);
  });

  // The first deploy on a host waits for Canyon's build image before the build's first line (contracts/events.md, build.image):
  // the Build step counts that wait and names it, as the CLI's row does (D5; #2749). Without the row, the build's own time.
  it("counts the wait for Canyon's build image in the Build step, and names it", async () => {
    const built = (waited: boolean) => [
      ...(waited ? [{ id: 1, ts: at(0), kind: "build.image", data: { waited_ms: 278000 } }] : []),
      { id: 2, ts: at(1), kind: "progress", data: { step: "build", status: "running" } },
      { id: 3, ts: at(53), kind: "progress", data: { step: "build", status: "done", duration_ms: 52000 } },
    ];
    for (const [waited, want] of [[true, "Build done, 4 min 38 s of it preparing Canyon's images5 min 30 s"], [false, "Build done52 s"]] as const) {
      vi.stubGlobal("fetch", vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ rows: built(waited), next: null }) }) as unknown as Response));
      render(<Harness going />);
      await vi.waitFor(() => expect(screen.getByRole("list", { name: "After review" })).toBeDefined());
      expect(steps("After review")).toEqual([want]);
      cleanup();
    }
  });

  // The words are the CLI's, byte for byte (D5): its golden deploy, which waited, says them with its own clock.
  it("names the wait in the CLI's own words", () => {
    const cli = readFileSync(join(import.meta.dirname, "../../../cli/testdata/deploy-prepared.txt"), "utf8");
    expect(cli).toContain(S.buildPrepared.replace("<wait>", "4m 38s"));
  });

  it("reads nothing for a run that isn't going, and says nothing", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    function Idle() { const s: Step[] = useProgress(null, false); return <Progress steps={s} />; }
    const { container } = render(<Idle />);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(fetch).not.toHaveBeenCalled();
    expect(container.textContent).toBe("");
  });

  it("says nothing of checks there are none of yet", () => {
    expect(progressWords([])).toBeNull();
    expect(progressWords([{ key: "build:", kind: "after", name: "Build", status: "running" }])).toBeNull();
  });
});
