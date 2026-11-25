// The literals design/EXPERIENCE.md pins: D5's strings and the fleet tiles' sub-lines. They are
// the product surface, byte for byte: a paraphrase here or there is the red, and changing a D5
// string means changing README, CLI, UI and the hero GIF together. strings.test.ts asserts every value below
// appears verbatim in EXPERIENCE.md.
export const S = {
  verdictBlocked: "✗ Blocked — 1 blocking finding",
  verdictNotChecked: "✓ Nothing blocking in what was checked",
  verdictApproved: "! Approved — 1 blocking finding, approved by <who> until <date>",
  verdictApprovedEach: "! Approved — <n> blocking findings, each approved",
  nothingBuilt: "Nothing was built and nothing was deployed.",
  // A deploy run no buildpack on the host detected (contracts/events.md, failure_reason detect).
  undetected: "This host can't build this kind of app.",
  // Telemetry (#2807): Settings › Telemetry, on by default (the owner, 2026-11-12). The off lines say what the
  // CLI's say (design/specs/cli.md §6.12), shorter: an admin reading them is already in the dashboard.
  telemetryFields: "What each field is",
  telemetryTurnOff: "Turn off",
  telemetryTurnOn: "Turn on",
  telemetryOn: "Telemetry is on: Canyon sends one report a day.",
  telemetryOffDone: "Telemetry is off: Canyon sends nothing.",
  telemetryOffEnv: "Telemetry is off: this server's environment turns it off (CANYON_TELEMETRY, DO_NOT_TRACK or CI).",
  telemetryOffAdmin: "Telemetry is off: an admin turned it off.",
  telemetryOffUnreachable: "Telemetry is off: it couldn't reach Canyon's collector.",
  telemetryRetrying: "Telemetry is on, but no report has reached Canyon's collector yet. Canyon tries again after <when>, and gives up by <day>.",
  telemetryNext: "The next report, exactly as it is sent:",
  telemetryLast: "The last report sent, <when>:",
  telemetryNotSent: "Nothing has been sent yet.",
  telemetryAdmin: "Only an admin can see and change the report Canyon sends.",
  // The Enterprise licence (#2879; contracts/routes.md, Licence): Settings › Licence, an admin's, and the badge and line
  // in place of an Enterprise feature's add or change while no licence in force unlocks it.
  enterprise: "Enterprise",
  licenceRequiredOutbound: "Adding credentials to apps' calls needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
  licenceRequiredPatching: "Turning on patching needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
  licenceRequiredGroups: "Access by sign-in group needs Canyon Enterprise. What's set keeps working; to change it, an admin adds a licence in Settings › Licence.",
  licenceNone: "No licence: this Canyon runs the free edition. Enterprise features need a licence.",
  licenceValid: "Canyon Enterprise, licensed to <customer> until <date>.",
  licenceGrace: "Canyon Enterprise, licensed to <customer>, ended on <date>. Its features stay unlocked until <grace>; after that, what's set keeps working and can't be changed.",
  licenceLapsed: "Canyon Enterprise, licensed to <customer>, ended on <date>. What's set keeps working; Enterprise features can't be changed until an admin adds a new licence.",
  licenceUnreadable: "Canyon can't read this server's licence. What's set keeps working; changing an Enterprise feature needs a licence Canyon can read.",
  licenceUnsigned: "Canyon didn't sign this server's licence. What's set keeps working; changing an Enterprise feature needs a licence Canyon signed.",
  licenceSeats: "Seats",
  licenceValidUntil: "Valid until",
  licenceFeatures: "Features",
  featureOutbound: "Credentials Canyon adds",
  featurePatching: "Patching",
  featureGroups: "Access by sign-in group",
  licencePaste: "Paste a licence",
  licenceSave: "Save licence",
  licenceFromEnv: "CANYON_LICENSE sets this server's licence, so it can't be changed in the dashboard.",
  licenceAdmin: "Only an admin can see and change the licence.",
  // The fleet's Needs attention, an admin's, for the licence in grace, lapsed or invalid (org_needs, licence), and for
  // patching the server is set to do that no licence unlocks (patching_unlicensed; #2881).
  licenceNeedGrace: "Canyon's Enterprise licence ended on <date>. Its features stay unlocked until <grace>.",
  licenceNeedLapsed: "Canyon's Enterprise licence has lapsed. What's set keeps working, but Enterprise features can't be changed.",
  licenceNeedInvalid: "Canyon can't use its Enterprise licence. What's set keeps working, but Enterprise features can't be changed.",
  licenceNeedPatching: "This server is set to patch apps, but patching needs Canyon Enterprise, so Canyon patches nothing.",
  licenceNeedOpen: "Open Settings › Licence",
  // The rail's last line: Canyon's source, the offer AGPL-3.0 section 13 asks a modified Canyon to make (#2878).
  sourceCode: "Source code",
  undetectedBuilds: "Canyon builds <kinds>.",
  // The kinds of app a host's builder makes, by its buildpacks' own names (#102): the languages, then the rest.
  buildsApps: "<languages> apps",
  buildsStatic: "static sites",
  buildsProcfile: "apps with a Procfile",
  undetectedNone: "Its builder doesn't say what it builds.",
  undetectedErrored: "A buildpack also errored while checking. If this app is meant to be one of those stacks, check that its manifest is valid (for example package.json).",
  neverReviewed: "Never reviewed. Canyon can't tell you whether this app is safe.",
  restartedWayOut: "Deploy again once it's fixed: the build is reused.",
  // Said after restartedWayOut only where the app's last lines aren't shown beside it (#2843).
  restartedHasWhy: " canyon logs --env <env> has why.",
  restartedLastExit: "It last exited with code <code>.<below> Deploy again once it's fixed: the build is reused. canyon logs --env <env> has the rest.",
  // A restarting environment's last log lines under the app page's band, masked (#2744); the second when a secret of
  // the environment changed since what runs there started, the third when the read fails.
  lastLines: "The app's last lines, each secret's value replaced by its name:",
  // lastLinesCli follows each to the app's team, whose canyon logs reads it raw; never to a responder, whose read is masked.
  lastLinesUnmasked: "Canyon can't show the app's last lines here: it can't tell which secret values the app started with, so it can't hide them.",
  lastLinesTooLong: "Canyon can't show the app's last lines here: they're over 1 MiB, too much to check for secrets.",
  lastLinesUnread: "Canyon couldn't read the app's last lines.",
  lastLinesCli: " canyon logs --env <env> shows them.",
  // An environment's logs on its card (A of incidents and logs): its last 200 lines, masked, read again on Refresh, never followed.
  showLogs: "Show logs",
  logsOf: "<app>'s logs in <env>",
  // What masked hides, said plainly (#223's security review).
  logsLines: "The last 200 lines. Only the secrets set in Canyon are hidden, each shown as its name: anything else the app printed shows, its users' tokens included.",
  // A security responder's read where a value is too short to hide by its name (409 too_short_to_mask).
  logsTooShort: "Canyon can't show you the app's last lines: a secret in them is too short to hide by its name.",
  logsReading: "Reading…",
  logsNone: "No log lines yet.",
  logsCopied: "Copied.",
  logsUncopied: "Couldn't copy: select the lines and copy them yourself.",
  logsTeamOnly: "Only the app's contributors see its logs.",
  signInForCli: "Sign in to approve the Canyon CLI",
  signInForCliLine: "Then you'll come back here to approve the code from your terminal.",
  signInForAgent: "Sign in to connect an agent to Canyon",
  signInForAgentLine: "Then you'll come back here to allow it.",
  // The mark beside an app nobody has reviewed: the fleet row and the owner card carry it.
  neverReviewedMark: "Never reviewed",
  provenance: "<version> · passed review",
  provenanceApproved: "<version> · shipped with approved problems",
  // Who approves a blocking finding, in place of its button for anyone else (design/PERMISSIONS.md, #2204).
  approvers: "Only an admin or a security responder can approve a blocking finding, and never their own.",
  // A push's blocking finding, to a reader who would approve it but linked no GitHub account while the org asks it (#2342).
  pushApproversLink: "<org> asks whoever approves a push's blocking findings to link their GitHub account first.",
  // In place of the link while the host can't link an account (#2351): no GitHub App with a client secret.
  pushApproversCantLink: "This Canyon can't link GitHub accounts yet: an admin sets it up in <settings>.",
  // How a custom rule's version is checked, on its card, and the two ways an admin switches it (#2639).
  checkedByCode: "Checked by code: <says>",
  // A finding's step after its fix where its custom rule points at one of the org's playbooks (contracts/finding.md,
  // canyon.playbook): the finding's card and its Copy for your agent, as canyon check --ai prints it.
  playbookStep: "Read the playbook <name>: canyon playbook <slug>",
  // Guardrails › Playbooks (design/specs/screens.md; contracts/routes.md, the playbooks row), an admin's.
  playbooksIntro: "How your org builds, in its own words. Every coding agent is told each playbook's name and when to read it, and reads the ones that fit its task. Canyon never checks a playbook: a rule does that.",
  playbooksNone: "No playbooks yet.",
  playbookSkill: "Skill",
  playbookNote: "Note",
  playbookSkillMeans: "How to do a kind of task, such as adding a route.",
  playbookNoteMeans: "Something to know, such as how the database fails over.",
  playbookWhen: "When to read it",
  playbookRemoveAsk: "Remove <name>? Agents stop being handed it. Its versions are kept.",
  playbookImport: "Import a SKILL.md",
  playbookImportHow: "Its name becomes the playbook's name and slug, its description when to read it, and the rest the body. Nothing is saved until you save it.",
  playbookImportNoFrontmatter: "This isn't a SKILL.md: it needs a frontmatter block between two --- lines.",
  playbookImportNoName: "The SKILL.md's frontmatter needs a name and a description.",
  playbookRestore: "Restore this version",
  // Each playbook's two counts on its card, and the line under the tab's that says what the second can't see (§3.5).
  playbookReads: "<n> reads in 30 days",
  playbookMachines: "on <n> machines",
  playbookCountsMean: "Canyon can't see an agent reading its local copy of a skill: \"on N machines\" says where the current version is, not how often it's read.",
  // The custom rule editor's playbook field and the rule's line naming it (contracts/routes.md, the custom-rules row).
  rulePlaybook: "Playbook",
  rulePlaybookMeans: "A finding of this rule tells the agent to read this playbook after its fix.",
  rulePlaybookLine: "Its findings tell the agent to read the playbook <slug>.",
  checkedByAI: "Checked by AI",
  checkWithAI: "Check it with the AI instead",
  checkByCode: "Check it by code where Canyon can",
  // A dry run of a rule checked by code on the org's apps (#2640): asked after a save, and on demand.
  dryRun: "Try it on your apps",
  dryRunning: "Checking it on your apps…",
  dryRunPass: "<app>: Would pass",
  dryRunFail: "<app>: Would fail",
  dryRunNotChecked: "<app>: Not checked: <why>",
  // Guardrails → Policies' row (#2342): not a policy, who approves.
  pushApproversTitle: "Approvers of a push must link GitHub",
  pushApproversMeaning: "A blocking finding of a GitHub push is approved only by an admin or a security responder who linked their GitHub account, so nobody approves their own push.",
  cleanGate: "<org> requires a passing review to reach production.",
  // Under platform.known-vuln's go-live row: the packages it counts, each advisory with what fixes it (#2300).
  knownVulnPackages: "See the <n> packages",
  knownVulnPackage: "See the package",
  fixedIn: "<id> fixed in <fixed>",
  // The chip on a feature that works but isn't complete or fully tested yet, its explanation the title (#2384).
  beta: "Beta",
  betaWhy: "This works, but it isn't complete or fully tested yet. Tell us what you find.",
  bannerLoopback:
    "No auth in demo mode. Anyone who can reach this port is an admin — bound to localhost only.",
  bannerExposed: "This Canyon is reachable from the network and has no authentication.",
  promoteConfirm: "Production runs the same image as preview, with production's own secrets. Nothing is rebuilt.",
  // What the promotion's question says first: the app, its version, and what that replaces (#145).
  promoteReplaces: "Promotes <app> <version>, replacing <old>.",
  promoteFirst: "Promotes <app> <version>. Nothing runs in production.",
  promoteAnyway: "Promote <version> anyway",
  inProduction: "<version> is in production.",
  promoted: "Promotion of <app> started. Production shows the new version here once it is live.",
  // Putting an earlier version back in production (design/OPERATIONS.md §1.7). The code line names two
  // versions with one placeholder: the one put back first, then the one it replaces.
  putBackReviewing: "Reviewing <version> again under today's rules. Preview is not touched.",
  putBack: "Putting <version> back in production.",
  putBackReplaces: "It replaces <version>.",
  putBackNothing: "Nothing runs in production.",
  putBackNotThat: "<version>'s review doesn't name the image production ran, so nothing was promoted",
  putBackImage: "Production gets the image it ran on <date>.",
  putBackCode: "Canyon puts back the code, not the data, the secrets or the outbound rules: <version> runs against production as <version> left it.",
  putBackCodeBare: "Canyon puts back the code, not the data, the secrets or the outbound rules.",
  // The headline of a production that's down (#188): the newest kept earlier version, or why none can go back.
  putBackNamed: "Put <version> back",
  putBackNone: "No earlier version can be put back as it ran: fix it, deploy to preview, then promote.",
  imageGone: "<version>'s image is no longer on this Canyon, so it can't be put back as it ran. Deploy <version> to preview and promote it if you want it rebuilt.",
  patched: "<version> · patched <date>",
  noDescription: "No description yet",
  // The app page under its title until its first reads answer (#2561).
  appReading: "Reading the app…",
  reviewReading: "Reading the review…",
  // Guardrails → Scanners: the tools Canyon runs itself (contracts/finding.md, Canyon-run tools).
  scanners: "Canyon runs these tools itself on every review, on this host.",
  runsWhile: "Canyon runs it only while its findings are advisory or blocking.",
  trivyDefault: "Leave all unticked for vulnerabilities and misconfiguration.",
  skipBlinds: "A skipped path is never scanned: a pattern like ** skips everything, and the tool finds nothing.",
  bundledRules: "Canyon's open-source security rules (GitLab's sast-rules) and its own SQL rule always run, beside any rules repo named here.",
  rulesFolders: "Only the rules files in these folders of the repo are read; none, and every rules file is.",
  communityRules: "The community rules add Ruby, PHP and Java SQL injection. They carry the Commons Clause, so whether your organization may use them is its decision; Canyon does not ship them.",
  credentialAsk: "A private rules repo is read with the username and token set here. Canyon never shows the token again.",
  credentialSet: "A credential is set for <repo>.",
  // Who can open an app (design/PERMISSIONS.md). `…` is what follows "Shared with" in the shared line.
  private: "Private: only its owners and admins can open it.",
  sharedAsk: "Shared with … · Ask <owner> or an admin to change it.",
  sharedAskNoOwner: "Shared with … · Ask a Canyon admin to change it.",
  sharedAskGroup: "Shared with … · Ask a member of the <group> group or an admin to change it.",
  roles: "Roles are what this app lets a person do: its canyon.toml declares them, and the app is told a person's roles when they open it.",
  noAccess: "You don't have access to <app>.",
  // An app this Canyon has none of, and the way back from it or from an app the reader can't open (#102).
  noApp: "There's no app called <app> on this Canyon.",
  backToFleet: "Back to the fleet",
  // Sharing an app: what the box takes, and a group's grant by its name, never a prefix (#102).
  shareWithHint: "An email address, a group or Everyone at <org>",
  shareWithHintNoGroups: "An email address or Everyone at <org>",
  group: "group",
  groupNamed: "the group <group>",
  noAccessOwner: "<owner> owns <app>. Ask them or a Canyon admin for access.",
  noAccessNoOwner: "Ask a Canyon admin.",
  noAccessGroup: "Members of the <group> group own <app>. Ask one of them or a Canyon admin for access.",
  accessNow: "You have access to <app> now. Open its link again: it can take up to a minute to reach it.",
  askForAccess: "Ask for access",
  // A read-only member's ask: to join the app's team, which changes it (#2774).
  askToJoin: "Ask to join its team",
  readOnly: "Your access: read-only.",
  // A security responder's band on an app not shared with them: what the role does there (#2204).
  responderAccess: "Your access: security responder. You can approve its blocking findings and hold its production.",
  // A held production (design/OPERATIONS.md §3.7, §3.8): the banner above production on the app page and
  // the disabled Promote's reason; on the fleet the row's mark, Stop production and Lift the hold, the
  // stop's question and lines as the CLI asks them, and the lift's two choices.
  held: "Production was stopped by <who> on <date>: <reason>. It stays stopped until a responder or an admin lets it run again.",
  heldTitle: "Production is held",
  heldMark: "Held",
  // The go-live box while held (#2723): beside the version the hold stopped, in place of Promote; else Promote's reason.
  heldVersion: "<version> is production's version, stopped by the hold. Lifting the hold can start it again.",
  heldNoPromote: "Production is held: nothing is promoted until a responder or an admin lifts the hold.",
  // The fleet's side pane's production card while held, under the band that says the whole hold (#2723).
  heldSince: "Held · since <date>",
  heldFilter: "Only held apps",
  stopProduction: "Stop production",
  stopAsk: "Stop and hold <app> in production?",
  stopping: "Stopping <app> in production and holding it: nobody can promote it until you or an admin let it run again.",
  stopPreview: "Its team can still deploy to preview.",
  // Each of the two ways to take production down names the other (#102).
  stopOrOffline: "To take it offline until the next promotion, use Take production offline.",
  offlineOrStop: "To stop it and block every promotion during an incident, use Stop production.",
  liftHold: "Lift the hold",
  liftAsk: "Lift the hold on <app>?",
  // The lift's two answers, wherever the hold is lifted: the app page, the fleet and its pane (#102).
  liftKeepStopped: "Lift the hold, keep it stopped",
  liftAndStart: "Lift the hold and start <version>",
  // A stop whose answer never came, the network's failure and not the server's (#145).
  stopUnknown: "Canyon's answer didn't arrive, so whether production stopped isn't known. Asking again stops it if it runs.",
  holdChanged: "production's hold changed since you read it: read it again",
  // The same hold where production runs nothing (#2696): nothing stops, so the row, its question and its
  // confirm say a hold, and a hold of a production that ran nothing says held, never stopped.
  holdProduction: "Hold production",
  holdAsk: "Hold <app>'s production?",
  holdNothing: "Nothing runs in production. A hold stops every promotion until a responder or an admin lifts it.",
  heldNothing: "Production was held by <who> on <date>: <reason>. It stays held until a responder or an admin lifts it.",
  // The newest version's review when the version uploaded just before it was blocked (#2696): fixed only where its
  // review proves it, else passed, and never passed where it left something unchecked.
  earlierFixed: "Blocked earlier, fixed in <version>",
  earlierPassed: "Blocked earlier, passed in <version>",
  earlierNotChecked: "Blocked earlier, nothing blocking in <version>",
  earlierFixedOne: "<earlier> had 1 blocking finding, and it isn't in <version>'s review.",
  earlierFixedMany: "<earlier> had <n> blocking findings, and <version>'s review has none of them.",
  earlierOne: "<earlier> had 1 blocking finding.",
  earlierMany: "<earlier> had <n> blocking findings.",
  earlierPassedTail: "<version> passed review.",
  earlierNotCheckedTail: "<version> had nothing blocking in what was checked.",
  earlierSee: "See <earlier>'s review →",
  // Who acted, on a host with sign-in off: its one caller, the anonymous admin (contracts/routes.md, `person / anonymous`
  // with no display). Nobody signed in, so the screens say that, never the subject as if it were a name (#2696).
  notSignedIn: "someone not signed in",
  // An upload forced past stale_base (detail.forced): the copy recorded another base, or none, so it replaced the app's
  // current version without starting from it (contracts/routes.md, the packages row; cli.md §3.6), after the upload's words.
  auditForced: ", replacing a version it didn't start from",
  // Settings › People & sign-in, above who the security responders are (design/OPERATIONS.md §3.5).
  responders: "Security responders can stop any app's production and approve its blocking findings. They never deploy, promote or change secrets.",
  asked: "Asked. <owner> will see your request.",
  askedNoOwner: "Asked. A Canyon admin will see your request.",
  askedGroup: "Asked. Members of the <group> group will see your request.",
  // Settings › App hosting (contracts/routes.md, /api/v1/front-door).
  frontDoor: "The front door is how people get to an app on this Canyon: the address it answers at, and who signs them in before the app sees them.",
  // A pull request's version on preview (#1947).
  prDeploy: "Deploy this pull request to preview",
  prPreview: "Preview runs a pull request's version. Merge it, then deploy main.",
  githubConnect: "Connect a GitHub organization",
  githubConnectNote: "Canyon registers its own App on the organization. The App reviews pushes and pull requests, and can bring repositories in as apps.",
  githubOnly: "This organization holds only Canyon apps",
  githubOnlyNote: "Recommended for code Canyon keeps. New repositories here become apps automatically.",
  githubOther: "It also holds other code",
  githubOtherNote: "Nothing is imported until an admin picks repositories.",
  githubKeep: "Keep code for apps that start in Canyon here",
  githubKeepNote: "Canyon may create repositories here and commit to them. Without this, its App only reads.",
  githubKeeper: "<org> already keeps Canyon's code.",
  githubRegister: "Register Canyon on <org>",
  githubRegisterNote: "You need to be an owner of the organization on GitHub, or an App manager there. GitHub opens, you confirm, then pick which repositories the App sees.",
  githubUnreachable: "GitHub can't send events to localhost. Connect GitHub from a Canyon that has a public address.",
  githubConnected: "Connected organizations",
  // An App registered before Canyon kept its client secret links no GitHub accounts until an admin supplies one (#2228).
  githubSecretTitle: "Let people link their GitHub accounts on <org>",
  githubSecretNote: "Canyon got this App before it kept client secrets. On GitHub, open the App's settings, add <callback> as a Callback URL, choose Generate a new client secret, and paste it here.",
  githubSecretSave: "Save client secret",
  githubRepositories: "Repositories",
  githubImportsWarning: "Every new repository in <org> becomes a Canyon app. Turn this on only if <org> holds Canyon apps alone.",
  autoDeploy: "Auto-deploy pushes to preview",
  autoDeployNote: "A push to the default branch deploys to preview once its review passes. Production is always a person's call.",
  autoDeployOn: "Pushes deploy to preview",
  githubImportNote: "Making an app links it and reviews the default branch's newest commit. Nothing is deployed. The app arrives with no owner, and an admin assigns one.",
  githubUnanswered: "GitHub didn't answer for <org>, so its repositories aren't listed. Try again in a minute.",
  sourceKept: "Kept in Canyon only",
  // Linking the app to a repository from its page, for its contributors (design/PERMISSIONS.md, linking a repository; #1917).
  sourceLink: "Link a repository",
  sourceLinkName: "Repository",
  sourceLinkFolder: "Folder, if the app is one folder of it",
  sourceLinkNote: "Your own GitHub account must be able to read it.",
  sourceLinkYes: "Link",
  sourceLinkEmpty: "Type the repository as owner/name.",
  // Kept in GitHub (#1926), and handing it to developers (#1938).
  sourceKeptBy: "Kept by Canyon in",
  sourcePending: "Kept by Canyon in GitHub · the repository isn't made yet",
  handOver: "Developers push from now on",
  handOverConfirm: "Hand <repo> to developers? Canyon stops committing this app's versions there. This can't be undone.",
  handOverYes: "Hand it over",
  handOverNote: "Canyon stops committing this app's versions there. The repository, its history and its link stay, and each push becomes a version.",
  sourceDeleted: "<repo> was deleted on GitHub. Every version is still in Canyon.",
  sourceArchived: "<repo> is archived on GitHub, so it's read-only. Unarchive it there, or unlink it.",
  sourceRemoved: "Canyon's App can't reach <repo> any more. Add it back under the App's repository access on GitHub.",
  frontDoorDocker: "This Canyon runs apps on Docker and serves them itself, so there's nothing to set here.",
  // Settings › People & sign-in on a host with sign-in off, and Settings › AI review.
  signInOff: "Sign-in is off on this Canyon, so anyone who can reach it can open every app and change every setting. Whoever installed Canyon turns it on.",
  aiOn: "AI review is on: a model is set up on this Canyon.",
  aiOff: "AI review is off: no model is set up on this Canyon. Scanners still run.",
  frontDoorAdmin: "Only an admin can change the front door.",
  // Settings › App hosting, an admin's: a scanner's data a day before it turns stale, or once it has (D5; #2743).
  scannerStaleSoon: "<data> goes stale in <left>, and reviews can't check with <tool> after that. Canyon couldn't refresh it: its logs say why.",
  scannerStale: "<data> is over 5 days old, so reviews can't check with <tool> until Canyon refreshes it. Canyon's logs say why it couldn't.",
  githubAdmin: "Only an admin can change how Canyon connects to GitHub.",
  // Settings › Integrations, an admin's (contracts/routes.md, the channels row; #2725).
  integrationsAdmin: "Only an admin can see and change the channels Canyon posts to.",
  integrationsEveryone: "Everyone in this channel will see your apps' names and what production can't reach.",
  integrationsNote: "Canyon posts to these channels when something in production needs a person, and again when it no longer does.",
  integrationsNone: "No channels yet.",
  integrationsAdd: "Add a channel",
  integrationsSlackHow: "In Slack, add an incoming webhook to the channel and paste its URL here.",
  integrationsTeamsHow: "In the channel, choose More options (…) → Workflows → \"Send webhook alerts to a channel\", then copy its URL.",
  integrationsKeep: "Leave it empty to keep the current URL.",
  integrationsSaveNote: "Saving posts a summary of what needs a person now to the channel. If that post fails, nothing is saved.",
  integrationsSaved: "Saved. <channel> got a summary of what needs a person now.",
  integrationsTest: "Send a test message",
  integrationsTested: "Sent <channel> a summary of what needs a person now.",
  integrationsRemoved: "Removed <channel>. Canyon no longer posts there.",
  integrationsFailing: "Canyon stopped posting here <age>: the webhook URL answered <status>, so it no longer works. Change it to a working one.",
  integrationsFull: "Canyon posts to at most 5 channels. Remove one first.",
  // Settings › Outbound, an admin's (design/EGRESS.md §8.3; #2099).
  outboundAdmin: "Only an admin can change how apps call out.",
  via: "How apps call out",
  viaOff: "Off",
  viaOffNote: "Apps call out directly.",
  // Under Off, the apps whose network predates it, which still go through Canyon until it is recreated (#2723).
  viaOffStillOne: "<app> still goes through Canyon until you recreate its network below.",
  viaOffStill: "<apps> still go through Canyon until you recreate their networks below.",
  // Kubernetes, which has no network to recreate: an app keeps the policy it was deployed under until its next deploy.
  viaOffKube: "Apps deployed while this was Through Canyon still go through it until their next deploy.",
  viaCanyon: "Through Canyon",
  viaCanyonNote: "Apps call out through Canyon's egress gateway, which records each destination.",
  recreateHeading: "Networks to recreate",
  recreateNote: "These apps still run on their old network. Recreating one restarts it, production included, on the network this setting gives it. If that fails, the app stays stopped until you recreate it again.",
  recreateAsk: "Recreate <app>'s network? Its production and preview restart on the network this setting gives it. If that fails, they stay stopped until you recreate it again.",
  // The same question naming only the environment Canyon runs of the app, or none while its read is unanswered (#2774).
  recreateAskPreview: "Recreate <app>'s network? Its preview restarts on the network this setting gives it. If that fails, it stays stopped until you recreate it again.",
  recreateAskProd: "Recreate <app>'s network? Its production restarts on the network this setting gives it. If that fails, it stays stopped until you recreate it again.",
  recreateAskUnread: "Recreate <app>'s network? Each of its environments that Canyon runs restarts on the network this setting gives it. If that fails, they stay stopped until you recreate it again.",
  recreateYes: "Recreate the network",
  recreating: "Recreating <app>'s network…",
  // The org's mode, below how apps call out, and an app env's on its Outbound card (#2422).
  mode: "What apps may call",
  modeObserve: "Observe",
  modeObserveNote: "Canyon records each call. Public host names on port 80 or 443 get through, up to a limit on new destinations a minute.",
  modeAllowlist: "Allowlist",
  modeAllowlistNote: "Only destinations on an app environment's allowlist, and Canyon's own sign-in hosts, get through. Canyon refuses every other call.",
  modeFollowed: "Each app environment follows this unless its owner made it stricter.",
  // Credentials, the API's credential_routes (#2143; contracts/routes.md, the egress setting row): whether Canyon adds a credential a team
  // sets for a destination to apps' calls, an admin's to turn, off until then.
  routes: "Credentials",
  routesOff: "Off",
  routesOffNote: "Canyon adds no credential to apps' calls, and no one can set one.",
  routesOn: "On",
  routesAdd: "Add credentials to apps' calls",
  routesOnNote: "Canyon adds the credential an app's team sets for a destination to the app's calls there, so the app never holds it. To add it, Canyon decrypts calls to that destination alone, so each app trusts Canyon's certificate from its next deploy or promotion.",
  routesTurningOff: "Canyon stops adding credentials at once, so calls that need one will fail. Apps keep trusting its certificate until their next deploy or promotion.",
  routesUnread: "Canyon couldn't read whether it adds credentials. Reload the page to try again.",
  routesSaved: "Saved.",
  modeViaOff: "Turn on Through Canyon first: the mode applies only to calls that go through Canyon.",
  modeNow: "This applies to running apps within seconds, not at their next deploy.",
  modeRefuses: "Allowlist would refuse <n> destinations apps called in the last 30 days:",
  modeRefusesOne: "Allowlist would refuse 1 destination apps called in the last 30 days:",
  modeRefusesNone: "No app called anything in the last 30 days that allowlist would refuse. A call to a new destination will be refused.",
  modeUnread: "Canyon couldn't read what apps call, so it can't say what allowlist would refuse.",
  modeWiden: "Every app environment that follows the org's mode goes back to Observe.",
  modeAsk: "Refuse <n> destinations now? Apps that call them get a 403 from Canyon until they're allowed or you switch back to Observe.",
  modeAskOne: "Refuse 1 destination now? Apps that call it get a 403 from Canyon until it's allowed or you switch back to Observe.",
  modeAskNone: "Switch to allowlist now? Apps get a 403 from Canyon for any destination not on their allowlist.",
  modeYes: "Switch to allowlist",
  modeSaved: "Saved. Running apps follow it within seconds.",
  // An outbound rule change saved while a gateway replica hadn't taken it (contracts/routes.md, Canyon-Egress-Pending;
  // #2789), in place of the change's own confirmation: a widening or a narrowing alike.
  gatewayPending: "Saved, but one of Canyon's gateways (<replica>) hasn't taken it yet. Until it does, it may still let calls through or refuse them as before this change.",
  gatewaysUnread: "Saved, but Canyon couldn't confirm every gateway has taken it. Until they all do, one may still let calls through or refuse them as before this change.",
  modeHeld: "Not saved. A held production can't be given more than it may call now: lift its hold, or give that app's production a mode of its own, then save again.",
  viaHeld: "Not saved. A held production can't start calling out directly: lift its hold, then save again.",
  modeOrgs: "<mode>, as the org sets it",
  modeOwn: "<mode>, set for this app",
  modeFollow: "Same as the org: <mode>",
  modeOwner: "Only an owner or an admin can change this environment's mode.",
  envModeAsk: "Refuse <n> destinations in <env> now? Its calls to them get a 403 from Canyon until they're allowed.",
  envModeAskOne: "Refuse 1 destination in <env> now? Its calls to it get a 403 from Canyon until it's allowed.",
  envModeAskNone: "Switch <env> to allowlist now? It gets a 403 from Canyon for any destination not on its allowlist.",
  // Through Canyon saved while the org's mode is allowlist starts refusing too, at each app's next deploy (#2422's review).
  viaUnderAllowlist: "The org's mode is Allowlist, so once apps' calls go through Canyon, each app environment that follows it gets only what its allowlist allows.",
  viaUnseen: "While calls don't go through Canyon it records none, so it can't say all that allowlist will refuse.",
  viaAsk: "Send calls through Canyon under allowlist? From each app's next deploy, Canyon refuses every destination not on its allowlist.",
  viaYes: "Turn on Through Canyon",
  allow: "Allow",
  allowed: "Allowed <destination>",
  allowlist: "Allowlist: <entries>",
  // Each allowlist entry's Remove, a narrowing, and the line once it is gone (#2561).
  remove: "Remove",
  removed: "Removed <destination>",
  refusedOnce: "Not allowed once, <age>",
  refusedMany: "Not allowed <n> times, last <age>",
  dismiss: "Dismiss",
  dismissedMark: "Dismissed",
  dismissedOne: "Dismissed <destination>",
  // The app page's headline while production can't reach a destination nobody has allowed yet, in place of live (#2723).
  cantReach: "<version> can't reach <destination> in production",
  cantReachMore: " · and <n> more",
  cantReachWhy: "Nobody has allowed <destination> in production yet, so Canyon refuses the app's calls to it.",
  openOutbound: "Open Outbound",
  // The promote question while prod.egress-allowed names what production doesn't allow and doesn't stop it (#2723).
  willRefuse: "Production will refuse <destinations> once <version> goes live.",
  // A clean review's heading when a finding of it blocks production only (#2723).
  blocksProduction: "nothing blocking preview · <n> blocking production",
  // A check that couldn't finish for the host's sake, where a review again fails the same way (#2723).
  reviewWontHelpDatabase: "Reviewing it again won't help until whoever installed Canyon updates <tool>'s vulnerability database. Until then, an admin or a security responder who didn't start this review can approve it with a reason.",
  reviewWontHelpImage: "Reviewing it again won't help until whoever installed Canyon gives this host <tool>'s image, or an admin turns <tool> off in Guardrails → Scanners. Until then, an admin or a security responder who didn't start this review can approve it with a reason.",
  reviewWontHelpRules: "Reviewing it again won't help until whoever installed Canyon fixes <tool>'s rules on this host. Until then, an admin or a security responder who didn't start this review can approve it with a reason.",
  reviewWontHelpTool: "Reviewing it again won't help until whoever installed Canyon fixes <tool> on this host. Until then, an admin or a security responder who didn't start this review can approve it with a reason.",
  reviewWontHelpOverCap: "Reviewing it again won't help: <tool> says more about this code than Canyon reads. An admin or a security responder who didn't start this review can approve it with a reason.",
  // A row's last result where no allowlist can let the call through, which every mode refuses (#2723).
  refusedAddress: "refused: an IP address",
  refusedPort: "refused: port <port>",
  // A call whose host name Canyon couldn't read (egress.schema.json's invalid), and how the destination answered, by
  // its status class (#102).
  noHostName: "a call with no valid host name, port <port>",
  answered2xx: "answered (2xx)",
  answered3xx: "redirected (3xx)",
  answered4xx: "refused by the destination (4xx)",
  answered5xx: "failed at the destination (5xx)",
  // Under an env's table holding one of those rows: prod.egress-allowed's how-to-fix (gate/policy/canonical.go), byte for byte.
  egressHowToFix: "A person allows each host name in Canyon's dashboard; anything else has to change in the code.",
  // The Outbound card's own line under its heading while the app's calls may go around Canyon, by the host's runtime (#2723).
  floorCard: "This app's calls may skip Canyon, so Canyon may not record or refuse them. An admin can recreate its network in Settings › Outbound.",
  floorCardKube: "This app's calls may skip Canyon, so Canyon may not record or refuse them. Its next deploy or promotion fixes this.",
  floorCardUnread: "This app's calls may skip Canyon, so Canyon may not record or refuse them.",
  // The same line to an admin, who recreates it: Settings › Outbound is a link (#2774).
  floorCardAdmin: "This app's calls may skip Canyon, so Canyon may not record or refuse them. Recreate its network in Settings › Outbound.",
  // The card under Off while the app's network still sends its calls through Canyon, as Settings › Outbound says (#2774):
  // an admin's, with the link; anyone else's; and the note beside the 30 days' destinations.
  offStillThrough: "Outbound is Off, but this app's calls still go through Canyon until you recreate its network in Settings › Outbound.",
  offStillThroughAsk: "Outbound is Off, but this app's calls still go through Canyon until its network is recreated. Ask an admin to recreate it.",
  outboundOffNote: "Outbound is Off",
  prodRefuses: "Preview connected to these in the last 30 days, and production doesn't allow them:",
  // Settings › Outbound's fold over the limits, the gateway replicas and the certificates (#118).
  advanced: "Advanced",
  advancedWhat: "Limits, gateway replicas and credential certificates",
  replicasHeading: "Gateway replicas",
  replicasNote: "A replica with an old policy has stopped, or can't reach Canyon and is refusing every call.",
  replicaCurrent: "policy up to date",
  replicaAge: "policy <age> old",
  replicaNone: "no policy yet",
  // Settings › Outbound's limits, an admin's (#2691): each app environment's, its range under each.
  capsHeading: "Limits",
  capsNote: "Each environment of each app gets the full limit.",
  capNewNames: "New destinations a minute",
  capNewNamesNote: "New destinations past this are refused until the next minute. A destination called in the last 30 days, or on the allowlist, isn't new. The gateway replicas split this limit between them, so keep it at least as high as their number, or no new destination gets through.",
  capDestinations: "Destinations recorded",
  capDestinationsNote: "How many destinations each environment's records keep. Past this, calls to a new destination are still allowed or refused as usual, and are counted without their names. Lowering it deletes nothing.",
  capPending: "Requests waiting",
  capPendingNote: "How many requests to allow a destination can wait at once. Past this, a refused call files no new request.",
  capRange: "<min> to <max>",
  capsSaved: "Limits saved.",
  // In place of the Outbound card while nothing of the app goes through Canyon (#2561).
  outboundOff: "Outbound is Off on this Canyon, so it records none of this app's calls. An admin can turn it on in Settings › Outbound.",
  // Layer 3's screens (#2145): an env's credentials on the Outbound card, never a value, and the roots Canyon signs under
  // in Settings › Outbound, an admin's.
  routesHeading: "Credentials Canyon adds",
  routesNone: "Canyon adds no credential to this environment's calls.",
  routePlaceholder: "The app gets canyon-adds-the-real-key in <variable>.",
  routeFromSecret: "moved from a secret",
  routeWaitHost: "Canyon isn't adding it: apps here don't call out through Canyon.",
  routeWaitCredentials: "Canyon isn't adding it: an admin turns on credentials in Settings › Outbound.",
  routeWaitSecret: "Canyon isn't adding it: the app's secret <secret> sets its own certificates. Delete that secret first.",
  routeWaitRedeploy: "Canyon starts adding it at this environment's next deploy or promotion.",
  routeOwnerOnly: "Only an owner of the app or an admin can change a credential moved from a secret.",
  routeNoDestination: "Allow a destination first: Canyon adds credentials only to destinations on the allowlist.",
  routeValueNote: "Canyon never shows it again.",
  routePlaceholderNote: "Optional. Canyon puts canyon-adds-the-real-key in this variable, so code that expects a key starts. Every call gets the real one.",
  routeSaved: "Saved. Canyon adds it to <env>'s calls to <destination>.",
  routeRemoved: "Removed the credential for <destination>.",
  routeMoveNote: "The app stops getting the secret: Canyon adds its value to the calls instead.",
  routeMoveNone: "This environment has no secret to move.",
  routeMoved: "Moved <secret> into a credential for <destination>.",
  keysHeading: "Credential certificates",
  keysNote: "To add a credential to a call, Canyon opens it with a certificate the app trusts, signed under one of these.",
  keysNone: "No root yet: Canyon makes one when an app first needs it.",
  keysCanyon: "Canyon's own root",
  keysByo: "Your sub-CA",
  keysSigning: "signing now",
  keysDates: "made <created> · valid until <until> · in use until <ends>",
  keysRotatesAt: "Canyon replaces its root on <date>, and apps trust both for 60 days.",
  keysByoEnds: "Your sub-CA ends on <date>. Canyon doesn't replace it on its own: bring a new one, or switch back to Canyon's own root, before then.",
  keysRotate: "Replace Canyon's root now",
  keysRotateAsk: "Replace the root? Apps trust the new one from their next deploy or promotion, and the old one keeps working for 60 days.",
  keysByoHeading: "Use your own sub-CA",
  keysByoNote: "A sub-CA your devices already trust, name-constrained to the destinations Canyon adds credentials to, and allowing no IP addresses.",
  keysKeyNote: "Canyon never shows the key again.",
  keysSaved: "Saved. Apps trust it from their next deploy or promotion.",
  frontDoorNone: "No front door is set, so no app can run on this Canyon yet.",
  // Beside a front door saved with the probe's warning (#118): the dashboard's plain line, and the docs a link away.
  reachesNode: "Apps on this cluster can reach the server they run on.",
  reachesNodeHow: "How your cluster admin closes this",
  handOffNeeds: "To hand apps off to your platform, Canyon's server still needs:",
  gateway: "Your gateway signs people in",
  gatewayNote: "Canyon checks who your gateway says it signed in: a standard OIDC ID token, or the token an AWS load balancer signs.",
  proxy: "Canyon signs people in",
  proxyNote: "A sign-in proxy runs beside each app, against your identity provider.",
  callbacks: "Your identity provider must accept every app's sign-in address in both environments, https://<address>/oauth2/callback: one entry per app and environment, or a wildcard where your provider allows it.",
  idpResponse: "Your sign-in provider must accept https://<app address>/oauth2/idpresponse for every app, in preview and in production: one entry each, or a wildcard where your provider allows one.",
  cookieNote: "The cookie your gateway keeps people signed in with. Canyon keeps it from every app, so no app can use it to reach another.",
  certificate: "The certificate must cover <names>.",
  secretAsk: "Canyon never shows the secret again.",
  secretSet: "A secret is set for <client_id>.",
  checking: "Checking this cluster and the certificate…",
  frontDoorSaved: "Saved. Apps change at their next deploy.",
  // Where apps run: Canyon, or the org's platform (design/HAND-OFF.md §1; #2439).
  runWhere: "Where apps run",
  runCanyon: "Canyon runs apps on this cluster",
  runOrg: "Your platform runs apps",
  runOrgNote: "Canyon reviews and builds each app, then hands the image to your own platform, which runs it and reports back.",
  runAdmin: "Only an admin can change where apps run.",
  stepServer: "Canyon's server settings",
  reloadAfter: "Reload this page after the restart.",
  stepWebhook: "Your platform's webhook",
  webhookNote: "The https address your platform receives releases at.",
  stepDoorNone: "No front door is set yet. Set it below, to Your gateway signs people in.",
  stepDoorProxy: "The front door is set to Canyon signs people in, which your platform can't run. Change it below to Your gateway signs people in.",
  toFrontDoor: "Go to the front door",
  handOn: "Hand apps to your platform",
  handOnFirst: "Finish the steps above first.",
  revealTickBoth: "Tick the box once both are stored.",
  revealTickOne: "Tick the box once it is stored.",
  copyFailed: "Couldn't copy. The browser did not allow it.",
  // An app's link to share, from its page's ⋯ (#193): the app's own address at the host's public URL.
  copyLink: "Copy link",
  linkCopied: "Link copied",
  onAsk: "Hand apps to your platform? From the next deploy or promotion, your platform runs every app Canyon builds, previews included. Apps running now stay where they are until their next deploy.",
  offAsk: "Run apps on this cluster again? From the next deploy or promotion Canyon runs each app here. What your platform runs now stays with it until then, and nothing moves: a database your platform made stays where it is.",
  revealTitle: "Store these now",
  revealAsk: "Copy both now. Canyon shows them only once. Store them in your platform's secret store.",
  revealAskOne: "Copy it now. Canyon shows it only once. Store it in your platform's secret store.",
  revealAck: "I've stored both",
  revealAckOne: "I've stored it",
  secretNote: "Your platform checks each release against it.",
  tokenNote: "Your platform sends it back to Canyon when it reports a release.",
  revealLost: "Lost one later? Replace it on this page: the old one stops working.",
  revealLeave: "Copy both first: Canyon shows them only once.",
  tokenAsk: "Replace the host token? The current one stops working at once: your platform can't report or fetch access tokens until it has the new one.",
  webhookSecretAsk: "Replace the webhook secret? From now on every release is signed with the new one, a release being retried right now included: your platform refuses them until it has it.",
  pingAnswered: "Your platform answered <status>.",
  pingReachable: "Your platform is reachable. The host token is first used by the first release.",
  pingSecret: "Check it has the current webhook secret.",
  pingRedirect: "Redirects are not followed: give the final address.",
  pingNone: "Canyon couldn't reach your platform: no connection, or no answer in 10 seconds. Canyon's server must be allowed to reach it.",
  lastDelivery: "Last release: your platform answered <status>",
  lastNoAnswer: "Last release: no answer from your platform",
  lastNone: "No release has been sent yet. Send a test delivery to check the address.",
  lastNoneOn: "No release has been sent yet. Deploy an app to see its first release here.",
  onSummary: "Your platform runs apps. Canyon sends each release to <url>.",
  graceNote: "How long an app keeps letting people in on its last access list when it can't reach Canyon: 0 to 24h, like 30m or 1h30m.",
  teamTitle: "For your platform team",
  teamWebhook: "Releases arrive at <url>, signed in Canyon-Signature-256: HMAC-SHA256 of the body under the webhook secret. Dispatch on the body's event, never the header.",
  teamReport: "Each release names its own report_url: your platform reports there, and fetches the release's access token, with the host token.",
  teamDocs: "The exact shapes are release.schema.json, in Canyon's docs under Contracts. How it all works is Your platform runs the apps, in Canyon's docs under Self-host.",
  // How releases reach the platform: the deploy repo or the webhook (design/HAND-OFF.md §3b; #2470).
  deliveryLegend: "How releases reach your platform",
  deliveryRepo: "Canyon commits to your deploy repo (recommended)",
  deliveryRepoNote: "Canyon writes each release's files to the git repo your Argo CD or Flux syncs: previews are pushed, production is a pull request.",
  deliveryWebhook: "Canyon calls your webhook",
  deliveryWebhookNote: "Canyon posts each release to your own pipeline, which runs it and reports back.",
  stepRepo: "Your deploy repo",
  repoUrl: "Repository URL",
  repoUser: "User name",
  repoToken: "Token",
  repoPreview: "Preview branch",
  repoProd: "Production branch",
  repoProdMode: "Production changes",
  repoPR: "Through a pull request",
  repoPush: "Pushed to the branch",
  repoRoot: "Path in the repo",
  repoAPI: "Pull requests API",
  repoAPIGitHub: "GitHub's (GitHub, GitHub Enterprise, Gitea, Forgejo)",
  repoAPIGitLab: "GitLab's merge requests (GitLab.com, GitLab self-managed)",
  repoAPINone: "None",
  repoAPIURL: "API address",
  repoStore: "Secret store",
  repoCA: "CA for the sign-in sidecar and External Secrets (optional)",
  repoTokenNote: "A token for this one repository: on GitHub, a fine-grained token with Contents and Pull requests read and write. Canyon never shows it again.",
  repoTokenSet: "A token is set. Canyon never shows it.",
  repoRootNote: "Canyon writes each app under <path>/<app>/<env>, and reads your templates from <path>/.canyon.",
  repoCANote: "Only when a private CA signed Canyon's own address: the sign-in sidecar in each pod and External Secrets trust it. A git host on a private CA needs its CA in Canyon's server instead: A git host at a private address, under Your deploy repo in Canyon's docs.",
  repoNoReview: "With no pull requests API, Canyon pushes production to its branch with no review in git.",
  repoGitLabNote: "Production goes through a merge request, which Canyon's lines call a pull request. GitLab's API is on the repo's own host: https://<host>/api/v4 when the address is left empty.",
  repoTokenReplace: "Replace the repo token",
  repoTokenAsk: "Replace the repo token? Canyon commits with the new one from now on.",
  repoHostReplace: "Replace the host token",
  repoHostAsk: "Replace the host token? The current one stops working at once: your platform reports with the new one from now on.",
  fetchReplace: "Replace the fetch-only token",
  fetchAsk: "Replace the fetch-only token? The current one stops working at once: External Secrets can't fetch access tokens until it has the new one.",
  fetchLabel: "Fetch-only token",
  fetchNote: "External Secrets fetches each release's access token with it, and it opens nothing else.",
  check: "Check the repo",
  checkPending: "Checking…",
  checkPassed: "Canyon reached the repo, read both branches, pushed a test branch to it and rendered your templates.",
  checkTemplates: "Templates found: <list>",
  checkNoTemplates: "No templates found in <path>/.canyon.",
  checkCannot: "This check can't prove your branch rules, that Canyon may open a pull request, or that Argo CD or Flux, External Secrets and your CA are set up: the first deploy does.",
  checkedAt: "Checked <age>.",
  onRepoSummary: "Your platform runs apps. Canyon commits each release to <url>.",
  onRepoWaiting: "Committed, waiting for the first check-in.",
  teamRepo: "Canyon commits each release to <path>/<app>/<env> in <url>. Apply these once, outside that repo: an Argo CD ApplicationSet for each branch, the namespaces' ApplicationSet, which reads your platform repo's copy of deploy/gitops/namespaces, the AppProjects, the admission policy on the apps' Services and the ClusterSecretStore.",
  teamFetch: "The ClusterSecretStore fetches access tokens with the fetch-only token you stored: Canyon never shows it here.",
  teamRepoDocs: "The reference set-up is deploy/gitops in Canyon's source. How it all works is Your deploy repo, in Canyon's docs under Self-host.",
  teamNext: "Next, your platform team applies these once. The first deploy proves the set-up: it goes live when a pod checks in.",
  teamReadUser: "Argo CD's read-only user",
  teamReadToken: "Argo CD's read-only token",
  teamPlatformRepo: "Your platform repo",
  teamPlatformBranch: "Its branch",
  teamPlatformPath: "Where it holds namespaces/",
  teamFieldsNote: "These fill in the files below, in this browser alone: Canyon never receives them.",
  teamTokenKubectl: "Argo CD's read-only token never goes in a file you commit, nor on a command line: create its Secret with kubectl, which reads the token from its input, then label it.",
  teamCopy: "Copy",
  teamCopyAll: "Copy all",
  teamCopied: "<file> copied",
  teamChart: "Into your platform repo, under <path>",
  // The environment's pending line under delivery: repo (design/EXPERIENCE.md).
  pendingPod: "Your platform hasn't reported it running since <time>",
  pendingMerge: "waiting on your merge since <time>",
  pendingPR: "Pull request #<n>",
  removingMerge: "Being removed once you merge",
  removingPlatform: "Being removed by your platform",
  pendingPlatform: "Sent to your platform at <time>, not running yet",
  // What the platform says of an environment the org runs (EXPERIENCE.md D5): <version> is "it" where the reader has none.
  reported: "Your platform reported it running <ago>",
  reportedStill: "Still running: your platform last reported it <ago>",
  notReportedSince: "Your platform hasn't reported <version> running since <time>",
  reportedNotRunning: "Your platform reported <version> deployed, but not running",
  // The same, short, in the fleet's cells and a phone's cards, which stay a line or two (the UX review); the side pane
  // and the app page say them whole.
  pendingShort: "Sent <time> · not running yet",
  committedShort: "Committed <time> · not running yet",
  removingShort: "Being removed",
  removingMergeShort: "Removed once merged",
  notReportedShort: "not reported since <time>",
  notRunningShort: "deployed, not running",
  running: "Running",
  repoUndelivered: "Canyon couldn't commit the release",
  repoRefused: "Your deploy repo refused the release",
  // The two gates' lines on Guardrails › Pipeline (D14).
  beforePreview: "Runs on every check and deploy. A Blocking check keeps the version out of preview.",
  beforeProduction: "Checked when a person clicks Promote. A Blocking check keeps the button disabled.",
  // What needs a person, as the fleet's Needs you lists it, one line a need in the server's order (D15; #2724).
  // <env> is preview or production; <age> as a row's Last seen reads it.
  needsMore: "Show <n> more",
  needsUnread: "Canyon couldn't read what needs you: <error>.",
  needsUncheckedOne: "Canyon couldn't check whether 1 deployment is running, so something here may need you.",
  needsUnchecked: "Canyon couldn't check whether <n> deployments are running, so something here may need you.",
  needUnknown: "Something here needs a person.",
  needAdmins: "Admins: <admins>.",
  needHeld: "<app>'s production is held.",
  needHeldWho: "An admin, or a security responder who isn't on its team, can lift it.",
  needDown: "<app> is down in <env>.",
  needRestarting: "<app> keeps restarting in <env>.",
  // A down environment's fix: its page shows its last log lines, masked, stopped or restarting (#2744, #188).
  needDownFixPreview: "Its last log lines, on its page, say why. Fix the cause, then deploy again.",
  needDownFixProd: "Its last log lines, on its page, say why. Fix the cause, deploy to preview, then promote.",
  needRefused: "<app> can't reach <destination> in <env>.",
  needRefusedWhy: "Nobody has allowed it for <env> yet, so Canyon refused <calls>, the last <age>.",
  needRefusedOther: "<other> tried to reach it <times> in the last 30 days.",
  needAllowWho: "Anyone on its team, or an admin, can allow it.",
  // The same, to a reader who can't allow it (the need's you false): a read-only member is on the team too (#2816).
  needAllowWhoOther: "An owner or contributor of <app>, or an admin, can allow it.",
  needAllowHeld: "Production is held, so nothing more can be allowed there until the hold is lifted.",
  needAllowed: "Allowed <destination> in <env> for <app>. Its next call goes through within seconds.",
  needDismissed: "Dismissed <destination> in <env> for <app>. Canyon keeps refusing it there and won't ask about it again.",
  needAllowInstead: "Allow it",
  undo: "Undo",
  needCode: "<app> can't reach <destination> in <env>, and nobody can allow it.",
  needCodeAddress: "It's an IP address, so the code has to use a host name.",
  needCodePort: "It's on port <port>, so the code has to use port 80 or 443.",
  needSecret: "<app> has a secret in its code.",
  needSecretFix: "Rotate the key, then upload a fix.",
  // The blocked headline's step for whoever fixes the code (#102).
  copyFix: "Copy the fix for your agent",
  // What blocks the newest version, said to a reader whose read names it no version (#102).
  whatBlocksNewest: "What blocks the newest version",
  // A finding's chip where it lets preview go ahead and blocks production (the run read's blocks_production; #102): never
  // Stops production, which beside the button of that name reads as one (#145).
  blocksAtProduction: "Doesn't block preview. Blocks going live until it's fixed or approved.",
  // The review's two lists after what blocks (#102): what blocks production alone, and what doesn't block, its chip too.
  blocksLive: "Blocks going live",
  doesntBlock: "Doesn't block",
  // A secret's finding by where it is: its scanner's own sentence is its Why (#102).
  secretIn: "A secret is in <file>:<line>",
  // Approving a secret's finding, said above its Approve (#102).
  // What an approval covers, said above its Approve: a fix doesn't end one before its date (design/PERMISSIONS.md; #145).
  approveCovers: "Covers this problem in every version of the app until the end of <date> (UTC), production included, even if it's fixed and comes back.",
  approveCoversAll: "Covers these problems in every version of the app until the end of <date> (UTC), production included, even if they're fixed and come back.",
  approveDates: "Pick a date from <first> to <last>.",
  approveSecret: "Approving lets this version run with the key in its code. Rotate the key anyway.",
  // A runtime secret preview has and production lacks, said in the promotion's question with the way to set it (#102).
  prodLacks: "<name> is set for preview but not production.",
  prodLacksSet: "Set it for production",
  prodLacksWhy: "Without it, <version> may fail to start in production.",
  backToPromote: "Back to promote <version>",
  needApproval: "<who> asked to put <app> <version> in production.",
  needApprovalWhy: "Asked <age>. A person approves production here, in the dashboard.",
  reviewAndApprove: "Review and approve",
  needBlocked: "<app> <version> is blocked by <findings>.",
  needBlockedFixOne: "Fix it in the code and deploy again, or ask an admin or a security responder to approve it with a reason.",
  needBlockedFixMany: "Fix them in the code and deploy again, or ask an admin or a security responder to approve each with a reason.",
  needUnchecked: "<app> <version> is blocked: <checks> couldn't finish.",
  needUncheckedAgain: "Review it again: the checks wait their turn instead of failing.",
  needPatch: "Canyon's fix for <app> is waiting for approval to go to production.",
  needPatchWhy: "Anyone on its team can approve it here, in the dashboard.",
  needAccess: "<who> asked for access to <app>.",
  needAccessWhy: "They can't open it until an owner or an admin adds them. Asked <age>.",
  addReadOnly: "Add as read-only",
  needAdded: "Added <who> as read-only. They can open it within about a minute.",
  needRequestDismissed: "Dismissed <who>'s request. They can ask again.",
  // A read-only member's request (its level viewer): to join the team, which Add makes them, a contributor, with no Undo,
  // which would take away the read-only access they had; and the line of a request from before its level was recorded,
  // which claims neither (#2774).
  needJoin: "<who> asked to join <app>'s team.",
  needJoinWhy: "When they asked, they could open it read-only. Asked <age>.",
  addToTeam: "Add to its team",
  needAddedTeam: "Added <who> to its team. They can change it now.",
  needAccessAsked: "Asked <age>.",
  needUnowned: "<app> has no owner.",
  needUnownedClaim: "Claim it, or ask an admin to assign an owner.",
  needUnownedClaimAdmin: "Claim it.",
  needUnownedAsk: "Ask an admin to assign an owner.",
  needUnownedAssign: "Assign one so someone gets its alerts.",
  needNeverReviewed: "<app> has never been reviewed.",
  needNeverReviewedFix: "Deploy or check it so Canyon reviews it.",
  needRecreateOpen: "<app>'s calls out may skip Canyon until an admin recreates its network in Settings › Outbound.",
  needRecreateInternal: "<app> still goes through Canyon until an admin recreates its network in Settings › Outbound.",
  needRecreateUnknown: "Canyon can't tell what network <app> is on: a recreation is under way, or one didn't finish.",
  needBlockedProdOne: "<app> <version> can't go to production: 1 finding blocks production.",
  needBlockedProdMany: "<app> <version> can't go to production: <n> findings block production.",
  needBlockedProdUnread: "<app> <version> can't go to production: findings block production.",
  needWillRefuseBlocks: "<app> <version> can't go to production yet: production doesn't allow everything it connects to in preview.",
  needWillRefuse: "Production won't allow everything <app> <version> connects to in preview.",
  needAllowWhoMany: "Anyone on its team, or an admin, can allow them.",
  // The same, to a reader who can't allow them (the need's you false), as needAllowWhoOther (#2816).
  needAllowWhoOtherMany: "An owner or contributor of <app>, or an admin, can allow them.",
  invalidAddress: "an invalid address",
  needWillRefuseAllowable: "Production doesn't allow <destinations> yet.",
  needWillRefuseCode: "Nobody can allow <destinations>: the code has to change.",
  allowInProduction: "Allow in production",
  needRecreateUnknownStopped: "If one didn't finish, the app stays stopped until an admin recreates it again.",
  openSettingsOutbound: "Open Settings › Outbound",
  // The org's own need on the fleet's Needs attention, an admin's (the fleet read's org_needs; #2725).
  needNotifyFailing: "Canyon can't post to <channel>.",
  needNotifyFailingWhy: "Its webhook stopped working <age>, so the channel gets nothing from Canyon until an admin sets a new webhook URL.",
  openSettingsIntegrations: "Open Settings › Integrations",
  // The row's own line under the app's name: its first need, short, and how many more.
  needRowMore: " · and <n> more",
  needRowHeld: "Production is held",
  needRowDown: "Down in <env>",
  needRowRestarting: "Keeps restarting in <env>",
  needRowRefused: "Can't reach <destination> in <env>",
  needRowSecret: "A secret is in its code",
  needRowApproval: "<who> asked to put <version> in production",
  needRowBlocked: "<version> can't deploy: <findings>",
  needRowUnchecked: "<version> can't deploy: a check couldn't finish",
  needRowUncheckedMany: "<version> can't deploy: <n> checks couldn't finish",
  needRowPatch: "Canyon's fix waits for approval",
  needRowAccess: "<who> asked for access",
  needRowUnowned: "No owner",
  needRowNeverReviewed: "Never reviewed",
  needRowRecreate: "May skip Canyon",
  needRowBlockedProd: "<version> can't go to production",
  needRowWillRefuse: "Production won't allow some of what <version> connects to",
  needRowRecreateInternal: "Still goes through Canyon",
  needRowRecreateUnknown: "Network unknown",
  needRowUnknown: "Needs a person",
  needEnvRefused: "<n> refused",
  // The rail's count, and the browser tab's, of the needs the reader's fleet carries.
  needsCountOne: "1 thing needs you",
  needsCount: "<n> things need you",
  needsTab: "(<n>) <org> · Canyon",
  needsBand: "Needs you · <n>",
  buildPrepared: "<wait> of it preparing Canyon's images",
  whoCanOpen: "Who can open it",
  share: "Share",
  readOnlyUse: "You can open it where it runs. Its team sees its reviews and versions, and changes it.",
  whoLooksAfter: "Who looks after it",
  whoLooksAfterOwner: "To change it, or if something isn't working, ask <owner>, its owner.",
  whoLooksAfterGroup: "To change it, or if something isn't working, ask a member of the <group> group, which owns it.",
  whoLooksAfterNone: "To change it, or if something isn't working, ask a Canyon admin: it has no owner.",
  callsOutOne: "1 destination in the last 30 days",
  callsOut: "<n> destinations in the last 30 days",
  callsOutRefused: " · <n> refused on their last call",
  callsOutNoneRefused: " · none refused",
  needsAPerson: "Needs a person",
  modeOwnerAsk: "Ask <owner>, its owner.",
  modeOwnerAskGroup: "Ask a member of the <group> group, which owns it.",
  modeSwitchBack: "Switching to the org's Observe lets calls to any public host name through.",
  dismissedHeading: "Dismissed",
  networksUnread: "Canyon couldn't read these apps' networks: <apps>.",
  needsRailUnchecked: "Canyon couldn't check whether some deployments are running",
  needsRailUnread: "Canyon couldn't read what needs you",
} as const;

