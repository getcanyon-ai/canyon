package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// trivyResult is a stored trivy finding of a dependency, its problem carrying trivy's own lines as the
// adapter keeps them (contracts/finding.md, A dependency's name and version).
func trivyResult(cve, pkg, installed, fixed, fp string, blocks bool) string {
	problem := cve + " in " + pkg + ". Package: " + pkg + "\nInstalled Version: " + installed + "\nVulnerability " + cve + "\nSeverity: HIGH"
	if fixed != "" {
		problem += "\nFixed Version: " + fixed
	}
	problem += "\nLink: [" + cve + "](https://avd.aquasec.com/nvd/" + strings.ToLower(cve) + ")"
	return fmt.Sprintf(`{"ruleId": "deps.known-vuln.trivy.%s", "partialFingerprints": {"canyon/v1": %q}, "properties": {"canyon": {"blocks": %t, "problem": %q}}}`, cve, fp, blocks, problem)
}

// lodashRun is a blocked run of claims-portal whose verdict holds three blocking trivy findings of lodash
// 4.17.4, one of minimist, a trivy misconfiguration (no dependency), the AI review's finding and an
// advisory lodash CVE with no fixed version. The fingerprints
// are generated, never literals: a fingerprint literal is secret-shaped.
func lodashRun(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, base string, c *http.Client, fps []string) {
	t.Helper()
	pool, ctx, base, c = withProject(t)
	_, up := upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"a": "1"}), "", "")
	for n := range 7 {
		fps = append(fps, fmt.Sprintf("f_%064x", n+1))
	}
	verdict := `{"state": "blocked", "blocking": 4, "warnings": 3, "findings": {"runs": [{"tool": {"driver": {"name": "sarif/trivy"}}, "results": [` +
		trivyResult("CVE-2018-16487", "lodash", "4.17.4", "4.17.11", fps[0], true) + ", " + trivyResult("CVE-2019-10744", "lodash", "4.17.4", "4.17.12", fps[1], true) + ", " +
		trivyResult("CVE-2021-44906", "minimist", "0.0.8", "1.2.6, 0.2.4", fps[2], true) + ", " + trivyResult("CVE-2021-23337", "lodash", "4.17.4", "4.17.21", fps[3], true) + ", " +
		fmt.Sprintf(`{"ruleId": "iac.misconfig.trivy.DS026", "partialFingerprints": {"canyon/v1": %q}, "properties": {"canyon": {"blocks": false, "problem": "No HEALTHCHECK defined. Artifact: Dockerfile\nType: dockerfile"}}}`, fps[4]) + ", " +
		trivyResult("CVE-2025-13465", "lodash", "4.17.4", "", fps[6], false) +
		`]}, {"tool": {"driver": {"name": "canyon-review"}}, "results": [` +
		fmt.Sprintf(`{"ruleId": "auth.unauthenticated-route", "partialFingerprints": {"canyon/v1": %q}, "properties": {"canyon": {"blocks": false, "problem": "Package: lodash\nInstalled Version: 4.17.4"}}}`, fps[5]) +
		`]}]}}`
	if _, err := pool.Exec(ctx, `WITH r AS (INSERT INTO runs (id, project_id, package_id, state, ended_at, started_by_kind, started_by_subject, started_by_display) VALUES ('gr_lodash', 'claims-portal', $1, 'blocked', now(), 'person', 'anonymous', ''))
		INSERT INTO timeline_rows (scope, run_id, project_id, kind, dedupe, data) VALUES ('run', 'gr_lodash', 'claims-portal', 'verdict', 'verdict', $2)`, packageOf(up)["id"], verdict); err != nil {
		t.Fatal(err)
	}
	return pool, ctx, base, c, fps
}

