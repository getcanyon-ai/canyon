// Package executortest is a fake executor over httptest speaking contracts/executor.md: the
// json part first, the tree second, two build-log lines then the result — or the executor's
// refusal — /scan, /rebase, /run, /stop, /forget, /probe, PUT /access, GET /platform, POST /callers and POST /networks/recreate. The runs workflow tests and the server's route tests share it.
package executortest

import (
	"archive/tar"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
)

// Fake counts what it was asked and can be told to refuse a build or a run.
// Buildpacks is what a fake build ran unless BuiltWith says otherwise: the real Paketo group for a Node app,
// each named as the builder names it.
var Buildpacks = []executor.Buildpack{{ID: "paketo-buildpacks/node-engine", Name: "Paketo Buildpack for Node Engine"}, {ID: "paketo-buildpacks/npm-install", Name: "Paketo Buildpack for NPM Install"}}

type Fake struct {
	Builds, Runs, Logs atomic.Int32
	Stops              atomic.Int32
	FailBuild          bool
	HostFailBuild      bool                 // the build ends on the executor's own failure (contracts/executor.md), its build network's
	BuiltWith          []executor.Buildpack // what POST /build answers as the buildpacks that ran; nil, Buildpacks
	Undetected         []string             // not nil: POST /build answers a failed detect naming these (contracts/executor.md)
	UndetectedErrored  bool                 // and says a buildpack errored while detecting
	NoRoom             string               // set: POST /build answers the cluster had no room, this the scheduler's message
	FailRun            bool
	FailRunOf          string               // set, POST /run of this target alone answers 502, as FailRun
	NoImage            atomic.Int32         // POST /run answers 404, the image not on this host, this many times before it runs
	RefuseRun          atomic.Int32         // POST /run answers 400, a refusal, this many times before it runs
	PreviousGone       atomic.Pointer[bool] // POST /run answers it as previous_gone, Kubernetes' (contracts/executor.md); nil answers none
	TokenCallers       atomic.Bool          // POST /run answers token_callers true, Kubernetes' from layer 3; false answers none, as Docker and an older executor do
	Exited             *executor.Exited     // POST /run answers 502 with it: an app that did not stay up
	NoContainer        atomic.Bool          // GET /logs answers 404
	Follow             atomic.Bool          // GET /logs holds a following stream open after its two lines until the request ends
	Silent             atomic.Bool          // GET /logs holds the stream open having written nothing: a running, quiet container
	FailLogs           atomic.Bool          // GET /logs answers 500: any executor failure but a missing container
	FailStop           atomic.Bool          // POST /stop answers 502: docker failed
	FailRunning        atomic.Bool          // GET and POST /running answer 502: the executor cannot tell
	Forgets            atomic.Int32         // POST /forget calls
	FailForget         atomic.Bool          // POST /forget answers 502: docker failed
	Lines              atomic.Int32         // POST /build streams this many "line N" rows before its result; zero is the two below
	Scans              atomic.Int32         // POST /scan calls
	ScanFor            time.Duration        // each POST /scan takes this long to answer once accepted
	scanning           atomic.Int32
	MostScans          atomic.Int32 // the most POST /scan calls accepted at once
	Busy               atomic.Int32 // POST /scan answers 503 busy, Retry-After 1, this many times before it scans
	Stall              atomic.Bool  // POST /scan answers nothing until the caller gives up
	BusyReason         string       // set, each busy /scan answer carries it as its reason: "database", the tool's database downloading
	BuildBusy          atomic.Int32 // POST /build answers 503 busy, Retry-After 1, this many times before it builds
	DropBuilds         atomic.Int32 // POST /build cuts its stream after its lines, before a final status, this many times: an executor restarting under the build
	SARIF              string       // what POST /scan answers as the tool's SARIF; unset, a run with no result
	NotFinished        string       // set, POST /scan answers not_finished with it instead
	Reason             string       // with NotFinished no_room, the scheduler's message it carries
	Unloaded           []string     // what POST /scan answers beside the SARIF as the rules the tool could not load
	Unparsed           []string     // what POST /scan answers beside the SARIF as the files the tool did not read whole
	Probes             atomic.Int32 // POST /probe calls
	FailProbe          atomic.Bool  // POST /probe answers 502: the probe could not tell
	FailPlatform       atomic.Bool  // GET /platform answers 502: the image's architecture could not be read
	GonePlatform       atomic.Bool  // GET /platform answers 404 image_gone: the registry has no such image
	FailAccess         atomic.Bool  // PUT /access answers 502: the API server failed
	URL                string
	Image              string // set, POST /build answers it in place of canyon/<app>@Digest: an image a cluster can pull

	ImageScans    atomic.Int32 // POST /scan calls of target image naming an app
	RunImageScans atomic.Int32 // POST /scan calls of target image naming the run image, no app (contracts/executor.md, The run image)
	Rematches     atomic.Int32 // POST /scan calls of target sbom
	PackageScans  atomic.Int32 // POST /scan calls of target packages
	// ImageAnswer is what POST /scan of target image answers; unset, not_finished no_image. RunImageAnswer is
	// what it answers of the run image GET /builder answers, and of any other run image 409
	// run_image_moved; unset, not_finished no_image. Rematch is what target sbom answers of its part; unset,
	// not_finished no_image (contracts/executor.md).
	ImageAnswer    atomic.Pointer[executor.ScanResult]
	OnImageScan    atomic.Pointer[func(image string)] // set, each POST /scan of target image naming an app calls it with the image first, and answers once it returns
	RunImageAnswer atomic.Pointer[executor.ScanResult]
	Rematch        atomic.Pointer[func(part []byte) executor.ScanResult]
	// Packages is what POST /scan of target packages answers; unset, not_finished no_image.
	Packages atomic.Pointer[executor.ScanResult]

	Rebases      atomic.Int32                          // POST /rebase calls
	Rebased      atomic.Pointer[executor.RebaseResult] // what POST /rebase answers; unset, RebasedOf's
	RefuseRebase atomic.Pointer[string]                // set, POST /rebase answers 409 with this reason
	FailRebase   atomic.Int32                          // set, POST /rebase answers this status, 404 or 502, or with -1 drops the connection
	HoldRebase   atomic.Pointer[chan struct{}]         // set, POST /rebase, counted, waits until it is closed or its caller goes
	HoldBusy     atomic.Pointer[chan struct{}]         // set, a busy POST /scan, counted, answers only once it is closed or its caller goes

	Builder   atomic.Pointer[string] // GET /builder answers it; unset, "sha256:" and 64 "b"
	RunImage  atomic.Pointer[string] // GET /builder answers it as run_image; unset, "sha256:" and 64 "c"
	NoBuilder atomic.Bool            // GET /builder answers 404, as an executor from before it does
	Waited    atomic.Int64           // set, GET /builder answers it as waited_ms: it waited for Canyon's images (#2711)

	HoldAccess   atomic.Pointer[chan struct{}] // set, PUT /access waits, recorded, until it is closed
	SlowAccess   atomic.Int64                  // set, each PUT /access takes this many nanoseconds, recorded, before it answers
	HoldBuild    atomic.Pointer[chan struct{}] // set, POST /build, counted, waits until it is closed or its caller goes, and answers nothing
	OnBuild      atomic.Pointer[func()]        // set, POST /build calls it first: what changes while a run builds
	HoldRunning  atomic.Pointer[chan struct{}] // set, POST /running waits until it is closed
	SlowRunning  atomic.Int64                  // set, each POST /running takes this many nanoseconds before it answers
	HoldRun      atomic.Pointer[chan struct{}] // set, POST /run, counted, waits until it is closed, then answers as it would: a switch under way
	NoRunningOf  atomic.Bool                   // POST /running answers 404, as an executor from before it does
	RunningOfs   atomic.Int32                  // POST /running calls
	RunningAsked [][]executor.Container        // what each POST /running asked about, in the order it asked
	Why          []executor.Stopped            // POST /running asked why answers these as why containers stopped, asked or not
	Restarting   []executor.Restarting         // POST /running answers these as restarting, asked or not, and never as running
	Starting     []executor.Container          // POST /running answers these as starting, asked or not, and never as running
	WhyAsked     atomic.Int32                  // POST /running calls that asked why

	mu          sync.Mutex
	buildValues map[string]string    // what the last /build was handed
	run         executor.RunRequest  // the last /run
	logs        LogsRequest          // the last /logs
	stop        executor.StopRequest // the last /stop
	scan        executor.ScanRequest // the last /scan
	rules       map[string]string    // the last /scan's rules part, by file
	running     map[string]string    // <app>-<target> → the image a successful /run left running, until a /stop
	hold        chan struct{}        // set, POST /stop waits, counted, until it is closed
	probe       executor.ProbeResult // what POST /probe answers: enforced and ok until ProbeAnswers
	probed      [2]string            // the last /probe's domain and form
	arch        string               // what GET /platform answers as every image's architecture: this machine's until Platform
	platformed  []string             // every image GET /platform was asked of, in order
	accessed    []AccessPut          // every PUT /access, in order
	holds       []executor.Caller    // what POST /callers names: each whose address was asked, in this order
	CallersAsks atomic.Int32         // POST /callers calls
	FailCallers atomic.Bool          // POST /callers answers 502: the executor cannot tell

	nets         map[string]bool // an app's network, as Docker's executor keeps it: internal or not, made by its first /run, gone with its last /stop
	Recreates    atomic.Int32    // POST /networks/recreate calls
	FailRecreate atomic.Bool     // POST /networks/recreate answers 502 once it stopped the app's containers, its network gone
}

