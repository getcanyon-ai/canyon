//go:build oidc && patch

package oidc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The rebase patch end to end on Docker (design/PATCHING.md §4.8, #2108), on the stack make e2e-up OIDC=1 started:
// patching needs sign-in on (§4.5), so it is the team setup's. The journey turns patching on itself
// (e2e/compose.patch.yml) and moves CANYON_IMAGE_RUN between two digests of one repository, in a registry of its own.
const (
	registry = "localhost:5000" // the daemon pulls a run image by digest only from a registry; loopback's is one it trusts over http
	runRepo  = registry + "/canyon-e2e-run"
	marker   = "/etc/canyon-e2e-exit" // the patched run image carries it; a fragile app exits 45 s after it starts on one that does
	patchFor = 40 * time.Minute       // the three patch runs one at a time, each its gate, rebase, SBOM and a minute's watch, and production's watch
)

const busybox = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e" // the executor's pin

// oldRun is the Paketo run image the journey starts on, in place of run.Dockerfile's own: an older one, pinned here
// apart from the file's, which the Paketo bump moves to Paketo's newest (.github/workflows/paketo-bump.yml). The
// newest carries every Ubuntu update there is, so an app on it has no os finding for a newer run image to fix. The
// bump never moves this pin (internal/tools/paketobump, apart).
const oldRun = "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:1fc3397ff5fb46923b8dd91548b9196ecc7d56fd7ea79d9814cef8e3bcc3450a"

// TestRebasePatchToProductionAndARevert: three apps on a run image, then CANYON_IMAGE_RUN moved to a newer digest of
// it with every Ubuntu update and the marker. The canary, the first app in canary order, ignores the marker: its
// preview is patched with only the run image changed, its watch passes, and the patch approval Canyon files is
// approved, so production runs preview's digest. The other two, in the next wave, exit 45 s after their patched
// start: Canyon puts back the image each replaced and records reverted, with a revert patch preview row, and the
// second revert pauses the identity naming both.
func TestRebasePatchToProductionAndARevert(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	docker(t, "run", "-d", "--rm", "--name", "canyon-e2e-registry", "-p", "127.0.0.1:5000:5000",
		"registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "canyon-e2e-registry").Run() })
	before := runImage(t, "before", "")
	after := runImage(t, "after", "FROM "+before+"\nUSER root\nRUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get -y upgrade && rm -rf /var/lib/apt/lists/* && touch "+marker+"\nUSER 1002:1001\n")
	patchingOn(t, before, true)

	browser := signIn(t, admin)
	send(t, browser, http.MethodPut, host+"/api/v1/preferences", map[string]any{"onboarded_at": time.Now().UTC().Format(time.RFC3339)}, http.StatusOK)
	send(t, browser, http.MethodPut, host+"/api/v1/policies/platform.known-vuln?apply=true", map[string]any{"state": "advisory"}, http.StatusOK)

	// the canary is the first in canary order, a hash of the app's id (runs/waves.go): it ignores the marker
	tag := suffix()
	apps := []string{"patch-a-" + tag, "patch-b-" + tag, "patch-c-" + tag}
	slices.SortFunc(apps, func(x, y string) int { return strings.Compare(canaryOrder(x), canaryOrder(y)) })
	steady, fragile := apps[0], apps[1:]
	home := t.TempDir()
	for i, app := range apps {
		source := patchSource(t, app, app != steady)
		send(t, browser, http.MethodPost, host+"/api/v1/projects", map[string]string{"id": app}, http.StatusCreated)
		if i == 0 {
			login(t, command(t, home, source, "login", host), browser)
		}
		if out, code := run(t, command(t, home, source, "deploy")); code != 0 {
			t.Fatalf("canyon deploy of %s exited %d:\n%s", app, code, out)
		}
		if app == steady {
			if out, code := run(t, command(t, home, source, "deploy", "--env", "prod", "--yes")); code != 0 {
				t.Fatalf("canyon deploy --env prod of %s exited %d:\n%s", app, code, out)
			}
		}
	}

	// every preview's image read checked before the move, so the first plan names all three and the canary is steady's
	was := map[string]string{}
	for _, app := range apps {
		read := waitFor(t, 10*time.Minute, app+"'s preview image read checked", func() (imageRead, bool) {
			r := image(t, browser, app, "preview")
			return r, r.State == "checked"
		})
		was[app] = read.ImageDigest
		inOS := 0
		for _, f := range read.Findings {
			if f.Class == "os" {
				inOS++
			}
		}
		if inOS == 0 {
			t.Fatalf("%s's preview on %s has no os finding for the newer run image to fix: %+v", app, before, read)
		}
	}
	waitFor(t, 5*time.Minute, steady+"'s production on preview's image", func() (string, bool) {
		d := image(t, browser, steady, "prod").ImageDigest
		return d, d == was[steady]
	})

	// a stale vulnerability database (design/PATCHING.md §3.5, #2081): each tick's re-match fails, so every preview's
	// platform row reads could not check, its patch state could_not_rescan, and Settings › Patching shows the admin why
	// Canyon patched none of them. Then the database back: the next tick re-matches each from its stored layers, and
	// the daemon the executor drives saved no image the while, so the rescan read none.
	since := time.Now()
	restore := staleDB(t)
	for _, app := range apps {
		waitFor(t, 5*time.Minute, app+"'s preview could not check", func() (imageRead, bool) {
			r := image(t, browser, app, "preview")
			return r, r.State == "could_not_check" && r.Why == "could not check: its last rescan did not finish"
		})
		if p := status(t, browser, app).Envs.Preview.Patch; p.State != "could_not_rescan" {
			t.Errorf("%s's preview patch on a stale database: %+v, want could_not_rescan", app, p)
		}
	}
	waitFor(t, 3*time.Minute, "the plan's cause for every app, unread", func() (planRead, bool) {
		var ids struct{ Plan planRead }
		if code := get(t, browser, host+"/api/v1/patching/identities", &ids); code != http.StatusOK {
			t.Fatalf("the identities: %d", code)
		}
		unread := 0
		for _, a := range ids.Plan.Apps {
			if slices.Contains(apps, a.Project) && a.Why == "unread" {
				unread++
			}
		}
		return ids.Plan, unread == len(apps)
	})
	patchingSays(t, browser, apps)
	restore()
	for _, app := range apps {
		waitFor(t, 5*time.Minute, app+"'s preview checked again", func() (imageRead, bool) {
			r := image(t, browser, app, "preview")
			return r, r.State == "checked" && r.ImageDigest == was[app]
		})
	}
	if saved := docker(t, "events", "--since", strconv.FormatInt(since.Unix(), 10), "--until", strconv.FormatInt(time.Now().Unix(), 10),
		"--filter", "type=image", "--filter", "event=save", "--format", "{{.Actor.ID}}"); saved != "" {
		t.Fatalf("the rescans saved images, reading what they re-match from stored layers alone:\n%s", saved)
	}

	patchingOn(t, after, false)

	// the canary: only the run image changed, the watch passed, production asked
	code := waitFor(t, patchFor, steady+"'s patch approval", func() (envPatch, bool) {
		s := status(t, browser, steady)
		return s.Envs.Prod.Patch, s.Envs.Prod.Patch.State == "asked"
	}).Approval
	patched := image(t, browser, steady, "preview")
	if patched.ImageDigest == was[steady] || !slices.ContainsFunc(patched.Patched, func(f fixed) bool { return f.Class == "os" && f.From != f.To }) {
		t.Fatalf("%s's patched preview: %s, was %s, patched %+v", steady, patched.ImageDigest, was[steady], patched.Patched)
	}
	t.Logf("%s's patch fixed %+v", steady, patched.Patched)
	lines := runLines(t, browser, status(t, browser, steady).Envs.Preview.Patch.Run)
	for _, want := range []string{"Only the system libraries Canyon adds changed: ", "The watch passed: the patched app neither exited nor restarted"} {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("no %q on %s's patch run: %q", want, steady, lines)
		}
	}
	send(t, browser, http.MethodPost, host+"/api/v1/promotion-approvals/"+code+"/approve", nil, http.StatusAccepted)
	waitFor(t, 10*time.Minute, steady+"'s production patched", func() (envPatch, bool) {
		p := status(t, browser, steady).Envs.Prod.Patch
		return p, p.State == "patched"
	})
	if prod := image(t, browser, steady, "prod").ImageDigest; prod != patched.ImageDigest {
		t.Errorf("%s's production runs %s, preview %s", steady, prod, patched.ImageDigest)
	}

	// the next wave: each fragile app exits in its watch, and Canyon puts back what the patch replaced
	for _, app := range fragile {
		p := waitFor(t, patchFor, app+"'s patch reverted", func() (envPatch, bool) {
			p := status(t, browser, app).Envs.Preview.Patch
			if p.Run != "" && slices.ContainsFunc(runLines(t, browser, p.Run), func(l string) bool { return strings.HasPrefix(l, "The watch passed") }) {
				t.Fatalf("%s's patched app never exited: its watch passed", app)
			}
			return p, p.State == "reverted"
		})
		if !slices.ContainsFunc(runLines(t, browser, p.Run), func(l string) bool {
			// Docker's restart policy starts the app again after its exit, so the watch reads either (contracts/routes.md, the reverted line)
			return strings.HasPrefix(l, "Canyon reverted preview at ") && (strings.Contains(l, "the patched app exited") || strings.Contains(l, "the patched app restarted"))
		}) {
			t.Errorf("no revert line on %s's patch run: %q", app, runLines(t, browser, p.Run))
		}
		if now := image(t, browser, app, "preview").ImageDigest; now != was[app] {
			t.Errorf("%s's preview runs %s after the revert, want the image the patch replaced, %s", app, now, was[app])
		}
		var audit struct{ Events []struct{ Action string } }
		if code := get(t, browser, host+"/api/v1/audit?action="+url.QueryEscape("revert patch preview")+"&project="+app, &audit); code != http.StatusOK || len(audit.Events) != 1 {
			t.Errorf("%s's revert patch preview rows: %d %+v", app, code, audit)
		}
	}
	var ids struct {
		Identities []struct {
			RunImage string `json:"run_image"`
			Paused   *struct{ Projects []string }
		}
	}
	if code := get(t, browser, host+"/api/v1/patching/identities", &ids); code != http.StatusOK {
		t.Fatalf("the identities: %d", code)
	}
	paused := false
	for _, i := range ids.Identities {
		paused = paused || i.Paused != nil && slices.Equal(slices.Sorted(slices.Values(i.Paused.Projects)), slices.Sorted(slices.Values(fragile)))
	}
	if !paused {
		t.Errorf("no identity paused naming %v: %+v", fragile, ids.Identities)
	}
}

// runImage builds a run image of the repository runRepo, tagged name, pushed to the registry, and answers it by
// digest: when dockerfile is empty, executor/run.Dockerfile, canyon-run's own file, on oldRun.
func runImage(t *testing.T, name, dockerfile string) string {
	t.Helper()
	if dockerfile == "" {
		own, err := os.ReadFile(filepath.Join("..", "..", "executor", "run.Dockerfile"))
		must(t, err)
		lines := strings.Split(string(own), "\n")
		from := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "FROM ") })
		if from < 0 || slices.ContainsFunc(lines[from+1:], func(l string) bool { return strings.HasPrefix(l, "FROM ") }) {
			t.Fatalf("executor/run.Dockerfile is not one stage on one FROM:\n%s", own)
		}
		lines[from] = "FROM " + oldRun
		dockerfile = strings.Join(lines, "\n")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "Dockerfile")
	must(t, os.WriteFile(file, []byte(dockerfile), 0o600))
	docker(t, "build", "-q", "-f", file, "-t", runRepo+":"+name, dir)
	docker(t, "push", "-q", runRepo+":"+name)
	digests := strings.Fields(docker(t, "image", "inspect", "--format", `{{join .RepoDigests " "}}`, runRepo+":"+name))
	if i := slices.IndexFunc(digests, func(d string) bool { return strings.HasPrefix(d, runRepo+"@sha256:") }); i >= 0 {
		return digests[i]
	}
	t.Fatalf("%s:%s pushed, with no digest of %s: %v", runRepo, name, runRepo, digests)
	return ""
}

