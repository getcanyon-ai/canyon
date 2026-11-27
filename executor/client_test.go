package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheClientBuildsAndRunsAgainstTheService drives the real Handler with the script fakes:
// the client is exercised against the service, no Docker, no buildpack.
func TestTheClientBuildsAndRunsAgainstTheService(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	var lines []string
	res, err := c.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{}},
		bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), func(_ context.Context, l string) error { lines = append(lines, l); return nil })
	if err != nil || !strings.HasPrefix(res.Image, "canyon/claims-portal@sha256:") || strings.Join(lines, ",") != "===> DETECTING,===> BUILDING" {
		t.Fatalf("build: %+v %v lines %v", res, err, lines)
	}
	run, err := c.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: res.Image, Env: map[string]string{}, Network: "canyon-app-claims-portal"})
	if err != nil || run.Container != "claims-portal-preview" || run.Addr != "claims-portal-preview:3000" {
		t.Fatalf("run: %+v %v", run, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "--name\nclaims-portal-preview") {
		t.Fatalf("docker argv: %s", argv)
	}
}

func TestTheClientTellsABuildTheExecutorRefusedFromItsOwnFailure(t *testing.T) {
	e, dir := fakes(t)
	if err := os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\necho '===> DETECTING'; echo 'ERROR: No buildpack groups passed detection'; exit 1\n"), 0o700); err != nil { //nolint:gosec // a test fake must be executable
		t.Fatal(err)
	}
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	var lines []string
	_, err := c.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(_ context.Context, l string) error { lines = append(lines, l); return nil })
	var failed *BuildFailed
	if !errors.As(err, &failed) || !strings.Contains(failed.Message, "pack:") || len(lines) != 2 {
		t.Fatalf("a build the executor refused is the user's: %v (lines %v)", err, lines)
	}
	// the executor unreachable: Canyon's own failure, never a BuildFailed
	srv.Close()
	_, err = c.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
	if err == nil || errors.As(err, &failed) {
		t.Fatalf("an unreachable executor: %v", err)
	}
	// a request the service refuses before the stream (a bad app name) is not a BuildFailed either
	srv2 := httptest.NewServer(Handler(e))
	defer srv2.Close()
	c.Base = srv2.URL
	_, err = c.Build(context.Background(), BuildRequest{App: "Claims Portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
	if err == nil || errors.As(err, &failed) || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a refused request: %v", err)
	}
	if _, err := c.Run(context.Background(), RunRequest{App: "claims-portal", Target: "staging", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}); !errors.Is(err, ErrRequest) || !strings.Contains(err.Error(), "400") {
		t.Fatalf("a refused run: %v", err)
	}
}

// A tree no buildpack group detects is a failed detect, not a failed build (contracts/executor.md,
// /build; #1242): pack's last line carries the detector's status, and the answer names what the builder
// builds, from its order label, each buildpack by its metadata label's name. testdata/detect-21.log is a
// real pack 0.40.3 run of a Rust tree on the Paketo noble builder, cut to its first and last groups:
// node-start errors on a tree with no Node entry point, so it ends 21, errored. detect-20.log is the same
// run with no buildpack erroring. Names the executor cannot read leave each buildpack its id; an order it
// cannot read still answers a failed detect, naming none; a 51 is the app's own build failing.
func TestAFailedDetectNamesWhatTheBuilderBuilds(t *testing.T) {
	order := `[{"group":[{"id":"paketo-buildpacks/ruby","version":"3.1.1"}]},` +
		`{"group":[{"id":"paketo-buildpacks/nodejs","version":"10.13.0"},{"id":"paketo-buildpacks/procfile","version":"5.15.2","optional":true}]},` +
		`{"group":[{"id":"paketo-buildpacks/ruby","version":"3.1.1"}]}]`
	meta := `{"buildpacks":[{"id":"paketo-buildpacks/nodejs","name":"Paketo Buildpack for Node.js","version":"10.13.0"},{"id":"paketo-buildpacks/ruby","name":"Paketo Buildpack for Ruby","version":"3.1.1"}]}`
	named := []string{"Paketo Buildpack for Ruby", "Paketo Buildpack for Node.js"}
	ids := []string{"paketo-buildpacks/ruby", "paketo-buildpacks/nodejs"}
	for _, c := range []struct {
		log, order, meta string   // order "!": the builder cannot be inspected
		builds           []string // nil: not a failed detect
		errored          bool
	}{
		{"detect-21.log", order, meta, named, true},
		{"detect-20.log", order, meta, named, false},
		{"detect-20.log", order, "", ids, false},
		{"detect-20.log", order, "not metadata", ids, false},
		{"detect-21.log", "", meta, []string{}, true},
		{"detect-20.log", "not an order", meta, []string{}, false},
		{"detect-20.log", "!", meta, []string{}, false},
		{"build-51.log", order, meta, nil, false},
	} {
		e, dir := fakes(t)
		labels := map[string]string{"io.buildpacks.buildpack.order": c.order}
		if c.meta != "" {
			labels[MetadataLabel] = c.meta
		}
		raw, err := json.Marshal(labels)
		must(t, err)
		if c.order == "!" {
			raw = []byte("!")
		}
		must(t, os.WriteFile(filepath.Join(dir, "labels"), append(raw, '\n'), 0o600))
		log, err := filepath.Abs(filepath.Join("testdata", c.log))
		must(t, err)
		if c.log == "build-51.log" {
			log = filepath.Join(dir, c.log)
			must(t, os.WriteFile(log, []byte("[builder] npm ERR! missing script: build\nERROR: failed to build: executing lifecycle: failed with status code: 51\n"), 0o600))
		}
		pack := "#!/bin/sh\nsed '$d' '" + log + "'; tail -n 1 '" + log + "' >&2; exit 1\n" // pack's own last line is on stderr
		must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte(pack), 0o700))             //nolint:gosec // a test fake must be executable
		docker := floored("#!/bin/sh\ncase \"$*\" in *\"json .Config.Labels\"*) [ \"$(cat '" + dir + "/labels')\" = '!' ] && exit 1; cat '" + dir + "/labels';; *) echo sha256:" + strings.Repeat("ab", 32) + ";; esac\n")
		must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // a test fake must be executable
		srv := httptest.NewServer(Handler(e))
		var lines []string
		_, err = (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"Cargo.toml": "[package]"})), func(_ context.Context, l string) error { lines = append(lines, l); return nil })
		srv.Close()
		var undetected *Undetected
		var failed *BuildFailed
		switch {
		case len(lines) < 2 || !strings.Contains(lines[len(lines)-1], "failed with status code"):
			t.Fatalf("%s: every line streams before the answer: %q", c.log, lines)
		case c.builds == nil && (!errors.As(err, &failed) || errors.As(err, &undetected)):
			t.Fatalf("%s is the app's own build failing: %v", c.log, err)
		case c.builds != nil && (!errors.As(err, &undetected) || undetected.Builds == nil || !slices.Equal(undetected.Builds, c.builds) || undetected.Errored != c.errored):
			t.Fatalf("%s, order %q, metadata %q: %#v, want a failed detect naming %q, errored %v", c.log, c.order, c.meta, err, c.builds, c.errored)
		}
	}
}

