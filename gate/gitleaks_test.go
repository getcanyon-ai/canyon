package gate_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
)

// gitleaksRun is a report's results as gitleaks' SARIF writes them: rule, uri, region.
type gitleaksRun struct {
	Runs []struct {
		Results []struct {
			RuleID    string
			Locations []struct {
				PhysicalLocation struct {
					ArtifactLocation struct{ URI string }
					Region           struct{ StartLine, StartColumn, EndLine, EndColumn int }
				}
			}
		}
	}
}

// leaks runs the producer over files, planted under a temp root, and answers its report.
func leaks(t *testing.T, files map[string]string) ([]byte, gitleaksRun, gate.Tree) {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := gate.Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := gate.Gitleaks(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	var rep gitleaksRun
	if err := json.Unmarshal(raw, &rep); err != nil || len(rep.Runs) != 1 {
		t.Fatalf("not one run of SARIF: %v", err)
	}
	return raw, rep, tree
}

// Keys built at run time, so no source file of the repo carries one whole (gitleaks scans this repo too).
var (
	awsKey    = "AKIA" + "QJ5K7MZ2RT4WX6YB"
	stripeKey = "sk_" + "test_" + "51MZq8fQ2xYv7Kp3Lw9Rt4Nb"
)

// A Python app's AWS key and Stripe test key — which no Canyon scanner read — are gitleaks' findings,
// each at its 1-based line, its region the whole line, and the report carries no byte of either key.
func TestGitleaksFindsAPythonAppsKeysAndKeepsNone(t *testing.T) {
	raw, rep, _ := leaks(t, map[string]string{
		"app.py":     "import boto3\n\nclient = boto3.client('s3', aws_access_key_id='" + awsKey + "')\n",
		"billing.py": "# billing\nstripe.api_key = \"" + stripeKey + "\"  # gitleaks:allow 😀\n",
	})
	got := map[string]string{}
	for _, r := range rep.Runs[0].Results {
		pl := r.Locations[0].PhysicalLocation
		got[r.RuleID] = pl.ArtifactLocation.URI + ":" + strings.Join([]string{itoa(pl.Region.StartLine), itoa(pl.Region.StartColumn), itoa(pl.Region.EndLine), itoa(pl.Region.EndColumn)}, ",")
	}
	want := map[string]string{"aws-access-token": "app.py:3,1,3,70", "stripe-access-token": "billing.py:2,1,2,73"} //nolint:gosec // rule ids and places, no credential; 73: the emoji is two UTF-16 units
	for rule, at := range want {
		if got[rule] != at {
			t.Errorf("%s at %q, want %q (all: %v)", rule, got[rule], at, got)
		}
	}
	for _, key := range []string{awsKey, stripeKey, awsKey[4:12], stripeKey[8:16]} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("the report carries %q", key)
		}
	}
}

// Its report attaches as the tool gitleaks: the attach takes every result, none refused or dropped.
func TestGitleaksReportAttaches(t *testing.T) {
	tree, err := gate.Walk("testdata/apps/claims-portal")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := gate.Gitleaks(context.Background(), tree)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := sarif.NormalizeAs(raw, &snap, tree, "gitleaks")
	if err != nil || rep.Dropped != 0 || len(rep.Results) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	if pl := rep.Results[0].Locations[0].PhysicalLocation; pl.ArtifactLocation.URI != "apps/web/src/lib/payments.ts" || pl.Region.StartLine != 14 ||
		pl.Region.StartColumn != 1 || pl.Region.EndColumn != 65 || pl.Region.Snippet.Text != "••••" {
		t.Fatalf("the claims portal's key: %+v", pl)
	}
}

// A rule on a file's name alone — gitleaks' pkcs12-file — names the whole file, as SARIF's 1:1 does, and
// attaches as a file-level result.
func TestAFileNameFindingIsTheWholeFile(t *testing.T) {
	_, rep, _ := leaks(t, map[string]string{"certs/client.p12": "not really a keystore\n"})
	if len(rep.Runs[0].Results) != 1 {
		t.Fatalf("results: %+v", rep.Runs[0].Results)
	}
	r := rep.Runs[0].Results[0]
	if g := r.Locations[0].PhysicalLocation.Region; r.RuleID != "pkcs12-file" || g.StartLine != 1 || g.StartColumn != 1 || g.EndLine != 1 || g.EndColumn != 1 {
		t.Fatalf("%s at %+v", r.RuleID, g)
	}
}

