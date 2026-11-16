#!/usr/bin/env bash
# Generates the E2E fixture apps into <dir>, one directory each, and each as <name>.tar.gz: the fake
# GitHub's tarball of the repository of that name. None is committed: F-mask's AWS-shaped key is
# made here, so no key-shaped literal is ever in the repo. Pages behind the claims portal's
# middleware need the cookie cp_session set to anything.
set -euo pipefail

out=${1:?usage: gen.sh <dir>}
repo=$(cd "$(dirname "$0")/../.." && pwd)
portal=$repo/gate/testdata/apps/claims-portal
mkdir -p "$out"
for f in f-cp f-core f-clean f-vite f-mask f-sql f-vuln f-cluster f-outbound f-flask f-go f-java; do rm -rf "${out:?}/$f" "$out/$f.tar.gz"; done

# F-cp: the canonical claims portal. F-clean: a Next app with its routes behind a session (e2e/fixtures/next-app), which no scanner flags.
cp -R "$portal" "$out/f-cp"
cp -R "$repo/e2e/fixtures/next-app" "$out/f-clean"

# F-outbound: CASES.md O1's app, which says what it reached in its logs, so nobody signs in to it.
cp -R "$repo/e2e/fixtures/outbound-app" "$out/f-outbound"

# F-sql: the Next app Canyon's own SQL rule must flag (gate/opengrep/canyon.yaml). F-vuln: F-clean
# pinning lodash and xlsx at versions with known advisories (gate/sarif/testdata/trivy-lockfile),
# checked against trivy's database.
cp -R "$repo/gate/opengrep/testdata/canyon/flagged/next-app" "$out/f-sql"
cp -R "$repo/e2e/fixtures/next-app" "$out/f-vuln"
cp "$repo/gate/sarif/testdata/trivy-lockfile/package-lock.json" "$out/f-vuln/package-lock.json"

# F-core: F-cp declaring a postgres database, with pages the journeys read: visits counted in it, a
# secret's digest, the version, and a boot that exits when CRASH_ON_BOOT=1 (imported under NEXT_RUNTIME,
# so the edge build never sees process.exit).
cp -R "$portal" "$out/f-core"
web=$out/f-core/apps/web
printf 'app = "f-core"\n\n[resources]\ndb = { type = "postgres" }\n' >"$out/f-core/canyon.toml"
python3 - "$web/package.json" <<'EOF'
import json, sys
with open(sys.argv[1]) as f:
    pkg = json.load(f)
pkg["dependencies"]["pg"] = "^8.13.1"
pkg["devDependencies"]["@types/pg"] = "^8.11.10"
with open(sys.argv[1], "w") as f:
    json.dump(pkg, f, indent=2)
EOF
mkdir -p "$web/src/app/db" "$web/src/app/secret" "$web/src/app/version"
cat >"$web/src/app/db/page.tsx" <<'EOF'
import { Client } from "pg";

export const dynamic = "force-dynamic";

export default async function Visits() {
  const client = new Client({ connectionString: process.env.DATABASE_URL });
  await client.connect();
  try {
    await client.query("CREATE TABLE IF NOT EXISTS visits (at timestamptz NOT NULL DEFAULT now())");
    await client.query("INSERT INTO visits DEFAULT VALUES");
    const { rows } = await client.query("SELECT count(*)::int AS n FROM visits");
    return <p id="visits">{rows[0].n}</p>;
  } finally {
    await client.end();
  }
}
EOF
cat >"$web/src/app/secret/page.tsx" <<'EOF'
import { createHash } from "node:crypto";

export const dynamic = "force-dynamic";

export default function Secret() {
  return <p id="digest">{createHash("sha256").update(process.env.API_TOKEN ?? "").digest("hex")}</p>;
}
EOF
printf 'export default function Version() {\n  return <p id="version">1</p>;\n}\n' >"$web/src/app/version/page.tsx"
cat >"$web/src/instrumentation.ts" <<'EOF'
export async function register() {
  if (process.env.NEXT_RUNTIME === "nodejs") {
    await import("./crash-on-boot");
  }
}
EOF
printf 'if (process.env.CRASH_ON_BOOT === "1") {\n  process.exit(1);\n}\nexport {};\n' >"$web/src/crash-on-boot.ts"

