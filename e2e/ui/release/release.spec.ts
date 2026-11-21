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
    await expect(page.getByRole("heading", { name: "Promote this app to production?" })).toBeVisible();
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
