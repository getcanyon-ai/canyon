package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	sarifID   = "sarif-schema-2.1.0.json"
	profileID = "finding.schema.json"
)

// The golden log — the three canonical claims-portal findings (EXPERIENCE D4) — is plain
// SARIF the tools understand and the profile Canyon requires. Both, or it is not a finding.
func TestGoldenLogPassesBothSchemas(t *testing.T) {
	golden, err := os.ReadFile("testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckFindingSchemas(golden); err != nil {
		t.Fatalf("golden log:\n%v", err)
	}
}

// One broken contract field fails the schema that owns it, at that field — each schema is
// live on its own, and the error says which one fired and where, never what the value was.
func TestBrokenGoldenLogFailsTheOwningSchema(t *testing.T) {
	golden, err := os.ReadFile("testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ from, to, schema, at string }{
		"profile: severity outside the four":                    {`"severity": "critical"`, `"severity": "sev-critical"`, profileID, "canyon/properties/severity: enum at indexes [2 0]"},
		"profile: region without an end":                        {`"endColumn": 65,`, ``, profileID, "definitions/region: required at indexes [2 0 0] (missing endColumn)"},
		"profile: line over the contract's maximum":             {`"startLine": 14,`, `"startLine": 1000000001,`, profileID, "region/properties/startLine: maximum at indexes [2 0 0]"},
		"profile: title over 120 characters":                    {`"title": "Found a Stripe Access Token, posing a risk to payment processing services and sensitive financial data"`, `"title": "` + strings.Repeat("x", 121) + `"`, profileID, "canyon/properties/title: maxLength at indexes [2 0]"},
		"sarif: unknown top-level property":                     {`"version": "2.1.0",`, `"version": "2.1.0", "canyonExtras": 1,`, sarifID, "sarif-schema-2.1.0.json#: additionalProperties at indexes []"},
		"profile: a fingerprint that is not the recipe's shape": {`"f_c351cc0ee0961d1358536ca1d3a581a2853f97607b3887a60eeebfaccc751d75"`, `"f_abcd"`, profileID, "partialFingerprints/properties/canyon~1v1: pattern at indexes [2 0]"},
	} {
		t.Run(name, func(t *testing.T) {
			if n := bytes.Count(golden, []byte(c.from)); n != 1 {
				t.Fatalf("the golden log contains %q %d times, the mutation needs exactly one", c.from, n)
			}
			err := CheckFindingSchemas(bytes.Replace(golden, []byte(c.from), []byte(c.to), 1))
			if err == nil || !strings.Contains(err.Error(), c.schema) || !strings.Contains(err.Error(), c.at) {
				t.Fatalf("got %v\nwant a failure of %s at %s", err, c.schema, c.at)
			}
			if strings.Contains(err.Error(), "sev-critical") || strings.Contains(err.Error(), "xxxx") || strings.Contains(err.Error(), "canyonExtras") {
				t.Fatalf("the error echoes a value or a name from the log:\n%v", err)
			}
		})
	}
}

