package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// clock is what poll waits on: real time in the binary, a counter in the tests, so a poll over
// several pages costs no seconds.
type clock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

var wallClock = clock{now: time.Now, sleep: time.Sleep}

const (
	pollLimit = 50          // small pages, read often: one row can carry a whole SARIF log
	pollEvery = time.Second // "clients poll about once a second while a run is pending or running"

	checkCeiling  = 15 * time.Minute // the ceiling is the caller's: a build may legitimately outlast a review
	deployCeiling = 35 * time.Minute // runs.buildCeiling is 30m, so 15 would abandon a legitimate build

	platformCeiling = 10 * time.Second // canyon status's read of an ended patch run: a status waits on nobody (#2730)

	retryFor  = 2 * time.Minute  // how long a poll the host doesn't answer is sent again (cli.md §3.5, #2721)
	retryMost = 30 * time.Second // the longest wait between two of them
)

// runKinds is the run-scope catalogue of contracts/events.md, in the schema's order; a test
// holds it to the schema's enum. A row of any other kind is counted and ignored: a newer server
// says more than this build reads, and that must never stop the rest of the run from rendering.
var runKinds = []string{"run.state", "check.started", "check.progress", "check.completed",
	"producer.sarif", "progress", "scan.waiting", "verdict", "build.image", "build.log", "deploy.completed"}

// timeline is what one run's rows said: when the server started it, and how many rows this build
// could not read.
type timeline struct {
	StartedAt string
	Unknown   int
}

type timelinePage struct {
	Rows []timelineRow `json:"rows"`
	Next *int64        `json:"next"`
}

type timelineRow struct {
	ID   int64           `json:"id"`
	TS   string          `json:"ts"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"` // the kind's own shape: the renderer reads it, the poll does not
}

// pastCeiling is a poll that reached its ceiling with the run still going on the host.
type pastCeiling struct{ ceiling time.Duration }

func (e *pastCeiling) Error() string {
	return fmt.Sprintf("the review has not finished after %s", e.ceiling)
}

// lostHost is a poll the host didn't answer, sent again for retryFor: the run goes on there without this command.
type lostHost struct{ err error }

func (e *lostHost) Error() string {
	return fmt.Sprintf("Canyon didn't answer for %s (%s).", plural(int(retryFor.Minutes()), "minute", "minutes"), unansweredBy(e.err))
}

// unansweredBy is why an ask went unanswered, as a reason line names it: a gateway's status beside the host that answered it,
// as the other causes name their host; the error's own line otherwise.
func unansweredBy(err error) string {
	var api *APIError
	if errors.As(err, &api) && gateway(api.Status) {
		return fmt.Sprintf("the last answer from %s was %d %s", api.Host, api.Status, http.StatusText(api.Status))
	}
	return err.Error()
}

// gateway is a status a proxy in front of a restarting host answers: 502, 503 or 504.
func gateway(status int) bool {
	return slices.Contains([]int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}, status)
}

func (e *lostHost) Unwrap() error { return e.err }

// poll reads a run's timeline to its end, handing every row to each as it arrives so a renderer
// can narrate the run instead of replaying it afterwards. It keeps its place with the cursor the
// server answered, and stops on `next: null` and nothing else — an empty page answers the cursor
// it was sent, so "no rows this time" is a run still going (contracts/events.md).
func poll(ctx context.Context, c *Client, run string, clk clock, ceiling time.Duration, each func(timelineRow)) (timeline, error) {
	return follow(ctx, c, run, clk, ceiling, each, patiently)
}

// askedOnce asks once, never again: a read that waits on nobody, such as canyon status's platform row (#2730).
func askedOnce(_ context.Context, _ clock, _ time.Time, _ time.Duration, ask func() error) error {
	return ask()
}

// follow is poll, each page asked as ask asks it.
func follow(ctx context.Context, c *Client, run string, clk clock, ceiling time.Duration, each func(timelineRow),
	ask func(context.Context, clock, time.Time, time.Duration, func() error) error) (timeline, error) {
	var t timeline
	after, until := int64(0), clk.now().Add(ceiling)
	for {
		if !clk.now().Before(until) { // every asker's ceiling, askedOnce's among them
			return t, &pastCeiling{ceiling: ceiling}
		}
		var page timelinePage
		path := fmt.Sprintf("/api/v1/gate-runs/%s/timeline?after=%d&limit=%d", url.PathEscape(run), after, pollLimit)
		if err := ask(ctx, clk, until, ceiling, func() error { return c.getUpTo(ctx, path, timelineLimit, &page) }); err != nil {
			return t, err
		}
		for _, row := range page.Rows {
			if t.StartedAt == "" {
				t.StartedAt = row.TS // server time; the run read carries no timestamp
			}
			if !slices.Contains(runKinds, row.Kind) {
				t.Unknown++
			}
			if each != nil {
				each(row)
			}
		}
		if page.Next == nil {
			return t, nil
		}
		echoed := *page.Next == after // a full page whose cursor did not move is a host stalling, not a host ahead of us
		after = *page.Next            // the server's cursor, handed back exactly as it came
		if len(page.Rows) < pollLimit || echoed {
			clk.sleep(pollEvery) // a full page means the next one is already there to read
		}
	}
}

// patiently asks until the host answers (cli.md §3.5): an ask a restart or the proxy in front of the host cuts short
// (transient) is sent again after 1 s, the wait doubling up to retryMost, for up to retryFor from the first one it
// cut, and lostHost after that. A host that answered anything else did answer. pastCeiling once until passes, before
// any ask.
func patiently(ctx context.Context, clk clock, until time.Time, ceiling time.Duration, ask func() error) error {
	var lost time.Time // when the first ask the host hasn't answered since was sent
	for wait := pollEvery; ; wait = min(2*wait, retryMost) {
		if !clk.now().Before(until) {
			return &pastCeiling{ceiling: ceiling}
		}
		sent := clk.now()
		err := ask()
		if err == nil || !transient(err) || ctx.Err() != nil {
			return err
		}
		if lost.IsZero() {
			lost = sent
		}
		if clk.now().Sub(lost) >= retryFor {
			return &lostHost{err: err}
		}
		clk.sleep(wait) // the same ask again
	}
}

// transient is an ask the host never answered itself (#2730): no answer in time, a connection refused, reset or cut
// mid-answer (a restart), or a gateway's 502, 503 or 504 from the proxy in front of it while it restarts.
func transient(err error) bool {
	var cut *Interrupted
	var api *APIError
	return unanswered(err) || errors.As(err, &cut) || errors.As(err, &api) && gateway(api.Status)
}
