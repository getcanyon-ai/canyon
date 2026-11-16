//go:build !noee

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// envPatchOf is envs.<env>.patch on a read's envs, nil when the environment carries none.
func envPatchOf(envs any, env string) map[string]any {
	e, _ := envs.(map[string]any)[env].(map[string]any)
	p, _ := e["patch"].(map[string]any)
	return p
}

// needs_you counts an open patch approval (contracts/routes.md, the fleet read): before Canyon asks, claims-portal is in
// nobody's queue; once a tick files its patch approval it is in a contributor's and the admin's, never in a read-only
// member's, whose row is reduced; once production moves, the approval is no longer open and it leaves the queue.
func TestFleetNeedsYouCountsPatchApproval(t *testing.T) {
	pool, ctx, base, browser, _, deploy := patchHost(t)
	carl := teamMember(t, ctx, pool, base, "sub-carl", "carl@meridian.example", "contributor")
	vic := teamMember(t, ctx, pool, base, "sub-vic", "vic@meridian.example", "viewer")
	queue := base + "/api/v1/fleet?needs_you=true"
	for who, c := range map[string]*http.Client{"the admin": browser, "a contributor": carl, "a read-only member": vic} {
		if ids := fleetIDs(t, c, queue); slices.Contains(ids, "claims-portal") {
			t.Fatalf("%s's queue holds claims-portal before Canyon asks: %v", who, ids)
		}
	}
	passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	patchCode(t, ctx, pool)
	for who, c := range map[string]*http.Client{"the admin": browser, "a contributor": carl} {
		if ids := fleetIDs(t, c, queue); !slices.Contains(ids, "claims-portal") {
			t.Fatalf("%s's queue lacks claims-portal with its patch approval open: %v", who, ids)
		}
	}
	if ids := fleetIDs(t, vic, queue); slices.Contains(ids, "claims-portal") {
		t.Fatalf("a read-only member's queue holds claims-portal: %v", ids)
	}
	if _, err := pool.Exec(ctx, "UPDATE deploys SET image = 'canyon/claims-portal@sha256:' || repeat('7b', 32) WHERE project_id = 'claims-portal' AND env = 'prod'"); err != nil {
		t.Fatal(err)
	}
	if ids := fleetIDs(t, carl, queue); slices.Contains(ids, "claims-portal") {
		t.Fatalf("production moved, so the approval is outdated, yet the queue holds claims-portal: %v", ids)
	}
}

