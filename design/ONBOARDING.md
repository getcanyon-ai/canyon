# Canyon — Organisation onboarding

**The rule this document exists to enforce: the person who installs Canyon answers four questions
on a screen, not five environment variables in a values file.**

`design/AUTH.md` is authentication: who may sign in, how, and what the session means. It reads the
organisation's values from the environment, because that ships first and it unblocks a customer
running Canyon on their own network. This document moves those values into the product. It is a
separate milestone, **M2c**, on purpose: a settings screen must not be what authentication waits
for.

Nothing here changes how authentication works. The membership lists, the admin list and the admin
claim keep their shapes and their rules exactly as `AUTH.md` §3 defines them. What changes is where
they are read from and who can change them.

---

## 1. Two kinds of value, and why the split

| | Stays in the environment | Moves into the product |
|---|---|---|
| | `CANYON_AUTH`, `CANYON_OIDC_ISSUER`, `CANYON_OIDC_CLIENT_ID`, `CANYON_OIDC_CLIENT_SECRET`, `CANYON_PUBLIC_URL` | org name, `allowed_domains`, `allowed_emails`, `admin_emails`, `admin_claim`, `admin_value` |
| Why | an operator sets them, they describe the installation rather than the company, and **a client secret must not live in the database** | a person sets them, they change as the company changes, and editing a values file and restarting a control plane to add one colleague is not a product |

The organisation's values live in the **preferences row**. It already exists, is already one per
host, is already admin-only to write, and is already audited with each changed key's before and
after. A second settings table for six keys would be a second place to look.

| Key | Shape |
|---|---|
| `org_name` | string, 1 to 128 characters; the receipt's org, replacing `CANYON_ORG` |
| `allowed_domains` | array of bare lower-cased domains |
| `allowed_emails` | array of lower-cased addresses |
| `admin_emails` | array of lower-cased addresses |
| `admin_claim` · `admin_value` | strings, both set or neither |
| `onboarded_at` | RFC 3339, or null while the host is in the setup state |

**The environment variables survive as bootstrap and override.** `CANYON_ADMINS` names who may
sign in and administer before anybody has configured anything, and it keeps granting admin
afterwards, which is the recovery path when every admin in the row has left; whoever can edit the
host's environment can already edit its database, so this grants no power they lacked.
`CANYON_OIDC_ALLOWED_DOMAINS`, `CANYON_OIDC_ALLOWED_EMAILS`, `CANYON_ADMIN_CLAIM` and
`CANYON_ADMIN_VALUE` become **overrides**: an estate that provisions Canyon from Helm or Terraform,
or one that is air-gapped, configures a host with almost no human clicking anything — one click
remains, on Finish: the setup state ends when somebody sets `onboarded_at`, and treating an
all-overridden host as already set up is more logic than the saved click is worth — and an operator who
wants the membership lists pinned in code review can pin them. A key the environment sets is shown
**read-only** on screen, naming the variable it comes from (`Set on the server, by <VARIABLE>.
Change it there, not here.`), so nobody edits a value that will not take. With the addresses pinned,
step 2 offers no Add and drops its line about adding an address.

## 2. The setup state

A host in `oidc` mode whose preferences row has no `onboarded_at` is in setup.

- **Only an admin can sign in**, and admin is a union: the bootstrap list (`CANYON_ADMINS`), the
  row's admin addresses, or a matched admin claim (`canyonv2-cr23`). The callback checks that union,
  not the variable. On a host nobody has set up yet the row's list is empty, so the first person
  through the door can only be a bootstrap admin — but from step 3 onwards there is a window, and it
  is deliberate: an admin added during setup can sign in before setup is finished, which is how two
  people set a host up together. What the union costs is that the list can change under a sign-in;
  what it buys is that adding an admin means adding an admin, on every host, including this one.
- **Every other sign-in is refused and changes nothing.** The token is validated, the email is read,
  it grants admin by none of the three, and the callback redirects to `/#/setup-required`, which
  says one line: `Canyon isn't set up yet. Ask the person who installed it to finish setup.` No session row,
  one log line, and one `sign in refused` audit row (`reason: setup`), which only an admin reads
  (amended 2026-10-05: a reviewer of privileged access read pod logs for refused attempts; the trail
  now keeps them, and so keeps the emails of a company that arrived early, as long as it keeps rows).
