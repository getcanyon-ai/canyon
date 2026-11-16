import { readFileSync } from "node:fs";
import { join } from "node:path";
import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { EgressCA, EgressSetting, Preferences } from "../api";
import { S } from "../strings";
import { OutboundKeys } from "./OutboundKeys";
import { Settings } from "./Settings";

const read = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
const egress = read<{ setting_admin: EgressSetting & { ca: EgressCA }; setting_member: EgressSetting }>("egress/claims-portal.json");
const prefs = read<{ preferences_admin: Preferences; preferences_member: Preferences }>("guardrails/preferences.json");
const ca = egress.setting_admin.ca;
// A sub-CA's certificate and key in their shapes, never real ones (AGENTS.md, Fixtures), built here so no file holds them.
const CERT = "-----BEGIN CERTIFICATE-----\nTUlJQ2V4YW1wbGU=\n-----END CERTIFICATE-----\n";
const KEY_BODY = "TUlJRXZnSUJBREFOQmdrcWhraUc5dzBCQVFFRkFBU0NCS2d3Z2dTa0FnRUFBb0lCQVFD";
const KEY = `-----BEGIN PRIVATE KEY-----\n${KEY_BODY}\n-----END PRIVATE KEY-----\n`;

type Answer = { status: number; body?: unknown };
function server(answers: Record<string, Answer>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const answer = answers[`${init?.method ?? "GET"} ${url}`] ?? { status: 404, body: { error: "not_found", message: "not found" } };
    return { ok: answer.status < 300, status: answer.status, json: async () => answer.body, headers: new Headers() } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([, init]) => init?.method === "POST").map(([url, init]) => [url, JSON.parse(String(init?.body)) as unknown]);
}
const NOW = Date.parse("2026-11-12T00:00:00Z");
// Settings › Outbound holds the read and adopts what a rotation answers; this holds it the same way.
function Keys({ setting, now = NOW }: { setting: EgressSetting; now?: number }) {
  const [held, setHeld] = useState(setting);
  return <OutboundKeys setting={held} adopt={setHeld} now={now} />;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.location.hash = "";
});

