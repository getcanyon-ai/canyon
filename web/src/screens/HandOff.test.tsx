import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FrontDoorRead, PlatformWebhook, Preferences, WebhookSaved } from "../api";
import { S } from "../strings";
import { HandOff, pinged } from "./HandOff";

// The contracts' own shapes: the front door's read, the webhook's read and first save, the preferences.
const fixture = (path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8"));
const door = fixture("front-door/meridian.json") as { get: FrontDoorRead; get_unprobed: FrontDoorRead };
const release = fixture("release/meridian.json") as { webhook_get: PlatformWebhook; webhook_saved: WebhookSaved };
const prefs = fixture("guardrails/preferences.json") as { preferences_admin: Preferences; preferences_member: Preferences };

const gateway: FrontDoorRead = { ...door.get_unprobed, probe_note: "" , front_door: { ...door.get_unprobed.front_door!, probed: true, enforcing: true } }; // a choice 1, probed
const proxy = door.get; // a choice 2
const none: FrontDoorRead = { ...door.get, front_door: null, warning: "" };
const needs = [{ error: "identity_image_required", message: "No sidecar image." }, { error: "https_required", message: "No https." }] as const;
const hook = { ...release.webhook_get, last_delivery: null };
const canyon = { ...prefs.preferences_admin, run_by: "canyon" as const };
const org = { ...prefs.preferences_admin, run_by: "org" as const };

// The server: each route answers what the test says, a function answering per call; every call is kept.
type Answer = { status?: number; body?: unknown };
type Routes = Record<string, Answer | (() => Answer | Promise<Answer>)>;
function server(routes: Routes) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const key = `${init?.method ?? "GET"} ${url}`;
    const route = routes[key] ?? (key.startsWith("GET /api/v1/platform-") ? { body: null } : { status: 404, body: { error: "not_found" } }); // none saved: 200 null (#2560)
    const { status = 200, body } = typeof route === "function" ? await route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return (method: string, url: string) => fetch.mock.calls.filter(([u, init]) => u === url && (init?.method ?? "GET") === method).map(([, init]) => (init?.body === undefined ? undefined : JSON.parse(String(init.body)) as unknown));
}

type Props = Partial<Parameters<typeof HandOff>[0]>;
function draw(props: Props = {}) {
  const adopt = vi.fn();
  const reload = vi.fn(async () => {});
  const toFrontDoor = vi.fn();
  const all = { read: gateway, record: canyon, admin: true, adopt, reload, toFrontDoor, ...props };
  const view = render(<HandOff {...all} />);
  return { adopt, reload, toFrontDoor, rerender: (more: Props) => view.rerender(<HandOff {...all} {...more} />) };
}
const pickPlatform = async () => fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
const step = (n: number) => screen.getAllByRole("listitem").filter((one) => one.parentElement?.tagName === "OL")[n - 1]!;

afterEach(() => { vi.unstubAllGlobals(); cleanup(); sessionStorage.clear(); });

