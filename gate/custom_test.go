package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

const emailRule = "custom.customer-email-encrypted"

// custom is the canonical snapshot with one custom rule in state, the custom-rules agent carried,
// reviewed by the custom-rules agent (contracts/snapshot.md, Custom rules).
func custom(t *testing.T, m Model, state policy.State) *Reviewer {
	r := reviewer(t, m)
	r.Snapshot.Rules = append(r.Snapshot.Rules, policy.Rule{ID: emailRule, Name: "Customer emails are encrypted at rest", State: state, Producer: policy.LLMReview,
		Instructions: "Flag code that writes a customer's email address to a database column or a file without encrypting it.\nHashing it for a lookup is fine."})
	r.Snapshot.Agents = append(r.Snapshot.Agents, policy.Agent{ID: CustomRules, Namespace: []string{"custom"}, Model: "resolved-model-id"})
	r.Agent = CustomRules
	return r
}

// A custom rule's finding is high whatever severity the model answered, so Blocking blocks it.
func TestACustomRuleFindingIsHigh(t *testing.T) {
	c := candidate()
	c.Rule, c.Severity = emailRule, "low"
	c.RequiredEndState, c.DoNot = "The email is encrypted before it is written.", []string{"hash it and call that encryption"}
	r := custom(t, &model{resp: Response{Results: []Candidate{c}}}, policy.Blocking)
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	if err := finding.Derive(&out, r.Snapshot); err != nil {
		t.Fatal(err)
	}
	res := out.Runs[len(out.Runs)-1].Results
	if len(res) != 1 || res[0].Properties.Canyon.Severity != finding.High || !*res[0].Properties.Canyon.Blocks {
		t.Fatalf("a custom rule's finding: %+v", res)
	}
}

// The override comes before the governance check: a low candidate with no prose under a Blocking
// custom rule is high, so it blocks with Canyon's prose rather than being let through as low.
func TestALowCustomCandidateWithoutProseBlocksWithCanyons(t *testing.T) {
	c := candidate()
	c.Rule, c.Severity = emailRule, "low"
	r := custom(t, &model{resp: Response{Results: []Candidate{c}, Recorded: true}}, policy.Blocking)
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	if err := finding.Derive(&out, r.Snapshot); err != nil {
		t.Fatal(err)
	}
	run := out.Runs[len(out.Runs)-1]
	got := run.Results[0].Properties.Canyon
	if got.Severity != finding.High || !*got.Blocks || got.RequiredEndState != "Canyon's review no longer reports "+emailRule+" for this code when it runs again, and the app still does what it did." {
		t.Fatalf("%+v", got)
	}
	if notes := run.Invocations[0].ToolExecutionNotifications; notes[0].Message.Text != "custom rules · replayed from a recording" {
		t.Fatalf("a replayed custom-rules run says so as its own: %+v", notes)
	}
}

