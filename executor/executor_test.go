package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakes returns an Executor whose pack and docker are shell scripts: pack prints two log
// lines and writes NEXT_PUBLIC_API_URL from its environment to <dir>/build-value; docker lists
// the build's image and the contract fixture's, lists every container asked as running,
// records its argv to <dir>/argv and writes STRIPE_SECRET_KEY from its environment to
// <dir>/secret — the children see values only in their environment.
func fakes(t *testing.T) (*Executor, string) {
	t.Helper()
	dir := t.TempDir()
	script := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o700); err != nil { //nolint:gosec // a test fake must be executable
			t.Fatal(err)
		}
		return p
	}
	pack := script("pack", `printf '%s\n' "$@" >> "`+dir+`/pack-argv"; [ -n "${NEXT_PUBLIC_API_URL+x}" ] && printf '%s' "$NEXT_PUBLIC_API_URL" > "`+dir+`/build-value"; printf '%s' "${NODE_ENV-}" > "`+dir+`/node-env"; printf '%s' "${NEXT_PRIVATE_OUTPUT_TRACE_ROOT-}" > "`+dir+`/trace-root"; echo "===> DETECTING"; echo "===> BUILDING"; exit 0`)
	docker := script("docker", `printf '%s\n' "$@" >> "`+dir+`/argv"
`+floorFake+`[ -n "${STRIPE_SECRET_KEY+x}" ] && printf '%s' "$STRIPE_SECRET_KEY" > "`+dir+`/secret"
[ -n "${PORT+x}" ] && printf '%s' "$PORT" > "`+dir+`/port"
[ "$1" = run ] && [ "$2" = -d ] && env > "`+dir+`/run-env"
[ "$1" = create ] && env > "`+dir+`/run-env"
[ "$1" = cp ] && cat > "`+dir+`/cp-tar"
[ "$1" = run ] && [ "$2" = --rm ] && env > "`+dir+`/helper-env"
[ "$1" = build ] && : > "`+dir+`/built-$4"
[ "$1" = tag ] && : > "`+dir+`/built-$3"
[ "$1 $2" = "network ls" ] && exit 0
[ "$1 $2" = "image inspect" ] && case "$5" in canyon-run:*|canyon-builder:*) [ -f "`+dir+`/built-$5" ] || exit 1;; esac
while [ $# -gt 0 ]; do case "$1" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; echo sha256:4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c4f1c0b9a7e2a9d3c canyon/claims-portal:pkg_7d3e8f-fixture; exit 0;; *.Names*) for a; do case "$a" in name=*) n=${a#name=^}; echo "${n%?} running sha256:abababababababababababababababababababababababababababababababab";; esac; done; exit 0;; *State.Status*) echo running 0 sha256:`+strings.Repeat("ab", 32)+`; exit 0;; esac; shift; done
echo sha256:`+strings.Repeat("ab", 32))
	e := &Executor{Pack: pack, Docker: docker, Settle: time.Millisecond}
	t.Cleanup(e.collecting.Wait) // before the directory goes: a /run leaves its image cleanup running docker here
	return e, dir
}

// floorFake answers what a build's floor asks of docker as a daemon that floors it would: canyon-builds as the
// executor creates it, the holder started, and its floor in place; and a builder running as 1001. A docker fake a build runs against carries
// it after recording its argv.
const floorFake = hostAddrsFake + `case "$1 $2 $5" in "network inspect canyon-builds") echo '{"Name":"canyon-builds","Driver":"bridge","EnableIPv6":false,"Internal":false,"Labels":{"canyon.build":""},"Options":{"com.docker.network.bridge.enable_icc":"false"},"IPAM":{"Config":[{"Subnet":"172.18.0.0/16"}]}}'; exit 0;; esac
case "$1 $2 $3 $4" in "run -d --name canyon-build-"*) exit 0;; esac
case "$*" in *"{{.Config.User}}"*) echo "sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd 1001:1001"; exit 0;; esac
[ "$1" = exec ] && exit 0
`

// hostAddrsFake answers the helper that lists the Docker host's own addresses, from the host's namespace, as a
// Linux host with a private and a public address on one interface would, and the network mode of a container set
// aside as a floored app's, its holder's namespace; a docker fake an app's /run on an open
// network runs against carries it after recording its argv.
const hostAddrsFake = `case "$*" in *"--network host "*" ip addr show") printf '1: lo: <LOOPBACK,UP> mtu 65536\n    inet 127.0.0.1/8 scope host lo\n2: eth0: <UP> mtu 1500\n    inet 10.0.0.4/24 brd 10.0.0.255 scope global eth0\n    inet 203.0.113.7/32 scope global eth0\n    inet6 fe80::1/64 scope link\n'; exit 0;; esac
case "$*" in *"{{.HostConfig.NetworkMode}}"*) echo container:f00d; exit 0;; esac
`

// holdersLs is the listing of an app's floor holders a reap begins with: on a fake that lists none, all of it.
func holdersLs(app string) string {
	return "container\nls\n--all\n--filter\nlabel=canyon.holder\n--filter\nlabel=canyon.app=" + app + "\n--format\n{{.Names}}\n"
}

// floored is a docker fake's script with floorFake first.
func floored(script string) string {
	return strings.Replace(script, "#!/bin/sh\n", "#!/bin/sh\n"+floorFake, 1)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(w io.Writer, b []byte) error { _, err := w.Write(b); return err }

func tarOf(t *testing.T, entries map[string]string) []byte {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		must(t, write(tw, []byte(body)))
	}
	must(t, tw.Close())
	return b.Bytes()
}

func TestBuildStreamsTheLogThenReturnsTheDigest(t *testing.T) {
	e, dir := fakes(t)
	var log bytes.Buffer
	value := "https://api.example\nsecond line" // a value may span lines: it travels in pack's environment, never argv
	res, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: map[string]string{"NEXT_PUBLIC_API_URL": value, "VITE_API_URL": ""}}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), &log)
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv"))   //nolint:gosec // test dir
	seen, _ := os.ReadFile(filepath.Join(dir, "build-value")) //nolint:gosec // test dir
	if !strings.Contains(log.String(), "===> BUILDING") || !strings.HasPrefix(res.Image, "canyon/claims-portal@sha256:") {
		t.Fatalf("log %q image %q", log.String(), res.Image)
	}
	if !strings.Contains(string(argv), "--env\nNEXT_PUBLIC_API_URL\n") || strings.Contains(string(argv), "NEXT_PUBLIC_API_URL=") || strings.Contains(string(argv), "second line") || string(seen) != value {
		t.Fatalf("a build value reaches pack through its environment and never argv: argv=%q seen=%q", argv, seen)
	}
}

// Every build runs with NODE_ENV=production, which a build value of that name replaces
// (contracts/executor.md, /build): Paketo's npm-install hands the buildpacks after it
// NODE_ENV=development, and a stock next build fails under it (canyonv2-g6il).
func TestEveryBuildRunsWithNodeEnvProduction(t *testing.T) {
	for want, values := range map[string]map[string]string{"production": nil, "test": {"NODE_ENV": "test"}} {
		e, dir := fakes(t)
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: values}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
		seen, _ := os.ReadFile(filepath.Join(dir, "node-env"))  //nolint:gosec // test dir
		if !strings.Contains(string(argv), "--env\nNODE_ENV\n") || string(seen) != want {
			t.Fatalf("build values %v: pack's NODE_ENV %q, want %q, through its environment (argv %q)", values, seen, want, argv)
		}
	}
}

// Every build runs with NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/, which a build value of that name replaces
// (contracts/executor.md, /build; #1240): Turbopack refuses npm-install's node_modules link, which
// leaves the app's directory, unless its root holds the layer the link points into.
func TestEveryBuildRunsWithTurbopacksRootAtTheFilesystemsRoot(t *testing.T) {
	for want, values := range map[string]map[string]string{"/": nil, "/workspace": {"NEXT_PRIVATE_OUTPUT_TRACE_ROOT": "/workspace"}} {
		e, dir := fakes(t)
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: values}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv"))  //nolint:gosec // test dir
		seen, _ := os.ReadFile(filepath.Join(dir, "trace-root")) //nolint:gosec // test dir
		if !strings.Contains(string(argv), "--env\nNEXT_PRIVATE_OUTPUT_TRACE_ROOT\n") || string(seen) != want {
			t.Fatalf("build values %v: pack's NEXT_PRIVATE_OUTPUT_TRACE_ROOT %q, want %q, through its environment (argv %q)", values, seen, want, argv)
		}
	}
}

