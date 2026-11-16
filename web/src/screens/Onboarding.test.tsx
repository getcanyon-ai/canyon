import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Preferences } from "../api";
import { Onboarding } from "./Onboarding";

// The admin's projection of the golden row, turned back into the host it was before anybody
// answered anything: no name, no lists, nothing pinned, and no setup instant.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as { preferences_admin: Preferences };
const FRESH: Preferences = {
  ...golden.preferences_admin, org_name: "", allowed_emails: [], allowed_domains: [], admin_emails: [],
  admin_claim: undefined, admin_value: undefined, overridden: [], onboarded_at: null,
};
// The address the reader is signed in as, its domain, and a colleague's.
const MINE = FRESH.viewer!.email;
const MY_DOMAIN = MINE.slice(MINE.indexOf("@") + 1);
const THEIRS = "tomas@meridian.example";
const CONSEQUENCE = "Anyone at this domain who can sign in to your identity provider can sign in to Canyon.";
// The sentence the server refuses with, as cmd/canyon-server/routes_preferences.go writes it. The
// screen recognises none of them — it renders whatever body came back.
const NO_ADMINS = "an admin has to be able to sign in";

function server(answer: { status?: number; body: unknown }) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (`${init?.method} ${url}` !== "PUT /api/v1/preferences") return { ok: false, status: 404, json: async () => ({ error: "not_found" }) } as unknown as Response;
    const { status = 200, body } = answer;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([, init]) => JSON.parse(String(init?.body)) as Record<string, unknown>);
}

const show = (record: Preferences, done = () => undefined) => render(<Onboarding record={record} done={done} />);
const next = () => fireEvent.click(screen.getByRole("button", { name: "Continue" }));
const finish = () => fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
const box = () => screen.getByRole("checkbox", { name: `Allow everyone at ${MY_DOMAIN}` });

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("setting a host up", () => {
  // THE RED. Step 2 is the one input in the product where a wrong value lets the world in, and the
  // reader's own domain is never prefilled and never ticked: their address proves their account
  // works, it does not prove the domain is a company's. An admin at a consumer mail provider must
  // not admit that whole provider on one Continue click (design/ONBOARDING.md §3).
  it("the domain box starts unticked, and finishing allows no domain", async () => {
    const sent = server({ body: FRESH });
    show(FRESH);
    next();
    expect(box()).toHaveProperty("checked", false);
    expect(screen.getByText(CONSEQUENCE)).toBeTruthy(); // the consequence is written under it, not hidden
    next();
    next();
    finish();
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.allowed_domains).toEqual([]);
    // The other half of the same rule: ticking it is the whole of allowing the domain, so the box
    // is a decision somebody makes rather than one the screen made for them.
    cleanup();
    show(FRESH);
    next();
    fireEvent.click(box());
    next();
    next();
    finish();
    await waitFor(() => expect(sent()).toHaveLength(2));
    expect(sent()[1]!.allowed_domains).toEqual([MY_DOMAIN]);
  });

  // The one value that is always right: they are signed in, so that address demonstrably works.
  // Without it an admin can finish setup having locked themselves out.
  it("step 2 arrives with the reader's own address allowed, marked as theirs", () => {
    show(FRESH);
    next();
    expect(screen.getByRole("textbox", { name: "Address 1" })).toHaveProperty("value", MINE);
    expect(screen.getByText("you")).toBeTruthy();
  });

  // An empty membership is either an open door or a brick, so there is nowhere to go from it.
  it("Continue is disabled while both membership lists are empty", () => {
    show(FRESH);
    next();
    fireEvent.click(screen.getByRole("button", { name: "Remove address 1" }));
    expect(screen.getByRole("button", { name: "Continue" })).toHaveProperty("disabled", true);
    expect(screen.getByText("At least one person or one domain has to be allowed to sign in.")).toBeTruthy();
  });

  // One write at the end, carrying the instant that ends the setup state. Nothing is saved before
  // it: somebody who walks away half-way leaves the host exactly as they found it.
  it("Finish writes the row and the instant that ends the setup state", async () => {
    const sent = server({ body: { ...FRESH, onboarded_at: "2026-09-23T10:00:00Z" } });
    const done = vi.fn();
    show(FRESH, done);
    fireEvent.change(screen.getByRole("textbox", { name: "Organisation name" }), { target: { value: "Meridian Insurance" } });
    next();
    next();
    next();
    expect(screen.getByText("Meridian Insurance")).toBeTruthy(); // the review says what is about to be written
    finish();
    await waitFor(() => expect(done).toHaveBeenCalled());
    expect(sent()[0]!.org_name).toBe("Meridian Insurance");
    expect(sent()[0]!.allowed_emails).toEqual([MINE]);
    expect(sent()[0]!.admin_emails).toEqual([MINE]);
    expect(String(sent()[0]!.onboarded_at)).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d/);
  });

  // People pinned by the environment: the list says the server sets it, and nothing on the step asks
  // the reader to add an address they cannot add (the k3s walkthrough's run).
  it("people pinned on the server: no prompt to add an address", () => {
    const pinned: Preferences = { ...FRESH, allowed_emails: [THEIRS], overridden: ["allowed_emails"] };
    server({ body: pinned });
    show(pinned);
    next();
    expect(screen.getByText("Set on the server, by CANYON_OIDC_ALLOWED_EMAILS. Change it there, not here.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add person" })).toBeNull();
    expect(screen.queryByText(/Canyon does not send invitations/)).toBeNull();
  });

  // A key the environment pins is read-only and names its variable, and the write leaves it out:
  // naming it is 409 set_by_operator, and the value on screen is already the environment's.
  it("a key the environment pins is read-only and is not written", async () => {
    const pinned: Preferences = { ...FRESH, allowed_domains: [MY_DOMAIN], overridden: ["allowed_domains"] };
    const sent = server({ body: pinned });
    show(pinned);
    next();
    expect(screen.getByText("Set on the server, by CANYON_OIDC_ALLOWED_DOMAINS. Change it there, not here.")).toBeTruthy();
    expect(screen.queryByRole("checkbox")).toBeNull();
    next();
    next();
    finish();
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(Object.keys(sent()[0]!)).not.toContain("allowed_domains");
  });

  // Two lists that disagree about one person is a bug report waiting to happen, so an admin on
  // neither membership list joins the allowed addresses — and step 3 says so before Finish.
  it("an admin on neither membership list is added to the allowed addresses, and says so", async () => {
    const sent = server({ body: FRESH });
    show(FRESH);
    next();
    next();
    fireEvent.click(screen.getByRole("button", { name: "Add an admin" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Admin 2" }), { target: { value: THEIRS } });
    expect(screen.getByText(`${THEIRS} will also be added to the addresses allowed to sign in.`)).toBeTruthy();
    next();
    finish();
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.allowed_emails).toEqual([MINE, THEIRS]);
    expect(sent()[0]!.admin_emails).toEqual([MINE, THEIRS]);
  });

  // Every rule is the server's. The screen sends what was typed and renders the sentence that came
  // back, beside a Finish that can be pressed again once the reason is fixed.
  it("a refusal is the server's own sentence, on the review step", async () => {
    server({ status: 400, body: { error: "no_admins", message: NO_ADMINS } });
    show(FRESH);
    next();
    next();
    next();
    finish();
    expect(await screen.findByText(NO_ADMINS)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Finish setup" })).toBeTruthy();
  });
});
