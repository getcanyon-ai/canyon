package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/finding"
	gatepolicy "github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs"
)

// platformGolden is contracts/testdata/platform/claims-portal.json: an image answer, its re-matches, and
// the image read and receipt row they derive (contracts/finding.md, System libraries and build tools).
type platformGolden struct {
	Env       string          `json:"env"`
	Digest    string          `json:"image_digest"`
	ScannedAt time.Time       `json:"scanned_at"`
	RuleName  string          `json:"rule_name"`
	RuleState string          `json:"rule_state"`
	ReadAt    time.Time       `json:"read_at"`
	ImageRead json.RawMessage `json:"image_read"`
	Row       json.RawMessage `json:"receipt_row"`
	Image     struct {
		SBOM struct {
			Components []json.RawMessage `json:"components"`
		} `json:"sbom"`
		DiffIDs   []string `json:"diff_ids"`
		Lifecycle string   `json:"lifecycle_metadata"`
	} `json:"image"`
	Matches []struct {
		DiffID string `json:"diff_id"`
		Answer struct {
			DBBuiltAt time.Time       `json:"db_built_at"`
			Report    json.RawMessage `json:"report"`
			Unscanned []string        `json:"unscanned"`
		} `json:"answer"`
	} `json:"matches"`
}

func loadPlatformGolden(t *testing.T) platformGolden {
	t.Helper()
	raw, err := os.ReadFile("../../contracts/testdata/platform/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var g platformGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// layered is the golden image's packages stored as Canyon stores them: each under the layer trivy's
// LayerDiffID names, as trivy wrote it.
func (g platformGolden) layered(t *testing.T) (map[string][]json.RawMessage, []json.RawMessage) {
	t.Helper()
	layers, unplaced := map[string][]json.RawMessage{}, []json.RawMessage{}
	for _, raw := range g.Image.SBOM.Components {
		var c trivy.Component
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		switch d := c.Prop(trivy.LayerDiffID); {
		case c.Type == "operating-system":
		case slices.Contains(g.Image.DiffIDs, d):
			layers[d] = append(layers[d], raw)
		default:
			unplaced = append(unplaced, raw)
		}
	}
	return layers, unplaced
}

// facts is what the store would hold of the golden image: every match, of which the reader keeps each
// layer's newest.
func (g platformGolden) facts(t *testing.T) imageFacts {
	t.Helper()
	layers, unplaced := g.layered(t)
	f := imageFacts{scanned: &g.ScannedAt, diffIDs: g.Image.DiffIDs, label: g.Image.Lifecycle, layers: map[string][]trivy.Component{}, newest: map[string]layerMatch{}}
	for _, d := range g.Image.DiffIDs { // a layer with no package is stored with none
		f.layers[d] = []trivy.Component{}
	}
	decode := func(raw []json.RawMessage) []trivy.Component {
		var out []trivy.Component
		if err := json.Unmarshal(mustJSON(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for d, cs := range layers {
		f.layers[d] = decode(cs)
	}
	f.unplaced = decode(unplaced)
	for _, m := range g.Matches {
		if have, ok := f.newest[m.DiffID]; ok && !m.Answer.DBBuiltAt.After(have.dbBuiltAt) {
			continue
		}
		var report trivy.Report
		if err := json.Unmarshal(m.Answer.Report, &report); err != nil {
			t.Fatal(err)
		}
		f.newest[m.DiffID] = layerMatch{dbBuiltAt: m.Answer.DBBuiltAt, report: report, unscanned: m.Answer.Unscanned}
	}
	return f
}

func (g platformGolden) rule() gatepolicy.Rule {
	return gatepolicy.Rule{ID: platformRule, Name: g.RuleName, State: gatepolicy.State(g.RuleState), Producer: gatepolicy.PromoteGate}
}

func (g platformGolden) read(t *testing.T, f imageFacts, at time.Time) imageRead {
	t.Helper()
	return readImage(f, g.Env, g.Digest, gatepolicy.State(g.RuleState), g.RuleName, at)
}

func sameJSON(t *testing.T, label string, got any, want json.RawMessage) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(mustJSON(got), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("%s:\n got %s\nwant %s", label, mustJSON(got), want)
	}
}

// The golden's image read and receipt row are what the server derives from its image answer and
// re-matches: the classes, the counted findings and their fingerprints, the notes — a purl-less package
// and an unscanned one among them, never clean — each layer's newest match, and the row's count. Its
// findings are a profile log the seam's schemas take, IMAGE and all.
func TestPlatformKnownVulnReadsTheGolden(t *testing.T) {
	g := loadPlatformGolden(t)
	read := g.read(t, g.facts(t), g.ReadAt)
	sameJSON(t, "the image read", read, g.ImageRead)
	sameJSON(t, "the receipt row", platformRow(g.rule(), read), g.Row)
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints,
		Tool: finding.Tool{Driver: finding.Driver{Name: "canyon/promote-gate", Version: "1", Rules: []finding.Rule{{ID: platformRule, Name: g.RuleName}}}}}}}
	for _, f := range read.Findings {
		log.Runs[0].Results = append(log.Runs[0].Results, f.Result)
	}
	if err := contracts.CheckFindingSchemas(mustJSON(log)); err != nil {
		t.Fatalf("the image read's findings as a log: %v", err)
	}
	for i := range log.Runs[0].Results {
		log.Runs[0].Results[i].RuleID = "deps.known-vuln"
	}
	if contracts.CheckFindingSchemas(mustJSON(log)) == nil {
		t.Fatal("an IMAGE finding under deps.known-vuln passed the schema")
	}
}

// The round trip keeps the layer: a vulnerability is joined to the package its BOMRef names in the layer
// its match is stored under, and its dependency names that layer, the package's own LayerDiffID. One
// whose Layer.DiffID is another layer, or whose BOMRef names a package the layer does not hold, makes the
// match unreadable: could not check, never clean.
func TestPlatformKnownVulnRoundTripKeepsLayerDiffID(t *testing.T) {
	g := loadPlatformGolden(t)
	f := g.facts(t)
	for _, found := range g.read(t, f, g.ReadAt).Findings {
		dep := found.Dependency
		i := slices.IndexFunc(f.layers[dep.Layer], func(c trivy.Component) bool { return c.FullName() == dep.Name })
		if i < 0 || f.layers[dep.Layer][i].Prop(trivy.LayerDiffID) != dep.Layer {
			t.Fatalf("%s is read in layer %s, which holds no such package", dep.Name, dep.Layer)
		}
	}
	runtime := g.Image.DiffIDs[2]
	for name, mutate := range map[string]func(v *trivy.Vuln){
		"another layer":         func(v *trivy.Vuln) { v.Layer.DiffID = g.Image.DiffIDs[0] },
		"a package not in it":   func(v *trivy.Vuln) { v.PkgIdentifier.BOMRef = "pkg:npm/express@4.19.2" },
		"a package of no layer": func(v *trivy.Vuln) { v.PkgIdentifier.BOMRef = "pkg:npm/left-pad@1.3.0" },
		"no layer named at all": func(v *trivy.Vuln) { v.Layer.DiffID = "" },
	} {
		f := g.facts(t)
		m := f.newest[runtime]
		m.report.Results = slices.Clone(m.report.Results)
		m.report.Results[0].Vulnerabilities = slices.Clone(m.report.Results[0].Vulnerabilities)
		mutate(&m.report.Results[0].Vulnerabilities[0])
		f.newest[runtime] = m
		read := g.read(t, f, g.ReadAt)
		row := platformRow(g.rule(), read)
		if read.State != "could_not_check" || row.Passed || row.Why != "could not check: a match names a package the image does not hold" || len(read.Findings)+len(read.Notes) != 0 {
			t.Fatalf("a vulnerability in %s: %s %q, %d findings, %d notes", name, read.State, row.Why, len(read.Findings), len(read.Notes))
		}
	}
}

// A match on a database built more than 5 days before the read is not read: could not check, and the
// row blocks wherever the rule blocks. A days-old scan never passes a version.
func TestPlatformKnownVulnStaleMatchCouldNotCheckAndBlocks(t *testing.T) {
	g := loadPlatformGolden(t)
	f := g.facts(t)
	for d, m := range f.newest { // every finding gone: only the database's age can fail it
		m.report = trivy.Report{}
		f.newest[d] = m
	}
	if row := platformRow(g.rule(), g.read(t, f, g.ReadAt)); !row.Passed {
		t.Fatalf("matches 1 and 2 days old, nothing found: %+v", row)
	}
	sixDays := time.Date(2026, 10, 19, 9, 0, 1, 0, time.UTC) // the oldest newest match was built 2026-10-13T09:00:00Z
	read := g.read(t, f, sixDays)
	row := platformRow(g.rule(), read)
	if read.State != "could_not_check" || row.Passed || row.Why != "could not check: its vulnerability database is over 5 days old" || row.DBBuiltAt != nil {
		t.Fatalf("a 6-day-old match: %s %+v", read.State, row)
	}
	in := receiptInput{org: "Meridian", rules: []gatepolicy.Rule{g.rule()}, preview: &pkgRef{id: "pkg_9e40", label: "v1.4"}, platform: &read}
	if got := evaluate(in); !slices.Equal(got.BlockedBy, []string{platformRule}) {
		t.Fatalf("blocking, could not check: blocked by %v", got.BlockedBy)
	}
	in.rules[0].State = gatepolicy.Advisory
	if got := evaluate(in); len(got.BlockedBy) != 0 || len(got.Rules) != 1 || got.Rules[0].Passed {
		t.Fatalf("advisory, could not check: an amber row that blocks nothing: %+v", got)
	}
}

// The row reads each layer's newest match, by its database's build: a rescan on a newer database moves
// the receipt with no verdict written.
func TestPlatformKnownVulnNewerMatchMovesTheReceipt(t *testing.T) {
	g := loadPlatformGolden(t)
	f := g.facts(t)
	before := platformRow(g.rule(), g.read(t, f, g.ReadAt))
	ssl := g.Image.DiffIDs[1] // libssl3's layer: the newer of its two matches names CVE-2024-6119, the older CVE-2024-5535
	m := f.newest[ssl]
	m.dbBuiltAt, m.report = m.dbBuiltAt.Add(12*time.Hour), trivy.Report{} // the next database: Ubuntu's fix is in the run image now
	f.newest[ssl] = m
	after := platformRow(g.rule(), g.read(t, f, g.ReadAt))
	if before.Why != "3 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 4 notes: "+platformNotes || after.Why != "2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 4 notes: "+platformNotes {
		t.Fatalf("a newer match moves the row: %q, then %q", before.Why, after.Why)
	}
}

// Only a finding Canyon can fix counts: with no fix published it is a note, as is one of an unknown
// layer, and neither blocks.
func TestPlatformKnownVulnUnfixableFindingIsANote(t *testing.T) {
	g := loadPlatformGolden(t)
	f := g.facts(t)
	for d, m := range f.newest {
		m.report.Results = slices.Clone(m.report.Results)
		for i := range m.report.Results {
			m.report.Results[i].Vulnerabilities = slices.Clone(m.report.Results[i].Vulnerabilities)
			for j := range m.report.Results[i].Vulnerabilities {
				m.report.Results[i].Vulnerabilities[j].FixedVersion = ""
			}
		}
		f.newest[d] = m
	}
	read := g.read(t, f, g.ReadAt)
	row := platformRow(g.rule(), read)
	var unfixed []string
	for _, n := range read.Notes {
		if n.Why == "no_fix" {
			unfixed = append(unfixed, n.Name)
		}
	}
	slices.Sort(unfixed)
	if len(read.Findings) != 0 || !row.Passed || !slices.Equal(unfixed, []string{"cross-spawn", "libc6", "libssl3", "stdlib"}) || *row.Notes != 7 {
		t.Fatalf("no fix published anywhere: %d findings, notes %v, row %+v", len(read.Findings), unfixed, row)
	}
}

// Before an image scan the row reads no image scan recorded; a scan that did not finish, could not check
// with its reason; a layer never matched, could not check. None passes.
func TestPlatformKnownVulnWithoutAMatchNeverPasses(t *testing.T) {
	g := loadPlatformGolden(t)
	unmatched := g.facts(t)
	delete(unmatched.newest, g.Image.DiffIDs[6])
	for name, c := range map[string]struct {
		f   imageFacts
		why string
	}{
		"no image scan":          {imageFacts{}, "no image scan recorded"},
		"no Java database":       {imageFacts{scanned: &g.ScannedAt, notFinished: "db_stale", reason: "java"}, "could not check: its Java database is over 5 days old"},
		"an image over cap":      {imageFacts{scanned: &g.ScannedAt, notFinished: "over_cap", reason: "image"}, "could not check: the image is larger than this Canyon scans"},
		"an answer unheard of":   {imageFacts{scanned: &g.ScannedAt, notFinished: "eaten"}, "could not check: its image scan did not finish"},
		"the scanner busy":       {imageFacts{scanned: &g.ScannedAt, notFinished: "busy"}, "could not check: Canyon's scanner was busy with other apps, and checks again on its next round"},
		"the launcher unmatched": {unmatched, "could not check: the image has not been matched yet"},
	} {
		row := platformRow(g.rule(), g.read(t, c.f, g.ReadAt))
		if row.Passed || row.Why != c.why || !row.Blocks {
			t.Errorf("%s: %+v, want %q, not passed", name, row, c.why)
		}
	}
}

// While the rule is off it is not on the receipt; on, it follows the four in the snapshot's order.
func TestPlatformKnownVulnIsOnTheReceiptWhileOn(t *testing.T) {
	snap := testPolicy()
	in := receiptInput{org: "Meridian", rules: promoteRules(snap), preview: &pkgRef{id: "pkg_9e40", label: "v1.4"}}
	ids := func(r receipt) (out []string) {
		for _, rule := range r.Rules {
			out = append(out, rule.ID)
		}
		return out
	}
	if got := ids(evaluate(in)); slices.Contains(got, platformRule) || len(got) != 4 {
		t.Fatalf("off: %v", got)
	}
	on, err := gatepolicy.WithStates(snap, map[string]gatepolicy.State{platformRule: gatepolicy.Advisory})
	if err != nil {
		t.Fatal(err)
	}
	in.rules = promoteRules(on)
	if got := ids(evaluate(in)); !slices.Equal(got, []string{"prod.clean-gate", "prod.human-approval", "stack.blessed", "owner.required", platformRule}) {
		t.Fatalf("on: %v", got)
	}
}

// seedPlatform stores the golden image under project and digest as Canyon's store holds it, its matches as old as the
// golden's, counted back from now, so that each layer's newest is current.
func seedPlatform(t *testing.T, ctx context.Context, pool *pgxpool.Pool, g platformGolden, project, digest string) {
	t.Helper()
	layers, unplaced := g.layered(t)
	if _, err := pool.Exec(ctx, "INSERT INTO scanned_images (project_id, digest, scanned_at, diff_ids, lifecycle, unplaced) VALUES ($1, $2, now(), $3, $4, $5)",
		project, digest, g.Image.DiffIDs, g.Image.Lifecycle, mustJSON(unplaced)); err != nil {
		t.Fatal(err)
	}
	part := func(d string) string { // the slice each layer is, by its content
		return fmt.Sprintf("sha256:%x", sha256.Sum256(mustJSON(append([]json.RawMessage{}, layers[d]...))))
	}
	for _, d := range g.Image.DiffIDs { // every layer stored, one holding no package with none
		if _, err := pool.Exec(ctx, "INSERT INTO layer_parts (part_sha, components) VALUES ($1, $2) ON CONFLICT DO NOTHING", part(d), mustJSON(append([]json.RawMessage{}, layers[d]...))); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "INSERT INTO image_layers (project_id, digest, diff_id, part_sha) VALUES ($1, $2, $3, $4)", project, digest, d, part(d)); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range g.Matches {
		age := g.ReadAt.Sub(m.Answer.DBBuiltAt) // as old as in the golden, relative to now
		if _, err := pool.Exec(ctx, "INSERT INTO part_matches (part_sha, db_built_at, matched_at, report, unscanned) VALUES ($1, now() - $2::interval, now(), $3, $4)",
			part(m.DiffID), age.String(), m.Answer.Report, append([]string{}, m.Answer.Unscanned...)); err != nil {
			t.Fatal(err)
		}
	}
}

// GET …/envs/:env/image answers a contributor what the image preview runs holds of Canyon's, read from the
// store alone, whatever the rule's state; a viewer is refused team_required. Nothing deployed is 404.
func TestPlatformKnownVulnImageReadIsAContributors(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	vic := signedIn(t, ctx, pool, base, "sub-vic", "vic@meridian.example", false)
	path := base + "/api/v1/projects/claims-portal/envs/preview/image"
	if code, _, _ := do(t, carl, http.MethodGet, path, ""); code != 404 {
		t.Fatalf("nothing deployed: %d %s", code, lastBody)
	}
	g := loadPlatformGolden(t)
	seedPlatform(t, ctx, pool, g, "claims-portal", g.Digest)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT 'claims-portal', 'preview', package_id, 'canyon/claims-portal@' || $1, 'http://claims-portal.preview.localhost:7777', id, 'canyon' FROM runs WHERE id = 'gr_seed'`, g.Digest); err != nil {
		t.Fatal(err)
	}
	code, body, _ := do(t, carl, http.MethodGet, path, "")
	if code != 200 {
		t.Fatalf("a contributor: %d %s", code, lastBody)
	}
	var want map[string]any
	if err := json.Unmarshal(g.ImageRead, &want); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"state", "why", "layers", "notes"} {
		if !bytes.Equal(mustJSON(body[k]), mustJSON(want[k])) {
			t.Errorf("%s: %s, the golden's %s", k, mustJSON(body[k]), mustJSON(want[k]))
		}
	}
	if fs, _ := body["findings"].([]any); len(fs) != 4 || !strings.Contains(string(mustJSON(fs)), `"blocks":false`) {
		t.Errorf("the findings, the rule off so none blocks: %s", mustJSON(body["findings"]))
	}
	if code, _, _ := do(t, vic, http.MethodGet, path, ""); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Fatalf("a viewer: %d %s, want team_required", code, lastBody)
	}
	if code, _, _ := do(t, carl, http.MethodGet, base+"/api/v1/projects/claims-portal/envs/staging/image", ""); code != 400 {
		t.Fatalf("an env that is none: %d %s", code, lastBody)
	}
}

// A project reads its own image scan alone (contracts/finding.md, System libraries and build tools: read as the project's
// own): another project's checked scan of the very digest claims-portal's preview runs never answers
// claims-portal's image read, which reads no image scan recorded; a signed-in member of no team of it is
// refused team_required.
func TestPlatformImageReadIsTheProjectsOwn(t *testing.T) {
	withOIDC(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	olive := signedIn(t, ctx, pool, base, "sub-olive", "olive@meridian.example", false)
	seedTeamApp(t, ctx, pool, base, olive)
	carl := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false)
	nina := signedIn(t, ctx, pool, base, "sub-nina", "nina@meridian.example", false)
	g := loadPlatformGolden(t)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-twin')"); err != nil {
		t.Fatal(err)
	}
	seedPlatform(t, ctx, pool, g, "claims-twin", g.Digest)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by)
		SELECT 'claims-portal', 'preview', package_id, 'canyon/claims-portal@' || $1, 'http://claims-portal.preview.localhost:7777', id, 'canyon' FROM runs WHERE id = 'gr_seed'`, g.Digest); err != nil {
		t.Fatal(err)
	}
	path := base + "/api/v1/projects/claims-portal/envs/preview/image"
	if code, body, _ := do(t, carl, http.MethodGet, path, ""); code != 200 || body["state"] != "not_scanned" || body["why"] != "no image scan recorded" {
		t.Fatalf("another project's scan of the digest read as claims-portal's: %d %s", code, lastBody)
	}
	if code, _, _ := do(t, nina, http.MethodGet, path, ""); code != 403 || lastBody != teamRequiredBody {
		t.Fatalf("a member of no team of it: %d %s, want team_required", code, lastBody)
	}
}

