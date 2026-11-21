package cli

import (
	"context"
	"errors"
	"time"
)

// stopped is why a verb stopped following a run that goes on in Canyon (cli.md §3.5): the host unanswered for
// retryFor (lost), or the run past the verb's ceiling, said of noun; "" when err is neither.
func stopped(err error, noun string) (reason string, lost *lostHost, late *pastCeiling) {
	switch {
	case errors.As(err, &lost):
		return lost.Error(), lost, nil
	case errors.As(err, &late) && late.ceiling < time.Minute: // /mcp's hand-off (mcptools.RemoteHandoff)
		return "the " + noun + " is still running after " + plural(int(late.ceiling.Seconds()), "second", "seconds") + ", the longest one call waits.", nil, late
	case errors.As(err, &late):
		return "the " + noun + " is still running after " + plural(int(late.ceiling.Minutes()), "minute", "minutes") + ", the longest this command waits.", nil, late
	}
	return "", nil, nil
}

// readRun is the run's read after its poll, asked as patiently as the poll was (#2730): a host that answered the
// poll's last page may be restarting by the next read.
func (a *app) readRun(ctx context.Context, c *Client, id string, v any) error {
	return patiently(ctx, a.clock, a.clock.now().Add(2*retryFor), retryFor, func() error { return c.get(ctx, "/api/v1/gate-runs/"+id, v) })
}

// stillRunning is cli.md §3.5's exit 3 (#2721): a preview deploy this command stopped following, which goes on in
// Canyon. Exit 1 read as a failed deploy, and an agent that cleaned up after it took a running one down. false when
// err is no reason to stop following.
func (a *app) stillRunning(run checkRun, id string, seen timeline, err error) (int, bool) {
	reason, lost, late := stopped(err, "deploy")
	if reason == "" {
		return 0, false
	}
	page := run.Host + "/#/apps/" + run.App
	switch {
	case a.render.Format == FormatAI && a.remote: // on /mcp the call ends while the run goes on, and canyon_review reads it
		return a.aiStillRunning(run, id, seen, reason, "the deploy is still running in Canyon. This call stopped following it.",
			"Tell the user the deploy is still running in Canyon.",
			"Call canyon_review with app "+cell(run.App)+" and run "+cell(id)+" in a minute, until it says the verdict. Then tell the user what it says.",
			"Do not deploy again or take the app offline for this. Nothing failed."), true
	case a.render.Format == FormatAI: // its own block replaces the parked transcript, and stderr stays empty
		return a.aiStillRunning(run, id, seen, reason, "the deploy was still running when Canyon last answered. This command stopped following it.",
			"Tell the user the deploy was still running when Canyon last answered, and this command stopped following it.",
			"Run canyon status until its review line for run "+cell(id)+" no longer says reviewing. Then tell the user what it says, and what preview runs. If canyon status can't reach Canyon either, tell the user and stop.",
			"Do not deploy again or take the app offline for this. This command did not see it fail."), true
	case lost != nil:
		a.errf("\n  %s\n  The deploy was still running when Canyon last answered, and canyon status says how it ends.\n  Don't deploy again or take the app offline for this.\n  %s\n", cell(reason), cell(page))
	case run.Reviewed: // the review is in, printed above: what goes on is the build, a first one on a slow line among them (#2697)
		a.errf("\n  %s is still building after %s, and it carries on without this command: canyon status says when it's live.\n",
			cell(run.App), plural(int(late.ceiling.Minutes()), "minute", "minutes"))
	default:
		a.fail(err)
		a.errf("\n  The deploy keeps going without this terminal:\n  %s\n", cell(page)) // its page, never its run id
	}
	return exitStillRunning, true
}

