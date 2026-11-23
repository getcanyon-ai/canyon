import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { GO_LIVE, type Blocked, type Policy, type PolicyChange, type PolicyState } from "../api";
import { S } from "../strings";
import { Pipeline } from "./Pipeline";

// Every rule, state, project and sentence here comes from the contract samples at runtime.
type Apply = { rule: string; request: PolicyChange & { grace_days?: number }; response: { policy: Policy; would_block: Blocked[] } };
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/policies/meridian.json"), "utf8")) as {
  policies: { policies: Policy[] };
  production_apply: Apply;
  production_scheduled: Apply;
  production_no_model: { rule: string; request: PolicyChange; response: { error: string; message: string } };
  app_policy_put: { project: string; rule: string; response: { policy: Policy } };
};
const POLICIES = "/api/v1/policies";
const WORD = { off: "Off", advisory: "Advisory", blocking: "Blocking" } as const;
// Dependencies: Advisory before preview, the rule the samples place before production.
const deps = golden.policies.policies.find((one) => one.id === golden.production_apply.rule)!;
const DEPS = `${POLICIES}/${deps.id}`;
const impact = golden.production_apply.response.would_block;

// The server answers "<METHOD> <url>" from a table and 404s anything else; the reader lists every
// request as "<METHOD> <url>", with its body when it has one.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer | (() => Answer)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status = 200, body } = typeof route === "function" ? route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const routes = {
  [`GET ${POLICIES}`]: { body: golden.policies },
  [`PUT ${DEPS}`]: { body: { would_block: impact } },
  [`PUT ${DEPS}?apply=true`]: { body: golden.production_apply.response },
};
// The GET read with one rule replaced, as another sample answers it.
const reading = (policy: Policy) => ({ body: { policies: golden.policies.policies.map((rule) => (rule.id === policy.id ? policy : rule)) } });
// One answer per call, in order.
const queue = (...answers: Answer[]) => () => answers.shift()!;
// A rule in both gates is named for the gate in its second.
const there = (name: string) => `${name}, before production`;
// "Also before production" is a checkbox (#118), ticked while the rule is there: a click places it or takes it out.
const place = (rule: string) => row("Before preview", rule).getByRole("checkbox", { name: `Also before production: ${rule}` }) as HTMLInputElement;
const gate = (name: "Before preview" | "Before production") => within(screen.getByRole("region", { name }));
const row = (name: "Before preview" | "Before production", rule: string) => within(gate(name).getByRole("region", { name: rule }));
const choose = (name: "Before preview" | "Before production", rule: string, state: PolicyState) =>
  fireEvent.click(within(gate(name).getByRole("radiogroup", { name: rule })).getByRole("radio", { name: WORD[state] }));
const checked = (scope: ReturnType<typeof gate>, rule: string) =>
  (within(scope.getByRole("radiogroup", { name: rule })).getAllByRole("radio") as HTMLInputElement[]).find((one) => one.checked)?.value;
