//go:build e2e

package executor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate/opengrep"
	"github.com/getcanyon-ai/canyon/internal/names"
	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// TestEndToEndBuildsAndServesTheFixtureApp needs Docker, pack and network access: it builds
// gate/testdata/apps/claims-portal with the real builder, runs it on its own network, and asks it
// for /login (200) and the unguarded /api/admin/policies (200 — the finding). CI runs it with
// -tags e2e; locally it needs a Linux Docker daemon.
func TestEndToEndBuildsAndServesTheFixtureApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}} // no server or app-postgres here: the app's network is its own alone
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "claims-portal", Package: "pkg_e2e0", BuildValues: map[string]string{"NEXT_PUBLIC_E2E": "set at build"}}, treeOf(t, filepath.Join("..", "gate", "testdata", "apps", "claims-portal")), &log) // value-less --env: pack must read it from its environment
	if err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/claims-portal@")).Run() // by id: its tag goes with it
	// The real builder's group for a Node app, named by its metadata (contracts/executor.md, The buildpacks that ran).
	if !slices.Contains(res.Buildpacks, Buildpack{ID: "paketo-buildpacks/node-engine", Name: "Paketo Buildpack for Node Engine"}) {
		t.Fatalf("the buildpacks that ran: %+v", res.Buildpacks)
	}
	value := "line one\nline two" // a value spans lines and reaches the container whole
	run, err := e.Run(ctx, RunRequest{App: "claims-portal", Target: "preview", Image: res.Image, Network: "canyon-app-claims-portal", Env: map[string]string{"CANYON_E2E_VALUE": value}})
	must(t, err)
	defer exec.Command("docker", "rm", "-f", run.Container).Run()
	if out, err := exec.CommandContext(ctx, "docker", "exec", run.Container, "printenv", "CANYON_E2E_VALUE").Output(); err != nil || string(out) != value+"\n" {
		t.Fatalf("the container's environment: %q (%v), want the value whole", out, err)
	}
	for _, path := range []string{"/login", "/api/admin/policies?orgId=meridian"} {
		get(ctx, t, "claims-portal", run, path)
	}
	for range 2 { // the second finds nothing on the real daemon and is the same success (invariant 6)
		must(t, e.Stop(ctx, "claims-portal", "preview"))
	}
	if exec.Command("docker", "container", "inspect", run.Container).Run() == nil || exec.Command("docker", "network", "inspect", "canyon-app-claims-portal").Run() == nil {
		t.Fatalf("%s or its network outlived /stop", run.Container)
	}
}

// A tree no buildpack recognises, on the real builder, is a failed detect naming what the builder builds,
// never a failed build (contracts/executor.md, /build; #1242): a README alone. Paketo's node-start errors
// on any tree with no Node entry point, so the detector ends 21 and the answer is errored. pack runs with
// --no-color, so its last line is read as written.
func TestEndToEndATreeNoBuildpackDetectsIsAFailedDetect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	defer exec.Command("docker", "volume", "rm", "-f", names.CacheVolume("readme-only", "build"), names.CacheVolume("readme-only", "launch")).Run()
	var log bytes.Buffer
	_, err := e.Build(ctx, BuildRequest{App: "readme-only", Package: "pkg_e2e0", BuildValues: map[string]string{}}, bytes.NewReader(tarOf(t, map[string]string{"README.txt": "nothing to build\n"})), &log)
	var undetected *Undetected
	if !errors.As(err, &undetected) || !slices.Contains(undetected.Builds, "Paketo Buildpack for Node.js") || !undetected.Errored || strings.ContainsRune(log.String(), 0x1b) {
		t.Fatalf("a README alone: %#v\n%s", err, log.String())
	}
}

// TestEndToEndBuildsAndServesAStaticSPA builds executor/testdata/apps/vite-spa, a stock Vite React app
// carrying the project.toml cli/skill/canyon.md gives a static single-page app, and serves what its build
// wrote: / is the index.html vite build emitted, and a client-side route answers it too.
func TestEndToEndBuildsAndServesAStaticSPA(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "vite-spa", Package: "pkg_e2e1"}, treeOf(t, filepath.Join("testdata", "apps", "vite-spa")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, log.String())
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/vite-spa@")).Run()
	run, err := e.Run(ctx, RunRequest{App: "vite-spa", Target: "preview", Image: res.Image, Network: "canyon-app-vite-spa"})
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "vite-spa", "preview") }()
	for _, path := range []string{"/", "/reports"} {
		if body := get(ctx, t, "vite-spa", run, path); !strings.Contains(body, `src="/assets/index-`) {
			t.Fatalf("GET %s is not the index.html the build wrote:\n%s", path, body)
		}
	}
}

// TestEndToEndBuildsAndServesAStockNextApp builds executor/testdata/apps/next-stock, a create-next-app
// with its default "build": "next build", no project.toml and TypeScript as a devDependency, and serves
// its page: Paketo's npm-install hands the build script NODE_ENV=development, under which next build
// fails prerendering its 404 page (canyonv2-g6il).
func TestEndToEndBuildsAndServesAStockNextApp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "next-stock", Package: "pkg_e2e2"}, treeOf(t, filepath.Join("testdata", "apps", "next-stock")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/next-stock@")).Run()
	run, err := e.Run(ctx, RunRequest{App: "next-stock", Target: "preview", Image: res.Image, Network: "canyon-app-next-stock"})
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "next-stock", "preview") }()
	if body := get(ctx, t, "next-stock", run, "/"); !strings.Contains(body, "Expense tracker") {
		t.Fatalf("GET / is not the app's page:\n%s", body)
	}
}

// TestEndToEndBuildsAndServesAStockNext16App builds executor/testdata/apps/next16-stock, a
// create-next-app 16.3.8 as it comes (Turbopack, Tailwind, "build": "next build"), with Turbopack,
// and serves its page: Turbopack refused npm-install's node_modules link, which leaves the app's
// directory, until every build ran with NEXT_PRIVATE_OUTPUT_TRACE_ROOT=/ (#1240). Red after a Next
// upgrade means Next moved its private name (#1461).
func TestEndToEndBuildsAndServesAStockNext16App(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "next16-stock", Package: "pkg_e2e4"}, treeOf(t, filepath.Join("testdata", "apps", "next16-stock")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/next16-stock@")).Run()
	if !strings.Contains(log.String(), "Next.js 16.3.8 (Turbopack)") {
		t.Fatalf("the build did not run Turbopack, the bundler a stock Next 16 builds with:\n%s", tail(log.String(), 60))
	}
	run, err := e.Run(ctx, RunRequest{App: "next16-stock", Target: "preview", Image: res.Image, Network: "canyon-app-next16-stock"})
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "next16-stock", "preview") }()
	if body := get(ctx, t, "next16-stock", run, "/"); !strings.Contains(body, "To get started, edit the") {
		t.Fatalf("GET / is not the app's page:\n%s", body)
	}
}

// TestEndToEndBuildsAndServesANodeAppOnTheNewestNode builds executor/testdata/apps/node-engines, whose
// package.json says engines ">=20", as many apps' do: Paketo gives it the newest Node it offers, which
// links libatomic.so.1, and the app must start on the image it runs on and answer (canyonv2-aeur).
func TestEndToEndBuildsAndServesANodeAppOnTheNewestNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "node-engines", Package: "pkg_e2e3"}, treeOf(t, filepath.Join("testdata", "apps", "node-engines")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/node-engines@")).Run()
	_, picked, _ := strings.Cut(log.String(), "Selected Node Engine version (using package.json): ")
	major, _, _ := strings.Cut(picked, ".")
	if n, err := strconv.Atoi(major); err != nil || n < 25 { // the fixture proves something only on a Node that links the library
		t.Fatalf("Paketo picked Node %q for engines >=20; this test is about 25 and later:\n%s", major, tail(log.String(), 60))
	}
	run, err := e.Run(ctx, RunRequest{App: "node-engines", Target: "preview", Image: res.Image, Network: "canyon-app-node-engines"})
	var exited *Exited
	if errors.As(err, &exited) {
		t.Fatalf("Node %s did not start: %s\n%s", major, exited.Reason, exited.Output)
	}
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "node-engines", "preview") }()
	if body := get(ctx, t, "node-engines", run, "/"); !strings.HasPrefix(body, "running on node "+major+".") {
		t.Fatalf("GET / is not the app's answer: %q", body)
	}
}

// TestEndToEndBuildsAndServesAStockDjangoAppOnSQLite builds executor/testdata/apps/django-sqlite,
// `django-admin startproject` on Python 3.12 with gunicorn: its database is SQLite, so the app imports
// Python's sqlite3 as it boots, which links libsqlite3.so.0 — canyon-run must carry it (#1708).
func TestEndToEndBuildsAndServesAStockDjangoAppOnSQLite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "django-sqlite", Package: "pkg_e2e5"}, treeOf(t, filepath.Join("testdata", "apps", "django-sqlite")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/django-sqlite@")).Run()
	run, err := e.Run(ctx, RunRequest{App: "django-sqlite", Target: "preview", Image: res.Image, Network: "canyon-app-django-sqlite"})
	var exited *Exited
	if errors.As(err, &exited) {
		t.Fatalf("Django did not start: %s\n%s", exited.Reason, exited.Output)
	}
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "django-sqlite", "preview") }()
	if body := get(ctx, t, "django-sqlite", run, "/"); !strings.Contains(body, "The install worked successfully!") {
		t.Fatalf("GET / is not Django's welcome page:\n%s", tail(body, 20))
	}
}

// TestEndToEndBuildsAndServesAPHPAppOnPostgres builds executor/testdata/apps/php-pgsql, a plain PHP app whose
// composer.json requires ext-pdo_pgsql, which links libpq.so.5: Composer checks the extension at build, so
// the builder must carry the library, and the app loads it at run, so canyon-run must too (#1734). Served
// beside a Postgres joined to its network, it answers the count of the rows it wrote.
func TestEndToEndBuildsAndServesAPHPAppOnPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	const pg = "canyon-e2e-php-pg"
	must(t, exec.Command("docker", "run", "-d", "--rm", "--name", pg, "-e", "POSTGRES_PASSWORD=e2e", "postgres:16-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea").Run())
	defer exec.Command("docker", "rm", "-f", pg).Run()
	for i := 0; exec.Command("docker", "exec", pg, "pg_isready", "-U", "postgres", "-q").Run() != nil; i++ {
		if i == 60 {
			t.Fatal("postgres never became ready")
		}
		time.Sleep(time.Second)
	}
	e := &Executor{Join: []string{pg}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "php-pgsql", Package: "pkg_e2e6"}, treeOf(t, filepath.Join("testdata", "apps", "php-pgsql")), &log)
	if err != nil {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/php-pgsql@")).Run()
	run, err := e.Run(ctx, RunRequest{App: "php-pgsql", Target: "preview", Image: res.Image, Network: "canyon-app-php-pgsql",
		Env: map[string]string{"DATABASE_URL": "postgres://postgres:e2e@" + pg + ":5432/postgres?sslmode=disable"}})
	var exited *Exited
	if errors.As(err, &exited) {
		t.Fatalf("PHP did not start: %s\n%s", exited.Reason, exited.Output)
	}
	must(t, err)
	defer func() { _ = e.Stop(context.Background(), "php-pgsql", "preview") }()
	if body := get(ctx, t, "php-pgsql", run, "/"); !strings.HasPrefix(body, `{"n":`) {
		t.Fatalf("GET / is not the row count: %q", tail(body, 10))
	}
}

