package cli

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What the demo needs of this machine (docs: Try it on your laptop, What you need). The disk is the
// engine's: on Docker Desktop, OrbStack and Colima that is the Docker VM's, not the Mac's.
const (
	demoDockerMajor = 28
	demoCPUs        = 4
	demoMemoryGB    = 6
	demoDiskGB      = 18
	demoTries       = 3
	demoTail        = 20 // lines of the deployer's log a failed start shows
)

// demoProbe is the image the disk check runs df in: docker-compose.yml's own busybox pin, which the
// demo downloads anyway. demo_test.go holds the two equal.
const demoProbe = "docker.io/library/busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// gap is one thing this machine lacks, and the one fix for it. canyon demo up prints them and
// stops: it installs, deletes and repairs nothing, and neither should the agent that ran it.
type gap struct{ what, fix string }

// demoUp is cli.md §6.14's five stages. Each can be run again: a second run finds the bundle and the
// images the first one downloaded, and the sample apps' deployer carries on where it stopped.
func (a *app) demoUp(ctx context.Context, dir string) int {
	a.printf("\n")
	a.demoStep("Checking this machine")
	stack := demoStack(dir)
	if gaps := a.demoPreflight(ctx, stack); len(gaps) > 0 {
		a.errf("\n  %s This machine is not ready for the demo. Nothing was started.\n", a.render.glyph(stateFail))
		for i, g := range gaps {
			a.errf("\n  %d. %s\n     Fix: %s\n", i+1, g.what, g.fix)
		}
		a.errf("\n  Then run: canyon demo up\n")
		return exitError
	}
	base := demoBase()
	if from := a.getenv("CANYON_DEMO_FROM"); from != "" { // a release built from a tree, as CI serves one
		base = from
	}
	if a.demoRunning(ctx, stack) {
		if demoStale(ctx, dir, base) {
			a.printf("\n  %s The demo running here is from a different release than this CLI (%s). To switch: canyon demo down, then canyon demo up.\n", a.render.glyph(stateWarn), Version)
		}
		return a.demoReady(ctx, dir)
	}
	a.demoStep("Fetching the demo into ~/.canyon/demo")
	if err := demoFetch(ctx, dir, base); err != nil {
		return a.demoFailed("The demo could not be downloaded", err)
	}
	a.demoStep("Downloading Canyon's images")
	if err := demoRetry(ctx, func() error {
		_, err := demoDocker(ctx, stack, nil, demoCompose("pull", "--quiet")...)
		return err
	}); err != nil {
		return a.demoFailed("Canyon's images could not be downloaded", err)
	}
	a.demoStep("Starting Canyon and its sample apps. A first start takes 20 to 30 minutes.")
	if _, err := demoDocker(ctx, stack, nil, demoCompose("up", "-d")...); err != nil {
		return a.demoFailed("docker compose up failed", err)
	}
	// The deployer's log ends when the deployer exits, with the sample apps built and running.
	// Their builds download the build image and the scanners' databases, so nothing is left to
	// download in the middle of the visitor's first review. Its few hundred lines are kept, not
	// printed: a start that works says its stages and each sample app as it runs, and one that
	// fails shows the log's end. A start that outlasts demoDeadline stops waiting and says where
	// the log is; the demo carries on starting.
	waiting, stop := context.WithTimeout(ctx, demoDeadline)
	defer stop()
	progress := &demoProgress{a: a}
	_, followed := demoDocker(waiting, stack, progress, demoCompose("logs", "--follow", "--no-log-prefix", demoDeployer)...)
	log := progress.log.String()
	if errors.Is(waiting.Err(), context.DeadlineExceeded) {
		a.errf("\n  %s The sample apps haven't finished deploying after %d minutes. Their log: cd ~/.canyon/demo/canyon && docker compose -p %s --profile %s logs %s\n\n  To carry on, run: canyon demo up\n",
			a.render.glyph(stateFail), int(demoDeadline.Minutes()), demoProject, demoProfile, demoDeployer)
		return exitError
	}
	if code := demoDeployed(ctx, stack); code != "0" {
		if log = strings.TrimSpace(log); log != "" {
			lines := strings.Split(log, "\n")
			a.errf("\n%s\n", strings.Join(lines[max(0, len(lines)-demoTail):], "\n"))
		}
		why := fmt.Errorf("their deployer exited %s; its last lines are above", code)
		if code == "" { // the log ended, and the deployer has no exit status to name
			why := "The sample apps haven't finished deploying, and Docker stopped showing their progress"
			if followed != nil {
				why += ": " + plainly(followed)
			}
			a.errf("\n  %s %s\n\n  To carry on, run: canyon demo up\n", a.render.glyph(stateFail), why)
			return exitError
		}
		return a.demoFailed("The sample apps did not finish deploying", why)
	}
	return a.demoReady(ctx, dir)
}

