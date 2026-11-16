package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"
)

// licenceRead is GET /api/v1/licence (contracts/routes.md, Licence): {state, allows} to a member, all of it to an admin.
type licenceRead struct {
	State      string   `json:"state"`
	Allows     []string `json:"allows"`
	Source     string   `json:"source"`
	Customer   string   `json:"customer"`
	Seats      int      `json:"seats"`
	Expires    string   `json:"expires"`
	GraceUntil string   `json:"grace_until"`
	Features   []string `json:"features"`
	Problem    string   `json:"problem"`
}

// featureNames are the Enterprise features by their names in Settings › Licence (design/EXPERIENCE.md, D5).
var featureNames = map[string]string{"outbound_keys": "Credentials Canyon adds", "platform_patching": "Patching", "group_sync": "Access by sign-in group"}

// runLicence prints the Canyon Enterprise licence as the host answers the reader; an admin sets it in the dashboard.
func runLicence(ctx context.Context, a *app, args []string) int {
	extra, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(extra) > 0 {
		return a.usage(fmt.Errorf("licence takes no arguments, and got %q", extra[0]))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	var raw json.RawMessage
	var l licenceRead
	err = client.get(ctx, "/api/v1/licence", &raw)
	if err == nil {
		err = json.Unmarshal(raw, &l)
	}
	if err != nil {
		return a.fail(err)
	}
	if a.render.Format == FormatJSON {
		return a.json(raw)
	}
	a.printf("%s\n", licenceLine(l))
	if l.Customer != "" {
		a.printf("  %-10s %d\n  %-10s %s\n", "Seats", l.Seats, "Features", namesOf(l.Features))
	}
	return exitOK
}

// licenceLine is the line for the licence's state, an admin's as Settings › Licence says it, a member's naming only what
// is unlocked; a state this build doesn't know is said as the host gave it.
func licenceLine(l licenceRead) string {
	date := func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return cell(ts)
		}
		return t.UTC().Format("2 Jan 2006")
	}
	customer := cell(l.Customer)
	switch {
	case l.State == "none":
		return "No licence: this Canyon runs the free edition. Enterprise features need a licence."
	case l.Source == "" && len(l.Allows) > 0:
		return "Only an admin can see the licence. Unlocked on this Canyon: " + namesOf(l.Allows) + "."
	case l.Source == "":
		return "Only an admin can see the licence. No Enterprise feature is unlocked on this Canyon."
	case l.State == "valid":
		return "Canyon Enterprise, licensed to " + customer + " until " + date(l.Expires) + "."
	case l.State == "grace":
		return "Canyon Enterprise, licensed to " + customer + ", ended on " + date(l.Expires) + ". Its features stay unlocked until " + date(l.GraceUntil) + "; after that, what's set keeps working and can't be changed."
	case l.State == "lapsed":
		return "Canyon Enterprise, licensed to " + customer + ", ended on " + date(l.Expires) + ". What's set keeps working; Enterprise features can't be changed until an admin adds a new licence."
	case l.State == "invalid" && l.Problem == "unsigned":
		return "Canyon didn't sign this server's licence. What's set keeps working; changing an Enterprise feature needs a licence Canyon signed."
	case l.State == "invalid":
		return "Canyon can't read this server's licence. What's set keeps working; changing an Enterprise feature needs a licence Canyon can read."
	}
	return "The licence is " + cell(l.State) + "."
}

// namesOf is the features by name, joined; one this build doesn't know by its id.
func namesOf(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		if name, ok := featureNames[id]; ok {
			out[i] = name
		} else {
			out[i] = cell(id)
		}
	}
	return strings.Join(out, ", ")
}