describe("the checklist", () => {
  it("picking the platform saves nothing and opens the steps, the first undone one current", async () => {
    const sent = server({});
    draw({ read: { ...none, hand_off_needs: [...needs] } });
    await pickPlatform();
    expect(sent("PUT", "/api/v1/preferences")).toEqual([]);
    expect(step(1).getAttribute("aria-current")).toBe("step");
    expect(within(step(1)).getAllByRole("listitem").map((one) => one.textContent)).toEqual(needs.map((one) => one.message));
    expect(within(step(1)).getByText(S.reloadAfter)).toBeDefined();
    expect(within(step(4)).getByRole("button", { name: S.handOn }).hasAttribute("disabled")).toBe(true);
  });

  it("the server's settings clear once a read after the restart says nothing is missing", async () => {
    server({});
    const { rerender } = draw({ read: { ...gateway, hand_off_needs: [...needs] } });
    await pickPlatform();
    expect(within(step(1)).queryByText(S.handOffNeeds)).not.toBeNull();
    rerender({ read: gateway });
    expect(within(step(1)).queryByText(S.handOffNeeds)).toBeNull();
    expect(within(step(1)).getByRole("heading").textContent).toBe(`✓ ${S.stepServer}`);
  });

  it("the webhook step is a field until one is saved, then the saved address", async () => {
    server({ "GET /api/v1/platform-webhook": { body: null } });
    draw();
    await pickPlatform();
    expect(step(2).getAttribute("aria-current")).toBe("step");
    expect(within(step(2)).getByLabelText("Webhook URL")).toBeDefined();
    expect(within(step(2)).getByText(S.webhookNote)).toBeDefined();
  });

  for (const [what, read, line] of [["none set", none, S.stepDoorNone], ["choice 2", proxy, S.stepDoorProxy]] as const) {
    it(`the front door step with ${what} says why and takes the admin to the door, never by a link`, async () => {
      server({ "GET /api/v1/platform-webhook": { body: hook } });
      const { toFrontDoor } = draw({ read });
      await pickPlatform();
      expect(await screen.findByText(line)).toBeDefined();
      expect(step(3).getAttribute("aria-current")).toBe("step");
      expect(within(step(3)).queryByRole("link")).toBeNull();
      fireEvent.click(within(step(3)).getByRole("button", { name: S.toFrontDoor }));
      expect(toFrontDoor).toHaveBeenCalledOnce();
    });
  }

  it("a choice 1 door whose save did not find NetworkPolicy enforced leaves its step open, the note saying why", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook } });
    const { toFrontDoor } = draw({ read: door.get_unprobed }); // the golden's: not enforcing, so nothing would build
    await pickPlatform();
    expect((await screen.findByRole("note")).textContent).toBe(door.get_unprobed.probe_note);
    expect(within(step(3)).getByRole("heading").textContent).toBe(`3. ${S.gateway}`);
    expect(step(3).getAttribute("aria-current")).toBe("step");
    fireEvent.click(within(step(3)).getByRole("button", { name: S.toFrontDoor }));
    expect(toFrontDoor).toHaveBeenCalledOnce();
  });

  it("a choice 1 door ticks its step once its save found NetworkPolicy enforced, probed or not, and says when it is not checked on this cluster", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook } });
    const uncovered = "Not checked on this cluster: your platform can run apps behind this front door, but Canyon won't until a save passes the check. Cover the names.";
    const certificate: FrontDoorRead = { ...gateway, probe_note: uncovered, front_door: { ...gateway.front_door!, probed: false } }; // enforced, the certificate missing
    const { rerender } = draw({ read: certificate });
    await pickPlatform();
    expect((await screen.findByRole("note")).textContent).toBe(uncovered);
    expect(within(step(3)).getByRole("heading").textContent).toBe(`✓ ${S.gateway}`);
    rerender({ read: gateway }); // the front door saved below, its probe passed: the step reads it again
    expect(within(step(3)).queryByRole("note")).toBeNull();
  });

  // Where apps run is run_by's, never whether a webhook is saved (#2560): the admin's choice reads as the member's line does.
  it("a reload with the webhook saved and run_by canyon says Canyon runs apps; picking the platform opens the checklist, its last step open", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook } });
    draw();
    expect(await screen.findByText(hook.url)).toBeDefined();
    expect(screen.getByRole("radio", { name: S.runCanyon })).toHaveProperty("checked", true);
    expect(screen.getByRole("radio", { name: S.runOrg })).toHaveProperty("checked", false);
    expect(screen.queryByText(`✓ ${S.stepWebhook}`)).toBeNull();
    await pickPlatform();
    expect(screen.getByText(`✓ ${S.stepWebhook}`)).toBeDefined();
    expect(step(4).getAttribute("aria-current")).toBe("step");
    expect(within(step(4)).getByRole("button", { name: S.handOn }).hasAttribute("disabled")).toBe(false);
  });

  it("hands apps to the platform only after the question, and adopts the answer", async () => {
    const sent = server({ "GET /api/v1/platform-webhook": { body: hook }, "PUT /api/v1/preferences": { body: org } });
    const { adopt } = draw();
    await pickPlatform();
    fireEvent.click(await within(await screen.findByText(`✓ ${S.stepWebhook}`).then(() => step(4))).findByRole("button", { name: S.handOn }));
    expect(screen.getByText(S.onAsk)).toBeDefined();
    expect(sent("PUT", "/api/v1/preferences")).toEqual([]);
    fireEvent.click(within(screen.getByText(S.onAsk).parentElement!).getByRole("button", { name: S.handOn }));
    await waitFor(() => expect(adopt).toHaveBeenCalledWith(org));
    expect(sent("PUT", "/api/v1/preferences")).toEqual([{ run_by: "org" }]);
  });

  // Each of the PUT's refusals (contracts/routes.md, the preferences row) is the server's own sentence, under the button,
  // and every step is read again: the server's state moved under the checklist.
  for (const error of ["identity_image_required", "https_required", "webhook_required", "front_door_required", "signin_unsupported"]) {
    it(`a ${error} refusal is shown in place and every step is read again`, async () => {
      const message = `the server's own words for ${error}`;
      const sent = server({ "GET /api/v1/platform-webhook": { body: hook }, "PUT /api/v1/preferences": { status: 409, body: { error, message } } });
      const { reload, adopt } = draw();
      await pickPlatform();
      fireEvent.click(await within(await screen.findByText(`✓ ${S.stepWebhook}`).then(() => step(4))).findByRole("button", { name: S.handOn }));
      fireEvent.click(within(screen.getByText(S.onAsk).parentElement!).getByRole("button", { name: S.handOn }));
      expect((await within(step(4)).findByRole("alert")).textContent).toBe(message);
      expect(reload).toHaveBeenCalledOnce();
      await waitFor(() => expect(sent("GET", "/api/v1/platform-webhook")).toHaveLength(2));
      expect(adopt).not.toHaveBeenCalled();
    });
  }
});