// TestEndToEndABuildReachesNoPrivateNorLinkLocalAddress is invariant 12 on Docker: executor/testdata/apps/egress-probe's
// postinstall, the app's own build step, opens TCP to the cloud's metadata address, a link-local stand-in and a
// private one and a public address of the host's own, each on 80, and Azure's WireServer — every one refused — and to
// a public registry on 443, reached. The stand-ins answer from a plain bridge first, the network pack gave builds
// before, so their refusal is the floor's: a listener on the host at 169.254.42.1, at 198.51.100.9 on the host's
// loopback, and at this host's own private address.
func TestEndToEndABuildReachesNoPrivateNorLinkLocalAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	conn, err := net.Dial("udp", "1.1.1.1:53") // no packet: the address this host's route out leaves from
	must(t, err)
	private, _ := netip.ParseAddrPort(conn.LocalAddr().String())
	_ = conn.Close()
	if !private.Addr().Is4() || !private.Addr().IsPrivate() {
		t.Fatalf("this host leaves from %s, not a private IPv4 address: the private stand-in would prove nothing", private.Addr())
	}
	// a link-local address, and a public one of the host's own (TEST-NET-2's), as a server with a public IP on its
	// interface has, where Canyon's own server may be published on 80 and 443
	const linkLocal, public = "169.254.42.1", "198.51.100.9"
	docker := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // the test's own arguments
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return string(out)
	}
	docker("run", "-d", "--name", "canyon-e2e-standin", "--network", "host", "--cap-add", "NET_ADMIN", busyboxImage, "sh", "-c",
		"ip addr add "+linkLocal+"/32 dev lo && ip addr add "+public+"/32 dev lo && httpd -p "+linkLocal+":80 -h /etc && httpd -p "+public+":80 -h /etc && "+
			"exec httpd -f -p "+private.Addr().String()+":80 -h /etc")
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "canyon-e2e-standin").Run()
		for _, a := range []string{linkLocal, public} {
			_ = exec.Command("docker", "run", "--rm", "--network", "host", "--cap-add", "NET_ADMIN", busyboxImage, "ip", "addr", "del", a+"/32", "dev", "lo").Run()
		}
	})
	reached := func(target string) bool {
		host, port, _ := strings.Cut(target, ":")
		return exec.CommandContext(ctx, "docker", "run", "--rm", busyboxImage, "nc", "-z", "-w", "5", host, port).Run() == nil //nolint:gosec // the test's own targets
	}
	standins := []string{linkLocal + ":80", private.Addr().String() + ":80", public + ":80"}
	for deadline := time.Now().Add(30 * time.Second); !reached(standins[0]) || !reached(standins[1]) || !reached(standins[2]); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatalf("the stand-ins %v answer no container on Docker's bridge: refusing them would prove nothing", standins)
		}
	}
	for _, cloud := range []string{"169.254.169.254:80", "168.63.129.16:80"} { // on GitHub's runners, Azure VMs, both answer
		t.Logf("%s from Docker's bridge: reached %t", cloud, reached(cloud))
	}
	targets := append([]string{"169.254.169.254:80", "168.63.129.16:80"}, standins...)
	e := &Executor{Join: []string{}}
	var log bytes.Buffer
	res, err := e.Build(ctx, BuildRequest{App: "egress-probe", Package: "pkg_e2e1", BuildValues: map[string]string{
		"PROBE_TARGETS": strings.Join(targets, ","), "PROBE_PUBLIC": "registry.npmjs.org:443"}}, treeOf(t, filepath.Join("testdata", "apps", "egress-probe")), &log)
	if err == nil {
		defer exec.Command("docker", "image", "rm", "-f", strings.TrimPrefix(res.Image, "canyon/egress-probe@")).Run()
	}
	for _, target := range targets {
		if !strings.Contains(log.String(), "probe: refused "+target+" (ECONNREFUSED)") { // the floor's REJECT, never a silence that might be the network's
			t.Errorf("the build step was not refused %s", target)
		}
	}
	if !strings.Contains(log.String(), "probe: reached registry.npmjs.org:443") {
		t.Error("the build step did not reach registry.npmjs.org:443: a floor that lets nothing out proves nothing")
	}
	if err != nil || t.Failed() {
		t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
	}
}

// The real daemon's answer for a container it does not have is the one status reads as none, so
// GET /running is 404 for it and never the 502 of a daemon that did not answer.
func TestRunningE2EIsNoContainerForOneTheDaemonDoesNotHave(t *testing.T) {
	if _, err := (&Executor{}).Running(context.Background(), "canyon-e2e-absent", "prod"); !errors.Is(err, ErrNoContainer) {
		t.Fatalf("%v, want ErrNoContainer", err)
	}
}

// TestAppNetworkE2E runs a stand-in app, busybox's httpd, on the real daemon: its network is created on
// the pool's first /24 docker does not refuse as overlapping, its gateway the /24's first address, with
// the joined container on it and that container's default gateway where it was; /stop of the app's last
// container removes it and leaves the joined container running; /forget removes one a crash left. A
// stand-in listening on another port, or on port 3000 of its loopback alone, is a failed start.
func TestAppNetworkE2E(t *testing.T) {
	ctx := context.Background()
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	standIn := func(tag, listen string) string { // busybox's httpd listening on listen, as this app's build
		build := exec.Command("docker", "build", "-q", "-t", "canyon/appnet-e2e:"+tag, "-")
		build.Stdin = strings.NewReader("FROM " + busybox + "\nCMD [\"httpd\", \"-f\", \"-p\", \"" + listen + "\"]\n")
		id, err := build.Output()
		must(t, err)
		return "canyon/appnet-e2e@" + strings.TrimSpace(string(id))
	}
	join, network, taken := "canyon-appnet-e2e-join", "canyon-app-appnet-e2e", "canyon-appnet-e2e-taken"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "appnet-e2e-preview", "appnet-e2e-preview-previous", join).Run()
		rmHolders("appnet-e2e")
		_ = exec.Command("docker", "network", "rm", network, taken).Run()
		_ = exec.Command("docker", "image", "rm", "canyon/appnet-e2e:e2e", "canyon/appnet-e2e:elsewhere", "canyon/appnet-e2e:loopback").Run()
	})
	image, elsewhere, loopback := standIn("e2e", "3000"), standIn("elsewhere", "8080"), standIn("loopback", "127.0.0.1:3000")
	// a pool outside Docker's default address pools (172.17-31/16, 192.168/16), its first /24 taken here
	if out, err := exec.Command("docker", "network", "create", "--subnet", "10.254.254.0/24", taken).CombinedOutput(); err != nil {
		if strings.Contains(string(out), overlaps) {
			t.Skipf("this daemon already holds part of the test pool 10.254.254.0/23: %s", out)
		}
		t.Fatalf("docker network create: %v: %s", err, out)
	}
	// The joined container's own network sorts after the app's, as canyon-internal does after
	// canyon-app-<app>: between equal priorities docker gives the default gateway to the first by name.
	must(t, exec.Command("docker", "run", "-d", "--name", join, "--network", taken, busybox, "sleep", "600").Run())
	e := &Executor{Pool: netip.MustParsePrefix("10.254.254.0/23"), Join: []string{join}, Settle: time.Second}
	_, err := e.Run(ctx, RunRequest{App: "appnet-e2e", Target: "preview", Image: image, Network: network, Env: map[string]string{}})
	must(t, err)
	// Docker publishes a container's ports through its default gateway's network, and moving the gateway
	// cuts every connection open through them: a DELETE whose /stop disconnected canyon-server lost its answer.
	if route, err := exec.Command("docker", "exec", join, "ip", "route", "show", "default").Output(); err != nil || !strings.HasPrefix(string(route), "default via 10.254.254.1 ") {
		t.Fatalf("the joined container's default route after /run: %q (%v); want via its own network's gateway, 10.254.254.1", route, err)
	}
	out, err := exec.Command("docker", "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}} {{.Gateway}}{{end}}{{range .Containers}} {{.Name}}{{end}}", network).Output()
	got := strings.Fields(string(out))
	if err != nil || len(got) != 4 || got[0] != "10.254.255.0/24" || got[1] != "10.254.255.1" || !slices.Contains(got, join) || !slices.ContainsFunc(got, func(m string) bool { return strings.HasPrefix(m, "canyon-appfloor-") }) {
		t.Fatalf("subnet, gateway, members: %q (%v); want the pool's second /24, its first address, the app's floor holder and %s", got, err, join)
	}
	// nothing answers on port 3000 at the address the server uses: the start fails with the app's lines,
	// and the container that ran runs on, under its name
	preview := func() string {
		out, _ := exec.Command("docker", "inspect", "--format", "{{.Id}} {{.State.Running}}", "appnet-e2e-preview").Output()
		return strings.TrimSpace(string(out))
	}
	before := preview()
	if !strings.HasSuffix(before, " true") {
		t.Fatalf("appnet-e2e-preview after /run: %q, want running", before)
	}
	for _, deaf := range []string{elsewhere, loopback} {
		_, err := e.Run(ctx, RunRequest{App: "appnet-e2e", Target: "preview", Image: deaf, Network: network, Env: map[string]string{}})
		var exited *Exited
		if !errors.As(err, &exited) || exited.Reason != NothingOnPort {
			t.Fatalf("%s: %v, want %q", deaf, err, NothingOnPort)
		}
		if after := preview(); after != before || exec.Command("docker", "container", "inspect", "appnet-e2e-preview-previous").Run() == nil {
			t.Fatalf("after %s: appnet-e2e-preview is %q, want %q named back, nothing left aside", deaf, after, before)
		}
	}
	gone := func(what string) {
		t.Helper()
		running, _ := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", join).Output()
		if exec.Command("docker", "network", "inspect", network).Run() == nil || strings.TrimSpace(string(running)) != "true" {
			t.Fatalf("after %s: the network outlived it, or %s did not (running %q)", what, join, running)
		}
	}
	must(t, e.Stop(ctx, "appnet-e2e", "preview"))
	gone("/stop")
	must(t, exec.Command("docker", "network", "create", network).Run()) // what a crash between the create and the start leaves
	must(t, exec.Command("docker", "network", "connect", "--gw-priority=-1", network, join).Run())
	must(t, e.Forget(ctx, "appnet-e2e"))
	gone("/forget")
}

