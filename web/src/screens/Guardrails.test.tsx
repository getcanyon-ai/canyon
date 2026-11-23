import { readFileSync } from "node:fs";
import { StrictMode } from "react";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { DesignSystemRow, Manifest, Preferences, SetupAnswer, SetupMessage } from "../api";
import { Guardrails } from "./Guardrails";
import { handed } from "./SkillSection";

// Every id, language, framework and row on this page comes from the golden at runtime; nothing here
// writes one down.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as {
  preferences: Preferences;
  preferences_member: Preferences;
  preferences_admin: Preferences;
  manifest: Manifest;
  design_systems: { design_systems: DesignSystemRow[] };
  preferences_put: { response: Preferences };
  design_system_create: { request: { name: string; namespace: string; registry_url: string; tokens_url: string }; response: { design_system: DesignSystemRow } };
  setup: { request: { messages: SetupMessage[] }; response: SetupAnswer };
  proposal: { request: { messages: SetupMessage[] }; response: SetupAnswer };
  setup_unavailable: { error: string };
};
const PREFERENCES = "/api/v1/preferences";
const SYSTEMS = "/api/v1/design-systems";
const SETUP = "/api/v1/guardrails/setup";
const MANIFEST = "/api/v1/skill/manifest";
const EGRESS = "/api/v1/egress";
// The sentences the server refuses with, as cmd/canyon-server writes them: routes_preferences.go
// (stacksRule), routes_design_systems.go (the 409) and routes_setup.go (messagesRule, the 502's
// line and setupBusy). The screen recognises none of them — it renders whatever body came back.
const STACKS_RULE = "stacks is at most 16 entries, one per language, each with 1 to 16 frameworks, every name 1 to 64 characters of plain text";
const EXISTS = "a design system with that namespace exists";
const MESSAGES_RULE = "messages is 1 to 20 entries, each a user or assistant line of 1 to 8192 characters, and at most 32 KiB in all";
const MODEL_ERROR = "the model could not be reached";
const BUSY = "the setup chat is busy; try again in a moment";

// The server answers "<METHOD> <url>" from a table of plain objects and 404s anything else; the
// returned reader lists every request as "<METHOD> <url>", with its body when it has one.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer | (() => Answer | Promise<Answer>)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found" } };
    const { status = 200, body } = typeof route === "function" ? await route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
// The host's via is canyon, as cli/manifest_test.go's host answers it, so the manifest carries its outbound line.
const reads = {
  [`GET ${PREFERENCES}`]: { body: golden.preferences }, [`GET ${SYSTEMS}`]: { body: golden.design_systems }, [`GET ${MANIFEST}`]: { body: golden.manifest },
  [`GET ${EGRESS}`]: { body: { via: "canyon", mode: "observe" } },
};
// The registry as it stood before that row was registered, and an org with no default: a POST's
// answer is a row the picker does not carry yet.
const created = golden.design_system_create.response.design_system;
const fresh = {
  [`GET ${PREFERENCES}`]: { body: { ...golden.preferences, design_system: null } },
  [`GET ${SYSTEMS}`]: { body: { design_systems: golden.design_systems.design_systems.filter((row) => row.id !== created.id) } },
  [`GET ${MANIFEST}`]: { body: { ...golden.manifest, design_system: null } },
};
const made = { name: created.name, namespace: created.namespace, registry_url: created.registry_url };

const region = (name: string) => within(screen.getByRole("region", { name }));
// Every assertion about the manifest goes through this scope: its labels are the same words as two
// of the form's headings, and its URLs are the same strings as the form's.
const skill = () => region("What agents are told");
const states = () => skill().getAllByText(/^(blocking|advisory)$/);
const picker = () => screen.getByLabelText("Default design system") as HTMLSelectElement;
const options = () => within(picker()).getAllByRole("option").map((one) => one.textContent);
const value = (scope: ReturnType<typeof region>, label: string) => (scope.getByLabelText(label) as HTMLInputElement).value;
const loaded = async () => { render(<Guardrails />); await screen.findByRole("button", { name: "Save" }); };
const fill = (body: { name: string; namespace: string; registry_url: string; tokens_url?: string }) => {
  fireEvent.change(screen.getByLabelText("Name"), { target: { value: body.name } });
  fireEvent.change(screen.getByLabelText("Namespace"), { target: { value: body.namespace } });
  fireEvent.change(screen.getByLabelText("Registry URL"), { target: { value: body.registry_url } });
  if (body.tokens_url !== undefined) fireEvent.change(screen.getByLabelText("Tokens URL (optional)"), { target: { value: body.tokens_url } });
};
const register = () => fireEvent.click(screen.getByRole("button", { name: "Add a design system" }));
// One answer per call, in order; past the last one the server is broken, which no test relies on.
const queue = (...answers: Answer[]) => () => answers.shift() ?? { status: 500, body: { error: "internal" } };
const posted = (sent: () => string[]) => sent().filter((one) => one.startsWith(`POST ${SETUP} `))
  .map((one) => JSON.parse(one.slice(`POST ${SETUP} `.length)) as { messages: SetupMessage[] });