// Network is the app's network: whether it is internal, and whether it exists.
func (f *Fake) Network(app string) (internal, made bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	internal, made = f.nets[app]
	return internal, made
}

// Holds sets who POST /callers says holds an address: each of these whose address is asked, in this order,
// twice when named twice — as an executor that broke its contract would answer. One without a since holds
// it since heldSince, before any call a test makes.
func (f *Fake) Holds(callers ...executor.Caller) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds = slices.Clone(callers)
	for i := range f.holds {
		if f.holds[i].Since.IsZero() {
			f.holds[i].Since = heldSince
		}
	}
}

// heldSince is the since of a caller Holds was given none for.
var heldSince = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Start marks <app>-<target> running this app's image, as a successful /run leaves it: for a test
// whose rows were written without a run.
func (f *Fake) Start(app, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[app+"-"+target] = "canyon/" + app + "@" + Digest
}

// StartAs marks <app>-<target> running image, as a /run of it leaves it: for a test whose /run happened before
// a crash.
func (f *Fake) StartAs(app, target, image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running[app+"-"+target] = image
}

// Restart marks <app>-<target> started again since its /run, while the server may be reading it: POST /running
// answers it restarting.
func (f *Fake) Restart(app, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Restarting = append(f.Restarting, executor.Restarting{App: app, Target: target, Restarts: 1})
}

