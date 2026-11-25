package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// demoMachine stands in for everything canyon demo reaches beyond this binary: Docker, the release
// it downloads from, the demo's own address, and the port it publishes. Its Docker keeps what the
// demo and anything else made, so a test can see what down removed and what it left alone.
type demoMachine struct {
	t        *testing.T
	home     string
	calls    []string                                // every docker command, in order
	docker   func(args string) (string, error, bool) // a case's own answer to one; false leaves the machine's
	up       bool                                    // docker compose up has run
	deployer string                                  // the deployer's exit status once up; "" while it runs
	sums     string                                  // the release's SHA256SUMS; "" names the bundle it serves
	fetched  int                                     // downloads from the release
	carried  []string
	env      map[string]string // what canyon reads of the environment
	run      func(args ...string) (out, errs string, code int)

	apps     []string            // the apps the demo's server lists
	answers  bool                // the demo's server answers
	server   string              // the compose project of the container named canyon-server, "" for none
	serverAt string              // that project's folder
	ctrs     map[string]string   // every container: its name, and the app its canyon.app label names ("" for none)
	transit  []string            // the build and scan containers made since the demo's executor started
	nets     map[string]string   // every network labelled canyon.app, and the app it names
	vols     []string            // every volume
	images   map[string]string   // canyon/*, canyon-scan/*, canyon-run, canyon-builder and buildpacksio/lifecycle: id → repository:tag
	refs     map[string]bool     // the images pulled by digest: the compose file's, those the executor pulls, the lifecycle's
	tags     map[string][]string // a pulled image's tags (RepoTags), by its reference: none unless a case gives some
	users    map[string][]string // the containers that use a pulled image, by its reference: "<name> <compose project>"
	project  []string            // the containers of the demo's compose project
	hangs    bool                // the deployer's log never ends
	imaged   bool                // every image the demo's compose file names is here
	cleanEnv int                 // compose config --images asked without the shell's CANYON_IMAGE_* variables
	store    string              // docker info's DriverStatus, as JSON: overlay2's unless a case says otherwise
}

// docker info's DriverStatus on each of Docker's two image stores, as hosted runners answered it (#165).
const (
	overlay2Status   = `[["Backing Filesystem","extfs"],["Supports d_type","true"],["Using metacopy","false"],["Native Overlay Diff","false"],["userxattr","false"]]`
	containerdStatus = `[["driver-type","io.containerd.snapshotter.v1"]]`
)

// A machine with everything the demo needs: Docker 28 with Compose, 8 CPUs, 8 GB, 57 GB free, port free.
func demoOn(t *testing.T) *demoMachine {
	t.Helper()
	m := &demoMachine{t: t, home: t.TempDir(), deployer: "0", answers: true, apps: []string{"claims-portal", "witty-wren"},
		ctrs: map[string]string{}, nets: map[string]string{}, images: map[string]string{}, refs: map[string]bool{},
		tags: map[string][]string{}, users: map[string][]string{}, store: overlay2Status}
	bundle := demoTar(t, map[string]string{"canyon/docker-compose.yml": "services: {}\n", "canyon/deploy/egress/floor.sh": "#!/bin/sh\n"})
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.fetched++
		switch r.URL.Path {
		case "/latest/download/SHA256SUMS":
			sums := m.sums
			if sums == "" {
				sums = fmt.Sprintf("%x  canyon_darwin_arm64.tar.gz\n%x  %s\n", sha256.Sum256(nil), sha256.Sum256(bundle), demoBundle)
			}
			_, _ = io.WriteString(w, sums)
		case "/latest/download/" + demoBundle:
			_, _ = w.Write(bundle)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(release.Close)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie(sessionCookie)
		m.carried = append(m.carried, cookie.String())
		if !m.answers {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/auth/config":
			_, _ = io.WriteString(w, `{"canyon":true,"auth":"off","org":"","device":false}`)
		case "/api/v1/fleet":
			rows := []map[string]string{}
			for _, app := range m.apps {
				rows = append(rows, map[string]string{"id": app, "name": app})
			}
			must(t, json.NewEncoder(w).Encode(map[string]any{"tiles": map[string]int{}, "apps": rows, "next": nil}))
		default:
			_, _ = io.WriteString(w, `{"stacks":[],"design_system":null,"setup_available":false}`)
		}
	}))
	t.Cleanup(host.Close)
	free, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	must(t, free.Close()) // an address nothing listens at
	was := [...]any{demoDocker, demoReleases, demoURL, demoPort, demoPause, goos}
	t.Cleanup(func() {
		demoDocker = was[0].(func(context.Context, string, io.Writer, ...string) (string, error))
		demoReleases, demoURL, demoPort = was[1].(string), was[2].(string), was[3].(string)
		demoPause, goos = was[4].(time.Duration), was[5].(string)
	})
	demoReleases, demoURL, demoPort, demoPause, goos = release.URL, host.URL, free.Addr().String(), 0, "darwin"
	demoDocker = func(ctx context.Context, _ string, out io.Writer, args ...string) (string, error) {
		line := strings.Join(args, " ")
		m.calls = append(m.calls, line)
		if m.docker != nil {
			if answer, err, ok := m.docker(line); ok {
				return answer, err
			}
		}
		if line == compose("config --images") && ctx.Value(demoNoImageEnv{}) != nil {
			m.cleanEnv++ // asked without the shell's CANYON_IMAGE_* variables
		}
		if strings.Contains(line, "--filter since=e1e2e3e4e5e6") && !m.up { // Docker answers a since= that names a removed container with an error
			return "", errors.New("exit status 1: Error response from daemon: No such container: e1e2e3e4e5e6")
		}
		if ref, one := strings.CutPrefix(line, "image inspect --format {{.Id}} "); one && !strings.Contains(ref, " ") && m.refs[ref] {
			return demoID(ref), nil // one image pulled by digest, there
		}
		if ref, one := strings.CutPrefix(line, "image inspect --format {{json .RepoTags}} "); one && m.refs[ref] {
			tags, err := json.Marshal(append([]string{}, m.tags[ref]...))
			must(t, err)
			return string(tags), nil
		}
		if id, ok := strings.CutPrefix(line, "ps -a --filter ancestor="); ok {
			id, _, _ = strings.Cut(id, " ")
			for ref, rows := range m.users {
				if demoID(ref) == id {
					return strings.Join(rows, "\n"), nil
				}
			}
			return "", nil
		}
		if strings.HasPrefix(line, "image inspect ") && !m.imaged {
			return "", errors.New("exit status 1: Error response from daemon: No such image")
		}
		if line == compose("logs --follow --no-log-prefix demo-apps") && m.hangs {
			<-ctx.Done()
			return "", ctx.Err()
		}
		answer := m.answer(line)
		if out != nil {
			_, _ = io.WriteString(out, answer+"\n")
			return "", nil
		}
		return answer, nil
	}
	m.env = map[string]string{"HOME": m.home, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	m.run = func(args ...string) (string, string, int) {
		t.Helper()
		return run(t, m.env, false, append([]string{"demo"}, args...)...)
	}
	return m
}