// Two builds of one app name the same cache volumes: pack's default names them after the image,
// whose tag is unique per build, so every build left two volumes nothing reused or removed.
func TestBuildsOfOneAppShareOneCache(t *testing.T) {
	e, dir := fakes(t)
	for range 2 {
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	want := "--cache\ntype=build;format=volume;name=canyon-cache-claims-portal-build\n--cache\ntype=launch;format=volume;name=canyon-cache-claims-portal-launch\n"
	if n := strings.Count(string(argv), want); n != 2 {
		t.Fatalf("pack argv %q names the app's cache %d times over two builds; want 2", argv, n)
	}
}

// pack trusts a builder named exactly as on its built-in list (the Paketo builders among them) and
// then runs every phase, app code included, in one container with the Docker socket. The executor
// builds with canyon-builder, a local image on no list, built from builder.Dockerfile under its hash; a
// CANYON_BUILDER is named with its registry, which matches no entry; the lifecycle image is by digest.
func TestBuildNeverTrustsTheBuilder(t *testing.T) {
	tag := "canyon-builder:" + fmt.Sprintf("%x", sha256.Sum256([]byte(builderDockerfile)))[:12]
	for configured, want := range map[string]string{"": tag, "paketobuildpacks/ubuntu-noble-builder": "docker.io/paketobuildpacks/ubuntu-noble-builder"} {
		e, dir := fakes(t)
		e.Builder, builderImage.id = configured, ""
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
			t.Fatal(err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
		for _, want := range []string{"\n--builder\n" + want + "\n", "\n--lifecycle-image\nbuildpacksio/lifecycle@sha256:ffdceee500cbd55cdaed0c05f63271d801d124e4c81cdb2e46ae958e98f4df91\n"} {
			if !strings.Contains(string(argv), want) {
				t.Fatalf("CANYON_BUILDER %q: pack argv %q lacks %q", configured, argv, want)
			}
		}
	}
	if lines := strings.Split(strings.TrimSpace(builderDockerfile), "\n"); lines[4] != "FROM docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:f5dd94c89be567f151b4a3ca1d86267fbc304991f27ee0608dfde70d6bbab65c" ||
		!strings.Contains(lines[6], "apt-get install -y --no-install-recommends libpq5 ") || lines[len(lines)-1] != "USER 1001:1001" {
		t.Fatalf("builder.Dockerfile is not the pinned noble builder plus libpq5, ending as the build user:\n%s", builderDockerfile)
	}
	kpack, err := os.ReadFile("build.Dockerfile") // canyon-build, kpack's build image: the builder's lines on Paketo's build image
	got, want := strings.Split(strings.TrimSpace(string(kpack)), "\n"), strings.Split(strings.TrimSpace(builderDockerfile), "\n")
	if err != nil || len(got) != len(want) || !slices.Equal(got[5:], want[5:]) || got[4] != "FROM docker.io/paketobuildpacks/ubuntu-noble-build@sha256:709c6561b13527882ce6030ef1e6ac1e3d747f3088be1d22dc8cf04e1c16df01" {
		t.Fatalf("build.Dockerfile is not the pinned noble build image plus builder.Dockerfile's libpq5, as the build user: %v\n%s", err, kpack)
	}
}

// Every app runs on canyon-run (contracts/executor.md): the executor builds it from run.Dockerfile, fed
// on stdin under the tag its hash names, before pack runs, names it canyon-run:noble, and hands pack that
// name as the run image: the name the lifecycle records, and a later rebase must match.
// The file is Paketo's noble run image by digest plus libatomic1, libsqlite3-0 and libpq5 at no pinned version (#1754), ending as the
// stack's non-root user.
func TestBuildRunsTheAppOnCanyonRun(t *testing.T) {
	e, dir := fakes(t)
	runImage.id = "" // built by this test's docker, whatever another test left
	if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
		t.Fatal(err)
	}
	tag := "canyon-run:" + fmt.Sprintf("%x", sha256.Sum256([]byte(runDockerfile)))[:12]
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))          //nolint:gosec // test dir
	packArgv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "build\n--quiet\n--tag\n"+tag+"\n-\ntag\nsha256:"+strings.Repeat("ab", 32)+"\ncanyon-run:noble\n") || !strings.Contains(string(packArgv), "\n--run-image\ncanyon-run:noble\n") {
		t.Fatalf("docker argv %q\npack argv %q\nwant canyon-run built as %s, named canyon-run:noble and handed to pack by that name", argv, packArgv, tag)
	}
	lines := strings.Split(strings.TrimSpace(runDockerfile), "\n")
	if distro := strings.TrimPrefix(runTag, "canyon-run:"); !strings.Contains(lines[7], "/ubuntu-"+distro+"-run@") { // the name follows the distribution the file builds on
		t.Fatalf("%s names %s, and run.Dockerfile builds on %s", runTag, distro, lines[7])
	}
	if lines[7] != "FROM docker.io/paketobuildpacks/ubuntu-noble-run@sha256:2d4960f5bbde527fdd2f821102ead22b7bddb1e8b015808db5d33c737a473506" ||
		!strings.Contains(lines[11], "apt-get install -y --no-install-recommends libatomic1 libsqlite3-0 libpq5 ") || lines[len(lines)-1] != "USER 1002:1001" {
		t.Fatalf("run.Dockerfile is not the noble run image plus libatomic1, libsqlite3-0 and libpq5, ending non-root:\n%s", runDockerfile)
	}
}

// pack 0.40.3 only warns when the builder's stack is not the run image's, so a CANYON_BUILDER on another
// stack — the jammy one docker-compose.yml named before canyon-run — fails the build before pack runs.
func TestBuildRefusesABuilderOnAnotherStack(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"case \"$*\" in *stack.id*builder-jammy-base*) echo io.buildpacks.stacks.jammy;; *stack.id*) echo io.buildpacks.stacks.noble;; *) echo sha256:"+strings.Repeat("ab", 32)+";; esac\n"), 0o700)) //nolint:gosec // test fake
	e.Builder = "paketobuildpacks/builder-jammy-base"
	_, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
	if err == nil || !strings.Contains(err.Error(), `CANYON_BUILDER docker.io/paketobuildpacks/builder-jammy-base builds on stack "io.buildpacks.stacks.jammy" and canyon-run is "io.buildpacks.stacks.noble"`) {
		t.Fatalf("a jammy builder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pack-argv")); !os.IsNotExist(err) {
		t.Fatalf("pack ran for a builder on another stack (%v)", err)
	}
}

func TestUntrustedQualifiesOnlyDockerHubNames(t *testing.T) {
	for in, want := range map[string]string{
		"paketobuildpacks/ubuntu-noble-builder":           "docker.io/paketobuildpacks/ubuntu-noble-builder",
		"heroku/builder:24":                               "docker.io/heroku/builder:24",
		"mybuilder":                                       "docker.io/library/mybuilder",
		"docker.io/paketobuildpacks/builder-jammy-base":   "docker.io/paketobuildpacks/builder-jammy-base",
		"gcr.io/buildpacks/builder:google-22":             "gcr.io/buildpacks/builder:google-22",
		"localhost/builder":                               "localhost/builder",
		"registry.local:5000/team/builder@sha256:0123abc": "registry.local:5000/team/builder@sha256:0123abc",
	} {
		if got := untrusted(in); got != want {
			t.Errorf("untrusted(%q) = %q, want %q", in, got, want)
		}
	}
}

// A build value is checked the same way, before any tree is read.
func TestBuildRefusesABadValueName(t *testing.T) {
	e, _ := fakes(t)
	for name, values := range map[string]map[string]string{"DOCKER_CONFIG": {"DOCKER_CONFIG": "/x"}, "= in a name": {"A=B": "x"}, "NUL in a value": {"VITE_A": "a\x00b"}} {
		var log bytes.Buffer
		if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f", BuildValues: values}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), &log); !errors.Is(err, ErrRequest) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

func TestRunRefusesAnythingButThisAppsDigestOnTheAppNetwork(t *testing.T) {
	e, _ := fakes(t)
	good := "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32)
	for name, req := range map[string]RunRequest{
		"tag":            {App: "claims-portal", Target: "preview", Image: "canyon/claims-portal:latest", Network: "canyon-app-claims-portal"},
		"no target":      {App: "claims-portal", Image: good, Network: "canyon-app-claims-portal"},
		"staging target": {App: "claims-portal", Target: "staging", Image: good, Network: "canyon-app-claims-portal"},
		"short digest":   {App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:abc", Network: "canyon-app-claims-portal"},
		"bare digest":    {App: "claims-portal", Target: "preview", Image: "sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"},
		"other app":      {App: "claims-portal", Target: "preview", Image: "canyon/quote-engine@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"},
		"other app's id": {App: "quote-engine", Target: "preview", Image: "canyon/quote-engine@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"},
		"other network":  {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-internal"},
		"shared network": {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-apps"},
		"other app's":    {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-quote-engine"},
		"PORT in env":    {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"PORT": "80"}},
		"identity name":  {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"CANYON_IDENTITY_AUDIENCE": "quote-engine"}},
		"identity other": {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"CANYON_IDENTITY_HEADER": "x"}}, // the prefix is the executor's
		"NUL in a value": {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"DATABASE_URL": "a\x00b"}},
		"= in a name":    {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"A=B": "x"}},
		"DOCKER_HOST":    {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"DOCKER_HOST": "tcp://elsewhere:2375"}}, // the child's own: a later NAME=value wins in exec
		"PATH":           {App: "claims-portal", Target: "preview", Image: good, Network: "canyon-app-claims-portal", Env: map[string]string{"PATH": "/tmp"}},
		"flag as a name": {App: "--rm", Target: "preview", Image: good, Network: "canyon-app-claims-portal"},
	} {
		if _, err := e.Run(context.Background(), req); !errors.Is(err, ErrRequest) { // a refusal is the request's fault (400), never the daemon's (502)
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
}

func TestRunEnvIsHeldForTheCallOnly(t *testing.T) {
	e, dir := fakes(t)
	value := "line one\nline two" // a value may span lines (contracts/executor.md): a PEM key does
	req := RunRequest{App: "claims-portal", Target: "prod", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Env: map[string]string{"STRIPE_SECRET_KEY": value}, Network: "canyon-app-claims-portal"}
	res, err := e.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))   //nolint:gosec // test dir
	seen, _ := os.ReadFile(filepath.Join(dir, "secret")) //nolint:gosec // test dir
	if string(seen) != value || strings.Contains(string(argv), "line one") || strings.Contains(string(argv), "--env-file") {
		t.Fatalf("env must reach docker through its environment and never argv or a file: argv=%q seen=%q", argv, seen)
	}
	if !strings.Contains(string(argv), "-e\nSTRIPE_SECRET_KEY\n") || !strings.Contains(string(argv), "-e\nPORT\n") {
		t.Fatalf("docker is told the names, value-less: argv %q", argv)
	}
	if port, _ := os.ReadFile(filepath.Join(dir, "port")); string(port) != appPort { //nolint:gosec // test dir
		t.Fatalf("PORT reaches docker through its environment as %s: %q", appPort, port)
	}
	reaped := "rm\n-f\nclaims-portal-prod-previous\n" + holdersLs("claims-portal") // then the holders no container names, the one set aside's
	if res.Addr != res.Container+":3000" || res.Container != "claims-portal-prod" || !strings.Contains(string(argv), "run\n-d\n--name\nclaims-portal-prod\n") || !strings.HasSuffix(string(argv), reaped) {
		t.Fatalf("result %+v; the previous container of that target is replaced once the new one runs: argv %q", res, argv)
	}
	for _, limit := range []string{"--memory\n1g", "--pids-limit\n512", "--cap-drop\nALL", "--security-opt\nno-new-privileges", "--ulimit\nnofile=1024:1024"} {
		if !strings.Contains(string(argv), limit) {
			t.Fatalf("untrusted app code runs without %q: argv %q", strings.ReplaceAll(limit, "\n", " "), argv)
		}
	}
}