- **The fleet is not served.** Every `/api/v1` route except `GET` and `PUT /preferences` answers
  `409 {error:"setup_required"}`, and the dashboard renders onboarding rather than the fleet.
- **`/ready` still answers `200`.** The process is healthy, migrated and connected; what is missing
  is somebody's decision. A `503` would make a load balancer pull the host out of rotation, and
  then nobody could reach the onboarding screen to fix it. Readiness is about the host, not about
  whether a human has finished a form.

**An upgrade is not a setup; a fresh install is.** The migration that adds these keys sets
`onboarded_at` to `now()` **only where the database shows signs of use** — an audit trail, or
projects that predate the audit table (`db/migrations/0018_org_settings.sql`). A host that was
already serving a fleet is not a host somebody needs to onboard, and leaving it null would drop a
working install into the setup state. A database nothing has touched is a fresh install, and its
first admin must be shown onboarding.

**Do not "correct" the migration to set the marker on every existing row.** The row exists before
anybody signs in — it is seeded with the preferences defaults — so an unconditional `UPDATE` marks
every new host as already onboarded. Onboarding then becomes unreachable on every install, with no
error and nothing in the log to look at: the host simply serves an empty fleet and no screen ever
asks the questions. The `WHERE` clause is the whole rule, not a precaution.

## 3. The screens

### Step 1 — the organisation

```
┌────────────────────────────────────────────────────────────────────────────┐
│  Set up Canyon                                                    1 of 4   │
│                                                                            │
│  What is this organization called?                                         │
│                                                                            │
│  ┌──────────────────────────────────────┐                                  │
│  │ Meridian Insurance                   │                                  │
│  └──────────────────────────────────────┘                                  │
│  It appears on every promotion receipt.                                    │
│                                                                            │
│                                             [ Continue ]                   │
└────────────────────────────────────────────────────────────────────────────┘
```

Prefilled from `CANYON_ORG` when the operator set it.

### Step 2 — who may sign in

The screen that carries the security consequence, built around the two lists rather than around a
guess.

```
│  Who may sign in?                                                 2 of 4   │
│                                                                            │
│  People                                                                    │
│  ┌──────────────────────────────────────┐                                  │
│  │ dana@meridian.example             ×  │  you                             │
│  └──────────────────────────────────────┘                                  │
│  + Add someone's email address                                             │
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │ ☐  Allow everyone at meridian.example                                │  │
│  │    Anyone at this domain who can sign in to your identity provider   │  │
│  │    can sign in to Canyon.                                            │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│  + Allow another domain                                                    │
│                                                                            │
│  Canyon does not send invitations. Add an address, then tell that person   │
│  to open Canyon and sign in.                                               │
│                                                                            │
│                                             [ Continue ]                   │
```

Four decisions are doing work here.

**The admin's own address is prefilled as an allowed address, marked `you`.** It is the one value
that is always right: they are signed in, so that address demonstrably works, and without it an
admin can finish onboarding having locked themselves out.

**The domain is never prefilled.** It is one unticked checkbox, phrased as the thing it does, with
the consequence written underneath. An admin whose address is `dana@gmail.com` sees
`Allow everyone at gmail.com` unticked and does not tick it. The previous draft prefilled the
domain, which would have admitted the entire internet on one Continue click for every small team.

**There is no list of consumer domains.** Blocking `gmail.com`, `outlook.com` and the rest would be
a heuristic, it would be wrong for a company whose staff genuinely are on one of them, and it would
need maintaining forever. An unticked box with a true sentence under it does the same job and
cannot be out of date.

**The box is offered only when the provider itself says the domain is an organisation's.** Google
puts an `hd` claim in the ID token for a Workspace account and none for a personal account, so a
`dana@gmail.com` admin never sees `Allow everyone at gmail.com` at all: there is nothing to allow,
and Google said so. A single-tenant Entra issuer and an Okta org issuer are bound to one
organisation by construction, so the box is shown. Generic OIDC has nothing to assert against and
the box is shown. The sign-in records the `hd` claim on the session row (`hosted_domain`, empty
when absent) because the token is discarded; that is the one fact this control reads. It is the
provider's assertion, not Canyon's guess, which is why it is not a heuristic. `+ Allow another
domain` stays for a typed domain in every case, with the same sentence beneath it.