// The sign-in screen's and the header's copy (design/specs/screens.md §A0, design/AUTH.md §8):
// not D5's, so not in S and not held to EXPERIENCE.md. The button never names the provider.
// A box's title and its line are two keys, `<key>` and `<key>Line`.
// The fleet and first run (the dashboard UX review, 2026-11-23): the empty fleet's two ways in, setup's intro, the
// Access column's one line, a row the reader can't open whole, and the counts' scope.
export const FLEET = {
  emptyTitle: "No apps yet",
  emptyLead: "Add your first app in one of two ways:",
  emptyCli: "From your machine: install the CLI and run `canyon deploy` in the app's folder, or have your coding agent do it.",
  emptyCliLink: "Connect an agent →",
  emptyGitHubAdmin: "From GitHub: connect your organization's repositories, then make an app from one.",
  emptyGitHubLink: "Connect GitHub →",
  emptyGitHubMember: "From GitHub: an admin connects your organization's repositories in Settings.",
  emptyRules: "Every app is checked against your org's rules.",
  emptyRulesLink: "See them in Guardrails →",
  setupIntro: "Canyon reviews every deploy your coding agents make and runs the apps that pass. Four steps: name your organization, choose who can sign in, choose its admins, then check and finish.",
  owner: "Owner",
  contributor: "Contributor",
  private: "private",
  sharedEveryone: "shared with everyone",
  sharedWith: "shared with <n>",
  sharedWithAdmin: "Shared with <n>",
  updated: "Updated <age>",
  readOnly: "Read-only",
  noAccess: "No access",
  askToJoin: "Ask to join",
  asked: "Asked",
  allScopeMember: "Every app in the org. Counts are for your apps.",
} as const;

