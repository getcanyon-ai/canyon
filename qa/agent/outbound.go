package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// outboundRead is the app's outbound records as GET /api/v1/projects/:id/egress answers them (contracts/routes.md;
// egress.schema.json's project_read), only what the runner and the scorer read. The runner reads it as the tester,
// who owns the app: at a refusal wait, kept as outbound-first.json, and after the last turn, as outbound-last.json.
type outboundRead struct {
	Envs map[string]outboundEnv `json:"envs"`
}

type outboundEnv struct {
	Mode         string            `json:"mode"`
	Allowed      []string          `json:"allowed"`
	Requests     []outboundRequest `json:"requests"`
	Destinations []struct {
		Destination     string `json:"destination"`
		Port            int    `json:"port"`
		LastSeenAt      string `json:"last_seen_at"`
		Calls           int    `json:"calls"`
		LastOutcome     string `json:"last_outcome"`
		LastStatusClass string `json:"last_status_class"`
	} `json:"destinations"`
}

// outboundRequest is an env's egress request (design/EGRESS.md §3.4): pending, or dismissed by a person.
type outboundRequest struct {
	Destination   string `json:"destination"`
	State         string `json:"state"`
	LastRefusedAt string `json:"last_refused_at"`
	Refusals      int    `json:"refusals"`
}

var outboundEnvs = []string{"preview", "prod"}

const (
	outboundFirst = "outbound-first.json"
	outboundLast  = "outbound-last.json"
)

func outboundPath(app string) string { return "/api/v1/projects/" + app + "/egress" }

// readOutbound is a kept read; nil when the run took none, or it does not parse.
func readOutbound(path string) *outboundRead {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r outboundRead
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return &r
}

// refusedYet is a refusal wait's judge: done once an env of the app holds a pending egress request, which Canyon files
// at the first refusal of a destination (design/EGRESS.md §3.4).
func refusedYet(body []byte) (string, bool) {
	var r outboundRead
	if err := json.Unmarshal(body, &r); err != nil {
		return "unread: " + err.Error(), false
	}
	var pending []string
	for _, env := range outboundEnvs {
		for _, q := range r.Envs[env].Requests {
			if q.State == "pending" {
				pending = append(pending, q.Destination+" in "+env)
			}
		}
	}
	if len(pending) == 0 {
		return "no egress request", false
	}
	return "an egress request: " + strings.Join(pending, ", "), true
}

// allowedYet is an allow wait's judge: done once every destination the refusal wait read pending is on its env's
// allowlist, which a person's Allow puts it on. A Dismiss leaves it dismissed, never allowed, so the wait goes on.
func allowedYet(first outboundRead) func([]byte) (string, bool) {
	return func(body []byte) (string, bool) {
		var r outboundRead
		if err := json.Unmarshal(body, &r); err != nil {
			return "unread: " + err.Error(), false
		}
		var said []string
		done := true
		for _, env := range outboundEnvs {
			for _, q := range first.Envs[env].Requests {
				if q.State != "pending" {
					continue
				}
				word := "allowed"
				if !allows(r.Envs[env].Allowed, q.Destination) {
					word, done = "not allowed yet", false
				}
				said = append(said, q.Destination+" in "+env+" "+word)
			}
		}
		return strings.Join(said, ", "), done
	}
}

// waitingFor names what an allow wait waits for, so the operator can ask a person for exactly that.
func waitingFor(first outboundRead) string {
	var d []string
	for _, env := range outboundEnvs {
		for _, q := range first.Envs[env].Requests {
			if q.State == "pending" {
				d = append(d, q.Destination+" in "+env)
			}
		}
	}
	return strings.Join(d, ", ")
}

// allows is whether an allowlist entry matches the destination: the name itself, or a *. entry any depth above it
// (design/EGRESS.md §3.3).
func allows(entries []string, d string) bool {
	return slices.ContainsFunc(entries, func(e string) bool {
		return e == d || strings.HasPrefix(e, "*.") && strings.HasSuffix(d, e[1:])
	})
}

