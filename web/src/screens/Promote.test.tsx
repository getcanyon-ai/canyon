import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Receipt } from "../api";
import { Promote } from "./Promote";

afterEach(() => vi.unstubAllGlobals());
const receipt = { rules: [], blocked_by: [] } as unknown as Receipt;

// Enter on Promote, and Enter again, must not promote (the expert review): the question's safe answer has the keyboard,
// before production's secrets are read and after.
describe("promoting is answered on purpose", () => {
  it("lands the keyboard on Cancel and leaves it there, so a second Enter promotes nothing", async () => {
    let answer = (_: unknown) => {};
    vi.stubGlobal("fetch", vi.fn(() => new Promise((done) => { answer = done; })));
    const send = vi.fn(async () => null);
    render(<Promote id="claims-portal" shown="v1.4" live={false} receipt={receipt} answered={undefined} gateRun="gr_1" send={send} ended={() => {}} />);
    const open = screen.getByRole("button", { name: "Promote v1.4 to production" });
    open.focus();
    fireEvent.click(open);
    const cancel = screen.getByRole("button", { name: "Cancel" });
    expect(document.activeElement).toBe(cancel);
    await act(async () => answer({ ok: true, status: 200, json: async () => ({ secrets: [] }) }));
    expect(screen.getByRole("button", { name: "Promote v1.4" })).toBeDefined();
    expect(document.activeElement).toBe(cancel);
    fireEvent.click(document.activeElement!); // what Enter does to it
    expect(send).not.toHaveBeenCalled();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Promote v1.4 to production" }));
  });
});