// TestEgressE2EPinsTheAppAndLeavesItNoWayOutButTheDoor runs a stand-in app with egress on the real daemon
// (contracts/executor.md, Egress): its network internal on the pool's first free /24, docker's addresses in the
// upper /25, the app at preview's .10, then .12 after a redeploy, prod at .11, the proxy variables set; a raw
// dial from it, and a connect to a service on the host's every address through its gateway, reach nothing a
// container on an open network reaches; POST /callers names each fixed address and nothing else; a /run without
// egress there is refused; and with the door down a redeploy fails and the one that ran runs on, while another
// app's /run without egress, whose network the door never joins, starts.
func TestEgressE2EPinsTheAppAndLeavesItNoWayOutButTheDoor(t *testing.T) {
	ctx := context.Background()
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	build := exec.Command("docker", "build", "-q", "-t", "canyon/egress-e2e:e2e", "-")
	build.Stdin = strings.NewReader("FROM " + busybox + "\nCMD [\"httpd\", \"-f\", \"-p\", \"3000\"]\n")
	id, err := build.Output()
	must(t, err)
	image := "canyon/egress-e2e@" + strings.TrimSpace(string(id))
	door, own, taken, network := "canyon-egress-e2e", "canyon-egress-e2e-own", "canyon-egress-e2e-taken", "canyon-app-egress-e2e"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "egress-e2e-preview", "egress-e2e-preview-previous", "egress-e2e-prod", "egress-e2e-plain-preview", door).Run()
		rmHolders("egress-e2e-plain")
		_ = exec.Command("docker", "network", "rm", network, "canyon-app-egress-e2e-plain", own, taken).Run()
		_ = exec.Command("docker", "image", "rm", "canyon/egress-e2e:e2e", "canyon/egress-e2e-plain:e2e").Run()
	})
	// the pool's first /24 held by another network: with --ip-range docker would not refuse it as overlapping,
	// it fails on the gateway, so the executor must skip it itself
	for name, subnet := range map[string]string{own: "10.254.249.0/24", taken: "10.254.250.0/24"} {
		if out, err := exec.Command("docker", "network", "create", "--subnet", subnet, name).CombinedOutput(); err != nil {
			if strings.Contains(string(out), overlaps) {
				t.Skipf("this daemon already holds part of the test pools 10.254.249.0/24-10.254.251.0/24: %s", out)
			}
			t.Fatalf("docker network create: %v: %s", err, out)
		}
	}
	// the door: something answering on 3128 where canyon-egress would be, joined to the app's network as it is
	must(t, exec.Command("docker", "run", "-d", "--name", door, "--network", own, busybox, "httpd", "-f", "-p", "3128").Run())
	e := &Executor{Pool: netip.MustParsePrefix("10.254.250.0/23"), Join: []string{}, Door: []string{door}, Settle: time.Second}
	req := RunRequest{App: "egress-e2e", Target: "preview", Image: image, Network: network, Env: map[string]string{},
		Identity: Identity{JWKSURL: "http://canyon-server:7780/.well-known/jwks.json"}, Run: "gr_e2e0",
		Egress: &Egress{Proxy: "http://" + door + ":3128", NoProxy: []string{}}}
	up := func(req RunRequest) {
		t.Helper()
		_, err := e.Run(ctx, req)
		must(t, err)
	}
	up(req)
	inspect := func(format, of string) string {
		out, err := exec.Command("docker", "inspect", "--format", format, of).Output()
		must(t, err)
		return strings.TrimSpace(string(out))
	}
	if got := inspect("{{.Internal}} {{range .IPAM.Config}}{{.Subnet}} {{.IPRange}}{{end}}", network); got != "true 10.254.251.0/24 10.254.251.128/25" {
		t.Fatalf("%s: %q, want internal on the pool's free /24, docker's addresses in its upper /25", network, got)
	}
	at := func(c string) string {
		return inspect(`{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, c)
	}
	if got := at("egress-e2e-preview"); got != "10.254.251.10" {
		t.Fatalf("preview at %s, want .10", got)
	}
	if env := inspect(`{{join .Config.Env " "}}`, "egress-e2e-preview"); !strings.Contains(env, "HTTPS_PROXY=http://"+door+":3128 ") || !strings.Contains(env, "NO_PROXY=canyon-server,localhost,127.0.0.1 ") {
		t.Fatalf("preview's environment: %q, want the proxy and what reaches past it", env)
	}
	dial := func(from string) int { // a raw dial to a public address: 0 reached, 1 not
		err := exec.Command("docker", "run", "--rm", "--network", from, busybox, "nc", "-z", "-w", "5", "1.1.1.1", "80").Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		if err != nil {
			return exit.ExitCode()
		}
		return 0
	}
	if dial(own) != 0 {
		t.Fatalf("1.1.1.1:80 was not reached from %s, an open network: the refusal below proves nothing", own)
	}
	if code := dial("container:egress-e2e-preview"); code == 0 {
		t.Fatal("a raw dial from the app routed through the door reached 1.1.1.1:80")
	}
	// a service on the host's every address — a natively run executor's :7778 — answers on each bridge's gateway
	ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // the host's wildcard address is what this probes
	must(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	host := func(from, gateway string) bool { // nc's 1 is not reached; anything else is docker's
		err := exec.Command("docker", "run", "--rm", "--network", from, busybox, "nc", "-z", "-w", "3", gateway, port).Run()
		var exit *exec.ExitError
		if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
			t.Fatal(err)
		}
		return err == nil
	}
	if !host(own, "10.254.249.1") {
		t.Fatalf("the host's :%s was not reached from %s through its gateway: the refusal below proves nothing", port, own)
	}
	if host("container:egress-e2e-preview", "10.254.251.1") {
		t.Fatalf("the host's :%s, on every address, was reached from the routed app through its gateway", port)
	}
	up(req) // a redeploy takes preview's other address, the previous one gone with its container
	req.Target = "prod"
	up(req)
	if got := at("egress-e2e-preview") + " " + at("egress-e2e-prod"); got != "10.254.251.12 10.254.251.11" {
		t.Fatalf("preview and prod at %s, want .12 and .11", got)
	}
	callers, err := e.Callers(ctx, []string{"10.254.251.10", "10.254.251.11", "10.254.251.12", at(door)})
	must(t, err)
	started := func(c string) time.Time { // since is the container's State.StartedAt, once docker gave it the address, never its Created (#2265)
		at, err := time.Parse(time.RFC3339Nano, inspect("{{.State.StartedAt}}", c))
		must(t, err)
		created, err := time.Parse(time.RFC3339Nano, inspect("{{.Created}}", c))
		must(t, err)
		if !at.After(created) {
			t.Fatalf("%s started at %v, not after its Created %v: since below would check nothing", c, at, created)
		}
		return at.UTC()
	}
	gateRun, want := "gr_e2e0", []Caller{{Address: "10.254.251.11", App: "egress-e2e", Target: "prod", Kind: "web", Since: started("egress-e2e-prod")},
		{Address: "10.254.251.12", App: "egress-e2e", Target: "preview", Kind: "web", Since: started("egress-e2e-preview")}}
	want[0].Run, want[1].Run = &gateRun, &gateRun
	if !reflect.DeepEqual(callers, want) {
		t.Fatalf("POST /callers: %+v, want prod's and preview's fixed addresses alone, each since its container started", callers)
	}
	plain := req
	plain.Egress, plain.Run = nil, ""
	if _, err := e.Run(ctx, plain); !errors.Is(err, ErrInternal) {
		t.Fatalf("a /run without egress on the internal network: %v, want ErrInternal", err)
	}
	must(t, exec.Command("docker", "stop", "-t", "0", door).Run())
	req.Target = "preview"
	if _, err := e.Run(ctx, req); err == nil || !strings.Contains(err.Error(), "nothing answers at "+door+":3128, Canyon's egress gateway") {
		t.Fatalf("a redeploy with the door down: %v, want the door named", err)
	}
	if got := inspect("{{.State.Running}}", "egress-e2e-preview") + " " + at("egress-e2e-preview"); got != "true 10.254.251.12" {
		t.Fatalf("after the failed redeploy preview is %q, want the one that ran, running at .12", got)
	}
	must(t, exec.Command("docker", "tag", "canyon/egress-e2e:e2e", "canyon/egress-e2e-plain:e2e").Run()) // an app runs its own repository's images alone
	elsewhere := &Executor{Pool: netip.MustParsePrefix("10.254.252.0/24"), Join: []string{}, Door: []string{door}, Settle: time.Second}
	if _, err := elsewhere.Run(ctx, RunRequest{App: "egress-e2e-plain", Target: "preview", Image: "canyon/egress-e2e-plain@" + strings.TrimSpace(string(id)), Network: "canyon-app-egress-e2e-plain", Env: map[string]string{}}); err != nil {
		t.Fatalf("a /run without egress while the door is down: %v, want it started on an open network the door never joins", err)
	}
}

// TestRecreateNetworkE2EMovesAnAppAcrossTheFloorBothWays recreates a stand-in app's network on the real daemon
// (contracts/executor.md, POST /networks/recreate; the owner's decision, #2254): an app started without egress on an
// open network is stopped and left so, its network made internal on the same /24 with the door joined, and a
// /run with egress then starts it routed through the door, no raw dial out; recreated open, it is stopped again,
// the door gone from its network, and a /run without egress starts it reaching out directly.
func TestRecreateNetworkE2EMovesAnAppAcrossTheFloorBothWays(t *testing.T) {
	ctx := context.Background()
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	build := exec.Command("docker", "build", "-q", "-t", "canyon/recreate-e2e:e2e", "-")
	build.Stdin = strings.NewReader("FROM " + busybox + "\nCMD [\"httpd\", \"-f\", \"-p\", \"3000\"]\n")
	id, err := build.Output()
	must(t, err)
	door, own, network := "canyon-recreate-e2e", "canyon-recreate-e2e-own", "canyon-app-recreate-e2e"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "recreate-e2e-preview", "recreate-e2e-preview-previous", door).Run()
		rmHolders("recreate-e2e")
		_ = exec.Command("docker", "network", "rm", network, own).Run()
		_ = exec.Command("docker", "image", "rm", "canyon/recreate-e2e:e2e").Run()
	})
	if out, err := exec.Command("docker", "network", "create", own).CombinedOutput(); err != nil {
		t.Fatalf("docker network create: %v: %s", err, out)
	}
	must(t, exec.Command("docker", "run", "-d", "--name", door, "--network", own, busybox, "httpd", "-f", "-p", "3128").Run())
	e := &Executor{Pool: netip.MustParsePrefix("10.254.253.0/24"), Join: []string{}, Door: []string{door}, Settle: time.Second}
	plain := RunRequest{App: "recreate-e2e", Target: "preview", Image: "canyon/recreate-e2e@" + strings.TrimSpace(string(id)), Network: network, Env: map[string]string{},
		Identity: Identity{JWKSURL: "http://canyon-server:7780/.well-known/jwks.json"}}
	routed := plain
	routed.Run, routed.Egress = "gr_e2e1", &Egress{Proxy: "http://" + door + ":3128", NoProxy: []string{}}
	if _, err := e.Run(ctx, plain); err != nil {
		if strings.Contains(err.Error(), "no free /24") {
			t.Skipf("this daemon already holds the test pool 10.254.253.0/24: %v", err)
		}
		t.Fatal(err)
	}
	inspect := func(format, of string) string {
		out, err := exec.Command("docker", "inspect", "--format", format, of).Output()
		must(t, err)
		return strings.TrimSpace(string(out))
	}
	dial := func() bool { // a raw dial to a public address from the app's namespace
		err := exec.Command("docker", "run", "--rm", "--network", "container:recreate-e2e-preview", busybox, "nc", "-z", "-w", "5", "1.1.1.1", "80").Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return err == nil
	}
	if !dial() {
		t.Fatal("1.1.1.1:80 was not reached from the app on its open network: the refusal below proves nothing")
	}
	if _, err := e.Run(ctx, routed); !errors.Is(err, ErrOpen) {
		t.Fatalf("a /run with egress on the open network: %v, want ErrOpen: a /run never changes a network", err)
	}
	must(t, e.RecreateNetwork(ctx, "recreate-e2e", true))
	if got := inspect("{{.State.Running}}", "recreate-e2e-preview"); got != "false" {
		t.Fatalf("after the recreation the app is running %s, want stopped: nothing is started", got)
	}
	if got := inspect("{{.Internal}} {{range .IPAM.Config}}{{.Subnet}} {{.IPRange}}{{end}} {{range .Containers}}{{.Name}}{{end}}", network); got != "true 10.254.253.0/24 10.254.253.128/25 "+door {
		t.Fatalf("%s: %q, want internal on the same /24, the door on it", network, got)
	}
	if _, err := e.Run(ctx, routed); err != nil {
		t.Fatalf("a /run with egress on the recreated network: %v", err)
	}
	if env := inspect(`{{join .Config.Env " "}}`, "recreate-e2e-preview"); !strings.Contains(env, "HTTPS_PROXY=http://"+door+":3128 ") || dial() {
		t.Fatalf("the app on the internal network: environment %q; or it dialled out", env)
	}
	must(t, e.RecreateNetwork(ctx, "recreate-e2e", false))
	if got := inspect("{{.State.Running}}", "recreate-e2e-preview") + " " + inspect("{{.Internal}} {{range .IPAM.Config}}{{.Subnet}}{{end}} {{len .Containers}}", network); got != "false false 10.254.253.0/24 0" {
		t.Fatalf("after the recreation open: %q, want the app stopped, the network open on the same /24, the door off it", got)
	}
	if _, err := e.Run(ctx, plain); err != nil {
		t.Fatalf("a /run without egress on the network recreated open: %v", err)
	}
	if !dial() {
		t.Fatal("the app on the network recreated open does not reach out directly")
	}
}

// TestRunE2ERestartsAnAppThatExitsOnceUp runs stand-in apps on the real daemon (#1241): one whose
// first start stays up through the settle window and then exits 1 is started again, and POST
// /running names it restarting, once, while it runs — never running, until its next deploy (#1722); one
// that exits inside the window is a failed start, though docker started it again.
func TestRunE2ERestartsAnAppThatExitsOnceUp(t *testing.T) {
	ctx := context.Background()
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	image := func(tag, script string) string {
		build := exec.Command("docker", "build", "-q", "-t", "canyon/restart-e2e:"+tag, "-")
		build.Stdin = strings.NewReader("FROM " + busybox + "\nCMD [\"sh\", \"-c\", " + strconv.Quote(script) + "]\n")
		id, err := build.Output()
		must(t, err)
		return "canyon/restart-e2e@" + strings.TrimSpace(string(id))
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "restart-e2e-preview", "restart-e2e-prod").Run()
		rmHolders("restart-e2e")
		_ = exec.Command("docker", "network", "rm", "canyon-app-restart-e2e").Run()
		_ = exec.Command("docker", "image", "rm", "canyon/restart-e2e:once", "canyon/restart-e2e:at-once").Run()
	})
	e := &Executor{Settle: time.Second, Join: []string{}} // nothing of the stack to join on a bare daemon
	run := func(target, image string) error {
		_, err := e.Run(ctx, RunRequest{App: "restart-e2e", Target: target, Image: image, Network: "canyon-app-restart-e2e", Env: map[string]string{}})
		return err
	}
	// the container keeps its filesystem across a restart: the second start finds the mark and stays up
	must(t, run("preview", image("once", "httpd -p 3000; [ -e /tmp/ran ] && exec sleep 600; touch /tmp/ran; sleep 2; exit 1")))
	preview := []Container{{App: "restart-e2e", Target: "preview"}}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(250 * time.Millisecond) {
		out, _ := exec.Command("docker", "container", "inspect", "--format", "{{.State.Status}} {{.RestartCount}}", "restart-e2e-preview").Output()
		running, err := e.RunningOf(ctx, preview, false)
		if strings.TrimSpace(string(out)) == "running 1" && err == nil && len(running.Running) == 0 && slices.Equal(running.Restarting, []Restarting{{App: "restart-e2e", Target: "preview", Restarts: 1}}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("an app that exited once up: %q, running %v (%v); want it started again and named restarting, once", out, running, err)
		}
	}
	var exited *Exited
	if err := run("prod", image("at-once", "httpd -p 3000; sleep 0.3; exit 1")); !errors.As(err, &exited) {
		t.Fatalf("an app that exits inside the settle window: %v, want a failed start", err)
	}
}

// rmHolders removes every floor holder of app's a test left, so its network can go.
func rmHolders(app string) {
	out, _ := exec.Command("docker", "container", "ls", "--all", "--quiet", "--filter", "label=canyon.holder", "--filter", "label=canyon.app="+app).Output()
	if ids := strings.Fields(string(out)); len(ids) > 0 {
		_ = exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
	}
}

// TestAppE2EReachesNoAddressOfTheHostsAndComesBackFloored runs a stand-in app on an open network on the real
// daemon (contracts/executor.md, An app reaches no private, link-local or host address; #2503): a service on
// the host's every address, which a container on an open network without the floor reaches through its
// gateway and through host-gateway — the address host.docker.internal names, which Docker Desktop, OrbStack and
// Colima forward to the Mac's own loopback — and the host's private address, a link-local stand-in and the
// metadata address are all refused — the host's private address on 81 too, which CANYON_APPS_ALLOW names: no entry
// opens the host — while a public address and the app's own network are reached. Then what a daemon's restart leaves — the holder stopped, docker's own start of the app
// refused for want of it — and a holder started again under the running app each come back, by the
// executor's reconcile, floored and answering.
func TestAppE2EReachesNoAddressOfTheHostsAndComesBackFloored(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	docker := func(args ...string) string {
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput() //nolint:gosec // the test's own arguments
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	conn, err := net.Dial("udp", "1.1.1.1:53") // no packet: the address this host's route out leaves from
	must(t, err)
	private, _ := netip.ParseAddrPort(conn.LocalAddr().String())
	_ = conn.Close()
	if !private.Addr().Is4() || !private.Addr().IsPrivate() {
		t.Fatalf("this host leaves from %s, not a private IPv4 address: the private stand-in would prove nothing", private.Addr())
	}
	// a link-local address, and a public one (TEST-NET-2's) as a server with a public IP on its interface has
	const linkLocal, public = "169.254.42.2", "198.51.100.8"
	app, network := "floor-e2e", "canyon-app-floor-e2e"
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", "floor-e2e-preview", "floor-e2e-preview-previous", "floor-e2e-standin").Run()
		rmHolders(app)
		_ = exec.Command("docker", "network", "rm", network, "floor-e2e-open").Run()
		_ = exec.Command("docker", "image", "rm", "canyon/floor-e2e:e2e").Run()
		for _, a := range []string{linkLocal, public} {
			_ = exec.Command("docker", "run", "--rm", "--network", "host", "--cap-add", "NET_ADMIN", busybox, "ip", "addr", "del", a+"/32", "dev", "lo").Run()
		}
	})
	// the host's private address on 80 and 81, a link-local one and a public one on 80: natively run services, a cloud's metadata
	docker("run", "-d", "--name", "floor-e2e-standin", "--network", "host", "--cap-add", "NET_ADMIN", busybox, "sh", "-c",
		"ip addr add "+linkLocal+"/32 dev lo; ip addr add "+public+"/32 dev lo; httpd -p "+linkLocal+":80 -h /etc && httpd -p "+public+":80 -h /etc && "+
			"httpd -p "+private.Addr().String()+":81 -h /etc && exec httpd -f -p "+private.Addr().String()+":80 -h /etc")
	// a service on the host's every address, as canyon-server's published port is to the Mac's forward
	ln, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // the host's wildcard address is what this probes
	must(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	hostGateway := docker("run", "--rm", "--add-host", "host.docker.internal:host-gateway", busybox, "awk", `$2 == "host.docker.internal" { print $1; exit }`, "/etc/hosts")
	reached := func(from, target string) bool { // nc's 1 is not reached; anything else is docker's
		host, p, _ := net.SplitHostPort(target)
		err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", from, busybox, "nc", "-z", "-w", "3", host, p).Run() //nolint:gosec // the test's own targets
		var exit *exec.ExitError
		if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != 1) {
			t.Fatalf("nc from %s to %s: %v", from, target, err)
		}
		return err == nil
	}
	build := exec.Command("docker", "build", "-q", "-t", "canyon/floor-e2e:e2e", "-")
	build.Stdin = strings.NewReader("FROM " + busybox + "\nCMD [\"httpd\", \"-f\", \"-p\", \"3000\"]\n")
	id, err := build.Output()
	must(t, err)
	// every stand-in answers a container on an open network without the floor, as an app's was, before the app
	// starts: its holder takes the host's addresses as it starts, the public stand-in's among them
	standins := []string{hostGateway + ":" + port, private.Addr().String() + ":80", private.Addr().String() + ":81", linkLocal + ":80", public + ":80"}
	docker("network", "create", "--subnet", "10.254.246.0/24", "floor-e2e-open")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		all := true
		for _, target := range append([]string{"10.254.246.1:" + port}, standins...) {
			all = all && reached("floor-e2e-open", target)
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the host's services answer no container on an open network without the floor: refusing them would prove nothing")
		}
	}
	e := &Executor{Pool: netip.MustParsePrefix("10.254.248.0/24"), Join: []string{}, Settle: time.Second, AppsAllow: []string{private.Addr().String() + "/32:81"}}
	if _, err := e.Run(ctx, RunRequest{App: app, Target: "preview", Image: "canyon/floor-e2e@" + strings.TrimSpace(string(id)), Network: network, Env: map[string]string{}}); err != nil {
		if strings.Contains(err.Error(), "no free /24") {
			t.Skipf("this daemon already holds the test pool 10.254.248.0/24: %v", err)
		}
		t.Fatal(err)
	}
	gateway := docker("network", "inspect", "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}", network)
	host := append([]string{gateway + ":" + port}, standins...)
	floored := func(when string) {
		t.Helper()
		for _, target := range append(host, "169.254.169.254:80") {
			if reached("container:floor-e2e-preview", target) {
				t.Errorf("%s: the app reached %s", when, target)
			}
		}
		// a lookup of a public name answers: Docker's resolver, on the namespace's own loopback, forwards from the host
		if out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "container:floor-e2e-preview", busybox, "nslookup", "example.com").CombinedOutput(); err != nil {
			t.Errorf("%s: the app could not resolve example.com: %v %s", when, err, out)
		}
		for _, target := range []string{"1.1.1.1:80", "floor-e2e-preview:3000"} {
			if !reached("container:floor-e2e-preview", target) {
				t.Errorf("%s: the app did not reach %s: a public address and its own name are its", when, target)
			}
		}
		if !reached(network, "floor-e2e-preview:3000") {
			t.Errorf("%s: the app does not answer on its network at its name", when)
		}
	}
	floored("after /run")
	holder := docker("inspect", "--format", `{{index .Config.Labels "canyon.floor"}}`, "floor-e2e-preview")
	if os.Getenv("GITHUB_ACTIONS") == "true" { // a real restart of the daemon, on CI's throwaway runner alone
		if out, err := exec.CommandContext(ctx, "sudo", "systemctl", "restart", "docker").CombinedOutput(); err != nil {
			t.Fatalf("sudo systemctl restart docker: %v %s", err, out)
		}
		for deadline := time.Now().Add(90 * time.Second); exec.CommandContext(ctx, "docker", "info").Run() != nil; time.Sleep(time.Second) {
			if time.Now().After(deadline) {
				t.Fatal("the daemon did not answer within 90 s of its restart")
			}
		}
		state := func(c string) string {
			return docker("inspect", "--format", "{{.State.Running}} {{.State.ExitCode}} {{.State.Error}}", c)
		}
		h, a := state(holder), state("floor-e2e-preview")
		t.Logf("after the daemon's restart, before the reconcile: holder %q, app %q", h, a)
		if strings.HasPrefix(h, "true") {
			t.Fatalf("the daemon started the holder again (%q): it runs --restart no", h)
		}
		if strings.HasPrefix(a, "true") {
			t.Fatalf("the app runs with its holder down (%q): unfloored", a)
		}
		docker("start", "floor-e2e-standin") // the restart stopped the stand-ins too; their addresses stay on lo
		for deadline := time.Now().Add(30 * time.Second); !reached("floor-e2e-open", public+":80"); time.Sleep(time.Second) {
			if time.Now().After(deadline) {
				t.Fatal("the stand-ins did not answer again after the daemon's restart")
			}
		}
	} else { // what a restart leaves, by hand: the holder stopped, and docker's own start of the app refused for want of it
		docker("stop", "-t", "0", holder)
		docker("stop", "-t", "0", "floor-e2e-preview")
		if out, err := exec.CommandContext(ctx, "docker", "start", "floor-e2e-preview").CombinedOutput(); err == nil {
			t.Fatalf("the app started with its holder down: %s", out)
		}
	}
	e.Reconcile(ctx)
	if got := docker("inspect", "--format", "{{.State.Running}}", holder, "floor-e2e-preview"); got != "true\ntrue" {
		t.Fatalf("after the reconcile the holder and the app run: %q, want both", got)
	}
	floored("after a daemon's restart")
	// a holder started again under the running app, which keeps the namespace its stop emptied
	docker("restart", "-t", "0", holder)
	e.Reconcile(ctx)
	floored("after its holder started again")
}

// busybox stands in for a tool on the real daemon, pinned as one is: busybox 1.37.0's multi-arch index.
const busybox = "busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// scanE2E serves busybox running script as checkov; any canyon.scan container outliving it fails.
func scanE2E(t *testing.T, script string, deadline time.Duration) string {
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	pinTool(t, busybox, []string{"sh", "-c", script}, deadline)
	srv := httptest.NewServer(Handler(&Executor{}))
	t.Cleanup(func() {
		srv.Close()
		if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
			t.Errorf("containers labelled canyon.scan outlived the scan: %s", out)
		}
	})
	return srv.URL
}

// Invariant 7: no network, a read-only root fs, nobody reads the copied tree, the volume goes with it.
func TestScanE2EReachesNoNetworkAndLeavesNothing(t *testing.T) {
	url := scanE2E(t, `sleep 2; printf '{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"%s"}}}]}' "$(cat /scan/tree/src/hello.txt)"`, time.Minute)
	seen := make(chan []string, 1)
	go func() { // the container, inspected while it runs
		for range 100 {
			if id, _ := exec.Command("docker", "ps", "-q", "--filter", "label=canyon.scan").Output(); len(id) > 0 {
				out, _ := exec.Command("docker", "inspect", "--format", "{{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}} {{.Config.User}} {{range .Mounts}}{{.Name}}{{end}}", strings.TrimSpace(string(id))).Output()
				seen <- strings.Fields(string(out))
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		seen <- nil
	}()
	status, body := postScan(t, url, checkovReq, tarOf(t, map[string]string{"src/hello.txt": "hello"}))
	got := <-seen
	if len(got) != 4 || got[0] != "none" || got[1] != "true" || got[2] != "65534:65534" {
		t.Fatalf("network mode, read-only root fs, user, volume: %q", got)
	}
	if status != 200 || !strings.Contains(body, `"name":"hello"`) {
		t.Fatalf("the tool's SARIF, naming the tree's file: %d %s", status, body)
	}
	if exec.Command("docker", "volume", "inspect", got[3]).Run() == nil {
		t.Fatalf("the scan's volume %s outlived it", got[3])
	}
}

func TestScanE2ERefusesASymlinkAndRemovesTheContainerPastItsCapAndDeadline(t *testing.T) {
	tree := tarOf(t, map[string]string{"a": "x"})
	for _, tc := range []struct {
		name, script string
		tree         []byte
		want         string
	}{
		{"a symlink", "cat /scan/tree/link", headersTar(t, tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}), `{"error":"invalid_request"`},
		{"over_cap", overCapSARIF, tree, `"not_finished":"over_cap"`},
		{"timeout", "sleep 60", tree, `"not_finished":"timeout"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := scanE2E(t, tc.script, 5*time.Second)
			start := time.Now()
			if _, body := postScan(t, url, checkovReq, tc.tree); !strings.Contains(body, tc.want) || time.Since(start) > 30*time.Second {
				t.Fatalf("%s after %s", body, time.Since(start))
			}
		})
	}
}

