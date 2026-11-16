package telemetrydoc

import (
	"os"
	"strings"
	"testing"
)

// TELEMETRY.md is what Render makes of the schema, byte for byte: a field added to the report without the page, or
// the page edited by hand, fails here. CANYON_UPDATE_DOCS=1 (make telemetry-docs) rewrites it.
func TestTelemetryMDIsTheSchema(t *testing.T) {
	schema, err := os.ReadFile("../../../contracts/schema/telemetry.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := Render(schema)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CANYON_UPDATE_DOCS") == "1" {
		if err := os.WriteFile("../../../TELEMETRY.md", []byte(want), 0o644); err != nil { //nolint:gosec // a page in the repo, read by everyone
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile("../../../TELEMETRY.md")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("TELEMETRY.md is not what contracts/schema/telemetry.schema.json says: run make telemetry-docs")
	}
	for _, field := range []string{"instance_id", "rules_fired", "unsupported"} { // the generator is not handing back an empty table
		if !strings.Contains(want, "| `"+field+"` |") {
			t.Errorf("the page has no row for %s:\n%s", field, want)
		}
	}
}