// compose is the demo's own compose call, as canyon demo makes it.
func compose(args string) string { return "compose -p canyon-demo --profile demo-apps " + args }

// answer is the machine's Docker: what it says to each command canyon demo runs, and what a removal
// takes away.
func (m *demoMachine) answer(line string) string {
	fields := strings.Fields(line)
	switch {
	case strings.HasPrefix(line, "info "):
		return "28.1.1 8 8323002368 docker-desktop " + m.store
	case line == "compose version --short":
		return "2.35.0"
	case strings.HasPrefix(line, "run --rm "+demoProbe+" df"):
		return "Filesystem           1K-blocks      Used Available Use% Mounted on\noverlay               61202244   1000000  60202244   2% /"
	case line == compose("up -d"):
		m.up, m.server, m.serverAt, m.answers = true, demoProject, filepath.Join(m.home, ".canyon", "demo", "canyon"), true
		m.project = []string{"canyon-server", "canyon-demo-executor-1", "canyon-demo-demo-apps-1"}
		for _, app := range m.apps {
			m.ctrs[app+"-preview"], m.nets["canyon-app-"+app] = app, app
			m.vols = append(m.vols, "canyon-cache-"+app+"-build", "canyon-cache-"+app+"-launch")
			m.images["id-"+app] = "canyon/" + app + ":pkg_1"
		}
		m.images["id-builder"], m.images["id-scan"] = "canyon-builder:0a1b2c3d4e5f", "canyon-scan/opengrep:latest"
		m.images["id-lifecycle-other"] = "buildpacksio/lifecycle:0.20.0" // another lifecycle, pack's own pull for another builder: never the demo's
		// the lifecycle pack pulled by its platform's digest, the scanners' images and the compose file's
		for _, ref := range append(strings.Split(m.answer(compose("config --images")), "\n"), names.TrivyImage, names.CheckovImage, names.LifecyclePlatforms[1]) {
			m.refs[ref] = true
		}
		m.vols = append(m.vols, "canyon-scandb-trivy")
	case line == compose("logs --follow --no-log-prefix demo-apps"):
		return "Deploying claims-portal\ncanyon-demo: running claims-portal\ncanyon-demo: running endorsement-hub\ncanyon-demo: running claims-archive\ncanyon-demo: running premium-ledger\nThe sample apps are running, and claims-portal's next version is blocked at review."
	case line == compose("ps -aq --status exited demo-apps"):
		if m.up && m.deployer != "" {
			return "4f1c9d2ab3e0"
		}
	case line == "inspect --format {{.State.ExitCode}} 4f1c9d2ab3e0":
		return m.deployer
	case line == "ps -aq --filter name=^canyon-server$":
		if m.server != "" {
			return "9a8b7c6d5e4f"
		}
	case line == "ps -q --filter name=^canyon-server$":
		if m.server != "" && m.up {
			return "9a8b7c6d5e4f"
		}
	case line == "inspect --format {{json .Config.Labels}} canyon-server":
		labels, err := json.Marshal(map[string]string{"com.docker.compose.project": m.server, "com.docker.compose.project.working_dir": m.serverAt})
		must(m.t, err)
		return string(labels)
	case line == compose("config --images"):
		return "ghcr.io/getcanyon-ai/canyon-server@sha256:aa\nghcr.io/getcanyon-ai/canyon-executor@sha256:bb\ndocker.io/library/busybox@sha256:cc\npostgres:16-alpine@sha256:dd"
	case line == compose("ps -aq executor"):
		if m.up {
			return "e1e2e3e4e5e6"
		}
	case line == compose("ps -a --format {{.Name}}"):
		return strings.Join(m.project, "\n")
	case strings.HasPrefix(line, compose("down -v")): // the server stops with it; --rmi all left the images it pulled by digest (#170)
		m.up, m.server, m.project, m.answers = false, "", nil, false
	case line == `ps -a --filter label=canyon.app --format {{.Names}} {{.Label "canyon.app"}}`:
		return m.listed(m.ctrs)
	case line == `network ls --filter label=canyon.app --format {{.Name}} {{.Label "canyon.app"}}`:
		return m.listed(m.nets)
	case line == "ps -a --filter label=canyon.build --filter since=e1e2e3e4e5e6 --format {{.Names}}":
		return strings.Join(m.transit, "\n")
	case line == "ps -a --filter label=canyon.scan --filter since=e1e2e3e4e5e6 --format {{.Names}}", line == "ps -a --filter label=canyon.scan --format {{.Names}}":
		return ""
	case line == "ps -a --filter label=canyon.build --format {{.Names}}": // every build on the host: one from before the demo's executor too
		return strings.Join(append([]string{"canyon-build-0ld0ld00"}, m.transit...), "\n")
	case line == "volume ls -q --filter name=canyon-cache-", line == "volume ls -q --filter dangling=true --filter name=canyon-scandb-":
		return strings.Join(m.vols, "\n") // name= is a substring: canyon keeps only the names it means
	case line == "images --filter reference=canyon/* --format {{.ID}} {{.Repository}}:{{.Tag}}":
		return m.imageRows(func(ref string) bool { return strings.HasPrefix(ref, "canyon/") })
	case line == "images --filter reference=canyon-scan/* --filter reference=canyon-run --filter reference=canyon-builder --format {{.ID}} {{.Repository}}:{{.Tag}}":
		return m.imageRows(func(ref string) bool {
			return !strings.HasPrefix(ref, "canyon/") && !strings.HasPrefix(ref, "buildpacksio/")
		})
	case line == "images --filter reference=buildpacksio/lifecycle --format {{.ID}} {{.Repository}}:{{.Tag}}":
		return m.imageRows(func(ref string) bool { return strings.HasPrefix(ref, "buildpacksio/lifecycle:") })
	case line == "network ls --filter label=canyon.build --format {{.Name}}":
		return ""
	case len(fields) > 2 && fields[0] == "rm" && fields[1] == "-f":
		for _, name := range fields[2:] {
			delete(m.ctrs, name)
			m.transit = slices.DeleteFunc(m.transit, func(s string) bool { return s == name })
		}
	case len(fields) > 2 && fields[0] == "network" && fields[1] == "rm":
		for _, name := range fields[2:] {
			delete(m.nets, name)
		}
	case len(fields) > 2 && fields[0] == "volume" && fields[1] == "rm":
		m.vols = slices.DeleteFunc(m.vols, func(v string) bool { return slices.Contains(fields[2:], v) })
	case len(fields) > 3 && fields[0] == "image" && fields[1] == "rm" && fields[2] == "-f":
		for _, id := range fields[3:] {
			delete(m.images, id)
			delete(m.refs, id)
		}
	}
	return ""
}