// TestSweepE2ERemovesWhatACrashLeftBehind leaves a container and its anonymous volume labelled
// canyon.scan, exactly as a scan a crash interrupted would, and asserts Sweep leaves none of
// either (contracts/executor.md invariant 8).
func TestSweepE2ERemovesWhatACrashLeftBehind(t *testing.T) {
	must(t, exec.Command("docker", "pull", "-q", busybox).Run())
	name := "canyon-scan-e2e-" + nonce()
	must(t, exec.Command("docker", "create", "--name", name, "--label", "canyon.scan", "--volume", "/scan", busybox).Run())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", name).Run() })
	vol, err := exec.Command("docker", "inspect", "--format", "{{range .Mounts}}{{.Name}}{{end}}", name).Output()
	must(t, err)
	volume := strings.TrimSpace(string(vol))
	holder := "canyon-build-e2e" + nonce() // a build's holder its build's crash left, still sleeping
	must(t, exec.Command("docker", "run", "-d", "--name", holder, "--label", "canyon.build", busybox, "sleep", "600").Run())
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", holder).Run() })
	e := &Executor{}
	e.Sweep(context.Background())
	if exec.Command("docker", "container", "inspect", name).Run() == nil {
		t.Errorf("%s (label canyon.scan) outlived Sweep", name)
	}
	if exec.Command("docker", "container", "inspect", holder).Run() == nil {
		t.Errorf("%s (label canyon.build) outlived Sweep", holder)
	}
	if volume != "" && exec.Command("docker", "volume", "inspect", volume).Run() == nil {
		t.Errorf("%s's volume %s outlived Sweep", name, volume)
	}
}

