import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Channel } from "../api";
import { S } from "../strings";
import { idFor, IntegrationsSettings } from "./Integrations";

const dana = { kind: "person" as const, subject: "sub-dana", display: "Dana Whitfield" };
const alerts: Channel = { id: "platform-alerts", name: "#platform-alerts", kind: "slack", host: "hooks.slack.com", tail: "a1B9", set_by: dana, set_at: new Date(Date.now() - 3_600_000).toISOString(), failing: null };
const risk: Channel = { ...alerts, id: "risk-team", name: "Risk team", kind: "teams", host: "prod-12.westeurope.logic.azure.com", tail: "Qx7Z", failing: { since: new Date(Date.now() - 2 * 3_600_000).toISOString(), status: 404 } };

// The server answers the list with channels, each write with write's answer, recording "METHOD url body".
function server(channels: Channel[], write: (method: string, url: string) => { status: number; body: unknown } = () => ({ status: 200, body: { channel: alerts } })) {
  const sent: string[] = [];
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? "GET";
    if (method === "GET") return { ok: true, status: 200, json: async () => ({ channels }) } as unknown as Response;
    sent.push(`${method} ${url} ${String(init?.body ?? "")}`.trim());
    const { status, body } = write(method, url);
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  }));
  return sent;
}
afterEach(() => vi.unstubAllGlobals());

// Settings › Integrations (design/INTEGRATIONS.md; #2725): the org's chat channels, by host and tail, never a URL.
describe("IntegrationsSettings", () => {
  it("lists each channel by host and tail, and says which one is failing", async () => {
    server([alerts, risk]);
    render(<IntegrationsSettings />);
    const [first, second] = within(await screen.findByRole("list")).getAllByRole("listitem");
    expect(first!.textContent).toContain("#platform-alerts · Slack");
    expect(first!.textContent).toContain("hooks.slack.com/…a1B9");
    expect(first!.textContent).not.toContain("Canyon stopped posting");
    expect(second!.textContent).toContain("Canyon stopped posting here 2 hours ago: the webhook URL answered 404, so it no longer works. Change it to a working one.");
    expect(screen.getByText(S.integrationsNote)).toBeDefined();
  });

  it("adds a channel under an id from its name, warning who will read it, and says the summary went", async () => {
    const sent = server([]);
    render(<IntegrationsSettings />);
    fireEvent.click(await screen.findByRole("button", { name: S.integrationsAdd }));
    const form = within(screen.getByRole("form", { name: S.integrationsAdd }));
    expect(form.getByText(S.integrationsEveryone)).toBeDefined();
    expect(form.getByText(S.integrationsSlackHow)).toBeDefined();
    fireEvent.click(form.getByRole("radio", { name: "Teams" }));
    expect(form.getByText(S.integrationsTeamsHow)).toBeDefined();
    fireEvent.click(form.getByRole("radio", { name: "Slack" }));
    fireEvent.change(form.getByLabelText("Name"), { target: { value: "#Platform Alerts" } });
    fireEvent.change(form.getByLabelText("Webhook URL"), { target: { value: "https://hooks.slack.com/services/T0/B0/XXXX" } });
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(S.integrationsSaved.replace("<channel>", "#platform-alerts"))).toBeDefined();
    expect(sent).toEqual([`PUT /api/v1/integrations/channels/platform-alerts {"name":"#Platform Alerts","kind":"slack","url":"https://hooks.slack.com/services/T0/B0/XXXX"}`]);
  });

  it("keeps the saved URL on a change that leaves it empty, and shows the server's refusal as it said it", async () => {
    const said = "Teams answered 404: that webhook URL doesn't exist or was removed. Nothing was saved.";
    const sent = server([risk], () => ({ status: 422, body: { error: "channel_unreachable", message: said, status: 404 } }));
    render(<IntegrationsSettings />);
    fireEvent.click(await screen.findByRole("button", { name: "Change Risk team" }));
    const form = within(screen.getByRole("form", { name: "Change Risk team" }));
    expect((form.getByLabelText("Webhook URL") as HTMLInputElement).placeholder).toBe("prod-12.westeurope.logic.azure.com/…Qx7Z");
    expect(form.getByText(`${S.integrationsTeamsHow} ${S.integrationsKeep}`)).toBeDefined();
    fireEvent.click(form.getByRole("button", { name: "Save" }));
    expect(await form.findByRole("alert")).toHaveProperty("textContent", said);
    expect(sent).toEqual(['PUT /api/v1/integrations/channels/risk-team {"name":"Risk team","kind":"teams"}']);
  });

  it("tests and removes a channel, saying what each did", async () => {
    const sent = server([alerts], () => ({ status: 204, body: null }));
    render(<IntegrationsSettings />);
    fireEvent.click(await screen.findByRole("button", { name: `${S.integrationsTest}: #platform-alerts` }));
    expect(await screen.findByText(S.integrationsTested.replace("<channel>", "#platform-alerts"))).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Remove #platform-alerts" }));
    expect(await screen.findByText(S.integrationsRemoved.replace("<channel>", "#platform-alerts"))).toBeDefined();
    expect(sent).toEqual(["POST /api/v1/integrations/channels/platform-alerts/test", "DELETE /api/v1/integrations/channels/platform-alerts"]);
  });

  it("offers no sixth channel", async () => {
    server([1, 2, 3, 4, 5].map((n) => ({ ...alerts, id: `ch-${n}`, name: `#ch-${n}` })));
    render(<IntegrationsSettings />);
    expect(await screen.findByText(S.integrationsFull)).toBeDefined();
    await waitFor(() => expect(screen.queryByRole("button", { name: S.integrationsAdd })).toBeNull());
  });
});

describe("idFor", () => {
  it("is the name in the id's alphabet, never one taken", () => {
    expect(idFor("#Platform Alerts", [])).toBe("platform-alerts");
    expect(idFor("#platform-alerts", ["platform-alerts"])).toBe("platform-alerts-2");
    expect(idFor("123 ops", [])).toBe("ops");
    expect(idFor("🚨", [])).toBe("channel");
    expect(idFor("x".repeat(80), [])).toMatch(/^x{36}$/);
  });
});
