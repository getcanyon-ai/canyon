package runs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

const (
	PatchesQueue      = "patches"    // patch runs (design/PATCHING.md §4.3), PatchConcurrency at a time across the replicas
	ImageQueue        = "image-sbom" // the image scans after a deploy or a tick (§3.1), one at a time across the replicas, never behind a patch run
	ImageSBOMWorkflow = "image.sbom" // one per attempt: a project's image is read until a scan of it finishes
	platformRule      = "platform.known-vuln"
	depsRule          = "deps.known-vuln"
	maxStored         = 10 << 20 // an SBOM or a report, as the executor keeps a tool's output (contracts/executor.md)
	maxLayers         = 128      // as the executor bounds an image (contracts/executor.md, target image)
	maxOS             = 64 << 10 // the operating-system component, copied beside every layer's slice
)

// busyFor is how long a scan waits on a busy executor in all; a variable so a test can lower it.
var busyFor = 15 * time.Minute

// imageScan is image.sbom's input: the image a deploy run or a rescan tick asked about, the project it runs
// in, which the scan is stored under — never anything a caller names — and who asked.
type imageScan struct {
	Run     string `json:"run"`
	Project string `json:"project"`
	Image   string `json:"image"`
}

// ImageDigest is the sha256 digest of an image named by it, "" for any other name: no scan names one.
func ImageDigest(image string) string {
	_, digest, _ := strings.Cut(image, "@")
	if hex, ok := strings.CutPrefix(digest, "sha256:"); !ok || len(hex) != 64 || strings.Trim(hex, "0123456789abcdef") != "" {
		return ""
	}
	return digest
}

// ReadsImages is whether a snapshot has platform.known-vuln on: off, nothing is scanned or matched for it
// to act on (design/PATCHING.md §3.3).
func ReadsImages(snap policy.Snapshot) bool {
	return slices.ContainsFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == platformRule && r.State != policy.Off })
}

// readsBuiltApp is whether a snapshot places deps.known-vuln before production, whose row there counts what only
// the image holds (design/PATCHING.md §3.6): its images are scanned, and their app layers matched too.
func readsBuiltApp(snap policy.Snapshot) bool {
	return slices.ContainsFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == depsRule && r.Production != "" && r.Production != policy.Off })
}

// scansImages is whether Canyon scans a snapshot's images: a rule reads them (§3.1).
func scansImages(snap policy.Snapshot) bool { return ReadsImages(snap) || readsBuiltApp(snap) }

// readsApp is whether project's images' app layers are matched: its deps.known-vuln reads the built app.
func (p *Pipeline) readsApp(ctx context.Context, project string) (bool, error) {
	policyOf := p.ProjectPolicy.Load()
	if policyOf == nil {
		return false, nil
	}
	snap, err := policyOf(ctx, p.Pool, project)
	return err == nil && readsBuiltApp(snap), err
}

// askImage is a deploy run's last step before its end (design/PATCHING.md §3.1): image.sbom of the image
// it started, under the run's project. Each run asks anew, and a scan of the image that finished already
// is all the workflow reads, so only a finished scan is never asked again. A failed ask is logged and the
// deploy goes on: no scan stored reads no image scan recorded, which never passes, and the next rescan
// tick asks again.
func (p *Pipeline) askImage(ctx dbos.Context, in Input, image string) error {
	digest := ImageDigest(image)
	if digest == "" || !scansImages(in.Policy) {
		return nil
	}
	_, err := dbos.Enqueue[any](ctx, ImageQueue, ImageSBOMWorkflow, imageScan{Run: in.Run, Project: in.Project, Image: image},
		dbos.WithEnqueueWorkflowID(imageScanID(in.Project, digest, in.Run)))
	if err != nil && ctx.Err() == nil && !errors.Is(err, dbos.ErrWorkflowCancelled) {
		log.Printf("runs: run %s (%s): its image's SBOM was not asked for: %v", in.Run, in.Project, err)
		return nil
	}
	return err
}