// Each custom rule's instructions reach the model under its id and name; canyon-review's request is
// what it always was, no instructions in it, so every recording keys as before.
func TestCustomRulesAreSentWithTheirInstructions(t *testing.T) {
	m := &model{resp: Response{Results: []Candidate{}}}
	r := custom(t, m, policy.Advisory)
	_, scanned := golden(t)
	if _, err := r.Review(context.Background(), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	req := m.calls[0]
	_, prompt := reviewPrompt(req)
	if req.Instructions[emailRule] == "" || len(req.Rules) != 1 || req.Rules[0].ID != emailRule ||
		!strings.Contains(prompt, "- "+emailRule+": Customer emails are encrypted at rest\n  Instructions (written by an admin of this org): Flag code that writes") ||
		!strings.Contains(prompt, "without encrypting it.\n  Hashing it for a lookup is fine.\n") {
		t.Fatalf("the request: %+v\nprompt:\n%.400s", req, prompt)
	}
	plain := &model{resp: Response{Results: []Candidate{}}}
	rr := custom(t, plain, policy.Advisory)
	rr.Agent = "canyon-review"
	if _, err := rr.Review(context.Background(), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	_, plainRules := reviewPrompt(plain.calls[0])
	if raw, _ := json.Marshal(plain.calls[0]); strings.Contains(string(raw), "instructions") || strings.Contains(plainRules, "Instructions") {
		t.Fatalf("canyon-review's request carries instructions: %s", raw)
	}
}

// The custom-rules run says what it is: its notifications read "custom rules ·", never "ai review ·".
func TestTheCustomRulesRunNotesAreItsOwn(t *testing.T) {
	r := custom(t, nil, policy.Advisory)
	r.Model = nil
	_, scanned := golden(t)
	out, err := r.Review(context.Background(), claimsPortal, scanned)
	if err != nil {
		t.Fatal(err)
	}
	notes := out.Runs[len(out.Runs)-1].Invocations[0].ToolExecutionNotifications
	if len(notes) != 1 || notes[0].Message.Text != "custom rules · skipped (no model configured on this Canyon host)" {
		t.Fatalf("notes %+v", notes)
	}
	c := candidate() // a perf candidate: outside the custom namespace, dropped and counted
	r = custom(t, &model{resp: Response{Results: []Candidate{c}}}, policy.Advisory)
	if out, err = r.Review(context.Background(), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	if notes := out.Runs[len(out.Runs)-1].Invocations[0].ToolExecutionNotifications; notes[0].Message.Text != "custom rules · dropped 1 invalid result(s)" {
		t.Fatalf("notes %+v", notes)
	}
}

// bothCalls runs canyon-review then custom-rules over the fixture app with m, as a review with a custom
// rule on does, and returns the files the model was shown.
func bothCalls(t *testing.T, m Model) map[string]string {
	t.Helper()
	_, scanned := golden(t)
	for _, agent := range []string{"canyon-review", CustomRules} {
		r := custom(t, m, policy.Advisory)
		r.Agent = agent
		if _, err := r.Review(context.Background(), claimsPortal, scanned); err != nil {
			t.Fatal(err)
		}
	}
	_, files, _, err := excerpts(claimsPortal)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// sharedFiles fails unless text, the leading content both calls share, carries every file shown.
func sharedFiles(t *testing.T, text string, files map[string]string) {
	t.Helper()
	for name := range files {
		if !strings.Contains(text, "File: "+name+"\n") {
			t.Errorf("the shared prefix lacks %s:\n%.200s", name, text)
		}
	}
}

// The two calls of one review send the source once (canyon #1395): on Anthropic the files are each
// call's first content block, byte for byte the same and marked an ephemeral cache prefix.
func TestAnthropicBothCallsShareACachedFilesPrefix(t *testing.T) {
	m, bodies := fakeAPI(t, 200, answer(`{"results":[]}`, "end_turn"))
	files := bothCalls(t, m)
	if len(*bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(*bodies))
	}
	type block struct {
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	var sent [2]struct {
		System   []block `json:"system"`
		Messages []struct {
			Content []block `json:"content"`
		} `json:"messages"`
	}
	for i, raw := range *bodies {
		if err := json.Unmarshal(raw, &sent[i]); err != nil {
			t.Fatal(err)
		}
		if len(sent[i].Messages) != 1 || len(sent[i].Messages[0].Content) != 2 {
			t.Fatalf("call %d: want one message of two blocks, files then rules: %.600s", i, raw)
		}
	}
	first, second := sent[0].Messages[0].Content, sent[1].Messages[0].Content
	if first[0].Text != second[0].Text || fmt.Sprint(sent[0].System) != fmt.Sprint(sent[1].System) {
		t.Fatal("the two calls' leading content differs")
	}
	sharedFiles(t, first[0].Text, files)
	if strings.Contains(first[0].Text, "Rules:") {
		t.Errorf("the cached block carries rules:\n%.400s", first[0].Text)
	}
	for i, c := range [][]block{first, second} {
		if string(c[0].CacheControl) != `{"type":"ephemeral"}` || c[1].CacheControl != nil {
			t.Errorf("call %d: cache_control %s on the files, %s on the rules; want ephemeral on the files only", i, c[0].CacheControl, c[1].CacheControl)
		}
	}
	if !strings.HasPrefix(first[1].Text, "Rules:\n") || !strings.Contains(second[1].Text, emailRule) {
		t.Fatalf("each call's rules follow its files:\n%.300s\n---\n%.300s", first[1].Text, second[1].Text)
	}
}

// On OpenAI the prefix cache is automatic: both calls' user message starts with the same files.
func TestOpenAIBothCallsShareAFilesPrefix(t *testing.T) {
	m, bodies := fakeOpenAI(t, 200, completion("stop", content(`{"results":[]}`)))
	files := bothCalls(t, m)
	if len(*bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(*bodies))
	}
	var users [2]string
	for i, raw := range *bodies {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Messages) != 2 || body.Messages[1].Role != "user" {
			t.Fatalf("call %d: messages %.400s", i, raw)
		}
		users[i] = body.Messages[1].Content
	}
	n := 0
	for n < min(len(users[0]), len(users[1])) && users[0][n] == users[1][n] {
		n++
	}
	sharedFiles(t, users[0][:n], files)
}

// A custom rule checked by code is the scanners' run's: the custom-rules agent is never sent it, so no
// rule is judged twice (contracts/snapshot.md, Custom rules).
func TestACustomRuleCheckedByCodeIsNotSentToTheModel(t *testing.T) {
	m := &model{resp: Response{Results: []Candidate{}}}
	r := custom(t, m, policy.Advisory)
	r.Snapshot.Rules = append(r.Snapshot.Rules, policy.Rule{ID: "custom.no-lodash", Name: "No lodash", State: policy.Blocking, Producer: policy.Scanner,
		Instructions: "No app may depend on lodash.", Check: &policy.Check{By: "code", Expr: `!dependencies.exists(d, d.name == 'lodash')`, Says: "No dependency is named lodash.", FactsVersion: 2}})
	_, scanned := golden(t)
	if _, err := r.Review(context.Background(), claimsPortal, scanned); err != nil {
		t.Fatal(err)
	}
	if req := m.calls[0]; len(req.Rules) != 1 || req.Rules[0].ID != emailRule || req.Instructions["custom.no-lodash"] != "" {
		t.Fatalf("the request: %+v", req)
	}
}