# F-cluster: F-core declaring a role, with the two pages the cluster journeys read. /whoami verifies
# the identity token the sidecar hands the app (design/PERMISSIONS.md) against the key set at
# CANYON_IDENTITY_JWKS_URL; /probe dials host:port from inside the pod. Both are outside the claims
# portal's middleware: cluster traffic carries the sidecar's token, not cp_session. verify.test.js runs
# by hand: npm install in apps/web, then node --test src/lib/verify.test.js.
cp -R "$out/f-core" "$out/f-cluster"
web=$out/f-cluster/apps/web
printf 'app = "f-cluster"\n\n[resources]\ndb = { type = "postgres" }\n\n[roles]\napprover = "Approves claims"\n' >"$out/f-cluster/canyon.toml"
python3 - "$web/package.json" "$web/src/middleware.ts" <<'EOF'
import json, sys
with open(sys.argv[1]) as f:
    pkg = json.load(f)
pkg["dependencies"]["jose"] = "^5.10.0"
with open(sys.argv[1], "w") as f:
    json.dump(pkg, f, indent=2)
with open(sys.argv[2]) as f:
    src = f.read()
assert "(?!api|login|" in src, "the claims portal's middleware matcher changed: update gen.sh"
with open(sys.argv[2], "w") as f:
    f.write(src.replace("(?!api|login|", "(?!api|login|whoami|probe|fetch|"))
EOF
mkdir -p "$web/src/app/whoami" "$web/src/app/probe" "$web/src/app/fetch"
cat >"$web/src/lib/verify.test.js" <<'EOF'
import assert from "node:assert/strict";
import { test } from "node:test";
import { SignJWT, createLocalJWKSet, exportJWK, generateKeyPair } from "jose";
import { verifyToken } from "./verify.js";

const issuer = "https://canyon.test";
const audience = "e2e-app";
const canyon = await generateKeyPair("ES256");
const stranger = await generateKeyPair("ES256");
const jwks = createLocalJWKSet({ keys: [{ ...(await exportJWK(canyon.publicKey)), kid: "k1", alg: "ES256" }] });

// mint signs a token shaped as the sidecar's (identity.Mint), with claims overriding its defaults
// and exp overriding the 5-minute TTL (a past time, e.g. "-10s", mints an already-expired token).
function mint(key, claims = {}, exp = "5m") {
  return new SignJWT({ iss: issuer, aud: audience, sub: "u1", email: "alice@e2e.test", roles: ["approver"], ...claims })
    .setProtectedHeader({ alg: "ES256", kid: "k1", typ: "JWT" })
    .setIssuedAt()
    .setExpirationTime(exp)
    .sign(key);
}

test("a token Canyon signed for this app verifies", async () => {
  const who = await verifyToken(await mint(canyon.privateKey), { issuer, audience, jwks });
  assert.deepEqual(who, { email: "alice@e2e.test", roles: ["approver"], aud: audience });
});

test("a token signed with another key is refused", async () => {
  await assert.rejects(verifyToken(await mint(stranger.privateKey), { issuer, audience, jwks }));
});

test("a token for another app is refused", async () => {
  await assert.rejects(verifyToken(await mint(canyon.privateKey, { aud: "e2e-peer" }), { issuer, audience, jwks }));
});

test("no audience to check against is refused", async () => {
  await assert.rejects(verifyToken(await mint(canyon.privateKey), { issuer, audience: undefined, jwks }));
});

test("an expired token is refused", async () => {
  await assert.rejects(verifyToken(await mint(canyon.privateKey, {}, "-10s"), { issuer, audience, jwks }));
});

test("a non-ES256 token is refused", async () => {
  const hs256 = await new SignJWT({ iss: issuer, aud: audience, sub: "u1", email: "alice@e2e.test", roles: ["approver"] })
    .setProtectedHeader({ alg: "HS256" })
    .setIssuedAt()
    .setExpirationTime("5m")
    .sign(new TextEncoder().encode("not-the-real-key-anyone-would-use"));
  await assert.rejects(verifyToken(hs256, { issuer, audience, jwks }));
});
EOF
cat >"$web/src/lib/verify.js" <<'EOF'
import { jwtVerify } from "jose";

// verifyToken checks the identity token the sidecar hands the app: ES256, signed by a key in jwks,
// from issuer, for audience, not expired. jose skips an issuer or audience check it is not given, so
// both are required.
export async function verifyToken(token, { issuer, audience, jwks }) {
  if (!issuer || !audience) {
    throw new Error("verifyToken: issuer and audience are required");
  }
  const { payload } = await jwtVerify(token, jwks, { issuer, audience, algorithms: ["ES256"] });
  return { email: String(payload.email ?? ""), roles: Array.isArray(payload.roles) ? payload.roles.map(String) : [], aud: String(payload.aud ?? "") };
}
EOF
cat >"$web/src/app/whoami/page.tsx" <<'EOF'
import { createRemoteJWKSet } from "jose";
import { headers } from "next/headers";
import { verifyToken } from "@/lib/verify";

