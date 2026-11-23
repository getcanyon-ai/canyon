import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FleetPage, Hold, Reduced } from "../api";
import { S } from "../strings";
import { HoldDialog } from "./HoldDialog";

const responder = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/responder.json"), "utf8")) as FleetPage;
const app = responder.apps.find((one) => one.id === "claims-portal") as Reduced & { hold: Hold };
const title = `Lift the hold on ${app.name}?`;

afterEach(() => vi.unstubAllGlobals());

// Enter on the button that opens a question, and Enter again, must not answer it (the expert review).
describe("lifting a hold is answered on purpose", () => {
  it("lands the keyboard on Cancel, so a second Enter sends nothing", () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const close = vi.fn();
    render(<HoldDialog acting={{ app, kind: "lift", asked: 1 }} now={app} close={close} />);
    expect(document.activeElement).toBe(within(screen.getByRole("dialog", { name: title })).getByRole("button", { name: "Cancel" }));
    fireEvent.click(document.activeElement!); // what Enter does to it
    expect(close).toHaveBeenCalledOnce();
    expect(fetch).not.toHaveBeenCalled();
  });

  it("draws neither answer as the main one, the one that starts production least of all", () => {
    render(<HoldDialog acting={{ app, kind: "lift", asked: 1 }} now={app} close={() => {}} />);
    const start = screen.getByRole("button", { name: S.liftAndStart.replace("<version>", app.hold.version!) });
    expect(start.className).toBe(screen.getByRole("button", { name: S.liftKeepStopped }).className);
    expect(start.className).not.toContain("bg-accent");
  });

  it("still lands a stop on its reason", () => {
    render(<HoldDialog acting={{ app: { ...app, hold: undefined }, kind: "stop", asked: 1 }} now={undefined} close={() => {}} />);
    expect(document.activeElement).toBe(screen.getByRole("textbox", { name: "Reason" }));
  });
});

// A stop whose answer never came (the network, not the server): nobody knows here whether it stopped. Asking again is
// safe: the server resumes a stop it began, and answers 409 held to one it finished (contracts/routes.md, The hold).
describe("a stop whose answer never arrives", () => {
  const ask = async (fetch: () => Promise<unknown>, running = true) => {
    vi.stubGlobal("fetch", vi.fn(fetch));
    const close = vi.fn();
    render(<HoldDialog acting={{ app: { ...app, hold: undefined, envs: running ? { prod: { url: "http://claims-portal.prod.localhost:7777" } } : {} }, kind: "stop", asked: 1 }} now={undefined} close={close} />);
    fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "why" } });
    fireEvent.click(screen.getByRole("button", { name: /^(Stop|Hold) production$/ }));
    await waitFor(() => expect(screen.queryByRole("status")).toBeNull());
    expect(close).not.toHaveBeenCalled();
  };
  const LOST = "Canyon's answer didn't arrive, so whether production stopped isn't known. Asking again is safe.";
  it("says the outcome isn't known and that asking again is safe, and keeps the question open", async () => {
    await ask(() => Promise.reject(new TypeError("Failed to fetch")));
    expect(screen.getByRole("alert").textContent).toBe(LOST);
    expect(screen.queryByText("Failed to fetch")).toBeNull();
    expect((screen.getByLabelText("Reason") as HTMLTextAreaElement).value).toBe("why");
    expect(screen.getByRole("button", { name: "Stop production" })).toBeDefined();
  });
  it("says the server's own words when it answered", async () => {
    await ask(async () => ({ ok: false, status: 409, json: async () => ({ error: "held", message: "Production was stopped by Rhea." }) }));
    expect(screen.getByRole("alert").textContent).toBe("Production was stopped by Rhea.");
  });
  it("claims nothing of a stop where nothing ran", async () => {
    await ask(() => Promise.reject(new TypeError("Failed to fetch")), false);
    expect(screen.queryByText(LOST)).toBeNull();
  });
});
