import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, getDefaultNormalizer, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FleetPage, Preferences, TelemetryState } from "../api";
import { S } from "../strings";
import { Fleet } from "./Fleet";
import { Settings } from "./Settings";
import { TelemetrySettings } from "./Telemetry";

// The contract's golden report and state (contracts/testdata/telemetry/report.json).
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/telemetry/report.json"), "utf8")) as { report: unknown; state: Omit<TelemetryState, "report">; state_retrying: Omit<TelemetryState, "report"> };
const fleet = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8")) as FleetPage;
const prefs = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences; preferences_member: Preferences };
const asSent = JSON.stringify(golden.report); // the server's compact bytes, as its read's string holds them
const on: TelemetryState = { ...golden.state, report: asSent, enabled: true, off_because: undefined };

// The host answers telemetry from state, the fleet and preferences from their goldens, and anything else as an empty
// object. Each call is recorded with whether the notice was on screen when it was made.
function host(state: TelemetryState, page: FleetPage = fleet, record: Preferences = prefs.preferences_admin) {
  const calls: { method: string; url: string }[] = [];
  let now = state;
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    calls.push({ method, url });
    let body: unknown = {};
    const status = 200;
    if (url === "/api/v1/telemetry" && method === "PUT") { const on = (JSON.parse(String(init?.body)) as { enabled: boolean }).enabled; now = { ...now, enabled: on, off_because: on ? undefined : "admin" }; body = now; }
    else if (url === "/api/v1/telemetry") body = now;
    else if (url.startsWith("/api/v1/fleet")) body = page;
    else if (url === "/api/v1/preferences") body = record;
    else if (url === "/auth/config") body = { auth: "oidc" };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  }));
  return calls;
}
const telemetryCalls = (calls: ReturnType<typeof host>) => calls.filter((c) => c.url.startsWith("/api/v1/telemetry"));

afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.location.hash = ""; });

describe("the fleet", () => {
  it("shows no telemetry notice and never calls the telemetry routes, an admin's fleet included: it is on by default", async () => {
    for (const role of ["admin", "member"] as const) {
      const calls = host(on, { ...fleet, viewer: { ...fleet.viewer, role } });
      render(<Fleet org="Meridian" />);
      await waitFor(() => expect(calls.some((c) => c.url.startsWith("/api/v1/fleet"))).toBe(true));
      await new Promise((r) => setTimeout(r, 50));
      expect(telemetryCalls(calls)).toEqual([]);
      cleanup();
    }
  });
});

describe("Settings › Telemetry", () => {
  it("says whether it is on and why, shows the next report exactly as it is sent and the last one, and switches it", async () => {
    const calls = host({ ...on, enabled: false, off_because: "unreachable", last_sent_at: null, last_report: null });
    render(<TelemetrySettings />);
    await screen.findByText(S.telemetryOffUnreachable);
    expect(screen.getByText(asSent)).toBeTruthy();
    expect(screen.getByText(S.telemetryNotSent)).toBeTruthy();
    expect(telemetryCalls(calls).map((c) => c.method)).toEqual(["GET"]); // a read, and nothing said about having seen it
    expect(screen.getByRole("link", { name: S.telemetryFields }).getAttribute("href")).toBe("https://www.getcanyon.ai/docs/org/usage-reports/");
    fireEvent.click(screen.getByRole("button", { name: S.telemetryTurnOn }));
    await screen.findByText(S.telemetryOn);
    fireEvent.click(screen.getByRole("button", { name: S.telemetryTurnOff }));
    await screen.findByText(S.telemetryOffAdmin);
  });

  it("says while no report has reached the collector that it still tries, when next and until when", async () => {
    host({ ...golden.state_retrying, report: asSent, next_try_at: "2026-11-24T21:04:00", gives_up_at: "2027-02-21T14:04:00" }); // local times, whatever the machine's zone
    render(<TelemetrySettings />);
    await screen.findByText("Telemetry is on, but no report has reached Canyon's collector yet. Canyon tries again after 24 Nov, 21:04, and gives up by 21 Feb.");
    expect(screen.getByRole("button", { name: S.telemetryTurnOff })).toBeTruthy();
    expect(screen.queryByText(S.telemetryOn)).toBeNull();
  });

  it("names the last report sent and when", async () => {
    host({ ...on, enabled: true, off_because: undefined, last_sent_at: new Date(Date.now() - 3 * 3600_000).toISOString(), last_report: asSent });
    render(<TelemetrySettings />);
    await screen.findByText(S.telemetryLast.replace("<when>", "3 hours ago"));
    expect(screen.getAllByText(asSent)).toHaveLength(2);
  });

  it("is an admin's page: a member sees no Telemetry tab and the page never calls the routes", async () => {
    window.location.hash = "#/settings/telemetry";
    const calls = host(on, fleet, prefs.preferences_member);
    render(<Settings />);
    await waitFor(() => expect(calls.some((c) => c.url === "/api/v1/preferences")).toBe(true));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole("link", { name: "Telemetry" })).toBeNull();
    expect(telemetryCalls(calls)).toEqual([]);
  });
});

describe("the report as sent", () => {
  it("shows the next report and the last one sent exactly as the host's strings hold them, never re-written", async () => {
    // the host answers each report as a string of the send's bytes: spacing JSON.stringify would change stays
    const next = '{"version":"v0.9.0", "apps":7,"checks":{"passed":12,  "advisory":1,"blocked":3}}';
    const last = '{"apps": 6,"version":"v0.9.0"}';
    host({ ...on, enabled: true, off_because: undefined, report: next, last_sent_at: "2026-11-12T09:00:00Z", last_report: last });
    render(<TelemetrySettings />);
    const exact = { normalizer: getDefaultNormalizer({ trim: false, collapseWhitespace: false }) }; // every space, as sent
    await screen.findByText(next, exact);
    expect(screen.getByText(last, exact)).toBeTruthy();
  });
});
