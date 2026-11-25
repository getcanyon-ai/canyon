# Permissions — who may see and do what

The one source for Canyon's access rules: app teams, sharing, who opens an app, who changes it,
who sees what. Every route, screen, CLI verb and executor behaviour that reads or changes an app
follows this file; `contracts/routes.md` states each route's minimum level and its refusals, and
where the two disagree this file wins and the other is fixed in the same PR.

Status: decided 2026-09-25 (the owner); being built under `canyonv2-ula5.11` (single host) and
`canyonv2-ula5.12` (Kubernetes). Until a line here is built, the code's current behaviour is the
gap, not the rule.

## The model

**Admins** can do everything on every app, except write a production that is held (below), and
approve a blocking finding of a run they started (*Approve findings*, below).
Everyone else holds, per app, one **level**:

| Level | Open the app | See technical detail | Change it | Approve findings, promote | Manage access |
|---|---|---|---|---|---|
| **Owner** | yes | yes | yes | yes | yes |
| **Contributor** | yes | yes | yes | yes | no |
| **Read-only** (`viewer`) | yes, with the app roles granted | no | no | no | no |
| **None** | no — the no-access screen | no | no | no | no |

- *Technical detail*: gate results, findings, runs, deploys, logs, versions, secrets names,
  the app's audit rows, and where the org's platform says each of the app's resources is (its
  `provided` resources' endpoints, #2432): contributor and above read them on the project read, the app
  page and `canyon status`; the reduced row below contributor carries no `resources[]`.
- *Change it*: upload, check, deploy, undeploy, cancel, secrets, and linking a repository (*Link a repository*, below).
- *Promote* includes approving or denying a production approval, when the org has a person approve
  production in the dashboard (design/AUTH.md §4.5); only a browser session does that, whatever the
  level.
