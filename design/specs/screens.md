# Canyon — Product Experience Spec

Screen-by-screen design for the first 20 minutes and day 2. Judged against the personas in `design/USER-STORIES.md` and the map in `design/EXPERIENCE.md`.

**Thesis this spec is built to serve:** recognition → one command → *watching it catch something real*. Every screen below either advances that or is cut. Where I cut something a normal dashboard would have, I say so and why.

**Three rules I applied throughout:**

1. **The product state is the tour.** No coach marks, no modal welcome, no "3 of 7 steps" checklist. The guided task is surfaced because an app is *genuinely blocked right now* and the fleet sorts by risk. Live state draws the eye better than any onboarding overlay.
2. **The gate's subject is the code, never the person.** "Stripe live secret key is bundled into the browser build." Not "you exposed a secret." Not "failed." Not "violation." This one grammatical rule is what makes the gate read as a senior engineer instead of a cop.
3. **Show what passed, not just what failed.** A blocked promote that lists four policy checks with three green and one red is a gate. A blocked promote that just says "blocked" is a wall.

---

## 0. The handoff (minute 2, terminal → browser)

Not my lens, but the seam matters. What the one command prints on success:

```
  Canyon is up.

  → http://localhost:7777

  Seeded: Meridian Insurance — 8 apps, 6 frameworks, 3 databases, and the
  problems that come with them. Nothing to sign up for.
```

Three constraints on the handoff, because they determine my first screen:

- **The demo has no login**, and neither does a host with `CANYON_AUTH=off`, which serves loopback
  alone (`go test`, a laptop); every other host has one. `CANYON_AUTH=demo`
  is what the shipped compose file sets, so the browser opens straight onto the fleet. Every other
  host opens on the sign-in screen (A0 below), because **authentication is mandatory and on by
  default** (`design/AUTH.md`).
- **Be honest about that.** A security product that quietly ships an unauthenticated admin panel would be funny in the worst way. See the demo strip in A1.
- **The seeded org is named in the terminal**, so the fleet header doesn't have to explain itself.

---

## Shared vocabulary