const shown = (name: "Before preview" | "Before production", rule: string) => checked(gate(name), rule);
const bar = () => within(screen.getByRole("region", { name: "Unsaved changes" }));
const drawn = async () => { const view = render(<Pipeline />); await screen.findByRole("region", { name: "Before preview" }); return view; };
const sentences = `${impact.length} app could not promote today: ${impact.map((one) => one.project).join(", ")}`;

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("Guardrails › Pipeline", () => {
  it("draws the two gates in order, every rule before preview at its state, and the go-live rules only before production", async () => {
    server(routes);
    await drawn();
    const stages = within(screen.getByRole("list", { name: "Pipeline stages" })).getAllByRole("listitem");
    expect(stages.map((one) => [...one.children].map((part) => part.textContent))).toEqual([["Upload", "agent or person"], ["Preview", "agents deploy"], ["Production", "people promote"]]);
    expect(screen.getAllByRole("heading").map((one) => one.textContent)).toEqual(["Before preview", "Before production"]);
    expect(gate("Before preview").getByText(S.beforePreview)).toBeDefined();
    expect(gate("Before production").getByText(S.beforeProduction)).toBeDefined();
    // Each gate looked up once, not twice a rule over the whole page: this is the file's slowest test, the first a loaded machine runs past its timeout (#2189).
    const gates = { "Before preview": gate("Before preview"), "Before production": gate("Before production") };
    for (const one of golden.policies.policies) {
      const where = GO_LIVE.some((id) => id === one.id) ? "Before production" : "Before preview";
      const other = where === "Before preview" ? "Before production" : "Before preview";
      expect(checked(gates[where], one.name)).toBe(one.state);
      expect(gates[other].queryByRole("region", { name: one.name })).toBeNull();
    }
    // A go-live rule can't be placed before preview: nothing on its row moves it.
    expect(row("Before production", GO_LIVE.map((id) => golden.policies.policies.find((one) => one.id === id)!.name)[0]!).queryByRole("button")).toBeNull();
  });

  it("an admin drags Dependencies to before production, sets it Blocking there, and saves: one PUT naming that gate", async () => {
    const sent = server(routes);
    await drawn();
    const dragged = gate("Before preview").getByRole("region", { name: deps.name });
    expect(dragged.getAttribute("draggable")).toBe("true");
    fireEvent.dragStart(dragged, { dataTransfer: { setData: vi.fn(), effectAllowed: "" } });
    expect(gate("Before production").getByText(`Drop here: ${deps.name}, before production`)).toBeDefined();
    fireEvent.dragOver(screen.getByRole("region", { name: "Before production" }));
    fireEvent.drop(screen.getByRole("region", { name: "Before production" }));
    expect(gate("Before production").queryByText(/^Drop here/)).toBeNull();
    // It goes there at its state before preview, named for that gate; its Dial there makes it Blocking.
    expect(shown("Before production", there(deps.name))).toBe(deps.state);
    expect(gate("Before production").queryByRole("radiogroup", { name: deps.name })).toBeNull();
    choose("Before production", there(deps.name), golden.production_apply.request.production!);
    await bar().findByText(sentences);
    const said = bar().getByText(`${deps.name} also blocks before production. It stays ${WORD[deps.state]} before preview.`, { exact: false });
    expect(said.closest("[aria-live]")?.getAttribute("aria-live")).toBe("polite"); // a screen reader hears the change
    expect(shown("Before preview", deps.name)).toBe(deps.state);
    fireEvent.click(bar().getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull());
    expect(sent().filter((one) => one.startsWith("PUT"))).toEqual([
      `PUT ${DEPS} ${JSON.stringify({ production: deps.state })}`,
      `PUT ${DEPS} ${JSON.stringify({ production: golden.production_apply.request.production })}`,
      `PUT ${DEPS}?apply=true ${JSON.stringify(golden.production_apply.request)}`,
    ]);
    expect(shown("Before production", there(deps.name))).toBe(golden.production_apply.response.policy.production);
  });

  it("a tightening before production may wait 14 days, and the row says what it becomes and when", async () => {
    const sent = server({ ...routes, [`PUT ${DEPS}?apply=true`]: { body: golden.production_scheduled.response } });
    await drawn();
    fireEvent.click(place(deps.name));
    choose("Before production", there(deps.name), golden.production_scheduled.request.production!);
    fireEvent.click(await bar().findByRole("button", { name: "Save, tightening in 14 days" }));
    const when = new Date(golden.production_scheduled.response.policy.scheduled_production!.effective_at).toLocaleDateString();
    await row("Before production", there(deps.name)).findByText(`Becomes ${WORD[golden.production_scheduled.request.production!]} on ${when}`);
    expect(sent().filter((one) => one.includes("?apply=true"))).toEqual([`PUT ${DEPS}?apply=true ${JSON.stringify(golden.production_scheduled.request)}`]);
  });

  // The keyboard's way: the row's button places it, the Dials set each gate, and moving it out of
  // before preview is setting it Off there — one change, one PUT naming both gates.
  it("the keyboard moves Dependencies to before production and takes it out again", async () => {
    const sent = server(routes);
    await drawn();
    const box = place(deps.name);
    box.focus();
    expect(document.activeElement).toBe(box);
    expect(box.checked).toBe(false);
    fireEvent.click(box); // a native checkbox: Space is its click
    expect(shown("Before production", there(deps.name))).toBe(deps.state);
    expect(document.activeElement).toBe(box);
    expect(box.checked).toBe(true);
    expect(gate("Before preview").getByRole("region", { name: deps.name }).getAttribute("draggable")).toBe("false"); // no grip: it is there already
    choose("Before production", there(deps.name), "blocking");
    choose("Before preview", deps.name, "off");
    await bar().findByText(sentences);
    expect(bar().getByText(`${deps.name} blocks before production. It is Off before preview.`, { exact: false })).toBeDefined();
    // It tightens one gate and loosens the other, so it saves now: no grace is offered.
    expect(bar().queryByRole("button", { name: /in 14 days/ })).toBeNull();
    fireEvent.click(bar().getByRole("button", { name: "Save" }));
    await waitFor(() => expect(sent().filter((one) => one.includes("?apply=true"))).toEqual([`PUT ${DEPS}?apply=true ${JSON.stringify({ state: "off", production: "blocking" })}`]));

    // Taken out before it is saved, the change goes with it.
    cleanup();
    server(routes);
    await drawn();
    fireEvent.click(place(deps.name));
    await bar().findByText(sentences);
    fireEvent.click(place(deps.name));
    expect(gate("Before production").queryByRole("region", { name: there(deps.name) })).toBeNull();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  it("a refused preview takes the placement with it and says the server's sentence after the rule's name", async () => {
    const rule = golden.policies.policies.find((one) => one.id === golden.production_no_model.rule)!;
    server({ ...routes, [`PUT ${POLICIES}/${rule.id}`]: { status: 409, body: golden.production_no_model.response } });
    await drawn();
    fireEvent.click(place(rule.name));
    await bar().findByText(`${rule.name}: ${golden.production_no_model.response.message}`);
    expect(gate("Before production").queryByRole("region", { name: there(rule.name) })).toBeNull();
  });

  it("a refused preview puts back the change the rule held before it", async () => {
    const refused = golden.production_no_model.response;
    server({ ...routes, [`PUT ${DEPS}`]: queue({ body: { would_block: impact } }, { status: 409, body: refused }) });
    await drawn();
    fireEvent.click(place(deps.name));
    await bar().findByText(sentences);
    choose("Before production", there(deps.name), "blocking");
    await bar().findByText(`${deps.name}: ${refused.message}`);
    expect(shown("Before production", there(deps.name))).toBe(deps.state);
    expect(bar().getByText(`${deps.name} also warns before production. It stays ${WORD[deps.state]} before preview.`, { exact: false })).toBeDefined();
  });

  // A rule already saved before production: taking it out and putting it back is no change at all.
  it("taking a saved check out and putting it back restores what is saved there", async () => {
    const placed = golden.production_apply.response.policy;
    const sent = server({ ...routes, [`GET ${POLICIES}`]: reading(placed) });
    await drawn();
    expect(shown("Before production", there(placed.name))).toBe(placed.production);
    fireEvent.click(place(placed.name));
    await bar().findByText(`${placed.name} comes out of before production. It stays ${WORD[placed.state]} before preview.`, { exact: false });
    expect(shown("Before production", there(placed.name))).toBe("off");
    fireEvent.click(place(placed.name));
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    expect(shown("Before production", there(placed.name))).toBe(placed.production);
    expect(sent().filter((one) => one.startsWith("PUT"))).toEqual([`PUT ${DEPS} ${JSON.stringify({ production: "off" })}`]);
  });

  // Scheduled there but not yet in force: it is in the gate, and taking it out withdraws the edit.
  it("a placement scheduled ahead is taken out by keeping the gate Off", async () => {
    const scheduled = golden.production_scheduled.response.policy;
    const sent = server({ ...routes, [`GET ${POLICIES}`]: reading(scheduled), [`PUT ${DEPS}?apply=true`]: { body: { policy: { ...scheduled, scheduled_production: undefined }, would_block: [] } } });
    await drawn();
    expect(gate("Before preview").getByRole("region", { name: scheduled.name }).getAttribute("draggable")).toBe("false");
    fireEvent.click(place(scheduled.name));
    await bar().findByText(`${scheduled.name}, keep Off before production.`, { exact: false });
    fireEvent.click(await bar().findByRole("button", { name: "Save" }));
    await waitFor(() => expect(gate("Before production").queryByRole("region", { name: there(scheduled.name) })).toBeNull());
    expect(sent().filter((one) => one.includes("?apply=true"))).toEqual([`PUT ${DEPS}?apply=true ${JSON.stringify({ production: "off" })}`]);
  });

  it("an admin sees an app's addition dashed in the gate it names, read-only, with its app and who added it", async () => {
    const added = golden.app_policy_put.response.policy;
    const one = added.additions![0]!;
    server({ ...routes, [`GET ${POLICIES}`]: reading(added) });
    await drawn();
    const name = `${added.name}, ${one.project} only`;
    const drawnAddition = gate("Before production").getByRole("region", { name });
    expect(drawnAddition.className).toContain("border-dashed");
    expect(within(drawnAddition).getByText(`${one.project} only · added by ${one.set_by.display}`)).toBeDefined();
    expect(shown("Before production", name)).toBe(one.production);
    for (const radio of within(drawnAddition).getAllByRole("radio") as HTMLInputElement[]) expect(radio.disabled).toBe(true);
    // The addition names no state before preview, and the org's rule is not placed before production.
    expect(gate("Before preview").queryByRole("region", { name })).toBeNull();
    expect(gate("Before production").queryByRole("region", { name: there(added.name) })).toBeNull();
  });

  // design/EXPERIENCE.md D5 (#2384): platform.known-vuln is beta in v0.2.0, its row marked beside its name; no other row is.
  it("marks platform.known-vuln's row Beta, with what beta means, and no other row", async () => {
    server(routes);
    await drawn();
    const name = (id: string) => golden.policies.policies.find((one) => one.id === id)!.name;
    expect(row("Before production", name("platform.known-vuln")).getByText(S.beta).textContent).toBe(`${S.beta}: ${S.betaWhy}`);
    expect(screen.getAllByText(S.beta)).toHaveLength(1);
  });
});
