import { expect, test, type Page } from "@playwright/test";
import { journey } from "../harness/factory";
import { api, appId, deploy, uploadVersion } from "../harness/seed";
import { stillness } from "../harness/still";

// design/EGRESS.md §11's layer-2 flow on the stack, beside the test cluster's O2 (e2e/cluster/CASES.md): an app's call out is
// refused under an empty allowlist, the refusal shows on its Outbound card and in Needs you, a person clicks Allow on the
// card, and the app's next call goes through with no deploy. The stack runs CANYON_AUTH=off, so its one caller is an
// admin, who allows as a contributor does.
//
// The app is e2e/fixtures/outbound-app, calling the stack's echo (e2e/echo, e2e/compose.e2e.yml) every 10 s through
// Envoy, as Canyon routes an app's calls once the host's via is canyon. The echo's certificate is no app's to trust, so
// the app itself never gets an answer: what Canyon records of each call, refused or allowed, is what this reads.

const TARGET = "echo.canyon-floor.test";
type Setting = { via: "off" | "canyon"; mode: "observe" | "allowlist" };
type Destination = { destination: string; last_outcome: "allowed" | "refused"; last_seen_at: string };
type Read = { envs: { preview: { destinations: Destination[]; requests: { destination: string; state: string }[] } } };

let made: string | null = null; // the journey's app, deleted after it however it ended: it calls out every 10 s
// The stack's setting as every other journey expects it: via off. Before the journey and after it however it ended.
async function putBack() {
  const was = await api<Setting>(null, "GET", "/api/v1/egress");
  if (was.via !== "off") await api(null, "PUT", "/api/v1/egress", { via: "off" });
}
// After the journey, however it ended: the app deleted, via off, and the stack still. The app's last calls reach its records
// for some seconds after it is gone (the receiver's flush, its caller's row), and the next journey on this shard declares
// none of them. A DELETE that fails still fails the test, but never before via is put back.
test.afterEach(async () => {
  try {
    if (made !== null) await api(null, "DELETE", `/api/v1/projects/${made}?confirm=${made}`); // its resources go with it, none here
  } finally {
    made = null;
    await putBack();
    await stillness(180_000, "after the journey's app was deleted");
  }
});

const read = (app: string) => api<Read>(null, "GET", `/api/v1/projects/${app}/egress`);
const preview = (page: Page) => page.getByRole("region", { name: "Preview outbound" });
// The app calls out every 10 s through every step, and each call moves its records, its caller row's last seen among them:
// never a step's own change.
const calling = { egress_destinations: { added: "any", changed: "any" }, egress_callers: { added: "any", changed: "any" }, egress_requests: { added: "any", changed: "any", removed: "any" } } as const;
// A change answers once every gateway replica holds it (contracts/routes.md, A change waits for the gateway): up to 30 s.
const taken = { timeout: 35_000 };

journey("allow a refused call out, and the next call goes through with no deploy", {
  given: async () => {
    await putBack();
    await api(null, "PUT", "/api/v1/egress", { via: "canyon" }); // the app's calls go through Canyon from its first /run
    const app = appId("ux-allow");
    await api(null, "POST", "/api/v1/projects", { id: app });
    made = app;
    await uploadVersion(app, "f-outbound", { "target.txt": `https://${TARGET}/\n` });
    await deploy(app);
    // Preview narrowed to an empty allowlist, as its owner may from any session: the app's next call is refused and files
    // an egress request, which waits on a person.
    await api(null, "PUT", `/api/v1/projects/${app}/egress/preview/mode`, { mode: "allowlist" });
    await expect.poll(async () => (await read(app)).envs.preview.requests.some((one) => one.destination === TARGET && one.state === "pending"), { timeout: 90_000 }).toBe(true);
    return { app };
  },
  steps: [
    {
      name: "the refused call waits on the app's Outbound card, with Allow",
      do: async (page, { app }) => { await page.goto(`/#/apps/${app}/outbound`); },
      see: async (page) => {
        await expect(preview(page).getByText("Needs a person")).toBeVisible();
        await expect(preview(page).getByRole("button", { name: `Allow ${TARGET}` })).toBeVisible();
      },
      changes: calling,
    },
    {
      name: "and in Needs you on the fleet",
      // The box shows the most urgent first and folds the rest under "Show <n> more": a preview refusal, rank 7, may be folded.
      // Until it has read its lines that is a link to the whole list; once it has, a button that unfolds them in place.
      do: async (page, { app }) => {
        await page.goto("/#/fleet");
        const needs = page.getByRole("region", { name: /^Needs (attention|you)$/ });
        const more = needs.getByRole("button", { name: /^Show \d+ more$/ });
        await expect(needs.getByText(`${app} can't reach ${TARGET} in preview.`).or(more).first()).toBeVisible();
        if (await more.isVisible()) await more.click();
      },
      see: async (page, { app }) => {
        const needs = page.getByRole("region", { name: /^Needs (attention|you)$/ });
        await expect(needs.getByText(`${app} can't reach ${TARGET} in preview.`)).toBeVisible(); // its title, D5's "<app> can't reach <destination> in <env>."
        await expect(needs.getByRole("button", { name: `Allow ${TARGET} in preview for ${app}` })).toBeVisible();
      },
      changes: calling,
    },
    {
      name: "allow it on the card",
      do: async (page, { app }) => {
        await page.goto(`/#/apps/${app}/outbound`);
        await preview(page).getByRole("button", { name: `Allow ${TARGET}` }).click();
      },
      see: async (page) => { await expect(preview(page).getByText(`Allowed ${TARGET}`)).toBeVisible(taken); },
      changes: { ...calling, egress_allowed: { added: 1 }, egress_policy: { changed: 1 }, audit_events: { added: 1, actions: ["egress.allow"] } },
    },
    {
      // No deploy: the step declares no container, run or deploy, so one started fails it.
      name: "the app's next call goes through, with no deploy",
      do: async (page) => { await page.reload(); },
      see: async (page, { app }) => {
        const since = Date.now();
        await expect.poll(async () => {
          const call = (await read(app)).envs.preview.destinations.find((one) => one.destination === TARGET);
          return call !== undefined && Date.parse(call.last_seen_at) >= since - 1_000 ? call.last_outcome : "not called since";
        }, { timeout: 40_000 }).toBe("allowed");
        await page.reload();
        await expect(preview(page).getByRole("button", { name: `Allow ${TARGET}` })).toHaveCount(0);
      },
      changes: calling,
    },
  ],
});