// The buildpacks that ran are the group in order, each once, each named only by a contract-shaped name; a
// group with an id of another shape, or of more than 64, is none (contracts/executor.md, The buildpacks
// that ran).
func TestRanOfNamesTheGroupInOrder(t *testing.T) {
	names := map[string]string{"paketo-buildpacks/node-engine": "Paketo Buildpack for Node Engine", "paketo-buildpacks/npm-install": "npm\x1b[2J"}
	got, err := RanOf([]byte(`[{"id":"paketo-buildpacks/node-engine","version":"4.1.0"},{"id":"paketo-buildpacks/npm-install"},{"id":"paketo-buildpacks/node-engine"}]`), names)
	want := []Buildpack{{ID: "paketo-buildpacks/node-engine", Name: "Paketo Buildpack for Node Engine"}, {ID: "paketo-buildpacks/npm-install"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("RanOf: %+v %v, want %+v", got, err, want)
	}
	var many []string
	for i := range 70 {
		many = append(many, fmt.Sprintf(`{"id":"bp/%d"}`, i))
	}
	if got, err := RanOf([]byte("["+strings.Join(many[:64], ",")+"]"), nil); err != nil || len(got) != 64 {
		t.Fatalf("64 is a group: %d %v", len(got), err)
	}
	for _, group := range []string{"[" + strings.Join(many, ",") + "]", `[{"id":"paketo-buildpacks/node-engine"},{"id":"bad id"}]`, `[{"id":""}]`} {
		if got, err := RanOf([]byte(group), names); err == nil || got == nil || len(got) != 0 {
			t.Fatalf("%.50s: a group missing what ran is none and an error: %#v %v", group, got, err)
		}
	}
	if got, err := RanOf([]byte("null"), nil); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no group is none, never nil: %#v %v", got, err)
	}
	if got, err := RanOf([]byte("not a group"), nil); err == nil || got == nil {
		t.Fatalf("an unreadable group is none and an error: %#v %v", got, err)
	}
}

// A Docker build answers the buildpacks its image's io.buildpacks.build.metadata label names, each by the
// builder's metadata name; labels that cannot be read are none, and the build still succeeds.
func TestADockerBuildNamesTheBuildpacksItRan(t *testing.T) {
	labels := `{"io.buildpacks.build.metadata":"{\"buildpacks\":[{\"id\":\"paketo-buildpacks/node-engine\",\"version\":\"4.1.0\"},{\"id\":\"paketo-buildpacks/npm-install\"}]}",` +
		`"io.buildpacks.builder.metadata":"{\"buildpacks\":[{\"id\":\"paketo-buildpacks/node-engine\",\"name\":\"Paketo Buildpack for Node Engine\"}]}"}`
	for answer, want := range map[string][]Buildpack{
		labels: {{ID: "paketo-buildpacks/node-engine", Name: "Paketo Buildpack for Node Engine"}, {ID: "paketo-buildpacks/npm-install"}},
		"!":    {},
	} {
		e, dir := fakes(t)
		must(t, os.WriteFile(filepath.Join(dir, "labels"), []byte(answer+"\n"), 0o600))
		docker := floored("#!/bin/sh\ncase \"$*\" in *\"json .Config.Labels\"*) [ \"$(cat '" + dir + "/labels')\" = '!' ] && exit 1; cat '" + dir + "/labels';; *) echo sha256:" + strings.Repeat("ab", 32) + ";; esac\n")
		must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // a test fake must be executable
		srv := httptest.NewServer(Handler(e))
		res, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), func(context.Context, string) error { return nil })
		srv.Close()
		if err != nil || !slices.Equal(res.Buildpacks, want) {
			t.Fatalf("labels %q: %+v %v, want %+v", answer, res.Buildpacks, err, want)
		}
	}
}