**An app is named by its display name** wherever a list or a sentence names apps: Needs attention,
recent activity, the audit table, the CLI's fleet. Two apps in one list that share a name say their
ids too, `claims-app (tidy-finch-euk), claims-app (funny-cloud-xij)`, and activity says `created
claims-app (snappy-otter-0fz)` when another app has that name. An app no list holds any more (deleted)
is named by its id. The app's own page and the fleet table show its id beside its name whenever the two
differ (#1911).

Subordinate to whatever the visual lens lands on — these are the semantics the copy and layouts below assume.

**Severity ramp** (5 steps, only 3 get color):

| | Meaning | Blocks production |
|---|---|---|
| **Critical** | Exploitable now, or a live credential is exposed | Yes |
| **High** | Exploitable with one more step, or auth is missing | Yes |
| **Medium** | Real problem, not a shipping problem | No |
| **Low** | Worth knowing | No |
| **Info** | Context the review surfaced | No |

Blocking = Critical, High, or anything the security lens flagged. **An unparseable severity is treated as blocking, never dropped.** A gate that fails open is not a gate.

**Review state is not finding count, and it is not a state of the app.** The fleet's **Latest**
column names the newest version and how far it got: its label, or else its id without `pkg_`, one
stage word from the nine (`contracts/routes.md`, *The package's stage*), and the age. Never a gate
word — an app runs environments and has a newest version, so "clean" describes a verdict and never a row:

- `v2.1 · ✓ Passed · 2d` — the newest version cleared the review; nothing was deployed.
- `v1.4 · ✗ Blocked · 3m` — the review stopped the newest version. Nothing was built and nothing was deployed.

An app nobody has ever reviewed is counted by the "apps never reviewed" box, not named in this column.

**Resolved ≠ approved.** A finding that got fixed is `resolved`. A finding someone shipped anyway is `approved`, forever, with a name and a reason attached. Collapsing these would falsify the audit trail, so the UI never uses one word for both.

**Three actor types**, everywhere an action is attributed. In an org where half the commits come from harnesses, this is the distinction that makes an audit log legible:

- **A person** — avatar + name.
- **An agent** — harness mark + `Dana's agent · Claude Code`.
- **Canyon** — the policy engine acting on its own. Monospace, no avatar.

**Density.** Table rows 44px, not 64. This is a tool someone keeps open on a second monitor, not a marketing page. Numbers are tabular, timestamps are relative with absolute on hover.

---

# ACT 1 — the first 20 minutes

---

## A0 — Sign in

**Purpose:** be the shortest screen in the product. It exists on every host except the demo.

**Emotional beat:** none. A sign-in screen that has a mood is a sign-in screen somebody spent too
long on.

The dashboard asks for the fleet, gets a `401`, and replaces itself with this. Nothing else on the
page, no marketing, no product tour, no "why Canyon".

**One card for the three pages a person signs in or approves on** (amended 2026-10-08, #1880,
the owner's approved redesign): A0, the device approval screen and the production approval screen are
one card on the page grey, the Canyon mark and name above it. The card's top row is the org: its
initials in a tile, its name (`/auth/config`), and under it the address in the browser, read from
the browser and never from the server, so a person with two Canyon hosts open knows which one is
asking. Text reads down one left edge. The main action is one full-width button;
where there are two choices both are the same size. What a click hands over sits above the buttons
in an amber box. Every state has a title and one line saying what to do now. On a phone the card is
the page: its edges drop away and the buttons are thumb height.

```
┌────────────────────────────────────────────────────────────────────────────┐
│                                                                            │
│                ▲ Canyon                                                    │
│               ┌──────────────────────────────────────────┐                 │
│               │ [MI] Meridian Insurance                  │                 │
│               │      🔒 canyon.meridian.example          │                 │
│               │ ──────────────────────────────────────── │                 │
│               │ Sign in to Canyon                        │                 │
│               │ Use your Meridian Insurance company      │                 │
│               │ account. You'll come straight back here. │                 │
│               │                                          │                 │
│               │ ┌──────────────────────────────────────┐ │                 │
│               │ │     Continue to company sign-in      │ │                 │
│               │ └──────────────────────────────────────┘ │                 │
│               │ 🔒 Canyon never sees your password.      │                 │
│               └──────────────────────────────────────────┘                 │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

- **One button, and it does not name the provider.** Not "Sign in with Microsoft". The bundle
  carries no configuration (`contracts/routes.md`, the dashboard rows), the org may switch
  providers, and a person arriving here already knows which login their company uses. It reads
  `Continue to company sign-in`, so the hop to the company's page is no surprise, and once clicked
  `Opening company sign-in…` with a spinner, so a slow provider is not a dead click. The button
  navigates to `GET /auth/start` and the identity provider takes over; after a sign-out, to
  `GET /auth/start?prompt=select_account`, so the provider asks which account to use.
- **The org is the card's top row**, read from the host. The heading is `Sign in to Canyon`, and the line under it
  `Use your <org> company account. You'll come straight back here.` Under the button:
  `Canyon never sees your password.` It is true: Canyon has no password form.
- **Each reason to be here has its own box above the button**, a title and a line, and then no line
  under the heading:
  - **A failed sign-in comes back here** with `Sign-in didn't finish` / `Try again. If it keeps
    failing, ask your Canyon admin.` in red, and the button reads `Try again`. No error code, no
    provider message, no detail. The reason is in the server's log where an operator can read it,
    and echoing an identity provider's text to an unauthenticated page is how a phishing page gets
    its copy written for it.
  - **After a sign-out the person chose, and only then**, `You're signed out of Canyon` / `Your
    company account is still signed in. Sign out of it too if this computer is shared.`, because
    sign-out does not do what people assume. Canyon revokes its own session; the identity
    provider's session is the provider's. Signing in again asks the provider to let the person
    choose an account, but the provider may still list the company account without a password, so
    somebody leaving a shared machine should sign out of their company account too.
  - **A session that ended** says `Your session ended` / `Sign in again to keep going.` The server
    answers the same `401` for no session and for one that ended, so the page says it only when it
    knows: a read in this tab was answered, and a later one answers `401`. The first read of a tab
    answering `401` is a person with no session yet, and says nothing.
- **Signing in for the CLI, or for an app** (amended 2026-10-01, `canyonv2-5r11.4`,
  `canyonv2-c9px`; the approval page amended 2026-10-08, #1880): opened from `/#/device`, the card says `Canyon CLI · Step 1 of 2` over a
  two-step bar, the heading reads `Sign in to approve the Canyon CLI` and the line `Then you'll come
  back here to approve the code from your terminal.`; signing in lands back on `/#/device` with its
  code; opened from `/#/approve` it lands back there with its code; opened from `/#/no-access/<app>`,
  signing in lands back there. The browser keeps that page
  for the length of the sign-in and nothing else.
- **Expiry lands here too**, with no warning modal and no countdown beforehand. A `401` mid-session
  shows this screen; signing back in returns to the fleet (or to one of the two pages above), not
  to whatever was half-done. Losing an
  unsaved approval reason is the right trade against carrying a draft across an authentication
  boundary.

### The device approval screen

`/#/device`, the page `canyon login` sends a person to. It needs a signed-in browser session, so an
unsigned-in visitor gets A0 first and lands back here. It is A0's card, `Canyon CLI · Step 2 of 2`
over the two-step bar.

```
┌──────────────────────────────────────────────┐
│ [MI] Meridian Insurance                      │
│      🔒 canyon.meridian.example              │
│ ──────────────────────────────────────────── │
│ Canyon CLI · Step 2 of 2                     │
│ Sign in to the Canyon CLI                    │
│ Check this matches your terminal.            │
│ ┌──────────────────────────────────────────┐ │
│ │ Code                        DKTG-4B9V    │ │
│ │ Requested from              10.4.2.19    │ │
│ │ When              21:08 today · Change   │ │
│ └──────────────────────────────────────────┘ │
│ ⚠ Only approve a code you generated yourself.│
│   Approving gives that machine your access   │
│   for 7 days. Anything running there acts as │
│   you, including deploys.                    │
│ (DW) Approving as Dana Whitfield [Admin]     │
│      dana@meridian.example   Switch account  │
│ ┌──────────────────┐ ┌──────────────────┐    │
│ │     Approve      │ │       Deny       │    │
│ └──────────────────┘ └──────────────────┘    │
└──────────────────────────────────────────────┘
```

- **The sentence about what approving hands over is the whole security story, and it is the honest
  one:** the CLI session *is* this person, for everything the CLI can do. It does not promise a
  boundary the server cannot hold (`design/AUTH.md` §4.4). A person approving a code for an agent
  should know exactly what they just handed over, which is their access, not a subset of it. It
  sits with `Only approve a code you generated yourself.` in the amber box above the buttons
  (amended 2026-10-08, #1880: under the buttons it was read after the click, if at all).
- **The address and the time are the defence against being talked into it.** Device-code phishing
  works by getting somebody else to approve a code the attacker generated. A person who did not
  just run `canyon login` on `10.4.2.19` has the one fact that tells them so, above the button and
  not in a footnote. Once looked up, the field folds into a short table (`Code`, `Requested from`,
  `When`) under `Check this matches your terminal.`; `Change code` opens the field again.
- **The account that approves is named above the buttons** (amended 2026-10-07; restyled
  2026-10-08, #1880): a strip with the approver's initials, `Approving as <name>`, an `Admin` tag
  when the approver is an admin, the email under it and `Switch account` on the right, read with the
  grant (`approver`, `contracts/routes.md`). A provider that signs a person in silently can sign
  them in as an account they did not mean, and approving hands that account's access over.
  `Switch account` signs out of Canyon (`DELETE /api/v1/sessions`) and shows A0 as any chosen
  sign-out does, whose button asks the provider which account (`/auth/start?prompt=select_account`);
  that sign-in lands back on this page with its code.
- **`verification_uri_complete` prefills the code and never skips the click.** A link can fill the
  field; only a person can press Approve. One link must never be able to complete a sign-in.
- **A wrong, used or expired code** keeps the field, outlined red, with `That code isn't valid or was
  already used` / `If your terminal says you're signed in, you're done. Otherwise run canyon login
  again.` and nothing else. Codes last ten minutes and are consumed once, and the server answers all
  three alike (404, `not valid or has already been used`), so the screen never says expired: a person
  who entered an approved code again read `That code has expired` as a failed sign-in (amended
  2026-11-04, #2647).
- **Arriving with no code** is normal: the CLI's link pre-fills the field, and typing it by hand is
  the fallback for a person reading a screen across a desk; the field is labelled `Code` under
  `Enter the code shown in your terminal.`, and a Continue button (or Enter) looks the typed code
  up. What is shown is always the code that was looked up.
- **Deny is Approve's size and beside it** (`POST …/deny`): a person who did not run `canyon login`
  should be able to end the attempt, not only ignore it. After the click the card says
  `The CLI is signed in` / `That machine now has your access for 7 days. You can close this tab.` or
  `Request denied` / `Nothing was handed over. If you didn't start this, tell your Canyon admin.`,
  and nothing else.

### The production approval screen

`/#/approve?code=`, the page `canyon deploy --env prod` sends a person to when the org has a person
approve production (decided 2026-10-07, #1820; `design/AUTH.md` §4.5). Like the device page it needs a
signed-in browser session, has no rail, and an unsigned-in visitor gets A0 first and lands back here
with the code. It reads `GET /api/v1/promotion-approvals/:code` and nothing else. It is A0's card,
wider, headed `Production approval`.

```
┌──────────────────────────────────────────────────────────┐
│ [MI] Meridian Insurance                                  │
│      🔒 canyon.meridian.example                          │
│ ──────────────────────────────────────────────────────── │
│ Production approval                                      │
│ Promote claims-portal to production?                     │
│ (CD) Carol Diaz asked from the CLI, 14:22 today.         │
│ ┌──────────────────────────────────────────────────────┐ │
│ │ In production now          →         Goes live       │ │
│ │ v1.3                                 v1.4            │ │
│ └──────────────────────────────────────────────────────┘ │
│ Production runs the same image as preview, with          │
│ production's own secrets. Nothing is rebuilt.            │
│ GO-LIVE CHECKLIST                                        │
│ ┌──────────────────────────────────────────────────────┐ │
│ │ ✓ Production needs a passing review                  │ │
│ │   v1.4 passed review: …                              │ │
│ │ ✓ A person promotes   …                              │ │
│ │ · Approved stacks only   no approved stacks set      │ │
│ │ ✓ Every app has an owner   owned by Dana Whitfield   │ │
│ └──────────────────────────────────────────────────────┘ │
│ (DW) Approving as Dana Whitfield [Admin]                 │
│      dana@meridian.example             Switch account    │
│ ┌────────────────────────┐ ┌────────────────────────┐    │
│ │        Approve         │ │          Deny          │    │
│ └────────────────────────┘ └────────────────────────┘    │
│ ──────────────────────────────────────────────────────── │
│ Expires 14:22 tomorrow.                Code QX7M-2KPD    │
└──────────────────────────────────────────────────────────┘
```

- **Who asked, and from where, comes first**: `<name> asked from the CLI, <time>.` beside the
  asker's initials — the request's actor and channel. A person who did not expect a request has the
  one fact that tells them so.
- **What goes live is the promote receipt's**: the two versions side by side, `In production now`
  and `Goes live` (`Nothing yet` for a first release), and D5's no-rebuild sentence, then the
  go-live checklist, the same rows from the same renderer as the app page's, read for this reader,
  in a bordered list under `Go-live checklist` with a blocking row tinted red. The reader's own
  `prod.human-approval` row passes: they are a person in the dashboard.
- **The account that approves is named above the buttons**, the same strip as the device page's,
  with *Switch account* (amended 2026-10-07: an admin approved two CLI codes without noticing it was
  the admin approving).
- **Approve is offered only while the receipt blocks nothing** (amended 2026-10-08, #1880): otherwise
  a red box above the versions says `This can't go to production now` and names each blocking rule,
  with `Open the review →` to the app's page when the review is what blocks, and Approve stays in
  its place, disabled, with `Blocked by the go-live checklist.` under it (D6: a disabled button says
  why). Deny is offered either way.
- **What preview answered is said when it is an error** (amended 2026-10-08, the owner): when the read
  carries `answered` and it is an answer to say beside readiness (contracts/routes.md,
  `envs.<env>.answered`), `<version> answered <status> at / on preview. Check the app before approving
  it.` (or `gave no answer at /`) stands in amber above the checklist. Approve stays offered and is the
  person's override: it sends `promote_anyway`, and the trail's row says they approved despite it.
- **The foot says when it expires and the code**, `Expires <time>.` and `Code <code>`, so the
  approver can match it with what the asker pasted.
- **Approve is the Promote button**: it starts the same promotion, and the card then says
  `Promotion started` / `<app> <version> is going live. Follow it on its page →`, the link to the
  app's page. Deny says `Denied` / `Nothing was promoted.`
- **A code that is not pending** says why, as a title and the next step, and nothing else:
  `Already approved by <name>` or `Already denied by <name>` / `Nothing more to do here.`,
  `This approval expired` / `Ask for a new one from the CLI.`, `Preview runs another version now` /
  `Ask for a new approval from the CLI.` (Deny stays offered on that one), and one a hold outdated (the
  read's `held`) `Production is held` over the held sentence (EXPERIENCE.md D5), naming who held it,
  when and why, with nothing offered (amended 2026-10-15, #2243). A code no approval has:
  `That code isn't valid, or a newer request replaced it.`
- **Below contributor** the screen is the no-access screen's ask (`team_required`), as on the app page.
- **A patch approval** (the read's `kind: "patch"`, Canyon asking; design/PATCHING.md §8; #2105): it is titled
  `Promote Canyon's patch of <app> to production?` and who asked reads `Canyon asked, <time>.`; the version is the
  same on both sides, so under each a faint line tells the images apart, like with like (#2593): `image <12 hex>`
  under *In production now* (the read's `replaces_image`) and under *Goes live* (its `image`, the patched image), and
  no image of Canyon's system libraries, which only Settings › Patching names (#2605); under the versions a bordered `What the patch fixes` box lists each fix as `<name>
  <from> → <to> (<vulnerability>)`, one line a package and version with its vulnerabilities joined by `, `
  (#2579); then under `Still there after this patch` what the checklist's `platform.known-vuln` row still counts —
  the patched image's — `<n> known-vulnerable packages, listed in the checklist below.` and whether production
  runs every one already, by production's image read (`GET …/envs/prod/image`): `Production already runs them,
  so approving adds none.`, `Production doesn't run all of them: approving adds some.`, or, when that read fails
  or did not check, `Canyon can't tell whether production already runs them.`; with one package left,
  `1 known-vulnerable package, listed in the checklist below.` and the same in the singular, `Production already
  runs it, so approving adds nothing.`, `Production doesn't run it yet: approving adds it.` or `Canyon can't tell
  whether production already runs it.` (#2591); with none, `Nothing else known-vulnerable with a published fix.`,
  and with the row unchecked `Canyon couldn't check what remains: the checklist below says why.` (all D5's); then the proof, `The app's own files are byte for byte the ones in production.`,
  and under `Preview's watch` the patch run's passed line, `The watch passed: the patched app neither exited nor
  restarted`; then D5's no-rebuild sentence and the checklist as for any approval. Expired or outdated, it says
  `Canyon asks again if the patch still applies.` in place of asking from the CLI, an outdated one titled
  `This approval is out of date`, since either environment may have moved.
- **The checklist's `platform.known-vuln` row**, here and on the app page (#2579, all D5's): under the server's
  `why`, while it names packages or notes, `None of these are in the app's own code, which its review checks.`;
  folded under `See the <n> packages`, what it counts by what fixes it — `No fix from Canyon yet` for a package in an `os`
  layer, `Waits for Canyon's next build tools update` for a `runtime` or
  `lifecycle` one, from the receipt's `layers`, never its text; on the app page, while preview's patch state is
  `planned` (contracts/routes.md, `envs.<env>.patch`), the system libraries Canyon uses now fix the `os` ones, so they read
  `Canyon is trying a fix in preview` instead (#2594, #2605) — any other state, and the approval page, which reads
  none, keep `No fix from Canyon yet`: once the patch is in preview the receipt reads the patched image, and what it still counts
  waits — each package once by name with its versions joined
  by `, ` and `· in <places>`: `the system libraries` (`run-image`), `the app launcher` (`lifecycle`), `the Go
  version Canyon built the app with` (`app`), else the buildpack's id; Go's standard library (`go_stdlib`) reads `Go's
  standard library`; each advisory once. A receipt without `layers` (a responder's) groups by name alone, under no
  heading and with no place. Grouped by package, not by vulnerability: the decision is what to update, and one
  package's vulnerabilities share its fix, where a list by vulnerability repeated each package once per CVE.
- **The checklist's `deps.known-vuln` row**, here and on the app page (#2610): while it carries `built_app`, folded under
  `See the <n> packages` (`See the package` for one), each package only the built app holds by `<name> <version>`, then each
  advisory against it, `<id> fixed in <fixed>`, or `<id> · no fix published` where none is, in the receipt's order; an
  approved one carries the `approved` chip. To an admin or a responder alone, on the app page, each not yet approved has
  `Approve with a reason`, which opens the finding's approval form (*The approval*: a reason, an expiry 30 days out) and
  files every fingerprint of the package together on the receipt's `promote_run`; nobody else sees it, as the route
  refuses them (design/PERMISSIONS.md). The approval page shows the list and no approval.


---

## A0b — Setting up Canyon

**Moved.** The setup state and the four onboarding screens are their own feature, with their own
milestone (M2c) and their own document: **`design/ONBOARDING.md`**, sections 2 and 3. They are not
on the v1 demo path, which runs `CANYON_AUTH=demo` and has no sign-in at all, and they
are deliberately not what authentication waits for.

What stays true here: on a host that has been onboarded, A0 is the first screen. The organisation's
settings live under **Settings** (amended 2026-10-02, redesign #1401, #1348; they were under
Guardrails), as four pages, each its own address and one Save: **Organisation**
(`#/settings/organisation`, the name), **People & sign-in** (`#/settings/people`: who may sign in,
who administers Canyon, the security responders — to an admin alone, since the read answers their
keys to nobody else: `Security responders can stop any app's production and approve its blocking
findings. They never deploy, promote or change secrets.`, the responders by address, and the claim and
value that name them, set both or neither (amended 2026-10-14, #2068); the admins' note under who
administers Canyon reads `Admins change these settings and the policies. Everyone else deploys and
promotes; only an admin or a security responder approves a blocking finding.`, and an address list's
*Add* puts the cursor in the row it adds, a field wide enough for a whole address (amended 2026-10-15,
#2258) — the groups claim, and
*Production is approved in the dashboard*, a switch
with the one line `On: canyon deploy --env prod asks, and a person on the app's team approves it here
in Canyon. Off: anyone on the app's team can promote from the CLI, including an agent they ask to.` (amended
2026-10-07, #1820; `design/AUTH.md` §4.5); on a host with sign-in off, one line saying so and no
form), **App hosting** (`#/settings/hosting`, *How people reach apps*: the front door), and **AI
review** (`#/settings/ai`: whether a model is set up). A key the environment pins is shown with its
variable and never sent, and a Save sends only what changed. An edit not saved is dropped when the
page changes. Guardrails keeps only the rules.

**Patching**, on App hosting, is an admin's alone (design/PERMISSIONS.md, *Platform patches*; #2105): a member
sees none of it and the page never reads the identities for them. Marked Beta: the `patching` switch as two
choices, `Canyon doesn't patch apps` · `Canyon patches previews and asks before production`, saved as it is
changed; the system libraries image Canyon uses and `<n> apps wait for newer system libraries: <apps>`, each app named and linked
to its page (the read's `projects`; an admin sees every app), or `No app waits for newer system libraries`, or `Canyon
can't tell which system libraries it uses now, or whether apps wait for newer ones.` when the read can't tell; then each
platform identity, its system libraries image, wave and apps — paused, `Canyon paused patching with <image>: <n> apps
reverted after the patch.` with **Resume** (also offered on a stalled one), and **Approve for every app**, which
asks for a reason, then reads `Approved for every app by <name>, <date>: <reason>` (all D5's). Every such image
reads `system libraries image <12 hex>`, the only place Canyon names it (#2605), its whole reference folded under it (#2582). Its apps by
state include `turned down for production`, those whose team denied the patch, production still unpatched (#2601). An
identity's work left is its apps `planned`, `patched_preview` and `asked`: its wave is said, and **Approve for every
app** offered, only while it has some, since the fleet approval decides only patch approvals, filed once a
preview's watch passed; one with no app reads `No app to patch with these system libraries`. The system libraries image Canyon uses now and every
identity with work left or a breaker to resume show; the rest fold under `<n> earlier fixes, nothing left to do`. What Canyon's last check said (#2588; contracts/routes.md, the identities row's `plan`) shows in the card of the identity it checked against: `Canyon's last check, <ago>`, then each cause once, its apps after ` · `, each named and linked to its page, the causes in strings.ts's order and one the page does not know read as `Not patched, for a reason Canyon can't name`; a check of no app says why under the image Canyon uses now. With sign-in off the identities are not served and only the switch shows.

A fifth page, **GitHub** (`#/settings/github`, amended 2026-10-09, #1914), connects GitHub
organizations, one App each (contracts/routes.md, `GET /api/v1/github`). Only admins see it; a member
who follows a link to it reads `Only an admin can change how Canyon connects to GitHub.` It opens
with **Connect a GitHub organization**:

- a `github.com/` field for the organization's login;
- two choices: `This organization holds only Canyon apps` (under it: `Recommended for code Canyon
  keeps. New repositories here become apps automatically.`) and `It also holds other code` (under
  it: `Nothing is imported until an admin picks repositories.`), with neither picked until the admin
  picks one;
- a checkbox, `Keep code for apps that start in Canyon here`, with `Canyon may create repositories
  here and commit to them. Without this, its App only reads.` under it. When another organization
  already keeps code, the checkbox is off and can't be ticked, and the line names that organization.
- the button `Register Canyon on <org>`, which opens `GET /api/v1/github/app/new` with the three
  answers, and under it: `You need to be an owner of the organization on GitHub, or an App manager
  there. GitHub opens, you confirm, then pick which repositories the App sees.`

On a host GitHub can't reach (`reachable` false), the button and its line are replaced by `GitHub
can't send events to localhost. Connect GitHub from a Canyon that has a public address.` Below the
form, **Connected organizations** has one row per org: Organization (linked to its App on GitHub),
New repositories (`Become apps` or `Picked by an admin`), `Keeps code` beside the one that does, Last
event (its age, or `None yet`), and State (`Receiving events`, or `Not installed` with a link to
install the App). With no org connected, the table isn't shown. Each row's New repositories cell is a switch between `Become apps` and `Picked by an
admin` (#1923, `PUT /api/v1/github/orgs/:account`), saved as it is flipped, with the line `Every new
repository in <org> becomes a Canyon app. Turn this on only if <org> holds Canyon apps alone.` under it.

Under it, **Repositories** (#1917, `GET /api/v1/github/repositories`) has one row per repository the
Apps can see, and one more for each further folder linked in it. The columns are Repository (its
full name, linked to GitHub), Folder (`/` for the root) and Canyon app (the app's name, linked to its
page, with `Unlink`). A repository with no app has `Make it an app`, which asks for a folder (empty
for the whole repository) and a name (the folder's last segment, or the repository's name), and
`Link to an app`, which asks which app. A linked repository has `Add another folder`. Under the
table: `Making an app links it and reviews the default branch's newest commit. Nothing is deployed.
The app arrives with no owner, and an admin assigns one.` An organization GitHub didn't answer for is
one line above the table: `GitHub didn't answer for <org>, so its repositories aren't listed. Try
again in a minute.`

A sixth page, **Outbound** (`#/settings/outbound`, marked Beta; design/EGRESS.md §8.3; amended 2026-10-22,
#2422), is every member's to read and an admin's to change (design/PERMISSIONS.md, *Outbound*, Host
settings). A member reads `Only an admin can change how apps call out.` above the host's choices, read-only.
An admin gets **How apps call out** (`Off` · `Through Canyon`, with Save) and, below it, **What apps may
call**, the org's mode: `Observe` · `Allowlist`, off while the host's via is `off`. Choosing `Allowlist`
shows, before anything is sent, that it applies to running apps within seconds, and every destination it
would refuse — one an app env that follows the org's mode and runs through Canyon called in the last 30 days,
its last call allowed, that neither the env's allowlist nor the platform destinations allow — by app and
environment, as the app's Outbound card lists them (destination, last seen, calls), each with `Allow`, so an
admin allows what apps need before switching. Its Save turns into a question in the page, `Switch to
allowlist` beside `Cancel`; going back to `Observe` widens and saves with no question. A read the list needs
that fails says so and the question is still asked: fail closed, never "nothing would be refused". Saving
`Through Canyon` while the org's mode is `allowlist` starts refusing too, at each app's next deploy: it shows
the same list, from what Canyon recorded before, counting every app env that follows the org's mode whatever
its via, says Canyon can't know all of it, and asks first. The
app's Outbound card says each environment's mode and whose it is; an owner sets the environment stricter
(`Allowlist`) or back to the org's, an admin either way, with the same list and question for that
environment alone, and a contributor reads `Only an owner can change this environment's mode.` Beside
each destination the environment's mode refuses, a contributor and up has `Allow`. An environment's egress
requests lead it, under `Refused`, pending first with `Allow` and `Dismiss`, then dismissed ones with `Allow`;
under Production while it runs an allowlist, what preview connected to that it doesn't hold, each with `Allow`
(design/EGRESS.md §8.3; #2125). For an admin the page ends on each gateway replica's policy age.

---

## A1 — The Fleet

**Purpose:** Show the visitor their own company in under three seconds.

**Amended 2026-10-02 (redesign #1401, #1345).** What ships, top to bottom, where it differs from
the drafts below:

- On a host with sign-in off, the D5 sentence is a banner above everything (amber on a loopback
  host, red when reachable from the network) with `Turn on sign-in →`.
- The org's name as the page title; beside it one search box (`Search apps`, sent as `q` once the
  typing settles, no Apply) and a `Filter` menu: Status, Deployed (where it is deployed, not whether it runs now), Access (sign-in hosts only),
  Stack, and `Only apps that need you`.
- The demo strip: `Start here: claims-portal v1.4 is blocked.` and `Open claims-portal →`, with a
  Dismiss icon button.
- The tiles as one row of filter chips in D4's words, a zero grey, no sub-lines.
- The table: `App · Version · Review · Preview · Production · Owner · Updated`, plus `Access` on a
  host with sign-in. App carries the summary under the name; Review is the newest version's review
  (`Never reviewed` when no run has ever looked at the app; once that version builds or runs, the
  app's newest run says it — a deploy is never a pass, and a block is never hidden behind where a
  version runs); Preview and Production the version each runs, linking to it, or `—`;
  Owner the person, or `No owner` with `Assign` (admins, sign-in hosts) and `Claim` where the row says
  `claimable` (amended 2026-10-12: never on an app the server would refuse the claim on). A row click
  opens the details panel; so does the chevron at the row's end, for the keyboard.
- `Recent activity` under the table, `Open audit` beside its title; empty, it reads `Nothing
  recorded yet. Deploys, reviews and promotions appear here.` A member's is what the audit read
  answers them: an org's policy or settings change is an admin's to see
  (**amended 2026-10-07**, contracts/routes.md, the audit read).
- Under 700px each app is a two-line card: name and review, then what runs and who owns it.

**Emotional beat:** *"oh shit, that's us."* Then, half a beat later: *"and something is on fire right now."*

### Layout, above the fold

```
┌────────────────────────────────────────────────────────────────────────────┐
│ Demo org — Meridian Insurance. 8 seeded apps, real problems.               │  ← demo strip, 32px
│ Start here → claims-portal is blocked from shipping.              dismiss ✕ │
├────────────────────────────────────────────────────────────────────────────┤
│  Canyon    Fleet  Reviews  Policies  Audit                    ⌘K   Demo ▾   │  ← 52px
├────────────────────────────────────────────────────────────────────────────┤
│                                                                            │
│  Meridian Insurance                                                        │
│  8 apps · 6 frameworks · 3 databases · 4 people                            │
│                                                                            │
│  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌──────────────┐      │
│  │ 2            │ │ 5            │ │ 2            │ │ 2            │      │
│  │ secrets in   │ │ apps never   │ │ apps with    │ │ apps blocked │      │
│  │ client       │ │ reviewed     │ │ no owner     │ │              │      │
│  │ bundles      │ │              │ │              │ │              │      │
│  │ ● critical   │ │ ▲ unknown    │ │ ▲ unowned    │ │ ✗ blocked    │      │
│  └──────────────┘ └──────────────┘ └──────────────┘ └──────────────┘      │
│                                                                            │
│  Apps                          Sorted by risk ▾   [ Review all 5 unknown ] │
│  ─────────────────────────────────────────────────────────────────────────│
│  ● claims-portal      Next.js 15 · Postgres   Dana W.   Blocked — needs you│
│    commission-calc    React + Vite            Tomas N.  1 critical · 6d    │
│    quote-engine       FastAPI · Postgres      No owner  Never reviewed     │
│    adjuster-notes     Next.js 14              Priya R.  Never reviewed     │
│    broker-dashboard   React + Express         Priya R.  Never reviewed     │
│    renewal-reminder   Node · cron             No owner  Never reviewed     │
│    fnol-intake        Nuxt 3 · Postgres       Marcus B. Passed · 2d        │
│    policy-lookup      SvelteKit               Marcus B. Never reviewed     │
└────────────────────────────────────────────────────────────────────────────┘
```

### Canyon's patches in the cells (#2105)

No new column (design/PATCHING.md §8). On a row read whole — contributor and above, never a reduced row — the
Preview and Production cells gain a second line: `patched by Canyon · <d Mon>` while the environment runs
Canyon's patch (`envs.<env>.patch.state` `patched_preview` or `patched`, its `since`), and amber `fix ready for
production` while production's is `asked`, a link to the approval page with its code. The app page's Preview and
Production cards (A2) say the same, from the same component. **Amended 2026-10-31** (#2602, #2601): the cards, never the fleet's cells, also say what `canyon status`'s platform row
says while the environment's patch is `reverted`, `held` or `denied`, in its words without the mark (D5): a revert red, its
run's own last `build.log` line, else `Canyon reverted its patch of <env>`; a hold amber, `held: production is held`,
`held: <version> is blocked by its review`, `held: Canyon paused this fix until an admin resumes it`,
`held: promote <version> to take the fix to production` (`<version>` preview's), or a patch run's end, `held: <its last
line>`; and production's denial amber, `Your team turned down Canyon's fix for production. Production still has libssl3 3.0.13-0ubuntu3.4.
Canyon will ask again when a newer fix is out.` (#2605), each package the denied approval fixes once, as production runs it. `needs_you` counts an app whose
patch approval is open, so the Needs you filter finds it (contracts/routes.md, the fleet read).

### The four tiles are the entire pitch

They are not KPIs. They are **accusations**, and each one is a filter. Clicking "5 apps never reviewed" filters the table to those five. This is the inversion that makes the screen work: a dashboard whose hero numbers are all bad news, and all clickable.

Tile copy, exact:

| Number | Label | Sub |
|---|---|---|
| **2** | secrets in client bundles | — (the row carries no key names, so the screen shows none) |
| **5** | apps never reviewed | `Canyon has no opinion about these` |
| **2** | apps with no owner | `Nobody gets paged` |
| **2** | apps blocked | `Can't go live until fixed` |

That third sub-line — "Nobody gets paged" — is doing more work than the number. It translates a governance abstraction into an on-call reality in three words.

### Default sort is risk

Not alphabetical. Not recently-updated. **Risk.** So `claims-portal` is row one without a single arrow pointing at it. This is the primary mechanism by which the guided task surfaces itself.

Sort options: `Risk` (default) · `Last deployed` · `Name` · `Owner`.

### How the guided task surfaces — three layers, zero tour

1. **Risk sort** puts the blocked app in row one.
2. **Row one is alive.** Amber pulsing dot, and the status cell reads `Blocked — needs you` in amber rather than a static badge. Motion in one row of a still table is an irresistible target. No coach mark can compete with it.
3. **The demo strip** — one line, one link, dismissible, honest about being seeded data. `Start here → claims-portal is blocked from shipping.`

Deliberately absent: welcome modal, coach marks, progress dots, confetti, "getting started" checklist widget. A checklist implies homework and admits the product isn't self-evident.

### The honest banner

**Shown only when this host has authentication off**, which the fleet read already reports:
`banner` is `loopback` or `exposed` on those hosts and `none` on every other
(`contracts/routes.md`, the fleet row). Under the `Demo ▾` menu, and inline on first load as a
single muted line beneath the org name:

> **No auth in demo mode.** Anyone who can reach this port is an admin. Real deployments use your SSO — see Policies.

A security product that hid this would deserve the finding. Naming it *builds* trust rather than spending it.

On an authenticated host the line is not shown: the signed-in person, their role and Sign out sit
at the foot of the rail on every page (`Dana Whitfield` · `Member · Sign out`). The sentence a
visitor reads in that slot is always true of the host they are looking at, which is the point of
deriving it rather than asserting it.

### "Review all 5 unknown"

One button, top-right of the table. This is the second wow and it's self-serve — the visitor triggers it, so the results can't feel planted.

Running state: the five rows animate to `Reviewing…` with a thin indeterminate bar, sequentially, ~8s each. Result lands as a toast plus a table update:

> **Review finished. 5 apps, 41 seconds.**
> `quote-engine` — 1 critical, 2 high · `adjuster-notes` — 1 high · `broker-dashboard` — clean · `renewal-reminder` — 2 medium · `policy-lookup` — clean

The two horrors from the seed — the unauthenticated `/admin` route on `adjuster-notes` and the hand-rolled SQL string interpolation in `quote-engine` — are **not** in the initial finding list. They're hidden behind "Never reviewed" until the visitor runs this themselves. The scariest number on the fleet is concealing the scariest bugs, and the visitor is the one who pulls the sheet off.

### Not on this screen

- **Uptime, latency, request volume, cost.** Canyon is not an APM. The moment a green "99.98%" appears next to a red "2 secrets exposed," the red one gets 30% less scary.
- **A create/deploy button.** Apps arrive from harnesses via the CLI. A "New app" button in the dashboard would misstate the entire product model.
- **A sort control.** The server sorts by risk, and the fleet read takes no sort. Changing that is a contract change.
- **Anything about Canyon itself** — version, docs links, GitHub star button, changelog. The README already sold them. This screen sells Meridian's problem.

---

## A2 — App detail: `claims-portal`

**Purpose:** Make the block legible, specific, and obviously correct.

**Emotional beat:** *"it stopped it."* Immediately followed by *"…and it's right."* The second half is what converts.

### Layout

Two columns. Left 62% is the gate. Right 38% is state and the promote decision. Both above the fold at 1440×900 — the finding and the blocked button must be visible **simultaneously**. If the visitor has to scroll to connect "there's a leaked key" with "that's why the button is off," the demo has failed.

```
┌─ claims-portal ──────────────────────────────────── Dana Whitfield ─ ⋯ ──┐
│  Next.js 15 · Postgres · built with Claude Code                          │
│  ┌────────────────────────────────────┐  ┌────────────────────────────┐  │
│  │ Review gate                        │  │ Preview                    │  │
│  │ 3 findings · 2 blocking            │  │ ● live 14m                 │  │
│  │ pkg 4a91c2 · 11.4s · 3m ago        │  │ claims-portal--preview     │  │
│  │                                    │  │   :7777/preview/…      ↗   │  │
│  │ ┌────────────────────────────────┐ │  ├────────────────────────────┤  │
│  │ │ CRITICAL  secret-in-client-... │ │  │ Production                 │  │
│  │ │ Stripe live secret key is      │ │  │ ○ not deployed             │  │
│  │ │ bundled into the browser build │ │  │                            │  │
│  │ │ apps/web/src/lib/payments.ts:14│ │  │  ┌──────────────────────┐  │  │
│  │ └────────────────────────────────┘ │  │  │ 🔒 Promote to prod   │  │  │
│  │ ┌────────────────────────────────┐ │  │  └──────────────────────┘  │  │
│  │ │ HIGH auth.unauthenticated-route│ │  │                            │  │
│  │ │ /api/admin/policies is reach-  │ │  │  Blocked by 2 findings.    │  │
│  │ │ able without a session         │ │  │  Meridian requires a clean │  │
│  │ │ .../api/admin/policies/route:8 │ │  │  gate to reach production. │  │
│  │ └────────────────────────────────┘ │  │                            │  │
│  │ ┌────────────────────────────────┐ │  │  ✕ No critical or high     │  │
│  │ │ MEDIUM  unbounded-refetch      │ │  │    findings open — 2 open  │  │
│  │ │ ClaimsTable refetches all      │ │  │  ✓ Preview healthy — 14m   │  │
│  │ │ claims on every keystroke      │ │  │  ✓ App has an owner —      │  │
│  │ │ Advisory — doesn't block       │ │  │    Dana Whitfield          │  │
│  │ └────────────────────────────────┘ │  │  ✓ Stack approved —        │  │
│  └────────────────────────────────────┘  │    Next.js 15              │  │
│                                          │                            │  │
│  Activity ─────────────────────────────  │  Override with a reason →  │  │
│  09:12  Gate  3 findings (1 critical…)   └────────────────────────────┘  │
│  09:11  Dana's agent  Pushed pkg 4a91c2                                  │
└──────────────────────────────────────────────────────────────────────────┘
```

### Renaming it (#1909)

The header's ⋯ offers **Rename…** to the app's owners and admins, above **Delete app…**, and to nobody
else. It opens a panel under the ⋯: `Name` and a field holding the current name, `Save` and `Cancel`,
and under them `Only the name changes. Its address, canyon.toml and the CLI keep <id>.` Save sends
`PUT /api/v1/projects/:id/name`; the page then reads the app again, so the title, the breadcrumb and
the id beside the name (*Shared vocabulary*) follow. A refusal (`invalid_name`, `owner_required`)
stays in the panel, in the server's words, with the field as it was typed. Enter saves; Escape closes
it unsent.

### Where its code lives (#1917)

One line under the app's name, beside Owner, for its team and admins (the project read's
`repository`):

- linked: `Source github.com/<full_name>`, and ` · <folder>` for a folder, linked to the repository
  on GitHub; an admin sees `Unlink` after it;
- `deleted`: `<full_name> was deleted on GitHub. Every version is still in Canyon.`
- `archived`: `<full_name> is archived on GitHub, so it's read-only. Unarchive it there, or unlink it.`
- `removed`: `Canyon's App can't reach <full_name> any more. Add it back under the App's repository
  access on GitHub.`
- after any of these three an admin sees `Unlink` too, to link the app again, or to another repository;
- no link: `Source Kept in Canyon only`, and an admin sees `Link a repository`, which opens Settings ›
  GitHub.
- kept by Canyon (#1926): `Source Kept by Canyon in github.com/<full_name>`, then ` · commit <7 hex>`
  of `kept_commit` once there is one, nothing before; while the repository is `pending`, `Kept by Canyon in GitHub · the repository
  isn't made yet`; a `kept_error` is a second line under it, in the host's words. Its owners and admins
  see `Developers push from now on`, with the line `Canyon stops committing this app's versions
  there. The repository, its history and its link stay, and each push becomes a version.` (#1938,
  `PUT …/repository/keeps`), which asks first, in place, `Hand <full_name> to developers? Canyon
  stops committing this app's versions there. This can't be undone.` with `Hand it over` and
  `Cancel`, nothing sent until the yes; after it, the source line is a linked repository's.

Beside the source line of an app linked to a repository GitHub still serves (`linked`), its owners and admins see a switch, `Auto-deploy pushes to
preview`, off until one of them turns it on (#1923, `PUT …/auto-deploy`), with the line `A push to
the default branch deploys to preview once its review passes. Production is always a person's call.`
Contributors see `Pushes deploy to preview` when it is on, and nothing when it is off.

### A pull request's version on preview (#1947)

In History's runs, a pull request's run row — its package from a pull request, named `PR #<number>
(<branch>)` — carries a button, `Deploy this pull request to preview`, for the app's contributors and
admins, only while the newest run of that package ended clean or with every blocking finding approved (the run row's `approved`); nothing otherwise. It
deploys that version to the app's one preview, replacing what runs there (`POST …/deploys` with that
package), and the deploy is followed as any deploy is. While preview runs it, the preview line reads
`Preview runs PR #<number> (<branch>)`, until the next deploy replaces it, and the promote button is
disabled with the line `Preview runs a pull request's version. Merge it, then deploy main.` A fork's
pull request is never reviewed, so it never has the button.

### The blocked promote button — exactly how it explains itself

This is the single highest-stakes component in the product. Design:

- The button is **present, disabled, and labeled with the thing you want**: `Promote to production`, lock icon, muted fill. Not hidden. Not relabeled "Blocked." You should see the door you can't walk through.
- **The explanation is always visible, never a tooltip.** A tooltip on the most important sentence in the product is a design failure. It sits directly beneath the button, always rendered.
- **It names the policy, not the software.** `Meridian requires a passing review to reach production.` — not "Canyon blocked this." The org made this rule; Canyon is enforcing it. That is a completely different emotional register and it's the one that makes the tool adoptable.
- **It lists the checks that passed.** Three green, one red. This is the difference between a gate and a wall, and it takes four rows.
- Each policy line is a link to that policy on the Policies screen. The seam into governance is right here, at the moment of maximum motivation. For a member it is the policy's name as plain text: Guardrails is an admin's screen (A4b, **amended 2026-10-07**).

Exact copy of the receipt block:

> **Blocked by 2 findings.**
> Meridian requires a passing review to reach production.
>
> ✕ **No critical or high findings open** — 2 open
> ✓ **Preview deploy healthy** — live 14m
> ✓ **App has an owner** — Dana Whitfield
> ✓ **Stack is approved** — Next.js 15

### The approval

On each blocking finding's card, its third button, `[ Approve with a reason ]`: it approves that
finding and no other, and opens an inline field under the card, not a modal (amended 2026-10-01,
`canyonv2-5r11.10`; the checklist's `Approve these 2 problems with a reason →` link is gone, so
there is one way to approve). A blocking finding is one that blocks before preview, or before
production — the run read's `blocks_production`, the server's answer, never worked out from the rules
on the page (#2206) — and that one reads `advisory before preview · blocks before production` and
carries the button too. The button shows only where the run read is `approvable` for the reader: an
admin, or a responder, who did not start the run (amended 2026-10-12, #2204; design/PERMISSIONS.md) —
the run shown, the newest or one pinned by `?run=` alike, an older run's or a rollback review's, and
the approval is that run's (amended 2026-10-15, #2242).
Anyone else reads it once, above the findings, in place of every card's button: `Only an admin or a
security responder can approve a blocking finding, and never their own.` (EXPERIENCE.md D5). The
headline of a version blocked by findings says who approves too: `Fix it in the code and deploy again,
or an admin or a security responder approves it with a reason.` (`them` and `each` for more than one;
amended 2026-10-12, #2204: it said a contributor). A responder on an app not shared with them reads
the page a read-only member reads, its band `Your access: security responder. You can approve its
blocking findings and hold its production.`, and below it the newest review's findings, approved
here as above, and the receipt — never its versions, progress or logs (amended 2026-10-13, the owner).
Their read has nothing of preview, so the page claims nothing of it (amended 2026-10-15, #2258): the
headline speaks of production alone (`Not running in production` where a read-only member's says
`Not running`), and the Preview card reads `Not shown to you` over `A security responder reads an
app's production, never its preview.` The expiry starts filled, 30 days from today, for
every finding:

> **Reason**
> `[                              ]`
>
> **Approval expires**
> `[ 2026-12-14 ]`
> After that date the problems come back in the checklist.
> Attributed to Dana Whitfield · in the audit, visible to every Meridian admin
>
> `[ Approve ]`  `Cancel`

It approves and promotes nothing: one approval per blocking problem, and the next deploy of that
version goes ahead carrying them. Once it lands, the card reads `Approved by Dana Whitfield until
14 Dec · <reason>` in place of the button, and when every blocking finding is approved the verdict
reads D1's amber `! Approved — …` (EXPERIENCE.md D5), never `✗ Blocked`.

**One dependency's findings together** (amended 2026-10-08, #1848: lodash 4.17.4's five CVEs took five
forms and five reasons; amended again for #1875: the card printed trivy's message as its why). The
blocking findings that are one dependency's from one tool — the run read's `dependency` and `tool`
(contracts/finding.md, *A dependency's name and version*) — are **one card**, where the first of them
stood, among what blocks while any of them is not approved and among the rest once all are:

> `blocks` `Critical` **lodash 4.17.4: 5 known vulnerabilities**
> Found by trivy
> `package-lock.json:17 · lodash 4.17.4`
>
> Lodash: Prototype pollution in utilities function
> [CVE-2018-16487](https://avd.aquasec.com/nvd/cve-2018-16487) · High · fixed in 4.17.11 `Approve with a reason`
> nodejs-lodash: prototype pollution in defaultsDeep function leading to modifying properties
> [CVE-2019-10744](https://avd.aquasec.com/nvd/cve-2019-10744) · Critical · fixed in 4.17.12 `Approve with a reason`
> …
>
> Fix  Upgrade lodash to 4.18.0 (fixes all 5) and commit package-lock.json.
>
> `[ Copy for your agent ]` `[ Approve all 5 for lodash 4.17.4 ]`

- **Its head** is the highest severity among them and `<name> <version>: <n> known vulnerabilities`
  (`1 known vulnerability` for one), `<n>` counting its findings, then `Found by <tool>`.
- **No snippet.** A dependency's region is a lockfile entry, hidden whole (finding.md), so the card
  shows one line in its place: `<file>:<line> · <name> <version>`, the first finding's line in each
  file its findings name, one line a file.
- **A row per finding**, in the run's order: its title, then `<vulnerability> · <Severity> · fixed in
  <fixed>` — `<Severity>` the finding's own, capitalised; `· no fixed version yet` when it carries no
  `fixed`; its rule id when it carries no `vulnerability` — the id a link to `link` when it carries
  one and plain text when not, and nothing of the problem's text: trivy's message is its fields, read
  once, never a paragraph. A row keeps `[ Approve with a reason ]`, which opens the single approval
  under the card, since each finding stays its own to approve; an approved row reads `approved by
  <name> until <date> · <reason>` in its place.
- **The fix names the version**: `Upgrade <name> to <fixed_in> (fixes all <n>) and commit <files>.`,
  `<files>` each file the card names, joined by `, `; `(fixes all <n>)` for two or more when every
  finding carries `fixed`, `(fixes <k> of <n>; <n-k> with no fixed version yet)` when only `<k>` do,
  and nothing for one; with no `fixed_in`, the first finding's own fix.
- **Approve all.** For two to the route's 200 findings nobody has approved, `[ Approve all <n> for
  <name> <version> ]` opens the same form under the card, `<n>` counting those, headed `Approves these
  <n> blocking findings of <name> <version>, found by <tool>:` with each one's title on its own line,
  and its expiry note reads `After that date the problems come back in the checklist.` `[ Approve all
  <n> ]` sends them together (`POST /findings/approve`): one approval per finding, each as if approved
  alone, and one audit row naming the dependency and listing the findings, which the trail reads
  `approved <n> problems in <name> <version> with a reason`. Every row then reads approved.
- **Copy for your agent** copies the card as one block: `Fix these findings from Canyon's review of
  <app>:`, then the group's entry in `canyon check --ai`'s words (cli.md §5.2, *A package's CVEs are
  one entry here too*) — `title:` the head's line, `severity:`, `blocks: yes`, `rule:` the Canyon
  rule (`deps.known-vuln`), `found by: trivy`, `file:`, `line:`, `findings: <n>, one per CVE:` and a line each, `  - <vulnerability>
  (<severity>): <title> · fixed in <fixed>` — then `suggested fix:` the card's fix line, and the
  steps after the fix every finding's copy ends with.
- A dependency's advisory finding keeps its own card, with the same location line and its row in
  place of its snippet and its why, and, when it carries `fixed`, the fix line for one: `Upgrade
  <name> to <fixed_in> and commit <file>.` A finding whose tool names no dependency is never grouped.

**A card is an entry, and every count counts entries** (amended 2026-10-11, #2013: claims-portal's
one hand-built query read "2 blocking findings" here and "1 blocking finding" in the CLI). The run
read numbers each finding's entry (contracts/finding.md, *One problem, one entry*), and the screen
joins by that number alone: the findings of one entry are one card where the first stood. A
package's CVEs are the dependency card above, blocking or advisory; one Canyon rule on one line found
by several tools' rules is one finding card, the first finding's title, place and excerpt, its
`Found by` line naming each tool and how many of their rules agreed (`Found by opengrep (2 rules)`),
and its approval acts on every finding of it together. Every count — the headline's `<n> blocking
findings`, the verdict, the Review cell, `What blocks <version> <n>`, each advisory group's,
`Doesn't block <n> findings` and the fleet's details panel's headline — counts entries, open while
any finding of the entry has no approval, so the screen, the CLI and the check run say one number.

Three deliberate frictions: a required free-text reason, an expiry, and naming who will see it. The bypass is legitimate; it is never silent. The name is the signed-in person's, and this form exists only here: `POST /findings/:fingerprint/approve` refuses a CLI session, so a waiver is an act a person performs in Canyon rather than one an agent performs with a person's session (`design/AUTH.md` §4.4). Including this *strengthens* the demo — a gate with no escape hatch reads as a toy that a real org would rip out in week two.

### Finding cards — the full text

Collapsed cards show severity, rule id, title, location. Expanding shows what a senior engineer would actually say.

A finding an earlier AI review made, carried while its code is unchanged (its `carried_from`; contracts/finding.md, *A
blocking finding an earlier review made stays while its code stands*; #2900), carries the tag `Found in an earlier
review` beside its severity, and under its problem the line `Canyon's AI review found this in an earlier review. This
review didn't mention it, but the code hasn't changed, so it still blocks until the code changes or someone approves
it.` (EXPERIENCE.md D5). Its agent block carries the AI format's `earlier review:` lines (cli.md §5.2).

---

**Finding 1 — expanded**

> **CRITICAL** · Security · `secrets.client-bundle`
>
> ### Stripe live secret key is bundled into the browser build
>
> `apps/web/src/lib/payments.ts:14`
> ```
> 12 │ import Stripe from "stripe";
> 13 │
> 14 │ const stripe = new Stripe("sk_live_51MZq8f••••7Kx2");
> 15 │
> ```
> **Confirmed in the shipped bundle** — `dist/assets/index-B7f2a9.js`, byte 41,208.
>
> **Why this matters.** `payments.ts` is imported by `CheckoutPanel.tsx`, which is a client component. Everything a client component imports ends up in the JavaScript your users download. This key can charge cards and issue refunds on Meridian's live Stripe account.
>
> **The fix.** Move the Stripe call behind a server route — `app/api/checkout/route.ts` — and read the key from `process.env.STRIPE_SECRET_KEY` there. Canyon can hold the key so it never enters the repo.
>
> **Also: rotate it.** Fixing the code doesn't un-leak the key. It's been in a public bundle for 6 days. Roll it in the Stripe dashboard.
>
> Canyon stores the redacted value only. The full key was never written to disk.
>
> Violates **No secrets in the code** · Blocks production
>
> `[ Copy for your agent ]`  `[ Mark resolved ]`  `[ Approve with a reason ]`

Three details in that card are load-bearing:

- **"Confirmed in the shipped bundle."** Anyone can grep source for `sk_live`. Proving the string survived the build into a file a browser downloads is the difference between a linter and a gate. It's also the sentence that makes a skeptical platform engineer sit up.
- **"Also: rotate it."** No cop tells you this. A senior engineer always does. This is the single strongest trust moment in the twenty minutes, and it costs one sentence.
- **"Canyon stores the redacted value only."** A security tool that hoovers up your secrets is a new problem. Say it where the secret is.

---

**Finding 2 — expanded**

> **HIGH** · Security · `auth.unauthenticated-route`
>
> ### `/api/admin/policies` is reachable without a session
>
> `apps/web/src/app/api/admin/policies/route.ts:8`
>
> **Why this matters.** The handler reads `params.orgId` from the URL and returns every policy row for that org. There's no session check in the handler, and `middleware.ts:22` excludes `/api/*` from the matcher — so nothing upstream is checking either. Any unauthenticated caller can enumerate org IDs and read Meridian's policies.
>
> **The fix.** Either add `/api/admin/:path*` to the middleware matcher, or call `requireSession()` at the top of the handler. The matcher is the safer of the two — it covers routes nobody's written yet.
>
> Violates **Every route needs sign-in** · Advisory — does not block promotion
>
> `[ Copy for your agent ]`  `[ Mark resolved ]`  `[ Approve with a reason ]`

"It covers routes nobody's written yet" is the tell that a human-grade reviewer looked at this. Recommending between two valid fixes, with a reason, is what seniority sounds like.

---

**Finding 3 — expanded**

> **MEDIUM** · Code · `perf.unbounded-refetch`
>
> ### `ClaimsTable` refetches every claim on each keystroke
>
> `apps/web/src/components/ClaimsTable.tsx:61`
>
> **Why this matters.** The `useEffect` depends on `query` with no debounce, and the endpoint has no pagination. Meridian's claims table is 12,400 rows — that's a full scan per character typed. It's fine in the demo dataset and will be slow in production.
>
> **The fix.** Debounce to ~250ms, or move the filter server-side. Neither is urgent.
>
> **Advisory — this does not block promotion.**
>
> `[ Copy for your agent ]`  `[ Dismiss ]`

**This finding is the most important one on the screen, and it isn't even blocking.** Two blockers alone read as a security scanner. A third finding that's genuinely useful, honestly labeled "neither is urgent," and explicitly *not* blocking proves three things at once: the gate reads code and not just patterns, it distinguishes severity honestly, and it isn't looking for excuses to say no.

### "Copy for your agent"

The primary action on every finding. Copies a paste-ready block. Canyon never writes code — that's the harness's job, and the boundary is the product.

The line under the first names the severity and the finding's rule, as `canyon check --ai`'s head names it (cli.md §5.3). A tool's finding names the Canyon rule its result is mapped to, and the tool's rule on the next line: `CRITICAL secrets.client-bundle`, then `found by: gitleaks's rule stripe-access-token` (amended 2026-10-10, QA round 7). A finding no Canyon policy covers, its rule `sarif.<tool>.<its rule>`, is named by its title instead: `MEDIUM User input from the q search parameter reaches db.claims.search unvalidated`, then `found by: semgrep's rule typescript.nextjs.unvalidated-search-param` (amended 2026-10-09, QA round 5).

```
Fix this finding from Canyon's review of claims-portal:

CRITICAL secrets.client-bundle
blocks: yes
apps/web/src/lib/payments.ts:14 — a Stripe live secret key is
constructed in a module imported by a client component, so it ships
in dist/assets/index-B7f2a9.js.

Move the Stripe call to a server route and read the key from
process.env.STRIPE_SECRET_KEY. Don't inline the value anywhere.

required end state:
  No credential literal appears anywhere under apps/web/src/. The Stripe client is constructed only in server-side code, and the key is read from process.env.STRIPE_SECRET_KEY at runtime.

do not:
  - move the value to a NEXT_PUBLIC_, VITE_, REACT_APP_, or PUBLIC_ variable
  - move the literal into .env, a comment, a config file, or a different module
  - rename the variable
  - add an ignore comment, suppression pragma, or --force
  Each of these produces the same finding on the next run.

Then run: canyon check
This runs the identical review without deploying. It takes about a minute.
If it prints RESULT: PASSED, run: canyon deploy

If canyon check still prints RESULT: BLOCKED after two full rounds of fixes,
stop and show the user the remaining findings. Do not keep looping.
```

`blocks`, the required end state, the do-not list and the steps after the fix are the words `canyon check --ai` prints for the same finding, so an agent gets one instruction whichever surface it was copied from. A finding that does not block copies `blocks: no`, a required end state of "Not required. This severity does not block deployment.", and no do-not list. The column and the source line stay on the command line.

Harness-agnostic by construction — it's plain English with file:line, which is the one interface every AI coding tool shares.

### Review provenance

Under the gate header, small and monospace: `pkg 4a91c2 · gate v1 · 4 checks · 11.4s · 3m ago`.

For a never-reviewed app, this block is replaced by the most important empty state in the product:

> **Never reviewed.**
> Canyon can't tell you whether this app is safe. It has never been through a gate.
> `[ Run review ]`

Note what this does *not* say: "0 findings." Zero findings and never-looked are opposite facts and the UI must never render them the same way.

### System libraries (#2079)

Below Outbound, for a contributor and above (design/PERMISSIONS.md, the *System libraries* card): what the image each
environment runs holds of Canyon's, as `platform.known-vuln` reads it, from `GET …/envs/:env/image` (contracts/routes.md),
read once per deploy of each environment that runs something. Titled `System libraries`, with `Beta`, then `What Canyon
adds under the app's own code: system libraries such as OpenSSL, and the build tools that install Node, Python or Java
and start the app.` Under `Preview` and `Production`, each its own region:

- the read's `why`, the server's words, after `✓` when it checked and counts nothing, `✗` in red when it `could not
  check`, else `·`; then, when it checked, `Packages listed <ago> · checked against vulnerability data from <ago>`
  (`scanned_at`, `db_built_at`);
- each package `patched[]` names, once, `Fixed by Canyon · <name> <from> → <to> · <vulnerability> · <place>`, its
  vulnerabilities joined by `, ` and `<place>` where it was, as below (amended 2026-11-03, #2631);
- each counted package one card, its findings together: its severity, `<name> <version>: <n> known vulnerabilities`,
  where it is — `system libraries` for the `os` class, `build tools` for `runtime` and `lifecycle`, never the class's
  own name — each `<vulnerability> · fixed in <fixed>`, and the finding's fix. No `Copy for your agent` and no
  approval: nothing in it is the app's to change;
- the notes folded under `<n> notes`, each `<name> <version> · <why>` — `no fix published`, `Canyon can't tell what
  put it there`, `not checked` — and where it is, when Canyon can tell.

An environment the read answers `404` (nothing deployed) or `403` has no region; a read that fails, or does not read
whole, says `Canyon couldn't read what runs in <env>: <error>`, never nothing found. While every environment reads
`not_scanned` there is no card: the rule is off by default and Canyon lists no image then. A read-only member and a
responder with no grant never read it, nor see the card (the reduced page reads no image).

### Secrets (design/specs/mcp.md §10.1)

Below System libraries, for a contributor and above, an owner and an admin among them (design/PERMISSIONS.md, *Change
it*: secrets): the app's secrets by name, from `GET …/secrets`, and the one form on the dashboard that sets one
(`PUT …/secrets/:name`) or removes one (`DELETE …/secrets/:name?env=`; contracts/routes.md, unchanged). It is how a
person whose agent has no CLI sets what the app needs (mcp.md §10.1). Titled `Secrets`, then `Values the app reads by
name. Canyon keeps each in its vault and never shows one back.`

- **The list**, one row per environment and name: the name, `Preview` or `Production`, `Runtime` or `Build`, its
  version, who set it and when. A row the PUT wrote (`origin: user`) has `Replace` and `Remove`; one a deploy wrote
  from a declared resource (`provisioned`) says `Canyon sets this one for the app`, with neither, as the routes refuse it (`409
  provisioned`). With none: `No secrets set.`
- **`Set a secret`**: `Name`, `Value`, `Environment` (`Preview`, the default, or `Production`) and `Where` (`Runtime`,
  the default, or `Build`), then `Set secret` (until a name and a value are typed, it says `Name the secret first.` or
  `Type the value first.`), and under it `Setting a name that's already set replaces its value.`
  `Value` is a password field: never filled from anything, sent once in the PUT, emptied when the PUT lands, and held
  in no read, request log or audit row (the audit row names who set which name). Nothing is checked here that the
  route checks: a name, an environment and a `where` are sent as typed, and a refusal is the route's own message, the
  form as it was. Done: `Set <name> for preview. It takes effect at the next deploy.`, for production `Set <name> for
  production. It takes effect at the next promotion.`, and `Name` is emptied too, so the next value can't overwrite the
  same secret by accident.
- **`Replace`** fills the form's name, environment and where from its row, and puts focus on `Value`.
- **`Remove`** asks `Remove <name> from preview?` (or `production`), `Remove` and `Cancel`; done: `Removed <name> from
  preview. It takes effect at the next deploy.` (`… production. It takes effect at the next promotion.`).
- **Who sees it.** A contributor, an owner and an admin. A read-only member, a person with no access and a responder
  with no grant see no section, and no form: the reduced page reads no secret.
- The History card has no Secrets tab: the list is here, beside the form.

### Not on this screen

- **A code editor.** Canyon governs; harnesses build. An "Apply fix" button would collapse that boundary and make Canyon a competitor to the tool the visitor already loves.
- **A diff viewer.** Tempting, adds a viewport, and the file:line is what their agent consumes.
- **Logs, metrics, traces, restart button.** All of it is a PaaS control panel. Not the story. (Secrets, above, are
  the one exception, for a person whose agent has no CLI: design/specs/mcp.md §10.1.)
- **A severity filter / sort control.** Three findings.
- **A "share" button.** Nobody shares in minute five.

---

## A3 — Fix → re-review → clean → promote

**Purpose:** Close the loop the gate opened, in the visitor's hands, in under four minutes.

**Emotional beat:** The unlock. A button that was gray becomes a button that is not.

### The two-window moment

The fix happens in the terminal, and that's deliberate. Canyon does not write code, so the demo must not pretend it does. The strip at the top of the gate panel in demo mode:

> **Demo:** run `canyon demo fix claims-portal` to simulate Dana's agent pushing the fix. Watch this page.

"Watch this page" is the whole instruction. What follows is a genuinely great piece of product theater — terminal on the left, dashboard live-updating on the right — and it's the same pattern that makes Supabase's realtime and PostHog's live events demos land. It also front-loads the CLI three minutes before minute 9, when they'll run `canyon deploy` on their own app.

### Live choreography, in order

The Activity rail streams. Each entry arrives with a 200ms fade-and-rise, and the panel above it changes state:

```
09:31  Dana's agent   Pushed pkg 7d3e8f — 2 files changed
09:31  Canyon         Packaging… 2.1s
09:31  Gate           Reviewing — 4 checks
09:32  Gate           Clean. 0 findings. 9.8s
09:32  Canyon         Preview updated → claims-portal.preview.localhost:7777
```

Gate panel during the run: the three finding cards stay visible and **dim rather than disappear**, with a progress bar across the panel header. Findings vanishing before the result is known would be a lie about what's happening.

On `clean`, the cards animate out (stagger 40ms, fade + 4px rise) and the panel resolves to:

> **Gate clean**
> 0 findings · pkg 7d3e8f · 9.8s
>
> 2 findings resolved · 1 advisory dismissed
> `View what changed →`

"2 findings resolved" — resolved, not approved. The word is doing governance work.

### The unlock — the most important 400ms in the product

Sequence, right column, on gate-clean:

1. **The receipt row flips first.** `✕ No critical or high findings open — 2 open` → `✓ No critical or high findings open — 0 open`. The ✕ crossfades to ✓, the row's text color travels red → green over 240ms, and the row scales 1.0 → 1.02 → 1.0. **The reason moves before the button does.** Cause visibly precedes effect.
2. **160ms later, the button.** Lock icon crossfades out. Fill travels muted → primary over 200ms. No bounce, no pulse, no shimmer sweep.
3. **Nothing else moves at the unlock.** No confetti, no sound, and nothing announcing it — the row and the button are the whole event. (The line that names a promotion belongs to the promotion, and appears only after one lands.) The visitor just watched a governance system correctly change its mind, and the restraint is what sells that it's real rather than scripted.

If only one animation in Canyon gets built carefully, it's this one.

### Promote

Click → a small confirm inline in the same panel, not a modal:

> **Promote `claims-portal` to production?**
> Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.
> `[ Promote ]`  `Cancel`

**When preview answered an error** (amended 2026-10-08, the owner; QA round 4, django-400 was promoted
answering 400): when preview's `answered` is an answer to say beside readiness (contracts/routes.md,
`envs.<env>.answered`), the confirm says so in amber under the question —
`v1.4 answered 400 at / on preview. Check the app before promoting it.`, or `gave no answer at /` —
and its button reads `Promote anyway`, secondary, never the primary fill: the eye goes to the warning, not to the risky button. That click is the person's override: it sends
`promote_anyway`, which counts only from the dashboard (the CLI is refused, whatever its flags), and the trail's row reads `Started a promotion of v1.4, despite preview
answering 400 at /` (`Approved for production v1.4, despite …` from the approval screen).

"Production runs the same image as preview, with production's own secrets. Nothing is rebuilt." is a real technical guarantee in one line, and it's the sentence that makes a platform engineer trust the whole pipeline. What you reviewed is what ships.

Then the promotion's own lines, live under the button as the promote run writes them — its
timeline's `build.log` rows, read until the run ends:

```
postgres db: creating
postgres db: waiting on the platform
postgres db: ready
deploying to production
live at http://claims-portal.prod.localhost:7777
```

A resource the platform is still making says so and waits; the list ends on `live at <url>`, when the
Production card is read again, or on the reason the promotion failed, with production as it was.
(Built 2026-09-24, canyonv2-cn93.)

### The landing

The Production card resolves to the payoff:

```
┌────────────────────────────┐
│ Production                 │
│ ● live · 4s                │
│                            │
│ claims-portal.localhost    │
│   :7777                ↗   │
│                            │
│ 7d3e8f · passed review     │
│ promoted by you · just now │
└────────────────────────────┘
```

The URL is large, real, and clickable. `7d3e8f · passed review` sits under it permanently — every production deployment carries its review provenance on its face. That line is the entire product in seven words.

### Earlier versions and Put back (#2060)

Design: design/OPERATIONS.md §1.7. The Production card gains *Earlier versions* under its facts, the
status read's `earlier[]` (at most five, newest first), behind a summary `Earlier versions · 3` so the
card stays the size of Preview's. It is there after production is taken offline too, on the empty card,
and absent when the read carries no `earlier` (a host from before it). Each row reads `v1.3 · 2 Oct ·
promoted by Dana W.`, a patched one `v1.4 · patched 4 Oct · promoted by Dana W.`, and has *Put back*.

```
│ Production                                   │
│ ● Running v1.4                         Open ↗│
│ ▾ Earlier versions · 3                       │
│   v1.3 · 2 Oct · promoted by Dana W. Put back│
│   v1.4 · patched 30 Sep · …          Put back│
│     v1.4's image is no longer on this Canyon, …
│   v1.2 · 28 Sep                      Put back│
│     v1.2's image is no longer on this Canyon, …
│ Putting v1.3 back in production. It replaces v1.4.
│ Production gets the image it ran on 2 Oct.   │
│ Canyon puts back the code, not the data, the secrets or the outbound
│ rules: v1.3 runs against production as v1.4 left it.
│ [ Promote v1.3 ]                             │
│ Close                                        │
```

- **A row whose image is not `kept`** has *Put back* disabled, its reason `v1.2's image is no longer on
  this Canyon, so it can't be put back as it ran. Deploy v1.2 to preview and promote it if you want it
  rebuilt.`: no review is asked for, so none spends the run slot or the hour's limit to find that out.
- **Put back** asks for a rollback review (`POST /gate-runs {package, rollback_to}`); the button reads
  `Putting back…` while it asks, and a second click asks nothing. Then `Reviewing v1.3 again under
  today's rules. Preview is not touched.`, `the run →` (that run's findings, in a new tab, so the card
  keeps its place) and `Cancel review`, which cancels it. A refusal (`not_earlier`, `run_in_progress`,
  `rate_limited`) is the server's sentence, and Put back asks again. A read of the review that fails is
  said and the next read is asked anyway; the line goes once one answers. Another row's *Put back*
  starts over with that row.
- **When the review passes**, its receipt (`GET /status?run=`) carries `ships`, and the card shows the
  confirm's three lines and `Promote v1.3`, as the CLI's (design/specs/cli.md §6.3): with nothing
  running in production the first ends `Nothing runs in production yet.` and the third reads `Canyon
  puts back the code, not the data, the secrets or the outbound rules.` The click promotes that run
  directly, as a browser session does today; the button reads `Promoting v1.3…` while it asks. The
  promotion's lines follow under it, the keyboard with them, and stay to its end; then the review's
  lines go. A passing receipt with no `ships`, or one naming another image than the row's, says `v1.3's
  review doesn't name the image production ran, so nothing was promoted` and offers no Promote.
- **Blocked today**, the run's verdict and a disabled `Promote v1.3` carrying the receipt's reason; a
  receipt waiting on a person (`prod.human-approval`) the same, with that rule's words. While the review
  ended `blocked` and its receipt blocks, the receipt is read again every two seconds: its findings are
  approved on `the run →` (*The approval*), and the card offers `Promote v1.3` once they are (#2198).
- **The image gone** (`failed`/`image_gone`): `v1.3's image is no longer on this Canyon, so it can't be
  put back as it ran. Deploy v1.3 to preview and promote it if you want it rebuilt.`, and no Promote.
- **A refused promotion** is the server's sentence. `outdated` and `nothing_to_ship` take the confirm
  away — the review is asked again — while `run_in_progress` leaves it to ask again; one carrying a
  receipt shows that receipt's reason on the disabled button.
- **Close** puts the review's lines away once it has ended; it is not offered while a request is in
  flight or a promotion runs (the promotion has its own *Cancel promotion*).
- **Who sees it**: contributors, owners and admins, the status read's readers. A read-only member reads
  the reduced project row alone: no *Earlier versions*, no *Put back*.

### A held production (#2069)

Design: design/OPERATIONS.md §3.7, §3.8; who reads and does what is design/PERMISSIONS.md's *Security
responders*. While production is held, the read's `hold` (and the receipt's `held`) drives three places:

- **The app page's banner, above production**: the headline band reads `Production is held` over D5's
  sentence, `Production was stopped by <who> on <date>: <reason>. It stays stopped until a responder or an
  admin lets it run again.` It outranks every other headline, as a stopped production does. Under
  *Go-live checks*, `Promote <version> to production` is present and disabled, that sentence its reason
  (D6). Production's card reads `Held` over the same sentence, never `Promote a version from preview
  to run it here.` (amended 2026-10-15, #2258). The team and admins read it; a responder with no grant
  reads the same banner under their band, the same card, and no Promote; a read-only member reads
  production as one with nothing deployed, and nothing of the hold.
- **The fleet row**: `Held` in the Production cell, for whoever reads the row's `hold`; the Filter
  menu's `Only held apps` asks for `held=true`. For a responder or an admin, `Stop production` on every
  row not held asks `Stop and hold <app> in production?` over the CLI's two lines and a reason (sent
  once one is typed). On a held row, `Lift the hold`, offered to an admin and to a responder whose row
  is reduced — off the app's team; one on it reads the team's row and is offered none — asks `Lift the
  hold on <app>?` over the sentence, with `Leave production stopped` and `Start <version> again` (no
  start when production ran nothing). The row's buttons are named with the app (`Stop production of
  <app>`, `Lift the hold on <app>`). A question sends once — while it sends, `Stopping production…` or
  `Lifting the hold…` stands in for its buttons and neither Cancel nor Escape closes it — and its answer
  closes that question alone, never one asked since. A lift names the hold it read (`held_at`): when the
  fleet's next read shows another hold on the row, or the server answers `409 hold_changed`, the question
  says `production's hold changed since you read it: read it again` and offers no choice. The server
  decides each, so a refusal (`responder_required`, a team on the hold's record) is said in its own words.
  The row still opens the app: a responder reads its runs to approve them (#2204).
- **Who is offered nothing**: an app's owners and contributors read `Held` and the banner, and neither
  stop nor lift it; read-only and no-access members read no hold anywhere.

### Not in this flow

- **A success modal.** It would interrupt exactly the moment the visitor wants to click the URL.
- **Confetti.** We just blocked a leaked payment credential. Celebrating with party emoji cheapens it.
- **A "what's next?" upsell.** The fleet's remaining `5 apps never reviewed` is the next step, and it's already on screen when they navigate back.

---

## A3.5 — Their turn (minutes 9–14)

Mostly CLI, but it terminates in my lens and the landing must be right.

The visitor runs `canyon deploy` in some app directory of their own. Whatever it is — a Vite thing from Lovable, a FastAPI service, a half-finished Next app. The dashboard side of that:

**A new row appears in the fleet, live, while they're watching.** No refresh. It lands at the top with a brief highlight, and it is honest about what Canyon doesn't know yet:

```
● receipt-scanner    Vite + React (detected)   No owner   Reviewing…
```

`(detected)` is a small but important word — Canyon inferred the stack and says so rather than asserting it. When inference is uncertain the row reads `Python (likely)` and the app detail offers `Not right? Set the stack →`. Confidently wrong is the fastest way to lose a platform engineer.

Then the review runs on *their* code, and this is the real test — findings on the demo app can be dismissed as planted, findings on their own repo cannot.

The empty-owner state is the hook into governance: `No owner` in amber with a one-click `Claim this app` that sets them as owner and writes an audit row. Twelve seconds of work, and now they've used the ownership model instead of reading about it.

### Not here

- No project-creation wizard, no framework picker, no "connect your GitHub" gate. `canyon deploy` from any directory, or it doesn't fit their reality.

---

## A4 — The governance payoff (minutes 14–20)

Two screens. Both are proof, not features.

### A4a — Audit

**Purpose:** Prove that everything that just happened was recorded, in a form a human can read.

**Emotional beat:** *"I could hand this to our auditor."* Quieter than the gate, and it's the one that closes an enterprise adoption.

**Layout:** Full-width feed. Filter row on top. Rows are 44px, timestamps monospace and tabular, each row is a **sentence**.

```
[ All ] [ People ] [ Agents ] [ Canyon ]   App ▾   Last 24 hours ▾   Export ▾

09:33  ● You                Promoted claims-portal to production — pkg 7d3e8f
09:32  ⬡ Canyon             Reviewed claims-portal @ 7d3e8f: clean,
                            0 blocking, 0 advisory                               view →
09:31  ◆ Dana's agent       Pushed pkg 7d3e8f for claims-portal · Claude Code · 2 files
09:12  ⬡ Canyon             Reviewed claims-portal @ 4a91c2: blocked,
                            2 blocking, 1 advisory                               view →
09:11  ◆ Dana's agent       Pushed pkg 4a91c2 for claims-portal · Claude Code
08:02  ● Priya Raman        Deployed adjuster-notes to production — pkg c1f440
                            no review on record                                  ▲
```

**The actor filter is the novel element.** `People · Agents · Canyon` as a first-class facet. In an org where most changes originate from an AI harness, "who did this" has three genuinely different answers, and no existing audit UI models that. Filtering to **Agents** answers a question a platform engineer has never been able to ask before: *what did the robots ship this week?*

Two more decisions:

- **Every Canyon row links to its evidence.** `view →` on a gate row opens that run on its app's page (`#/apps/<app>?run=<run>`): the finding set as it existed at that moment, including findings later resolved. An audit trail you can't drill into is a receipt for a receipt.
- **A refused promotion is not on the trail.** The trail records changes that took place (contracts/routes.md, the promotions row); the refusal is on the receipt, and the verdict that caused it is the gate row above it.
- **`no review on record` is rendered as a warning, inline.** The audit log is where the absence of governance shows up as a visible defect, not a blank cell.

Empty state (no activity in range): `Nothing happened in the last 24 hours. Try a wider range.` — at the widest range, 30 days, the sentence ends at `days.`. Not an illustration. Not "You're all caught up! 🎉".

**A member's trail is their own work** (**amended 2026-10-07**): the server answers a member the rows of the apps they are a contributor or owner on and, of the rows naming no app, only their own sign-ins, sign-outs and refused attempts (contracts/routes.md, the audit read), so the feed, its count and its pages are exactly those, never a page thinned in the browser. Under the heading a member reads `Your apps' changes and your own sign-ins, newest first.`, and the `Admin and sign-in changes` and `Guardrail changes` questions are not offered; an admin's screen is as above.

Filters stay put: each one changes its own key in the address and keeps the others, so People then 30 days is still People. The App filter narrows to one app's rows (`project=`), its choices the apps the fleet lists. A custom rule's card links its **History** here (`#/audit?rule=<rule>`): every change to that rule, whatever the range, under `Changes to <rule>` and a `Show every change` link; with none yet, `Nothing has changed this rule yet.` A saved version reads `saved version <n> of the custom rule <rule>`, with its text before and after folded under `The rule's text`.

Export: `CSV` · `JSON` · `Copy link to this view`. One line, no configuration dialog. CSV and JSON hold every row the filters select, read page by page from the same route the feed reads: CSV one row per event (`When, Kind, Who, What, App, Run, Action`, the sentence as the feed writes it, never a cell a spreadsheet would run as a formula), JSON the events as the route answers them. While it reads it says `Reading the trail…`, then `Exported <n> rows.`; the link says `Link copied.` or `The link couldn't be copied.`. The App filter's first choice is `All apps`.

**Not on this screen:** charts, an "audit health score," retention settings, per-event configuration, IP addresses and user agents in the row (they're in the expanded detail where a forensic reader will look, not cluttering a feed a human is skimming).

### A4b — Policies

**Purpose:** Show that the rule that blocked the promote is a *dial the visitor owns*, not vendor behavior.

**Emotional beat:** *"I can govern this."* Specifically: *"I could make this stricter, and I could make it looser, and both are one click."*

**An admin's screen** (**amended 2026-10-07**). A member's rail has no Guardrails, and `#/guardrails` opened by a member is the admins-only page: `Guardrails` as its heading, `Only an admin can open Guardrails.`, `What an app must pass is on that app's page.` and `Back to the fleet` — never the form read-only. It is the dashboard's choice, not the API's: agents and the CLI still read the policies (`GET /api/v1/policies`, `canyon manifest`, the session hook) as any member.

**Layout:** Four rows. That's the screen. Each row is a policy, and the copy is a plain sentence, not a rule name.

```
Policies                                        Meridian Insurance

┌──────────────────────────────────────────────────────────────────────┐
│ No secrets in the code                                [ Blocking ▾ ] │
│ API keys and tokens must not be committed anywhere in the code.      │
│ Blocking in production · advisory in preview                          │
│ Catching 2 apps today — claims-portal, commission-calc          view →│
├──────────────────────────────────────────────────────────────────────┤
│ Every route needs sign-in                             [ Advisory ▾ ] │
│ Every route needs a session check, including API routes.             │
│ Advisory everywhere — findings raised, nothing blocked                │
│ Would catch 3 apps if set to Blocking                           view →│
├──────────────────────────────────────────────────────────────────────┤
│ Approved stacks only                                  [ Advisory ▾ ] │
│ New apps use Next.js, FastAPI, or SvelteKit. Meridian has 6           │
│ frameworks in production today.                                       │
│ 2 exceptions · 1 expires in 12 days                             view →│
├──────────────────────────────────────────────────────────────────────┤
│ Production needs a passing review                      [ Blocking ▾ ] │
│ An app reaches production only with zero critical or high findings,   │
│ and only when a person clicks promote.                                │
│ Blocked 1 promotion today                                       view →│
└──────────────────────────────────────────────────────────────────────┘
```

**A rule with nothing to check against says so.** While no default design system is set (`GET /preferences`' `design_system` is `null`), the row of *Apps use the org's design system* (`design.org-system`) reads under its meaning `Checks nothing until you pick a default design system.`, those last words a link to the picker under Guardrails › What agents are told (`#/guardrails/agents`): the review says nothing of the rule until then (contracts/finding.md), and the row says why (amended 2026-10-11, #714).

**A link to a rule marks its row.** `#/guardrails/policies?rule=<id>` (and the older `#/guardrails?rule=<id>`) opens Policies with that rule's row scrolled to the middle of the screen and marked: a tinted background and an accent bar on its left, `aria-current="true"`. No other row is marked. The guided tour sends a first visitor to `?rule=design.org-system` this way (`design/specs/cli.md` §6.14.1).

**Approvers of a push must link GitHub** (#2342) is a row below the policies, not a policy: it gates who approves, not what passes. One switch, `Off` · `On`, saved as it is changed, off by default; with it on, a GitHub push's blocking finding is approved only by an admin or a responder who linked their GitHub account (design/PERMISSIONS.md). The app page tells an approver who linked none why they can't approve it, and links to *Link GitHub account* while the host can link one; where it can't (the fleet's `viewer.github_link` false), it says so and names Settings › GitHub instead, never a link that ends on the route's `409` (#2351).

**Three states per policy: `Off` · `Advisory` · `Blocking`.** Three states, not a boolean, because "raise it but don't stop anyone" is how every real org adopts a rule — and it's how a platform engineer gets a policy past their VP.

**The impact preview is the reason this screen exists.** Each policy is one row with one control, a segmented `Off · Advisory · Blocking`. Choosing a state does not apply it; the row previews what it would block today, in its *Affects today* column — `3 apps could not promote today: adjuster-notes, quote-engine, renewal-reminder`, or `No app is affected today.` (before any change, the column says the same of the rule at its own state, **amended 2026-10-04**, #1450) — and the change joins one save bar at the foot of the page, which holds every change on the page until one Save:

> **2 changes:** Every route needs sign-in, Advisory → Blocking · Approved stacks only, Advisory → Off
>
> Apps with these findings will be able to reach production. *(when a change loosens)*
>
> Apps already in production keep running: policies gate promotion, never running apps. *(when a change tightens)*
>
> `Discard`  `Save, tightening in 14 days`  `Save`

A Save that writes every change takes the bar away and leaves `Saved.` in its place, the word the rest of Guardrails uses, until the next change (**amended 2026-11-15**, the tour's visitor run).

That tightening sentence — *policies gate promotion, never running apps* — is the single most important reassurance in Act 2. Without it, no platform engineer will touch a control on a screen that governs live production.

`Save, tightening in 14 days` is a grace period, offered only when a change tightens, one click — a grace in days, 14 here; the contract accepts any value from 0 to 365. Loosening takes effect at once. It's how a rule actually gets adopted in a real company: announce, wait, enforce. Giving it a button instead of a calendar reminder is the difference between a policy engine and a policy document. The list ends with `Files Canyon can't read` — `A file the scanners can't open is never waved through.` — locked: a lock and `Always blocks until a person approves`, and no control, since `gate.unreadable` is the gate's own rule, not a policy (contracts/finding.md).

**Exceptions have expiry dates, always.** `2 exceptions · 1 expires in 12 days`. An exception without an expiry is a permanent hole with good intentions, so the UI has no field for "never expires."

**What agents are told.** Guardrails' third tab closes with the manifest, rendered exactly as `canyon manifest` prints it and as nothing else — the panel reads, it does not edit. Under the heading, `Every coding agent in <org> reads this at the start of each session. It's built from the settings above.`, then a line that counts it the way an agent is handed it — `canyon manifest` as its session hook prints it, written for an agent (cli.md §7.4; amended 2026-10-07) — `847 lines · ~1220 tokens`. Past 500 lines — Cursor's own published guidance for a rules file, a recommendation and not a refusal — the panel adds a second line: `Over 500 lines. Agents read all of it every session: consider retiring policies or stacks that no longer apply.`

**Not on this screen:** a policy editor, YAML, a rule DSL, a "custom policy" builder, an import/export of policy-as-code, a policy marketplace. The brief says *tune guardrails, don't write them*. Four dials that ship on. If someone needs a fifth policy in v1, they file an issue and we write it — that's a healthier signal than a DSL nobody uses correctly.

---

# ACT 2 — day 2, the governor's arc

---

## B1 — Make it yours (BYO infrastructure)

**Purpose:** Move apps from the demo's local Docker into the visitor's real AWS/K8s, without a wizard.

**Emotional beat:** *"my apps, my account, my rules — Canyon is just the control plane."* And underneath: *"what exactly is this thing about to do in my account?"*

**Layout:** One page, `Infrastructure`. Two sections.

```
Where apps run

┌──────────────────────────────────────────────────────────┐
│ ● Canyon demo (local Docker)                    default  │
│   8 apps · this machine · nothing leaves your laptop     │
└──────────────────────────────────────────────────────────┘

  + Connect AWS       + Connect Kubernetes       + Connect Fly.io
```

Connecting AWS is **one form**: account role ARN, region, domain. No stepper, no 7-page wizard. Then the screen that earns the trust:

> **Connected — `meridian-prod` · eu-central-1**
>
> **What Canyon will create in your account**
> · One ECR repository per app — `canyon/claims-portal`
> · One namespace per app in your cluster
> · One IAM role, scoped to those two things
> · Ingress records under `*.apps.meridian.example`
>
> **What Canyon will never do**
> · Read anything outside the namespaces it creates
> · Store your AWS credentials — Canyon assumes a role you own and can revoke
> · Delete anything it didn't create
>
> **Verified just now:** role assumption ✓ · ECR write ✓ · cluster reachable ✓ · DNS delegated ✓
>
> `[ Move an app here ]`

**The "will never" list is the point of the screen.** Every infra tool tells you what it does. Almost none tell you the boundary. For a governance product asking for a role in a production AWS account, the negative space is the pitch — and "a role you own and can revoke" is the sentence that gets it past a security review.

Moving an app: pick an app, pick the provider, and it redeploys **the same reviewed package** to the new home. The app detail's Production card then reads `meridian-prod · eu-central-1` instead of `local`. Nothing else in the product changes — the gate, the policies, the audit trail are identical. That invariance *is* the control-plane story, and it lands better by being uneventful than by being announced.

**Not on this screen:** a provider registry, cost estimates, a region picker map, Terraform export, multi-region, staging environments beyond preview/production.

---

## B2 — Policies, day 2

Same screen as A4b, now used rather than admired. Two additions that only matter on day 2:

**Per-policy detail (`view →`)**: which apps it catches, which findings it produced in the last 30 days, and the exception list — each exception showing who granted it, why, and when it expires.

```
Exceptions — Approved stacks only

  quote-engine · FastAPI          granted by Priya Raman · expires in 12 days
  "Actuarial team owns this; migration planned for Q4."          [ Renew ] [ Revoke ]

  renewal-reminder · Node cron    granted by Priya Raman · expired 3 days ago  ▲
  "Temporary — replacing with a scheduled job."                  [ Renew ] [ Revoke ]
```

**An expired exception is a finding.** It appears in the review queue as `Exception expired — renewal-reminder is running on an unapproved stack`. This is what makes governance a living system rather than a spreadsheet of good intentions, and it costs one query.

---

## B3 — Onboard a builder (zero behavior change)

**Purpose:** Get a non-technical employee shipping through the gate without ever opening Canyon.

**Emotional beat:** *"I don't have to retrain anyone."*

**Layout:** `People` → `Invite`. The output of the invite is not an email with a login link. It's **one line the builder pastes into whatever harness they already use.**

```
Invite a builder

  Email    [ tomas@meridian.example      ]
  Can ship to    ( ) preview only   (•) preview, and request production

  [ Send invite ]

  ─────────────────────────────────────────────────────────────

  Tomas gets one line to paste into their AI tool:

    npx canyon@latest connect --org meridian --token cny_••••••••

  That's it. They keep using Lovable, Cursor, Claude Code — whatever they
  already have open. Their next deploy goes through the gate.
```

**"That's it."** is the whole promise, and the screen has to be short enough to make it credible. A long onboarding screen would contradict its own copy.

The People list shows what actually matters — which harness each builder ships from, detected, not declared:

```
People                                              4 people · 3 harnesses

  Dana Whitfield     ships from Claude Code     3 apps    last shipped 2h ago
  Priya Raman        ships from Bolt, Cursor    2 apps    last shipped 1d ago
  Marcus Bell        ships from v0              2 apps    last shipped 5d ago
  Tomas Neri         ships from Lovable         1 app     last shipped 6d ago
```

That column is a small joke that lands as a proof: Canyon genuinely doesn't care which tool built the app, and the fleet is heterogeneous on purpose. It also gives a platform engineer a fact they currently cannot get anywhere — *which AI tools are actually in use in my company.*

**Not on this screen:** roles beyond two, SSO configuration wizard, SCIM, group mapping, seat management, a permissions matrix. All of it is real and none of it is day 2.

---

## B4 — The morning review queue

**Purpose:** The screen the platform engineer opens with coffee. This is the habit; everything else is an event.

**Emotional beat:** *"fifteen minutes and I know the state of my fleet."*

**Layout:** Three sections, in this order, on `Reviews`. Ordered by *what needs a human*, not by time.

```
Reviews                                                 Overnight · 6 events

Needs you — 2
┌──────────────────────────────────────────────────────────────────────┐
│ commission-calc → production          Tomas Neri · requested 21:40    │
│ Gate clean. Waiting on a person.                    [ Approve ] [ ↗ ] │
├──────────────────────────────────────────────────────────────────────┤
│ renewal-reminder                      exception expired 3 days ago    │
│ Running on an unapproved stack (Node cron).      [ Renew ] [ Review ] │
└──────────────────────────────────────────────────────────────────────┘

Blocked — 1
┌──────────────────────────────────────────────────────────────────────┐
│ quote-engine → production             Marcus's agent · 23:12          │
│ 1 critical — user input concatenated into SQL                         │
│ quotes/repository.py:88 · sent to Marcus's agent 23:12          [ ↗ ] │
└──────────────────────────────────────────────────────────────────────┘

Shipped — 3
  broker-dashboard  → production  06:02  clean  Priya's agent
  fnol-intake       → preview     04:41  clean  Marcus's agent
  policy-lookup     → preview     23:55  1 medium (advisory)  Marcus's agent
```

**"Needs you" is first and it is small.** Everything Canyon could decide by itself, it decided. The two things on the list genuinely require a human — an approval and an expired exception. If this section is routinely long, the policies are wrong, and that's a signal worth designing for rather than hiding.

**"Blocked" reads as informational, not alarming.** `sent to Marcus's agent 23:12` — the system already did the useful thing overnight. The platform engineer's job is to notice, not to intervene. That's the whole rhythm.

The empty state is a reward, not an illustration:

> **Nothing needs you.**
> 4 apps shipped overnight, all clean. 0 blocked.

Keyboard-first: `j`/`k` to move, `a` to approve, `x` to open, `⌘K` for anything. This screen gets opened every day by the same person; it should feel like a mail client, not a web app.

**Not on this screen:** charts, a date-range picker (it's *overnight*, that's the point), notification settings, a "mark all read."

---

## B5 — Fleet trends: entropy goes down

**Purpose:** The slide they show their boss. Provable, not vibes.

**Emotional beat:** *"I can show this to my VP on Monday and the number goes the right way."*

**Layout:** `Fleet → Trends`. 60-day window. The four hero metrics are **the same four tiles from the fleet home**, now with history. The continuity is the design: on day 1 those tiles were the accusation; on day 60 they're the receipt. Same numbers, same order, same position.

```
Fleet trends                                    Last 60 days

  Secrets in client bundles          2  →  0        ▔▔╲___________
  Apps never reviewed                5  →  0        ▔▔╲╲__________
  Apps without an owner              2  →  0        ▔▔▔╲__________
  Frameworks in production           6  →  3        ▔▔▔▔╲▔▔╲______

  Review coverage                  37%  →  100%     ___╱▔▔▔▔▔▔▔▔▔▔
  Deploys per week                  11  →  19       __╱╲_╱▔╲__╱▔▔▔
  Median blocked → clean         4h 12m →  38m      ▔▔╲__╲________
```

**`Deploys per week: 11 → 19` is the most important row on this screen and it must never be cut.** Every governance tool is presumed to slow teams down. Showing shipping velocity *increasing* alongside entropy falling, on the same chart, in the same window, is the argument. `Median blocked → clean: 4h 12m → 38m` is the mechanism behind it — findings that arrive as file:line get fixed by an agent in minutes instead of sitting in a review queue for days.

**`[ Copy as image ]`** — top right, one button. This screen's actual job is to become a slide. Design for the paste. The exported image includes the org name, the date range, and nothing else — no Canyon watermark, no URL, no "generated by." Their win, their slide.

Below the fold, one honest table: `What changed` — the 8 apps, with their day-1 and day-60 stack, owner, and review state. Because the first question the VP asks is "which ones," and the answer should already be on the page.

**Not on this screen:** a metric builder, custom dashboards, a compliance score out of 100, SOC2 badges, benchmarks against "similar companies." A single invented composite score would undermine seven real numbers.

---

## B6 — The incident drill

**Purpose:** A CVE drops. Answer "are we affected" in seconds and "are we fixed" by lunch.

**Emotional beat:** *"this is the day the tool pays for itself."* It's also the moment the fleet inventory — boring for 59 days — becomes the most valuable thing they own.

**Layout:** Advisories land as a banner on the fleet and open a dedicated incident view.

```
┌──────────────────────────────────────────────────────────────────────┐
│ ▲ GHSA-vv3q-8h9m-2r4c — prototype pollution in deep-merge-lite < 4.2.1│
│   4 of 8 Meridian apps have it.                          [ Open →  ] │
└──────────────────────────────────────────────────────────────────────┘
```

The incident view groups by **exploitability, not by app**, because that's the triage a human actually does at 8:41am:

```
GHSA-vv3q-8h9m-2r4c                                     08:41 · High
Prototype pollution in deep-merge-lite < 4.2.1 · fixed in 4.2.1

Reachable from user input — 2 apps                           patch now
  claims-portal        4.0.3    apps/web/src/lib/merge.ts:12 ← req.body
  broker-dashboard     4.1.0    server/routes/quote.js:44    ← req.query

Present, not reachable — 1 app                          patch this week
  fnol-intake          4.0.9    only in a build script

Not affected — 5 apps
  quote-engine, adjuster-notes, renewal-reminder, policy-lookup, commission-calc

  [ Open 3 patch PRs ]
```

**The reachability split is the whole design.** "4 apps are affected" is a fire drill. "2 apps are reachable from user input, here's the line where the request data enters" is a morning. Canyon knows the call path because it already reads these apps for the gate — so it should say so, and say which ones can wait.

`Open 3 patch PRs` opens PRs in each app's repo bumping the dependency. Each PR triggers a package, each package hits the gate, and the incident view tracks it live:

```
09:03  3 PRs opened
10:20  claims-portal    merged → gate clean → promoted
10:24  broker-dashboard merged → gate clean → promoted
11:47  fnol-intake      merged → gate found 1 medium (unrelated) → awaiting Marcus

  2 of 3 patched and in production. 1 waiting on a person.
```

**That last line is the honest version and the better one.** "All clear ✓" would be a lie whenever it isn't true, and a governance tool that rounds up on incident status is worse than no tool. `1 waiting on a person` with a name attached is what closing an incident actually looks like.

Timeline persists in the audit trail as a single linked incident. Six weeks later, when someone asks "how fast did we patch that," the answer is a URL.

**Not on this screen:** a CVE database browser, a vulnerability feed configuration UI, severity scoring calculators, an SBOM viewer, a "risk score." One advisory, the affected apps, the fix, done.

---

# Cross-cutting

## Voice — the gate reads as a senior engineer

Enforceable rules, not vibes:

1. **The subject is the code.** "Stripe live secret key is bundled into the browser build." Never "you exposed," never "this app fails."
2. **Never the word "violation," "non-compliant," "forbidden," or "failed."** Say what's true: `Blocked by 2 findings.`
3. **One sentence of why, always, and it names the real consequence.** "This key can charge cards and issue refunds on Meridian's live Stripe account" — not "secrets should not be exposed."
4. **Give the fix, and where there are two, pick one and say why.**
5. **Say when something doesn't matter.** "Neither is urgent." "Advisory — this does not block promotion." A reviewer who only ever escalates is one you learn to ignore.
6. **Say the thing the fix doesn't cover.** "Fixing the code doesn't un-leak the key. Rotate it."
7. **No exclamation marks. No emoji in findings.** The severity chip carries the alarm; the prose stays level.
8. **The org owns the rule, Canyon enforces it.** "Meridian requires a passing review" — not "Canyon blocked this."

## Motion

Three animations, total. Everything else is instant.

- **The unlock** (A3) — the receipt row flips 160ms before the button changes. Cause visibly precedes effect. Build this one carefully.
- **Live rows arriving** — 200ms fade + 4px rise, in the activity rail and the fleet table. This is what makes the two-window moment work.
- **Findings dimming during a re-review** — they dim, they don't disappear. Never animate away information before the result is known.

No skeleton shimmer on tables under 20 rows, no page transitions, no parallax, no confetti anywhere in the product.

## Empty states

Every one is a fact plus one action. No illustrations, no mascot, no exclamation marks.

| Screen | Copy |
|---|---|
| No apps yet | `No apps yet. Run canyon deploy in any app directory.` |
| Never reviewed | `Canyon can't tell you whether this app is safe. It has never been through a gate.` `[ Run review ]` |
| Review queue clear | `Nothing needs you.` `4 apps shipped overnight, all clean. 0 blocked.` |
| Audit, no events | `Nothing happened in the last 24 hours. Try a wider range.` |
| No findings | `Gate clean. 0 findings.` — never rendered where "never reviewed" is the truth. |

**Amended 2026-10-04** (#1251): an app with no current package — nothing uploaded but by `canyon check`,
whose upload is never the app's version — still reads `Never reviewed` on the fleet, in `canyon status`
and on its page, which under that line shows its newest check's review read-only: `From a check — not
deployed yet · <when>`, the verdict and the findings, with no approve on them. A check's run is the one
the runs list marks `check` (contracts/routes.md). The first upload or deploy takes over, and from then
on a check's run is never shown as the app's review.

## Keyboard

`⌘K` everywhere — jump to app, run review, open policy. Review queue is `j`/`k`/`a`/`x`. That's the entire shortcut surface for v1. This tool gets opened daily by the same person; that's the argument for the command bar and against everything beyond it.

## Accessibility, stated once

Severity is never color alone — every chip carries its word (`CRITICAL`, `HIGH`). The go-live checklist uses ✓/✗ glyphs, not red/green fills. Live regions announce gate state changes so the unlock is perceivable without sight. Everything in the review queue is reachable by keyboard because it's designed keyboard-first anyway.

---

## What I cut, and why

| Cut | Why |
|---|---|
| Onboarding checklist / progress widget | Implies homework, and admits the product isn't self-evident. The blocked app *is* the task. |
| "Apply fix" button in the dashboard | Canyon governs, harnesses build. Collapsing that boundary makes Canyon a competitor to the tool they already love. |
| Uptime / latency / cost metrics on the fleet | A green 99.98% next to a red "2 secrets exposed" makes the red one less scary. Canyon is not an APM. |
| Policy editor / DSL / policy-as-code | Brief says *tune guardrails, don't write them*. Four dials that ship on. |
| Compliance score out of 100 | One invented composite number undermines seven real ones. |
| Success modal + confetti on promote | We just blocked a leaked payment credential. Celebrating cheapens it, and it covers the URL. |
| Diff viewer on findings | Costs a viewport; the file:line is what the agent consumes. |
| SSO wizard, SCIM, roles matrix, billing, multi-region | Named as non-goals. Not on the 20-minute path or the day-2 arc. |
| "All clear ✓" on the incident view | It would be a lie whenever it isn't true. `1 waiting on a person` is the honest close. |

## Open questions for the other lenses

1. **Dark or light default.** I've assumed dark, because the first fleet screenshot goes in the README and dark reads as infra. If the visual lens goes light, nothing in this spec breaks except the tile treatment.
2. **The severity ramp needs four steps but only three colors.** Medium/Low must be visually quiet enough that a Medium finding never reads as blocking — that's what makes finding 3 in A2 do its job.
3. **The unlock animation (A3) is a shared dependency.** It's the one place motion carries meaning rather than polish, and it needs to survive whatever the visual system decides about transitions.
4. **The four fleet tiles appear twice** — as the accusation on day 1 (A1) and as the receipt on day 60 (B5). They must be recognizably the same component in the same order, or the payoff doesn't land.