// TestScanE2EAnswersKilledOnOOM lowers the scan's memory limit so a tool that fills tmpfs past
// it is OOM-killed quickly, and asserts /scan answers not_finished killed, never not_sarif
// (canyonv2-ula5.23).
func TestScanE2EAnswersKilledOnOOM(t *testing.T) {
	old := scanMemory
	scanMemory = "20m"
	t.Cleanup(func() { scanMemory = old })
	url := scanE2E(t, "dd if=/dev/zero of=/tmp/big bs=1M count=200 2>/dev/null", time.Minute)
	status, body := postScan(t, url, checkovReq, tarOf(t, map[string]string{"a": "x"}))
	if status != 200 || !strings.Contains(body, `"not_finished":"killed"`) || strings.Contains(body, `"sarif":`) {
		t.Fatalf("OOM past a 20m limit: %d %s", status, body)
	}
}

// trivyLock pins lodash 4.17.20, which CVE-2021-23337 (command injection, fixed in 4.17.21) affects.
const trivyLock = `{"name":"fixture","lockfileVersion":3,"requires":true,"packages":{"":{"name":"fixture","dependencies":{"lodash":"4.17.20"}},"node_modules/lodash":{"version":"4.17.20"}}}`

// TestScanE2ETrivyReportsTheSameWhateverSuppressionsTheTreeCarries needs network once, to refresh
// trivy's database (1.4 GB), then scans a vulnerable lockfile, a privileged pod (KSV-0017), a
// Terraform security group open to the world and a Dockerfile, plain and planted: a .trivyignore, a
// trivy.yaml turning vuln and misconfig off, a trivy:ignore:* above each misconfiguration. The tree
// never steers its scan (invariant 9): both answer the same rules, the CVE and the pod among them. Each
// scan mounts the database volume, and neither leaves it changed by one byte, mode or mtime.
func TestScanE2ETrivyReportsTheSameWhateverSuppressionsTheTreeCarries(t *testing.T) {
	e := &Executor{}
	start := time.Now()
	must(t, e.refreshDB(context.Background(), "trivy", scanTools["trivy"]))
	t.Logf("trivy's database refreshed in %s", time.Since(start).Round(time.Second))
	db := volumeState(t, "canyon-scandb-trivy")
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	pod := "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n    - name: c\n      image: nginx:1.25\n      securityContext: {privileged: true}\n"
	tf := "resource \"aws_security_group\" \"sg\" {\n  ingress {\n    from_port   = 22\n    to_port     = 22\n    protocol    = \"tcp\"\n    cidr_blocks = [\"0.0.0.0/0\"]\n  }\n}\n"
	plain := map[string]string{"package-lock.json": trivyLock, "pod.yaml": pod, "main.tf": tf, "Dockerfile": "FROM alpine:3.10\nRUN apk add curl\n"}
	planted := map[string]string{"package-lock.json": trivyLock, ".trivyignore": "CVE-2021-23337\nKSV-0017\n", "trivy.yaml": "scan:\n  scanners: [secret]\n",
		"pod.yaml": strings.Replace(pod, "    - name", "    # trivy:ignore:*\n    - name", 1), "main.tf": "#trivy:ignore:*\n" + tf, "Dockerfile": "FROM alpine:3.10\n# trivy:ignore:*\nRUN apk add curl\n"}
	var rules [2]string
	for i, tree := range []map[string]string{plain, planted} {
		start := time.Now()
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"trivy","settings":{"scanners":["vuln","misconfig","secret"]}}`, tarOf(t, tree))
		t.Logf("scan %d answered in %s", i+1, time.Since(start).Round(10*time.Millisecond))
		var res struct {
			SARIF struct {
				Runs []struct{ Results []struct{ RuleID string } }
			}
		}
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || !strings.Contains(body, `"db_built_at":"`) || len(res.SARIF.Runs) != 1 {
			t.Fatalf("SARIF on a database with its build time: %d %.600s", status, body)
		}
		ids := map[string]bool{}
		for _, r := range res.SARIF.Runs[0].Results {
			ids[r.RuleID] = true
		}
		rules[i] = strings.Join(slices.Sorted(maps.Keys(ids)), " ")
	}
	if rules[0] != rules[1] || !strings.Contains(rules[0], "CVE-2021-23337") || !strings.Contains(rules[0], "KSV-0017") {
		t.Errorf("the plain tree and the planted one, the same rules, the CVE and the pod among them:\n%s\n%s", rules[0], rules[1])
	}
	if after := volumeState(t, "canyon-scandb-trivy"); after != db {
		t.Errorf("the scans changed canyon-scandb-trivy:\n%s--- after:\n%s", db, after)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
		t.Errorf("containers labelled canyon.scan outlived the scans: %s", out)
	}
}

