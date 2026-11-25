//go:build stack

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFirstCallAfterADeploy measures #144, and fixes nothing: on Docker a new container's address reaches the check
// only by a refetch (design/EGRESS.md §3.2), so its first call out waits on one, up to 4 s, and an address the server
// named no one for is refused unknown_caller for 15 s after. It deploys CANYON_E2E_FIRST_CALLS fresh apps one at a
// time, then as many again firstCallsAtOnce at a time, which churns the apps' networks. Each app's first act is a call
// through Envoy to the public stand-in, tried again every 500 ms until one goes through or a minute has passed. It
// prints how each phase's first calls were answered, how long they waited, how long a refused one took to get a call
// through, and egress-control's refetches by result over the run. A measurement, asked for alone: unset, it skips,
// and the floor's suite never names it.
func TestFirstCallAfterADeploy(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("CANYON_E2E_FIRST_CALLS"))
	if err != nil || n < 1 {
		t.Skip("a measurement (#144), run alone: CANYON_E2E_FIRST_CALLS=<deploys a phase>")
	}
	floorUp(t)
	metricsOn(t)
	setVia(t, "canyon")
	t.Cleanup(func() { setVia(t, "off") })
	before := refetches(t)
	quiet := firstCalls(t, "q", n, 1)
	busy := firstCalls(t, "b", n, firstCallsAtOnce)
	after := refetches(t)

	var out strings.Builder
	report := func(phase string, calls []firstCall) {
		fmt.Fprintf(&out, "%s: %d deploys\n", phase, len(calls))
		answers, waits, through := map[string]int{}, []time.Duration{}, []string{}
		for _, c := range calls {
			answers[c.first]++
			waits = append(waits, c.wait)
			if c.first != "200" {
				through = append(through, c.app+" "+c.through)
			}
		}
		keys := make([]string, 0, len(answers))
		for k := range answers {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "  first call %s: %d\n", k, answers[k])
		}
		if len(waits) > 0 {
			slices.Sort(waits)
			fmt.Fprintf(&out, "  first call waited: median %v, 90th %v, longest %v\n", waits[len(waits)/2], waits[len(waits)*9/10], waits[len(waits)-1])
		}
		for _, line := range through {
			fmt.Fprintf(&out, "  refused first, then a call through after: %s\n", line)
		}
	}
	report("quiet, one at a time", quiet)
	report(fmt.Sprintf("busy, %d at a time", firstCallsAtOnce), busy)
	for _, r := range []string{"known", "unknown", "failed"} {
		fmt.Fprintf(&out, "canyon_egress_refetches_total{result=%q}: %d before, %d after\n", r, before[r], after[r])
	}
	t.Log("\n" + out.String())
	if got := len(quiet) + len(busy); got < 2*n {
		t.Errorf("%d of %d deploys measured: each one missing failed above", got, 2*n)
	}
}

// firstCallsAtOnce is the busy phase's deploys at once: the executor creates and removes a network for each.
const firstCallsAtOnce = 4

// firstCall is one deploy's: its first call's answer — the status and the Canyon-Egress reason, or curl-style error —
// how long it took, and for one refused how long from it until a call went through, or "never" within the minute.
type firstCall struct {
	app, first, through string
	wait                time.Duration
}

// firstCalls deploys n fresh apps, atOnce at a time, each removed once read, and answers each one measured.
func firstCalls(t *testing.T, phase string, n, atOnce int) []firstCall {
	t.Helper()
	var mu sync.Mutex
	var calls []firstCall
	slots := make(chan struct{}, atOnce)
	t.Run(phase, func(t *testing.T) {
		for i := range n {
			app := fmt.Sprintf("e2e-fc-%s-%s%03d", tag, phase, i)
			t.Run(app, func(t *testing.T) {
				t.Parallel()
				slots <- struct{}{}
				defer func() { <-slots }()
				t.Cleanup(func() { forget(t, app) })
				c := firstCallOf(t, app)
				mu.Lock()
				calls = append(calls, c)
				mu.Unlock()
			})
		}
	})
	return calls
}

