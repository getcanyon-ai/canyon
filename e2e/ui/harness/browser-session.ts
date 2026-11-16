// browser-session <user>: a browser sign-in's session for the Go harness (e2e/cluster/signin_test.go), whose
// requests then come from a browser session: what approving a blocking finding takes (#2204). Signs the test cluster's
// <user> in through the provider (signin.ts) and writes the session cookie's value to stdout, which the
// harness reads and never logs. Run from e2e/ui: `node harness/browser-session.ts admin`.
import { chromium } from "@playwright/test";
import { signIn } from "./signin.ts";

const [user] = process.argv.slice(2);
if (!(user === "admin" || user === "alice" || user === "bob")) {
  console.error("usage: browser-session <admin|alice|bob>");
  process.exit(2);
}
const canyon = process.env.E2E_CANYON_URL;
if (!canyon) throw new Error("browser-session needs E2E_CANYON_URL");

const browser = await chromium.launch();
try {
  const page = await browser.newPage({ baseURL: canyon });
  await signIn(page, user);
  const session = (await page.context().cookies(canyon)).find((c) => c.name === "__Host-canyon_session"); // e2e/cluster/session_test.go sessionCookie
  if (session === undefined) throw new Error(`browser-session: ${user} signed in but holds no session cookie`);
  process.stdout.write(session.value);
} finally {
  await browser.close();
}