// A builder's order names each buildpack once, by its metadata's name, never an optional one or one with
// no id; a buildpack the metadata does not name is named by its id.
func TestBuildsOfNamesEachRequiredBuildpackOnceByItsName(t *testing.T) {
	names, err := NamesOf([]byte(`{"buildpacks":[{"id":"paketo-buildpacks/go","name":"Paketo Buildpack for Go","version":"4.15.1"},{"id":"paketo-buildpacks/go","name":"Paketo Buildpack for Go, again"},{"id":"paketo-buildpacks/procfile","name":"Paketo Buildpack for Procfile"}]}`))
	must(t, err)
	got, err := BuildsOf([]byte(`[{"group":[{"id":""},{"id":"paketo-buildpacks/go"}]},{"group":[{"id":"paketo-buildpacks/go"},{"id":"paketo-buildpacks/procfile","optional":true}]},{"group":[{"id":"paketo-buildpacks/nodejs"}]}]`), names)
	if err != nil || !slices.Equal(got, []string{"Paketo Buildpack for Go", "paketo-buildpacks/nodejs"}) {
		t.Fatalf("BuildsOf: %q (%v)", got, err)
	}
	// A kpack ClusterBuilder's image lists its buildpacks with no names (the test cluster, #1599): its ids show, and the log says why.
	if _, err := NamesOf([]byte(`{"buildpacks":[{"id":"paketo-buildpacks/yarn","version":"2.4.5","homepage":"https://github.com/paketo-buildpacks/yarn"}]}`)); err == nil || err.Error() != "the builder image's metadata has no buildpack names; showing ids" {
		t.Errorf("metadata with no names: %v", err)
	}
	if _, err := NamesOf([]byte("not metadata")); err == nil {
		t.Fatal("NamesOf read metadata that is not JSON")
	}
}

// A name is the builder's text and reaches a terminal: only one matching the contract's pattern is named —
// printable ASCII from a letter or digit, at most 128 characters — and one with a control or bidi
// character, or a run-on name, is named by its id instead; an id that fails too is left out. At most 64
// (contracts/executor.md, /build).
func TestBuildsOfNamesOnlyContractShapedNamesAndAtMost64(t *testing.T) {
	names := map[string]string{
		"bp/control":   "Paketo Buildpack for Go\u001b[2J",
		"bp/bidi":      "Paketo Buildpack for Ruby\u202e",
		"bp/long":      strings.Repeat("P", 129),
		"bp/space":     " Paketo Buildpack for PHP",
		"bp/max":       strings.Repeat("P", 128),
		"bp/ok":        "Paketo Buildpack for Node.js",
		"bp/bad\u202e": "Paketo Buildpack for Java",
	}
	order := `[{"group":[{"id":"bp/control"},{"id":"bp/bidi"},{"id":"bp/long"},{"id":"bp/space"},{"id":"bp/max"},{"id":"bp/ok"},` +
		`{"id":"bp/bad\u202e"},{"id":"bp/\u001b[2J"},{"id":"` + strings.Repeat("p", 129) + `"}]}`
	for i := range 70 {
		order += fmt.Sprintf(`,{"group":[{"id":"bp/%02d"}]}`, i)
	}
	got, err := BuildsOf([]byte(order+"]"), names)
	want := []string{"bp/control", "bp/bidi", "bp/long", "bp/space", strings.Repeat("P", 128), "Paketo Buildpack for Node.js", "Paketo Buildpack for Java"}
	if err != nil || len(got) != 64 || !slices.Equal(got[:len(want)], want) || got[63] != "bp/56" {
		t.Fatalf("BuildsOf: %d entries, %q (%v)", len(got), got[:min(len(want)+1, len(got))], err)
	}
}

// A failed detect is the contract's exact line: an error line with builds and any other message is a
// failed build, so a stream cannot dress a build failure up as a host that builds nothing (#1242).
func TestTheClientReadsAFailedDetectOnlyFromTheContractsLine(t *testing.T) {
	for _, c := range []struct {
		line       string
		undetected bool
	}{
		{`{"error":"no buildpack group passed detection","builds":["paketo-buildpacks/go"]}`, true},
		{`{"error":"pack: exit status 1","builds":["paketo-buildpacks/go"]}`, false},
		{`{"error":"no buildpack group passed detection"}`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, c.line+"\n")
		}))
		_, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
		srv.Close()
		var undetected *Undetected
		var failed *BuildFailed
		if errors.As(err, &undetected) != c.undetected || (!c.undetected && !errors.As(err, &failed)) {
			t.Errorf("%s: %#v, a failed detect: %v", c.line, err, c.undetected)
		}
	}
}

