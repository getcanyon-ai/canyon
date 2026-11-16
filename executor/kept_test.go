package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// opengrepLike is a stdout as Opengrep prints it under ThenJSON: its SARIF, a NUL, its JSON output. Each
// of its results quotes line, as Opengrep quotes the whole minified line a match is on, in the SARIF's
// snippet and context region and in the JSON's lines.
func opengrepLike(results int, line string) []byte {
	l, _ := json.Marshal(line)
	var sarif, js []string
	for i := range results {
		sarif = append(sarif, `{"ruleId":"rules_lgpl_javascript_dos_rule-regex-dos","message":{"text":"regex DoS"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"wwwroot/lib/jquery/dist/jquery.min.js"},"region":{"startLine":2,"startColumn":`+itoa(i+1)+`,"snippet":{"text":`+string(l)+`}},"contextRegion":{"startLine":1,"endLine":3,"snippet":{"text":`+string(l)+`}}}}]}`)
		js = append(js, `{"check_id":"x","path":"wwwroot/lib/jquery/dist/jquery.min.js","extra":{"lines":`+string(l)+`}}`)
	}
	out := `{"version":"2.1.0","$schema":"https://json.schemastore.org/sarif-2.1.0.json","runs":[{"tool":{"driver":{"name":"Opengrep OSS","rules":[{"id":"x","properties":{"tags":["CWE-1333"],"precision":0.9}}]}},"results":[` + strings.Join(sarif, ",") + `],"invocations":[{"executionSuccessful":true,"toolExecutionNotifications":[]}]}]}` + "\n"
	return []byte(out + "\x00" + `{"version":"1.30.0","results":[` + strings.Join(js, ",") + `],"errors":[{"type":"Syntax error","path":"wwwroot/lib/broken.js"}],"paths":{"scanned":["a"]}}`)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

// A stock .NET webapp's vendored jQuery made Opengrep print one 91 KB minified line per result, twice:
// over 10 MiB, so every such deploy was blocked (#1684). What the executor keeps drops the tool's
// snippets, which the attach never reads, and of the JSON output its errors alone, so it fits, and the
// SARIF and the files not read whole come through as they were.
func TestWhatTheExecutorKeepsDropsTheToolsSnippets(t *testing.T) {
	line := strings.Repeat("a", 91_000)
	raw := opengrepLike(80, line)
	if len(raw) <= maxScanOut {
		t.Fatalf("the stdout is %d bytes: not past the cap it must pass", len(raw))
	}
	out, err := kept(bytes.NewReader(raw), true)
	if err != nil || len(out) > 64<<10 || bytes.Contains(out, []byte("aaaa")) {
		t.Fatalf("kept %d bytes (%v)", len(out), err)
	}
	sarif, unloaded, unparsed, whole := splitJSON(out, true)
	got, ok := sarifOf(sarif, nil)
	if !ok || !whole || len(unloaded) != 0 || len(unparsed) != 1 || unparsed[0] != "wwwroot/lib/broken.js" {
		t.Fatalf("sarif %v, whole %v, unloaded %v, unparsed %v", ok, whole, unloaded, unparsed)
	}
	var doc struct {
		Runs []struct {
			Tool    json.RawMessage
			Results []struct {
				RuleID    string
				Locations []struct {
					PhysicalLocation map[string]json.RawMessage
				}
			}
		}
	}
	if err := json.Unmarshal(got, &doc); err != nil || len(doc.Runs[0].Results) != 80 || !bytes.Contains(doc.Runs[0].Tool, []byte(`"precision":0.9`)) {
		t.Fatalf("the run: %v %s", err, got[:min(len(got), 400)])
	}
	pl := doc.Runs[0].Results[0].Locations[0].PhysicalLocation
	if _, ctx := pl["contextRegion"]; ctx || string(pl["region"]) != `{"startColumn":1,"startLine":2}` || doc.Runs[0].Results[0].RuleID != "rules_lgpl_javascript_dos_rule-regex-dos" {
		t.Fatalf("a result's location: %s", pl)
	}
}

// What the executor keeps is still capped: a SARIF whose results alone pass 10 MiB is over_cap, the
// blocking "could not check" it was (#1555), never a partial answer; a stdout that is not JSON is kept as
// far as it parsed, so it reads as not_sarif, and one with no NUL after its SARIF is never whole.
func TestWhatTheExecutorKeepsIsCapped(t *testing.T) {
	big := `{"version":"2.1.0","runs":[{"results":[{"message":{"text":"` + strings.Repeat("x", maxScanOut) + `"}}]}]}`
	if _, err := kept(strings.NewReader(big), false); !errors.Is(err, errOverCap) {
		t.Fatalf("results past the cap: %v", err)
	}
	out, err := kept(strings.NewReader("Traceback (most recent call last):\n"+strings.Repeat("x", maxScanOut)), false)
	if _, ok := sarifOf(out, nil); err != nil || ok {
		t.Fatalf("not JSON: %q %v", out, err)
	}
	out, err = kept(strings.NewReader(`{"version":"2.1.0","runs":[{"results":[]}]}`+"\n"), true)
	if _, _, _, whole := splitJSON(out, true); err != nil || whole {
		t.Fatalf("no JSON output after the SARIF: %q %v", out, err)
	}
}

// Only one whole SARIF document is an answer: bytes after it, or a JSON output cut short or followed by
// more, are not_sarif as before; a read that fails is the read's error, never a tool's not_sarif; and a
// single result longer than the cap is over_cap before the executor holds it whole (#1690's review).
func TestWhatTheExecutorKeepsIsOneWholeDocument(t *testing.T) {
	sarif := `{"version":"2.1.0","runs":[{"results":[]}]}`
	for name, stdout := range map[string]string{
		"a traceback after it": sarif + "\nTraceback: boom",
		"two documents":        sarif + sarif,
	} {
		out, err := kept(strings.NewReader(stdout), false)
		if _, ok := sarifOf(out, nil); err != nil || ok {
			t.Errorf("%s: answered as SARIF (%v)", name, err)
		}
	}
	for name, tail := range map[string]string{
		"cut inside":        `{"errors":[]`,
		"cut after a key":   `{"errors":[],"results":[{"a":1}]`,
		"garbage after":     `{"errors":[]}garbage`,
		"a second document": `{"errors":[]} {"x":1}`,
		"no errors at all":  `{"results":[]}`,
	} {
		out, err := kept(strings.NewReader(sarif+"\n\x00"+tail), true)
		if _, _, _, whole := splitJSON(out, true); err != nil || whole {
			t.Errorf("%s: whole (%v)", name, err)
		}
	}
	if out, err := kept(strings.NewReader(sarif+"\n\x00"+`{"results":[{"extra":{"lines":"x"}}],"errors":[]}`+"\n"), true); err != nil {
		t.Fatal(err)
	} else if _, _, _, whole := splitJSON(out, true); !whole {
		t.Errorf("a whole JSON output with errors is not whole: %q", out)
	}
	broken := io.MultiReader(strings.NewReader(`{"version":"2.1.0","ru`), iotest.ErrReader(errors.New("stream reset")))
	if _, err := kept(broken, false); err == nil || err.Error() != "stream reset" {
		t.Errorf("a failed read: %v", err)
	}
	huge := io.MultiReader(strings.NewReader(`{"version":"2.1.0","runs":[{"results":[{"message":{"text":"`), endless{})
	if _, err := kept(huge, false); !errors.Is(err, errOverCap) {
		t.Errorf("one result past the cap: %v", err)
	}
}

// endless is a stream of x that never ends.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
