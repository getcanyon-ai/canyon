import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, within } from "@testing-library/react";
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
