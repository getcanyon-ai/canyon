package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	rebaseOld = "sha256:4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c" // the contract fixture's image
	rebaseNew = "sha256:6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a6a"
	rebaseRun = "sha256:5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f" // canyon-run on the daemon
)

func layer(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// inspected is docker image inspect's {{json .}} of a built app image: its layers bottom first, the
// run image's top one first among them, on the run image whose ID is run, under the labels pack writes —
// the run image named canyon-run:noble, as a build names it.
func inspected(t *testing.T, top, run string, labels map[string]string, layers ...string) string {
	t.Helper()
	l := map[string]string{
		lifecycleLabel:                 `{"runImage":{"topLayer":"` + top + `","reference":"` + strings.TrimPrefix(run, "sha256:") + `","image":"canyon-run:noble"}}`,
		"io.buildpacks.stack.id":       "io.buildpacks.stacks.noble",
		"io.buildpacks.build.metadata": `{"buildpacks":[{"id":"paketo-buildpacks/node-engine"}],"launcher":{"version":"0.21.20"}}`,
	}
	for k, v := range labels {
		if v == "" {
			delete(l, k)
		} else {
			l[k] = v
		}
	}
	b, err := json.Marshal(map[string]any{"Id": "x", "Config": map[string]any{"Labels": l, "Entrypoint": []string{"/cnb/process/web"}, "Cmd": nil, "Env": []string{"PATH=/cnb/process:/usr/bin", "NODE_ENV=production"}, "WorkingDir": "/workspace", "User": "1002:1001"},
		"RootFS": map[string]any{"Type": "layers", "Layers": layers}})
	must(t, err)
	return string(b)
}

// rebaseFakes is an Executor whose docker holds this app's image rebaseOld, on an older canyon-run, the
// current canyon-run as rebaseRun on the noble stack, and after pack's rebase rebaseNew under the fresh tag,
// on rebaseRun: old.json and new.json in dir are the two images' inspect, run-labels the run image's
// labels. Each binary records its argv.
func rebaseFakes(t *testing.T) (*Executor, string) {
	t.Helper()
	e, dir := fakes(t)
	runImage.id = ""
	must(t, os.WriteFile(filepath.Join(dir, "old.json"), []byte(inspected(t, layer('1'), layer('1'), nil, layer('1'), layer('2'), layer('3'))), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "new.json"), []byte(inspected(t, layer('5'), rebaseRun, nil, layer('5'), layer('2'), layer('3'))), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "run-labels"), []byte(`{"io.buildpacks.stack.id":"io.buildpacks.stacks.noble"}`), 0o600))
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
case "$*" in
*Repository*) echo "` + rebaseOld + ` canyon/claims-portal:pkg_7d3e8f-abcd";;
"image inspect --format {{.Id}} canyon-run:"*|"image inspect --format {{.Id}} registry.example/golden/run@"*) echo ` + rebaseRun + `;;
"image inspect --format {{json .Config.Labels}} ` + rebaseRun + `") cat "` + dir + `/run-labels";;
"image inspect --format {{json .}} ` + rebaseOld + `") cat "` + dir + `/old.json";;
"image inspect --format {{.Id}} canyon/claims-portal:"*) echo ` + rebaseNew + `;;
"image inspect --format {{json .}} ` + rebaseNew + `") cat "` + dir + `/new.json";;
"tag "*|"image rm "*) ;;
*) echo "unexpected: $*" >&2; exit 1;;
esac
`
	must(t, os.WriteFile(e.Docker, []byte(docker), 0o700)) //nolint:gosec // test fake
	return e, dir
}

func rebaseRequest() RebaseRequest {
	return RebaseRequest{App: "claims-portal", Package: "pkg_7d3e8f", Image: "canyon/claims-portal@" + rebaseOld, RunImage: rebaseRun}
}

func postRebase(t *testing.T, e *Executor, req any) (int, []byte) {
	t.Helper()
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	body, err := json.Marshal(req)
	must(t, err)
	resp, err := srv.Client().Post(srv.URL+"/rebase", "application/json", bytes.NewReader(body))
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	must(t, err)
	return resp.StatusCode, out.Bytes()
}

func schemaOf(t *testing.T, definition string) *jsonschema.Schema {
	t.Helper()
	at := "#/definitions/" + definition
	if definition == "rebase_busy" { // the fixture's own: busy, never a scan's reason
		at = "#/properties/rebase_busy"
	}
	s, err := jsonschema.NewCompiler().Compile("../contracts/schema/executor.schema.json" + at)
	must(t, err)
	return s
}

func validates(t *testing.T, s *jsonschema.Schema, body []byte) {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	must(t, err)
	if err := s.Validate(v); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
}

// contracts/executor.md, POST /rebase on Docker: the image is tagged with a fresh tag, pack rebases that tag
// onto canyon-run's tag with --pull-policy never, and the answer is the new image ID with each image's facts
// as docker image inspect has them — the run image's layers below, the app's above, digests of the build
// metadata label and of the five config fields, never their values.
func TestRebaseDockerAnswersTheFactsOfBothImages(t *testing.T) {
	e, dir := rebaseFakes(t)
	status, body := postRebase(t, e, rebaseRequest())
	if status != http.StatusOK {
		t.Fatalf("POST /rebase: %d %s", status, body)
	}
	validates(t, schemaOf(t, "rebase_result"), body)
	var res RebaseResult
	must(t, json.Unmarshal(body, &res))
	meta := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(`{"buildpacks":[{"id":"paketo-buildpacks/node-engine"}],"launcher":{"version":"0.21.20"}}`)))
	config := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(`{"Cmd":null,"Entrypoint":["/cnb/process/web"],"Env":["PATH=/cnb/process:/usr/bin","NODE_ENV=production"],"User":"1002:1001","WorkingDir":"/workspace"}`)))
	want := RebaseResult{Image: "canyon/claims-portal@" + rebaseNew, DurationMS: res.DurationMS,
		Before: RebaseFacts{DiffIDs: []string{layer('1'), layer('2'), layer('3')}, TopLayer: layer('1'), RunImage: layer('1'), StackID: "io.buildpacks.stacks.noble", BuildMetadata: meta, Config: config},
		After:  RebaseFacts{DiffIDs: []string{layer('5'), layer('2'), layer('3')}, TopLayer: layer('5'), RunImage: rebaseRun, StackID: "io.buildpacks.stacks.noble", BuildMetadata: meta, Config: config}}
	got, _ := json.Marshal(res)
	wanted, _ := json.Marshal(want)
	if !bytes.Equal(got, wanted) {
		t.Fatalf("the answer:\n%s\nwant\n%s", got, wanted)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))          //nolint:gosec // test dir
	packArgv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	fresh, _, _ := strings.Cut(strings.TrimPrefix(string(packArgv), "rebase\n"), "\n")
	if !strings.HasPrefix(fresh, "canyon/claims-portal:pkg_7d3e8f-") || fresh == "canyon/claims-portal:pkg_7d3e8f-abcd" ||
		!strings.Contains(string(argv), "tag\n"+rebaseOld+"\n"+fresh+"\n") || string(packArgv) != "rebase\n"+fresh+"\n--run-image\ncanyon-run:noble\n--pull-policy\nnever\n--no-color\n" {
		t.Fatalf("docker argv %q\npack argv %q\nwant the image tagged fresh and that tag rebased onto canyon-run:noble", argv, packArgv)
	}
}

// io.buildpacks.rebasable=false is refused, never forced (design/PATCHING.md §4.3): 409 not_rebasable,
// and nothing is tagged and pack never runs.
func TestRebaseRebasableFalseRefused(t *testing.T) {
	e, dir := rebaseFakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "old.json"), []byte(inspected(t, layer('1'), layer('1'), map[string]string{"io.buildpacks.rebasable": "false"}, layer('1'), layer('2'))), 0o600))
	status, body := postRebase(t, e, rebaseRequest())
	validates(t, schemaOf(t, "rebase_refused"), body)
	if status != http.StatusConflict || !strings.Contains(string(body), `"error":"not_rebasable"`) {
		t.Fatalf("a rebasable=false image: %d %s", status, body)
	}
	nothingWritten(t, dir)
}

// nothingWritten fails when a refused or failed /rebase tagged the app's image or ran pack.
func nothingWritten(t *testing.T, dir string) {
	t.Helper()
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if _, err := os.Stat(filepath.Join(dir, "pack-argv")); !os.IsNotExist(err) || strings.Contains(string(argv), "tag\n"+rebaseOld) {
		t.Fatalf("pack ran or docker tagged (%v): %q", err, argv)
	}
}

// Each other refusal is 409 with its own error, naming the label, repository or stack; a fact that cannot be
// read is a failed rebase, never one run blind; a request out of shape is 400 and an image the daemon does
// not hold 404, as /run's. None of them writes anything.
func TestRebaseRefusesNeverForces(t *testing.T) {
	for _, tc := range []struct {
		name, old, runLabels string
		req                  func(*RebaseRequest)
		status               int
		says                 string
	}{
		{name: "the run image moved since the plan", req: func(r *RebaseRequest) { r.RunImage = layer('9') }, status: 409, says: `"error":"run_image_moved"`},
		{name: "another repository", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + strings.Repeat("1", 64) + `","image":"registry.example/golden/run:1"}}`}, layer('1')), status: 409, says: `"error":"another_run_image","message":"the image was built onto registry.example/golden/run:1 and the run image is index.docker.io/library/canyon-run:noble`},
		{name: "canyon-run under its file's tag", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + strings.Repeat("1", 64) + `","image":"canyon-run:0123456789ab"}}`}, layer('1')), status: 409, says: `"error":"another_run_image","message":"the image was built onto index.docker.io/library/canyon-run:0123456789ab and the run image is index.docker.io/library/canyon-run:noble`},
		{name: "another stack", runLabels: `{"io.buildpacks.stack.id":"io.buildpacks.stacks.jammy"}`, status: 409, says: `"error":"another_stack","message":"the image is on stack \"io.buildpacks.stacks.noble\" and the run image on \"io.buildpacks.stacks.jammy\""`},
		{name: "a run image with no stack", runLabels: `{}`, status: 409, says: `"error":"another_stack"`},
		{name: "a run image name that is a flag", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + strings.Repeat("1", 64) + `","image":"--force"}}`}, layer('1')), status: 409, says: `"error":"another_run_image"`},
		{name: "rebasable neither true nor absent", old: inspected(t, layer('1'), layer('1'), map[string]string{"io.buildpacks.rebasable": "FALSE"}, layer('1')), status: 409, says: `"error":"not_rebasable","message":"the image's io.buildpacks.rebasable label is \"FALSE\"`},
		{name: "rebasable empty", old: inspected(t, layer('1'), layer('1'), map[string]string{"io.buildpacks.rebasable": " "}, layer('1')), status: 409, says: `"error":"not_rebasable"`},
		{name: "a run image reference that is no daemon ID", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"canyon-run@` + layer('1') + `","image":"canyon-run:noble"}}`}, layer('1')), status: 502, says: "not the daemon's image ID"},
		{name: "a run image reference with its prefix", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + layer('1') + `","image":"canyon-run:noble"}}`}, layer('1')), status: 502, says: "not the daemon's image ID"},
		{name: "no lifecycle metadata", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: ""}, layer('1')), status: 502, says: lifecycleLabel},
		{name: "lifecycle metadata naming no run image", old: inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + strings.Repeat("1", 64) + `"}}`}, layer('1')), status: 502, says: "names no run image"},
		{name: "no top layer", old: inspected(t, "", layer('1'), nil, layer('1')), status: 502, says: "top layer"},
		{name: "no build metadata", old: inspected(t, layer('1'), layer('1'), map[string]string{"io.buildpacks.build.metadata": ""}, layer('1')), status: 502, says: "io.buildpacks.build.metadata"},
		{name: "no stack label", old: inspected(t, layer('1'), layer('1'), map[string]string{"io.buildpacks.stack.id": ""}, layer('1')), status: 502, says: "io.buildpacks.stack.id"},
		{name: "no layers", old: inspected(t, layer('1'), layer('1'), nil), status: 502, says: "diff IDs"},
		{name: "a tag", req: func(r *RebaseRequest) { r.Image = "canyon/claims-portal:pkg_7d3e8f-abcd" }, status: 400},
		{name: "another app's image", req: func(r *RebaseRequest) { r.Image = "canyon/other@" + rebaseOld }, status: 400},
		{name: "no run_image", req: func(r *RebaseRequest) { r.RunImage = "" }, status: 400},
		{name: "a package out of shape", req: func(r *RebaseRequest) { r.Package = "../x" }, status: 400},
		{name: "an image the daemon does not hold", req: func(r *RebaseRequest) { r.Image = "canyon/claims-portal@" + rebaseNew }, status: 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, dir := rebaseFakes(t)
			if tc.old != "" {
				must(t, os.WriteFile(filepath.Join(dir, "old.json"), []byte(tc.old), 0o600))
			}
			if tc.runLabels != "" {
				must(t, os.WriteFile(filepath.Join(dir, "run-labels"), []byte(tc.runLabels), 0o600))
			}
			req := rebaseRequest()
			if tc.req != nil {
				tc.req(&req)
			}
			status, body := postRebase(t, e, req)
			if status != tc.status || !strings.Contains(string(body), tc.says) {
				t.Fatalf("%d %s, want %d saying %s", status, body, tc.status, tc.says)
			}
			if status == http.StatusConflict {
				validates(t, schemaOf(t, "rebase_refused"), body)
			}
			nothingWritten(t, dir)
		})
	}
}