**Continue is disabled while both lists are empty**, with one line saying at least one person or
one domain is needed. An empty membership is either an open door or a brick.

### Step 3 — admins

```
│  Who administers Canyon?                                          3 of 4   │
│                                                                            │
│  By email                                                                  │
│  ┌──────────────────────────────────────┐                                  │
│  │ dana@meridian.example             ×  │  you                             │
│  └──────────────────────────────────────┘                                  │
│  + Add an admin                                                            │
│                                                                            │
│  By group (optional)                                                       │
│  ┌──────────────────┐  ┌──────────────────────────────────┐                │
│  │ groups           │  │ 4c9f…  (the group's object id)   │                │
│  └──────────────────┘  └──────────────────────────────────┘                │
│  Anyone whose sign-in carries this claim is an admin. We read it at        │
│  sign-in, so this takes effect for you at your next one.                   │
│                                                                            │
│  Admins change these settings and the policies. Everyone else deploys      │
│  and promotes; only an admin or a security responder approves a blocking   │
│  finding.                                                                  │
```

Prefilled with the bootstrap list. The note about the next sign-in is the important line: the claim
is read from the token and the token is discarded (`AUTH.md` §3), so the person setting this up has
already signed in and does not carry it yet. Saying so here is cheaper than a support conversation
later.

An admin address that is on neither membership list is added to `allowed_emails` automatically,
with a line saying so. Two lists that disagree about one person is a bug report waiting to happen.

### Step 4 — review and finish

```
│  Ready                                                            4 of 4   │
│                                                                            │
│  Organisation   Meridian Insurance                             edit        │
│  Sign-in        dana@meridian.example, and anyone at meridian.example edit │
│  Admins         dana@meridian.example, and anyone in group 4c9f…   edit    │
│  Identity       Microsoft Entra ID                                         │
│                 login.microsoftonline.com/…/v2.0                           │
│                                                                            │
│                                             [ Finish setup ]               │
```

The identity row is read-only and not offered for editing, because it is the operator's, from the
environment. Showing it anyway is what tells the admin their sign-in and these settings belong to
the same host.

`Finish setup` writes the preferences row, `onboarded_at`, and one audit row, `complete setup`,
naming them. The host leaves the setup state on that write and the fleet loads. **No confirmation
dialog:** every value here is editable afterwards, on the same screen.

### Afterwards: the organisation settings page

The same three sections, on one page instead of four steps, under **Guardrails**, for admins. Not a
second screen: an org changing its membership a year later should meet the form it already filled
in, with the same words in the same order. A member who reaches the URL gets it read-only with one
line saying an admin changes these.

- `Save` per section, and **each change is one audit row with its before and after**, which the
  preferences route already does for every key.
- **A removal takes effect at once.** Membership is checked per request against the stored email
  (`AUTH.md` §3), so taking somebody off a list ends their sessions on their next request, browser
  and CLI alike, with no revoke step to remember.
- **Removing the last member is refused**, the same rule as step 2's disabled Continue.
- **Removing the last admin address is refused when no admin claim is set.** `CANYON_ADMINS` would
  still be the recovery, but recovering through a restart is not something a screen should let
  somebody walk into.
- **Removing a domain cannot say who has been signing in from it, and the screen must not imply it
  can.** There is no store to ask: a `sign in` row on the audit trail carries the actor's subject and
  display name, never an address, and the only place the address lives is the session row, which is
  deleted when it expires. A "3 people in the last 30 days" count would have to be invented. What the
  screen can say is what is on the row in front of the reader: which of the org's listed admin
  addresses are at that domain — the same addresses `admin_locked_out` names before the write — and
  whether the reader's own address is one of them, which `viewer.email` tells it. Say those, and say
  that the removal takes effect on the next request.

## 4. A worked example: four people, no domain

This is the story the address list exists for, and it is worth walking end to end because every
step of it is somebody's first ten minutes with Canyon.

Four people building something together. Personal Google accounts, no Workspace, no company domain,
one box running `docker compose`.

**What they set up at Google**, once, in the Google Cloud console: create a project, open
*APIs & Services → OAuth consent screen*, choose **External**, name the app and add the three
scopes `openid`, `email` and `profile`. Then *Credentials → Create credentials → OAuth client ID →
Web application*, and add one authorized redirect URI: their Canyon's public URL plus
`/auth/callback`. That hands them a client id and a client secret.

