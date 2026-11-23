import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Pending } from "../api";
import { EnvCell, EnvLink, EnvOpen, EnvVersion, notRunning, restarts } from "./EnvLink";

describe("EnvLink", () => {
  afterEach(() => vi.useRealTimers());

  it("renders the environment as an anchor on its URL", () => {
    render(<EnvLink url="http://claims-portal.prod.localhost:7777" version="pkg_a1" deployedAt={new Date().toISOString()} />);
    const link = screen.getByRole("link");
    expect(link.getAttribute("href")).toBe("http://claims-portal.prod.localhost:7777");
    expect(link.textContent).toContain("claims-portal.prod.localhost:7777");
    expect(screen.getByText(/pkg_a1/)).toBeDefined();
  });

  // routes.md's envs row: an environment waiting on its first release (env_pending alone) is drawn by
  // the release it waits on, never as a link to an address it does not have yet.
  it("draws an environment waiting on its first release by its release", () => {
    const sentAt = "2026-09-27T10:00:00Z";
    const clock = new Date(sentAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    const lines: [Pending, string][] = [
      [{ via: "webhook" as const, release: "rel_8f2a", event: "deploy", sent_at: sentAt, state: "sent" }, `Sent to your platform at ${clock}, not running yet`],
      [{ via: "webhook" as const, release: "rel_8f2a", event: "deploy", sent_at: sentAt, state: "accepted" }, `Sent to your platform at ${clock}, not running yet`],
      [{ via: "webhook" as const, release: "rel_8f2a", event: "remove", sent_at: sentAt, state: "accepted" }, "Being removed by your platform"],
      [{ via: "webhook" as const, release: "rel_8f2a", event: "deploy", sent_at: sentAt, state: "undelivered" }, "Your platform did not accept the release"],
      [{ via: "webhook" as const, release: "rel_8f2a", event: "deploy", sent_at: sentAt, state: "refused" }, "Your platform refused the release"],
    ];
    for (const [pending, line] of lines) {
      const { container, unmount } = render(<EnvCell env={{ run_by: "org", pending }} />);
      expect(container.textContent).toBe(line);
      expect(screen.queryByRole("link")).toBeNull();
      unmount();
    }
  });

  // A release open over what runs (a redeploy or a removal under org): the live version stays, and
  // the release it waits on is said beside it, never silently gone (HAND-OFF §4).
  it("says the release a deployed environment waits on beside what runs there", () => {
    const pending: Pending = { via: "webhook" as const, release: "rel_7a15", event: "remove", sent_at: "2026-09-02T11:30:00Z", state: "accepted" };
    const { container } = render(<EnvCell env={{ url: "http://claims-archive.prod.localhost:7777", package: "pkg_8b21", label: "v3.1", image_digest: "sha256:767d", deployed_at: "2026-08-24T13:55:02Z", gate_run: "gr_2e88", deployed_by: null, run_by: "org", pending }} />);
    expect(screen.getByRole("link").getAttribute("href")).toBe("http://claims-archive.prod.localhost:7777");
    expect(container.textContent).toContain("Being removed by your platform");
  });

  // A version that keeps restarting opens nothing, up or not at this instant, and says so in red; its
  // count reads as the status read gives it (contracts/routes.md, envs.<env>.state; #1722).
  it("draws an environment that keeps restarting as not running, with no link", () => {
    const at = { url: "http://case-dies-later.preview.localhost:7777", package: "pkg_36e2", label: "v2", image_digest: "sha256:36e2", deployed_at: new Date(Date.now() - 2 * 3600_000).toISOString(),
      gate_run: "gr_1", deployed_by: null, run_by: "canyon" as const, state: "restarting" as const, restarting: { restarts: 7 } };
    render(<EnvVersion env={at} name="preview" />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText("Keeps restarting").className).toContain("text-critical");
    expect(notRunning(at)).toBe("Keeps restarting");
    expect(notRunning({ url: at.url, state: "paused" as never })).toBe("Couldn't check whether it runs"); // a state this build does not know is never live
    expect(restarts(at)).toBe("7 times since it was deployed 2 hours ago");
    expect(restarts({ ...at, restarting: { restarts: 1 } })).toBe("once since it was deployed 2 hours ago");
  });

  // The fleet says where the org's pods stopped checking in, or never began (EXPERIENCE.md D5; #2455), by the
  // app page's one rule: the amber line under the version on a team row's cell, and on a read-only member's,
  // whose environment carries seen_at but no deploy time. A fresh check-in, a report within its first ten
  // minutes, a pod still pulling while its environment is removed and an environment Canyon runs say nothing.
  it("says under the version where the org's pods stopped checking in, or never began", () => {
    // the clock pinned at midday: 20 minutes before a run just after midnight is yesterday, which the line dates
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date(2026, 9, 28, 12, 0));
    const minutes = (n: number) => new Date(Date.now() - n * 60_000).toISOString();
    const clock = (at: string) => new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
    const org = { url: "https://ledger.prod.meridian.example", package: "pkg_1a", label: "v2", image_digest: "sha256:1a", deployed_at: minutes(120), gate_run: "gr_1", deployed_by: null, run_by: "org" as const };
    const stale = minutes(20);
    const cases: [string, Parameters<typeof EnvVersion>[0]["env"], string | null][] = [
      ["a check-in 20 minutes ago", { ...org, seen_at: stale }, `not reported since ${clock(stale)}`], // the table's short words (the UX review)
      ["a report 30 minutes ago, no check-in", { ...org, deployed_at: minutes(30) }, "not running yet"],
      ["a check-in a minute ago", { ...org, seen_at: minutes(1) }, null],
      ["a report 2 minutes ago, no check-in yet", { ...org, deployed_at: minutes(2) }, null],
      ["a pod still pulling while it is removed", { ...org, seen_at: minutes(1), pending: { via: "webhook" as const, release: "rel_1", event: "remove", sent_at: minutes(2), state: "accepted" } }, null],
      ["an environment Canyon runs", { ...org, run_by: "canyon" as const, state: "running" as const, seen_at: stale }, null],
    ];
    for (const [what, env, line] of cases) {
      const { container } = render(<EnvVersion env={env} name="production" />);
      const said = [...container.querySelectorAll("time")].map((one) => one.textContent);
      expect(said, what).toEqual(line === null ? [] : [line]);
      if (line !== null) expect(container.querySelector("time")?.closest("span")?.className, what).toContain("text-amber");
      cleanup();
    }
    // A read-only member's row: an environment with a url and when its pods last checked in, no deploy time.
    const { container } = render(<EnvOpen env={{ url: org.url, seen_at: stale }} name="production" />);
    expect(container.querySelector("time")?.textContent).toBe(`not reported since ${clock(stale)}`);
    cleanup();
    expect(render(<EnvOpen env={{ url: org.url }} name="production" />).container.querySelector("time")).toBeNull();
  });
});
