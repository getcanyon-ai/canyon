# Canyon — Authentication

**The rule this document exists to enforce: every action Canyon records was taken by somebody, and
Canyon knows who.** There is no anonymous identity. A host that cannot say who is calling refuses
the call.

Canyon's product is a ledger. A gate that blocks, an approval that waives, a promotion that ships:
all three are theatre if the row says nobody did them. Today every row says `anonymous`
(`session.go:78`), and `prod.human-approval` passes unconditionally because the only actor the
server can mint is `kind: person` (`receipt.go:75`). This document is how that stops.

Position, 2026-09-19 (the owner): authentication is mandatory and on by default; enterprise SSO is a
v1 capability, not a day-2 register entry.

---

## 1. What changes, in one paragraph

Canyon speaks OIDC to the customer's own identity provider. The dashboard signs in with
Authorization Code plus PKCE. The CLI signs in with the device authorization grant, run by Canyon
itself rather than by the identity provider. Canyon mints its own opaque session and stores no
identity-provider tokens, ever. A request without a valid session gets `401`, and the session
stage stops minting sessions for callers who have none. One environment variable turns
authentication off for local demos, and a host with it off refuses to bind beyond loopback.

---

## 2. The modes

`CANYON_AUTH` is the only switch. Unset means `oidc`.

| Value | Who may call | Bind | What it is for |
|---|---|---|---|
| `oidc` (default) | a signed-in person | anywhere, at an `https` public URL (or a loopback one) | every real deployment |
| `off` | this machine: a loopback peer | loopback only, no override | `go test`, a laptop, a five minute look |
| `demo` | this machine: a loopback peer, or the Docker network's gateway its published port arrives from, once Docker is seen to publish that port on loopback alone | anywhere, at a loopback public URL alone, and a loopback `CANYON_PUBLISHED_ADDR` when set | the shipped compose demo, and nothing else |

Three properties of this table matter more than the values.

**Unset is `oidc`, and `oidc` without a configured issuer refuses to boot.** A host with no
configuration does not start open. It stops with a message naming the variables it needs and
naming `CANYON_AUTH=off` for a local try. Nobody reaches an unauthenticated Canyon by forgetting
something.

**`off` must be loopback at both ends, and there is no override.** `CANYON_ADDR` must be a
loopback address **and** `CANYON_PUBLIC_URL` must be a loopback URL. The second half matters:
without it, a host bound to `127.0.0.1` behind a reverse proxy on a public address is an
unauthenticated Canyon on the internet, and the bind check alone would call that safe. Before
this design, `CANYON_ALLOW_REMOTE_ANONYMOUS=1` let the compose demo bind its container network.
That variable is **deleted**. The escape hatch it
provided becomes the third value of `CANYON_AUTH`, `demo`, which names the one thing it is for. A
reader of a compose file sees `demo` and knows (amended 2026-10-22, #2416: the value was
`off-and-exposed`, which read as an option; the old name is refused at boot by an error naming
`demo`). Net change to the environment surface: one variable replaces two.

