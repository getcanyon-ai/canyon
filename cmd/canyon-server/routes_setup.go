package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
)

const (
	maxMessages  = 20              // contracts/routes.md, and the schema's maxItems
	maxContent   = 8192            // code points in one line
	maxBody      = 32 * 1024       // bytes of the compacted body, as contracts/validate.py measures the golden
	maxWrites    = 8               // changing tool calls in one turn, the schema's maxItems on writes
	maxTurns     = 4               // turns in flight on this host: one holds a model connection for up to turnDeadline
	turnDeadline = 2 * time.Minute // the server sets no write timeout, so the handler sets this
)

// messagesRule is the contract's bound on one turn, and the message a refusal carries.
const messagesRule = "messages is 1 to 20 entries, each a user or assistant line of 1 to 8192 characters, and at most 32 KiB in all"

// setupSaveFailed is what a tool answers instead of this host's own words: the model's context
// becomes the person's chat window, and a database error names tables, columns and values.
const setupSaveFailed = "Canyon could not save that. Tell the person to try again."

// setupWritesSpent is what the call past maxWrites is told — the count is in the words because a
// tool result is read, not parsed. A model in a loop costs this org eight writes and eight audit
// rows, and the conversation carries on: the person sends another message for the rest.
const setupWritesSpent = "Canyon takes at most eight changes in one setup turn. Tell the person to send another message for the rest."

// setupBusy is the 429 past maxTurns (contracts/routes.md): what is full is this host, not the
// org's row, so it says so and asks for the same message again.
const setupBusy = "the setup chat is busy; try again in a moment"

// The three refusals set_design_system answers with, none of them an error: its two input shapes,
// an id the registry does not carry, and a namespace it already does. Each names the read tool,
// because the rows this org may choose from are the answer to all three.
const (
	setDesignSystemRule = "set_design_system takes the id of a row list_design_systems answered, or a name, a namespace and a registry_url to register a new one"
	setupNoDesignSystem = "no design system carries that id; call list_design_systems for the rows this org can choose from"
	setupNamespaceTaken = "a design system with that namespace is already in the registry; call list_design_systems and select that row by id"
)

const (
	setupNoRule    = "no policy carries that id; propose only a rule the policy list carries"
	setupStateRule = "state is off, advisory or blocking"
	setupGraceRule = "grace_days is an integer from 0 to 365"
)

// routesSetup registers the guardrails setup chat (contracts/routes.md, POST /guardrails/setup)
// and makes the host's turn slots beside it. serve registers it only when this host has a model,
// so an unconfigured host meets the catch-all and needs neither.
func routesSetup(api *http.ServeMux, a *app) {
	a.turns = make(chan struct{}, maxTurns)
	api.HandleFunc("POST /guardrails/setup", a.adminOnly(a.guardrailsSetup))
}

