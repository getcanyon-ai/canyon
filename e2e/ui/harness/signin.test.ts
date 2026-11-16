// requireProvider (signin.ts), run without a cluster: `cd e2e/ui && npm run unit`.
import assert from "node:assert/strict";
import { test } from "node:test";
import { requireProvider } from "./signin.ts";

test("requireProvider passes on the provider's origin and refuses any other, naming the origin alone", () => {
  assert.doesNotThrow(() => requireProvider("https://idp.e2e.test/login?state=x", "https://idp.e2e.test/realms/e2e"));
  assert.throws(() => requireProvider("https://evil.example/login", "https://idp.e2e.test/realms/e2e"), /^Error: refusing to type a password on https:\/\/evil\.example: the provider is https:\/\/idp\.e2e\.test$/);
  assert.throws(() => requireProvider("https://idp.e2e.test:8443/login", "https://idp.e2e.test"), /refusing to type a password on https:\/\/idp\.e2e\.test:8443/);
  assert.throws(() => requireProvider("https://idp.e2e.test/login", undefined), /needs E2E_OIDC_ISSUER/);
});
