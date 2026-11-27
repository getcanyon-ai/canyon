// Package executor is the build executor of contracts/executor.md: a separate trust domain
// that turns a source tree into a buildpack image and runs an image as a container. It holds
// the Docker socket and a builder name and nothing else — no org config, no vault, no
// database — and it shells out to pack and docker rather than linking either.
package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/internal/names"
)

var (
	// ErrRequest marks a refusal of the caller's request, as opposed to a failure of pack or docker.
	ErrRequest = errors.New("bad request")
	// ErrNoContainer is a /logs call naming a container the daemon does not have.
	ErrNoContainer = errors.New("no such container")
	// ErrInUse is a /forget call while one of the app's containers still exists.
	ErrInUse = errors.New("in use")
	// ErrStale is a PUT /access whose updated_at is older than the one the ConfigMap holds (Kubernetes): 409,
	// and nothing changes.
	ErrStale = errors.New("stale")
	// ErrNoImage is a /run naming an image this host does not have: 404, and the server builds it again
	// (contracts/executor.md, /run).
	ErrNoImage = errors.New("no such image")
	// ErrBusy is a /scan of a tool whose database is being refreshed or not yet downloaded: the caller asks again.
	ErrBusy = errors.New("busy")
	// ErrQueued is a /scan of a tool with a database while another scan of it runs (Kubernetes): 503 busy
	// with no reason, and the caller asks again rather than waiting inside the call (contracts/executor.md).
	ErrQueued = errors.New("queued")
)

// Executor runs pack and docker. Zero values mean the binaries on PATH and the defaults below.
type Executor struct {
	Builder     string         // buildpack builder image, a noble one; "" = paketobuildpacks/ubuntu-noble-builder
	NodeBuilder string         // CANYON_BUILDER_NODE, Canyon's Node builder, which a Node app builds on; "" = none (nodebuilder.go)
	Project     string         // the compose project of this executor's own container (OwnProject), which what it makes for an app carries as canyon.project; "" = none
	RunImage    string         // CANYON_IMAGE_RUN, the run image by digest; "" = canyon-run, which the executor builds
	Lifecycle   string         // CANYON_IMAGE_LIFECYCLE, pack's lifecycle image by digest; "" = lifecycleImage
	Busybox     string         // CANYON_IMAGE_BUSYBOX, the port check's busybox by digest; "" = busyboxImage
	Pool        netip.Prefix   // CANYON_APPS_POOL, the prefix app networks' /24s are cut from; zero = 172.28.0.0/16
	Join        []string       // CANYON_EXECUTOR_JOIN, the containers every app network is shared with; nil = canyon-server, app-postgres
	Door        []string       // CANYON_EXECUTOR_EGRESS_JOIN, the containers every internal app network is shared with besides; nil = canyon-egress
	Holder      string         // CANYON_IMAGE_EGRESS_HOLDER, each build's holder by digest; "" = HolderImage
	Allow       []string       // CANYON_BUILD_ALLOW's entries, checked by AllowSetting: what a build reaches besides public 80 and 443
	AppsAllow   []string       // CANYON_APPS_ALLOW's entries, checked by AllowSetting: the private addresses an app on an open network reaches besides its own
	Canyon      []netip.Prefix // CANYON_EGRESS_POOLS, Canyon's networks, which a build's floor refuses before Allow; nil = canyonPools
	Pack        string         // pack binary; "" = "pack"
	Docker      string         // docker binary; "" = "docker"
	Settle      time.Duration  // how long a started container must keep running before /run answers; 0 = 10 s
	Roots       string         // the system roots a /run's egress.ca joins in system-and-canyon.pem; "" = SystemRoots

	collecting sync.WaitGroup // what answered calls left running: /run's image cleanups, the database refresh a /scan started
	nets       sync.Mutex     // the one lock over app networks: creating, joining, removing, and a /run's start onto one
	nonRoots   sync.Map       // builder image IDs whose user nonRoot read as no root's, so id -u runs once per image
	warned     sync.Map       // the apps deployed before the floor already logged, so each is logged once a process
	nodeReady  atomic.Int64   // when this executor pulled the Node builder, Unix nanoseconds: zero when it never had to
}

// BuildRequest is the json part of POST /build.
type BuildRequest struct {
	App         string            `json:"app"`
	Package     string            `json:"package"`
	BuildValues map[string]string `json:"build_values"`
}

// BuildResult is the last line of a /build response.
type BuildResult struct {
	Image      string      `json:"image"`
	DurationMS int64       `json:"duration_ms"`
	Buildpacks []Buildpack `json:"buildpacks"`
}

// RunRequest is the body of POST /run.
type RunRequest struct {
	App      string            `json:"app"`
	Target   string            `json:"target"` // preview | prod
	Image    string            `json:"image"`
	Env      map[string]string `json:"env"`
	Network  string            `json:"network"`
	Identity Identity          `json:"identity"`
	Keep     Digests           `json:"keep,omitempty"` // this app's images the cleanup after this /run spares
	Run      string            `json:"run,omitempty"`  // the server's run id, the label canyon.gate_run (contracts/executor.md, Egress)
	Egress   *Egress           `json:"egress,omitempty"`
	// Kubernetes only: the app's access file and how people reach it, and the apps database's CA
	// bundle, which the app finds at DatabaseCAPath (contracts/executor.md, On Kubernetes). The Docker
	// executor refuses each.
	Access     json.RawMessage `json:"access,omitempty"`
	FrontDoor  json.RawMessage `json:"front_door,omitempty"`
	DatabaseCA *string         `json:"database_ca,omitempty"`
}

// Egress is a /run's egress: present when the run's recorded spec says via canyon, so the app calls out
// through Canyon's Envoy at Proxy, every host in NoProxy reached directly (contracts/executor.md, Egress).
type Egress struct {
	Proxy   string   `json:"proxy"`
	NoProxy []string `json:"no_proxy"`
	CA      []Root   `json:"ca,omitempty"` // layer 3: the roots the app trusts for a destination Canyon adds a credential to (#2143)
}

// Root is one of egress.ca: a CA certificate, PEM, under its OpenSSL subject-hash file name (contracts/executor.md,
// egress.ca).
type Root struct {
	Name string `json:"name"`
	PEM  string `json:"pem"`
}

// Caller is one address POST /callers names: who holds it now (contracts/executor.md, POST /callers).
// Run is nil on a caller started without one; Since is when it came to hold the address, in UTC.
type Caller struct {
	Address string    `json:"address"`
	App     string    `json:"app"`
	Target  string    `json:"target"`
	Kind    string    `json:"kind"`
	Run     *string   `json:"run"`
	Since   time.Time `json:"since"`
}

// maxKeep bounds a /run's keep: a sanity bound, not a budget.
const maxKeep = 16

// Digests is a /run's keep: a list — never null — of at most maxKeep image digests, each sha256: and
// 64 lowercase hex digits, never a tag, which moves (contracts/executor.md, What keep spares). One that
// is not fails the decode, so /run is 400 before anything else is done.
type Digests []string

func (d *Digests) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err != nil || list == nil {
		return fmt.Errorf("%w: keep is a list of digests", ErrRequest)
	}
	if len(list) > maxKeep {
		return fmt.Errorf("%w: keep holds at most %d digests", ErrRequest, maxKeep)
	}
	for i, digest := range list {
		if !imageIDRe.MatchString(digest) {
			return fmt.Errorf("%w: keep[%d] is not a digest: sha256: and 64 lowercase hex digits", ErrRequest, i)
		}
	}
	*d = list
	return nil
}

// DatabaseCAPath is where a Kubernetes app finds /run's database_ca: the file its DATABASE_URL's
// sslrootcert names (contracts/executor.md, contracts/provisioner.md).
const DatabaseCAPath = "/etc/canyon-db/ca.pem"

// Identity is how the app verifies the signed-in person's token (design/PERMISSIONS.md): Run sets
// it in every container as CANYON_IDENTITY_ISSUER, _AUDIENCE and _JWKS_URL.
type Identity struct {
	Issuer   string `json:"issuer"`   // Canyon's public URL
	Audience string `json:"audience"` // the app id
	JWKSURL  string `json:"jwks_url"` // the server's key set, as the app's network reaches it
}

// StopRequest is the body of POST /stop.
type StopRequest struct {
	App    string `json:"app"`
	Target string `json:"target"` // preview | prod
}

// Container is one app's container, <app>-<target>: what POST /running asks about and answers.
type Container struct {
	App    string `json:"app"`
	Target string `json:"target"` // preview | prod
}

// Stopped is one asked of POST /running whose container is there and has exited — stopped, or restarting
// and its last exit known (#1597): its last exit code and its last line, raw — the executor holds no app's values, so the server masks it (contracts/executor.md).
type Stopped struct {
	App      string `json:"app"`
	Target   string `json:"target"`
	ExitCode int    `json:"exit_code"`
	Said     string `json:"said"`
}

// Restarting is one asked of POST /running whose container has been started again since /run started it
// and is up or waiting to start: never running, even while it is up (contracts/executor.md, Restarting).
type Restarting struct {
	App      string `json:"app"`
	Target   string `json:"target"`
	Restarts int    `json:"restarts"`
}

// Runs is POST /running's answer: those asked that run, those that keep restarting, and why of those
// asked that have exited. An older executor answers no Restarting.
type Runs struct {
	Running    []Container  `json:"running"`
	Restarting []Restarting `json:"restarting"`
	Starting   []Container  `json:"starting"` // a /run is starting it (contracts/executor.md, POST /running, Starting): never stopped
	Stopped    []Stopped    `json:"stopped"`
}

// SaidBytes bounds a Stopped's line: a longer one is dropped, never cut, so no cut line holds the
// start of a value the server's mask would not recognise.
const SaidBytes = 4 << 10

// MaxRunningOf is how many containers one POST /running asks about: a fleet page of 200 apps, both
// environments of each.
const MaxRunningOf = 400

// Sidecars is POST /sidecars' answer on Kubernetes: the image every app's canyon-identity is rolled onto,
// each environment a roll has moved onto it whose pod is up, and how many the executor left alone or could
// not move (contracts/executor.md, POST /sidecars).
type Sidecars struct {
	Image  string        `json:"image"`
	Rolled []SidecarRoll `json:"rolled"`
	Failed int           `json:"failed"`
}

// SidecarRoll is one environment a roll moved onto Sidecars' image; Roll names that roll, the same on
// every answer until the environment's next /run.
type SidecarRoll struct {
	App    string `json:"app"`
	Target string `json:"target"` // preview | prod
	Roll   string `json:"roll"`
}

// ForgetRequest is the body of POST /forget.
type ForgetRequest struct {
	App string `json:"app"`
}

// RecreateRequest is the body of POST /networks/recreate: Internal is the host's via being canyon.
type RecreateRequest struct {
	App      string `json:"app"`
	Internal bool   `json:"internal"`
}

