import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, render, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { FleetPage, FleetRow, Need, Viewer } from "../api";
import { S } from "../strings";
import { NeedLine } from "./Need";

const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/fleet/page.json"), "utf8")) as FleetPage;
const app: FleetRow = { ...(golden.apps[0] as FleetRow), id: "ux-outbound", name: "ux-outbound" };
const member: Viewer = { display: "Tomas Berg", role: "member", github_logins: [], github_link: false };
const refused = (you: boolean): Need => ({ kind: "refused", env: "preview", rank: 7, since: "2026-11-08T09:00:00Z", you, ask: [], destination: "echo.canyon-floor.test",
  refusals: 3, last_refused_at: new Date(Date.now() - 60_000).toISOString(), other_calls: 0, held: false });
const line = (need: Need) => within(render(<ul><NeedLine app={app} need={need} viewer={member} signedIn lift={() => {}} done={() => {}} /></ul>).container);

afterEach(cleanup);

// What needs a person (contracts/routes.md): `you` is whether the reader can resolve it. A refused call is the app's
// team's to allow (design/EGRESS.md §3.5): a read-only member reads it with nothing to click, and is told who can.
describe("a refused call in Needs you", () => {
  it("offers a reader who can't allow it no Allow or Dismiss, and says who can", () => {
    const row = line(refused(false));
    expect(row.getByText(/^ux-outbound can't reach echo\.canyon-floor\.test in preview\./)).toBeDefined();
    expect(row.queryByRole("button")).toBeNull();
    // its own line: "anyone on its team" would read as the reader, who is on it too, read-only
    expect(row.getByText("An owner or contributor of ux-outbound, or an admin, can allow it.")).toBeDefined();
    expect(row.queryByText(S.needAllowWho)).toBeNull();
    expect(row.getByRole("link", { name: "Open the app: ux-outbound" })).toBeDefined(); // the app's page, which a read-only member opens; never Outbound
  });

  // A version production will refuse names, after the line, who can allow what it connects to: to a reader who can't, its
  // own line, one host's or many's, and nothing to click.
  it("tells a reader who can't allow what a version will be refused who can, one host or many", () => {
    const denied = [{ destination: "hooks.slack.com", port: 443, allowable: true }, { destination: "api.stripe.com", port: 443, allowable: true }];
    const willRefuse = (you: boolean, hosts: number): Need => ({ kind: "will_refuse", env: "prod", rank: 5, since: null, you, ask: [], version: "v1.4", blocks: false, denied: denied.slice(0, hosts) });
    for (const [you, hosts, said, not] of [
      [false, 1, "An owner or contributor of ux-outbound, or an admin, can allow it.", S.needAllowWho],
      [false, 2, "An owner or contributor of ux-outbound, or an admin, can allow them.", S.needAllowWhoMany],
      [true, 1, S.needAllowWho, "An owner or contributor of ux-outbound, or an admin, can allow it."],
      [true, 2, S.needAllowWhoMany, "An owner or contributor of ux-outbound, or an admin, can allow them."],
    ] as const) {
      const row = line(willRefuse(you, hosts));
      expect(row.getByText(said), `you ${you}, ${hosts} hosts`).toBeDefined();
      expect(row.queryByText(not), `you ${you}, ${hosts} hosts`).toBeNull();
      if (!you) expect(row.queryByRole("button"), `you ${you}, ${hosts} hosts`).toBeNull();
      cleanup();
    }
  });

  it("offers a reader who can allow it Allow and Dismiss", () => {
    const row = line(refused(true));
    expect(row.getByText(S.needAllowWho)).toBeDefined();
    expect(row.getByRole("button", { name: "Allow echo.canyon-floor.test in preview for ux-outbound" })).toBeDefined();
    expect(row.getByRole("button", { name: "Dismiss echo.canyon-floor.test for ux-outbound" })).toBeDefined();
  });
});
