import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { Env } from "../api";
import { EnvCard } from "./EnvCards";

const env = (extra: Partial<Env> = {}): Env => ({ url: "https://claims.example.com", package: "pkg_7a1c", label: "v1.3", image_digest: "sha256:0", deployed_at: "2026-10-01T00:00:00Z", gate_run: "gr_x", deployed_by: null, state: "running", ...extra });
const card = () => within(screen.getByRole("region"));
afterEach(cleanup);

// One environment at a glance (the app-page redesign, block 2): what runs there now, where it answers,
// who put it there; empty is dashed and says so, stopped is red and says why, and only something that
// may answer has an Open link.
describe("an environment card", () => {
  it("says what runs, where, its review, who and when, and opens it", () => {
    render(<EnvCard name="Production" env={env()} empty="" note="v1.3 · passed review" who="promoted by Dana" />);
    expect(card().getByText("Running")).toBeDefined();
    expect(card().getByText("v1.3")).toBeDefined();
    expect(card().getByText(/claims\.example\.com/).closest("p")?.textContent).toMatch(/^claims\.example\.com · promoted by Dana · /);
    expect(card().getByText("v1.3 · passed review")).toBeDefined(); // D5's line, whole
    expect(card().getByRole("link", { name: "Open v1.3 in production" }).getAttribute("href")).toBe("https://claims.example.com");
  });

  it("is dashed and says so when nothing runs there", () => {
    render(<EnvCard name="Preview" env={undefined} empty="v1.4 goes here once it passes review." />);
    expect(card().getByText("Nothing running")).toBeDefined();
    expect(card().getByText("v1.4 goes here once it passes review.")).toBeDefined();
    expect(screen.getByRole("region").className).toMatch(/border-dashed/);
    expect(card().queryByRole("link")).toBeNull();
  });

  it("says a stopped one stopped and why, and opens nothing", () => {
    render(<EnvCard name="Production" env={env({ state: "stopped", stopped: { exit_code: 1, said: "boom" } })} empty="" />);
    expect(card().getByText("Stopped")).toBeDefined();
    expect(card().getByText(/exited 1 — boom/)).toBeDefined();
    expect(card().queryByRole("link")).toBeNull();
  });

  // A card that knows no reason says where it is once (#2843): never "says why" beside "has the rest".
  it("says canyon logs says why, once, when it knows no reason", () => {
    render(<EnvCard name="Preview" env={env({ state: "restarting", deployed_at: new Date().toISOString() })} empty="" />);
    expect(card().getByText("Keeps restarting").closest("section")?.textContent).toContain("canyon logs says why.");
    expect(card().queryByText(/has the rest/)).toBeNull();
  });

  it("says how one that keeps restarting last exited, when the read says it", () => {
    render(<EnvCard name="Preview" env={env({ state: "restarting", deployed_at: new Date().toISOString(), restarting: { restarts: 7, exit_code: 1, said: "lost the queue: exiting" } })} empty="" />);
    expect(card().getByText("Keeps restarting")).toBeDefined();
    expect(card().getByText(/started again 7 times since it was deployed/).closest("p")?.textContent).toMatch(/ · last exited 1 — lost the queue: exiting · canyon logs has the rest$/);
    expect(card().queryByRole("link")).toBeNull();
  });

  it("says what the app answered at its root, plainly, with its first line under it", () => {
    render(<EnvCard name="Preview" env={env({ answered: { status: 400, said: "<h1>Bad Request (400)</h1>" } })} empty="" />);
    const said = card().getByText("Answered 400 at /");
    expect(said.textContent).toBe("Answered 400 at /<h1>Bad Request (400)</h1>");
    expect(said.className).not.toMatch(/critical/); // a fact, never a verdict
    expect(card().getByText("Running")).toBeDefined();
    cleanup();
    render(<EnvCard name="Preview" env={env({ answered: { error: "no answer in 5 s" } })} empty="" />);
    expect(card().getByText("No answer at /: no answer in 5 s")).toBeDefined();
  });

  it("never calls one Canyon couldn't check running, and keeps the way to open it", () => {
    render(<EnvCard name="Preview" env={env({ state: "unknown" })} empty="" />);
    expect(card().getByText("Couldn't check whether it runs")).toBeDefined();
    expect(card().queryByText("Running")).toBeNull();
    expect(card().getByRole("link", { name: "Open v1.3 in preview" })).toBeDefined();
  });

  it("says a first release waits on the platform, with no address yet", () => {
    render(<EnvCard name="Production" env={{ run_by: "org", pending: { via: "webhook" as const, release: "rel_1", event: "deploy", sent_at: "2026-10-01T00:00:00Z", state: "refused" } } as never} empty="" />);
    expect(card().getByText("Waiting on your platform")).toBeDefined();
    expect(card().getByText("Your platform refused the release")).toBeDefined();
    expect(card().queryByRole("link")).toBeNull();
  });

  it("gives a read-only member the state and the way in, without a version", () => {
    render(<EnvCard name="Preview" env={{ url: "https://p.example.com", state: "running" }} empty="" />);
    expect(card().getByRole("link", { name: "Open preview" }).getAttribute("href")).toBe("https://p.example.com");
    expect(card().getByText("Running")).toBeDefined();
  });

  // Where the org runs it there is no executor state: deployed, never "Running"; and a read-only member's
  // stopped card names no logs, which are the team's.
  it("says deployed where Canyon has no state, and points a read-only member at no logs", () => {
    render(<EnvCard name="Production" env={env({ state: undefined })} empty="" />);
    expect(card().getByText("Deployed")).toBeDefined();
    expect(card().queryByText("Running")).toBeNull();
    cleanup();
    render(<EnvCard name="Preview" env={{ url: "https://p.example.com", state: "stopped" }} empty="" />);
    expect(card().getByText("Stopped")).toBeDefined();
    expect(card().queryByText(/canyon logs/)).toBeNull();
  });
});