// RunResult is the body of a /run response.
type RunResult struct {
	Container string           `json:"container"`
	Addr      string           `json:"addr"`
	Answered  *answered.Answer `json:"answered,omitempty"` // Kubernetes alone: what the app answered its sidecar's one GET /
	// PreviousGone is Kubernetes' alone: whether no pod of an earlier template remained before the answer; nil, not
	// answered, is not gone (contracts/executor.md, On Kubernetes)
	PreviousGone *bool `json:"previous_gone,omitempty"`
	// TokenCallers is Kubernetes' alone, from layer 3: true when the pods this /run started run as the env's own
	// ServiceAccount with its projected token and canyon-identity's forwarder, so their calls carry the token
	// (design/EGRESS.md §4.3); absent from Docker and from an executor before it
	TokenCallers bool `json:"token_callers,omitempty"`
}

// Exited is /run's 502 for an app that exited or restarted before it was up: the reason, and the
// app's own last lines, which are all that is left of it once its container is gone.
type Exited struct {
	Reason string `json:"error"`
	Output string `json:"output"`
}

func (e *Exited) Error() string { return e.Reason }

// ExitedLines is how many of the app's last lines an Exited carries, in at most exitedBytes; a Tail
// holds at most exitedRead of them while they are read, whatever one line of an app's weighs.
const (
	ExitedLines = 20
	exitedBytes = 8 << 10
	exitedRead  = 1 << 20
)

// Tail keeps the last exitedRead bytes written to it: an exiting app's lines, read with a bound.
type Tail struct {
	buf []byte
	cut bool
}

func (t *Tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - exitedRead; over > 0 {
		t.buf, t.cut = t.buf[over:], true
	}
	return len(p), nil
}

// Lines is what was kept, from a whole line on: a line the bound began inside is dropped, so no part
// of a value in it escapes the naming.
func (t *Tail) Lines() string {
	if !t.cut {
		return string(t.buf)
	}
	_, rest, _ := bytes.Cut(t.buf, []byte("\n"))
	return string(rest)
}

// NewExited is the answer for an app that stopped as it started: its last lines with each non-empty
// value of the call's env replaced by <NAME>, longest first, and each line of a multi-line value too,
// so a tail that starts inside one carries none of it, and a URL value's userinfo password on its own
// as <NAME password>, since an app that fails to connect logs it alone; then cut to the last exitedBytes.
func NewExited(reason, lines string, env map[string]string) *Exited {
	lines = Mask(lines, env)
	if len(lines) > exitedBytes { // after the naming, so no cut leaves part of a value behind
		lines = lines[len(lines)-exitedBytes:]
	}
	return &Exited{Reason: reason, Output: strings.ToValidUTF8(lines, "")}
}

// Mask replaces each non-empty value of env in text by <NAME>, the longest first, and each line of a
// multi-line value too, and a URL value's userinfo password on its own as <NAME password>: /run's rule,
// which the server applies to POST /running's lines too (contracts/executor.md).
func Mask(text string, env map[string]string) string {
	values := maskValues(env)
	slices.SortFunc(values, func(a, b maskValue) int { return cmp.Or(len(b.text)-len(a.text), strings.Compare(a.name, b.name)) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v.text, "<"+v.name+">")
	}
	return text
}

// Short says whether Mask would replace a text of env's shorter than n characters: where it stands in what was masked,
// the name in its place would give a short value away.
func Short(env map[string]string, n int) bool {
	return slices.ContainsFunc(maskValues(env), func(v maskValue) bool { return utf8.RuneCountInString(v.text) < n })
}

type maskValue struct{ text, name string }

// maskValues is each text Mask replaces, with the name it stands for.
func maskValues(env map[string]string) []maskValue {
	var values []maskValue
	for name, v := range env {
		for _, t := range append(strings.Split(v, "\n"), v) { // the value, and each line as it is and without its carriage return
			for _, t := range []string{t, strings.TrimSuffix(t, "\r")} {
				if t != "" {
					values = append(values, maskValue{t, name})
				}
			}
		}
		if u, err := url.Parse(v); err == nil && u.User != nil {
			if password, ok := u.User.Password(); ok && password != "" {
				values = append(values, maskValue{password, name + " password"})
			}
		}
	}
	return values
}

const appPort = "3000"

// NothingOnPort is the reason of a start whose app runs with nothing answering on port 3000, on
// either host (contracts/executor.md, /run).
const NothingOnPort = "the app is running and nothing answers on port " + appPort + ": it must listen on $PORT"

// busyboxImage tries an app's port 3000 from its network namespace (contracts/executor.md, /run):
// busybox 1.37.0's multi-arch index, pinned by digest per Canyon release, as /probe's on Kubernetes.
const busyboxImage = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

func (e *Executor) pack() string   { return or(e.Pack, "pack") }
func (e *Executor) docker() string { return or(e.Docker, "docker") }

func (e *Executor) pool() netip.Prefix {
	if e.Pool.IsValid() {
		return e.Pool
	}
	return netip.MustParsePrefix("172.28.0.0/16")
}
func (e *Executor) join() []string {
	if e.Join == nil {
		return []string{"canyon-server", "app-postgres"}
	}
	return e.Join
}

// joinTo is the containers n is shared with: the joined ones, and on an internal network the door, Envoy's
// namespace holder, which a network that is not internal never needs — so a holder that is down breaks no
// deploy but those routed through it (contracts/executor.md, Egress).
func (e *Executor) joinTo(n appNet) []string {
	if !n.Internal {
		return e.join()
	}
	if e.Door == nil {
		return append(slices.Clone(e.join()), "canyon-egress")
	}
	return append(slices.Clone(e.join()), e.Door...)
}
func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// runDockerfile is canyon-run, the image every app runs on (contracts/executor.md): the executor builds
// it as it builds opengrep's, tagged with the file's own hash, so a changed file is never served by an
// old build, and a pack that finds it gone looks in Docker Hub's official namespace, which only Docker
// publishes to.
//
//go:embed run.Dockerfile
var runDockerfile string

var runImage = &scanImage{dockerfile: runDockerfile, repo: "canyon-run:" + fmt.Sprintf("%x", sha256.Sum256([]byte(runDockerfile)))[:12]}

// runTag is the name pack is handed canyon-run by, which the executor moves onto each build of the file:
// the lifecycle records it in the image and rebases only onto it, so a name that moved with the file would
// leave every image built before unrebasable (contracts/executor.md, POST /rebase). It follows the
// distribution, so a file on another Ubuntu release takes another name and is never rebased onto.
const runTag = "canyon-run:noble"

// builderDockerfile is canyon-builder, the builder every app builds with when no CANYON_BUILDER is set
// (contracts/executor.md, Every app builds on Canyon's build image): built as canyon-run is, under its
// file's own hash, and named as built — a local image matches no entry of pack's trusted list.
//
//go:embed builder.Dockerfile
var builderDockerfile string

// builderImage is built once per Docker host, and the first deploy waits on it: its log line says what that
// costs, and its build has an hour, not dbDeadline, which a slow line runs past (#2697).
var builderImage = &scanImage{dockerfile: builderDockerfile, repo: "canyon-builder:" + fmt.Sprintf("%x", sha256.Sum256([]byte(builderDockerfile)))[:12],
	about: builderAbout, deadline: time.Hour}

// builderAbout is what the log says of canyon-builder as its build starts.
const builderAbout = ", Canyon's build image (once per Docker host, and again when a Canyon release changes it): " + builderSize

// builderSize is canyon-builder's download, its size on disk and how long it takes.
const builderSize = "up to a 1.4 GB download, 3.3 GB on disk, about 7 minutes at 25 Mbit/s and longer on a slower line"

// sameStack refuses a builder on another stack than the run image's: pack 0.40.3 only warns on the
// mismatch and builds onto the run image anyway (contracts/executor.md). A builder not on the daemon yet
// is pulled first, as pack would pull it.
func (e *Executor) sameStack(ctx context.Context, builder string) error {
	stack := func(image string) (string, error) {
		out, err := child(ctx, e.docker(), "image", "inspect", "--format", `{{index .Config.Labels "io.buildpacks.stack.id"}}`, image).Output()
		return strings.TrimSpace(string(out)), err
	}
	built, err := stack(builder)
	if err != nil {
		if out, err := child(ctx, e.docker(), "pull", "--quiet", builder).CombinedOutput(); err != nil {
			return fmt.Errorf("builder %s: %w: %s", builder, err, strings.TrimSpace(string(out)))
		}
		if built, err = stack(builder); err != nil {
			return fmt.Errorf("builder %s: %w", builder, err)
		}
	}
	runs, err := stack(e.runName())
	if err != nil {
		return fmt.Errorf("run image %s: %w", e.runName(), err)
	}
	switch {
	case built == runs:
		return nil
	case e.RunImage != "":
		return fmt.Errorf("CANYON_BUILDER %s builds on stack %q and CANYON_IMAGE_RUN %s is %q: name a builder and a run image on one stack", builder, built, e.RunImage, runs)
	}
	return fmt.Errorf("CANYON_BUILDER %s builds on stack %q and canyon-run is %q: name a builder on %s", builder, built, runs, runs)
}

// runName is the run image as pack is handed it: CANYON_IMAGE_RUN, else canyon-run's stable name.
func (e *Executor) runName() string { return or(e.RunImage, runTag) }

// RunImageID is the run image a /build builds onto and a /rebase rebases onto, by its ID on the daemon
// (contracts/executor.md, GET /builder): CANYON_IMAGE_RUN's, pulled first when the daemon has none — the
// daemon resolves a reference by digest only to an image it pulled as that digest — else canyon-run's,
// built first when the daemon has none and named runTag.
func (e *Executor) RunImageID(ctx context.Context) (string, error) {
	if e.RunImage != "" {
		return e.idOf(ctx, e.RunImage)
	}
	id, err := runImage.built(ctx, e.docker())
	if err != nil {
		return "", fmt.Errorf("canyon-run: %w", err)
	}
	if out, err := child(ctx, e.docker(), "tag", id, runTag).CombinedOutput(); err != nil {
		return "", fmt.Errorf("canyon-run: docker tag %s: %w: %s", runTag, err, strings.TrimSpace(string(out)))
	}
	return id, nil
}

