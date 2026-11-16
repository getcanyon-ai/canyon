import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Blocked, CustomRule, DryRunApp, Policy, PolicyState } from "../api";
import { slugOf } from "./CustomRules";
import { POLICY_MEANING, Policies } from "./Policies";
import { SCANNER_RULES } from "./Scanners";
import { S } from "../strings";

// Every rule, state, project and sentence here comes from the contract sample at runtime.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/policies/meridian.json"), "utf8")) as {
  policies: { policies: Policy[] };
  preview: { rule: string; request: { state: PolicyState }; response: { would_block: Blocked[] } };
  apply: { rule: string; request: { state: PolicyState }; response: { policy: Policy; would_block: Blocked[] } };
  member: { error: string; message: string };
  custom_rules: { custom_rules: CustomRule[] };
  custom_rule_put: { slug: string; request: { name: string; instructions: string }; response: { custom_rule: CustomRule } };
  custom_rule_dry_run: { slug: string; response: { rule: string; version: number; apps: DryRunApp[] } };
  no_model: { rule: string; request: { state: PolicyState }; response: { error: string; message: string } };
};
const POLICIES = "/api/v1/policies";
const RULE = `${POLICIES}/${golden.preview.rule}`;
const rule = golden.policies.policies.find((one) => one.id === golden.preview.rule)!;
const scheduled = golden.policies.policies.find((one) => one.scheduled !== undefined)!;
const WORD = { off: "Off", advisory: "Advisory", blocking: "Blocking" } as const;

// The server answers "<METHOD> <url>" from a table and 404s anything else; the reader lists every
// request as "<METHOD> <url>", with its body when it has one.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer | (() => Promise<Answer>)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status = 200, body } = typeof route === "function" ? await route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const CUSTOM = "/api/v1/custom-rules";
const routes = {
  [`GET ${POLICIES}`]: { body: golden.policies },
  [`GET ${CUSTOM}`]: { body: { custom_rules: [] } },
  [`PUT ${RULE}`]: { body: golden.preview.response },
  [`PUT ${RULE}?apply=true`]: { body: golden.apply.response },
};
const applied = (sent: () => string[]) => sent().filter((one) => one.includes("?apply=true"));
const row = (name = rule.name) => within(screen.getByRole("region", { name }));
// A rule's one control, and the state it shows.
const choose = async (name: string, state: PolicyState) => fireEvent.click(within(await screen.findByRole("radiogroup", { name })).getByRole("radio", { name: WORD[state] }));
const shown = (name: string) => (within(screen.getByRole("radiogroup", { name })).getAllByRole("radio") as HTMLInputElement[]).find((one) => one.checked)?.value;
const bar = () => within(screen.getByRole("region", { name: "Unsaved changes" }));
const change = async () => {
  render(<Policies />);
  await choose(rule.name, golden.preview.request.state);
  await row().findByText(`${golden.preview.response.would_block.length} apps could not promote today: ${golden.preview.response.would_block.map((one) => one.project).join(", ")}`);
};

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const DESIGN = golden.policies.policies.find((one) => one.id === "design.org-system")!;
const prefs = (designSystem: unknown) => ({ [`GET /api/v1/preferences`]: { body: { design_system: designSystem } } });