export const AUTH = {
  signIn: "Sign in to Canyon",
  signInLine: "Use your <org> company account. You'll come straight back here.",
  signInGo: "Continue to company sign-in",
  signingIn: "Opening company sign-in…",
  tryAgain: "Try again",
  noPassword: "Canyon never sees your password.",
  signInFailed: "Sign-in didn't finish",
  signInFailedLine: "Try again. If it keeps failing, ask your Canyon admin.",
  signedOut: "You're signed out of Canyon",
  signedOutLine: "Your company account is still signed in. Sign out of it too if this computer is shared.",
  sessionEnded: "Your session ended",
  sessionEndedLine: "Sign in again to keep going.",
  cliStep: "Canyon CLI · Step <n> of 2",
  setupRequired: "Canyon isn't set up yet. Ask the person who installed it to finish setup.",
  signOut: "Sign out",
  // A sign-out the server couldn't make, or whose answer never came: whether the session ended isn't known (#153).
  signOutFailed: "Canyon couldn't sign you out. Try again.",
  role: { admin: "Admin", responder: "Security responder", member: "Member" },
  // The GitHub accounts the reader linked, at the rail's foot, or the link to link one (#2228).
  githubLinked: "GitHub: <logins>",
  githubLink: "Link GitHub account",
  // The rail's way to the device approval page, its code field empty (#2564): never a pending request.
  // The device approval screen (design/specs/screens.md, "The device approval screen").
  deviceTitle: "Sign in to the Canyon CLI",
  deviceEnter: "Enter the code shown in your terminal.",
  deviceCheck: "Check this matches your terminal.",
  deviceCode: "Code",
  deviceContinue: "Continue",
  deviceFrom: "Requested from",
  deviceWhen: "When",
  deviceChange: "Change code",
  deviceOnlyYours: "Only approve a code you generated yourself.",
  deviceHandsOver: "Approving gives that machine your access for 7 days. Anything running there acts as you, including deploys.",
  // The account approving, its name in bold after it.
  approvingAs: "Approving as ",
  deviceSwitch: "Switch account",
  deviceApprove: "Approve",
  deviceDeny: "Deny",
  // A code the server can't find (404): not valid, already used, or past its ten minutes. The server
  // can't tell which, so the line never says expired (#2647: a used code read as a failed sign-in).
  deviceUnknown: "That code isn't valid or was already used",
  deviceUnknownLine: "If your terminal says you're signed in, you're done. Otherwise run canyon login again.",
  deviceApproved: "The CLI is signed in",
  deviceApprovedLine: "That machine now has your access for 7 days. You can close this tab.",
  deviceDenied: "Request denied",
  deviceDeniedLine: "Nothing was handed over. If you didn't start this, tell your Canyon admin.",
} as const;