// A pod's check-in, under an environment the org runs (contracts/routes.md, envs.<env>.seen_at; #2452): the
// pod's sign-in checking in, never the app, so never "Running" and never green; amber for the two warnings.
describe("an environment the org runs", () => {
  const now = new Date("2026-10-23T12:00:00Z");
  const org = (extra: Partial<Env> = {}) => env({ state: undefined, run_by: "org", deployed_at: "2026-10-23T11:00:00Z", ...extra });
  const dot = () => screen.getByRole("region").querySelector("[aria-hidden]")!.className;
  beforeEach(() => vi.useFakeTimers({ now, toFake: ["Date"] }));
  afterEach(() => vi.useRealTimers());

  it("says a pod checked in, neutral, and still says deployed", () => {
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:58:00Z" })} empty="" />);
    const line = card().getByText("A pod checked in 2m ago");
    expect(line.closest("p")?.className).not.toMatch(/amber/);
    expect(line.getAttribute("datetime")).toBe("2026-10-23T11:58:00Z");
    expect(card().getByText("Deployed")).toBeDefined();
    expect(card().queryByText("Running")).toBeNull();
    expect(dot()).not.toMatch(/bg-ok|bg-amber/);
  });

  it("says in amber that no pod has checked in since a time, the day too before today", () => {
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:50:00Z" })} empty="" />);
    const clock = new Date("2026-10-23T11:50:00Z").toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    expect(card().getByText(`No pod has checked in since ${clock}`).closest("p")?.className).toMatch(/text-amber/);
    expect(dot()).toMatch(/bg-amber/);
    cleanup();
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-21T09:00:00Z" })} empty="" />);
    const day = new Date("2026-10-21T09:00:00Z").toLocaleString([], { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" });
    expect(card().getByText(`No pod has checked in since ${day}`)).toBeDefined();
  });

  it("says in amber that no pod has checked in ten minutes after the report, and nothing before", () => {
    render(<EnvCard name="Production" env={org({ deployed_at: "2026-10-23T11:49:00Z" })} empty="" />);
    expect(card().getByText("Reported deployed, but no pod has checked in").closest("p")?.className).toMatch(/text-amber/);
    expect(dot()).toMatch(/bg-amber/);
    cleanup();
    render(<EnvCard name="Production" env={org({ deployed_at: "2026-10-23T11:52:00Z" })} empty="" />);
    expect(card().queryByText(/pod/)).toBeNull();
    expect(dot()).not.toMatch(/bg-amber/);
  });

  it("says in amber that a pod still checked in while the environment waits on its removal", () => {
    const removing = { via: "webhook" as const, release: "rel_9c2d", event: "remove" as const, sent_at: "2026-10-23T11:57:00Z", state: "accepted" as const };
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:59:00Z", pending: removing })} empty="" />);
    expect(card().getByText("A pod still checked in 1m ago").closest("p")?.className).toMatch(/text-amber/);
    expect(card().queryByText("A pod checked in 1m ago")).toBeNull();
    expect(dot()).toMatch(/bg-amber/);
    cleanup();
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:59:40Z", pending: removing })} empty="" />);
    expect(card().getByText("A pod still checked in just now")).toBeDefined();
    cleanup();
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:59:00Z", pending: { ...removing, event: "deploy" } })} empty="" />);
    expect(card().getByText("A pod checked in 1m ago").closest("p")?.className).not.toMatch(/amber/);
  });

  it("says nothing of pods where Canyon runs it", () => {
    render(<EnvCard name="Production" env={env({ run_by: "canyon", seen_at: "2026-10-23T11:00:00Z" })} empty="" />);
    expect(card().queryByText(/pod/)).toBeNull();
    expect(card().getByText("Running")).toBeDefined();
  });

  it("says just now within the first minute, as canyon status does", () => {
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T11:59:30Z" })} empty="" />);
    expect(card().getByText("A pod checked in just now")).toBeDefined();
    cleanup();
    render(<EnvCard name="Production" env={org({ seen_at: "2026-10-23T12:00:20Z" })} empty="" />); // ahead of the clock: now
    expect(card().getByText("A pod checked in just now")).toBeDefined();
  });

  it("gives a read-only member the check-in beside the way in", () => {
    render(<EnvCard name="Preview" env={{ url: "https://p.example.com", seen_at: "2026-10-23T11:59:00Z" }} empty="" />);
    expect(card().getByText("A pod checked in 1m ago")).toBeDefined();
    expect(card().getByRole("link", { name: "Open preview" })).toBeDefined();
  });

  it("says what the platform's report left out, in place of waiting since", () => {
    render(<EnvCard name="Preview" env={{ run_by: "org", pending: { via: "webhook" as const, release: "rel_1", event: "deploy", sent_at: "2026-10-23T11:00:00Z", state: "accepted", missing: ["db", "files"] } } as never} empty="" />);
    expect(card().getByText("Waiting on your platform")).toBeDefined();
    expect(card().getByText("Your platform's report left out where db, files are")).toBeDefined();
  });
});
