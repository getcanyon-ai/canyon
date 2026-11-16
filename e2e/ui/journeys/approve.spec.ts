import { expect } from "@playwright/test";
import { journey } from "../harness/factory";
import { appId, review, upload } from "../harness/seed";

const approve = "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve";
const inDays = (n: number) => new Date(Date.now() + n * 864e5).toISOString().slice(0, 10);

journey("approve a blocking finding with a reason", {
  given: async () => {
    const app = appId("ux-approve");
    await upload(app, "f-cp");
    return { app, run: await review(app) };
  },
  steps: [
    {
      name: "open the blocked app",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}`); },
      see: async (page) => {
        await expect(page.getByRole("main")).toContainText("Blocked");
        await expect(page.getByRole("button", { name: "Approve with a reason", exact: true })).toBeVisible(); // on the finding's own card (#581)
      },
    },
    {
      name: "approve the Stripe key with a reason and an expiry",
      do: async (page) => {
        await page.getByRole("button", { name: "Approve with a reason", exact: true }).click();
        await expect(page.getByRole("button", { name: "Approve", exact: true })).toBeDisabled();
        await page.getByLabel("Reason", { exact: true }).fill("UX journey: the key is a test fixture");
        await page.getByLabel("Approval expires").fill(inDays(30));
        await page.getByRole("button", { name: "Approve", exact: true }).click();
      },
      see: async (page) => {
        await expect(page.getByText(/approved by .* until/).first()).toBeVisible();
        await expect(page.getByRole("status")).toContainText("Approved — 1 blocking finding, approved by");
      },
      changes: { approvals: { added: 1 }, audit_events: { added: 1, actions: [approve] } },
    },
    {
      name: "reload: the approval stays on the finding with its reason",
      do: async (page) => { await page.reload(); },
      see: async (page) => {
        await expect(page.getByText(/approved by .* until/).first()).toBeVisible();
        await expect(page.getByText("UX journey: the key is a test fixture").first()).toBeVisible();
      },
    },
  ],
});