describe("Settings › Outbound's certificates", () => {
  // design/EGRESS.md §8.3: key dates, never a key: each root, the one signing, and when Canyon replaces its own.
  it("lists each root with its dates, the one signing marked", () => {
    render(<Keys setting={egress.setting_admin} />);
    expect(screen.getAllByRole("listitem").map((one) => one.textContent)).toEqual([
      `${S.keysCanyon}SHA-256 ca7b8f9fa5bcee3b…made 1 Oct 2026 · valid until 1 Oct 2031 · in use until 1 Oct 2031`,
      `${S.keysCanyon}SHA-256 d4ee9f8ddd95a685…${S.keysSigning}made 11 Jan 2026 · valid until 11 Jan 2031 · in use until 30 Nov 2026`,
    ]);
    expect(screen.getByText("Canyon replaces its root on 2 Aug 2031, and apps trust both for 60 days.")).toBeDefined();
  });

  it("says nothing while the read names no roots, as a member's and a server's from before them answer", () => {
    render(<Keys setting={egress.setting_member} />);
    expect(screen.queryByRole("region")).toBeNull();
  });

  it("replaces Canyon's root once asked, and shows the roots the server answers", async () => {
    const replaced = { ...ca, roots: [{ ...ca.roots[0]!, fingerprint: "f".repeat(64) }, ...ca.roots], signing: "f".repeat(64) };
    const posts = server({ "POST /api/v1/egress/ca/rotate": { status: 200, body: { ...egress.setting_admin, ca: replaced } } });
    render(<Keys setting={egress.setting_admin} />);
    fireEvent.click(screen.getByRole("button", { name: S.keysRotate }));
    expect(screen.getByText(S.keysRotateAsk)).toBeDefined();
    expect(posts()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    expect((await screen.findByRole("status")).textContent).toBe(S.keysSaved);
    expect(posts()).toEqual([["/api/v1/egress/ca/rotate", {}]]);
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
  });

  // The sub-CA's key goes to the server once and is then nowhere: not in the page, the field or a log.
  it("sends a sub-CA's key once and never shows it again", async () => {
    const posts = server({ "POST /api/v1/egress/ca/rotate": { status: 200, body: { ...egress.setting_admin, ca: { ...ca, rotates_at: null } } } });
    const logged = (["log", "info", "warn", "error", "debug"] as const).map((level) => vi.spyOn(console, level));
    render(<Keys setting={egress.setting_admin} />);
    const form = within(screen.getByRole("form", { name: S.keysByoHeading }));
    fireEvent.change(form.getByLabelText("Certificates: the sub-CA first, ending at your root"), { target: { value: CERT } });
    fireEvent.change(form.getByLabelText("Private key (PKCS #8)"), { target: { value: KEY } });
    expect(document.body.innerHTML).not.toContain(KEY_BODY);
    fireEvent.click(form.getByRole("button", { name: "Use this sub-CA" }));
    expect((await screen.findByRole("status")).textContent).toBe(S.keysSaved);
    expect(posts()).toEqual([["/api/v1/egress/ca/rotate", { cert: CERT, key: KEY }]]);
    expect((form.getByLabelText("Private key (PKCS #8)") as HTMLTextAreaElement).value).toBe("");
    expect(document.body.innerHTML).not.toContain(KEY_BODY);
    for (const spy of logged) expect(JSON.stringify(spy.mock.calls)).not.toContain(KEY_BODY);
  });

  it("says the server's refusal in its words", async () => {
    server({ "POST /api/v1/egress/ca/rotate": { status: 409, body: { error: "rotating", message: "a rotation is under way", ends_at: "2026-11-30T09:00:00Z" } } });
    render(<Keys setting={egress.setting_admin} />);
    fireEvent.click(screen.getByRole("button", { name: S.keysRotate }));
    fireEvent.click(screen.getByRole("button", { name: "Replace" }));
    expect((await screen.findByRole("alert")).textContent).toBe("a rotation is under way");
  });

  // An admin's sub-CA never rotates by itself (design/EGRESS.md §4.5): its end is warned of from 60 days before it.
  it("warns of a sub-CA's end from 60 days before it", () => {
    const byo = { fingerprint: "e".repeat(64), byo: true, created_at: "2026-11-01T00:00:00Z", not_after: "2027-01-01T00:00:00Z", ends_at: "2027-01-01T00:00:00Z" };
    const setting = { ...egress.setting_admin, ca: { signing: byo.fingerprint, roots: [byo], rotates_at: null } };
    render(<Keys setting={setting} now={Date.parse("2026-11-03T00:00:00Z")} />);
    expect(screen.getByRole("note").textContent).toBe("Your sub-CA ends on 1 Jan 2027. Canyon doesn't replace it on its own: bring a new one, or switch back to Canyon's own root, before then.");
    cleanup();
    render(<Keys setting={setting} now={Date.parse("2026-11-01T00:00:00Z")} />);
    expect(screen.getByText(S.keysByo)).toBeDefined();
    expect(screen.queryByRole("note")).toBeNull(); // 61 days ahead
  });
});

// design/EGRESS.md §3.5: host settings are an admin's. One allowed and one refused caller at the boundary.
describe("who sees the certificates", () => {
  const settings = (record: Preferences, setting: EgressSetting) => {
    window.location.hash = "#/settings/outbound";
    server({
      "GET /auth/config": { status: 200, body: { canyon: true, auth: "oidc" } },
      "GET /api/v1/preferences": { status: 200, body: record },
      "GET /api/v1/egress": { status: 200, body: setting },
    });
    render(<Settings />);
  };

  it("shows an admin the roots and the forms, and a member none of them", async () => {
    settings(prefs.preferences_admin, egress.setting_admin);
    expect(await screen.findByRole("heading", { name: S.keysHeading })).toBeDefined();
    expect(screen.getByRole("button", { name: S.keysRotate })).toBeDefined();
    cleanup();
    // the admin's read, though the server answers a member none of it: the screen's own check, not the read's, is under test
    settings(prefs.preferences_member, egress.setting_admin);
    await screen.findByText(S.outboundAdmin);
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.queryByRole("heading", { name: S.keysHeading })).toBeNull();
    expect(screen.queryByRole("button", { name: S.keysRotate })).toBeNull();
  });
});