// TestScanE2ETrivyPackagesListsWhatTheFilesNameOffline lists a yarn.lock's packages, direct and not, and
// a pom.xml whose parent is Spring Boot's: offline, trivy fetches no parent, so the dependency whose version
// only the parent gives is listed with none, never a version found elsewhere. No database is read or made.
func TestScanE2ETrivyPackagesListsWhatTheFilesNameOffline(t *testing.T) {
	srv := httptest.NewServer(Handler(&Executor{}))
	defer srv.Close()
	yarn := "# yarn lockfile v1\n\n\ndebug@2.6.9:\n  version \"2.6.9\"\n\nexpress@^4.21.2:\n  version \"4.21.2\"\n  dependencies:\n    debug \"2.6.9\"\n"
	pom := `<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion>` +
		`<parent><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-parent</artifactId><version>3.4.1</version></parent>` +
		`<artifactId>svc</artifactId><dependencies><dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-web</artifactId></dependency></dependencies></project>`
	volumes, _ := exec.Command("docker", "volume", "ls", "--quiet").Output()
	status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"trivy","target":"packages"}`,
		tarOf(t, map[string]string{"package.json": `{"name":"web","dependencies":{"express":"^4.21.2"}}`, "yarn.lock": yarn, "svc/pom.xml": pom}))
	var res struct {
		DBBuiltAt string `json:"db_built_at"`
		Packages  struct {
			Results []struct {
				Target   string
				Packages []struct{ Name, Version, Relationship string }
			}
		}
	}
	if status != 200 || json.Unmarshal([]byte(body), &res) != nil || res.DBBuiltAt != "" {
		t.Fatalf("%d %.600s", status, body)
	}
	got := map[string]string{}
	for _, r := range res.Packages.Results {
		for _, p := range r.Packages {
			got[r.Target+" "+p.Name] = p.Version + " " + p.Relationship
		}
	}
	for k, want := range map[string]string{"yarn.lock express": "4.21.2 direct", "yarn.lock debug": "2.6.9 indirect",
		"svc/pom.xml org.springframework.boot:spring-boot-starter-web": " direct"} {
		if got[k] != want {
			t.Errorf("%s: %q, want %q (all: %v)", k, got[k], want, got)
		}
	}
	if after, _ := exec.Command("docker", "volume", "ls", "--quiet").Output(); strings.Count(string(after), "canyon-scandb-") != strings.Count(string(volumes), "canyon-scandb-") {
		t.Errorf("a packages scan made a database volume:\n%s", after)
	}
}

// TestScanE2EGrypeReportsTheSameWhateverSuppressionsTheTreeCarries needs network once, to refresh grype's
// database (3 GB), then scans a vulnerable lockfile, a log4j-core 2.14.1 jar (read through /tmp, which must
// be the scan's user's), docs/ and a*b/ (skipped: a path, never a glob, so axb/ is not), plain and planted: .grype.yaml and .grype/config.yaml ignoring
// both packages, excluding both files and dropping every fixed vulnerability, the same as .syft.yaml and
// .grypeignore, and an OpenVEX document calling lodash not affected. The tree never steers its scan
// (invariant 9): both answer the same results, at the same levels and the package's paths, lodash's
// command injection and Log4Shell among them; the database volume is unchanged by either scan.
func TestScanE2EGrypeReportsTheSameWhateverSuppressionsTheTreeCarries(t *testing.T) {
	e := &Executor{}
	start := time.Now()
	must(t, e.refreshDB(context.Background(), "grype", scanTools["grype"]))
	t.Logf("grype's database refreshed in %s", time.Since(start).Round(time.Second))
	db := volumeState(t, "canyon-scandb-grype")
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	var jar bytes.Buffer
	zw := zip.NewWriter(&jar)
	for name, body := range map[string]string{"META-INF/MANIFEST.MF": "Manifest-Version: 1.0\n",
		"META-INF/maven/org.apache.logging.log4j/log4j-core/pom.properties": "groupId=org.apache.logging.log4j\nartifactId=log4j-core\nversion=2.14.1\n"} {
		f, err := zw.Create(name)
		must(t, err)
		must(t, write(f, []byte(body)))
	}
	must(t, zw.Close())
	steer := "ignore:\n  - package: {name: lodash}\n  - package: {name: log4j-core}\nexclude: [./package-lock.json, ./lib]\nonly-notfixed: true\n"
	vex := `{"@context":"https://openvex.dev/ns/v0.2.0","@id":"https://example.com/vex","author":"planted","timestamp":"2026-09-25T00:00:00Z","version":1,` +
		`"statements":[{"vulnerability":{"name":"GHSA-35jh-r3h4-6jhm"},"products":[{"@id":"pkg:npm/lodash@4.17.20"}],"status":"not_affected","justification":"vulnerable_code_not_present"}]}`
	plain := map[string]string{"package-lock.json": trivyLock, "lib/log4j-core-2.14.1.jar": jar.String(), "docs/package-lock.json": trivyLock, "a*b/package-lock.json": trivyLock, "axb/package-lock.json": trivyLock}
	planted := maps.Clone(plain)
	maps.Copy(planted, map[string]string{".grype.yaml": steer, ".grype/config.yaml": steer, ".syft.yaml": steer, ".grypeignore": "GHSA-35jh-r3h4-6jhm\nlodash\nlog4j-core\n",
		"openvex.json": vex, "vex.json": vex, ".openvex.json": vex})
	var found [2]string
	for i, tree := range []map[string]string{plain, planted} {
		start := time.Now()
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"grype","settings":{"skip_paths":["docs","a*b"]}}`, tarOf(t, tree))
		t.Logf("scan %d answered in %s", i+1, time.Since(start).Round(10*time.Millisecond))
		var res struct {
			SARIF struct {
				Runs []struct {
					Results []struct {
						RuleID    string
						Level     string
						Locations []struct {
							PhysicalLocation struct{ ArtifactLocation struct{ URI string } }
						}
					}
				}
			}
		}
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || !strings.Contains(body, `"db_built_at":"`) || len(res.SARIF.Runs) != 1 {
			t.Fatalf("SARIF on a database with its build time: %d %.600s", status, body)
		}
		var rs []string
		for _, r := range res.SARIF.Runs[0].Results {
			for _, l := range r.Locations {
				rs = append(rs, l.PhysicalLocation.ArtifactLocation.URI+" "+r.RuleID+" "+r.Level)
			}
		}
		found[i] = strings.Join(slices.Sorted(slices.Values(rs)), "\n")
	}
	for _, want := range []string{"package-lock.json GHSA-35jh-r3h4-6jhm-lodash error", "lib/log4j-core-2.14.1.jar GHSA-jfh8-c2jp-5v3q-log4j-core error", "axb/package-lock.json GHSA-35jh-r3h4-6jhm-lodash error"} {
		if !strings.Contains("\n"+found[0]+"\n", "\n"+want+"\n") {
			t.Errorf("the plain tree's results lack %q:\n%s", want, found[0])
		}
	}
	if strings.Contains(found[0], "docs/") || strings.Contains(found[0], "a*b/") || found[0] != found[1] {
		t.Errorf("the plain tree and the planted one, the same results, docs/ and a*b/ skipped:\n%s\n--- planted:\n%s", found[0], found[1])
	}
	if after := volumeState(t, "canyon-scandb-grype"); after != db {
		t.Errorf("the scans changed canyon-scandb-grype:\n%s--- after:\n%s", db, after)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
		t.Errorf("containers labelled canyon.scan outlived the scans: %s", out)
	}
}

// volumeState is every path in the volume with its owner, mode, size and mtime, then every file's sha256:
// stdout alone, so a first run's pull of busybox is not part of it.
func volumeState(t *testing.T, volume string) string {
	var stderr bytes.Buffer
	cmd := exec.Command("docker", "run", "--rm", "--network", "none", "--volume", volume+":/v:ro", busybox, "sh", "-c",
		`cd /v && find . -exec stat -c "%n %u:%g %a %s %y" {} + | sort && find . -type f -exec sha256sum {} + | sort`)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the state of %s: %v: %s", volume, err, stderr.String())
	}
	return string(out)
}

// TestScanE2ECheckovReportsTheSameWhateverSuppressionsTheTreeCarries scans a Terraform security group
// open to the world, directly and through a module, a CloudFormation bucket and a Dockerfile, plain and
// planted: checkov:skip comments in the resource (before and after its attribute), in the module block
// and in the Dockerfile, a Metadata skip on the bucket, a .checkov.yaml skipping checks and a path, a
// .checkov.yml running one check only. The tree never steers its scan (invariant 9): both answer the
// same results, at the tree's paths, each at the level the attach reads — its own, else its rule's.
func TestScanE2ECheckovReportsTheSameWhateverSuppressionsTheTreeCarries(t *testing.T) {
	must(t, exec.Command("docker", "pull", "-q", checkovImage).Run())
	srv := httptest.NewServer(Handler(&Executor{}))
	defer srv.Close()
	tf := "resource \"aws_security_group\" \"sg\" {\n  ingress {\n    from_port   = 22\n    to_port     = 22\n    protocol    = \"tcp\"\n    cidr_blocks = [\"0.0.0.0/0\"]\n  }\n}\n"
	cfn := "AWSTemplateFormatVersion: \"2010-09-09\"\nResources:\n  Bucket:\n    Type: AWS::S3::Bucket\n    Properties:\n      BucketName: fixture\n"
	module := "module \"m\" {\n  source = \"./mod\"\n}\n"
	plain := map[string]string{"main.tf": tf + module, "mod/main.tf": tf, "template.yaml": cfn, "Dockerfile": "FROM alpine:3.10\nRUN apk add curl\n"}
	planted := map[string]string{
		"main.tf": strings.Replace(tf, "  ingress", "  #checkov:skip=CKV_AWS_24:planted\n  #checkov:skip=CKV_AWS_23:planted\n  ingress", 1) + "  #checkov:skip=CKV2_AWS_5:planted\n" +
			strings.Replace(module, "  source", "  #checkov:skip=CKV_AWS_24:planted\n  source", 1),
		"mod/main.tf":   tf,
		"template.yaml": strings.Replace(cfn, "    Properties", "    Metadata:\n      checkov:\n        skip:\n          - id: CKV_AWS_18\n          - id: CKV_AWS_21\n    Properties", 1),
		"Dockerfile":    "FROM alpine:3.10\n#checkov:skip=CKV_DOCKER_2:planted\n#checkov:skip=CKV_DOCKER_3:planted\nRUN apk add curl\n",
		".checkov.yaml": "skip-check: [CKV_AWS_24, CKV_DOCKER_2, CKV_DOCKER_3]\nskip-path: [template.yaml]\n",
		".checkov.yml":  "check: [CKV_AWS_23]\n",
	}
	var found [2]string
	for i, tree := range []map[string]string{plain, planted} {
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"checkov","settings":{}}`, tarOf(t, tree))
		var res struct {
			SARIF struct {
				Runs []struct {
					Tool struct {
						Driver struct {
							Rules []struct {
								ID                   string
								DefaultConfiguration struct{ Level string }
							}
						}
					}
					Results []struct {
						RuleID    string
						Level     string
						Locations []struct {
							PhysicalLocation struct{ ArtifactLocation struct{ URI string } }
						}
					}
				}
			}
		}
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.SARIF.Runs) != 1 || strings.Contains(body, "suppressions") || strings.Contains(body, `"db_built_at"`) {
			t.Fatalf("SARIF, suppressions removed: %d %.600s", status, body)
		}
		run, defaults, rs := res.SARIF.Runs[0], map[string]string{}, []string(nil)
		for _, rule := range run.Tool.Driver.Rules {
			defaults[rule.ID] = rule.DefaultConfiguration.Level
		}
		for _, r := range run.Results {
			for _, l := range r.Locations {
				rs = append(rs, l.PhysicalLocation.ArtifactLocation.URI+" "+r.RuleID+" "+cmp.Or(r.Level, defaults[r.RuleID]))
			}
		}
		found[i] = strings.Join(slices.Sorted(slices.Values(rs)), "\n")
	}
	for _, want := range []string{"main.tf CKV_AWS_24 error", "mod/main.tf CKV_AWS_24 error", "template.yaml CKV_AWS_18 error", "Dockerfile CKV_DOCKER_3 error"} {
		if !strings.Contains("\n"+found[0]+"\n", "\n"+want+"\n") { // a whole line: the uri is the tree's path
			t.Errorf("the plain tree's results lack %q:\n%s", want, found[0])
		}
	}
	if found[0] != found[1] {
		t.Errorf("the plain tree and the planted one, the same results:\n%s\n--- planted:\n%s", found[0], found[1])
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
		t.Errorf("containers labelled canyon.scan outlived the scans: %s", out)
	}
}

// TestScanE2EOpengrepReportsTheSameWhateverSuppressionsTheTreeCarries builds opengrep's image (network,
// once) and runs the vendored sast-rules — UTF-8 text among them, which the image's locale must read —
// over a page that renders request data as raw HTML, and the same
// page under docs/ (a skip path), plain, planted — a // nosemgrep above the line and a .semgrepignore
// listing the page — and padded past opengrep's default 1 MB file limit. The tree never steers its scan
// (invariant 9): all three answer the same results, at the package's paths and the same level, the XSS
// among them and docs/ skipped.
func TestScanE2EOpengrepReportsTheSameWhateverSuppressionsTheTreeCarries(t *testing.T) {
	bundle, err := opengrep.Rules()
	must(t, err)
	rules := map[string]string{}
	for name, b := range bundle {
		rules[name] = string(b)
	}
	page, err := os.ReadFile("../gate/opengrep/testdata/probes/tsx/page.tsx")
	must(t, err)
	srv := httptest.NewServer(Handler(&Executor{}))
	defer srv.Close()
	plain := map[string]string{"app/page.tsx": string(page), "docs/page.tsx": string(page)}
	planted := map[string]string{"app/page.tsx": strings.Replace(string(page), "  return <div", "  // nosemgrep\n  return <div", 1), "docs/page.tsx": string(page), ".semgrepignore": "app/page.tsx\n"}
	padded := map[string]string{"app/page.tsx": string(page) + "\n// " + strings.Repeat("x", 1<<20) + "\n", "docs/page.tsx": string(page)}
	var found [3]string
	for i, tree := range []map[string]string{plain, planted, padded} {
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"opengrep","settings":{"skip_paths":["docs"]}}`, tarOf(t, tree), tarOf(t, rules))
		var res struct {
			Image string
			SARIF struct {
				Runs []struct {
					Tool struct {
						Driver struct {
							Rules []struct {
								ID                   string
								DefaultConfiguration struct{ Level string }
							}
						}
					}
					Results []struct {
						RuleID    string
						Level     string
						Locations []struct {
							PhysicalLocation struct{ ArtifactLocation struct{ URI string } }
						}
					}
				}
			}
		}
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.SARIF.Runs) != 1 || !strings.HasPrefix(res.Image, "canyon-scan/opengrep@sha256:") || strings.Contains(body, "suppressions") {
			t.Fatalf("SARIF from the built image, suppressions removed: %d %.600s", status, body)
		}
		run, defaults, rs := res.SARIF.Runs[0], map[string]string{}, []string(nil)
		for _, rule := range run.Tool.Driver.Rules {
			defaults[rule.ID] = rule.DefaultConfiguration.Level
		}
		for _, r := range run.Results {
			for _, l := range r.Locations {
				rs = append(rs, l.PhysicalLocation.ArtifactLocation.URI+" "+r.RuleID+" "+cmp.Or(r.Level, defaults[r.RuleID]))
			}
		}
		found[i] = strings.Join(slices.Sorted(slices.Values(rs)), "\n")
	}
	if found[0] != "app/page.tsx javascript_react_rule-dangerouslysetinnerhtml warning" || found[1] != found[0] || found[2] != found[0] {
		t.Errorf("the plain tree, the planted one and the padded one, the XSS at the package's path and its rule's level, docs/ skipped:\n%s\n--- planted:\n%s\n--- padded:\n%s", found[0], found[1], found[2])
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
		t.Errorf("containers labelled canyon.scan outlived the scans: %s", out)
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// treeOf tars the fixture app at root as a /build tree, without what an install or a build there left.
func treeOf(t *testing.T, root string) *bytes.Buffer {
	t.Helper()
	var tree bytes.Buffer
	tw := tar.NewWriter(&tree)
	must(t, filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == root {
			return err
		}
		if info.IsDir() && slices.Contains([]string{"node_modules", ".next", "dist"}, info.Name()) {
			return filepath.SkipDir // SkipDir from a file would skip the rest of its directory
		}
		rel, _ := filepath.Rel(root, path)
		h, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		h.Name = rel
		if err := tw.WriteHeader(h); err != nil || info.IsDir() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	}))
	must(t, tw.Close())
	return &tree
}