// A /build row the contract does not name ends the build as an error, even with a result after it: the
// client never skips a row it cannot read (#728).
func TestABuildRowTheClientCannotReadEndsTheBuild(t *testing.T) {
	for _, row := range []string{`{"progress":"50%"}`, `{}`, `{"line":null}`, `{"image":""}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, row+"\n"+`{"image":"canyon/claims-portal@sha256:`+strings.Repeat("a", 64)+`","duration_ms":1}`+"\n")
		}))
		res, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
		srv.Close()
		if err == nil || res.Image != "" {
			t.Errorf("%s: %+v (%v), want the build ended on the row", row, res, err)
		}
	}
}

// An answer that is not the executor's JSON error is named by its status alone: its body, a proxy's page or
// anything else, never reaches the error a run's journal keeps (#728). The executor's own error is kept.
func TestAnAnswerThatIsNotTheExecutorsErrorIsItsStatusAlone(t *testing.T) {
	for body, want := range map[string]string{
		"<html><body>" + strings.Repeat("upstream connect error ", 150) + "</body></html>": "executor: /run answered 500",
		`{"error":"the image is not on this host"}`:                                        "executor: /run answered 500: the image is not on this host",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, body)
		}))
		_, err := (&Client{Base: srv.URL}).Run(t.Context(), RunRequest{App: "claims-portal", Target: "preview"})
		srv.Close()
		if err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
}

// Only a pack that exited on its own says how detection went: one killed by a signal, or by the build's
// ceiling, is never a failed detect, whatever its last line read.
func TestAKilledPackIsNeverAFailedDetect(t *testing.T) {
	line := "echo 'ERROR: failed to build: executing lifecycle: failed with status code: 21'"
	for name, c := range map[string]struct {
		script  string
		timeout time.Duration
	}{
		"killed":    {line + "; kill -9 $$", time.Minute},
		"timed out": {line + "; exec sleep 30", 2 * time.Second},
	} {
		e, dir := fakes(t)
		must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\n"+c.script+"\n"), 0o700)) //nolint:gosec // a test fake must be executable
		ctx, cancel := context.WithTimeout(t.Context(), c.timeout)
		_, err := e.Build(ctx, BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), io.Discard)
		cancel()
		if undetected := (*Undetected)(nil); err == nil || errors.As(err, &undetected) {
			t.Errorf("pack %s: %#v, never a failed detect", name, err)
		}
	}
}

// A build step that fails says which step and its code, as Kubernetes says them, read from pack's own lifecycle
// line: never "pack: exit status 1", which names neither (#1734). A pack failing for any other reason keeps its own words.
func TestAFailedBuildSaysTheStepAndItsCode(t *testing.T) {
	for script, want := range map[string]string{
		"echo 'ERROR: failed to build: executing lifecycle: failed with status code: 51'; exit 1": "the build step exited 51",
		"echo 'ERROR: failed to fetch builder'; exit 1":                                           "pack: exit status 1",
	} {
		e, dir := fakes(t)
		must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\n"+script+"\n"), 0o700)) //nolint:gosec // a test fake must be executable
		if _, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), io.Discard); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", script, err, want)
		}
	}
}

// The client reads /scan as the service answers it: the tool's SARIF with its database's build time, a
// scan that did not finish, busy as *Busy carrying the Retry-After and, while the tool's database
// downloads, the reason "database", and a refused request as an error.
func TestTheClientScansAgainstTheService(t *testing.T) {
	req := ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &ScanSettings{SkipPaths: []string{"docs"}}}
	scan := func(url string, req ScanRequest) (ScanResult, error) {
		return (&Client{Base: url}).Scan(t.Context(), req, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), nil)
	}
	srv, _, _ := dbFake(t, builtAgo(time.Hour))
	if res, err := scan(srv.URL, req); err != nil || res.Tool != "trivy" || res.Image != trivyImage || res.DBBuiltAt == "" || !strings.Contains(string(res.SARIF), `"ruleId":"r"`) {
		t.Fatalf("a finished scan: %+v %v", res, err)
	}
	stale, _, _ := dbFake(t, builtAgo(dbMaxAge+time.Hour))
	if res, err := scan(stale.URL, req); err != nil || res.NotFinished != "db_stale" || res.SARIF != nil {
		t.Fatalf("a stale database: %+v %v", res, err)
	}
	var busy *Busy
	missing, e, _ := dbFake(t, "")
	if _, err := scan(missing.URL, req); !errors.As(err, &busy) || busy.RetryAfter != 10*time.Second || busy.Reason != "database" {
		t.Fatalf("a scan before the database exists: %v", err)
	}
	e.collecting.Wait() // the download it started
	if _, err := scan(srv.URL, ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy"}); err == nil || errors.As(err, &busy) {
		t.Fatalf("a request without settings is refused, not busy: %v", err)
	}
	for range cap(scans) {
		scans <- struct{}{}
		defer func() { <-scans }()
	}
	if _, err := scan(srv.URL, req); !errors.As(err, &busy) || busy.RetryAfter != 10*time.Second || busy.Reason != "" {
		t.Fatalf("past the scans' limit: %v", err)
	}
}

// Past the shared limit /build answers busy as /scan does: *Busy carrying the Retry-After, nothing built.
func TestTheClientReadsABusyBuild(t *testing.T) {
	e, _ := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	for range cap(builds) {
		builds <- struct{}{}
		defer func() { <-builds }()
	}
	_, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
	var busy *Busy
	if !errors.As(err, &busy) || busy.RetryAfter != 10*time.Second {
		t.Fatalf("past the shared limit: %v", err)
	}
}

// The client asks /probe about the domain and form and reads its answer, and refuses any other: the
// Docker executor's 404, a 502, and a 200 outside the contract's shape.
func TestTheClientProbes(t *testing.T) {
	e, _ := fakes(t)
	docker := httptest.NewServer(Handler(e))
	defer docker.Close()
	if _, err := (&Client{Base: docker.URL}).Probe(t.Context(), "canyon.meridian.example", "app.env.domain"); err == nil || !strings.Contains(err.Error(), "/probe answered 404") {
		t.Fatalf("the Docker executor routes no /probe: %v", err)
	}
	var status int
	var answer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var asked map[string]string
		if err := json.NewDecoder(r.Body).Decode(&asked); err != nil || r.URL.Path != "/probe" || len(asked) != 2 || asked["domain"] != "canyon.meridian.example" || asked["form"] != "app-env.domain" {
			t.Errorf("asked %s %v (%v)", r.URL.Path, asked, err)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		status int
		answer string
		want   *ProbeResult
	}{
		{200, `{"network_policy":"enforced","certificate":"ok","fix":""}`, &ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"}},
		{200, `{"network_policy":"not_enforced","certificate":"missing","fix":"Turn it on. Cover the names."}`, &ProbeResult{NetworkPolicy: "not_enforced", Certificate: "missing", Fix: "Turn it on. Cover the names."}},
		{200, `{"network_policy":"enforced","certificate":"ok","fix":"stray"}`, nil},
		{200, `{"network_policy":"not_enforced","certificate":"ok","fix":""}`, nil},
		{200, `{"network_policy":"unknown","certificate":"ok","fix":"x"}`, nil},
		{200, `{"network_policy":"enforced","certificate":"ok","fix":"","extra":1}`, nil},
		{502, `{"error":"the probe could not tell"}`, nil},
	} {
		status, answer = tc.status, tc.answer
		res, err := (&Client{Base: srv.URL}).Probe(t.Context(), "canyon.meridian.example", "app-env.domain")
		if tc.want == nil && err == nil || tc.want != nil && (err != nil || res != *tc.want) {
			t.Fatalf("%d %s: %+v %v", tc.status, tc.answer, res, err)
		}
	}
}

// A build that runs on after its tree is in is not cut off by the tree's read deadline: a small tree
// arrives whole, the body's end is read with it, and a deadline left armed then ends the request —
// team-dashboard's build ended "context canceled" 60 s after its 1.9 KB upload (canyonv2-amvo).
func TestABuildOutlastsTheTreesReadDeadline(t *testing.T) {
	defer func(d time.Duration) { treeIdle = d }(treeIdle)
	treeIdle = 100 * time.Millisecond
	srv := httptest.NewServer(Handler(slowBuild{wait: 10 * treeIdle}))
	defer srv.Close()
	var body bytes.Buffer // whole, with a Content-Length: the body's end arrives with the tree, as a small tree's does
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormField("json")
	_ = json.NewEncoder(part).Encode(BuildRequest{App: "team-dashboard", Package: "pkg_7d3e8f"})
	part, _ = mw.CreateFormFile("tree", "tree.tar")
	_, _ = part.Write(tarOf(t, map[string]string{"package.json": "{}"}))
	_ = mw.Close()
	resp, err := http.Post(srv.URL+"/build", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), `"image":"canyon/team-dashboard@sha256:`) {
		t.Fatalf("a build longer than the tree's read deadline: %s", out)
	}
}