// An MCP client's consent page (design/AUTH.md §4.7; design/EXPERIENCE.md, D5).
export const CONSENT = {
  title: "Allow <client> to use Canyon as you?",
  line: "<client> will act as <display> (<email>) on <host>. Then Canyon sends you back to <redirect host>.",
  unverified: "Canyon can't verify this name: the app chose it itself.",
  onlyYours: "Only allow a connection you started yourself.",
  scope: {
    read: "See your org's rules, and the apps you can see: their status, and logs where you're a contributor",
    act: "Check, deploy and promote the apps you contribute to",
  },
  only: "It can do only what you can. It can't approve findings, change settings or sharing, or see secret values.",
  allow: "Allow",
  refuse: "Don't allow",
  expired: "This request has expired. Go back to <client> and connect again.",
  expiredNoClient: "This request has expired. Go back to the app and connect again.",
  invalid: "This connection request isn't valid: <reason>. Go back to the app that sent you here.",
  reason: {
    client: "its client isn't registered",
    redirect: "it names a redirect the client didn't register",
    response: "it doesn't ask for a sign-in code",
    pkce: "it's missing the PKCE check Canyon requires",
    resource: "it isn't for this Canyon's MCP server",
    scope: "it asks for access Canyon doesn't grant",
    state: "its state value is too long",
  },
} as const;