// demoID is the ID the machine's Docker gives the image a reference pulled.
func demoID(ref string) string { return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(ref))) }

func (m *demoMachine) listed(byName map[string]string) string {
	var rows []string
	for name, app := range byName {
		rows = append(rows, name+" "+app)
	}
	slices.Sort(rows)
	return strings.Join(rows, "\n")
}

func (m *demoMachine) imageRows(keep func(string) bool) string {
	var rows []string
	for id, ref := range m.images {
		if keep(ref) {
			rows = append(rows, id+" "+ref)
		}
	}
	slices.Sort(rows)
	return strings.Join(rows, "\n")
}

func (m *demoMachine) ran(prefix string) int {
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// another is a Canyon this demo did not start: an app of its own, its network and caches, its image,
// and a build of its own from before the demo's executor started.
func (m *demoMachine) another() {
	m.ctrs["ux-data-preview"], m.nets["canyon-app-ux-data"] = "ux-data", "ux-data"
	m.ctrs["some-postgres"] = ""
	m.vols = append(m.vols, "canyon-cache-ux-data-build", "canyon-cache-ux-data-launch", "canyon-cache-claims-portal-old")
	m.images["id-ux-data"] = "canyon/ux-data:pkg_9"
}

// golden compares out, with the test's own addresses and folder put back to the ones a person sees,
// to the file in testdata. CANYON_UPDATE_GOLDEN=1 rewrites it.
func (m *demoMachine) golden(name, out string) {
	m.t.Helper()
	out = strings.NewReplacer(demoURL, "http://localhost:7777", m.home, "/home/you").Replace(out)
	path := filepath.Join("testdata", name)
	if os.Getenv("CANYON_UPDATE_GOLDEN") == "1" {
		must(m.t, os.WriteFile(path, []byte(out), 0o644)) //nolint:gosec // a fixture, world-readable like every file in the repo
	}
	if want := golden(m.t, name); out != want {
		m.t.Errorf("%s is not what canyon printed; got\n%s", path, out)
	}
}

func demoTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := io.WriteString(tw, body)
		must(t, err)
	}
	must(t, tw.Close(), gz.Close())
	return b.Bytes()
}

// The acceptance's first line: on a machine with no engine, the list, exit 1, and nothing changed.
func TestDemoUpOnAMachineWithNoDockerSaysTheFixAndStartsNothing(t *testing.T) {
	m := demoOn(t)
	m.docker = func(string) (string, error, bool) { return "", exec.ErrNotFound, true }
	out, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "This machine is not ready for the demo. Nothing was started.") ||
		!strings.Contains(errs, "1. Docker is not installed.\n     Fix: brew install colima docker docker-compose && colima start --cpu 4 --memory 8 --disk 60") ||
		!strings.Contains(errs, "Then run: canyon demo up") {
		t.Fatalf("exit %d, stdout %q, stderr\n%s", code, out, errs)
	}
	if m.fetched != 0 || len(m.calls) != 1 {
		t.Errorf("a machine that is not ready was still worked on: %d downloads, docker %q", m.fetched, m.calls)
	}
	if _, err := os.Stat(filepath.Join(m.home, ".canyon")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused start left ~/.canyon behind (%v)", err)
	}
}

// docker info exits 0 with no engine behind it: an empty version and no CPUs are an engine that
// does not answer, never a Docker that is version 0.
func TestDemoUpReadsAnEngineThatDoesNotAnswer(t *testing.T) {
	m := demoOn(t)
	m.docker = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "info ") {
			return "0 0", nil, true
		}
		return "", errors.New("exit status 1: Cannot connect to the Docker daemon"), true
	}
	_, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "1. Docker is installed, but its engine does not answer.\n     Fix: colima start --cpu 4 --memory 8 --disk 60\n          Or start Docker Desktop or OrbStack.") || strings.Contains(errs, "2. ") {
		t.Fatalf("exit %d, stderr\n%s", code, errs)
	}
	goos = "linux"
	if _, errs, _ := m.run("up"); !strings.Contains(errs, "Fix: sudo systemctl start docker") {
		t.Errorf("on Linux the fix reads\n%s", errs)
	}
}

// The disk preflight asks for is the one this Docker's image store needs: more on the containerd store, which keeps an
// image's layers twice (#174's decision 2). A store docker info doesn't name readably counts as containerd, the one
// that needs more. The gap names the store as Docker does, and never tells anyone to switch.
func TestDemoUpAsksTheDiskItsImageStoreNeeds(t *testing.T) {
	for _, c := range []struct {
		name, store, want string
	}{
		{"overlay2", overlay2Status, ""},
		{"containerd", containerdStatus, "1. Docker has 20 GB of free disk, and the demo needs about 30 GB with Docker's containerd image store, which this Docker uses."},
		{"unread", `"not a list"`, "1. Docker has 20 GB of free disk, and the demo needs about 30 GB with Docker's containerd image store, which this Docker uses."},
		{"no status", "null", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := demoOn(t)
			m.store = c.store
			m.docker = func(line string) (string, error, bool) {
				if strings.HasPrefix(line, "run ") {
					return "Filesystem 1K-blocks Used Available Use% Mounted on\noverlay 61202244 40000000 20971520 66% /", nil, true // 20 GB free
				}
				return "", nil, false
			}
			_, errs, code := m.run("up")
			if c.want == "" && (code != exitOK || strings.Contains(errs, "free disk")) {
				t.Fatalf("20 GB on %s: exit %d, stderr %q", c.name, code, errs)
			}
			if c.want != "" && (code != exitError || !strings.Contains(errs, c.want) || strings.Contains(strings.ToLower(errs), "switch")) {
				t.Fatalf("20 GB on %s: exit %d, stderr\n%s", c.name, code, errs)
			}
		})
	}
}