describe("once the platform runs apps", () => {
  it("sums it up: where releases go, none sent yet, the grace and the platform team's card", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook } });
    draw({ record: org });
    expect(await screen.findByText(S.onSummary.replace("<url>", hook.url))).toBeDefined();
    expect(screen.getByText(S.lastNoneOn)).toBeDefined();
    expect(screen.getByLabelText(S.graceLabel)).toHaveProperty("value", "1h");
    const card = screen.getByRole("region", { name: S.teamTitle });
    expect(within(card).getByText(S.teamWebhook.replace("<url>", hook.url))).toBeDefined();
    expect(within(card).getByText(S.teamDocs)).toBeDefined();
    expect(screen.queryByRole("list")).toBeNull();
  });

  it("the last delivery: its status and when, or that nothing answered", async () => {
    server({ "GET /api/v1/platform-webhook": { body: release.webhook_get } });
    draw({ record: org });
    expect(await screen.findByText((text) => text.startsWith(S.lastDelivery.replace("<status>", "200") + " · "))).toBeDefined();
    cleanup();
    server({ "GET /api/v1/platform-webhook": { body: { ...release.webhook_get, last_delivery: { ...release.webhook_get.last_delivery!, status: 0 } } } });
    draw({ record: org });
    expect(await screen.findByText((text) => text.startsWith(S.lastNoAnswer + " · "))).toBeDefined();
  });

  it("switching back asks first, and the server's refusal is shown in place", async () => {
    const unchecked = "How people reach apps hasn't passed Canyon's check of this cluster, so Canyon can't run apps here. Save it again so Canyon checks it, then switch.";
    const sent = server({ "GET /api/v1/platform-webhook": { body: hook }, "PUT /api/v1/preferences": { status: 409, body: { error: "front_door_unchecked", message: unchecked } } });
    const { reload, adopt } = draw({ record: org });
    fireEvent.click(await screen.findByRole("radio", { name: S.runCanyon }));
    expect(screen.getByText(S.offAsk)).toBeDefined();
    expect(sent("PUT", "/api/v1/preferences")).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Run apps on this cluster" }));
    expect((await screen.findByRole("alert")).textContent).toBe(unchecked);
    expect(sent("PUT", "/api/v1/preferences")).toEqual([{ run_by: "canyon" }]);
    expect(reload).toHaveBeenCalledOnce(); // the server's state moved under the page: every step is read again
    await waitFor(() => expect(sent("GET", "/api/v1/platform-webhook")).toHaveLength(2));
    expect(adopt).not.toHaveBeenCalled();
  });

  it("before any release, the address saved and Canyon still running apps, says to send a test delivery", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook } });
    draw();
    expect(await screen.findByText(S.lastNone)).toBeDefined();
    expect(screen.queryByText(S.lastNoneOn)).toBeNull();
  });

  it("an access grace the server refuses is shown in place, and the field reads the server's again", async () => {
    const message = "access_grace: a duration from 0 to 24h";
    server({ "GET /api/v1/platform-webhook": { body: hook }, "PUT /api/v1/preferences": { status: 400, body: { error: "invalid_access_grace", message } } });
    const { reload, rerender } = draw({ record: org });
    fireEvent.change(await screen.findByLabelText(S.graceLabel), { target: { value: "25h" } });
    fireEvent.click(screen.getByRole("button", { name: S.graceSave }));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(reload).toHaveBeenCalledOnce();
    rerender({ record: { ...org, access_grace: "30m" } });
    expect(screen.getByLabelText(S.graceLabel)).toHaveProperty("value", "30m");
  });
});

