//go:build stack

package e2e

import (
	"encoding/json"
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
// to the public stand-in, through the proxy Canyon names in its environment, as any Node app's call goes, tried again
// every 500 ms until one goes through or a minute has passed. It prints how each phase's first calls were answered,
// how long they waited, how long a refused one took to get a call through, one app's tries for each first answer, and
// egress-control's refetches by result over the run. It fails when a deploy wasn't measured, and when it measured
// nothing of #144's path: no call went through, or the check refetched no caller. A measurement, asked for alone:
// unset, it skips, and the floor's suite never names it.
func TestFirstCallAfterADeploy(t *testing.T) {
	n, err := strconv.Atoi(os.Getenv("CANYON_E2E_FIRST_CALLS"))
	if err != nil || n < 1 {
		t.Skip("a measurement (#144), run alone: CANYON_E2E_FIRST_CALLS=<deploys a phase>")
	}
	floorUp(t)
	standIn(t, "198.51.100.0/24", holder) // the public stand-in, 198.51.100.9, which publicName resolves to
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
		answers, waits, through, tries := map[string]int{}, []time.Duration{}, []string{}, map[string][]string{}
		for _, c := range calls {
			answers[c.first]++
			if _, ok := tries[c.first]; !ok {
				tries[c.first] = append([]string{c.app}, c.lines...)
			}
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
		for _, k := range keys { // first-call <try> <started ms> <ended ms> <status or error> <Canyon-Egress reason or -> server=<header> <body's start>
			fmt.Fprintf(&out, "  %s's tries, its first call %s:\n    %s\n", tries[k][0], k, strings.Join(tries[k][1:], "\n    "))
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
	if !slices.ContainsFunc(slices.Concat(quiet, busy), func(c firstCall) bool { return c.through != "never" }) {
		t.Errorf("no app's call went through in its minute: the measurement reached nothing past the floor (its tries above)")
	}
	if moved := after["known"] + after["unknown"] + after["failed"] - before["known"] - before["unknown"] - before["failed"]; moved == 0 {
		t.Errorf("egress-control refetched no caller: no new container's call reached the check, so #144's path was never taken")
	}
}

// firstCallsAtOnce is the busy phase's deploys at once: the executor creates and removes a network for each.
const firstCallsAtOnce = 4

// firstCall is one deploy's: its first call's answer — the status and the Canyon-Egress reason, or Node's error —
// how long it took, for one refused how long from it until a call went through, or "never" within the minute, and
// every try's line as the app logged it.
type firstCall struct {
	app, first, through string
	wait                time.Duration
	lines               []string
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
	deployedOnce(t, dir, app)
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
	// first-call <try> <started ms> <ended ms> <status or error> <Canyon-Egress reason or -> ...
	f := strings.Fields(lines[0])
	started, _ := strconv.Atoi(f[2])
	ended, _ := strconv.Atoi(f[3])
	first := f[4]
	if len(f) > 5 && f[5] != "-" {
		first += " " + f[5]
	}
	c := firstCall{app: app, first: first, wait: time.Duration(ended-started) * time.Millisecond, through: "never", lines: lines}
	if last := strings.Fields(lines[len(lines)-1]); last[4] == "200" {
		end, _ := strconv.Atoi(last[3])
		c.through = (time.Duration(end-started) * time.Millisecond).String()
	}
	return c
}

// firstCallApp answers on $PORT and, as it starts, calls the public stand-in by its plain URL, which Node's global agent
// sends through the proxy Canyon names (NODE_USE_ENV_PROXY, HTTP_PROXY; design/EGRESS.md), as any Node app's call
// goes. It logs each try, with the answer's server header and the start of its body so an answer says who gave it;
// again every 500 ms until one goes through, for a minute at most.
const firstCallApp = `const http = require("node:http");
const t0 = Date.now();
http.createServer((_, res) => res.end("ok\n")).listen(process.env.PORT || 3000);
let tries = 0;
const again = () => { if (Date.now() - t0 < 60000) setTimeout(call, 500); else console.log("first-call-end"); };
const call = () => {
  const n = ++tries, at = Date.now() - t0;
  const req = http.get("http://` + publicName + `/etc/dnsd.conf", { timeout: 10000 }, (res) => {
    let body = "";
    res.setEncoding("utf8");
    res.on("data", (c) => { body += c; });
    res.on("end", () => {
      console.log("first-call " + n + " " + at + " " + (Date.now() - t0) + " " + res.statusCode + " " + (res.headers["canyon-egress"] || "-") + " server=" + (res.headers.server || "-") + " " + JSON.stringify(body.slice(0, 60)));
      if (res.statusCode !== 200) again(); else console.log("first-call-end");
    });
  });
  req.on("timeout", () => req.destroy(new Error("timeout")));
  req.on("error", (e) => { console.log("first-call " + n + " " + at + " " + (Date.now() - t0) + " " + (e.code || e.message).replace(/\s+/g, "_") + " -"); again(); });
};
call();
`

// deployedOnce deploys dir under --format json. Empty stdout and exit 1 is a refusal, explained on stderr
// (design/specs/cli.md §5.4), the review limit's (60 an hour) among them: it waits a minute and asks again, for 15
// minutes at most, so a deploy the limit holds back is measured late, never missed.
func deployedOnce(t *testing.T, dir, app string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Minute); ; {
		stdout, code := canyon(t, dir, "", "--format", "json", "deploy")
		if strings.TrimSpace(stdout) == "" && code == 1 && time.Now().Before(deadline) {
			time.Sleep(time.Minute)
			continue
		}
		var out cliRun
		if err := json.Unmarshal([]byte(stdout), &out); err != nil || code != 0 || out.Deployment == nil {
			t.Fatalf("deploy %s: exit %d, %q (%v)", app, code, stdout, err)
		}
		return
	}
}

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