// patchingOn is the executor on runImage, as an operator moves CANYON_IMAGE_RUN, until GET /builder names it; and
// with server named, the server patching too (e2e/compose.patch.yml).
func patchingOn(t *testing.T, runImage string, server bool) {
	t.Helper()
	if server {
		must(t, compose([]string{"CANYON_E2E_RUN_IMAGE=" + runImage}, "up", "-d", "--no-deps", "server", "executor"))
		must(t, compose(nil, "up", "-d", "--no-deps", "--force-recreate", "adapter")) // a recreated server took the sidecar's namespace with it
	} else {
		must(t, compose([]string{"CANYON_E2E_RUN_IMAGE=" + runImage}, "up", "-d", "--no-deps", "executor"))
	}
	id := docker(t, "image", "inspect", "--format", "{{.Id}}", runImage)
	waitFor(t, 3*time.Minute, "GET /builder naming "+runImage, func() (string, bool) {
		err := compose(nil, "exec", "-T", "executor", "sh", "-c", `wget -qO- http://127.0.0.1:7778/builder | grep -qF '"run_image":"`+id+`"'`)
		return fmt.Sprint(err), err == nil
	})
	waitFor(t, 2*time.Minute, "the server ready", func() (int, bool) {
		var ready any
		code := get(t, &http.Client{Timeout: 5 * time.Second}, host+"/ready", &ready)
		return code, code == http.StatusOK
	})
}

