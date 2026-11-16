// signIn is the one place that knows the identity provider's pages (e2e/cluster/README.md): it
// signs page's browser in to Canyon as one of the test cluster's three people and answers the session
// (the Cookie header) api() takes explicitly -- never carried implicitly from whoever signed in
// last, which would let a case meant to run as a different person, or anonymously, silently run
// as the previous one instead (canyon#482 review).
import type { Page } from "@playwright/test";

export type Session = { cookie: string };

export async function signIn(page: Page, user: "admin" | "alice" | "bob"): Promise<Session> {
  const env = `E2E_${user.toUpperCase()}`;
  const [email, password, canyon] = [process.env[`${env}_EMAIL`], process.env[`${env}_PASSWORD`], process.env.E2E_CANYON_URL];
  if (!email || !password || !canyon) throw new Error(`signIn(${user}) needs ${env}_EMAIL, ${env}_PASSWORD and E2E_CANYON_URL`);
  await page.goto("/auth/start");
  const secret = page.locator("input[type=password]");
  await page.getByRole("textbox", { name: /email|user|login/i }).fill(email);
  if (!(await secret.count())) await page.keyboard.press("Enter"); // a provider that asks for the address first
  await secret.waitFor();
  requireProvider(page.url(), process.env.E2E_OIDC_ISSUER);
  await secret.fill(password);
  await secret.press("Enter");
  const home = new URL(canyon).origin;
  await page.waitForURL((u) => u.origin === home, { timeout: 60_000 });
  return { cookie: (await page.context().cookies(canyon)).map((c) => `${c.name}=${c.value}`).join("; ") };
}

// requireProvider is the check before a password is typed: the page is the identity provider's
// origin ($E2E_OIDC_ISSUER's), never another's -- a redirect anywhere else would otherwise be
// handed the password. The failure names the origin it found, never what was about to be typed.
export function requireProvider(url: string, issuer: string | undefined): void {
  if (!issuer) throw new Error("signIn needs E2E_OIDC_ISSUER to know the provider's origin before typing a password");
  const [here, provider] = [new URL(url).origin, new URL(issuer).origin];
  if (here !== provider) throw new Error(`refusing to type a password on ${here}: the provider is ${provider}`);
}