// Settings › Agents, the sessions list.
export const SESSIONS = {
  title: "Sessions",
  line: "Where you're signed in. Admins see everyone's.",
  person: "Person",
  how: "How",
  since: "Since",
  lastUsed: "Last used",
  expires: "Expires",
  flow: { browser: "Browser", device: "CLI", demo: "Browser", oauth: "<client> (MCP)" },
  current: "This session",
  disconnect: "Disconnect",
} as const;

// The production approval screen (design/specs/screens.md, "The production approval screen") and the
// switch on Settings › People & sign-in that turns it on (design/AUTH.md §4.5).
export const APPROVAL = {
  eyebrow: "Production approval",
  // The question names what ships, as the CLI's and canyon_promote's do, byte for byte (design/EXPERIENCE.md D5).
  title: "Promote <app> <version> to production?",
  titlePutBack: "Put <app> <version> back in production, replacing <replaces>?",
  titlePutBackNothing: "Put <app> <version> back in production?",
  // Before the read answers, and when it is refused, nothing is known to name: not even whether it promotes or puts back.
  titleUnread: "A request to change production",
  asked: "<name> asked from the CLI, <time>.",
  asked_elsewhere: "<name> asked, <time>.",
  now: "In production now",
  live: "Goes live",
  nothingYet: "Nothing yet",
  blocked: "This can't go to production now",
  openReview: "Open the review →",
  blockedReason: "Blocked by the go-live checklist.",
  approve: "Approve",
  deny: "Deny",
  expires: "Expires <time>.",
  code: "Code ",
  approved: "Promotion started",
  approvedLine: "<app> <version> is going live. ",
  approvedLink: "Follow it on its page →",
  denied: "Denied",
  deniedLine: "Nothing was promoted.",
  wasApproved: "Already approved by <name>",
  wasDenied: "Already denied by <name>",
  wasDecidedLine: "Nothing more to do here.",
  expired: "This approval expired",
  expiredLine: "Ask for a new one from the CLI.",
  outdated: "Preview runs another version now",
  outdatedLine: "Ask for a new approval from the CLI.",
  unknown: "That code isn't valid, or a newer request replaced it.",
  switchLabel: "Production is approved in the dashboard",
  switchNote: "On: the CLI and agents only ask, and a person on the app's team approves it here in Canyon. Off: anyone on the app's team can promote, and so can an agent they ask, through the CLI or Canyon's tools.",
} as const;

