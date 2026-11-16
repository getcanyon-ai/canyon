package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
)

// runVersion answers the two things anyone asks of a binary they were handed: which one is this,
// and where is it pointed. `canyon --version` answers only the first and so can never fail.
func runVersion(_ context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("version takes no arguments, and got %q", rest[0]))
	}
	host, err := a.host()
	if err != nil {
		return a.usage(err)
	}
	if a.render.Format == FormatJSON {
		if err := json.NewEncoder(a.out).Encode(map[string]string{"version": Version, "host": host}); err != nil {
			return a.usage(err)
		}
		return exitOK
	}
	dot := a.render.punct("·", "|")
	if a.hostFlag == "" && a.getenv("CANYON_HOST") == "" && a.config().Host == "" { // the default rung: nobody chose it
		a.printf("Canyon CLI %s %s not signed in: it would reach %s, a local demo\nSign in to your Canyon with: canyon login <its address>\n", Version, dot, host)
		return exitOK
	}
	a.printf("Canyon CLI %s %s connected to %s\n", Version, dot, host)
	return exitOK
}