// The app's identity reaches its container as the three CANYON_IDENTITY_* variables, the way every
// value does: named in argv, the value in docker's own environment.
func TestRunHandsTheContainerItsIdentity(t *testing.T) {
	e, dir := fakes(t)
	id := Identity{Issuer: "http://localhost:7777", Audience: "claims-portal", JWKSURL: "http://canyon-server:7780/.well-known/jwks.json"}
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Env: map[string]string{"DATABASE_URL": "v"}, Network: "canyon-app-claims-portal", Identity: id})
	must(t, err)
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))   //nolint:gosec // test dir
	env, _ := os.ReadFile(filepath.Join(dir, "run-env")) //nolint:gosec // test dir
	for name, value := range map[string]string{"CANYON_IDENTITY_ISSUER": id.Issuer, "CANYON_IDENTITY_AUDIENCE": id.Audience, "CANYON_IDENTITY_JWKS_URL": id.JWKSURL} {
		if !strings.Contains(string(argv), "-e\n"+name+"\n") || !strings.Contains("\n"+string(env), "\n"+name+"="+value+"\n") {
			t.Fatalf("%s=%s reaches the container: argv %q env %q", name, value, argv, env)
		}
	}
}

func TestUntarRefusesEscapesAndLinks(t *testing.T) {
	if err := untar(bytes.NewReader(tarOf(t, map[string]string{"../evil": "x"})), t.TempDir()); err == nil {
		t.Fatal("accepted an entry that escapes the tree")
	}
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	must(t, tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	must(t, tw.Close())
	if err := untar(&b, t.TempDir()); err == nil {
		t.Fatal("accepted a symlink")
	}
}

func TestUntarRefusesSparseEntries(t *testing.T) {
	f, err := os.Open("testdata/sparse.tar") // a 19 KiB pax archive whose one entry expands to 64 MiB
	must(t, err)
	defer func() { _ = f.Close() }()
	if err := untar(f, t.TempDir()); err == nil || !strings.Contains(err.Error(), "sparse") {
		t.Fatalf("expanded a sparse archive: %v", err)
	}
}

func TestEachBuildGetsItsOwnTag(t *testing.T) {
	e, dir := fakes(t)
	for range 2 {
		_, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard)
		must(t, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "pack-argv")) //nolint:gosec // test dir
	tags := map[string]bool{}
	for _, a := range strings.Split(string(argv), "\n") {
		if strings.HasPrefix(a, "canyon/claims-portal:pkg_7d3e8f-") {
			tags[a] = true
		}
	}
	if len(tags) != 2 {
		t.Fatalf("two builds of one package must not share a tag: %v", tags)
	}
}

func TestDockerRunOutputIsWithheldEverywhere(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"case \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; esac\ncase \"$1 $4\" in \"run canyon-appfloor-\"*) exit 0;; esac\n[ \"$1\" = run ] && { echo STRIPE_SECRET_KEY=leak; exit 7; }; exit 0\n"), 0o700)) //nolint:gosec // test fake: the image carries this app's tag, its holder starts, then docker run fails loudly
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	if err == nil || strings.Contains(err.Error(), "leak") || strings.Contains(logged.String(), "leak") || !strings.Contains(logged.String(), "claims-portal-preview") {
		t.Fatalf("docker run's output is withheld everywhere — the call carries the env; only the status is logged: err=%v log=%q", err, logged.String())
	}
}

// A failed start's container that won't go is said, by its name, so the app left down has a reason in the log; the
// env the start carried never is (#2143's review).
func TestARestoreThatCannotRemoveSaysSo(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"case \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; esac\ncase \"$1 $4\" in \"run canyon-appfloor-\"*) exit 0;; esac\n[ \"$1\" = run ] && { echo STRIPE_SECRET_KEY=leak; exit 7; }\n[ \"$1\" = rm ] && { echo \"Error response from daemon: removal of container $3 is already in progress\" >&2; exit 1; }\nexit 0\n"), 0o700)) //nolint:gosec // test fake: docker run fails, then so does rm of what it left
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	if _, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}); err == nil {
		t.Fatal("a docker run that failed answered no error")
	}
	// the fake's run prints the env where docker prints the id, and its rm repeats what it was given: neither is said
	if !strings.Contains(logged.String(), "docker rm -f of claims-portal-preview's failed start failed") || strings.Contains(logged.String(), "leak") {
		t.Fatalf("a restore whose rm -f fails names the container, never the env nor what docker printed: %q", logged.String())
	}
}

