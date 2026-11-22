package cli

import "fmt"

// The tour's words (cli.md §6.14.1). Canyon checks at three stages: the agent's deploy, the pull
// request and production. Each chapter shows a different kind of rule: the org's standards, then
// security, then what production needs. The end names the stage a laptop cannot show. Every beat
// is reported in the same four lines (tourRules). The agent's text is instructions; the person's
// is the same chapter to read along with. tour_test.go holds both to the forms and verbs this
// binary runs.

var tourTitles = map[int]string{
	tourFirst:    "Standards: your agent ignores the design system",
	tourSecurity: "Security: your agent writes a flaw",
	tourProd:     "Production: the company's rules decide",
	tourEnd:      "Take it to real work",
}

// tourRules open every chapter the agent reads: a new session has nothing but this text.
const tourRules = `You are giving the user a guided tour of Canyon. They have never seen it. Do the beats in
order, then stop and let the user choose.

Rules:
- Report every beat in exactly these four lines, one short sentence each, and add only the links
  or code the beat names. No other prose, no raw output.

    What happened:
    How Canyon helped:
    Without Canyon:
    At scale:

  Each beat gives you the facts for them. Say nothing about Canyon that this text or a command's
  output does not say.
- Say one short sentence before each command, on what it is for.
- Canyon's tools. Where a beat names canyon_check, canyon_deploy, canyon_status or canyon_promote,
  call that tool with dir set to the full path of ./canyon-tour. If it is not among your tools, run
  the command in brackets after it, in ./canyon-tour, instead: it does the same and says the same.
- The user can leave at any time. If they say skip, run canyon demo tour skip, then canyon demo tour, and
  follow what it prints. If they say end, run canyon demo tour end, then canyon demo tour.
- The only code the tour changes is what canyon demo tour plant writes and the one fix chapter 2
  names. Never write a flaw of your own.
- Never install, delete or repair anything this text does not name. If a command fails in a way
  a beat does not expect, show the user what it printed and ask.
- If a canyon command can't reach a Canyon host, the laptop demo is not running. Don't start it
  yourself: a first start outlasts your command time limit. Ask the user to run this in a
  terminal of their own, and to tell you when it says Canyon is ready:
    canyon demo up
  It checks the machine first and says what is missing; it installs nothing.
`

// tourChoices close every chapter but the end. The third is the way to real use; it is offered
// once, and never again in other words after a 1 or a 2.
const tourChoices = `
End with exactly these three choices, and wait:

  What next?
  1. %s
  2. Skip to the end
  3. %s

- On 1: run canyon demo tour next, then canyon demo tour, and follow what it prints.
- On 2: run canyon demo tour end, then canyon demo tour, and follow what it prints.
- On 3: %s
After a 1 or a 2, do not offer 3 again before the end of the tour.
`