describe("Policies", () => {
  it("the design-system row says it checks nothing until a default is picked, and links the picker", async () => {
    server({ ...routes, ...prefs(null) });
    render(<Policies />);
    const link = await within(await screen.findByRole("region", { name: DESIGN.name })).findByRole("link", { name: "pick a default design system" });
    expect(link.getAttribute("href")).toBe("#/guardrails/agents");
    expect(row(DESIGN.name).getByText(/Checks nothing until you/)).toBeDefined();
  });

  it("with a default picked, or no read of it, the design-system row says nothing more", async () => {
    for (const answer of [prefs({ id: "ds_0003", name: "Meridian UI", namespace: "@meridian/ui", registry_url: "https://registry.meridian.example/ui" }), {}]) {
      const sent = server({ ...routes, ...answer });
      render(<Policies />);
      await screen.findByRole("region", { name: DESIGN.name });
      await waitFor(() => expect(sent()).toContain("GET /api/v1/preferences"));
      expect(row(DESIGN.name).queryByText(/Checks nothing until you/)).toBeNull();
      cleanup();
    }
  });

  it("draws one row per rule at its state, what it is about, and a scheduled change as what it becomes and when", async () => {
    server(routes);
    render(<Policies />);
    for (const one of golden.policies.policies.filter((p) => !SCANNER_RULES.includes(p.id))) {
      await screen.findByRole("radiogroup", { name: one.name });
      expect(shown(one.name)).toBe(one.state);
    }
    expect(screen.queryByRole("radiogroup", { name: "Findings from trivy" })).toBeNull(); // the tools Canyon runs are the Scanners tab's
    expect(row().getByText(POLICY_MEANING[rule.id]!)).toBeDefined();
    const when = new Date(scheduled.scheduled!.effective_at).toLocaleDateString();
    expect(screen.getByText(`Becomes ${WORD[scheduled.scheduled!.state]} on ${when}`)).toBeDefined();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  // design/EXPERIENCE.md D5 (#2384): platform.known-vuln is beta in v0.2.0. Here an admin turns it on, and its
  // checklist row's link lands here: its row is marked beside its name, and no other row is.
  it("marks platform.known-vuln's row Beta, with what beta means, and no other row", async () => {
    server(routes);
    render(<Policies />);
    const name = golden.policies.policies.find((one) => one.id === "platform.known-vuln")!.name;
    await screen.findByRole("radiogroup", { name });
    expect(row(name).getByText(S.beta).textContent).toBe(`${S.beta}: ${S.betaWhy}`);
    expect(screen.getAllByText(S.beta)).toHaveLength(1);
  });

  // Keeping the state in force supersedes the scheduled edit (contracts/routes.md, the policies row):
  // held like any change, saved now only, and the row no longer says what it becomes.
  it("an admin keeps the state in force instead of a scheduled change", async () => {
    const SCHEDULED = `${POLICIES}/${scheduled.id}`;
    const kept = { ...scheduled, scheduled: undefined };
    const sent = server({ ...routes, [`PUT ${SCHEDULED}`]: { body: { would_block: [] } }, [`PUT ${SCHEDULED}?apply=true`]: { body: { policy: kept, would_block: [] } } });
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: `Keep ${WORD[scheduled.state]}: ${scheduled.name}` }));
    expect(bar().getByText(`${scheduled.name}, keep ${WORD[scheduled.state]}`)).toBeDefined();
    fireEvent.click(await bar().findByRole("button", { name: "Save" }));
    expect(bar().queryByRole("button", { name: /in 14 days/ })).toBeNull();
    await waitFor(() => expect(screen.queryByText(/^Becomes /)).toBeNull());
    expect(applied(sent)).toEqual([`PUT ${SCHEDULED}?apply=true ${JSON.stringify({ state: scheduled.state })}`]);
  });

  it("a checklist link lands on its rule's row", async () => {
    server(routes);
    // jsdom has no scrollIntoView; the stub is removed below.
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    window.location.hash = `#/guardrails?rule=${rule.id}`;
    render(<Policies />);
    await waitFor(() => expect(scrolled).toHaveBeenCalledTimes(1));
    expect(scrolled.mock.contexts[0]).toBe(screen.getByRole("region", { name: rule.name }));
    // The row the link names is marked, and no other: a reader sent here sees which row is meant.
    expect(screen.getAllByRole("region").filter((row) => row.getAttribute("aria-current") === "true")).toEqual([screen.getByRole("region", { name: rule.name })]);
    window.location.hash = "";
    delete (Element.prototype as { scrollIntoView?: unknown }).scrollIntoView;
  });

  // Affects today says what each rule does now, before anything is changed: the server's preview of
  // the rule at its own state, the same words a change's preview uses.
  it("says what each rule blocks today before any change", async () => {
    const sent = server(routes);
    render(<Policies />);
    await screen.findByRole("region", { name: rule.name });
    await row().findByText(`${golden.preview.response.would_block.length} apps could not promote today: ${golden.preview.response.would_block.map((one) => one.project).join(", ")}`);
    expect(sent()).toContain(`PUT ${RULE} ${JSON.stringify({ state: rule.state })}`);
    expect(applied(sent)).toHaveLength(0);
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  it("a change previews what it would block and writes nothing until Save", async () => {
    const sent = server(routes);
    await change();
    expect(sent()).toContain(`PUT ${RULE} ${JSON.stringify(golden.preview.request)}`);
    expect(applied(sent)).toHaveLength(0);
    expect(shown(rule.name)).toBe(golden.preview.request.state); // the control shows what is asked
    expect(bar().getByText(`${rule.name}, ${WORD[rule.state]} → ${WORD[golden.preview.request.state]}`)).toBeDefined();
    expect(bar().getByText("Apps already in production keep running: policies gate promotion, never running apps.")).toBeDefined();
    expect(bar().queryByText(/able to reach production/)).toBeNull();
    fireEvent.click(bar().getByRole("button", { name: "Save" }));
    await waitFor(() => expect(applied(sent)).toEqual([`PUT ${RULE}?apply=true ${JSON.stringify(golden.apply.request)}`]));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull());
    expect(shown(rule.name)).toBe(golden.apply.response.policy.state);
    await row().findByText(/could not promote today/); // nothing asked of it any more: what it blocks today, at its new state
    expect(screen.getByRole("status")).toHaveProperty("textContent", "Saved."); // the bar went, and the page says why
    fireEvent.click(within(screen.getByRole("radiogroup", { name: rule.name })).getByText(WORD[rule.state]));
    await bar().findByRole("button", { name: "Discard" });
    expect(screen.queryByText("Saved.")).toBeNull(); // a new change is not saved yet
  });

  it("a tightening saved in 14 days sends the grace", async () => {
    const sent = server(routes);
    await change();
    fireEvent.click(bar().getByRole("button", { name: "Save, tightening in 14 days" }));
    await waitFor(() => expect(applied(sent)).toEqual([`PUT ${RULE}?apply=true ${JSON.stringify({ ...golden.preview.request, grace_days: 14 })}`]));
  });

  it("a loosening says what it lets through, and offers no grace", async () => {
    const loose = golden.policies.policies.find((one) => one.state === "blocking")!;
    server({ ...routes, [`PUT ${POLICIES}/${loose.id}`]: { body: { would_block: [] } } });
    render(<Policies />);
    await choose(loose.name, "off");
    expect(await row(loose.name).findByText("No app is affected today.")).toBeDefined();
    expect(bar().getByText("Apps with these findings will be able to reach production.")).toBeDefined();
    expect(bar().queryByRole("button", { name: /tightening/ })).toBeNull();
  });

  it("collects every change in one bar and saves them together", async () => {
    const loose = golden.policies.policies.find((one) => one.state === "blocking")!;
    const sent = server({ ...routes, [`PUT ${POLICIES}/${loose.id}`]: { body: { would_block: [] } }, [`PUT ${POLICIES}/${loose.id}?apply=true`]: { body: { policy: { ...loose, state: "off" }, would_block: [] } } });
    await change();
    await choose(loose.name, "off");
    expect(bar().getByText("2 changes:")).toBeDefined();
    await row(loose.name).findByText("No app is affected today.");
    fireEvent.click(bar().getByRole("button", { name: "Save" }));
    await waitFor(() => expect(applied(sent)).toEqual([
      `PUT ${RULE}?apply=true ${JSON.stringify(golden.apply.request)}`,
      `PUT ${POLICIES}/${loose.id}?apply=true ${JSON.stringify({ state: "off" })}`,
    ]));
  });

  it("Discard drops every change and sends nothing", async () => {
    const sent = server(routes);
    await change();
    const before = sent().length;
    fireEvent.click(bar().getByRole("button", { name: "Discard" }));
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    expect(shown(rule.name)).toBe(rule.state);
    expect(sent()).toHaveLength(before);
  });

  it("choosing a rule's own state again drops its change", async () => {
    server(routes);
    await change();
    await choose(rule.name, rule.state);
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  it("saves nothing until every preview has answered", async () => {
    // Every preview of the rule waits, the row's own of today among them, in whatever order they go out.
    const waiting: ((_: Answer) => void)[] = [];
    server({ ...routes, [`PUT ${RULE}`]: () => new Promise<Answer>((done) => { waiting.push(done); }) });
    render(<Policies />);
    await choose(rule.name, golden.preview.request.state);
    expect(bar().getByText("Checking what this blocks…")).toBeDefined();
    expect(bar().queryByRole("button", { name: "Save" })).toBeNull();
    for (const done of waiting) done({ body: golden.preview.response });
    expect(await bar().findByRole("button", { name: "Save" })).toBeDefined();
  });

  it("a refusal mid-save names its rule and keeps it, with the ones before it written", async () => {
    const loose = golden.policies.policies.find((one) => one.state === "blocking")!;
    const sent = server({ ...routes, [`PUT ${POLICIES}/${loose.id}`]: { body: { would_block: [] } }, [`PUT ${POLICIES}/${loose.id}?apply=true`]: { status: 403, body: golden.member } });
    await change();
    await choose(loose.name, "off");
    fireEvent.click(await bar().findByRole("button", { name: "Save" }));
    expect(await bar().findByText(`${loose.name}: ${golden.member.message}`)).toBeDefined();
    expect(applied(sent)).toHaveLength(2);
    expect(bar().getByText("1 change:")).toBeDefined();
    expect(shown(rule.name)).toBe(golden.apply.response.policy.state);
    expect(shown(loose.name)).toBe("off"); // still asked, for another try
  });

  it("a choice made while saving is kept, not swept away with the one saved", async () => {
    let answer = (_: Answer) => {};
    server({ ...routes, [`PUT ${RULE}?apply=true`]: () => new Promise<Answer>((done) => { answer = done; }) });
    await change();
    fireEvent.click(bar().getByRole("button", { name: "Save" }));
    expect(await bar().findByText("Saving…")).toBeDefined();
    await choose(rule.name, "off");
    answer({ body: golden.apply.response });
    expect(await bar().findByText(new RegExp(`^${rule.name}, \\w+ → Off$`))).toBeDefined();
    expect(shown(rule.name)).toBe("off");
  });

  it("a refused preview takes its change with it", async () => {
    server({ ...routes, [`PUT ${RULE}`]: { status: 403, body: golden.member } });
    render(<Policies />);
    await choose(rule.name, golden.preview.request.state);
    expect(await bar().findByText(`${rule.name}: ${golden.member.message}`)).toBeDefined();
    expect(shown(rule.name)).toBe(rule.state);
    expect(bar().queryByRole("button", { name: "Save" })).toBeNull();
    expect(row().queryByText("Checking…")).toBeNull();
  });

  it("a preview that fails after Discard brings nothing back", async () => {
    let answer = (_: Answer) => {};
    server({ ...routes, [`PUT ${RULE}`]: () => new Promise<Answer>((done) => { answer = done; }) });
    render(<Policies />);
    await choose(rule.name, golden.preview.request.state);
    fireEvent.click(bar().getByRole("button", { name: "Discard" }));
    answer({ status: 500, body: { error: "internal", message: "the preview failed" } });
    await new Promise((done) => setTimeout(done, 0));
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  it("a read that fails says so above the rows, not in the save bar", async () => {
    server({ ...routes, [`GET ${POLICIES}`]: { status: 500, body: { error: "internal", message: "policies could not be read" } } });
    render(<Policies />);
    expect(await screen.findByText("policies could not be read")).toBeDefined();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
  });

  // gate.unreadable is the gate's own rule, not a policy: it is shown, locked, with nothing to change.
  it("shows files Canyon can't read as always blocking, with no control", async () => {
    server(routes);
    render(<Policies />);
    const locked = within(await screen.findByRole("region", { name: "Files Canyon can't read" }));
    expect(locked.getByText("Always blocks until a person approves")).toBeDefined();
    expect(locked.queryByRole("radiogroup")).toBeNull();
    expect(locked.queryByRole("button")).toBeNull();
  });

  it("a refusal shows the server's own sentence", async () => {
    server({ ...routes, [`PUT ${RULE}`]: { status: 403, body: golden.member } });
    render(<Policies />);
    await choose(rule.name, golden.preview.request.state);
    expect(await bar().findByText(`${rule.name}: ${golden.member.message}`)).toBeDefined();
  });
});

// The org's custom rules, from the contract's samples: each a card with its text, its author and
// version, and the policies' dial; a new one is written in the editor and installs Off.
describe("Custom rules", () => {
  const [email, pii, lodash] = golden.custom_rules.custom_rules as [CustomRule, CustomRule, CustomRule];
  const withCustoms = {
    ...routes,
    [`GET ${POLICIES}`]: { body: { ...golden.policies, policies: [...golden.policies.policies, ...golden.custom_rules.custom_rules.map((one) => ({ id: one.rule, name: one.name, state: "off" }))] } },
    [`GET ${CUSTOM}`]: { body: golden.custom_rules },
  };
  const card = (name: string) => within(screen.getByRole("region", { name }));

  it("shows a saved rule Off, with its text, its author and its version", async () => {
    server(withCustoms);
    render(<Policies />);
    await screen.findByRole("radiogroup", { name: email.name });
    expect(shown(email.name)).toBe("off");
    expect(card(email.name).getByText(`Written by ${email.set_by.display} · version ${email.version} · changed 30 Sept`)).toBeDefined();
    expect(card(email.name).getByText(email.instructions, { normalizer: (text) => text })).toBeDefined();
    expect(screen.getByText(/start in Advisory to see what it finds/)).toBeDefined();
  });

  it("writes a new rule in the editor and lists it, Off", async () => {
    const saved = { ...golden.custom_rule_put.response.custom_rule, rule: "custom.customer-emails-are-encrypted-at-rest", version: 1 };
    let list: CustomRule[] = [];
    let policies = golden.policies;
    const sent = server({
      ...routes,
      [`GET ${POLICIES}`]: { get body() { return policies; } } as unknown as Answer,
      [`GET ${CUSTOM}`]: { get body() { return { custom_rules: list }; } } as unknown as Answer,
      [`PUT ${CUSTOM}/customer-emails-are-encrypted-at-rest`]: { body: { custom_rule: saved } },
    });
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: email.name } });
    fireEvent.change(screen.getByLabelText("What should Canyon check?"), { target: { value: email.instructions } });
    expect(screen.getByText(`${email.instructions.length} / 2,000`)).toBeDefined();
    expect(screen.getByText(`${email.name.length} / 80 · its id: ${saved.rule}`)).toBeDefined();
    list = [saved];
    policies = { ...golden.policies, policies: [...golden.policies.policies, { id: saved.rule, name: email.name, state: "off" }] };
    fireEvent.click(screen.getByRole("button", { name: "Save the new rule" }));
    await screen.findByRole("radiogroup", { name: email.name });
    expect(shown(email.name)).toBe("off");
    expect(sent()).toContain(`PUT ${CUSTOM}/customer-emails-are-encrypted-at-rest ${JSON.stringify({ name: email.name, instructions: email.instructions })}`);
  });

  it("says what the server refuses in a rule, in its words", async () => {
    const message = "the instructions are 1 to 2,000 characters of plain text";
    server({ ...withCustoms, [`PUT ${CUSTOM}/${golden.custom_rule_put.slug}`]: { status: 400, body: { error: "invalid_custom_rule", message } } });
    render(<Policies />);
    fireEvent.click(await within(await screen.findByRole("region", { name: email.name })).findByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByRole("button", { name: `Save ${email.name}` }));
    expect(await screen.findByText(message)).toBeDefined();
  });

  it("says a rule can't run on a host with no model, when an admin turns one on", async () => {
    server({ ...withCustoms, [`PUT ${POLICIES}/${golden.no_model.rule}`]: { status: 409, body: golden.no_model.response } });
    render(<Policies />);
    await choose(pii.name, golden.no_model.request.state);
    expect(await bar().findByText(`${pii.name}: ${golden.no_model.response.message}`)).toBeDefined();
  });

  it("says how each rule is checked: by code, with what it checks, or by AI", async () => {
    server(withCustoms);
    render(<Policies />);
    await screen.findByRole("region", { name: lodash.name });
    expect(card(lodash.name).getByText(`Checked by code: ${lodash.check.says}`)).toBeDefined();
    expect(card(email.name).getByText("Checked by AI")).toBeDefined();
  });

  it("switches a rule checked by code to the AI, and an AI one to code where Canyon can", async () => {
    const sent = server({ ...withCustoms,
      [`PUT ${CUSTOM}/no-lodash`]: { body: { custom_rule: { ...lodash, version: 2, check: { by: "ai", facts_version: lodash.check.facts_version } } } },
      [`PUT ${CUSTOM}/${golden.custom_rule_put.slug}`]: { body: golden.custom_rule_put.response } });
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: `Check it with the AI instead: ${lodash.name}` }));
    await waitFor(() => expect(sent()).toContain(`PUT ${CUSTOM}/no-lodash ${JSON.stringify({ name: lodash.name, instructions: lodash.instructions, check: "ai" })}`));
    fireEvent.click(screen.getByRole("button", { name: `Check it by code where Canyon can: ${email.name}` }));
    await waitFor(() => expect(sent()).toContain(`PUT ${CUSTOM}/${golden.custom_rule_put.slug} ${JSON.stringify({ name: email.name, instructions: email.instructions, check: "code" })}`));
  });

  it("says why a rule asked to be checked by code stays with the AI", async () => {
    const why = "Canyon's AI model said the AI review should check this rule: it needs the code read.";
    server({ ...withCustoms, [`PUT ${CUSTOM}/${golden.custom_rule_put.slug}`]: { body: { custom_rule: email, why } } });
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: `Check it by code where Canyon can: ${email.name}` }));
    expect(await card(email.name).findByText(why)).toBeDefined();
  });

  it("dry runs a rule checked by code on the org's apps when asked: would pass, would fail, not checked and why", async () => {
    const dry = golden.custom_rule_dry_run;
    const sent = server({ ...withCustoms, [`POST ${CUSTOM}/${dry.slug}/dry-run`]: { body: dry.response } });
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: `Try it on your apps: ${lodash.name}` }));
    const list = await screen.findByRole("list", { name: `${lodash.name} on your apps` });
    expect(within(list).getByText("claims-portal: Would fail")).toBeDefined();
    expect(within(list).getByText("ledger: Would pass")).toBeDefined();
    expect(within(list).getByText("status-page: Not checked: It has no review yet.")).toBeDefined();
    expect(sent()).toContain(`POST ${CUSTOM}/${dry.slug}/dry-run`);
    expect(card(email.name).queryByRole("button", { name: `Try it on your apps: ${email.name}` })).toBeNull(); // a rule checked by AI has none
  });

  it("dry runs a rule on save when the save leaves it checked by code", async () => {
    const dry = golden.custom_rule_dry_run;
    const sent = server({ ...withCustoms,
      [`PUT ${CUSTOM}/no-lodash`]: { body: { custom_rule: { ...lodash, version: 2 } } },
      [`POST ${CUSTOM}/${dry.slug}/dry-run`]: { body: dry.response } });
    render(<Policies />);
    fireEvent.click(await within(await screen.findByRole("region", { name: lodash.name })).findByRole("button", { name: "Edit" }));
    fireEvent.click(screen.getByRole("button", { name: `Save ${lodash.name}` }));
    expect(await screen.findByText("ledger: Would pass")).toBeDefined();
    expect(sent()).toContain(`POST ${CUSTOM}/${dry.slug}/dry-run`);
  });

  it("links each rule to its history on the audit trail", async () => {
    server(withCustoms);
    render(<Policies />);
    await screen.findByRole("region", { name: email.name });
    expect(card(email.name).getByRole("link", { name: "History" }).getAttribute("href")).toBe(`#/audit?rule=${encodeURIComponent(email.rule)}`);
  });

  it("refuses a new rule whose id is one already written, and sends nothing", async () => {
    const sent = server(withCustoms);
    render(<Policies />);
    fireEvent.click(await screen.findByRole("button", { name: "New rule" }));
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Customer email: encrypted!" } }); // custom.customer-email-encrypted, the golden's
    fireEvent.change(screen.getByLabelText("What should Canyon check?"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Save the new rule" }));
    expect(await screen.findByText("A rule with this id already exists: edit that one instead.")).toBeDefined();
    expect(sent().filter((one) => one.startsWith("PUT") && !one.includes("/policies/"))).toHaveLength(0); // the rows' previews of today aside
  });
});

