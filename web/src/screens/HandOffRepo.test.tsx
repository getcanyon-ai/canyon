import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { FrontDoorRead, PlatformRepo, Preferences } from "../api";
import { S } from "../strings";
import { HandOff } from "./HandOff";
import { filled } from "./HandOffRepo";

// The contracts' own shapes: the deploy repo's read and its first save, the front door, the preferences.
const fixture = (path: string) => JSON.parse(readFileSync(join(import.meta.dirname, "../../../contracts/testdata", path), "utf8"));
const door = fixture("front-door/meridian.json") as { get_unprobed: FrontDoorRead };
const release = fixture("release/meridian.json") as { repo_get: PlatformRepo; repo_saved: PlatformRepo & { fetch_token: string } };
const prefs = fixture("guardrails/preferences.json") as { preferences_admin: Preferences };
const gateway: FrontDoorRead = { ...door.get_unprobed, probe_note: "", front_door: { ...door.get_unprobed.front_door!, probed: true, enforcing: true } };
const viaRepo = { ...prefs.preferences_admin, run_by: "canyon" as const, delivery: "repo" as const };
const repo = { ...release.repo_get, checked_at: null };
const gitops = (path: string) => readFileSync(join(import.meta.dirname, "../../../deploy/gitops", path), "utf8");

type Answer = { status?: number; body?: unknown };
function server(routes: Record<string, Answer | (() => Answer | Promise<Answer>)>) {
  const fetch = vi.fn(async (url: string, init?: RequestInit) => {
    const key = `${init?.method ?? "GET"} ${url}`;
    const route = routes[key] ?? (key.startsWith("GET /api/v1/platform-") ? { body: null } : { status: 404, body: { error: "not_found" } }); // none saved: 200 null (#2560)
    const { status = 200, body } = typeof route === "function" ? await route() : route;
    return { ok: status < 300, status, json: async () => body } as unknown as Response;
  });
  vi.stubGlobal("fetch", fetch);
  return (method: string, url: string) => fetch.mock.calls.filter(([u, init]) => u === url && (init?.method ?? "GET") === method).map(([, init]) => (init?.body === undefined ? undefined : JSON.parse(String(init.body)) as unknown));
}
const draw = (props: Partial<Parameters<typeof HandOff>[0]> = {}) => {
  const adopt = vi.fn();
  render(<HandOff read={gateway} record={viaRepo} admin adopt={adopt} reload={vi.fn(async () => {})} toFrontDoor={vi.fn()} {...props} />);
  return { adopt };
};
const step = (n: number) => screen.getAllByRole("listitem").filter((one) => one.parentElement?.tagName === "OL")[n - 1]!;
const title = (n: number) => within(step(n)).getAllByRole("heading")[0]!; // the step's own, above the platform team's card under the repo step

afterEach(() => { vi.unstubAllGlobals(); cleanup(); });