// demoDeployed is the deployer's exit status, "" while it runs or when there is none.
func demoDeployed(ctx context.Context, stack string) string {
	ids, err := demoListed(ctx, stack, demoCompose("ps", "-aq", "--status", "exited", demoDeployer)...)
	if err != nil || len(ids) != 1 {
		return ""
	}
	code, _ := demoDocker(ctx, "", nil, "inspect", "--format", "{{.State.ExitCode}}", ids[0])
	return code
}

// demoRunning is whether canyon demo up has nothing left to start: the server runs and the sample
// apps' deployer finished clean. A second up then signs in and reports, and deploys nothing again.
func (a *app) demoRunning(ctx context.Context, stack string) bool {
	if _, err := os.Stat(filepath.Join(stack, "docker-compose.yml")); err != nil {
		return false
	}
	running, err := demoListed(ctx, "", "ps", "-q", "--filter", "name=^canyon-server$")
	if err != nil || len(running) == 0 {
		return false
	}
	project, _, _, err := demoServer(ctx)
	return err == nil && project == demoProject && demoDeployed(ctx, stack) == "0"
}

// demoStale is whether the demo running here came from a bundle other than the one this CLI's
// release names: up then starts nothing new, and says how to. A release that cannot be read says
// nothing, since the demo runs either way.
func demoStale(ctx context.Context, dir, base string) bool {
	want, err := demoSum(ctx, base)
	if err != nil {
		return false
	}
	kept, err := os.ReadFile(filepath.Join(dir, demoBundle)) //nolint:gosec // this CLI's own file, under ~/.canyon
	return err == nil && fmt.Sprintf("%x", sha256.Sum256(kept)) != want
}

// demoReady signs this machine in to the demo, as canyon login does, and says the one line the
// stages end on. Signing in makes the demo the host every canyon command reaches: a host it
// replaces is said, and kept for canyon demo down to name the way back.
func (a *app) demoReady(ctx context.Context, dir string) int {
	before := a.config().Host
	if code := runLogin(ctx, a, []string{demoURL}); code != exitOK {
		return code
	}
	if project, _, found, err := demoServer(ctx); err == nil && found && project == demoProject { // only the demo's own server is asked
		if apps, err := demoApps(ctx); err == nil { // kept for down, which may run when the server no longer can answer (#170)
			a.demoKeep(dir, apps)
		}
	}
	a.printf("  %s Canyon is ready at %s\n", a.render.paint(ansiGreen, a.render.glyph(statePass)), demoURL)
	a.printf("\n  Next, take the guided tour: canyon demo tour\n")
	if before != "" && before != demoURL {
		if err := atomicWrite(demoHostBefore(dir), []byte(before+"\n"), true); err != nil {
			a.errf("\n  %s Couldn't save which Canyon you used before (~/.canyon/demo/host-before): %s\n", a.render.glyph(stateWarn), plainly(err))
		}
		a.printf("\n  canyon commands now go to the demo instead of %s. canyon demo down tells you how to switch back.\n", cell(before))
	}
	return exitOK
}

// demoRetry tries a download demoTries times, a moment apart: one refused connection or failed name
// lookup must not end a start that takes half an hour.
func demoRetry(ctx context.Context, try func() error) error {
	var err error
	for i := range demoTries {
		if err = try(); err == nil || ctx.Err() != nil {
			return err
		}
		if i < demoTries-1 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(demoPause):
			}
		}
	}
	return err
}