// reviewStillRunning is exit 3 for a review this command stopped following (#2730): a check's, which changes
// nothing, or a rollback's, which puts nothing back until this command promotes it. Either is the agent's to run
// again once Canyon answers, never a failure of the app's.
func (a *app) reviewStillRunning(run checkRun, id string, seen timeline, err error) (int, bool) {
	reason, lost, _ := stopped(err, "review")
	if reason == "" {
		return 0, false
	}
	page := run.Host + "/#/apps/" + run.App
	again := "canyon " + a.verb.name
	if run.Rollback != nil {
		v := run.Rollback.version
		if a.render.Format == FormatAI {
			return a.aiStillRunning(run, id, seen, reason, "nothing was put back in production. The review of "+v+" was still running when Canyon last answered.",
				"Tell the user Canyon stopped answering while it reviewed "+v+", and that production runs what it ran before.",
				"Run "+again+" again in a minute. If it can't reach Canyon either, tell the user and stop.",
				"Do not change the app's code for this."), true
		}
		if lost != nil {
			a.errf("\n  %s\n  Nothing was put back in production: the review of %s was still running when Canyon last answered.\n  Run %s again once Canyon answers.\n  %s\n", cell(reason), v, again, cell(page))
			return exitStillRunning, true
		}
	} else {
		if a.render.Format == FormatAI && a.remote { // on /mcp the call ends while the review goes on, and canyon_review reads it
			return a.aiStillRunning(run, id, seen, reason, "the review is still running in Canyon. A check changes nothing in preview or production.",
				"Tell the user the review is still running in Canyon.",
				"Call canyon_review with app "+cell(run.App)+" and run "+cell(id)+" in a minute, until it says the verdict. Then tell the user what it says.",
				"Do not change the app's code or check again for this."), true
		}
		if a.render.Format == FormatAI {
			return a.aiStillRunning(run, id, seen, reason, "the review was still running when Canyon last answered. A check changes nothing in preview or production.",
				"Tell the user Canyon stopped answering while it reviewed the app, and that a check changes nothing.",
				"Run canyon check again in a minute. If it can't reach Canyon either, tell the user and stop.",
				"Do not change the app's code for this."), true
		}
		if lost != nil {
			a.errf("\n  %s\n  The review was still running when Canyon last answered. A check changes nothing, so run canyon check again once Canyon answers.\n  %s\n", cell(reason), cell(page))
			return exitStillRunning, true
		}
	}
	a.fail(err)
	a.errf("\n  The review keeps going without this terminal:\n  %s\n", cell(page)) // its page, never its run id
	return exitStillRunning, true
}

// aiStillRunning is §3.5's block for a run this command stopped following, CANYON REVIEW's: the head, the run, why it
// stopped, the outcome and the steps, its exit 3 printed as text.
func (a *app) aiStillRunning(run checkRun, id string, seen timeline, reason, outcome string, steps ...string) int {
	return a.aiNotFollowed(aiStillRunningResult, run, id, seen, reason, outcome, steps...)
}

// aiNotFollowed is the block of a run this command doesn't know the end of, under result: STILL RUNNING, or UNKNOWN for
// one Canyon said had ended (cli.md §3.5).
func (a *app) aiNotFollowed(result string, run checkRun, id string, seen timeline, reason, outcome string, steps ...string) int {
	a.unpark(false)
	run.Run, run.StartedAt = id, seen.StartedAt
	a.aiHead(run, result)
	a.aiGateRun(run)
	recap := []string{"reason: " + cell(reason)}
	a.printf("%s\noutcome: %s\nfleet url: %s\n", recap[0], outcome, cell(run.Host+"/#/apps/"+run.App))
	return a.aiEnd(numbered(steps), result, recap, exitStillRunning)
}