describe("the test delivery", () => {
  it("says what each status means", () => {
    expect(pinged(202)).toBe(`${S.pingAnswered.replace("<status>", "202")} ${S.pingReachable}`);
    expect(pinged(401)).toBe(`${S.pingAnswered.replace("<status>", "401")} ${S.pingSecret}`);
    expect(pinged(403)).toBe(`${S.pingAnswered.replace("<status>", "403")} ${S.pingSecret}`);
    expect(pinged(301)).toBe(`${S.pingAnswered.replace("<status>", "301")} ${S.pingRedirect}`);
    expect(pinged(404)).toBe(S.pingAnswered.replace("<status>", "404"));
    expect(pinged(503)).toBe(S.pingAnswered.replace("<status>", "503"));
    expect(pinged(0)).toBe(S.pingNone);
  });

  it("sends once while it waits, then says what the platform answered", async () => {
    let answer: (value: Answer) => void = () => {};
    const sent = server({ "GET /api/v1/platform-webhook": { body: hook }, "POST /api/v1/platform-webhook/ping": () => new Promise<Answer>((resolve) => { answer = resolve; }) });
    draw({ record: org });
    fireEvent.click(await screen.findByRole("button", { name: "Send a test delivery" }));
    expect(screen.getByRole("button", { name: "Sending…" })).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Sending…" }));
    expect(sent("POST", "/api/v1/platform-webhook/ping")).toHaveLength(1);
    await act(async () => answer({ body: { delivery: "d", sent_at: "2026-10-23T10:00:00Z", status: 202 } }));
    expect(screen.getByRole("status").textContent).toBe(pinged(202));
  });

  it("a ping nothing answered says so, and one Canyon could not send says the server's words", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook }, "POST /api/v1/platform-webhook/ping": { body: { delivery: "d", sent_at: "2026-10-23T10:00:00Z", status: 0 } } });
    draw({ record: org });
    fireEvent.click(await screen.findByRole("button", { name: "Send a test delivery" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe(S.pingNone));
    cleanup();
    server({ "GET /api/v1/platform-webhook": { body: hook }, "POST /api/v1/platform-webhook/ping": { status: 500, body: { error: "internal", message: "internal error" } } });
    draw({ record: org });
    fireEvent.click(await screen.findByRole("button", { name: "Send a test delivery" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("internal error"));
    expect(screen.getByRole("button", { name: "Send a test delivery" })).toBeDefined(); // ready to send again
  });

  it("a ping already in flight elsewhere is the server's own sentence", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook }, "POST /api/v1/platform-webhook/ping": { status: 429, body: { error: "ping_in_flight", message: "A test delivery is being sent already. Try again once it answers." } } });
    draw({ record: org });
    fireEvent.click(await screen.findByRole("button", { name: "Send a test delivery" }));
    await waitFor(() => expect(screen.getByRole("status").textContent).toBe("A test delivery is being sent already. Try again once it answers."));
  });
});

