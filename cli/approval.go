package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// approvalAsked is the promotions route's answer where the org has a person approve production in the
// dashboard (contracts/routes.md, Production approvals): the code, its page and when it expires.
type approvalAsked struct {
	Code      string `json:"code"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

// approvalRead is what --wait reads of it: its state, who decided, the run an approval started, and
// the hold that outdated it.
type approvalRead struct {
	State     string    `json:"state"`
	Run       string    `json:"run"`
	DecidedBy *actorRef `json:"decided_by"`
	Held      *prodHold `json:"held"`
}

// approvalEvery is how often --wait asks: a person reading a page is not in a hurry, and the host is
// asked nothing heavier than one row.
const approvalEvery = 5 * time.Second

// awaitApproval says where the person approves and, with wait, waits for their answer: approved, it
// follows the promotion the approval started; any other end promotes nothing. Nothing was promoted
// until then, which a script must not read as done: exit 2 (cli.md §6.3).
func (a *app) awaitApproval(ctx context.Context, client *Client, at string, read statusRead, out promoted, asked approvalAsked, wait bool) int {
	if !wait {
		if a.render.Format == FormatAI {
			a.aiPromoteHead("WAITING FOR APPROVAL", out.app, out.host)
			a.printf("version: %s\nreplaces: %s\napproval url: %s\ncode: %s\nexpires: %s\n", cell(out.version), cell(out.replaced), cell(asked.URL), cell(asked.Code), cell(asked.ExpiresAt))
			steps := aiApprovalSteps
			if out.preview != "" { // first, as the question's line says it to a person (#1833)
				steps = append([]string{"Tell the user first: preview " + out.preview + "; the person who approves should check the app first."}, steps...)
			}
			a.printf("outcome: nothing was promoted. This org has a person approve production in Canyon's dashboard.\n\n%s\nexit code: %d\n", numbered(steps), exitBlocked)
			return exitBlocked
		}
		a.askedFor(asked)
		a.printf("\n  Nothing was promoted yet. %s --wait waits here for the answer.\n", out.again())
		return exitBlocked
	}
	if a.render.Format != FormatAI {
		a.askedFor(asked)
	}
	a.printf("\n  Waiting for a person to approve… Ctrl-C stops waiting; the approval stays open.\n")
	spin := a.spinner()
	spin.phase("Waiting for a person to approve")
	var got approvalRead
	for {
		err := patiently(ctx, a.clock, a.clock.now().Add(2*retryFor), retryFor, func() error { // a restart while it waits is no answer (#2730)
			return client.get(ctx, "/api/v1/promotion-approvals/"+url.PathEscape(asked.Code), &got)
		})
		var api *APIError
		if errors.As(err, &api) && api.Status == http.StatusNotFound {
			got.State = "replaced"
		} else if lost := (*lostHost)(nil); errors.As(err, &lost) { // a person may have approved it meanwhile, which starts the promotion: never the demo's "Is the demo running?" (#2730)
			spin.stop(a)
			return a.approvalUnknown(out, lost)
		} else if err != nil {
			spin.stop(a)
			return a.fail(err)
		}
		if got.State != "pending" {
			break
		}
		a.clock.sleep(approvalEvery)
	}
	spin.stop(a)
	who := "a person"
	if got.DecidedBy != nil {
		who = cell(got.DecidedBy.name()) // with sign-in off, someone not signed in (#2705)
	}
	if h := got.Held; got.State == "outdated" && h != nil && h.who() != "" && h.Reason != "" { // a hold outdated it: production was held, not moved (#2257)
		return a.heldReceipt(ctx, client, out.app, *h)
	}
	if got.State == "approved" {
		if a.render.Format != FormatAI {
			a.printf("\n  %s Approved by %s\n", a.render.glyph(statePass), who)
		}
		return a.followPromotion(ctx, client, at, read, out, got.Run, a.clock.now())
	}
	said, ai := map[string][2]string{ // what a person reads, and the ai block's outcome
		"denied":   {a.render.glyph(stateFail) + " Denied by " + who + ". Nothing was promoted.", who + " denied the approval. Nothing was promoted."},
		"expired":  {"The approval expired. Nothing was promoted. Run: " + out.again(), "the approval expired. Nothing was promoted."},
		"outdated": {"Preview runs another version now. Nothing was promoted. Run: canyon deploy --env prod", "preview runs another version now. Nothing was promoted."},
		"replaced": {"This approval is gone: a newer request replaced it, or the app was deleted. Nothing was promoted.", "the approval is gone: a newer request replaced it, or the app was deleted. Nothing was promoted."},
	}[got.State], ""
	if got.State == "outdated" && out.putBack != "" { // a rollback's approval is outdated once production moved (contracts/routes.md, Production approvals)
		said = [2]string{"Production runs another version now. Nothing was promoted. Run: " + out.again(), "production runs another version now. Nothing was promoted."}
	}
	if said[0] == "" { // a state this build does not know: never read as approved
		said = [2]string{fmt.Sprintf("The approval is %s. Nothing was promoted.", cell(got.State)), fmt.Sprintf("the approval is %s. Nothing was promoted.", cell(got.State))}
	}
	ai = said[1]
	if a.render.Format == FormatAI {
		a.aiPromoteHead("BLOCKED", out.app, out.host)
		a.printf("outcome: %s\nfleet url: %s\n\n%s\nexit code: %d\n", ai, cell(out.host+"/#/apps/"+out.app), aiPromoteBlockedSteps, exitBlocked)
		return exitBlocked
	}
	a.printf("\n  %s\n", said[0])
	return exitBlocked
}

// askedFor is where the person approves, as canyon login says where to sign in.
func (a *app) askedFor(asked approvalAsked) {
	a.printf("\n  This org has a person approve production in Canyon.\n")
	a.printf("  Ask a person to approve: %s\n", cell(asked.URL))
	left := "expires " + cell(asked.ExpiresAt)
	if t, err := time.Parse(time.RFC3339, asked.ExpiresAt); err == nil {
		left = "expires in " + hoursLeft(t.Sub(a.clock.now()))
	}
	a.printf("  Code %s %s %s\n", cell(asked.Code), a.dot(), left)
}

// hoursLeft is how long an approval has left, in whole hours, or under one.
func hoursLeft(d time.Duration) string {
	switch h := int(d.Round(time.Hour) / time.Hour); {
	case h < 1:
		return "less than an hour"
	case h == 1:
		return "1 hour"
	default:
		return fmt.Sprintf("%d hours", h)
	}
}

var aiApprovalSteps = []string{
	"Stop. Give the user the approval url and say a person on the app's team approves it in Canyon, signed in.",
	"Never open, click or approve the approval url yourself, and do not try another way to promote: only a person approves production here.",
	"When the user says it is approved, run canyon status to see production.",
}

// approvalUnknown is exit 3 for an approval wait whose host stopped answering (#2730). An approval starts the promotion
// on the host, so in that silence a person may have approved it, denied it or let it expire: this command doesn't know
// what production runs now, and canyon status does.
func (a *app) approvalUnknown(out promoted, lost *lostHost) int {
	if a.render.Format == FormatAI {
		a.aiPromoteHead(aiUnknownResult, out.app, out.host)
		a.printf("reason: %s\noutcome: the approval was still open when Canyon last answered. A person may have approved it since.\nfleet url: %s\n",
			cell(lost.Error()), cell(out.host+"/#/apps/"+out.app))
		a.printf("\n%s\nexit code: %d\n", numbered([]string{
			"Run canyon status: its production line says what production runs. If canyon status can't reach Canyon either, tell the user and stop.",
			"Tell the user what canyon status says.",
			"Do not promote again or take production offline because of this.",
		}), exitStillRunning)
		return exitStillRunning
	}
	a.errf("\n  %s\n  The approval was still open when Canyon last answered. A person may have approved it since: canyon status says what production runs.\n  Don't promote again or take production offline because of this.\n", cell(lost.Error()))
	return exitStillRunning
}