// demoPause is the wait between two tries; a var so a test does not wait.
var demoPause = 5 * time.Second

// demoPreflight is everything the demo needs of this machine, checked before anything is fetched or
// started, and answered as one list. Without an engine there is nothing to ask the rest of.
func (a *app) demoPreflight(ctx context.Context, stack string) []gap {
	var gaps []gap
	mac := goos == "darwin"
	if set := a.getenv("CANYON_HOST"); set != "" { // it outranks the host up signs in to (§8.2): the tour would go there
		if host, err := validHost("CANYON_HOST", set); err != nil || host != demoURL {
			gaps = append(gaps, gap{fmt.Sprintf("CANYON_HOST is set to %s, so canyon commands would go there instead of the demo.", cell(set)), "unset CANYON_HOST, then run canyon demo up again."})
		}
	}
	info, err := demoDocker(ctx, "", nil, "info", "--format", "{{.ServerVersion}} {{.NCPU}} {{.MemTotal}} {{.Name}}")
	engine := strings.Fields(info)
	if err == nil && (len(engine) < 4 || engine[1] == "0") { // docker info exits 0 with no engine behind it, and answers an empty version and no CPUs
		err = errors.New("docker info named no engine")
	}
	switch {
	case errors.Is(err, exec.ErrNotFound) && mac:
		gaps = append(gaps, gap{"Docker is not installed.",
			"brew install colima docker docker-compose && colima start --cpu 4 --memory 8 --disk 60\n          Or install Docker Desktop or OrbStack, and start it."})
	case errors.Is(err, exec.ErrNotFound):
		gaps = append(gaps, gap{"Docker is not installed.", "install Docker Engine with its Compose plugin: https://docs.docker.com/engine/install/"})
	case err != nil && mac:
		gaps = append(gaps, gap{"Docker is installed, but its engine does not answer.",
			"colima start --cpu 4 --memory 8 --disk 60\n          Or start Docker Desktop or OrbStack."})
	case err != nil:
		gaps = append(gaps, gap{"Docker is installed, but its engine does not answer.", "sudo systemctl start docker"})
	}
	if err != nil {
		return append(gaps, demoPortGap(ctx)...)
	}
	colima := engine[3] == "colima"
	if major, _ := strconv.Atoi(strings.SplitN(engine[0], ".", 2)[0]); major < demoDockerMajor {
		gaps = append(gaps, gap{fmt.Sprintf("Docker is %s, and the demo needs %d or newer.", cell(engine[0]), demoDockerMajor), "upgrade Docker, then start it again."})
	}
	version, err := demoDocker(ctx, "", nil, "compose", "version", "--short")
	if major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(version, "v"), ".", 2)[0]); err != nil || major < 2 {
		fix := "install the Compose plugin: https://docs.docker.com/compose/install/linux/"
		if mac {
			fix = `brew install docker-compose && mkdir -p ~/.docker/cli-plugins && ln -sfn "$(brew --prefix)/opt/docker-compose/bin/docker-compose" ~/.docker/cli-plugins/docker-compose`
		}
		gaps = append(gaps, gap{"Docker has no Compose v2 (docker compose).", fix})
	}
	if a.demoRunning(ctx, stack) || demoImaged(ctx, stack) { // what it needs of the machine is here already
		return append(gaps, a.demoNamesTaken(ctx)...)
	}
	cpus, _ := strconv.Atoi(engine[1])
	memory, _ := strconv.ParseInt(engine[2], 10, 64)
	if cpus < demoCPUs || memory < (demoMemoryGB<<30)-(1<<29) { // a 6 GB VM reports a little under 6 GiB
		fix := fmt.Sprintf("give Docker's VM %d CPUs and %d GB of memory in its settings, then start it again.", demoCPUs, demoMemoryGB)
		if colima {
			fix = "colima stop && colima start --cpu 4 --memory 8"
		}
		gaps = append(gaps, gap{fmt.Sprintf("Docker has %d CPUs and %.1f GB of memory, and the demo needs %d CPUs and %d GB.", cpus, float64(memory)/(1<<30), demoCPUs, demoMemoryGB), fix})
	}
	// The engine's own disk, read where an image's layers land. The probe is 2 MB and the demo's own.
	df, err := demoDocker(ctx, "", nil, "run", "--rm", demoProbe, "df", "-k", "/")
	free, ok := demoFreeGB(df)
	switch {
	case err != nil || !ok:
		gaps = append(gaps, gap{"Docker could not download and run a small image: " + cell(plainly(cmp.Or(err, errors.New("df answered nothing Canyon can read")))) + ".",
			"check this machine's network connection and that Docker can reach docker.io."})
	case free < demoDiskGB:
		fix := "free disk space, or give Docker's VM a larger disk in its settings."
		if colima {
			fix = "free disk space in the VM, or make a larger one. A new VM loses what the old one holds: colima delete && colima start --cpu 4 --memory 8 --disk 60"
		}
		gaps = append(gaps, gap{fmt.Sprintf("Docker has %d GB of free disk, and the demo needs about %d GB.", free, demoDiskGB), fix})
	}
	return append(gaps, a.demoNamesTaken(ctx)...)
}