**What they set on the host:**

```
CANYON_OIDC_ISSUER=https://accounts.google.com
CANYON_OIDC_CLIENT_ID=<from the console>
CANYON_OIDC_CLIENT_SECRET=<from the console>
CANYON_PUBLIC_URL=https://canyon.theirbox.example
CANYON_ADMINS=ana@gmail.com
```

No domain list. No `CANYON_OIDC_ALLOWED_DOMAINS` at all, because `gmail.com` would be the entire
internet and they have four people.

**What happens next.** Ana opens Canyon, signs in with Google, and lands on step 1 because she is
the bootstrap admin. She names the organisation. Step 2 shows her own address already there, and
no domain box at all, because her Google token carried no `hd` claim; she adds the other three addresses.
Step 3 shows her as the admin and she adds one more. She finishes. The other three open the host,
sign in with Google, and they are in.

Two things this story surfaces, both worth stating before somebody discovers them.

**Personal Google accounts carry `email_verified`,** and it is `true`, so the rule in `AUTH.md` §3
holds without an exception for this case: an address on `allowed_emails` still has to be verified,
and Google verifies it. The address list is not a way around verification, it is a way around
needing a domain.

**GitHub is where teams like this actually live, and GitHub is not OIDC.** GitHub's sign-in is
OAuth 2.0 with no ID token and no discovery document: you get an access token and call
`GET /user` and `GET /user/emails` to find out who it is. Supporting it means a second protocol
inside Canyon, with its own token handling, its own verified-email logic and its own failure modes,
next to the one this design is built on. **Not in v1.** The named trigger is a customer who will
deploy Canyon and cannot use any OIDC provider; if that is three customers rather than one, it is a
capability and not an exception. Until then a team on GitHub does what the four above did: a Google
account each, which every one of them already has, and five minutes in a console they will never
open again.

## 5. Contract changes (seam PR)

Its own seam PR, approved by both humans, merged before any consumer. It is separate from
`AUTH.md`'s seam PR and lands after it.

**`GET` / `PUT /api/v1/preferences`** gain the organisation's settings: `org_name` (string),
`allowed_domains` and `allowed_emails` (arrays, **not both empty**), `admin_emails` (array),
`admin_claim` and `admin_value` (strings, both set or neither), and `onboarded_at` (RFC 3339 or
null). `GET` also answers `overridden: [...]`, the keys the environment is setting, so the screen
can render a field read-only and name the variable rather than letting somebody edit a value that
will not take. `GET` answers two read-only keys besides: `viewer` as `{email, role}`, so the page can
find the reader in the lists it is drawing; and `bootstrap_admins`, the addresses `CANYON_ADMINS`
grants, **answered apart from `admin_emails` and never merged into it** — admin is a union and
`admin_emails` is not an overridden key, so without this the screen would draw one list and a
removal from the bootstrap half would succeed and change nothing. `GET` also answers
`admin_claim`/`admin_value` **only to an admin** (`canyonv2-7mtz`), with `overridden` still naming
the pair to a member so a redacted field is distinguishable from an unset one. `PUT` refuses both
membership lists empty with
`400 {error:"no_members"}`, refuses a change that would leave no admin across `admin_emails`, `bootstrap_admins` and a set
`admin_claim` together with `400 {error:"no_admins"}` (an organisation may empty its own
`admin_emails` while bootstrap admins remain, because they still administer the host), and refuses an overridden key with
`409 {error:"set_by_operator", keys:[...]}`. Admin-only and audited like the rest of the row.

**The setup state:** while `onboarded_at` is null under `oidc`, every route under `/api/v1` except
`GET` and `PUT /preferences` answers `409 {error:"setup_required"}`. `/ready` is explicitly
unaffected and still answers `200`.

**The audit read's `action` prose** gains `complete setup`.

## 6. What changes in existing docs

| File | To |
|---|---|
| `design/AUTH.md` §3 | a line saying these values move here, and the variables become bootstrap and override |
| `design/AUTH.md` §4.6 | already a pointer to this document |
| `design/specs/screens.md` | a pointer where A0b sat, naming this document |
| `README.md` | the SSO block gains one sentence: after the first sign-in, the rest is a screen |
