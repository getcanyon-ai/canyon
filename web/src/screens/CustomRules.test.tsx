import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { CustomRule, PlaybookEntry, Policy } from "../api";
import { Policies } from "./Policies";

const read = (path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as unknown;
const golden = read("policies/meridian.json") as { policies: { policies: Policy[] }; custom_rules: { custom_rules: CustomRule[] } };
const { playbooks } = read("guardrails/preferences.json") as { playbooks: { playbooks: PlaybookEntry[] } };
const [email, pii] = golden.custom_rules.custom_rules as [CustomRule, CustomRule];

function server(routes: Record<string, unknown>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const body = routes[`${init?.method} ${url}`];
    return { ok: body !== undefined, status: body !== undefined ? 200 : 404, json: async () => body ?? { error: "not_found" } } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const routes = {
  "GET /api/v1/policies": { policies: [...golden.policies.policies, ...golden.custom_rules.custom_rules.map((one) => ({ id: one.rule, name: one.name, state: "off" }))] },
  "GET /api/v1/custom-rules": golden.custom_rules,
  "GET /api/v1/playbooks": playbooks,
};
const card = (name: string) => within(screen.getByRole("region", { name }));

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

// A custom rule may point at one of the org's playbooks, per version (contracts/routes.md, the custom-rules row): its
// card names it, the editor picks it from the playbooks that stand or none, and every save sends it whole, so editing
// a rule never unlinks its playbook unasked.
describe("A custom rule's playbook", () => {
  it("names the playbook a rule points at on its card, and none on a rule with none", async () => {
    server(routes);
    render(<Policies />);
    await screen.findByRole("region", { name: email.name });
    expect(card(email.name).getByText(`Its findings tell the agent to read the playbook ${email.playbook}.`)).toBeDefined();
    expect(card(pii.name).queryByText(/read the playbook/)).toBeNull();
  });

  it("keeps a rule's playbook on a save of its text, and lists the standing playbooks or none", async () => {
    const sent = server({ ...routes, [`PUT /api/v1/custom-rules/${email.rule.slice("custom.".length)}`]: { custom_rule: email } });
    render(<Policies />);
    fireEvent.click(within(await screen.findByRole("region", { name: email.name })).getByRole("button", { name: "Edit" }));
    const field = screen.getByLabelText("Playbook") as HTMLSelectElement;
    await waitFor(() => expect([...field.options].map((one) => one.text)).toEqual(["None", ...playbooks.playbooks.map((one) => one.name)]));
    expect(field.value).toBe(email.playbook);
    fireEvent.click(await screen.findByRole("button", { name: `Save ${email.name}` }));
    await waitFor(() => expect(sent()).toContain(`PUT /api/v1/custom-rules/customer-email-encrypted ${JSON.stringify({ name: email.name, instructions: email.instructions, playbook: email.playbook })}`));
  });

  it("points a rule at a playbook, and at none", async () => {
    const slug = pii.rule.slice("custom.".length);
    const sent = server({ ...routes, [`PUT /api/v1/custom-rules/${slug}`]: { custom_rule: pii } });
    render(<Policies />);
    fireEvent.click(within(await screen.findByRole("region", { name: pii.name })).getByRole("button", { name: "Edit" }));
    const save = await screen.findByRole("button", { name: `Save ${pii.name}` });
    fireEvent.change(screen.getByLabelText("Playbook"), { target: { value: "incident-notes" } });
    fireEvent.click(save);
    await waitFor(() => expect(sent()).toContain(`PUT /api/v1/custom-rules/${slug} ${JSON.stringify({ name: pii.name, instructions: pii.instructions, playbook: "incident-notes" })}`));
    const unlink = server({ ...routes, [`PUT /api/v1/custom-rules/customer-email-encrypted`]: { custom_rule: email } });
    cleanup();
    render(<Policies />);
    fireEvent.click(within(await screen.findByRole("region", { name: email.name })).getByRole("button", { name: "Edit" }));
    const again = await screen.findByRole("button", { name: `Save ${email.name}` });
    fireEvent.change(screen.getByLabelText("Playbook"), { target: { value: "" } });
    fireEvent.click(again);
    await waitFor(() => expect(unlink()).toContain(`PUT /api/v1/custom-rules/customer-email-encrypted ${JSON.stringify({ name: email.name, instructions: email.instructions, playbook: null })}`));
  });
});