// Every gap is in one list, each with its own fix, so a person fixes the machine in one round.
func TestDemoUpListsEveryGapAtOnce(t *testing.T) {
	m := demoOn(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { _ = held.Close() })
	demoPort = held.Addr().String()
	m.docker = func(line string) (string, error, bool) {
		switch {
		case strings.HasPrefix(line, "info "):
			return "27.5.1 2 2063007744 colima " + overlay2Status, nil, true
		case line == "compose version --short":
			return "", errors.New("exit status 1: docker: 'compose' is not a docker command"), true
		case strings.HasPrefix(line, "run "):
			return "Filesystem 1K-blocks Used Available Use% Mounted on\noverlay 61202244 49667908 11534336 82% /", nil, true
		}
		return "", nil, false
	}
	_, errs, code := m.run("up")
	for _, want := range []string{
		"1. Docker is 27.5.1, and the demo needs 28 or newer.",
		"2. Docker has no Compose v2 (docker compose).\n     Fix: brew install docker-compose && mkdir -p ~/.docker/cli-plugins",
		"3. Docker has 2 CPUs and 1.9 GB of memory, and the demo needs 4 CPUs and 6 GB.\n     Fix: colima stop && colima start --cpu 4 --memory 8",
		"4. Docker has 11 GB of free disk, and the demo needs about 18 GB.",
		"5. Port 7777 on 127.0.0.1 is in use by another program, and Canyon's dashboard needs it.",
	} {
		if !strings.Contains(errs, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	if code != exitError || m.fetched != 0 || m.ran("compose -p") != 0 {
		t.Errorf("exit %d, %d downloads, docker %q:\n%s", code, m.fetched, m.calls, errs)
	}
	m.golden("demo-up-not-ready.txt", errs)
}

// A Canyon started another way, from a folder with docker compose, holds the names the demo's
// containers take: the list says which, where, and the command that stops it there.
func TestDemoUpNamesACanyonStartedAnotherWay(t *testing.T) {
	m := demoOn(t)
	m.server, m.serverAt = "canyon", "/home/you/canyon"
	_, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "1. A Canyon started another way is running here: Docker Compose project canyon, in /home/you/canyon.\n     Fix: stop it there: cd /home/you/canyon && docker compose --profile demo-apps down") {
		t.Fatalf("exit %d, stderr\n%s", code, errs)
	}
	if m.fetched != 0 || m.ran("compose -p") != 0 {
		t.Errorf("a machine whose names are taken was still worked on: %d downloads, docker %q", m.fetched, m.calls)
	}
}

// The acceptance's second line: the five stages in order, ending on the one line.
func TestDemoUpFetchesDownloadsStartsAndSignsIn(t *testing.T) {
	m := demoOn(t)
	failed := false
	m.docker = func(line string) (string, error, bool) {
		if line == compose("pull --quiet") && !failed {
			failed = true // one failed lookup, tried again
			return "", errors.New("exit status 1: dial tcp: lookup ghcr.io: no such host"), true
		}
		return "", nil, false
	}
	out, errs, code := m.run("up")
	if code != exitOK || !strings.HasSuffix(out, "\n  ✓ Canyon is ready at "+demoURL+"\n\n  Next, take the guided tour: canyon demo tour\n") {
		t.Fatalf("exit %d, stderr %q, stdout\n%s", code, errs, out)
	}
	for _, step := range []string{"Checking this machine", "Fetching the demo into ~/.canyon/demo", "Downloading Canyon's images", "Starting Canyon and its sample apps. A first start takes 20 to 30 minutes.", "Connected to " + demoURL} {
		if !strings.Contains(out, step) {
			t.Errorf("the run does not say %q:\n%s", step, out)
		}
	}
	if strings.Contains(out+errs, "The sample apps are running") || strings.Contains(out, "canyon fleet") {
		t.Errorf("a start that worked printed the deployer's log, or a next step of login's:\n%s%s", out, errs)
	}
	if got := m.ran(compose("pull --quiet")); got != 2 {
		t.Errorf("the pull ran %d times, want one failure and one retry", got)
	}
	stack := filepath.Join(m.home, ".canyon", "demo", "canyon")
	body, err := os.ReadFile(filepath.Join(stack, "docker-compose.yml")) //nolint:gosec // the test's own temporary home
	if err != nil || string(body) != "services: {}\n" {
		t.Errorf("the bundle was not unpacked into ~/.canyon/demo: %q, %v", body, err)
	}
	m.golden("demo-up.txt", out)

	// A second up on a running demo checks, signs in and reports: it downloads and deploys nothing again.
	before, fetched := len(m.calls), m.fetched
	out, _, code = m.run("up")
	if code != exitOK || !strings.HasSuffix(out, "Canyon is ready at "+demoURL+"\n\n  Next, take the guided tour: canyon demo tour\n") || strings.Contains(out, "different release") {
		t.Fatalf("a second up: exit %d\n%s", code, out)
	}
	for _, c := range m.calls[before:] {
		if strings.HasPrefix(c, compose("pull")) || strings.HasPrefix(c, compose("up")) {
			t.Errorf("a second up on a running demo ran docker %s", c)
		}
	}
	if m.fetched != fetched+1 { // SHA256SUMS alone, to tell whether the demo running is this release's
		t.Errorf("a second up on a running demo downloaded %d files", m.fetched-fetched)
	}
}

// A demo another release started keeps running when this CLI's up finds it, and up says so, with
// the two commands that start this release's.
func TestDemoUpSaysWhenTheDemoRunningIsAnotherRelease(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.sums = strings.Repeat("1", 64) + "  " + demoBundle + "\n" // the release has moved on since
	out, _, code := m.run("up")
	if code != exitOK || !strings.Contains(out, "! The demo running here is from a different release than this CLI ("+Version+"). To switch: canyon demo down, then canyon demo up.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if m.ran(compose("up")) != 1 {
		t.Errorf("up started the demo again: %q", m.calls)
	}
}

// up signs this machine in to the demo, which makes it the host every canyon command reaches: up
// says so when it replaces another, and down names the command that goes back.
func TestDemoUpSaysTheHostItReplacesAndDownNamesTheWayBack(t *testing.T) {
	m := demoOn(t)
	writeConfig(t, m.env["XDG_CONFIG_HOME"], "https://canyon.example.com", aSession("ab"), 0o600)
	out, errs, code := m.run("up")
	if code != exitOK || !strings.Contains(out, "canyon commands now go to the demo instead of https://canyon.example.com. canyon demo down tells you how to switch back.") {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, out)
	}
	if out, _, _ := m.run("up"); strings.Contains(out, "now go to the demo") {
		t.Errorf("a second up said the switch again, though the demo was the host already:\n%s", out)
	}
	out, errs, code = m.run("down")
	if code != exitOK || !strings.Contains(out, "To use https://canyon.example.com again: canyon login https://canyon.example.com") {
		t.Fatalf("down: exit %d, stderr %q\n%s", code, errs, out)
	}

	m = demoOn(t)
	m.env["CANYON_HOST"] = "https://canyon.example.com"
	if _, errs, code := m.run("up"); code != exitError || m.ran(compose("")) != 0 ||
		!strings.Contains(errs, "1. CANYON_HOST is set to https://canyon.example.com, so canyon commands would go there instead of the demo.\n     Fix: unset CANYON_HOST, then run canyon demo up again.") {
		t.Errorf("exit %d, docker %q\n%s", code, m.calls, errs)
	}
}