// envs.<env>.patch on the reads (contracts/routes.md, Patch runs): a contributor reads preview patched_preview on the
// project, status and fleet reads, then production asked naming the approval's code; approved, production runs the
// patch and reads patched, and its image read's patched[] names the fix, found by the image, at when it reached
// production. A read-only member's rows carry no patch.
func TestEnvPatchOnTheReads(t *testing.T) {
	pool, ctx, base, _, _, deploy := patchHost(t)
	carl := teamMember(t, ctx, pool, base, "sub-carl", "carl@meridian.example", "contributor")
	vic := teamMember(t, ctx, pool, base, "sub-vic", "vic@meridian.example", "viewer")
	var replaced string
	if err := pool.QueryRow(ctx, "SELECT split_part(image, '@', 2) FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'").Scan(&replaced); err != nil {
		t.Fatal(err)
	}
	image := passedPatch(t, ctx, pool, deploy, "gr_patcha", "6a")
	digest := image[strings.Index(image, "@")+1:]
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET detail = detail || jsonb_build_object('image', $1::text, 'replaces', $2::text) WHERE run_id = 'gr_patcha' AND action = 'patch preview'`, digest, replaced); err != nil {
		t.Fatal(err)
	}
	reads := func(c *http.Client) map[string]any {
		_, project, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal", "")
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		_, fleet, _ := do(t, c, http.MethodGet, base+"/api/v1/fleet", "")
		var row map[string]any
		for _, r := range fleet["apps"].([]any) {
			if r.(map[string]any)["id"] == "claims-portal" {
				row = r.(map[string]any)
			}
		}
		return map[string]any{"project": project["project"].(map[string]any)["envs"], "status": status["envs"], "fleet": row["envs"]}
	}
	for read, envs := range reads(carl) {
		p := envPatchOf(envs, "preview")
		if p["state"] != "patched_preview" || p["run"] != "gr_patcha" || p["identity"] != patchIdentity {
			t.Fatalf("the %s read's preview: %s", read, mustJSON(p))
		}
		patchShape(t, "env_patch", p)
		want := "none"
		if read == "status" { // no scan of production's image is stored, which the status read alone reads
			want = "could_not_rescan"
		}
		if p := envPatchOf(envs, "prod"); p["state"] != want {
			t.Fatalf("the %s read's production before Canyon asks: %s", read, mustJSON(p))
		}
	}
	for read, envs := range reads(vic) {
		if envs != nil && (envPatchOf(envs, "preview") != nil || envPatchOf(envs, "prod") != nil) {
			t.Fatalf("a read-only member's %s read carries a patch: %s", read, mustJSON(envs))
		}
	}
	code := patchCode(t, ctx, pool)
	for read, envs := range reads(carl) {
		p := envPatchOf(envs, "prod")
		if p["state"] != "asked" || p["approval"] != code[:4]+"-"+code[4:] || p["run"] != "gr_patcha" {
			t.Fatalf("the %s read's production while asked: %s", read, mustJSON(p))
		}
		patchShape(t, "env_patch", p)
	}
	owner := teamMember(t, ctx, pool, base, "sub-olga", "olga@meridian.example", "owner")
	got, started, _ := do(t, owner, http.MethodPost, base+approvalsAt+code+"/approve", "")
	run, _ := runOf(started)["id"].(string)
	if got != 202 || waitVerdict(t, owner, base, run)["state"] != "clean" {
		t.Fatalf("an owner approves the patch: %d %s", got, lastBody)
	}
	var reached string
	if err := pool.QueryRow(ctx, "SELECT to_char(deployed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"') FROM deploys WHERE project_id = 'claims-portal' AND env = 'prod'").Scan(&reached); err != nil {
		t.Fatal(err)
	}
	for read, envs := range reads(carl) {
		if p := envPatchOf(envs, "prod"); p["state"] != "patched" || p["since"] != reached || p["run"] != "gr_patcha" {
			t.Fatalf("the %s read's production once the patch is live: %s", read, mustJSON(p))
		}
	}
	_, img, _ := do(t, carl, http.MethodGet, base+"/api/v1/projects/claims-portal/envs/prod/image", "")
	want := `[{"at":"` + reached + `","class":"os","from":"3.0.13-0ubuntu3.4","name":"libssl3","run":"gr_patcha","to":"3.0.13-0ubuntu3.5","vulnerability":"CVE-2024-5535"}]`
	if string(mustJSON(img["patched"])) != want {
		t.Fatalf("production's image read names the patch that made its image: %s", mustJSON(img["patched"]))
	}
	patchShape(t, "patched", img["patched"])
	if _, before, _ := do(t, carl, http.MethodGet, base+"/api/v1/projects/claims-portal/envs/preview/image", ""); string(mustJSON(before["patched"])) == "[]" {
		t.Fatalf("preview runs the patch too: %s", lastBody)
	}
}

// wait and could_not_rescan are the status read's alone (contracts/routes.md, envs.<env>.patch): with no image scan
// stored, preview and production read could_not_rescan there and none on the project read; scanned with libssl3's
// high os finding and no run image read, still could_not_rescan, never wait; once the run image Canyon names is read
// and holds the same libssl3, both read wait, and the admin's identities read's run_image says one app is behind and
// names it, which the app's contributor is refused. It says null, Canyon can't tell, while an app reads
// could_not_rescan, and while the operator's or the org's patching is off.
func TestStatusReadsWaitAndCouldNotRescan(t *testing.T) {
	pool, ctx, base, _, cli, _ := patchHost(t)
	carl := teamMember(t, ctx, pool, base, "sub-carl", "carl@meridian.example", "contributor")
	states := func(path string) [2]any {
		_, body, _ := do(t, carl, http.MethodGet, base+"/api/v1/projects/claims-portal"+path, "")
		envs := body["envs"]
		if path == "" {
			envs = body["project"].(map[string]any)["envs"]
		}
		return [2]any{envPatchOf(envs, "preview")["state"], envPatchOf(envs, "prod")["state"]}
	}
	if got := states("/status"); got != [2]any{"could_not_rescan", "could_not_rescan"} {
		t.Fatalf("no image scan stored: the status read says %v", got)
	}
	if got := states(""); got != [2]any{"none", "none"} {
		t.Fatalf("the project read never reads could_not_rescan: %v", got)
	}
	g := loadPlatformGolden(t)
	var digest string
	if err := pool.QueryRow(ctx, "SELECT split_part(image, '@', 2) FROM deploys WHERE project_id = 'claims-portal' AND env = 'preview'").Scan(&digest); err != nil {
		t.Fatal(err)
	}
	seedPlatform(t, ctx, pool, g, "claims-portal", digest)
	if got := states("/status"); got != [2]any{"could_not_rescan", "could_not_rescan"} {
		t.Fatalf("no run image read: an os finding is never read as wait: %v", got)
	}
	if _, body, _ := do(t, cli, http.MethodGet, base+identitiesAt, ""); body["run_image"] != nil {
		t.Fatalf("no run image read: the identities read can't tell: %s", lastBody)
	}
	// the run image Canyon names is the golden image's own two run-image layers, libssl3 3.0.13-0ubuntu3.4 among them
	layers, _ := g.layered(t)
	run := "sha256:" + strings.Repeat("9d", 32)
	if _, err := pool.Exec(ctx, "INSERT INTO run_image_scans (digest, run_image, scanned_at, diff_ids, rematched_at) VALUES ($1, 'canyon-run:noble', now(), $2, now())",
		run, g.Image.DiffIDs[:2]); err != nil {
		t.Fatal(err)
	}
	for _, d := range g.Image.DiffIDs[:2] {
		part := fmt.Sprintf("sha256:%x", sha256.Sum256(mustJSON(append([]json.RawMessage{}, layers[d]...))))
		if _, err := pool.Exec(ctx, "INSERT INTO run_image_layers (digest, diff_id, part_sha) VALUES ($1, $2, $3)", run, d, part); err != nil {
			t.Fatal(err)
		}
	}
	if got := states("/status"); got != [2]any{"wait", "wait"} {
		t.Fatalf("the run image Canyon names carries no fix: the status read says %v", got)
	}
	if got := states(""); got != [2]any{"none", "none"} {
		t.Fatalf("the project read never reads wait: %v", got)
	}
	_, body, _ := do(t, cli, http.MethodGet, base+identitiesAt, "")
	if string(mustJSON(body["run_image"])) != `{"behind":1,"image":"canyon-run:noble","projects":["claims-portal"]}` {
		t.Fatalf("the admin's identities read names the app behind: %s", lastBody)
	}
	patchShape(t, "identities_read", body)
	if got, _, _ := do(t, carl, http.MethodGet, base+identitiesAt, ""); got != 403 || lastBody != adminRequired { // the app's own contributor reads none of it
		t.Fatalf("a contributor reads the identities: %d %s", got, lastBody)
	}
	runImage := func() string {
		_, body, _ := do(t, cli, http.MethodGet, base+identitiesAt, "")
		return string(mustJSON(body["run_image"]))
	}
	// an app Canyon could not look at says nothing of how many are behind
	if _, err := pool.Exec(ctx, "UPDATE scanned_images SET rescan_failed_at = now(), rescan_failed = 'timeout' WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	if got := states("/status"); got != [2]any{"could_not_rescan", "could_not_rescan"} {
		t.Fatalf("a rescan that did not finish: the status read says %v", got)
	}
	if got := runImage(); got != "null" {
		t.Fatalf("an app that reads could_not_rescan: the identities read's run_image is %s, want null", got)
	}
	if _, err := pool.Exec(ctx, "UPDATE scanned_images SET rescan_failed_at = NULL, rescan_failed = NULL WHERE project_id = 'claims-portal'"); err != nil {
		t.Fatal(err)
	}
	// the operator's patching off: Canyon looks at nothing, so it can't tell
	pipe.Patches.Store(false)
	if got := runImage(); got != "null" {
		t.Fatalf("CANYON_PATCHING off: the identities read's run_image is %s, want null", got)
	}
	pipe.Patches.Store(true)
	if _, err := pool.Exec(ctx, "UPDATE org_preferences SET patching = 'off'"); err != nil {
		t.Fatal(err)
	}
	if got := states("/status"); got != [2]any{"none", "none"} {
		t.Fatalf("patching off reads none: %v", got)
	}
	if got := runImage(); got != "null" {
		t.Fatalf("the org's patching off: the identities read's run_image is %s, want null", got)
	}
}
