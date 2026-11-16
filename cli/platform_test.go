package cli

import (
	"cmp"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// knownVulnStatus is the fixture's status read with platform.known-vuln's row as the test cluster answered it for
// v0.1.13 (#2300): six packages Canyon can fix.
func knownVulnStatus(t *testing.T) (statusRead, *runRow) {
	t.Helper()
	s, latest := reads(t, page(t), "status")
	raw, err := os.ReadFile(filepath.Join("testdata", "known-vuln-row.json"))
	if err != nil {
		t.Fatal(err)
	}
	var row receiptRule
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	s.Receipt.Rules = append(s.Receipt.Rules, row)
	return s, latest
}

// Status names the packages Canyon can fix under platform.known-vuln's row, never a count alone: the
// first three, each with an advisory and what fixes it, then how to read every one (cli.md §6.1; #2300).
func TestStatusNamesTheKnownVulnerablePackages(t *testing.T) {
	s, latest := knownVulnStatus(t)
	wants(t, "status-known-vuln.txt", rendered(t, s, latest))
}

// (beta) reads the same on every row it marks (EXPERIENCE.md D5, #2384): plain, a space after the row's own
// words, painted on neither the outbound row nor platform.known-vuln's.
func TestBetaReadsTheSameOnEveryRow(t *testing.T) {
	s, latest := knownVulnStatus(t)
	s.Outbound = egressRead(t, "canyon")
	var painted strings.Builder
	(&app{out: &painted, render: Render{Color: true}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	out := painted.String()
	if strings.Count(out, "(beta)") != 2 || strings.Count(out, " (beta)") != 2 || strings.Contains(out, "m(beta)") || strings.Contains(out, "(beta)\x1b") {
		t.Errorf("(beta) is not said once a row, alike on both:\n%q", out)
	}
}

// Under --format ai and json an agent reads every package and every advisory, with what fixes each.
func TestStatusUnderAIAndJSONNamesEveryPackage(t *testing.T) {
	s, latest := knownVulnStatus(t)
	var out strings.Builder
	(&app{out: &out, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai-known-vuln.txt", out.String())
	raw, err := json.Marshal(statusJSON{App: "claims-portal", Receipt: &s.Receipt})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"undici","version":"6.28.0","advisories":[{"id":"CVE-2026-19534","fixed":"6.28.1, 7.29.1, 8.10.2"}]`,
		`{"id":"CVE-2026-97031","fixed":"1.26.9, 1.27.2"}`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("status --format json carries no %s:\n%s", want, raw)
		}
	}
}

// The test cluster's Node app on preview (611ca7f2, #2579): the platform row said Ubuntu had a fix waiting while the row
// below led with npm and Go packages of the Node buildpack's and the lifecycle's layers, which no rebase fixes. To a
// contributor the receipt names each package's layers, so status says what fixes each: the system libraries' first,
// under no fix from Canyon yet, then the rest under its next build tools update, and Go's standard
// library by that name; under --format ai each package line ends with what it waits for.
func TestStatusSaysWhatFixesEachKnownVulnerablePackage(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	layer := func(owner, class string) []packageLayer { return []packageLayer{{Owner: owner, Class: class}} }
	advisory := func(id, fixed string) []platformAdvisory { return []platformAdvisory{{ID: id, Fixed: fixed}} }
	s.Receipt.Rules = append(s.Receipt.Rules, receiptRule{ID: "platform.known-vuln", Name: "No known-vulnerable system libraries or build tools",
		Why: "5 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds", Packages: []platformPackage{
			{Name: "tar", Version: "7.5.11", Advisories: advisory("CVE-2026-23745", "7.5.12"), Layers: layer("paketo-buildpacks/node-engine", "runtime")},
			{Name: "brace-expansion", Version: "2.0.2", Advisories: advisory("CVE-2026-102276", "2.0.3"), Layers: layer("paketo-buildpacks/node-engine", "runtime")},
			{Name: "github.com/containerd/containerd/v2", Version: "v2.3.3", Advisories: advisory("CVE-2026-31337", "2.3.4"), Layers: layer("lifecycle", "lifecycle")},
			{Name: "libbrotli1", Version: "1.1.0-2build2", Advisories: advisory("CVE-2025-6176", "1.1.0-2ubuntu0.1"), Layers: layer("run-image", "os")},
			{Name: "stdlib", Version: "v1.26.5", GoStdlib: true, Advisories: advisory("CVE-2026-33818", "1.26.9"), Layers: layer("lifecycle", "lifecycle")},
		}})
	human := rendered(t, s, latest)
	if want := "      no fix from Canyon yet:\n" +
		"      libbrotli1 1.1.0-2build2 · CVE-2025-6176 fixed in 1.1.0-2ubuntu0.1\n" +
		"      waits for Canyon's next build tools update:\n" +
		"      tar 7.5.11 · CVE-2026-23745 fixed in 7.5.12\n" +
		"      brace-expansion 2.0.2 · CVE-2026-102276 fixed in 2.0.3\n" +
		"      and 2 more: canyon status --format ai lists every one\n"; !strings.Contains(human, want) {
		t.Errorf("want\n%s\nin\n%s", want, human)
	}
	var ai strings.Builder
	(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	for _, want := range []string{
		"    package: tar 7.5.11 · CVE-2026-23745 fixed in 7.5.12 · waits for Canyon's next build tools update\n",
		"    package: github.com/containerd/containerd/v2 v2.3.3 · CVE-2026-31337 fixed in 2.3.4 · waits for Canyon's next build tools update\n",
		"    package: libbrotli1 1.1.0-2build2 · CVE-2025-6176 fixed in 1.1.0-2ubuntu0.1 · no fix from Canyon yet\n",
		"    package: Go's standard library v1.26.5 · CVE-2026-33818 fixed in 1.26.9 · waits for Canyon's next build tools update\n",
	} {
		if !strings.Contains(ai.String(), want) {
			t.Errorf("want %q in\n%s", want, ai.String())
		}
	}
}

// #2594: a system libraries package has no fix from Canyon only while no patch of it is planned. While preview's
// patch is planned the system libraries Canyon uses now fix it, so status says Canyon is trying a fix, on the human
// checklist and under --format ai; once the patch is in preview the receipt reads the patched image, and what it still
// counts does wait. A build tools package waits for the build tools update whatever the state.
func TestPlatformPackagesFollowPreviewsPatch(t *testing.T) {
	layer := func(owner, class string) []packageLayer { return []packageLayer{{Owner: owner, Class: class}} }
	advisory := func(id, fixed string) []platformAdvisory { return []platformAdvisory{{ID: id, Fixed: fixed}} }
	for _, c := range []struct {
		state, human, ai string
	}{
		{"planned", "Canyon is trying a fix in preview", "Canyon is trying a fix in preview"},
		{"wait", "no fix from Canyon yet", "no fix from Canyon yet"},
		{"could_not_rescan", "no fix from Canyon yet", "no fix from Canyon yet"},
		{"patched_preview", "no fix from Canyon yet", "no fix from Canyon yet"},
		{"", "no fix from Canyon yet", "no fix from Canyon yet"},
	} {
		t.Run(cmp.Or(c.state, "none"), func(t *testing.T) {
			s, latest := reads(t, page(t), "status")
			if c.state != "" {
				preview := *s.Envs["preview"]
				preview.Patch = &envPatch{State: c.state, Since: "2026-10-30T09:00:00Z"}
				s.Envs = map[string]*env{"preview": &preview, "prod": s.Envs["prod"]}
			}
			s.Receipt.Rules = append(slices.Clone(s.Receipt.Rules), receiptRule{ID: "platform.known-vuln", Name: "No known-vulnerable system libraries or build tools",
				Why: "2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds", Packages: []platformPackage{
					{Name: "tar", Version: "7.5.11", Advisories: advisory("CVE-2026-23745", "7.5.12"), Layers: layer("paketo-buildpacks/node-engine", "runtime")},
					{Name: "libssl3", Version: "3.0.13-0ubuntu3.4", Advisories: advisory("CVE-2024-5535", "3.0.13-0ubuntu3.5"), Layers: layer("run-image", "os")},
				}})
			human := rendered(t, s, latest)
			if want := "      " + c.human + ":\n" +
				"      libssl3 3.0.13-0ubuntu3.4 · CVE-2024-5535 fixed in 3.0.13-0ubuntu3.5\n" +
				"      waits for Canyon's next build tools update:\n" +
				"      tar 7.5.11 · CVE-2026-23745 fixed in 7.5.12\n"; !strings.Contains(human, want) {
				t.Errorf("want\n%s\nin\n%s", want, human)
			}
			var ai strings.Builder
			(&app{out: &ai, render: Render{Format: FormatAI}}).renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
			for _, want := range []string{
				"    package: libssl3 3.0.13-0ubuntu3.4 · CVE-2024-5535 fixed in 3.0.13-0ubuntu3.5 · " + c.ai + "\n",
				"    package: tar 7.5.11 · CVE-2026-23745 fixed in 7.5.12 · waits for Canyon's next build tools update\n",
			} {
				if !strings.Contains(ai.String(), want) {
					t.Errorf("want %q in\n%s", want, ai.String())
				}
			}
		})
	}
}

// A promotion that goes ahead past platform.known-vuln as an advisory names, under --format ai, every package
// it counts below its advisory line, as status does (cli.md §6.3; #2300).
func TestAPromotionUnderAINamesTheKnownVulnerablePackages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "known-vuln-row.json"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	h := clean(t, page(t), http.StatusAccepted, started)
	h.status = mutate(t, h.status, func(read map[string]any) {
		r := read["receipt"].(map[string]any)
		r["rules"] = append(r["rules"].([]any), row)
	})
	out, errs, code := promoting(t, h, "", false, "--yes", "--format", "ai")
	want := "\nadvisory: No known-vulnerable system libraries or build tools (platform.known-vuln): 6 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 18 notes: no fix published, or Canyon can't tell what put it there\n" +
		"    package: brace-expansion 5.0.9 · CVE-2026-102276 fixed in 5.0.10, 3.0.7, 2.1.5, 1.1.19; CVE-2026-102278 fixed in 5.0.11, 3.0.8, 2.1.6, 1.1.20\n" +
		"    package: libssl3t64 3.0.13-0ubuntu3.15 · CVE-2026-84782 fixed in 3.0.13-0ubuntu3.16\n"
	if code != exitOK || errs != "" || !strings.Contains(out, want) || !strings.Contains(out, "\n    package: undici 6.28.0 · CVE-2026-19534 fixed in 6.28.1, 7.29.1, 8.10.2\n") {
		t.Errorf("exit %d, stderr %q: want %q in\n%s", code, errs, want, out)
	}
}