// CI starts the demo from a release built from its tree: CANYON_DEMO_FROM names where that one is served.
func TestDemoUpFetchesFromTheReleaseItIsPointedAt(t *testing.T) {
	m := demoOn(t)
	from := demoReleases + "/latest/download"
	demoReleases = "http://127.0.0.1:1" // nothing answers here: only the named release can
	m.env["CANYON_DEMO_FROM"] = from
	if _, errs, code := m.run("up"); code != exitOK || m.fetched != 2 {
		t.Fatalf("exit %d, %d downloads from the named release, stderr %q", code, m.fetched, errs)
	}
}

// The bundle is held to SHA256SUMS before a byte of it is unpacked, as install.sh holds the CLI.
func TestDemoUpRefusesABundleTheReleaseDidNotSum(t *testing.T) {
	m := demoOn(t)
	m.sums = strings.Repeat("0", 64) + "  " + demoBundle + "\n"
	_, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "but SHA256SUMS says "+strings.Repeat("0", 64)+"; nothing was unpacked") || !strings.Contains(errs, "To carry on, run: canyon demo up") {
		t.Fatalf("exit %d, stderr\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(m.home, ".canyon", "demo", "canyon")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a bundle with the wrong hash was unpacked (%v)", err)
	}
	if m.ran("compose -p") != 0 {
		t.Errorf("docker compose ran on a bundle that was refused: %q", m.calls)
	}
}

func TestDemoUpRefusesABundleThatWritesOutsideItsFolder(t *testing.T) {
	err := demoUnpack(demoTar(t, map[string]string{"../../.ssh/authorized_keys": "x"}), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "a path outside the demo's folder") {
		t.Errorf("got %v", err)
	}
}

// A deployer that failed is said, with the one command that carries on, and nobody is signed in.
func TestDemoUpSaysHowToCarryOnWhenTheSampleAppsDoNotDeploy(t *testing.T) {
	m := demoOn(t)
	m.deployer = "1"
	out, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "The sample apps did not finish deploying: their deployer exited 1") || !strings.Contains(errs, "To carry on, run: canyon demo up") {
		t.Fatalf("exit %d, stderr\n%s", code, errs)
	}
	if !strings.Contains(errs, "canyon-demo: running premium-ledger\nThe sample apps are running") {
		t.Errorf("a start that failed does not show the end of the deployer's log:\n%s", errs)
	}
	if strings.Contains(out, "Canyon is ready") || len(m.carried) != 0 {
		t.Errorf("a demo whose apps did not deploy was called ready:\n%s", out)
	}

	// A deployer whose log ended while it had not exited has no exit status to name.
	m = demoOn(t)
	m.deployer = ""
	if _, errs, code := m.run("up"); code != exitError || strings.Contains(errs, "exited ;") ||
		!strings.Contains(errs, "The sample apps haven't finished deploying, and Docker stopped showing their progress") {
		t.Fatalf("exit %d, stderr\n%s", code, errs)
	}
}

// pack writes each Windows-reserved name inside a cache volume's name spelt out, falcon as falc_o_n
// (contracts/executor.md, /forget): demo down --purge removes the volumes pack made.
func TestDemoDownRemovesTheCachesAsPackNamedThem(t *testing.T) {
	m := demoOn(t)
	m.apps = []string{"smooth-falcon-lei"}
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	for i, v := range m.vols {
		m.vols[i] = strings.ReplaceAll(v, "falcon", "falc_o_n") // what pack made of canyon-cache-smooth-falcon-lei-*
	}
	m.calls = nil
	if _, errs, code := m.run("down", "--purge"); code != exitOK || m.ran("volume rm canyon-cache-smooth-falc_o_n-lei-build canyon-cache-smooth-falc_o_n-lei-launch canyon-scandb-trivy") != 1 {
		t.Errorf("down --purge: exit %d, stderr %q, ran %q", code, errs, m.calls)
	}
}

// The acceptance's fourth line. down removes what the demo started and nothing another Canyon did;
// --purge takes the images and the bundle too.
func TestDemoDownRemovesWhatTheDemoStarted(t *testing.T) {
	m := demoOn(t)
	if out, _, code := m.run("down"); code != exitOK || !strings.Contains(out, "Nothing to stop: no demo is running (looked in ~/.canyon/demo and for a container named canyon-server).") {
		t.Fatalf("down with no demo: exit %d, docker %q\n%s", code, m.calls, out)
	}
	m.another()
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.transit = []string{"canyon-build-1a2b3c4d"}
	m.calls = nil
	out, errs, code := m.run("down")
	if code != exitOK || errs != "" || !strings.Contains(out, "Its images stay, so the next start is quick.") {
		t.Fatalf("down: exit %d, stderr %q\n%s", code, errs, out)
	}
	if m.ran(compose("down -v")) != 1 || m.ran("rm -f canyon-build-1a2b3c4d claims-portal-preview witty-wren-preview") != 1 ||
		m.ran("network rm canyon-app-claims-portal canyon-app-witty-wren") != 1 ||
		m.ran("volume rm canyon-cache-claims-portal-build canyon-cache-claims-portal-launch canyon-cache-witty-wren-build canyon-cache-witty-wren-launch") != 1 ||
		m.ran("image rm") != 0 {
		t.Errorf("down ran %q", m.calls)
	}
	for _, other := range []string{"ux-data-preview", "some-postgres"} {
		if _, ok := m.ctrs[other]; !ok {
			t.Errorf("down removed %s, which the demo did not start", other)
		}
	}
	if _, ok := m.nets["canyon-app-ux-data"]; !ok || !slices.Contains(m.vols, "canyon-cache-ux-data-build") || !slices.Contains(m.vols, "canyon-cache-claims-portal-old") || !slices.Contains(m.vols, "canyon-scandb-trivy") {
		t.Errorf("down removed what another Canyon made, or the scanners' databases: networks %v, volumes %v", m.nets, m.vols)
	}
	if _, err := os.Stat(filepath.Join(m.home, ".canyon", "demo", demoBundle)); err != nil {
		t.Errorf("down without --purge removed the bundle: %v", err)
	}
	m.golden("demo-down.txt", out)

	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up again: exit %d, %s", code, errs)
	}
	m.calls = nil
	out, errs, code = m.run("down", "--purge")
	if code != exitOK || errs != "" || !strings.Contains(out, "The demo is gone: its containers, data and images, and its files in ~/.canyon/demo.") {
		t.Fatalf("down --purge: exit %d, stderr %q\n%s", code, errs, out)
	}
	removed := slices.Sorted(slices.Values([]string{names.LifecyclePlatforms[1], names.CheckovImage, names.TrivyImage, "docker.io/library/busybox@sha256:cc", "postgres:16-alpine@sha256:dd",
		"ghcr.io/getcanyon-ai/canyon-executor@sha256:bb", "ghcr.io/getcanyon-ai/canyon-server@sha256:aa", "id-builder", "id-claims-portal", "id-scan", "id-witty-wren"}))
	if m.ran(compose("down -v --rmi all")) != 1 || m.ran("image rm -f "+strings.Join(removed, " ")) != 1 ||
		m.ran("volume rm canyon-cache-claims-portal-build canyon-cache-claims-portal-launch canyon-cache-witty-wren-build canyon-cache-witty-wren-launch canyon-scandb-trivy") != 1 {
		t.Errorf("down --purge ran %q", m.calls)
	}
	if _, ok := m.images["id-ux-data"]; !ok {
		t.Errorf("down --purge removed another Canyon's app image")
	}
	if _, err := os.Stat(filepath.Join(m.home, ".canyon", "demo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("down --purge left ~/.canyon/demo behind (%v)", err)
	}
	m.golden("demo-down-purge.txt", out)
}