// dockerWithNames is a docker that keeps its containers as files under <dir>/c, its restart count of one
// under <dir>/rc (1 for one restarting, else 0, when absent), refusing a rename
// onto a name in use or from a name it lacks, as docker does; run fails while <dir>/fail exists, and
// fails having created the container, printing no id, while <dir>/created does. A container started
// while <dir>/exits exists has exited by the time it is inspected.
func dockerWithNames(t *testing.T, dir string, running ...string) func(name string) string {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(dir, "c"), 0o700))
	for _, name := range running {
		must(t, os.WriteFile(filepath.Join(dir, "c", name), []byte("old"), 0o600))
	}
	script := `#!/bin/sh
` + hostAddrsFake + `c="` + dir + `/c"
st() { s=running; [ -e "` + dir + `/exits" ] && [ "$(cat "$c/$1")" = new ] && s=exited; [ "$(cat "$c/$1")" = restarting ] && s=restarting; echo $s; }
case "$*" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; esac
case "$1 $2" in "container ls") echo ls >> "` + dir + `/inspects"
	case "$*" in *name=*) ;; *) for p in "$c"/*; do [ -e "$p" ] && echo "${p##*/} $(st "${p##*/}") sha256:abababababababababababababababababababababababababababababababab"; done; exit 0;; esac # no filter: every container (containersOf)
	for a; do case "$a" in name=*) n=${a#name=^}; n=${n%?}; [ -e "$c/$n" ] && echo "$n $(st "$n") sha256:abababababababababababababababababababababababababababababababab";; esac; done; exit 0;; esac
case "$1" in
rm) rm -f "$c/$3";;
rename) [ -e "$c/$2" ] && [ ! -e "$c/$3" ] && mv "$c/$2" "$c/$3";;
container) echo inspect >> "` + dir + `/inspects"; f="$4"; shift 4; rc=0; for n in "$@"; do
	[ -e "$c/$n" ] || { echo "Error response from daemon: No such container: $n" >&2; rc=1; continue; }
	s=$(st "$n"); r=0; [ -e "` + dir + `/restarts" ] && [ "$(cat "$c/$n")" = new ] && r=1
	n2=0; [ "$s" = restarting ] && n2=1; [ -e "` + dir + `/rc/$n" ] && n2=$(cat "` + dir + `/rc/$n")
	case "$f" in *Name*RestartCount*) echo "/$n $n2";; *Name*) echo "/$n $s sha256:` + strings.Repeat("ab", 32) + `";; *) echo "$s $r sha256:` + strings.Repeat("ab", 32) + `";; esac; done; exit $rc;;
run) case "$4" in canyon-appfloor-*) exit 0;; esac # an app's floor holder: always started, floored at once (exec)
	[ -e "` + dir + `/created" ] && [ ! -e "$c/$4" ] && echo new > "$c/$4" && exit 125; [ -e "` + dir + `/fail" ] && exit 125; [ -e "$c/$4" ] && exit 125; echo new > "$c/$4"; echo id;;
esac
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700)) //nolint:gosec // test fake
	return func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, "c", name)) //nolint:gosec // test dir
		if err != nil {
			return "none"
		}
		return strings.TrimSpace(string(b))
	}
}

// A redeploy that does not start leaves the running container serving: it is set aside, never
// removed, before the start, and named back when the start fails; removed only after one succeeds.
// A container left aside by a crash between the two is named back before anything else.
func TestARunThatFailsToStartKeepsThePreviousContainer(t *testing.T) {
	req := RunRequest{App: "claims-portal", Target: "prod", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}
	for _, tc := range []struct {
		name        string
		running     []string
		fail        string
		prod, aside string
	}{
		{"a failed start", []string{"claims-portal-prod"}, "fail", "old", "none"},
		{"a failed start after a crash left prod aside", []string{"claims-portal-prod-previous"}, "fail", "old", "none"},
		{"a failed start that created the container and printed no id", []string{"claims-portal-prod"}, "created", "old", "none"},
		{"a start whose app exits at once", []string{"claims-portal-prod"}, "exits", "old", "none"},
		{"a start whose app restarted inside the settle window", []string{"claims-portal-prod"}, "restarts", "old", "none"},
		{"a start", []string{"claims-portal-prod"}, "", "new", "none"},
		{"a start after a crash left both", []string{"claims-portal-prod", "claims-portal-prod-previous"}, "", "new", "none"},
	} {
		e, dir := fakes(t)
		state := dockerWithNames(t, dir, tc.running...)
		if tc.fail != "" {
			must(t, os.WriteFile(filepath.Join(dir, tc.fail), nil, 0o600))
		}
		if _, err := e.Run(context.Background(), req); (err != nil) != (tc.fail != "") {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if prod, aside := state("claims-portal-prod"), state("claims-portal-prod-previous"); prod != tc.prod || aside != tc.aside {
			t.Fatalf("%s: prod runs %s, %s aside; want %s, %s", tc.name, prod, aside, tc.prod, tc.aside)
		}
	}
}

// An app that exits once up is started again (contracts/executor.md, /run; #1241), and up is its
// first start staying up: one docker started again inside the settle window is a failed start.
func TestRunRestartsAnAppButNotInsideTheSettleWindow(t *testing.T) {
	req := RunRequest{App: "claims-portal", Target: "prod", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"}
	e, dir := fakes(t)
	must(t, func() error { _, err := e.Run(context.Background(), req); return err }())
	if argv, _ := os.ReadFile(filepath.Join(dir, "argv")); !strings.Contains(string(argv), "--restart\non-failure:10\n") { //nolint:gosec // test dir
		t.Fatalf("docker run without the restart policy: %q", argv)
	}
	dockerWithNames(t, dir)
	must(t, os.WriteFile(filepath.Join(dir, "restarts"), nil, 0o600))
	var exited *Exited
	if _, err := e.Run(context.Background(), req); !errors.As(err, &exited) || exited.Reason != "the app did not stay up: its container restarted" {
		t.Fatalf("a start docker restarted inside the window: %v", err)
	}
}

// POST /running answers those asked whose container runs, each once, in the order asked, from one
// docker call however many are asked: one that exited or is between two restarts is not running, nor
// is a name the daemon has not (docker inspect exits 1 for it and still answers the others, as the
// real daemon does), and a daemon that does not answer is a 502, never an answer naming fewer (#1241).
func TestRunningOfAsksTheDaemonOnceForAll(t *testing.T) {
	e, dir := fakes(t)
	dockerWithNames(t, dir, "claims-portal-prod", "claims-archive-prod", "claims-portal-preview", "fnol-intake-prod")
	must(t, os.WriteFile(filepath.Join(dir, "c", "claims-archive-prod"), []byte("new"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "c", "fnol-intake-prod"), []byte("restarting"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "exits"), nil, 0o600))
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	asked := []Container{{"claims-portal", "prod"}, {"claims-archive", "prod"}, {"fnol-intake", "preview"}, {"fnol-intake", "prod"}, {"claims-portal", "preview"}, {"claims-portal", "prod"}}
	got, err := c.RunningOf(context.Background(), asked, true)
	if want := []Container{{"claims-portal", "prod"}, {"claims-portal", "preview"}}; err != nil || !slices.Equal(got.Running, want) {
		t.Fatalf("running of %v: %v %v, want %v", asked, got, err, want)
	}
	if calls, _ := os.ReadFile(filepath.Join(dir, "inspects")); strings.Count(string(calls), "ls\n") != 1 { //nolint:gosec // test dir
		t.Fatalf("docker ran %q; one container ls answers every one asked", calls)
	}
	for name, body := range map[string]string{
		"none asked":       `{"containers":[]}`,
		"401 asked":        `{"containers":[` + strings.Repeat(`{"app":"a","target":"prod"},`, 400) + `{"app":"a","target":"prod"}]}`,
		"a name refused":   `{"containers":[{"app":"Bad","target":"prod"}]}`,
		"a target refused": `{"containers":[{"app":"claims-portal","target":"staging"}]}`,
	} {
		if resp, err := srv.Client().Post(srv.URL+"/running", "application/json", strings.NewReader(body)); err != nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %v %v, want 400", name, resp, err)
		}
	}
	if resp, err := srv.Client().Post(srv.URL+"/running", "application/json", strings.NewReader(`{"containers":[`+strings.Repeat(`{"app":"a","target":"prod"},`, 399)+`{"app":"a","target":"prod"}]}`)); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("400 asked, the most one read asks: %v %v, want 200", resp, err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"echo 'failed to connect to the docker API at unix:///var/run/docker.sock' >&2\nexit 1\n"), 0o700)) //nolint:gosec // test fake
	if got, err := c.RunningOf(context.Background(), asked, true); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a daemon that does not answer: %v %v, want the 502", got, err)
	}
}

// The daemon lists every container once, with no name filter — each filter is a pattern it tries against every
// container, 15 s for POST /running's 400 over 4,000 (#2752) — and RunningOf keeps those it was asked about: one
// listed but not asked is no answer, nor inspected, and one asked but not listed is not running, whatever labels
// either carries.
func TestRunningOfListsOnceAndKeepsWhatWasAsked(t *testing.T) {
	e, dir := fakes(t)
	ab := " sha256:" + strings.Repeat("ab", 32)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + dir + `/calls"
case "$1 $2" in
"container ls") printf '%s\n' "claims-portal-preview running` + ab + `" "other-app-prod running` + ab + `" "claims-portal-prod-previous running` + ab + `" "canyon-build-1f2e,alias running` + ab + `";;
"container inspect") echo "/claims-portal-preview 0";;
esac
exit 0
`
	must(t, os.WriteFile(e.Docker, []byte(script), 0o700)) //nolint:gosec // test fake
	asked := []Container{{"claims-portal", "preview"}, {"claims-portal", "prod"}, {"fnol-intake", "prod"}}
	got, err := e.RunningOf(context.Background(), asked, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("a container listed but not asked is no answer, nor inspected", func(t *testing.T) {
		if !slices.Equal(got.Running, asked[:1]) {
			t.Errorf("running %v; want claims-portal preview alone, never other-app, a previous container or a build", got.Running)
		}
		// every running container on the host in one inspect is slow, and past ARG_MAX at thousands
		calls, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test dir
		for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
			if strings.HasPrefix(call, "container inspect") && !strings.HasSuffix(call, "{{.Name}} {{.RestartCount}} claims-portal-preview") {
				t.Errorf("docker %s; want the asked container's restarts read alone", call)
			}
		}
	})
	t.Run("one asked but not listed is not running", func(t *testing.T) {
		if slices.Contains(got.Running, asked[1]) || slices.Contains(got.Running, asked[2]) {
			t.Errorf("running %v; claims-portal prod and fnol-intake prod are on no list", got.Running)
		}
	})
	t.Run("the list sends no name filter", func(t *testing.T) {
		calls, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test dir
		for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
			if strings.HasPrefix(call, "container ls") && strings.Contains(call, "--filter") {
				t.Errorf("docker %s; want every container listed once, with no filter", call)
			}
		}
	})
}

// POST /running says why of each asked that has exited, in the order asked: its exit code from one
// docker container inspect for them all, its last line from one docker logs --tail 1 each, raw — the
// server masks it. A running read asks nothing more; a line or code that cannot be read leaves that
// one out and keeps the answer (#1597).
func TestRunningOfSaysWhyAnExitedContainerStopped(t *testing.T) {
	e, dir := fakes(t)
	ab := " sha256:" + strings.Repeat("ab", 32)
	script := `#!/bin/sh
echo "$1 $2" >> "` + dir + `/calls"
case "$1 $2" in
"container ls") printf '%s\n' "claims-portal-preview running` + ab + `" "claims-portal-prod exited` + ab + `" "fnol-intake-prod dead` + ab + `";;
"container inspect") case "$4" in *RestartCount*) echo "/claims-portal-preview 0"; exit 0;; esac
	[ -e "` + dir + `/noinspect" ] && exit 1; echo "/claims-portal-prod exited 1"; echo "/fnol-intake-prod dead 137";;
