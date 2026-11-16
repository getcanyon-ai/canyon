package runs

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/executortest"
	"github.com/getcanyon-ai/canyon/internal/trivy"
	"github.com/getcanyon-ai/canyon/runs/runstest"
)

// platformGolden is contracts/testdata/platform/claims-portal.json: an image answer and its re-matches.
type platformGolden struct {
	ReadAt  time.Time           `json:"read_at"`
	Image   executor.ScanResult `json:"image"`
	Matches []struct {
		DiffID string              `json:"diff_id"`
		Answer executor.ScanResult `json:"answer"`
	} `json:"matches"`
}

func loadGolden(t *testing.T) platformGolden {
	t.Helper()
	raw, err := os.ReadFile("../contracts/testdata/platform/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var g platformGolden
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// newest is the golden's newest re-match of layer, as old against now as it is against the golden's read,
// or a report that matched nothing for a layer the golden has none of.
func (g platformGolden) newest(layer string, now time.Time) executor.ScanResult {
	res := executor.ScanResult{Tool: "trivy", DBBuiltAt: now.Add(-time.Hour).UTC().Format(time.RFC3339), Report: json.RawMessage(`{"SchemaVersion":2,"Results":[]}`)}
	var built time.Time
	for _, m := range g.Matches {
		at, _ := time.Parse(time.RFC3339, m.Answer.DBBuiltAt)
		if m.DiffID == layer && at.After(built) {
			built, res = at, m.Answer
			res.DBBuiltAt = now.Add(-g.ReadAt.Sub(at)).UTC().Format(time.RFC3339)
		}
	}
	return res
}

// layerOf is the layer a re-match part holds: its packages' LayerDiffID.
func layerOf(t *testing.T, part []byte) string {
	var doc struct{ Components []trivy.Component }
	if err := json.Unmarshal(part, &doc); err != nil {
		t.Errorf("a part that does not read: %v", err)
	}
	for _, c := range doc.Components {
		if d := c.Prop(trivy.LayerDiffID); d != "" {
			return d
		}
	}
	return ""
}

// rematching has the fake answer each layer's part with the golden's newest match of it — of the packages
// the part holds alone, as trivy matches what it is sent — refusing the layers in failing with
// not_finished; it answers which layers it was asked, in order.
func rematching(t *testing.T, f *executortest.Fake, g platformGolden, failing ...string) func() []string {
	return rematchingOn(t, f, g, time.Time{}, failing...)
}

// rematchingOn is rematching on a database built at built: the golden's own age when it is zero.
func rematchingOn(t *testing.T, f *executortest.Fake, g platformGolden, built time.Time, failing ...string) func() []string {
	var mu sync.Mutex
	var asked []string
	answer := func(part []byte) executor.ScanResult {
		d := layerOf(t, part)
		mu.Lock()
		asked = append(asked, d)
		mu.Unlock()
		if slices.Contains(failing, d) {
			return executor.ScanResult{Tool: "trivy", NotFinished: "timeout"}
		}
		res := holding(t, part, g.newest(d, time.Now()))
		if !built.IsZero() {
			res.DBBuiltAt = built.UTC().Format(time.RFC3339)
		}
		return res
	}
	f.Rematch.Store(&answer)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// holding is res with only the vulnerabilities of packages the part holds.
func holding(t *testing.T, part []byte, res executor.ScanResult) executor.ScanResult {
	var doc struct{ Components []trivy.Component }
	var report map[string]any
	if json.Unmarshal(part, &doc) != nil || json.Unmarshal(res.Report, &report) != nil {
		t.Errorf("a part or a report that does not read")
		return res
	}
	refs := map[string]bool{}
	for _, c := range doc.Components {
		refs[c.BOMRef] = true
	}
	results, _ := report["Results"].([]any)
	for _, r := range results {
		r, _ := r.(map[string]any)
		vulns, _ := r["Vulnerabilities"].([]any)
		kept := []any{}
		for _, v := range vulns {
			if ref, _ := v.(map[string]any)["PkgIdentifier"].(map[string]any)["BOMRef"].(string); refs[ref] {
				kept = append(kept, v)
			}
		}
		if vulns != nil {
			r["Vulnerabilities"] = kept
		}
	}
	res.Report, _ = json.Marshal(report)
	return res
}

// covered are the golden image's layers a re-match covers, as Canyon's read decides them.
func (g platformGolden) covered(t *testing.T) []string {
	t.Helper()
	img, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	owners, _ := trivy.Owners(img.DiffIDs, *img.Lifecycle)
	layers := map[string][]trivy.Component{}
	for _, l := range img.Layers {
		layers[l.DiffID], _ = trivy.DecodeComponents(l.Components)
	}
	return trivy.Covered(img.DiffIDs, trivy.Packages(img.DiffIDs, owners, layers), false)
}

// platformOn is every project's snapshot with platform.known-vuln advisory, as the server composes it.
func platformOn(context.Context, Querier, string) (policy.Snapshot, error) {
	return policy.WithStates(testPolicy(), map[string]policy.State{platformRule: policy.Advisory})
}

// withSBOM is the golden image answer with its SBOM edited.
func (g platformGolden) withSBOM(t *testing.T, edit func(components []any) []any) executor.ScanResult {
	t.Helper()
	var sbom map[string]any
	if err := json.Unmarshal(g.Image.SBOM, &sbom); err != nil {
		t.Fatal(err)
	}
	sbom["components"] = edit(sbom["components"].([]any))
	res := g.Image
	res.DiffIDs = slices.Clone(g.Image.DiffIDs)
	res.SBOM, _ = json.Marshal(sbom)
	return res
}

// What an image answer stores is what Canyon's read reads whole: every layer, bottom first, each of its
// packages under the layer trivy named, the image's operating-system component beside each, the rest
// unplaced; an executor's not_finished as it said it.
func TestAnImageAnswerStoresWhatTheReadReads(t *testing.T) {
	g := loadGolden(t)
	img, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(img.DiffIDs, g.Image.DiffIDs) || len(img.Layers) != len(g.Image.DiffIDs) || img.Lifecycle == nil || *img.Lifecycle != g.Image.Lifecycle {
		t.Fatalf("the image's scan: %v %d layers, label %v", img.DiffIDs, len(img.Layers), img.Lifecycle)
	}
	held := 0
	for i, l := range img.Layers {
		cs, ok := trivy.DecodeComponents(l.Components)
		part, err := sbomPart(l.OS, l.Components)
		if l.DiffID != g.Image.DiffIDs[i] || !ok || !trivy.StoredIn(cs, l.DiffID) || !strings.Contains(string(l.OS), `"operating-system"`) || err != nil || partSHA(part) != l.Part {
			t.Errorf("layer %d: %s %s, os %s, part %s (%v)", i, l.DiffID, l.Components, l.OS, l.Part, err)
		}
		held += len(cs)
	}
	if unplaced, ok := trivy.DecodeComponents(img.Unplaced); !ok || held+len(unplaced) != 9 { // ten in the golden's SBOM, one of them its operating system
		t.Errorf("every package stored once: %d in layers, unplaced %s", held, img.Unplaced)
	}
	if covered := g.covered(t); len(covered) != 5 {
		t.Errorf("the layers a re-match covers: %v", covered)
	}
	img, err = imageOf(executor.ScanResult{Tool: "trivy", NotFinished: "db_missing", Reason: "java"})
	if err != nil || img.NotFinished != "db_missing" || img.Reason != "java" || img.Layers != nil {
		t.Errorf("a not_finished answer stores itself alone: %+v %v", img, err)
	}
}

// Trivy draws a bom-ref at random on each scan for an operating system and a package without a purl or
// of a repeated one. Two scans of one run image, under other such refs, still store each shared layer as the
// same slice — one part, matched once for every image holding it — and the part is the bytes the re-match
// sends, however the store hands the slice back; a slice that differs in a package is another part.
func TestTwoScansOfOneRunImageShareItsSlices(t *testing.T) {
	g := loadGolden(t)
	uuids := 0
	redrawn := g.withSBOM(t, func(cs []any) []any {
		for _, c := range cs {
			c := c.(map[string]any)
			if c["purl"] == nil {
				uuids++
				c["bom-ref"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", uuids)
			}
		}
		return cs
	})
	one, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	two, err := imageOf(redrawn)
	if err != nil || uuids < 2 { // the operating system and undici-types, which carries no purl
		t.Fatalf("%d refs redrawn (%v)", uuids, err)
	}
	for i := range one.Layers {
		if one.Layers[i].Part != two.Layers[i].Part || !bytes.Equal(one.Layers[i].Components, two.Layers[i].Components) {
			t.Errorf("layer %d under other random refs: part %s, then %s", i, one.Layers[i].Part, two.Layers[i].Part)
		}
	}
	var stored []map[string]any // as jsonb hands it back: keys in its own order, spaced
	l := one.Layers[2]
	if err := json.Unmarshal(l.Components, &stored); err != nil {
		t.Fatal(err)
	}
	var spaced bytes.Buffer
	if err := json.Indent(&spaced, l.Components, "", "  "); err != nil {
		t.Fatal(err)
	}
	if part, err := sbomPart(l.OS, spaced.Bytes()); err != nil || partSHA(part) != l.Part {
		t.Errorf("the slice handed back spaced: %v, part %s, want %s", err, partSHA(part), l.Part)
	}
	other, err := imageOf(g.withSBOM(t, func(cs []any) []any {
		for _, c := range cs {
			if c := c.(map[string]any); c["name"] == "libssl3" {
				c["version"] = "3.0.13-0ubuntu3.5"
			}
		}
		return cs
	}))
	if err != nil || other.Layers[1].Part == one.Layers[1].Part || other.Layers[0].Part != one.Layers[0].Part {
		t.Errorf("a slice another package makes is another part, the rest the same: %s %s (%v)", other.Layers[1].Part, one.Layers[1].Part, err)
	}
}

// An image answer any part of which Canyon's read would not read stores nothing at all: never a scan that
// reads as half the image.
func TestAnImageAnswerCanyonWouldNotReadStoresNothing(t *testing.T) {
	g := loadGolden(t)
	with := func(edit func(sbom map[string]any, res *executor.ScanResult)) executor.ScanResult {
		var sbom map[string]any
		if err := json.Unmarshal(g.Image.SBOM, &sbom); err != nil {
			t.Fatal(err)
		}
		res := g.Image
		res.DiffIDs = slices.Clone(g.Image.DiffIDs)
		edit(sbom, &res)
		res.SBOM, _ = json.Marshal(sbom)
		return res
	}
	component := func(sbom map[string]any, i int) map[string]any { return sbom["components"].([]any)[i].(map[string]any) }
	for name, res := range map[string]executor.ScanResult{
		"a package without its bom-ref": with(func(sbom map[string]any, _ *executor.ScanResult) { delete(component(sbom, 1), "bom-ref") }),
		"a package without its name":    with(func(sbom map[string]any, _ *executor.ScanResult) { delete(component(sbom, 2), "name") }),
		"a component that is null": with(func(sbom map[string]any, _ *executor.ScanResult) {
			sbom["components"] = append(sbom["components"].([]any), nil)
		}),
		"a component nesting components":             with(func(sbom map[string]any, _ *executor.ScanResult) { component(sbom, 3)["components"] = []any{} }),
		"a component of a type trivy does not write": with(func(sbom map[string]any, _ *executor.ScanResult) { component(sbom, 4)["type"] = "file" }),
		"two operating systems": with(func(sbom map[string]any, _ *executor.ScanResult) {
			sbom["components"] = append(sbom["components"].([]any), component(sbom, 0))
		}),
		"no CycloneDX":          with(func(sbom map[string]any, _ *executor.ScanResult) { sbom["bomFormat"] = "SPDX" }),
		"a diff ID named twice": with(func(_ map[string]any, res *executor.ScanResult) { res.DiffIDs[1] = res.DiffIDs[0] }),
		"a diff ID not sha256":  with(func(_ map[string]any, res *executor.ScanResult) { res.DiffIDs[2] = "sha512:00" }),
		"no diff IDs":           with(func(_ map[string]any, res *executor.ScanResult) { res.DiffIDs = nil }),
		"no SBOM and no reason": {Tool: "trivy"},
	} {
		if img, err := imageOf(res); err == nil {
			t.Errorf("%s: stored %d layers, want nothing", name, len(img.Layers))
		}
	}
}

// An image past what Canyon keeps — an SBOM over 10 MiB, more layers than an image may have, or an
// operating-system component, copied beside every layer, over 64 KiB — is over_cap: could not check, asked
// again by the next deploy or tick, and never stored as one copy per layer.
func TestAnImagePastWhatCanyonKeepsIsOverCap(t *testing.T) {
	g := loadGolden(t)
	many := g.Image
	for i := range maxLayers + 1 - len(many.DiffIDs) {
		many.DiffIDs = append(slices.Clone(many.DiffIDs), fmt.Sprintf("sha256:%064x", i+1))
	}
	for name, res := range map[string]executor.ScanResult{
		"an SBOM past 10 MiB": g.withSBOM(t, func(cs []any) []any {
			return append(cs, map[string]any{"bom-ref": "pad", "type": "library", "name": strings.Repeat("x", maxStored)})
		}),
		"more layers than an image has": many,
		"an operating system over 64 KiB": g.withSBOM(t, func(cs []any) []any {
			cs[0].(map[string]any)["description"] = strings.Repeat("x", maxOS)
			return cs
		}),
	} {
		if img, err := imageOf(res); err != nil || img.NotFinished != "over_cap" || img.Layers != nil {
			t.Errorf("%s: %s %d layers (%v), want over_cap alone", name, img.NotFinished, len(img.Layers), err)
		}
	}
}

// A re-match stores trivy's report on a database built within the 5 days, and the bom-refs trivy never
// matched beside it, which Canyon's read counts as notes. A stale or failed answer — any not_finished, a
// database over 5 days old, of no date or built after now, a report not trivy's or naming a package the
// layer does not hold — stores nothing: never a clean match (design/PATCHING.md §3.2).
func TestRescanStaleDatabasePlansNothing(t *testing.T) {
	g := loadGolden(t)
	now := time.Now()
	img, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	layers := map[string][]trivy.Component{}
	for _, l := range img.Layers {
		layers[l.DiffID], _ = trivy.DecodeComponents(l.Components)
	}
	pkgs := trivy.Packages(img.DiffIDs, nil, layers)
	certs := g.Image.DiffIDs[4] // the layer whose golden match carries unscanned
	good := g.newest(certs, now)
	if m, err := matchOf(certs, pkgs, good, now); err != nil || len(m.Unscanned) != 1 || m.DiffID != certs {
		t.Fatalf("a fresh match stores its report and its unscanned: %+v %v", m, err)
	}
	other := g.newest(g.Image.DiffIDs[0], now)
	for name, res := range map[string]executor.ScanResult{
		"db_stale":                   {Tool: "trivy", NotFinished: "db_stale"},
		"db_missing":                 {Tool: "trivy", NotFinished: "db_missing"},
		"timeout":                    {Tool: "trivy", NotFinished: "timeout"},
		"killed":                     {Tool: "trivy", NotFinished: "killed"},
		"no_image":                   {Tool: "trivy", NotFinished: "no_image"},
		"unread":                     {Tool: "trivy", NotFinished: "not_sarif", Reason: "unread"},
		"over_cap":                   {Tool: "trivy", NotFinished: "over_cap", Reason: "report"},
		"a database 6 days old":      {Tool: "trivy", DBBuiltAt: now.Add(-6 * 24 * time.Hour).UTC().Format(time.RFC3339), Report: good.Report},
		"a database built after now": {Tool: "trivy", DBBuiltAt: now.Add(time.Hour).UTC().Format(time.RFC3339), Report: good.Report},
		"a database of no date":      {Tool: "trivy", Report: good.Report},
		"no report":                  {Tool: "trivy", DBBuiltAt: good.DBBuiltAt},
		"a report not trivy's":       {Tool: "trivy", DBBuiltAt: good.DBBuiltAt, Report: json.RawMessage(`{"SchemaVersion":1,"Results":[]}`)},
		"another layer's match":      {Tool: "trivy", DBBuiltAt: good.DBBuiltAt, Report: other.Report},
		"a report past 10 MiB":       {Tool: "trivy", DBBuiltAt: good.DBBuiltAt, Report: json.RawMessage(`{"SchemaVersion":2,"Results":[],"x":"` + strings.Repeat("x", maxStored) + `"}`)},
	} {
		if m, err := matchOf(certs, pkgs, res, now); err == nil {
			t.Errorf("%s: stored a match on %s, want nothing", name, m.DBBuiltAt)
		}
	}
}

// Each tick falls on the same instants on every replica, so DBOS's id for it — the schedule's name and the
// tick's time — is one workflow; an interval that falls on no such instants is refused.
func TestRescanScheduleFallsOnTheClock(t *testing.T) {
	for every, want := range map[time.Duration]string{6 * time.Hour: "0 0 */6 * * *", 24 * time.Hour: "0 0 */24 * * *", 30 * time.Minute: "0 */30 * * * *",
		time.Minute: "0 */1 * * * *", time.Second: "*/1 * * * * *"} {
		if got, err := RescanSchedule(every); got != want || err != nil {
			t.Errorf("%s: %q %v, want %q", every, got, err, want)
		}
	}
	for _, every := range []time.Duration{0, -time.Hour, 7 * time.Hour, 45 * time.Minute, 90 * time.Second, 1500 * time.Millisecond, 48 * time.Hour} {
		if got, err := RescanSchedule(every); err == nil {
			t.Errorf("%s: %q, want refused", every, got)
		}
	}
}

// A busy executor is asked again no sooner than a second after a Retry-After of 0, and for no longer than
// busyFor in all: then the scan is an error, never a slot held for ever.
func TestABusyExecutorIsAskedAgainWithinBounds(t *testing.T) {
	var asks, busy atomic.Int32
	busy.Store(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asks.Add(1) <= busy.Load() {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(executor.ScanResult{Tool: "trivy", NotFinished: "timeout"})
	}))
	t.Cleanup(srv.Close)
	p := &Pipeline{Executor: &executor.Client{Base: srv.URL}}
	start := time.Now()
	if res, err := p.askScan(t.Context(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, []byte("{}")); err != nil || res.NotFinished != "timeout" || asks.Load() != 3 || time.Since(start) < 2*time.Second {
		t.Fatalf("asked %d times in %s: %+v %v", asks.Load(), time.Since(start), res, err)
	}
	was := busyFor
	busyFor = 2500 * time.Millisecond
	t.Cleanup(func() { busyFor = was })
	asks.Store(0)
	busy.Store(1 << 30)
	if _, err := p.askScan(t.Context(), executor.ScanRequest{Tool: "trivy", Target: "sbom"}, []byte("{}")); err == nil || asks.Load() > 3 {
		t.Fatalf("always busy: asked %d times, %v", asks.Load(), err)
	}
}

// onDeploy is a deploy run of the package under the rule's state, ended clean.
func onDeploy(t *testing.T, ctx context.Context, p *pgxpool.Pool, state policy.State) string {
	t.Helper()
	snap, err := policy.WithStates(testPolicy(), map[string]policy.State{platformRule: state})
	if err != nil {
		t.Fatal(err)
	}
	run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Deploy: true, Actor: dana, Policy: snap})
	if err != nil {
		t.Fatal(err)
	}
	if state, reason := waitEnded(t, ctx, p, run.ID); state != "clean" {
		t.Fatalf("the deploy: %s %s", state, why(reason))
	}
	return run.ID
}

func freshImage(t *testing.T, f *executortest.Fake) string {
	t.Helper()
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	f.Image = "canyon/" + project + "@sha256:" + hex.EncodeToString(b)
	return ImageDigest(f.Image)
}

// imageScans are the image.sbom workflows of project's digest once every one of them has ended.
func imageScans(t *testing.T, project, digest string) []dbos.WorkflowStatus {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		all, err := dbos.ListWorkflows(durable, dbos.WithFilterWorkflowIDPrefix(project+"@"+digest+"@"), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(all, func(w dbos.WorkflowStatus) bool {
			return w.Status == dbos.WorkflowStatusEnqueued || w.Status == dbos.WorkflowStatusPending
		}) {
			return all
		}
		if time.Now().After(deadline) {
			t.Fatalf("image.sbom of %s never ended", digest)
		}
	}
}

// scanOf is what is stored of project's scan of digest: why it did not finish, if it did not, and whether
// its layers were matched and whether a later match of them failed.
func scanOf(t *testing.T, ctx context.Context, p *pgxpool.Pool, project, digest string) (notFinished string, matched, failed bool) {
	t.Helper()
	if err := p.QueryRow(ctx, "SELECT coalesce(not_finished, ''), rematched_at IS NOT NULL, rescan_failed_at IS NOT NULL FROM scanned_images WHERE project_id = $1 AND digest = $2",
		project, digest).Scan(&notFinished, &matched, &failed); err != nil {
		t.Fatalf("%s's scan of %s: %v", project, digest, err)
	}
	return notFinished, matched, failed
}

// A deploy asks for its image's SBOM, and the scan matches its layers at once: two deploys of one image
// read it once, every layer stored with the scan and each covered layer matched, so production waits the
// scan's length, never a rescan interval. While the rule is off nothing is read (design/PATCHING.md §3.1,
// §3.3).
func TestImageSBOMIdempotentByDigest(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	rematching(t, f, g)
	digest := freshImage(t, f)
	onDeploy(t, ctx, p, policy.Off)
	if asked := imageScans(t, project, digest); len(asked) != 0 || f.ImageScans.Load() != 0 {
		t.Fatalf("the rule off, an image read anyway: %d scans, %d workflows", f.ImageScans.Load(), len(asked))
	}
	onDeploy(t, ctx, p, policy.Advisory) // the image built above, reused: no build value changed
	onDeploy(t, ctx, p, policy.Advisory)
	if asked := imageScans(t, project, digest); len(asked) != 2 || slices.ContainsFunc(asked, func(w dbos.WorkflowStatus) bool { return w.Status != dbos.WorkflowStatusSuccess }) {
		t.Fatalf("each deploy asks: %d workflows %+v", len(asked), asked)
	}
	if n := f.ImageScans.Load(); n != 1 {
		t.Fatalf("two deploys of one image read it %d times, want once", n)
	}
	var diffIDs []string
	var layers int
	if err := p.QueryRow(ctx, "SELECT diff_ids, (SELECT count(*) FROM image_layers l WHERE (l.project_id, l.digest) = (s.project_id, s.digest)) FROM scanned_images s WHERE project_id = $1 AND digest = $2", project, digest).Scan(&diffIDs, &layers); err != nil ||
		!slices.Equal(diffIDs, g.Image.DiffIDs) || layers != len(diffIDs) {
		t.Fatalf("the image's scan and every layer: %v %d (%v)", diffIDs, layers, err)
	}
	if _, matched, failed := scanOf(t, ctx, p, project, digest); !matched || failed || len(matchesOf(t, ctx, p)) != len(g.covered(t)) {
		t.Fatalf("matched at once: %v, failed %v, %v", matched, failed, matchesOf(t, ctx, p))
	}
}

// A scan that did not finish is no answer to keep: the next deploy of the same image, reusing it, asks
// again, and that scan's finished answer is stored and matched; a finished scan is never asked again.
func TestANotFinishedScanIsAskedAgain(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	rematching(t, f, g)
	f.ImageAnswer.Store(&executor.ScanResult{Tool: "trivy", NotFinished: "timeout"})
	digest := freshImage(t, f)
	onDeploy(t, ctx, p, policy.Advisory)
	imageScans(t, project, digest)
	if notFinished, _, _ := scanOf(t, ctx, p, project, digest); notFinished != "timeout" {
		t.Fatalf("the first scan: %q", notFinished)
	}
	f.ImageAnswer.Store(&g.Image)
	onDeploy(t, ctx, p, policy.Advisory)
	imageScans(t, project, digest)
	if notFinished, matched, failed := scanOf(t, ctx, p, project, digest); notFinished != "" || !matched || failed || f.ImageScans.Load() != 2 {
		t.Fatalf("asked again: %q, matched %v, failed %v, %d scans", notFinished, matched, failed, f.ImageScans.Load())
	}
	onDeploy(t, ctx, p, policy.Advisory)
	imageScans(t, project, digest)
	if n := f.ImageScans.Load(); n != 2 {
		t.Fatalf("a finished scan read again: %d scans", n)
	}
}

// One digest deployed from two projects is two scans, each through its own image.sbom, stored apart.
func TestTheSameDigestFromTwoProjectsIsTwoScans(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	rematching(t, f, g)
	digest := freshImage(t, f)
	for _, app := range []string{"twin-a", "twin-b"} {
		if _, err := p.Exec(ctx, "INSERT INTO projects (id) VALUES ($1)", app); err != nil {
			t.Fatal(err)
		}
		in := imageScan{Run: "gr_" + app, Project: app, Image: "canyon/" + app + "@" + digest}
		if _, err := dbos.Enqueue[any](durable, PatchesQueue, ImageSBOMWorkflow, in, dbos.WithEnqueueWorkflowID(imageScanID(app, digest, in.Run))); err != nil {
			t.Fatal(err)
		}
	}
	for _, app := range []string{"twin-a", "twin-b"} {
		if asked := imageScans(t, app, digest); len(asked) != 1 || asked[0].Status != dbos.WorkflowStatusSuccess {
			t.Fatalf("%s's image.sbom: %+v", app, asked)
		}
		if notFinished, matched, _ := scanOf(t, ctx, p, app, digest); notFinished != "" || !matched {
			t.Errorf("%s's own scan: %q, matched %v", app, notFinished, matched)
		}
	}
	if n := f.ImageScans.Load(); n != 2 {
		t.Fatalf("two projects' image read %d times, want once each", n)
	}
}

// storedGolden stores the golden image under digest as image.sbom does, and runs it in preview.
func storedGolden(t *testing.T, ctx context.Context, p *pgxpool.Pool, g *platformGolden, digest string) {
	t.Helper()
	storedAs(t, ctx, p, &g.Image, project, digest)
}

// storedAs stores an image answer under app and digest as image.sbom does — none when answer is nil — and
// runs it in app's preview.
func storedAs(t *testing.T, ctx context.Context, p *pgxpool.Pool, answer *executor.ScanResult, app, digest string) {
	t.Helper()
	if _, err := p.Exec(ctx, "INSERT INTO projects (id) VALUES ($1) ON CONFLICT DO NOTHING", app); err != nil {
		t.Fatal(err)
	}
	if answer != nil {
		img, err := imageOf(*answer)
		if err != nil {
			t.Fatal(err)
		}
		if err := storeImage(ctx, p, app, digest, img); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"INSERT INTO packages (id, project_id, tree_hash, commit, size_bytes, source_kind) VALUES ('pkg_' || $1, $1, 'aa', 'bb', 1, 'upload') ON CONFLICT DO NOTHING",
		"INSERT INTO runs (id, project_id, package_id, state, kind, ended_at) VALUES ('gr_' || $1, $1, 'pkg_' || $1, 'clean', 'gate', now()) ON CONFLICT DO NOTHING",
		"INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, run_by) VALUES ($1, 'preview', 'pkg_' || $1, $2, 'http://' || $1 || '.preview.localhost:7777', 'gr_' || $1, 'canyon')",
	} {
		args := []any{app}
		if strings.Contains(q, "$2") {
			args = append(args, "canyon/"+app+"@"+digest)
		}
		if _, err := p.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// matchesOf is each layer's stored matches, by their databases.
func matchesOf(t *testing.T, ctx context.Context, p *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := p.Query(ctx, "SELECT l.diff_id, count(*) FROM part_matches m JOIN image_layers l USING (part_sha) GROUP BY l.diff_id")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	var d string
	var n int
	for rows.Next() {
		if err := rows.Scan(&d, &n); err != nil {
			t.Fatal(err)
		}
		out[d] = n
	}
	return out
}

// matchRows are the stored matches as text: each slice's database and when it was matched.
func matchRows(t *testing.T, ctx context.Context, p *pgxpool.Pool) []string {
	t.Helper()
	rows, err := p.Query(ctx, "SELECT part_sha || ' ' || db_built_at::text || ' ' || matched_at::text FROM part_matches ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// tick runs one rescan tick scheduled at at, and answers the slices it matched.
func tick(t *testing.T, at time.Time) int {
	t.Helper()
	h, err := dbos.RunWorkflow(durable, pipe.rescan, dbos.ScheduledWorkflowInput{ScheduledTime: at})
	if err != nil {
		t.Fatal(err)
	}
	n, err := h.GetResult()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// A rescan stores an image's new matches together or not at all: one covered layer that did not match
// leaves every layer of the image unmatched and marks the scan's rescan failed, which its read reads as
// could not check; a whole stale tick stores nothing; once every layer matches, each is stored and the mark
// clears.
func TestRescanAPartialFailureStoresNothing(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	digest := freshImage(t, f)
	storedGolden(t, ctx, p, &g, digest)
	covered := g.covered(t)
	asked := rematching(t, f, g, covered[len(covered)-1])
	if n := tick(t, time.Now()); n != 0 {
		t.Fatalf("a failed image matched %d slices", n)
	}
	if got := matchesOf(t, ctx, p); len(got) != 0 || !slices.Equal(asked(), covered) {
		t.Fatalf("one layer of %v failed, yet stored %v (asked %v)", covered, got, asked())
	}
	if _, _, failed := scanOf(t, ctx, p, project, digest); !failed {
		t.Fatal("a failed rescan is not marked on the scan")
	}
	stale := func([]byte) executor.ScanResult { return executor.ScanResult{Tool: "trivy", NotFinished: "db_stale"} }
	f.Rematch.Store(&stale)
	if n := tick(t, time.Now()); n != 0 || len(matchesOf(t, ctx, p)) != 0 {
		t.Fatalf("a stale database stored %v", matchesOf(t, ctx, p))
	}
	rematching(t, f, g)
	if n := tick(t, time.Now()); n != len(covered) {
		t.Fatalf("matched %d slices, want %d", n, len(covered))
	}
	if got := matchesOf(t, ctx, p); len(got) != len(covered) || slices.ContainsFunc(covered, func(d string) bool { return got[d] != 1 }) {
		t.Fatalf("every covered layer's newest match alone: %v, covered %v", got, covered)
	}
	if _, matched, failed := scanOf(t, ctx, p, project, digest); !matched || failed {
		t.Fatalf("a full match clears the mark: matched %v, failed %v", matched, failed)
	}
}

// Each slice keeps its newest match alone, by its database's build: the same build again changes no row, a
// newer build replaces the older, an older one after a newer is dropped, and a failed tick after a good one
// keeps the good rows but marks the scan, so its read says could not check.
func TestRescanKeepsEachSlicesNewestMatch(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	digest := freshImage(t, f)
	storedGolden(t, ctx, p, &g, digest)
	covered := g.covered(t)
	now := time.Now().Truncate(time.Second)
	builds := func() []string {
		t.Helper()
		rows, err := p.Query(ctx, "SELECT DISTINCT db_built_at FROM part_matches")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, at := range must(pgx.CollectRows(rows, pgx.RowTo[time.Time])) {
			out = append(out, at.UTC().Format(time.RFC3339))
		}
		return out
	}
	at := func(d time.Duration) string { return now.Add(d).UTC().Format(time.RFC3339) }
	rematchingOn(t, f, g, now.Add(-3*time.Hour))
	if n := tick(t, time.Now()); n != len(covered) {
		t.Fatalf("the first build: %d slices, want %d", n, len(covered))
	}
	first := matchRows(t, ctx, p)
	if n := tick(t, time.Now()); n != len(covered) || !slices.Equal(matchRows(t, ctx, p), first) {
		t.Fatalf("the same build again: %d slices, rows %v, were %v", n, matchRows(t, ctx, p), first)
	}
	rematchingOn(t, f, g, now.Add(-2*time.Hour))
	tick(t, time.Now())
	if got := builds(); !slices.Equal(got, []string{at(-2 * time.Hour)}) || len(matchRows(t, ctx, p)) != len(covered) {
		t.Fatalf("a newer build replaces the older: %v, %d rows", got, len(matchRows(t, ctx, p)))
	}
	rematchingOn(t, f, g, now.Add(-4*time.Hour))
	tick(t, time.Now())
	if got := builds(); !slices.Equal(got, []string{at(-2 * time.Hour)}) {
		t.Fatalf("an older build after a newer: %v", got)
	}
	rematchingOn(t, f, g, now.Add(-time.Hour), covered[0])
	if n := tick(t, time.Now()); n != 0 {
		t.Fatalf("a failed tick matched %d slices", n)
	}
	if _, _, failed := scanOf(t, ctx, p, project, digest); !failed || !slices.Equal(builds(), []string{at(-2 * time.Hour)}) {
		t.Fatalf("a failed tick after a good one: failed %v, builds %v", failed, builds())
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// A tick keeps the scans of what runs and what is kept for a rollback, and the slices they name, and drops
// the rest: a scan of an image no environment runs any more, a deleted project's, and every slice no scan
// names, with its matches. It matches the kept image too, so a rollback mid-incident never meets a stale
// database.
func TestRescanKeepsWhatRunsAndIsKept(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	rematching(t, f, g)
	running, kept, replaced, deleted := freshImage(t, f), freshImage(t, f), freshImage(t, f), freshImage(t, f)
	storedGolden(t, ctx, p, &g, running)
	img, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	other, err := imageOf(g.withSBOM(t, func(cs []any) []any {
		for _, c := range cs {
			if c := c.(map[string]any); c["name"] == "libssl3" {
				c["version"] = "3.0.13-0ubuntu3.5" // a slice the replaced image alone holds
			}
		}
		return cs
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		app, digest string
		img         storedImage
	}{{project, kept, img}, {project, replaced, other}} {
		if err := storeImage(ctx, p, s.app, s.digest, s.img); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Exec(ctx, `INSERT INTO deploy_history (project_id, package_id, image, gate_run, run, kept) VALUES ($1, 'pkg_' || $1, $2, 'gr_' || $1, 'gr_' || $1, true), ($1, 'pkg_' || $1, $3, 'gr_' || $1, 'gr_' || $1, false)`,
		project, "canyon/"+project+"@"+kept, "canyon/"+project+"@"+replaced); err != nil {
		t.Fatal(err)
	}
	storedAs(t, ctx, p, &g.Image, "deleted-app", deleted)
	if _, err := p.Exec(ctx, "UPDATE projects SET deleted_at = now() WHERE id = 'deleted-app'"); err != nil {
		t.Fatal(err)
	}
	tick(t, time.Now())
	var scans []string
	rows, err := p.Query(ctx, "SELECT project_id || ' ' || digest FROM scanned_images ORDER BY 1")
	if err == nil {
		scans, err = pgx.CollectRows(rows, pgx.RowTo[string])
	}
	if want := []string{project + " " + min(running, kept), project + " " + max(running, kept)}; err != nil || !slices.Equal(scans, want) {
		t.Fatalf("the scans kept: %v (%v), want %v", scans, err, want)
	}
	var orphans int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM layer_parts s WHERE NOT EXISTS (SELECT 1 FROM image_layers l WHERE l.part_sha = s.part_sha))
		+ (SELECT count(*) FROM part_matches m WHERE NOT EXISTS (SELECT 1 FROM image_layers l WHERE l.part_sha = m.part_sha))`).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("%d slices or matches no scan names (%v)", orphans, err)
	}
	if _, matched, _ := scanOf(t, ctx, p, project, kept); !matched {
		t.Fatal("the image kept for a rollback is not matched")
	}
}

// A tick reads the images that run with no finished scan — deployed before the rule was turned on, say —
// of a project whose rule is on, and matches them; a project whose rule is off is left alone. A tick an
// interval late does nothing: the next one is due.
func TestRescanReadsWhatRunsUnscanned(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	rematching(t, f, g)
	unscanned, off := freshImage(t, f), freshImage(t, f)
	storedAs(t, ctx, p, nil, project, unscanned)
	storedAs(t, ctx, p, nil, "rule-off", off)
	was := pipe.ProjectPolicy.Load()
	pipe.ProjectPolicy.Store(func(ctx context.Context, q Querier, app string) (policy.Snapshot, error) {
		if app == "rule-off" {
			return testPolicy(), nil
		}
		return platformOn(ctx, q, app)
	})
	t.Cleanup(func() { pipe.ProjectPolicy.Store(was) })
	if n := tick(t, time.Now().Add(-2*DefaultRescan)); n != 0 || f.ImageScans.Load() != 0 {
		t.Fatalf("a late tick did something: %d slices, %d scans", n, f.ImageScans.Load())
	}
	tick(t, time.Now())
	if asked := imageScans(t, project, unscanned); len(asked) != 1 || asked[0].Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("the unscanned image's read: %+v", asked)
	}
	if notFinished, matched, _ := scanOf(t, ctx, p, project, unscanned); notFinished != "" || !matched {
		t.Fatalf("read and matched: %q %v", notFinished, matched)
	}
	if asked := imageScans(t, "rule-off", off); len(asked) != 0 || f.ImageScans.Load() != 1 {
		t.Fatalf("a project whose rule is off was read: %d workflows, %d scans", len(asked), f.ImageScans.Load())
	}
}

// A project that places deps.known-vuln before production, platform.known-vuln off, has the image it runs read
// and its app layers matched too, as its row reads the built app; the run image is not read for it, as the patch
// plan is platform.known-vuln's alone. A project whose platform.known-vuln alone is on matches no app layer
// (design/PATCHING.md §3.2, §3.6).
func TestRescanMatchesTheBuiltAppWhereDepsIsPlaced(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	f.ImageAnswer.Store(&g.Image)
	asked := rematching(t, f, g)
	img := freshImage(t, f)
	storedAs(t, ctx, p, nil, project, img)
	was := pipe.ProjectPolicy.Load()
	pipe.ProjectPolicy.Store(func(context.Context, Querier, string) (policy.Snapshot, error) {
		snap := testPolicy()
		for i := range snap.Rules {
			if snap.Rules[i].ID == depsRule {
				snap.Rules[i].Production = policy.Blocking
			}
		}
		return snap, nil
	})
	t.Cleanup(func() { pipe.ProjectPolicy.Store(was) })
	runImages := f.RunImageScans.Load()
	tick(t, time.Now())
	if asked := imageScans(t, project, img); len(asked) != 1 || asked[0].Status != dbos.WorkflowStatusSuccess {
		t.Fatalf("the image's read: %+v", asked)
	}
	if notFinished, matched, _ := scanOf(t, ctx, p, project, img); notFinished != "" || !matched {
		t.Fatalf("read and matched: %q %v", notFinished, matched)
	}
	app := "sha256:" + strings.Repeat("6", 64)
	if !slices.Contains(asked(), app) {
		t.Fatalf("the app's own layer was not matched: %v", asked())
	}
	if n := f.RunImageScans.Load(); n != runImages {
		t.Fatalf("the run image was read for a project whose platform.known-vuln is off: %d reads", n-runImages)
	}
	if got := g.covered(t); slices.Contains(got, app) {
		t.Fatalf("platform.known-vuln alone covers the app's own layer: %v", got)
	}
}

// An app layer deps.known-vuln alone reads that does not match marks only the built app's read: the platform
// layers' matches are stored and the scan's own rescan reads finished, so the platform rule's read and the patch
// plan go on; once it matches, the mark clears (design/PATCHING.md §3.2).
func TestAnAppLayerThatDoesNotMatchMarksTheBuiltAppAlone(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	app := "sha256:" + strings.Repeat("6", 64)
	rematching(t, f, g, app)
	img := freshImage(t, f)
	storedGolden(t, ctx, p, &g, img)
	was := pipe.ProjectPolicy.Load()
	pipe.ProjectPolicy.Store(func(context.Context, Querier, string) (policy.Snapshot, error) {
		snap := testPolicy()
		for i := range snap.Rules {
			switch snap.Rules[i].ID {
			case depsRule:
				snap.Rules[i].Production = policy.Blocking
			case platformRule: // both rules read the image: the platform's read must not stop for the app's layer
				snap.Rules[i].State = policy.Advisory
			}
		}
		return snap, nil
	})
	t.Cleanup(func() { pipe.ProjectPolicy.Store(was) })
	appFailed := func() bool {
		t.Helper()
		var failed bool
		if err := p.QueryRow(ctx, "SELECT app_rescan_failed_at IS NOT NULL FROM scanned_images WHERE project_id = $1 AND digest = $2", project, img).Scan(&failed); err != nil {
			t.Fatal(err)
		}
		return failed
	}
	tick(t, time.Now())
	if _, matched, failed := scanOf(t, ctx, p, project, img); !matched || failed || !appFailed() {
		t.Fatalf("an app layer that did not match: matched %v, rescan failed %v, app rescan failed %v", matched, failed, appFailed())
	}
	if covered := matchesOf(t, ctx, p); covered[app] != 0 || len(covered) == 0 {
		t.Fatalf("the matches stored: %v", covered)
	}
	rematching(t, f, g)
	tick(t, time.Now())
	if _, _, failed := scanOf(t, ctx, p, project, img); failed || appFailed() {
		t.Fatalf("matched again: rescan failed %v, app rescan failed %v", failed, appFailed())
	}
}

// Two server replicas, each with the rescan scheduled every second, run each tick once between them: each
// tick one workflow, DBOS's own id for it — the schedule's name and the tick's time — on the rescan's
// queue, and each covered layer matched once a tick (design/PATCHING.md §3.2).
func TestRescanTwoReplicasOneTick(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	storedGolden(t, ctx, p, &g, freshImage(t, f))
	rematching(t, f, g)
	schema := "dbos_rescan_" + strings.ToLower(hex.EncodeToString([]byte(t.Name()))[:12])
	admin, err := pgxpool.New(ctx, os.Getenv("CANYON_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		admin.Close()
	})
	var replicas []dbos.Context
	for range 2 {
		dc, err := NewDurable(ctx, os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		replica := &Pipeline{Pool: p, Executor: pipe.Executor, RescanEvery: time.Second}
		replica.ProjectPolicy.Store(platformOn)
		if err := replica.Register(dc); err != nil {
			t.Fatal(err)
		}
		if err := dbos.Launch(dc); err != nil {
			t.Fatal(err)
		}
		replicas = append(replicas, dc)
	}
	list := func() []dbos.WorkflowStatus {
		t.Helper()
		ticks, err := dbos.ListWorkflows(replicas[0], dbos.WithFilterName(RescanWorkflow), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
		if err != nil {
			t.Fatal(err)
		}
		return ticks
	}
	// A tick that starts an interval late or more does nothing past its "now" step (rescan), which a loaded
	// runner makes of any tick: only the done ticks that went on to "retain" owe their layers (#2307).
	onTime := func(dc dbos.Context, ticks []dbos.WorkflowStatus) int {
		t.Helper()
		n := 0
		for _, w := range ticks {
			if w.Status != dbos.WorkflowStatusSuccess {
				continue
			}
			steps, err := dbos.GetWorkflowSteps(dc, w.ID)
			if err != nil {
				t.Fatalf("the tick %s's steps: %v", w.ID, err)
			}
			if slices.ContainsFunc(steps, func(s dbos.StepInfo) bool { return s.StepName == "retain" }) {
				n++
			}
		}
		return n
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) { // both replicas have ticked on time a while
		done := onTime(replicas[0], list())
		if done >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d ticks done on time in 30 s", done)
		}
	}
	for _, dc := range replicas {
		if err := dbos.Shutdown(dc, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	other, err := NewDurable(ctx, os.Getenv("CANYON_TEST_DATABASE_URL"), schema, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ticks, err := dbos.ListWorkflows(other, dbos.WithFilterName(RescanWorkflow), dbos.WithFilterLoadInput(false), dbos.WithFilterLoadOutput(false))
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]int{}
	for _, w := range ticks {
		when, ok := strings.CutPrefix(w.ID, "sched-"+RescanWorkflow+"-") // DBOS's own id: the schedule's name, then the tick's time
		if _, err := time.Parse(time.RFC3339, when); !ok || err != nil || w.QueueName != RescanQueue {
			t.Errorf("a tick's workflow: id %s on %s", w.ID, w.QueueName)
		}
		at[when]++
	}
	if len(ticks) < 4 || len(at) != len(ticks) {
		t.Fatalf("two replicas: %d workflows on %d ticks, want one each (%v)", len(ticks), len(at), at)
	}
	if n, covered, done := int(f.Rematches.Load()), len(g.covered(t)), onTime(other, ticks); n < done*covered || n > len(ticks)*covered {
		t.Fatalf("%d re-matches for %d ticks, %d of them done on time: want each covered layer once a tick", n, len(ticks), done)
	}
}

// What trivy lists in a layer depends on the whole image, and two apps may run one digest, so what one app's
// scan stored never changes what another reads (contracts/finding.md, System libraries and build tools: stored by content,
// read as the project's own):
//   - app A's crafted image, scanned first, whose SBOM lists the run-image layer B's image shares without
//     libssl3 and on another OS — an upper layer can rewrite or white out the dpkg status file or os-release
//     trivy reads them from — leaves B's layer B's own, and its match still names libssl3's vulnerability;
//   - one digest scanned under two projects is two scans: each reads only its own, label and layers.
func TestACraftedImageNeverChangesAnotherAppsSharedLayer(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	crafted := g.withSBOM(t, func(cs []any) []any {
		var kept []any
		for _, c := range cs {
			c := c.(map[string]any)
			switch {
			case strings.HasPrefix(fmt.Sprint(c["purl"]), "pkg:deb/ubuntu/libssl3@"):
				continue
			case c["type"] == "operating-system":
				c["name"], c["version"] = "debian", "12"
			}
			kept = append(kept, c)
		}
		return kept
	})
	crafted.Lifecycle = strings.Replace(g.Image.Lifecycle, "paketo-buildpacks/node-engine", "crafted/node-engine", 1)
	base := g.Image.DiffIDs[1] // the run image's layer holding libssl3, which both images share
	layerOf := func(app, digest string) (components, report string) {
		t.Helper()
		if err := p.QueryRow(ctx, `SELECT s.components::text, coalesce((SELECT m.report::text FROM part_matches m WHERE m.part_sha = l.part_sha ORDER BY m.db_built_at DESC LIMIT 1), '')
			FROM image_layers l JOIN layer_parts s USING (part_sha) WHERE l.project_id = $1 AND l.digest = $2 AND l.diff_id = $3`, app, digest, base).Scan(&components, &report); err != nil {
			t.Fatalf("%s's layer %s of %s: %v", app, base, digest, err)
		}
		return components, report
	}
	t.Run("a shared layer", func(t *testing.T) {
		a, b := freshImage(t, f), freshImage(t, f)
		storedAs(t, ctx, p, &crafted, "crafted-app", a)
		storedAs(t, ctx, p, &g.Image, project, b)
		rematching(t, f, g)
		tick(t, time.Now())
		if components, report := layerOf(project, b); !strings.Contains(components, "pkg:deb/ubuntu/libssl3@") || !strings.Contains(report, "pkg:deb/ubuntu/libssl3@") {
			t.Fatalf("B's shared layer as A's crafted scan listed it:\n packages %.300s\n match %.300s", components, report)
		}
	})
	t.Run("one digest in two projects", func(t *testing.T) {
		same := freshImage(t, f)
		storedAs(t, ctx, p, &crafted, "crafted-twin", same)
		storedAs(t, ctx, p, &g.Image, "honest-twin", same)
		for app, want := range map[string]struct {
			label   string
			libssl3 bool
		}{"crafted-twin": {crafted.Lifecycle, false}, "honest-twin": {g.Image.Lifecycle, true}} {
			var label string
			if err := p.QueryRow(ctx, "SELECT lifecycle FROM scanned_images WHERE project_id = $1 AND digest = $2", app, same).Scan(&label); err != nil || label != want.label {
				t.Errorf("%s's own label: %.80s (%v)", app, label, err)
			}
			if components, _ := layerOf(app, same); strings.Contains(components, "pkg:deb/ubuntu/libssl3@") != want.libssl3 {
				t.Errorf("%s reads another project's layer: %.300s", app, components)
			}
		}
	})
}

// A tick's sweep never drops what is stood on while it runs: a redeploy of an image nothing ran, finding its
// finished scan after the sweep began, marks it seen, and the sweep keeps it; a scan storing a slice no scan
// names yet does the same for the slice. A sweep with nothing seen since it began drops them, and a deploy
// after it finds no scan to stand on, so its image is read again.
func TestASweepNeverDropsAScanADeployStandsOn(t *testing.T) {
	p, ctx, f := deploying(t, runstest.CleanGolden)
	g := loadGolden(t)
	digest := freshImage(t, f) // stored, and nothing runs it: the sweep's to drop
	img, err := imageOf(g.Image)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeImage(ctx, p, project, digest, img); err != nil {
		t.Fatal(err)
	}
	orphan := partSHA([]byte("a slice no scan names yet"))
	if _, err := p.Exec(ctx, "INSERT INTO layer_parts (part_sha, components) VALUES ($1, '[]')", orphan); err != nil {
		t.Fatal(err)
	}
	sweep := func(between func()) {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT now()"); err != nil { // the sweep has begun
			t.Fatal(err)
		}
		between()
		for _, q := range []string{dropScans, dropParts} {
			if _, err := tx.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	held := func() (scan, part bool) {
		t.Helper()
		if err := p.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM scanned_images WHERE project_id = $1 AND digest = $2), EXISTS (SELECT 1 FROM layer_parts WHERE part_sha = $3)",
			project, digest, orphan).Scan(&scan, &part); err != nil {
			t.Fatal(err)
		}
		return scan, part
	}
	sweep(func() {
		if ok, err := standOn(ctx, p, project, digest); !ok || err != nil {
			t.Fatalf("a redeploy finds the finished scan: %v %v", ok, err)
		}
		if _, err := p.Exec(ctx, storePart, orphan, nil, []byte("[]")); err != nil {
			t.Fatal(err)
		}
	})
	if scan, part := held(); !scan || !part {
		t.Fatalf("the sweep dropped what was stood on while it ran: scan %v, slice %v", scan, part)
	}
	sweep(func() {})
	if scan, part := held(); scan || part {
		t.Fatalf("a sweep with nothing seen since it began kept: scan %v, slice %v", scan, part)
	}
	if ok, err := standOn(ctx, p, project, digest); ok || err != nil {
		t.Fatalf("a deploy after the sweep stands on a dropped scan: %v %v", ok, err)
	}
}
