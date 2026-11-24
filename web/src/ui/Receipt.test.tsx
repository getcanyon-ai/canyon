import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { Receipt as ReceiptRead } from "../api";
import { KNOWN_VULN, S } from "../strings";
import { RailContext } from "./Rail";
import { Receipt } from "./Receipt";

// One rule, read twice. The flip belongs to the moment the rule turns, not to the value it holds:
// a checklist that loads already-green has nothing to announce and must sit still.
const receipt = (passed: boolean): ReceiptRead => ({
  org: "Meridian",
  blocked_by: passed ? [] : ["prod.clean-gate"],
  rules: [{
    id: "prod.clean-gate",
    name: "Production needs a passing review",
    blocks: true,
    passed,
    why: passed ? "gr_8f2a ended clean" : "gr_8f2a ended blocked: 1 blocking finding, no approval",
  }],
});
const row = (container: HTMLElement) => container.querySelector("li") as HTMLLIElement;

describe("the go-live checklist", () => {
  it("flips a rule that turns passed, and leaves one that loads passed alone", () => {
    const { container, rerender } = render(<Receipt receipt={receipt(false)} />);
    expect(row(container).className).not.toContain("flip-row");
    rerender(<Receipt receipt={receipt(true)} />);
    expect(row(container).className).toContain("flip-row");
    expect(row(render(<Receipt receipt={receipt(true)} />).container).className).not.toContain("flip-row");
  });

  it("clears the flip when the animation ends, so the next turn can run it again", () => {
    const { container, rerender } = render(<Receipt receipt={receipt(false)} />);
    rerender(<Receipt receipt={receipt(true)} />);
    fireEvent.animationEnd(row(container));
    expect(row(container).className).not.toContain("flip-row");
  });

  it("names each rule as a link to its row under Policies", () => {
    render(<Receipt receipt={receipt(false)} />);
    expect(screen.getByRole("link", { name: "Production needs a passing review" }).getAttribute("href")).toBe("#/guardrails?rule=prod.clean-gate");
  });

  // Guardrails is an admin's screen (design/specs/screens.md, the blocked promote button): a member reads the name, with nowhere to go.
  it("names each rule as plain text to a member", () => {
    render(<RailContext value={{ org: null, viewer: { display: "Sam Reyes", role: "member", github_logins: [], github_link: false }, admin: false }}><Receipt receipt={receipt(false)} /></RailContext>);
    expect(screen.getByText("Production needs a passing review").closest("a")).toBeNull();
    expect(screen.queryByRole("link")).toBeNull();
  });

  // A rule with nothing set to check against is neutral, never a failure, and stays where an admin
  // sees it (contracts/routes.md, decision D4).
  it("draws an unset rule as a grey dot, with no advisory chip", () => {
    const unset: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [{ id: "stack.blessed", name: "Approved stacks only", blocks: false, passed: false, why: "no approved stacks set", unset: true }] };
    const { container } = render(<Receipt receipt={unset} />);
    expect(row(container).textContent).toContain("Nothing to checkApproved stacks onlyno approved stacks set"); // the icon's words, for a screen reader
    expect(row(container).textContent).not.toContain("Not met");
    expect(screen.queryByText("advisory")).toBeNull();
  });

  // Neutral whatever its state: a framed row at blocking is no failure, so it isn't tinted as one, and a row that blocks is.
  it("tints a blocking row that failed, never a neutral one", () => {
    const line = { id: "prod.egress-allowed" as const, name: "Production allows everything preview connects to", blocks: true, passed: false };
    const neutral: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [{ ...line, why: "not checked: preview runs on your own platform", unset: true }] };
    const { container, rerender } = render(<Receipt receipt={neutral} framed />);
    expect(row(container).className).not.toContain("bg-critical-dim");
    rerender(<Receipt receipt={{ ...neutral, blocked_by: [line.id], rules: [{ ...line, why: "production does not allow api.openai.com" }] }} framed />);
    expect(row(container).className).toContain("bg-critical-dim");
  });

  // The receipt answers this preview, never the app's status (contracts/routes.md, canyonv2-5r11.20):
  // with nothing in preview it is the server's one line and no checklist; an advisory miss stops
  // nothing, so it is an amber !, never the ✗ of a rule that blocks.
  it("waits with one line when nothing is in preview, and marks an advisory miss with !", () => {
    const waiting: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [], waiting: "Deploy a new version to preview to promote it." };
    const { container } = render(<Receipt receipt={waiting} />);
    expect(container.textContent).toContain("Deploy a new version to preview to promote it.");
    expect(container.querySelector("li")).toBeNull();
    const advisory: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [{ id: "stack.blessed", name: "Approved stacks only", blocks: false, passed: false, why: "no stack recorded" }] };
    expect(row(render(<Receipt receipt={advisory} />).container).textContent).toContain("Not metApproved stacks only"); // and the advisory chip says the rest
  });

  // The approval page's list is bordered under a heading, a blocking rule tinted red; the app page's is
  // the plain list it was (design/specs/screens.md, the production approval screen).
  it("frames the list on the approval page and leaves the app page's plain", () => {
    const plain = render(<Receipt receipt={receipt(false)} />).container;
    expect(plain.querySelector("h2")).toBeNull();
    expect(plain.querySelector("ul")?.className ?? "").not.toContain("border");
    expect(row(plain).className).toContain("py-1");
    expect(row(plain).className).not.toContain("bg-critical-dim");
    const framed = render(<Receipt receipt={receipt(false)} framed />).container;
    expect(framed.querySelector("h2")?.textContent).toBe("Go-live checklist");
    expect(framed.querySelector("ul")?.className).toContain("border");
    expect(row(framed).className).toContain("bg-critical-dim");
    expect(row(render(<Receipt receipt={receipt(true)} framed />).container).className).not.toContain("bg-critical-dim");
  });

  // platform.known-vuln's row as the test cluster answered it for v0.1.13, trimmed: a count alone told nobody what to
  // update (#2300 item 4), and its icon sat on a line of its own above its long name.
  const knownVuln: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [{
    id: "platform.known-vuln", name: "No known-vulnerable system libraries or build tools", blocks: false, passed: false,
    why: "2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 18 notes: no fix published, or Canyon can't tell what put it there",
    packages: [
      { name: "brace-expansion", version: "5.0.9", advisories: [{ id: "CVE-2026-102276", fixed: "5.0.10, 3.0.7, 2.1.5, 1.1.19" }, { id: "CVE-2026-102278", fixed: "5.0.11, 3.0.8, 2.1.6, 1.1.20" }] },
      { name: "openssl", version: "3.0.13-0ubuntu3.15", advisories: [{ id: "CVE-2026-84782", fixed: "3.0.13-0ubuntu3.16" }] },
    ],
  }] };

  it("names every package platform.known-vuln counts, each advisory with what fixes it, folded under the row", () => {
    const { container } = render(<Receipt receipt={knownVuln} />);
    const details = row(container).querySelector("details") as HTMLDetailsElement;
    expect(details.querySelector("summary")?.textContent).toBe("See the 2 packages");
    expect([...details.querySelectorAll("ul ul li")].map((one) => one.textContent)).toEqual([
      "CVE-2026-102276 fixed in 5.0.10, 3.0.7, 2.1.5, 1.1.19", "CVE-2026-102278 fixed in 5.0.11, 3.0.8, 2.1.6, 1.1.20", "CVE-2026-84782 fixed in 3.0.13-0ubuntu3.16"]);
    expect(details.textContent).toContain("brace-expansion 5.0.9");
    expect(details.textContent).toContain("openssl 3.0.13-0ubuntu3.15");
    const [rule] = knownVuln.rules as [ReceiptRead["rules"][number]];
    const one: ReceiptRead = { ...knownVuln, rules: [{ ...rule, packages: rule.packages?.slice(1) ?? [] }] };
    expect(row(render(<Receipt receipt={one} />).container).querySelector("summary")?.textContent).toBe("See the package");
    expect(row(render(<Receipt receipt={receipt(false)} />).container).querySelector("details")).toBeNull();
  });

  // The test cluster's patch approval of patch-probe, trimmed (#2579): the row's old name said nowhere where, stdlib nowhere that it
  // is Go's, three versions repeated one CVE, and nothing said what fixes each. A contributor's receipt names each
  // package's layers, so the row says where they are, groups them by what fixes them, and names each package once.
  const where: ReceiptRead = { org: "Meridian", blocked_by: [], rules: [{
    id: "platform.known-vuln", name: "No known-vulnerable system libraries or build tools", blocks: false, passed: false, read_at: "2026-10-30T09:00:00Z", notes: 14,
    why: "4 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 14 notes: no fix published, or Canyon can't tell what put it there",
    packages: [
      { name: "libbrotli1", version: "1.1.0-2build2", advisories: [{ id: "CVE-2025-6176", fixed: "1.1.0-2ubuntu0.1" }], layers: [{ owner: "run-image", class: "os" }] },
      { name: "stdlib", version: "v1.26.5", go_stdlib: true, advisories: [{ id: "CVE-2026-33818", fixed: "1.26.9, 1.27.2" }, { id: "CVE-2026-33819", fixed: "1.26.9, 1.27.2" }],
        layers: [{ owner: "lifecycle", class: "lifecycle" }, { owner: "paketo-buildpacks/ca-certificates", class: "runtime" }] },
      { name: "stdlib", version: "v1.26.8", go_stdlib: true, advisories: [{ id: "CVE-2026-33818", fixed: "1.26.9, 1.27.2" }], layers: [{ owner: "paketo-buildpacks/node-engine", class: "runtime" }] },
      { name: "stdlib", version: "v1.27.1", go_stdlib: true, advisories: [{ id: "CVE-2026-33818", fixed: "1.26.9, 1.27.2" }], layers: [{ owner: "app", class: "runtime" }] },
    ],
  }] };

  it("says where the packages are, what fixes each, and names a package once however many versions the image holds", () => {
    const { container } = render(<Receipt receipt={where} />);
    const body = row(container);
    expect(body.textContent).toContain(KNOWN_VULN.where);
    const details = body.querySelector("details") as HTMLDetailsElement;
    expect(details.querySelector("summary")?.textContent).toBe("See the 4 packages");
    const sections = [...details.querySelectorAll("details > div")].map((one) => [one.querySelector("p")?.textContent, ...[...one.querySelectorAll(":scope > ul > li")].map((li) => li.firstChild?.textContent + (li.children[1]?.tagName === "SPAN" ? li.children[1].textContent : ""))]);
    expect(sections).toEqual([
      ["No fix from Canyon yet", "libbrotli1 1.1.0-2build2 · in the system libraries"],
      ["Waits for Canyon's next build tools update",
        "Go's standard library v1.26.5, v1.26.8, v1.27.1 · in the app launcher, paketo-buildpacks/ca-certificates, paketo-buildpacks/node-engine, the Go version Canyon built the app with"],
    ]);
    expect([...details.querySelectorAll("ul ul li")].map((one) => one.textContent)).toEqual([
      "CVE-2025-6176 fixed in 1.1.0-2ubuntu0.1", "CVE-2026-33818 fixed in 1.26.9, 1.27.2", "CVE-2026-33819 fixed in 1.26.9, 1.27.2"]); // each advisory once
    expect(details.textContent).not.toContain("stdlib");
  });

  // The test cluster's Node app on preview (#2579): beside a patch state waiting on Ubuntu, the row led with npm and Go packages of the
  // Node buildpack's and the lifecycle's layers. Those wait for a build tools update, never the system libraries'.
  it("puts a buildpack's and the lifecycle's packages under the build tools update, never the system libraries'", () => {
    const node: ReceiptRead = { ...where, rules: [{ ...where.rules[0]!, packages: [
      { name: "tar", version: "7.5.11", advisories: [{ id: "CVE-2026-23745", fixed: "7.5.12" }], layers: [{ owner: "paketo-buildpacks/node-engine", class: "runtime" }] },
      { name: "brace-expansion", version: "2.0.2", advisories: [{ id: "CVE-2026-102276", fixed: "2.0.3" }], layers: [{ owner: "paketo-buildpacks/node-engine", class: "runtime" }] },
      { name: "github.com/containerd/containerd/v2", version: "v2.3.3", advisories: [{ id: "CVE-2026-31337", fixed: "2.3.4" }], layers: [{ owner: "lifecycle", class: "lifecycle" }] },
      { name: "libbrotli1", version: "1.1.0-2build2", advisories: [{ id: "CVE-2025-6176", fixed: "1.1.0-2ubuntu0.1" }], layers: [{ owner: "run-image", class: "os" }] },
    ] }] };
    const details = row(render(<Receipt receipt={node} />).container).querySelector("details") as HTMLDetailsElement;
    const under = (heading: string) => [...details.querySelectorAll("details > div")].find((one) => one.querySelector("p")?.textContent === heading);
    const names = (section: Element | undefined) => [...(section?.querySelectorAll(":scope > ul > li") ?? [])].map((li) => li.firstChild?.textContent);
    expect(names(under(KNOWN_VULN.osFix))).toEqual(["libbrotli1 1.1.0-2build2"]);
    expect(names(under(KNOWN_VULN.toolsFix))).toEqual(["tar 7.5.11", "brace-expansion 2.0.2", "github.com/containerd/containerd/v2 v2.3.3"]);
  });

  // #2594: a system libraries package has no fix from Canyon only while no patch of it is planned. While preview's
  // patch is planned the system libraries Canyon uses now fix it, so the heading says Canyon is trying a fix; once the patch is in
  // preview the receipt reads the patched image, and what it still counts does wait. The build tools' heading never moves.
  it.each([
    ["planned", KNOWN_VULN.osPatching],
    ["wait", KNOWN_VULN.osFix],
    ["could_not_rescan", KNOWN_VULN.osFix],
    ["patched_preview", KNOWN_VULN.osFix],
    [undefined, KNOWN_VULN.osFix],
  ] as const)("heads a system libraries package by preview's patch state: %s", (state, heading) => {
    const patch = state === undefined ? undefined : { state, since: "2026-10-30T09:00:00Z" };
    const details = row(render(<Receipt receipt={where} patch={patch} />).container).querySelector("details") as HTMLDetailsElement;
    expect([...details.querySelectorAll("details > div")].map((one) => one.querySelector("p")?.textContent)).toEqual([heading, KNOWN_VULN.toolsFix]);
  });

  // A responder with no grant reads each package as #2300 had it (design/PERMISSIONS.md): no layers, so nothing says what fixes
  // it nor where, and a package is named as the receipt names it.
  it("groups a receipt with no layers by name alone, under no heading", () => {
    const [rule] = where.rules as [ReceiptRead["rules"][number]];
    const bare: ReceiptRead = { ...where, rules: [{ ...rule, packages: rule.packages?.map(({ name, version, advisories }) => ({ name, version, advisories })) ?? [] }] };
    const details = row(render(<Receipt receipt={bare} />).container).querySelector("details") as HTMLDetailsElement;
    expect(details.querySelector("p")).toBeNull();
    expect([...details.querySelectorAll(":scope > div > ul > li")].map((li) => li.firstChild?.textContent)).toEqual(["libbrotli1 1.1.0-2build2", "stdlib v1.26.5, v1.26.8, v1.27.1"]);
    expect(details.textContent).not.toContain(" · in ");
  });

  it("keeps every row's icon in its own column, beside the name however long", () => {
    const { container } = render(<Receipt receipt={knownVuln} />);
    expect(row(container).className).toContain("grid-cols-[14px_minmax(0,1fr)]");
    const [icon, body] = [...row(container).children] as [Element, Element];
    expect(icon.querySelector("svg")).not.toBeNull();
    expect(body.textContent).toContain("No known-vulnerable system libraries or build tools");
    expect(body.querySelector("svg")).toBeNull();
  });

  // design/EXPERIENCE.md D5 (#2384): platform.known-vuln is beta in v0.2.0, its row marked beside its name; no other row is.
  it("marks platform.known-vuln's row Beta, with what beta means, and no other row", () => {
    const advisory = { id: "owner.required", name: "Every app has an owner", blocks: false, passed: false, why: "nobody owns claims-portal" };
    const all: ReceiptRead = { ...knownVuln, blocked_by: ["prod.clean-gate"], rules: [...receipt(false).rules, advisory, ...knownVuln.rules] };
    const { container } = render(<Receipt receipt={all} />);
    expect(screen.getAllByText(S.beta)).toHaveLength(1);
    const beta = [...container.querySelectorAll("li")].find((one) => one.textContent?.includes(knownVuln.rules[0]!.name))!;
    expect(within(beta).getByText(S.beta).textContent).toBe(`${S.beta}: ${S.betaWhy}`);
    expect(screen.getAllByText("advisory")).toHaveLength(2); // the advisory rule is drawn, and only known-vuln's row is Beta
  });

  // A package only the built app holds is approved as a blocking finding is (#2610): its go-live check lists each one
  // deps.known-vuln counts, with what fixes each, and where the page passes an approval — to whom the review's run read
  // lets approve — each not yet approved has it, one reason and expiry for every fingerprint of it; without one, none,
  // and an approved one says so.
  const builtApp: ReceiptRead = {
    org: "Meridian",
    blocked_by: ["deps.known-vuln"],
    rules: [{
      id: "deps.known-vuln", name: "No known-vulnerable dependencies", blocks: true, passed: false,
      why: "2 known-vulnerable packages are in v1.4's built app, but its review didn't see them: lodash 4.17.20 and 1 more",
      built_app: [
        { name: "lodash", version: "4.17.20", advisories: [{ id: "CVE-2021-23337", fixed: "4.17.21" }], fingerprints: [`f_${"a".repeat(64)}`, `f_${"b".repeat(64)}`], approved: false },
        { name: "left-pad", version: "1.3.0", advisories: [{ id: "CVE-2099-0001", fixed: "" }], fingerprints: [`f_${"c".repeat(64)}`], approved: false },
        { name: "urllib3", version: "1.26.5", advisories: [{ id: "CVE-2023-43804", fixed: "1.26.17, 2.0.6" }], fingerprints: [`f_${"d".repeat(64)}`], approved: true },
      ],
    }],
  };
  const as = (role: "member" | "responder" | "admin", send?: (fingerprints: string[], body: unknown) => Promise<void>) => render(
    <RailContext value={{ org: null, viewer: { display: "Sam Reyes", role, github_logins: [], github_link: false }, admin: role === "admin" }}>
      <Receipt receipt={builtApp} approveBuiltApp={send === undefined ? undefined : { org: "Meridian", send }} />
    </RailContext>);

  it("lists the built app's packages with what fixes each, and marks an approved one", () => {
    as("member");
    for (const line of ["lodash 4.17.20", "left-pad 1.3.0", "urllib3 1.26.5", "CVE-2021-23337 fixed in 4.17.21", "CVE-2099-0001 · no fix published", "CVE-2023-43804 fixed in 1.26.17, 2.0.6"]) {
      expect(screen.getByText(line)).toBeTruthy();
    }
    const urllib3 = screen.getByText("urllib3 1.26.5").closest("li")!;
    expect(within(urllib3).getByText("approved")).toBeTruthy();
  });

  it("offers no approval of the built app's packages where the page passes none", () => {
    as("member");
    expect(screen.queryByRole("button", { name: "Approve with a reason" })).toBeNull();
  });

  it("lets an admin or a responder approve a package with every fingerprint of it, never an approved one", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(new Date("2026-11-02T12:00:00Z"));
    for (const role of ["admin", "responder"] as const) {
      const sent: unknown[] = [];
      as(role, async (fingerprints, body) => { sent.push({ fingerprints, ...(body as object) }); });
      expect(screen.getAllByRole("button", { name: "Approve with a reason" })).toHaveLength(2); // lodash and left-pad; urllib3 is approved
      fireEvent.click(within(screen.getByText("lodash 4.17.20").closest("li")!).getByRole("button", { name: "Approve with a reason" }));
      fireEvent.change(screen.getByLabelText("Reason"), { target: { value: "  the SDK's own fixture  " } });
      fireEvent.click(screen.getByRole("button", { name: "Approve" }));
      await vi.waitFor(() => expect(sent).toEqual([{ fingerprints: [`f_${"a".repeat(64)}`, `f_${"b".repeat(64)}`], reason: "the SDK's own fixture", expires_at: "2026-12-02T23:59:59Z" }]));
      cleanup();
    }
    vi.useRealTimers();
  });
});