"logs --tail") [ "$4" = fnol-intake-prod ] && [ -e "` + dir + `/nologs" ] && exit 1
	[ "$4" = claims-portal-prod ] && printf 'starting\nError: connect failed (pw-7f3a)\n' >&2; [ "$4" = fnol-intake-prod ] && printf 'killed\n';;
esac
exit 0
`
	must(t, os.WriteFile(e.Docker, []byte(script), 0o700)) //nolint:gosec // test fake
	calls := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test dir
		_ = os.Remove(filepath.Join(dir, "calls"))
		return string(b)
	}
	asked := []Container{{"fnol-intake", "prod"}, {"claims-portal", "preview"}, {"claims-portal", "prod"}, {"fnol-intake", "prod"}}
	got, err := e.RunningOf(context.Background(), asked, true)
	want := []Stopped{{"fnol-intake", "prod", 137, "killed"}, {"claims-portal", "prod", 1, "Error: connect failed (pw-7f3a)"}}
	if err != nil || !slices.Equal(got.Running, asked[1:2]) || !slices.Equal(got.Stopped, want) {
		t.Fatalf("running of %v: %v %v, want %v", asked, got, err, want)
	}
	if c := calls(); c != "container ls\ncontainer inspect\nlogs --tail\nlogs --tail\ncontainer inspect\n" {
		t.Fatalf("docker ran %q; one inspect for the running's restarts, one logs each, then one inspect for the exited", c)
	}
	if got, err := e.RunningOf(context.Background(), asked[1:2], true); err != nil || len(got.Stopped) != 0 || calls() != "container ls\ncontainer inspect\n" {
		t.Fatalf("a running read: %v %v; it asks one container ls and one inspect of the running and nothing more", got, err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "nologs"), nil, 0o600))
	if got, err := e.RunningOf(context.Background(), asked, true); err != nil || !slices.Equal(got.Running, asked[1:2]) || !slices.Equal(got.Stopped, want[1:]) {
		t.Fatalf("a line that cannot be read: %v %v, want %v alone", got, err, want[1:])
	}
	must(t, os.WriteFile(filepath.Join(dir, "noinspect"), nil, 0o600))
	if got, err := e.RunningOf(context.Background(), asked, true); err != nil || !slices.Equal(got.Running, asked[1:2]) || len(got.Stopped) != 0 {
		t.Fatalf("exit codes that cannot be read: %v %v, want the running answer and no why", got, err)
	}
}

// Asked why, a container docker lists as restarting — down, waiting out its delay — says how it last
// exited, as an exited one does: that exit's code and last line. One up again since a restart says
// nothing: its last line is the new start's and its exit code is gone (#1597).
func TestRunningOfSaysHowARestartingContainerLastExited(t *testing.T) {
	e, dir := fakes(t)
	ab := " sha256:" + strings.Repeat("ab", 32)
	script := `#!/bin/sh
echo "$1 $2" >> "` + dir + `/calls"
case "$1 $2" in
"container ls") printf '%s\n' "case-dies-later-preview restarting` + ab + `" "fnol-intake-prod running` + ab + `";;
"container inspect") case "$4" in *RestartCount*) echo "/case-dies-later-preview 7"; echo "/fnol-intake-prod 2"; exit 0;; esac
	[ -e "` + dir + `/upagain" ] && { echo "/case-dies-later-preview running 0"; exit 0; }; echo "/case-dies-later-preview restarting 1";;
"logs --tail") [ "$4" = case-dies-later-preview ] && printf 'listening on 3000\nlost the connection to the queue: exiting\n' >&2;;
esac
exit 0
`
	must(t, os.WriteFile(e.Docker, []byte(script), 0o700)) //nolint:gosec // test fake
	asked := []Container{{"case-dies-later", "preview"}, {"fnol-intake", "prod"}}
	got, err := e.RunningOf(context.Background(), asked, true)
	restarting, said := []Restarting{{"case-dies-later", "preview", 7}, {"fnol-intake", "prod", 2}}, []Stopped{{"case-dies-later", "preview", 1, "lost the connection to the queue: exiting"}}
	if err != nil || len(got.Running) != 0 || !slices.Equal(got.Restarting, restarting) || !slices.Equal(got.Stopped, said) {
		t.Fatalf("running of %v: %+v %v, want %v restarting and %v", asked, got, err, restarting, said)
	}
	if got, err := e.RunningOf(context.Background(), asked, false); err != nil || len(got.Stopped) != 0 {
		t.Fatalf("not asked why: %+v %v, want no why", got, err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "upagain"), nil, 0o600)) // started again between the list and the inspect
	if got, err := e.RunningOf(context.Background(), asked, true); err != nil || !slices.Equal(got.Restarting, restarting) || len(got.Stopped) != 0 {
		t.Fatalf("up again by the inspect: %+v %v, want %v restarting and no why: its line is the new start's", got, err, restarting)
	}
}

// An app docker has started again since /run is restarting, never running, whether it is up at that
// instant or waiting out docker's delay; one that has not restarted runs; one past its restarts has
// exited and is stopped. The counts come from one docker container inspect of the running ones, and a
// daemon that answers no count is a 502, never a running answer (contracts/executor.md, Restarting; #1722).
func TestRunningOfAnswersARestartedAppRestartingNeverRunning(t *testing.T) {
	e, dir := fakes(t)
	dockerWithNames(t, dir, "case-dies-later-preview", "claims-portal-preview", "fnol-intake-prod", "claims-archive-prod")
	must(t, os.MkdirAll(filepath.Join(dir, "rc"), 0o700))
	must(t, os.WriteFile(filepath.Join(dir, "rc", "case-dies-later-preview"), []byte("7"), 0o600))  // up, in one of its windows
	must(t, os.WriteFile(filepath.Join(dir, "c", "fnol-intake-prod"), []byte("restarting"), 0o600)) // waiting out the delay
	must(t, os.WriteFile(filepath.Join(dir, "rc", "fnol-intake-prod"), []byte("3"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "c", "claims-archive-prod"), []byte("new"), 0o600)) // past its ten
	must(t, os.WriteFile(filepath.Join(dir, "exits"), nil, 0o600))
	asked := []Container{{"case-dies-later", "preview"}, {"claims-portal", "preview"}, {"fnol-intake", "prod"}, {"claims-archive", "prod"}}
	got, err := e.RunningOf(context.Background(), asked, false)
	want := []Restarting{{"case-dies-later", "preview", 7}, {"fnol-intake", "prod", 3}}
	if err != nil || !slices.Equal(got.Running, asked[1:2]) || !slices.Equal(got.Restarting, want) || len(got.Stopped) != 0 {
		t.Fatalf("running of %v: %+v %v; want %v running and %v restarting", asked, got, err, asked[1:2], want)
	}
	must(t, os.WriteFile(filepath.Join(dir, "rc", "claims-portal-preview"), []byte("many"), 0o600))
	if got, err := e.RunningOf(context.Background(), asked, false); err == nil {
		t.Fatalf("a count docker did not give: %+v, want an error, never a running answer", got)
	}
}

// LastLine is the last non-empty line, valid UTF-8; one longer than SaidBytes is dropped, never cut,
// so no part of a value that crosses the bound reaches the server's mask, which could not name it.
func TestLastLine(t *testing.T) {
	crossing := strings.Repeat("a", SaidBytes-6) + " the line crosses the bound here " + strings.Repeat("b", 1<<10) // 5 KiB, its text across byte 4096
	for in, want := range map[string]string{"": "", "a\nb\n\n": "b", "a\r\nb\r\n": "b", crossing + "\n": "", "x\n" + strings.Repeat("é", SaidBytes/2): strings.Repeat("é", SaidBytes/2)} {
		if got := LastLine(in); got != want {
			t.Errorf("LastLine(%.20q) = %.20q, want %.20q", in, got, want)
		}
	}
}

// Not asked why — the fleet, and every read but the status read — POST /running asks the daemon one
// container ls and nothing more, however many asked have exited (#1597).
func TestRunningOfNotAskedWhyAsksNothingMore(t *testing.T) {
	e, dir := fakes(t)
	dockerWithNames(t, dir, "claims-archive-prod")
	must(t, os.WriteFile(filepath.Join(dir, "c", "claims-archive-prod"), []byte("new"), 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "exits"), nil, 0o600))
	got, err := e.RunningOf(context.Background(), []Container{{"claims-archive", "prod"}}, false)
	if calls, _ := os.ReadFile(filepath.Join(dir, "inspects")); err != nil || len(got.Running)+len(got.Stopped) != 0 || string(calls) != "ls\n" { //nolint:gosec // test dir
		t.Fatalf("an exited container, not asked why: %v %v, docker ran %q; want one container ls", got, err, calls)
	}
}

// /run's mask names each line of a CRLF value without its carriage return too: an app logs a line of
// it without the \r it was handed with.
func TestMaskNamesEachLineOfACRLFValue(t *testing.T) {
	if got := Mask("read line-one-91c2 and line-two-7f3a", map[string]string{"CERT": "line-one-91c2\r\nline-two-7f3a"}); got != "read <CERT> and <CERT>" {
		t.Fatalf("Mask = %q", got)
	}
}

// GET /running names the image the app's container runs, and only while it runs: nothing there,
// or a container that has exited, is a 404 the client reads as ErrNoContainer.
func TestRunningAnswersTheImageOfARunningContainerOnly(t *testing.T) {
	e, dir := fakes(t)
	dockerWithNames(t, dir, "claims-portal-prod")
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	c := &Client{Base: srv.URL}
	if image, err := c.Running(context.Background(), "claims-portal", "prod"); err != nil || image != "canyon/claims-portal@sha256:"+strings.Repeat("ab", 32) {
		t.Fatalf("a running prod: %q %v", image, err)
	}
	if _, err := c.Running(context.Background(), "claims-portal", "preview"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("no preview container: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "exits"), nil, 0o600))
	must(t, os.WriteFile(filepath.Join(dir, "c", "claims-portal-prod"), []byte("new"), 0o600))
	if _, err := c.Running(context.Background(), "claims-portal", "prod"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("an exited prod: %v", err)
	}
	if resp, err := srv.Client().Get(srv.URL + "/running?app=Bad&target=prod"); err != nil || resp.StatusCode != 400 {
		t.Fatalf("a name the pattern refuses: %v %v", resp, err)
	}
}

// A daemon that does not answer is a 502, never the 404 of a container that is not there: the
// promote run's retry reads a 404 as nothing running and would start the image a second time.
func TestRunningIs502WhenTheDaemonDoesNotAnswer(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"echo 'failed to connect to the docker API at unix:///var/run/docker.sock' >&2\nexit 1\n"), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/running?app=claims-portal&target=prod")
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

// runningFails is a Calls whose Running cannot tell, as a cluster's API that does not answer.
type runningFails struct{ Calls }

func (runningFails) Running(context.Context, string, string) (string, error) {
	return "", errors.New("the cluster did not answer")
}

// A Running that cannot tell is a 502, never a 200 naming no image: the promote run's retry would
// read that as nothing running and start the image a second time.
func TestHTTPRunningThatCannotTellIs502(t *testing.T) {
	srv := httptest.NewServer(Handler(runningFails{}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/running?app=claims-portal&target=prod")
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", resp.StatusCode)
	}
}

func TestHTTPBuildIsLogLinesThenResult(t *testing.T) {
	e, dir := fakes(t)
	// pack emits one 40 KiB line in two writes (crossing exec's 32 KiB chunk) and a normal line: three objects, in order, whole.
	must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\n"+hostAddrsFake+"head -c 30000 /dev/zero | tr '\\0' x; head -c 10000 /dev/zero | tr '\\0' x; echo; echo '===> BUILDING'\n"), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(`{"app":"claims-portal","package":"pkg_7d3e8f","build_values":{"NEXT_PUBLIC_API_URL":""}}`)))
	f, _ := mw.CreateFormFile("tree", "tree.tar")
	must(t, write(f, tarOf(t, map[string]string{"package.json": "{}"})))
	must(t, mw.Close())
	resp, err := srv.Client().Post(srv.URL+"/build", mw.FormDataContentType(), &body)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || lines[0] != `{"line":"`+strings.Repeat("x", 40000)+`"}` || lines[1] != `{"line":"===> BUILDING"}` || !strings.Contains(lines[2], `"image":"canyon/claims-portal@sha256:`) {
		t.Fatalf("got %d lines; first %d bytes; last %q", len(lines), len(lines[0]), lines[len(lines)-1])
	}
}