// The promote receipt reads the image preview runs under its own project alone: a checked scan of the same
// digest under another project leaves its row at no image scan recorded, blocking.
func TestPlatformKnownVulnRowReadsTheProjectsOwnScan(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	g := loadPlatformGolden(t)
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-twin')"); err != nil {
		t.Fatal(err)
	}
	seedPlatform(t, ctx, pool, g, "claims-twin", executortest.Digest)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+platformRule+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
	rules, _ := status["receipt"].(map[string]any)["rules"].([]any)
	if len(rules) == 0 {
		t.Fatalf("no receipt: %s", lastBody)
	}
	if row, _ := rules[len(rules)-1].(map[string]any); row["id"] != platformRule || row["passed"] != false || row["blocks"] != true || row["why"] != "no image scan recorded" {
		t.Fatalf("the receipt row read another project's scan: %s", mustJSON(row))
	}
}

// While the rule is on, the receipt preview's version is promoted on carries its row, read from each layer's
// newest match of the image preview runs; while it is off, the receipt has no such row.
func TestPlatformKnownVulnRowOnTheStatusReceipt(t *testing.T) {
	pool, ctx, base, c, _ := previewed(t)
	g := loadPlatformGolden(t)
	seedPlatform(t, ctx, pool, g, "claims-portal", executortest.Digest)
	last := func() map[string]any {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		rules, _ := status["receipt"].(map[string]any)["rules"].([]any)
		if len(rules) == 0 {
			t.Fatalf("the status read's receipt evaluates no rule: %s", lastBody)
		}
		row, _ := rules[len(rules)-1].(map[string]any)
		return row
	}
	if row := last(); row["id"] == platformRule {
		t.Fatalf("off, yet on the receipt: %s", mustJSON(row))
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+platformRule+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	row := last()
	if row["id"] != platformRule || row["passed"] != false || row["blocks"] != true || row["why"] != "3 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds; 4 notes: "+platformNotes || row["notes"] != float64(4) {
		t.Fatalf("the status read's receipt row: %s", mustJSON(row))
	}
}

// Every way the store can fail to say what the image holds reads could not check and blocks: never
// "nothing found" read as clean. A layer the image has whose packages were never stored; a stored scan,
// label or match that does not read, or a report not trivy's shape; an image with no lifecycle label, or one
// naming no layer the run image's, so no layer can be owned; a package stored under a layer trivy did not
// name; a database built after the read; an image scan that listed no package Canyon put there, so no
// complete report says it is clean; and an image named by anything but its digest, which no scan names.
func TestPlatformKnownVulnFailsClosed(t *testing.T) {
	g := loadPlatformGolden(t)
	unstored := g.facts(t)
	delete(unstored.layers, g.Image.DiffIDs[7]) // the config layer: it holds no package, and its row says so
	unreadable := g.facts(t)
	unreadable.unreadable = true
	garbled := g.facts(t)
	garbled.label = `{"runImage":`
	nothing := g.facts(t)
	for d := range nothing.layers {
		nothing.layers[d] = []trivy.Component{}
	}
	empty := imageFacts{scanned: &g.ScannedAt, diffIDs: []string{}, label: g.Image.Lifecycle, layers: map[string][]trivy.Component{}, newest: map[string]layerMatch{}}
	unlabelled := g.facts(t)
	unlabelled.label = ""
	relabel := func(edit func(meta map[string]any)) imageFacts {
		f := g.facts(t)
		var meta map[string]any
		if err := json.Unmarshal([]byte(f.label), &meta); err != nil {
			t.Fatal(err)
		}
		edit(meta)
		f.label = string(mustJSON(meta))
		return f
	}
	elsewhere := relabel(func(meta map[string]any) {
		meta["runImage"] = map[string]any{"topLayer": "sha256:" + strings.Repeat("9", 64)}
	})
	ownerless := relabel(func(meta map[string]any) { // the run image's one layer named the app's too: no layer is the run image's
		meta["runImage"] = map[string]any{"topLayer": g.Image.DiffIDs[0]}
		meta["app"] = []any{map[string]any{"sha": g.Image.DiffIDs[0]}, map[string]any{"sha": g.Image.DiffIDs[5]}}
	})
	moved := g.facts(t) // libssl3, stored under the node-engine layer though trivy names the run image's
	moved.layers[g.Image.DiffIDs[2]] = append(slices.Clone(moved.layers[g.Image.DiffIDs[2]]), moved.layers[g.Image.DiffIDs[1]]...)
	moved.layers[g.Image.DiffIDs[1]] = []trivy.Component{}
	junk := g.facts(t) // a layer holding a component that names no layer: decoded, it is no package of any layer
	junk.layers[g.Image.DiffIDs[7]] = []trivy.Component{{BOMRef: "x", Type: "application", Name: "x"}}
	twice := g.facts(t)
	twice.diffIDs = append(slices.Clone(twice.diffIDs), twice.diffIDs[7])
	blank := g.facts(t)
	blank.diffIDs = append(slices.Clone(blank.diffIDs), "")
	blank.layers[""] = []trivy.Component{}
	future := g.facts(t)
	ahead := future.newest[g.Image.DiffIDs[6]]
	ahead.dbBuiltAt = g.ReadAt.Add(time.Hour)
	future.newest[g.Image.DiffIDs[6]] = ahead
	const notRead = "could not check: a stored scan of the image does not read"
	const noPackage = "could not check: the image scan listed no package Canyon put there"
	for name, c := range map[string]struct {
		f   imageFacts
		why string
	}{
		"a layer whose packages were never stored":       {unstored, "could not check: Canyon kept only part of the image"},
		"a stored scan or match that does not read":      {unreadable, notRead},
		"a lifecycle label that does not parse":          {garbled, notRead},
		"an image scan listing no package":               {nothing, noPackage},
		"an image scan with no layer":                    {empty, notRead},
		"an image with no lifecycle label":               {unlabelled, notRead},
		"a run image whose top layer is not the image's": {elsewhere, notRead},
		"no layer the run image's":                       {ownerless, notRead},
		"a package stored under another layer":           {moved, notRead},
		"a database built after the read":                {future, notRead},
		"a component naming no layer":                    {junk, notRead},
		"a layer named twice":                            {twice, notRead},
		"a layer that is no diff ID":                     {blank, notRead},
	} {
		read := g.read(t, c.f, g.ReadAt)
		row := platformRow(g.rule(), read)
		if read.State != "could_not_check" || row.Passed || row.Why != c.why || !row.Blocks || len(read.Findings)+len(read.Notes) != 0 {
			t.Errorf("%s: %s %+v, want %q, blocking", name, read.State, row, c.why)
		}
	}
	vuln := `{"VulnerabilityID":"CVE-2024-6119","PkgIdentifier":{"BOMRef":"pkg:deb/ubuntu/libssl3@3.0.13"},"Layer":{"DiffID":"` + g.Image.DiffIDs[1] + `"}}`
	for raw, ok := range map[string]bool{`{}`: false, `null`: false, `{"SchemaVersion":2}`: false, `{"SchemaVersion":2,"Results":null}`: false,
		`{"SchemaVersion":1,"Results":[]}`: false, `[]`: false, `{"SchemaVersion":2,"Results":[]}`: true,
		`{"SchemaVersion":2,"Results":[null]}`: false, `{"SchemaVersion":2,"Results":[{"Vulnerabilities":[null]}]}`: false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[{}]}]}`:                                                                                          false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[` + strings.Replace(vuln, `"VulnerabilityID":"CVE-2024-6119",`, ``, 1) + `]}]}`:                  false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[` + strings.Replace(vuln, `"BOMRef":"pkg:deb/ubuntu/libssl3@3.0.13"`, `"BOMRef":""`, 1) + `]}]}`: false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[` + strings.Replace(vuln, g.Image.DiffIDs[1], ``, 1) + `]}]}`:                                    false,
		`{"SchemaVersion":2,"Results":[{"Vulnerabilities":[` + vuln + `]}]}`:                                                                                true} {
		if _, got := trivy.DecodeReport([]byte(raw)); got != ok {
			t.Errorf("a stored report %s reads: %t", raw, got)
		}
	}
	for raw, ok := range map[string]bool{`null`: false, `{}`: false, `[]`: true, `[{"bom-ref":"x","type":"library","name":"x"}]`: true,
		`[null]`: false, `[{}]`: false, `[{"type":"library","name":"x"}]`: false, `[{"bom-ref":"x","name":"x"}]`: false, `[{"bom-ref":"x","type":"library"}]`: false} {
		if _, got := trivy.DecodeComponents([]byte(raw)); got != ok {
			t.Errorf("stored components %s read: %t", raw, got)
		}
	}
	for _, image := range []string{"canyon/claims-portal:latest", "canyon/claims-portal@sha256:4f1c", ""} {
		if d := runs.ImageDigest(image); d != "" {
			t.Errorf("%q names digest %q", image, d)
		}
	}
}