var tourAgent = map[int]string{
	tourFirst: "CANYON TOUR — CHAPTER 1 of 3: Standards: your agent ignores the design system\n\n" + tourRules + `
Open with two lines, before beat 1: Canyon checks AI-built apps at three stages: when a coding
agent deploys, on every pull request, and before production. The three chapters show three kinds
of rule: the company's standards, security, and what production needs.

Then run canyon skill install. Say in one line what it wrote: Canyon's skill, the session hook that
hands you the company's rules, and Canyon's tools for you. Say in one more that the tools load in
this session, or after a restart where you load tools only at start, and that until then the tour
uses the same commands in the shell.

Then ask, and wait: "The full tour (three chapters), or the short one (security only)? Either
takes minutes. You can also say skip or end at any time." On short: run canyon demo tour skip,
then canyon demo tour, and follow what it prints.

Beat 1. What is running. Run canyon fleet.
- What happened: Canyon runs at the host the output names, as an insurance company with that many apps.
- How Canyon helped: one list shows every app, who owns it and whether it passed review. Give the
  dashboard link from the output's last line.
- Without Canyon: nobody knows which AI-built apps exist, or what is in them.
- At scale: the list reads the same for seven apps or seven hundred.
Add: sign-in is off in this demo, so everyone who reaches it is an admin.

Beat 2. You set a rule. Say: for one minute you are the platform team, and you make the company's
design system mandatory. Show the user this link, with the host's address in place of <host>, and
these two lines:

    <host>/#/guardrails/policies?rule=design.org-system
    On the marked row, "Apps use the org's design system", choose Blocking.
    Then press Save in the bar at the bottom, not "Save, tightening in 14 days": that one blocks nothing today.

Wait until they say it is saved. Then run canyon manifest. Go on only when it lists "Apps use the
org's design system" among the policies that block. If it does not, say so, show the link and the
two lines again, and wait. You cannot set a rule: only a person can. If the user would rather not,
offer to skip to the security chapter, which needs no rule.
- What happened: the user made the company's design system mandatory, with one click.
- How Canyon helped: I was handed the new rule at once, and so is every other coding agent.
- Without Canyon: a wiki page that agents never read.
- At scale: one change reaches every team's agents the same minute.

Beat 3. The roles. Say: now you are a developer who builds with AI, and I am your coding agent.
Send me:

    Build me a claims tracker, and deploy it.

Wait until they send it.

Beat 4. The deploy. Run canyon demo tour plant, then canyon_deploy (canyon deploy) for ./canyon-tour,
unchanged. Canyon blocks it. That is the chapter working.
- What happened: the tour wrote the app, the quick version an agent often writes, so every visitor
  gets the same code. Canyon's server reviewed the upload and rejected it: the page is hand-built
  and uses nothing from the company's design system. Show the finding's title and file.
- How Canyon helped: it stopped the deploy before anything was built, and named the standard.
- Without Canyon: one more app that looks and behaves unlike the rest, noticed late or never.
- At scale: a hundred agents build a hundred apps, and they are still one product.
Link: the app in the dashboard (the fleet url line). Then ask: "Shall I rebuild the page in the
design system?" Wait.

Beat 5. The fix. Run canyon demo tour plant once more, then canyon_check (canyon check), then
canyon_deploy (canyon deploy), both for ./canyon-tour. If it still blocks, stop and show what is left.
- What happened: the page is rebuilt in the design system and runs in preview. Give the preview
  link and who can open it (the access line). Say once: in a real project I fetch the components
  from the company's registry; this demo's registry is not online, so the tour handed them over.
- How Canyon helped: the finding said what the standard is, so it took one round.
- Without Canyon: a design review, and a rewrite after launch.
- At scale: the platform team sets the standard and never polices it.
If findings remain that do not block, name each in one line. Fix one only if asked.
` + fmt.Sprintf(tourChoices,
		"Next chapter: Security: your agent writes a flaw",
		"Try this on my own app",
		`say that the design-system rule they set is still Blocking here, and blocks an app with a page
  of its own, so first they set it back to Advisory at the host's address followed by
  /#/guardrails/policies?rule=design.org-system. Then in their own project canyon skill install teaches
  their coding agent to use Canyon and prints every file it writes, a session hook that runs in every
  project among them. Then they ask their agent to deploy. It goes to this same Canyon. canyon demo
  tour brings them back.`),

	tourSecurity: "CANYON TOUR — CHAPTER 2 of 3: Security: your agent writes a flaw\n\n" + tourRules + `
Open with one line: the same review, a different kind of rule. With GitHub connected, this is
also what every pull request gets.

If chapter 1 was skipped, first run canyon fleet and say in two lines: Canyon runs at that host as
an insurance company with that many apps, and the dashboard is at the output's last line. Add
that sign-in is off in this demo.

Beat 1. The roles. Say: you are the developer again. Send me:

    Add search by policy number.

Wait until they send it.

Beat 2. The deploy. Run canyon demo tour plant, then canyon_deploy (canyon deploy) for ./canyon-tour,
unchanged. Canyon blocks it.
- What happened: the tour wrote the search, the quick version an agent often writes. Canyon
  blocked it. Show the file, the line and the problem, as the review printed them.
- How Canyon helped: it stopped the deploy before anything was built, and handed me the fix.
  Preview still runs the version from chapter 1.
- Without Canyon: this search would be live, and anyone could rewrite its database query.
- At scale: no person reviewed this. The same check runs on every deploy by every agent.
Link: the app in the dashboard (the fleet url line).

Beat 3. The fix. Ask: "Shall I fix it, or do you want to try?" Wait. First read how the company writes
its claims apps: canyon_playbook with slug claims-portal-conventions (canyon playbook
claims-portal-conventions). Say in one line that the company's playbook names the same fix. The fix
is one line in ./canyon-tour/server.js:

    const rows = db.prepare("SELECT * FROM claims WHERE policy_number = ?").all(req.query.policy);

Change that line and nothing else. Run canyon_check (canyon check), then canyon_deploy
(canyon deploy), both for ./canyon-tour. If it still blocks, stop and show what is left.
- What happened: one line changed, Canyon reviewed it again and runs it in preview. Give the link.
- How Canyon helped: the finding named the fix, and the company's playbook showed how its apps write
  that query, so it took one round.
- Without Canyon: a security review, days later, by a person.
- At scale: agents fix their own findings, and the platform team only sets the rules.
If findings remain that do not block, name each in one line. Fix one only if asked.
` + fmt.Sprintf(tourChoices,
		"Next chapter: Production: the company's rules decide",
		"Put this in my pipeline",
		`say that with GitHub connected, Canyon runs this review on every push and pull request
  and reports it as a check named Canyon gate, which they can require on the default branch so
  nothing merges unreviewed. Canyon also reads the reports of scanners they already run. Canyon's
  docs have this under Build, GitHub. canyon demo tour brings them back.`),

	tourProd: "CANYON TOUR — CHAPTER 3 of 3: Production: the company's rules decide\n\n" + tourRules + `
Beat 1. What production needs. If ./canyon-tour is missing or was never deployed, the chapters
that build the app were skipped: run canyon demo tour plant, then canyon_deploy (canyon deploy) for
./canyon-tour, and say in one line that the tour handed over the finished app. Then run
canyon_status (canyon status) for ./canyon-tour. Show the production checklist, one line per rule.
- What happened: the app runs in preview, and Canyon checked it against the company's production rules.
- How Canyon helped: the chapters before were about the code. This one is about the company: a
  passing review, a person who asked, an owner, an approved stack.
- Without Canyon: a ticket, and a platform engineer who checks the same list by hand.
- At scale: the platform team writes the list once and never sits in the queue.

Beat 2. The roles. Say: you are still the developer. Send me:

    Put the claims tracker in production.

Wait until they send it. Never promote on something they said earlier.

Beat 3. The promotion. Never set confirm on your own: only the person's yes in this conversation
sets it, and their message in beat 2 is that yes. With it, call canyon_promote with confirm true
(canyon deploy --env prod --yes) for ./canyon-tour. If the client asks them itself, let them answer
there. Without it, ask whether to put the claims tracker in production, and wait. If Canyon refuses,
show what it printed and stop. Then run canyon fleet.
- What happened: the reviewed version runs in production. Give the link and who can open it.
- How Canyon helped: it promoted the exact version that passed, and recorded who asked.
- Without Canyon: either version Canyon blocked could have been the one that shipped.
- At scale: every app in the company reaches production the same way. Links: the fleet, from the
  output's last line, and the audit log, the host's address then /#/audit.
If the promotion printed advisories, name each in one line: they did not stop production.
` + fmt.Sprintf(tourChoices,
		"The end: a recap, and where to go from here",
		"Set this up for my team",
		`say that this demo has sign-in off and keeps nothing. A team needs Canyon with sign-in
  through its identity provider, on one Kubernetes node with the Helm chart or one Linux server
  with Docker. Canyon's docs, under Self-host, have both. canyon demo tour brings them back.`),

	tourEnd: "CANYON TOUR — THE END: Take it to real work\n\n" + `You are giving the user a guided tour of Canyon. This is its last part. Keep it short.

Recap in four lines, only what the user saw. Leave out a skipped chapter and say canyon demo tour reset
shows it.
- Standards: you made the design system mandatory, and Canyon rejected an app that ignored it.
- Security: Canyon blocked a flaw before anything was built, and handed over the fix.
- Production: the company's rules decided, with no ticket.
- The stage this demo could not show: with GitHub connected, the same review runs on every pull
  request and reports as a check named Canyon gate.

Then offer these four ways on, once, each as one line, and let the user pick or stop:

  1. Try this on my own app. First set the design-system rule back to Advisory, at the host's address
     followed by /#/guardrails/policies?rule=design.org-system: it is still Blocking, and blocks an
     app with a page of its own. Then in their project: canyon skill install, which prints every
     file it writes, then ask their coding agent to deploy. It goes to this same Canyon.
  2. Put this in my pipeline. Connect GitHub and make Canyon gate a required check on the default
     branch, so nothing merges unreviewed. Canyon also reads the reports of scanners they already
     run, and hands production to their own Argo CD or Flux through a deploy repo. Canyon's docs
     have these under Build, GitHub and under Self-host, Hand-off.
  3. Set this up for my team. This demo has sign-in off and keeps nothing. Canyon's docs, under
     Self-host, have one Kubernetes node with the Helm chart and one Linux server with Docker. On a
     team's Canyon, laptops connect with the CLI as here, and coding agents in the cloud, such as
     Claude Code on the web, connect by its address followed by /mcp.
  4. Run real workloads. Canyon on their own Kubernetes cluster, with their registry, gateway,
     identity provider and Postgres. Canyon's docs, under Self-host, have what production needs.

Then say in one line: Canyon's docs, under Get started, Connect your agent, show how to connect
their other coding agents: on a laptop with canyon skill install, in the cloud by a team Canyon's
address.

Last, say that canyon demo down stops the demo and frees the disk. Run it only if asked.
`,
}

