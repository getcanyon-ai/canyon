import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AUTH, S } from "../strings";
import { initials } from "./Avatar";
import { Rail, RailContext } from "./Rail";

const signedIn = (current: "fleet" | "guardrails" | "audit" | "settings") =>
  render(<RailContext value={{ org: "Meridian Insurance", viewer: { display: "Dana Whitfield", role: "admin", github_logins: [], github_link: false }, admin: true }}><Rail current={current} /></RailContext>);

afterEach(() => vi.unstubAllGlobals());

describe("the rail", () => {
  it("marks the screen you are on, and only that one", () => {
    render(<Rail current="guardrails" />);
    const fleet = screen.getByRole("link", { name: "Fleet" });
    const guardrails = screen.getByRole("link", { name: "Guardrails" });
    expect(guardrails.getAttribute("aria-current")).toBe("page");
    expect(fleet.hasAttribute("aria-current")).toBe(false);
    expect(fleet.getAttribute("href")).toBe("#/fleet");
    expect(guardrails.getAttribute("href")).toBe("#/guardrails");
    expect(screen.getByRole("link", { name: "Audit" }).getAttribute("href")).toBe("#/audit");
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("href")).toBe("#/settings");
  });

  // "Who am I acting as?" belongs on every page (the redesign): the foot is the rail's, so every screen with
  // the rail carries it.
  it("names who is signed in, their role and Sign out at its foot, on every screen", () => {
    for (const current of ["fleet", "guardrails", "audit", "settings"] as const) {
      const { unmount } = signedIn(current);
      expect(screen.getByText("Dana Whitfield")).toBeDefined();
      expect(screen.getByText(/Admin/)).toBeDefined();
      expect(screen.getByRole("button", { name: AUTH.signOut })).toBeDefined();
      expect(screen.getByText("Meridian Insurance")).toBeDefined();
      unmount();
    }
  });

  it("signs out through DELETE /api/v1/sessions", async () => {
    const fetch = vi.fn(async () => ({ ok: true, status: 204, json: async () => null }) as unknown as Response);
    vi.stubGlobal("fetch", fetch);
    signedIn("fleet");
    fireEvent.click(screen.getByRole("button", { name: AUTH.signOut }));
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledWith("/api/v1/sessions", expect.objectContaining({ method: "DELETE" })));
  });

  it("says Member to a member", () => {
    render(<RailContext value={{ org: null, viewer: { display: "Sam Reyes", role: "member", github_logins: [], github_link: false }, admin: false }}><Rail current="audit" /></RailContext>);
    expect(screen.getByText(/Member/)).toBeDefined();
    expect(screen.queryByText(/Admin/)).toBeNull();
  });

  // Guardrails is an admin's screen (design/specs/screens.md, A4b): a member's rail never offers it.
  it("offers a member no Guardrails, and an admin all four views", () => {
    const { unmount } = render(<RailContext value={{ org: null, viewer: { display: "Sam Reyes", role: "member", github_logins: [], github_link: false }, admin: false }}><Rail current="fleet" /></RailContext>);
    expect(screen.getAllByRole("link").map((one) => one.textContent)).toEqual(["Fleet", "Audit", "Settings", AUTH.approveCli, S.sourceCode]);
    unmount();
    signedIn("fleet");
    expect(screen.getAllByRole("link").map((one) => one.textContent)).toEqual(["Fleet", "Guardrails", "Audit", "Settings", AUTH.approveCli, S.sourceCode]);
  });

  // The CLI's approval page from the dashboard (#2564): every signed-in person's, at the foot, the page alone —
  // no code, no count, nothing pending named. A host with no one signed in has no foot and no entry.
  it("offers Approve a CLI sign-in at its foot, to the approval page alone", () => {
    const { unmount } = render(<RailContext value={{ org: null, viewer: { display: "Sam Reyes", role: "member", github_logins: [], github_link: false }, admin: false }}><Rail current="fleet" /></RailContext>);
    const entry = screen.getByRole("link", { name: AUTH.approveCli });
    expect(entry.getAttribute("href")).toBe("#/device");
    expect(entry.textContent).toBe(AUTH.approveCli);
    expect(readFileSync(join(import.meta.dirname, "../../../design/EXPERIENCE.md"), "utf8")).toContain(`| \`${AUTH.approveCli}\` |`); // D5's, byte for byte
    unmount();
    render(<Rail current="fleet" />);
    expect(screen.queryByRole("link", { name: AUTH.approveCli })).toBeNull();
  });

  it("draws at most two initials, each a whole character", () => {
    expect(initials("Dana Whitfield")).toBe("DW");
    expect(initials("Priya")).toBe("P");
    expect(initials("Ana María de la Cruz")).toBe("AM");
    expect(initials("🦊 Fox")).toBe("🦊F");
    expect(initials("")).toBe("");
  });

  // A push is the person's who linked the GitHub account that made it (#2228): the foot offers the link where
  // the host can make one and the reader has none, and names the accounts they linked.
  it("offers to link a GitHub account, and names the ones linked", () => {
    const foot = (github_logins: string[], github_link: boolean) =>
      render(<RailContext value={{ org: null, viewer: { display: "Dana Whitfield", role: "admin", github_logins, github_link }, admin: true }}><Rail current="fleet" /></RailContext>);
    let { unmount } = foot([], true);
    expect(screen.getByRole("link", { name: AUTH.githubLink }).getAttribute("href")).toBe("/api/v1/github/link");
    unmount();
    ({ unmount } = foot(["dana-gh", "dana-work"], true));
    expect(screen.getByText("GitHub: dana-gh, dana-work")).toBeDefined();
    expect(screen.queryByRole("link", { name: AUTH.githubLink })).toBeNull();
    unmount();
    foot([], false);
    expect(screen.queryByRole("link", { name: AUTH.githubLink })).toBeNull();
    expect(screen.queryByText(/GitHub:/)).toBeNull();
  });

  // A host with sign-in off has nobody signed in and nothing to sign out of: no foot, not a guess.
  it("has no foot when nobody is signed in", () => {
    render(<RailContext value={{ org: "Meridian Insurance", viewer: null, admin: true }}><Rail current="fleet" /></RailContext>);
    expect(screen.queryByRole("button", { name: AUTH.signOut })).toBeNull();
  });

  // On a phone the views fold behind one button, which says whether it is open.
  it("folds the views behind a menu button that says whether it is open", () => {
    signedIn("fleet");
    const menu = screen.getByRole("button", { name: "Menu" });
    expect(menu.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById("rail-menu")?.className).toContain("max-[860px]:hidden");
    fireEvent.click(menu);
    expect(menu.getAttribute("aria-expanded")).toBe("true");
    expect(document.getElementById("rail-menu")?.className).not.toContain("max-[860px]:hidden");
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(menu.getAttribute("aria-expanded")).toBe("false");
  });

  // AGPL-3.0 section 13 (LICENSE, #2878): every host offers Canyon's source to whoever uses it, signed in or not.
  it("links Canyon's source at its foot, with a reader and without one", () => {
    for (const rail of [() => signedIn("fleet"), () => render(<Rail current="fleet" />)]) {
      const { unmount } = rail();
      const source = screen.getByRole("link", { name: S.sourceCode });
      expect(source.getAttribute("href")).toBe("https://github.com/getcanyon-ai/canyon");
      expect(source.getAttribute("target")).toBe("_blank");
      unmount();
    }
  });
});
