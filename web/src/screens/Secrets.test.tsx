import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { App as AppRead, FleetPage, Secret, Status } from "../api";
import { SECRETS as T } from "../strings";
import { RailContext } from "../ui/Rail";
import { App } from "./App";
import { Secrets } from "./Secrets";

const golden = <T,>(path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8")) as T;
// The contract's own list, so a change to its shape breaks this screen's test rather than passing it.
const listed = golden<{ secrets: Secret[] }>("secrets/claims-portal.json");
const provisioned: Secret = { name: "DATABASE_URL", env: "preview", where: "runtime", version: 1, set_by: { kind: "canyon", subject: "provisioner", display: "Canyon" }, set_at: "2026-09-03T08:00:00Z", origin: "provisioned" };
const LIST = "GET /api/v1/projects/claims-portal/secrets";
const VALUE = "sk_test_51Hplanted0000000000000000"; // a synthetic shape, never a real value

// The server answers "<METHOD> <url>" from a table and 404s anything else; the reader lists every request with its body.
type Answer = { status?: number; body: unknown };
function server(routes: Record<string, Answer | (() => Answer)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const route = routes[`${init?.method} ${url}`] ?? { status: 404, body: { error: "not_found", message: "not found" } };
    const { status = 200, body } = typeof route === "function" ? route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.map(([url, init]) => [`${init?.method} ${url}`, init?.body].filter(Boolean).join(" "));
}
const section = () => within(screen.getByRole("region", { name: T.title }));
const form = () => within(screen.getByRole("form", { name: T.form }));
const value = () => form().getByLabelText(T.value) as HTMLInputElement;
// The form opens from its button (#102).
const openForm = () => { if (screen.queryByRole("form", { name: T.form }) === null) fireEvent.click(section().getByRole("button", { name: T.form })); };
const fill = (name: string, typed: string) => {
  openForm();
  fireEvent.change(form().getByLabelText(T.name), { target: { value: name } });
  fireEvent.change(value(), { target: { value: typed } }); // the field holds what is typed in it, and nothing else does
};

afterEach(() => vi.unstubAllGlobals());

describe("the app page's Secrets section", () => {
  it("lists each secret by name, a row it wrote with Replace and Remove, and one Canyon set with neither", async () => {
    server({ [LIST]: { body: { secrets: [...listed.secrets, provisioned] } } });
    render(<Secrets id="claims-portal" />);
    await section().findByText("API_KEY");
    const row = (name: string, env: string) => screen.getAllByText(name).map((cell) => cell.closest("tr")!).find((tr) => within(tr).queryByText(env) !== null)!;
    const cells = (tr: HTMLElement) => within(tr).getAllByRole("cell").map((cell) => cell.textContent);
    expect(cells(row("API_KEY", "Preview")).slice(0, 4)).toEqual(["API_KEY", "Preview", "Build", "2"]);
    expect(cells(row("DATABASE_URL", "Production")).slice(0, 4)).toEqual(["DATABASE_URL", "Production", "Runtime", "3"]);
    expect(within(row("API_KEY", "Preview")).getByRole("button", { name: "Replace API_KEY in Preview" })).toBeDefined();
    expect(within(row("API_KEY", "Preview")).getByRole("button", { name: "Remove API_KEY from Preview" })).toBeDefined();
    const canyons = row("DATABASE_URL", "Preview");
    expect(within(canyons).getByText(T.canyons)).toBeDefined();
    expect(within(canyons).queryByRole("button")).toBeNull();
  });

  it("opens on the form for the secret a link names, scrolled to and its value's field ready, and never fills a value", async () => {
    server({ [LIST]: { body: { secrets: [] } } });
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    render(<Secrets id="claims-portal" asked={{ name: "STRIPE_KEY", env: "prod" }} />);
    await section().findByText(T.none);
    expect((form().getByLabelText(T.name) as HTMLInputElement).value).toBe("STRIPE_KEY");
    expect((form().getByLabelText(T.env) as HTMLSelectElement).value).toBe("prod");
    expect(value().value).toBe("");
    expect(document.activeElement).toBe(value());
    expect(scrolled).toHaveBeenCalled();
  });

  it("takes from a link only a name a secret may have, and an environment there is", async () => {
    server({ [LIST]: { body: { secrets: [] } } });
    render(<Secrets id="claims-portal" asked={{ name: "<img src=x>", env: "staging" }} />);
    await section().findByText(T.none);
    expect((form().getByLabelText(T.name) as HTMLInputElement).value).toBe("");
    expect((form().getByLabelText(T.env) as HTMLSelectElement).value).toBe("preview");
  });

  it("says when there are none", async () => {
    server({ [LIST]: { body: { secrets: [] } } });
    render(<Secrets id="claims-portal" />);
    expect(await section().findByText(T.none)).toBeDefined();
  });

  // Replacing what a running production reads changes nothing there until a promotion (contracts/routes.md, the secrets row).
  it("says production keeps the old value, where production runs and had one", async () => {
    const had = { ...listed.secrets[0]!, name: "STRIPE_KEY", env: "prod", where: "runtime", origin: "user" } as Secret;
    for (const [live, rows, says] of [[true, [had], true], [false, [had], false], [true, [], false], [true, [{ ...had, env: "preview" } as Secret], false]] as const) {
      server({ [LIST]: { body: { secrets: rows } }, "PUT /api/v1/projects/claims-portal/secrets/STRIPE_KEY": { status: 204, body: null } });
      const { unmount } = render(<Secrets id="claims-portal" live={live} />);
      await waitFor(() => expect(screen.queryByText(T.none) !== null || screen.queryByText("STRIPE_KEY") !== null).toBe(true));
      fill("STRIPE_KEY", VALUE);
      fireEvent.change(form().getByLabelText(T.env), { target: { value: "prod" } });
      fireEvent.click(form().getByRole("button", { name: T.set }));
      const status = (await section().findByRole("status")).textContent!;
      expect(status).toContain("Set STRIPE_KEY for production. It takes effect at the next promotion.");
      expect(status.includes("Production keeps the old value until a version is promoted again."), `${live} ${rows.length}`).toBe(says);
      unmount();
    }
  });

  it("sets a value once, as typed, empties the name and the value, says where it takes effect, and never shows it back", async () => {
    let rows = listed.secrets;
    const PUT = "PUT /api/v1/projects/claims-portal/secrets/STRIPE_WEBHOOK_SECRET";
    const sent = server({ [LIST]: () => ({ body: { secrets: rows } }), [PUT]: () => { rows = [...rows, { ...provisioned, name: "STRIPE_WEBHOOK_SECRET", env: "prod", origin: "user" }]; return { status: 204, body: null }; } });
    const { container } = render(<Secrets id="claims-portal" />);
    await section().findByText("API_KEY");
    fill("STRIPE_WEBHOOK_SECRET", VALUE);
    fireEvent.change(form().getByLabelText(T.env), { target: { value: "prod" } });
    fireEvent.click(form().getByRole("button", { name: T.set }));
    expect(await section().findByRole("status")).toHaveProperty("textContent", "Set STRIPE_WEBHOOK_SECRET for production. It takes effect at the next promotion.");
    expect(sent().filter((line) => line.includes(VALUE))).toEqual([`${PUT} {"value":"${VALUE}","env":"prod","where":"runtime"}`]);
    expect(screen.queryByRole("form", { name: T.form })).toBeNull(); // done: the form closes
    openForm();
    expect(value().value).toBe("");
    expect(form().getByLabelText<HTMLInputElement>(T.name).value).toBe(""); // a name left there would invite the next value to overwrite it
    await section().findByText("STRIPE_WEBHOOK_SECRET"); // the list is read again
    expect(container.innerHTML).not.toContain(VALUE);
    expect(value().type).toBe("password");
  });

  it("says the route's refusal, keeps the form as it was, and sends nothing the route didn't take", async () => {
    const PUT = "PUT /api/v1/projects/claims-portal/secrets/PORT";
    const sent = server({ [LIST]: { body: { secrets: [] } }, [PUT]: { status: 400, body: { error: "invalid_name", message: "PORT is set by Canyon: choose another name" } } });
    render(<Secrets id="claims-portal" />);
    await section().findByText(T.none);
    fill("PORT", "8080");
    fireEvent.click(form().getByRole("button", { name: T.set }));
    expect(await section().findByRole("alert")).toHaveProperty("textContent", "PORT is set by Canyon: choose another name");
    expect(section().queryByRole("status")).toBeNull();
    expect((form().getByLabelText(T.name) as HTMLInputElement).value).toBe("PORT");
    expect(sent().filter((line) => line.startsWith("PUT"))).toHaveLength(1);
  });

  it("offers no Set until both a name and a value are typed, and says which is missing", async () => {
    server({ [LIST]: { body: { secrets: [] } } });
    render(<Secrets id="claims-portal" />);
    await section().findByText(T.none);
    openForm();
    const button = () => form().getByRole("button", { name: T.set });
    expect(button()).toHaveProperty("disabled", true);
    expect(form().getByText(T.nameFirst)).toBeDefined();
    fireEvent.change(form().getByLabelText(T.name), { target: { value: "API_KEY" } });
    expect(button()).toHaveProperty("disabled", true);
    expect(form().getByText(T.valueFirst)).toBeDefined();
    fireEvent.change(value(), { target: { value: "x" } });
    expect(button()).toHaveProperty("disabled", false);
  });

  it("Replace fills the form from its row and puts focus on the value", async () => {
    server({ [LIST]: { body: listed } });
    render(<Secrets id="claims-portal" />);
    fireEvent.click(await section().findByRole("button", { name: "Replace API_KEY in Preview" }));
    expect((form().getByLabelText(T.name) as HTMLInputElement).value).toBe("API_KEY");
    expect((form().getByLabelText(T.env) as HTMLSelectElement).value).toBe("preview");
    expect((form().getByLabelText(T.where) as HTMLSelectElement).value).toBe("build");
    expect(document.activeElement).toBe(value());
    expect(value().value).toBe("");
  });

  it("removes a secret after asking, from the environment of its row", async () => {
    let rows = listed.secrets;
    const DELETE = "DELETE /api/v1/projects/claims-portal/secrets/DATABASE_URL?env=prod";
    const sent = server({ [LIST]: () => ({ body: { secrets: rows } }), [DELETE]: () => { rows = rows.filter((one) => one.name !== "DATABASE_URL"); return { status: 204, body: null }; } });
    render(<Secrets id="claims-portal" />);
    fireEvent.click(await section().findByRole("button", { name: "Remove DATABASE_URL from Production" }));
    expect(section().getByText("Remove DATABASE_URL from production?")).toBeDefined();
    expect(sent().filter((line) => line.startsWith("DELETE"))).toEqual([]); // asking sends nothing
    fireEvent.click(section().getByRole("button", { name: "Remove DATABASE_URL from Production" }));
    expect(await section().findByRole("status")).toHaveProperty("textContent", "Removed DATABASE_URL from production. It takes effect at the next promotion.");
    expect(sent()).toContain(DELETE);
    await waitFor(() => expect(section().queryByText("DATABASE_URL")).toBeNull());
  });

  it("says a read it can't use couldn't be read, never no secrets", async () => {
    server({ [LIST]: { body: { project: {} } } });
    render(<Secrets id="claims-portal" />);
    expect(await section().findByRole("alert")).toHaveProperty("textContent", T.unread);
    expect(section().queryByText(T.none)).toBeNull();
  });
});

// Who sees it (design/PERMISSIONS.md, Change it: secrets; contracts/routes.md, the secrets rows' level): a contributor, an
// owner and an admin get the list and the form; a read-only member and a person with no access get neither, and the
// page asks for no secret on their behalf.
describe("the Secrets section, at each level", () => {
  const app = golden<FleetPage>("fleet/page.json").apps.find((one) => one.id === "claims-portal") as AppRead;
  const status = golden<{ status: Status }>("app-page/claims-portal.json").status;
  const read = (project: object, admin: boolean) => {
    const sent = server({
      "GET /api/v1/projects/claims-portal": { body: { project } },
      "GET /api/v1/projects/claims-portal/status": { body: status },
      [LIST]: { body: listed },
    });
    render(<RailContext.Provider value={{ org: null, viewer: null, admin }}><App id="claims-portal" auth="oidc" /></RailContext.Provider>);
    return sent;
  };
  for (const [who, level, admin] of [["an owner", "owner", false], ["a contributor", "contributor", false], ["an admin", "none", true]] as const) {
    it(`gives ${who} the list and the form`, async () => {
      read({ ...app, access: { ...app.access, level } }, admin);
      expect(await within(await screen.findByRole("region", { name: T.title })).findByText("API_KEY")).toBeDefined();
      expect(section().getByRole("button", { name: T.form })).toBeDefined();
    });
  }
  for (const [who, level] of [["a read-only member", "viewer"], ["a person with no access", "none"]] as const) {
    it(`gives ${who} no section and asks for no secret`, async () => {
      const sent = read({ id: "claims-portal", name: "claims-portal", owners: [], summary: "Tracks claims", access: { level } }, false);
      await screen.findAllByText("claims-portal");
      await act(async () => { await Promise.resolve(); });
      expect(screen.queryByRole("region", { name: T.title })).toBeNull();
      expect(screen.queryByRole("form", { name: T.form })).toBeNull();
      expect(sent().filter((line) => line.includes("/secrets"))).toEqual([]);
    });
  }

  it("keeps the form closed until its button opens it, and Cancel closes it again", async () => {
    server({ [LIST]: { body: listed } });
    render(<Secrets id="claims-portal" />);
    await section().findByText("API_KEY");
    expect(screen.queryByRole("form", { name: T.form })).toBeNull();
    fireEvent.click(section().getByRole("button", { name: T.form }));
    expect(document.activeElement).toBe(value());
    fireEvent.click(form().getByRole("button", { name: T.cancel }));
    expect(screen.queryByRole("form", { name: T.form })).toBeNull();
    fireEvent.click(section().getByRole("button", { name: `${T.replace} API_KEY in Preview` }));
    expect((form().getByLabelText(T.name) as HTMLInputElement).value).toBe("API_KEY");
  });

  it("on a phone, reads each secret as a card of its own rather than a table wider than the screen", async () => {
    vi.stubGlobal("matchMedia", (media: string) => ({ matches: media === "(max-width: 619px)", addEventListener: () => {}, removeEventListener: () => {} }));
    server({ [LIST]: { body: listed } });
    render(<Secrets id="claims-portal" />);
    await section().findByText("API_KEY");
    const card = within(section().getAllByRole("listitem").find((one) => one.textContent?.startsWith("API_KEY") && one.textContent.includes("Preview"))!);
    expect(section().queryByRole("table")).toBeNull();
    expect(card.getByText("Build")).toBeDefined();
    expect(card.getByRole("button", { name: `${T.replace} API_KEY in Preview` })).toBeDefined();
  });
});