// idOf is a pulled image's ID on the daemon, pulled first when the daemon has none.
func (e *Executor) idOf(ctx context.Context, image string) (string, error) {
	id := func() (string, error) {
		out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{.Id}}", image).Output()
		if s := strings.TrimSpace(string(out)); err != nil || !imageIDRe.MatchString(s) {
			return "", fmt.Errorf("image inspect: %v %q", err, s)
		}
		return strings.TrimSpace(string(out)), nil
	}
	if got, err := id(); err == nil {
		return got, nil
	}
	log.Printf("executor: pulling %s", image)
	began, stop := time.Now(), still("pulling "+image)
	out, err := child(ctx, e.docker(), "pull", "--quiet", image).CombinedOutput()
	if stop(); err != nil {
		err = fmt.Errorf("%s: %w: %s", image, err, strings.TrimSpace(string(out)))
		log.Printf("executor: %s failed after %s: %v", image, minutes(int(time.Since(began).Minutes())), err) // the log never just stops
		return "", err
	}
	got, err := id()
	if err != nil {
		return "", fmt.Errorf("%s: %w", image, err)
	}
	return got, nil
}

// builder is the builder a /build builds with: CANYON_BUILDER, else canyon-builder, built first when the daemon has
// none: with own, the one whose run image and two build variables are its own (ownBuilder), else builderImage's.
func (e *Executor) builder(ctx context.Context, own bool) (string, error) {
	if e.Builder != "" {
		return untrusted(e.Builder), nil
	}
	image := builderImage
	if own {
		image = ownBuilder
	}
	if _, err := image.built(ctx, e.docker()); err != nil {
		return "", builderFailed(err)
	}
	return image.repo, nil
}

// builderFailed names canyon-builder in its build's error, but for a build past its deadline, whose words do.
func builderFailed(err error) error {
	if slow := (*pastDeadline)(nil); errors.As(err, &slow) {
		return err
	}
	return fmt.Errorf("canyon-builder: %w", err)
}

// Prepared is when the later of Canyon's own images a /build needs, canyon-builder and canyon-run, was last built
// by this executor, zero when neither was: a GET /builder that began before then waited for it (contracts/executor.md,
// waited_ms; #2711). CANYON_BUILDER and CANYON_IMAGE_RUN are pulled, never built, so they count for nothing.
func (e *Executor) Prepared() time.Time {
	var at time.Time
	switch {
	case e.nodeOn(): // the Node builder's pull, in place of canyon-builder's build, which only a tree that needs it starts
		if n := e.nodeReady.Load(); n != 0 {
			at = time.Unix(0, n)
		}
	case e.Builder == "":
		at = builderImage.builtAt()
		if own := ownBuilder.builtAt(); own.After(at) {
			at = own
		}
	}
	if ran := runImage.builtAt(); e.RunImage == "" && ran.After(at) {
		at = ran
	}
	return at
}

// BuilderImage is the identity of the builder a /build now builds with: its image's ID on the daemon —
// CANYON_BUILDER's pulled first when the daemon has none, as a build pulls it, canyon-builder's own built
// first, which covers the canyon-builder it is built from — (contracts/executor.md, GET /builder).
func (e *Executor) BuilderImage(ctx context.Context) (string, error) {
	if e.nodeOn() {
		_, id, err := e.nodeBuilder(ctx)
		if err != nil {
			return "", fmt.Errorf("the Node builder %w", err)
		}
		return nodeIdentity(id), nil
	}
	if e.Builder == "" {
		id, err := ownBuilder.built(ctx, e.docker())
		if err != nil {
			return "", builderFailed(err)
		}
		return id, nil
	}
	id, err := e.idOf(ctx, untrusted(e.Builder))
	if err != nil {
		return "", fmt.Errorf("builder %w", err)
	}
	return id, nil
}

// lifecycleImage runs analyze, restore and export when the builder is untrusted. It is pinned by
// digest (buildpacksio/lifecycle:0.21.20, the multi-arch index) because pack otherwise pulls a tag
// named after whatever lifecycle the builder carries: canyon-builder's base, the pinned
// paketobuildpacks/ubuntu-noble-builder, carries 0.21.22, whose platform APIs 0.21.20's are.
// pack still asks its registry for its manifest on every build (contracts/executor.md).
const lifecycleImage = names.LifecycleRepository + "@sha256:ffdceee500cbd55cdaed0c05f63271d801d124e4c81cdb2e46ae958e98f4df91"

// untrusted names a Docker Hub builder with its registry. pack 0.40.3 trusts a builder whose name
// string-equals an entry of its built-in list (paketobuildpacks/builder-jammy-base,
// paketobuildpacks/ubuntu-noble-builder, heroku/builder:24, ...) whatever --trust-builder says, and
// a trusted builder runs every phase, app code included, in one container with the Docker socket.
// The qualified name is the same image and matches no entry, so pack runs the five-phase flow: only
// the lifecycle image's phases get the socket. Docker's rule: a first component with a "." or ":",
// or "localhost", is a registry host; anything else is on Docker Hub. The one listed builder off
// Docker Hub, gcr.io/buildpacks/builder:google-22, stays trusted if an operator configures it.
func untrusted(builder string) string {
	first, _, found := strings.Cut(builder, "/")
	switch {
	case !found:
		return "docker.io/library/" + builder
	case strings.ContainsAny(first, ".:") || first == "localhost":
		return builder
	}
	return "docker.io/" + builder
}

// Build untars tree into a private directory, runs pack, streams the build log to out, and
// returns the image by digest. BuildEnv of the build values is the build-time env for the buildpacks.
func (e *Executor) Build(ctx context.Context, req BuildRequest, tree io.Reader, out io.Writer) (BuildResult, error) {
	if !validApp(req.App) || !validPackage(req.Package) {
		return BuildResult{}, fmt.Errorf("%w: app must match %s and package %s (contracts/schema/executor.schema.json)", ErrRequest, appPattern, packagePattern)
	}
	if _, err := envPairs(BuildEnv(req.BuildValues), true); err != nil {
		return BuildResult{}, err
	}
	dir, err := os.MkdirTemp("", "canyon-build-")
	if err != nil {
		return BuildResult{}, &HostFailed{Message: "the build's directory: " + err.Error()} // the host's (#2537)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := untar(tree, dir); err != nil {
		return BuildResult{}, fmt.Errorf("tree: %w", err) // the package's
	}
	// The run image, the builder and their stacks are the host's, never the package's (#2537).
	if _, err := e.RunImageID(ctx); err != nil {
		return BuildResult{}, &HostFailed{Message: err.Error()}
	}
	if e.nodeOn() { // contracts/executor.md, A Node app builds on Canyon's Node builder
		why := e.fullBuilderFor(ctx, dir, req.BuildValues)
		if why == "" {
			res, err := e.buildOn(ctx, req, dir, out, true)
			if undetected := (*Undetected)(nil); !errors.As(err, &undetected) {
				return res, err
			}
			why = "the Node builder detects nothing"
		}
		log.Printf("executor: %s builds on canyon-builder: %s", req.App, why)
	}
	return e.buildOn(ctx, req, dir, out, false)
}

// buildOn builds the tree in dir on the Node builder, or else on the one builder (CANYON_BUILDER or canyon-builder).
// The Node builder and ownBuilder carry canyon-run:noble and the executor's two build variables, so with no build
// values pack is handed neither --run-image nor --env and builds on them as they are, never on an ephemeral copy; the
// Node builder's log is held back until its detection passed.
func (e *Executor) buildOn(ctx context.Context, req BuildRequest, dir string, out io.Writer, node bool) (BuildResult, error) {
	own := !node && e.ownBuilderFor(ctx, dir, req.BuildValues)
	env := BuildEnv(req.BuildValues)
	if node || own {
		env = req.BuildValues // a build that names either of the executor's own two builds on canyon-builder's base
	}
	values, err := envPairs(env, true)
	if err != nil {
		return BuildResult{}, err
	}
	var builder string
	switch {
	case node:
		builder, _, err = e.nodeBuilder(ctx)
	case e.nodeOn() && !builderImage.on(ctx, e.docker()): // the first app on this host the Node builder doesn't build
		_, _ = fmt.Fprintf(out, "This app needs Canyon's full build image, which this Docker host doesn't have yet. Downloading it once: %s\n", builderSize)
		fallthrough
	default:
		builder, err = e.builder(ctx, own)
	}
	if err != nil {
		return BuildResult{}, &HostFailed{Message: err.Error()}
	}
	if err := e.sameStack(ctx, builder); err != nil {
		return BuildResult{}, &HostFailed{Message: err.Error()}
	}
	if err := e.nonRoot(ctx, builder); err != nil {
		return BuildResult{}, err
	}
	descriptor, forget, err := e.descriptor(ctx, dir, builder) // pack reads Canyon's descriptor, never the app's project.toml
	if err != nil {
		return BuildResult{}, err
	}
	defer forget()
	holder, remove, err := e.floored(ctx) // contracts/executor.md, A build reaches no private or link-local address
	if err != nil {
		return BuildResult{}, &HostFailed{Message: "build network: " + err.Error()} // the host's, never the package's (#2489)
	}
	defer remove()
	for _, kind := range []string{"build", "launch"} { // pack makes a cache volume it lacks unlabelled: made here first, labelled as the app's other things
		create := append([]string{"volume", "create", "--label", "canyon.app=" + req.App}, e.projectLabel()...)
		if out, err := child(ctx, e.docker(), append(create, names.CacheVolume(req.App, kind))...).CombinedOutput(); err != nil {
			return BuildResult{}, &HostFailed{Message: fmt.Sprintf("build cache: %v: %s", err, strings.TrimSpace(string(out)))}
		}
	}
	tag := "canyon/" + req.App + ":" + req.Package + "-" + nonce() // unique per build: two builds never read each other's image
	args := []string{"build", tag, "--no-color", "--path", dir, "--builder", builder, "--pull-policy", "if-not-present",
		"--lifecycle-image", or(e.Lifecycle, lifecycleImage), "--network", "container:" + holder, // every phase in the holder's floored namespace
		"--descriptor", descriptor,
		// one pair per app, one flag per type (pack reads one type from each): pack's default is named after the per-build tag, so each build left two volumes behind
		"--cache", "type=build;format=volume;name=" + names.CacheVolume(req.App, "build"), "--cache", "type=launch;format=volume;name=" + names.CacheVolume(req.App, "launch")}
	if !node && !own || e.RunImage != "" { // the Node builder and ownBuilder name canyon-run:noble themselves
		args = append(args, "--run-image", e.runName())
	}
	for _, k := range sortedKeys(env) {
		args = append(args, "--env", k) // value-less: pack reads it from its own environment, so no value is in argv
	}
	start := time.Now()
	cmd := child(ctx, e.pack(), args...)
	cmd.Env = append(cmd.Env, values...)
	last := &lastLine{} // pack's own last line, uncoloured (--no-color): it says which lifecycle status ended the build
	gate := &gated{out: out, open: !node}
	both := io.MultiWriter(gate, last)
	cmd.Stdout, cmd.Stderr = both, both // one writer: exec hands both streams over one pipe, in order
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError // only a pack that exited on its own says how detection went: never one a signal or the ceiling killed
		status, said := strings.CutPrefix(last.String(), lifecycleFailed)
		said = said && errors.As(err, &exit) && exit.Exited() && ctx.Err() == nil
		if said && status == "21" && !node && nginxWantsYmlIn(dir, req.BuildValues) { // the app's to change (#242)
			return BuildResult{}, errors.New(NginxWithoutYml)
		}
		if said && (status == "20" || status == "21") {
			return BuildResult{}, &Undetected{Builds: e.builds(ctx, builder), Errored: status == "21"} // the Node builder's log is dropped: canyon-builder decides
		}
		_ = gate.release()                                         // a build that failed for another reason shows all of its log
		if code, err := strconv.Atoi(status); said && err == nil { // the step and its code, as Kubernetes says them (contracts/executor.md, /build; #1734)
			return BuildResult{}, fmt.Errorf("the build step exited %d", code)
		}
		return BuildResult{}, fmt.Errorf("pack: %w", err)
	}
	_ = gate.release() // all of a build that passed, whatever pack printed
	id, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{.Id}}", tag).CombinedOutput()
	if err != nil {
		log.Printf("executor: image inspect %s: %s", tag, strings.TrimSpace(string(id)))
		return BuildResult{}, &HostFailed{Message: "image id: " + err.Error()} // pack built it: the daemon's (#2537)
	}
	if err := e.projectTag(ctx, tag); err != nil {
		return BuildResult{}, &HostFailed{Message: err.Error()}
	}
	return BuildResult{Image: "canyon/" + req.App + "@" + strings.TrimSpace(string(id)), DurationMS: time.Since(start).Milliseconds(), Buildpacks: e.ran(ctx, tag, builder)}, nil
}