// get asks app's running container for path from the app's network until it answers 200, for up to a
// minute: an app takes a moment to listen. It answers the body, or fails with the container's log.
func get(ctx context.Context, t *testing.T, app string, run RunResult, path string) string {
	t.Helper()
	var code, body string
	var curlErr error
	for i := 0; i < 30 && code != "200"; i++ {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "canyon-app-"+app, "curlimages/curl:8.11.1", "-s", "-w", "\n%{http_code}", "http://"+run.Addr+path).Output()
		at := bytes.LastIndexByte(out, '\n')
		body, code, curlErr = string(out[:at+1]), string(out[at+1:]), err
	}
	if code != "200" {
		out, _ := exec.Command("docker", "logs", run.Container).CombinedOutput()
		t.Fatalf("GET %s: %q, want 200 (curl: %v)\n%s", path, code, curlErr, tail(string(out), 20))
	}
	return body
}

// ESLint, built from executor/eslint.Dockerfile, runs its four rules over the tree whatever the tree
// carries: its own eslint.config.js turning them off, and inline disable comments, change nothing;
// a skipped path is not read; each uri is the package's path (contracts/executor.md).
func TestScanE2EESLintRunsItsFourRulesWhateverTheTreeSays(t *testing.T) {
	src := "import { exec } from \"child_process\";\nimport { readFile } from \"fs/promises\";\n" +
		"export async function run(name: string, code: string) {\n  exec(\"convert \" + name);\n  await readFile(\"/data/\" + name);\n  // eslint-disable-next-line\n  eval(code);\n  return new Function(code);\n}\n"
	srv := httptest.NewServer(Handler(&Executor{}))
	defer srv.Close()
	plain := map[string]string{"src/run.ts": src, "docs/run.ts": src}
	planted := map[string]string{"src/run.ts": src, "docs/run.ts": src, "eslint.config.js": "export default [{ rules: {} }];\n",
		".eslintrc.json": `{"root": true, "rules": {}}`}
	// A package with no JavaScript or TypeScript is a scan with no result, never not_sarif.
	if status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"eslint","settings":{}}`, tarOf(t, map[string]string{"app.py": "print(1)\n"})); status != 200 || !strings.Contains(body, `"results":[]`) {
		t.Fatalf("a package with no JS: %d %.400s", status, body)
	}
	for name, tree := range map[string]map[string]string{"plain": plain, "planted": planted} {
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_e2e0","tool":"eslint","settings":{"skip_paths":["docs"]}}`, tarOf(t, tree))
		var res struct {
			Image string
			SARIF struct {
				Runs []struct {
					Results []struct {
						RuleID    string
						Locations []struct {
							PhysicalLocation struct{ ArtifactLocation struct{ URI string } }
						}
					}
				}
			}
		}
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || len(res.SARIF.Runs) != 1 || !strings.HasPrefix(res.Image, "canyon-scan/eslint@sha256:") {
			t.Fatalf("%s: SARIF from the built image: %d %.600s", name, status, body)
		}
		var got []string
		for _, r := range res.SARIF.Runs[0].Results {
			got = append(got, r.RuleID+" "+r.Locations[0].PhysicalLocation.ArtifactLocation.URI)
		}
		slices.Sort(got)
		if want := []string{"no-eval src/run.ts", "no-new-func src/run.ts", "security/detect-child-process src/run.ts", "security/detect-non-literal-fs-filename src/run.ts"}; !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// ubuntuNoble is ubuntu:noble-20240429's multi-arch index: its libssl3t64, 3.0.13-0ubuntu3, has fixes Ubuntu
// published since, which trivy's database keeps for good.
const ubuntuNoble = "docker.io/library/ubuntu@sha256:3f85b7caad41a95462cf5b787d8a04604c8262cdcdf9a472b8c52ef83375fe15"

