import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AUTH } from "../strings";
import { Shell } from "./Shell";

const CODE = "DKTG-4B9V";
const GRANT = `/api/v1/device-grants/${CODE}`;
// The screen says "today" by the clock, so each test sets it: at midday a code asked a minute before is today's,
// where the real clock's first minute after midnight made it yesterday's (#2709).
const NOON = new Date(2026, 10, 8, 12, 0);
const at = new Date(NOON.getTime() - 60_000);
const pending = { requested_by: "10.4.2.19", requested_at: at.toISOString(), expires_at: new Date(at.getTime() + 600_000).toISOString(), approver: { display: "Carol Diaz", email: "carol@meridian.example" } };
// The 404's sentence as the server writes it (cmd/canyon-server/routes_device.go): the screen never shows it.
const SERVER_404 = { error: "not_found", message: "that code is not valid or has already been used. Check the code in your terminal" };
const answer = (status: number, body: unknown) => ({ ok: status < 300, status, json: async () => body }) as unknown as Response;

// The lookup answers from `grant`, the two writes 204, and everything else as the routes say.
function server(grant: () => Response) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
    if (url === "/api/v1/preferences") return answer(200, { onboarded_at: "2026-09-16T09:00:00Z" }); // the shell's setup gate reads this one field: a host somebody has set up
    if (init?.method === "POST" && url.startsWith(GRANT)) return answer(204, null);
    if (init?.method === "DELETE" && url === "/api/v1/sessions") return answer(204, null);
    if (init?.method === "GET" && url === GRANT) return grant();
    return answer(404, SERVER_404);
  });
  vi.stubGlobal("fetch", fetch);
  // The shell reads /auth/config and the preferences row before it draws anything, to decide
  // whether this host is still being set up. Neither read is this screen's, so neither is counted:
  // every line below is what looking up and approving a device did.
  return () => fetch.mock.calls.map(([url, init]) => `${init?.method ?? "GET"} ${url}`).filter((line) => !line.endsWith("/auth/config") && !line.endsWith("/api/v1/preferences"));
}

const open = (grant = () => answer(200, pending)) => {
  window.location.hash = `#/device?code=${CODE}`;
  const sent = server(grant);
  render(<Shell />);
  return sent;
};
// Once looked up, the field folds into the request: its code, the address and the time, to check against the terminal.
const requestedLine = async () => {
  const request = await screen.findByRole("region", { name: "The request" });
  expect(within(request).getByText(CODE)).toBeDefined();
  expect(within(request).getByText(pending.requested_by)).toBeDefined();
  expect(within(request).getByText(`${at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} today`)).toBeDefined();
  return request;
};

beforeEach(() => { vi.setSystemTime(NOON); });
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  window.location.hash = "";
  sessionStorage.clear();
});