// The run read names a finding's dependency only from trivy's own lines: lodash's and minimist's carry it,
// with its vulnerability, fixed versions and link, and fixed_in, the one version that fixes every finding
// of its tool, dependency and blocks (lodash's blocking three: 4.17.21; its advisory names no fix); trivy's
// misconfiguration has no such lines, and the AI review's finding is no trivy finding, whatever its problem
// says (contracts/routes.md, the run read).
func TestGateRunNamesADependencyOnlyFromTrivysLines(t *testing.T) {
	_, _, base, c, _ := lodashRun(t)
	if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_lodash", ""); code != 200 {
		t.Fatalf("the run read: %d %s", code, lastBody)
	}
	var read struct {
		Run struct {
			Findings []struct {
				RuleID     string          `json:"ruleId"`
				Dependency json.RawMessage `json:"dependency"`
			}
		}
	}
	if err := json.Unmarshal([]byte(lastBody), &read); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range read.Run.Findings {
		got = append(got, f.RuleID+" "+string(f.Dependency))
	}
	want := []string{
		`deps.known-vuln.trivy.CVE-2018-16487 {"name":"lodash","version":"4.17.4","vulnerability":"CVE-2018-16487","fixed":"4.17.11","link":"https://avd.aquasec.com/nvd/cve-2018-16487","fixed_in":"4.17.21"}`,
		`deps.known-vuln.trivy.CVE-2019-10744 {"name":"lodash","version":"4.17.4","vulnerability":"CVE-2019-10744","fixed":"4.17.12","link":"https://avd.aquasec.com/nvd/cve-2019-10744","fixed_in":"4.17.21"}`,
		`deps.known-vuln.trivy.CVE-2021-44906 {"name":"minimist","version":"0.0.8","vulnerability":"CVE-2021-44906","fixed":"1.2.6, 0.2.4","link":"https://avd.aquasec.com/nvd/cve-2021-44906","fixed_in":"0.2.4"}`,
		`deps.known-vuln.trivy.CVE-2021-23337 {"name":"lodash","version":"4.17.4","vulnerability":"CVE-2021-23337","fixed":"4.17.21","link":"https://avd.aquasec.com/nvd/cve-2021-23337","fixed_in":"4.17.21"}`,
		`iac.misconfig.trivy.DS026 `,
		`deps.known-vuln.trivy.CVE-2025-13465 {"name":"lodash","version":"4.17.4","vulnerability":"CVE-2025-13465","link":"https://avd.aquasec.com/nvd/cve-2025-13465"}`,
		`auth.unauthenticated-route `,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dependencies:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// One dependency's findings are approved together: one approval row per finding, as the single approve
// writes it, answered in the body's order, and one audit row naming the dependency and listing them.
func TestApproveFindingsApprovesOneDependencyTogether(t *testing.T) {
	pool, ctx, base, c, fps := lodashRun(t)
	count := func(q string) int { t.Helper(); return count(t, ctx, pool, q) }
	expires := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"fingerprints":[%q,%q,%q],"reason":"Pinned by the vendor SDK","expires_at":%q}`, fps[3], fps[0], fps[1], expires)
	if code, _, _ := do(t, c, http.MethodPost, base+"/api/v1/gate-runs/gr_lodash/findings/approve", body); code != 200 {
		t.Fatalf("approving lodash: %d %s", code, lastBody)
	}
	approval := func(cve, fp string) string {
		return `{"approved_by":{"kind":"person","subject":"anonymous","display":""},"expires_at":"` + expires + `","fingerprint":"` + fp +
			`","reason":"Pinned by the vendor SDK","rule":"deps.known-vuln.trivy.` + cve + `"}`
	}
	if want := `{"approvals":[` + approval("CVE-2021-23337", fps[3]) + "," + approval("CVE-2018-16487", fps[0]) + "," + approval("CVE-2019-10744", fps[1]) + `]}`; lastBody != want {
		t.Fatalf("the answer:\n got %s\nwant %s", lastBody, want)
	}
	if n := count("SELECT count(*) FROM approvals WHERE project_id = 'claims-portal' AND reason = 'Pinned by the vendor SDK' AND expires_at = '" + expires + "' AND approved_by_kind = 'person'"); n != 3 || count("SELECT count(*) FROM approvals") != 3 {
		t.Fatalf("%d approvals of lodash's three findings", n)
	}
	detail := `{"tool": "sarif/trivy", "reason": "Pinned by the vendor SDK", "findings": [{"rule": "deps.known-vuln.trivy.CVE-2021-23337", "fingerprint": "` + fps[3] +
		`"}, {"rule": "deps.known-vuln.trivy.CVE-2018-16487", "fingerprint": "` + fps[0] + `"}, {"rule": "deps.known-vuln.trivy.CVE-2019-10744", "fingerprint": "` + fps[1] +
		`"}], "dependency": {"name": "lodash", "version": "4.17.4"}, "expires_at": "` + expires + `", "author": {"kind": "person", "subject": "anonymous", "display": ""}}`
	if n := count("SELECT count(*) FROM audit_events WHERE action = 'POST /api/v1/gate-runs/{run}/findings/approve' AND run_id = 'gr_lodash' AND detail = '" + detail + "'::jsonb"); n != 1 || count("SELECT count(*) FROM audit_events WHERE action LIKE '%approve'") != 1 {
		t.Fatalf("%d audit rows naming lodash and its three findings", n)
	}
	do(t, c, http.MethodGet, base+"/api/v1/gate-runs/gr_lodash", "")
	if n := strings.Count(lastBody, `"reason":"Pinned by the vendor SDK"`); n != 3 {
		t.Fatalf("the run read carries %d approvals, want lodash's three", n)
	}
}

