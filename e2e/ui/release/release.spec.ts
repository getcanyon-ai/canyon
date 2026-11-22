// v0.11.0's new screens as each person sees them, which e2e/oidc/release_test.go runs once per person with their session
// (never make e2e-ui): levels need sign-in on. One screenshot per screen and person into .work/walk, the oidc-walk
// artifact, and what design/PERMISSIONS.md lets that person see on it, and not.
import { expect, test, type Page } from "@playwright/test";

const env = (name: string) => {
  const value = process.env[name];
  if (!value) throw new Error(`${name}: set by e2e/oidc/release_test.go`);
  return value;
};
const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

test.beforeEach(async ({ context }) => {
  const name = env("RELEASE_COOKIE_NAME");
  await context.addCookies([{ name, value: env("RELEASE_COOKIE_VALUE"), url: env("E2E_CANYON_URL"), secure: name.startsWith("__"), httpOnly: true, sameSite: "Lax" }]);
});

const walked = (page: Page, screen: string, who: string) => page.screenshot({ path: `.work/walk/release-${screen}-${who}.png`, fullPage: true });
const members = ["owner", "contributor", "viewer", "none"] as const;
const deciders = ["owner", "contributor", "admin"];

// Guardrails › Playbooks: an admin's screen (design/specs/screens.md, A4b); everyone else reads the admins-only page.
async function playbooks(page: Page, who: string) {
  await page.goto("/#/guardrails/playbooks");
  if (who !== "admin") {
    await expect(page.getByText("Only an admin can open Guardrails.")).toBeVisible();
    await expect(page.getByRole("button", { name: "New playbook" })).toHaveCount(0);
    await expect(page.getByText(env("RELEASE_PLAYBOOK"))).toHaveCount(0);
    await walked(page, "playbooks", who);
    return;
  }
  const name = env("RELEASE_PLAYBOOK");
  const card = page.getByRole("region", { name, exact: true });
  await expect(card).toContainText(`${env("RELEASE_SLUG")} · version 1`);
  await expect(card).toContainText("reads in 30 days · on 0 machines");
  await walked(page, "playbooks", who);
  await card.getByRole("button", { name: `Edit ${name}` }).click();
  await expect(page.getByLabel("Body", { exact: true })).toHaveValue(/^# How the release walk builds/);
  await page.getByRole("tab", { name: "Preview" }).click();
  await expect(page.getByLabel("Preview").getByRole("heading", { name: "How the release walk builds" })).toBeVisible();
  await walked(page, "playbook-editor", who);
  await page.getByRole("button", { name: "Cancel" }).click();
  await card.getByRole("button", { name: `History of ${name}` }).click();
  await expect(page.getByRole("region", { name: `History of ${name}` })).toContainText("Version 1 · ");
  await walked(page, "playbook-history", who);
  await page.getByRole("button", { name: "Close" }).click();
  await page.getByRole("button", { name: "Import a SKILL.md" }).click();
  await page.getByLabel("Or paste it").fill("---\nname: Release checklist\ndescription: Read before you promote an app.\n---\n\n# Before you promote\n");
  await page.getByRole("button", { name: "Read the SKILL.md" }).click();
  await expect(page.getByRole("region", { name: "New playbook" }).getByLabel("Slug")).toHaveValue("release-checklist");
  await walked(page, "playbook-import", who); // nothing is saved: the editor opens on it unsaved
}

// The custom rule editor's Playbook picker: Guardrails is an admin's, so the admin alone has it.
async function picker(page: Page) {
  await page.goto("/#/guardrails/policies");
  const rule = page.getByRole("region", { name: env("RELEASE_RULE"), exact: true });
  await expect(rule).toContainText(`Its findings tell the agent to read the playbook ${env("RELEASE_SLUG")}.`);
  await rule.getByRole("button", { name: "Edit" }).click();
  await expect(page.getByLabel("Playbook")).toHaveValue(env("RELEASE_SLUG"));
  await page.getByLabel("Playbook").scrollIntoViewIfNeeded();
  await walked(page, "rule-playbook", "admin");
}

// The app page's finding whose rule points at the playbook: technical detail, contributor and above (and an admin).
async function finding(page: Page, who: string) {
  await page.goto(`/#/apps/${env("RELEASE_APP")}`);
  const step = page.getByText(`Read the playbook ${env("RELEASE_PLAYBOOK")}: canyon playbook ${env("RELEASE_SLUG")}`);
  if (who === "none") {
    await expect(page).toHaveURL(/#\/no-access\//);
    await expect(step).toHaveCount(0);
  } else if (who === "viewer") {
    await expect(page.getByText("Your access: read-only.")).toBeVisible();
    await expect(page.getByText(/^Every app carries RELEASE-WALK\.md/)).toHaveCount(0); // no finding: technical detail is contributor and above
    await expect(step).toHaveCount(0);
  } else {
    await expect(step).toBeVisible();
    // With production's approval asked, the page offers its one step, Review and approve, never a second Promote.
    await expect(page.getByText("Review and approve", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Promote \S+ to production$/ })).toHaveCount(0);
    await step.scrollIntoViewIfNeeded();
  }
  await walked(page, "finding", who);
}

// The production approval page: a person at contributor or above, or an admin, decides; anyone else is refused the read.
async function approval(page: Page, who: string, screen: string, question: RegExp) {
  await page.goto(`/#/approve?code=${env("RELEASE_APPROVAL")}`);
  if (deciders.includes(who)) {
    await expect(page.getByRole("heading", { name: question })).toBeVisible();
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Deny", exact: true })).toBeVisible();
  } else {
    // Refused, the page can't tell a promotion from a put-back: its heading names neither, and the line under it says the
    // button that asks, on the app's page, with its full stop.
    await expect(page.getByRole("heading", { name: "A request to change production" })).toBeVisible();
    await expect(page.getByRole("heading", { name: /^Promote / })).toHaveCount(0);
    await expect(page.getByText(who === "viewer" ? /'s team: on its page in Canyon, use Ask to join its team\.$/ : /'s team: on its page in Canyon, use Ask for access\.$/)).toBeVisible();
    await expect(page.getByRole("heading", { name: question })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Approve", exact: true })).toHaveCount(0);
  }
  await walked(page, screen, who);
}

// Settings › People & sign-in: an admin's to change; a member reads it, and not the responders.
async function people(page: Page, who: string) {
  await page.goto("/#/settings/people");
  await expect(page.getByRole("region", { name: "Who may sign in" })).toBeVisible();
  if (who === "admin") {
    await expect(page.getByRole("region", { name: "Security responders" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Save people and sign-in" })).toBeVisible();
  } else {
    await expect(page.getByText("An admin changes these settings.")).toBeVisible();
    await expect(page.getByRole("region", { name: "Security responders" })).toHaveCount(0);
    await expect(page.getByRole("button", { name: "Save people and sign-in" })).toHaveCount(0);
  }
  await walked(page, "people", who);
}

// An MCP client's consent page, unanswered: at read it names only seeing, at act seeing and acting, and each names the
// person who opened it, never anyone else.
async function consentPage(page: Page, who: string, request: string, scope: "read" | "act") {
  await page.goto(`/#/oauth?request=${request}`);
  const agent = env("RELEASE_AGENT");
  await expect(page.getByRole("heading", { name: `Allow ${agent} to use Canyon as you?` })).toBeVisible();
  await expect(page.getByText(new RegExp(`^${escape(agent)} will act as .* \\(${escape(env("RELEASE_EMAIL"))}\\) on `))).toBeVisible();
  const may = page.getByRole("region", { name: "What it may do" });
  await expect(may.getByText(READ_SCOPE)).toBeVisible();
  await expect(may.getByText(ACT_SCOPE)).toHaveCount(scope === "act" ? 1 : 0);
  await expect(page.getByText("It can do only what you can. It can't approve findings, change settings or sharing, or see secret values.")).toBeVisible();
  await expect(page.getByRole("button", { name: "Allow", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Don't allow", exact: true })).toBeVisible();
  await walked(page, `consent-${scope}`, who);
}
const READ_SCOPE = "See your org's rules, and the apps you can see: their status, and logs where you're a contributor";
const ACT_SCOPE = "Check, deploy and promote the apps you contribute to";

// Settings › People & sign-in: the Connect an agent panel, the same for everyone, then Sessions with the person's own
// agent listed and Disconnect, which ends it. Only an admin sees everyone's sessions, and the rail's Guardrails (#52).
async function sessions(page: Page, who: string) {
  await page.goto("/#/settings/people");
  const connect = page.getByRole("region", { name: "Connect an agent" });
  await expect(connect.getByRole("region", { name: "On your laptop" }).getByText(`canyon login ${env("E2E_CANYON_URL")}`, { exact: true })).toBeVisible();
  await expect(connect.getByRole("textbox", { name: "Canyon's address for agents", exact: true })).toHaveValue(`${env("E2E_CANYON_URL")}/mcp`);
  await expect(connect.getByText("Claude Code on the web", { exact: true })).toBeVisible();
  await expect(connect.getByText("claude mcp add", { exact: false })).toHaveCount(0);
  // directly above Sessions, so an agent is connected and disconnected in one place (screens.md, Connect an agent)
  expect(await connect.evaluate((panel) => panel.nextElementSibling?.getAttribute("aria-label"))).toBe("Sessions");
  await connect.scrollIntoViewIfNeeded();
  await walked(page, "connect", who);
  await expect(page.getByRole("navigation", { name: "Canyon" }).getByRole("link", { name: "Guardrails" })).toHaveCount(who === "admin" ? 1 : 0);

  const list = page.getByRole("region", { name: "Sessions" });
  const mine = list.getByRole("row").filter({ hasText: `${env("RELEASE_AGENT")} (MCP)` });
  await expect(mine).toHaveCount(1);
  await expect(list.getByRole("columnheader", { name: "Person" })).toHaveCount(who === "admin" ? 1 : 0);
  // someone else's sessions: the owner's, to the admin alone; nobody else's agent, to anyone but the admin
  if (who === "admin") await expect(list.getByRole("cell", { name: env("RELEASE_OWNER_EMAIL"), exact: true }).first()).toBeVisible();
  else await expect(list.getByRole("row").filter({ hasText: /Release walk agent / }).filter({ hasNotText: env("RELEASE_AGENT") })).toHaveCount(0);
  await list.scrollIntoViewIfNeeded();
  await walked(page, "sessions", who);
  await mine.getByRole("button", { name: /^Disconnect / }).click();
  await expect(mine).toHaveCount(0);
  await walked(page, "sessions-disconnected", who);
}

// The app page a secret link opens (canyon_secret_link): for a contributor and above the Secrets form on that secret and
// environment, its value's field focused and empty, whatever the link says of a value; read-only for a viewer, and the
// no-access page for no access.
async function secretLink(page: Page, who: string) {
  await page.goto(`/#/apps/${env("RELEASE_APP")}/secrets?name=RELEASE_WALK_TOKEN&env=prod&value=${LINKED}`);
  const form = page.getByRole("form", { name: "Set a secret" });
  if (who === "none") {
    await expect(page).toHaveURL(/#\/no-access\//);
    await expect(form).toHaveCount(0);
  } else if (who === "viewer") {
    await expect(page.getByText("Your access: read-only.")).toBeVisible();
    await expect(page.getByRole("region", { name: "Secrets" })).toHaveCount(0);
    await expect(form).toHaveCount(0);
  } else {
    await expect(form.getByLabel("Name")).toHaveValue("RELEASE_WALK_TOKEN");
    await expect(form.getByLabel("Environment")).toHaveValue("prod");
    await expect(form.getByLabel("Value")).toBeFocused();
    await expect(form.getByLabel("Value")).toHaveValue("");
    // the review beside it read and said, never a heading over white space (the v0.11.0 role walk)
    await expect(page.getByRole("region", { name: "Review" }).getByText(/^nothing blocking/)).toBeVisible();
    await expect(page.getByText("Reading the review…")).toHaveCount(0);
  }
  expect(await page.content()).not.toContain(LINKED);
  await walked(page, "secret-link", who);
}
const LINKED = "sk_test_51Hlinked000000000000000000"; // a synthetic shape a link may carry, never taken as a value

for (const who of [...members, "admin"]) {
  test(`the agent screens as ${who}`, async ({ page }) => {
    await consentPage(page, who, env("RELEASE_CONSENT_READ"), "read");
    await consentPage(page, who, env("RELEASE_CONSENT_ACT"), "act");
    await sessions(page, who);
    await secretLink(page, who);
  });
}

for (const who of [...members, "admin"]) {
  test(`the release screens as ${who}`, async ({ page }) => {
    await playbooks(page, who);
    if (who === "admin") await picker(page);
    await finding(page, who);
    await approval(page, who, "approval-version", new RegExp(`^Promote ${escape(env("RELEASE_APP"))} \\S+ to production\\?$`));
    await people(page, who);
  });

  test(`the put-back question as ${who}`, async ({ page }) => {
    await approval(page, who, "approval-put-back", new RegExp(`^Put ${escape(env("RELEASE_APP"))} ${escape(env("RELEASE_PUT_BACK"))} back in production, replacing \\S+\\?$`));
  });
}
