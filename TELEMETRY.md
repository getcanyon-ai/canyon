# Telemetry

A Canyon server sends one report of counts a day. This page lists every field of it. It is generated from
[contracts/schema/telemetry.schema.json](contracts/schema/telemetry.schema.json), the schema the report is
held to, so the two never differ.

## What is sent

| Field | Example | What it is |
|---|---|---|
| `instance_id` | `"7c9e6679-7425-40de-944b-e07fc1f90ae7"` | A random UUID made at the server's first start and kept in its database; never derived from a host, a MAC address or anything else. Reset it with DELETE FROM telemetry; the next start makes a new one. |
| `version` | `"v0.9.0"` | The server's version. |
| `os` | `"linux"` | The server's operating system. |
| `arch` | `"arm64"` | The server's processor architecture. |
| `install_mode` | `"helm_one_node"` | How Canyon was installed, as the install set CANYON_INSTALL_MODE. Left out when it is unset. |
| `apps` | `7` | How many apps the server has, deleted ones left out. |
| `checks` | `{"passed":12,"advisory":1,"blocked":3}` | Reviews that ended in the period (period_hours), by result: passed with nothing to say, passed with advice, blocked. A report without period_hours counts the last 24 hours. |
| `milestones` | `["first_check","first_block","first_fix"]` | The steps this server has ever reached: a first review, a first blocked review, a first blocked app that then passed. |
| `fixes` | `{"count":2,"minutes_p50":18,"attempts_p50":2}` | Blocked apps that passed again in the period (period_hours): how many, and the median minutes and reviews from the first block to the pass. The medians are 0 when there are none. A report without period_hours counts the last 24 hours. |
| `promotions` | `4` | Promotions to production in the period (period_hours), or the last 24 hours in a report without it. |
| `rules_fired` | `{"secrets.client-bundle":2,"custom":1}` | Findings in the period's reviews (the last 24 hours' in a report without period_hours), by rule: a built-in rule by its id (gate/policy/canonical.go, or a tool Canyon runs itself), every other rule counted as custom and never named. The names are this list alone, so the seam itself refuses any other. |
| `scanners` | `["opengrep","trivy"]` | The scanners the server runs, by their tool names. |
| `ai_review` | `true` | Whether the AI review runs: it is on, and a model is set up on this host. |
| `agents` | `{"claude_code":10,"cursor":2,"codex":1,"other":0}` | Reviews in the period by the coding agent that asked for them, from the CLI's known agent markers: claude_code and codex by their own markers, other for a review a coding agent's session asked for with no marker Canyon knows. Cursor sets no marker yet, so it counts under other. |
| `skill_installs` | `{"claude_code":1}` | Canyon skill installs, by coding agent. Left out until the CLI says when it installs one. |
| `unsupported` | `{"language_unrecognised":2,"own_dockerfile_ignored":1}` | Things people asked for that Canyon doesn't do yet, in the period (the last 24 hours in a report without period_hours), by category: an app no buildpack recognised, and a Dockerfile Canyon didn't read. |
| `period_hours` | `24` | The hours the period's counts cover: from the previous send to this one, at most a week, so no review is counted twice or missed. A report without it is an older server's, whose counts cover the last 24 hours. |
| `demo` | `false` | Whether this is Canyon's demo, started with canyon demo up and its sample history: counted apart from installs. |
| `sign_in` | `"oidc"` | Whether people sign in to this Canyon: oidc, or off. |
| `age_days` | `"7-27"` | How old the install is, in days, bucketed: from when its setup finished, else its first app. |
| `people` | `{"d1":3,"d7":5,"d28":8}` | How many people used Canyon in the last 1, 7 and 28 days: signed in, made a change or started a review. Counted, never named. |
| `people_total` | `"6-25"` | How many people have a session or access to an app, bucketed. |
| `agent_sessions` | `{"d1":2,"d7":4,"d28":6}` | How many coding agents' sessions were active in the last 1, 7 and 28 days. |
| `apps_active` | `{"d7":4,"d28":6}` | How many apps had a review in the last 7 and 28 days. |
| `deploys` | `{"preview":9,"prod":2}` | Deploys started in the period, to preview and to production (a promotion counts as production). |
| `authors` | `{"person":3,"agent":11}` | The period's reviews, by who asked for them: a person, or a coding agent's session. |
| `failures` | `{"build":2,"detect":1,"error.scan":1}` | Reviews and deploys that failed in the period, by why: a step Canyon couldn't finish, or Canyon's own error, by the step it was in. Patch runs are counted under patching. |
| `handoff` | `{"deployed":3,"failed":0,"refused":0,"undelivered":1}` | Releases handed to the org's own platform in the period, by how they ended. |
| `first` | `{"check":"lt_10m","block":"10m_1h","fix":"1h_1d"}` | How long after the install started (the same start as age_days) each first came: the first review, the first blocked review, the first blocked app that then passed, and the first promotion to production. A step not reached yet is left out. |
| `custom_rules` | `{"ai":2,"code":1}` | The org's own rules that are on, by how each is checked: by the AI review, or by code. Never named. |
| `policy_changed` | `3` | How many built-in rules are set away from their default. |
| `ai_provider` | `"anthropic"` | Which kind of model the AI review runs on, none when it doesn't run. |
| `ai_gateway` | `false` | Whether the AI review's model is reached through a gateway the install set. |
| `run_by` | `"canyon"` | Where apps run: on Canyon, or on the org's own platform. |
| `db_sessions` | `{"setting":"on","prod":"read","read":4,"write":1}` | Database sessions: whether they are on, how far production is open, and how many were opened in the period to read and to write. |
| `outbound` | `{"via":"canyon","allowlist_apps":2,"credential_routes":true,"routes":"2-5","refusals":7}` | Outbound control: whether apps' calls go through Canyon, how many apps allow only listed destinations, whether Canyon adds credentials to calls and to how many destinations (bucketed), and to how many destinations it refused calls in the period. Never a destination. |
| `patching` | `{"clean":5,"regressed":0,"ineffective":1,"reverted":0,"not_same_inputs":0,"not_platform_only":1,"paused":0}` | Patch runs that ended in the period, by how they ended, and how many patch waves are paused now. |
| `licence` | `{"state":"valid"}` | Whether a Canyon Enterprise licence is in use: its state alone, never what it unlocks, its seats, its dates or whose it is. |
| `prod_approval` | `true` | Whether a person must approve each promotion to production. |
| `approvals` | `{"approved":2,"denied":0}` | Promotions to production a person approved or denied in the period. |
| `notify` | `{"slack":1,"teams":0}` | Notification channels set up, by kind. |
| `github` | `true` | Whether Canyon's GitHub App is installed. |
| `group_sync` | `false` | Whether a groups claim is set, so access follows people's sign-in groups. |
| `stacks` | `{"node":4,"python":2}` | Apps by language, from the buildpacks of each app's latest build. |
| `last_install_failure` | `{"step":"build_image","class":"timeout"}` | The step and error class of the last install that failed, sent once. Left out until installs record it. |
| `ref` | `"docs"` | The docs page's utm_source, if the install command carried one. Left out until installs record it. |

## Never sent

Code, file names or paths, repo or app names, hostnames, domains, cluster names, user names, emails, IP
addresses, finding text, custom rule names or content, secrets, environment variables, AI review content. If a
field could identify a company or a person, it isn't sent. Custom rules are never named: they count only as
`custom` in `rules_fired`.

## Turning it off

- An admin runs `canyon telemetry off`, or turns it off in Settings › Telemetry, where an admin also turns it back on.
- `CANYON_TELEMETRY=0` or `DO_NOT_TRACK=1` on the server, or `telemetry.enabled: false` in the Helm values.
- It is off by itself when `CI` is set, and when the server can't reach the collector at its first send.
- It's on by default. `canyon telemetry show` prints the next report exactly as it is sent.
- It goes once a day, over HTTPS, to `https://usage.myagentcontrol.ai/v1/report`.

To get a new instance id, run `DELETE FROM telemetry;` on Canyon's database: the server makes a new one.