// The image the request names keeps its own tag: the rebase adds one and removes none, so the server can
// /run either, and the tag cleanup after a /run treats the new tag as any other.
func TestRebaseOldTagSurvives(t *testing.T) {
	e, dir := rebaseFakes(t)
	if status, body := postRebase(t, e, rebaseRequest()); status != http.StatusOK {
		t.Fatalf("POST /rebase: %d %s", status, body)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	for _, line := range strings.Split(string(argv), "\n") {
		if line == "rm" || line == "rmi" || line == "untag" {
			t.Fatalf("a rebase removed an image: %q", argv)
		}
	}
}

// Every failure after the fresh tag is written removes it, and only it — pack failing, the new image's ID
// unread or not an ID, its run image not the plan's, the caller gone — with the error as the executor got
// it, never a <nil>. The caller leaves once the step it cuts short has started, never on a clock: a step
// before it that takes its time on a busy host never takes the cancel in its place (#2876).
func TestRebaseRemovesTheFreshTagOnEveryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, pack, tag, newID, newRun, says string
		cancel                               bool
	}{
		{name: "pack fails", pack: "echo '===> REBASING'; echo 'ERROR: failed to rebase: image not found'; exit 1", says: "ERROR: failed to rebase: image not found"},
		{name: "the new ID unread", newID: "echo 'Error: No such image' >&2; exit 1", says: "image inspect canyon/claims-portal:pkg_7d3e8f-"},
		{name: "the new ID not an ID", newID: "echo garbage", says: `answered "garbage", not an image ID`},
		{name: "another run image under it", newRun: layer('9'), says: "the run image moved under pack"},
		{name: "the caller gone", pack: `: > "$REACHED"; exec sleep 5`, cancel: true, says: "pack rebase"},
		{name: "docker tag refused", tag: "echo 'Error response from daemon: refused' >&2; exit 1", says: "docker tag"},
		{name: "docker tag cut short by the caller", tag: `: > "$REACHED"; exec sleep 5`, cancel: true, says: "docker tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, dir := rebaseFakes(t)
			// reached is a FIFO the step the caller cuts short opens as it starts: opening it blocks until the
			// test reads it, so the cancel follows the step's start, whatever came before it.
			reached := filepath.Join(dir, "reached")
			tc.pack, tc.tag = strings.ReplaceAll(tc.pack, "$REACHED", reached), strings.ReplaceAll(tc.tag, "$REACHED", reached)
			if tc.pack != "" {
				must(t, os.WriteFile(e.Pack, []byte("#!/bin/sh\n"+tc.pack+"\n"), 0o700)) //nolint:gosec // test fake
			}
			if tc.newID != "" {
				docker, err := os.ReadFile(e.Docker) //nolint:gosec // test dir
				must(t, err)
				old := `"image inspect --format {{.Id}} canyon/claims-portal:"*) echo ` + rebaseNew + `;;`
				must(t, os.WriteFile(e.Docker, []byte(strings.Replace(string(docker), old, `"image inspect --format {{.Id}} canyon/claims-portal:"*) `+tc.newID+`;;`, 1)), 0o700)) //nolint:gosec // test fake
			}
			if tc.tag != "" {
				docker, err := os.ReadFile(e.Docker) //nolint:gosec // test dir
				must(t, err)
				must(t, os.WriteFile(e.Docker, []byte(strings.Replace(string(docker), `"tag "*|"image rm "*) ;;`, `"tag `+rebaseOld+` "*) `+tc.tag+`;; "tag "*|"image rm "*) ;;`, 1)), 0o700)) //nolint:gosec // test fake
			}
			if tc.newRun != "" {
				must(t, os.WriteFile(filepath.Join(dir, "new.json"), []byte(inspected(t, layer('5'), tc.newRun, nil, layer('5'), layer('2'), layer('3'))), 0o600))
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tc.cancel {
				// A busy host (#2876): the steps before the one the caller leaves in take their time. The run
				// image's ID, read before the tag, is held 400ms.
				docker, err := os.ReadFile(e.Docker) //nolint:gosec // test dir
				must(t, err)
				must(t, os.WriteFile(e.Docker, []byte(strings.Replace(string(docker), `registry.example/golden/run@"*) echo `, `registry.example/golden/run@"*) sleep 0.4; echo `, 1)), 0o700)) //nolint:gosec // test fake
				must(t, syscall.Mkfifo(reached, 0o600))
				go func() {
					if f, err := os.Open(reached); err == nil { //nolint:gosec // test dir; returns once the step has opened it to write
						_, _ = io.Copy(io.Discard, f)
						_ = f.Close()
					}
					cancel()
				}()
			}
			_, err := e.Rebase(ctx, rebaseRequest())
			if err == nil || !strings.Contains(err.Error(), tc.says) || strings.Contains(err.Error(), "<nil>") {
				t.Fatalf("%v, want an error saying %s", err, tc.says)
			}
			argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
			tagged := regexp.MustCompile(`tag\n` + rebaseOld + `\n(canyon/claims-portal:pkg_7d3e8f-[0-9a-f]{8})\n`).FindStringSubmatch(string(argv))
			if tagged == nil || !strings.HasSuffix(string(argv), "image\nrm\n"+tagged[1]+"\n") || strings.Contains(string(argv), "rm\ncanyon/claims-portal:pkg_7d3e8f-abcd") {
				t.Fatalf("docker argv %q: want the fresh tag removed last, and the image's own kept", argv)
			}
		})
	}
}