describe("the deploy repo (delivery: repo)", () => {
  // Where apps run is run_by's, never whether a deploy repo is saved (#2560).
  it("a saved repo under run_by canyon leaves Canyon running apps", async () => {
    server({ "GET /api/v1/platform-repo": { body: repo } });
    draw();
    expect((await screen.findAllByText(repo.url, { exact: false })).length).toBeGreaterThan(0);
    expect(screen.getByRole("radio", { name: S.runCanyon })).toHaveProperty("checked", true);
    expect(screen.getByRole("radio", { name: S.runOrg })).toHaveProperty("checked", false);
  });

  it("is the checklist's second step: a form until a repo is saved, the first save showing its tokens once", async () => {
    const answered = server({ "PUT /api/v1/platform-repo": { body: { ...release.repo_saved, token: "host-token-shown-once" } } });
    draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    expect(title(2).textContent).toBe(`2. ${S.stepRepo}`);
    fireEvent.change(within(step(2)).getByLabelText(S.repoUrl), { target: { value: "https://github.com/meridian/deploy" } });
    fireEvent.change(within(step(2)).getByLabelText(S.repoToken), { target: { value: "repo-token" } });
    expect(within(step(2)).getByText(S.repoTokenNote)).toBeDefined();
    fireEvent.click(within(step(2)).getByRole("button", { name: "Save the deploy repo" }));
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText(S.fetchLabel)).toHaveProperty("value", release.repo_saved.fetch_token);
    expect(within(dialog).getByLabelText("Host token")).toHaveProperty("value", "host-token-shown-once");
    expect(within(dialog).getByText(S.fetchNote)).toBeDefined();
    expect(answered("PUT", "/api/v1/platform-repo")).toEqual([{ url: "https://github.com/meridian/deploy", username: "x-access-token", token: "repo-token", preview: { branch: "preview" },
      prod: { branch: "main", mode: "pull_request" }, root: "apps", api: "github", api_url: "https://api.github.com", store: "canyon", ca: null }]);
  });

  it("says before the save that production is pushed with no review in git under no pulls API", async () => {
    const answered = server({ "PUT /api/v1/platform-repo": { body: release.repo_saved } });
    draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPINone }));
    expect(within(step(2)).getByRole("note").textContent).toBe(S.repoNoReview);
    expect(within(step(2)).queryByRole("radio", { name: S.repoPR })).toBeNull();
    fireEvent.click(within(step(2)).getByRole("button", { name: "Save the deploy repo" }));
    await waitFor(() => expect(answered("PUT", "/api/v1/platform-repo")).toHaveLength(1));
    expect(answered("PUT", "/api/v1/platform-repo")[0]).toMatchObject({ api: "none", prod: { branch: "main", mode: "push" } });
  });

  it("opens production's merge requests on GitLab, its API on the repo's own host unless named", async () => {
    const answered = server({ "PUT /api/v1/platform-repo": { body: { ...release.repo_saved, api: "gitlab", api_url: "https://gitlab.meridian.example/api/v4" } } });
    draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    fireEvent.change(within(step(2)).getByLabelText(S.repoUrl), { target: { value: "https://gitlab.meridian.example/platform/deploy" } });
    fireEvent.change(within(step(2)).getByLabelText(S.repoToken), { target: { value: "repo-token" } });
    fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitLab }));
    expect(within(step(2)).getByRole("note").textContent).toBe(S.repoGitLabNote.replace("<host>", "gitlab.meridian.example"));
    expect(within(step(2)).getByRole("radio", { name: S.repoPR })).toHaveProperty("checked", true);
    expect(within(step(2)).getByLabelText(S.repoAPIURL)).toHaveProperty("value", "");
    fireEvent.click(within(step(2)).getByRole("button", { name: "Save the deploy repo" }));
    await waitFor(() => expect(answered("PUT", "/api/v1/platform-repo")).toHaveLength(1));
    const sent = answered("PUT", "/api/v1/platform-repo")[0] as Record<string, unknown>;
    expect(sent).toMatchObject({ api: "gitlab", prod: { branch: "main", mode: "pull_request" } });
    expect(sent).not.toHaveProperty("api_url");
  });

  it("names the repo's host in the GitLab note, and shows it under a merge request alone", async () => {
    server({});
    draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitLab }));
    expect(within(step(2)).getByRole("note").textContent).toBe(S.repoGitLabNote);
    expect(within(step(2)).getByRole("note").textContent).toContain("https://<host>/api/v4");
    fireEvent.change(within(step(2)).getByLabelText(S.repoUrl), { target: { value: "https://git.meridian.example:8443/platform/deploy" } });
    expect(within(step(2)).getByRole("note").textContent).toBe(S.repoGitLabNote.replace("<host>", "git.meridian.example:8443"));
    fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoPush }));
    expect(within(step(2)).queryByRole("note")).toBeNull();
  });

  it("gives each API its own address: GitHub's back on GitHub, GitLab's empty, a GitLab address never kept for GitHub", () => {
    server({});
    draw();
    return screen.findByRole("radio", { name: S.runOrg }).then((org) => {
      fireEvent.click(org);
      const address = () => within(step(2)).getByLabelText(S.repoAPIURL);
      expect(address()).toHaveProperty("value", "https://api.github.com");
      fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitLab }));
      expect(address()).toHaveProperty("value", "");
      fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitHub }));
      expect(address()).toHaveProperty("value", "https://api.github.com");
      fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitLab }));
      fireEvent.change(address(), { target: { value: "https://git.meridian.example/gitlab/api/v4" } });
      fireEvent.click(within(step(2)).getByRole("radio", { name: S.repoAPIGitHub }));
      expect(address()).toHaveProperty("value", "https://api.github.com");
    });
  });

  it("ticks the step only once a check passed, and says what a check can't prove", async () => {
    let checked = false;
    server({
      "GET /api/v1/platform-repo": () => ({ body: { ...repo, checked_at: checked ? "2026-09-27T08:05:00Z" : null } }),
      "POST /api/v1/platform-repo/check": () => { checked = true; return { body: { passed: true, problems: [], templates: [".canyon/resources/postgres.yaml", ".canyon/route.yaml"] } }; },
    });
    draw();
    await screen.findByText(repo.url);
    fireEvent.click(screen.getByRole("radio", { name: S.runOrg }));
    expect(title(2).textContent).toBe(`2. ${S.stepRepo}`);
    expect(within(step(2)).getByText(S.checkCannot)).toBeDefined();
    fireEvent.click(within(step(2)).getByRole("button", { name: S.check }));
    expect(await within(step(2)).findByText(S.checkPassed)).toBeDefined();
    expect(within(step(2)).getByText(S.checkTemplates.replace("<list>", ".canyon/resources/postgres.yaml, .canyon/route.yaml"))).toBeDefined();
    await waitFor(() => expect(title(2).textContent).toBe(`✓ ${S.stepRepo}`));
  });

  it("lists each problem a check found in D5's words, never ticking the step", async () => {
    server({
      "GET /api/v1/platform-repo": { body: repo },
      "POST /api/v1/platform-repo/check": { body: { passed: false, problems: [{ code: "auth_refused", message: "The repo refused the token. Check it is current and has this repository, and replace it here." }], templates: [] } },
    });
    draw();
    await screen.findByText(repo.url);
    fireEvent.click(screen.getByRole("radio", { name: S.runOrg }));
    fireEvent.click(within(step(2)).getByRole("button", { name: S.check }));
    expect(await within(step(2)).findByText("The repo refused the token. Check it is current and has this repository, and replace it here.")).toBeDefined();
    expect(within(step(2)).getByText(S.checkNoTemplates.replace("<path>", "apps"))).toBeDefined();
    expect(title(2).textContent).toBe(`2. ${S.stepRepo}`);
  });

  // It changes how every release reaches the platform, so a pick asks first, saying what changes, and Cancel sends
  // nothing (D16).
  it("is chosen in the dashboard: a pick asks first, then saves delivery", async () => {
    const answered = server({ "PUT /api/v1/preferences": { body: { ...viaRepo, delivery: "webhook" } } });
    const { adopt } = draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    fireEvent.click(screen.getByRole("radio", { name: S.deliveryWebhook }));
    const ask = within(screen.getByRole("group", { name: S.deliveryAsk }));
    expect(ask.getByText(S.deliveryWebhookNote)).toBeDefined();
    fireEvent.click(ask.getByRole("button", { name: "Cancel" }));
    expect((screen.getByRole("radio", { name: S.deliveryRepo }) as HTMLInputElement).checked).toBe(true);
    expect(answered("PUT", "/api/v1/preferences")).toEqual([]);
    fireEvent.click(screen.getByRole("radio", { name: S.deliveryWebhook }));
    fireEvent.click(within(screen.getByRole("group", { name: S.deliveryAsk })).getByRole("button", { name: S.deliveryYes }));
    await waitFor(() => expect(adopt).toHaveBeenCalled());
    expect(answered("PUT", "/api/v1/preferences")).toEqual([{ delivery: "webhook" }]);
  });

  it("once on, says where releases go and that the first commit waits on a check-in, and a member changes nothing", async () => {
    server({ "GET /api/v1/platform-repo": { body: release.repo_get } });
    draw({ record: { ...viaRepo, run_by: "org" }, admin: false });
    expect(await screen.findByText(S.onRepoSummary.replace("<url>", release.repo_get.url))).toBeDefined();
    expect(screen.getByText(S.onRepoWaiting)).toBeDefined();
    expect(screen.queryByRole("button", { name: S.check })).toBeNull();
    expect(screen.queryByLabelText(S.repoUrl)).toBeNull();
    const card = screen.getByRole("region", { name: S.teamTitle });
    expect(card.textContent).toContain("repoURL: https://github.com/meridian/deploy");
    expect(card.textContent).toContain(`url: "${window.location.origin}/releases/{{ .remoteRef.key }}/access-token"`);
    expect(card.textContent).not.toContain("${");
    expect(card.textContent).not.toMatch(/redacted|fetch_token/);
  });
});