// lifecycleFailed is pack's last line when a lifecycle phase exits non-zero; the status that follows is
// the phase's. The detector's 20 is the CNB platform's "no buildpack group passed detection", and its 21
// the same with a buildpack erroring while detecting (contracts/executor.md, /build).
const lifecycleFailed = "ERROR: failed to build: executing lifecycle: failed with status code: "

// lastLine keeps the last line written to it, its last 512 bytes at most: pack's own line is shorter.
type lastLine struct{ b []byte }

func (l *lastLine) Write(p []byte) (int, error) {
	l.b = append(l.b, p...)
	l.b = l.b[max(0, len(l.b)-512):]
	return len(p), nil
}

func (l *lastLine) String() string {
	s := strings.TrimRight(string(l.b), "\n")
	return s[strings.LastIndexByte(s, '\n')+1:]
}

// builds is what the builder builds, from its image's order label, each buildpack named by its metadata
// label: a failed detect is one whether or not the order can be read, so one that cannot names none, and
// names that cannot be read leave each buildpack its id; the executor's log says why.
func (e *Executor) builds(ctx context.Context, builder string) []string {
	labels, err := e.labels(ctx, builder)
	if err != nil {
		log.Printf("executor: the labels of builder %s: %v", builder, err)
		return []string{}
	}
	names, err := NamesOf([]byte(labels[MetadataLabel]))
	if err != nil {
		log.Printf("executor: builder %s: %v; its buildpacks are named by id", builder, err)
	}
	builds, err := BuildsOf([]byte(labels["io.buildpacks.buildpack.order"]), names)
	if err != nil {
		log.Printf("executor: builder %s: %v", builder, err)
		return []string{}
	}
	return builds
}