// down --purge after a down, as down's own last line suggests: the server is gone with the first down, and purge
// still removes every image the demo made (#170): its apps', from the list up and down kept, the compose file's and
// those its executor pulled, by their references; and nothing another Canyon made.
func TestDemoDownPurgeAfterADownRemovesWhatTheDemoMade(t *testing.T) {
	for _, other := range []bool{false, true} {
		m := demoOn(t)
		if other {
			m.another()
		}
		if _, errs, code := m.run("up"); code != exitOK {
			t.Fatalf("up: exit %d, %s", code, errs)
		}
		if _, errs, code := m.run("down"); code != exitOK {
			t.Fatalf("down: exit %d, %s", code, errs)
		}
		out, errs, code := m.run("down", "--purge")
		if code != exitOK || errs != "" || !strings.Contains(out, "The demo is gone: its containers, data and images") {
			t.Fatalf("another Canyon here %v: down --purge after a down: exit %d, stderr %q\n%s", other, code, errs, out)
		}
		want := []string{"buildpacksio/lifecycle:0.20.0"} // another lifecycle: the demo's is its pin's platform digest alone
		if other {
			want = append(want, "canyon/ux-data:pkg_9")
		}
		if left := slices.Sorted(maps.Values(m.images)); !slices.Equal(left, want) {
			t.Errorf("another Canyon here %v: down --purge after a down left images %q, or took another Canyon's", other, left)
		}
		if len(m.refs) != 0 {
			t.Errorf("another Canyon here %v: down --purge after a down left the images pulled by digest: %v", other, slices.Sorted(maps.Keys(m.refs)))
		}
		if other {
			if _, ok := m.ctrs["ux-data-preview"]; !ok {
				t.Errorf("down --purge removed another Canyon's container")
			}
		}
	}
}

// Another Canyon's compose project holds canyon-server, while ~/.canyon/demo still holds the demo's files, as after a
// down and a team Canyon started since: down and down --purge refuse it by name, never ask its server which apps are
// its own, never keep them as the demo's, and touch nothing of it (#170).
func TestDemoDownNeverActsOnAnotherCanyonWhileTheDemosFilesAreHere(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	if _, errs, code := m.run("down"); code != exitOK {
		t.Fatalf("down: exit %d, %s", code, errs)
	}
	m.server, m.serverAt, m.answers, m.apps = "canyon", "/home/you/team", true, []string{"team-app"} // a team Canyon, on the demo's port
	m.ctrs["team-app-preview"] = "team-app"
	for _, args := range [][]string{{"down"}, {"down", "--purge"}} {
		m.calls = nil
		out, errs, code := m.run(args...)
		if code != exitError || !strings.Contains(errs, "This Canyon wasn't started by canyon demo up: Docker Compose project canyon, in /home/you/team.") || m.ran("rm") != 0 || m.ran("image rm") != 0 || m.ran("compose") != 0 {
			t.Errorf("%v with another Canyon running: exit %d, stdout %q, stderr %q, ran %q", args, code, out, errs, m.calls)
		}
	}
	if kept, _ := demoKept(filepath.Join(m.home, ".canyon", "demo")); slices.Contains(kept, "team-app") {
		t.Errorf("the team Canyon's app was kept as the demo's: %v", kept)
	}
	if _, ok := m.ctrs["team-app-preview"]; !ok {
		t.Errorf("down removed the team Canyon's app")
	}
}

// On Docker's containerd image store, removing a reference by digest takes every tag of its repository at that digest
// with it (#170): purge keeps an image the demo pulled when a container outside the demo uses it, running or stopped,
// or when it carries a tag, and says which and why.
func TestDemoDownPurgeKeepsAnImageSomethingElseHolds(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.tags["docker.io/library/busybox@sha256:cc"] = []string{"busybox:1.37"}        // the person's own tag, at the demo's digest
	m.users["postgres:16-alpine@sha256:dd"] = []string{"team-postgres canyon-team"} // a stopped team Canyon's database
	m.users[names.CheckovImage] = []string{"canyon-demo-scan-1 " + demoProject}     // the demo's own: no reason to keep it
	// the containerd store names an image pulled by digest by that reference: no tag of anyone's
	m.tags["ghcr.io/getcanyon-ai/canyon-server@sha256:aa"] = []string{"ghcr.io/getcanyon-ai/canyon-server@sha256:aa"}
	if _, errs, code := m.run("down"); code != exitOK {
		t.Fatalf("down: exit %d, %s", code, errs)
	}
	out, errs, code := m.run("down", "--purge")
	if code != exitOK || errs != "" || !strings.Contains(out, "These images stay, because something else on this Docker holds them:\n"+
		"    image docker.io/library/busybox@sha256:cc (it is also tagged busybox:1.37)\n"+
		"    image postgres:16-alpine@sha256:dd (the container team-postgres uses it)\n") {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, out)
	}
	if left := slices.Sorted(maps.Keys(m.refs)); !slices.Equal(left, []string{"docker.io/library/busybox@sha256:cc", "postgres:16-alpine@sha256:dd"}) {
		t.Errorf("purge left %q, want the two something else holds", left)
	}
}

// A plain tag on an image the compose file names as <name>:<tag>@<digest> is a person's own, even that very tag: the
// demo's pull by digest makes no plain tag on the containerd store, where removing the digest would take it (#170).
func TestDemoDownPurgeKeepsAPersonsTagTheComposeFileAlsoWrites(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.tags["postgres:16-alpine@sha256:dd"] = []string{"postgres:16-alpine"} // the person's own, no container on it
	out, errs, code := m.run("down", "--purge")
	if code != exitOK || !strings.Contains(out, "    image postgres:16-alpine@sha256:dd (it is also tagged postgres:16-alpine)\n") || !m.refs["postgres:16-alpine@sha256:dd"] {
		t.Fatalf("exit %d, stderr %q\n%s", code, errs, out)
	}
}

