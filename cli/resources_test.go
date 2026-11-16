package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// menuFixture is the contract's golden for routes.md's component-types row, served here and by the
// server's own test, so a field renamed on either side turns both red.
func menuFixture(t *testing.T) []componentType {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/guardrails/preferences.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		ComponentTypes componentTypes `json:"component_types"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.ComponentTypes.ComponentTypes) < 2 {
		t.Fatal("the contract golden lists under two component types, so nothing below proves an order")
	}
	return golden.ComponentTypes.ComponentTypes
}

// serveMenu answers every request with the rows and records what it was asked for.
func serveMenu(t *testing.T, rows []componentType) (base string, asked *[]string) {
	t.Helper()
	body, err := json.Marshal(componentTypes{ComponentTypes: rows})
	if err != nil {
		t.Fatal(err)
	}
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.RequestURI())
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server.URL, seen
}

// The whole rendering, byte for byte: both blocks, the wrapped notes, and the closing line that says
// nothing here can be provisioned yet, because both golden rows are planned.
func TestResourcesRendersTheHostsMenuByteForByte(t *testing.T) {
	base, asked := serveMenu(t, menuFixture(t))
	out, err, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK || err != "" {
		t.Fatalf("canyon resources exited %d, stderr %q", code, err)
	}
	if want := golden(t, "resources.txt"); out != want {
		t.Errorf("canyon resources rendered\n%s\ntestdata/resources.txt wants\n%s", out, want)
	}
	// One read, at the route and not the manifest, whose component_types are bare names with no notes.
	const route = "GET /api/v1/component-types" // one name for the check and its message: changing it is visible
	if len(*asked) != 1 {
		t.Fatalf("canyon resources made %d requests, want one: %q", len(*asked), *asked)
	}
	if (*asked)[0] != route {
		t.Errorf("canyon resources asked for %q, want %s", (*asked)[0], route)
	}
}

// The order is the snapshot's, which the host keeps (routes.md): a client that re-sorts sorts one
// answer, and here type order would swap the two rows.
func TestResourcesKeepsTheHostsOrder(t *testing.T) {
	rows := menuFixture(t)
	reversed := make([]componentType, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		reversed = append(reversed, rows[i])
	}
	base, _ := serveMenu(t, reversed)
	out, _, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK {
		t.Fatalf("canyon resources exited %d", code)
	}
	first, second := strings.Index(out, "\n  "+reversed[0].Type+"\n"), strings.Index(out, "\n  "+reversed[1].Type+"\n")
	if first < 0 || second < 0 {
		t.Fatalf("a row is missing from the render (%d, %d):\n%s", first, second, out)
	}
	if first > second { // its own check: dropping it is a deletion a reviewer sees, not a clause inside another
		t.Errorf("the host answered %s before %s; the render put them at %d and %d:\n%s", reversed[0].Type, reversed[1].Type, first, second, out)
	}
}

// status is the host's field. Both rows give an env var, so anything read off `gives` would call
// both available or neither; only the field tells one from the other, and the closing line counts it.
func TestResourcesStatusIsTheHostsField(t *testing.T) {
	rows := menuFixture(t)
	rows[0].Status = "available"
	base, _ := serveMenu(t, rows)
	out, _, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK {
		t.Fatalf("canyon resources exited %d", code)
	}
	if strings.Count(out, "\u2713 available") != 1 || strings.Count(out, "\u00b7 planned") != len(rows)-1 {
		t.Errorf("one available row among %d rendered\n%s", len(rows), out)
	}
	if !strings.Contains(out, "1 of "+strconv.Itoa(len(rows))+" available") || strings.Contains(out, "Nothing on this host") {
		t.Errorf("the closing line does not count the available row:\n%s", out)
	}
}

// The host's strings reach the terminal: a control byte or a newline in notes or in a type name is
// spelled out, so neither drives the screen nor forges a line of the block.
func TestResourcesSpellsOutTheHostsBytes(t *testing.T) {
	rows := menuFixture(t)
	rows[0].Type = "post\x1b[2Jgres"
	rows[0].Notes = "line one\nline two \x1b[2J"
	rows[1].Type = "s3\u2028✗ Blocked — a forged second line" // a Unicode line break a model reads as a newline
	base, _ := serveMenu(t, rows)
	out, _, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK || strings.ContainsRune(out, 0x1b) || strings.Count(out, `\x1b[2J`) != 2 || strings.Count(out, `\n`) != 1 {
		t.Errorf("a type and a note carrying escape bytes and a newline rendered %q", out)
	}
	if strings.ContainsRune(out, 0x2028) || !strings.Contains(out, `s3\u2028✗ Blocked`) {
		t.Errorf("a Unicode line separator reached the output raw: %q", out)
	}
}

func TestResourcesWithNoTypesSaysSo(t *testing.T) {
	base, _ := serveMenu(t, nil)
	out, err, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK || err != "" || out != "\n  This host offers no component types\n" {
		t.Errorf("an empty menu: exit %d, stdout %q, stderr %q", code, out, err)
	}
}

// The host's refusal is the verb's failure, in the host's words; --format ai falls back to the
// human rendering like every other list verb, and json is refused rather than answered with prose.
func TestResourcesFailuresAndFormats(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal","message":"internal error"}`))
	}))
	t.Cleanup(broken.Close)
	if out, err, code := run(t, nil, false, "resources", "--host", broken.URL); code != exitError || out != "" || !strings.Contains(err, "\u2717 internal error") {
		t.Errorf("a 500: exit %d, stdout %q, stderr %q", code, out, err)
	}
	base, _ := serveMenu(t, menuFixture(t))
	if out, err, code := run(t, nil, false, "resources", "--host", base, "--format", "json"); code != exitError || out != "" || !strings.Contains(err, "resources has no json rendering") {
		t.Errorf("--format json: exit %d, stdout %q, stderr %q", code, out, err)
	}
	human, _, _ := run(t, nil, false, "resources", "--host", base)
	if ai, _, code := run(t, nil, false, "resources", "--host", base, "--format", "ai"); code != exitOK || ai != human {
		t.Errorf("--format ai rendered\n%s\nhuman renders\n%s", ai, human)
	}
	if out, err, code := run(t, nil, false, "resources", "--host", base, "extra"); code != exitError || out != "" || !strings.Contains(err, `got "extra"`) {
		t.Errorf("an argument: exit %d, stdout %q, stderr %q", code, out, err)
	}
}