// ran is the buildpacks the build of image ran, from its io.buildpacks.build.metadata label, each named
// by builder's metadata label: a group that cannot be read is none, never a failed build, and names that
// cannot be read leave each buildpack its id; the executor's log says why.
func (e *Executor) ran(ctx context.Context, image, builder string) []Buildpack {
	labels, err := e.labels(ctx, image)
	var meta struct {
		Buildpacks json.RawMessage `json:"buildpacks"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(labels["io.buildpacks.build.metadata"]), &meta)
	}
	if err != nil {
		log.Printf("executor: the buildpacks image %s ran: %v", image, err)
		return []Buildpack{}
	}
	var names map[string]string
	if builderLabels, err := e.labels(ctx, builder); err != nil {
		log.Printf("executor: the labels of builder %s: %v; the buildpacks that ran are named by id", builder, err)
	} else if names, err = NamesOf([]byte(builderLabels[MetadataLabel])); err != nil {
		log.Printf("executor: builder %s: %v; the buildpacks that ran are named by id", builder, err)
	}
	ran, err := RanOf(meta.Buildpacks, names)
	if err != nil {
		log.Printf("executor: image %s: %v", image, err)
	}
	return ran
}

// labels is an image's labels, as the daemon has them.
func (e *Executor) labels(ctx context.Context, image string) (map[string]string, error) {
	var labels map[string]string
	out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{json .Config.Labels}}", image).Output()
	if err == nil {
		err = json.Unmarshal(out, &labels)
	}
	return labels, err
}

// Run starts the image — by digest, never by tag, and only this app's image — on the app's own
// network with exactly this call's env and identity, under memory, pid and capability limits. The
// container is named <app>-<target> and replaces any previous one. The env rides in the
// docker child's own environment for the length of the call; nothing keeps it.
func (e *Executor) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if !validApp(req.App) {
		return RunResult{}, fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	if req.Target != "preview" && req.Target != "prod" {
		return RunResult{}, fmt.Errorf("%w: target must be preview or prod", ErrRequest)
	}
	if req.Network != "canyon-app-"+req.App {
		return RunResult{}, fmt.Errorf("%w: network must be canyon-app-%s: an app runs on its own network and no other", ErrRequest, req.App)
	}
	if req.Access != nil || req.FrontDoor != nil || req.DatabaseCA != nil {
		return RunResult{}, fmt.Errorf("%w: access, front_door and database_ca are for the Kubernetes executor; this one runs on Docker", ErrRequest)
	}
	if req.Run != "" && !runRe.MatchString(req.Run) {
		return RunResult{}, fmt.Errorf("%w: run is the server's run id, gr_ and [a-z0-9]", ErrRequest)
	}
	var proxied map[string]string // what egress sets in the app's environment, refused before docker is asked anything
	var trust []byte              // egress.ca's files, a tar of TrustDir, copied in before the app starts; nil without
	if req.Egress != nil {
		var err error
		if proxied, err = ProxyEnv(req.Egress, req.Identity.JWKSURL, req.Env); err != nil {
			return RunResult{}, err
		}
		if len(req.Egress.CA) > 0 {
			if trust, err = e.trustTar(req.Egress.CA); err != nil {
				return RunResult{}, err
			}
			maps.Copy(proxied, TrustEnv(req.Env))
		}
	}
	repo, digest, ok := strings.Cut(req.Image, "@sha256:")
	if !ok || repo != "canyon/"+req.App || strings.Trim(digest, "0123456789abcdef") != "" || len(digest) != 64 {
		return RunResult{}, fmt.Errorf("%w: image must be this app's digest reference (canyon/<app>@sha256:<64 hex>), never a tag", ErrRequest)
	}
	tags, err := e.images(ctx, req.App) // what the daemon lists, never read off an error's words: a daemon that does not answer is an error
	if err != nil {
		return RunResult{}, err
	}
	if !slices.ContainsFunc(tags, func(t imageTag) bool { return t.id == "sha256:"+digest }) { // not one of this app's builds on the daemon
		return RunResult{}, fmt.Errorf("%w: %s", ErrNoImage, req.Image)
	}
	env := map[string]string{"CANYON_IDENTITY_ISSUER": req.Identity.Issuer, "CANYON_IDENTITY_AUDIENCE": req.Identity.Audience, "CANYON_IDENTITY_JWKS_URL": req.Identity.JWKSURL}
	for k, v := range req.Env {
		if k == "PORT" || strings.HasPrefix(k, "CANYON_IDENTITY_") {
			return RunResult{}, fmt.Errorf("%w: env %s is the executor's", ErrRequest, k)
		}
		env[k] = v
	}
	maps.Copy(env, proxied)
	values, err := envPairs(env, false)
	if err != nil {
		return RunResult{}, err
	}
	// a redeploy replaces the previous container only once the new one runs: it is set aside under
	// <name>-previous, removed after the start, and named back when the start fails. One a crash left
	// aside is named back first; docker refuses that rename while <name> exists, and then it goes.
	name := req.App + "-" + req.Target
	var host []netip.Prefix // the Docker host's own addresses, which an open network's floor refuses, read before the lock
	if req.Egress == nil {
		if host, err = e.hostAddrs(ctx); err != nil {
			return RunResult{}, fmt.Errorf("app network: %w", err)
		}
	}
	e.nets.Lock()                                           // until the container is on the network: a /stop in between would remove it
	locked, unlock := context.WithTimeout(ctx, netDeadline) // and no longer than this: a stuck docker call would hold every app behind it
	network, err := e.ensureNetwork(locked, req.Network, req.Egress != nil)
	var ip, holder string
	switch {
	case err != nil:
	case network.Internal && req.Egress == nil:
		err = fmt.Errorf("%w: %s is internal: an app without egress would reach nothing outside it", ErrInternal, req.Network)
	case !network.Internal && req.Egress != nil:
		err = fmt.Errorf("%w: %s is not internal: an app routed through Canyon's egress gateway would reach past it until the network is recreated", ErrOpen, req.Network)
	case network.Internal: // the floor holds: the app at its target's fixed address, which names it to POST /callers
		ip, err = slot(network, name, req.Target)
	default: // an open network: the app runs in its holder's namespace, behind its floor, which is in place before the app starts
		holder, err = e.holdFloor(locked, network, req.App, req.Target, host)
	}
	if err != nil {
		unlock()
		e.nets.Unlock()
		return RunResult{}, err
	}
	_, _ = child(locked, e.docker(), "rename", name+"-previous", name).CombinedOutput()
	_, _ = child(locked, e.docker(), "rm", "-f", name+"-previous").CombinedOutput()
	_, _ = child(locked, e.docker(), "rename", name, name+"-previous").CombinedOutput() // fails on a first deploy: nothing to set aside
	// the labels are what make demo-down finds; the executor reads canyon.gate_run, for POST /callers, and canyon.floor,
	// the holder the app runs in (contracts/executor.md)
	joined := req.Network
	if holder != "" { // the network is the holder's: its endpoint, its floor and the app's name as its alias
		joined = "container:" + holder
	}
	verb := []string{"run", "-d"}
	if trust != nil { // created, given its trust, then started: an app never starts without the trust its /run named
		verb = []string{"create"}
	}
	args := append(verb, "--name", name, "--network", joined, "--label", "canyon.app="+req.App, "--label", "canyon.target="+req.Target)
	args = append(args, e.projectLabel()...)
	if ip != "" {
		args = append(args, "--ip", ip)
	}
	args = append(args, "--label", floorLabel+"="+holder) // always /run's own, empty on an internal network: an image's never stands
	if req.Run != "" {
		args = append(args, "--label", "canyon.gate_run="+req.Run)
	}
	for _, k := range sortedKeys(env) {
		args = append(args, "-e", k) // value-less: docker reads it from its own environment, so no value is in argv
	}
	// an app that exits non-zero once up is started again, ten times in the container's life; every /run
	// makes a new container and the executor never starts one again, so each deploy gives the app ten:
	// enough for one that crashes now and then, and a bound on one that crashes at once
	args = append(args, "--restart", "on-failure:10", "-e", "PORT", "--memory", "1g", "--pids-limit", "512", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--ulimit", "nofile=1024:1024", "sha256:"+digest)
	cmd := child(locked, e.docker(), args...)
	cmd.Env = append(cmd.Env, values...)
	cmd.Env = append(cmd.Env, "PORT="+appPort)
	var started strings.Builder
	cmd.Stdout = &started // docker run -d prints the container id; stderr is withheld: the call carries the env
	// what a failed start created goes, so the one set aside can take the name back, even past a
	// timeout: the env keeps running
	restore := func(created string) {
		keep := context.WithoutCancel(ctx)
		// said by the container's name alone: created is what a failed docker run printed, and rm's words repeat it
		if _, err := child(keep, e.docker(), "rm", "-f", created).CombinedOutput(); err != nil {
			log.Printf("executor: docker rm -f of %s's failed start failed: %v (output withheld); %s-previous can't take its name back, so %s is down", name, err, name, name)
		}
		if holder != "" && e.unfloored(keep, name+"-previous") { // one from before the floor is never named back onto an open network: the app is left down
			log.Printf("executor: %s-previous runs in no floor holder, deployed before the floor: removed, not named back; %s is down until it deploys", name, name)
			_, _ = child(keep, e.docker(), "rm", "-f", name+"-previous").CombinedOutput()
		} else {
			_, _ = child(keep, e.docker(), "rename", name+"-previous", name).CombinedOutput()
		}
		if holder != "" { // this call's holder goes with its container; the one set aside keeps its own
			e.reap(keep, req.App)
		}
	}
	err = cmd.Run()
	if err == nil && trust != nil {
		err = e.trustIn(locked, name, trust)
	}
	unlock()
	e.nets.Unlock()
	if err != nil {
		restore(cmp.Or(strings.TrimSpace(started.String()), name)) // by the id docker printed, else by the name a container created before the failure holds
		log.Printf("executor: docker run %s failed: %v (output withheld: the call carries the env)", name, err)
		return RunResult{}, fmt.Errorf("docker run: %w", err)
	}
	// up means the first start still running after the settle window and answering on port 3000, not
	// docker run exiting 0: an app that exits, or restarts under its restart policy, at once, or listens
	// elsewhere, is a failed start
	select {
	case <-time.After(cmp.Or(e.Settle, 10*time.Second)):
	case <-ctx.Done():
	}
	exited := func(reason string) error { // the app's last lines, read before restore removes its container and them with it
		var lines Tail
		logs := child(context.WithoutCancel(ctx), e.docker(), "logs", "--tail", strconv.Itoa(ExitedLines), name)
		logs.Stdout, logs.Stderr = &lines, &lines
		err := logs.Run()
		restore(name)
		if err != nil { // docker's words, never the app's
			return NewExited(reason+"; its last lines could not be read", "", req.Env)
		}
		return NewExited(reason, lines.Lines(), req.Env)
	}
	status, restarts, _, _ := e.status(ctx, name) // a daemon that does not answer here fails the /run as well
	if status == "" {                             // gone, or a daemon that did not answer: nothing says the app stopped itself
		restore(name) // this call's: its start succeeded under the name
		return RunResult{}, errors.New("the app did not stay up: its container is gone")
	}
	if status != "running" {
		return RunResult{}, exited("the app did not stay up: its container is " + status)
	}
	if restarts > 0 {
		return RunResult{}, exited("the app did not stay up: its container restarted")
	}
	probe := name // the address the server uses: on an open network the holder's, which only this call's app answers on
	if holder != "" {
		probe = holder
	}
	answers, err := e.answers(ctx, probe, probe, appPort)
	if err != nil { // the daemon's failure, unless the app stopped since its status was read: no namespace is left to join
		if status, _, _, _ := e.status(ctx, name); status != "" && status != "running" {
			return RunResult{}, exited("the app did not stay up: its container is " + status)
		}
		restore(name)
		return RunResult{}, err
	}
	if !answers {
		return RunResult{}, exited(NothingOnPort)
	}
	if req.Egress != nil { // the door must answer: an app whose calls out have no door is never left running (contracts/executor.md, Egress)
		host := proxyRe.FindStringSubmatch(req.Egress.Proxy)[1]
		door, err := e.answers(ctx, name, host, proxyPort)
		if err == nil && !door {
			err = fmt.Errorf("nothing answers at %s:%s, Canyon's egress gateway, from the app's network", host, proxyPort)
		}
		if err != nil {
			restore(name)
			return RunResult{}, err
		}
	}
	_, _ = child(ctx, e.docker(), "rm", "-f", name+"-previous").CombinedOutput()
	if holder != "" { // the holder of the one set aside goes with it
		e.reap(ctx, req.App)
	}
	return RunResult{Container: name, Addr: name + ":" + appPort}, nil
}

// answers says whether something answers on host:port from the container's network namespace — the
// app's own port 3000 at the address the server uses, or Canyon's egress gateway — tried once by a
// busybox that shares the namespace and nothing else of the container, so the app's image does not
// matter. nc's own failure is exit 1; any other is docker's — the helper not started, its image not
// pulled, the daemon not answering — and an error: it says nothing of the app.
func (e *Executor) answers(ctx context.Context, name, host, port string) (bool, error) {
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), "run", "--rm", "--network", "container:"+name, "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		cmp.Or(e.Busybox, busyboxImage), "nc", "-z", "-w", "2", host, port)
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("port check of %s:%s from %s: %w: %s", host, port, name, err, strings.TrimSpace(stderr.String()))
}

// containers is each container the daemon lists of names, by name: its state and the image it was
// started from. docker's name filter is a pattern, so the answer is matched whole against what was asked.
// For a few names: each filter is a pattern the daemon tries against every container (containersOf).
func (e *Executor) containers(ctx context.Context, names ...string) (map[string][2]string, error) {
	var filters []string
	for _, n := range names {
		filters = append(filters, "--filter", "name=^"+n+"$")
	}
	return e.listed(ctx, filters, names)
}

// containersOf is containers for many names, POST /running's up to MaxRunningOf: the daemon lists every container
// once, with no filter, and the names asked are kept here. 400 name filters over 4,000 containers took 15 s, the
// whole list 157 ms (#2752, #2809). No label decides it, so a container started before one was set is still what
// it is, never one that is not there.
func (e *Executor) containersOf(ctx context.Context, names []string) (map[string][2]string, error) {
	return e.listed(ctx, nil, names)
}

// listed is docker container ls --all under filters, each container named in names by one of its names: its state
// and image. What the daemon lists is the answer, never an error's words: one that does not answer is an error.
func (e *Executor) listed(ctx context.Context, filters, names []string) (map[string][2]string, error) {
	asked := map[string]bool{}
	for _, n := range names {
		asked[n] = true
	}
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), append([]string{"container", "ls", "--all", "--no-trunc", "--format", "{{.Names}} {{.State}} {{.Image}}"}, filters...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker container ls: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	listed := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 3 {
			for _, n := range strings.Split(f[0], ",") {
				if asked[n] {
					listed[n] = [2]string{f[1], f[2]}
				}
			}
		}
	}
	return listed, nil
}

// status is the state docker reports for a container, how often its restart policy started it
// again, and what it was started from; "" when the daemon lists no container of that name. Any
// failure is an error: a daemon that does not answer is not a container that is gone.
func (e *Executor) status(ctx context.Context, name string) (state string, restarts int, image string, err error) {
	listed, err := e.containers(ctx, name)
	if _, there := listed[name]; err != nil || !there {
		return "", 0, "", err
	}
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), "container", "inspect", "--format", "{{.State.Status}} {{.RestartCount}} {{.Config.Image}}", name)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", 0, "", fmt.Errorf("docker container inspect %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 3 {
		return "", 0, "", fmt.Errorf("docker container inspect %s: %q is not a state, a restart count and an image", name, out)
	}
	restarts, err = strconv.Atoi(fields[1])
	return fields[0], restarts, fields[2], err
}

// Running answers the image <app>-<target> runs, as /run was handed it, while it runs; a container
// that is not there, or not running, is ErrNoContainer, and a daemon that does not answer an error.
func (e *Executor) Running(ctx context.Context, app, target string) (string, error) {
	if !validApp(app) {
		return "", fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	if target != "preview" && target != "prod" {
		return "", fmt.Errorf("%w: target must be preview or prod", ErrRequest)
	}
	state, _, image, err := e.status(ctx, app+"-"+target)
	if err != nil {
		return "", err
	}
	if !runs(state, image) {
		return "", fmt.Errorf("%w: %s-%s", ErrNoContainer, app, target)
	}
	return "canyon/" + app + "@" + image, nil
}

// runs is a container Running answers for: running, from an image /run started by digest.
func runs(state, image string) bool { return state == "running" && strings.HasPrefix(image, "sha256:") }

// RunningOf answers those of containers whose container runs, as Running would answer for each, in
// the order asked, from one docker call: a read that shows many environments asks once, never once
// per app. A name the daemon has not is not running; any other failure is an error. Asked why, it
// says why those that exited stopped; not asked, it asks the daemon nothing more.
func (e *Executor) RunningOf(ctx context.Context, containers []Container, why bool) (Runs, error) {
	if len(containers) == 0 || len(containers) > MaxRunningOf {
		return Runs{}, fmt.Errorf("%w: ask for 1 to %d containers", ErrRequest, MaxRunningOf)
	}
	var names, aside []string
	for _, c := range containers {
		if !validApp(c.App) || (c.Target != "preview" && c.Target != "prod") {
			return Runs{}, fmt.Errorf("%w: app must match %s and target be preview or prod", ErrRequest, appPattern)
		}
		names, aside = append(names, c.App+"-"+c.Target), append(aside, c.App+"-"+c.Target+"-previous")
	}
	listed, err := e.containersOf(ctx, append(names, aside...)) // a name the daemon has not is not listed
	if err != nil {
		return Runs{}, err
	}
	// a container /run set aside while its successor starts (Run): it says the asked one is starting, and is no answer of its own
	switching := map[string]bool{}
	for _, n := range aside {
		if _, ok := listed[n]; ok {
			switching[strings.TrimSuffix(n, "-previous")] = true
			delete(listed, n)
		}
	}
	restarts, err := e.restarts(ctx, listed)
	if err != nil {
		return Runs{}, err
	}
	running := map[string]bool{}
	var exited []Container
	restarting, starting := []Restarting{}, []Container{}
	for _, c := range containers {
		n := c.App + "-" + c.Target
		l, ok := listed[n]
		switch {
		case !ok && switching[n], ok && l[0] == "created": // set aside while its successor is made, or made and not yet started (#230)
			if !slices.Contains(starting, c) {
				starting = append(starting, c)
			}
		case !ok:
		case (restarts[n] > 0 || l[0] == "restarting") && !slices.ContainsFunc(restarting, func(r Restarting) bool { return r.App == c.App && r.Target == c.Target }):
			restarting = append(restarting, Restarting{App: c.App, Target: c.Target, Restarts: max(restarts[n], 1)}) // docker counts a restart before it says restarting
			if why && l[0] == "restarting" {                                                                         // down between two starts, so its exit code and last line are its last exit's (#1597)
				exited = append(exited, c)
			}
		case why && !slices.Contains(exited, c) && (l[0] == "exited" || l[0] == "dead"):
			exited = append(exited, c)
		}
	}
	for n, l := range listed {
		running[n] = runs(l[0], l[1]) && restarts[n] == 0
	}
	return Runs{Running: Asked(containers, running), Restarting: restarting, Starting: starting, Stopped: e.why(ctx, exited)}, nil
}

// restarts is how often docker started each listed container that is up or waiting to start again since
// /run made it, from one docker container inspect of them all: an exited container is not restarting,
// whatever its count (contracts/executor.md, Restarting). A daemon that does not answer is an error: a
// count it did not give is not a container that never restarted.
func (e *Executor) restarts(ctx context.Context, listed map[string][2]string) (map[string]int, error) {
	var names []string
	for n, l := range listed {
		if l[0] == "running" || l[0] == "restarting" {
			names = append(names, n)
		}
	}
	counts := map[string]int{}
	if len(names) == 0 {
		return counts, nil
	}
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), append([]string{"container", "inspect", "--format", "{{.Name}} {{.RestartCount}}"}, names...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker container inspect: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		n := 0
		if len(f) == 2 {
			n, err = strconv.Atoi(f[1])
		}
		if len(f) != 2 || err != nil {
			return nil, fmt.Errorf("docker container inspect: %q is not a name and a restart count", line)
		}
		counts[strings.TrimPrefix(f[0], "/")] = n
	}
	return counts, nil
}

// why is each exited container's last exit code and its last line: one docker logs --tail 1 each, stdout
// and stderr, then one docker container inspect for them all, which keeps only those still not up — one
// started again between the reads has a new start's line and its exit code is gone (#1597). Raw, the
// server masks it. One that cannot be read is left out, and why logged: it is still not running.
func (e *Executor) why(ctx context.Context, exited []Container) []Stopped {
	stopped := []Stopped{}
	said, names := map[string]string{}, []string{}
	for _, c := range exited {
		n := c.App + "-" + c.Target
		var tail Tail
		cmd := child(ctx, e.docker(), "logs", "--tail", "1", n)
		cmd.Stdout, cmd.Stderr = &tail, &tail
		if err := cmd.Run(); err != nil {
			log.Printf("executor: why %s stopped: docker logs: %v", n, err)
			continue
		}
		said[n], names = LastLine(tail.Lines()), append(names, n)
	}
	if len(names) == 0 {
		return stopped
	}
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), append([]string{"container", "inspect", "--format", "{{.Name}} {{.State.Status}} {{.State.ExitCode}}"}, names...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		log.Printf("executor: why %v stopped: docker container inspect: %v: %s", names, err, strings.TrimSpace(stderr.String()))
		return stopped
	}
	codes := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) == 3 && (f[1] == "exited" || f[1] == "dead" || f[1] == "restarting") {
			if code, err := strconv.Atoi(f[2]); err == nil {
				codes[strings.TrimPrefix(f[0], "/")] = code
			}
		}
	}
	for _, c := range exited {
		n := c.App + "-" + c.Target
		line, read := said[n]
		code, ok := codes[n]
		switch {
		case read && ok:
			stopped = append(stopped, Stopped{App: c.App, Target: c.Target, ExitCode: code, Said: line})
		case read:
			log.Printf("executor: why %s stopped: docker container inspect answered no exit code, or it is up again", n)
		}
	}
	return stopped
}

// LastLine is the last non-empty line of text, valid UTF-8: what a Stopped says; "" when it is
// longer than SaidBytes, dropped whole, never cut.
func LastLine(text string) string {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimRight(lines[i], "\r"); line != "" {
			if len(line) > SaidBytes {
				return ""
			}
			return strings.ToValidUTF8(line, "")
		}
	}
	return ""
}

// Asked is those of containers that run, by <app>-<target>, each once, in the order asked: POST
// /running's answer on either executor. None is an empty list, never a null.
func Asked(containers []Container, running map[string]bool) []Container {
	answer := []Container{}
	for _, c := range containers {
		if running[c.App+"-"+c.Target] && !slices.Contains(answer, c) {
			answer = append(answer, c)
		}
	}
	return answer
}

// Stop removes the container <app>-<target>, and the <app>-<target>-previous a redeploy cut short
// left aside: left, it would keep /forget refusing; then, when no container of the app is left, the
// app's network. Idempotent: docker rm -f exits 0 on a name the daemon does not have, so stopping
// what is already stopped is success (contracts/executor.md invariant 6). No image, no volume, no
// app data.
func (e *Executor) Stop(ctx context.Context, app, target string) error {
	if !validApp(app) {
		return fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	if target != "preview" && target != "prod" {
		return fmt.Errorf("%w: target must be preview or prod", ErrRequest)
	}
	if out, err := child(ctx, e.docker(), "rm", "-f", app+"-"+target, app+"-"+target+"-previous").CombinedOutput(); err != nil {
		log.Printf("executor: docker rm %s-%s: %s", app, target, strings.TrimSpace(string(out)))
		return fmt.Errorf("docker rm: %w", err)
	}
	return e.dropNetwork(ctx, app)
}

// ensureNetwork makes network exist with every joined container on it, and answers what it is. None
// there, it is created on the pool's first /24 docker does not refuse as overlapping, its gateway the
// /24's first address, the one address of it the server's accept guard serves
// (cmd/canyon-server/appnet.go); for a /run that carries egress, internal, docker handing out addresses
// in its upper /25 alone (contracts/executor.md, Egress). IPv4 alone, whatever the daemon's defaults: the
// guard knows the pool, never an IPv6 prefix a daemon default would add. A joined container
// is connected below its own networks' priority, so its default gateway stays where it was: docker
// publishes a container's ports through the gateway's network, and a gateway moving on a connect or
// on /stop's disconnect cuts every connection open through them — the server's, the request that
// asked for the /stop among them. A joined container already on it is skipped, so a crash between
// the create and a connect is repaired by the next /run. The caller holds e.nets.
func (e *Executor) ensureNetwork(ctx context.Context, network string, egress bool) (appNet, error) {
	n, exists := e.network(ctx, network)
	if !exists {
		var err error
		if n, err = e.createNetwork(ctx, network, egress, netip.Prefix{}); err != nil {
			return n, err
		}
	}
	for _, c := range e.joinTo(n) {
		if slices.Contains(n.members(), c) {
			continue
		}
		if out, err := child(ctx, e.docker(), "network", "connect", "--gw-priority=-1", network, c).CombinedOutput(); err != nil {
			log.Printf("executor: network connect %s %s: %s", network, c, strings.TrimSpace(string(out)))
			return n, fmt.Errorf("network connect %s %s: %w", network, c, err)
		}
	}
	return n, nil
}

// Rejoin connects each joined container to every app network it is not on, now and every rejoinEvery
// until ctx ends: a compose recreate of canyon-server or app-postgres makes a container on its own
// networks alone, which reaches no running app until that app's next /run without this (#1104). Each pass
// then reconciles the apps that run in a holder (Reconcile): after a daemon's restart or a holder's, each is
// floored and running again within one pass.
func (e *Executor) Rejoin(ctx context.Context) {
	for {
		e.rejoin(ctx)
		e.Reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(rejoinEvery):
		}
	}
}

// rejoinEvery is how long a recreated joined container waits to reach the apps again; a var so a test
// can lower it.
var rejoinEvery = 15 * time.Second

func (e *Executor) rejoin(ctx context.Context) {
	out, err := child(ctx, e.docker(), "network", "ls", "--filter", "name=canyon-app-", "--format", "{{.Name}}").Output()
	if err != nil {
		log.Printf("executor: rejoin: network ls: %v", err)
		return
	}
	e.nets.Lock()
	defer e.nets.Unlock()
	ctx, cancel := context.WithTimeout(ctx, netDeadline)
	defer cancel()
	for _, network := range strings.Fields(string(out)) {
		if _, exists := e.network(ctx, network); !exists || !strings.HasPrefix(network, "canyon-app-") { // the filter matches anywhere in a name; one /stop removed since is not made again
			continue
		}
		if _, err := e.ensureNetwork(ctx, network, false); err != nil {
			log.Printf("executor: rejoin %s: %v", network, err)
		}
	}
}

// netDeadline bounds the docker calls made while e.nets is held; a var so a test can lower it.
var netDeadline = 2 * time.Minute

// overlaps is how docker refuses a subnet another network holds.
const overlaps = "Pool overlaps with other one on this address space"

// createNetwork creates network on prefer when it is a /24 no other network holds — a recreated network
// keeps its own, so its addresses go on naming the same app — else on the pool's first free one.
func (e *Executor) createNetwork(ctx context.Context, network string, internal bool, prefer netip.Prefix) (appNet, error) {
	pool := e.pool()
	var taken []netip.Prefix
	if internal { // docker refuses no /24 another network holds once --ip-range is given: it fails on the gateway instead
		var err error
		if taken, err = e.subnets(ctx); err != nil {
			return appNet{}, err
		}
	}
	var subs []netip.Prefix
	if prefer.IsValid() {
		subs = append(subs, prefer)
	}
	if pool.Addr().Is4() && pool.Bits() <= 24 {
		a := pool.Masked().Addr().As4()
		first := binary.BigEndian.Uint32(a[:])
		for i := range uint32(1) << (24 - pool.Bits()) {
			binary.BigEndian.PutUint32(a[:], first+i<<8)
			subs = append(subs, netip.PrefixFrom(netip.AddrFrom4(a), 24))
		}
	}
	for _, sub := range subs {
		if slices.ContainsFunc(taken, sub.Overlaps) {
			continue
		}
		args := []string{"network", "create", "--ipv6=false", "--subnet", sub.String(), "--gateway", sub.Addr().Next().String()}
		if internal { // nothing routed out of it, no address of the host's on it, and the lower /25 kept for the fixed addresses
			args = append(args, "--internal", "-o", "com.docker.network.bridge.gateway_mode_ipv4=isolated", "--ip-range", netip.PrefixFrom(at(sub, 128), 25).String())
		}
		args = append(append(args, "--label", "canyon.app="+strings.TrimPrefix(network, "canyon-app-")), e.projectLabel()...)
		out, err := child(ctx, e.docker(), append(args, network)...).CombinedOutput()
		if err == nil {
			log.Printf("executor: created %s on %s (internal %t)", network, sub, internal)
			n := appNet{Name: network, Internal: internal}
			n.IPAM.Config = []ipamConfig{{Subnet: sub.String(), Gateway: sub.Addr().Next().String()}}
			return n, nil
		}
		if !strings.Contains(string(out), overlaps) {
			log.Printf("executor: network create %s: %s", network, strings.TrimSpace(string(out)))
			return appNet{}, fmt.Errorf("network create %s: %w", network, err)
		}
	}
	return appNet{}, fmt.Errorf("CANYON_APPS_POOL %s has no free /24 for %s", pool, network)
}

// subnets is every subnet a network on the daemon holds, from one docker network inspect of them all.
// A daemon that does not answer is an error: a /24 it did not name is not a free one.
func (e *Executor) subnets(ctx context.Context) ([]netip.Prefix, error) {
	out, err := child(ctx, e.docker(), "network", "ls", "--quiet").Output()
	if err != nil {
		return nil, fmt.Errorf("docker network ls: %w", err)
	}
	var nets []appNet
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		out, err = child(ctx, e.docker(), append([]string{"network", "inspect"}, ids...)...).Output()
		if err == nil {
			err = json.Unmarshal(out, &nets)
		}
		if err != nil {
			return nil, fmt.Errorf("docker network inspect: %w", err)
		}
	}
	var taken []netip.Prefix
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if p, err := netip.ParsePrefix(c.Subnet); err == nil {
				taken = append(taken, p.Masked())
			}
		}
	}
	return taken, nil
}

// dropNetwork removes canyon-app-<app> once no container of the app is left, the joined containers
// disconnected first: docker refuses to remove a network anything is still on. Gone is success.
func (e *Executor) dropNetwork(ctx context.Context, app string) error {
	e.nets.Lock()
	defer e.nets.Unlock()
	ctx, cancel := context.WithTimeout(ctx, netDeadline)
	defer cancel()
	if err := e.reapLocked(ctx, app); err != nil { // the holders of what was removed, before the network they are on
		return err
	}
	network := "canyon-app-" + app
	n, exists := e.network(ctx, network)
	if !exists {
		return nil
	}
	if used, err := e.inUse(ctx, app); err != nil || len(used) > 0 {
		return err
	}
	for _, c := range e.joinTo(n) {
		if !slices.Contains(n.members(), c) {
			continue
		}
		if out, err := child(ctx, e.docker(), "network", "disconnect", network, c).CombinedOutput(); err != nil {
			log.Printf("executor: network disconnect %s %s: %s", network, c, strings.TrimSpace(string(out)))
			return fmt.Errorf("network disconnect %s %s: %w", network, c, err)
		}
	}
	if out, err := child(ctx, e.docker(), "network", "rm", network).CombinedOutput(); err != nil {
		log.Printf("executor: network rm %s: %s", network, strings.TrimSpace(string(out)))
		return fmt.Errorf("network rm %s: %w", network, err)
	}
	log.Printf("executor: removed %s", network)
	return nil
}

// network is what docker says of network, and whether it exists: inspect prints a network that
// exists and fails on one that does not.
func (e *Executor) network(ctx context.Context, network string) (appNet, bool) {
	out, _ := child(ctx, e.docker(), "network", "inspect", "--format", "{{json .}}", network).Output()
	var n appNet
	if json.Unmarshal(out, &n) != nil || n.Name != network {
		return appNet{}, false
	}
	return n, true
}

// maxCollect caps the tags one pass after a /run removes; the rest wait for the next deploy.
const maxCollect = 50

// Collect runs collect off the caller for at most 5 minutes, counted in e.collecting before it
// returns. ctx's end does not cut it short: the request ends with the answer, the cleanup outlives it.
func (e *Executor) Collect(ctx context.Context, app string, keep []string) {
	e.collecting.Go(func() {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		e.collect(bounded, app, keep)
	})
}

// collect removes this app's image tags that none of its containers runs and keep does not name:
// every build tags a new image and nothing else ever removed one. A plain docker image rm, never -f: a
// tag docker refuses is kept, and a tag whose image a container runs is never asked for — so an
// identical rebuild, two tags on one image, never leaves the running image untagged. It logs, it never fails.
func (e *Executor) collect(ctx context.Context, app string, keep []string) {
	spared, err := e.inUse(ctx, app)
	if err != nil {
		log.Printf("executor: containers of %s: %v; nothing removed", app, err)
		return
	}
	for _, id := range keep { // on Docker a digest in keep is the image ID
		spared[id] = true
	}
	tags, err := e.images(ctx, app)
	if err != nil {
		log.Printf("executor: images of %s: %v; nothing removed", app, err)
		return
	}
	var removed, kept []string
	for _, t := range tags {
		if spared[t.id] || len(removed) == maxCollect {
			kept = append(kept, t.ref)
			continue
		}
		if out, err := child(ctx, e.docker(), "image", "rm", t.ref).CombinedOutput(); err != nil {
			log.Printf("executor: keeping %s: %s", t.ref, strings.TrimSpace(string(out)))
			kept = append(kept, t.ref)
			continue
		}
		removed = append(removed, t.ref)
	}
	log.Printf("executor: images of %s: removed %v, kept %v", app, removed, kept)
}

// Forget removes everything the app left on the daemon once its containers are gone: every
// canyon/<app> image, its two build cache volumes and the network a crash left. Idempotent: what
// is absent is fine.
func (e *Executor) Forget(ctx context.Context, app string) error {
	if !validApp(app) {
		return fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	if used, err := e.inUse(ctx, app); err != nil {
		return err
	} else if len(used) > 0 {
		return fmt.Errorf("%w: a container of %s still exists: stop it first", ErrInUse, app)
	}
	tags, err := e.images(ctx, app)
	if err != nil {
		return err
	}
	for _, t := range tags {
		if out, err := child(ctx, e.docker(), "image", "rm", t.ref).CombinedOutput(); err != nil {
			log.Printf("executor: image rm %s: %s", t.ref, strings.TrimSpace(string(out)))
			return fmt.Errorf("image rm %s: %w", t.ref, err)
		}
	}
	// inspect prints the volumes that exist and fails on the rest; only those are removed
	found, _ := child(ctx, e.docker(), "volume", "inspect", "--format", "{{.Name}}", names.CacheVolume(app, "build"), names.CacheVolume(app, "launch")).Output()
	volumes := strings.Fields(string(found))
	if len(volumes) > 0 {
		if out, err := child(ctx, e.docker(), append([]string{"volume", "rm"}, volumes...)...).CombinedOutput(); err != nil {
			log.Printf("executor: volume rm %v: %s", volumes, strings.TrimSpace(string(out)))
			return fmt.Errorf("volume rm: %w", err)
		}
	}
	log.Printf("executor: forgot %s: %d image tags, volumes %v", app, len(tags), volumes)
	return e.dropNetwork(ctx, app)
}

// Sweep removes every container and volume labelled canyon.scan, every scan's staging directory, and every
// build's holder, labelled canyon.build: what a crash mid-scan or mid-build left behind (contracts/executor.md
// invariant 8; A build reaches no private or link-local address). Called once at start-up, before the executor
// serves, so a container a previous process never reached its deferred removal for is gone
// before any request can find it. A ps, ls or rm that fails is logged, never fatal — the
// executor still serves.
func (e *Executor) Sweep(ctx context.Context) {
	for _, label := range []string{"canyon.scan", "canyon.build"} { // a build's holder too: its build died with the executor
		out, err := child(ctx, e.docker(), "ps", "-aq", "--filter", "label="+label).Output()
		if err != nil {
			log.Printf("executor: sweep: docker ps: %v", err)
		} else if containers := strings.Fields(string(out)); len(containers) > 0 {
			if out, err := child(ctx, e.docker(), append([]string{"rm", "-f", "-v"}, containers...)...).CombinedOutput(); err != nil {
				log.Printf("executor: sweep: docker rm %v: %v: %s", containers, err, strings.TrimSpace(string(out)))
			} else {
				log.Printf("executor: sweep: removed containers %v", containers)
			}
		}
	}
	out, err := child(ctx, e.docker(), "volume", "ls", "-q", "--filter", "label=canyon.scan").Output()
	if err != nil {
		log.Printf("executor: sweep: docker volume ls: %v", err)
	} else if volumes := strings.Fields(string(out)); len(volumes) > 0 {
		if out, err := child(ctx, e.docker(), append([]string{"volume", "rm"}, volumes...)...).CombinedOutput(); err != nil {
			log.Printf("executor: sweep: docker volume rm %v: %v: %s", volumes, err, strings.TrimSpace(string(out)))
		} else {
			log.Printf("executor: sweep: removed volumes %v", volumes)
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "canyon-scan-*")) // a scan's staging, an image scan's image.tar among it; a pattern errs only when malformed
	for _, dir := range dirs {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("executor: sweep: %v", err)
		}
	}
}

// inUse is the image IDs the app's containers run: <app>-preview, <app>-prod and the two a
// redeploy sets aside, as the daemon lists them; a daemon that cannot list is an error, never none.
func (e *Executor) inUse(ctx context.Context, app string) (map[string]bool, error) {
	listed, err := e.containers(ctx, app+"-preview", app+"-prod", app+"-preview-previous", app+"-prod-previous")
	ids := map[string]bool{}
	for _, l := range listed {
		ids[l[1]] = true
	}
	return ids, err
}

type imageTag struct{ id, ref string }

// images is every tag of the repository canyon/<app> exactly — never canyon/<app>-other — with its image ID.
func (e *Executor) images(ctx context.Context, app string) ([]imageTag, error) {
	repos := []string{"canyon/" + app}
	if e.Project != "" { // its second name (projectTag): removed with the first, or the image outlives every canyon/<app> tag
		repos = append(repos, e.Project+"/"+app)
	}
	list := []string{"image", "ls", "--no-trunc"}
	for _, repo := range repos {
		list = append(list, "--filter", "reference="+repo)
	}
	out, err := child(ctx, e.docker(), append(list, "--format", "{{.ID}} {{.Repository}}:{{.Tag}}")...).Output()
	if err != nil {
		return nil, fmt.Errorf("image ls: %w", err)
	}
	var tags []imageTag
	for _, line := range strings.Split(string(out), "\n") {
		id, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		if repo, tag, _ := strings.Cut(ref, ":"); slices.Contains(repos, repo) && tag != "" && tag != "<none>" {
			tags = append(tags, imageTag{id, ref})
		}
	}
	return tags, nil
}

// Logs writes the log of <app>-<target> to out: the last tail lines, then, when follow is set,
// live until the container stops or ctx ends, which is a clean end. Both streams go to out in the order docker
// writes them. ready is called once every refusal has been decided and before the stream starts,
// so a caller can answer 200 at once: a running container that is silent is not a dead one. The
// call carries app, target, tail and follow and nothing else (contracts/executor.md, invariant 5): no env
// in, no vault value out.
func (e *Executor) Logs(ctx context.Context, app, target string, tail int, follow bool, out io.Writer, ready func()) error {
	if !validApp(app) {
		return fmt.Errorf("%w: app must match %s", ErrRequest, appPattern)
	}
	if target != "preview" && target != "prod" {
		return fmt.Errorf("%w: target must be preview or prod", ErrRequest)
	}
	name := app + "-" + target
	state, _, _, err := e.status(ctx, name)
	if err != nil {
		return err // a daemon that does not answer is a 502, never the 404 of a container that is gone
	}
	if state == "" {
		return fmt.Errorf("%w: %s", ErrNoContainer, name)
	}
	if ready != nil {
		ready()
	}
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	cmd := child(ctx, e.docker(), append(args, "--tail", strconv.Itoa(tail), "--", name)...)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("docker logs: %w", err)
	}
	return nil
}

const (
	appPattern     = "^[a-z0-9][a-z0-9_-]{0,127}$" // contracts/schema/executor.schema.json: name
	packagePattern = "^pkg_[a-z0-9]{4,}$"          // contracts/schema/executor.schema.json: package
)

var (
	appRe     = regexp.MustCompile(appPattern)
	packageRe = regexp.MustCompile(packagePattern)
)

func validApp(s string) bool     { return appRe.MatchString(s) }
func validPackage(s string) bool { return packageRe.MatchString(s) }

// validName accepts the identifiers the contract leaves to the executor, the env
// keys. A leading dash is refused so a name can never read as a flag to docker.
func validName(s string) bool {
	if s == "" || len(s) > 128 || s[0] == '-' {
		return false
	}
	for _, c := range s {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if !letter && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// maxTree and maxEntries bound what an untrusted tree may expand to; sparseSlack is how far
// the bytes written may run ahead of the bytes read before an entry counts as sparse.
const (
	maxTree     = 1 << 30
	maxEntries  = 200000
	sparseSlack = 1 << 20
)

// What an untrusted tree may expand to, and the pod's start, on every host: the Kubernetes executor
// (executor/kube) takes them from here, and runs/ holds its /run ceiling above twice RunAvailable — a
// pod has RunAvailable to become available, and a failed first run as long again to be cleaned up
// (contracts/executor.md).
const (
	MaxTree      = maxTree
	MaxEntries   = maxEntries
	SparseSlack  = sparseSlack
	RunAvailable = 300 * time.Second
)

// untar extracts regular files and directories under dir, readable by anyone who reaches dir (a
// scan's tool runs as nobody). Anything that would escape dir, anything that is not a file or a
// directory, and anything past maxTree is refused.
func untar(r io.Reader, dir string) error {
	cr := &countingReader{r: r}
	tr := tar.NewReader(cr)
	var total int64
	for n := 0; ; n++ {
		if n >= maxEntries {
			return fmt.Errorf("tree exceeds %d entries", maxEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader { // git archive writes one first; ingest.Untar skips it the same way
			continue
		}
		clean := filepath.Clean(h.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("entry %q escapes the tree", h.Name)
		}
		target := filepath.Join(dir, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // readable by the scan's tool; the temp dir above dir is private
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // readable by the scan's tool; the temp dir above dir is private
				return err
			}
			if total += h.Size; total > maxTree {
				return fmt.Errorf("tree exceeds %d bytes", maxTree)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777|0o644) //nolint:gosec // target is confined under dir by the check above
			if err != nil {
				return err
			}
			if _, err := io.CopyN(f, tr, h.Size); err != nil && !errors.Is(err, io.EOF) {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if total > cr.n+sparseSlack { // a sparse entry: written far more than the archive carried
				return fmt.Errorf("entry %q expands beyond the archive; sparse entries are refused", h.Name)
			}
		default:
			return fmt.Errorf("entry %q: only files and directories are accepted", h.Name)
		}
	}
}

// BuildEnv is what a build runs with (contracts/executor.md, /build): the build values, over the
// executor's own two variables, which a value of the same name replaces.
//
// NEXT_PRIVATE_OUTPUT_TRACE_ROOT is Next's private name for the root it falls back to when the app's
// config sets neither turbopack.root nor outputFileTracingRoot. Paketo's npm-install links
// node_modules to a layer outside the app's directory, and Turbopack, Next 16's default bundler,
// refuses a link that leaves its root, so a stock create-next-app failed at next build (#1240). With
// the filesystem's root as Next's root the layer is inside it. A private name can move in any Next
// release: TestEndToEndBuildsAndServesAStockNext16App catches that, and #1461 drops this once
// Paketo or Next handles the link upstream.
func BuildEnv(values map[string]string) map[string]string {
	env := map[string]string{"NODE_ENV": "production", "NEXT_PRIVATE_OUTPUT_TRACE_ROOT": "/"}
	maps.Copy(env, values)
	return env
}

// envPairs is m as NAME=value strings for a child's environment — the one place a value
// travels, never argv or a file, so it may span lines (contracts/executor.md). A name the
// schema refuses, a reserved one (a later NAME=value wins in exec, so a value named DOCKER_HOST
// would point docker elsewhere with every value in hand; build is a build's values, which pack
// reads), or a value with a NUL, which no environment can carry, refuses the call.
func envPairs(m map[string]string, build bool) ([]string, error) {
	out := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		if !validName(k) || isReserved(k, build) || strings.IndexByte(m[k], 0) >= 0 {
			return nil, fmt.Errorf("%w: env %q: names are [A-Za-z0-9_-] not starting with a dash, values carry no NUL, and none is reserved: %s", ErrRequest, k, reservedSaid(build))
		}
		out = append(out, k+"="+m[k])
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// countingReader counts the bytes the archive actually carried.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func nonce() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// child is a pack or docker command on caller-named, validated inputs — running those is this
// package's job — with the environment below and nothing else.
func child(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // see above
	cmd.Env = childEnv()
	return cmd
}

// reserved is every name a call's env may not take, one list: what pack and docker need to find the daemon and
// their config, which childEnv hands them from the executor's own environment (child); and what pack's own process
// would read besides, which a build value, travelling in pack's environment, would otherwise steer outside the
// build's floor (contracts/executor.md, Build values pack would read): a proxy, a certificate file, GODEBUG, where
// registry credentials are read from. A build's are matched in any case, as Go reads the proxies in both, and so is
// every name starting PACK_ on a build; and every name starting DOCKER_, in any case, is reserved by that prefix, on
// both, for docker's client reads them.
var reserved = []reservation{
	{"PATH", true}, {"HOME", true}, {"TMPDIR", true}, {"DOCKER_HOST", true}, {"DOCKER_CONFIG", true}, {"DOCKER_CERT_PATH", true},
	{"DOCKER_TLS_VERIFY", true}, {"PACK_HOME", true}, {"XDG_CONFIG_HOME", true}, {"XDG_CACHE_HOME", true}, {"PACK_VOLUME_KEY", true},
	{"HTTP_PROXY", false}, {"HTTPS_PROXY", false}, {"ALL_PROXY", false}, {"NO_PROXY", false}, {"SSL_CERT_FILE", false},
	{"SSL_CERT_DIR", false}, {"GODEBUG", false}, {"REGISTRY_AUTH_FILE", false}, {"XDG_RUNTIME_DIR", false},
}

type reservation struct {
	name  string
	child bool // pack and docker need it from the executor's environment, else pack alone reads it, on a build
}

// childVars is reserved's child names: what childEnv hands pack and docker.
func childVars() []string {
	var names []string
	for _, r := range reserved {
		if r.child {
			names = append(names, r.name)
		}
	}
	return names
}

// isReserved is whether a call's env may not name k: a child's name, a DOCKER_ one, or on a build one pack reads.
func isReserved(k string, build bool) bool {
	prefixed := func(p string) bool { return len(k) >= len(p) && strings.EqualFold(k[:len(p)], p) }
	if prefixed("DOCKER_") || build && prefixed("PACK_") { // pack reads its own configuration from PACK_ names
		return true
	}
	return slices.ContainsFunc(reserved, func(r reservation) bool {
		return r.child && r.name == k || !r.child && build && strings.EqualFold(r.name, k)
	})
}

// reservedSaid names what isReserved refuses, for a refusal's line.
func reservedSaid(build bool) string {
	names := childVars()
	if build {
		for _, r := range reserved {
			if !r.child {
				names = append(names, r.name+" (in any case)")
			}
		}
	}
	if build {
		names = append(names, "any PACK_ name")
	}
	return strings.Join(append(names, "any DOCKER_ name"), ", ")
}

// packVolumeKey is PACK_VOLUME_KEY when the executor's environment sets none: pack names the volumes
// of a build it was given no cache name for after it, and warns, at the top of every app's build log,
// when it is unset. Every build here names its caches (canyon-cache-<app>), so the key names nothing
// the next build reads; it is random so no other user of a shared daemon can guess one (#1245).
var packVolumeKey = rand.Text()

// childEnv is the environment pack and docker get: childVars from the executor's, never its
// whole environment, so a stray secret in it cannot reach a build log.
func childEnv() []string {
	var env []string
	for _, k := range childVars() {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		} else if k == "PACK_VOLUME_KEY" {
			env = append(env, k+"="+packVolumeKey)
		}
	}
	return env
}

// trustTar is egress.ca as TrustDir's files (TrustFiles), a tar of etc/canyon-egress/ that docker cp lays at the
// container's root: the directory 0555 and each file 0444, root's. The system roots are the executor image's own,
// read now.
func (e *Executor) trustTar(roots []Root) ([]byte, error) {
	system, err := os.ReadFile(cmp.Or(e.Roots, SystemRoots)) //nolint:gosec // the executor's own setting or its image's path, never a request's
	if err != nil {
		return nil, fmt.Errorf("the executor's system roots: %w", err)
	}
	files, err := TrustFiles(roots, system)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	dir := strings.TrimPrefix(TrustDir, "/") + "/"
	if err := tw.WriteHeader(&tar.Header{Name: dir, Mode: 0o555, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: dir + name, Mode: 0o444, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// trustIn copies trust into the created container name and starts it.
func (e *Executor) trustIn(ctx context.Context, name string, trust []byte) error {
	cp := child(ctx, e.docker(), "cp", "-", name+":/")
	cp.Stdin = bytes.NewReader(trust)
	if out, err := cp.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp of %s into %s: %w: %s", TrustDir, name, err, strings.TrimSpace(string(out)))
	}
	if out, err := child(ctx, e.docker(), "start", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker start %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