- *Approve findings* is an advisory finding's approval at these levels. **A blocking finding** — one
  whose rule blocks it before preview or before production — **is approved only by an admin or a
  responder, never by the author of the run being approved, and only from a browser session**
  (decided 2026-10-12, the owner; #2204). The author is the person whose session started the run, a run
  an agent started under their session included. A run a GitHub push started is authored by every
  person who linked the GitHub account that pushed (decided 2026-10-18; #2228): a person links their
  own account, from a browser, by signing in to GitHub through Canyon's GitHub App, which proves the
  account is theirs, and Canyon keeps GitHub's numeric id for it, so a renamed login stays linked. A
  push by an account nobody linked has no Canyon author, so whoever pushed, if they are an admin or a
  responder, may approve that push's run — unless the org asks every approver of a push to link
  GitHub (Guardrails, *Approvers of a push must link GitHub*, `push_approvers_linked`; off by default;
  decided 2026-10-18, the owner; #2342). Then an admin or a responder who linked no GitHub account is
  refused `github_link_required` on a push run's blocking finding, and no approval of theirs covers
  one, wherever it was filed, while the setting is on. Only an admin changes it, from a browser, and
  never turns it on with sign-in off.
  Their own runs stay theirs to no one: an approval they file never covers a finding
  of a run they started, whatever run it was filed on (below). A responder approves on any app, shared with them or not, as a security team does (decided
  2026-10-13, the owner): with no grant they reach the approve routes and what approving needs (*Security
  responders*, below). Only an
  approval filed so covers a blocking finding, wherever an approval is read: one filed before this
  rule, or by anyone else of a finding then advisory, covers it nowhere while it blocks (fail
  closed). Nor does an approval ever cover a finding of a run its approver started, whichever run
  it was filed on — the same fingerprint recurs across runs — so "never the author" holds where an
  approval is read as well as where it is filed. A fixed finding needs no approval: a new version is
  reviewed from scratch. The approval keeps its expiry and its audit row, whose actor is the approver
  and whose detail names the author. An approval within 10 minutes of its expiry no longer clears a
  deploy or a promotion, which judges it as it starts the container (`runs.ShipMargin`), though every
  read shows it until it expires. An approval stands when its approver later loses the role: a
  demotion revokes none. One ends at its date, and the finding's fix doesn't end it: the same finding
  back before then is covered again (corrected 2026-11-23, #145: it said a fix ends one). Turning sign-in
  on voids those filed with it off. Another
  approval of the finding, by someone who may approve it, replaces its date.
  A known-vulnerable package only the built app holds — what `deps.known-vuln` counts in the image before
  production, in no run's findings (design/PATCHING.md §3.6) — is approved as a blocking finding of the run
  the receipt is read for: by the same people, under the same rules, through the dependency's approve
  (decided 2026-11-02, #2610).
- **Why a responder on the team may approve a teammate's finding, yet never lift its hold.**
  Approving asks for a second person, someone other than the author: a responder on the team who did
  not start the run is one. Lifting a hold asks for independence from the team whose judgement is in
  question (below), which nobody on it has. On a host with
  sign-in off every caller is the anonymous admin and none of this refuses anything.
- *Manage access*: share and unshare, change levels, add and remove owners, dismiss access
  requests, rename the app (its display name alone: its id, address and audit subject stay, #1909), delete the app, turn *Auto-deploy pushes to preview* on or off: a deploy no person
  starts is the app's owners' call (#1923, the approved GitHub plan), and hand the repository Canyon
  keeps the app's code in to developers (*Developers push from now on*, #1938): who writes the
  app's code is its owners' call too. Linking an app to a repository is a contributor's, below.
- *Link a repository* (decided 2026-11-20, the owner, design/specs/mcp.md §10.1, built in its PR 8; #1917): a
  contributor links their app to a repository, or a folder of it, and unlinks it; an admin links any repository
  Canyon's App sees. **A member below admin links only a repository that one of their own linked GitHub accounts
  can read on GitHub now**, asked of GitHub with the installation's token, and names it as `owner/name`. Why: Canyon's
  App usually sees every repository of an org, and the security reviews of #1917 found that letting any member link
  one hands them a private repository's code they can't read on GitHub, through its packages, `canyon pull`,
  findings' snippets and build logs. With no GitHub account of their own linked, the link is refused
  `github_link_required`; a repository they can't read answers as one no App sees, `not_found`, so no private
  repository's existence leaks; and the org's list of repositories (`GET /github/repositories`) stays an admin's.
  **Packaging a ref of the linked repository** (`POST …/packages/from-repository`, and the remote MCP tools
  `canyon_check_ref` and `canyon_deploy_ref` that call it) asks GitHub again on every call whether the caller's own
  linked GitHub account reads that repository, an admin's included: another contributor on the app may not read it,
  and a person who loses access on GitHub loses it here at once. GitHub not answering refuses the call. A push still
  packages what it pushes, as before.
- *Hold production* belongs to no level: owning an app lets nobody hold or lift its production.
  A responder or an admin does (below).

**Security responders** (decided 2026-10-11, the owner; `design/OPERATIONS.md` §3). A responder stops
any app's production and holds it, and approves any app's blocking findings (#2204); by the role, they
never ship a new version. It is an org role
(`member | responder | admin`), not a level:

| A responder, on an app not shared with them | |
|---|---|
| Reads | the app's fleet row and project read as a read-only member's reads (name, owners, summary, production's address and state, or when a pod last checked in where the org runs it) plus `hold`; the fleet's **Held** filter (`GET /fleet?held=true`); their own `stop app` and `lift hold` rows; and what approving a blocking finding needs (decided 2026-10-13, the owner; #2204): the runs list (`GET …/gate-runs`); a run's read and its SARIF (`GET /gate-runs/:id`, `…/sarif`), its findings whole — their snippets, a line or a few of the app's code each, masked where they hold a secret, since a finding is approved as it reads; and the status read trimmed to the receipt (whole: `platform.known-vuln`'s row names the packages it counts in the system libraries and build tools Canyon adds, #2300; but each package's `layers` and `go_stdlib`, a contributor's technical detail, #2579; and `deps.known-vuln`'s `built_app` whole, the packages only the built app holds and their fingerprints, which they approve, #2610; and `prod.egress-allowed`'s line naming no destination, since outbound is technical detail, #2125), `last_run` and `promote_run`, production's address and state, or when a pod last checked in (`seen_at`, #2452: when one of the org's pods pulled its access list, which is no answer of the app's), and the hold (`GET …/status`). And production's logs, masked (decided 2026-11-25, the owner; A of incidents and logs): `GET …/logs?env=prod&redaction=masked`, the dashboard's read, at most 200 lines, never followed, each read an audit row as every logs read is, so they can see why production is failing before they stop it. Nothing else: no preview, no earlier versions, nothing an environment answered or said, nobody who deployed, no run timelines or build logs, no preview logs, no unmasked or followed logs, packages or source beyond the findings' snippets (no `canyon pull`), secret names, outbound, access, or other audit rows of any app |
| Does | holds the app's production, with a reason, from the CLI (`canyon undeploy --app <app> --env prod --hold --reason "…"`) or the fleet row's *Stop production*; lifts a hold from the fleet row in the dashboard, optionally starting the held image again; approves a blocking finding — or an advisory one — in the dashboard, never of a run they started (*Approve findings*, above) |
| Never | upload, check, deploy, promote, cancel, set or delete a secret, share, delete the app, undeploy, take preview down, change a policy or setting, open the app |

- **Assignment.** An admin lists responders on Settings › People & sign-in by address
  (`responder_emails`), or the identity provider's `responder_claim`/`responder_value` names them.
  The pair mirrors `admin_claim`/`admin_value` (`design/AUTH.md` §3): both set or neither, set
  from the row or pinned by the operator (`CANYON_RESPONDER_CLAIM` · `CANYON_RESPONDER_VALUE`,
  then `overridden` and `set_by_operator` as for the admin pair), matched at sign-in and kept on
  the session (`claimed_responder`). All three keys are answered to an admin only.
- **The role** is `admin` if admin, else `responder` if listed or claimed, else `member`; the list
  is read live, a claim lasts its session, and an urgent demotion is
  `DELETE /api/v1/sessions?subject=…`, as for admins (`design/AUTH.md` §3). It is computed only for
  a current org member, and a role that cannot be computed is `member`.
- **Responders are not grants: one who also owns an app holds both.** A responder's access to an
  app route is the grant's level, as anyone's; the fleet row and the hold routes admit them with
  none. A responder who holds any grant on an app acts on it with that grant, and may hold its
  production but never lift that hold while on its team (below). A responder asks for access to an app as any member does.
- **Holding** is a person's act, whatever the org's policies: an `agent` or `canyon` actor is
  refused `403 person_required` (`design/AUTH.md` §4.4). While held, every production write is
  refused `409 held`, even an admin's — a promotion, a rollback's promote run, a Canyon patch, an
  org's `deployed` report under hand-off, approving a production approval, deleting the app — except
  the lift's own start. Undeploying production while held answers `409 held` too: nothing runs
  there. Preview, checks and secrets go on, so the team ships its fix to preview and rotates what
  leaked. A hold that cannot be read fails the transaction, so nothing starts.
- **Lifting** (decided 2026-10-11, the owner) is a responder's or an admin's, from a browser session
  only; any other session is `403 dashboard_required` (on a host with sign-in off, where no session
  tells the browser from the CLI, any session). It is refused to anyone on the app's **team** unless
  they are an admin: the team whose judgement is in question never lifts its own app's hold. The
  team is the owner and contributor grants, to a person directly or through a group; a viewer grant
  or an `everyone` share puts nobody on it. The hold records the team as it stood when production
  was held — every owner and contributor grant's address, bound or not; every subject such a grant is
  bound to, or a session at such an address or in a team group carries; and its groups — and a lift is
  refused to anyone whose subject, address or group is on that record, or who is on the team now, so
  no one leaves the team or its group, lifts and comes back. Anyone who signs in to a recorded address
  or in a recorded group after the hold, verified or not, is added to the record by subject in the
  sign-in's transaction, serialised with the hold's, as is a grant a claim or a share binds: refusing
  more people is the safe way. While the app has a team group, now or on the record, a lift is refused
  to a session whose sign-in could not read the person's groups whole (an overage, a claim absent). *Start again* is the one production start a responder or an admin
  makes: the held image's exact bytes, promoted once already, not reviewed again, and recorded as
  `started` with the actor on the trail.

**Grants.** A grant is *who × level × roles*. Who is a named person (email), an identity-provider
group, or **everyone** (org membership: the allowed domains and emails, never "anyone the IdP
signs in"). People and groups may hold any level; everyone only read-only. A grant to a person
names them by address until it is bound to their identity provider's subject, and from then by that
subject alone. A sign-in binds it only when the provider asserts the address verified
(`email_verified: true`, on the `email` claim; an absent claim or an address from
`preferred_username` lets the person in and binds nothing), only onto grants to that address that
name no subject yet under Canyon's current sign-in — never one bound already — and writes one `bind grants` row naming the apps. A
grant bound to one person never names another who holds the address later, and an address the
provider changes takes its person neither off an app nor off a hold's team record. Every path that
writes a grant's subject — that sign-in, creating an app, claiming one, sharing on an access request —
applies the same rule: a session whose address was not asserted verified writes none. A grant
records the issuer of the sign-in that bound it (**amended 2026-10-17**, #2306), since a subject names
a person only under the issuer that issued it. One bound under any issuer but Canyon's current
sign-in's (`CANYON_OIDC_ISSUER`, which an operator changes to move Canyon to another tenant or
provider), matches nobody by its subject: it is read as not yet bound,
matching by address, and that person's next sign-in binds it again under the current issuer, by the
rule above; a claim never promotes it to anyone before then. A grant bound with sign-in off is the
anonymous admin's, bound under no identity provider: once sign-in is on it is read as not yet bound,
and an account whose subject is the same string never matches it. A grant with no issuer recorded —
bound before issuers were, or by a server from before them during a rolling upgrade — is the current
issuer's, as that server reads it, so an upgrade rebinds nothing; the anonymous admin's is under none,
recorded so at the server's start, and every other is recorded the current issuer's at the first
sign-in each server process verifies under it — never on sign-in off, and never on discovery alone,
which a wrong but real issuer passes. So run a release with issuers on the current issuer, and sign in
through it, before changing it: an upgrade and a switch in one rollout read every
subject bound before as the new issuer's. A Canyon session signed in through another issuer than the
current one is refused, and its person signs in again. Group and `everyone` grants are unchanged. On Kubernetes
(**amended 2026-10-16**, #2220) the sidecar in the app's pod matches a bound grant by subject only
when the front door's token comes from Canyon's own sign-in's issuer, since a subject names a person
only under the issuer that issued it. A provider whose subjects differ per client (Entra ID) needs
the front door on Canyon's own client. A token from any other issuer, or an access file that does not
say which issuer its subjects are from, matches a bound grant to nobody: the person still opens the
app through a group or `everyone` grant, and saving such a front door says that person grants will
not apply there. An `alb` front door's issuer is known only from a request the load balancer signs, so
saving one always says that person grants apply only when the load balancer signs people in through
Canyon's own sign-in (**amended 2026-10-17**, #2299). Neither refuses the save. A grant not yet bound
matches by address everywhere. A person's level is
the highest of the grants that match them; their app roles are the union.

**Defaults.** Every app sits behind company sign-in. A new app is **private**: its creator is its
owner, and nobody else but admins can open it until an owner shares it. Nobody becomes an owner by
deploying; owners and admins add co-owners.
An app an admin makes from a GitHub repository (Settings › GitHub, #1917) has **no owner**: it is
private and admins alone can open it. **An app linked to a repository, or holding a package from
GitHub, is never claimed**, unlinked since or not: its versions are, or will be, a repository's code, and approving its findings turns that
repository's Canyon gate check green, so an admin adopts it, naming its owner (contracts/routes.md,
`PUT …/owner`). Anyone may claim any other app no grant owns while it holds no secret and runs nothing
(2026-10-09: the approved GitHub plan said anyone may claim one; the security reviews of #1917 found
that hands any member a private repository's code, and that an unlink alone would hand it back).

**What people see.** Someone below contributor sees an app as its name, its owners and its
one-line summary (`description` in `canyon.toml`) — on the fleet, the app page and every read.
Read-only people can also open it, and see its live addresses — *Open preview*, *Open production* — and whether each runs, or, where the org runs it, when a pod last checked in (#2452), on the fleet and the app page; nothing of what runs there or who put it there. Everything else is team-only.
A responder with no grant on the app is the one exception: they also read production's address
and state (or check-in) and its hold, and what approving a blocking finding needs (above). A hold on production — who, when, why —
shows to the app's contributors and owners, to responders and to admins. To a read-only person, and
on every reduced row, a held or stopping production reads exactly as one with nothing deployed, and
the fleet's Held filter reveals nothing they cannot see. The app's users never see the reason.

**Access requests.** A signed-in person without access sees the owner's name and *Ask for access*;
a read-only person sees *Ask to join its team*, beside *Open app*, to ask to change the app (2026-10-05).
Their request records that they could open it read-only when they asked, and an owner's answer adds
them to the team as a contributor (2026-11-12, #2774).
The request shows to owners on the app's Access card, in `canyon share` and in the next deploy's
output. No email is sent.

**App roles.** An app declares its roles in `canyon.toml` `[roles]` (name + one line of what it
lets a person do); owners assign them per grant. The app receives them in the identity token and
compares the string. Canyon has no finer-grained authorization engine.

**Leavers.** Removing someone from the org's allowed domains or emails removes them from Canyon
and from every app — on the next request on a single host, and by rewriting every running app's
access file on Kubernetes — and from the responder role, which only a current member holds. Their
grants stay on record; an admin adds a successor as owner.

**The org's own records** (decided 2026-10-07, the owner). The audit trail answers someone who is not an
admin the rows of the apps they are contributor or owner on and, of the rows naming no app, only
their own sign-ins, sign-outs and refused attempts. An app's access requests are its owners' (*Access
requests*, above; 2026-10-26, #2500), and so are its grants and its refusals (2026-10-26, the owner; #2505):
the `share app` and `unshare app` rows name who holds what on the app, which only *Manage access*
reads, and the `denied` rows naming it say who tried something on it and was turned away. A
contributor reads none of the three. Every other row naming no app (a policy changed,
an admin's or a setting's change, another person's sign-in, a change they made while they were an
admin) is an admin's. The Guardrails screen is an admin's too. The org's current settings stay
readable to every member through the API, who last set each among them (the policies, the custom
rules, the preferences): the CLI and every agent's manifest are built from them. The history of
those changes is what is an admin's. A responder also reads the `stop app` and `lift hold` rows they
are the actor of, on any app.

**Integrations** (added 2026-11-09, #2725; design/INTEGRATIONS.md). The org's chat channels, where Canyon
posts when production needs a person, are an admin's alone: an admin adds, changes, removes and tests one,
and reads them, never their webhook URLs, which no read answers. Anyone else is answered `403
admin_required`, the reads included. A message goes to everyone in the channel, Canyon users or not, which
is why it carries names only, and the admin who connects a channel decides who reads it. Adding or changing
a channel widens who reads every app's name, so it is a person's, in the dashboard, like every widening: on a
host with sign-in an admin's CLI session is answered `403 dashboard_required`, an agent `403 person_required`.
Removing or testing one works from any session (decided 2026-11-12, #2725). A channel that stops
taking messages is an admin's need, `notify_failing`, on the fleet read's `org_needs`: an admin reads it, and
anyone else reads `[]` there.

**Demo** (`CANYON_AUTH=off`): every caller is the anonymous admin; nothing is refused and nothing
can be shared.

## The gate pipeline — who changes the gates

Decided 2026-10-02 with the redesign (`design/EXPERIENCE.md` D14); built after #1315 and #1316.

| Who | The org's pipeline (Guardrails › Pipeline) | An app's additions |
|---|---|---|
| **Admin** | places, moves and sets every rule | adds and removes them on any app |
| **Owner** | reads it, through the API and the CLI | adds **stricter** rules to their own app, and removes additions there |
| **Contributor** | reads it, through the API and the CLI | reads them |
| **Read-only, none** | reads it, through the API and the CLI | sees none (they are technical detail) |

- **Stricter only.** An addition can never remove or weaken an org rule. It adds a rule the org
  does not run, or raises an org rule's state in a gate. For every rule and gate, the state in
  force is the stricter of the two. A request that would weaken is refused before anything is
  written; the contract names the refusal.
- **No CLI verb changes the pipeline.** An agent working under a person's session acts as that
  person (`design/AUTH.md` §4.4). An `agent` actor is refused, as on approve.
- Changing the org's pipeline is `403 admin_required` for a non-admin. An app's additions are
  `403 owner_required` for anyone but its owners and admins.
- **A dry run of a custom rule** (#2640) judges the rule on every app's latest review, so it is an
  admin's, who sees every app: `403 admin_required` for anyone else, who sees nothing of it.

## Playbooks — who writes how the org builds

Decided 2026-11-19 with the MCP design (design/specs/mcp.md §3, §4). A playbook is org-written
guidance an agent reads and Canyon never checks: free, not Canyon Enterprise.

| Who | The org's playbooks (Guardrails › Playbooks) | A custom rule's `playbook` |
|---|---|---|
| **Admin** | writes, imports, restores and removes them; reads every version, a removed playbook's too | sets it, as the rule itself |
| **Any signed-in member** | reads each playbook that stands, body and all, and the list | reads it on the rule |

- **Writing is a widening of what agents are told**, so it is an admin's, a person's, from a browser,
  as a custom rule's instructions are: `403 admin_required` for anyone else, `403 person_required` to
  an `agent` or `canyon` actor, and on a host with sign-in `403 dashboard_required` from any session
  but a browser's — an admin's agent replaying the CLI's session never rewrites what every agent
  reads. Each refusal writes one `denied` row, before the body is read.
- **Reading is every member's.** A playbook is handed to every agent in the org (the manifest, and
  later the tool), so it is never a secret: a save carrying one is refused.
- **History is an admin's.** Every version is kept, and only an admin reads them, a removed
  playbook's included; a member reads the newest version of one that stands.
- **A playbook changes nobody's level and loosens no rule.** The review never reads one, and a rule
  pointing at one is checked exactly as before.

## Outbound — who changes what an app calls out to

Decided 2026-10-11 (the owner) with `design/EGRESS.md` §3.5 and §14: contributors widen, from the
dashboard only; agents never widen. Built with EGRESS layers 1 to 3: the reads — an app's
records and the host setting, `GET /api/v1/projects/:id/egress` and `GET`/`PUT /api/v1/egress` —
landed in `contracts/routes.md` with layer 1 (#2085, #2092, #2093); the widening rows land with #2104
(layer 2) and #2135 (layer 3).

| Who | Read | Allow · add or change a route | Remove · dismiss | Mode | Host settings |
|---|---|---|---|---|---|
| **Admin** | every app | yes, dashboard only | yes | either way | yes, dashboard only |
| **Owner** | apps they own | yes, dashboard only | yes | stricter than the org | read |
| **Contributor** | apps they contribute to | yes, dashboard only | yes | read | read |
| **Read-only, none** | nothing | no | no | no | read |

- *Read* is an app env's outbound records, its egress requests, its allowed destinations and its
  routes: technical detail, on the app page's *Outbound* and in `canyon status`'s `outbound` row.
  Below contributor there is neither, every egress route of the app is `403 team_required`,
  the fleet's `needs_you` and its `refused` tile count an app's pending requests it refuses now only for its contributors, owners and
  admins (the tile amended 2026-11-08, #2723), and the go-live row `prod.egress-allowed` names no destination to a responder with no
  grant, who reads the rest of the receipt (amended 2026-11-06, #2125). No route, row or setting
  returns a route's value, to any level, an admin's included.
- **Widening is a person's, in a browser.** Allowing a destination, adding or changing a route,
  moving a runtime secret into a route (`from-secret`, layer 3), and setting an app's mode back to
  the org's from a stricter one answer `403 person_required` to an `agent` or `canyon` actor and
  `403 {error:"dashboard_required", message:"change it in Canyon's dashboard"}` to any session but
  a browser's (`contracts/routes.md`, the preferences row's body, byte for byte), at every level
  that may widen, an admin's included. No CLI verb widens. An agent that edits the repository
  never widens its own app's reach.
- **A secret's route is an owner's.** `from-secret` is an owner's or an admin's
  (`403 owner_required`), and so is replacing or deleting a route that came from it: a
  contributor never re-points or swaps a secret an owner placed. Its audit row names the secret
  and the destination, never the value.
- **Narrowing an app's settings works from any session and any actor**: removing a destination or
  a route, and an owner's or an admin's stricter mode. **Dismissing a request** works from any
  session and is a person's (`403 person_required` to an `agent` or `canyon` actor): a dismissed
  destination files no new request.
- **The caps** (decided 2026-11-05, the owner; #2691) — the destinations an app env keeps, the new names it
  may call a minute and the egress requests it may hold pending — are host settings: an admin's to set, from a
  browser (`PUT /api/v1/egress {caps}`), which answers `403 person_required` and `dashboard_required` as the rest
  of it does. Only an admin reads them; a member's read of the setting names none.
- **Mode.** The org default is an admin's. An owner may set their app's mode stricter than the
  org's, as with an app's gate additions (above); setting it back to the org's is a widening;
  anything looser than the org's is `409 looser_than_org`. A contributor setting any mode, a
  stricter one included, is `403 owner_required`. An admin sets it either way, a looser one as a
  widening.
- **A held production.** While production is held, widening the `prod` env's outbound answers
  `409 held`; narrowing goes on, as rotating a secret does. Preview's outbound is unaffected. An
  admin's host change that widens a held production is `409 held` too: the org's mode to `observe`
  while it follows the org's, or via to `off` (amended 2026-10-18, #2110).
- **Adding credentials** (decided 2026-11-09, the owner; #2143). Whether Canyon adds a route's
  credential to apps' calls (`credential_routes`) is a host setting, off until an admin turns it on, an admin's from a
  browser as every host setting is, and audited (`egress.set`). While it is off, adding or changing a
  route, and `from-secret`, answer `409 credential_routes_off` at every level; removing a route still
  works. Every member reads whether it is on.
- **Host settings.** Every member reads `via`, the org default mode and `credential_routes` through the API
  (`GET /api/v1/egress`), as the org's other current settings; the rest of that read (networks to
  recreate, resolvers, each replica's policy age, key dates) is answered to an admin only, and the
  fleet's destinations (`GET /api/v1/egress/destinations`) are `403 admin_required`. Every change
  to them, a tighter one included, is an admin's from a browser (`403 admin_required`, then
  `dashboard_required`).
- **Order.** Level (`team_required`, `owner_required`, `admin_required`), then actor
  (`person_required`), then session (`dashboard_required`), then state (`409 looser_than_org`,
  `409 floor_not_enforced`, `409 egress_external`, `409 credential_routes_off`), then `409 held`. Each of these 403s writes one
  `denied` audit row, a level refusal's as every route's does (contracts/routes.md, the audit read),
  the actor's and the session's before the body is read (amended 2026-10-18, #2110). On an app env's
  mode route alone the body is parsed first, after the level and before the actor, since only the mode
  it names says whether the change widens; a body it cannot parse is `400` with no audit row (amended
  2026-10-18, #2104).
- **A responder** on an app not shared with them reads nothing of its outbound beyond the fleet row
  (above) and the go-live row that names no destination, and changes none of it: `403 team_required`. They read `via` and the org default mode as
  any member. One who holds a grant on the app acts on its outbound with that grant.
- **Demo** (`CANYON_AUTH=off`): every caller is the anonymous admin, so widening is unrestricted
  there, as everything else in demo.

## The deploy repo — how releases reach the platform

Decided 2026-10-24 with `design/HAND-OFF.md` §3b (#2470). How releases reach the org's platform is a
host setting, an admin's, like where apps run; what a release waits on is an app's technical detail.

| What | Minimum | Below it |
|---|---|---|
| `GET /api/v1/platform-repo` — the repo, its branches, path, API, store and CA, who set it, the last check and the last commit; never the token | any signed-in member | — (`401` signed out) |
| `PUT /api/v1/platform-repo`, `POST …/token`, `POST …/fetch-token`, `POST …/check`; `delivery` on the preferences | admin | `403 admin_required`, before the body is read |
| `envs.<env>.pending.via` and `.pull_request`, the link to the pull request a production release waits on | contributor (technical detail), as `pending` is | absent: the reduced row has no `pending`, so a viewer and no access never learn the deploy repo's name |

- A check-in that settles a deploy is Canyon's, never a member's: its audit row's actor is
  `{kind: canyon, subject: check-in}`, and nobody's level is read for it.
- The fetch-only token is the platform's, as the host token is: it reads one release's access token and
  nothing else, and no level of anyone's is ever given by it.

## Platform patches — who decides Canyon's fixes

Decided 2026-10-18 (the owner) with `design/PATCHING.md` §4.5, §9 and §14 (decision 2): an admin approves a
platform patch for the whole fleet, from a browser only, for the platform class only. The routes are
in `contracts/routes.md` (*Patch runs*, *Production approvals*, and the patching rows under Platform);
built with PATCHING's Phase 2.

| What | Minimum | Below it |
|---|---|---|
| The *System libraries* card, `GET …/envs/:env/image`, `patched[]`, `envs.<env>.patch`, the receipt's known-vulnerable packages' `layers` and `go_stdlib` (#2579) | contributor (technical detail) | `403 team_required` naming the owners; the reduced row has none; a responder's receipt names each package without them |
| Attach an image report | contributor, as package reports | `403 team_required` |
| Decide an app's patch approval | contributor, a person, a browser session | `403 team_required` · `403 dashboard_required` · an agent never decides |
| `patching`, `rebuilds`, an identity's resume, a fleet approval, the identities read | admin; each change from a browser session | `403 admin_required` · `403 dashboard_required` with one `denied` row |
| `platform.known-vuln`'s state and source | admin; an owner may only make it stricter (D14) | as the policies rows |

- **The fleet approval is the one change to the model.** An app's patch approval is a production
  approval, its team's to decide as any is (*Promote*, above). An admin may instead approve one
  platform identity — one target run image — for production across the fleet:
  `POST /api/v1/patching/identities/:id/approve {reason}`. Each app whose preview patch passed its watch
  is then promoted by Canyon in that identity's waves, its approval decided by that admin, and the
  receipt's `prod.human-approval` reads that person. It covers Canyon's run image under the apps and
  nothing a builder put in the image, so it never covers a rebuild patch's identity, which is always the app's team's to decide (`409 always_asked`; #2117); it never expires, covers the identity's later waves, and is not
  withdrawn in v1 — an admin stops it with `patching: off`. A held production is skipped, as every
  production write is (*Security responders*); a team may still decide its own app first, and a team's
  denial stands — nothing is asked again of that app for that identity, and the fleet approval never
  decides for it; an app with a scheduled job is always its team's to decide (`design/PATCHING.md`
  §4.6). Each approval it decides goes through the approve route's checks, with the admin as the actor, and only while that person is still an admin: once they are not, each due approval stays pending for its team (#2571).
- **A patch run's author is the author of the run whose image it rebases**, for every rule that never
  lets an author approve (*Approve findings*): no one approves, through a patch Canyon started, a
  blocking finding of code they shipped.
- **Admin, then a browser.** A member is `403 admin_required`. A session that is not a browser's is
  `403 {error:"dashboard_required", message:"approve it in Canyon's dashboard"}` on the fleet approval —
  the production approvals' body, byte for byte — and `403 {error:"dashboard_required",
  message:"change it in Canyon's dashboard"}` on `patching` and a resume — the preferences row's body,
  byte for byte — each with one `denied` audit row before the body is read. With sign-in off nobody can decide a patch approval, so `patching` reads `off` there.
- **Read-only members** keep `Open preview` and `Open production`; a patch changes nothing they see. A
  responder acts on an app's patches with their grant alone, as on any app route.

## Database sessions — who reaches an app's data

Decided 2026-10-18 (the owner) with `design/DATABASE-SESSIONS.md` §5 and §17: a person reads or changes an
app's Canyon-provisioned Postgres as themselves; production data is off until an admin turns it on, and
writing it is an owner's. The routes are in `contracts/routes.md` (*Database sessions*); built with
DATABASE-SESSIONS's part 1 (`canyon db query`) and part 2 (`canyon db connect`).

Levels from the table above. Admins pass the level checks; `db_prod`, the reason, the browser rule and
the hold bind them as anyone. Data is technical detail, so `viewer` and `none` get nothing here, and see
no card.

| Action | Minimum | Also needs |
|---|---|---|
| Preview, `read` or `write` | `contributor` | — |
| Prod, `read` | `contributor` | `db_prod` ∈ `read, write`; a **reason**. Refused while it could run one of the app's `SECURITY DEFINER` functions, which run as the app and may write (design/DATABASE-SESSIONS.md §4) |
| Prod, `write` | **`owner`** | `db_prod` = `write`; a reason; the terminal's confirmation (`--yes` non-interactive); under `prod_approval`, a browser (below) |
| List an app's open database sessions | `contributor` | — |
| End one | its person · the app's owners · admins | — |
| Set `db_prod` | admin, from a browser sign-in | as `prod_approval` |
| Turn database sessions on (`db_sessions`) | admin, from a browser sign-in, a person | — |
| Turn database sessions off | admin, from any session | — |

- **`db_prod` ∈ `off | read | write`, `off` by default**, on the preferences row. A PUT from a sign-in
  whose `flow` is not `browser` is `403 dashboard_required`, so an admin's agent cannot turn production
  data on for itself. While sign-in is off it reads `off`, and a PUT of `read` or `write` is `409
  signin_required`, as `prod_approval`'s is; every open, attach and check judges the value it reads,
  so a stored `write` is `off` there.
- **A reason for every prod database session**, read included: 1–500 characters, at least one visible,
  no control, format or bidirectional character, on the audit row. It is asked when a request opens the
  database session and at the first attach of each other sign-in, which carries its own onto its `db
  session attached` row; later requests from a sign-in already on its trail, the opener's included,
  need none.
- **While `prod_approval` is on**, a prod write opened from a non-browser sign-in is `403
  dashboard_required`. The owner opens it on the app page instead, and the CLI and their agent then
  attach to it: `dashboard_required` judges only an open of a new prod write, and turning `prod_approval`
  on closes every open prod write not opened from a browser (`setting_changed`), and an attach from any of
  the person's own sign-ins passes it — never the hold or any other check. Each connection rides on the
  sign-in it came on: revoking that sign-in ends its connections within 15 s, whoever opened the
  database session.
- **Only a person.** An `agent` or `canyon` actor is `403 person_required`. An agent under a person's
  sign-in is that person (`design/AUTH.md` §4.4).
- **Ending** another's database session is its app's owners' and admins'; to anyone else it is `404`,
  as one that is not theirs. Connecting to and cancelling in a database session are its person's alone:
  an owner or an admin gets `404` there, so nobody acts as another person's role.
- **A held production** (*Security responders*): opening or attaching a prod `write` database session
  is `409 held`; a prod read goes on. An open prod write database session closes at the next check,
  `held`.
- **`db_sessions` ∈ `off | on`, `off` by default** (#2835): while `off`, nobody opens, attaches to,
  queries or connects to a database session, admins included: `409 db_sessions_off`. Turning it on widens
  who reaches app data, so it is an admin's, a person's (`403 person_required` to an agent) and, with
  sign-in on, a browser's (`403 dashboard_required`); turning it off narrows, so any of an admin's
  sessions may, and it ends every open database session at once. Members read its state, as every
  reader of the preferences does.
- **Order** (`contracts/routes.md`, *Database sessions*): `team_required`, then `person_required`, then
  `db_sessions_off`, then the body (`invalid_env`, `invalid_mode`), then `db_prod` (`409 db_prod_off`, `db_prod_read_only`), then
  `reason_required`, then `owner_required`, then `dashboard_required`, then `409 held`. The level a prod write
  needs is read from the body, so the body is judged before it; a host whose production is off says so before
  asking why (amended 2026-11-12, the v0.9.0 walkthrough).
- **A responder** on an app not shared with them gets `403 team_required` here, as on any app route;
  one with a grant acts with that grant.

## Licence — who sets it, and what it locks

Decided 2026-11-14 (the owner, #2879): Canyon is AGPL-3.0 core plus a commercial Enterprise edition, unlocked by a
licence (contracts/routes.md, *Licence*). The licence is the org's, so setting it is an **admin's**, a person's in the
dashboard, as every widening is: `PUT /api/v1/licence` is `403 admin_required` for anyone else, an owner of every app
included, `403 person_required` to an agent (`A person sets Canyon's licence, in the dashboard: Settings › Licence.`), and on a host with sign-in `403 dashboard_required` from any session
but a browser's, each with one `denied` row. Reading it is every member's, in two parts: whether a licence is in
force and which features it unlocks (`{state, allows}`), which a screen needs to show an owner that a group grant is
an Enterprise feature before they try one; and what it says — the customer, seats, dates, features and where it came
from — an admin's alone. `canyon licence` prints what the reader is answered. The Settings › Licence tab is an
admin's; a member who follows a link to it reads one line.

**The licence never changes who may do what.** It adds no level and takes none away: each Enterprise route keeps
its minimum from its own section (*Outbound*, *Platform patches*, the Access group), and the licence is checked after
that minimum and the session's refusals, so a caller below it is refused exactly as before. Without a licence that
unlocks the feature, adding it or turning it on or up is `409 licence_required` at every level, an admin's included: a
new credential, a group grant added or widened. Rotating and narrowing are never refused for the licence, nor are
removing and turning off: replacing a leaked key's value, lowering a group's level or taking a role from it, removing a
grant (amended 2026-11-14, #2881's review: a paywall never stands between an admin and a security fix).
What is set keeps working whatever the licence's state: a group grant made under a licence keeps granting after it
lapses, and one removed is not regained, so nobody loses or gains access when a licence lapses (the fail-closed rule,
decided 2026-11-14). **A server built without Canyon Enterprise** (`ee/`; contracts/routes.md, *Licence*; #2913)
changes nobody's level either: it refuses each Enterprise change `409 licence_required` at every level, as a server
with no licence does, after the same minimum and session refusals, and never refuses removing or narrowing. Its
`PUT /api/v1/licence` is `409 enterprise_absent` after the same `403`s. Of *Platform patches* it refuses a fleet
approval and an identity's resume `409 licence_required` after the same refusals, never turning `patching` or
`rebuilds` off, and an app's patch approval filed before reads `outdated`, since such a server patches nothing
(#2923).
On such a server no group grant is added or widened and no other groups claim is set, at any level (#2924). A group
grant set before keeps granting its level and a sign-in keeps reading groups, so rebuilding a server without Canyon
Enterprise changes nobody's access. An owner can still lower or remove a group's grant, and an admin can still clear the
claim.
Outbound keys' code is Canyon Enterprise's whole (#2922), so such a server also refuses replacing a credential's value
and rotating the interception root, `409 licence_required` at every level after the same refusals; removing a
credential and turning credentials off stay each caller's who may today.

## Telemetry — who sees and turns off what Canyon reports

Decided 2026-11-12 (#2807): a Canyon server sends one report of counts a day, never a name
(contracts/schema/telemetry.schema.json, TELEMETRY.md). It is on by default, as Grafana's and Coder's are, and
said in the docs, the release notes, the Helm install notes and the server's first boot log, never behind an admin
having seen it (amended 2026-11-12, the owner). The counts are the whole org's, so reading the next report and
turning it on or off are an **admin's**: `GET` and `PUT /api/v1/telemetry` are `403 admin_required` for anyone
else, an owner of every app included, who sees nothing of it. Reading the report and turning it off work from the
CLI (`canyon telemetry show`, `canyon telemetry off`) or the dashboard alike. Turning it on starts the sends again,
so it is a person's, in the dashboard, like every widening (decided 2026-11-12, #2807): on a host with
sign-in, `403 dashboard_required` from any session but a browser's, and `403 person_required` to an agent. The
server's environment (`CANYON_TELEMETRY`, `DO_NOT_TRACK`, `CI`; contracts/routes.md, *Telemetry*) turns it off whatever an admin sets, and no
route turns it back on.

## MCP — what an agent's client may do

Decided 2026-11-20 (design/specs/mcp.md §4; design/AUTH.md §4.7). **A tool is its route**: each MCP tool calls a route
the CLI already calls, at that route's level, with its refusals and its audit rows, and MCP adds no permission and
widens none. **An OAuth session is a person's session**: a client a person allowed acts as that person, recorded under
their name, and never more than they may; its scope (`read` or `act`) only narrows what it may ask, and a viewer's
`act` token still can't deploy. **Consent is a browser's**: a person allows a client in the dashboard, from a browser
session alone, so no CLI or MCP session ever hands its access to another client.

**Sessions.** Every member sees their own live sessions, each with how it signed in and an MCP client's name, and ends
any of them; an admin sees everyone's and ends anyone's, and the row names whose (`DELETE /api/v1/sessions/:id`). Ending
one is a person's, in a browser: `403 person_required` to an agent, `403 dashboard_required` from a CLI or MCP session.
A member asking to end another person's is answered `404`, as if there were none.

**Remote `/mcp`** (added 2026-11-21, design/specs/mcp.md §2). Any member whose OAuth session is live: each tool runs as
that person through the route it stands for, so a member below contributor on an app is refused `canyon_status`'s
contributor reads and every change there, as in the dashboard. `canyon_promote`, `canyon_check_ref` and
`canyon_deploy_ref` also need the session's `act` scope; with `read` alone they answer `insufficient_scope`, whatever
the person's level. `canyon_create_app` (act) is any member's, as `POST /api/v1/projects` is: they become the new app's
owner, and its link is *Link a repository*'s, their own GitHub account reading the repository; it makes at most 10 apps
a person a day, and deletes only the app it made when the link is refused. With sign-in off there is no `/mcp`: every
caller would be the anonymous admin.

## Enforcement — one decision point per app

The same code (`identity/`) decides level and roles everywhere:

- **Single host, and Canyon's own proxy (front-door choice 3):** the server checks the person's
  session and the app's grants on every request, then hands the app a signed identity token.
  Each app has its own network, so one app cannot reach another around the check.
- **Kubernetes (choices 1 and 2):** a Canyon sidecar in each app's pod verifies the signed-in
  user (from the org's gateway in choice 1, from the oauth2-proxy beside it in choice 2), checks
  the app's access file, and hands the app the token. The app's port is reachable only through it.
- **The identity token** (ES256, 5 minutes): who (`sub`, `email`, `name`), the app's `roles`, the
  app as audience. Apps verify it with `CANYON_IDENTITY_ISSUER`, `_AUDIENCE`, `_JWKS_URL` — the
  same code in every setup.
- **Acting as the signed-in user** against data sources comes after Kubernetes, choices 1 and 2
  only; Canyon's own proxy never stores the identity provider's tokens (`design/AUTH.md`).

Refusals:

- `403 team_required` — below the route's level; names the owners, and says whom to ask: the first
  owner, else a member of the owning group, else an admin (amended 2026-10-12). It is also what a responder
  gets on any app route other than the hold routes and the approver's (above), on an app not shared
  with them.
- `403 owner_required` — managing access without owning the app, saying whom to ask as
  `team_required` does; on outbound, setting an app's
  mode, `from-secret`, and replacing or deleting a route that came from it; opening a prod `write`
  database session.
- `403 responder_required` — holding or lifting production without being a responder or an admin,
  or lifting it while holding a grant on the app and not an admin; names the admins. An owner
  lifting from the CLI gets this, not `dashboard_required`.
- `403 approver_required` — approving a blocking finding without being an admin or a responder;
  names an admin, as `admin_required` does.
- `403 author_cannot_approve` — an admin or a responder approving a blocking finding of a run they
  started, a push by a GitHub account they linked included (#2228), or of a patch run Canyon started
  of code they shipped (#2098).
- `403 person_required` — an `agent` or `canyon` actor holding production, widening an app's
  outbound or dismissing its egress request, and opening a database session.
- `403 github_link_required` — while the org asks approvers of a push to link GitHub, an admin or a
  responder who linked no GitHub account approving a blocking finding of a run a GitHub push started
  (#2342).
- `403 dashboard_required` — a responder or an admin lifting a hold, or approving a blocking
  finding, from any session but a browser's; anyone widening an app's outbound, or an admin
  changing the outbound host settings, from any session but a browser's; anyone deciding an app's
  patch approval, and an admin setting `patching`, resuming a platform identity or approving one for
  the fleet, from any session but a browser's; an admin setting `db_prod`, and an owner or an admin
  opening a new prod `write` database session while `prod_approval` is on, from any session but a
  browser's.
- `409 looser_than_org` — an owner setting an app's outbound mode looser than the org's.
- `409 held` — a production write while production is held, widening the `prod` env's outbound
  and opening a prod `write` database session included; names who held it, when, why, and the admins.

The level or role is checked first, then the actor and the session, and only then the hold (a
database session reads its body before `owner_required`, since the env and mode it names set the level:
*Database sessions*), so a
caller refused for who they are never learns of a hold. Approving a blocking finding is judged once
the finding is found: the role, then the author, then a push's GitHub link (#2342), then the session. The responder codes and `409 held` land in
`contracts/routes.md` with #2061.

## Applying it — for every feature and every test plan

- A new route, screen, CLI verb or executor call that touches an app **names its minimum level**
  (from the table above) in `contracts/routes.md` or its spec, and says what someone below that
  level sees instead.
- Its tests cover **one allowed and one refused caller** at the boundary level, and that a refused
  caller learns nothing beyond name, owners and summary — and, for a responder with no grant,
  production's address, state or check-in (`seen_at`), and hold.
- A responder holds no level: a new app route admits them at their grant's level and refuses
  them `403 team_required` without one; only the fleet row, the hold routes and the approver's
  routes (*Security responders*) admit them with none. A new production writer goes through the hold's checks (`design/OPERATIONS.md` §3.4),
  never a check of its own.
- A patching feature covers each row of *Platform patches* for owner, contributor, read-only, no
  access and admin, one allowed and one refused caller each: the fleet approval, `patching` and a
  resume allowed to an admin in a browser, `dashboard_required` to an admin's CLI and `admin_required`
  to an owner.
- QA plans for any app-facing feature include the four viewpoints: owner, contributor, read-only,
  no access — plus an admin and a responder. The responder, on an app not shared with them: holds
  production with a reason from the CLI and from the fleet row; reads the fleet row's `hold` and
  `?held=true`; lifts from the fleet row in a browser; reads the runs list, a run's findings and
  SARIF and the status read, and approves a blocking finding in a browser, never of a run they
  started. They are refused every upload, check, deploy, cancel, undeploy, preview take-down,
  promotion, secret, log, share, delete and policy or setting change, and may ask for access as any
  member. A responder who also holds a grant on the
  app acts on it with the grant and cannot lift that app's hold.
- A feature that approves a finding covers, at a blocking finding: owner and contributor
  (`approver_required`), read-only and no access (`team_required`), an admin, a responder with a
  contributor grant and a responder with none (approved, from a browser; `dashboard_required` from
  the CLI), and an admin and a responder who started the run, or whose linked GitHub account pushed it
  (`author_cannot_approve`). An advisory finding is approved at
  contributor, as before.
- An outbound feature (`design/EGRESS.md`) covers the same six, one allowed and one refused caller
  each. Owner: reads; allows, adds a route and runs `from-secret` in a browser, and is refused each
  `dashboard_required` with a CLI token (a raw API call: no CLI verb widens) and `person_required`
  as an agent; removes from any session; dismisses as a person; sets a stricter mode and is refused
  a looser one (`409 looser_than_org`). Contributor: as the owner, but refused any mode,
  `from-secret` and replacing a route that came from it (`owner_required`). Read-only and no
  access: no *Outbound*, no `outbound` row, `team_required` on every egress route of the app.
  Admin: every app, either mode, the host settings, each change to them from a browser only. While
  production is held, widening `prod` is `409 held` and removing is not. Responder: on an app not
  shared with them, `team_required` and the fleet row alone; with a grant, that grant's result.
- A database-sessions feature covers the same six, one allowed and one refused caller each. Owner:
  preview and prod read and write; a new prod write `dashboard_required` from the CLI while
  `prod_approval` is on, and the CLI attaching to one opened in the dashboard; `409 held` while
  production is held; ends anyone's, and `404` connecting to another's. Contributor: preview read and
  write, prod read with a reason (`reason_required` without), prod write `owner_required`; ends only
  their own, `404` on another's. Read-only and no access: no card, `team_required` on the app's routes
  and `404` on a database session's own. Admin: every app, bound by `db_prod`, the reason, the browser
  rule and the hold; sets `db_prod` from a browser, `dashboard_required` from the CLI. Responder: on an
  app not shared with them, `team_required`; with a grant, that grant's result. An agent actor:
  `person_required`. Revoking a CLI sign-in that attached ends its connections, whoever opened the
  database session.
- A change to this model is a change to this file first, as the first commit of the PR that builds it
  (AGENTS.md, rule 5).