describe("slugOf", () => {
  it("is the name in lowercase letters and hyphens, a letter first, at most 40", () => {
    expect(slugOf("Customer emails: encrypted!")).toBe("customer-emails-encrypted");
    expect(slugOf("123")).toBe("");
    expect(slugOf("Секреты")).toBe("");
    expect(slugOf("2fa required on admin")).toBe("fa-required-on-admin");
    expect(slugOf("a".repeat(39) + " b c")).toBe("a".repeat(39));
  });
});

// Who approves a GitHub push's blocking finding (#2342): a row below the policies, Off by default, saved
// as it is changed; with sign-in off, where nobody links an account, it can't be turned on.
describe("Approvers of a push must link GitHub", () => {
  const PREFS = "/api/v1/preferences";
  it("reads the org's answer and saves a change at once", async () => {
    const sent = server({ ...routes, [`GET ${PREFS}`]: { body: { push_approvers_linked: false } }, "undefined /auth/config": { body: { org: "Meridian", auth: "oidc" } },
      [`PUT ${PREFS}`]: { body: { push_approvers_linked: true } } });
    render(<Policies />);
    const control = within(await within(await screen.findByRole("region", { name: S.pushApproversTitle })).findByRole("radiogroup", { name: S.pushApproversTitle }));
    expect((control.getByRole("radio", { name: "Off" }) as HTMLInputElement).checked).toBe(true);
    fireEvent.click(control.getByRole("radio", { name: "On" }));
    await waitFor(() => expect((control.getByRole("radio", { name: "On" }) as HTMLInputElement).checked).toBe(true));
    expect(sent()).toContain(`PUT ${PREFS} {"push_approvers_linked":true}`);
  });

  it("can't be turned on with sign-in off", async () => {
    server({ ...routes, [`GET ${PREFS}`]: { body: { push_approvers_linked: false } }, "undefined /auth/config": { body: { org: "Meridian", auth: "off" } } });
    render(<Policies />);
    const region = within(await screen.findByRole("region", { name: S.pushApproversTitle }));
    expect(await region.findByText("Needs sign-in")).toBeDefined();
    expect(region.queryByRole("radiogroup")).toBeNull();
  });
});