// outboundState is what Canyon's records say of destination d in one read: REACHED when, in an env where the refusal
// wait (first) saw it pending, its row says its last call went through — allowed, and answered 2xx (a tunnel's answer is
// its CONNECT's) — after the latest refusal of it that read or first holds, so a call from before the env was restricted
// never counts, nor one from an env the run never saw refuse it (a promotion to a production that allows it is no
// person's Allow); such a call reads NOT REACHED, said before a refusal, so it shows. Else REFUSED while an env holds
// its egress request pending; DISMISSED once a person dismissed it; NOT REACHED for any other row; none when no env has
// heard of it.
func outboundState(r, first *outboundRead, d string) (string, string) {
	const (
		reached = iota
		around  // a call that went through where the run never saw it refused
		refused
		dismissed
		other
		none
	)
	rank, word, detail := none, "none", ""
	better := func(k int, w, why string) {
		if k < rank {
			rank, word, detail = k, w, why
		}
	}
	for _, env := range outboundEnvs {
		waited := first != nil && slices.ContainsFunc(first.Envs[env].Requests, func(q outboundRequest) bool {
			return q.Destination == d && q.State == "pending"
		})
		latest := ""
		for _, read := range []*outboundRead{r, first} {
			if read == nil {
				continue
			}
			for _, q := range read.Envs[env].Requests {
				if q.Destination == d && q.LastRefusedAt > latest {
					latest = q.LastRefusedAt
				}
			}
		}
		e := r.Envs[env]
		for _, q := range e.Requests {
			if q.Destination != d {
				continue
			}
			if q.State == "pending" {
				better(refused, "REFUSED", fmt.Sprintf("%s: pending, %s, last %s", env, count(q.Refusals, "refusal"), q.LastRefusedAt))
			} else {
				better(dismissed, "DISMISSED", fmt.Sprintf("%s: dismissed, %s", env, count(q.Refusals, "refusal")))
			}
		}
		for _, row := range e.Destinations {
			if row.Destination != d {
				continue
			}
			said := fmt.Sprintf("%s: %s %s, last %s", env, row.LastOutcome, row.LastStatusClass, row.LastSeenAt)
			switch through := row.LastOutcome == "allowed" && row.LastStatusClass == "2xx"; {
			case through && waited && row.LastSeenAt > latest:
				better(reached, "REACHED", said)
			case through && !waited && first != nil:
				better(around, "NOT REACHED", said+", where the run never saw it refused")
			default:
				better(other, "NOT REACHED", said)
			}
		}
	}
	return word, detail
}

// outboundVerdict judges an outbound expect against a read: every destination it names in the state it names.
func outboundVerdict(e expect, r, first *outboundRead) (string, bool) {
	if r == nil {
		return "none: no read of the outbound records", false
	}
	ok := true
	var got []string
	for _, d := range e.Keys {
		word, detail := outboundState(r, first, d)
		ok = ok && word == e.Result
		if detail == "" {
			got = append(got, word+": "+d)
			continue
		}
		got = append(got, word+" "+d+" ("+detail+")")
	}
	return strings.Join(got, "; "), ok
}

// reachedYet is the last read's judge: done at once unless the last expect is an outbound one, which it waits for, so
// a call the app makes on its timer and a flush still on its way are read before the scorer judges it.
func reachedYet(e expect, first *outboundRead) func([]byte) (string, bool) {
	return func(body []byte) (string, bool) {
		if !e.outbound() {
			return "read", true
		}
		var r outboundRead
		if err := json.Unmarshal(body, &r); err != nil {
			return "unread: " + err.Error(), false
		}
		return outboundVerdict(e, &r, first)
	}
}

// outboundLines are a read as the scorecard lists it: per env, its mode, allowlist, requests and rows.
func outboundLines(r *outboundRead) []string {
	var lines []string
	for _, env := range outboundEnvs {
		e := r.Envs[env]
		line := fmt.Sprintf("%s · %s · allowed: %s", env, cmp.Or(e.Mode, "no mode"), cmp.Or(strings.Join(e.Allowed, ", "), "none"))
		for _, q := range e.Requests {
			line += fmt.Sprintf(" · request %s %s, %s, last %s", q.Destination, q.State, count(q.Refusals, "refusal"), q.LastRefusedAt)
		}
		for _, row := range e.Destinations {
			line += fmt.Sprintf(" · row %s:%d %s %s, %s, last %s", row.Destination, row.Port, row.LastOutcome, row.LastStatusClass, count(row.Calls, "call"), row.LastSeenAt)
		}
		lines = append(lines, line)
	}
	return lines
}

// count is n and the word, plural but for one.
func count(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