// A tree that stops arriving still ends its build once one read has waited treeIdle.
func TestAStalledTreeEndsItsBuild(t *testing.T) {
	defer func(d time.Duration) { treeIdle = d }(treeIdle)
	treeIdle = 100 * time.Millisecond
	srv := httptest.NewServer(Handler(slowBuild{wait: time.Minute}))
	defer srv.Close()
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	mw := multipart.NewWriter(pw)
	go func() {
		part, _ := mw.CreateFormField("json")
		_ = json.NewEncoder(part).Encode(BuildRequest{App: "team-dashboard", Package: "pkg_7d3e8f"})
		part, _ = mw.CreateFormFile("tree", "tree.tar")
		_, _ = part.Write([]byte("the first bytes of a tree, then nothing"))
	}()
	resp, err := http.Post(srv.URL+"/build", mw.FormDataContentType(), pr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(out), "i/o timeout") {
		t.Fatalf("a stalled tree: %s", out)
	}
}

// slowBuild reads the tree, then builds for wait, or until its request ends.
type slowBuild struct {
	Calls
	wait time.Duration
}

func (s slowBuild) Build(ctx context.Context, _ BuildRequest, tree io.Reader, _ io.Writer) (BuildResult, error) {
	if _, err := io.Copy(io.Discard, tree); err != nil {
		return BuildResult{}, err
	}
	select {
	case <-ctx.Done():
		return BuildResult{}, ctx.Err()
	case <-time.After(s.wait):
		return BuildResult{Image: "canyon/team-dashboard@sha256:" + strings.Repeat("ab", 32)}, nil
	}
}