// The image read's findings are profile findings the shared validator takes, against a snapshot that has the
// rule blocking: a region that spans the snippet, a declared rule, unique fingerprints, blocks derived from
// state and severity, the snapshot's policy name, and the governance prose a blocking finding needs.
func TestPlatformKnownVulnFindingsPassTheProfile(t *testing.T) {
	g := loadPlatformGolden(t)
	read := g.read(t, g.facts(t), g.ReadAt)
	log := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{{ColumnKind: finding.ColumnKindCodePoints,
		Tool: finding.Tool{Driver: finding.Driver{Name: "canyon/promote-gate", Version: "1", Rules: []finding.Rule{{ID: platformRule, Name: g.RuleName}}}}}}}
	for _, f := range read.Findings {
		log.Runs[0].Results = append(log.Runs[0].Results, f.Result)
	}
	snap, err := gatepolicy.WithStates(testPolicy(), map[string]gatepolicy.State{platformRule: gatepolicy.State(g.RuleState)})
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Runs[0].Results) == 0 {
		t.Fatal("the golden read has no finding to validate")
	}
	if err := log.Validate(); err != nil {
		t.Fatalf("the image read's findings: %v", err)
	}
	if err := log.ValidateAgainst(snap); err != nil {
		t.Fatalf("the image read's findings against the snapshot: %v", err)
	}
}