// Settings › Databases (design/DATABASE-SESSIONS.md §5; #2835): the switch for database sessions as a whole.
export const DATABASES = {
  title: "Database sessions",
  on: "On. An app's owners, contributors and admins can query its preview databases and connect a database client to them, each as themselves.",
  off: "Off. Nobody can open a database session, admins included.",
  turnOnMeans: "Turning this on lets an app's owners, contributors and admins into its preview databases through Canyon. Each session runs as the person who opened it, and every one is in the audit log.",
  turnOffMeans: "Turning this off ends every open database session at once.",
  turnOn: "Turn on database sessions",
  turnOff: "Turn off database sessions",
  admin: "An admin turns database sessions on or off.",
} as const;

// Canyon's patches as people see them (design/PATCHING.md §8; #2105): the approval page's patch variant, the fleet's
// second line and Settings › App hosting for admins. `watchPassed` is the contract's own words, byte for byte
// (contracts/routes.md, Patch runs' passed line), as the approval read's proof is; the rest are D5's (design/EXPERIENCE.md).
export const PATCHING = {
  approvalTitle: "Promote Canyon's patch of <app> to production?",
  image: "image <hex>",
  liveImage: "system libraries image <hex>",
  builderImage: "build tools <hex>",
  fixes: "Fixes",
  fix: "<name> <from> → <to> (<vulnerability>)",
  watch: "Preview's watch",
  watchPassed: "The watch passed: the patched app neither exited nor restarted",
  // A rebuild approval's confirm, in place of D5's promote confirm, which says nothing is rebuilt beside a proof that says it was (#2631).
  rebuildConfirm: "Production gets the image preview ran and Canyon watched, with production's own secrets. Approving doesn't build it again.",
  outdated: "This approval is out of date",
  asksAgain: "Canyon asks again if the patch still applies.",
  patchedBy: "patched by Canyon · <date>",
  waiting: "fix ready for production",
  title: "Patching",
  off: "Canyon doesn't patch apps",
  rebuildsTitle: "Rebuilds",
  rebuildsOff: "Canyon doesn't rebuild apps",
  rebuildsAsk: "Canyon rebuilds previews on newer Node.js, Python, Java and Go releases and asks before production",
  ask: "Canyon patches previews and asks before production",
  behind: "<n> apps wait for newer system libraries: <apps>",
  behindOne: "1 app waits for newer system libraries: <apps>",
  current: "No app waits for newer system libraries",
  unknown: "Canyon can't tell which system libraries it uses now, or whether apps wait for newer ones.",
  // A card's name in plain words (#118): the system libraries or build tools in use are the newest, older ones earlier, each with when it was planned;
  // a sentence in the card names them these, and the 12 hex sits folded in the card as its image.
  newestLibraries: "Newest system libraries",
  earlierLibraries: "Earlier system libraries, planned <date>",
  newestTools: "Newest build tools",
  earlierTools: "Earlier build tools, planned <date>",
  theseLibraries: "these system libraries",
  theseTools: "these build tools",
  imageHex: "Image <hex>",
  paused: "Canyon paused patching with <image>: <n> apps reverted after the patch.",
  pausedOne: "Canyon paused patching with <image>: 1 app reverted after the patch.",
  stalled: "Canyon is holding the next wave of patches with <image>: <app> reverted after the patch.",
  resume: "Resume",
  approveAll: "Approve for every app",
  approveAllAsk: "Approve <image> for production on every app?",
  approved: "Approved for every app by <name>, <date>: <reason>",
  wave: ["Step 1 of 4: the first app", "Step 2 of 4: the next 5 apps", "Step 3 of 4: the next 25 apps", "Step 4 of 4: the rest"],
  askedOne: "1 app waits for a person's approval",
  noApps: "No app to patch with these system libraries",
  noRebuildApps: "No app to rebuild with these build tools",
  earlier: "<n> earlier fixes, nothing left to do",
  earlierOne: "1 earlier fix, nothing left to do",
  counts: { planned: "planned", patched_preview: "patched in preview", asked: "apps wait for a person's approval", patched: "patched in production", reverted: "reverted", held: "held", denied: "turned down for production" },
  // What Canyon's last check said (#2588; contracts/routes.md, the identities row's plan), beside the system libraries
  // it checked against: why it checked no app, or each app it checked under its one cause, in this order. A cause the
  // page does not know reads `unknown`, never nothing to do.
  lastCheck: "Canyon's last check, <ago>",
  checkedNone: {
    server_off: "Patching is off on this Canyon server",
    org_off: "Patching is off for this organization",
    not_wired: "This Canyon server isn't set up to patch apps",
    no_preview: "No app has a preview to patch",
    rule_off: "The known-vulnerable packages check is off on every app",
    failed: "Canyon couldn't finish its last check: the server log says why",
  } as Record<string, string>,
  why: {
    started: "Patch started",
    under_way: "Patch under way",
    passed: "Patched in preview",
    wave: "Waiting for its wave",
    stalled: "Waiting for its wave, held after a revert: an admin can resume it",
    paused: "Paused after two reverts: an admin can resume it",
    goes_first: "Another run of it goes first",
    no_finding: "Nothing high or critical to fix in its system libraries",
    no_fix: "No fix from Canyon for this app's findings yet",
    runtime: "Waits for Canyon's next build tools update",
    stopped: "Its preview isn't running",
    rule_off: "The known-vulnerable packages check is off for it",
    refused: "Canyon can't patch its current image, so its next deploy picks up the fix",
    ended_early: "Its watch ended early, so Canyon tries again after its next deploy",
    failed: "The patch failed, so Canyon tries again after its next deploy",
    reverted: "Reverted after the patch, and not tried again with this fix",
    failed_start: "Didn't start after the patch, and not tried again with this fix",
    offline: "Being taken offline",
    moved: "Changed since Canyon's check: Canyon looks again next time",
    unread: "Canyon couldn't check its image yet",
    run_image_unread: "Canyon hasn't checked its fix yet",
    no_digest: "Canyon can't tell exactly which image its preview runs",
    policy: "Canyon couldn't read its policies",
    unknown: "Not patched, for a reason Canyon can't name",
  } as Record<string, string>,
  // The app page's cards say what canyon status's platform row says while an environment's patch is reverted, held or
  // denied (#2602, #2601), its words without the mark: a revert or a patch run's end its run's own last line, the
  // contract's, the rest D5's. `<packages>` each package the denied approval fixes, as production runs it.
  reverted: "Canyon reverted its patch of <env>",
  heldLine: "held: <line>",
  unfinished: "Canyon could not finish patching <version>, so it did not deploy it",
  held: {
    blocked: "held: <version> is blocked by its review",
    production_held: "held: production is held",
    paused: "held: Canyon paused this fix until an admin resumes it",
    preview_ahead: "held: promote <version> to take the fix to production",
  } as Record<string, string>,
  declined: "Your team turned down Canyon's fix for production. Production still has <packages>. Canyon will ask again when a newer fix is out.",
  declinedUnread: "Your team turned down Canyon's fix for production. Canyon will ask again when a newer fix is out.",
  reason: "Reason",
  reasonFirst: "Type the reason above first.",
  // What a patch leaves (#2579): the checklist's known-vulnerable row is the patched image's, so the approval says what
  // remains of it and whether production, read from its image read, already runs that.
  remains: "Still there after this patch",
  remainsNone: "Nothing else known-vulnerable with a published fix.",
  remainsOne: "1 known-vulnerable package, listed in the checklist below.",
  remainsSome: "<n> known-vulnerable packages, listed in the checklist below.",
  remainsUnchecked: "Canyon couldn't check what remains: the checklist below says why.",
  inProduction: "Production already runs them, so approving adds none.",
  notInProduction: "Production doesn't run all of them: approving adds some.",
  productionUnknown: "Canyon can't tell whether production already runs them.",
  inProductionOne: "Production already runs it, so approving adds nothing.",
  notInProductionOne: "Production doesn't run it yet: approving adds it.",
  productionUnknownOne: "Canyon can't tell whether production already runs it.",
} as const;

