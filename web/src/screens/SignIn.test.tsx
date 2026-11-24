import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SignIn, takeReturn } from "./SignIn";

const config = () => {
  const fetch = vi.fn(async () => ({ ok: true, status: 200, json: async () => ({ canyon: true, auth: "oidc", org: "Meridian Insurance", device: true }) }) as unknown as Response);
  vi.stubGlobal("fetch", fetch);
};

afterEach(() => {
  vi.unstubAllGlobals();
  window.location.hash = "";
  sessionStorage.clear();
});

// The failure line exists because the callback redirects a refused sign-in back to this screen.
// It says no more than that it did not work: the reason is in the server's log, and echoing an
// identity provider's text onto an unauthenticated page is how a phishing page gets its copy
// written for it (design/specs/screens.md).
describe("the sign-in screen", () => {
  // One card, the org on top with the address in the browser: the one fact that tells a person a
  // sign-in link is genuine is read from the browser, never from the server (design/specs/screens.md, A0).
  it("leads with the org and the browser's own address, and one button that says where it goes", async () => {
    config();
    render(<SignIn />);
    expect(await screen.findByText("Meridian Insurance")).toBeDefined();
    expect(screen.getByText(window.location.host)).toBeDefined();
    expect(screen.getByRole("heading", { name: "Sign in to Canyon" })).toBeDefined();
    expect(screen.getByText("Use your Meridian Insurance company account. You'll come straight back here.")).toBeDefined();
    expect(screen.getByRole("link", { name: "Continue to company sign-in" }).getAttribute("href")).toBe("/auth/start");
    expect(screen.getByText("Canyon never sees your password.")).toBeDefined();
    expect(screen.queryByText("Sign-in didn't finish")).toBeNull();
  });

  // A slow provider must not look like a dead click.
  it("says it is opening the company sign-in once clicked", async () => {
    config();
    render(<SignIn />);
    fireEvent.click(await screen.findByRole("link", { name: "Continue to company sign-in" }));
    expect(screen.getByRole("link", { name: "Opening company sign-in…" }).getAttribute("href")).toBe("/auth/start");
  });

  // The failure box exists because the callback redirects a refused sign-in back to this screen.
  // It says no more than that it did not work: the reason is in the server's log, and echoing an
  // identity provider's text onto an unauthenticated page is how a phishing page gets its copy
  // written for it (design/specs/screens.md).
  it("a refused sign-in has its own title above the button, which reads Try again", async () => {
    config();
    render(<SignIn reason="failed" />);
    const title = await screen.findByText("Sign-in didn't finish");
    expect(screen.getByText("Try again. If it keeps failing, ask your Canyon admin.")).toBeDefined();
    const button = screen.getByRole("link", { name: "Try again" });
    expect(button.getAttribute("href")).toBe("/auth/start");
    expect(title.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(screen.queryByText("You're signed out of Canyon")).toBeNull();
  });

  // Sign-out ends Canyon's session, never the company's: said after a sign-out, and only then.
  it("says the company account is still signed in after a sign-out, and only then", async () => {
    config();
    const { unmount } = render(<SignIn />);
    await screen.findByText("Meridian Insurance");
    expect(screen.queryByText("You're signed out of Canyon")).toBeNull();
    unmount();
    render(<SignIn reason="signed-out" />);
    expect(await screen.findByText("You're signed out of Canyon")).toBeDefined();
    expect(screen.getByText("Your company account is still signed in. Sign out of it too if this computer is shared.")).toBeDefined();
  });

  it("asks the provider which account after a sign-out, and only then", async () => {
    config();
    const { unmount } = render(<SignIn />);
    expect((await screen.findByRole("link", { name: "Continue to company sign-in" })).getAttribute("href")).toBe("/auth/start");
    unmount();
    render(<SignIn reason="signed-out" />);
    expect((await screen.findByRole("link", { name: "Continue to company sign-in" })).getAttribute("href")).toBe("/auth/start?prompt=select_account");
  });

  it("says the session ended when it did", async () => {
    config();
    render(<SignIn reason="ended" />);
    expect(await screen.findByText("Your session ended")).toBeDefined();
    expect(screen.getByText("Sign in again to keep going.")).toBeDefined();
    expect(screen.getByRole("link", { name: "Continue to company sign-in" }).getAttribute("href")).toBe("/auth/start");
  });

  it("names no reason, no error code and no provider", async () => {
    config();
    render(<SignIn reason="failed" />);
    const text = (await screen.findByText("Sign-in didn't finish")).ownerDocument.body.textContent ?? "";
    for (const leak of ["auth_failed", "400", "nonce", "state", "id token", "email_verified"]) {
      expect(text.toLowerCase()).not.toContain(leak.toLowerCase());
    }
  });

  // Signing in for the CLI says so, as step one of two, and the device page with its code is where
  // the sign-in comes back to (design/specs/screens.md, A0); any other page is not kept.
  it("says it is step one of approving the CLI, and keeps the device page for the sign-in to come back to", async () => {
    config();
    window.location.hash = "#/device?code=K7QP-4RXM";
    render(<SignIn />);
    expect(await screen.findByRole("heading", { name: "Sign in to approve the Canyon CLI" })).toBeDefined();
    expect(screen.getByText("Canyon CLI · Step 1 of 2")).toBeDefined();
    expect(screen.getByText("Then you'll come back here to approve the code from your terminal.")).toBeDefined();
    fireEvent.click(screen.getByRole("link", { name: "Continue to company sign-in" }));
    expect(sessionStorage.getItem("canyon_return")).toBe("#/device?code=K7QP-4RXM");
  });

  // A link someone was sent opens what it names once they sign in (the expert review), as the device page always did.
  it("keeps the page a link names, with its query, and no other", async () => {
    config();
    for (const [hash, kept] of [...["#/no-access/claims-portal", "#/approve?code=QX7M-2KPD", "#/apps/claims-portal", "#/apps/claims-portal?run=gr_8f2a", "#/apps/claims-portal/outbound",
      "#/apps/claims-portal/secrets?name=STRIPE_KEY&env=prod", "#/audit?question=production&range=7d", "#/audit?since=2026-11-01T00%3A00%3A00Z", "#/guardrails/policies?rule=sql.string-concat", "#/settings", "#/settings/people"].map((one) => [one, one] as const),
      ["#/fleet?q=claims", null], ["", null]] as const) {
      sessionStorage.clear();
      window.location.hash = hash;
      const { unmount } = render(<SignIn />);
      expect(await screen.findByText("Meridian Insurance")).toBeDefined();
      expect(screen.queryByText("Canyon CLI · Step 1 of 2")).toBeNull();
      fireEvent.click(screen.getByRole("link", { name: "Continue to company sign-in" }));
      expect(sessionStorage.getItem("canyon_return")).toBe(kept);
      unmount();
    }
  });

  // Only ever an address of this dashboard's own pages: whatever is in the tab's storage or the address bar is not taken.
  it("takes back nothing that is not one of those pages", () => {
    const long = `#/audit?q=${"a".repeat(600)}`;
    for (const hash of ["https://evil.example/", "//evil.example", "/\\evil.example", "javascript:alert(1)", "#javascript:alert(1)", "#//evil.example", "#/\\evil.example", "#/apps//evil.example", "#/apps/../settings", "#/apps/%2e%2e/settings",
      "#/apps/Claims", "#/apps/claims-portal/", "#/apps/claims-portal/delete", "#/apps/claims-portal#/settings", "#/apps/claims-portal\n", "#/apps/claims-portal?run=<img src=x>", "#/apps/claims-portal?run=a b", "#/audit?x=\u202egnp",
      "#/auditor", "#/settings/../apps", "#/settings/people/x/y", "#/settings//evil.example", "#/guardrails\t", "#/signin", "#/setup-required", "#/fleet", "x#/audit", " #/audit", "#/AUDIT", long, ""]) {
      sessionStorage.setItem("canyon_return", hash);
      expect(takeReturn(), hash.slice(0, 60)).toBeNull();
      expect(sessionStorage.getItem("canyon_return")).toBeNull();
    }
  });

  // A page kept by a sign-in that was given up must not come back after another, begun somewhere else.
  it("forgets the page an earlier, unfinished sign-in began on", async () => {
    config();
    for (const [reason, left] of [[undefined, null], ["signed-out", null], ["failed", "#/apps/claims-portal"]] as const) {
      sessionStorage.setItem("canyon_return", "#/apps/claims-portal");
      window.location.hash = reason === "failed" ? "#/signin" : "#/fleet";
      const { unmount } = render(<SignIn reason={reason} />);
      fireEvent.click(await screen.findByRole("link", { name: /sign-in|Try again/ }));
      expect(sessionStorage.getItem("canyon_return"), String(reason)).toBe(left);
      unmount();
    }
  });

  // Signing out was chosen, and the next person to sign in here may be another: they start on the fleet.
  it("keeps no page after a sign-out the person chose", async () => {
    config();
    sessionStorage.clear();
    window.location.hash = "#/apps/claims-portal";
    render(<SignIn reason="signed-out" />);
    fireEvent.click(await screen.findByRole("link", { name: /sign-in/ }));
    expect(sessionStorage.getItem("canyon_return")).toBeNull();
  });
});