// Exit marks <app>-<target> exited, while the server may be reading it: POST /running answers it neither running
// nor restarting.
func (f *Fake) Exit(app, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, app+"-"+target)
}

// AccessPut is one PUT /access: the app, the target and the access file.
type AccessPut struct {
	App    string          `json:"app"`
	Target string          `json:"target"`
	Access json.RawMessage `json:"access"`
}

// Accessed is every PUT /access so far, in order, those answered 404 among them.
func (f *Fake) Accessed() []AccessPut {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accessed)
}

// ProbeAnswers sets what every later POST /probe answers.
func (f *Fake) ProbeAnswers(res executor.ProbeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probe = res
}

// Platform sets the architecture every later GET /platform answers, os linux: an image built for arch.
func (f *Fake) Platform(arch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.arch = arch
}

// Platformed is every image GET /platform was asked of, in order.
func (f *Fake) Platformed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.platformed)
}

// Probed is what the last /probe asked about: the domain and the form.
func (f *Fake) Probed() (domain, form string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probed[0], f.probed[1]
}

// HoldStop parks every POST /stop, once counted, until release is called (once or more): a stop mid-way.
func (f *Fake) HoldStop() (release func()) {
	hold := make(chan struct{})
	f.mu.Lock()
	f.hold = hold
	f.mu.Unlock()
	return sync.OnceFunc(func() {
		f.mu.Lock()
		f.hold = nil
		f.mu.Unlock()
		close(hold)
	})
}

// Stopped is the last /stop request: which container.
func (f *Fake) Stopped() executor.StopRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stop
}

// LogsRequest is what a /logs call was handed: the four parameters and nothing else.
type LogsRequest struct {
	App, Target string
	Tail        int
	Follow      bool
}