export const dynamic = "force-dynamic";

// WhoAmI prints who the verified token names, or verified: false when there is no token that verifies,
// and which of the front door's own credentials reached the app (door: -, none: canyon-identity keeps
// the ALB's x-amzn-oidc-* headers and session cookie from it, CASES M8).
export default async function WhoAmI() {
  const h = await headers();
  const token = (h.get("authorization") ?? "").replace(/^Bearer /, "");
  const door = [...h.keys()].filter((n) => n.startsWith("x-amzn-oidc-"));
  for (const pair of (h.get("cookie") ?? "").split(";")) {
    const name = pair.trim().split("=")[0];
    if (name.startsWith("AWSELBAuthSessionCookie")) door.push(name);
  }
  let who = { email: "", roles: [] as string[], aud: "" };
  let verified = false;
  try {
    who = await verifyToken(token, {
      issuer: process.env.CANYON_IDENTITY_ISSUER,
      audience: process.env.CANYON_IDENTITY_AUDIENCE,
      jwks: createRemoteJWKSet(new URL(process.env.CANYON_IDENTITY_JWKS_URL ?? "")),
    });
    verified = true;
  } catch (err) {
    console.error("whoami: the token did not verify:", err);
  }
  return (
    <>
      <p id="email">email: {who.email}</p>
      <p id="roles">roles: {who.roles.length ? who.roles.join(", ") : "-"}</p>
      <p id="aud">aud: {who.aud}</p>
      <p id="verified">verified: {String(verified)}</p>
      <p id="door">door: {door.length ? door.join(", ") : "-"}</p>
    </>
  );
}
EOF
cat >"$web/src/app/probe/page.tsx" <<'EOF'
import { connect } from "node:net";

export const dynamic = "force-dynamic";

// The isolation probe (CASES.md I1–I3): one TCP connect from inside the pod, nothing returned.
// Cluster only: the app port admits the canyon-identity sidecar alone, and it admits only a
// signed-in person.
function dial(to: string): Promise<string> {
  const at = to.lastIndexOf(":");
  const port = Number(to.slice(at + 1));
  if (at < 1 || !Number.isInteger(port) || port < 1 || port > 65535) {
    return Promise.resolve("invalid");
  }
  return new Promise((resolve) => {
    const socket = connect({ host: to.slice(0, at), port, timeout: 3000 });
    const done = (result: string) => {
      socket.destroy();
      resolve(result);
    };
    socket.on("connect", () => done("connected"));
    socket.on("timeout", () => done("timeout"));
    socket.on("error", (err: NodeJS.ErrnoException) => {
      if (err.code === "ECONNREFUSED") return done("refused");
      if (err.code === "ENOTFOUND" || err.code === "EAI_AGAIN") return done("unresolved");
      done(err.code ?? "error"); // never "timeout": that word is the real socket timeout alone
    });
  });
}

export default async function Probe({ searchParams }: { searchParams: Promise<{ to?: string }> }) {
  return <p id="result">{await dial((await searchParams).to ?? "")}</p>;
}
EOF

cat >"$web/src/app/fetch/page.tsx" <<'EOF'
export const dynamic = "force-dynamic";

// The way out (CASES.md I5): Node's own fetch, which the proxy variables send through Canyon's egress gateway,
// where /probe's raw dial reaches nothing. The target's status, or a word for no answer. Cluster only, as /probe.
async function call(url: string): Promise<string> {
  try {
    const answer = await fetch(url, { cache: "no-store", redirect: "manual", signal: AbortSignal.timeout(8000) });
    return String(answer.status);
  } catch (err) {
    const { cause, name } = err as Error & { cause?: { code?: string } };
    return cause?.code ?? name;
  }
}

export default async function Fetch({ searchParams }: { searchParams: Promise<{ url?: string }> }) {
  return <p id="result">{await call((await searchParams).url ?? "")}</p>;
}
EOF

