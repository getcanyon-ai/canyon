package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A prod database session's reason is 1 to 500 characters, at least one visible — not a filler or the blank
// Braille pattern — with no control, format, bidirectional, private-use or unassigned character (contracts/routes.md, Database sessions: the body): what reaches the trail
// reads as it was typed.
func TestAProdReasonIsOneTo500VisibleCharacters(t *testing.T) {
	for reason, ok := range map[string]bool{
		"find the duplicated March invoices": true,
		"доступ к счетам за март":            true,
		strings.Repeat("é", 500):             true,
		"":                                   false,
		"   ":                                false,
		strings.Repeat("é", 501):             false,
		"tab\there":                          false,
		"a\x7fb":                             false,
		"zero\u200bwidth":                    false,
		"right\u202eto left":                 false,
		"isolate\u2066d\u2069":               false,
		"\xff":                               false,
		"\u3164\u3164":                       false,
		"\u2800 \u2800":                      false,
		"\ue000 private":                     false,
		"unassigned \u0378":                  false,
		"\u3164 INC-2231":                    true,
	} {
		if prodReason(reason) != ok {
			t.Errorf("%q: want %v", reason, ok)
		}
	}
}

// dbShapeOf reads the golden as the contract does, and refuses a read the contract refuses: a reason with
// nothing visible, and an address that is none.
func TestTheDBSessionShapeCheckReadsTheGolden(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/testdata/db-session/meridian.json")
	var doc map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &doc)
	}
	if err != nil {
		t.Fatal(err)
	}
	read := doc["prod_read"].(map[string]any)["answer"].(map[string]any)
	dbShape(t, "db_session", string(mustJSON(read)))
	dbShape(t, "list", string(mustJSON(doc["list"])))
	for _, bad := range []func(){func() { read["reason"] = "   " }, func() { read["reason"] = "a\u202eb" }, func() { read["person"].(map[string]any)["email"] = "dana" }} {
		bad()
		if dbShapeOf("db_session", string(mustJSON(read))) == nil {
			t.Errorf("dbShape passed %s", mustJSON(read))
		}
	}
}