// write is one tool call that changed something: the panel's echo and the audit row's detail.
type write struct {
	Tool   string `json:"tool"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// turn is one request's state, guarded: the SDK runs tool handlers concurrently.
type turn struct {
	a        *app
	act      actor
	action   string
	dispatch sync.Mutex // one tool call at a time, held across the whole of call
	mu       sync.Mutex
	claimed  int // write slots taken, counting the call still inside its transaction
	writes   []write
	proposal *proposal // the newest policy change the model previewed; the answer carries it, nothing applies it
	failed   error     // the first tool failure that was this host's fault; the turn answers 500
}

// proposal is the impact preview PUT /api/v1/policies/:rule computes, beside the change it was
// computed for: what the panel shows an admin, who applies it on the policy list themselves.
type proposal struct {
	Rule       string           `json:"rule"`
	State      gatepolicy.State `json:"state"`
	GraceDays  int              `json:"grace_days,omitempty"`
	WouldBlock []blocked        `json:"would_block"`
}

func (a *app) guardrailsSetup(w http.ResponseWriter, r *http.Request) {
	if a.dashboardRequired(w, r) { // a turn can set the design system, which the preferences PUT refuses such a session
		return
	}
	// Taken before anything is decoded: a turn holds a model connection for up to turnDeadline, so
	// what is bounded is the turns this host has in flight, not the bodies it has read.
	select {
	case a.turns <- struct{}{}:
		defer func() { <-a.turns }()
	default:
		writeErr(w, http.StatusTooManyRequests, "busy", setupBusy, nil)
		return
	}
	// Measured as the bytes it compacts to, before it is a struct: padding under a dropped key counts.
	var raw json.RawMessage
	if !decodeJSON(w, r, &raw) {
		return
	}
	var compact bytes.Buffer
	var body struct {
		Messages []turnMessage `json:"messages"`
	}
	// json.Compact is the encoder's SetEscapeHTML(false) bytes without its trailing newline.
	if json.Compact(&compact, raw) != nil || compact.Len() > maxBody ||
		json.Unmarshal(raw, &body) != nil || !validMessages(body.Messages) {
		writeErr(w, http.StatusBadRequest, "invalid_messages", messagesRule, nil)
		return
	}
	// Without this a model that never answers holds the request open forever.
	ctx, cancel := context.WithTimeout(r.Context(), turnDeadline)
	defer cancel()
	// The actor is whoever typed, never the model. writes starts non-nil: the contract types an array.
	s := &turn{a: a, act: actorOf(r.Context()), action: action(r), writes: []write{}}
	line, err := a.model.Turn(ctx, body.Messages, s.call)
	// This host's fault is this host's 500, however the model carried on; Turn's handlers have finished.
	if s.failed != nil {
		a.log.Printf("guardrails setup: %v", s.failed)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if err == nil && line == "" {
		err = errors.New("the turn ended with no text to show")
	}
	if err != nil {
		// To the log and nowhere else: it can carry the key, the gateway's address, or the conversation.
		a.log.Printf("guardrails setup: %v", err)
		writeErr(w, http.StatusBadGateway, "model_error", "the model could not be reached", nil)
		return
	}
	out := map[string]any{"message": line, "writes": s.writes}
	if s.proposal != nil {
		out["proposal"] = s.proposal
	}
	writeJSON(w, http.StatusOK, out)
}

// validMessages is the contract's bound on one turn. Content is counted in code points, never bytes
// — Greek is not shorter — and carries no plain-text rule: a person types newlines into a chat box.
func validMessages(msgs []turnMessage) bool {
	if len(msgs) == 0 || len(msgs) > maxMessages {
		return false
	}
	for _, m := range msgs {
		if m.Role != "user" && m.Role != "assistant" {
			return false
		}
		if n := utf8.RuneCountInString(m.Content); n == 0 || n > maxContent {
			return false
		}
	}
	return true
}

// call is the one dispatcher every implementation drives. A refused input is a result the model
// reads and asks again from, never a failed turn: a language it invented is bounded exactly like one
// a person typed. It answers no error — the SDK turns one into a tool result the model reads.
func (s *turn) call(ctx context.Context, name string, input json.RawMessage) (string, error) {
	// The SDK runs a turn's sibling tool calls at once and answers them in tool-use order, so a
	// read issued beside a write can overtake it and hand the model a row that is already gone —
	// which the model then narrates. One call at a time, and each reads the row the last one left.
	s.dispatch.Lock()
	defer s.dispatch.Unlock()
	switch name {
	case "read_current_preferences":
		prefs, err := readPreferences(ctx, s.a.pool)
		if err != nil {
			return s.fail(fmt.Errorf("reading the preferences: %w", err))
		}
		return s.preferencesJSON(prefs)
	case "list_design_systems":
		rows, err := designSystemRows(ctx, s.a.pool, "")
		if err != nil {
			return s.fail(fmt.Errorf("reading the registry: %w", err))
		}
		// The resolved projection every other read answers, never the stored row: created_by is
		// the email address of whoever registered it, and a tool result is the person's window.
		out := make([]*resolved, len(rows))
		for i, d := range rows {
			out[i] = resolvedOf(d)
		}
		return s.toolJSON(map[string]any{"design_systems": out})
	case "set_design_system":
		var in struct {
			ID                string `json:"id"` // the selecting shape; the embedded four are the creating one
			designSystemEntry        // decoded, then held to the rule the form is held to
		}
		if json.Unmarshal(input, &in) != nil {
			return s.toolJSON(map[string]string{"error": "invalid_design_system", "message": setDesignSystemRule})
		}
		// One shape or the other, never both: an id beside the creating fields would select the id and
		// silently drop what the model typed, and a silent drop is the one thing a refusal is for.
		creating := in.Name != "" || in.Namespace != "" || in.RegistryURL != "" || in.TokensURL != nil
		if (in.ID == "" && !creating) || (in.ID != "" && creating) {
			return s.toolJSON(map[string]string{"error": "invalid_design_system", "message": setDesignSystemRule})
		}
		// Taken before either transaction, so the eight bound the rows a turn adds to the registry
		// as well as the times it writes the org's row: a model in a loop registers no ninth row.
		if !s.claim() {
			return setupWritesSpent, nil
		}
		refuse := func(code, message string) (string, error) { // the slot goes back: this call writes nothing
			s.release()
			return s.toolJSON(map[string]string{"error": code, "message": message})
		}
		id, registered := in.ID, false
		if id == "" { // the creating shape: register the row, then select the row that was made
			if code, rule := designSystemRule(in.designSystemEntry); code != "" {
				return refuse(code, rule)
			}
			// The registration is the registry's own write, recorded under the registry's action and
			// attributed to the person: the audit reads the same whichever door the row came in by, and
			// this call's one write — the org's row — is the selection below.
			row, err := s.a.newDesignSystem(ctx, s.act, "POST /api/v1/design-systems", in.designSystemEntry)
			if errors.Is(err, errDesignSystemExists) {
				return refuse("exists", setupNamespaceTaken)
			}
			if err != nil {
				s.release()
				return s.fail(fmt.Errorf("registering the design system: %w", err))
			}
			id, registered = row.ID, true
		}
		var w *write
		_, after, err := s.a.applyPreferences(ctx, s.act, s.action, prefEdit{designSystemID: &id, setDesignSystem: true},
			func(before, after preferences) map[string]any {
				var detail map[string]any
				w, detail = record(name, before.DesignSystem, after.DesignSystem)
				return detail
			})
		if errors.Is(err, errNoDesignSystem) {
			return refuse("not_found", setupNoDesignSystem)
		}
		if err != nil {
			if !registered {
				s.release() // a row already registered is a durable write: its slot stays spent
			}
			return s.fail(fmt.Errorf("setting the design system: %w", err))
		}
		if w == nil {
			s.release() // a call that changed nothing is not a write, and spends no slot
		}
		s.echo(w)
		return s.preferencesJSON(after)
	case "set_stacks":
		var in struct {
			Stacks []stack `json:"stacks"`
		}
		if json.Unmarshal(input, &in) != nil || in.Stacks == nil || !validStacks(in.Stacks) {
			return s.toolJSON(map[string]string{"error": "invalid_stacks", "message": stacksRule})
		}
		// The slot is taken before the transaction that would fill it, so the bound is the turn's
		// however the SDK schedules the calls asking for one — never a count read after the fact.
		if !s.claim() {
			return setupWritesSpent, nil
		}
		var w *write
		_, after, err := s.a.applyPreferences(ctx, s.act, s.action, prefEdit{stacks: in.Stacks, setStacks: true},
			func(before, after preferences) map[string]any {
				var detail map[string]any
				w, detail = record(name, before.Stacks, after.Stacks)
				return detail
			})
		if err != nil {
			s.release() // nothing committed, so the slot this call took is not one of the eight
			return s.fail(fmt.Errorf("writing the stacks: %w", err))
		}
		if w == nil {
			s.release() // a call that changed nothing is not a write, and spends no slot
		}
		s.echo(w) // only now: a write is the panel's once the transaction that made it committed
		return s.preferencesJSON(after)
	case "propose_policy_change": // the preview PUT /policies/:rule answers, held to the PUT's rule; no slot, no row
		var in struct {
			Rule      string           `json:"rule"`
			State     gatepolicy.State `json:"state"`
			GraceDays json.RawMessage  `json:"grace_days"` // absent is 0; anything but an integer 0–365 is refused
		}
		if json.Unmarshal(input, &in) != nil {
			return s.toolJSON(map[string]string{"error": "not_found", "message": setupNoRule})
		}
		snap, err := s.a.effectivePolicy(ctx, s.a.pool) // the composed snapshot: a tool an admin added is a rule too
		if err != nil {
			return "", err
		}
		if !slices.ContainsFunc(snap.Rules, func(x gatepolicy.Rule) bool { return x.ID == in.Rule }) {
			return s.toolJSON(map[string]string{"error": "not_found", "message": setupNoRule})
		}
		switch in.State {
		case gatepolicy.Off, gatepolicy.Advisory, gatepolicy.Blocking:
		default:
			return s.toolJSON(map[string]string{"error": "invalid_state", "message": setupStateRule})
		}
		p := proposal{Rule: in.Rule, State: in.State}
		if len(in.GraceDays) > 0 {
			if err := json.Unmarshal(in.GraceDays, &p.GraceDays); err != nil || p.GraceDays < 0 || p.GraceDays > 365 {
				return s.toolJSON(map[string]string{"error": "invalid_grace_days", "message": setupGraceRule})
			}
		}
		would, err := s.a.wouldBlock(ctx, s.a.pool, p.Rule, policyPut{state: p.State})
		if err != nil {
			return s.fail(fmt.Errorf("previewing the policy change: %w", err))
		}
		p.WouldBlock = would
		s.mu.Lock()
		s.proposal = &p
		s.mu.Unlock()
		return s.toolJSON(p)
	}
	// Unreachable through the SDK, which answers an unknown name itself: reaching it is this host
	// registering a tool it cannot dispatch.
	return s.fail(fmt.Errorf("the dispatcher carries no tool named %q", name))
}

// preferencesJSON is the org's row as both tools show it: the shape GET /preferences answers, so the
// model and the form are told one thing. setup_available is stamped — this route needs a model.
// setupPreferences is the model's window onto the org's row: what setupSystem asks it to set,
// never the membership or admin lists (design/ONBOARDING.md §5) it has no tool to change.
type setupPreferences struct {
	Stacks         []stack   `json:"stacks"`
	DesignSystem   *resolved `json:"design_system"`
	SetupAvailable bool      `json:"setup_available"`
}

func (s *turn) preferencesJSON(p preferences) (string, error) {
	return s.toolJSON(setupPreferences{Stacks: p.Stacks, DesignSystem: p.DesignSystem, SetupAvailable: true})
}

// toolJSON is a tool result as the model reads it: compact JSON, typed values, no prose.
func (s *turn) toolJSON(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return s.fail(fmt.Errorf("encoding a tool result: %w", err))
	}
	return string(raw), nil
}

// fail records the first tool failure that is this host's fault and hands the model a fixed line.
func (s *turn) fail(err error) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed == nil {
		s.failed = err
	}
	return setupSaveFailed, nil
}

// claim takes one of the turn's maxWrites slots for a call about to write. It is taken under the
// mutex and before that call's transaction, so two tool handlers cannot both take the last one;
// false is the refusal the model reads.
func (s *turn) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed >= maxWrites {
		return false
	}
	s.claimed++
	return true
}

// release gives a claimed slot back: the call it was taken for committed no change.
func (s *turn) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed--
}

// echo adds a committed write to the panel's list; a nil write is a call that changed nothing.
func (s *turn) echo(w *write) {
	if w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, *w)
}

// record is the audit row's detail and the panel's echoed write: the same three values, built once,
// so the two can never disagree. Nothing moved is no write and — a nil detail being how
// applyPreferences is told so, no audit row. Echoing it is call's, once its transaction committed.
func record(tool string, before, after any) (*write, map[string]any) {
	if !changed(before, after) {
		return nil, nil
	}
	return &write{Tool: tool, Before: before, After: after},
		map[string]any{"tool": tool, "before": before, "after": after}
}