describe("the values Canyon shows once", () => {
  const saveFirst = async () => {
    const sent = server({ "GET /api/v1/platform-webhook": () => ({ body: null }), "PUT /api/v1/platform-webhook": { body: release.webhook_saved } });
    draw();
    await pickPlatform();
    fireEvent.change(screen.getByLabelText("Webhook URL"), { target: { value: "https://deploy.meridian.example/hooks/canyon" } });
    fireEvent.click(screen.getByRole("button", { name: "Save the webhook" }));
    return { sent, dialog: await screen.findByRole("dialog", { name: S.revealTitle }) };
  };

  it("the first save shows the secret and the host token in a dialog, Done off until both are stored", async () => {
    const { sent, dialog } = await saveFirst();
    expect(sent("PUT", "/api/v1/platform-webhook")).toEqual([{ url: "https://deploy.meridian.example/hooks/canyon" }]);
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(within(dialog).getByLabelText("Webhook secret")).toHaveProperty("value", release.webhook_saved.secret);
    expect(within(dialog).getByLabelText("Host token")).toHaveProperty("value", release.webhook_saved.token);
    expect(within(dialog).getByLabelText("Webhook secret")).toHaveProperty("readOnly", true);
    expect(within(dialog).getByLabelText("Webhook secret").getAttribute("name")).toBeNull();
    expect(within(dialog).getByLabelText("Host token").getAttribute("name")).toBeNull();
    expect(within(dialog).getByLabelText("Host token")).toHaveProperty("readOnly", true);
    expect(dialog.closest("form")).toBeNull();
    expect(within(dialog).getByText(S.secretNote)).toBeDefined();
    expect(within(dialog).getByText(S.tokenNote)).toBeDefined();
    expect(within(dialog).getByText(S.revealLost)).toBeDefined();
    expect(within(dialog).getByRole("button", { name: "Done" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(within(dialog).getByRole("checkbox", { name: S.revealAck }));
    expect(within(dialog).getByRole("button", { name: "Done" }).hasAttribute("disabled")).toBe(false);
  });

  it("asks before the page is left, keeps Escape from closing it, and forgets the values once Done", async () => {
    const kept = vi.fn(); // Node's own localStorage global shadows jsdom's (Fleet.test.tsx): a stub records any write
    vi.stubGlobal("localStorage", { setItem: kept, getItem: () => null, removeItem: () => {}, clear: () => {}, key: () => null, length: 0 });
    const added = vi.spyOn(window, "addEventListener");
    const removed = vi.spyOn(window, "removeEventListener");
    const { dialog } = await saveFirst();
    // The listener the dialog arms, called as the browser would: the page asks before it is left.
    const ask = await waitFor(() => {
      const listener = added.mock.calls.find(([type]) => type === "beforeunload")?.[1];
      expect(listener).toBeTypeOf("function");
      return listener as (event: Event) => void;
    });
    const leave = { preventDefault: vi.fn(), returnValue: undefined as unknown };
    ask(leave as unknown as Event);
    expect(leave.preventDefault).toHaveBeenCalledOnce();
    fireEvent.keyDown(within(dialog).getByLabelText("Webhook secret"), { key: "Escape" });
    expect(screen.getByRole("dialog")).toBeDefined();
    expect(within(dialog).getByText(S.revealLeave)).toBeDefined();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: S.revealAck }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.body.innerHTML).not.toContain(release.webhook_saved.secret!);
    expect(document.body.innerHTML).not.toContain(release.webhook_saved.token!);
    expect([kept.mock.calls, sessionStorage.length]).toEqual([[], 0]);
    expect(removed).toHaveBeenCalledWith("beforeunload", ask); // once Done, leaving asks nothing
    added.mockRestore();
    removed.mockRestore();
  });

  it("a webhook save the server refuses shows its sentence at the field, and nothing is revealed", async () => {
    server({ "GET /api/v1/platform-webhook": { body: null }, "PUT /api/v1/platform-webhook": { status: 400, body: { error: "invalid_url", message: "url: required, https://" } } });
    draw();
    await pickPlatform();
    fireEvent.change(screen.getByLabelText("Webhook URL"), { target: { value: "http://deploy.meridian.example" } });
    fireEvent.click(screen.getByRole("button", { name: "Save the webhook" }));
    expect((await within(step(2)).findByRole("alert")).textContent).toBe("url: required, https://");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByLabelText("Webhook URL")).toHaveProperty("value", "http://deploy.meridian.example");
  });

  it("once Done, the keyboard is back on the section's heading", async () => {
    const { dialog } = await saveFirst();
    fireEvent.click(within(dialog).getByRole("checkbox", { name: S.revealAck }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("heading", { name: S.runWhere })));
  });

  it("once a replaced secret is stored, the keyboard is on Send a test delivery: the platform refuses releases until it has it", async () => {
    server({ "GET /api/v1/platform-webhook": { body: hook }, "POST /api/v1/platform-webhook/secret": { body: { secret: "sec-new-not-real" } } });
    draw({ record: org });
    fireEvent.click(await screen.findByRole("button", { name: "Replace the webhook secret" }));
    fireEvent.click(within(screen.getByText(S.webhookSecretAsk).parentElement!).getByRole("button", { name: "Replace the webhook secret" }));
    const dialog = await screen.findByRole("dialog", { name: S.revealTitle });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: S.revealAckOne }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Send a test delivery" })));
  });

  it("keeps the keyboard inside it", async () => {
    const { dialog } = await saveFirst();
    const secret = within(dialog).getByLabelText("Webhook secret");
    await waitFor(() => expect(document.activeElement).toBe(secret)); // the dialog focuses its first field in an effect, which may run after it shows
    const box = within(dialog).getByRole("checkbox", { name: S.revealAck }); // the last stop while Done is off
    fireEvent.keyDown(secret, { key: "Tab", shiftKey: true });
    expect(document.activeElement).toBe(box);
    fireEvent.keyDown(box, { key: "Tab" });
    expect(document.activeElement).toBe(secret);
  });

  it("a copy the browser refuses says so in place", async () => {
    vi.stubGlobal("navigator", { clipboard: { writeText: () => Promise.reject(new Error("no")) } });
    const { dialog } = await saveFirst();
    fireEvent.click(within(dialog).getByRole("button", { name: "Copy the webhook secret" }));
    expect((await within(dialog).findByRole("alert")).textContent).toBe("Couldn't copy. The browser did not allow it.");
  });

  for (const [what, button, question, route, body, label] of [
    ["host token", "Replace the host token", S.tokenAsk, "POST /api/v1/platform-webhook/token", { token: "tok-new-not-real" }, "Host token"],
    ["webhook secret", "Replace the webhook secret", S.webhookSecretAsk, "POST /api/v1/platform-webhook/secret", { secret: "sec-new-not-real" }, "Webhook secret"],
  ] as const) {
    it(`replacing the ${what} asks first, Cancel sends nothing, and the new one shows only once the server answered`, async () => {
      let answer: (value: Answer) => void = () => {};
      const [method, url] = route.split(" ") as [string, string];
      const sent = server({ "GET /api/v1/platform-webhook": { body: hook }, [route]: () => new Promise<Answer>((resolve) => { answer = resolve; }) });
      draw({ record: org });
      fireEvent.click(await screen.findByRole("button", { name: button }));
      expect(screen.getByText(question)).toBeDefined();
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      expect(sent(method, url)).toEqual([]);
      fireEvent.click(screen.getByRole("button", { name: button }));
      fireEvent.click(within(screen.getByText(question).parentElement!).getByRole("button", { name: button }));
      expect(screen.queryByRole("dialog")).toBeNull();
      await act(async () => answer({ body }));
      const dialog = screen.getByRole("dialog", { name: S.revealTitle });
      expect(dialog.parentElement?.className).toContain("bg-scrim"); // the design system's scrim: bg-black/40 drew nothing (#118)
      expect(within(dialog).getByLabelText(label)).toHaveProperty("value", Object.values(body)[0]);
      expect(within(dialog).getAllByRole("textbox")).toHaveLength(1);
      expect(within(dialog).getByText(S.revealAskOne)).toBeDefined();
    });

    it(`a refused ${what} replace shows the server's sentence and reveals nothing`, async () => {
      server({ "GET /api/v1/platform-webhook": { body: hook }, [route]: { status: 409, body: { error: "conflict", message: "the server's own words" } } });
      draw({ record: org });
      fireEvent.click(await screen.findByRole("button", { name: button }));
      fireEvent.click(within(screen.getByText(question).parentElement!).getByRole("button", { name: button }));
      expect((await screen.findByRole("alert")).textContent).toBe("the server's own words");
      expect(screen.queryByRole("dialog")).toBeNull();
    });
  }
});

// Who may see and do what (design/PERMISSIONS.md: the org's settings are every member's to read, an admin's to change).
describe("a member", () => {
  for (const [who, record] of [["a member on canyon", prefs.preferences_member], ["a member under org", { ...prefs.preferences_member, run_by: "org" as const }]] as const) {
    it(`${who} reads where apps run, the webhook and the card, and is offered nothing`, async () => {
      const sent = server({ "GET /api/v1/platform-webhook": { body: release.webhook_get } });
      draw({ admin: false, record, read: { ...none, hand_off_needs: [...needs] } });
      expect(await screen.findByText(S.runAdmin)).toBeDefined();
      expect(screen.getByText(record.run_by === "org" ? S.runOrg : S.runCanyon)).toBeDefined();
      expect(await screen.findByText(release.webhook_get.url)).toBeDefined();
      expect(screen.getByRole("region", { name: S.teamTitle })).toBeDefined();
      expect(screen.queryByText(S.handOffNeeds)).toBeNull();
      for (const role of ["button", "radio", "textbox", "checkbox"]) expect(screen.queryAllByRole(role)).toEqual([]);
      expect(sent("PUT", "/api/v1/preferences")).toEqual([]);
    });
  }
});