// Tailed is what the last /logs carried.
func (f *Fake) Tailed() LogsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs
}

// Handed is what the last /build and /run carried: the split the tests assert exactly.
func (f *Fake) Handed() (buildValues, env map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buildValues, f.run.Env
}

// Scanned is the last /scan request: which tool, with which settings, and its rule files by name.
func (f *Fake) Scanned() (executor.ScanRequest, map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scan, f.rules
}

// Ran is the last /run request: which container, which image.
func (f *Fake) Ran() executor.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.run
}

// HostFailLine is the executor's own failure a HostFailBuild ends on.
const HostFailLine = "build network: network create canyon-builds: exit status 1: could not find an available, non-overlapping IPv4 address pool"

// Digest is the fixed digest every successful build answers with.
var Digest = "sha256:" + strings.Repeat("a", 64)

// DBBuiltAt is when the fake's trivy and grype databases were built, as their /scan answer says.
const DBBuiltAt = "2026-09-24T06:12:40Z"

// New starts the fake; it closes with the test.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{running: map[string]string{}, nets: map[string]bool{}, probe: executor.ProbeResult{NetworkPolicy: "enforced", Certificate: "ok"}, arch: goruntime.GOARCH}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /build", func(w http.ResponseWriter, r *http.Request) {
		f.Builds.Add(1)
		if on := f.OnBuild.Load(); on != nil {
			(*on)()
		}
		if h := f.HoldBuild.Load(); h != nil { // a build cut off by a crash: it never answers
			select {
			case <-*h:
			case <-r.Context().Done():
			}
			return
		}
		if f.BuildBusy.Add(-1) >= 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "busy"})
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("build: %v", err)
			return
		}
		p, err := mr.NextPart()
		var req executor.BuildRequest
		if err != nil || p.FormName() != "json" || json.NewDecoder(p).Decode(&req) != nil {
			t.Errorf("the json part comes first: %v", err)
			return
		}
		p, err = mr.NextPart()
		if err != nil || p.FormName() != "tree" {
			t.Errorf("the tree part comes second: %v", err)
			return
		}
		if n, _ := io.Copy(io.Discard, p); n == 0 {
			t.Error("the tree is empty")
		}
		if req.BuildValues == nil {
			t.Error("build_values is absent; the contract carries the map, empty or not")
		}
		f.mu.Lock()
		f.buildValues = req.BuildValues
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		if n := int(f.Lines.Load()); n > 0 {
			for i := 1; i <= n; i++ {
				_ = enc.Encode(map[string]string{"line": fmt.Sprintf("line %d", i)})
			}
		} else {
			_ = enc.Encode(map[string]string{"line": "===> DETECTING"})
			_ = enc.Encode(map[string]string{"line": "===> BUILDING"})
		}
		if f.DropBuilds.Add(-1) >= 0 { // the connection closed with the chunked body unterminated: the client reads an unexpected EOF
			w.(http.Flusher).Flush()
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		}
		if f.FailBuild {
			_ = enc.Encode(map[string]string{"error": "pack: exit status 1"})
			return
		}
		if f.HostFailBuild {
			_ = enc.Encode(map[string]any{"error": HostFailLine, "own": true})
			return
		}
		if f.NoRoom != "" {
			_ = enc.Encode(map[string]string{"error": "the cluster had no room to run the build", "no_room": f.NoRoom})
			return
		}
		if f.Undetected != nil {
			line := map[string]any{"error": "no buildpack group passed detection", "builds": f.Undetected}
			if f.UndetectedErrored {
				line["errored"] = true
			}
			_ = enc.Encode(line)
			return
		}
		ran := Buildpacks
		if f.BuiltWith != nil {
			ran = f.BuiltWith
		}
		_ = enc.Encode(executor.BuildResult{Image: cmp.Or(f.Image, "canyon/"+req.App+"@"+Digest), DurationMS: 48000, Buildpacks: ran})
	})
	mux.HandleFunc("POST /rebase", func(w http.ResponseWriter, r *http.Request) {
		f.Rebases.Add(1)
		var req executor.RebaseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.HasPrefix(req.Image, "canyon/"+req.App+"@sha256:") || req.RunImage == "" {
			t.Errorf("rebase request: %+v (%v)", req, err)
		}
		if h := f.HoldRebase.Load(); h != nil {
			select {
			case <-*h:
			case <-r.Context().Done():
				return
			}
		}
		switch status := int(f.FailRebase.Load()); status {
		case 0:
		case -1:
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
			return
		default:
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("the rebase answered %d", status)})
			return
		}
		if reason := f.RefuseRebase.Load(); reason != nil {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": *reason, "message": "the image's io.buildpacks.rebasable label is \"false\": Canyon never forces a rebase"})
			return
		}
		res := RebasedOf(req.App, f.runImage())
		if a := f.Rebased.Load(); a != nil {
			res = *a
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /builder", func(w http.ResponseWriter, r *http.Request) {
		if f.NoBuilder.Load() {
			http.NotFound(w, r)
			return
		}
		builder := "sha256:" + strings.Repeat("b", 64)
		if b := f.Builder.Load(); b != nil {
			builder = *b
		}
		answer := map[string]any{"builder": builder, "run_image": f.runImage()}
		if waited := f.Waited.Load(); waited > 0 {
			answer["waited_ms"] = waited
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	mux.HandleFunc("POST /scan", func(w http.ResponseWriter, r *http.Request) {
		f.Scans.Add(1)
		if f.Stall.Load() { // the body read first: the server notices a caller gone only once it has
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		if f.Busy.Add(-1) >= 0 {
			if h := f.HoldBusy.Load(); h != nil {
				select {
				case <-*h:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			body := map[string]string{"error": "busy"}
			if f.BusyReason != "" {
				body["reason"] = f.BusyReason
			}
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("scan: %v", err)
			return
		}
		var req executor.ScanRequest
		p, err := mr.NextPart()
		if err == nil && p.FormName() == "json" {
			dec := json.NewDecoder(p)
			dec.DisallowUnknownFields() // as the executor: settings are the tool's allow-list, nothing else
			err = dec.Decode(&req)
		}
		if err == nil && req.Target != "" {
			f.target(t, w, req, mr)
			return
		}
		if err != nil || req.Settings == nil {
			t.Errorf("the json part comes first, settings and only the contract's keys: %+v %v", req, err)
			return
		}
		if p, err = mr.NextPart(); err != nil || p.FormName() != "tree" {
			t.Errorf("the tree part comes second: %v", err)
			return
		}
		if n, _ := io.Copy(io.Discard, p); n == 0 {
			t.Error("the tree is empty")
		}
		rules := map[string]string{}
		if p, err = mr.NextPart(); err == nil && p.FormName() == "rules" {
			for tr := tar.NewReader(p); ; {
				h, err := tr.Next()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(tr)
				rules[h.Name] = string(b)
			}
		}
		if (req.Tool == "opengrep") != (err == nil) { // as the executor: opengrep takes part rules, and no other tool a part
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request", "message": "after tree, opengrep takes part rules and no other tool takes a part"})
			return
		}
		f.mu.Lock()
		f.scan, f.rules = req, rules
		f.mu.Unlock()
		now := f.scanning.Add(1)
		defer f.scanning.Add(-1)
		for most := f.MostScans.Load(); now > most && !f.MostScans.CompareAndSwap(most, now); most = f.MostScans.Load() {
		}
		time.Sleep(f.ScanFor)
		res := executor.ScanResult{Tool: req.Tool, Image: "ghcr.io/aquasecurity/trivy@" + Digest, NotFinished: f.NotFinished, Reason: f.Reason}
		if res.NotFinished == "" { // unset, the tool found nothing: one run, no result, as a clean scan answers
			res.SARIF, res.Unloaded, res.Unparsed = json.RawMessage(cmp.Or(f.SARIF, `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"`+req.Tool+`"}},"results":[]}]}`)), f.Unloaded, f.Unparsed
			if req.Tool == "trivy" || req.Tool == "grype" { // their database's build date, on their SARIF answer alone (contracts/executor.md)
				res.DBBuiltAt = DBBuiltAt
			}
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		f.Runs.Add(1)
		if h := f.HoldRun.Load(); h != nil {
			<-*h
		}
		var req executor.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Target != "preview" && req.Target != "prod") || req.Network != "canyon-app-"+req.App || req.Env == nil || !strings.HasPrefix(req.Image, "canyon/"+req.App+"@sha256:") {
			t.Errorf("run request: %+v (%v)", req, err)
		}
		f.mu.Lock()
		f.run = req
		f.mu.Unlock()
		if f.RefuseRun.Add(-1) >= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "image must be this app's digest reference"})
			return
		}
		if f.NoImage.Add(-1) >= 0 {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such image: " + req.Image})
			return
		}
		f.mu.Lock()
		internal, made := f.nets[req.App]
		if !made { // a first /run makes the network: internal for one carrying egress (contracts/executor.md, Egress)
			internal, f.nets[req.App] = req.Egress != nil, req.Egress != nil
		}
		f.mu.Unlock()
		if internal != (req.Egress != nil) && req.FrontDoor == nil { // a /run never changes a network: only POST /networks/recreate does — Docker's alone: a Kubernetes /run, which carries the front door, writes the app's NetworkPolicy instead
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("canyon-app-%s is internal %t: a /run carrying egress %t is refused", req.App, internal, req.Egress != nil)})
			return
		}
		if f.FailRun || (f.FailRunOf != "" && f.FailRunOf == req.Target) {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker run: exit status 125"})
			return
		}
		if f.Exited != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(f.Exited)
			return
		}
		f.mu.Lock()
		f.running[req.App+"-"+req.Target] = req.Image
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(executor.RunResult{Container: req.App + "-" + req.Target, Addr: req.App + "-" + req.Target + ":3000", PreviousGone: f.PreviousGone.Load(), TokenCallers: f.TokenCallers.Load()})
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		hold := f.hold
		f.mu.Unlock()
		f.Stops.Add(1)
		if hold != nil {
			<-hold
		}
		var req executor.StopRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Target != "preview" && req.Target != "prod") {
			t.Errorf("stop request: %+v (%v)", req, err)
		}
		f.mu.Lock()
		f.stop = req
		delete(f.running, req.App+"-"+req.Target)
		if _, other := f.running[req.App+"-"+map[string]string{"preview": "prod", "prod": "preview"}[req.Target]]; !other { // the network goes with the app's last container
			delete(f.nets, req.App)
		}
		f.mu.Unlock()
		if f.FailStop.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker rm: exit status 1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /forget", func(w http.ResponseWriter, r *http.Request) {
		f.Forgets.Add(1)
		var req executor.ForgetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.App == "" {
			t.Errorf("forget request: %+v (%v)", req, err)
		}
		if f.FailForget.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "image rm: exit status 1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /probe", func(w http.ResponseWriter, r *http.Request) {
		f.Probes.Add(1)
		var req struct{ Domain, Form string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Domain == "" || req.Form == "" {
			t.Errorf("probe request: %+v (%v)", req, err)
		}
		f.mu.Lock()
		f.probed = [2]string{req.Domain, req.Form}
		res := f.probe
		f.mu.Unlock()
		if f.FailProbe.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the probe could not tell: its prober never reached its target's 8080 within 30 s"})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /platform", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.platformed = append(f.platformed, r.URL.Query().Get("image"))
		arch := f.arch
		f.mu.Unlock()
		if f.GonePlatform.Load() {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "image " + r.URL.Query().Get("image") + " is not in the registry", "reason": "image_gone"})
			return
		}
		if f.FailPlatform.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the image " + r.URL.Query().Get("image") + "'s architecture could not be read, so Canyon runs it nowhere: image index"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"os": "linux", "architecture": arch})
	})
	mux.HandleFunc("PUT /access", func(w http.ResponseWriter, r *http.Request) {
		var req AccessPut
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Target != "preview" && req.Target != "prod") || len(req.Access) == 0 {
			t.Errorf("access request: %+v (%v)", req, err)
		}
		f.mu.Lock()
		f.accessed = append(f.accessed, req)
		runs := strings.HasPrefix(f.running[req.App+"-"+req.Target], "canyon/"+req.App+"@")
		f.mu.Unlock()
		if hold := f.HoldAccess.Load(); hold != nil {
			<-*hold
		}
		time.Sleep(time.Duration(f.SlowAccess.Load()))
		switch {
		case f.FailAccess.Load():
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "configmap canyon-access-" + req.App + "-" + req.Target + ": the server is currently unable to handle the request"})
		case !runs:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such container: " + req.App + "-" + req.Target})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /running", func(w http.ResponseWriter, r *http.Request) {
		if f.FailRunning.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker inspect: exit status 1"})
			return
		}
		f.mu.Lock()
		image, ok := f.running[r.URL.Query().Get("app")+"-"+r.URL.Query().Get("target")]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"image": image})
	})
	mux.HandleFunc("POST /running", func(w http.ResponseWriter, r *http.Request) {
		f.RunningOfs.Add(1)
		if f.NoRunningOf.Load() {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such call: POST /build, POST /run, POST /stop, POST /forget, POST /scan, GET /running or GET /logs"})
			return
		}
		if hold := f.HoldRunning.Load(); hold != nil {
			<-*hold
		}
		time.Sleep(time.Duration(f.SlowRunning.Load()))
		if f.FailRunning.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker container inspect: exit status 1"})
			return
		}
		var req struct {
			Containers []executor.Container `json:"containers"`
			Why        bool                 `json:"why"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Containers) == 0 || len(req.Containers) > executor.MaxRunningOf {
			t.Errorf("POST /running asks for 1 to %d: %d (%v)", executor.MaxRunningOf, len(req.Containers), err)
		}
		f.mu.Lock()
		f.RunningAsked = append(f.RunningAsked, req.Containers)
		f.mu.Unlock()
		running := map[string]bool{}
		f.mu.Lock()
		for name := range f.running {
			running[name] = true
		}
		restarting, starting := append([]executor.Restarting{}, f.Restarting...), append([]executor.Container{}, f.Starting...)
		f.mu.Unlock()
		for _, r := range restarting {
			running[r.App+"-"+r.Target] = false
		}
		for _, s := range starting {
			running[s.App+"-"+s.Target] = false
		}
		stopped := []executor.Stopped{}
		if req.Why {
			f.WhyAsked.Add(1)
			stopped = append(stopped, f.Why...)
		}
		_ = json.NewEncoder(w).Encode(executor.Runs{Running: executor.Asked(req.Containers, running), Restarting: restarting, Starting: starting, Stopped: stopped})
	})
	mux.HandleFunc("POST /callers", func(w http.ResponseWriter, r *http.Request) {
		f.CallersAsks.Add(1)
		if f.FailCallers.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "the cluster API did not answer"})
			return
		}
		var req struct {
			Addresses []string `json:"addresses"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Addresses) == 0 || len(req.Addresses) > 1024 {
			t.Errorf("POST /callers asks for 1 to 1,024 addresses: %d (%v)", len(req.Addresses), err)
		}
		callers := []executor.Caller{}
		f.mu.Lock()
		for _, c := range f.holds {
			if slices.Contains(req.Addresses, c.Address) {
				callers = append(callers, c)
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"callers": callers})
	})
	mux.HandleFunc("POST /networks/recreate", func(w http.ResponseWriter, r *http.Request) {
		f.Recreates.Add(1)
		var req executor.RecreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.App == "" {
			t.Errorf("recreate request: %+v (%v)", req, err)
		}
		f.mu.Lock()
		delete(f.running, req.App+"-preview") // stopped, and left so: the server /runs each env again
		delete(f.running, req.App+"-prod")
		f.nets[req.App] = req.Internal
		if f.FailRecreate.Load() {
			delete(f.nets, req.App)
		}
		f.mu.Unlock()
		if f.FailRecreate.Load() {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "network create canyon-app-" + req.App + ": exit status 1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		f.Logs.Add(1)
		q := r.URL.Query()
		tail, err := strconv.Atoi(q.Get("tail"))
		follow := q.Get("follow")
		if err != nil || (q.Get("target") != "preview" && q.Get("target") != "prod") || (follow != "true" && follow != "false") || len(q) != 4 {
			t.Errorf("logs request carries app, target, tail and follow and nothing else: %v (%v)", q, err)
		}
		f.mu.Lock()
		f.logs = LogsRequest{App: q.Get("app"), Target: q.Get("target"), Tail: tail, Follow: follow == "true"}
		f.mu.Unlock()
		if f.FailLogs.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "docker is not answering"})
			return
		}
		if f.NoContainer.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no such container: " + q.Get("app") + "-" + q.Get("target")})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if f.Silent.Load() {
			w.WriteHeader(http.StatusOK)
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = fmt.Fprint(w, "GET /login 200 12ms\nGET /api/claims 200 31ms\n")
		if f.Follow.Load() && follow == "true" { // follow=false is the lines and the end of the body
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// runImage is the run_image GET /builder answers.
func (f *Fake) runImage() string {
	if r := f.RunImage.Load(); r != nil {
		return *r
	}
	return "sha256:" + strings.Repeat("c", 64)
}

// target answers an image or sbom scan (contracts/executor.md, POST /scan): an image target takes the json
// part alone, naming the app and its image; an sbom target names neither and takes part sbom.
func (f *Fake) target(t *testing.T, w http.ResponseWriter, req executor.ScanRequest, mr *multipart.Reader) {
	p, err := mr.NextPart()
	res := executor.ScanResult{Tool: "trivy", Image: "ghcr.io/aquasecurity/trivy@" + Digest, NotFinished: "no_image"}
	switch {
	case req.Target == "packages" && (req.Tool != "trivy" || req.Settings != nil || req.App == "" || req.Package == "" || req.Image != "" || err != nil || p.FormName() != "tree"):
		t.Errorf("a packages scan is trivy's, naming its app and package, with no settings, and takes part tree: %+v %v", req, err)
	case req.Target == "packages":
		f.PackageScans.Add(1)
		if n, _ := io.Copy(io.Discard, p); n == 0 {
			t.Error("the tree is empty")
		}
		if a := f.Packages.Load(); a != nil {
			res = *a
		}
	case req.Tool != "trivy" || req.Settings != nil || req.Package != "":
		t.Errorf("an %s scan is trivy's, with no settings and no package: %+v", req.Target, req)
	case req.Target == "image" && req.App == "" && err != io.EOF:
		t.Errorf("a run image scan takes no part: %+v %v", req, err)
	case req.Target == "image" && req.App == "" && req.Image != f.runImage():
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "run_image_moved", "message": "the run image is now " + f.runImage() + ", not " + req.Image + ": plan again"})
		return
	case req.Target == "image" && req.App == "":
		f.RunImageScans.Add(1)
		if a := f.RunImageAnswer.Load(); a != nil {
			res = *a
		}
	case req.Target == "image" && (err != io.EOF || !strings.Contains(req.Image, "@sha256:")):
		t.Errorf("an image scan names its app and image by digest, and takes no part: %+v %v", req, err)
	case req.Target == "image":
		f.ImageScans.Add(1)
		if on := f.OnImageScan.Load(); on != nil {
			(*on)(req.Image)
		}
		if a := f.ImageAnswer.Load(); a != nil {
			res = *a
		}
	case req.Target == "sbom" && (err != nil || p.FormName() != "sbom" || req.App != "" || req.Image != ""):
		t.Errorf("an sbom scan names no app or image, and takes part sbom: %+v %v", req, err)
	case req.Target == "sbom":
		f.Rematches.Add(1)
		part, _ := io.ReadAll(p)
		if m := f.Rematch.Load(); m != nil {
			res = (*m)(part)
		}
	default:
		t.Errorf("a scan of target %q", req.Target)
	}
	_ = json.NewEncoder(w).Encode(res)
}

// RebasedOf is a rebase of app's image onto runImage that only moved the run image: the golden's facts
// (contracts/testdata/patch/claims-portal.json), its rebased image canyon/<app>@sha256 and 64 "6a".
func RebasedOf(app, runImage string) executor.RebaseResult {
	h := func(s string) string { return "sha256:" + strings.Repeat(s, 32) }
	before := executor.RebaseFacts{DiffIDs: []string{h("1a"), h("2b"), h("3c")}, TopLayer: h("1a"), RunImage: h("4d"), StackID: "io.buildpacks.stacks.noble",
		BuildMetadata: h("7e"), Config: h("8f")}
	after := before
	after.DiffIDs, after.TopLayer, after.RunImage = []string{h("5e"), h("2b"), h("3c")}, h("5e"), runImage
	return executor.RebaseResult{Image: "canyon/" + app + "@" + h("6a"), Before: before, After: after, DurationMS: 2140}
}