func TestHTTPRunMapsRefusalsTo400AndDaemonFailuresTo502(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	good := `{"app":"claims-portal","target":"preview","image":"canyon/claims-portal@sha256:` + strings.Repeat("ab", 32) + `","env":{},"network":"canyon-app-claims-portal"}`
	failing := "#!/bin/sh\ncase \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; esac\n[ \"$1\" = run ] && exit 125; exit 0\n"
	for _, tc := range []struct {
		name, body, docker string
		want               int
	}{
		{"ok", good, "", 200},
		{"refusal", strings.Replace(good, `"target":"preview"`, `"target":"staging"`, 1), "", 400},
		{"daemon failure", good, failing, 502}, // last: it replaces the fake docker
	} {
		if tc.docker != "" {
			// Written beside and renamed over: the previous case's background calls may still be running
			// the old fake, and overwriting a running executable fails with "text file busy".
			must(t, os.WriteFile(filepath.Join(dir, "docker.next"), []byte(tc.docker), 0o700)) //nolint:gosec // test fake
			must(t, os.Rename(filepath.Join(dir, "docker.next"), filepath.Join(dir, "docker")))
		}
		resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(tc.body))
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// contracts/executor.md, POST /run: access, front_door and database_ca are Kubernetes only, and "the
// Docker executor refuses each with 400" — never a /run that drops them and starts the app anyway.
func TestHTTPRunRefusesAccessAndFrontDoor(t *testing.T) {
	raw, err := os.ReadFile("../contracts/testdata/executor/claims-portal.json")
	must(t, err)
	var fixture struct {
		RunRequest     map[string]json.RawMessage `json:"run_request"`
		KubeRunRequest map[string]json.RawMessage `json:"kube_run_request"`
	}
	must(t, json.Unmarshal(raw, &fixture))
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	post := func(req map[string]json.RawMessage) int {
		body, err := json.Marshal(req)
		must(t, err)
		resp, err := srv.Client().Post(srv.URL+"/run", "application/json", bytes.NewReader(body))
		must(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	access, frontDoor, null := fixture.KubeRunRequest["access"], fixture.KubeRunRequest["front_door"], json.RawMessage("null")
	for i, fields := range []map[string]json.RawMessage{
		{"access": access},
		{"front_door": frontDoor},
		{"access": access, "front_door": frontDoor},
		{"access": null, "front_door": null}, // named is refused, whatever the value
		{"database_ca": fixture.KubeRunRequest["database_ca"]},
		{"database_ca": json.RawMessage(`""`)}, // named is refused, as above
	} {
		req := maps.Clone(fixture.RunRequest)
		maps.Copy(req, fields)
		if code := post(req); code != http.StatusBadRequest {
			t.Errorf("case %d: status %d, want 400", i, code)
		}
	}
	if argv := readIf(dir, "argv"); argv != "" {
		t.Fatalf("docker ran for a /run it refuses: %q", argv)
	}
	if code := post(fixture.RunRequest); code != http.StatusOK { // the same request without them runs: the refusal is theirs
		t.Fatalf("the fixture's run_request: status %d, want 200", code)
	}
}

func TestHTTPBodiesAreBoundedAndWhole(t *testing.T) {
	e, dir := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	good := `{"app":"claims-portal","target":"preview","image":"canyon/claims-portal@sha256:` + strings.Repeat("ab", 32) + `","env":{},"network":"canyon-app-claims-portal"}`
	for name, body := range map[string]string{"trailing data": good + ` {"x":1}`, "over 1 MiB": `{"app":"` + strings.Repeat("a", 1<<20) + `"}`} {
		resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(body))
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	// the daemon being unreachable is 502, not a refusal
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"exit 1\n"), 0o700)) //nolint:gosec // test fake: docker cannot run
	resp, err := srv.Client().Post(srv.URL+"/run", "application/json", strings.NewReader(good))
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("daemon down: status %d, want 502", resp.StatusCode)
	}
}

func TestLineWriterEdges(t *testing.T) {
	rec := httptest.NewRecorder()
	l := &lineWriter{w: rec}
	_, _ = l.Write([]byte("a\r\n" + strings.Repeat("x", maxLine)))
	_, _ = l.Write([]byte("\nb"))
	l.flush()
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 3 || lines[0] != `{"line":"a"}` || lines[1] != `{"line":"`+strings.Repeat("x", maxLine)+`"}` || lines[2] != `{"line":"b"}` {
		t.Fatalf("CRLF must be one clean line and a line of exactly maxLine must not be followed by an empty one; got %d lines: %q…", len(lines), lines[0][:20])
	}
}

func TestHTTPBuildSurvivesACappedLogLineAndAnUnterminatedLastLine(t *testing.T) {
	e, dir := fakes(t)
	// a line of exactly maxLine bytes, then a last line with no newline: the build must still report its result, after both lines
	must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\n"+hostAddrsFake+"head -c 65536 /dev/zero | tr '\\0' y; echo; printf 'last line, no newline'\n"), 0o700)) //nolint:gosec // test fake
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(`{"app":"claims-portal","package":"pkg_7d3e8f","build_values":{}}`)))
	f, _ := mw.CreateFormFile("tree", "tree.tar")
	must(t, write(f, tarOf(t, map[string]string{"package.json": "{}"})))
	must(t, mw.Close())
	resp, err := srv.Client().Post(srv.URL+"/build", mw.FormDataContentType(), &body)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || lines[0] != `{"line":"`+strings.Repeat("y", maxLine)+`"}` || lines[1] != `{"line":"last line, no newline"}` || !strings.Contains(lines[2], `"image":`) {
		t.Fatalf("a capped line must not fail the build and the unterminated last line must precede the result; got %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

func TestHTTPBuildBodyIsCappedOnTheWire(t *testing.T) {
	e, _ := fakes(t)
	old := maxBody
	maxBody = 1 << 20
	defer func() { maxBody = old }()
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(`{"app":"claims-portal","package":"pkg_7d3e8f","build_values":{}}`)))
	f, _ := mw.CreateFormFile("tree", "tree.tar")
	must(t, write(f, tarOf(t, map[string]string{"big": strings.Repeat("x", 2<<20)})))
	must(t, mw.Close())
	resp, err := srv.Client().Post(srv.URL+"/build", mw.FormDataContentType(), &body)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	if !strings.Contains(out.String(), "too large") {
		t.Fatalf("a body over the cap must be refused on the wire: %q", out.String())
	}
}