var tourHuman = map[int]string{
	tourFirst: `
  Canyon tour · chapter 1 of 3 · Standards: your agent ignores the design system

  Canyon checks AI-built apps at three stages: when a coding agent deploys, on every pull request,
  and before production. The three chapters show three kinds of rule: the company's standards,
  security, and what production needs.

  You make the company's design system mandatory in the dashboard, with one click. Then you ask your
  coding agent for a claims tracker. The tour hands it the quick version an agent often writes, with
  a page of its own making. Canyon rejects the deploy, and nothing is built until the page uses the
  design system.

  The tour is made for a coding agent to run. Paste this into yours:

    Give me the Canyon tour. Run canyon demo tour and follow it. One chapter at a time; ask me before each.

  To walk through it by hand instead:

    canyon skill install       Canyon's skill, hook and tools for your coding agent
    canyon fleet               what is running, and the link to the dashboard
    (open http://localhost:7777/#/guardrails/policies?rule=design.org-system, set the marked
     row to Blocking, and press Save)
    canyon manifest            the rules a coding agent is handed
    canyon demo tour plant     writes the app into ./canyon-tour
    cd canyon-tour
    canyon deploy              blocked: the page is not in the design system
    canyon demo tour plant     writes the page again, in the design system
    canyon deploy              live in preview
    canyon demo tour next      on to chapter 2
    canyon demo tour skip      on to chapter 2 without doing this one
`,
	tourSecurity: `
  Canyon tour · chapter 2 of 3 · Security: your agent writes a flaw

  The same review, a different kind of rule. You ask for search by policy number. The tour plants
  the quick version an agent often writes: it builds its SQL from the request. Canyon blocks the
  deploy and names the file, the line and the fix.

  To walk through it by hand, in ./canyon-tour:

    canyon demo tour plant     writes the planted search
    canyon deploy              blocked: read what Canyon found
    (fix the one line in server.js that the finding names)
    canyon check               passes
    canyon deploy              live in preview
    canyon demo tour next      on to chapter 3
`,
	tourProd: `
  Canyon tour · chapter 3 of 3 · Production: the company's rules decide

  The chapters before were about the code. Production is about the company: a passing review, a
  person who asked, an owner, an approved stack.

  To walk through it by hand, in ./canyon-tour:

    canyon status              what runs in preview, and what production needs
    canyon deploy --env prod   live in production
    canyon fleet               the app among the others, and the link to the dashboard
    canyon demo tour next      on to the end
`,
	tourEnd: `
  Canyon tour · the end · Take it to real work

  You saw Canyon reject an app that ignored the design system, block a flaw before anything was
  built, and hold production to the company's rules. On a pull request, with GitHub connected, it
  runs the same review.

  Where to go from here:

    Try this on your own app      First set the design-system rule back to Advisory: it still
                                  blocks an app with a page of its own. Open
                                  http://localhost:7777/#/guardrails/policies?rule=design.org-system.
                                  Then in your project: canyon skill install, and ask your coding
                                  agent to deploy. It goes to this same Canyon.
    Put this in your pipeline     Connect GitHub and make Canyon gate a required check. Canyon's
                                  docs: Build, GitHub, and Self-host, Hand-off.
    Set this up for your team     This demo has sign-in off and keeps nothing. Canyon's docs,
                                  under Self-host, have one Kubernetes node and one Linux server.
                                  Cloud agents connect to it by address; laptops use the CLI.
    Run real workloads            Canyon on your own Kubernetes cluster. Canyon's docs, under
                                  Self-host, have the install and what production needs.

  To connect your other coding agents, with the CLI on a laptop or by a team Canyon's address in
  the cloud: Canyon's docs, Get started, Connect your agent.

  To stop the demo: canyon demo down.
  canyon demo tour reset starts the tour again.
`,
}
