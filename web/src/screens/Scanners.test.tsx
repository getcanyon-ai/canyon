import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Actor, Policy, ToolSettings } from "../api";
import { S } from "../strings";
import { Scanners } from "./Scanners";

// The contract's own scanner apply: its request is what the Trivy card sends, its answer the rule it stores.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/policies/meridian.json"), "utf8")) as {
  policies: { policies: Policy[] };
  scanner_apply: { rule: string; request: { state: string; run: boolean; settings: ToolSettings }; response: { policy: Policy; would_block: [] } };
};
const dana: Actor = { kind: "person", subject: "dana@meridian.example", display: "Dana Whitfield" };
const REPO = "https://github.com/meridian/rules";
const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const CREDENTIAL = "/api/v1/scanners/opengrep/credential";
const apply = (tool: string) => `/api/v1/policies/sarif.${tool}?apply=true`;
const opengrep: Policy = { id: "sarif.opengrep", name: "Findings from opengrep", state: "advisory", run: true, settings: { rules: { repo: REPO, commit: COMMIT } } };

type Answer = { status?: number; body?: unknown };
function server(routes: Record<string, Answer | Answer[]>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`];
    const { status = 200, body } = (Array.isArray(route) ? (route.length > 1 ? route.shift() : route[0]) : route) ?? { status: 404, body: { error: "not_found", message: "not found" } };
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const bodyOf = (sent: string[], path: string) => sent.filter((one) => one.startsWith(`PUT ${path} `)).map((one) => JSON.parse(one.slice(`PUT ${path} `.length)) as unknown);
const card = (name: string) => within(screen.getByRole("region", { name }));
// The tab reads its own rules: the host's policies, with the given scanner rules among them.
async function show(rules: Policy[], routes: Record<string, Answer | Answer[]> = {}) {
  const sent = server({ "GET /api/v1/policies": { body: { policies: rules } }, ...routes });
  const view = render(<Scanners />);
  await screen.findByRole("region", { name: "Trivy" });
  return { sent, ...view };
}
const configure = (tool: string) => fireEvent.click(card(tool).getByRole("button", { name: "Configure" }));
const findings = (tool: string, state: string) => fireEvent.click(within(screen.getByRole("radiogroup", { name: `Findings from ${tool}` })).getByRole("radio", { name: state }));

afterEach(() => vi.unstubAllGlobals());

describe("Guardrails → Scanners", () => {
  it("adds a tool with what its findings do, whether Canyon runs it and its settings, sent once", async () => {
    const { sent } = await show([], { [`PUT ${apply("trivy")}`]: { body: golden.scanner_apply.response } });
    fireEvent.click(card("Trivy").getByRole("checkbox", { name: "Canyon runs Trivy" }));
    findings("Trivy", "Blocking");
    fireEvent.click(card("Trivy").getByRole("checkbox", { name: "Vulnerabilities" }));
    fireEvent.click(card("Trivy").getByRole("checkbox", { name: "Secrets" }));
    fireEvent.click(screen.getByRole("button", { name: "Save Trivy" }));
    fireEvent.click(screen.getByRole("button", { name: "Save Trivy" }));
    expect(await card("Trivy").findByText("Saved.")).toBeDefined();
    expect(bodyOf(sent(), apply("trivy"))).toEqual([golden.scanner_apply.request]);
    fireEvent.click(card("Trivy").getByRole("checkbox", { name: "Misconfiguration" }));
    expect(card("Trivy").queryByText("Saved.")).toBeNull(); // an edit after the save is not saved
    // The stored rule is the tool's now: the next save sends no state, so it no longer adds it.
    fireEvent.click(screen.getByRole("button", { name: "Save Trivy" }));
    await waitFor(() => expect(bodyOf(sent(), apply("trivy"))).toHaveLength(2));
    expect(bodyOf(sent(), apply("trivy"))[1]).not.toHaveProperty("state");
  });

  // An added tool keeps its settings folded behind Configure: its row is the tool and its one control.
  it("folds an added tool's settings behind Configure", async () => {
    await show([{ id: "sarif.grype", name: "Findings from grype", state: "advisory", run: true }]);
    expect(card("Grype").queryByRole("checkbox", { name: "Canyon runs Grype" })).toBeNull();
    expect(card("Grype").getByRole("button", { name: "Configure" }).getAttribute("aria-expanded")).toBe("false");
    configure("Grype");
    expect(card("Grype").getByRole("button", { name: "Configure" }).getAttribute("aria-expanded")).toBe("true");
    expect((card("Grype").getByRole("checkbox", { name: "Canyon runs Grype" }) as HTMLInputElement).checked).toBe(true);
    expect(card("Trivy").getByRole("checkbox", { name: "Canyon runs Trivy" })).toBeDefined(); // one not added opens on its settings
  });

  // An added tool's findings change as a policy's do (PolicyChange.tsx): previewed with the apps they
  // would block, then saved from the save bar, tightening now or in 14 days; its own Save then sends
  // run and settings alone, {} clearing them.
  it("changes an added tool's findings through the save bar and the grace, and clears its settings apart", async () => {
    const rule: Policy = { id: "sarif.grype", name: "Findings from grype", state: "advisory", run: true, settings: { skip_paths: ["vendor"] } };
    const later: Policy = { ...rule, scheduled: { state: "blocking", effective_at: "2026-10-10T00:00:00Z" } };
    const { sent } = await show([rule], {
      "PUT /api/v1/policies/sarif.grype": { body: { would_block: [{ project: "claims-portal", findings: 2 }] } },
      [`PUT ${apply("grype")}`]: [{ body: { policy: later, would_block: [] } }, { body: { policy: { ...later, run: undefined, settings: undefined }, would_block: [] } }],
    });
    findings("Grype", "Blocking");
    expect(await card("Grype").findByText("1 app could not promote today: claims-portal")).toBeDefined();
    const bar = within(screen.getByRole("region", { name: "Unsaved changes" }));
    expect(bar.getByText("Grype, Advisory → Blocking")).toBeDefined();
    expect(bodyOf(sent(), apply("grype"))).toEqual([]); // the preview writes nothing
    fireEvent.click(bar.getByRole("button", { name: "Save, tightening in 14 days" }));
    expect(await card("Grype").findByText(/^Becomes Blocking on /)).toBeDefined();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).toBeNull();
    configure("Grype");
    fireEvent.click(card("Grype").getByRole("checkbox", { name: "Canyon runs Grype" }));
    fireEvent.change(card("Grype").getByLabelText("Paths to skip (one per line)"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Grype" }));
    await card("Grype").findByText("Saved.");
    expect(bodyOf(sent(), apply("grype"))).toEqual([{ state: "blocking", grace_days: 14 }, { run: false, settings: {} }]);
  });

  it("keeps the state in force instead of a scheduled change, with no grace offered", async () => {
    const rule: Policy = { id: "sarif.checkov", name: "Findings from checkov", state: "advisory", scheduled: { state: "blocking", effective_at: "2026-10-07T08:30:00Z" } };
    const { sent } = await show([rule], {
      "PUT /api/v1/policies/sarif.checkov": { body: { would_block: [] } },
      [`PUT ${apply("checkov")}`]: { body: { policy: { ...rule, scheduled: undefined }, would_block: [] } },
    });
    fireEvent.click(card("Checkov").getByRole("button", { name: "Keep Advisory: Checkov" }));
    const bar = within(await screen.findByRole("region", { name: "Unsaved changes" }));
    expect(bar.getByText("Checkov, keep Advisory")).toBeDefined();
    expect(bar.queryByRole("button", { name: /in 14 days/ })).toBeNull();
    fireEvent.click(bar.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(card("Checkov").queryByText(/^Becomes /)).toBeNull());
    expect(bodyOf(sent(), apply("checkov"))).toEqual([{ state: "advisory" }]);
  });

  it("names Opengrep's rules repo at a commit, beside Canyon's bundled rules", async () => {
    const { sent } = await show([], { [`PUT ${apply("opengrep")}`]: { body: { policy: opengrep, would_block: [] } }, [`GET ${CREDENTIAL}`]: { body: { credential: null } } });
    expect(card("Opengrep").getByText(S.bundledRules)).toBeDefined();
    fireEvent.change(card("Opengrep").getByLabelText("Rules repo"), { target: { value: REPO } });
    fireEvent.change(card("Opengrep").getByLabelText("Commit"), { target: { value: COMMIT } });
    fireEvent.click(screen.getByRole("button", { name: "Save Opengrep" }));
    await waitFor(() => expect(bodyOf(sent(), apply("opengrep"))).toEqual([{ state: "off", run: false, settings: { rules: { repo: REPO, commit: COMMIT } } }]));
  });

  // ESLint is one of the tools Canyon runs, with its skipped paths and its run switch.
  it("gives ESLint a row of its own, with its run switch and paths to skip", async () => {
    await show([{ id: "sarif.eslint", name: "Findings from eslint", state: "advisory", run: true }]);
    configure("ESLint");
    expect((card("ESLint").getByRole("checkbox", { name: "Canyon runs ESLint" }) as HTMLInputElement).checked).toBe(true);
    expect(card("ESLint").getByLabelText("Paths to skip (one per line)")).toBeDefined();
  });

  // The rules folders go as the rules' paths, one per line; none leaves paths out, so every rules file is read.
  it("sends the rules folders as paths, and none as no paths", async () => {
    const { sent } = await show([], { [`PUT ${apply("opengrep")}`]: { body: { policy: opengrep, would_block: [] } }, [`GET ${CREDENTIAL}`]: { body: { credential: null } } });
    fireEvent.change(card("Opengrep").getByLabelText("Rules repo"), { target: { value: REPO } });
    fireEvent.change(card("Opengrep").getByLabelText("Commit"), { target: { value: COMMIT } });
    fireEvent.change(card("Opengrep").getByLabelText("Rules folders, one per line"), { target: { value: "ruby\n\nphp/lang\n" } });
    fireEvent.click(screen.getByRole("button", { name: "Save Opengrep" }));
    await waitFor(() => expect(bodyOf(sent(), apply("opengrep"))).toEqual([{ state: "off", run: false, settings: { rules: { repo: REPO, commit: COMMIT, paths: ["ruby", "php/lang"] } } }]));
  });

  // The community rules are a ready-made rules repo, never Canyon's: one click fills the repo and its
  // pinned commit, the licence note says whose decision it is, and a save sends them as any repo.
  it("offers the community rules for Ruby, PHP and Java as a rules repo, with their licence", async () => {
    const { sent } = await show([], { [`PUT ${apply("opengrep")}`]: { body: { policy: opengrep, would_block: [] } }, [`GET ${CREDENTIAL}`]: { body: { credential: null } } });
    expect(card("Opengrep").getByText(S.communityRules)).toBeDefined();
    fireEvent.click(card("Opengrep").getByRole("button", { name: "Use the community rules" }));
    fireEvent.click(screen.getByRole("button", { name: "Save Opengrep" }));
    await waitFor(() => expect(bodyOf(sent(), apply("opengrep"))).toEqual([{ state: "off", run: false, settings: {
      rules: { repo: "https://github.com/Software-Improvement-Group/opengrep-community-rules", commit: "fd85cc8cdb2156123607afbe51005463f692389a", paths: ["java", "php", "ruby"] } } }]));
  });

  it("warns under every tool's paths to skip that a pattern like ** blinds it", async () => {
    await show([]);
    expect(screen.getAllByText(S.skipBlinds)).toHaveLength(5);
  });

  it("a refusal is the server's own sentence, and Saved. does not show", async () => {
    const message = 'settings "scanners": a non-empty set of vuln, misconfig and secret';
    await show([], { [`PUT ${apply("grype")}`]: { status: 400, body: { error: "invalid_settings", message } } });
    fireEvent.click(screen.getByRole("button", { name: "Save Grype" }));
    expect(await card("Grype").findByText(message)).toBeDefined();
    expect(card("Grype").queryByText("Saved.")).toBeNull();
  });

  it("says when the rules can't be read", async () => {
    server({ "GET /api/v1/policies": { status: 500, body: { error: "internal", message: "policies could not be read" } } });
    render(<Scanners />);
    expect(await screen.findByText("policies could not be read")).toBeDefined();
    expect(screen.queryByRole("region", { name: "Trivy" })).toBeNull();
  });

});

describe("Opengrep's credential", () => {
  it("is set for the saved repo, said without the token, and removed", async () => {
    const set = { credential: { repo: REPO, set_at: "2026-09-25T08:00:00Z", set_by: dana } };
    const { sent } = await show([opengrep], {
      [`GET ${CREDENTIAL}`]: [{ body: { credential: null } }, { body: set }, { body: { credential: null } }],
      [`PUT ${CREDENTIAL}`]: { status: 204 },
      [`DELETE ${CREDENTIAL}`]: { status: 204 },
    });
    configure("Opengrep");
    expect(await card("Opengrep").findByText(S.credentialAsk)).toBeDefined();
    expect(card("Opengrep").getByLabelText("Token").getAttribute("type")).toBe("password");
    fireEvent.change(card("Opengrep").getByLabelText("Username"), { target: { value: "rules-bot" } });
    fireEvent.change(card("Opengrep").getByLabelText("Token"), { target: { value: "ghp_example" } });
    fireEvent.click(screen.getByRole("button", { name: "Save credential" }));
    expect(await card("Opengrep").findByText(`A credential is set for ${REPO}.`)).toBeDefined();
    expect(sent()).toContain(`PUT ${CREDENTIAL} ${JSON.stringify({ repo: REPO, username: "rules-bot", token: "ghp_example" })}`);
    expect(screen.queryByDisplayValue("ghp_example")).toBeNull();
    // The clicked button is gone; the control that replaced it has the keyboard.
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Remove the credential" })));
    fireEvent.click(screen.getByRole("button", { name: "Remove the credential" }));
    expect(await card("Opengrep").findByText(S.credentialAsk)).toBeDefined();
    expect(sent()).toContain(`DELETE ${CREDENTIAL}`);
    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText("Username")));
  });

  it("is not asked about while no rules repo is saved, and a refusal is the server's sentence", async () => {
    const routes = { [`GET ${CREDENTIAL}`]: { body: { credential: null } }, [`PUT ${CREDENTIAL}`]: { status: 400, body: { error: "invalid_credential", message: "a credential is a repo, a username and a token" } } };
    const { sent, unmount } = await show([], routes);
    expect(sent()).toEqual(["GET /api/v1/policies"]);
    unmount();
    await show([opengrep], routes);
    configure("Opengrep");
    fireEvent.change(await screen.findByLabelText("Username"), { target: { value: "rules-bot" } });
    fireEvent.click(screen.getByRole("button", { name: "Save credential" }));
    expect(await screen.findByText("a credential is a repo, a username and a token")).toBeDefined();
    // The form stays, with what was typed, so the person can correct it and try again.
    expect(screen.getByRole("button", { name: "Save credential" })).toBeDefined();
    expect(screen.getByDisplayValue("rules-bot")).toBeDefined();
  });
});