// An image is missing only when the daemon lists this app's images and that one is not among them:
// /run's 404, which the client answers as ErrNoImage. A daemon that does not answer is the executor's 502,
// and what docker writes on stderr never decides it, whatever it says (contracts/executor.md, /run; #1244).
func TestARunOfAnImageTheDaemonDoesNotListIsNoImage(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	req := RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Env: map[string]string{}, Network: "canyon-app-claims-portal"}
	listed := "sha256:" + strings.Repeat("ab", 32) + " canyon/claims-portal:pkg_7d3e8f-abcd"
	for name, tc := range map[string]struct {
		docker string
		gone   bool
	}{
		"listed without it":       {"[ \"$1 $2\" = 'image ls' ] && { echo 'sha256:" + strings.Repeat("cd", 32) + " canyon/claims-portal:pkg_1-a'; exit 0; }; exit 1", true},
		"a daemon not answering":  {"echo 'failed to connect to the docker API at unix:///var/run/docker.sock' >&2; exit 1", false},
		"listed, stderr says not": {"[ \"$1 $2\" = 'image ls' ] && { echo '" + listed + "'; exit 0; }; echo 'Error response from daemon: No such image: sha256:" + strings.Repeat("ab", 32) + "' >&2; exit 1", false},
	} {
		must(t, os.WriteFile(filepath.Join(dir, "docker.next"), []byte("#!/bin/sh\n"+tc.docker+"\n"), 0o700)) //nolint:gosec // a test fake must be executable
		must(t, os.Rename(filepath.Join(dir, "docker.next"), filepath.Join(dir, "docker")))
		if _, err := c.Run(t.Context(), req); err == nil || errors.Is(err, ErrNoImage) != tc.gone {
			t.Errorf("%s: %v, want no-image %v", name, err, tc.gone)
		}
	}
}

// A 502 without output is the executor's reason whole, however long: a restarted canyon-identity's ends
// with its last words, quoted, which JSON escapes again, so a 2 KiB tail of an issuer's HTML can pass the
// 4 KiB any other answer is cut to and lose the end that says why (#1790).
func TestARunsReasonIsReadWhole(t *testing.T) {
	reason := "the app did not stay up: container canyon-identity of pod p restarted (Error, exit 1), its last words: " +
		strconv.Quote(strings.Repeat(`<a href="x">`, 200)+"the end")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
	}))
	defer srv.Close()
	_, err := (&Client{Base: srv.URL}).Run(t.Context(), RunRequest{App: "claims-portal", Target: "preview"})
	if want := "executor: /run answered 502: " + reason; err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A container is gone only when the daemon lists none of that name: a daemon that does not answer, or
// one whose stderr says no such container while it cannot list, is an error, never a container gone.
func TestAContainerIsGoneOnlyWhenTheDaemonListsNone(t *testing.T) {
	e, dir := fakes(t)
	for name, tc := range map[string]struct {
		docker string
		gone   bool
	}{
		"listed none":            {"[ \"$1 $2\" = 'container ls' ] && exit 0; exit 1", true},
		"a daemon not answering": {"echo 'failed to connect to the docker API at unix:///var/run/docker.sock' >&2; exit 1", false},
		"stderr says not":        {"echo 'Error response from daemon: No such container: claims-portal-prod' >&2; exit 1", false},
	} {
		must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+tc.docker+"\n"), 0o700)) //nolint:gosec // a test fake must be executable
		state, _, _, err := e.status(t.Context(), "claims-portal-prod")
		if (state == "" && err == nil) != tc.gone || (!tc.gone && err == nil) {
			t.Errorf("%s: state %q, %v; want gone %v", name, state, err, tc.gone)
		}
	}
}

// GET /builder answers the builder image's ID on the daemon, pulling it first when the daemon has none,
// and a daemon that does not answer is a 502; an executor from before the call answers 404, which the
// client reads as no builder, "" (contracts/executor.md, GET /builder; #1244).
func TestGetBuilderIsTheBuilderImagesID(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c, id := &Client{Base: srv.URL}, "sha256:"+strings.Repeat("cd", 32)
	// no CANYON_BUILDER: canyon-builder's own ID, it and the canyon-builder it is built from built first when the
	// daemon has none
	builderImage.id, ownBuilder.id = "", ""
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ncase \"$*\" in *canyon-run:*) echo "+id+"; exit 0;; *io.buildpacks.builder.metadata*) echo '"+builderMetadataFake+"'; exit 0;; esac\n"+ //nolint:gosec // a test fake must be executable
		"[ \"$1 $2 $3\" = 'build --quiet --tag' ] && [ \"$4\" = '"+builderImage.repo+"' ] && { echo sha256:"+strings.Repeat("ef", 32)+"; exit 0; }\n"+
		"[ \"$1 $2 $3\" = 'build --quiet --tag' ] && [ \"$4\" = '"+ownBuilder.repo+"' ] && { echo "+id+"; exit 0; }; exit 1\n"), 0o700))
	if got, err := c.BuilderImage(t.Context()); got != id || err != nil {
		t.Errorf("canyon-builder: %q %v, want %q", got, err, id)
	}
	e.Builder = "paketobuildpacks/ubuntu-noble-builder" // a CANYON_BUILDER's: pulled first when the daemon has none
	for name, tc := range map[string]struct{ docker, want string }{
		"on the daemon":          {"[ \"$1 $2\" = 'image inspect' ] && { echo " + id + "; exit 0; }; exit 1", id},
		"pulled first":           {"[ \"$1\" = pull ] && { : > '" + dir + "/pulled'; exit 0; }; [ \"$1 $2\" = 'image inspect' ] && [ -e '" + dir + "/pulled' ] && { echo " + id + "; exit 0; }; exit 1", id},
		"a daemon not answering": {"echo 'failed to connect to the docker API' >&2; exit 1", ""},
	} {
		must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ncase \"$*\" in *canyon-run:*) echo "+id+"; exit 0;; esac\n"+tc.docker+"\n"), 0o700)) //nolint:gosec // a test fake must be executable: canyon-run on the daemon, as GET /builder's run_image needs it
		if got, err := c.BuilderImage(t.Context()); got != tc.want || (err == nil) != (tc.want != "") {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
	}
	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	if got, err := (&Client{Base: old.URL}).BuilderImage(t.Context()); got != "" || err != nil {
		t.Fatalf("an executor from before GET /builder: %q %v, want no builder", got, err)
	}
}