# F-vite: a clean Vite app, which the gate checks whole: every file as if it ships.
mkdir -p "$out/f-vite/src"
printf '{ "name": "f-vite", "private": true, "scripts": { "build": "vite build" }, "devDependencies": { "vite": "^6.0.0" } }\n' >"$out/f-vite/package.json"
printf '<!doctype html>\n<div id="app"></div>\n<script type="module" src="/src/main.ts"></script>\n' >"$out/f-vite/index.html"
printf 'document.querySelector("#app")!.textContent = "Hello";\n' >"$out/f-vite/src/main.ts"

# F-mask: F-cp with a server-only module holding an AWS-shaped key id and secret, fresh each run.
cp -R "$portal" "$out/f-mask"
read -r key_id secret < <(python3 -c '
import secrets, string
pick = lambda alphabet, n: "".join(secrets.choice(alphabet) for _ in range(n))
print("AKIA" + pick(string.ascii_uppercase + "234567", 16), pick(string.ascii_letters + string.digits + "/+", 40))')
cat >"$out/f-mask/apps/web/src/lib/aws.ts" <<EOF
// The claims archive's upload credentials, read on the server only.
export const aws = { accessKeyId: "$key_id", secretAccessKey: "$secret", region: "eu-central-1" };
EOF

# The probes (#1235): SQL built from a request where Canyon's rules read the language, a pinned
# dependency with a known advisory, and an /admin that asks nobody. F-flask and F-go are the
# opengrep bundle's own probes (gate/opengrep/testdata/probes); no permissive rule reads Java's SQL.
cp -R "$repo/gate/opengrep/testdata/probes/flask" "$out/f-flask"
rm "$out/f-flask/expect"
cat >>"$out/f-flask/app.py" <<'EOF'

@app.route("/admin")
def admin():
    return str(sqlite3.connect("x.db").execute("SELECT * FROM users").fetchall())
EOF
printf 'flask==2.2.2\npyyaml==5.3\n' >"$out/f-flask/requirements.txt"
cp -R "$repo/gate/opengrep/testdata/probes/go" "$out/f-go"
rm "$out/f-go/expect"
cat >>"$out/f-go/main.go" <<'EOF'

func admin(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, _ := db.Query("SELECT * FROM users")
		defer rows.Close()
	}
}

func main() {
	http.Handle("/claims", handler(nil))
	http.Handle("/admin", admin(nil))
	_ = http.ListenAndServe(":8080", nil)
}
EOF
printf 'module example.com/probe\n\ngo 1.22\n\nrequire golang.org/x/text v0.3.5\n' >"$out/f-go/go.mod"
mkdir -p "$out/f-java/src/main/java/probe"
cat >"$out/f-java/src/main/java/probe/Claims.java" <<'EOF'
package probe;

import java.sql.Connection;
import java.sql.ResultSet;
import javax.servlet.http.HttpServlet;
import javax.servlet.http.HttpServletRequest;
import javax.servlet.http.HttpServletResponse;

public class Claims extends HttpServlet {
    private Connection db;

    @Override
    protected void doGet(HttpServletRequest req, HttpServletResponse resp) throws java.io.IOException {
        try (ResultSet rows = db.createStatement().executeQuery("SELECT * FROM claims WHERE owner = '" + req.getParameter("owner") + "'")) {
            resp.getWriter().print(rows.next());
        } catch (java.sql.SQLException e) {
            resp.sendError(500);
        }
    }
}
EOF
cat >"$out/f-java/pom.xml" <<'EOF'
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>probe</groupId>
  <artifactId>claims</artifactId>
  <version>1.0.0</version>
  <dependencies>
    <dependency>
      <groupId>org.apache.logging.log4j</groupId>
      <artifactId>log4j-core</artifactId>
      <version>2.14.1</version>
    </dependency>
  </dependencies>
</project>
EOF

# Each as GitHub's tarball of it: one root directory holding the tree. A fixture edited later is tarred again.
python3 - "$out" f-cp f-core f-clean f-vite f-mask f-cluster f-sql f-vuln f-flask f-go f-java <<'EOF'
import sys, tarfile
for name in sys.argv[2:]:
    with tarfile.open(f"{sys.argv[1]}/{name}.tar.gz", "w:gz") as t:
        t.add(f"{sys.argv[1]}/{name}", arcname=name)
EOF

