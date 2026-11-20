// Package contracts holds the seams in executable form. A seam without a failing check is
// a document, not a contract (AGENTS.md rule 5): the schemas under schema/ and vendor/ are
// embedded here so the gate checks producer output against the same files the CI check
// reads, and so nothing on a gate path needs the network.
package contracts

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
)

//go:embed vendor/sarif-2.1.0.schema.json
var sarifSchema []byte

//go:embed schema/finding.schema.json
var findingSchema []byte

//go:embed schema/snapshot.schema.json
var snapshotSchema []byte

//go:embed schema/egress.schema.json
var egressSchema []byte

// TelemetrySchema is the daily report's schema (#2807): the collector keeps a report only if it passes (#2808).
//
//go:embed schema/telemetry.schema.json
var TelemetrySchema []byte

// GoldenLog is the recorded claims-portal log, the seam-1 fixture, embedded for the run
// driver's stand-in gate until the gate library lands.
//
//go:embed testdata/finding/claims-portal.sarif.json
var GoldenLog []byte

// Snapshot is the policy snapshot fixture (contracts/snapshot.md), embedded for the server:
// v1 reads it once at boot; stored snapshots come with the policy screen.
//
//go:embed testdata/snapshot/meridian.json
var Snapshot []byte

// findingSchemas compiles once: SARIF 2.1.0 first, then the profile. Each is registered
// under its own $id with a loader that resolves no scheme at all, so a $ref that leaves the
// embedded file fails to compile instead of reaching the network or the filesystem.
var findingSchemas = sync.OnceValues(func() ([]*jsonschema.Schema, error) {
	var out []*jsonschema.Schema
	for _, raw := range [][]byte{sarifSchema, findingSchema} {
		sch, err := compileSchema(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, sch)
	}
	return out, nil
})

var snapshotSchemas = sync.OnceValues(func() (*jsonschema.Schema, error) { return compileSchema(snapshotSchema) })

var policySchema = sync.OnceValues(func() (*jsonschema.Schema, error) { return compileSchema(egressSchema, "#/definitions/policy") })

// egressDefinitions is egress.schema.json's definitions, which knownOnly walks.
var egressDefinitions = sync.OnceValues(func() (map[string]any, error) {
	var doc struct {
		Definitions map[string]any `json:"definitions"`
	}
	err := json.Unmarshal(egressSchema, &doc)
	return doc.Definitions, err
})

// compileSchema compiles one schema under its own $id, or the definition at fragment within it; a $ref that
// leaves it is an error.
func compileSchema(raw []byte, fragment ...string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("embedded schema is not JSON: %w", err)
	}
	obj, _ := doc.(map[string]any)
	id, _ := obj["$id"].(string)
	if id == "" {
		return nil, errors.New("embedded schema has no $id to compile under")
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(jsonschema.SchemeURLLoader{})
	if err := c.AddResource(id, doc); err != nil {
		return nil, err
	}
	return c.Compile(id + strings.Join(fragment, ""))
}

// maxNumberLen bounds a numeric literal. The schemas' integer keywords turn each number into
// an exact rational, so a compact literal like 1e999999 costs gigabytes; nothing in a
// finding or a snapshot needs more digits than this.
const maxNumberLen = 32

// CheckFindingSchemas checks one serialized SARIF log against the two schemas of seam 1:
// SARIF 2.1.0, then the Canyon profile. That is all it checks. The invariants a schema
// cannot express — level projected from severity, title = message.text, unique
// fingerprints, secret_name only on secrets.*, no live-key literal anywhere — live in
// gate/finding.Validate (Go) and contracts/validate.py (CI); blocks, namespaces and
// policy_name against the snapshot are checked by gate/finding.Validate. A gate runs all
// of it, or it has not checked seam 1.
//
// The error names the schema location and keyword that failed and the array indexes on
// the way there, never a value or a name from the log: a producer that puts a credential
// where the profile expects a name, or as a fingerprint key, must not have it echoed into
// whatever records the error. The caller bounds the size of log.
func CheckFindingSchemas(log []byte) error {
	schemas, err := findingSchemas()
	if err != nil {
		return err
	}
	return check("finding log", log, schemas...)
}

// CheckSnapshotSchema checks one serialized policy snapshot against seam 5's schema. The
// invariants it cannot express — the twenty-one rules as EXPERIENCE D2 states them, unique ids,
// each id inside its producer's namespace, new rules installing off — live in gate/policy
// (Go) and contracts/validate.py (CI). Errors are value-free, as for the finding log.
func CheckSnapshotSchema(raw []byte) error {
	sch, err := snapshotSchemas()
	if err != nil {
		return err
	}
	return check("policy snapshot", raw, sch)
}

// CheckEgressPolicy checks one GET /egress/policy answer against the fields seam 3's policy definition names,
// strictly, ignoring every field it does not name, which a later server may add (contracts/routes.md, The
// check): egress-control refuses, whole, a policy that fails it. Errors are value-free, as for the finding log.
func CheckEgressPolicy(raw []byte) error {
	sch, err := policySchema()
	if err != nil {
		return err
	}
	defs, err := egressDefinitions()
	if err != nil {
		return err
	}
	doc, err := decode(raw)
	if err != nil {
		return fmt.Errorf("egress policy is not JSON: %w", err)
	}
	if err := checkNumbers(doc, nil); err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	if err := knownOnly(doc, defs["policy"], defs); err != nil {
		return fmt.Errorf("egress policy: %w", err)
	}
	if err := sch.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(describe("egress policy", doc, ve))
		}
		return err
	}
	return nil
}

