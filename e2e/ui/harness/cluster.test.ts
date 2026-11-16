// The pure pieces of cluster.ts, run without a cluster: `cd e2e/ui && npm run unit`.
import assert from "node:assert/strict";
import { test } from "node:test";
import { parseWhoami, podSummary, sessionId, waitMessage } from "./cluster.ts";

test("podSummary sums every container's restarts, init containers included, and keys images by name", () => {
  const pod = podSummary({
    metadata: { name: "e2e-app-preview-7d9f8b6c4-x2k9q", uid: "u-1" },
    spec: { containers: [{ name: "app", image: "reg/canyon/e2e-app@sha256:aa" }, { name: "canyon-identity", image: "reg/identity@sha256:bb" }], initContainers: [{ name: "oauth2-proxy", image: "reg/oauth2-proxy@sha256:cc" }] },
    status: { phase: "Running", containerStatuses: [{ restartCount: 1 }, { restartCount: 2 }], initContainerStatuses: [{ restartCount: 3 }] },
  });
  assert.deepEqual(pod, { name: "e2e-app-preview-7d9f8b6c4-x2k9q", uid: "u-1", phase: "Running", restarts: 6, images: { app: "reg/canyon/e2e-app@sha256:aa", "canyon-identity": "reg/identity@sha256:bb" }, initImages: { "oauth2-proxy": "reg/oauth2-proxy@sha256:cc" } });
  assert.equal(podSummary({ metadata: { name: "p", uid: "u" } }).restarts, 0);
});

test("waitMessage is README's line: the deadline in seconds, what, the last answer and when", () => {
  const at = new Date(2026, 8, 27, 10, 4, 59);
  assert.equal(waitMessage(120_000, "bob to reach e2e-app", "302 /#/no-access/e2e-app", at), "waited 120s for bob to reach e2e-app: last answer 302 /#/no-access/e2e-app at 10:04:59");
});

test("parseWhoami reads the fixture's four lines and refuses any other page", () => {
  assert.deepEqual(parseWhoami("email: alice@e2e.test\nroles: -\naud: e2e-app\nverified: true\n"), { email: "alice@e2e.test", roles: "-", aud: "e2e-app", verified: "true" });
  assert.deepEqual(parseWhoami("  email: bob@e2e.test\n  roles: approver, editor\n  aud: e2e-app\n  verified: true").roles, "approver, editor");
  assert.throws(() => parseWhoami("<html>Sign in to your account</html>"), /not the fixture's \/whoami: no "email:" line/);
  assert.throws(() => parseWhoami("email: alice@e2e.test\nroles: -\naud: e2e-app\n"), /no "verified:" line/);
});


test("sessionId is the session cookie's value under either name, whatever else the browser holds", () => {
  assert.equal(sessionId({ cookie: "canyon_signin=abc; __Host-canyon_session=s3ss10n; other=1" }), "s3ss10n");
  assert.equal(sessionId({ cookie: "canyon_session=plain=with=equals" }), "plain=with=equals");
  assert.throws(() => sessionId({ cookie: "canyon_signin=abc" }), /no Canyon session cookie among canyon_signin/);
});