// A no-room build's last line is the client's NoRoom, the scheduler's message on it (contracts/executor.md,
// /build; #1637); the same line without no_room is a failed build.
func TestANoRoomBuildIsNoRoom(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{`{"error":"the cluster had no room to run the build","no_room":"0/4 nodes are available: 4 Insufficient cpu."}`, "0/4 nodes are available: 4 Insufficient cpu."},
		{`{"error":"the cluster had no room to run the build"}`, ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(tc.line + "\n"))
		}))
		_, err := (&Client{Base: srv.URL}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"a": "x"})), func(context.Context, string) error { return nil })
		srv.Close()
		var noRoom *NoRoom
		if got := errors.As(err, &noRoom); got != (tc.want != "") || got && noRoom.Reason != tc.want {
			t.Errorf("%s: %#v", tc.line, err)
		}
	}
}

// POST /sidecars' answer is taken only in the contract's shape (contracts/executor.md, POST /sidecars):
// the fixture's sidecars_result is read as it is, and an answer out of shape, or any status but 200 — an
// older executor's or the Docker executor's 404 — is an error, so the server records nothing it says.
func TestTheClientTakesOnlyAContractShapedSidecarsAnswer(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		Sidecars json.RawMessage `json:"sidecars_result"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	image := `"ghcr.io/getcanyon-ai/canyon-identity@sha256:` + strings.Repeat("ab", 32) + `"`
	rolled := func(entries string) string { return `{"image":` + image + `,"rolled":[` + entries + `],"failed":0}` }
	for _, tc := range []struct {
		name, answer string
		status       int
		ok           bool
	}{
		{"the fixture", string(fixture.Sidecars), 200, true},
		{"nothing rolled", rolled(""), 200, true},
		{"an executor without the call", `{"error":"not found"}`, 404, false},
		{"a key the contract does not name", `{"image":` + image + `,"rolled":[],"failed":0,"skipped":1}`, 200, false},
		{"something after the answer", rolled("") + ` {"image":""}`, 200, false},
		{"a stray brace after the answer", rolled("") + `}`, 200, false},
		{"a stray bracket after the answer", rolled("") + `]`, 200, false},
		{"no rolled", `{"image":` + image + `,"failed":0}`, 200, false},
		{"rolled null", `{"image":` + image + `,"rolled":null,"failed":0}`, 200, false},
		{"no failed", `{"image":` + image + `,"rolled":[]}`, 200, false},
		{"failed below 0", `{"image":` + image + `,"rolled":[],"failed":-1}`, 200, false},
		{"an empty image", `{"image":"","rolled":[],"failed":0}`, 200, false},
		{"an image by tag", `{"image":"ghcr.io/getcanyon-ai/canyon-identity:v1.4","rolled":[],"failed":0}`, 200, false},
		{"an image by a short digest", `{"image":"ghcr.io/getcanyon-ai/canyon-identity@sha256:abab","rolled":[],"failed":0}`, 200, false},
		{"an image by tag and digest", `{"image":"ghcr.io/getcanyon-ai/canyon-identity:v1.4@sha256:` + strings.Repeat("ab", 32) + `","rolled":[],"failed":0}`, 200, false},
		{"a target out of shape", rolled(`{"app":"claims-portal","target":"staging","roll":"4f1c0b9a"}`), 200, false},
		{"an app out of shape", rolled(`{"app":"Claims Portal","target":"prod","roll":"4f1c0b9a"}`), 200, false},
		{"an environment without its target", rolled(`{"app":"claims-portal","roll":"4f1c0b9a"}`), 200, false},
		{"an environment without its app", rolled(`{"target":"prod","roll":"4f1c0b9a"}`), 200, false},
		{"an environment without its roll", rolled(`{"app":"claims-portal","target":"prod"}`), 200, false},
		{"a roll out of shape", rolled(`{"app":"claims-portal","target":"prod","roll":"r1"}`), 200, false},
		{"an environment twice", rolled(`{"app":"claims-portal","target":"prod","roll":"4f1c0b9a"},{"app":"claims-portal","target":"prod","roll":"0000aaaa"}`), 200, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/sidecars" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.answer)
		}))
		res, err := (&Client{Base: srv.URL}).Sidecars(t.Context())
		srv.Close()
		if (err == nil) != tc.ok || !tc.ok && (res.Image != "" || res.Rolled != nil) {
			t.Errorf("%s: %+v %v, want taken %v", tc.name, res, err, tc.ok)
		}
	}
}

// GET /platform's answer is taken only in the contract's shape (contracts/executor.md, GET /platform; #2528): the
// fixture's platform_result is read as it is, asked by the image's digest, and an answer out of shape, or any
// status but 200 — an older executor's or the Docker executor's 404 — is an error, so no release names it.
func TestTheClientTakesOnlyAContractShapedPlatform(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		Platform json.RawMessage `json:"platform_result"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	image := "123456789012.dkr.ecr.eu-central-1.amazonaws.com/canyon/claims-portal@sha256:" + strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, answer string
		status       int
		ok           bool
	}{
		{"the fixture", string(fixture.Platform), 200, true},
		{"an executor without the call", `{"error":"no such call"}`, 404, false},
		{"an image it could not read", `{"error":"the image … 's architecture could not be read, so Canyon runs it nowhere: image index"}`, 502, false},
		{"a key the contract does not name", `{"os":"linux","architecture":"arm64","variant":"v8"}`, 200, false},
		{"something after the answer", `{"os":"linux","architecture":"arm64"} {}`, 200, false},
		{"another os", `{"os":"windows","architecture":"amd64"}`, 200, false},
		{"no architecture", `{"os":"linux","architecture":""}`, 200, false},
		{"an architecture out of shape", `{"os":"linux","architecture":"arm64/v8"}`, 200, false},
		{"no os", `{"architecture":"arm64"}`, 200, false},
	} {
		var asked string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/platform" {
				http.NotFound(w, r)
				return
			}
			asked = r.URL.Query().Get("image")
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.answer)
		}))
		res, err := (&Client{Base: srv.URL}).Platform(t.Context(), image)
		srv.Close()
		if (err == nil) != tc.ok || asked != image || tc.ok && (res.OS != "linux" || res.Architecture != "arm64") || !tc.ok && res != (Platform{}) {
			t.Errorf("%s: %+v %v, asked of %q, want taken %v", tc.name, res, err, asked, tc.ok)
		}
	}
}