// The profile's cardinality rule — exactly one location — is SARIF-legal and profile-illegal.
func TestTwoLocationsFailTheProfile(t *testing.T) {
	golden, err := os.ReadFile("testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	var log map[string]any
	if err := json.Unmarshal(golden, &log); err != nil {
		t.Fatal(err)
	}
	result := log["runs"].([]any)[2].(map[string]any)["results"].([]any)[0].(map[string]any)
	result["locations"] = append(result["locations"].([]any), result["locations"].([]any)[0])
	doubled, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	err = CheckFindingSchemas(doubled)
	if err == nil || !strings.Contains(err.Error(), profileID) || !strings.Contains(err.Error(), "properties/locations: maxItems at indexes [2 0]") {
		t.Fatalf("got %v, want the profile to refuse two locations", err)
	}
}

// Profile rules SARIF itself does not have: every region carries its snippet and nothing SARIF
// allows beside it, a run's columnKind is code points when it is set, there is no context region
// (its lines are unreviewed source, canyonv2-9ah), and a location carries nothing beside its physicalLocation.
func TestTheProfileRefusesWhatSARIFAllowsAroundARegion(t *testing.T) {
	golden, err := os.ReadFile("testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	physical := func(log map[string]any) map[string]any {
		result := log["runs"].([]any)[2].(map[string]any)["results"].([]any)[0].(map[string]any)
		return result["locations"].([]any)[0].(map[string]any)["physicalLocation"].(map[string]any)
	}
	for name, c := range map[string]struct {
		mutate func(log map[string]any)
		at     string
	}{
		"a region without its snippet":                  {func(log map[string]any) { delete(physical(log)["region"].(map[string]any), "snippet") }, "definitions/region: required at indexes [2 0 0] (missing snippet)"},
		"a run that does not say how it counts columns": {func(log map[string]any) { delete(log["runs"].([]any)[0].(map[string]any), "columnKind") }, "required at indexes [0] (missing columnKind)"},
		"columns in UTF-16 code units":                  {func(log map[string]any) { log["runs"].([]any)[0].(map[string]any)["columnKind"] = "utf16CodeUnits" }, "properties/columnKind: const at indexes [0]"},
		"a snippet carrying more than its text": {func(log map[string]any) {
			physical(log)["region"].(map[string]any)["snippet"].(map[string]any)["binary"] = "c2tfbGl2ZV8="
		}, "region/properties/snippet: additionalProperties at indexes [2 0 0]"},
		"a region field the profile does not have": {func(log map[string]any) { physical(log)["region"].(map[string]any)["charOffset"] = 0 }, "definitions/region: additionalProperties at indexes [2 0 0]"},
		"a context region, snippet and all": {func(log map[string]any) {
			physical(log)["contextRegion"] = map[string]any{"startLine": 12, "endLine": 16, "snippet": map[string]any{"text": "a neighbouring line"}}
		}, "location/properties/physicalLocation: additionalProperties at indexes [2 0 0]"},
		"a location field the profile does not have": {func(log map[string]any) {
			result := log["runs"].([]any)[2].(map[string]any)["results"].([]any)[0].(map[string]any)
			result["locations"].([]any)[0].(map[string]any)["message"] = map[string]any{"text": "a location message"}
		}, "definitions/location: additionalProperties at indexes [2 0 0]"},
	} {
		t.Run(name, func(t *testing.T) {
			var log map[string]any
			if err := json.Unmarshal(golden, &log); err != nil {
				t.Fatal(err)
			}
			c.mutate(log)
			mutated, err := json.Marshal(log)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckFindingSchemas(mutated)
			if err == nil || !strings.Contains(err.Error(), profileID) || !strings.Contains(err.Error(), c.at) {
				t.Fatalf("got %v\nwant the profile to refuse it at %s", err, c.at)
			}
		})
	}
}

// Anything that is not one SARIF object fails closed: not JSON, not an object, trailing
// data, a repeated name, or a number that would cost the validator unbounded memory. And
// nothing the log said — value or name — comes back in the error.
func TestNotALogFailsClosed(t *testing.T) {
	for name, log := range map[string]string{
		"empty":                     "",
		"null":                      "null",
		"array":                     "[]",
		"trailing data":             `{"version":"2.1.0","runs":[]} {}`,
		"number bomb":               `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"x"}},"results":[{"message":{"text":"t"},"locations":[{"physicalLocation":{"region":{"startLine":1e999999}}}]}]}]}`,
		"number with 33 digits":     `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"x"}},"results":[{"message":{"text":"t"},"locations":[{"physicalLocation":{"region":{"startLine":100000000000000000000000000000000}}}]}]}]}`,
		"a value where a name goes": `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"x"}},"results":[{"message":{"text":"t"},"properties":{"canyon":{"secret_name":"a-value-not-a-name-7f3a"}}}]}]}`,
		"a value as a map key":      `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"x"}},"results":[{"message":{"text":"t"},"fingerprints":{"a-value-as-a-key-7f3a":1}}]}]}`,
		"duplicate name, last wins": `{"version":"9.9.9","version":"2.1.0","runs":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := CheckFindingSchemas([]byte(log))
			if err == nil {
				t.Fatal("accepted a log that is not a Canyon SARIF log")
			}
			if strings.Contains(err.Error(), "7f3a") {
				t.Fatalf("the error echoes a value from the log:\n%v", err)
			}
		})
	}
}

// A digit-only name under an open map is the producer's, never an array index: the error at its value
// names the indexes on the way there and leaves the name out (#683).
func TestADigitOnlyMapKeyIsNeverPrintedAsAnIndex(t *testing.T) {
	golden, err := os.ReadFile("testdata/finding/claims-portal.sarif.json")
	if err != nil {
		t.Fatal(err)
	}
	fp := []byte(`"canyon/v1": "f_1ddb72ad9a5d06de4d2cb9c67e3fb584437c9f58dcc7f33b4f026c8fc7484dbf"`)
	if n := bytes.Count(golden, fp); n != 1 {
		t.Fatalf("the golden log contains the fingerprint %d times, the mutation needs exactly one", n)
	}
	err = CheckFindingSchemas(bytes.Replace(golden, fp, append(fp, `, "20261017": 1`...), 1))
	if err == nil || !strings.Contains(err.Error(), "partialFingerprints/additionalProperties: type at indexes [1 0]") || strings.Contains(err.Error(), "20261017") {
		t.Fatalf("got %v\nwant a failure at the fingerprint's value, under indexes [1 0], that never names its key", err)
	}
}

// The snapshot fixture passes seam 5's schema, and a state outside the three fails it at
// that rule without the value coming back in the error.
func TestSnapshotFixturePassesAndAFourthStateFails(t *testing.T) {
	if err := CheckSnapshotSchema(Snapshot); err != nil {
		t.Fatalf("the fixture:\n%v", err)
	}
	const from = `refetch on user input","state": "advisory", "producer": "llm-review"`
	if n := bytes.Count(Snapshot, []byte(from)); n != 1 {
		t.Fatalf("the fixture contains %q %d times, the mutation needs exactly one", from, n)
	}
	err := CheckSnapshotSchema(bytes.Replace(Snapshot, []byte(from), []byte(`refetch on user input","state": "on-7f3a", "producer": "llm-review"`), 1))
	if err == nil || !strings.Contains(err.Error(), "properties/state: enum at indexes [8]") {
		t.Fatalf("got %v\nwant the schema to refuse the state of rule 8", err)
	}
	if strings.Contains(err.Error(), "7f3a") {
		t.Fatalf("the error echoes a value from the snapshot:\n%v", err)
	}
}

// The effective-snapshot golden splits a rule between the two gates and passes seam 5's schema; a
// go-live rule placed before preview fails it at that rule (snapshot.md, Two gates).
func TestAGoLiveRuleTakesNoProductionState(t *testing.T) {
	golden, err := os.ReadFile("testdata/snapshot/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSnapshotSchema(golden); err != nil {
		t.Fatalf("the golden:\n%v", err)
	}
	const from = `"A person promotes",                 "state": "blocking",`
	if n := bytes.Count(golden, []byte(from)); n != 1 {
		t.Fatalf("the golden contains %q %d times, the mutation needs exactly one", from, n)
	}
	err = CheckSnapshotSchema(bytes.Replace(golden, []byte(from), []byte(from+` "production": "advisory",`), 1))
	if err == nil || !strings.Contains(err.Error(), "rules/items/allOf/0/then:") || !strings.Contains(err.Error(), "at indexes [3]") {
		t.Fatalf("got %v\nwant the schema to refuse rule 3's production", err)
	}
}

// The compiler resolves no scheme: a schema whose $ref leaves the embedded file fails to
// compile instead of reaching the network or the filesystem, and one without an $id is refused.
func TestSchemasResolveNothingOutsideThemselves(t *testing.T) {
	for name, raw := range map[string]string{
		"https ref": `{"$id":"https://example.com/a.json","$ref":"https://example.com/b.json"}`,
		"file ref":  `{"$id":"https://example.com/a.json","$ref":"file:///etc/hostname"}`,
		"no $id":    `{"type":"object"}`,
	} {
		if _, err := compileSchema([]byte(raw)); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}