// Gitleaks' harder places land on the right whole lines: a key base64-encoded in a CRLF file (decoded,
// placed on its line in the file, the CR not a column), and a private key spanning lines whose last
// line has no newline (gitleaks ends such a match where it starts; Canyon counts its lines).
func TestEncodedAndMultiLineKeysAreTheirWholeLines(t *testing.T) {
	body := strings.Repeat("MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n", 3)
	encoded := base64.StdEncoding.EncodeToString([]byte("aws_access_key_id=" + awsKey))
	_, rep, _ := leaks(t, map[string]string{
		"config.env": "# config\r\nBLOB=" + encoded + "\r\nDEBUG=1\r\n",
		"id_rsa":     "-----BEGIN RSA " + "PRIVATE KEY-----\n" + body + "-----END RSA " + "PRIVATE KEY-----",
	})
	got := map[string]string{}
	for _, r := range rep.Runs[0].Results {
		g := r.Locations[0].PhysicalLocation.Region
		got[r.Locations[0].PhysicalLocation.ArtifactLocation.URI+" "+r.RuleID] = strings.Join([]string{itoa(g.StartLine), itoa(g.StartColumn), itoa(g.EndLine), itoa(g.EndColumn)}, ",")
	}
	for at, want := range map[string]string{
		"config.env aws-access-token": "2,1,2," + itoa(len("BLOB="+encoded)+1),
		"id_rsa private-key":          "1,1,5,30",
	} {
		if got[at] != want {
			t.Errorf("%s at %q, want %q (all: %v)", at, got[at], want, got)
		}
	}
}

// The same tree makes the same bytes, whatever order gitleaks found things in; a cancelled scan is an
// error, never a short report.
func TestTheReportIsTheSameEveryTimeAndACancelFails(t *testing.T) {
	files := map[string]string{"a.py": "keys = ['" + awsKey + "', '" + stripeKey + "']\n", "b.py": "k = '" + stripeKey + "'\n"}
	first, _, tree := leaks(t, files)
	for range 20 {
		again, err := gate.Gitleaks(context.Background(), tree)
		if err != nil || !bytes.Equal(again, first) {
			t.Fatalf("a second report differs: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if raw, err := gate.Gitleaks(ctx, tree); err == nil {
		t.Fatalf("a cancelled scan answered %d bytes", len(raw))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// gitleaks' catch-all generic-api-key in a documentation file of prose is a warning, never a blocking
// secret: every stock NestJS app's README carries a placeholder token in its CircleCI badge (#1683). It
// keeps the row's severity, critical, so sarif.gitleaks set blocking blocks it, and its region is hidden whole, as
// every gitleaks result's is, so no value gitleaks decoded shows. The same pattern in code — a
// License.tsx, a docs/ app's config — and a key whose format names its provider in a README stay
// secrets.client-bundle's (contracts/snapshot.md, Tool results mapped to a policy).
func TestAPlaceholderInTheDocsWarnsAndARealKeyThereStillBlocks(t *testing.T) {
	// built at run time, so no file of the repo carries a value gitleaks matches (it scans this repo too)
	placeholder, key := "abc123"+"def456", "q8Zr4LmN"+"2vXw9TkB7pHs"
	blob := base64.StdEncoding.EncodeToString([]byte("api_" + "key = \"" + key + "\"")) // gitleaks decodes it
	generic := "export const apiKey = \"" + key + "\";\n"
	raw, _, tree := leaks(t, map[string]string{
		"README.md":                  "[circleci-image]: https://img.shields.io/circleci/build/github/nestjs/nest/master?token=" + placeholder + "\n", // the stock NestJS starter's line 5
		"docs/blob.md":               "Config blob: " + blob + "\n",
		"docs/setup.md":              "Billing uses `" + stripeKey + "`.\n",
		"src/config.ts":              "export const authToken = \"" + placeholder + "\";\n",
		"src/components/License.tsx": generic,
		"docs/.vitepress/config.ts":  generic,
	})
	snap, err := (&policy.Builder{}).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := sarif.NormalizeAs(raw, &snap, tree, "gitleaks")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range rep.Results {
		got[r.Locations[0].PhysicalLocation.ArtifactLocation.URI] = r.RuleID + " " + string(r.Properties.Canyon.Severity)
	}
	if out, _ := json.Marshal(rep); bytes.Contains(out, []byte(placeholder)) || bytes.Contains(out, []byte(blob[:12])) || bytes.Contains(out, []byte(key)) {
		t.Fatalf("a matched value is in the report: %s", out)
	}
	want := map[string]string{ // critical: blocking wherever its rule is, sarif.gitleaks's for the spared ones
		"README.md":                  "sarif.gitleaks.generic-api-key critical",
		"docs/blob.md":               "sarif.gitleaks.generic-api-key critical",
		"docs/setup.md":              "secrets.client-bundle.gitleaks.stripe-access-token critical",
		"src/config.ts":              "secrets.client-bundle.gitleaks.generic-api-key critical",
		"src/components/License.tsx": "secrets.client-bundle.gitleaks.generic-api-key critical",
		"docs/.vitepress/config.ts":  "secrets.client-bundle.gitleaks.generic-api-key critical",
	}
	if len(got) != len(want) {
		t.Fatalf("results %v", got)
	}
	for uri, w := range want {
		if got[uri] != w {
			t.Errorf("%s: %s, want %s", uri, got[uri], w)
		}
	}
}