// promotionStillRunning is exit 3 for a promotion this command stopped following (#2730): it goes on in Canyon, and
// production changes when it ends.
func (a *app) promotionStillRunning(out promoted, err error) (int, bool) {
	reason, lost, late := stopped(err, "promotion")
	if reason == "" {
		return 0, false
	}
	page := out.host + "/#/apps/" + out.app
	switch {
	case a.render.Format == FormatAI:
		a.aiPromoteHead(aiStillRunningResult, out.app, out.host)
		a.printf("run: %s\nreason: %s\noutcome: the promotion was still running when Canyon last answered. This command stopped following it.\nfleet url: %s\n",
			cell(out.run), cell(reason), cell(page))
		look := "Run canyon status in a minute: its production line says what production runs. If canyon status can't reach Canyon either, tell the user and stop."
		if a.remote {
			look = "Call canyon_status with app " + cell(out.app) + " in a minute: its production line says what production runs."
		}
		a.printf("\n%s\nexit code: %d\n", numbered([]string{
			"Tell the user the promotion was still running when Canyon last answered, and this command stopped following it.",
			look,
			"Do not promote again or take production offline for this. This command did not see it fail.",
		}), exitStillRunning)
	case lost != nil:
		a.errf("\n  %s\n  The promotion was still running when Canyon last answered, and canyon status says how it ends.\n  Don't promote again or take production offline for this.\n  %s\n", cell(reason), cell(page))
	default:
		a.errf("\n  The promotion is still running after %s, and it carries on without this command: canyon status says when it's live.\n",
			plural(int(late.ceiling.Minutes()), "minute", "minutes"))
	}
	return exitStillRunning, true
}

// The blocks' results (cli.md §3.5): a run that goes on, and one Canyon said ended whose end this command couldn't read.
const (
	aiStillRunningResult = "STILL RUNNING"
	aiUnknownResult      = "UNKNOWN"
)

// endedUnread is exit 3 for a run Canyon said had ended, whose result this command then couldn't read: the host
// unanswered when it asked how the run ended (#2730). Nothing says it still runs, so the lines say what is known.
// false when err is no lost host.
func (a *app) endedUnread(run checkRun, id string, seen timeline, noun string, err error) (int, bool) {
	var lost *lostHost
	if !errors.As(err, &lost) {
		return 0, false
	}
	reason := "Canyon said the " + noun + " ended, then didn't answer for " + plural(int(retryFor.Minutes()), "minute", "minutes") +
		" when this command asked how it ended (" + unansweredBy(lost.err) + ")."
	page := run.Host + "/#/apps/" + run.App
	again := "canyon " + a.verb.name
	ai := a.render.Format == FormatAI
	switch {
	case run.Rollback != nil && ai:
		return a.aiNotFollowed(aiUnknownResult, run, id, seen, reason, "nothing was put back in production. Canyon said the review of "+run.Rollback.version+" ended, and this command couldn't read how.",
			"Run "+again+" again in a minute. If it can't reach Canyon either, tell the user and stop.",
			"Do not change the app's code for this."), true
	case run.Rollback != nil:
		a.errf("\n  %s\n  Nothing was put back in production. Run %s again once Canyon answers.\n  %s\n", cell(reason), again, cell(page))
	case a.verb.name == "check" && ai:
		return a.aiNotFollowed(aiUnknownResult, run, id, seen, reason, "Canyon said the review ended, and this command couldn't read how. A check changes nothing in preview or production.",
			"Run canyon check again in a minute. If it can't reach Canyon either, tell the user and stop.",
			"Do not change the app's code for this."), true
	case a.verb.name == "check":
		a.errf("\n  %s\n  A check changes nothing, so run canyon check again once Canyon answers.\n  %s\n", cell(reason), cell(page))
	case ai:
		return a.aiNotFollowed(aiUnknownResult, run, id, seen, reason, "Canyon said the deploy ended, and this command couldn't read how.",
			"Run canyon status: its review line for run "+cell(id)+" and its preview line say how the deploy ended. If canyon status can't reach Canyon either, tell the user and stop.",
			"Tell the user what canyon status says.",
			"Do not deploy again or take the app offline because of this."), true
	default:
		a.errf("\n  %s\n  canyon status says how it ended.\n  Don't deploy again or take the app offline because of this.\n  %s\n", cell(reason), cell(page))
	}
	return exitStillRunning, true
}