// Only a running canyon-server of the demo's is asked which apps are its own: with it stopped, whatever answers on the
// demo's port is something else, and down takes the apps as up kept them (#170).
func TestDemoDownAsksNoServerWhileTheDemosIsStopped(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.up = false                  // canyon-server is there, stopped
	m.apps = []string{"stranger"} // what answers on the port now
	m.ctrs["stranger-preview"] = "stranger"
	if _, errs, code := m.run("down"); code != exitOK {
		t.Fatalf("down: exit %d, %s", code, errs)
	}
	kept, _ := demoKept(filepath.Join(m.home, ".canyon", "demo"))
	if _, there := m.ctrs["stranger-preview"]; !there || slices.Contains(kept, "stranger") {
		t.Fatalf("down took what answers on the port as the demo's: containers %v, kept %v", m.ctrs, kept)
	}
}

// The images purge removes are the ones the compose file named when up started the demo, as up kept them: a
// CANYON_IMAGE_* in the shell at purge time points purge nowhere else. A demo whose up kept none reads the file's own
// pins, without the shell's CANYON_IMAGE_* variables (#170).
func TestDemoDownPurgeRemovesTheImagesUpStartedWith(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.docker = func(line string) (string, error, bool) { // the shell now names another holder
		if line == compose("config --images") {
			return "mirror.example/holder@sha256:ee", nil, true
		}
		return "", nil, false
	}
	m.refs["mirror.example/holder@sha256:ee"] = true
	if _, errs, code := m.run("down", "--purge"); code != exitOK {
		t.Fatalf("down --purge: exit %d, %s", code, errs)
	}
	if !m.refs["mirror.example/holder@sha256:ee"] || m.refs["docker.io/library/busybox@sha256:cc"] {
		t.Errorf("purge followed the shell's images, not the ones up started with: %v", slices.Sorted(maps.Keys(m.refs)))
	}

	m = demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	must(t, os.Remove(demoImagesKept(filepath.Join(m.home, ".canyon", "demo"))))
	if _, errs, code := m.run("down", "--purge"); code != exitOK || m.cleanEnv == 0 {
		t.Errorf("a demo up kept no images for: exit %d, %s, the compose file read without CANYON_IMAGE_* %d times", code, errs, m.cleanEnv)
	}
	env := demoWithoutImageEnv([]string{"PATH=/bin", "CANYON_IMAGE_EGRESS_HOLDER=mirror.example/holder@sha256:ee", "CANYON_IMAGE_BUSYBOX=x", "HOME=/h"})
	if !slices.Equal(env, []string{"PATH=/bin", "HOME=/h"}) {
		t.Errorf("the environment without CANYON_IMAGE_*: %q", env)
	}
}

// Only Docker's "No such image" says an image the demo pulled is gone: any other error reading it fails down closed,
// never "The demo is gone" while it may remain (#170).
func TestDemoDownPurgeFailsClosedOnAnImageItCannotRead(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	asked := 0
	m.docker = func(line string) (string, error, bool) {
		if line == "image inspect --format {{.Id}} ghcr.io/getcanyon-ai/canyon-server@sha256:aa" {
			if asked++; asked > 1 { // the listing after the removal
				return "", errors.New("exit status 1: Cannot connect to the Docker daemon"), true
			}
		}
		return "", nil, false
	}
	out, errs, code := m.run("down", "--purge")
	if code != exitError || strings.Contains(out, "The demo is gone") || !strings.Contains(errs, "Cannot connect to the Docker daemon") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}

// With no server answering and no list of its apps, down names everything of the demo's it found left, its images
// among them, never only the containers (#170).
func TestDemoDownWithNoServerAndNoListNamesWhatIsLeft(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	must(t, os.Remove(demoAppsKept(filepath.Join(m.home, ".canyon", "demo"))))
	m.answers = false
	m.docker = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "image rm ") {
			return "", errors.New("exit status 1: Error response from daemon: conflict"), true // the images stay
		}
		return "", nil, false
	}
	out, errs, code := m.run("down", "--purge")
	if code != exitError || !strings.Contains(errs, "These may be the demo's:\n    container claims-portal-preview\n    container witty-wren-preview\n") || !strings.Contains(errs, "    image ghcr.io/getcanyon-ai/canyon-server@sha256:aa\n") {
		t.Fatalf("exit %d, stdout %q, stderr\n%s", code, out, errs)
	}
}

// down says done only when Docker holds nothing of the demo's: what it could not remove is named,
// and a listing Docker refused is a failure, never "nothing found".
func TestDemoDownNamesWhatItLeftAndExits1(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.docker = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "rm -f ") {
			return "", errors.New("exit status 1: Error response from daemon: cannot remove container"), true // the containers stay
		}
		return "", nil, false
	}
	out, errs, code := m.run("down")
	if code != exitError || strings.Contains(out, "✓ The demo is stopped") ||
		!strings.Contains(errs, "The demo is stopped, but these are left:\n    container claims-portal-preview\n    container witty-wren-preview") {
		t.Fatalf("exit %d, stdout %q, stderr\n%s", code, out, errs)
	}

	m = demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.docker = func(line string) (string, error, bool) {
		if strings.HasPrefix(line, "network ls ") {
			return "", errors.New("exit status 1: Cannot connect to the Docker daemon"), true
		}
		return "", nil, false
	}
	if out, errs, code := m.run("down"); code != exitError || strings.Contains(out, "✓") || !strings.Contains(errs, "Docker could not list the demo's networks: exit status 1: Cannot connect to the Docker daemon") {
		t.Fatalf("a listing Docker refused: exit %d, stdout %q, stderr\n%s", code, out, errs)
	}

	// A server that does not answer cannot say which apps are its own: with no list up or an earlier down kept
	// (#170), down removes none, and says so.
	m = demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	must(t, os.Remove(demoAppsKept(filepath.Join(m.home, ".canyon", "demo"))))
	m.answers = false
	out, errs, code = m.run("down")
	if code != exitError || m.ran("rm -f") != 0 || !strings.Contains(errs, "The demo's server didn't answer, so no app was removed. These may be the demo's:\n    container claims-portal-preview\n    container witty-wren-preview") {
		t.Fatalf("exit %d, stdout %q, stderr\n%s", code, out, errs)
	}
}

// A server that does not answer, on a demo whose up kept the list of its apps: down removes them all the same, and
// nothing another Canyon made (#170).
func TestDemoDownWithTheServerGoneRemovesTheAppsUpKept(t *testing.T) {
	m := demoOn(t)
	m.another()
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.answers = false
	out, errs, code := m.run("down")
	if code != exitOK || errs != "" || m.ran("rm -f claims-portal-preview witty-wren-preview") != 1 {
		t.Fatalf("exit %d, stdout %q, stderr %q, ran %q", code, out, errs, m.calls)
	}
	if _, ok := m.ctrs["ux-data-preview"]; !ok {
		t.Errorf("down removed another Canyon's container")
	}
}