**`demo` is the compose demo's mode and keeps the demo's behaviour.** `make demo` stays
`docker compose up --build`, the compose file sets `CANYON_AUTH=demo`, a caller with no session is
served as the anonymous admin and no session row or cookie is made (every caller is the same actor;
amended 2026-10-27, #2428), the actor stays `person / anonymous`, the escalated honesty
banner shows, and `POST /api/v1/demo/fix/:app` stays routed (`contracts/routes.md:183` already says
"not routed when auth is configured"). A stranger still runs `git clone && docker compose up` and
lands on the fleet.

The server logs one line at every boot naming the mode: `canyon-server: auth oidc, issuer
https://login.microsoftonline.com/…/v2.0` or `canyon-server: auth demo, every caller is an
admin`.

**In code the mode is a field on `app`, and its zero value is `off`.** `main()` is the only place
that reads the environment and the only place that defaults it to `oidc`. Every server test builds
an `app` directly, so a zero value of `oidc` would make the whole existing suite demand a session
and a zero value of `demo` would hide a bind bug. `off` is the value that is safe to
forget: a test that means to exercise authentication sets the field, and a test that does not, does
not.

## 3. The configuration surface

**This document is auth only.** Every organisation value below is read from the environment and
validated at boot. Moving them into the product, so a person sets them on a screen instead of an
operator setting them in a values file, is its own feature with its own milestone: `design/ONBOARDING.md`.
When that lands, these variables become the bootstrap and the override, and nothing here changes
shape.

| Variable | Required | Validated at boot | What a wrong value looks like |
|---|---|---|---|
| `CANYON_AUTH` | no, defaults to `oidc` | one of the three values | `CANYON_AUTH=on is not a mode: oidc, off, demo` |
| `CANYON_OIDC_ISSUER` | when `oidc` | absolute `https` URL (shape only; see below) | `CANYON_OIDC_ISSUER=example.com is not an https URL` |
| `CANYON_OIDC_CLIENT_ID` | when `oidc` | non-empty | named in the same refusal |
| `CANYON_OIDC_CLIENT_SECRET` | no | when absent, Canyon registers as a public PKCE client and sends no secret | a secret the provider rejects surfaces at the first sign-in, not at boot |
| `CANYON_PUBLIC_URL` | already exists | already validated (`publicURLOf`, `cmd/canyon-server/main.go`); its host must be loopback when `CANYON_AUTH=off` or `demo`; required and `https` when `oidc`, unless its host is loopback (amended 2026-10-23: unset, oidc defaulted to `http://localhost:7777` and booted a Canyon nobody could sign in to, and a public demo booted with every caller an admin) | the redirect URI is this plus `/auth/callback` |
| `CANYON_OIDC_ALLOWED_DOMAINS` | no — the row carries membership once somebody has onboarded the host (ONBOARDING.md §1) | comma separated bare domains, each with a dot, lower-cased, ASCII | `"meridian" is not a domain` |
| `CANYON_OIDC_ALLOWED_EMAILS` | no — as above | comma separated addresses, each with an `@`, lower-cased | `"dana" is not an email address` |
| `CANYON_ADMINS` | when `oidc` — the bootstrap admins, the only people who may sign in until setup is finished | comma separated addresses, each with an `@`, lower-cased; a **union** with the row's list, never an override | `CANYON_ADMINS: "ops" is not an email address` |
| `CANYON_ADMIN_CLAIM` · `CANYON_ADMIN_VALUE` | no | both set or neither | `CANYON_ADMIN_CLAIM and CANYON_ADMIN_VALUE are set together or not at all` |
| `CANYON_RESPONDER_CLAIM` · `CANYON_RESPONDER_VALUE` | no — the claim and value that mean security responder (design/PERMISSIONS.md), over the row's `responder_claim`/`responder_value` | both set or neither | `CANYON_RESPONDER_CLAIM and CANYON_RESPONDER_VALUE are set together or not at all` |
| `CANYON_GROUPS_CLAIM` | no — overrides the row's `groups_claim` (Settings), the claim whose groups each sign-in stores for group grants — its strings when it is an array, its keys when it is an object (Zitadel's project roles, `urn:zitadel:iam:org:project:roles`); over 200 groups, or none sent (Entra's overage), a sign-in stores none and logs it | a claim name, 1 to 256 characters of plain text | `CANYON_GROUPS_CLAIM is a claim name of 1 to 256 characters of plain text` |
| `CANYON_ORG` | no | already exists | the org name on the receipt |

### Who may sign in: two lists, and their union

**Membership is `allowed_domains ∪ allowed_emails`.** A verified email at a listed domain is a
member. A verified email on the address list is a member. Somebody on both is a member once;
there is no ranking and no precedence to remember.

**At least one of the two must be non-empty under `oidc`, and the host refuses to boot otherwise.**
Not a default of "everyone", not a default of "nobody": a host that starts with an empty membership
is either an open door or a brick, and both are worse than a boot message naming the two variables.

The address list exists because **a team without a domain is a normal customer, not an edge case.**
Four people on personal Google accounts have no `@company.com` to list, and telling them to buy a
domain and set up Workspace before they can try a deployment product is telling them to go away.
`design/ONBOARDING.md` walks that team through end to end.

Why not domains alone plus a "just add everybody at gmail.com" shrug: `https://accounts.google.com`
is one issuer shared by every Google account on earth, so `gmail.com` on the domain list is the
whole internet. The address list is how a small team is precise, and it is also how an org with a
domain adds one contractor without adding `partner.com`.

**Checked twice, and the second time is the one that matters.**

1. **At the callback, before any row is written.** A token that validates but whose email is on
   neither list is a `302` to `/#/signin`: no session row, one log line, and one `sign in refused` audit
row (`reason: not_a_member`, §9; amended 2026-10-05), written once the redirect is answered. The caller
   is a browser following a top-level navigation, so a refusal answers a redirect and never a
   body — a page of machine output is not something to show a person. No detail rides the
   redirect; the reason is in the log.
2. **On every request, from the email stored on the session.** Removing an address or a domain
   takes effect at once, on sessions that already exist, with no revoke step to remember. Somebody
   who leaves is off the list and is out of Canyon on their next request, including the CLI
   session on their laptop.

The cost of the second check is one extra table in the session stage's existing lookup, which is a
single row read by primary key on a query that was already running. In this milestone the lists
come from the environment and are constant for the life of the process, so the check is a map
lookup and costs nothing at all. When `design/ONBOARDING.md` moves them into the preferences row,
the session stage's `SELECT` gains a join to that one-row table: same round trip, no cache to
invalidate, and correct even if the control plane is ever run with two replicas.

The lists are small by construction. An org that needs hundreds of individual addresses has a
directory, and a directory has domains or groups.

### Email, and which claim carries it

Canyon matches on the `email` claim, falling back to `preferred_username` when `email` is absent
or empty. Both the claim value and every configured entry are lower-cased before comparison, and the
value must have an address's shape, since `preferred_username` is a UPN on an Entra guest and
user-settable on some providers; it is trusted at all only because the issuer is tenant-bound. An
`email_verified` that is present and not the boolean `true` (the string `"true"` included) refuses,
and so does an address whose part before the `@` is not ASCII, read before lower-casing (the Kelvin
sign folds into `k`), and a `sub`, an address or an `hd` over 200 characters: an identifier is never
cut to one naming someone else (a 200-character address with a domain after it would be cut to that
address). Only the display name is cut to fit.
The admin claim's value is compared exactly, case and all: group names and object ids are opaque
identifiers, not words.

An `email_verified` claim that is **present and false** never matches anything, so the sign-in is
refused. This applies to **every** list, the address list included: an address on
`allowed_emails` still has to be verified, because the whole point of listing an address rather
than a domain is that it names one human, and an unverified email claim does not name anybody. An
**absent** `email_verified` is accepted: Entra omits it entirely, and what bounds the population
there is the tenant-bound issuer plus the lists, not a claim the provider does not send.

**Guest and B2B users.** An Entra guest from `partner.com`, invited into your tenant, carries their
home domain in the email claim. They are refused unless somebody lists either their address or
`partner.com`, and the address is usually the right answer. That is the intended behaviour: a
contractor who can approve production deploys is a deliberate entry, not a side effect of an
invite.


### Admin by group claim

Beside the admin address list, an organisation may name **one claim and one value** that mean
admin: `CANYON_ADMIN_CLAIM` and `CANYON_ADMIN_VALUE`. For an **array** claim, admin means membership. For an
**object** claim, it means one of its keys: Zitadel's project roles, `urn:zitadel:iam:org:project:roles`, map
each role to where it was granted. For a **string** claim, it means equality. Both must be set or neither is.

**Admin is `CANYON_ADMINS contains the email OR the claim matched`.** Two ways in, because an
organisation with a directory group for platform engineers should not have to maintain a second
list by hand, and an organisation without one, which includes every Google Workspace customer and
every small team, should not have to create a group to grant one person admin.

Admin is a separate question from membership. Being an admin does not make somebody a member: an
address in `CANYON_ADMINS` that is on neither membership list cannot sign in, and the boot refuses
that configuration by name rather than letting an admin discover it at the callback.

**The claim result is recorded at sign-in, because the token is discarded.** The session row carries
one boolean, `claimed_admin`, and the role on every request is
`allowlist(stored email) OR claimed_admin`. The consequence is worth stating plainly rather than
discovering:

| Demotion | Takes effect |
|---|---|
| removed from `CANYON_ADMINS` | at the host's next restart, on their existing session |
| removed from the directory group | at their next sign-in: up to 12 hours in a browser, up to 7 days from the CLI |

A sign-in also binds the person's subject onto every app grant made to their address that names no
subject yet, in the session's transaction with one `bind grants` audit row naming the apps and the
issuer — only when the `email` claim matched and `email_verified` is the boolean `true`: the absent
claim Entra sends, and an address from `preferred_username`, are trusted to sign in, never to bind.
A bound grant records that issuer, is never rebound under it, and names its subject alone from then;
one bound under another issuer than `CANYON_OIDC_ISSUER` is read as not yet bound, and one bound with
no issuer recorded — bound before issuers were, or by a server from before them mid-upgrade — as
`CANYON_OIDC_ISSUER`'s. With sign-in off a grant is bound under `none`, never an issuer, so it is read
as not yet bound once sign-in is on. The server records the issuer on a grant with none: `none` on the
anonymous admin's at its start, before it serves, and `CANYON_OIDC_ISSUER` on every other in the
transaction of the first sign-in each server process verifies an ID token for under it — never with
sign-in off, and never on discovery alone, which a wrong but real issuer (another tenant, Entra's v1
for v2) passes, so fixing the variable undoes nothing it stamped. Run a release with issuers on the
current issuer, and sign in through it, before changing it: an upgrade and a switch in one rollout read every subject bound before as
the new issuer's. A session minted through another issuer than `CANYON_OIDC_ISSUER`
(`sessions.issuer`; one recording none is the current issuer's) is refused, as an expired one is, and
its person signs in again (design/PERMISSIONS.md, *Grants*; amended 2026-10-17, #2306). The
session records whether its address was so verified (`sessions.email_verified`, copied to a CLI
session its browser approves), and every other path that writes a grant's subject — creating an
app, claiming one, an access request a share grants — reads it: one rule, never a route's own.

The same holds for app access by group: the session row also carries `groups`, the groups claim's
array (`groups_claim`) as it stood at sign-in, and a group grant matches it until the session ends.

**Responders the same way** (design/PERMISSIONS.md, *Security responders*): the row's
`responder_emails`, or the claim pair `responder_claim`/`responder_value` (`CANYON_RESPONDER_CLAIM`
and `CANYON_RESPONDER_VALUE` pin it), tested as the admin claim is and stored at sign-in as
`claimed_responder`. The role is `admin` if admin, else `responder` if listed or claimed, else
`member`; the list is read live and a claim lasts its session, as for admins.

An admin who needs a group demotion to take effect now revokes that person's sessions
(`DELETE /api/v1/sessions?subject=…`), which is the same action they would take for a leaver. The
alternative, re-reading the claim per request, would mean holding a refresh token to ask the
provider again, and §6 explains why Canyon holds none.

| Provider | `CANYON_OIDC_ISSUER` | Email | Group claim |
|---|---|---|---|
| Microsoft Entra ID | `https://login.microsoftonline.com/<tenant-guid>/v2.0` | enable the optional `email` claim on the registration, or `preferred_username` is what Canyon matches | enable the `groups` claim on the registration; the values are **group object ids**, not names, so `admin_value` is a GUID. **Overage:** a user in more than ~200 groups gets no `groups` claim at all, only a `_claim_names` pointer Canyon does not follow, so they are **not admin** and the sign-in log says `admin_claim absent`. Put such a person on `CANYON_ADMINS`. |
| Okta | `https://<org>.okta.com/oauth2/default` | sends `email` and `email_verified` | add a groups claim to the authorization server; the values are the group names you configure |
| Google Workspace | `https://accounts.google.com` | sends both | **none.** Google does not put group membership in the ID token, so Google Workspace orgs use `CANYON_ADMINS` only |
| Generic OIDC | whatever its discovery document says its issuer is | needs `email` or `preferred_username` | whatever it emits; name the claim and the value |

**There are no provider presets.** All four are the same variables and one discovery document; a
preset table would be a tier system with one mechanism in it. The differences above are
documentation, not code.

**One log line per sign-in**, naming the subject, which claim matched the email, whether the admin
claim matched, and the role granted:
`canyon-server: sign in sub=00u1a… matched=email dana@meridian.example admin_claim=hit role=admin`. The
client secret, the PKCE verifier, the state, the nonce, the authorization code and every token are
**never logged**, at any level, including on failure.


### Discovery, and what is fatal

The **shape** of the configuration is validated fatally at boot: the mode, the issuer being an
absolute `https` URL, the client id being present, the domain list being present and well formed,
the admin list parsing. A host misconfigured in any of those ways does not start, so a broken
configuration never replaces a working process during a rolling restart.

The **discovery fetch** is not fatal. `GET <issuer>/.well-known/openid-configuration` is attempted
at boot and retried with bounded backoff, and while it has not succeeded the host is **not ready**:
`/ready` answers `503` with `oidc` in `waiting_on`. A provider having a bad ten minutes must not
turn into a crash loop that also takes out the fleet page and the audit trail, and a load balancer
already knows what to do with a host that is not ready. The document's own `issuer` field must
equal the configured value, and a mismatch keeps the host unready rather than killing it.

**SAML is not in v1.** Entra, Okta and Google all speak OIDC, and between them they cover the
market Canyon is sold into. A customer whose identity provider is SAML only (an old ADFS or Ping
estate) has two answers: put an OIDC-capable front on it, which every such product ships, or wait.
SAML is reconsidered when an organisation that cannot expose an OIDC issuer needs Canyon.
That is when an identity broker earns its container, and not before.

### Why no broker

The sibling platform runs a Zitadel broker because it is multi-tenant software as a service:
N customer organisations behind one login, one shared public PKCE client, org claims validated per
request. Canyon is the opposite shape. One binary plus one Postgres, self-hosted by one company,
one organisation per host (`contracts/routes.md`, `GET /api/v1/preferences`: "one org per host, one
row"). A broker would add a container, a second database, a second identity store, a second admin
console and a second set of credentials to rotate, in exchange for multi-tenancy Canyon does not
have. Canyon speaks OIDC to the customer's identity provider directly.

## 4. The flows

### 4.1 Dashboard sign-in

1. The browser loads the dashboard and calls `GET /api/v1/fleet`.
2. No session cookie, so the session stage answers `401 {error:"unauthenticated"}` and writes
   nothing: no session row, no audit row, one log line.
3. The dashboard renders the sign-in page, whole screen, one button.
4. The button navigates to `GET /auth/start`. Canyon generates a PKCE verifier, a state value and
   a **nonce**, all three from `crypto/rand`, and puts all three in one short-lived cookie built by
   the **same builder the session cookie uses** (`sessionCookieOf`): `__Host-` prefixed and
   `Secure` on https, `HttpOnly`, no `Domain`. It is `SameSite=Lax` and cannot be `Strict`, because
   it must survive the top-level cross-site navigation back from the identity provider.
5. Canyon redirects to the authorization endpoint with `response_type=code`,
   `scope=openid profile email`, `code_challenge_method=S256`, the challenge, the state and the
   nonce. A sign-in that follows a sign-out the person chose arrives as
   `/auth/start?prompt=select_account` and adds the standard OIDC `prompt=select_account`, which
   asks the provider to offer a choice of account instead of answering silently with the one it
   still holds (Zitadel shows its account picker; a provider that does not support it ignores it); every
   other sign-in sends no `prompt` and stays silent. `select_account` is the only value passed on:
   any other `prompt` is a `400`, so nothing a caller writes reaches the provider.
6. The person signs in at their identity provider, which redirects to
   `<CANYON_PUBLIC_URL>/auth/callback?code=…&state=…`.
7. Canyon checks the state against the cookie, exchanges the code for an ID token using the
   verifier, and validates the token: signature, `iss`, `aud`, `exp`, and the **nonce** against the
   cookie's. Then it **throws the tokens away**.
8. Canyon reads the email claim per section 3, refuses with a `302` to `/#/signin` if
   `email_verified` is present and false or the email is on neither membership list — or to
   `/#/setup-required` when the only thing wrong is that nobody has finished setting the host up
   (`design/ONBOARDING.md` §2) — and only then inserts a session row: `actor_kind = person`, `actor_subject` = the `sub` claim,
   `actor_display` = `name` (falling back to `email`, then `sub`), `email`, `issuer`,
   `flow = browser`, `expires_at` = now plus 12 hours. One audit row, `action: "sign in"`.
9. Canyon sets the session cookie, exactly as today (`__Host-` prefixed and `Secure` on https,
   `HttpOnly`, `SameSite=Lax`, no `Domain`), and redirects to `/`.
10. Every subsequent request reads the cookie, looks the row up, and refuses it when
    `revoked_at` is set or `expires_at` has passed.

**Signature verification uses `github.com/coreos/go-oidc/v3`** with its **remote key set**, which
refetches the provider's JWKS when it meets a key id it does not know. Canyon never takes a copy of
the keys at boot: every identity provider rotates signing keys on its own schedule, and a host that
cached them would start rejecting valid tokens hours or days later, at a time unconnected to any
deploy. The set is refreshed only by an unknown key id or a restart, so a key a provider withdraws
stays accepted until one of those; that is the library's behaviour and the right trade. go-oidc has no
clock leeway of its own, and Entra emits `nbf`, so Canyon verifies at a clock thirty seconds behind:
`exp` and `nbf` both tolerate that much and no more.

### 4.2 CLI sign-in

`canyon login <host>` on an authenticated host:

1. `GET /auth/config` on the Open group answers `{canyon: true, auth: "oidc", org: "Meridian",
   device: true, cli, public, setup}`. A host that answers anything else is not Canyon, which is what
   `firstContact` becomes (section 6).
2. `POST /auth/device` answers `{device_code, user_code, verification_uri,
   verification_uri_complete, expires_in: 600, interval: 5}`, RFC 8628 shapes. Canyon stores the
   pending request with a **hash** of the device code, never the code, and the same statement that
   inserts the row deletes every expired row: the table cleans itself on use and needs no sweeper.
3. The CLI prints the code large, prints the URL, and tries to open a browser. `--no-browser`
   skips the attempt. Nothing is required to open.
4. The person opens the URL on any machine, signs in through section 4.1 if they are not already,
   checks what the screen says about where the request came from and which account is approving,
   types the code, and confirms. `Not you? Switch account` signs out, and the sign-in after it asks
   the provider which account (section 4.1, step 5) and comes back to the same code. Only a browser
   session reads, approves or denies a code: a CLI session is `403 dashboard_required` (amended
   2026-10-12), because the session an approval copies would be a fresh CLI one, and a stolen CLI
   token could then renew itself before every expiry.
5. The CLI polls `POST /auth/device/token` every `interval` seconds. It answers
   `400 {error:"authorization_pending"}`, `400 {error:"slow_down"}` when the CLI polls faster than
   the interval, and then `{session, expires_at, subject, display, org}` once confirmed. A device
   code is consumed on first success and expires in ten minutes. On `expired_token` the CLI prints
   `The code expired before it was approved.` and `Run: canyon login <host>`; only when step 1
   answered `setup: true` does it add that nobody can approve a sign-in until setup is finished.
   A host restarting while the CLI waits is not an answer: a `502`, `503` or `504`, or a request
   that never reached an answer, is polled through again, each wait twice the last up to 30 s, until
   the code's `expires_in` has passed since the grant. Only then does the CLI stop, with
   `<host> stopped answering (<its status, or no answer>), and the code expired while Canyon waited.` and
   `Run: canyon login <host>` (**amended 2026-10-07**, QA batch: a test cluster restart ended a waiting login
   on a bare `Service Unavailable` or `Bad Gateway`).
6. The CLI writes host and session to `~/.config/canyon/config.json` at `0600`, as today, after
   moving the host that file named before, if another, with its session to
   `~/.config/canyon/hosts/` (design/specs/cli.md §8.4, amended 2026-10-11), and prints
   `✓ Signed in to <host> as <display>`.

**Canyon runs the device flow, not the identity provider.** The customer registers one client in
their identity provider, for the dashboard, and never has to register or distribute a CLI client.
Google's device grant does not serve every application type, Entra's needs its own public client
registration, and each provider's polling semantics differ slightly. Canyon has a browser sign-in
already, so the verification page is a Canyon page behind a Canyon session and the device flow is
two routes and one table. The alternative, a localhost redirect, needs a browser on the same
machine as the CLI and a free port: neither is true over SSH, in a container, or inside a coding
harness, which is where most Canyon CLI invocations happen.

`GET /auth/start`, `GET /auth/callback` and `POST /auth/device` are bounded per source address by
one in-memory token bucket (`cmd/canyon-server/limiter.go`); `POST /auth/device/token` is bounded
per grant by its five-second interval (`slow_down`), never per address, because behind an ingress
every CLI in the estate shares one address. The four are the only unauthenticated routes that
write. The bucket, a grant's last poll and a denial live in the process: a restart or a second
replica turns `access_denied` into `expired_token` and doubles the bound, which is accepted for
one host. Behind a reverse proxy the bucket and the requesting address were the proxy's; since
2026-10-01 (`canyonv2-ab4r`, decision D3) `CANYON_TRUSTED_PROXIES` names the proxies, as CIDRs.
`X-Forwarded-For` is read only when the immediate peer is one of them, and then the source address
is its right-most address that is not one of them — the approval screen's `Requested from` and the
bucket alike; a request whose peer is not a trusted proxy is its peer, whatever it claims. Unset,
it is the peer, as before. Routes
that check where a request came from for another reason (the fleet's banner, the app network) keep
reading the peer.

### 4.3 An agent's deploy

An agent inside Claude Code or Codex runs `canyon deploy`. It uses the session the person signed in
with, from the person's config file. **The actor is the person: `kind: person`, the person's
subject.** The agent is the person's tool, on the person's machine, under the person's session.
Recording a separate agent identity would require the agent to authenticate as itself, which means
issuing credentials to software the person already trusts with their shell. There is no agent
sign-in in v1.

The deploy is recorded with the person's name. The gate runs. If it blocks, the agent cannot waive
it and cannot ship it, for the reason in the next flow.

### 4.4 The person's session is the person

`prod.human-approval` means "a person clicked promote". Today it evaluates `actor.Kind == "person"`
and every session satisfies it because every session is anonymous (`receipt.go:75`). With identity
on it evaluates to a name, and that is the fix: the rule passes for Dana, the receipt says Dana,
and the audit row says Dana.

**Whatever runs under a person's session acts as that person, and is recorded as that person.** An
agent inside Claude Code holding Dana's session is Dana's tool on Dana's machine, exactly as her
shell is. `canyon deploy --env prod` (promote until 2026-09-30) is unchanged: one confirmation in
the terminal, `--yes` for the non-interactive case, the live URL at the end, and the receipt
naming her.

This was designed the other way on 2026-09-19 and reversed the same day, on the owner's ruling, and the
reason is worth keeping because it is the honest one. **An agent can drive a browser too.** A
rule that says "the confirming act must come from a browser session" stops an agent that shells
out to `canyon`, and does not stop an agent that opens the page and clicks. It is a speed bump
described as a boundary, and the cost of pretending otherwise is a product whose central claim,
that the receipt names who did it, would be resting on a test that can be walked around. The
server cannot distinguish a person at a keyboard from software driving one. It can distinguish a
session, and a session belongs to a person who signed in.

`flow` (`browser | device | demo | oauth`) is still written on the session row, because the org setting in
§4.5 reads it and a column costs nothing. **By default it gates nothing** — but for one override
(amended 2026-10-08): a promotion of a version whose preview answered an error is refused, and only a
browser session's `promote_anyway` promotes it anyway (contracts/routes.md, the promotions row; with
sign-in off there are no sessions, and it counts from any caller).

Approving a blocking finding stays as the contract has it: `kind = person`, so an
`agent` or `canyon` actor is refused, and there is no CLI verb for it in any case. With §4.5's
setting on, a finding is approved in the dashboard too (amended 2026-10-10, #1824). The four
org-wide writes are gated on `role = admin` and nothing else, for the same reason: no verb.

**Approver is not the deployer** (listed as not in v1) becomes expressible for the first time: the
promotion row already carries `deployed_by` (the preview deploy's actor) and `promoted_by` (the
person who confirmed), as two separate names (`contracts/routes.md:54`). Comparing them is a rule,
not a schema change, and it is not in this cut.

### 4.5 The org setting: "Production is approved in the dashboard"

> **Decided 2026-10-07** (#1820, the owner). The named trigger came from Canyon's own first
> tester run rather than a customer: carol's headless Claude Code ran `canyon deploy --env prod --yes`
> and promoted, after telling her twice that a person would approve. Nothing was broken — §4.4 is
> exactly what happened — but the receipt said `a person promotes; agents can't`, and that was not
> true. Both modes now say what they do.

Some organisations will not accept §4.4's answer, and they should not have to argue with the
product about it. One setting, **off by default**, admin-only, on Settings › People & sign-in, the
page about who is who:

> **Production is approved in the dashboard.** On: the CLI and agents only ask, and a person on the
> app's team approves it here in Canyon. Off: anyone on the app's team can promote, and so can an
> agent they ask, through the CLI or Canyon's tools. (Amended 2026-11-22: agents also promote through
> Canyon's MCP tools, signed in as their person.)

Wording rule: it names a place, not a prohibition. "Production is approved in the dashboard"
describes where the act happens. "Agents can't promote" would describe a control §4.4 has just
admitted is not airtight, and it would be the second sentence a sceptical platform engineer
disbelieves.

**On**, `canyon deploy --env prod` — with `--yes` or without — never promotes. The promotions route
answers a session whose `flow` is not `browser` by filing a **production approval** and returning
its link and code instead of a run (`contracts/routes.md`, *Production approvals*); the CLI prints
`Ask a person to approve: <host>/#/approve?code=K7QP-4RXM`, the way `canyon login` prints its
device code, and either waits for the decision (`--wait`) or ends there. A person signed in to the
dashboard opens the link, sees who asked and from where, what goes live and what it replaces, the
go-live checks, and the account they are approving as, and clicks Approve or Deny. Approving is the
Promote button: the same route, the same checks, the same run, recorded as the approver. The
dashboard's own Promote button, on a browser session, still promotes directly, because that already
is a person in the dashboard. Codes last **24 hours**: long enough for a person who is in a meeting,
short enough that a stale request does not sit around waiting for a click; a newer request for the
same app replaces the older one, so the link a person was handed last is the only one that works.

**Only a browser session decides.** The approve and deny routes refuse every session that is not a
browser sign-in's, before they read the code, so the agent that asked can never answer its own
request with the CLI session it asked with. Approving is a POST under `/api/v1` with the session
cookie, so the same-origin rule (`contracts/routes.md`, the Open group) is its CSRF defence, as it
is the device page's; the link only fills the page, and only the click approves. The dashboard
refuses to be framed (`frame-ancestors 'none'`), because a preview — code the agent wrote, served
under the dashboard's registrable domain — could otherwise put the approval page under a decoy and
have the person click it. The setting itself changes only from a browser session: an admin's agent
holding the admin's CLI session could otherwise turn it off and promote. Approving names the
version the page showed: if preview runs anything else by the time of the click, the approval is
`outdated` and nothing starts. The one who asked may approve in their own browser, as they could
click Promote; the setting decides where the act happens, not who (a second approver is not in
this cut, §4.4).

**What this is and is not.** An agent that drives a signed-in browser can still click Approve,
and one that reads the browser's session cookie off the machine can replay it — §4.4's reasoning
stands, and nothing here pretends otherwise. The setting covers promotion and approving a blocking
finding (amended 2026-10-10, #1824, the owner): with it on, the approve routes answer a CLI session
`403 dashboard_required` and say to fix the code and deploy again, or ask a person to approve it in
the dashboard — otherwise an agent replaying the CLI's session could approve its own finding, and the
receipt would read `every blocking finding approved`. It covers changing a policy too (amended
2026-10-10, #1966, the owner): with it on, `PUT /api/v1/policies/:rule` answers a CLI session
`403 dashboard_required` naming the Policies page — otherwise an admin's agent could turn a blocking
rule off and promote past it — and so do a custom rule's PUT and a preferences PUT naming
`approved_buildpacks` or `design_system_id`, and the guardrails setup chat, which change what a
blocking rule checks (the design system amended 2026-10-12: cleared, `design.org-system` checks
nothing). Taking production down (undeploy) is unchanged. What the setting buys is that the act
moves to where a person signs in with the identity provider, and that an agent shelling out to
`canyon` is told plainly to stop and hand the link over. The trail says which channel each step came
from (`request promotion` via the CLI, `approve promotion` via the dashboard), so an auditor reads
the difference rather than trusting it. The receipt's `prod.human-approval` line states whichever
rule is in force (`contracts/routes.md`, *The promote receipt*), the agent's manifest says it, and
the skill tells the agent to relay the link and never approve it.

**There is never a double confirmation.** On, the terminal asks nothing: the one confirmation is the
click in the dashboard. Off, the terminal's `[y/N]` (or `--yes`) is the only one.

**With sign-in off the setting cannot be on.** Every caller is the anonymous admin, so the dashboard
cannot be told from the CLI; the PUT that would turn it on is refused and every read answers it off.

**Where the setting lives: the preferences row** (`prod_approval`). `PUT /api/v1/preferences` is
already the org's one settings row, already admin-only, already audited with each changed key's
before and after, and already read by the dashboard on load. The alternative considered was a policy
state on `prod.human-approval`, and it is wrong: a policy's states are `off | advisory | blocking`, a
policy is a thing the gate evaluates against a package, and this is neither. While the setting is on
it decides that rule whatever the rule's own state. An approval is a row of its own table, keyed by
its code, with who asked, the run and package named, its expiry and its decision.

### 4.6 First boot

In this milestone there is no first-boot flow, because there is nothing to ask: the membership
lists, the admin list and the org name are all in the environment, validated at boot, and a host
that has them serves the fleet to the first person who signs in.

**`design/ONBOARDING.md` is the feature that changes that.** It adds a setup state, four screens
and an organisation settings page, and moves those values into the preferences row so a person sets
them in the product. The environment variables become the bootstrap and the override. It is its own
milestone (M2c) and its own build cut, deliberately: authentication is what unblocks a customer's
own network, and it should not wait on a settings screen.

### 4.7 MCP clients: Canyon as an OAuth authorization server

**Added 2026-11-20** (design/specs/mcp.md §2.2). `/mcp` is an OAuth 2.1 protected resource, and Canyon is its
authorization server, as it is the CLI's (§4.2) and for the same reasons: most corporate identity providers don't
register arbitrary MCP clients, and their tokens would name an audience Canyon doesn't control. The customer's provider
signs the person in, through §4.1 unchanged; Canyon decides what the client gets.

1. The client reads `/.well-known/oauth-protected-resource/mcp` (RFC 9728) and
   `/.well-known/oauth-authorization-server` (RFC 8414). It identifies itself one of two ways, since clients differ:
   a **Client ID Metadata Document** at its own `https` URL, which Canyon fetches at authorization, from public
   addresses alone, so a client's word never makes the server reach the org's own network; or **Dynamic Client
   Registration** (RFC 7591), public clients only, no secret. Either way a redirect is `https`, `http` on a
   loopback host, or a native app's own scheme Canyon names, `cursor` alone, since Cursor receives its sign-in on
   `cursor://`; nothing else, and never a scheme by pattern.
2. It sends the person's browser to `GET /oauth/authorize`, PKCE `S256` and `resource=<public URL>/mcp` (RFC 8707)
   required. Canyon answers every fault on its own page, never at the redirect: a redirect the client didn't name is
   where an attacker would want the code, and until the person has answered, Canyon sends their browser nowhere a
   stranger's link names (RFC 9700 §4.11.2), so no one turns its origin into a hop to their own site.
3. The request waits ten minutes on the **consent page**, `/#/oauth`. A browser with no session signs in first and
   comes back, as the device page does. The page names the client, where it sends the person back, which account,
   and the scopes, then where and when the request was made: the client's host (its document's, or a registration's
   redirect's) and the time. Like the device page, it says to allow only a connection the person started, and a
   registration's name, the client's own word, is called that.
4. **Allow** binds the person's browser session to the request and issues a code, single use, for a minute, which the
   browser carries back with `state` and `iss` (RFC 9207). Consent is a browser's alone, as a device code's approval
   is, because the session it hands over would otherwise let a stolen token mint itself more; the page and its routes
   are never framed, and a POST must say it came from Canyon's own page.
5. `POST /oauth/token` spends the code and makes **one session per consent**: `flow = oauth`, the approver's identity
   copied as a device token copies it, bound to the client and to `/mcp`, with the scopes. The session lives 7 days
   from the consent, as a CLI session does (amended 2026-11-21). Its access token lives an hour and its refresh token as
   long as the session; each refresh rotates both on that row in place and checks the person is still on the
   membership lists. A refresh token presented again within 10 seconds of its rotation, while the pair that rotation
   issued has served nothing, is a retry whose answer was lost, and gets a new pair in place of the one it lost, once;
   any other return of a replaced token, the lost pair's included, means one of its two holders isn't the client,
   so the session ends.

**Two scopes:** `read` (the rules, playbooks, status, logs and resources) and `act` (those, and check, deploy and
promote). A scope never widens a level: it only narrows what a token may ask. **The session is the person**, as §4.4
says of every session: `kind = person`, recorded under their name. `dashboard_required` refuses it as it refuses a CLI
session, and no cookie route takes it at all: it is bound to `/mcp`.

**Registrations don't pile up.** A registration counts as used when a code issued to it redeems, and each new
registration deletes those unused a day after they were made, unconditionally, and those with no session live or
started in 30 days. Per source, an IPv4 address or an IPv6 /64, at most 20 are unused: a new one evicts that source's
oldest, young ones too, but never one whose sign-in a person has approved while its code lives, so a source only ever
evicts its own. Past 10,000 unused in all, registration is refused and nothing evicted, while registered clients still
sign in. Registration is bounded per source address, as the sign-in routes are. A code whose registration is gone
redeems nothing. A client asking for a secret is registered as the public
client it is, told `none`.

**Revoking.** An admin's revoke-all of a person (`DELETE /api/v1/sessions?subject=`) ends their OAuth sessions with the
rest. A person sees their own sessions in Settings › People & sign-in, an admin everyone's, and ends one there by its
digest, in a browser alone, so no MCP client and no CLI ever ends a session.

**With sign-in off there is none of it.** A host where every caller is the anonymous admin serves neither the
metadata nor the OAuth routes, and neither does one whose public URL is plain `http` anywhere but loopback.

## 5. The data model

One migration, `0015_identity.sql`.

```sql
ALTER TABLE sessions
    ADD COLUMN issuer     text NOT NULL DEFAULT '',
    ADD COLUMN email      text NOT NULL DEFAULT '',
    ADD COLUMN flow       text NOT NULL DEFAULT 'browser' CHECK (flow IN ('browser', 'device', 'demo')),
    ADD COLUMN claimed_admin boolean NOT NULL DEFAULT false,  -- did the admin group claim match at sign-in
    ADD COLUMN hosted_domain text NOT NULL DEFAULT '',        -- Google's hd claim when present: the provider's word that the account is an organisation's (ONBOARDING.md)
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN revoked_at timestamptz;

DELETE FROM sessions;                                        -- every existing row is anonymous
DELETE FROM project_owners WHERE subject = 'anonymous';      -- an owner who is nobody is not an owner

CREATE TABLE device_grants (
    code_hash    bytea PRIMARY KEY,           -- sha256 of the device code; the code itself is never stored
    user_code    text NOT NULL UNIQUE,        -- 8 characters, Crockford base32, shown to the person
    session_id   text REFERENCES sessions(id) ON DELETE CASCADE,-- set when a person confirms; null while pending
    requested_by text NOT NULL DEFAULT '',    -- the source address, shown on the approval screen
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL
);
```

**`flow` gates nothing by default.** It is on the row because the org setting of §4.5 reads it, and
the promote override does (a browser session's `promote_anyway`, contracts/routes.md, amended 2026-10-08),
and because knowing whether a session came from a browser or the CLI is worth a column on its own
when somebody is reading the audit trail. It is not a permission.

**There is no `role` column, and `claimed_admin` is not one.** The role is computed on every
request as `CANYON_ADMINS contains the stored email OR claimed_admin`. The list half is read live,
so it follows the configuration the process is running with. The claim half is the one fact the
server cannot re-read, because the token was discarded at sign-in, so it is stored and it lasts as
long as the session does. §3 has the table of what that means and how an admin forces a group
demotion through. A single frozen `role` column would have made both halves stale, including the
one that did not need to be.

**Membership is checked on the same read.** The session stage already loads the session row; it
also tests the stored email against the two membership lists, so a person taken off a list is out
of Canyon on their next request rather than at their next sign-in (§3). `design/ONBOARDING.md`
moves the lists into the preferences row and this becomes a join; here they are in memory.

**The actor triple does not change, and there is no backfill.** This is what the triple was for
(`ARCHITECTURE.md:102`). `actor_subject` becomes the issuer's `sub` claim: immutable, unique, and
never reassigned when somebody's name or email changes. `actor_display` becomes the human name,
which is what every screen shows. `email` is stored beside them for the role computation and for
revocation by a person's address.

**Rows already written as `anonymous` are frozen, not backfilled.** Inventing an attribution in a
governance ledger is the worst move available to a security product. `audit_events`, promotions and
secrets keep the word. Two exceptions are deletions rather than rewrites: every session row (none
of them is valid) and every `project_owners` row naming `anonymous` (an app whose owner is nobody
is an unowned app, and `owner.required` should say so until a real person claims it). The demo
seed's owner rows are unaffected: they already carry email subjects such as `dana@meridian.example`
(`demo_seed.sql:9-12`), so the seeded fleet keeps its owners and the `unowned` tile keeps its count.

**`project_owners` needs no key change.** `canyonv2-l6f6` reports the key
`(project_id, kind, subject)` as a bug because every claim collides. It collides because every
subject is the same string. With real subjects, Alice and Bob claiming one app are two rows, and
the same person claiming twice renames their own row, which is exactly what
`contracts/routes.md:56` specifies. The ticket closes with a regression test, not a migration.

**Session lifetime and revocation.**

| Session | Lifetime | Renewal |
|---|---|---|
| browser | 12 hours absolute | sign in again; the identity provider's own session usually makes this a redirect the person never sees |
| device (CLI) | **7 days absolute** | `canyon login` again |
| demo (`CANYON_AUTH=off*`) | none, as today | none |

Seven days, not thirty: a CLI session is a bearer token in a file on a developer's laptop, and the
only thing that bounds the damage of a stolen one is how soon it dies. A week is one working
rhythm and a sign-in a person does on a Monday.

**There is no idle timeout**, on either session, which is a deliberate deviation from ASVS 3.3.1.
The reason is that the absolute caps are already short, an idle timeout costs a write on every
request to track last-use, and the case it protects against, an unattended signed-in browser, is
bounded to 12 hours here rather than the weeks a long-lived session would give it. If a customer's
policy requires an idle timeout, that is a named trigger to add one with the `last_seen` column it
needs.

**Canyon stores no refresh tokens and no identity-provider tokens of any kind.** The ID token is
validated and discarded. This removes refresh token theft from the threat model by construction,
along with the code to encrypt, rotate, refresh and revoke those tokens. The cost is that a person
removed from the identity provider keeps a Canyon session for up to 12 hours, or 7 days from the
CLI, which is what `DELETE /api/v1/sessions?subject=…` is for: it matches the session's **subject
or its email**, so an admin acting on "Dana left today" types the address they know rather than
looking up an opaque `sub`.

`canyonv2-9lj` and `canyonv2-m4o` (a session row inserted per cookieless request, nothing sweeps
them) close by construction: on an authenticated host, a cookieless request writes nothing. Expired
session rows are deleted by a sweep on the same boot path that runs migrations; expired device
grants are deleted by the insert that creates the next one.

## 6. The CLI

| Today | After |
|---|---|
| `login <host>` names a host and keeps whatever cookie came back | `login <host>` reads `/auth/config`, runs the device flow, keeps the session it was issued |
| `firstContact` proves a host is Canyon by receiving a session from `GET /api/v1/preferences` | `firstContact` proves it by `GET /auth/config` answering Canyon's shape on the Open group; on `auth: "off"` it reads `GET /api/v1/preferences` with no session and takes the host as Canyon only when the answer holds a `stacks` array; no cookie is needed, and one an older host still sets must have a session's shape (amended 2026-10-27, #2428) |
| `--token` exits 1: "this host mints anonymous sessions" | `--token` exits 1: "sign in with `canyon login <host>`; service tokens are not issued yet" |
| no `logout` | `canyon logout` calls `DELETE /api/v1/sessions` and removes the session from the config file |
| a 401 is unreachable | a 401 on a session it sent prints "your session has expired. Run: canyon login <host>"; with none sent, "You're not signed in to <host>." (amended 2026-10-01, `canyonv2-5r11.5`); both exit 1 |
| `promote` posts and ships | unchanged, and the receipt now names the signed-in person. With the §4.5 org setting on, a CLI promote files a production approval and the CLI prints its link for a person to approve (amended 2026-10-07, #1820) |
| config holds `{host, session}` | unchanged; the CLI learns a session is dead from a 401, not from a stored expiry |

**Service tokens for CI are not in this cut.** `README.md:194` already says v1 gates deploys from a
machine and that a PR or CI gate is on the roadmap, and M8's GitHub App courier authenticates by
webhook signature rather than by a session. The named trigger is the first customer running the
CLI in a pipeline. When it fires, the shape is an admin-minted named token of `kind: agent`,
which is why the actor triple has that kind.

## 7. Authorization

Two roles, and they exist for one verified reason: turning authentication on means everybody in the
company's directory can sign in, and today any session can `PUT /api/v1/policies/:rule` and set a
blocking rule to `off`. A member who can disable the gate for the whole org defeats the product.

| | member | admin |
|---|---|---|
| deploy, check, pull, logs, set a secret, promote to production | on apps where they are a contributor or an owner, else `403 {error:"team_required"}` | every app |
| approve a blocking finding (#2204; design/PERMISSIONS.md) | a responder, on every app; anyone else `403 {error:"approver_required"}` | every app; for both, never a run they started (`403 {error:"author_cannot_approve"}`), and only from a browser |
| share an app, change its team, delete it | on apps they own, else `403 {error:"owner_required"}` | every app |
| see an app's name, owners and summary | every app | every app |
| `PUT /policies/:rule`, `PUT /preferences`, `POST /design-systems`, `POST /guardrails/setup` | no, `403 {error:"admin_required"}` | yes |
| revoke another person's sessions | no | yes |
| the Guardrails screen; the org's rows on the audit trail (policy and settings changes, other people's sign-ins) | no — no Guardrails in the rail, `#/guardrails` is the admins-only page; the trail answers their team apps' rows and, of the rows naming no app, only their own `sign in`, `sign out`, `denied` and `sign in refused` (amended 2026-10-07). The policy reads stay theirs: agents and the CLI read them | yes |

Admin is `CANYON_ADMINS contains the email OR the admin group claim matched` (§3). Two ways in,
both from the start: an organisation with a directory group for platform engineers should not
maintain a second list by hand, and an organisation without one, which includes every Google
Workspace customer and every small team, should not have to create a group to grant one person
admin. An admin who is on neither membership list cannot sign in at all, and the boot says so.

**Not first-sign-in bootstrapping:** on a real directory that is a race between whoever opens the
tab first. `CANYON_ADMINS` names the first admins explicitly, which is what the setup state of
§4.6 gates on, and it keeps granting afterwards as the recovery path.

Those are the org's two roles; what a member may do on each app is that app's team, and
`design/PERMISSIONS.md` is the one source for it (levels, grants, sharing). A third role, the
security responder, who stops any app's production and never ships a new version, is defined there
(*Security responders*, decided 2026-10-11). The two receipt rules
need no role at all: they need a name, which
section 4.4 gives them. The roles are here only for the hole that switching authentication on
opens.

An agent running under a person's session promotes, because that person promotes. An org that
wants the act to happen in Canyon turns on the setting in §4.5.

## 8. The dashboard

- **Sign-in screen.** A 401 from any read replaces the whole app with the sign-in page: the org
  name, one line of explanation, and one button reading `Sign in`. It navigates to `/auth/start`.
  No provider logo, no provider name, no configuration in the bundle. After a sign-out the person
  chose, and only then, one muted line below it:
  `Signing out of Canyon does not sign you out of your company account.`
- **Sign-out.** At the foot of the rail on every page, beside the signed-in person's name. Calls
  `DELETE /api/v1/sessions`, then shows the sign-in page, whose button then navigates to
  `/auth/start?prompt=select_account` (§4.1, step 5): the provider's session outlives Canyon's, and
  without it the next click signs the same person straight back in, with no way to choose another
  account. The company session itself is left alone; signing somebody out of their whole SSO is
  not Canyon's to do.
- **Expiry.** Any 401 mid-session does the same thing. There is no countdown and no warning modal.
- **The honesty banner's fate.** `S.bannerLoopback` and `S.bannerExposed` (`web/src/strings.ts:11`)
  survive unchanged and are shown only when `CANYON_AUTH` is off, which the fleet read already
  reports as `banner: loopback | exposed` (`contracts/routes.md:172`). On an authenticated host the
  fleet answers a third value, `banner: "none"`, and the line goes; the signed-in person's name and
  role, which the fleet read carries as `viewer`, sit at the foot of the rail. The escalated string keeps meaning what
  it says, on the one host where it is true.
- **CSRF.** The same-origin rule on unsafe methods is already on main (`session.go:57`,
  `contracts/routes.md:18`) and covers `DELETE /api/v1/sessions`. The two new Open browser routes
  are safe methods: `/auth/start` is a `GET` that only redirects, and `/auth/callback` is protected
  by the state value in its own cookie, which is the standard defence and the reason that cookie
  cannot be `Strict`. `/auth/callback` takes no `next` parameter and always lands on `/`, so there
  is no open redirect to get wrong.
- **Session fixation** is not reachable: the callback inserts a new row and sets a new cookie, and
  there is no pre-authentication session to promote.

## 9. Threats

| Threat | Where it stands |
|---|---|
| The session in the CLI config file is stolen | It is Canyon's own opaque value, not an identity-provider credential. It dies in 7 days and an admin can revoke it by the person's email. Whoever holds it can do what that person can do, including promote, which is what makes 7 days rather than 30 the right number. The file stays `0600` and a looser mode already makes the CLI ignore it (`cli/config.go:53`). |
| Refresh token theft | Not possible. Canyon stores no refresh tokens and no identity-provider tokens. |
| Anyone with a Google account signs in | Membership is never empty: it comes from the environment or, once somebody has onboarded the host, from the row, and the write that would empty both lists is refused `no_members` (ONBOARDING.md §5). Until setup is finished only a `CANYON_ADMINS` address may sign in. The email is checked against both lists at the callback, inside the transaction that writes the session, before any row is written. |
| The off switch is left on | `off` requires a loopback bind **and** a loopback public URL, and has no override, so a proxy that passes the public `Host` cannot make it a public Canyon; one that rewrites `Host` to `localhost` can (*A demo published beyond loopback*, Not closed). `demo` says what it is for in the value, and refuses a public URL that is not loopback, so a demo compose file pointed at a company's domain does not start. The fleet banner escalates, and every boot logs the mode. |
| A demo published beyond loopback | `off` and `demo` serve only this machine's connections and close every other at accept, before any HTTP (`cmd/canyon-server/localpeer.go`, #2449): a loopback peer, or, under `demo` with `CANYON_PUBLISHED_ADDR` a loopback address as the compose demo sets it, the gateway of the Docker network the connection arrived on, where Docker re-originates the host's own connections to its published port. The first refusal is logged, once a process, and no other. Under `demo` a `CANYON_PUBLISHED_ADDR` that is not a loopback IP refuses the boot, naming it: published beyond loopback, Docker's proxy hands a remote client the gateway's address. A `Host` check can't do it: any client sends `Host: localhost`. So `CANYON_ADDR=:7777` on a bare VM gives a remote client an empty reply. **The gateway is served only once Docker is seen to publish on loopback** (amended 2026-10-26, #2491): a peer rule cannot tell the host from a remote client that Docker's proxy re-originates from the gateway — an IPv6 client of a port published on every address, a daemon with `iptables: false`, rootless Docker, Podman, Docker Desktop with a publish beyond loopback — so the server asks Docker instead of believing `CANYON_PUBLISHED_ADDR`. At boot, off the boot path, it asks the executor, which holds the Docker socket, about the running container `canyon-server` (contracts/executor.md, `GET /published`): its `Id`, and its bindings as created and as held now. It serves the gateway only when the answer is its own container's — its host name, which Docker sets to the `Id`'s first 12 hex, begins the `Id`, so a stopped or renamed `canyon-server` from another demo vouches for nothing — Docker holds at least one binding of its port now, and every binding of that port, created or held, is on `127.0.0.0/8` or `::1`. A binding on every address (`""`, `0.0.0.0`, `::`), on any other address, or no held binding of the port at all stops the server with a log line naming the binding and the fix: publish on `127.0.0.1` alone, or run `oidc`. An executor that cannot answer — not up, a `404`, a `502`, a container that is not running, an answer out of the contract's shape, an ask past its 10 seconds, or an answer about another container — keeps the gateway closed, each reason logged once and asked again from one second doubling to a minute; loopback is served all along. A container's bindings are fixed when Docker creates it, so the one answer holds for the process's life and nothing asks again on a timer or per connection. So a `ports:` line edited to `7777:7777` stops the demo rather than serving anyone. `make demo-check` publishes the server on `0.0.0.0` beside the shipped `127.0.0.1` in a throwaway override and expects it stopped, naming the binding. **Every network the server joins:** under `demo` the rule admits the gateway of whichever network a connection arrives on: `canyon-state`, `canyon-internal`, `canyon-provisioning`, `canyon-egress-ctl` and each app's /24. Only Docker's isolation between its networks keeps an app's connection from being masqueraded out through one of those gateways; `make demo-check` pins it, from an app's container, against the server's addresses on all four, which must not even connect. A container with `NET_ADMIN` on one of those networks could take its gateway's address; today only `egress`, on `canyon-egress-ctl`, and each app's floor holder on its app's network (#2503) have it, and in neither does anything run but Canyon's own floor script and `sleep`: the app that shares a holder's namespace holds no capability, and capabilities are a process's, never a namespace's. **Not closed, and said plainly.** *A relay on this machine:* any relay on the host that hands other machines' connections to the loopback port with `Host: localhost` — nginx or Caddy rewriting `Host`, `ngrok --host-header=rewrite`, cloudflared's `httpHostHeader`, `ssh -g -L` — is served as this machine, and each caller it forwards is an admin, as is a mesh sidecar that ends a remote connection on loopback under `demo` on a cluster, such as Istio's inbound Envoy, which dials from `127.0.0.6`; the port bindings say nothing about either. Only kind runs `demo` on a cluster (the Helm chart refuses `off` and `demo`, and deploy/k8s defaults to `oidc`): never run `demo` behind a relay or in a mesh. **An app calling the host's own forward, closed on the app side** (amended 2026-10-26, #2503): on Docker Desktop, OrbStack and Colima, `host.docker.internal` (Colima's `host.lima.internal`, `192.168.5.2`) leads to the Mac's own `127.0.0.1:7777` forward, and a connection that loops through it is this machine to every check here, correctly, so no rule of the server's can refuse it. So no app's connection reaches the host: each app on an open network runs in a floored holder's namespace that refuses its network's gateway, every address of the Docker host's own (read from the host's namespace as the holder starts, a public one among them), link-local and every private range but what `CANYON_APPS_ALLOW` opens, and an app on an internal network reaches only Envoy, whose floor refuses Canyon's networks, loopback, `0.0.0.0/8` (where OrbStack's `host.docker.internal` is), every private, shared, link-local and reserved range and every address of the Docker host's own (#2524), and every public port but 80 and 443 (contracts/executor.md, *An app reaches no private, link-local or host address*), whatever the sign-in mode. `make demo-check` proves it with a forward to the loopback publish on the Docker host's own addresses, host-gateway's, the app network's gateway and a public one put on the host's loopback for the check, which an app must not even connect to. Still open: an app deployed before this release runs on its open network unfloored until its next deploy — **redeploy every app once after upgrading**; the executor logs each one once each time it starts, and a failed redeploy never names one back; a `CANYON_APPS_ALLOW` entry whose range holds an address of the host's that the holder did not read reopens it (the vendors' `host.docker.internal` addresses are refused at the executor's start), and the configuration reference says never to name one; and an address the host takes after a holder starts (a new interface, a lease that moved) is the host's unrefused until the app's next deploy, unless it is in a private range. Without `CANYON_PUBLISHED_ADDR`, as on a bare host or a cluster, no gateway is served and the executor is never asked, so a router or a load balancer's address never passes for the host. `make demo-check` and the deploy bundle's check prove it with a curl from canyon-internal. |
| A web page drives a host with sign-in off (DNS rebinding) | `off` and `demo` serve only a `Host` naming this machine — `localhost`, a name under it, a loopback address — and answer any other `421` before any route (`loopbackOnly`, contracts/routes.md), `/ready` included: a cluster's probe asks it from inside the pod, at loopback (amended 2026-10-26, #2449). Browsers resolve `*.localhost` to loopback themselves and never ask DNS for it, so no page can rebind such a name; a page that rebinds a name of its own to `127.0.0.1` sends that name, and is refused. The demo's sample apps reach the server from its own network namespace, at `localhost` (amended 2026-10-23, #2447). |
| Identity provider misconfigured, nobody can sign in | The configuration's shape is fatal at boot, so a typo never replaces a working process. A provider outage keeps the host **not ready** rather than crash-looping. An empty or wrong `CANYON_ADMINS` is recovered by editing the environment and restarting, which every self-hoster can do by definition. There is deliberately no break-glass account, because a break-glass account is a permanent credential guarding against a five-minute restart. |
| An agent acts beyond the person | **Not bounded, and said plainly.** An agent holding the person's session can ship to production from that person's machine, in the same way it can push to their repositories or run anything else their shell can run. The receipt names the person, which is true in the only sense the server can know: they signed in, and their session did it. A rule requiring a browser would not change this, because an agent can drive a browser too. For an org that does not accept that, the answer is the §4.5 setting: turn it on and the act happens in Canyon, where the person is the one clicking. |
| **Device-code phishing** | Somebody talks a person into approving a code the attacker generated, and walks off with that person's session. Three defences, all on the approval screen: it names **the address the request came from and when**, it says `Only approve a code you generated yourself`, and `verification_uri_complete` prefills the code but **never skips the confirm click**, so no single link can complete a sign-in. |
| A preview container calls the control plane | On an authenticated host it gets a 401 and writes nothing. On the compose demo, where the session stage would mint it an admin session, the server closes the connection at accept when the peer is inside `CANYON_APPS_SUBNET`, the executor's pool of app networks, and not its /24's gateway, where host traffic arrives from, or is any IPv6 address but loopback (`cmd/canyon-server/appnet.go`), before any HTTP, session row or log line; `make demo-check` proves it with a curl from inside an app's network (`canyonv2-f8q`). |
| A sibling app's page sends a change to an app | Every app is on one site, so the app's `SameSite=Lax` cookie rides along on a request another app's page makes. The app surface refuses a `POST`, `PUT`, `PATCH` or `DELETE` whose `Sec-Fetch-Site` or `Origin` is not the app's own — `403 cross_origin`, as `/api/v1` does — so a change reaches an app only from its own pages or from a client that is no browser. |
| A sibling app plants an app's cookies | Under https the app session and flow cookies are `__Host-` prefixed, so no other host can set them, and a code is good only beside the flow cookie its sign-in set. A host whose public URL is plain http has no such prefix, as its session cookie has none. |
| Session rows accumulate | Only sign-ins create rows now. Expired sessions are swept at boot; expired device grants are deleted by the next insert. |
| Device code brute force | The user code is 8 Crockford base32 characters from `crypto/rand` (40 bits), single use, ten minute expiry, confirmed only behind an already signed-in browser session, and guessing one donates the guesser's own identity to a stranger's CLI, so the approval routes are unbounded. The device code is 256 bits, the only secret the flow routes take, and beyond guessing at any rate; its poll is bounded per grant by the five-second interval, so a code's ten minutes admit at most 120 polls. `GET /auth/start`, `GET /auth/callback` and `POST /auth/device` share one bucket per source address, **120 requests refilled at 2 a second** (`cmd/canyon-server/limiter.go`): a team signing in at once is never refused, and no address can start more than 1,200 sign-ins in ten minutes. |
| Secrets in logs | The client secret, PKCE verifier, state, nonce, authorization code and every token are never written to any log, at any level, on success or failure. |

**A refused sign-in writes one admin-only row; every other failed sign-in writes none** (amended
2026-10-05: a reviewer of privileged access read pod logs for refusals). A person the provider vouched
for and Canyon turned away — on neither membership list, or not an admin while Canyon is being set up — is
`sign in refused` on the trail (`contracts/routes.md`, the audit read), written once the browser is
answered, outside the transaction that refused it, and read by admins alone. A sign-in that fails
before the provider vouches for anyone (a state, nonce or code that does not match, the provider's own
error) changed nothing about anybody and stays one log line with the issuer, the time and the reason,
never the code or the token. With a provider whose issuer is every account on the internet's (Google),
anyone can put an email in such a row, bounded by the sign-in routes' per-address bucket, and the trail
keeps it as long as it keeps rows: the cost of reading refusals at all.

## 10. Contract changes (seam PR)

`contracts/` is not edited on this branch. The text below is the proposal for the seam PR, which
both humans approve and which merges before any consumer (rule 5).

### `contracts/routes.md`

**Line 4 to 5**, replacing "Auth: demo hosts issue anonymous sessions transparently; nothing here
changes when OIDC lands (day 2) except who the actor is."

> Auth: every route under `/api/v1` requires a session, and a request without a valid one is
> `401 {error:"unauthenticated", message:"sign in to Canyon"}` before the handler runs, with no
> session row and no audit row. Sessions are minted only by the flows in the Open group below. A
> host running `CANYON_AUTH=off` or `demo` serves a caller without a session as actor
> `person / anonymous` instead, storing no session and setting no cookie, and is the only configuration in
> which that happens. A session's `flow` (`browser | device`) is recorded and, with one org setting
> off by default, grants nothing and withholds nothing: whatever runs under a person's session
> acts as that person.

**The Open table** (after line 40) gains five rows and `GET /auth/config` stops being a 404:

> | `GET /auth/config` | `200 {canyon: true, auth: "oidc" \| "off", org, device, cli, public, setup}` — what the CLI reads to learn whether this host authenticates and how. `setup` is `true` while the host is in the setup state (`onboarded_at` null under `oidc`, above), when nobody can approve a CLI sign-in yet, so a CLI whose code expired can say so, and only then; `false` otherwise and when auth is off. The only route that proves a host is Canyon. `cli` is the CLI version this host was released with, so a CLI can tell the person it is older than its host; `public` is the host's configured public URL, the same string the skill manifest hands out, so a CLI can name the session cookie the way this host set it. `device` is whether this host serves the device flow: `true` under `oidc`, `false` when auth is off; `org` is the org's display name. |
> | `GET /auth/start?prompt=` | `302` to the issuer's authorization endpoint, with PKCE (`S256`), a state value and a nonce in one short-lived cookie built by the session cookie's builder: `__Host-` prefixed and `Secure` on https, `HttpOnly`, no `Domain`, `SameSite=Lax` · `prompt` is absent or `select_account`, passed on to the issuer as OIDC's `prompt` (the dashboard sends it after a sign-out), else `400 {error:"invalid_prompt"}` · `404` when `CANYON_AUTH` is off. |
> | `GET /auth/callback?code=&state=` | `302` to `/` with the session cookie set · `400 {error:"auth_failed"}` on a state or nonce mismatch, a code exchange failure, an ID token that does not validate, an `email_verified` that is present and false, or an email on neither membership list (`CANYON_OIDC_ALLOWED_DOMAINS`, `CANYON_OIDC_ALLOWED_EMAILS`). No detail is echoed; the reason goes to the log. `404` when `CANYON_AUTH` is off. |
> | `POST /auth/device` | `200 {device_code, user_code, verification_uri, verification_uri_complete, expires_in, interval}` (RFC 8628; `user_code` is 8 Crockford base32 characters from `crypto/rand`, `expires_in` is 600) · `404` when `CANYON_AUTH` is off |
> | `POST /auth/device/token` | `{device_code}` → `200 {session, expires_at, subject, display, org}` · `400 {error:"authorization_pending" \| "slow_down" \| "expired_token" \| "access_denied"}`. A device code is single use: consumed on first success, and dead after `expired_token` or `access_denied`. `404` when `CANYON_AUTH` is off. |

`slow_down` is RFC 8628's own value and belongs in the union: without it a CLI polling too fast
gets an error it has no rule for, and every conformant client already knows to back off on it.

**New row in the sessions group:**

> | `DELETE /api/v1/sessions?subject=` | `204` — without `subject`, signs the caller out by revoking their own session. With `subject`, revokes every session whose actor subject **or email** matches, and is `403 {error:"admin_required"}` (its message says which admin to ask, contracts/routes.md *App teams*, amended 2026-10-12) for a member. A `subject` matching no session is still `204`. One audit row either way: the act is recorded, not the count. |

**`GET /ready`** (line 34): `waiting_on`'s closed set gains `oidc`, for a host whose OIDC discovery
has not yet succeeded. It is in the closed set so the value is contracted rather than error text,
which is the rule that row already states.

**Line 54**, the project read: `banner` gains `none`. The project read is otherwise unchanged.

**Line 56**, `PUT /api/v1/projects/:id/owner`: strike "(demo) / from identity (day 2)". The body
stays `{display}` and the actor comes from the session, which is what the handler already does.

**Line 108**, the gate-runs `429`: strike "once user provisioning exists (the auth seam), … before
that no limit applies". Identity is the auth seam and it has landed, so the per-project and
per-actor limits are live from M2b and the sentence describing their absence is false.

**Line 112**, the approve route: unchanged. "Only a person approves" already says what it needs to,
and it is now true of a named person rather than of every session.

**Line 119**, the promotions route: `prod.human-approval` passes when the actor is a person, which
is unchanged, and the `why` now names them. Nothing else changes here. (§4.5's org setting was
designed and not built then, so the seam carried no key for it: an unconsumed contract field is a
promise nobody keeps. It was decided on 2026-10-07, #1820, with its key, `prod_approval`, and its
routes.)

**`GET` / `PUT /api/v1/preferences`**: no change in this milestone. The organisation's settings
arrive with `design/ONBOARDING.md`, which is its own seam PR.

**Line 172**, the fleet read: `banner` ∈ `none | loopback | exposed`, and the response gains
`viewer: {display, role}` — the signed-in person, for the header that replaces the banner. It is on
the fleet read because that is the dashboard's first call and a second round trip to learn who you
are would show the page with a blank header. On a demo host `viewer` is the anonymous actor.

**The four admin-only routes** (`PUT /policies/:rule`, `PUT /preferences`, `POST /design-systems`,
`POST /guardrails/setup`) each gain `403 {error:"admin_required"}` (its message says which admin to ask, contracts/routes.md *App teams*, amended 2026-10-12) for a member. There is no flow
test on them: the CLI has no verb for any of the four.

**Line 176**, the audit read: its `action` prose gains `sign in`, `sign out` and `revoke sessions`.
A **failed** sign-in writes no row.

### `contracts/events.md`

No change. A sign-in is an audit row, not a timeline row: the timeline is the run and fleet story.
The existing `audit.appended` row covers the audit screen's poll unchanged.

## 11. What changes in existing docs

Applied on this branch, except `contracts/`.

| File and line | From | To |
|---|---|---|
| `design/ARCHITECTURE.md:93` | `login` is "device flow — day 2, hosts with auth only" | device flow, v1, against Canyon's own device endpoints |
| `design/ARCHITECTURE.md:95` | "Demo hosts have no auth by construction" | authentication is on by default; the three modes of section 2 |
| `design/ARCHITECTURE.md:102-103` | owner "recorded with `actor = anonymous`"; "OIDC lands day 2 with no backfill" | owner recorded from the signed-in identity; OIDC has landed and there was no backfill |
| `design/ARCHITECTURE.md:106` | pipeline "session (anonymous in demo; OIDC day 2)" | "session (OIDC; anonymous only when `CANYON_AUTH` is off)" |
| `design/ARCHITECTURE.md:119-121` | rate limits wait on "user provisioning (the auth seam)" | identity is that seam; the limits are live from M2b |
| `design/specs/cli.md` §8.2 | login "does the two things that are real"; device flow is "Day 2, for hosts with auth" | rewritten to the device flow, `logout`, the 401 message, the new `--token` refusal; heading kept |
| `design/specs/cli.md` §6.3 | the receipt's human-approval row names nobody | unchanged transcript, `--yes` kept; the receipt names the signed-in person, and the §4.5 setting's approval link is a new case (decided 2026-10-07, #1820) |
| `design/specs/screens.md:30-31` | "No login. The demo has no auth." | the demo has no login; every other host opens on the sign-in screen |
| `design/specs/screens.md:147-151` | the honest banner, unconditional | the banner is the demo's, and the authenticated header carries the signed-in name |
| `design/specs/screens.md`, new sections | none | **A0 — Sign in** and **the device approval screen**. The onboarding screens live in `design/ONBOARDING.md`, with a pointer where they would have sat |
| `design/EXPERIENCE.md:96-97` | the two banner strings | unchanged as strings; a note that they are the demo's |
| `design/EXPERIENCE.md:159` | "anonymous demo sessions" | "authenticated sessions, anonymous only in the demo" |
| `README.md:57` | "You land, with no login" | true of the demo, said as the demo |
| `README.md:194` | "v1 has no team mode … everything is attributed to `anonymous`" | v1 authenticates against your identity provider; the demo profile is the unauthenticated one |
