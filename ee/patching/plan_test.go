package patching

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Fail closed (#2575): an outcome the plan does not name, and an app it gave no cause, are never read as nothing to do.
func TestAnUnknownCauseIsNeverNothingToDo(t *testing.T) {
	if why := whyWaiting("sideways", 0, 0, false, false); why != whyUnknown {
		t.Fatalf("an outcome the plan does not name: %q", why)
	}
	if line, want := (planSaid{run: "r", apps: map[string]string{"x": ""}}).String(), "runs: the patch plan onto r started 0 patch run(s): 1 not, for a reason the plan does not name (x)"; line != want {
		t.Fatalf("an app given no cause:\n got %s\nwant %s", line, want)
	}
}

// Every cause the plan keeps for the identities read is one the contract names (contracts/schema/patch.schema.json, the
// identities row's plan): a cause the schema does not know would fail the read's own check, and the dashboard would read
// it as unknown.
func TestEveryCauseThePlanKeepsIsTheContracts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "contracts", "schema", "patch.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions struct {
			IdentitiesRead struct {
				Properties struct {
					Plan struct {
						OneOf []struct {
							Properties struct {
								Why  struct{ Enum []string }
								Apps struct {
									Items struct {
										Properties struct{ Why struct{ Enum []string } }
									}
								}
							}
						}
					}
				}
			} `json:"identities_read"`
		}
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	var read, app []string
	for _, o := range schema.Definitions.IdentitiesRead.Properties.Plan.OneOf {
		read = append(read, o.Properties.Why.Enum...)
		app = append(app, o.Properties.Apps.Items.Properties.Why.Enum...)
	}
	if len(app) == 0 {
		t.Fatal("the schema's identities_read names no app cause: the test reads the wrong place")
	}
	for why, code := range causes {
		if !slices.Contains(app, code) && !slices.Contains(read, code) {
			t.Errorf("the plan keeps %q for %q, which the contract does not name", code, why)
		}
	}
}