// GET /platform's 404 reason image_gone (the contract's platform_gone) is ErrNoImage, which the server reads as /run's
// image gone; any other 404 — an older executor's "no such call", the Docker executor's, a proxy's page — is an error,
// never an image gone, so a rollback never forgets a kept image an old executor could not look at (#2528).
func TestOnlyPlatformsImageGoneIsNoImage(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		Gone json.RawMessage `json:"platform_gone"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	for _, tc := range []struct {
		name, answer string
		gone         bool
	}{
		{"the contract's image gone", string(fixture.Gone), true},
		{"an older executor's no such call", `{"error":"no such call: POST /build, POST /run, POST /stop, POST /forget, POST /scan, POST /rebase, GET /builder, GET /running, POST /running or GET /logs"}`, false},
		{"another reason", `{"error":"gone","reason":"elsewhere"}`, false},
		{"a mux's plain 404", "404 page not found\n", false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, tc.answer)
		}))
		_, err := (&Client{Base: srv.URL}).Platform(t.Context(), "123456789012.dkr.ecr.eu-central-1.amazonaws.com/canyon/claims-portal@sha256:"+strings.Repeat("ab", 32))
		srv.Close()
		if err == nil || errors.Is(err, ErrNoImage) != tc.gone {
			t.Errorf("%s: %v, want the image gone %v", tc.name, err, tc.gone)
		}
	}
}

// A build the executor went away under — the connection refused, the stream cut short or closed before its
// final status — is Gone, which the build step asks once more (#2671); a build the executor answered, its
// final status a failure, never is.
func TestABuildTheExecutorWentAwayUnderIsGone(t *testing.T) {
	build := func(base string) error {
		_, err := (&Client{Base: base}).Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"},
			bytes.NewReader(tarOf(t, map[string]string{"a": "1"})), func(context.Context, string) error { return nil })
		return err
	}
	answering := func(cut bool, rows ...string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, row := range rows {
				_, _ = io.WriteString(w, row+"\n")
			}
			w.(http.Flusher).Flush()
			if cut {
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	refused := httptest.NewServer(http.NotFoundHandler())
	refused.Close()
	var g *Gone
	for name, base := range map[string]string{
		"the stream cut before its final status":    answering(true, `{"line":"===> BUILDING"}`),
		"the stream closed before its final status": answering(false, `{"line":"===> BUILDING"}`),
		"the connection refused":                    refused.URL,
	} {
		if err := build(base); !errors.As(err, &g) {
			t.Errorf("%s: %v, want Gone", name, err)
		}
	}
	if err := build(answering(false, `{"error":"the build step exited 51"}`)); errors.As(err, &g) {
		t.Errorf("a build the executor answered failed is the package's, never Gone: %v", err)
	}
}
