import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Blocked, Policy, PolicyChange, PolicyState } from "../api";
import { Impact, type Flow } from "./PolicyChange";

// What a change's preview knows, in words that claim no more (the expert review): would_block names the apps a target of
// blocking would hold, from their latest review (contracts/routes.md, the policies row). It says nothing of what a looser
// rule lets through, of a rule no review had on, nor of a rule judged when an app is promoted, which no review holds.
const said = (rule: Partial<Policy>, change: PolicyChange | undefined, wouldBlock: Blocked[] | null = []) => {
  const policy = { id: "sql.string-concat", name: "No hand-built SQL", state: "off" as PolicyState, ...rule } as Policy;
  const flow = { of: () => (change === undefined ? undefined : { rule: policy, change, wouldBlock }) } as unknown as Flow;
  const { container, unmount } = render(<Impact flow={flow} rule={policy} today={change === undefined ? (wouldBlock ?? []) : undefined} />);
  const text = container.textContent;
  unmount();
  return text;
};
const NONE = "No app is affected today.";
const OFF = "Not known yet: reviews may not check a rule while it's Off.";
const PROMOTED = "Canyon checks this rule when an app is promoted, so it can't say which apps this affects.";
const LOOSER = "Canyon doesn't check which apps this lets through.";
const WARNS = "Advisory blocks no app.";

describe("a rule change's impact", () => {
  it("says no app is affected only where the preview read one that could be", () => {
    expect(said({ state: "advisory" }, { state: "blocking" })).toBe(NONE);
    expect(said({ production: "advisory" }, { production: "blocking" })).toBe(NONE);
    expect(said({}, { production: "blocking" })).toBe(NONE); // an app whose review had it off is named, at 0 findings
    expect(said({ state: "blocking" }, undefined)).toBe(NONE);
  });
  it("says it isn't known for a rule no review had on", () => {
    expect(said({}, { state: "blocking" })).toBe(OFF);
    expect(said({}, { state: "blocking", production: "blocking" })).toBe(OFF);
    expect(said({ production: "advisory" }, { state: "blocking" })).toBe(NONE); // on before production, so reviewed
  });
  it("says it isn't known for a rule judged when an app is promoted, on or off, changed or not", () => {
    for (const id of ["prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required"]) {
      for (const state of ["off", "advisory"] as const) expect(said({ id, state }, { state: "blocking" })).toBe(PROMOTED);
      expect(said({ id, state: "blocking" }, undefined)).toBe(PROMOTED);
    }
  });
  it("knows the two such rules the preview reads now", () => {
    for (const id of ["platform.known-vuln", "prod.egress-allowed"]) expect(said({ id }, { state: "blocking" })).toBe(NONE);
  });
  it("says what a looser rule's preview doesn't check, and never that it blocks no app: it may still, before production", () => {
    expect(said({ state: "blocking" }, { state: "advisory" })).toBe(LOOSER);
    expect(said({ state: "advisory" }, { state: "off" })).toBe(LOOSER);
    expect(said({ state: "blocking", production: "blocking" }, { state: "off" })).toBe(LOOSER);
  });
  it("says a rule that comes to warn blocks nothing", () => {
    expect(said({}, { state: "advisory" })).toBe(WARNS);
  });
  it("names the apps a change would hold, as before", () => {
    expect(said({ state: "advisory" }, { state: "blocking" }, [{ project: "claims-portal", findings: 2 }])).toBe("1 app could not promote today: claims-portal");
  });
});