// At the boundary of GET …/envs/:env/image: the owner and an admin read production's image; a security
// responder with no grant on the app, who reads only what approving a finding needs, is team_required.
func TestPlatformKnownVulnImageReadRefusesAResponderWithNoGrant(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	g := loadPlatformGolden(t)
	seedPlatform(t, ctx, pool, g, "claims-portal", executortest.Digest)
	path := base + "/api/v1/projects/claims-portal/envs/prod/image"
	for who, c := range map[string]*http.Client{"the owner": olive, "an admin": signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)} {
		if code, body, _ := do(t, c, http.MethodGet, path, ""); code != 200 || body["image_digest"] != executortest.Digest || body["state"] != "checked" {
			t.Errorf("%s: %d %s", who, code, lastBody)
		}
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if code, _, _ := do(t, rhea, http.MethodGet, path, ""); code != 403 || lastBody != teamRequiredBody {
		t.Fatalf("a responder with no grant: %d %s, want team_required", code, lastBody)
	}
}

// A deploy under the rule reads its image, and its scan matches the layers it covers at once: the image
// read answers from what they stored exactly what the golden derives from the same answers, checked, a
// scan's length after the deploy (design/PATCHING.md §3.1, §3.2; #2075). A scan that did not finish is
// asked again by the next deploy of the same image; another project's scan of the same digest is never
// this project's; and a rescan that did not match every layer reads could not check until one does.
func TestPlatformImageReadsCheckedAfterADeployAndARescan(t *testing.T) {
	cleanGate(t)
	appAnswersOK(t)
	pool, ctx, base, c := withProject(t)
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/policies/"+platformRule+"?apply=true", `{"state":"advisory"}`); code != 200 {
		t.Fatalf("the rule on: %d %s", code, lastBody)
	}
	g := loadPlatformGolden(t)
	raw, err := os.ReadFile("../../contracts/testdata/platform/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var answers struct {
		Image   executor.ScanResult `json:"image"`
		Matches []struct {
			DiffID string              `json:"diff_id"`
			Answer executor.ScanResult `json:"answer"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(raw, &answers); err != nil {
		t.Fatal(err)
	}
	var failing atomic.Pointer[string]                 // a layer whose re-match does not finish
	rematch := func(part []byte) executor.ScanResult { // each layer's newest golden match, as old against now as against the golden's read
		var doc struct{ Components []trivy.Component }
		_ = json.Unmarshal(part, &doc)
		layer := ""
		for _, c := range doc.Components {
			layer = cmp.Or(layer, c.Prop(trivy.LayerDiffID))
		}
		if f := failing.Load(); f != nil && *f == layer {
			return executor.ScanResult{Tool: "trivy", NotFinished: "timeout"}
		}
		res := executor.ScanResult{Tool: "trivy", DBBuiltAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), Report: json.RawMessage(`{"SchemaVersion":2,"Results":[]}`)}
		var newest time.Time
		for _, m := range answers.Matches {
			if at, _ := time.Parse(time.RFC3339, m.Answer.DBBuiltAt); m.DiffID == layer && at.After(newest) {
				newest, res = at, m.Answer
				res.DBBuiltAt = time.Now().Add(-g.ReadAt.Sub(at)).UTC().Format(time.RFC3339)
			}
		}
		return res
	}
	fake.Rematch.Store(&rematch)
	fake.ImageAnswer.Store(&executor.ScanResult{Tool: "trivy", NotFinished: "timeout"})
	fresh := make([]byte, 32)
	_, _ = rand.Read(fresh)
	fake.Image = "canyon/claims-portal@sha256:" + hex.EncodeToString(fresh) // an image no other test's deploy read
	if _, err := pool.Exec(ctx, "INSERT INTO projects (id) VALUES ('claims-twin')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO scanned_images (project_id, digest, scanned_at, not_finished) VALUES ('claims-twin', $1, now(), 'timeout')", runs.ImageDigest(fake.Image)); err != nil {
		t.Fatal(err)
	}
	path := base + "/api/v1/projects/claims-portal/envs/preview/image"
	read := func(until func(body map[string]any) bool) map[string]any {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) { // image.sbom runs after the deploy, off its path
			_, body, _ := do(t, c, http.MethodGet, path, "")
			if until(body) || time.Now().After(deadline) {
				return body
			}
		}
	}
	deploy := func() {
		t.Helper()
		_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/claims-portal/deploys", "")
		if run, _ := runOf(started)["id"].(string); waitVerdict(t, c, base, run)["state"] != "clean" {
			t.Fatalf("the deploy: %s", lastBody)
		}
	}
	upload(t, c, base, "claims-portal", tarOf(t, map[string]string{"src/app.ts": "export {}\n"}), "", "")
	deploy()
	if body := read(func(b map[string]any) bool { return b["state"] != "not_scanned" }); body["why"] != "could not check: did not finish in time" {
		t.Fatalf("its own scan, which did not finish: %s", lastBody)
	}
	fake.ImageAnswer.Store(&answers.Image)
	deploy() // the same image, reused: asked again, since its scan did not finish
	var want map[string]any
	if err := json.Unmarshal(g.ImageRead, &want); err != nil {
		t.Fatal(err)
	}
	checked := func(why string) {
		t.Helper()
		body := read(func(b map[string]any) bool { return b["why"] == why })
		for _, k := range []string{"state", "why", "layers", "notes"} {
			if !bytes.Equal(mustJSON(body[k]), mustJSON(want[k])) {
				t.Errorf("%s: %s, the golden's %s", k, mustJSON(body[k]), mustJSON(want[k]))
			}
		}
	}
	checked(fmt.Sprint(want["why"]))
	if n := fake.ImageScans.Load(); n != 2 {
		t.Errorf("the image read %d times, want twice", n)
	}
	rescan := func() {
		t.Helper()
		h, err := dbos.Enqueue[any](durable, runs.RescanQueue, runs.RescanWorkflow, dbos.ScheduledWorkflowInput{ScheduledTime: time.Now()}) // a tick, now
		if err == nil {
			_, err = h.GetResult()
		}
		if err != nil {
			t.Fatalf("the rescan: %v", err)
		}
	}
	failing.Store(&answers.Matches[0].DiffID)
	rescan()
	if _, body, _ := do(t, c, http.MethodGet, path, ""); body["state"] != "could_not_check" || body["why"] != "could not check: its last rescan did not finish" {
		t.Fatalf("a rescan that did not match every layer: %s", lastBody)
	}
	failing.Store(nil)
	rescan()
	checked(fmt.Sprint(want["why"]))
}

// The receipt row names each package it counts, once, with the advisories at high or above against it and
// what fixes each: an advisory found in two layers is one, a package at medium alone is not counted
// (contracts/routes.md, The promote receipt; #2300 item 4, the test cluster's v0.1.13 image trimmed). Each says where it
// is, the layers holding it bottom first by owner and class, each once, and whether it is Go's standard library
// (#2579: the test cluster's patch approval said stdlib three times, and nowhere what it was or where).
func TestPlatformKnownVulnRowNamesTheCountedPackages(t *testing.T) {
	const launcher, certs, node = "sha256:2d01", "sha256:3219", "sha256:5c89"
	at := func(sev finding.Severity, name, version, vuln, fixed, layer, class string) platformFinding {
		var f platformFinding
		f.Properties.Canyon.Severity, f.Class, f.goStdlib = sev, class, name == "stdlib"
		f.Dependency = platformDependency{Name: name, Version: version, Vulnerability: vuln, Fixed: fixed, Layer: layer}
		return f
	}
	got := counted([]platformFinding{
		at(finding.High, "brace-expansion", "5.0.9", "CVE-2026-102276", "5.0.10, 3.0.7, 2.1.5, 1.1.19", node, "runtime"),
		at(finding.High, "brace-expansion", "5.0.9", "CVE-2026-102278", "5.0.11, 3.0.8, 2.1.6, 1.1.20", node, "runtime"),
		at(finding.High, "stdlib", "v1.26.5", "CVE-2026-33818", "1.25.13, 1.26.6, 1.27.0-rc.3", certs, "runtime"),
		at(finding.High, "stdlib", "v1.26.5", "CVE-2026-33818", "1.25.13, 1.26.6, 1.27.0-rc.3", launcher, "lifecycle"),
		at(finding.High, "stdlib", "v1.27.1", "CVE-2026-78667", "1.26.9, 1.27.2", node, "runtime"),
		at(finding.High, "stdlib", "v1.27.1", "CVE-2026-78668", "1.26.9, 1.27.2", node, "runtime"),
		at(finding.Medium, "brace-expansion", "5.0.9", "CVE-2026-102277", "5.0.12, 3.0.9, 2.1.7, 1.1.21", node, "runtime"),
		at(finding.Medium, "ip-address", "10.5.0", "CVE-2026-101910", "10.5.1", node, "runtime"),
	}, []string{launcher, certs, node}, map[string]string{launcher: "lifecycle", certs: "paketo-buildpacks/ca-certificates", node: "paketo-buildpacks/node-engine"})
	want := []platformPackage{
		{Name: "brace-expansion", Version: "5.0.9", Advisories: []platformAdvisory{{"CVE-2026-102276", "5.0.10, 3.0.7, 2.1.5, 1.1.19"}, {"CVE-2026-102278", "5.0.11, 3.0.8, 2.1.6, 1.1.20"}},
			Layers: []packageLayer{{"paketo-buildpacks/node-engine", "runtime"}}},
		{Name: "stdlib", Version: "v1.26.5", Advisories: []platformAdvisory{{"CVE-2026-33818", "1.25.13, 1.26.6, 1.27.0-rc.3"}}, GoStdlib: true,
			Layers: []packageLayer{{"lifecycle", "lifecycle"}, {"paketo-buildpacks/ca-certificates", "runtime"}}},
		{Name: "stdlib", Version: "v1.27.1", Advisories: []platformAdvisory{{"CVE-2026-78667", "1.26.9, 1.27.2"}, {"CVE-2026-78668", "1.26.9, 1.27.2"}}, GoStdlib: true,
			Layers: []packageLayer{{"paketo-buildpacks/node-engine", "runtime"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counted:\n%+v\nwant:\n%+v", got, want)
	}
	// A responder with no grant reads each as #2300 had it: the layers and go_stdlib are a contributor's (design/PERMISSIONS.md).
	rec := receipt{Rules: []receiptRule{{ID: platformRule, Packages: got}}}
	for _, p := range rec.forResponder().Rules[0].Packages {
		if p.GoStdlib || p.Layers != nil || len(p.Advisories) == 0 {
			t.Fatalf("a responder reads %+v", p)
		}
	}
	if !got[1].GoStdlib || len(got[1].Layers) != 2 {
		t.Fatalf("trimming the responder's copy trimmed the contributor's: %+v", got[1])
	}
}

// Where each counted package is, and whether it is Go's standard library, is the image read's technical detail
// (design/PERMISSIONS.md; #2579): a contributor's status read names each package's layers and go_stdlib, as the golden's
// row does; a responder with no grant reads the same row with each package as #2300 had it.
func TestPlatformKnownVulnRowsWhereIsAContributors(t *testing.T) {
	pool, ctx, base := approverHost(t)
	carl, rex, dana := signedIn(t, ctx, pool, base, "sub-carl", "carl@meridian.example", false), signedIn(t, ctx, pool, base, "sub-rex", "rex@meridian.example", false),
		signedIn(t, ctx, pool, base, "sub-dana", "dana@meridian.example", false)
	g := loadPlatformGolden(t)
	seedPlatform(t, ctx, pool, g, "claims-portal", executortest.Digest)
	if _, err := pool.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display)
		SELECT project_id, 'preview', id, 'canyon/claims-portal@'||$1, 'http://claims-portal.preview.localhost:7777', 'gr_seed', 'person', 'sub-carl', 'sub-carl' FROM packages WHERE project_id = 'claims-portal'`, executortest.Digest); err != nil {
		t.Fatal(err)
	}
	fake.Start("claims-portal", "preview")
	if code, _, _ := do(t, dana, http.MethodPut, base+"/api/v1/policies/"+platformRule+"?apply=true", `{"state":"blocking"}`); code != 200 {
		t.Fatalf("setting the rule blocking: %d %s", code, lastBody)
	}
	packages := func(c *http.Client) []any {
		t.Helper()
		if code, _, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", ""); code != 200 {
			t.Fatalf("the status read: %d %s", code, lastBody)
		}
		var st struct {
			Receipt struct {
				Rules []map[string]any `json:"rules"`
			} `json:"receipt"`
		}
		if err := json.Unmarshal([]byte(lastBody), &st); err != nil {
			t.Fatal(err)
		}
		for _, r := range st.Receipt.Rules {
			if r["id"] == platformRule {
				out, _ := r["packages"].([]any)
				return out
			}
		}
		t.Fatalf("no %s row: %s", platformRule, lastBody)
		return nil
	}
	var want struct {
		Packages []any `json:"packages"`
	}
	if err := json.Unmarshal(g.Row, &want); err != nil {
		t.Fatal(err)
	}
	if got := packages(carl); !reflect.DeepEqual(got, want.Packages) {
		t.Fatalf("a contributor's packages:\n got %s\nwant %s", mustJSON(got), mustJSON(want.Packages))
	}
	got := packages(rex)
	if len(got) != len(want.Packages) {
		t.Fatalf("a responder's packages: %s", mustJSON(got))
	}
	for _, p := range got {
		if one, _ := p.(map[string]any); one["layers"] != nil || one["go_stdlib"] != nil || one["advisories"] == nil {
			t.Fatalf("a responder with no grant reads a package's technical detail: %s", mustJSON(p))
		}
	}
}