// platform.known-vuln's row in words a person deciding reads (#2579), D5's: where its packages are, and what
// fixes each, from each package's layers as the receipt names them — newer system libraries for the os class, a newer
// buildpack or lifecycle for the rest — never from its text.
export const KNOWN_VULN = {
  where: "None of these are in the app's own code, which its review checks.",
  osFix: "No fix from Canyon yet",
  osPatching: "Canyon is trying a fix in preview",
  toolsFix: "Waits for Canyon's next build tools update",
  goStdlib: "Go's standard library",
  in: "in <places>",
  inOs: "the system libraries",
  inLauncher: "the app launcher",
  inApp: "the Go version Canyon built the app with",
} as const;

// The app page's Secrets section (design/specs/screens.md, Secrets; design/specs/mcp.md §10.1; EXPERIENCE.md D5): a
// contributor's and up. The value is typed into a password field, sent once, and never shown back.
export const SECRETS = {
  title: "Secrets",
  about: "Values the app reads by name. Canyon keeps each in its vault and never shows one back.",
  form: "Set a secret",
  name: "Name",
  value: "Value",
  env: "Environment",
  where: "Where",
  preview: "Preview",
  prod: "Production",
  runtime: "Runtime",
  build: "Build",
  set: "Set secret",
  nameFirst: "Name the secret first.",
  valueFirst: "Type the value first.",
  replaces: "Setting a name that's already set replaces its value.",
  setPreview: "Set <name> for preview. It takes effect at the next deploy.",
  setProd: "Set <name> for production. It takes effect at the next promotion.",
  keepsOld: "Production keeps the old value until a version is promoted again.",
  replace: "Replace",
  remove: "Remove",
  askPreview: "Remove <name> from preview?",
  askProd: "Remove <name> from production?",
  cancel: "Cancel",
  removedPreview: "Removed <name> from preview. It takes effect at the next deploy.",
  removedProd: "Removed <name> from production. It takes effect at the next promotion.",
  canyons: "Canyon sets this one for the app",
  none: "No secrets set.",
  unread: "The app's secrets couldn't be read.",
} as const;

// Settings › Agents' Connect an agent (design/specs/screens.md, Connect an agent; design/specs/mcp.md §12;
// EXPERIENCE.md D5): every member's, its org-managed snippets an admin's alone.
export const CONNECT = {
  title: "Connect an agent",
  about: "Give your coding agents Canyon's tools. An agent acts as the person who connects it, and each one you connect is listed under Sessions, where you can disconnect it.",
  laptop: "On your laptop",
  stepInstall: "Install the CLI",
  stepLogin: "Sign in to this Canyon",
  stepSkill: "Set up your coding agents",
  // What to say to an agent once it's connected (#192): on a laptop, in the app's folder; in the cloud, of what's pushed.
  askLaptop: "Then open your agent in the app's folder and ask it: Deploy this app to Canyon.",
  askCloud: "Then push the app to GitHub and ask the agent: Deploy this repository to Canyon.",
  laptopNote: "The last step sets up every coding agent on your machine: Canyon's skill, your org's rules at each session start, and Canyon's tools. They load the next time each agent starts.",
  cloud: "In the cloud",
  cloudNote: "For agents that run on their vendor's machines. They review and deploy what you've pushed to GitHub. On a laptop, use the CLI above: one way per agent.",
  address: "Canyon's address for agents",
  addressNote: "The first time an agent uses it, Canyon opens in your browser and asks you to allow it.",
  copy: "Copy",
  copied: "Address copied",
  noAddress: "Agents can't connect to this Canyon by its address, because it isn't served over https. Use the CLI above.",
  claudeWeb: "Claude Code on the web",
  claudeWebHow: "Add this address as a custom connector in claude.ai. On Team and Enterprise plans, an owner of your claude.ai organization adds it once, and each person connects it. The connector also shows up in Claude Code on laptops, where the server canyon skill install set up stays the one to use.",
  claudeDocs: "Anthropic's connector docs",
  cursorCloud: "Cursor's cloud agents",
  cursorCloudHow: "A team admin adds this address as a team MCP server in Cursor's dashboard, under Plugins & MCPs.",
  codexCloud: "Codex's cloud tasks can't connect to an MCP server yet. Use Codex on your laptop, with the CLI above.",
  signInOff: "Sign-in is off on this Canyon, so it gives agents no address: anyone who could reach it would act as an admin. Use the CLI above. A team's Canyon with sign-in gives agents an address.",
} as const;
// Anthropic's docs for adding a claude.ai connector, which Claude Code on the web uses (Connect an agent).
export const CONNECTOR_DOCS = "https://support.claude.com/en/articles/11175166-getting-started-with-custom-connectors-using-remote-mcp";