// imageScanID is one attempt at a project's image: the run or the rescan tick that asked.
func imageScanID(project, digest, attempt string) string {
	return project + "@" + digest + "@" + attempt
}

// imageSBOM is the image.sbom workflow: the executor's SBOM of the image, stored whole or not at all, then
// its layers matched at once, so production waits the scan's length, never a rescan interval (§3.1). A
// project's image whose scan finished already is not read again.
func (p *Pipeline) imageSBOM(ctx dbos.Context, in imageScan) (string, error) {
	digest := ImageDigest(in.Image)
	stored, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
		finished, err := standOn(c, p.Pool, in.Project, digest)
		if err != nil || finished {
			return false, err
		}
		res, err := p.askScan(c, executor.ScanRequest{Tool: "trivy", Target: "image", App: in.Project, Image: in.Image}, nil)
		if err != nil {
			return false, err
		}
		img, err := imageOf(res)
		if err != nil {
			return false, err
		}
		return img.NotFinished == "", storeImage(c, p.Pool, in.Project, digest, img)
	}, dbos.WithStepName("sbom"))
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("runs: the SBOM of %s for %s, asked by %s, stores nothing: %v", in.Image, in.Project, in.Run, err)
		}
		return "", err
	}
	// a scan stored now matches its layers at once; one that finished before has the layers no match names yet
	// matched — those deps.known-vuln, placed since, reads (design/PATCHING.md §3.6) — and the rest left to the tick
	_, err = dbos.RunAsStep(ctx, func(c context.Context) ([]string, error) {
		app, err := p.readsApp(c, in.Project)
		if err != nil {
			return nil, err
		}
		img, err := p.toMatch(c, in.Project, digest, app)
		if err == nil && img != nil && !stored {
			if img.Layers, err = unmatched(c, p.Pool, img.Layers); err == nil {
				img.AppLayers, err = unmatched(c, p.Pool, img.AppLayers)
			}
		}
		if err != nil || img == nil || len(img.Layers) == 0 && len(img.AppLayers) == 0 {
			return nil, err
		}
		return p.matchImage(c, *img, nil)
	}, dbos.WithStepName("match"))
	if err != nil && ctx.Err() == nil {
		log.Printf("runs: the first match of %s for %s: %v", in.Image, in.Project, err)
	}
	return "", nil
}

// unmatched is the layers of layers whose slice no match names yet.
func unmatched(ctx context.Context, pool *pgxpool.Pool, layers []rescanLayer) ([]rescanLayer, error) {
	parts := make([]string, 0, len(layers))
	for _, l := range layers {
		parts = append(parts, l.Part)
	}
	rows, err := pool.Query(ctx, "SELECT DISTINCT part_sha FROM part_matches WHERE part_sha = ANY($1)", parts)
	if err != nil {
		return nil, err
	}
	matched, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(layers), func(l rescanLayer) bool { return slices.Contains(matched, l.Part) }), nil
}

// AskBuiltApps asks image.sbom, as a deploy asks it, of every image a live project runs or keeps — of projects
// alone when it names any — while the project's deps.known-vuln reads the built app: an apply that places the rule
// before production reads its images at once, never a rescan interval later (design/PATCHING.md §3.6). An image
// whose scan finished has its unmatched layers matched. by names who asked, in each workflow's id.
func (p *Pipeline) AskBuiltApps(ctx context.Context, dc dbos.Client, projects []string, by string) (int, error) {
	policyOf := p.ProjectPolicy.Load()
	if policyOf == nil {
		return 0, nil
	}
	rows, err := p.Pool.Query(ctx, liveImages+" ORDER BY 1, 2")
	if err != nil {
		return 0, err
	}
	var live []imageScan
	var project, image string
	if _, err := pgx.ForEachRow(rows, []any{&project, &image}, func() error {
		if ImageDigest(image) != "" && (len(projects) == 0 || slices.Contains(projects, project)) {
			live = append(live, imageScan{Run: by, Project: project, Image: image})
		}
		return nil
	}); err != nil {
		return 0, err
	}
	reads, asked := map[string]bool{}, 0
	for _, in := range live {
		on, known := reads[in.Project]
		if !known {
			snap, err := policyOf(ctx, p.Pool, in.Project)
			if err != nil {
				return asked, err
			}
			on = readsBuiltApp(snap)
			reads[in.Project] = on
		}
		if !on {
			continue
		}
		if _, err := dbos.Enqueue[any](dc, ImageQueue, ImageSBOMWorkflow, in, dbos.WithEnqueueWorkflowID(imageScanID(in.Project, ImageDigest(in.Image), by))); err != nil {
			return asked, err
		}
		asked++
	}
	return asked, nil
}