# The stub's OIDC issuer (e2e/compose.oidc.yml, make e2e-up OIDC=1): a throwaway CA, which the server trusts, and a
# certificate from it for the issuer's two names, stub on Canyon's network and localhost where the journeys' browser
# reaches its published port. And the team setup's values (deploy/docker/team.yml), as the guide's .env holds them,
# its passwords and token drawn here and never committed. Both are kept while they serve: a running stack read them at
# its start, its Postgres volumes hold the passwords, and both go with e2e/.work.
if ! { [ -e "$out/oidc.crt" ] && openssl x509 -checkend 86400 -noout -in "$out/oidc.crt" >/dev/null; }; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=e2e-oidc-ca \
    -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign \
    -keyout "$out/oidc-ca.key" -out "$out/oidc-ca.pem" 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj /CN=stub -keyout "$out/oidc.key" -out "$out/oidc.csr" 2>/dev/null
  openssl x509 -req -in "$out/oidc.csr" -CA "$out/oidc-ca.pem" -CAkey "$out/oidc-ca.key" -CAcreateserial -days 30 -out "$out/oidc.crt" \
    -extfile <(printf 'subjectAltName=DNS:stub,DNS:localhost\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\n') 2>/dev/null
fi
# The echo's (e2e/echo, e2e/compose.e2e.yml; #2144): a throwaway test CA, which the gateway's Envoy trusts beside its
# image's roots in the stack alone (e2e/fixtures/egress-trust.sh), a certificate from it for echo.canyon-floor.test,
# and one for echo-untrusted.canyon-floor.test from a second CA nothing trusts, so the stack proves the bundle trusts
# the one CA and widens nothing. Kept while they serve, as the issuer's are.
echo_cert() { # <name> <ca>: a server certificate for <name>.canyon-floor.test from <ca>, 30 days
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$1.canyon-floor.test" -keyout "$out/$1.key" -out "$out/$1.csr" 2>/dev/null
  openssl x509 -req -in "$out/$1.csr" -CA "$out/$2.pem" -CAkey "$out/$2.key" -CAcreateserial -days 30 -out "$out/$1.crt" \
    -extfile <(printf 'subjectAltName=DNS:%s.canyon-floor.test\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\n' "$1") 2>/dev/null
}
if ! { [ -e "$out/echo.crt" ] && [ -e "$out/echo-untrusted.crt" ] && openssl x509 -checkend 86400 -noout -in "$out/echo.crt" >/dev/null; }; then
  for ca in echo-ca echo-other-ca; do
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=e2e-$ca" \
      -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign -keyout "$out/$ca.key" -out "$out/$ca.pem" 2>/dev/null
  done
  echo_cert echo echo-ca
  echo_cert echo-untrusted echo-other-ca
fi
# The run's licence (#2879): a key made here and never kept anywhere else, whose public half the E2E stack's server,
# built with the e2e tag, trusts beside Canyon's own (cmd/canyon-server/licence_e2e.go), and a licence it signs for
# every Enterprise feature, which the server reads as CANYON_LICENSE.
mkdir -p "$out/licence"
lic=$(cd "$out/licence" && pwd)
[ -e "$lic/key.seed" ] || openssl rand -base64 32 >"$lic/key.seed" # an Ed25519 seed is 32 random bytes: no openssl that lacks ed25519 is in the way
(cd "$repo" && go run ./ee/tools/licence -seed "$lic/key.seed" -public) >"$lic/key.pub"
(cd "$repo" && go run ./ee/tools/licence -seed "$lic/key.seed" -customer "Canyon E2E" -expires 2099-12-31 -seats 100 \
  -features outbound_keys,platform_patching,group_sync) >"$lic/canyon.licence"
[ -e "$out/oidc.env" ] || cat >"$out/oidc.env" <<EOF2
CANYON_DOMAIN=localhost:7777
CANYON_OIDC_ISSUER=https://stub:8443
CANYON_OIDC_CLIENT_ID=canyon
CANYON_ADMINS=admin@e2e.test
CANYON_POSTGRES_PASSWORD=$(openssl rand -hex 24)
CANYON_APP_POSTGRES_PASSWORD=$(openssl rand -hex 24)
CANYON_EGRESS_RECEIVER_TOKEN=$(openssl rand -hex 32)
CANYON_CADDYFILE=./e2e/.work/fixtures/Caddyfile
EOF2
# The team setup's Caddy runs the guide's own Caddyfile, written by the guide's own block, for the domain localhost:
# Caddy issues localhost's names from its own CA, and asks Canyon before each app's, as it does on a server.
(cd "$out" && DOMAIN=localhost sh -eu -c "$("$repo/scripts/guide-block.sh" "$repo/docs/src/content/docs/self-host/docker.md" 'Write the Caddyfile')")
