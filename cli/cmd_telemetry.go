package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"time"
)

// telemetryState is GET and PUT /api/v1/telemetry's answer (contracts/schema/telemetry.schema.json, state): report is
// kept as it came, the very bytes the server's next send POSTs.
type telemetryState struct {
	Enabled    bool   `json:"enabled"`
	OffBecause string `json:"off_because"`
	NextTryAt  string `json:"next_try_at"` // with GivesUpAt, while no report has reached the collector
	GivesUpAt  string `json:"gives_up_at"`
	Report     string `json:"report"` // the send's exact bytes, printed as they are
}

// telemetryOff is why the report is not sent, as show says it.
var telemetryOff = map[string]string{
	"env":         "Telemetry is off: this server's environment turns it off (CANYON_TELEMETRY, DO_NOT_TRACK or CI).",
	"admin":       "Telemetry is off: an admin turned it off.",
	"unreachable": "Telemetry is off: it couldn't reach Canyon's collector. An admin turns it back on in Canyon's dashboard: Settings › Telemetry.",
}

// runTelemetry is canyon telemetry's three forms (cli.md §6.12; #2807): show prints the next report exactly as it
// would be sent, on and off switch it. All three are an admin's; the host refuses anyone else.
func runTelemetry(ctx context.Context, a *app, args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		a.telemetryForms(a.out)
		return exitOK
	}
	form, rest := args[0], args[1:]
	if form != "show" && form != "on" && form != "off" {
		a.errf("\n  Unknown telemetry command %q.\n", form)
		a.telemetryForms(a.err)
		return exitError
	}
	a.form = "telemetry " + form
	extra, err := a.parse(rest, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(extra) > 0 {
		return a.usage(fmt.Errorf("telemetry %s takes no arguments, and got %q", form, extra[0]))
	}
	client, err := a.client()
	if err != nil {
		return a.usage(err)
	}
	var raw json.RawMessage
	if form == "show" {
		err = client.get(ctx, "/api/v1/telemetry", &raw)
	} else {
		err = client.put(ctx, "/api/v1/telemetry", map[string]bool{"enabled": form == "on"}, http.StatusOK, &raw)
	}
	var s telemetryState
	if err == nil {
		err = json.Unmarshal(raw, &s)
	}
	if err == nil && len(s.Report) == 0 {
		err = errors.New("the host's answer carries no report")
	}
	var api *APIError
	if form == "on" && errors.As(err, &api) && api.Code == "dashboard_required" && a.render.Format != FormatJSON {
		a.printf("%s An admin turns telemetry on in Canyon's dashboard: Settings › Telemetry.\n", a.render.glyph(stateWarn))
		return exitBlocked // a person turns it on, in the dashboard (#2807)
	}
	if err != nil {
		return a.fail(err)
	}
	if a.render.Format == FormatJSON {
		return a.json(raw)
	}
	ok := a.render.paint(ansiGreen, a.render.glyph(statePass))
	switch {
	case form == "show":
		next, nextErr := time.Parse(time.RFC3339, s.NextTryAt)
		until, untilErr := time.Parse(time.RFC3339, s.GivesUpAt)
		if s.Enabled && nextErr == nil && untilErr == nil {
			a.printf("Telemetry is on, but no report has reached Canyon's collector yet. Canyon tries again after %s and gives up by %s. It would send:\n%s\n",
				next.Local().Format("2 Jan 15:04"), until.Local().Format("2 Jan"), s.Report)
		} else if s.Enabled {
			a.printf("Telemetry is on. Canyon sends this report once a day, exactly as it reads here:\n%s\n", s.Report)
		} else {
			a.printf("%s It would send:\n%s\n", a.offLine(s.OffBecause), s.Report)
		}
	case form == "off":
		a.printf("%s Telemetry is off: Canyon sends nothing.\n", ok)
	case s.Enabled:
		a.printf("%s Telemetry is on: Canyon sends one report a day.\n", ok)
	default:
		a.printf("%s %s\n", a.render.glyph(stateWarn), a.offLine(s.OffBecause))
	}
	return exitOK
}

// offLine is show's line for why it is off; a reason this build doesn't know is said as the host gave it.
func (a *app) offLine(why string) string {
	if line, ok := telemetryOff[why]; ok {
		return line
	}
	return "Telemetry is off (" + cell(why) + ")."
}

func (a *app) telemetryForms(w io.Writer) {
	p := func(f string, v ...any) { _, _ = fmt.Fprintf(w, f, v...) }
	p("\n%s\n", a.render.paint(ansiBold, "USAGE"))
	p("  canyon telemetry show         The report this Canyon sends next, exactly as it is sent\n")
	p("  canyon telemetry off          Stop sending it\n")
	p("  canyon telemetry on           Send it again\n")
}