describe("the device approval screen", () => {
  it("fills the code from ?code= and looks it up, and nothing is approved until the click", async () => {
    const sent = open();
    expect(((await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement).value).toBe(CODE);
    await requestedLine();
    expect(sent()).toEqual([`GET ${GRANT}`]);
    expect(screen.getByText(AUTH.deviceOnlyYours)).toBeDefined();
    expect(sent().filter((line) => line.startsWith("POST"))).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Change code" })); // the field again, with the code in it
    const input = (await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement;
    expect(input.value).toBe(CODE);
    fireEvent.keyDown(input, { key: "Enter" }); // Enter and the form's submit look up, never approve
    fireEvent.submit(input.closest("form") as HTMLFormElement);
    await waitFor(() => expect(sent()).toEqual([`GET ${GRANT}`, `GET ${GRANT}`]));
    expect(sent().filter((line) => line.startsWith("POST"))).toEqual([]);
  });

  it("names the day a code asked for before midnight was asked on, not today", async () => {
    vi.setSystemTime(new Date(2026, 10, 8, 0, 0, 30));
    const asked = new Date(2026, 10, 7, 23, 59, 30);
    open(() => answer(200, { ...pending, requested_at: asked.toISOString(), expires_at: new Date(asked.getTime() + 600_000).toISOString() }));
    const request = await screen.findByRole("region", { name: "The request" });
    expect(within(request).getByText(`${asked.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })} on ${asked.toLocaleDateString()}`)).toBeDefined();
  });

  it("asks for the code in the terminal's words, and shows its shape", async () => {
    window.location.hash = "#/device";
    server(() => answer(200, pending));
    render(<Shell />);
    expect(await screen.findByRole("heading", { name: "Sign in to the Canyon CLI" })).toBeDefined();
    expect(screen.getByText("Canyon CLI · Step 2 of 2")).toBeDefined();
    expect(screen.getByText("Enter the code shown in your terminal.")).toBeDefined();
    expect(screen.getByLabelText(AUTH.deviceCode).getAttribute("placeholder")).toBe("K7QP-4RXM");
  });

  // What approving hands over was grey text under the buttons, read after the click if at all.
  it("shows the requesting address and time, and what approving hands over, above the button", async () => {
    open();
    const line = await requestedLine();
    expect(screen.getByText("Check this matches your terminal.")).toBeDefined();
    const button = screen.getByRole("button", { name: AUTH.deviceApprove });
    expect(line.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy(); // the defence sits above the click, not in a footnote
    expect(screen.getByText("Only approve a code you generated yourself.").compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.getByText("Approving gives that machine your access for 7 days. Anything running there acts as you, including deploys.").compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  // A provider that signs a person in silently can sign them in as an account they did not mean:
  // the page names who is approving, and switching signs out and comes back to this same code.
  it("names the account that is approving, and switching account comes back to this code", async () => {
    const sent = open();
    const line = await screen.findByRole("region", { name: "Approving as" });
    expect(line.textContent).toContain("Approving as Carol Diaz");
    expect(within(line).getByText("carol@meridian.example")).toBeDefined();
    expect(within(line).queryByText("Admin")).toBeNull();
    expect(line.compareDocumentPosition(screen.getByRole("button", { name: AUTH.deviceApprove })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: AUTH.deviceSwitch }));
    await waitFor(() => expect(sent()).toContain("DELETE /api/v1/sessions"));
    const signIn = await screen.findByRole("link", { name: "Continue to company sign-in" });
    expect(signIn.getAttribute("href")).toBe("/auth/start?prompt=select_account"); // the provider asks which account
    expect(screen.getByRole("heading", { name: "Sign in to approve the Canyon CLI" })).toBeDefined();
    fireEvent.click(signIn);
    expect(sessionStorage.getItem("canyon_return")).toBe(`#/device?code=${CODE}`);
    expect(sent().filter((line) => line.startsWith("POST"))).toEqual([]);
  });

  // An admin approved two codes without noticing whose access they were handing over: the account
  // is its own bordered strip directly above the buttons, the name in bold, and an admin says so.
  it("names the approving account in a strip of its own above the buttons, in bold, and says when it is an admin", async () => {
    window.location.hash = `#/device?code=${CODE}`;
    server(() => answer(200, pending));
    const fetch = vi.mocked(globalThis.fetch);
    const base = fetch.getMockImplementation()!;
    fetch.mockImplementation(async (url, init) => url === "/api/v1/preferences"
      ? answer(200, { onboarded_at: "2026-09-16T09:00:00Z", viewer: { email: "carol@meridian.example", role: "admin" } })
      : base(url, init));
    render(<Shell />);
    const strip = await screen.findByRole("region", { name: "Approving as" });
    expect(within(strip).getByText("Carol Diaz").tagName).toBe("STRONG");
    expect(within(strip).getByRole("button", { name: AUTH.deviceSwitch })).toBeDefined();
    expect(await within(strip).findByText("Admin")).toBeDefined();
    expect(within(strip).getByText("carol@meridian.example")).toBeDefined();
    expect(strip.nextElementSibling?.contains(screen.getByRole("button", { name: AUTH.deviceApprove }))).toBe(true);
  });

  it("says so when switching account fails, and takes Approve away from the account it meant to leave", async () => {
    open();
    const switching = await screen.findByRole("button", { name: AUTH.deviceSwitch });
    vi.mocked(globalThis.fetch).mockImplementationOnce(async () => answer(500, { error: "internal", message: "internal error" }));
    fireEvent.click(switching);
    expect(await screen.findByText("internal error")).toBeDefined();
    expect(screen.queryByRole("button", { name: AUTH.deviceApprove })).toBeNull();
  });

  it("looks up a hand-typed code on Enter, and changing the code takes the address and the button away", async () => {
    window.location.hash = "#/device";
    const sent = server(() => answer(200, pending));
    render(<Shell />);
    const input = (await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement;
    fireEvent.change(input, { target: { value: CODE } });
    fireEvent.submit(input.closest("form") as HTMLFormElement);
    await requestedLine();
    expect(sent()).toEqual([`GET ${GRANT}`]);
    fireEvent.click(screen.getByRole("button", { name: "Change code" }));
    expect(screen.queryByText(pending.requested_by)).toBeNull();
    expect(screen.queryByRole("button", { name: AUTH.deviceApprove })).toBeNull();
    expect(((await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement).value).toBe(CODE);
  });

  it("does not say approved until the POST has resolved", async () => {
    let finish: () => void = () => {};
    const post = new Promise<Response>((resolve) => { finish = () => resolve(answer(204, null)); });
    const fetch = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === "/auth/config") return answer(200, { canyon: true, auth: "oidc", org: "Meridian Insurance", device: true });
      if (init?.method === "GET" && url === GRANT) return answer(200, pending);
      if (init?.method === "POST" && url === `${GRANT}/approve`) return post;
      return answer(404, SERVER_404);
    });
    vi.stubGlobal("fetch", fetch);
    window.location.hash = `#/device?code=${CODE}`;
    render(<Shell />);
    fireEvent.click(await screen.findByRole("button", { name: AUTH.deviceApprove }));
    expect(screen.queryByText("The CLI is signed in")).toBeNull(); // the click is not the completion; the server's answer is
    finish();
    expect(await screen.findByRole("heading", { name: "The CLI is signed in" })).toBeDefined();
  });

  it("does not put a slow read's address under a code typed over it", async () => {
    let release: (r: Response) => void = () => {};
    const slow = new Promise<Response>((resolve) => { release = resolve; });
    const sent = open(() => slow as unknown as Response);
    const input = (await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement;
    fireEvent.change(input, { target: { value: "XXXX-YYYY" } });
    release(answer(200, pending));
    await waitFor(() => expect(sent()).toEqual([`GET ${GRANT}`]));
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText(pending.requested_by)).toBeNull();
    expect(screen.queryByRole("button", { name: AUTH.deviceApprove })).toBeNull();
  });

  it("approves through POST …/approve on the click and says so", async () => {
    const sent = open();
    fireEvent.click(await screen.findByRole("button", { name: AUTH.deviceApprove }));
    expect(await screen.findByRole("heading", { name: "The CLI is signed in" })).toBeDefined();
    expect(screen.getByText("That machine now has your access for 7 days. You can close this tab.")).toBeDefined();
    expect(sent()).toEqual([`GET ${GRANT}`, `POST ${GRANT}/approve`]);
    expect(screen.queryByRole("button", { name: AUTH.deviceApprove })).toBeNull();
  });

  it("denies through POST …/deny", async () => {
    const sent = open();
    fireEvent.click(await screen.findByRole("button", { name: AUTH.deviceDeny }));
    expect(await screen.findByRole("heading", { name: "Request denied" })).toBeDefined();
    expect(screen.getByText("Nothing was handed over. If you didn't start this, tell your Canyon admin.")).toBeDefined();
    expect(sent()).toEqual([`GET ${GRANT}`, `POST ${GRANT}/deny`]);
  });

  // A person who entered a code again after approving it read "That code has expired" and took the
  // sign-in to have failed (#2647): the server's 404 means not valid or already used, never expired.
  it("says its own line for a code the server cannot find, never that it expired, keeps the code, and posts nothing", async () => {
    const sent = open(() => answer(404, SERVER_404));
    expect(await screen.findByText("That code isn't valid or was already used")).toBeDefined();
    expect(screen.getByText("If your terminal says you're signed in, you're done. Otherwise run canyon login again.")).toBeDefined();
    expect(screen.queryByText(/expired/i)).toBeNull();
    expect(screen.queryByText(SERVER_404.message)).toBeNull();
    const field = (await screen.findByLabelText(AUTH.deviceCode)) as HTMLInputElement;
    expect(field.value).toBe(CODE);
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(screen.queryByRole("button", { name: AUTH.deviceApprove })).toBeNull();
    expect(sent()).toEqual([`GET ${GRANT}`]);
  });

  it("says the same when the code was used between the lookup and the click", async () => {
    const sent = open();
    const fetch = vi.mocked(globalThis.fetch);
    const base = fetch.getMockImplementation()!;
    fetch.mockImplementation(async (url, init) => init?.method === "POST" ? answer(404, SERVER_404) : base(url, init)); // another tab approved it first
    fireEvent.click(await screen.findByRole("button", { name: AUTH.deviceApprove }));
    expect(await screen.findByText("That code isn't valid or was already used")).toBeDefined();
    expect(screen.queryByText(/expired/i)).toBeNull();
    expect(screen.queryByRole("heading", { name: "The CLI is signed in" })).toBeNull();
    expect(sent()).toEqual([`GET ${GRANT}`, `POST ${GRANT}/approve`]);
  });

  // The setup gate's read was answered, so this 401 is a session that ended (design/specs/screens.md, A0).
  it("leaves the shell on the sign-in page when the lookup answers 401", async () => {
    const sent = open(() => answer(401, { error: "unauthenticated", message: "sign in to Canyon" }));
    expect(await screen.findByText("Your session ended")).toBeDefined();
    expect(await screen.findByText("Meridian Insurance")).toBeDefined(); // the sign-in page's own /auth/config read, which lands after the box
    expect(screen.getByRole("link", { name: "Continue to company sign-in" }).getAttribute("href")).toBe("/auth/start");
    expect(screen.queryByLabelText(AUTH.deviceCode)).toBeNull();
    expect(sent().filter((line) => line.startsWith("POST"))).toEqual([]);
  });
});
