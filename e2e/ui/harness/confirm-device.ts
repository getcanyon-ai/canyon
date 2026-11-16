// confirm-device <user> <code>: the browser half of `canyon login` for the Go harness
// (e2e/cluster/signin_test.go). Signs the test cluster's <user> in through the provider (signin.ts; the
// password comes from E2E_<USER>_PASSWORD, never argv), types the code on /#/device and approves
// it. Run from e2e/ui: `node harness/confirm-device.ts alice WDJB-MJHT`. Exits non-zero, saying
// why, when the screen does not reach "The CLI is signed in".
import { chromium } from "@playwright/test";
import { signIn } from "./signin.ts";

const [user, code] = process.argv.slice(2);
if (!(user === "admin" || user === "alice" || user === "bob") || !code) {
  console.error("usage: confirm-device <admin|alice|bob> <code>");
  process.exit(2);
}
const canyon = process.env.E2E_CANYON_URL;
if (!canyon) throw new Error("confirm-device needs E2E_CANYON_URL");

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ baseURL: canyon });
  await signIn(page, user);
  await page.goto("/#/device");
  await page.getByLabel("Code").fill(code);
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("region", { name: "The request" }).waitFor({ timeout: 15_000 }); // a wrong or expired code never shows it
  await page.getByRole("button", { name: "Approve" }).click();
  await page.getByRole("heading", { name: "The CLI is signed in" }).waitFor({ timeout: 15_000 });
  console.log(`confirm-device: approved as ${user}`);
} finally {
  await browser.close();
}