// compose runs docker compose on make e2e-up OIDC=1's files and e2e/compose.patch.yml, from the repository's root,
// with env added to the journey's own.
func compose(env []string, args ...string) error {
	files := []string{"compose", "--env-file", "e2e/.work/fixtures/oidc.env", "-f", "docker-compose.yml", "-f", "e2e/compose.e2e.yml",
		"-f", "deploy/docker/team.yml", "-f", "e2e/compose.oidc.yml", "-f", "e2e/compose.patch.yml"}
	cmd := exec.Command("docker", append(files, args...)...) //nolint:gosec // the journey's own arguments
	cmd.Dir, cmd.Env = filepath.Join("..", ".."), append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// staleDB dates trivy's database in the executor's volume (canyon-scandb-trivy, executor/scan.go) six days back, past
// the 5 days a match takes, and answers what puts its file back as it was, which the test's cleanup calls too.
func staleDB(t *testing.T) func() {
	t.Helper()
	const meta = "/db/db/metadata.json"
	file := func(write []byte) []byte {
		t.Helper()
		cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "none", "-v", "canyon-scandb-trivy:/db", busybox, "sh", "-c", "cat "+meta) //nolint:gosec // the journey's own arguments
		if write != nil {
			cmd = exec.Command("docker", "run", "--rm", "-i", "--network", "none", "-v", "canyon-scandb-trivy:/db", busybox, "sh", "-c", "cat > "+meta) //nolint:gosec // the same
			cmd.Stdin = bytes.NewReader(write)
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("trivy's database metadata: %v\n%s", err, stderr.Bytes())
		}
		return out
	}
	was := file(nil)
	var md map[string]any
	if err := json.Unmarshal(was, &md); err != nil || md["UpdatedAt"] == nil {
		t.Fatalf("trivy's database metadata %q: %v", was, err)
	}
	md["UpdatedAt"] = time.Now().Add(-6 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	stale, err := json.Marshal(md)
	must(t, err)
	file(stale)
	done := false
	restore := func() {
		if !done {
			done = true
			file(was)
		}
	}
	t.Cleanup(restore)
	return restore
}

// patchingSays runs e2e/ui's patch project in Chromium as the admin signed in, their session cookie handed over:
// Settings › Patching shows that Canyon's last check could not check the image of any of apps.
func patchingSays(t *testing.T, browser *http.Client, apps []string) {
	t.Helper()
	u, err := url.Parse(host)
	must(t, err)
	var session *http.Cookie
	for _, c := range browser.Jar.Cookies(u) {
		if strings.HasSuffix(c.Name, "canyon_session") {
			session = c
		}
	}
	if session == nil {
		t.Fatal("the admin's browser holds no session")
	}
	cmd := exec.Command("npx", "playwright", "test", "--project=patch") //nolint:gosec // the harness's own runner
	cmd.Dir = filepath.Join("..", "ui")
	cmd.Env = append(os.Environ(), "E2E_CANYON_URL="+host, "PATCH_COOKIE_NAME="+session.Name, "PATCH_COOKIE_VALUE="+session.Value, "PATCH_APPS="+strings.Join(apps, ","))
	out, err := cmd.CombinedOutput()
	t.Logf("playwright, Settings › Patching:\n%s", out)
	if err != nil { // the journey goes on: what the page says is no step of the rescan's
		t.Errorf("Settings › Patching: %v", err)
	}
}

type planRead struct {
	Apps []struct{ Project, Why string }
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput() //nolint:gosec // the journey's own arguments
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// canaryOrder is the order a platform identity takes its apps in (runs/waves.go, order): a hash of the app's id.
func canaryOrder(app string) string {
	sum := sha256.Sum256([]byte(app))
	return hex.EncodeToString(sum[:])
}

// patchSource is an app that answers on its port; a fragile one exits 45 s after it starts on a run image carrying
// the marker, past /run's settle and inside a minute's watch.
func patchSource(t *testing.T, app string, fragile bool) string {
	t.Helper()
	dir := t.TempDir()
	exits := ""
	if fragile {
		exits = `if (fs.existsSync("` + marker + `")) setTimeout(() => process.exit(3), 45000);
`
	}
	files := map[string]string{
		"canyon.toml":  "app = \"" + app + "\"\n",
		"package.json": `{"name": "patch-e2e", "version": "1.0.0", "private": true, "scripts": {"start": "node server.js"}}` + "\n",
		"server.js": `const fs = require("node:fs");
const http = require("node:http");

` + exits + `http.createServer((req, res) => {
  res.setHeader("Content-Type", "text/plain");
  res.end("ok\n");
}).listen(Number(process.env.PORT) || 3000);
`,
	}
	for name, body := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

type envPatch struct {
	State, Identity, Run, Approval string
}

type statusRead struct {
	Envs struct {
		Preview, Prod struct{ Patch envPatch }
	}
}

func status(t *testing.T, browser *http.Client, app string) statusRead {
	t.Helper()
	var s statusRead
	if code := get(t, browser, host+"/api/v1/projects/"+app+"/status", &s); code != http.StatusOK {
		t.Fatalf("%s's status read: %d", app, code)
	}
	return s
}

type fixed struct {
	Name, From, To, Vulnerability, Class string
}

type imageRead struct {
	ImageDigest string `json:"image_digest"`
	State, Why  string
	Findings    []struct{ Class string }
	Patched     []fixed
}

// image is the image read of app's env (contracts/routes.md, System libraries and build tools); a zero one while nothing runs there.
func image(t *testing.T, browser *http.Client, app, env string) imageRead {
	t.Helper()
	var r imageRead
	_ = get(t, browser, host+"/api/v1/projects/"+app+"/envs/"+env+"/image", &r)
	return r
}

// runLines is a run's build.log lines.
func runLines(t *testing.T, browser *http.Client, run string) []string {
	t.Helper()
	var page struct {
		Rows []struct {
			Kind string
			Data struct{ Line string }
		}
	}
	if code := get(t, browser, host+"/api/v1/gate-runs/"+run+"/timeline?limit=500", &page); code != http.StatusOK {
		t.Fatalf("run %s's timeline: %d", run, code)
	}
	var lines []string
	for _, r := range page.Rows {
		if r.Kind == "build.log" {
			lines = append(lines, r.Data.Line)
		}
	}
	return lines
}

// waitFor reads until ok or within passes, failing the test with what it last read.
func waitFor[T any](t *testing.T, within time.Duration, what string, read func() (T, bool)) T {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		got, ok := read()
		if ok {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s, last %+v", what, within, got)
		}
		time.Sleep(10 * time.Second)
	}
}