// A refused body approves nothing and writes no audit row; every fingerprint is found before the
// findings are compared, and only a person approves.
func TestApproveFindingsRefusals(t *testing.T) {
	pool, ctx, base, c, fps := lodashRun(t)
	count := func(q string) int { t.Helper(); return count(t, ctx, pool, q) }
	url := base + "/api/v1/gate-runs/gr_lodash/findings/approve"
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	with := func(fingerprints ...string) string {
		quoted, _ := json.Marshal(fingerprints)
		return `{"fingerprints":` + string(quoted) + `,"reason":"r","expires_at":"` + expires + `"}`
	}
	var many []string
	for n := range 201 {
		many = append(many, fmt.Sprintf("f_%064x", n+100))
	}
	unknown := fmt.Sprintf("f_%064x", 99)
	for i, tc := range []struct {
		url, body string
		code      int
		want      string
	}{
		{url, with(fps[0], fps[2]), 400, "not_one_dependency"}, // lodash and minimist
		{url, with(fps[0], fps[4]), 400, "not_one_dependency"}, // a misconfiguration names no dependency
		{url, with(fps[0], fps[5]), 400, "not_one_dependency"}, // the AI review's finding is no trivy finding
		{url, with(fps[4]), 400, "not_one_dependency"},
		{url, with(fps[0], unknown), 404, "not_found"},
		{url, with(fps[0], fps[2], unknown), 404, "not_found"}, // found first, compared after
		{base + "/api/v1/gate-runs/gr_nothing/findings/approve", with(fps[0]), 404, "not_found"},
		{url, with(), 400, "invalid_body"},
		{url, with(fps[0], fps[0]), 400, "invalid_body"},
		{url, with(fps[0], "f_short"), 400, "invalid_body"},
		{url, with(many...), 400, "invalid_body"},
		{url, `{"reason":"r","expires_at":"` + expires + `"}`, 400, "invalid_body"},
		{url, `{"fingerprints":["` + fps[0] + `"],"reason":"r","expires_at":"` + expires + `","rule":"x"}`, 400, "invalid_body"},
		{url, `{"fingerprints":["` + fps[0] + `"],"reason":"","expires_at":"` + expires + `"}`, 400, "invalid_reason"},
		{url, `{"fingerprints":["` + fps[0] + `"],"reason":"r","expires_at":"next week"}`, 400, "invalid_expires_at"},
	} {
		if code, body, _ := do(t, c, http.MethodPost, tc.url, tc.body); code != tc.code || body["error"] != tc.want {
			t.Fatalf("case %d: %d %s", i, code, lastBody)
		}
	}
	for _, kind := range []string{"agent", "canyon"} { // the agent a gate stopped cannot approve its own way past it
		if _, err := pool.Exec(ctx, "UPDATE sessions SET actor_kind = $1", kind); err != nil {
			t.Fatal(err)
		}
		if code, body, _ := do(t, c, http.MethodPost, url, with(fps[0], fps[1])); code != 403 || body["error"] != "person_required" {
			t.Fatalf("%s approving: %d %s", kind, code, lastBody)
		}
	}
	if n := count("SELECT count(*) FROM approvals") + count("SELECT count(*) FROM audit_events WHERE action LIKE '%approve'"); n != 0 {
		t.Fatalf("a refused approval wrote %d rows", n)
	}
}

// dependencyOf reads trivy's two lines as finding.md words it: the last "Package: " before the first
// "\nInstalled Version: ", no line break between them, and neither half empty; only trivy's findings.
func TestDependencyOfReadsOnlyTrivysTwoLines(t *testing.T) {
	for _, tc := range []struct{ tool, problem, want string }{
		{"sarif/trivy", "lodash: x. Package: lodash\nInstalled Version: 4.17.4\nFixed Version: 4.17.21", "lodash 4.17.4"},
		{"sarif/trivy", "Package: a says Package: lodash\nInstalled Version: 4.17.4", "lodash 4.17.4"}, // the last one
		{"sarif/trivy", "Package: lodash\nInstalled Version: 4.17.4", "lodash 4.17.4"},                 // the version is the rest of the line
		{"sarif/trivy", "Package: lodash\nSeverity: HIGH\nInstalled Version: 4.17.4", ""},              // a line break between them
		{"sarif/trivy", "Package: \nInstalled Version: 4.17.4", ""},
		{"sarif/trivy", "Package: lodash\nInstalled Version: \nFixed Version: 1", ""},
		{"sarif/trivy", "Package: lodash", ""},
		{"sarif/grype", "Package: lodash\nInstalled Version: 4.17.4", ""},
	} {
		dep, ok := dependencyOf(tc.tool, tc.problem)
		if got := strings.TrimSpace(dep.Name + " " + dep.Installed); ok != (tc.want != "") || ok && got != tc.want {
			t.Errorf("%s %q: %q %v, want %q", tc.tool, tc.problem, got, ok, tc.want)
		}
	}
}