func TestRunRemovesOnlyTheContainerItStarted(t *testing.T) {
	e, dir := fakes(t)
	// docker run prints a container id and then fails: cleanup must target that id, never the name another call may own
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"printf '%s\\n' \"$@\" >> \""+dir+"/argv\"\ncase \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; esac\ncase \"$1 $4\" in \"run canyon-appfloor-\"*) exit 0;; esac\n[ \"$1\" = run ] && { echo deadbeefcafe; exit 125; }; exit 0\n"), 0o700)) //nolint:gosec // test fake: the holder starts, the app does not
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	if err == nil {
		t.Fatal("the failed run must be reported")
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if !strings.Contains(string(argv), "rm\n-f\ndeadbeefcafe") || strings.Count(string(argv), "rm\n-f\nclaims-portal-preview") != 1 {
		t.Fatalf("cleanup after a failed run must remove the started container by id, not by name: %q", argv)
	}
}

func TestHTTPRefusalsBeforeAnyLogByteAre400AndUnknownCallsAreJSON404(t *testing.T) {
	e, _ := fakes(t)
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	j, _ := mw.CreateFormField("json")
	must(t, write(j, []byte(`{"app":"--rm","package":"pkg_7d3e8f","build_values":{}}`)))
	f, _ := mw.CreateFormFile("tree", "tree.tar")
	must(t, write(f, tarOf(t, map[string]string{"package.json": "{}"})))
	must(t, mw.Close())
	resp, err := srv.Client().Post(srv.URL+"/build", mw.FormDataContentType(), &body)
	must(t, err)
	_ = resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("a flag-shaped app name must be refused before any log byte: status %d", resp.StatusCode)
	}
	for name, json := range map[string]string{
		"uppercase app":     `{"app":"Claims","package":"pkg_7d3e8f","build_values":{}}`,
		"short package":     `{"app":"claims","package":"pkg_e2e","build_values":{}}`,
		"uppercase package": `{"app":"claims","package":"pkg_7D3E8F","build_values":{}}`,
	} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		j, _ := mw.CreateFormField("json")
		must(t, write(j, []byte(json)))
		f, _ := mw.CreateFormFile("tree", "tree.tar")
		must(t, write(f, tarOf(t, map[string]string{"package.json": "{}"})))
		must(t, mw.Close())
		resp, err := srv.Client().Post(srv.URL+"/build", mw.FormDataContentType(), &body)
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: the contract's name patterns (executor.schema.json) are the executor's too: status %d", name, resp.StatusCode)
		}
	}
	resp, err = srv.Client().Get(srv.URL + "/nope")
	must(t, err)
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 404 || !strings.HasPrefix(out.String(), `{"error":`) {
		t.Errorf("unknown calls answer the contract's error shape: %d %q", resp.StatusCode, out.String())
	}
}

func TestChildrenNeverSeeTheExecutorsWholeEnvironment(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "pack"), []byte("#!/bin/sh\n"+hostAddrsFake+"env\n"), 0o700)) //nolint:gosec // test fake: prints its environment into the build log
	t.Setenv("CANYON_VAULT_KEY", "would-leak-into-a-build-log")
	var log bytes.Buffer
	_, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), &log)
	must(t, err)
	if strings.Contains(log.String(), "would-leak") || !strings.Contains(log.String(), "PATH=") {
		t.Fatalf("pack must get an allowlisted environment, never the executor's: %q", log.String()[:min(200, len(log.String()))])
	}
}

// The docker child gets the same allowlisted environment pack does: the call's values and
// what docker needs, never the executor's whole environment.
func TestTheDockerChildNeverSeesTheExecutorsWholeEnvironment(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"case \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; *State.Status*) echo running 0 sha256:abab; exit 0;; esac\n[ \"$1\" = run ] && [ \"$2\" = -d ] && env > \""+dir+"/docker-env\"; exit 0\n"), 0o700)) //nolint:gosec // test fake: writes its environment on run
	t.Setenv("CANYON_VAULT_KEY", "would-leak-into-a-container")
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Env: map[string]string{"DATABASE_URL": "v"}, Network: "canyon-app-claims-portal"})
	must(t, err)
	seen, _ := os.ReadFile(filepath.Join(dir, "docker-env")) //nolint:gosec // test dir
	if strings.Contains(string(seen), "would-leak") || !strings.Contains(string(seen), "PATH=") || !strings.Contains(string(seen), "DATABASE_URL=v") {
		t.Fatalf("docker must get an allowlisted environment plus the call's values, never the executor's: %q", seen)
	}
}

func TestFlushStripsACarriageReturn(t *testing.T) {
	rec := httptest.NewRecorder()
	l := &lineWriter{w: rec}
	_, _ = l.Write([]byte("last\r"))
	l.flush()
	if strings.TrimSpace(rec.Body.String()) != `{"line":"last"}` {
		t.Fatalf("got %q", rec.Body.String())
	}
}

// An app that exits inside the settle window is answered with its own last lines, read before its
// container goes, and every value /run handed it in them replaced by its name — the value spanning
// lines too — through /run's 502 and back out of the client (contracts/executor.md, /run).
func TestRunAnAppThatExitsAnswersItsLastLinesButNoValue(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"printf '%s\\n' \"$@\" >> \""+dir+"/argv\"\ncase \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; *State.Status*) echo 'exited 0 canyon/claims-portal'; exit 0;; esac\n[ \"$1\" = logs ] && { echo 'connecting to value-db-7f3a-long'; echo 'value-key-two' >&2; echo 'node: libatomic.so.1: cannot open shared object file' >&2; exit 0; }\n[ \"$1\" = run ] && echo cafe\nexit 0\n"), 0o700)) //nolint:gosec // test fake
	req := RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal",
		Env: map[string]string{"DATABASE_URL": "value-db-7f3a-long", "DB_PASSWORD": "7f3a", "SIGNING_KEY": "value-key-one\nvalue-key-two", "EMPTY": ""}}
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	_, err := (&Client{Base: srv.URL}).Run(context.Background(), req)
	var exited *Exited
	if !errors.As(err, &exited) {
		t.Fatalf("an app that exited is answered with its lines: %v", err)
	}
	want := "connecting to <DATABASE_URL>\n<SIGNING_KEY>\nnode: libatomic.so.1: cannot open shared object file\n"
	if exited.Reason != "the app did not stay up: its container is exited" || exited.Output != want {
		t.Fatalf("reason %q, output %q; want the app's lines with its values named:\n%q", exited.Reason, exited.Output, want)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if logs, rm := strings.Index(string(argv), "logs\n--tail\n20\nclaims-portal-preview\n"), strings.LastIndex(string(argv), "rm\n-f\nclaims-portal-preview\n"); logs < 0 || rm < logs {
		t.Fatalf("the lines are read before the container is removed: argv %q", argv)
	}
}

