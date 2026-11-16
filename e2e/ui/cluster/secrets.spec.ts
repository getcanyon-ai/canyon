// K1's app half (e2e/cluster/CASES.md, K — Secrets): prod's /secret shows sha256 of the value the
// Go harness just set and promoted (e2e/cluster/secrets_test.go rotate), handed here as
// E2E_K1_VALUE, never on argv. The value is read once, hashed, and nothing else in this file holds
// it: `want` is the hash and `saw` the page's digest, so no assertion message can carry it. That is
// the whole protection here -- the Go side's end-of-run grep reads the Go process's output, where
// the raw value is handled, not this one's. The suite runs this once per rotation, so the digest
// is proven for v1 and then for v2.
import { createHash } from "node:crypto";
import { expect, test } from "@playwright/test";
import { signIn } from "../harness/signin";
import { appUrl, bed, portalSession, until } from "../harness/cluster";

const app = "e2e-app";
const sha256 = (s: string) => createHash("sha256").update(s).digest("hex");

test("K1 prod's /secret shows the digest of the value just set", async ({ page }) => {
  const want = sha256(bed("E2E_K1_VALUE"));
  await signIn(page, "alice");
  await portalSession(page, app, "prod");
  const url = appUrl(app, "prod") + "/secret";
  await page.goto(url);
  await page.waitForURL((u) => u.origin === new URL(url).origin, { timeout: 60_000 });
  const shown = () => page.locator("#digest").innerText();
  expect(await shown(), "the fixture's /secret page").toMatch(/^[0-9a-f]{64}$/);
  await until(5 * 60_000, "prod's /secret to show sha256 of the value set", async () => {
    await page.goto(url);
    const digest = await shown();
    return { done: digest === want, saw: digest };
  });
});