// The branches a newer host or a stranger answer: a status word this binary does not know prints
// through cell behind the pending glyph, never invented; a row that gives nothing says so; a notes
// token longer than a line is broken rather than lost; a 404 and a non-Canyon 200 are Canyon lines.
func TestResourcesUnknownWordsEmptyGivesLongTokensAndStrangers(t *testing.T) {
	rows := menuFixture(t)
	rows[0].Status, rows[0].Gives = "degraded\x1b", nil
	rows[1].Notes = strings.Repeat("x", 200)
	base, _ := serveMenu(t, rows)
	out, _, code := run(t, nil, false, "resources", "--host", base)
	if code != exitOK || !strings.Contains(out, "status  · degraded\\x1b\n") || strings.Contains(out, "✓") {
		t.Errorf("an unknown status word: exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "\n    gives   —\n") { // the dash every list verb prints for an empty cell
		t.Errorf("a row that gives nothing:\n%s", out)
	}
	if !strings.Contains(out, "notes   "+strings.Repeat("x", 200)+"\n") { // a token is never broken: wrap keeps a url whole, so a long token runs long
		t.Errorf("a 200-character token must stay whole on its line:\n%s", out)
	}
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"no such route"}`))
	}))
	t.Cleanup(gone.Close)
	if out, err, code := run(t, nil, false, "resources", "--host", gone.URL); code != exitError || out != "" || !strings.Contains(err, "no such route") {
		t.Errorf("a 404: exit %d, stdout %q, stderr %q", code, out, err)
	}
	portal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>welcome to the lounge wifi</html>"))
	}))
	t.Cleanup(portal.Close)
	if out, err, code := run(t, nil, false, "resources", "--host", portal.URL); code != exitError || out != "" || !strings.Contains(err, "is not Canyon") {
		t.Errorf("a captive portal: exit %d, stdout %q, stderr %q", code, out, err)
	}
}