// A demo started the old way, by docker compose in a folder, is found and named, with where it runs
// and the command that stops it there: down never stops a compose project it did not start.
func TestDemoDownNamesADemoStartedAnotherWay(t *testing.T) {
	m := demoOn(t)
	m.server, m.serverAt = "canyon", "/home/you/canyon"
	out, errs, code := m.run("down")
	if code != exitError || !strings.Contains(errs, "This Canyon wasn't started by canyon demo up: Docker Compose project canyon, in /home/you/canyon.\n  To stop it: cd /home/you/canyon && docker compose --profile demo-apps down -v") {
		t.Fatalf("exit %d, stdout %q, stderr\n%s", code, out, errs)
	}
	if m.ran("compose") != 0 || m.ran("rm") != 0 {
		t.Errorf("down worked on a project it did not start: %q", m.calls)
	}

	// A project whose folder Docker does not name is stopped from its folder, by its project name.
	m = demoOn(t)
	m.server, m.serverAt = "canyon", ""
	if _, errs, code := m.run("down"); code != exitError || !strings.Contains(errs, "This Canyon wasn't started by canyon demo up: Docker Compose project canyon (Docker doesn't say which folder it runs from).\n  To stop it, run in its folder: docker compose -p canyon --profile demo-apps down\n") {
		t.Errorf("exit %d, stderr\n%s", code, errs)
	}
}

func TestDemoRefusesAFormItDoesNotHave(t *testing.T) {
	m := demoOn(t)
	out, errs, code := m.run("start")
	if code != exitError || out != "" || !strings.Contains(errs, `demo has no form "start"`) || !strings.Contains(errs, "canyon demo up") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if out, _, code := m.run(); code != exitOK || !strings.Contains(out, "canyon demo down --purge") {
		t.Errorf("canyon demo alone: exit %d\n%s", code, out)
	}
	if out, errs, code := m.run("--purge"); code != exitError || out != "" || !strings.Contains(errs, "--purge goes with down: canyon demo down --purge") || !strings.Contains(errs, "USAGE") {
		t.Errorf("canyon demo --purge: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if out, errs, code := m.run("--host", "http://x"); code != exitError || out != "" || !strings.Contains(errs, "✗ Put --host after up, tour or down.\n") || !strings.Contains(errs, "USAGE") {
		t.Errorf("canyon demo --host: exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if out, _, code := m.run("--help"); code != exitOK || !strings.Contains(out, "canyon demo up") {
		t.Errorf("canyon demo --help: exit %d\n%s", code, out)
	}
}

// The disk check runs df in the busybox the demo's compose file pins, so it downloads nothing the
// demo would not.
func TestDemoProbeIsTheComposeFilesBusybox(t *testing.T) {
	compose, err := os.ReadFile(filepath.Join("..", "docker-compose.yml"))
	must(t, err)
	if !strings.Contains(string(compose), "CANYON_IMAGE_BUSYBOX:-"+demoProbe+"}") {
		t.Errorf("docker-compose.yml no longer pins %s as its busybox", demoProbe)
	}
}

// Each sample app is a line of progress as the deployer reports it running, and a start that
// outlasts the deadline stops waiting, names the log, and leaves the demo starting.
func TestDemoUpSaysEachSampleAppAndGivesUpWaitingAtTheDeadline(t *testing.T) {
	m := demoOn(t)
	out, _, code := m.run("up")
	if code != exitOK || !strings.Contains(out, "  ✓ claims-portal is running (1 of 4)\n  ✓ endorsement-hub is running (2 of 4)\n  ✓ claims-archive is running (3 of 4)\n  ✓ premium-ledger is running (4 of 4)\n") {
		t.Fatalf("exit %d\n%s", code, out)
	}

	m = demoOn(t)
	m.hangs = true
	was := demoDeadline
	t.Cleanup(func() { demoDeadline = was })
	demoDeadline = 20 * time.Millisecond
	_, errs, code := m.run("up")
	if code != exitError || !strings.Contains(errs, "The sample apps haven't finished deploying after 0 minutes. Their log: cd ~/.canyon/demo/canyon && docker compose -p canyon-demo --profile demo-apps logs demo-apps") ||
		!strings.Contains(errs, "To carry on, run: canyon demo up") || m.ran(compose("down")) != 0 {
		t.Fatalf("exit %d, docker %q\n%s", code, m.calls, errs)
	}
}

// demo/apps/run.sh prints the line up counts for each sample app, in up's order.
func TestTheDeployerSaysEachSampleAppTheWayUpReadsIt(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "demo", "apps", "run.sh"))
	must(t, err)
	for _, want := range []string{"for app in " + strings.Join(demoSampleApps, " ") + "; do", `echo "` + demoReadyPrefix + `$app"`} {
		if !strings.Contains(string(script), want) {
			t.Errorf("demo/apps/run.sh does not say %q", want)
		}
	}
}

// What the demo needs of the machine is not asked again where it runs, or where its images are.
func TestDemoUpAsksNoDiskOrMemoryWhereTheDemoIsAlready(t *testing.T) {
	m := demoOn(t)
	if _, errs, code := m.run("up"); code != exitOK {
		t.Fatalf("up: exit %d, %s", code, errs)
	}
	m.docker = func(line string) (string, error, bool) {
		switch {
		case strings.HasPrefix(line, "info "):
			return "28.1.1 2 2063007744 colima " + overlay2Status, nil, true
		case strings.HasPrefix(line, "run "):
			return "Filesystem 1K-blocks Used Available Use% Mounted on\noverlay 61202244 49667908 1534336 98% /", nil, true
		}
		return "", nil, false
	}
	if out, errs, code := m.run("up"); code != exitOK || strings.Contains(errs, "free disk") {
		t.Fatalf("a running demo: exit %d, stderr %q\n%s", code, errs, out)
	}
	if _, errs, code := m.run("down"); code != exitOK {
		t.Fatalf("down: exit %d, %s", code, errs)
	}
	m.imaged = true
	if _, errs, code := m.run("up"); code != exitOK || strings.Contains(errs, "free disk") || strings.Contains(errs, "CPUs") {
		t.Fatalf("a stopped demo with its images: exit %d\n%s", code, errs)
	}
	m.imaged = false
	if _, errs, _ := m.run("down"); errs != "" {
		t.Fatalf("down: %s", errs)
	}
	if _, errs, code := m.run("up"); code != exitError || !strings.Contains(errs, "free disk") {
		t.Errorf("a stopped demo without its images is asked for its disk: exit %d\n%s", code, errs)
	}
}