// TestScanE2EImageLayerDiffIDRoundTrip needs network once, to refresh trivy's two databases (the Java one
// near 1 GB), then builds an app image on an old Ubuntu noble with a log4j-core 2.14.1 jar that carries no
// pom.properties — the Java database names it from its file name — and a baked ENV value. Its image scan
// lists libssl3 in the base layer and the jar in the top one, and never that value. Each layer's slice of
// the SBOM, as the server keeps them, is re-matched — no image, no Java database — and names libssl3's
// fixed CVEs on the base layer and Log4Shell on the jar's, each joined to its component by bom-ref: trivy
// image → trivy sbom keeps LayerDiffID (PATCHING.md §3.5). The base layer's slice without its
// operating-system component is refused: trivy would have matched none of its packages. A slice with a
// purl trivy skips — of a type it does not match, or one it cannot parse — answers no clean report. Trivy
// lists what an SBOM document in the image names, but never one of the lifecycle's in its SBOM layer, where it
// still lists a binary planted beside them, as an app's own buildpack could.
func TestScanE2EImageLayerDiffIDRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	e := &Executor{}
	for _, tool := range []string{"trivy", JavaDB} {
		start := time.Now()
		must(t, e.refreshDB(ctx, tool, scanTools[tool]))
		t.Logf("%s's database refreshed in %s", tool, time.Since(start).Round(time.Second))
	}
	var jar bytes.Buffer
	zw := zip.NewWriter(&jar)
	f, err := zw.Create("META-INF/MANIFEST.MF")
	must(t, err)
	must(t, write(f, []byte("Manifest-Version: 1.0\n")))
	must(t, zw.Close())
	canary := "canyon-canary-" + nonce()
	build := exec.CommandContext(ctx, "docker", "build", "--quiet", "--tag", "canyon/e2e-image:pkg_e2e0", "-")
	build.Stdin = bytes.NewReader(tarOf(t, map[string]string{"Dockerfile": "FROM " + ubuntuNoble + "\nENV CANARY_SECRET=" + canary + "\nCOPY rootfs/ /\n" +
		"COPY --from=" + lifecycleImage + " /cnb/lifecycle/launcher /layers/sbom/launch/planted/launcher\n",
		"rootfs/workspace/lib/log4j-core-2.14.1.jar":                                 jar.String(),
		"rootfs/workspace/node_modules/@babel/core/package.json":                     `{"name":"@babel/core","version":"7.0.0"}`,
		"rootfs/usr/lib/python3/dist-packages/zope_interface-6.0.dist-info/METADATA": "Metadata-Version: 2.1\nName: zope_interface\nVersion: 6.0\n",
		"rootfs/layers/sbom/launch/canyon/sbom.cdx.json":                             namesOne("left-pad", "1.3.0"),
		"rootfs/workspace/sbom.cdx.json":                                             namesOne("is-odd", "3.0.1")}))
	id, err := build.Output()
	must(t, err)
	defer exec.Command("docker", "image", "rm", "-f", strings.TrimSpace(string(id))).Run()
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()

	status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"image","app":"e2e-image","image":"canyon/e2e-image@`+strings.TrimSpace(string(id))+`"}`)
	var image ScanResult
	if status != 200 || json.Unmarshal([]byte(body), &image) != nil || image.SBOM == nil || len(image.DiffIDs) != 3 {
		t.Fatalf("the image's SBOM: %d %.600s", status, body)
	}
	if strings.Contains(body, canary) {
		t.Errorf("the image's ENV value is in its SBOM")
	}
	base, top := image.DiffIDs[0], image.DiffIDs[1]
	var sbom struct {
		Metadata   json.RawMessage
		Components []map[string]any
	}
	must(t, json.Unmarshal(image.SBOM, &sbom))
	layerOf := func(c map[string]any) string {
		props, _ := c["properties"].([]any)
		for _, p := range props {
			if p, _ := p.(map[string]any); p["name"] == "aquasecurity:trivy:LayerDiffID" {
				s, _ := p["value"].(string)
				return s
			}
		}
		return ""
	}
	refOf := map[string]string{} // purl → bom-ref, of the two packages looked for
	for _, c := range sbom.Components {
		purl, _ := c["purl"].(string)
		ref, _ := c["bom-ref"].(string)
		switch {
		case strings.HasPrefix(purl, "pkg:deb/ubuntu/libssl3t64@") && layerOf(c) == base: // noble's libssl3, renamed in the 64-bit time_t transition
			refOf["libssl3"] = ref
		case purl == "pkg:maven/org.apache.logging.log4j/log4j-core@2.14.1" && layerOf(c) == top:
			refOf["log4j-core"] = ref
		case purl == "pkg:npm/%40babel/core@7.0.0" && layerOf(c) == top: // a scoped name, its @ percent-encoded
			refOf["@babel/core"] = ref
		case purl == "pkg:pypi/zope-interface@6.0" && layerOf(c) == top: // named zope_interface, its purl's name normalised
			refOf["zope_interface"] = ref
		}
	}
	// an SBOM document trivy reads where it finds one, but in the lifecycle's SBOM layer (contracts/executor.md)
	if !strings.Contains(string(image.SBOM), `"pkg:npm/is-odd@3.0.1"`) || strings.Contains(string(image.SBOM), "left-pad") {
		t.Errorf("is-odd, named by /workspace/sbom.cdx.json, listed, and left-pad, named only in /layers/sbom, not:\n%.3000s", image.SBOM)
	}
	if !slices.ContainsFunc(sbom.Components, func(c map[string]any) bool { // the launcher's own modules, in the layer that holds it alone
		purl, _ := c["purl"].(string)
		return strings.HasPrefix(purl, "pkg:golang/github.com/buildpacks/lifecycle@") && layerOf(c) == image.DiffIDs[2]
	}) {
		t.Errorf("a Go binary planted in /layers/sbom, no SBOM document, is not listed:\n%.3000s", image.SBOM)
	}
	if len(refOf) != 4 {
		t.Fatalf("libssl3 in the base layer %s and the jar, named by the Java database, in the top one %s: %v\n%.3000s", base, top, refOf, image.SBOM)
	}
	slice := func(layer string, withOS bool, extra ...map[string]any) string { // what the server keeps of one layer, as it sends it
		kept := extra
		for _, c := range sbom.Components {
			if layerOf(c) == layer || withOS && c["type"] == "operating-system" {
				kept = append(kept, c)
			}
		}
		b, _ := json.Marshal(map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "version": 1, "metadata": sbom.Metadata, "components": kept})
		return string(b)
	}
	rematch := func(part, vulnerability, pkg, layer string) {
		t.Helper()
		status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"sbom"}`, [2]string{"sbom", part})
		var res ScanResult
		if status != 200 || json.Unmarshal([]byte(body), &res) != nil || res.Report == nil || res.DBBuiltAt == "" {
			t.Fatalf("the re-match of %s: %d %.600s", pkg, status, body)
		}
		var report struct {
			Results []struct {
				Vulnerabilities []struct {
					VulnerabilityID string
					PkgIdentifier   struct{ BOMRef string }
					Layer           struct{ DiffID string }
				}
			}
		}
		must(t, json.Unmarshal(res.Report, &report))
		found := false
		for _, r := range report.Results {
			for _, v := range r.Vulnerabilities {
				if strings.HasPrefix(v.VulnerabilityID, vulnerability) && v.PkgIdentifier.BOMRef == refOf[pkg] {
					found = found || v.Layer.DiffID == layer
				}
			}
		}
		if !found || strings.Contains(body, canary) {
			t.Errorf("%s* on %s, in its layer %s, and never the ENV value: %.2000s", vulnerability, pkg, layer, res.Report)
		}
	}
	rematch(slice(base, true), "CVE-", "libssl3", base)
	rematch(slice(top, false), "CVE-2021-44228", "log4j-core", top)
	if status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"sbom"}`, [2]string{"sbom", slice(base, false)}); status != 400 || !strings.Contains(body, "operating-system") {
		t.Errorf("the base layer's slice without its operating system: %d %.600s", status, body)
	}
	// trivy's own scoped npm and underscored PyPI packages pass its shape and its read check: read, never refused
	status, body = postParts(t, srv.URL, `{"tool":"trivy","target":"sbom"}`, [2]string{"sbom", slice(top, false)})
	var whole struct {
		Report struct {
			Results []struct {
				Packages []struct{ Identifier struct{ BOMRef string } }
			}
		}
	}
	read := map[string]bool{}
	if status == 200 && json.Unmarshal([]byte(body), &whole) == nil {
		for _, r := range whole.Report.Results {
			for _, p := range r.Packages {
				read[p.Identifier.BOMRef] = true
			}
		}
	}
	if !read[refOf["@babel/core"]] || !read[refOf["zope_interface"]] {
		t.Errorf("the top layer's slice, its scoped npm and underscored PyPI packages read: %d %.1500s", status, body)
	}
	part := slice(top, false, map[string]any{"bom-ref": "pkg:generic/foo@1", "type": "library", "name": "foo", "version": "1", "purl": "pkg:generic/foo@1"})
	if status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"sbom"}`, [2]string{"sbom", part}); status != 200 || !strings.HasSuffix(strings.TrimSpace(body), `"not_finished":"not_sarif","reason":"unread"}`) {
		t.Errorf("the top layer's slice with a purl of a type trivy skips: %d %.600s", status, body)
	}
	part = slice(top, false, map[string]any{"bom-ref": "4c1a2b3d-1111-4222-8333-444455556666", "type": "library", "name": "bad", "purl": "pkg:npm"})
	if status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"sbom"}`, [2]string{"sbom", part}); status != 400 {
		t.Errorf("the top layer's slice with a purl that does not parse: %d %.600s", status, body)
	}
	if out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=canyon.scan").Output(); len(out) > 0 {
		t.Errorf("containers labelled canyon.scan outlived the scans: %s", out)
	}
}

// TestEndToEndPaketoImagesHoldWhatPATCHINGsClassTableSays builds a Node, a Python and a Java app with
// Canyon's builder — node-engines, django-sqlite, and spring-maven, Spring Boot on Maven as Paketo's own
// Java sample is — and lists each image (POST /scan, target image) by layer, beside the layer's owner and
// each component's class as the server reads them, trivy.Owners and trivy.Class (contracts/finding.md,
// System libraries and build tools; design/PATCHING.md §2; #2216). Every component is in a class Canyon can name, every dpkg package is the run image's, and the
// packages the table names are in the class it gives them. Each of the Java image's jars is listed but the
// JRE's own, and the log says which carry no pom.properties: those trivy names from the Java database alone.
func TestEndToEndPaketoImagesHoldWhatPATCHINGsClassTableSays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	e := &Executor{Join: []string{}}
	must(t, e.refreshDB(ctx, JavaDB, scanTools[JavaDB]))
	srv := httptest.NewServer(Handler(e))
	defer srv.Close()
	every := []landmark{{"pkg:golang/stdlib@", trivy.Lifecycle, "lifecycle"}, // §3.5: the launcher's Go is the lifecycle's
		{"pkg:golang/github.com/paketo-buildpacks/ca-certificates/", "paketo-buildpacks/ca-certificates", "runtime"},
		{"pkg:golang/stdlib@", "paketo-buildpacks/ca-certificates", "runtime"}} // a helper's Go is its buildpack's
	const npmInstall = "paketo-buildpacks/npm-install"
	for _, sample := range []struct {
		app  string
		want []landmark
	}{
		{"node-engines", []landmark{{"pkg:npm/npm@", "paketo-buildpacks/node-engine", "runtime"}, {"pkg:npm/ms@", npmInstall, "app"},
			{"pkg:golang/github.com/paketo-buildpacks/npm-install", npmInstall, "runtime"}}}, // npm-install's own binary, beside what it installed
		{"django-sqlite", []landmark{{"pkg:pypi/pip@", "paketo-buildpacks/cpython", "runtime"}, {"pkg:pypi/setuptools@", "paketo-buildpacks/cpython", "runtime"},
			{"pkg:pypi/django@", "paketo-buildpacks/pip-install", "app"}, {"pkg:pypi/gunicorn@", "paketo-buildpacks/pip-install", "app"}}},
		{"spring-maven", []landmark{{"pkg:maven/org.springframework/spring-core@", trivy.App, "app"},
			{"pkg:maven/org.springframework.cloud/spring-cloud-bindings@", "paketo-buildpacks/spring-boot", "runtime"},
			{"pkg:golang/github.com/paketo-buildpacks/spring-boot/", "paketo-buildpacks/spring-boot", "runtime"}}},
	} {
		want := append(sample.want, every...)
		t.Run(sample.app, func(t *testing.T) {
			var log bytes.Buffer
			res, err := e.Build(ctx, BuildRequest{App: sample.app, Package: "pkg_e2e7"}, treeOf(t, filepath.Join("testdata", "apps", sample.app)), &log)
			if err != nil {
				t.Fatalf("build: %v\n%s", err, tail(log.String(), 60))
			}
			id := strings.TrimPrefix(res.Image, "canyon/"+sample.app+"@")
			defer exec.Command("docker", "image", "rm", "-f", id).Run()
			status, body := postParts(t, srv.URL, `{"tool":"trivy","target":"image","app":"`+sample.app+`","image":"`+res.Image+`"}`)
			var scan ScanResult
			if status != 200 || json.Unmarshal([]byte(body), &scan) != nil || scan.SBOM == nil {
				t.Fatalf("the image's SBOM: %d %.600s", status, body)
			}
			owners, ok := trivy.Owners(scan.DiffIDs, scan.Lifecycle)
			if !ok {
				t.Fatalf("no layer of the image can be owned by its label: %s", scan.Lifecycle)
			}
			var sbom struct {
				Components []struct {
					Type, Name, Version, Purl string
					Properties                []struct{ Name, Value string }
				}
			}
			must(t, json.Unmarshal(scan.SBOM, &sbom))
			byLayer, jars := map[string][]string{}, map[string][]string{} // diff ID → its lines; a jar's path → its purls
			found := make([]int, len(want))
			for _, c := range sbom.Components {
				if c.Type != "library" {
					continue
				}
				prop := map[string]string{}
				for _, p := range c.Properties {
					prop[p.Name] = p.Value
				}
				layer, path := prop["aquasecurity:trivy:LayerDiffID"], prop["aquasecurity:trivy:FilePath"]
				owner := owners[layer]
				class := trivy.Class(owner, prop["aquasecurity:trivy:PkgType"], c.Purl)
				byLayer[layer] = append(byLayer[layer], fmt.Sprintf("%-9s %s %s", class, cmp.Or(c.Purl, c.Name+" "+c.Version+" (no purl)"), path))
				if strings.HasSuffix(path, ".jar") {
					jars[path] = append(jars[path], c.Purl)
				}
				switch {
				case class == "unknown":
					t.Errorf("%s in %s (%s) is in no class Canyon can name", cmp.Or(c.Purl, c.Name), cmp.Or(owner, "a layer the label does not name"), path)
				case strings.HasPrefix(c.Purl, "pkg:deb/") && owner != trivy.RunImage:
					t.Errorf("%s is in %s, not the run image", c.Purl, owner)
				}
				for i, w := range want {
					if strings.HasPrefix(c.Purl, w.purl) && (w.owner == "" || w.owner == owner) {
						found[i]++
						if class != w.class {
							t.Errorf("%s in %s is %s, want %s", c.Purl, owner, class, w.class)
						}
					}
				}
			}
			for i, w := range want {
				if found[i] == 0 {
					t.Errorf("no %s* in %s", w.purl, cmp.Or(w.owner, "the image"))
				}
			}
			var listing strings.Builder
			for _, d := range append(slices.Clone(scan.DiffIDs), "") {
				if lines := byLayer[d]; d != "" || len(lines) > 0 {
					slices.Sort(lines)
					fmt.Fprintf(&listing, "%s  %s  (%d)\n", cmp.Or(d, "no LayerDiffID"), cmp.Or(owners[d], "no owner"), len(lines))
					for _, l := range lines {
						fmt.Fprintf(&listing, "    %s\n", l)
					}
				}
			}
			t.Logf("%s, %d components:\n%s", sample.app, len(sbom.Components), listing.String())
			if sample.app == "spring-maven" {
				var said strings.Builder
				onDisk := jarsIn(t, id)
				for _, path := range slices.Sorted(maps.Keys(onDisk)) {
					how := "pom.properties   "
					if !onDisk[path] {
						how = "no pom.properties"
					}
					fmt.Fprintf(&said, "%s %s %v\n", how, path, jars[path])
					if listed := len(jars[path]) > 0; listed == (path == jrtFS) {
						t.Errorf("%s listed %v: every jar is, but the JRE's own, which neither carries a pom.properties nor is in the Java database", path, listed)
					}
				}
				t.Logf("spring-maven's jars:\n%s", said.String())
			}
		})
	}
}

// namesOne is a CycloneDX document naming one npm package, as a buildpack's SBOM in /layers/sbom does.
func namesOne(name, version string) string {
	purl := "pkg:npm/" + name + "@" + version
	return `{"bomFormat":"CycloneDX","specVersion":"1.5","version":1,"metadata":{"component":{"type":"application","name":"fixture","bom-ref":"fixture"}},` +
		`"components":[{"type":"library","bom-ref":"` + purl + `","name":"` + name + `","version":"` + version + `","purl":"` + purl + `"}]}`
}

// jrtFS is the one jar Paketo's JRE ships: trivy names no component from it.
const jrtFS = "layers/paketo-buildpacks_bellsoft-liberica/jre/lib/jrt-fs.jar"

// landmark is a package design/PATCHING.md §2 names, by its purl's prefix, and the class it gives it;
// owner, when set, narrows it to the components in that owner's layers, as trivy.Owners names them.
type landmark struct{ purl, owner, class string }

// jarsIn is every jar in the image's filesystem, by its path as trivy writes it, and whether it carries
// its own pom.properties: one without is named by the Java database or not at all.
func jarsIn(t *testing.T, image string) map[string]bool {
	t.Helper()
	out, err := exec.Command("docker", "create", image).Output()
	must(t, err)
	ctr := strings.TrimSpace(string(out))
	defer exec.Command("docker", "rm", "-f", ctr).Run()
	export := exec.Command("docker", "export", ctr)
	fs, err := export.StdoutPipe()
	must(t, err)
	must(t, export.Start())
	jars, tr := map[string]bool{}, tar.NewReader(fs)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		if h.Typeflag != tar.TypeReg || !strings.HasSuffix(h.Name, ".jar") {
			continue
		}
		b, err := io.ReadAll(tr)
		must(t, err)
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		must(t, err)
		jars[strings.TrimPrefix(h.Name, "/")] = slices.ContainsFunc(zr.File, func(f *zip.File) bool {
			return strings.HasPrefix(f.Name, "META-INF/maven/") && strings.HasSuffix(f.Name, "/pom.properties")
		})
	}
	must(t, export.Wait())
	return jars
}
