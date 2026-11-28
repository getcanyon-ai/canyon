import { readFileSync } from "node:fs";
import { join } from "node:path";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Preferences } from "../api";
import { S } from "../strings";
import { OrganisationSettings, PeopleSettings } from "./OrgSettings";

// Every address, domain and variable on this page comes from the golden at runtime; nothing here
// writes one down. The two samples are the admin's and the member's projections of one row.
const golden = JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata/guardrails/preferences.json"), "utf8")) as {
  preferences_admin: Preferences;
  preferences_member: Preferences;
};
const PREFERENCES = "/api/v1/preferences";
const ADMIN = golden.preferences_admin;
const MEMBER = golden.preferences_member;
// The address the admin is signed in as, and the one the tests take off the list.
const MINE = ADMIN.viewer!.email;
const THEIRS = "tomas@meridian.example";
// The sentences the server refuses with, as cmd/canyon-server/routes_preferences.go writes them.
// The screen recognises none of them — it renders whatever body came back.
const NO_MEMBERS = "at least one person or one domain has to be allowed to sign in";
const LOCKED_OUT = `an admin has to stay able to sign in, and ${MINE} is on neither membership list`;

// A licence that unlocks access by sign-in group, and one that lapsed (#2879).
const UNLOCKED = { state: "valid", allows: ["outbound_keys", "platform_patching", "group_sync"] };
const LAPSED = { state: "lapsed", allows: [] };

// The server answers "PUT /api/v1/preferences" and records what it was sent, and the licence read with `licence`;
// anything else 404s.
function server(answer: { status?: number; body: unknown }, licence: unknown = UNLOCKED) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === "/api/v1/licence") return { ok: true, status: 200, json: async () => licence } as unknown as Response;
    if (`${init?.method} ${url}` !== `PUT ${PREFERENCES}`) return { ok: false, status: 404, json: async () => ({ error: "not_found" }) } as unknown as Response;
    const { status = 200, body } = answer;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return () => fetch.mock.calls.filter(([, init]) => init?.method === "PUT").map(([, init]) => JSON.parse(String(init?.body)) as Record<string, unknown>);
}

// Settings' two pages drawn from the row: People & sign-in unless the Organisation page is asked for.
// The page holds the row as Settings does: a save's answer is the record it draws from next.
function Page({ record, page }: { record: Preferences; page: "people" | "organisation" }) {
  const [row, setRow] = useState(record);
  return page === "people" ? <PeopleSettings record={row} adopt={setRow} /> : <OrganisationSettings record={row} adopt={setRow} />;
}
const show = (record: Preferences, page: "people" | "organisation" = "people") => render(<Page record={record} page={page} />);
const region = (name: string) => within(screen.getByRole("region", { name }));
// Each section has its own Save, named for what it saves, and shown only while the section differs from what is saved (D16).
const save = (label: string) => fireEvent.click(screen.getByRole("button", { name: label }));
const SIGN_IN = "Save who may sign in";
// The admin's row with the two people on it, which is the row a removal is made from.
const withBoth = (over: Partial<Preferences> = {}): Preferences => ({ ...ADMIN, allowed_emails: [MINE, THEIRS], ...over });

afterEach(cleanup);