const say = (line: string) => {
  fireEvent.change(screen.getByLabelText("Your message"), { target: { value: line } });
  fireEvent.click(screen.getByRole("button", { name: "Send" }));
};
const wrote = (write: { tool: string; before: unknown; after: unknown }) =>
  `wrote: ${write.tool} — ${JSON.stringify(write.before)} → ${JSON.stringify(write.after)}`;
// A turn is over when the disabled Send stops saying it is working: that reason is the working line.
const settled = () => waitFor(() => expect(screen.queryByText("Canyon is working on it.")).toBeNull());

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("the guardrails screen", () => {
  // The stacks, the design system and what agents are told are the third tab's.
  beforeEach(() => { window.location.hash = "#/guardrails/agents"; });
  afterEach(() => { window.location.hash = ""; });
  it("a bound the server keeps is the server's refusal, not the form's", async () => {
    const sent = server({ ...reads, [`PUT ${PREFERENCES}`]: { status: 400, body: { error: "invalid_stacks", message: STACKS_RULE } } });
    await loaded();
    const long = "R".repeat(65);
    fireEvent.change(region("Stack 1").getByLabelText("Framework 1"), { target: { value: long } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(STACKS_RULE)).toBeDefined();
    expect(sent().filter((request) => request.startsWith("PUT")).join("")).toContain(long);
    expect(value(region("Stack 1"), "Framework 1")).toBe(long);
    expect(screen.getAllByRole("textbox").filter((one) => one.hasAttribute("maxlength"))).toHaveLength(0);
    const before = screen.getAllByRole("region", { name: /^Stack / }).length;
    const add = screen.getByRole("button", { name: "Add a stack" }); // one role query, not fifteen over a growing form: it halves the test, which a loaded machine ran past its timeout (#2189)
    for (let i = 0; i < 15; i += 1) fireEvent.click(add);
    expect(screen.getAllByRole("region", { name: /^Stack / })).toHaveLength(before + 15);
    expect(screen.getByRole("button", { name: "Add a stack" }).hasAttribute("disabled")).toBe(false);
  });

  it("a save shows what the server stored, not what was typed", async () => {
    server({ ...reads, [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response } });
    await loaded();
    fireEvent.change(region("Stack 1").getByLabelText("Language"), { target: { value: "Elixir" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved.")).toBeDefined();
    const stored = golden.preferences_put.response;
    stored.stacks.forEach((stack, i) => {
      const row = region(`Stack ${i + 1}`);
      expect(value(row, "Language")).toBe(stack.language);
      stack.frameworks.forEach((framework, j) => expect(value(row, `Framework ${j + 1}`)).toBe(framework));
    });
    expect(picker().value).toBe(stored.design_system?.id);
    fireEvent.change(region("Stack 1").getByLabelText("Language"), { target: { value: "Go" } });
    expect(screen.queryByText("Saved.")).toBeNull(); // the record moved on; the word does not linger
  });

  it("a refusal is the server's words", async () => {
    server({ ...reads, [`POST ${SYSTEMS}`]: { status: 409, body: { error: "exists", message: EXISTS } } });
    await loaded();
    const before = options().length;
    register();
    expect(screen.getAllByRole("textbox").filter((one) => one.hasAttribute("maxlength"))).toHaveLength(0); // the registration form too
    fill(golden.design_system_create.request);
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    expect(await screen.findByText(EXISTS)).toBeDefined();
    expect(screen.getByLabelText("Name")).toBeDefined();
    expect(options()).toHaveLength(before);
  });

  it("the body says absent and null apart", async () => {
    const sent = server({ ...reads, [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response } });
    await loaded();
    fireEvent.change(picker(), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    expect(sent()).toContain(`PUT ${PREFERENCES} ${JSON.stringify({ stacks: golden.preferences.stacks, approved_buildpacks: golden.preferences.approved_buildpacks, design_system_id: null })}`);
    cleanup();
    const posted = server({ ...fresh, [`POST ${SYSTEMS}`]: { body: golden.design_system_create.response } });
    await loaded();
    register();
    fill(made); // no tokens URL typed: the key is not in the body at all
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
    expect(posted()).toContain(`POST ${SYSTEMS} ${JSON.stringify(made)}`);
  });

  it("the picker offers the registry the server answered, in its order", async () => {
    server(reads);
    await loaded();
    expect(options()).toEqual(["None", ...golden.design_systems.design_systems.map((row) => `${row.name} · ${row.namespace}`)]);
    expect(picker().value).toBe(golden.preferences.design_system?.id);
  });

  it("says what the review does with the design system, which issue 2004 found untrue", async () => {
    server(reads);
    await loaded();
    const section = region("Design system");
    expect(section.getByText(/Each review checks that an app's package\.json lists it \(its components\.json, for shadcn\/ui and Canyon\)/)).toBeDefined();
    expect(section.queryByText(/flagged, never blocked/)).toBeNull();
  });

  it("the URLs shown are the record's, not the picker's draft", async () => {
    server(reads);
    await loaded();
    const other = golden.design_systems.design_systems.find((row) => row.id !== golden.preferences.design_system?.id);
    fireEvent.change(picker(), { target: { value: other?.id } });
    expect(picker().value).toBe(other?.id);
    // Scoped to the form's own section: the skill section below renders the same URL, and this test
    // was always about what the picker's draft does to the form.
    expect(region("Design system").getByText(golden.preferences.design_system?.registry_url ?? "", { exact: false })).toBeDefined();
    expect(region("Design system").queryByText(other?.registry_url ?? "", { exact: false })).toBeNull();
  });

  it("a registered system is selectable at once and the org's only after Save", async () => {
    const sent = server({ ...fresh, [`POST ${SYSTEMS}`]: { body: golden.design_system_create.response }, [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response } });
    await loaded();
    register();
    fill(golden.design_system_create.request);
    fireEvent.click(screen.getByRole("button", { name: "Register" }));
    await waitFor(() => expect(screen.queryByLabelText("Name")).toBeNull());
    expect(options()).toContain(`${created.name} · ${created.namespace}`);
    expect(picker().value).toBe(created.id);
    expect(sent().filter((request) => request.startsWith("PUT"))).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    expect(sent()).toContain(`PUT ${PREFERENCES} ${JSON.stringify({ stacks: golden.preferences.stacks, approved_buildpacks: golden.preferences.approved_buildpacks, design_system_id: created.id })}`);
  });

  it("removing a stack renumbers the ones after it", async () => {
    server(reads);
    await loaded();
    const before = screen.getAllByRole("region", { name: /^Stack / }).length;
    fireEvent.click(screen.getByRole("button", { name: "Add a stack" }));
    fireEvent.change(region(`Stack ${before + 1}`).getByLabelText("Language"), { target: { value: "Go" } });
    fireEvent.click(region("Stack 1").getByRole("button", { name: "Remove stack" }));
    expect(screen.queryByRole("region", { name: `Stack ${before + 1}` })).toBeNull();
    expect(value(region(`Stack ${before}`), "Language")).toBe("Go");
  });

  // Whether AI review is on is Settings' now (Settings › AI review); Guardrails is only the rules.
  it("says nothing about the model, and draws none of the organisation's forms", async () => {
    server(reads);
    await loaded();
    expect(screen.queryByText(/model/)).toBeNull();
    expect(screen.queryByLabelText("Organisation name")).toBeNull();
    expect(screen.queryByRole("region", { name: "Who may sign in" })).toBeNull();
  });

  // The server's own words, as a re-read after a turn says them: one failing read, one sentence.
  it("a read that fails says so in the server's words and offers no form", async () => {
    server({ ...reads, [`GET ${PREFERENCES}`]: { status: 500, body: { error: "internal", message: "internal error" } } });
    render(<Guardrails />);
    expect(await screen.findByText("internal error")).toBeDefined();
    expect(screen.queryByText(`GET ${PREFERENCES} answered 500`)).toBeNull();
    expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  });

  it("Retry re-reads once, and the failed line clears", async () => {
    const sent = server({
      ...reads,
      [`GET ${PREFERENCES}`]: queue({ status: 500, body: { error: "internal", message: "internal error" } }, { body: golden.preferences }),
    });
    render(<Guardrails />);
    expect(await screen.findByText("internal error")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await screen.findByRole("button", { name: "Save" });
    expect(screen.queryByText("internal error")).toBeNull();
    expect(value(region("Stack 1"), "Language")).toBe(golden.preferences.stacks[0]?.language);
    expect(sent().filter((one) => one.startsWith(`GET ${PREFERENCES}`))).toHaveLength(2);
  });

  it("the screen reads four times and then leaves the server alone", async () => {
    vi.useFakeTimers();
    const sent = server(reads);
    render(<Guardrails />);
    await vi.waitFor(() => expect(screen.getByRole("button", { name: "Save" })).toBeDefined());
    expect(sent()).toHaveLength(4); // the form's three and the host's via: the policies are another tab's
    await vi.advanceTimersByTimeAsync(10_000);
    await act(async () => {});
    expect(sent()).toHaveLength(4);
  });

  it("a write is echoed and the form re-reads the record", async () => {
    const sent = server({
      ...reads,
      [`GET ${PREFERENCES}`]: queue({ body: { ...golden.preferences, stacks: [], design_system: null } }, { body: golden.preferences }),
      [`POST ${SETUP}`]: { body: golden.setup.response },
    });
    await loaded();
    say(golden.setup.request.messages[0]?.content ?? "");
    expect(await screen.findByText(golden.setup.response.message)).toBeDefined();
    expect(screen.getAllByText(/^wrote: /)).toHaveLength(golden.setup.response.writes.length);
    for (const write of golden.setup.response.writes) expect(screen.getByText(wrote(write))).toBeDefined();
    // The panel echoed the writes; the record on screen is the one the re-read brought back.
    await waitFor(() => expect(value(region("Stack 1"), "Language")).toBe(golden.preferences.stacks[0]?.language));
    expect(picker().value).toBe(golden.preferences.design_system?.id);
    expect(sent().filter((one) => one.startsWith(`GET ${PREFERENCES}`))).toHaveLength(2);
    expect(sent()).toHaveLength(7); // the mount's four, the POST, and the turn's two re-reads
  });

  it("the conversation goes back whole, and only as messages", async () => {
    const sent = server({ ...reads, [`POST ${SETUP}`]: queue({ body: golden.setup.response }, { body: golden.proposal.response }) });
    await loaded();
    say(golden.proposal.request.messages[0]?.content ?? "");
    await screen.findByText(golden.setup.response.message);
    await settled(); // the answer shows before the turn's re-read lands, and Send stays disabled until it does
    say(golden.proposal.request.messages[2]?.content ?? "");
    await screen.findByText(golden.proposal.response.message);
    await settled();
    // The golden's two-turn conversation is exactly what the panel resends: the assistant's line is
    // in it, and the writes it echoed on screen are not.
    expect(JSON.stringify(posted(sent)[1])).toBe(JSON.stringify(golden.proposal.request));
    const long = "N".repeat(9000);
    say(long);
    await settled();
    expect(posted(sent)[2]?.messages.at(-1)?.content).toBe(long);
    expect(screen.getByText(long).className).toContain("break-words"); // an unbroken line wraps, never scrolls the page
  });

  it("a chat turn clears the form's stale refusal", async () => {
    server({
      ...reads,
      [`PUT ${PREFERENCES}`]: { status: 400, body: { error: "invalid_stacks", message: STACKS_RULE } },
      [`POST ${SETUP}`]: { body: golden.setup.response },
    });
    await loaded();
    fireEvent.change(region("Stack 1").getByLabelText("Framework 1"), { target: { value: "R".repeat(65) } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText(STACKS_RULE);
    say(golden.setup.request.messages[0]?.content ?? "");
    await settled();
    expect(screen.queryByText(STACKS_RULE)).toBeNull(); // it described input the re-read replaced
  });

  it("Send stays working until the form is current", async () => {
    let release: (answer: Answer) => void = () => undefined;
    const held = new Promise<Answer>((resolve) => { release = resolve; });
    let calls = 0;
    const sent = server({
      ...reads,
      [`GET ${PREFERENCES}`]: () => (calls++ === 0 ? { body: golden.preferences } : held),
      [`POST ${SETUP}`]: { body: golden.setup.response },
    });
    await loaded();
    say(golden.setup.request.messages[0]?.content ?? "");
    await screen.findByText(golden.setup.response.message);
    expect(screen.getByText("Canyon is working on it.")).toBeDefined(); // the answer is in; the re-read is not
    await act(async () => { release({ body: golden.preferences }); });
    await settled();
    expect(sent().filter((one) => one.startsWith(`GET ${PREFERENCES}`))).toHaveLength(2);
  });

  it("an empty box does not send", async () => {
    const sent = server({ ...reads, [`POST ${SETUP}`]: { body: golden.setup.response } });
    await loaded();
    expect(screen.getByText("Type something first.")).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    expect(posted(sent)).toHaveLength(0);
  });

  it("a refusal is the server's words, and the conversation survives it", async () => {
    const sent = server({
      ...reads,
      // The row moved under the failed turn: a 502 can arrive with writes already committed.
      [`GET ${PREFERENCES}`]: queue({ body: golden.preferences }, { body: { ...golden.preferences, stacks: [] } }, { body: golden.preferences }),
      [`POST ${SETUP}`]: queue({ status: 502, body: { error: "model_error", message: MODEL_ERROR } }, { status: 429, body: { error: "busy", message: BUSY } }),
    });
    await loaded();
    const first = golden.proposal.request.messages[0]?.content ?? "";
    const third = golden.proposal.request.messages[2]?.content ?? "";
    say(first);
    expect(await screen.findByText(MODEL_ERROR)).toBeDefined();
    expect(screen.getByRole("status").textContent).toBe(MODEL_ERROR); // assistive tech hears the refusal
    await settled();
    expect(screen.queryByRole("region", { name: "Stack 1" })).toBeNull(); // the form shows what the failed turn left
    say(third);
    expect(await screen.findByText(BUSY)).toBeDefined();
    expect(screen.getByText(first)).toBeDefined();
    expect(screen.getByText(third)).toBeDefined();
    expect(posted(sent)[1]?.messages.map((one) => one.content)).toEqual([first, third]);
    // Both turns failed and both re-read: a turn that failed can still have written.
    expect(sent().filter((one) => one.startsWith(`GET ${PREFERENCES}`))).toHaveLength(3);
  });

  it("the panel keeps no count, and the twenty-first message is the server's refusal", async () => {
    const answers: Answer[] = Array.from({ length: 10 }, () => ({ body: golden.setup.response }));
    const sent = server({ ...reads, [`POST ${SETUP}`]: queue(...answers, { status: 400, body: { error: "invalid_messages", message: MESSAGES_RULE } }) });
    await loaded();
    const line = golden.setup.request.messages[0]?.content ?? "";
    for (let i = 0; i < 11; i += 1) {
      say(line);
      await settled();
    }
    expect(posted(sent)).toHaveLength(11);
    expect(posted(sent)[10]?.messages).toHaveLength(21);
    expect(screen.getByText(MESSAGES_RULE)).toBeDefined();
    expect(screen.getAllByRole("textbox").filter((one) => one.hasAttribute("maxlength"))).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Start over" }));
    expect(screen.queryByText(MESSAGES_RULE)).toBeNull();
    say(line);
    await settled();
    expect(posted(sent)[11]?.messages).toHaveLength(1);
  });

  it("a proposal is a card, and only its Set click applies it, exactly as proposed", async () => {
    const card = golden.proposal.response.proposal!;
    const APPLY = `/api/v1/policies/${card.rule}?apply=true`;
    const sent = server({
      ...reads,
      [`POST ${SETUP}`]: { body: golden.proposal.response },
      [`PUT ${APPLY}`]: { body: { policy: { id: card.rule, name: card.rule, state: card.state }, would_block: card.would_block } },
    });
    await loaded();
    say(golden.proposal.request.messages[0]?.content ?? "");
    await screen.findByText(golden.proposal.response.message);
    expect(screen.getByText(`${card.rule} → ${card.state} after ${card.grace_days} days`)).toBeDefined();
    expect(screen.getByText("Would block today")).toBeDefined();
    for (const row of card.would_block) {
      expect(screen.getByText(`${row.project} — ${row.findings} finding${row.findings === 1 ? "" : "s"}`)).toBeDefined();
    }
    expect(screen.queryAllByText(/^wrote: /)).toHaveLength(0);
    // Showing the card writes nothing; one button, the proposal's own state and grace.
    expect(sent().filter((one) => one.startsWith("PUT "))).toHaveLength(0);
    expect(screen.queryByRole("button", { name: "Set to Blocking" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: `Set to Blocking in ${card.grace_days} days` }));
    expect((await screen.findByRole("link", { name: "See it in Policies" })).getAttribute("href")).toBe("#/guardrails/policies");
    const writes = sent().filter((one) => one.startsWith(`PUT ${APPLY} `));
    expect(writes).toHaveLength(1);
    expect(JSON.parse(writes[0]!.slice(`PUT ${APPLY} `.length))).toEqual({ state: card.state, grace_days: card.grace_days });
  });

  it("no model, no panel", async () => {
    const sent = server({
      ...reads,
      [`GET ${PREFERENCES}`]: { body: { ...golden.preferences, setup_available: false } },
      [`POST ${SETUP}`]: { status: 404, body: golden.setup_unavailable },
    });
    await loaded();
    expect(screen.queryByLabelText("Your message")).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up with Canyon" })).toBeNull();
    expect(value(region("Stack 1"), "Language")).toBe(golden.preferences.stacks[0]?.language);
    expect(screen.getByRole("button", { name: "Add a stack" })).toBeDefined();
    expect(posted(sent)).toHaveLength(0);
  });

  it("the empty state offers the chat and the form, and only when there is nothing", async () => {
    server({ ...reads, [`GET ${PREFERENCES}`]: { body: { ...golden.preferences, stacks: [], design_system: null } } });
    await loaded();
    expect(screen.getByText("Nothing configured yet.")).toBeDefined();
    expect(screen.getAllByRole("button", { name: "Add a stack" })).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "Set up with Canyon" }));
    expect(document.activeElement).toBe(screen.getByLabelText("Your message"));
    cleanup();
    // A design system and no stacks is not nothing: the line goes, and the form says the rest.
    server({ ...reads, [`GET ${PREFERENCES}`]: { body: { ...golden.preferences, stacks: [] } } });
    await loaded();
    expect(screen.queryByText("Nothing configured yet.")).toBeNull();
    expect(screen.queryByRole("button", { name: "Set up with Canyon" })).toBeNull();
    expect(screen.getAllByRole("button", { name: "Add a stack" })).toHaveLength(1);
  });

  // Guardrails is an admin's screen (design/specs/screens.md, A4b): a member who opens it is told so and
  // sent back, and shown nothing of the form, read-only or otherwise.
  it("a member is shown the admins-only page, never the form", async () => {
    server({ ...reads, [`GET ${PREFERENCES}`]: { body: golden.preferences_member } });
    render(<Guardrails />);
    expect(await screen.findByText("Only an admin can open Guardrails.")).toBeDefined();
    expect(screen.getByRole("heading", { level: 1, name: "Guardrails" })).toBeDefined();
    expect(screen.getByText("What an app must pass is on that app's page.")).toBeDefined();
    expect(screen.getByRole("link", { name: "Back to the fleet" }).getAttribute("href")).toBe("#/fleet");
    expect(screen.queryByRole("navigation", { name: "Guardrails" })).toBeNull();
    const page = within(screen.getByRole("main"));
    expect(page.queryAllByRole("region")).toHaveLength(0);
    expect(page.queryAllByRole("button")).toHaveLength(0);
  });

  // What "Approved stacks only" approves: every buildpack an app's deploy ran, each by its builder's
  // name or else its id, ticked when approved; a tick is saved with the form, sorted by id.
  it("an admin approves the buildpacks apps build with", async () => {
    const admin = golden.preferences_admin;
    const sent = server({ ...reads, [`GET ${PREFERENCES}`]: { body: admin }, [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response } });
    await loaded();
    const boxes = within(screen.getByRole("region", { name: "Approved buildpacks" })).getAllByRole("checkbox") as HTMLInputElement[];
    expect(boxes.map((box) => [box.closest("label")?.textContent, box.checked])).toEqual(
      admin.buildpacks_seen.map((bp) => [bp.name ?? bp.id, admin.approved_buildpacks.includes(bp.id)]));
    fireEvent.click(screen.getByRole("checkbox", { name: "paketo-buildpacks/pip" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    const put = sent().find((one) => one.startsWith(`PUT ${PREFERENCES} `));
    expect(JSON.parse(put!.slice(`PUT ${PREFERENCES} `.length)).approved_buildpacks).toEqual([...admin.approved_buildpacks, "paketo-buildpacks/pip"].sort());
  });

  it("the section is the manifest the server answered, field by field", async () => {
    server(reads);
    await loaded();
    const handed = golden.manifest;
    expect(skill().getByText(handed.host)).toBeDefined();
    expect(states()).toHaveLength(handed.policies.length);
    for (const one of handed.policies) {
      expect(skill().getByText(one.name)).toBeDefined();
      expect(skill().getByText(one.id)).toBeDefined();
    }
    expect(skill().getByText(handed.component_types.join(" · "))).toBeDefined();
    for (const one of handed.stacks) {
      expect(skill().getByText(one.language)).toBeDefined();
      expect(skill().getByText(one.frameworks.join(" · "))).toBeDefined();
    }
    expect(skill().getByText(handed.design_system?.name ?? "")).toBeDefined();
    expect(skill().getByText(handed.design_system?.namespace ?? "")).toBeDefined();
    expect(skill().getByText(handed.design_system?.registry_url ?? "")).toBeDefined();
    expect(skill().getByText(handed.design_system?.tokens_url ?? "")).toBeDefined();
    // Read-only, and nothing on it is built into somewhere to go: the host is server configuration,
    // not a destination.
    expect(skill().queryByRole("link")).toBeNull();
    expect(skill().queryByRole("textbox")).toBeNull();
  });

  it("the section says how big it is, counted the way an agent is handed it", async () => {
    server(reads);
    await loaded();
    // The fixture as §7.4 renders it for an agent, cli/testdata/manifest-agent.txt without its last
    // newline: tokens as characters over four. Recorded, not recomputed: a wrong count is meant to fail here.
    expect(skill().getByText("49 lines · ~742 tokens")).toBeDefined();
    // Where production is promoted is always said, so an agent never guesses whether a person approves it.
    expect(skill().getByText("Promoted from the CLI or the dashboard")).toBeDefined();
    expect(skill().queryByRole("status")).toBeNull();
  });

  it("the text it counts is the text canyon manifest hands an agent, how to pass each rule that blocks among it", () => {
    // The CLI's golden for the same fixture (cli/testdata/manifest-agent.txt): one source of words, the
    // server's, rendered the same way by both, so what an admin reads is what an agent reads (#2622).
    const agent = readFileSync(join(import.meta.dirname, "../../../cli/testdata/manifest-agent.txt"), "utf8");
    expect(handed(golden.manifest, true)).toBe(agent.replace(/\n$/, ""));
  });

  it("says outbound only where the host's via is canyon, as canyon manifest does", async () => {
    // Where apps call out directly Canyon refuses nothing, and a via read that fails says nothing either (cli.md §7.4).
    const line = /^Outbound: an app's environment may be under an allowlist, /m;
    expect(handed(golden.manifest, true)).toMatch(line);
    expect(handed(golden.manifest, false)).not.toMatch(line);
    for (const egress of [{ body: { via: "off", mode: "allowlist" } }, { status: 500, body: { error: "internal" } }]) {
      cleanup();
      server({ ...reads, [`GET ${EGRESS}`]: egress });
      await loaded();
      expect(skill().getByText("47 lines · ~633 tokens")).toBeDefined(); // the fixture's text without the line
    }
  });

  it("says an AI twin's policy once, as canyon manifest does, case by case", () => {
    // The CLI's own table (cli/testdata/manifest-twins.json), which cli/manifest_test.go reads too, so the
    // two renderings of the fold cannot drift apart (#2694).
    const table = JSON.parse(readFileSync(join(import.meta.dirname, "../../../cli/testdata/manifest-twins.json"), "utf8")) as {
      cases: { name: string; manifest: Pick<Parameters<typeof handed>[0], "host" | "policies" | "app_additions">; agent: string }[];
    };
    expect(table.cases.length).toBeGreaterThan(0);
    for (const one of table.cases) {
      const text = handed({ component_types: [], stacks: [], design_system: null, prod_approval: false, ...one.manifest });
      expect(text, one.name).toContain(`\nCanyon host: http://h\n\n${one.agent}\n\n`);
    }
  });

  it("an org's own rule that blocks is handed in full under its own heading, every line indented", () => {
    const own = { id: "custom.audit-log", name: "Every write is logged", state: "blocking" as const, instructions: "Log every write.\nSay who did it." };
    const text = handed({ ...golden.manifest, policies: [...golden.manifest.policies, own, { id: "custom.naming", name: "Tables are plural", state: "advisory" }] });
    expect(text).toContain("\n\nYour org's own rules (written by its admins):\n- Every write is logged\n  Log every write.\n  Say who did it.\n\n");
    expect(text).not.toContain("  To pass: undefined");
    expect(text.split("Tables are plural")).toHaveLength(2); // its name, once, under the rules that only warn
  });

  it("above five hundred lines it recommends, and refuses nothing", async () => {
    const many = Array.from({ length: 600 }, (_, i) => ({ ...golden.manifest.policies[0]!, id: `rule.${i}`, name: `Rule ${i}` }));
    server({ ...reads, [`GET ${MANIFEST}`]: { body: { ...golden.manifest, policies: many } } });
    await loaded();
    expect(skill().getByRole("status").textContent).toContain("Over 500 lines.");
    expect(states()).toHaveLength(600); // recommended, not trimmed
  });

  it("every rule the server sends is shown", async () => {
    server(reads);
    await loaded();
    // The client filters nothing. An off rule cannot arrive, so the only thing a filter on state
    // could do here is hide a rule the agent really is handed.
    expect(states()).toHaveLength(golden.manifest.policies.length);
    for (const one of golden.manifest.policies) expect(skill().getByText(one.name)).toBeDefined();
  });

  it("the section is the record, not the form's draft", async () => {
    server(reads);
    await loaded();
    fireEvent.change(region("Stack 1").getByLabelText("Language"), { target: { value: "Elixir" } });
    expect(skill().queryByText("Elixir")).toBeNull(); // nobody has saved that word
    expect(skill().getByText(golden.manifest.stacks[0]?.language ?? "")).toBeDefined();
  });

  it("a save re-reads what the agents are handed", async () => {
    const sent = server({
      ...reads,
      [`GET ${MANIFEST}`]: queue({ body: golden.manifest }, { body: { ...golden.manifest, stacks: [], design_system: null } }),
      [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response },
    });
    await loaded();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    expect(skill().getByText("No approved stacks yet.")).toBeDefined();
    expect(skill().getByText("No design system yet.")).toBeDefined();
    expect(sent().filter((one) => one.startsWith(`GET ${MANIFEST}`))).toHaveLength(2);
  });

  it("a setup turn re-reads it too", async () => {
    const sent = server({
      ...reads,
      [`GET ${PREFERENCES}`]: queue({ body: { ...golden.preferences, stacks: [] } }, { body: golden.preferences }),
      [`GET ${MANIFEST}`]: queue({ body: { ...golden.manifest, stacks: [] } }, { body: golden.manifest }),
      [`POST ${SETUP}`]: { body: golden.setup.response },
    });
    await loaded();
    expect(skill().getByText("No approved stacks yet.")).toBeDefined();
    say(golden.setup.request.messages[0]?.content ?? "");
    await settled();
    // The chat wrote the row: the form moved, and so did what the agents are handed.
    expect(skill().getByText(golden.manifest.stacks[0]?.language ?? "")).toBeDefined();
    expect(skill().queryByText("No approved stacks yet.")).toBeNull();
    expect(sent().filter((one) => one.startsWith(`GET ${MANIFEST}`))).toHaveLength(2);
  });

  it("the section is the manifest even where the form disagrees with it", async () => {
    // The form shows the golden's stacks and design system; the manifest says nothing is
    // configured, and the section says what the manifest says. The policy list is the host's, not
    // the org's, so it is handed over either way.
    server({ ...reads, [`GET ${MANIFEST}`]: { body: { ...golden.manifest, stacks: [], design_system: null } } });
    await loaded();
    expect(skill().getByText("No approved stacks yet.")).toBeDefined();
    expect(skill().getByText("No design system yet.")).toBeDefined();
    expect(states()).toHaveLength(golden.manifest.policies.length);
  });

  it("a save clears the failure of a read before it", async () => {
    const READ_FAILED = "the manifest could not be built";
    const sent = server({
      ...reads,
      [`GET ${MANIFEST}`]: queue({ body: golden.manifest }, { status: 500, body: { error: "internal", message: READ_FAILED } }, { body: golden.manifest }),
      [`PUT ${PREFERENCES}`]: { body: golden.preferences_put.response },
    });
    await loaded();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    // The save landed and the manifest read after it did not: both sentences, as the brief says.
    expect(screen.getByText(READ_FAILED)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.queryByText(READ_FAILED)).toBeNull());
    expect(sent().filter((one) => one.startsWith(`GET ${MANIFEST}`))).toHaveLength(3);
  });

  it("the form survives StrictMode's second mount", async () => {
    // Dev runs setup, cleanup, setup on one instance: the guard that stops reads landing on a gone
    // screen outlives the cleanup, and the second setup has to turn it back on.
    server(reads);
    render(<StrictMode><Guardrails /></StrictMode>);
    await screen.findByRole("button", { name: "Save" });
    expect(states()).toHaveLength(golden.manifest.policies.length);
  });
});

// Guardrails' five tabs: each is its own route, the bare one is Pipeline, a rule's link is the tab its
// row is on, and only the page shown is read.
describe("the guardrails tabs", () => {
  afterEach(() => { window.location.hash = ""; });
  const policies = { "GET /api/v1/policies": { body: { policies: [] } }, "GET /api/v1/custom-rules": { body: { custom_rules: [] } }, "GET /api/v1/playbooks": { body: { playbooks: [] } } };
  const current = () => screen.getByRole("navigation", { name: "Guardrails" }).querySelector("[aria-current=page]")?.textContent;

  it.each([
    ["#/guardrails", "Pipeline", "Pipeline"],
    ["#/guardrails/policies", "Policies", "Policies"],
    ["#/guardrails?rule=prod.clean-gate", "Policies", "Policies"], // the app's checklist and the receipt link here
    ["#/guardrails/scanners", "Scanners", "Scanners"],
    ["#/guardrails/playbooks", "Playbooks", "Playbooks"],
    ["#/guardrails/agents", "What agents are told", "Approved stacks"],
    ["#/guardrails?rule=sarif.trivy", "Scanners", "Scanners"], // a tool's rule lives on the Scanners tab
  ])("%s shows %s", async (hash, tab, section) => {
    window.location.hash = hash;
    server({ ...reads, ...policies });
    render(<Guardrails />);
    expect(await screen.findByRole("region", { name: section })).toBeDefined();
    expect(current()).toBe(tab);
    for (const other of ["Pipeline", "Policies", "Scanners", "Playbooks", "Approved stacks"].filter((one) => one !== section)) expect(screen.queryByRole("region", { name: other })).toBeNull();
  });
});
