import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { LicenceState, Preferences } from "../api";
import { S } from "../strings";
import { LicenceSettings } from "./Licence";
import { Settings } from "./Settings";

// The contract's golden reads (contracts/testdata/licence/licence.json; #2879): a change to their shape breaks this test.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/licence/licence.json"), "utf8")) as
  { admin: LicenceState; member: LicenceState; lapsed: LicenceState; invalid: LicenceState; none: LicenceState; set: { licence: string } };
const prefs = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences; preferences_member: Preferences };
const LICENCE = "/api/v1/licence";

// The host answers the licence read with `read`, a PUT with `put`, the preferences and sign-in mode for Settings, and
// anything else 404. It returns every call made.
function host(read: LicenceState, put: { status: number; body: unknown } = { status: 200, body: read }, record: Preferences = prefs.preferences_admin) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    const { status, body } = url === LICENCE ? (method === "PUT" ? put : { status: 200, body: read })
      : url === "/api/v1/preferences" ? { status: 200, body: record } : url === "/auth/config" ? { status: 200, body: { auth: "oidc" } }
        : { status: 404, body: { error: "not_found", message: "not found" } };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}${init?.body === undefined ? "" : ` ${String(init.body)}`}`);
}

afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.location.hash = ""; });

describe("Settings › Licence", () => {
  it("says a licence in force, what it holds, and offers to replace it", async () => {
    host(golden.admin);
    render(<LicenceSettings />);
    expect(await screen.findByText("Canyon Enterprise, licensed to Meridian Insurance until 14 Nov 2027.")).toBeDefined();
    expect(screen.getByText(S.licenceSeats).nextElementSibling?.textContent).toBe("50");
    expect(screen.getByText(S.licenceValidUntil).nextElementSibling?.textContent).toBe("14 Nov 2027");
    for (const name of [S.featureOutbound, S.featurePatching, S.featureGroups]) expect(screen.getByText(name)).toBeDefined();
    expect(screen.getByLabelText(S.licencePaste)).toBeDefined();
    // Its Save shows once something is pasted, and nothing typed asks nothing on leaving (D16).
    expect(screen.queryByRole("button", { name: S.licenceSave })).toBeNull();
    fireEvent.change(screen.getByLabelText(S.licencePaste), { target: { value: "canyon-licence-v1 x" } });
    expect(screen.getByRole("button", { name: S.licenceSave })).toBeDefined();
  });

  it("says each other state in D5's words", async () => {
    const lines: [LicenceState, string][] = [
      [{ ...golden.admin, state: "grace" }, "Canyon Enterprise, licensed to Meridian Insurance, ended on 14 Nov 2027. Its features stay unlocked until 14 Dec 2027; after that, what's set keeps working and can't be changed."],
      [golden.lapsed, "Canyon Enterprise, licensed to Meridian Insurance, ended on 14 Nov 2027. What's set keeps working; Enterprise features can't be changed until an admin adds a new licence."],
      [golden.invalid, S.licenceUnsigned],
      [{ ...golden.invalid, problem: "unreadable" }, S.licenceUnreadable],
      [golden.none, S.licenceNone],
    ];
    for (const [read, line] of lines) {
      host(read);
      render(<LicenceSettings />);
      expect(await screen.findByText(line)).toBeDefined();
      cleanup();
    }
  });

  it("names a feature this Canyon doesn't know by its id", async () => {
    host({ ...golden.admin, features: ["outbound_keys", "audit_export"] });
    render(<LicenceSettings />);
    expect(await screen.findByText(S.featureOutbound)).toBeDefined();
    expect(screen.getByText("audit_export")).toBeDefined();
  });

  it("offers no paste box while CANYON_LICENSE sets the licence, and says so", async () => {
    host(golden.lapsed);
    render(<LicenceSettings />);
    expect(await screen.findByText(S.licenceFromEnv)).toBeDefined();
    expect(screen.queryByLabelText(S.licencePaste)).toBeNull();
    expect(screen.queryByRole("button", { name: S.licenceSave })).toBeNull();
  });

  it("saves a pasted licence as written and shows the licence after it", async () => {
    const calls = host(golden.none, { status: 200, body: golden.admin });
    render(<LicenceSettings />);
    fireEvent.change(await screen.findByLabelText(S.licencePaste), { target: { value: `  ${golden.set.licence}\n` } });
    fireEvent.click(screen.getByRole("button", { name: S.licenceSave }));
    expect(await screen.findByText("Canyon Enterprise, licensed to Meridian Insurance until 14 Nov 2027.")).toBeDefined();
    expect(calls().filter((call) => call.startsWith("PUT"))).toEqual([`PUT ${LICENCE} ${JSON.stringify(golden.set)}`]);
    expect(screen.getByText("Saved.")).toBeDefined(); // D16
  });

  it("shows a refused licence in the server's own words", async () => {
    const message = "This isn't a Canyon licence, or Canyon didn't sign it.";
    host(golden.none, { status: 400, body: { error: "invalid_licence", message } });
    render(<LicenceSettings />);
    fireEvent.change(await screen.findByLabelText(S.licencePaste), { target: { value: "{}" } });
    fireEvent.click(screen.getByRole("button", { name: S.licenceSave }));
    expect(await screen.findByText(message)).toBeDefined();
    expect(screen.getByText(S.licenceNone)).toBeDefined();
  });

  // design/PERMISSIONS.md, Licence: the tab is an admin's; a member who follows a link reads one line, and the screen
  // never asks the licence routes for them.
  it("is an admin's tab: a member reads one line and the routes are never asked", async () => {
    window.location.hash = "#/settings/licence";
    const calls = host(golden.member, undefined, prefs.preferences_member);
    render(<Settings />);
    expect(await screen.findByText(S.licenceAdmin)).toBeDefined();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls().filter((call) => call.includes(LICENCE))).toEqual([]);
    cleanup();
    host(golden.admin);
    render(<Settings />);
    expect(await screen.findByText("Canyon Enterprise, licensed to Meridian Insurance until 14 Nov 2027.")).toBeDefined();
    await waitFor(() => expect(screen.getByRole("link", { name: "Licence" }).getAttribute("aria-current")).toBe("page"));
  });
});