// demoNamesTaken is a Canyon started another way, which holds the names the demo's containers
// take, or else the dashboard's port held by another program.
func (a *app) demoNamesTaken(ctx context.Context) []gap {
	switch project, where, found, err := demoServer(ctx); {
	case err != nil:
		return []gap{{"Docker could not say what runs here: " + cell(plainly(err)) + ".", "check that Docker is running, then run canyon demo up again."}}
	case found && project != demoProject:
		if project == "" {
			return []gap{{"A container named canyon-server is running here, and the demo needs its name.", "docker rm -f canyon-server, if nothing else needs it"}}
		}
		at, fix := demoAnother(project, where, "down")
		if !strings.HasPrefix(fix, "run in its folder: ") {
			fix = "stop it there: " + fix
		}
		return []gap{{"A Canyon started another way is running here: " + at + ".", fix}}
	case !found: // the demo's own server holds the port while it runs
		return demoPortGap(ctx)
	}
	return nil
}

// demoImaged is whether the images of the demo unpacked here are all on this machine already: a
// start that downloaded them once needs no disk for them again.
func demoImaged(ctx context.Context, stack string) bool {
	if _, err := os.Stat(filepath.Join(stack, "docker-compose.yml")); err != nil {
		return false
	}
	images, err := demoListed(ctx, stack, demoCompose("config", "--images")...)
	if err != nil || len(images) == 0 {
		return false
	}
	_, err = demoDocker(ctx, "", nil, append([]string{"image", "inspect", "--format", "{{.Id}}"}, images...)...)
	return err == nil
}

// demoDeadline is how long up waits for the sample apps before it says where their log is; a var so
// a test does not wait.
var demoDeadline = 45 * time.Minute

// demoReadyPrefix starts the line demo/apps/run.sh prints as each sample app runs: up says it as
// one line of progress.
const demoReadyPrefix = "canyon-demo: running "

// demoProgress keeps the deployer's log for a start that fails, and says each sample app as the
// deployer's line for it arrives.
type demoProgress struct {
	a       *app
	log     bytes.Buffer
	partial []byte
	ready   int
}

func (p *demoProgress) Write(b []byte) (int, error) {
	p.log.Write(b)
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := strings.TrimSpace(string(p.partial[:i]))
		p.partial = p.partial[i+1:]
		if app, ok := strings.CutPrefix(line, demoReadyPrefix); ok {
			p.ready++
			p.a.printf("  %s %s is running (%d of %d)\n", p.a.render.glyph(statePass), cell(app), p.ready, len(demoSampleApps))
		}
	}
}

// demoSampleApps are the apps demo/apps/run.sh deploys, in its order.
var demoSampleApps = []string{"claims-portal", "endorsement-hub", "claims-archive", "premium-ledger"}