// firstCallOf deploys app, whose first act is the call, and reads its log until a call went through or it gave up.
func firstCallOf(t *testing.T, app string) firstCall {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"canyon.toml":  "app = \"" + app + "\"\n",
		"package.json": `{"name": "` + app + `", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}, "engines": {"node": "22"}}` + "\n",
		"server.js":    firstCallApp,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, out := api(t, "POST", "/api/v1/projects", map[string]string{"id": app}); code != 201 && !madeAlready(code, out) {
		t.Fatalf("POST /api/v1/projects %s: %d %s", app, code, out)
	}
	deployed, code := cliJSON(t, dir, "deploy")
	if code != 0 || deployed.Deployment == nil {
		t.Fatalf("deploy %s: exit %d, %s, deployment %+v", app, code, deployed.Result, deployed.Deployment)
	}
	var lines []string
	waitFor(t, 2*time.Minute, app+"'s calls to end", func() bool {
		lines = nil
		ended := false
		for line := range strings.SplitSeq(docker(t, "logs", app+"-preview"), "\n") {
			if f := strings.Fields(line); len(f) >= 5 && f[0] == "first-call" {
				lines = append(lines, line)
			}
			ended = ended || line == "first-call-end"
		}
		return ended && len(lines) > 0
	})
	// first-call <try> <started ms> <ended ms> <status or error> [<Canyon-Egress reason>]
	f := strings.Fields(lines[0])
	started, _ := strconv.Atoi(f[2])
	ended, _ := strconv.Atoi(f[3])
	c := firstCall{app: app, first: strings.Join(f[4:], " "), wait: time.Duration(ended-started) * time.Millisecond, through: "never"}
	if last := strings.Fields(lines[len(lines)-1]); last[4] == "200" {
		end, _ := strconv.Atoi(last[3])
		c.through = (time.Duration(end-started) * time.Millisecond).String()
	}
	return c
}

// firstCallApp answers on $PORT and, as it starts, calls the public stand-in through the proxy its /run named, in the
// absolute form a proxy takes, logging each try; again every 500 ms until one goes through, for a minute at most.
const firstCallApp = `const http = require("node:http");
const t0 = Date.now();
http.createServer((_, res) => res.end("ok\n")).listen(process.env.PORT || 3000);
const proxy = new URL(process.env.HTTP_PROXY || "http://no-proxy-named:3128");
let tries = 0;
const again = () => { if (Date.now() - t0 < 60000) setTimeout(call, 500); else console.log("first-call-end"); };
const call = () => {
  const n = ++tries, at = Date.now() - t0;
  const req = http.get({ host: proxy.hostname, port: proxy.port, path: "http://` + publicName + `/etc/dnsd.conf", headers: { host: "` + publicName + `" }, timeout: 10000 }, (res) => {
    res.resume();
    console.log("first-call " + n + " " + at + " " + (Date.now() - t0) + " " + res.statusCode + " " + (res.headers["canyon-egress"] || ""));
    if (res.statusCode !== 200) again(); else console.log("first-call-end");
  });
  req.on("timeout", () => req.destroy(new Error("timeout")));
  req.on("error", (e) => { console.log("first-call " + n + " " + at + " " + (Date.now() - t0) + " " + (e.code || e.message)); again(); });
};
call();
`

// metricsOn starts egress-control again serving its metrics on the holder's canyon-egress-ctl address, which the
// shipped compose file never does, and puts it back as it was at the end. Starting it again empties its callers: the
// measurement starts from none held.
func metricsOn(t *testing.T) {
	t.Helper()
	override := filepath.Join(t.TempDir(), "metrics.yml")
	if err := os.WriteFile(override, []byte("services:\n  egress-control:\n    environment:\n      - CANYON_EGRESS_CONTROL_METRICS_ADDR="+metricsAddr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	compose(t, "-f", override, "up", "-d", "--no-deps", "--no-build", "egress-control")
	t.Cleanup(func() { compose(t, "up", "-d", "--no-deps", "--no-build", "egress-control"); controlUp(t) })
	controlUp(t)
}

const metricsAddr = "10.214.3.3:9464" // the holder's pinned address on canyon-egress-ctl (docker-compose.yml)

// refetches is egress-control's canyon_egress_refetches_total by result, read from canyon-egress-ctl.
func refetches(t *testing.T) map[string]int {
	t.Helper()
	got := map[string]int{}
	for line := range strings.SplitSeq(docker(t, "run", "--rm", "--network", ctl, curl, "-sS", "-m", "10", "http://"+metricsAddr+"/metrics"), "\n") {
		name, value, ok := strings.Cut(line, " ")
		if r, found := strings.CutPrefix(name, `canyon_egress_refetches_total{result="`); ok && found {
			v, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("%s: %v", line, err)
			}
			got[strings.TrimSuffix(r, `"}`)] = v
		}
	}
	if len(got) != 3 {
		t.Fatalf("egress-control's metrics name %d refetch results, not known, unknown and failed: %v", len(got), got)
	}
	return got
}