// An app that runs but answers nothing on port 3000 — one that ignores PORT — is a failed start, its
// last lines read before its container goes, not an app that is up (contracts/executor.md, /run; #1238).
func TestRunAnAppThatAnswersNothingOnPort3000AnswersItsLastLines(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"printf '%s\\n' \"$@\" >> \""+dir+"/argv\"\ncase \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; *State.Status*) echo 'running 0 canyon/claims-portal'; exit 0;; esac\ncase \"$*\" in *' nc -z '*) exit 1;; esac\n[ \"$1\" = logs ] && { echo '{\"event\": \"listening\", \"port\": 8080}'; exit 0; }\n[ \"$1\" = run ] && echo cafe\nexit 0\n"), 0o700)) //nolint:gosec // test fake
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	var exited *Exited
	if !errors.As(err, &exited) || exited.Reason != "the app is running and nothing answers on port 3000: it must listen on $PORT" || exited.Output != "{\"event\": \"listening\", \"port\": 8080}\n" {
		t.Fatalf("an app deaf on port 3000: %#v (%v)", exited, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if logs, rm := strings.Index(string(argv), "logs\n--tail\n20\nclaims-portal-preview\n"), strings.LastIndex(string(argv), "rm\n-f\nclaims-portal-preview\n"); logs < 0 || rm < logs {
		t.Fatalf("the lines are read before the container is removed: argv %q", argv)
	}
	if !strings.HasSuffix(string(argv), "rename\nclaims-portal-preview-previous\nclaims-portal-preview\n"+holdersLs("claims-portal")) {
		t.Fatalf("the previous container is named back: argv %q", argv)
	}
}

// An app that stops between its status and its port check leaves the helper no namespace to join, and
// docker refuses the helper: that is the app's failure, answered with its last lines like any exit.
func TestRunAnAppThatStopsBeforeItsPortCheckAnswersItsLastLines(t *testing.T) {
	e, dir := fakes(t)
	docker := `#!/bin/sh
printf '%s\n' "$@" >> "` + dir + `/argv"
` + hostAddrsFake + `case "$*" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case "$a" in name=*) n=${a#name=^}; echo "${n%?} running sha256:abababababababababababababababababababababababababababababababab";; esac; done; exit 0;; *State.Status*) [ -e "` + dir + `/checked" ] && { echo 'exited 0 canyon/claims-portal'; exit 0; }; echo 'running 0 canyon/claims-portal'; exit 0;; esac
case "$*" in *' nc -z '*) touch "` + dir + `/checked"; echo 'docker: Error response from daemon: cannot join network namespace of a non running container' >&2; exit 125;; esac
[ "$1" = logs ] && { echo 'FATAL ERROR: Reached heap limit'; exit 0; }
[ "$1" = run ] && echo cafe
exit 0
`
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o700)) //nolint:gosec // test fake
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	var exited *Exited
	if !errors.As(err, &exited) || exited.Reason != "the app did not stay up: its container is exited" || exited.Output != "FATAL ERROR: Reached heap limit\n" {
		t.Fatalf("an app that stopped before its port check: %#v (%v)", exited, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	if logs, rm := strings.Index(string(argv), "logs\n--tail\n20\nclaims-portal-preview\n"), strings.LastIndex(string(argv), "rm\n-f\nclaims-portal-preview\n"); logs < 0 || rm < logs {
		t.Fatalf("the lines are read before the container is removed: argv %q", argv)
	}
	if !strings.HasSuffix(string(argv), "rename\nclaims-portal-preview-previous\nclaims-portal-preview\n"+holdersLs("claims-portal")) {
		t.Fatalf("the previous container is named back: argv %q", argv)
	}
}

// An app that answers on port 3000 after the settle window is up; the connect is made by a pinned
// busybox sharing the app's network namespace, locked down, to the address the server uses
// (contracts/executor.md, /run): on an open network the holder's, whose namespace the app runs in, at its
// own name, which only this call's app answers on while a redeploy's two holders share the app's alias.
func TestRunAnAppThatAnswersOnPort3000IsUp(t *testing.T) {
	e, dir := fakes(t)
	res, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	if err != nil || res.Addr != "claims-portal-preview:3000" {
		t.Fatalf("an app answering on 3000 is up: %+v (%v)", res, err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
	holder := regexp.MustCompile(`\nrun\n-d\n--name\n(canyon-appfloor-[0-9a-f]{8})\n`).FindStringSubmatch(string(argv))
	if holder == nil {
		t.Fatalf("no holder started: argv %q", argv)
	}
	want := "run\n--rm\n--network\ncontainer:" + holder[1] + "\n--user\n65534:65534\n--cap-drop\nALL\n--security-opt\nno-new-privileges\n" + busyboxImage + "\nnc\n-z\n-w\n2\n" + holder[1] + "\n3000\n"
	if !strings.Contains(string(argv), want) {
		t.Fatalf("the port is tried from the app's network namespace: argv %q", argv)
	}
}

// The port helper gets docker's allowlisted environment alone: the app's values are its container's,
// never the helper's.
func TestRunsPortHelperNeverSeesTheAppsValues(t *testing.T) {
	e, dir := fakes(t)
	env := map[string]string{"DATABASE_URL": "value-db-7f3a-long", "STRIPE_SECRET_KEY": "line one\nline two"} //nolint:gosec // test values, no credential
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal", Env: env})
	must(t, err)
	seen, err := os.ReadFile(filepath.Join(dir, "helper-env")) //nolint:gosec // test dir
	must(t, err)
	if !strings.Contains(string(seen), "PATH=") {
		t.Fatalf("the helper's docker gets the allowlisted environment: %q", seen)
	}
	for name, value := range env {
		for _, line := range strings.Split(value, "\n") {
			if strings.Contains(string(seen), name+"=") || strings.Contains(string(seen), line) {
				t.Fatalf("%s reaches the port helper's docker: %q", name, seen)
			}
		}
	}
}

// A helper docker cannot start says nothing about the app: the start fails with the reason alone,
// never the port reason and the app's lines, and the previous container is named back. Only exit 1 is
// nc's own failure: 125, docker's for a container it cannot start, and any other, as 2, are not.
func TestRunWhosePortHelperDockerCannotStartIsNotTheAppsFault(t *testing.T) {
	for _, code := range []string{"2", "125"} {
		e, dir := fakes(t)
		must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"printf '%s\\n' \"$@\" >> \""+dir+"/argv\"\ncase \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; *State.Status*) echo 'running 0 canyon/claims-portal'; exit 0;; esac\ncase \"$*\" in *' nc -z '*) echo 'docker: Error response from daemon: dead' >&2; exit "+code+";; esac\n[ \"$1\" = run ] && echo cafe\nexit 0\n"), 0o700)) //nolint:gosec // test fake
		_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
		var exited *Exited
		if err == nil || errors.As(err, &exited) {
			t.Fatalf("exit %s: a helper docker cannot start is not the app's failure: %v", code, err)
		}
		argv, _ := os.ReadFile(filepath.Join(dir, "argv")) //nolint:gosec // test dir
		if !strings.HasSuffix(string(argv), "rename\nclaims-portal-preview-previous\nclaims-portal-preview\n"+holdersLs("claims-portal")) {
			t.Fatalf("exit %s: the previous container is named back: argv %q", code, argv)
		}
	}
}

// A DSN is a container for its password, and an app that fails to connect logs the password alone:
// it is named on its own, decoded, after the whole value (contracts/executor.md, /run).
func TestNewExitedNamesAURLsPasswordOnItsOwn(t *testing.T) {
	password := "pw-" + "7f3a:b"                              // decoded; the URL spells the colon %3A
	dsn := "postgres://app:" + "pw-7f3a%3Ab" + "@db:5432/app" // spelled in parts: a whole literal reads as a credential to the scanners
	lines := "connecting to " + dsn + "\npassword authentication failed for user \"app\" (" + password + ")\n"
	want := "connecting to <DATABASE_URL>\npassword authentication failed for user \"app\" (<DATABASE_URL password>)\n"
	if got := NewExited("r", lines, map[string]string{"DATABASE_URL": dsn, "PLAIN": "no-url-here"}).Output; got != want {
		t.Fatalf("output %q\nwant   %q", got, want)
	}
}

// The lines are cut to their last 8 KiB after the values are named, so no cut leaves part of a value
// behind — and one line longer than that keeps its end rather than nothing.
func TestNewExitedCutsAfterNamingTheValues(t *testing.T) {
	secret := strings.Repeat("s", 100)
	lines := strings.Repeat("x", 50) + secret + strings.Repeat("y", exitedBytes) + " last words " + secret + "\n"
	got := NewExited("r", lines, map[string]string{"TOKEN": secret}).Output
	if len(got) != exitedBytes || strings.Contains(got, "sss") || !strings.HasSuffix(got, "y last words <TOKEN>\n") {
		t.Fatalf("cut output (%d bytes) ending %q", len(got), got[max(0, len(got)-40):])
	}
}

// A container whose lines docker cannot read is still an app that stopped: the reason says the lines
// are gone, and docker's own words are never passed off as the app's.
func TestRunAnAppWhoseLinesCannotBeReadAnswersTheReasonAlone(t *testing.T) {
	e, dir := fakes(t)
	must(t, os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+hostAddrsFake+"case \"$*\" in *Repository*) echo sha256:abababababababababababababababababababababababababababababababab canyon/claims-portal:pkg_7d3e8f-abcd; exit 0;; *.Names*) for a; do case \"$a\" in name=*) n=${a#name=^}; echo \"${n%?} running sha256:abababababababababababababababababababababababababababababababab\";; esac; done; exit 0;; *State.Status*) echo 'exited 0 canyon/claims-portal'; exit 0;; esac\n[ \"$1\" = logs ] && { echo 'Error response from daemon: dead' >&2; exit 1; }\n[ \"$1\" = run ] && echo cafe\nexit 0\n"), 0o700)) //nolint:gosec // test fake
	_, err := e.Run(context.Background(), RunRequest{App: "claims-portal", Target: "preview", Image: "canyon/claims-portal@sha256:" + strings.Repeat("ab", 32), Network: "canyon-app-claims-portal"})
	var exited *Exited
	if !errors.As(err, &exited) || exited.Output != "" || exited.Reason != "the app did not stay up: its container is exited; its last lines could not be read" {
		t.Fatalf("an unreadable exit: %#v (%v)", exited, err)
	}
}

// An app's lines are read into at most exitedRead bytes, however long its lines are, and when the
// bound falls inside a line that line is dropped, so a value it began in cannot escape the naming.
func TestATailHoldsItsBoundAndStartsAtALine(t *testing.T) {
	var tail Tail
	secret := "value-" + strings.Repeat("k", 100)
	for range 3 {
		_, _ = tail.Write([]byte(strings.Repeat("x", exitedRead/2) + secret)) // one line far longer than the bound
	}
	_, _ = tail.Write([]byte("\nlast line\n"))
	if len(tail.buf) > exitedRead || tail.Lines() != "last line\n" {
		t.Fatalf("held %d bytes; lines %q", len(tail.buf), tail.Lines()[:min(40, len(tail.Lines()))])
	}
	var short Tail
	_, _ = short.Write([]byte("one\ntwo\n"))
	if short.Lines() != "one\ntwo\n" {
		t.Fatalf("a tail within the bound is kept whole: %q", short.Lines())
	}
}
