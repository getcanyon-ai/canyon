// Package answered asks an app one GET / past Canyon's sign-in and says what it answered: the server's
// ask on Docker and canyon-identity's on Kubernetes (contracts/routes.md, envs.<env>.answered;
// contracts/executor.md, What the app answered).
package answered

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Answer is what the app answered: its status and the first non-empty line of its body, or why nothing
// answered. Host is the address the sidecar asked as, on its answer to the executor alone.
type Answer struct {
	Host   string `json:"host,omitempty"`
	Status int    `json:"status,omitempty"`
	Said   string `json:"said"`
	Error  string `json:"error,omitempty"`
}

// MarshalJSON is {status, said} or {error}, each with host when it is set.
func (a Answer) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	if a.Host != "" {
		m["host"] = a.Host
	}
	if a.Error != "" {
		m["error"] = a.Error
	} else {
		m["status"], m["said"] = a.Status, a.Said
	}
	return json.Marshal(m)
}

// Direct is the transport an ask goes through: straight to the app, never an environment's HTTP_PROXY.
var Direct http.RoundTripper = &http.Transport{}

// Ask makes one GET of url with host as its Host, through rt (nil is Direct): no cookie, no credential, no
// redirect followed, 5 s. Said is the body's first non-empty line when the answer's Content-Type is
// text/plain or application/json — never a page's markup — read from at most its first 64 KiB, trimmed,
// what could hide text removed — before anything masks it, so no such character splits a value — and at
// most 1 KiB of it.
func Ask(ctx context.Context, rt http.RoundTripper, url, host string) Answer {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Answer{Error: "unreachable"}
	}
	req.Host = host
	client := http.Client{Transport: cmp.Or(rt, Direct), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return Answer{Error: words(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); kind != "text/plain" && kind != "application/json" {
		return Answer{Status: resp.StatusCode}
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // a body cut short still has its first line
	var said string
	for line := range strings.Lines(string(body)) {
		if said = strings.TrimSpace(line); said != "" {
			break
		}
	}
	said = strings.Map(func(r rune) rune {
		if r == 0x7F || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, said)
	return Answer{Status: resp.StatusCode, Said: strings.ToValidUTF8(said[:min(len(said), 1<<10)], "")}
}

// Valid is an answer the contract allows: an HTTP status, or one of its words.
func (a Answer) Valid() bool {
	if a.Error != "" {
		return a.Status == 0 && a.Said == "" && slices.Contains([]string{"no answer in 5 s", "refused", "unreachable", "not an HTTP answer"}, a.Error)
	}
	return a.Status >= 100 && a.Status <= 599 && len(a.Said) <= 1<<10
}

// words is the contract's word for why nothing answered, never a library's message: an app's own bytes
// can be in one.
func words(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "no answer in 5 s"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &dns), errors.As(err, &op):
		return "unreachable"
	}
	return "not an HTTP answer"
}

// Flagged is an answer to say beside readiness (contracts/routes.md, envs.<env>.answered): nothing
// answered, or a status outside 2xx and 3xx but 401 and 404, the two an app's own sign-in or an API with
// no page at its root answer.
func (a Answer) Flagged() bool {
	return a.Error != "" || (a.Status < 200 || a.Status > 399) && a.Status != 401 && a.Status != 404
}

// Refused is the promotion's refusal of version, which answered a on preview (contracts/routes.md, the
// promotions row's preview_answered): the route's message and the CLI's line, one sentence. The reason
// phrase is the standard one, never the app's own.
func (a Answer) Refused(version string) string {
	fact := "gives no answer at / on preview (" + a.Error + ")"
	if a.Error == "" {
		fact = strings.TrimSpace(fmt.Sprintf("answers %d %s", a.Status, http.StatusText(a.Status))) + " at / on preview"
	}
	return "Not promoted: " + version + " " + fact + ". Fix the app and deploy again, or have a person promote it from the dashboard after checking it."
}