// knownOnly drops from v every property the schema s does not name, where s names its properties, through
// arrays' items and $refs into defs. A property that differs from a named one only by case is refused, never
// dropped: Go's encoding/json matches names case-blind, so a consumer would read "Callers" as the callers this
// check ignored. decode has refused a name given twice already.
func knownOnly(v, s any, defs map[string]any) error {
	node, _ := s.(map[string]any)
	for ref, ok := node["$ref"].(string); ok; ref, ok = node["$ref"].(string) {
		var at any = defs
		for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/definitions/"), "/") {
			m, _ := at.(map[string]any)
			at = m[seg]
		}
		node, _ = at.(map[string]any)
	}
	switch x := v.(type) {
	case map[string]any:
		props, ok := node["properties"].(map[string]any)
		if !ok {
			return nil
		}
		for k, e := range x {
			if p, named := props[k]; named {
				if err := knownOnly(e, p, defs); err != nil {
					return err
				}
				continue
			}
			for name := range props {
				if strings.EqualFold(name, k) {
					return fmt.Errorf("a name differing from %s only by case", name)
				}
			}
			delete(x, k)
		}
	case []any:
		for _, e := range x {
			if err := knownOnly(e, node["items"], defs); err != nil {
				return err
			}
		}
	}
	return nil
}

// check decodes raw strictly and validates it against each schema in order; what names the
// document in the error.
func check(what string, raw []byte, schemas ...*jsonschema.Schema) error {
	doc, err := decode(raw)
	if err != nil {
		return fmt.Errorf("%s is not JSON: %w", what, err)
	}
	if err := checkNumbers(doc, nil); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	for _, sch := range schemas {
		if err := sch.Validate(doc); err != nil {
			var ve *jsonschema.ValidationError
			if errors.As(err, &ve) {
				return errors.New(describe(what, doc, ve))
			}
			return err
		}
	}
	return nil
}

// decode parses a document into the shape the validator walks — objects, arrays, strings,
// json.Number, bool, nil — and refuses a duplicate name inside an object. encoding/json
// keeps the last duplicate; a first-wins consumer of the same bytes (the raw log is kept
// verbatim and uploaded to code scanning) would read a document the gate never checked.
func decode(log []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(log))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the top-level value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key := keyTok.(string) // encoding/json guarantees a string here
				if _, dup := obj[key]; dup {
					return nil, errors.New("an object repeats a name")
				}
				if obj[key], err = decodeValue(dec); err != nil {
					return nil, err
				}
			}
			_, err = dec.Token() // the closing brace
			return obj, err
		case '[':
			arr := []any{}
			for dec.More() {
				e, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, e)
			}
			_, err = dec.Token() // the closing bracket
			return arr, err
		}
		return nil, fmt.Errorf("unexpected %v", t)
	default:
		return tok, nil
	}
}

// checkNumbers bounds every numeric literal; at holds only array indexes, never a name a
// producer chose (map keys under fingerprints or capabilities are producer data).
func checkNumbers(v any, at []int) error {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			if err := checkNumbers(e, at); err != nil {
				return err
			}
		}
	case []any:
		for i, e := range x {
			if err := checkNumbers(e, append(at, i)); err != nil {
				return err
			}
		}
	case json.Number:
		if _, err := strconv.ParseFloat(string(x), 64); err != nil || len(x) > maxNumberLen {
			return fmt.Errorf("a number under indexes %v is not representable", at)
		}
	}
	return nil
}

// describe renders a validation error tree value-free: the schema location that failed,
// the keyword, the array indexes on the instance path, and for a missing property its name
// (schema vocabulary). Instance names are dropped — under an open map such as
// partialFingerprints they are producer data, a digit-only one too: a segment is an index
// only where doc holds an array. At most maxCauses leaves.
func describe(what string, doc any, ve *jsonschema.ValidationError) string {
	const maxCauses = 20
	var lines []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(lines) >= maxCauses {
			return
		}
		if len(e.Causes) == 0 {
			var indexes []string
			at := doc
			for _, seg := range e.InstanceLocation {
				switch x := at.(type) {
				case []any:
					if i, err := strconv.Atoi(seg); err == nil && i < len(x) { // the validator's own index into this array
						indexes, at = append(indexes, seg), x[i]
					}
				case map[string]any:
					at = x[seg]
				}
			}
			line := fmt.Sprintf("%s: %s at indexes [%s]", e.SchemaURL, strings.Join(e.ErrorKind.KeywordPath(), "/"), strings.Join(indexes, " "))
			if k, ok := e.ErrorKind.(*kind.Required); ok {
				line += " (missing " + strings.Join(k.Missing, ", ") + ")"
			}
			lines = append(lines, line)
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return what + " fails the schema:\n  " + strings.Join(lines, "\n  ")
}