// A rebase counts against CANYON_EXECUTOR_BUILDS beside /build: past it, 503 busy with Retry-After, and
// nothing runs.
func TestRebaseSharesTheBuildLimit(t *testing.T) {
	saved := builds
	t.Cleanup(func() { builds = saved })
	builds = make(chan struct{}, 1)
	builds <- struct{}{} // a /build in flight
	e, dir := rebaseFakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	body, _ := json.Marshal(rebaseRequest())
	resp, err := srv.Client().Post(srv.URL+"/rebase", "application/json", bytes.NewReader(body))
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	validates(t, schemaOf(t, "rebase_busy"), out.Bytes())
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("past the build limit: %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), out.Bytes())
	}
	if argv, _ := os.ReadFile(filepath.Join(dir, "argv")); len(argv) != 0 { //nolint:gosec // test dir
		t.Fatalf("docker ran while busy: %q", argv)
	}
}

// The contract's own request decodes, and its fixture's answers are the shapes this executor writes: its
// after.run_image is the run_image its request planned and GET /builder answered, byte for byte, and the
// executor, given the fixture's request, answers the same.
func TestRebaseMatchesTheContractFixture(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		Builder struct {
			RunImage string `json:"run_image"`
		} `json:"builder_result"`
		Request RebaseRequest   `json:"rebase_request"`
		Result  json.RawMessage `json:"rebase_result"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	if fixture.Request != rebaseRequest() {
		t.Fatalf("the fixture's request %+v is not the one these tests send", fixture.Request)
	}
	var res RebaseResult
	dec := json.NewDecoder(bytes.NewReader(fixture.Result))
	dec.DisallowUnknownFields()
	must(t, dec.Decode(&res))
	back, err := json.Marshal(res)
	must(t, err)
	validates(t, schemaOf(t, "rebase_result"), back)
	if res.After.RunImage != fixture.Request.RunImage || fixture.Builder.RunImage != fixture.Request.RunImage {
		t.Fatalf("the fixture's run images: after %s, request %s, GET /builder %s: one form, one value", res.After.RunImage, fixture.Request.RunImage, fixture.Builder.RunImage)
	}
	e, _ := rebaseFakes(t)
	got, err := e.Rebase(t.Context(), fixture.Request)
	if err != nil || got.After.RunImage != fixture.Request.RunImage || !imageIDRe.MatchString(got.Before.RunImage) {
		t.Fatalf("the executor's run images for the fixture's request: %+v %v", got, err)
	}
}

// The new base is verified after pack: the rebased image's runImage.reference must be the run image's ID
// the plan named, else the rebase failed and its fresh tag is removed — never answered as the plan's.
func TestRebaseVerifiesTheRunImageAfterPack(t *testing.T) {
	e, dir := rebaseFakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "new.json"), []byte(inspected(t, layer('5'), layer('9'), nil, layer('5'), layer('2'), layer('3'))), 0o600))
	status, body := postRebase(t, e, rebaseRequest())
	validates(t, schemaOf(t, "rebase_failed"), body)
	if status != http.StatusBadGateway || !strings.Contains(string(body), "the run image moved under pack") {
		t.Fatalf("a rebase onto another run image than the plan's: %d %s", status, body)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "image\nrm\ncanyon/claims-portal:pkg_7d3e8f-") {
		t.Fatalf("the fresh tag was kept: %q", argv)
	}
}

// Under CANYON_IMAGE_RUN pack is handed that reference by digest, and an image built onto another digest of
// it is rebased: the lifecycle compares names with the digest stripped. An image built onto canyon-run is
// refused: another name.
func TestRebaseOntoCanyonImageRun(t *testing.T) {
	golden := "registry.example/golden/run@sha256:" + strings.Repeat("a", 64)
	builtOn := func(image string) string {
		return inspected(t, layer('1'), layer('1'), map[string]string{lifecycleLabel: `{"runImage":{"topLayer":"` + layer('1') + `","reference":"` + strings.Repeat("1", 64) + `","image":"` + image + `"}}`}, layer('1'), layer('2'), layer('3'))
	}
	e, dir := rebaseFakes(t)
	e.RunImage = golden
	must(t, os.WriteFile(filepath.Join(dir, "old.json"), []byte(builtOn("registry.example/golden/run@sha256:"+strings.Repeat("b", 64))), 0o600))
	if status, body := postRebase(t, e, rebaseRequest()); status != http.StatusOK {
		t.Fatalf("an image on an older digest of CANYON_IMAGE_RUN: %d %s", status, body)
	}
	packArgv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	if !strings.Contains(string(packArgv), "\n--run-image\n"+golden+"\n") {
		t.Fatalf("pack argv %q, want the run image %s", packArgv, golden)
	}
	e, dir = rebaseFakes(t)
	e.RunImage = golden
	must(t, os.WriteFile(filepath.Join(dir, "old.json"), []byte(builtOn("canyon-run:noble")), 0o600))
	if status, body := postRebase(t, e, rebaseRequest()); status != http.StatusConflict || !strings.Contains(string(body), `"error":"another_run_image"`) {
		t.Fatalf("an image on canyon-run under CANYON_IMAGE_RUN: %d %s", status, body)
	}
	nothingWritten(t, dir)
}

// LifecycleName compares as the lifecycle's ParseMaybe does: the digest stripped, the tag kept, latest
// when none.
func TestLifecycleNameIsTheLifecyclesComparison(t *testing.T) {
	for in, want := range map[string]string{
		"canyon-run:noble": "index.docker.io/library/canyon-run:noble",
		"canyon-run":       "index.docker.io/library/canyon-run:latest",
		"registry.example/golden/run@sha256:" + strings.Repeat("a", 64):   "registry.example/golden/run:latest",
		"registry.example/golden/run:1@sha256:" + strings.Repeat("a", 64): "registry.example/golden/run:1",
	} {
		if got, err := LifecycleName(in); got != want || err != nil {
			t.Errorf("LifecycleName(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := LifecycleName(""); err == nil {
		t.Error("an empty name compares as a name")
	}
}

// RunImageName reads the run image's name as the lifecycle's rebaser does: runImage.image (platform API
// 0.12 and later, pack's builds) before stack.runImage.image (the platform APIs before it, kpack 0.18's
// builds), and neither is no name.
func TestRunImageNameReadsBothPlatforms(t *testing.T) {
	for metadata, want := range map[string]string{
		`{"runImage":{"image":"canyon-run:noble"},"stack":{"runImage":{"image":"other/run"}}}`:                                                               "index.docker.io/library/canyon-run:noble",
		`{"runImage":{"topLayer":"sha256:1a","reference":"r"},"stack":{"runImage":{"image":"r.example/canyon-run@sha256:` + strings.Repeat("a", 64) + `"}}}`: "r.example/canyon-run:latest",
	} {
		if got, err := RunImageName(metadata); got != want || err != nil {
			t.Errorf("RunImageName(%s) = %q %v, want %q", metadata, got, err, want)
		}
	}
	for _, metadata := range []string{`{"runImage":{"topLayer":"sha256:1a"}}`, `{"stack":{"runImage":{}}}`, `not json`} {
		if got, err := RunImageName(metadata); err == nil {
			t.Errorf("RunImageName(%s) = %q, want no name", metadata, got)
		}
	}
}

// GET /builder answers run_image, the run image's ID on the daemon: canyon-run's, built first and named
// canyon-run:noble, else CANYON_IMAGE_RUN's, pulled first when the daemon has none and then read by that
// reference — its digest — never by a tag; a run image that cannot be read is a 502.
func TestGetBuilderAnswersTheRunImage(t *testing.T) {
	golden := "registry.example/golden/run@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name, runImage, docker string
		status                 int
	}{
		{name: "canyon-run", docker: `case "$*" in "image inspect --format {{.Id}} canyon-run:"*) echo ` + rebaseRun + `;; "tag ` + rebaseRun + ` canyon-run:noble") ;; *) exit 1;; esac`, status: 200},
		{name: "CANYON_IMAGE_RUN on the daemon", runImage: golden, docker: `[ "$*" = "image inspect --format {{.Id}} ` + golden + `" ] && { echo ` + rebaseRun + `; exit 0; }; exit 1`, status: 200},
		{name: "CANYON_IMAGE_RUN pulled first", runImage: golden, docker: `[ "$*" = "pull --quiet ` + golden + `" ] && { : > "$0.pulled"; exit 0; }; [ "$*" = "image inspect --format {{.Id}} ` + golden + `" ] && [ -e "$0.pulled" ] && { echo ` + rebaseRun + `; exit 0; }; exit 1`, status: 200},
		{name: "CANYON_IMAGE_RUN not pullable", runImage: golden, docker: `echo 'manifest unknown' >&2; exit 1`, status: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, dir := fakes(t)
			runImage.id = ""
			e.Builder, e.RunImage = "registry.example/builder@sha256:"+strings.Repeat("c", 64), tc.runImage
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + dir + "/argv\"\n[ \"$*\" = \"image inspect --format {{.Id}} " + e.Builder + "\" ] && { echo sha256:" + strings.Repeat("c", 64) + "; exit 0; }\n" + tc.docker + "\n"
			must(t, os.WriteFile(e.Docker, []byte(script), 0o700)) //nolint:gosec // test fake
			srv := httptest.NewServer(Handler(e))
			defer srv.Close()
			resp, err := srv.Client().Get(srv.URL + "/builder")
			must(t, err)
			defer func() { _ = resp.Body.Close() }()
			var out bytes.Buffer
			_, _ = out.ReadFrom(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("%d %s, want %d", resp.StatusCode, out.Bytes(), tc.status)
			}
			if tc.status == 200 {
				validates(t, schemaOf(t, "builder_result"), out.Bytes())
				if !strings.Contains(out.String(), `"run_image":"`+rebaseRun+`"`) {
					t.Fatalf("%s, want run_image %s", out.Bytes(), rebaseRun)
				}
			}
		})
	}
}

// Under CANYON_IMAGE_RUN a build hands pack that reference as the run image, and a builder on another stack
// than it fails the build before pack runs, naming CANYON_IMAGE_RUN (contracts/executor.md, Every app runs
// on Canyon's run image).
func TestBuildOntoCanyonImageRun(t *testing.T) {
	golden := "registry.example/golden/run@sha256:" + strings.Repeat("a", 64)
	for _, stack := range []string{"io.buildpacks.stacks.noble", "io.buildpacks.stacks.jammy"} {
		e, dir := fakes(t)
		e.RunImage = golden
		must(t, os.WriteFile(e.Docker, []byte(floored("#!/bin/sh\ncase \"$*\" in *stack.id*"+golden+") echo "+stack+";; *stack.id*) echo io.buildpacks.stacks.noble;; *) echo sha256:"+strings.Repeat("ab", 32)+";; esac\n")), 0o700)) //nolint:gosec // test fake
		_, err := e.Build(t.Context(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
		packArgv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
		if stack == "io.buildpacks.stacks.noble" && (err != nil || !strings.Contains(string(packArgv), "\n--run-image\n"+golden+"\n")) {
			t.Fatalf("a build under CANYON_IMAGE_RUN: %v, pack argv %q", err, packArgv)
		}
		if stack != "io.buildpacks.stacks.noble" && (err == nil || !strings.Contains(err.Error(), "CANYON_IMAGE_RUN "+golden+` is "io.buildpacks.stacks.jammy"`) || len(packArgv) != 0) {
			t.Fatalf("a run image on another stack: %v, pack argv %q", err, packArgv)
		}
	}
}

// The server's client reads POST /rebase as the handler answers it: a refusal is *Refused with its reason, a
// busy executor *Busy, and a refusal the contract does not name an error, never a refusal for good.
func TestRebaseClientReadsTheHandlersAnswers(t *testing.T) {
	var servers []*httptest.Server
	answer := func(err error) *Client {
		srv := httptest.NewServer(RebaseHandler(func(context.Context, RebaseRequest) (RebaseResult, error) {
			return RebaseResult{Image: "canyon/claims-portal@" + rebaseNew}, err
		}))
		servers = append(servers, srv)
		t.Cleanup(srv.Close)
		return &Client{Base: srv.URL}
	}
	if res, err := answer(nil).Rebase(context.Background(), rebaseRequest()); err != nil || res.Image != "canyon/claims-portal@"+rebaseNew {
		t.Fatalf("a rebase: %+v %v", res, err)
	}
	var refused *Refused
	if _, err := answer(&Refused{"another_stack", "the image is on stack \"a\" and the run image on \"b\""}).Rebase(context.Background(), rebaseRequest()); !errors.As(err, &refused) || refused.Reason != "another_stack" {
		t.Fatalf("a refusal: %v", err)
	}
	if _, err := answer(&Refused{"forced", "no"}).Rebase(context.Background(), rebaseRequest()); err == nil || errors.As(err, &refused) {
		t.Fatalf("a refusal the contract does not name: %v", err)
	}
	if _, err := answer(fmt.Errorf("%w: canyon/claims-portal@%s", ErrNoImage, rebaseOld)).Rebase(context.Background(), rebaseRequest()); !errors.Is(err, ErrNoImage) {
		t.Fatalf("an image not there: %v", err)
	}
	// A handler frees its slot after it has answered, reading builds: every server closes, which waits for
	// its handlers, before builds is swapped, and the busy one's closes before builds is restored (#2402).
	for _, srv := range servers {
		srv.Close()
	}
	saved := builds
	t.Cleanup(func() { builds = saved })
	builds = make(chan struct{}, 1)
	builds <- struct{}{}
	var busy *Busy
	if _, err := answer(nil).Rebase(context.Background(), rebaseRequest()); !errors.As(err, &busy) || busy.RetryAfter != 10*time.Second {
		t.Fatalf("past the build limit: %v", err)
	}
}