describe("Settings › Organisation and People & sign-in", () => {
  // THE RED. Taking somebody off the allowed list has to reach the server as a list without them:
  // membership is checked per request against the stored email, so the write is the whole of the
  // removal and there is no revoke step (design/ONBOARDING.md §3). A screen that dropped the row
  // from its own draft and sent the list it was read would leave that person signed in for as long
  // as their session lasted, with the page reporting success.
  it("a removed address is gone from the list the save sends", async () => {
    const sent = server({ body: withBoth({ allowed_emails: [MINE] }) });
    show(withBoth());
    fireEvent.click(region("Who may sign in").getByRole("button", { name: "Remove address 2" }));
    save(SIGN_IN);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.allowed_emails).toEqual([MINE]);
    expect(sent()[0]!.allowed_emails).not.toContain(THEIRS);
  });

  // The environment's admins and the row's are two groups, never one merged list. `overridden` does
  // not name admin_emails — CANYON_ADMINS grants without pinning — so if the screen drew one list,
  // somebody would remove a bootstrap address, the PUT would succeed with nothing to refuse, and
  // that person would still be an admin.
  it("the bootstrap admins are their own group and no save can remove them", async () => {
    const sent = server({ body: ADMIN });
    show(ADMIN);
    const admins = region("Who administers Canyon");
    const granted = ADMIN.bootstrap_admins[0]!;
    expect(admins.getByText(granted)).toBeTruthy();
    expect(admins.getByText(/Granted by CANYON_ADMINS\. They cannot be removed here\./)).toBeTruthy();
    // Every removable row belongs to the row's own list: none of them is the bootstrap address.
    for (const button of admins.queryAllByRole("button", { name: /^Remove admin/ })) {
      fireEvent.click(button);
    }
    save("Save the admins");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.admin_emails).toEqual([]);
    expect(sent()[0]!.admin_emails).not.toContain(granted);
  });

  // Production is approved in the dashboard: a choice, so it saves as it is ticked and says "Saved." (D16), sending that
  // key alone; a member reads it without being able to move it (design/AUTH.md §4.5).
  it("the production approval switch saves as it is ticked, and a member cannot change it", async () => {
    const sent = server({ body: { ...ADMIN, prod_approval: true } });
    show(ADMIN);
    const production = region("Production");
    expect(production.getByText("On: the CLI and agents only ask, and a person on the app's team approves it here in Canyon. Off: anyone on the app's team can promote, and so can an agent they ask, through the CLI or Canyon's tools.")).toBeTruthy();
    const box = production.getByRole("checkbox", { name: "Production is approved in the dashboard" }) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(sent()).toHaveLength(0);
    fireEvent.click(box);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]).toEqual({ prod_approval: true });
    expect(await production.findByText("Saved.")).toBeTruthy();
    expect(box.checked).toBe(true);
    expect(screen.queryByRole("button", { name: /^Save/ })).toBeNull();
    cleanup();
    show({ ...MEMBER, prod_approval: true });
    const read = region("Production").getByRole("checkbox", { name: "Production is approved in the dashboard" }) as HTMLInputElement;
    expect(read.checked && read.disabled).toBe(true);
  });

  // A member reads the row and changes nothing: no Save anywhere, no editable field, and one line
  // saying who does change it.
  it("a member is shown the row read-only and told an admin changes it", () => {
    show(MEMBER);
    expect(screen.getByText("An admin changes these settings.")).toBeTruthy();
    expect(screen.queryAllByRole("button", { name: /^Save/ })).toHaveLength(0);
    expect(screen.queryAllByRole("textbox")).toHaveLength(0);
    expect(region("Who administers Canyon").getByText(MEMBER.admin_emails[0]!)).toBeTruthy();
  });

  // A key the environment sets is read-only and names the variable, so nobody edits a value that
  // will not take. The admin claim is that key in the golden, and the pair is named together.
  it("a key the environment sets names the variable it comes from", () => {
    show(ADMIN);
    const admins = region("Who administers Canyon");
    expect(admins.getByText(/Set on the server, by CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE\. Change it there, not here\./)).toBeTruthy();
    expect(admins.queryByRole("textbox", { name: "Claim" })).toBeNull();
  });

  // The people are everyone's to read, the environment's admins too; a variable's name is an admin's alone (#118). So a
  // member reads every admin there is in one list, what each pinned key holds, and no CANYON_ name, nor a sign-in group.
  it("a member reads every admin, what each pinned key holds, and no variable's name", () => {
    const pinned: Preferences = { ...MEMBER, allowed_emails: [THEIRS], groups_claim: "teams", overridden: ["org_name", "allowed_emails", "allowed_domains", "admin_claim", "admin_value", "groups_claim"] };
    show(pinned);
    const admins = region("Who administers Canyon");
    expect(admins.getByText("dana@meridian.example")).toBeTruthy();
    expect(admins.getByText("founder@meridian.example")).toBeTruthy();
    expect(admins.queryByText("From the environment")).toBeNull();
    expect(admins.queryByText("By sign-in group (optional)")).toBeNull();
    expect(region("Who may sign in").getByText(THEIRS)).toBeTruthy();
    expect(region("Who may sign in").getByText(MEMBER.allowed_domains[0]!)).toBeTruthy();
    expect(region("Groups").getByText("teams")).toBeTruthy();
    expect(document.body.textContent).not.toContain("CANYON_");
    cleanup();
    show(pinned, "organisation");
    expect(screen.getByText(MEMBER.org_name)).toBeTruthy();
    expect(document.body.textContent).not.toContain("CANYON_");
    cleanup();
    // An admin by both is one person, named once.
    show({ ...MEMBER, bootstrap_admins: MEMBER.admin_emails });
    expect(region("Who administers Canyon").getAllByText("dana@meridian.example")).toHaveLength(1);
  });

  // A member is not sent the admin group, so a list with no address says only that (#118), never nobody.
  it("a member where no admin is listed is never told nobody administers Canyon", () => {
    show({ ...MEMBER, admin_emails: [], bootstrap_admins: [] });
    const admins = region("Who administers Canyon");
    expect(admins.queryByText("Nobody.")).toBeNull();
    expect(admins.getByText("No admin is listed by email address.")).toBeTruthy();
  });

  // No one by address while a domain lets people in is not nobody (#118): the line says who can sign in.
  it("a member with no addresses and a domain reads who can sign in", () => {
    show(MEMBER);
    const who = region("Who may sign in");
    expect(who.getByText(`No one by address; anyone at ${MEMBER.allowed_domains[0]} can sign in.`)).toBeTruthy();
    expect(who.queryByText("Nobody.")).toBeNull();
    cleanup();
    show({ ...MEMBER, allowed_domains: ["meridian.example", "acme.example", "globex.example"] });
    expect(region("Who may sign in").getByText("No one by address; anyone at meridian.example, acme.example or globex.example can sign in.")).toBeTruthy();
  });

  // An empty list is said as what it is (#140): below admin, an empty Domains list and an unset groups claim are not
  // people, so neither reads "Nobody."; nor does an unset claim to an admin without the group-sync licence. An admin who
  // may set them edits them instead.
  it("an empty Domains list and an unset groups claim say what they are, never nobody", async () => {
    show({ ...MEMBER, allowed_emails: [THEIRS], allowed_domains: [], groups_claim: "" });
    expect(region("Who may sign in").getByText("No domain is allowed.")).toBeTruthy();
    expect(region("Groups").getByText("No groups claim is set.")).toBeTruthy();
    expect(screen.queryByText("Nobody.")).toBeNull();
    cleanup();
    server({ body: ADMIN }, LAPSED);
    show({ ...ADMIN, groups_claim: "" });
    expect(await region("Groups").findByText("No groups claim is set.")).toBeTruthy();
    expect(region("Groups").queryByText("Nobody.")).toBeNull();
    cleanup();
    server({ body: ADMIN });
    show({ ...ADMIN, allowed_domains: [], groups_claim: "" });
    expect(await screen.findByRole("textbox", { name: "Groups claim" })).toBeTruthy();
    expect(screen.queryByText("No groups claim is set.")).toBeNull();
    expect(screen.queryByText("No domain is allowed.")).toBeNull();
  });

  // Every rule is the server's. The screen sends what was typed, renders the sentence that comes
  // back, and knows nothing about last members or locked-out admins.
  it.each([
    ["the last member", NO_MEMBERS, "no_members"],
    ["an admin left off both lists", LOCKED_OUT, "admin_locked_out"],
  ])("a refusal about %s is the server's own sentence", async (_what, message, error) => {
    server({ status: 400, body: { error, message } });
    show(withBoth());
    fireEvent.click(region("Who may sign in").getByRole("button", { name: "Remove address 1" }));
    save(SIGN_IN);
    expect(await screen.findByText(message)).toBeTruthy();
  });

  // A refused save keeps what was typed: you need it to fix what the server refused.
  it("a refused save keeps the edit on screen", async () => {
    server({ status: 400, body: { error: "no_members", message: NO_MEMBERS } });
    show(withBoth());
    fireEvent.click(region("Who may sign in").getByRole("button", { name: "Remove address 2" }));
    save(SIGN_IN);
    expect(await screen.findByText(NO_MEMBERS)).toBeTruthy();
    expect(screen.getByRole("textbox", { name: "Address 1" })).toHaveProperty("value", MINE);
    expect(screen.queryByRole("textbox", { name: "Address 2" })).toBeNull();
  });

  // One Save per section, and each names only its own keys: one audit row with its before and after for what that
  // section changed, never a key the environment pins.
  it("each page saves only its own keys, and never a pinned one", async () => {
    const sent = server({ body: ADMIN });
    show(ADMIN, "organisation");
    fireEvent.change(screen.getByRole("textbox", { name: "Organization name" }), { target: { value: "Meridian" } });
    save("Save the organization name");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(Object.keys(sent()[0]!)).toEqual(["org_name"]);
    expect(sent()[0]!.org_name).toBe("Meridian");
    cleanup();
    show(ADMIN);
    fireEvent.change(await screen.findByRole("textbox", { name: "Groups claim" }), { target: { value: "teams" } });
    save("Save the groups claim");
    await waitFor(() => expect(sent()).toHaveLength(2));
    // Only what changed: an untouched list is not sent, so the server does not judge it again.
    expect(sent()[1]).toEqual({ groups_claim: "teams" });
  });

  // The groups claim (contracts/routes.md, the preferences row): the admin's to set with the page's
  // Save, read-only to a member, and naming CANYON_GROUPS_CLAIM, never sent, when the environment sets it.
  it("an admin sets the groups claim, and a member only reads it", async () => {
    const sent = server({ body: ADMIN });
    show({ ...ADMIN, groups_claim: "" });
    fireEvent.change(await screen.findByRole("textbox", { name: "Groups claim" }), { target: { value: "groups" } });
    save("Save the groups claim");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.groups_claim).toBe("groups");
    cleanup();
    show({ ...MEMBER, groups_claim: "groups" });
    expect(region("Groups").getByText("groups")).toBeTruthy();
    expect(region("Groups").queryByRole("textbox")).toBeNull();
  });

  // Setting or changing the groups claim is Canyon Enterprise's (#2879): with a lapsed licence an admin reads the claim,
  // may clear it, and is told why there is no field; with none set, the line alone.
  it("without a licence, an admin may clear the groups claim but not set or change it", async () => {
    const sent = server({ body: ADMIN }, LAPSED);
    show({ ...ADMIN, groups_claim: "groups" });
    expect(await region("Groups").findByText(S.licenceRequiredGroups)).toBeTruthy();
    expect(region("Groups").getByText(S.enterprise)).toBeTruthy();
    expect(region("Groups").queryByRole("textbox")).toBeNull();
    expect(region("Groups").getByText("groups")).toBeTruthy();
    fireEvent.click(region("Groups").getByRole("button", { name: "Clear the groups claim" }));
    save("Save the groups claim");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.groups_claim).toBe("");
    cleanup();
    server({ body: ADMIN }, LAPSED);
    show({ ...ADMIN, groups_claim: "" });
    await region("Groups").findByText(S.enterprise);
    expect(region("Groups").queryByRole("button", { name: "Clear the groups claim" })).toBeNull();
  });

  it("a groups claim the environment sets names its variable and is never sent", async () => {
    const sent = server({ body: ADMIN });
    show({ ...ADMIN, groups_claim: "groups", overridden: [...ADMIN.overridden, "groups_claim"] });
    expect(region("Groups").getByText("Set on the server, by CANYON_GROUPS_CLAIM. Change it there, not here.")).toBeTruthy();
    expect(region("Groups").queryByRole("button", { name: "Save the groups claim" })).toBeNull();
    expect(sent()).toHaveLength(0);
  });

  // The organisation's name pinned by the environment has no Save: its only answer is set_by_operator.
  it("a pinned organisation name names its variable and offers no Save", () => {
    show({ ...ADMIN, overridden: [...ADMIN.overridden, "org_name"] }, "organisation");
    expect(screen.getByText("Set on the server, by CANYON_ORG. Change it there, not here.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Save/ })).toBeNull();
  });

  // "Saved." and a refusal never share the line, and the word only appears once the server answered.
  // The Save shows only while the name differs from the saved one, and goes once the server has it (D16).
  it("Saved. appears only after the server answered, and the Save only while something changed", async () => {
    server({ body: { ...ADMIN, org_name: "Meridian" } });
    show(ADMIN, "organisation");
    expect(screen.queryByRole("button", { name: "Save the organization name" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Organization name" }), { target: { value: "Meridian" } });
    expect(screen.queryByRole("status")).toBeNull();
    save("Save the organization name");
    expect(await screen.findByRole("status")).toHaveProperty("textContent", "Saved.");
    expect(screen.queryByRole("button", { name: "Save the organization name" })).toBeNull();
    fireEvent.change(screen.getByRole("textbox", { name: "Organization name" }), { target: { value: "Meridian Ltd" } });
    expect(screen.queryByRole("status")).toBeNull();
  });

  // One section's Save never touches another's typing (D16): saving who may sign in keeps an admin typed and not saved.
  it("a section's save keeps another section's unsaved typing", async () => {
    const sent = server({ body: withBoth({ allowed_emails: [MINE] }) });
    show(withBoth());
    fireEvent.click(region("Who administers Canyon").getByRole("button", { name: "Add an admin" }));
    const typed = region("Who administers Canyon").getAllByRole("textbox").at(-1)!;
    fireEvent.change(typed, { target: { value: THEIRS } });
    fireEvent.click(region("Who may sign in").getByRole("button", { name: "Remove address 2" }));
    save(SIGN_IN);
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(Object.keys(sent()[0]!)).toEqual(["allowed_emails"]);
    expect(await region("Who may sign in").findByText("Saved.")).toBeTruthy();
    expect(typed).toHaveProperty("value", THEIRS);
    expect(region("Who administers Canyon").getByRole("button", { name: "Save the admins" })).toBeTruthy();
  });
});

// Settings › People & sign-in, the security responders (design/PERMISSIONS.md, Security responders): an
// admin's alone to read and to set — the server answers the three keys to an admin only — by address,
// or by a sign-in claim whose pair goes both or neither, as the admin pair does (contracts/routes.md,
// the preferences row).
describe("Settings › People & sign-in, the security responders", () => {
  const RESPONDERS = "Security responders";
  const RHEA = ADMIN.responder_emails![0]!;
  const COPY = "Security responders can stop any app's production and approve its blocking findings. They never deploy, promote or change secrets.";

  // The test cluster pass (#2258): adding a row left the cursor on the button, and the field cut the address off.
  it("adding a responder puts the cursor in the new field, wide enough for an address", () => {
    show(ADMIN);
    fireEvent.click(region(RESPONDERS).getByRole("button", { name: "Add a responder" }));
    const field = region(RESPONDERS).getByRole("textbox", { name: "Responder 2" });
    expect(document.activeElement).toBe(field);
    expect(field.className.split(" ")).toContain("flex-1");
    expect(region(RESPONDERS).getByRole("textbox", { name: "Responder 1" })).not.toBe(document.activeElement);
  });

  // An admin or a responder approves a blocking finding: the admins' note never says everyone else does.
  it("the admins' note says who approves a blocking finding", () => {
    show(ADMIN);
    const admins = region("Who administers Canyon");
    expect(admins.queryByText(/Everyone else deploys, promotes and approves/)).toBeNull();
    expect(admins.getByText("Admins change these settings and the policies. Everyone else deploys and promotes; only an admin or a security responder approves a blocking finding.")).toBeTruthy();
  });

  it("an admin reads who the responders are, by address and by claim, and what they may do", () => {
    show(ADMIN);
    const responders = region(RESPONDERS);
    expect(responders.getByText(COPY)).toBeTruthy();
    expect(responders.getByRole("textbox", { name: "Responder 1" })).toHaveProperty("value", RHEA);
    expect(responders.getByRole("textbox", { name: "Responder claim" })).toHaveProperty("value", ADMIN.responder_claim);
    expect(responders.getByRole("textbox", { name: "Responder value" })).toHaveProperty("value", ADMIN.responder_value);
  });

  // A member is answered none of the three keys; a responder is a member here: Settings is an admin's.
  it.each([["a member", MEMBER], ["a responder", { ...MEMBER, viewer: { email: RHEA, role: "responder" as const } }]])("%s is shown no responders at all", (_who, record) => {
    show(record);
    expect(screen.queryByRole("region", { name: RESPONDERS })).toBeNull();
    expect(screen.queryByText(RHEA)).toBeNull();
  });

  it("an added address is in the list the save sends, and an untouched list offers no Save", async () => {
    const sent = server({ body: ADMIN });
    show(ADMIN);
    expect(region(RESPONDERS).queryByRole("button", { name: "Save the security responders" })).toBeNull();
    fireEvent.click(region(RESPONDERS).getByRole("button", { name: "Add a responder" }));
    fireEvent.change(region(RESPONDERS).getByRole("textbox", { name: "Responder 2" }), { target: { value: THEIRS } });
    save("Save the security responders");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]!.responder_emails).toEqual([RHEA, THEIRS]);
  });

  // THE RED (#2068). The claim and its value are one setting: a save that sent the claim alone would
  // name a claim matching nobody, or one the server refuses. Changing either sends both; clearing the
  // claim clears the pair.
  it("the claim and its value are sent together or not at all", async () => {
    const sent = server({ body: ADMIN });
    show(ADMIN);
    const responders = region(RESPONDERS);
    fireEvent.change(responders.getByRole("textbox", { name: "Responder claim" }), { target: { value: "roles" } });
    save("Save the security responders");
    await waitFor(() => expect(sent()).toHaveLength(1));
    expect(sent()[0]).toMatchObject({ responder_claim: "roles", responder_value: ADMIN.responder_value });
    fireEvent.change(responders.getByRole("textbox", { name: "Responder claim" }), { target: { value: "" } });
    save("Save the security responders");
    await waitFor(() => expect(sent()).toHaveLength(2));
    expect(sent()[1]).toMatchObject({ responder_claim: null, responder_value: null });
  });

  // Pinned by CANYON_RESPONDER_CLAIM and CANYON_RESPONDER_VALUE: read-only, named, never sent.
  it("a claim the environment sets names its variables and is never sent", async () => {
    const sent = server({ body: ADMIN });
    show({ ...ADMIN, overridden: [...ADMIN.overridden, "responder_claim", "responder_value"] });
    const responders = region(RESPONDERS);
    expect(responders.getByText(/Set on the server, by CANYON_RESPONDER_CLAIM and CANYON_RESPONDER_VALUE\. Change it there, not here\./)).toBeTruthy();
    expect(responders.queryByRole("textbox", { name: "Responder claim" })).toBeNull();
    expect(responders.queryByRole("button", { name: "Save the security responders" })).toBeNull();
    expect(sent()).toHaveLength(0);
  });
});