describe("the repo's host token", () => {
  it("replacing it asks first, Cancel sends nothing, and the new one shows only once the server answered", async () => {
    let answer: (value: Answer) => void = () => {};
    const sent = server({ "GET /api/v1/platform-repo": { body: repo }, "POST /api/v1/platform-repo/host-token": () => new Promise<Answer>((resolve) => { answer = resolve; }) });
    draw();
    fireEvent.click(await within(await screen.findByText(repo.url).then(() => { fireEvent.click(screen.getByRole("radio", { name: S.runOrg })); return step(2); })).findByRole("button", { name: S.repoHostReplace }));
    expect(screen.getByText(S.repoHostAsk)).toBeDefined();
    expect(S.repoHostAsk).toBe("Replace the host token? The current one stops working at once: your platform reports with the new one from now on.");
    fireEvent.click(within(screen.getByText(S.repoHostAsk).parentElement!).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(S.repoHostAsk)).toBeNull();
    expect(sent("POST", "/api/v1/platform-repo/host-token")).toEqual([]);
    fireEvent.click(within(step(2)).getByRole("button", { name: S.repoHostReplace }));
    fireEvent.click(within(screen.getByText(S.repoHostAsk).parentElement!).getByRole("button", { name: S.repoHostReplace }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await act(async () => answer({ body: { token: "repo-host-token-not-real" } }));
    const dialog = screen.getByRole("dialog", { name: S.revealTitle });
    expect(within(dialog).getByLabelText("Host token")).toHaveProperty("value", "repo-host-token-not-real");
    expect(within(dialog).getAllByRole("textbox")).toHaveLength(1);
    expect(sent("POST", "/api/v1/platform-repo/host-token")).toEqual([undefined]);
    expect(sent("POST", "/api/v1/platform-webhook/token")).toEqual([]);
  });
});

describe("the CA field", () => {
  it("says what the CA is for: the sign-in sidecar and External Secrets, a git host's CA going in Canyon's server", async () => {
    server({});
    draw();
    fireEvent.click(await screen.findByRole("radio", { name: S.runOrg }));
    const ca = within(step(2)).getByLabelText("Your private certificate authority, if one signed Canyon's address (optional)");
    expect(ca.tagName).toBe("TEXTAREA");
    expect(ca.nextElementSibling?.textContent).toBe("Only when a private CA signed Canyon's own address: the sign-in sidecar in each pod and External Secrets trust it. A git host on a private CA needs its CA in Canyon's server instead: A git host at a private address, under Your deploy repo in Canyon's docs.");
  });
});

// The platform team's card (design/EXPERIENCE.md D5): the reference set-up filled in to copy, what Canyon doesn't know typed
// into fields that never leave the browser.
describe("the platform team's card", () => {
  const card = () => screen.getByRole("region", { name: S.teamTitle });
  const file = (name: string) => within(card()).getByRole("figure", { name });
  const applied = ["argocd/applicationsets.yaml", "argocd/appprojects.yaml", "admission/services.yaml", "argocd/namespaces.yaml", "external-secrets/clustersecretstore.yaml", "argocd/repository.yaml"];
  const chart = ["Chart.yaml", "values.yaml", "templates/namespace.yaml"];
  const shown = async () => {
    server({ "GET /api/v1/platform-repo": { body: repo } });
    draw();
    await screen.findByRole("region", { name: S.teamTitle }); // the saved repo, read
    fireEvent.click(screen.getByRole("radio", { name: S.runOrg }));
    return screen.findByRole("region", { name: S.teamTitle });
  };

  it("sits under the repo step once a repo is saved, its first line saying the platform team's part comes next", async () => {
    const here = await shown();
    expect(step(2).contains(here)).toBe(true);
    expect(step(3).contains(here)).toBe(false);
    expect(here.firstElementChild?.textContent).toBe("Next, your platform team applies these once. The first deploy proves the set-up: it goes live when a pod checks in.");
    expect(screen.getAllByRole("region", { name: S.teamTitle })).toHaveLength(1);
  });

  it("under org stays below the section, without the checklist's first line", async () => {
    server({ "GET /api/v1/platform-repo": { body: release.repo_get } });
    draw({ record: { ...viaRepo, run_by: "org" } });
    const here = await screen.findByRole("region", { name: S.teamTitle });
    expect(screen.queryByRole("list")).toBeNull();
    expect(here.firstElementChild?.textContent).toBe(S.teamTitle);
    expect(screen.queryByText(S.teamNext)).toBeNull();
  });

  it("shows each file to apply, then the namespaces chart's three files for the platform repo", async () => {
    await shown();
    for (const name of applied) expect(file(name)).toBeDefined();
    const heading = within(card()).getByRole("heading", { name: "Into your platform repo, under <Where it holds namespaces/>" });
    expect(heading.compareDocumentPosition(file("argocd/repository.yaml")) & Node.DOCUMENT_POSITION_PRECEDING).toBeTruthy();
    for (const name of chart) {
      expect(heading.compareDocumentPosition(file(name)) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
      expect(file(name).querySelector("pre")?.textContent).toBe(gitops(`namespaces/${name}`));
    }
  });

  it("shows each field's name in the files until it is filled", async () => {
    await shown();
    expect(file("argocd/repository.yaml").textContent).toContain("username: <Argo CD's read-only user>");
    expect(file("argocd/repository.yaml").textContent).toContain("password: <Argo CD's read-only token>");
    expect(file("argocd/namespaces.yaml").textContent).toContain("- <Your platform repo>");
    expect(file("argocd/namespaces.yaml").textContent).toContain("targetRevision: <Its branch>");
    expect(file("argocd/namespaces.yaml").textContent).toContain("path: <Where it holds namespaces/>");
    expect(card().textContent).not.toContain("${");
    fireEvent.change(within(card()).getByLabelText("Argo CD's read-only user"), { target: { value: "argocd-read" } });
    expect(file("argocd/repository.yaml").textContent).toContain("username: argocd-read");
    fireEvent.change(within(card()).getByLabelText("Argo CD's read-only user"), { target: { value: " " } });
    expect(file("argocd/repository.yaml").textContent).toContain("username: <Argo CD's read-only user>");
  });

  it("fills the files from its fields in this browser alone: never sent, never stored", async () => {
    const kept = vi.fn(); // Node's own localStorage global shadows jsdom's (Fleet.test.tsx): a stub records any write
    vi.stubGlobal("localStorage", { setItem: kept, getItem: () => null, removeItem: () => {}, clear: () => {}, key: () => null, length: 0 });
    await shown();
    expect(within(card()).getByText(S.teamFieldsNote)).toBeDefined();
    const typed = { "Argo CD's read-only user": "argocd-read", "Your platform repo": "https://git.meridian.example/platform/cluster",
      "Its branch": "main", "Where it holds namespaces/": "canyon/namespaces" };
    for (const [label, value] of Object.entries(typed)) fireEvent.change(within(card()).getByLabelText(label), { target: { value } });
    for (const field of within(card()).getAllByRole("textbox")) expect([field.getAttribute("name"), field.closest("form")]).toEqual([null, null]);
    expect(file("argocd/repository.yaml").textContent).toContain("username: argocd-read");
    expect(file("argocd/repository.yaml").textContent).toContain("password: <Argo CD's read-only token>");
    expect(file("argocd/namespaces.yaml").textContent).toContain("- https://git.meridian.example/platform/cluster");
    expect(file("argocd/namespaces.yaml").textContent).toContain("targetRevision: main");
    expect(file("argocd/namespaces.yaml").textContent).toContain("path: canyon/namespaces");
    expect(within(card()).getByRole("heading", { name: "Into your platform repo, under canyon/namespaces" })).toBeDefined();
    fireEvent.click(within(step(2)).getByRole("button", { name: S.check })); // a request after the typing: it carries nothing typed
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([url]) => url === "/api/v1/platform-repo/check")).toBe(true));
    for (const call of vi.mocked(fetch).mock.calls) expect(JSON.stringify(call)).not.toContain("argocd-read");
    expect([kept.mock.calls, sessionStorage.length]).toEqual([[], 0]);
  });

  it("copies each file, and every file at once, and says which landed", async () => {
    const writeText = vi.fn(async (_: string) => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    await shown();
    fireEvent.click(within(card()).getByRole("button", { name: "Copy argocd/repository.yaml" }));
    await waitFor(() => expect(within(card()).getByRole("status").textContent).toBe("argocd/repository.yaml copied"));
    expect(writeText).toHaveBeenLastCalledWith(filled(gitops("argocd/repository.yaml"), repo, window.location.origin));
    expect(within(file("Chart.yaml")).getByRole("button").textContent).toBe("Copy");
    fireEvent.click(within(card()).getByRole("button", { name: "Copy templates/namespace.yaml" }));
    await waitFor(() => expect(within(card()).getByRole("status").textContent).toBe("templates/namespace.yaml copied"));
    expect(writeText).toHaveBeenLastCalledWith(gitops("namespaces/templates/namespace.yaml"));
    fireEvent.change(within(card()).getByLabelText("Where it holds namespaces/"), { target: { value: "canyon/namespaces" } });
    fireEvent.click(within(card()).getByRole("button", { name: "Copy all" }));
    const every = [...applied.map((name) => [name, name] as const), ...chart.map((name) => [`canyon/namespaces/${name}`, `namespaces/${name}`] as const)];
    await waitFor(() => expect(within(card()).getByRole("status").textContent).toBe(`${every.map(([name]) => name).join(", ")} copied`));
    expect(writeText).toHaveBeenLastCalledWith(every.map(([name, path]) => `# ${name}\n${filled(gitops(path), repo, window.location.origin, { PLATFORM_PATH: "canyon/namespaces" })}`).join("---\n"));
    expect(writeText).toHaveBeenCalledTimes(3);
  });

  it("never takes Argo CD's token: the copied YAML keeps its placeholder, and kubectl reads the token from its input", async () => {
    const writeText = vi.fn(async (_: string) => {});
    vi.stubGlobal("navigator", { clipboard: { writeText } });
    await shown();
    expect(within(card()).queryByLabelText("Argo CD's read-only token")).toBeNull();
    expect(card().querySelector("input[type=password]")).toBeNull();
    fireEvent.change(within(card()).getByLabelText("Argo CD's read-only user"), { target: { value: "argocd-read" } });
    fireEvent.click(within(card()).getByRole("button", { name: "Copy argocd/repository.yaml" }));
    await waitFor(() => expect(writeText).toHaveBeenCalled());
    const copied = writeText.mock.calls.at(-1)?.[0] ?? "";
    expect(copied).toContain("username: argocd-read");
    expect(copied).toContain("password: <Argo CD's read-only token>");
    const here = file("argocd/repository.yaml");
    expect(within(here).getByText("Argo CD's read-only token never goes in a file you commit, nor on a command line: create its Secret with kubectl, which reads the token from its input, then label it.")).toBeDefined();
    expect(here.textContent).toContain(`kubectl -n argocd create secret generic canyon-deploy-repo --from-literal=type=git --from-literal=url='${repo.url}' --from-literal=username='argocd-read' --from-file=password=/dev/stdin`);
    expect(here.textContent).toContain("kubectl -n argocd label secret canyon-deploy-repo argocd.argoproj.io/secret-type=repository");
  });

  it("quotes the repo's URL and the user it hands to kubectl: nothing in them runs in the shell it is pasted into", async () => {
    server({ "GET /api/v1/platform-repo": { body: { ...repo, url: "https://git.meridian.example/a';touch x;$(id)`id`/deploy" } } });
    draw();
    await screen.findByRole("region", { name: S.teamTitle });
    fireEvent.change(within(card()).getByLabelText("Argo CD's read-only user"), { target: { value: "u'; rm -rf ~; $(id)" } });
    expect(file("argocd/repository.yaml").textContent).toContain(
      "--from-literal=url='https://git.meridian.example/a'\\'';touch x;$(id)`id`/deploy' --from-literal=username='u'\\''; rm -rf ~; $(id)' --from-file=password=/dev/stdin");
  });

  it("hands kubectl one line joined by &&: a paste feeds no second command to the token's input", async () => {
    await shown();
    const pres = file("argocd/repository.yaml").querySelectorAll("pre");
    const command = pres[pres.length - 1]?.textContent ?? "";
    expect(command).not.toContain("\n");
    expect(command).toContain("--from-file=password=/dev/stdin && kubectl -n argocd label secret canyon-deploy-repo argocd.argoproj.io/secret-type=repository");
  });

  it("a copy the browser refuses says so in place, as one outside a secure context does", async () => {
    vi.stubGlobal("navigator", {}); // no clipboard: a plain-http host
    await shown();
    fireEvent.click(within(card()).getByRole("button", { name: "Copy all" }));
    expect((await within(card()).findByRole("alert")).textContent).toBe("Couldn't copy. The browser did not allow it.");
    expect(within(card()).getByRole("status").textContent).toBe("");
  });
});

describe("the reference set-up, filled in", () => {
  it("is deploy/gitops with this host's values, the CA base64'd, never a token", () => {
    const text = "url: ${REPO_URL}\nroot: ${ROOT}\nsegment: ${APP_SEGMENT}\nstore: ${STORE}\nat: ${CANYON_URL}\n${CA_BUNDLE}\nkeep: ${UNKNOWN}";
    expect(filled(text, { ...release.repo_get, root: "deploy/apps", ca: "PEM" }, "https://canyon.meridian.example")).toBe(
      `url: https://github.com/meridian/deploy\nroot: deploy/apps\nsegment: 2\nstore: canyon\nat: https://canyon.meridian.example\n      caBundle: ${btoa("PEM")}\nkeep: \${UNKNOWN}`);
  });
});