// demoFreeGB reads df -k's second line: the kilobytes available on the filesystem.
func demoFreeGB(df string) (int, bool) {
	lines := strings.Split(strings.TrimSpace(df), "\n")
	if len(lines) < 2 {
		return 0, false
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 4 {
		return 0, false
	}
	kb, err := strconv.ParseInt(fields[len(fields)-3], 10, 64) // Available, counted from the end: a long device name wraps the line
	return int(kb >> 20), err == nil
}

// demoPortGap is the dashboard's port held by another program: Docker's own error does not say it,
// and under Colima the port is forwarded or not without a word (#717).
func demoPortGap(ctx context.Context) []gap {
	conn, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", demoPort)
	if err != nil {
		return nil
	}
	_ = conn.Close()
	return []gap{{"Port 7777 on 127.0.0.1 is in use by another program, and Canyon's dashboard needs it.",
		"stop that program. To see which it is: lsof -nP -iTCP:7777 -sTCP:LISTEN"}}
}

var demoRelease = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// demoBase is the release this CLI's demo comes from: its own, so the demo is the one this binary
// was built to talk to, and the latest for a build that is no release.
func demoBase() string {
	if demoRelease.MatchString(Version) {
		return demoReleases + "/download/" + Version
	}
	return demoReleases + "/latest/download"
}

const demoBundle = "canyon-deploy.tar.gz"

// demoFetch leaves the release's bundle unpacked in dir: canyon-deploy.tar.gz, held to the SHA-256
// the release's SHA256SUMS gives it, as install.sh holds the CLI's archive. A bundle already here
// with that hash is kept, and only unpacked again.
func demoFetch(ctx context.Context, dir, base string) error {
	want, err := demoSum(ctx, base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	kept := filepath.Join(dir, demoBundle)
	body, err := os.ReadFile(kept) //nolint:gosec // this CLI's own file, under ~/.canyon
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != want {
		if body, err = demoGet(ctx, base+"/"+demoBundle); err != nil {
			return err
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != want {
			return fmt.Errorf("%s has SHA-256 %s, but SHA256SUMS says %s; nothing was unpacked", demoBundle, got, want)
		}
		if err := atomicWrite(kept, body, true); err != nil {
			return err
		}
	}
	return demoUnpack(body, dir)
}

// demoSum is the SHA-256 the release's SHA256SUMS gives its bundle.
func demoSum(ctx context.Context, base string) (string, error) {
	sums, err := demoGet(ctx, base+"/SHA256SUMS")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == demoBundle {
			return f[0], nil
		}
	}
	return "", fmt.Errorf("%s/SHA256SUMS names no %s", base, demoBundle)
}

// demoGet downloads one release file, tried demoTries times.
func demoGet(ctx context.Context, url string) ([]byte, error) {
	var body []byte
	err := demoRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "canyon/"+Version)
		resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
		if err != nil {
			return fmt.Errorf("%s could not be downloaded: %s", url, plainly(err))
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s could not be downloaded: the server answered %d", url, resp.StatusCode)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		return err
	})
	return body, err
}

// demoUnpack writes the bundle's files and directories under dir, and refuses an entry that is
// neither or that names a path outside it.
func demoUnpack(bundle []byte, dir string) error {
	gz, err := gzip.NewReader(bytes.NewReader(bundle))
	if err != nil {
		return fmt.Errorf("%s is not a gzip archive: %w", demoBundle, err)
	}
	// An os.Root holds every write under dir, whatever a name or a link in the bundle says.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for tr := tar.NewReader(gz); ; {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s cannot be read: %w", demoBundle, err)
		}
		if !filepath.IsLocal(h.Name) {
			return fmt.Errorf("%s holds %q, a path outside the demo's folder; nothing more was unpacked", demoBundle, h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(h.Name, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(h.Name), 0o750); err != nil {
				return err
			}
			body, err := io.ReadAll(io.LimitReader(tr, 64<<20))
			if err != nil {
				return fmt.Errorf("%s cannot be read: %w", demoBundle, err)
			}
			if err := root.WriteFile(h.Name, body, h.FileInfo().Mode().Perm()&0o755|0o600); err != nil { // the bundle's own mode, a script's executable bit kept
				return err
			}
		case tar.TypeXGlobalHeader: // git archive's comment, which names the commit
		default:
			return fmt.Errorf("%s holds %q, which is neither a file nor a directory; nothing more was unpacked", demoBundle, h.Name)
		}
	}
}
