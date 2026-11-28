import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Preferences } from "../api";
import { DATABASES } from "../strings";
import { DatabasesSettings } from "./Databases";

const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as {
  preferences_admin: Preferences;
  preferences_member: Preferences;
};

// The server answers "PUT /api/v1/preferences" with the row it is given and records what it was sent.
function server(answer: Preferences) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (`${init?.method} ${url}` !== "PUT /api/v1/preferences") return { ok: false, status: 404, json: async () => ({ error: "not_found" }) } as unknown as Response;
    return { ok: true, status: 200, json: async () => answer } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([, init]) => JSON.parse(String(init?.body)) as Record<string, unknown>);
}

afterEach(cleanup);

// Settings › Databases (#2835): an admin turns database sessions on, and the screen shows what it now is; a member
// reads whether they are on, with nothing to change.
describe("Settings › Databases", () => {
  it("an admin turns database sessions on", async () => {
    const sent = server({ ...golden.preferences_admin, db_sessions: "on" });
    let row: Preferences = golden.preferences_admin;
    const { rerender } = render(<DatabasesSettings record={row} admin adopt={(next) => { row = next; }} />);
    expect(screen.getByText(DATABASES.off)).toBeTruthy();
    expect(screen.getByText(DATABASES.turnOnMeans)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: DATABASES.turnOn }));
    // It opens a way into apps' databases, so it asks first, saying what changes, and nothing is sent before yes (D16).
    const ask = within(screen.getByRole("group", { name: DATABASES.turnOnAsk }));
    expect(ask.getByText(DATABASES.turnOnMeans)).toBeTruthy();
    expect(sent()).toEqual([]);
    fireEvent.click(ask.getByRole("button", { name: DATABASES.turnOn }));
    await waitFor(() => expect(sent()).toEqual([{ db_sessions: "on" }]));
    await waitFor(() => expect(row.db_sessions).toBe("on"));
    rerender(<DatabasesSettings record={row} admin adopt={() => undefined} />);
    expect(screen.getByText(DATABASES.on)).toBeTruthy();
    expect(screen.getByRole("button", { name: DATABASES.turnOff })).toBeTruthy();
    expect(screen.getByText("Saved.")).toBeTruthy(); // a choice says it saved (D16)
  });

  // Turning them off ends every open session, so it asks first, saying so, and Cancel sends nothing (D16).
  it("asks before turning database sessions off, and sends nothing on Cancel", async () => {
    const sent = server({ ...golden.preferences_admin, db_sessions: "off" });
    render(<DatabasesSettings record={{ ...golden.preferences_admin, db_sessions: "on" }} admin adopt={() => undefined} />);
    fireEvent.click(screen.getByRole("button", { name: DATABASES.turnOff }));
    const ask = screen.getByRole("group", { name: DATABASES.turnOffAsk });
    expect(within(ask).getByText(DATABASES.turnOffMeans)).toBeTruthy();
    expect(sent()).toEqual([]);
    fireEvent.click(within(ask).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("group", { name: DATABASES.turnOffAsk })).toBeNull();
    expect(sent()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: DATABASES.turnOff }));
    fireEvent.click(within(screen.getByRole("group", { name: DATABASES.turnOffAsk })).getByRole("button", { name: DATABASES.turnOff }));
    await waitFor(() => expect(sent()).toEqual([{ db_sessions: "off" }]));
  });

  it("a member reads the switch, and changes nothing", () => {
    server(golden.preferences_member);
    render(<DatabasesSettings record={{ ...golden.preferences_member, db_sessions: "on" }} admin={false} adopt={() => undefined} />);
    expect(screen.getByText(DATABASES.on)).toBeTruthy();
    expect(screen.getByText(DATABASES.admin)).toBeTruthy();
    expect(screen.queryByRole("button")).toBeNull();
  });
});