// standOn is whether project's finished scan of digest is stored, marking it seen now if so: a tick's sweep
// that began before never drops it (retain), and one that dropped it already leaves none, so the image is
// read again.
func standOn(ctx context.Context, pool *pgxpool.Pool, project, digest string) (bool, error) {
	err := pool.QueryRow(ctx, "UPDATE scanned_images SET seen_at = now() WHERE project_id = $1 AND digest = $2 AND not_finished IS NULL RETURNING true",
		project, digest).Scan(new(bool))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// storedImage is what one image answer stores: its scan, and each layer's slice bottom first.
type storedImage struct {
	NotFinished, Reason string
	DiffIDs             []string
	Lifecycle           *string
	Unplaced            json.RawMessage
	Layers              []storedLayer
}

// storedLayer is one layer of an image as that image's own SBOM listed it: its slice, stored once under
// the slice's sha256, which the scan names for the diff ID (contracts/finding.md, System libraries and build tools).
type storedLayer struct {
	DiffID     string
	Part       string          // sha256 of the slice as the re-match sends it
	OS         json.RawMessage // the image's operating-system component, which trivy needs to match its packages; nil without one
	Components json.RawMessage
}

// imageOf is what an image answer stores (contracts/finding.md, System libraries and build tools): the executor's
// not_finished as it said it, or every layer's components, each under the layer its LayerDiffID names, the
// image's operating-system component beside each, and the rest unplaced. Each slice is in its canonical
// form, the bom-refs trivy draws at random on each scan made stable, so one layer's slice is the same bytes
// in every image that holds it, and keyed by its sha256. An image past what Canyon keeps is over_cap. An
// answer any part of which Canyon's read would refuse is an error: nothing of it is stored, never a scan
// that reads as half the image.
func imageOf(res executor.ScanResult) (storedImage, error) {
	if res.NotFinished != "" {
		return storedImage{NotFinished: res.NotFinished, Reason: schedulerSaid(res.Reason)}, nil
	}
	if len(res.SBOM) > maxStored {
		return storedImage{NotFinished: "over_cap", Reason: "report"}, nil
	}
	if len(res.DiffIDs) > maxLayers {
		return storedImage{NotFinished: "over_cap", Reason: "image"}, nil
	}
	var sbom struct {
		Format     string            `json:"bomFormat"`
		Components []json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal(res.SBOM, &sbom); err != nil || sbom.Format != "CycloneDX" {
		return storedImage{}, fmt.Errorf("an answer that is no CycloneDX SBOM (%v)", err)
	}
	if len(res.DiffIDs) == 0 || !trivy.DiffIDsRead(res.DiffIDs) {
		return storedImage{}, errors.New("diff IDs that do not read")
	}
	stable, err := stableRefs(sbom.Components)
	if err != nil {
		return storedImage{}, err
	}
	var system json.RawMessage
	layers, unplaced := map[string][]json.RawMessage{}, []json.RawMessage{}
	for _, raw := range stable {
		var c struct {
			trivy.Component
			Nested json.RawMessage `json:"components"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Nested != nil {
			return storedImage{}, fmt.Errorf("a component that is not trivy's (%v)", err)
		}
		switch layer := c.Prop(trivy.LayerDiffID); {
		case c.Type == "operating-system" && system != nil:
			return storedImage{}, errors.New("two operating-system components")
		case c.Type == "operating-system" && len(raw) > maxOS:
			return storedImage{NotFinished: "over_cap", Reason: "report"}, nil
		case c.Type == "operating-system":
			system = raw
		case c.Type != "library" && c.Type != "application":
			return storedImage{}, fmt.Errorf("a component of type %q, which trivy does not write", c.Type)
		case slices.Contains(res.DiffIDs, layer):
			layers[layer] = append(layers[layer], raw)
		default:
			unplaced = append(unplaced, raw)
		}
	}
	img := storedImage{DiffIDs: res.DiffIDs}
	if res.Lifecycle != "" {
		img.Lifecycle = &res.Lifecycle
	}
	if img.Unplaced, err = json.Marshal(unplaced); err != nil {
		return storedImage{}, err
	}
	if _, ok := trivy.DecodeComponents(img.Unplaced); !ok {
		return storedImage{}, errors.New("a component without its bom-ref, type or name")
	}
	for _, d := range res.DiffIDs {
		raw, err := json.Marshal(append([]json.RawMessage{}, layers[d]...)) // a layer holding no package is stored with none
		if err != nil {
			return storedImage{}, err
		}
		if cs, ok := trivy.DecodeComponents(raw); !ok || !trivy.StoredIn(cs, d) {
			return storedImage{}, errors.New("a component without its bom-ref, type or name")
		}
		part, err := sbomPart(system, raw)
		if err != nil {
			return storedImage{}, err
		}
		img.Layers = append(img.Layers, storedLayer{DiffID: d, Part: partSHA(part), OS: system, Components: raw})
	}
	return img, nil
}

func partSHA(part []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(part)) }

// canonical is a JSON value with its object keys sorted, compact and its numbers as written: the one form
// a slice is hashed, stored and sent in, whatever stored it in between.
func canonical(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// stableRefs is each component in its canonical form, its bom-ref the purl it carries, as trivy writes it
// for a package of a unique purl; any other bom-ref, which trivy draws at random on each scan (an
// operating system, a package without a purl or of a repeated one), becomes one derived from the
// component's content, the n-th of identical components its n-th. A component without a bom-ref keeps none,
// and the read refuses it.
func stableRefs(components []json.RawMessage) ([]json.RawMessage, error) {
	seen := map[string]int{}
	out := make([]json.RawMessage, 0, len(components))
	for _, raw := range components {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var c map[string]any
		if err := dec.Decode(&c); err != nil || c == nil {
			return nil, fmt.Errorf("a component that is not trivy's (%v)", err)
		}
		if ref, _ := c["bom-ref"].(string); ref != "" && ref != c["purl"] {
			delete(c, "bom-ref")
			body, err := json.Marshal(c)
			if err != nil {
				return nil, err
			}
			ref = "canyon:" + partSHA(body)
			if seen[ref]++; seen[ref] > 1 {
				ref = fmt.Sprintf("%s-%d", ref, seen[ref])
			}
			c["bom-ref"] = ref
		}
		b, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// storePart stores a slice, or marks the one stored as seen, so a sweep under way never drops it (retain).
const storePart = "INSERT INTO layer_parts (part_sha, os, components) VALUES ($1, $2, $3) ON CONFLICT (part_sha) DO UPDATE SET seen_at = now()"

// storeImage writes a project's scan of an image and its layers in one transaction: all of them or none.
// A scan that did not finish is replaced by the next; a finished one never is. A slice another scan stored
// already is the same bytes, kept as it is. Each layer is the scan's own: another image's view of the same
// diff ID, or another project's scan of the same digest, is never read for it.
func storeImage(ctx context.Context, pool *pgxpool.Pool, project, digest string, img storedImage) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var diffIDs []string // NULL beside not_finished
		if img.NotFinished == "" {
			diffIDs = img.DiffIDs
		}
		var wrote bool
		err := tx.QueryRow(ctx, `INSERT INTO scanned_images (project_id, digest, scanned_at, diff_ids, lifecycle, unplaced, not_finished, reason)
			VALUES ($1, $2, now(), $3, $4, coalesce($5::jsonb, '[]'), nullif($6, ''), nullif($7, ''))
			ON CONFLICT (project_id, digest) DO UPDATE SET scanned_at = EXCLUDED.scanned_at, diff_ids = EXCLUDED.diff_ids, lifecycle = EXCLUDED.lifecycle,
				unplaced = EXCLUDED.unplaced, not_finished = EXCLUDED.not_finished, reason = EXCLUDED.reason, seen_at = now()
			WHERE scanned_images.not_finished IS NOT NULL RETURNING true`,
			project, digest, diffIDs, img.Lifecycle, img.Unplaced, img.NotFinished, img.Reason).Scan(&wrote)
		if errors.Is(err, pgx.ErrNoRows) { // a finished scan of it is stored: it stands
			return nil
		}
		if err != nil {
			return err
		}
		for _, l := range img.Layers {
			if _, err := tx.Exec(ctx, storePart, l.Part, l.OS, l.Components); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO image_layers (project_id, digest, diff_id, part_sha) VALUES ($1, $2, $3, $4) ON CONFLICT (project_id, digest, diff_id) DO NOTHING",
				project, digest, l.DiffID, l.Part); err != nil {
				return err
			}
		}
		return nil
	})
}

// askScan sends one image or sbom scan (contracts/executor.md, POST /scan): the json part, then part sbom
// when there is one. A busy executor is asked again once its Retry-After has passed — a second at least —
// for up to busyFor in all, then the call is an error; a call past scanCeiling did not finish.
func (p *Pipeline) askScan(ctx context.Context, req executor.ScanRequest, sbom []byte) (executor.ScanResult, error) {
	if p.Executor == nil {
		return executor.ScanResult{}, errors.New("runs: no executor configured")
	}
	until := time.Now().Add(busyFor)
	for {
		res, err := p.scanOnce(ctx, req, sbom)
		var busy *executor.Busy
		if !errors.As(err, &busy) {
			return res, err
		}
		wait := max(busy.RetryAfter, time.Second)
		if time.Now().Add(wait).After(until) {
			return executor.ScanResult{}, fmt.Errorf("executor: /scan %s busy for %s", req.Target, busyFor)
		}
		select {
		case <-ctx.Done():
			return executor.ScanResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (p *Pipeline) scanOnce(ctx context.Context, req executor.ScanRequest, sbom []byte) (executor.ScanResult, error) {
	ctx, cancel := context.WithTimeout(ctx, scanCeiling)
	defer cancel()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormField("json")
	if err == nil {
		err = json.NewEncoder(part).Encode(req)
	}
	if err == nil && sbom != nil {
		if part, err = mw.CreateFormFile("sbom", "sbom.cdx.json"); err == nil {
			_, err = part.Write(sbom)
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return executor.ScanResult{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Executor.Base+"/scan", &body)
	if err != nil {
		return executor.ScanResult{}, err
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	client := p.Executor.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return executor.ScanResult{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode == http.StatusServiceUnavailable && err == nil && s >= 0 {
		return executor.ScanResult{}, &executor.Busy{RetryAfter: time.Duration(s) * time.Second}
	}
	if resp.StatusCode != http.StatusOK { // the executor's own words alone, quoted as a scheduler's are: never a body it echoed
		var said struct{ Error, Message string }
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&said)
		return executor.ScanResult{}, fmt.Errorf("executor: /scan %s answered %d: %s", req.Target, resp.StatusCode, schedulerSaid(strings.TrimSpace(said.Error+" "+said.Message)))
	}
	var res executor.ScanResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 3*maxStored)).Decode(&res); err != nil {
		return executor.ScanResult{}, fmt.Errorf("executor: /scan %s: %w", req.Target, err)
	}
	return res, nil
}