// The app page's System libraries card (#2079; design/specs/screens.md A2, D5's): a contributor's and up, each environment's
// image read in its own words beside its mark, then what it counts, one card a package, where it is in a developer's
// words (never its class), its notes and what Canyon's patches fixed in it.
export const LIBRARIES = {
  title: "System libraries",
  about: "What Canyon adds under the app's own code: system libraries such as OpenSSL, and the build tools that install Node, Python or Java and start the app.",
  checked: "Packages listed <listed> · checked against vulnerability data from <data>",
  head: "<name> <version>: <n> known vulnerabilities",
  headOne: "<name> <version>: 1 known vulnerability",
  inOs: "system libraries",
  inTools: "build tools",
  advisory: "<vulnerability> · fixed in <fixed>",
  notes: "<n> notes",
  notesOne: "1 note",
  note: { no_fix: "no fix published", unknown: "Canyon can't tell what put it there", not_checked: "not checked", unplaced: "Canyon can't tell which part of the image it's in" } as Record<string, string>,
  fixed: "Fixed by Canyon · <name> <from> → <to> · <vulnerability> · <place>",
  unread: "Canyon couldn't read what runs in <env>: <error>",
  notWhole: "its answer doesn't read whole",
} as const;

// An app level in the screens' words: read-only for the API's viewer, never "grant" or "level" (EXPERIENCE.md D6).
export const ACCESS_WORD = { owner: "owner", contributor: "contributor", viewer: "read-only" } as const;

// A rule's state in the screens' words (the policies and the scanners on Guardrails).
export const POLICY_STATE = { off: "Off", advisory: "Advisory", blocking: "Blocking" } as const;

// Taking an environment offline and deleting the app (design plan F1): not D5's, so not in S.
// `<env>` is "preview" or "production"; a resource is named "<type> <name> in <env>".
// The ⋯ menu's Rename (design/specs/screens.md, Renaming it; #1909).
export const RENAME = {
  title: "Rename…",
  name: "Name",
  idStays: "Only the name changes. Its address, canyon.toml and the CLI keep <id>.",
};
export const TEARDOWN = {
  takeOffline: "Take <env> offline",
  offlineConfirm: "Take <app>'s <env> offline? The link stops working. Its databases, data and secrets are kept. <back> again to bring it back.",
  deleteTitle: "Delete app…",
  destroys: "This permanently deletes <app>: <resources>, its preview and production, every version, its review history and its secrets. It can't be undone.",
  destroysNothing: "This permanently deletes <app>: every version, its review history and its secrets. It can't be undone.",
  typeName: "Type <app> to enable this button.",
  deleteButton: "Delete <app>",
  stillDeleting: "Still deleting: <resources>.",
  // one the org's platform provided (#2432): the delete asks the platform to delete it
  provided: "<resource> (your platform deletes it)",
  // A resource by the environment it serves and its kind, its name after it (#102): "the production Postgres database (db)".
  resource: "the <env> <kind> (<name>)",
  postgres: "Postgres database",
  // Production goes with the app: said where it runs, in the CLI's words (cli/cmd_delete.go; #102).
  prodLive: "Production is live at <url>, and goes offline with it.",
} as const;

// What the org's platform provided for the app (design/HAND-OFF.md §7, #2432): each row is
// "<type> <name> · <env>" and its endpoint, a place drawn as text, never a link.
export const PROVIDED = {
  title: "Provided by your platform",
  row: "<type> <name> · <env>",
} as const;

// The plain words for every audit action the server writes (contracts/routes.md, GET /audit).
// An action is the route pattern the change came through (cmd/canyon-server/audit.go's `action`)
// or, for a step outside any request, the step's name — the map is keyed by those exactly. A
// `<word>` is filled from the row's detail by ui/Audit.tsx; an action missing here renders as
// itself, never blank. The list is every AuditTx call in cmd/canyon-server and runs/pipeline.go,
// plus `policy.set` from the policies row: add a row here when a route starts writing one.
export const AUDIT_ACTIONS: Record<string, string> = {
  "sign in": "signed in",
  "sign out": "signed out",
  "revoke sessions": "signed <subject> out everywhere",
  "deploy preview": "deployed <version> to preview",
  "deploy prod": "promoted <version> to production",
  "request promotion": "asked a person to approve <version> for production",
  "approve promotion": "approved <version> for production",
  "deny promotion": "denied <version> for production",
  "verdict": "reviewed <version>: <verdict>, <blocking> blocking, <warnings> advisory",
  "build failed": "the build of <version> failed",
  "start failed": "<version> didn't stay up after it started",
  "detect failed": "this host can't build <version>'s kind of app",
  "build had no room": "the cluster had no room to build <version>",
  "policy.set": "set the policy <rule> to <state>",
  "custom_rule.set": "saved version <number> of the custom rule <rule>",
  "set playbook": "saved version <number> of the playbook <slug>",
  "restore playbook": "restored version <from> of the playbook <slug> as version <number>",
  "remove playbook": "removed the playbook <slug>",
  "POST /api/v1/projects": "created the app",
  "PUT /api/v1/projects/{id}/owner": "claimed the app",
  "DELETE /api/v1/projects/{id}": "deleted the app",
  "DELETE /api/v1/projects/{id}/envs/{env}": "took <env> offline, keeping its data",
  "request undeploy": "asked to take <env> offline while a run was writing it",
  "send remove again": "asked your platform again to remove <env>: it had started what was taken offline",
  "declare resource": "declared the resource <name>",
  "ensure resource": "made the resource <name> ready in <env>",
  "update sidecar": "updated the sign-in sidecar in <env> after an upgrade",
  "reissue resource password": "gave the resource <name> a new password in <env>",
  "delete resource": "deleted the resource <name> in <env>, with its data",
  "package from github": "pushed <version>",
  "gate run from github": "started a review of <version> from GitHub",
  "github app registered": "registered the GitHub App",
  "POST /api/v1/projects/{id}/packages": "uploaded <version>",
  "POST /api/v1/projects/{id}/packages/from-repository": "packaged <version> from GitHub",
  "POST /api/v1/projects/{id}/gate-runs": "started a review of <version>",
  "POST /api/v1/projects/{id}/deploys": "started a deploy of <version>",
  "POST /api/v1/projects/{id}/promotions": "started a promotion of <version>",
  "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve": "approved a problem with a reason",
  "POST /api/v1/gate-runs/{run}/findings/approve": "approved <count> problems in <dependency> with a reason",
  "POST /api/v1/gate-runs/{id}/cancel": "cancelled <run> of <version> · started by <started_by>",
  "PUT /api/v1/projects/{id}/secrets/{name}": "set the secret <name> for <env>",
  "DELETE /api/v1/projects/{id}/secrets/{name}": "removed the secret <name> from <env>",
  "GET /api/v1/projects/{id}/logs": "read the <env> logs",
  "POST /api/v1/design-systems": "registered the design system <name>",
  "PUT /api/v1/preferences": "changed the guardrails",
  "POST /api/v1/guardrails/setup": "changed the guardrails through setup",
  // Not a route pattern: the PUT that sets onboarded_at is named for what it did, and the trail
  // would otherwise render the raw action beside the org's very first change.
  "complete setup": "finished setting Canyon up",
  // An app's team (contracts/routes.md, Access): <grantee> an address, group:<name> or everyone.
  "share app": "shared the app with <grantee> as <access>",
  "unshare app": "removed <grantee> from the app",
  "request access": "asked for access to the app",
  "dismiss access request": "dismissed <email>'s request for access",
  // An app environment's allowlist (design/EGRESS.md §3.3): D5's words (#2560).
  "egress.allow": "added <destination> to the <env> allowlist",
  "egress.disallow": "removed <destination> from the <env> allowlist",
  // The other egress rows (#2563): the org's via or mode, an app env's mode (<mode> its app_mode.after, null the
  // org's), Canyon's request as a refusal files one, its Dismiss, and an admin's Recreate networks.
  "egress.set": "changed the org's outbound settings",
  "egress.mode.set": "set the <env> outbound mode to <mode>",
  "egress.request": "refused <destination> in <env> and asked a person to allow it",
  "egress.request.dismiss": "dismissed the request to allow <destination> in <env>",
  "egress.networks.recreate": "recreated the app's network",
  // A credential Canyon adds and the roots its certificates are signed under (#2145): never a value, a key or a fingerprint.
  "egress.route.set": "set the credential for <destination> in <env>",
  "egress.route.remove": "removed the credential for <destination> in <env>",
  "egress.ca.rotate": "replaced the root of Canyon's credential certificates",
  // The hold (design/OPERATIONS.md §3.9): a responder's or an admin's stop of production, and its lift.
  "stop app": "held production: <reason>",
  "lift hold": "lifted the hold without starting production",
  // An app's own guardrail (contracts/routes.md, the app policies row), as the Audit page's verb says it (#2603).
  "app_policy.set": "changed the app's guardrail <rule>",
  "app_policy.remove": "removed the app's guardrail <rule>",
  // Canyon's patches (design/PATCHING.md §0.1; #2603): its own rows, Canyon the actor, but patch prod, whose actor is
  // the person who approved it. <image> is how Settings › Patching names the identity: system libraries image <12 hex>.
  "patch preview": "patched <version> in preview",
  "patch prod": "promoted Canyon's patch of <version> to production",
  "revert patch preview": "reverted its patch of <version> in preview",
  "revert patch prod": "reverted its patch of <version> in production",
  "request patch approval": "asked a person to approve its patch of <version> for production",
  "approve patch identity": "approved <image> for production on every app: <reason>",
  "pause patching": "paused patching with <image>",
  "resume patching": "resumed patching with <image>",
  // Patching at upgrade (contracts/routes.md, Licence; #2881): a new install's first start with no licence for patching.
  "patching off at start": "turned patching off: it needs Canyon Enterprise, and this new install has no licence",
};
// lift hold when it started production again (detail.started), naming the version it started.
export const AUDIT_LIFT_STARTED = "lifted the hold and started <version>";
// A hold whose stop Canyon could not carry out (detail.stopped false). No other row says production stopped: one the org's
// platform runs is written as its remove is sent, and nothing in it tells the two apart (contracts/routes.md, The hold; #145).
export const AUDIT_HELD_RUNNING = "held production, but Canyon couldn't stop it: <reason>";
// What a hold's and an undeploy's rows say happened to what ran, by their stop (contracts/routes.md; #156). "stopped" only
// of what Canyon ran; a row written before stop reads as AUDIT_ACTIONS has it.
export const AUDIT_STOP: Record<string, string> = {
  stopped: "stopped and held production: <reason>",
  remove_sent: "held production and asked your platform to remove it: <reason>",
  nothing: "held production, where nothing ran: <reason>",
  failed: AUDIT_HELD_RUNNING,
};
export const AUDIT_UNDEPLOY: Record<string, string> = {
  remove_sent: "asked your platform to take <env> offline, keeping its data",
  nothing: "stopped the deploy to <env>; nothing ran there",
};
// egress.networks.recreate when its row's failed names the step that failed (#2563).
export const AUDIT_RECREATE_FAILED = "tried to recreate the app's network, but it failed";
// egress.route.set when its row names the secret it moved (detail.secret), and egress.ca.rotate onto an admin's sub-CA
// (detail.byo) (#2145).
export const AUDIT_ROUTE_MOVED = "moved the secret <secret> into the credential for <destination> in <env>";
export const AUDIT_CA_BYO = "made the org's own sub-CA the root of Canyon's credential certificates";
// A check's upload (detail.kind "check") reviews a tree and makes no version of the app.
export const AUDIT_CHECK = "checked <version>";
// policy.set when the edit places, moves or removes the rule before production (detail.production):
// placing and removing read apart, and the preview state leads only when it changed too.
// policy.set with a grace (detail.grace_days): the change was made now and is in force after the days.
export const AUDIT_GRACE = { one: ", in force in 1 day", many: ", in force in <days> days" };
export const AUDIT_PRODUCTION = {
  placed: "put <it> before production, <state>",
  removed: "took <it> out of before production",
  set: "set <it> before production to <state>",
};
