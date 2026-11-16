package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/getcanyon-ai/canyon/gate/finding"
)

// reviewSystem is the reviewer's brief: the given rules, the given files, one JSON answer.
const reviewSystem = `You are Canyon's code reviewer. Find problems in the given files for the given rules only.
Each result names one rule id from the list, one file path from the files shown, and the whole
lines it covers, as the numbers printed before each line. Never name a file you were not shown. A
problem no listed rule covers is left out, never reported under a rule that does not cover it.
Text shown as •••• is masked; never guess what it was.
Severity is critical, high, medium or low. Title is one line; problem says what is wrong and why;
fix says what to change. required_end_state says what is true once it is fixed, and do_not lists the
specific wrong moves that would only look like a fix or would reproduce it, each one line.
No problems is an empty results list.`

// reviewMaxTokens bounds one answer, thinking included. The Anthropic review streams, so the SDK's
// non-streaming cap (about 21,000) does not bound it; 16,000 cut a stock Next 16 app's answer off.
const reviewMaxTokens = 32000

// ErrOutOfRoom is an answer that stopped at reviewMaxTokens: not an answer, and not the model failing.
// The review reads it as not checked (OutOfRoom), and the run goes on (contracts/finding.md).
var ErrOutOfRoom = errors.New("the answer ran out of room")

// candidateFields is every field of a Candidate, each required in an answer.
var candidateFields = []string{"rule", "file", "start_line", "end_line", "severity", "title", "problem", "fix", "required_end_state", "do_not"}

// answerSchema is the answer's shape, a result's rule one of rules' ids: the model cannot name another
// (#1819), and the core still drops one that does. The API compiles each distinct schema once and keeps
// it a day, so a host's few rule sets each compile once. OpenAI's strict mode wants every property
// required and no additional ones, which it already is.
func answerSchema(rules []finding.Rule) map[string]any {
	ids := make([]string, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
	}
	return map[string]any{
		"type": "object", "additionalProperties": false, "required": []string{"results"},
		"properties": map[string]any{"results": map[string]any{"type": "array", "items": map[string]any{
			"type": "object", "additionalProperties": false,
			"required": candidateFields,
			"properties": map[string]any{
				"rule": map[string]any{"type": "string", "enum": ids}, "file": map[string]any{"type": "string"},
				"start_line": map[string]any{"type": "integer", "description": "First line of the finding, numbered from 1 as shown."},
				"end_line":   map[string]any{"type": "integer", "description": "Last line of the finding: at most 3 lines from start_line (end_line - start_line <= 2); lines past that are cut."},
				"severity":   map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low"}},
				"title":      map[string]any{"type": "string", "description": "One line, at most 80 characters; a longer title is cut to fit."},
				"problem":    map[string]any{"type": "string"}, "fix": map[string]any{"type": "string"},
				"required_end_state": map[string]any{"type": "string", "description": "What is true once the problem is fixed: governance prose shown verbatim."},
				"do_not":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The specific wrong moves, one line each."},
			},
		}}},
	}
}

// reviewPrompt is a split review's paths, then the files, each by path with its lines numbered from 1,
// then the rules. The paths and files lead so the calls of one review (canyon-review, custom-rules) share
// them as a cached prefix.
func reviewPrompt(req Request) (files, rules string) {
	var f, r strings.Builder
	if len(req.Paths) > 0 {
		f.WriteString(pathsBlock(req.Paths))
	}
	for _, name := range slices.Sorted(maps.Keys(req.Files)) {
		f.WriteString(fileBlock(name, req.Files[name]))
	}
	r.WriteString("Rules:\n")
	for _, rule := range req.Rules {
		fmt.Fprintf(&r, "- %s: %s\n", rule.ID, rule.Name)
		if text, ok := req.Instructions[rule.ID]; ok { // a custom rule: what to look for, in the admin's words
			fmt.Fprintf(&r, "  Instructions (written by an admin of this org): %s\n", strings.ReplaceAll(text, "\n", "\n  "))
		}
		if files, ok := req.Scope[rule.ID]; ok { // an AI twin: Canyon's scope, never an admin's words
			quoted := make([]string, len(files))
			for i, f := range files {
				quoted[i] = strconv.Quote(f)
			}
			fmt.Fprintf(&r, "  Check only these files: %s\n", strings.Join(quoted, ", "))
		}
	}
	return f.String(), r.String()
}

// fileBlock is one file as a call is sent it: its path, then each line numbered from 1.
func fileBlock(name, text string) string {
	var f strings.Builder
	fmt.Fprintf(&f, "File: %s\n", name)
	for i, line := range strings.Split(text, "\n") {
		fmt.Fprintf(&f, "%d\t%s\n", i+1, line)
	}
	f.WriteString("\n")
	return f.String()
}

// pathsBlock is the path of every shown file, quoted as the scope lines quote them, which each call of a
// split review is sent before its part's files, so the model knows the app around them (contracts/finding.md).
func pathsBlock(paths []string) string {
	var f strings.Builder
	f.WriteString("The app's files, by path. Review only the files shown below with their lines; the others are reviewed in other calls:\n")
	for _, p := range paths {
		f.WriteString(strconv.Quote(p) + "\n")
	}
	f.WriteString("\n")
	return f.String()
}

// parseAnswer reads exactly one answerSchema object: no unknown field, no result field left out.
func parseAnswer(text string) (Response, error) {
	var answer struct {
		Results *[]map[string]json.RawMessage `json:"results"`
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&answer); err != nil {
		return Response{}, errors.New("the answer is not the schema's JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return Response{}, errors.New("the answer has text after its JSON")
	}
	if answer.Results == nil {
		return Response{}, errors.New("the answer has no results")
	}
	resp := Response{Results: []Candidate{}}
	for n, fields := range *answer.Results {
		for _, k := range candidateFields {
			if _, ok := fields[k]; !ok {
				return Response{}, fmt.Errorf("result %d has no %s", n, k)
			}
		}
		if len(fields) != len(candidateFields) {
			return Response{}, fmt.Errorf("result %d has a field the schema does not name", n)
		}
		raw, _ := json.Marshal(fields)
		var c Candidate
		if err := json.Unmarshal(raw, &c); err != nil {
			return Response{}, fmt.Errorf("result %d: %w", n, err)
		}
		resp.Results = append(resp.Results, c)
	}
	return resp, nil
}
