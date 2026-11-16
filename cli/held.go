package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

// prodHold is production's hold as the receipt and a 409 held carry it (contracts/routes.md, The
// hold): a responder or an admin stopped production, and every production write is refused until
// one of them lifts it, in the dashboard.
type prodHold struct {
	By     actorRef `json:"by"`
	At     string   `json:"at"`
	Reason string   `json:"reason"`
}

// who is the holder as the line names them, as every actor is named (actorRef.name, #2705).
func (h prodHold) who() string { return h.By.name() }

// line is D5's sentence (design/EXPERIENCE.md), the day UTC as the host's 409 says it.
func (h prodHold) line() string {
	day := h.At
	if t, err := time.Parse(time.RFC3339, h.At); err == nil {
		day = t.UTC().Format(time.DateOnly)
	}
	return "Production was stopped by " + h.who() + " on " + day + ": " + ended(h.Reason) + " It stays stopped until a responder or an admin lets it run again."
}

// ended is the reason as the sentence ends it, as the host says it (runs/hold.go): a full stop after it, unless it
// already ends a sentence, which read "while we look.. It stays stopped" (#2723).
func ended(reason string) string {
	if strings.HasSuffix(reason, ".") || strings.HasSuffix(reason, "?") || strings.HasSuffix(reason, "!") {
		return reason
	}
	return reason + "."
}

const aiHeldSteps = "NEXT STEPS:\n" +
	"1. Tell the user production is held, and quote the reason line. Only a security responder or an admin lets it run again, in Canyon's dashboard: the user asks one of them.\n" +
	"2. Do not try to put it in production another way, and never work around the hold. Deploying a fix to preview is allowed: canyon deploy\n"

// heldIn is a 409 held's hold and admins, and the app its route named; ok false for any other
// answer, or one whose hold this build can't read, which prints as every refusal does.
func heldIn(err error) (h prodHold, admins []string, app string, ok bool) {
	var api *APIError
	if !errors.As(err, &api) || api.Status != http.StatusConflict || api.Code != "held" {
		return h, nil, "", false
	}
	app, named := pathApp(api.Path)
	if !named || json.Unmarshal(api.Extra["hold"], &h) != nil || h.who() == "" || h.Reason == "" {
		return h, nil, "", false
	}
	_ = json.Unmarshal(api.Extra["admins"], &admins) // none named is no ask line, never a refusal unsaid
	return h, admins, app, true
}

// orgAdmins is who the 409 held names to ask, read where a receipt carries the hold without them:
// the preferences' admin list, then CANYON_ADMINS' addresses it lacks, as the host joins them.
func orgAdmins(ctx context.Context, c *Client) ([]string, error) {
	var prefs struct {
		AdminEmails     []string `json:"admin_emails"`
		BootstrapAdmins []string `json:"bootstrap_admins"`
	}
	if err := c.get(ctx, "/api/v1/preferences", &prefs); err != nil {
		return nil, err
	}
	admins := prefs.AdminEmails
	for _, e := range prefs.BootstrapAdmins {
		if !slices.Contains(admins, e) {
			admins = append(admins, e)
		}
	}
	return admins, nil
}

// heldReceipt is refusedHeld for a receipt that carries the hold without the admins: they are read
// from the preferences, and a read that fails leaves the ask line out, never the hold unsaid.
func (a *app) heldReceipt(ctx context.Context, client *Client, app string, h prodHold) int {
	admins, _ := orgAdmins(ctx, client)
	return a.refusedHeld(app, client.page(), h, admins)
}

// refusedHeld is a held production met by any verb that would change it, exit 2: D5's line and the
// admins to ask on stderr — a responder lifts it too, but who they are is answered to an admin alone
// (design/PERMISSIONS.md, Security responders), so the line names the role, never one; under ai the
// verb's block, whose steps never look for a way around it.
func (a *app) refusedHeld(app, host string, h prodHold, admins []string) int {
	a.unpark(false) // the block, or the refusal, stands alone
	ask := cell(strings.Join(admins, ", "))
	if a.render.Format != FormatAI {
		a.errf("\n  %s %s\n", a.render.glyph(stateFail), cell(h.line()))
		if ask != "" {
			a.errf("  Ask a security responder, or an admin: %s\n", ask)
		}
		return exitBlocked
	}
	a.printf("%s — RESULT: HELD\n\napp: %s\nenv: prod\nhost: %s\nheld by: %s\nheld since: %s\nreason: %s\n",
		a.head(), cell(app), cell(host), cell(h.who()), cell(h.At), cell(h.Reason))
	if ask != "" {
		a.printf("ask: %s\n", ask)
	}
	outcome := "nothing was taken offline" // undeploy's: the three verbs that write production are the ones a hold refuses
	switch verb := a.verb.name; {
	case strings.HasPrefix(verb, "deploy --env prod"): // a rollback's too
		outcome = "nothing was promoted"
	case verb == "delete":
		outcome = "nothing was deleted"
	}
	a.printf("outcome: %s. Production is stopped.\nfleet url: %s\n\n%s\nexit code: %d\n", outcome, cell(host+"/#/apps/"+app), aiHeldSteps, exitBlocked)
	return exitBlocked
}
