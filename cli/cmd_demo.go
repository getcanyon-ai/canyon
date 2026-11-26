package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// The demo on this machine (cli.md §6.14). canyon demo is the one verb that runs Docker: it starts
// and stops the release's own compose file and decides nothing a host decides. What it reaches
// beyond this binary is in these four, each a var so a test stands in for Docker and the release.
var (
	// demoDocker runs docker with args in dir. With out it streams there; without, it answers what
	// docker wrote to stdout, and an error carries the last line docker wrote to stderr: Compose
	// writes its progress there first, and what went wrong after it.
	demoDocker = func(ctx context.Context, dir string, out io.Writer, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // docker by name, with this file's own arguments
		cmd.Dir = dir
		if ctx.Value(demoNoImageEnv{}) != nil {
			cmd.Env = demoWithoutImageEnv(os.Environ())
		}
		if trivy, ok := ctx.Value(demoTrivyEnv{}).(string); ok { // set either way: the shell's own never decides (#237)
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			cmd.Env = append(cmd.Env, "CANYON_DEMO_TRIVY="+trivy)
		}
		var answer, said bytes.Buffer
		cmd.Stdout, cmd.Stderr = &answer, &said
		if out != nil {
			cmd.Stdout, cmd.Stderr = out, out
		}
		err := cmd.Run()
		if lines := strings.Split(strings.TrimSpace(said.String()), "\n"); err != nil && lines[len(lines)-1] != "" {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(lines[len(lines)-1]))
		}
		return strings.TrimSpace(answer.String()), err
	}
	demoReleases = releases         // where SHA256SUMS and canyon-deploy.tar.gz are downloaded
	demoURL      = defaultHost      // the demo's address, which compose publishes
	demoPort     = "127.0.0.1:7777" // the same, as a program holding it is found
)

// demoNoImageEnv, as a context's value, has demoDocker run docker without the shell's CANYON_IMAGE_* variables: the
// compose file then names its own pins, never an image this shell names (#170).
type demoNoImageEnv struct{}

// demoTrivyEnv, as a context's value, is what demoDocker sets CANYON_DEMO_TRIVY to for docker: "on" for canyon demo up
// --with-trivy, "" without, which docker-compose.yml's demo-apps service hands its deployer (cli.md §6.14; #237).
type demoTrivyEnv struct{}

// demoWithoutImageEnv is environ without any CANYON_IMAGE_* variable.
func demoWithoutImageEnv(environ []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return strings.HasPrefix(kv, "CANYON_IMAGE_") })
}

// The compose file's profile for the sample apps, and the service that deploys them and exits.
const (
	demoProfile  = "demo-apps"
	demoDeployer = "demo-apps"
)

// demoProject is the Compose project the demo runs as, whatever its folder is called: a checkout
// cloned as canyon, or a bundle unpacked by hand, is a project of its own and never this one.
const demoProject = "canyon-demo"

// demoDir is where canyon demo up unpacks the release's bundle: under the home directory, never the
// folder the command was run in, so a visitor's project gains no file.
func (a *app) demoDir() (string, error) {
	home := a.getenv("HOME")
	if home == "" {
		return "", errors.New("canyon demo needs HOME, to keep the demo's files in ~/.canyon/demo")
	}
	return filepath.Join(home, ".canyon", "demo"), nil
}

// demoStack is the folder docker compose runs in: the bundle's canyon/, with docker-compose.yml and
// the override that names the release's images.
func demoStack(dir string) string { return filepath.Join(dir, "canyon") }

func runDemo(ctx context.Context, a *app, args []string) int {
	form := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		form, args = args[0], args[1:]
	}
	switch form {
	case "tour": // cli.md §6.14.1: the guided tour, on the demo up starts
		return runTour(ctx, a, args)
	case "up", "down":
		a.form = "demo " + form
	case "", "help":
		if len(args) > 0 && !slices.Contains([]string{"-h", "-help", "--help"}, args[0]) { // a flag needs the form it belongs to
			if args[0] == "--purge" || args[0] == "-purge" {
				a.errf("\n  %s --purge goes with down: canyon demo down --purge\n", a.render.glyph(stateFail))
			} else {
				flagName, _, _ := strings.Cut(args[0], "=")
				a.errf("\n  %s Put %s after up, tour or down.\n", a.render.glyph(stateFail), cell(flagName))
			}
			a.demoForms(a.err)
			return exitError
		}
		a.demoForms(a.out)
		return exitOK
	default:
		a.errf("\n  %s demo has no form %q\n", a.render.glyph(stateFail), form)
		a.demoForms(a.err)
		return exitError
	}
	var purge, trivy bool
	rest, err := a.parse(args, func(fs *flag.FlagSet) {
		if form == "down" {
			fs.BoolVar(&purge, "purge", false, "Also remove the demo's images and its files in ~/.canyon/demo")
		} else {
			fs.BoolVar(&trivy, "with-trivy", false, "also run Trivy, which the demo leaves off: about 2 GB more to download and keep")
		}
	})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("%s takes no arguments, and got %q", a.form, rest[0]))
	}
	dir, err := a.demoDir()
	if err != nil {
		return a.usage(err)
	}
	if form == "down" {
		return a.demoDown(ctx, dir, purge)
	}
	return a.demoUp(context.WithValue(ctx, demoTrivyEnv{}, map[bool]string{true: "on"}[trivy]), dir)
}

// demoForms is the forms this binary runs, as skillForms is skill's: `canyon --help` has one row.
func (a *app) demoForms(w io.Writer) {
	forms := [][2]string{
		{"canyon demo up", "Check this machine, then start Canyon and its sample apps at " + demoURL},
		{"canyon demo tour", "Take the guided tour with your coding agent, a chapter at a time"},
		{"canyon demo down", "Stop the demo and remove what it started"},
		{"canyon demo down --purge", "Also remove the demo's images and its files in ~/.canyon/demo"},
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", a.render.paint(ansiBold, "USAGE"))
	for _, f := range forms {
		_, _ = fmt.Fprintf(w, "  %-24s    %s\n", f[0], f[1])
	}
}

// demoFailed is a stage that stopped: what it was doing, what Docker or the download said, and the
// one command that carries on. Every stage can be run again, and keeps what the last run downloaded.
func (a *app) demoFailed(doing string, err error) int {
	a.errf("\n  %s %s: %s\n\n  To carry on, run: canyon demo up\n", a.render.glyph(stateFail), doing, plainly(err))
	return exitError
}

// demoStep is one line of the run, said as the stage starts: the first start takes 20 to 30 minutes,
// and a reader, an agent's user among them, should see which part of it they are in.
func (a *app) demoStep(f string, v ...any) { a.printf("  "+f+"\n", v...) }

// demoCompose is a docker compose command on the demo's own project, with its sample apps' profile.
func demoCompose(args ...string) []string {
	return append([]string{"compose", "-p", demoProject, "--profile", demoProfile}, args...)
}

// demoListed is what docker, run in dir, answers a listing with: one row a line. A listing Docker
// refused is an error, never an empty list.
func demoListed(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := demoDocker(ctx, dir, nil, args...)
	if err != nil {
		return nil, err
	}
	var rows []string
	for _, row := range strings.Split(out, "\n") {
		if row = strings.TrimSpace(row); row != "" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// demoServer is the Compose project, and its folder, of the container named canyon-server: every
// Canyon started from docker-compose.yml runs one by that name, so two cannot run on one Docker.
// found is false when there is none.
// demoServing is whether the container named canyon-server is running: only then is what answers on the demo's port
// its server (#170). A Docker that can't say is taken as no.
func demoServing(ctx context.Context) bool {
	running, err := demoListed(ctx, "", "ps", "-q", "--filter", "name=^canyon-server$")
	return err == nil && len(running) > 0
}

func demoServer(ctx context.Context) (project, dir string, found bool, err error) {
	ids, err := demoListed(ctx, "", "ps", "-aq", "--filter", "name=^canyon-server$")
	if err != nil || len(ids) == 0 {
		return "", "", false, err
	}
	out, err := demoDocker(ctx, "", nil, "inspect", "--format", "{{json .Config.Labels}}", "canyon-server")
	if err != nil {
		return "", "", false, err
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		return "", "", false, fmt.Errorf("docker inspect canyon-server answered labels Canyon cannot read: %w", err)
	}
	return labels["com.docker.compose.project"], labels["com.docker.compose.project.working_dir"], true, nil
}

// demoAnother is the line and the command for a canyon-server the demo did not start.
// where is the stack as a person finds it: its project, and its folder when Docker names one; stop
// is the command that stops it, from that folder.
func demoAnother(project, dir, stop string) (where, fix string) {
	switch {
	case project == "":
		return "a container named canyon-server, which Docker Compose did not start", "docker rm -f canyon-server, if nothing else needs it"
	case dir == "":
		return fmt.Sprintf("Docker Compose project %s (Docker doesn't say which folder it runs from)", cell(project)),
			fmt.Sprintf("run in its folder: docker compose -p %s --profile %s %s", cell(project), demoProfile, stop)
	}
	return fmt.Sprintf("Docker Compose project %s, in %s", cell(project), cell(dir)),
		fmt.Sprintf("cd %s && docker compose --profile %s %s", cell(dir), demoProfile, stop)
}

// demoStopping is the end of "To stop it" for a fix: a command to run there, or a folder to run it in.
func demoStopping(fix string) string {
	if rest, ok := strings.CutPrefix(fix, "run in its folder: "); ok {
		return ", run in its folder: " + rest
	}
	return ": " + fix
}

// demoHostBefore is the file that keeps the host canyon demo up replaced as this machine's default,
// for down to name the way back to it.
func demoHostBefore(dir string) string { return filepath.Join(dir, "host-before") }

// demoMade is what Docker holds of the demo beyond its compose project: the containers, networks and
// build caches of its apps, the builds and scans its executor started, and with purge its apps'
// images, Canyon's tool images, the scanners' databases and the build network. Each row is a kind
// and a name, and removing them takes the names in the order they come.
type demoThing struct{ kind, name string }

// demoMade answers what is the demo's to remove, and with purge what of it stays because something else on this
// Docker holds it, each with why.
func demoMade(ctx context.Context, stack string, transit []string, purge bool) (made, kept []demoThing, err error) {
	named := func(kind string, keep func(string) bool, args ...string) error {
		rows, err := demoListed(ctx, "", args...)
		if err != nil {
			return fmt.Errorf("could not list the demo's %ss: %w", kind, err)
		}
		for _, row := range rows {
			if keep(row) {
				made = append(made, demoThing{kind, row})
			}
		}
		return nil
	}
	every := func(string) bool { return true }
	// What the demo's executor made for its apps, by the label it put on each (contracts/executor.md; #178): their
	// containers and floors' holders, networks and build caches. No other Canyon's executor runs in the demo's
	// compose project, so none of its things carries it.
	own := "label=canyon.project=" + demoProject
	steps := []func() error{
		func() error { return named("container", every, "ps", "-a", "--filter", own, "--format", "{{.Names}}") },
		func() error {
			return named("network", every, "network", "ls", "--filter", own, "--format", "{{.Name}}")
		},
		func() error { return named("volume", every, "volume", "ls", "-q", "--filter", own) },
	}
	for _, label := range []string{"canyon.build", "canyon.scan"} { // the builds and scans the demo's executor started
		steps = append(steps, func() error {
			return named("container", func(name string) bool { return slices.Contains(transit, name) }, "ps", "-a", "--filter", "label="+label, "--format", "{{.Names}}")
		})
	}
	if purge {
		image := func(keep func(string) bool, filters ...string) func() error {
			return func() error {
				rows, err := demoListed(ctx, "", append(append([]string{"images"}, filters...), "--format", "{{.ID}} {{.Repository}}:{{.Tag}}")...)
				if err != nil {
					return fmt.Errorf("could not list the demo's images: %w", err)
				}
				for _, row := range rows {
					if id, ref, _ := strings.Cut(row, " "); keep(ref) {
						made = append(made, demoThing{"image", id})
					}
				}
				return nil
			}
		}
		// The images pulled by digest: the compose file's, which compose's own down --rmi all left behind, those the
		// executor pulls for its scans, and the lifecycle pack pulls by its platform's digest. Each is looked for, and
		// removed, by its own reference (#170), unless something else on this Docker still holds it (demoHeld).
		pulled := func() error {
			refs, err := demoComposeImages(ctx, stack)
			if err != nil {
				return fmt.Errorf("could not list the demo's images: %w", err)
			}
			for _, ref := range slices.Concat(refs, names.PulledImages, names.LifecyclePlatforms) {
				id, err := demoDocker(ctx, "", nil, "image", "inspect", "--format", "{{.Id}}", ref)
				switch {
				case err != nil && strings.Contains(err.Error(), "No such image"):
					continue // not on this Docker: Docker's own answer, and no other, says so
				case err != nil:
					return fmt.Errorf("could not read the demo's image %s: %w", ref, err)
				}
				why, err := demoHeld(ctx, ref, id)
				switch {
				case err != nil:
					return err
				case why != "":
					kept = append(kept, demoThing{"image", ref + " (" + why + ")"})
				default:
					made = append(made, demoThing{"image", ref})
				}
			}
			return nil
		}
		// The images the demo's executor built for its apps, by the second name it gives each in its project
		// (canyon-demo/<app>:<tag>; #178): removed by ID, which takes canyon/<app>:<tag> too, unless a container that
		// isn't the demo's uses it.
		built := func() error {
			rows, err := demoListed(ctx, "", "images", "--filter", "reference="+demoProject+"/*", "--format", "{{.ID}} {{.Repository}}:{{.Tag}}")
			if err != nil {
				return fmt.Errorf("could not list the demo's images: %w", err)
			}
			seen := map[string]bool{}
			for _, row := range rows {
				id, ref, _ := strings.Cut(row, " ")
				if seen[id] {
					continue
				}
				seen[id] = true
				switch why, err := demoUsed(ctx, id); {
				case err != nil:
					return err
				case why != "":
					kept = append(kept, demoThing{"image", ref + " (" + why + ")"})
				default:
					made = append(made, demoThing{"image", id})
				}
			}
			return nil
		}
		steps = append(steps, pulled, built,
			image(every, "--filter", "reference=canyon-scan/*", "--filter", "reference=canyon-run", "--filter", "reference=canyon-builder"),
			func() error {
				return named("volume", func(v string) bool { return strings.HasPrefix(v, "canyon-scandb-") }, "volume", "ls", "-q", "--filter", "dangling=true", "--filter", "name=canyon-scandb-")
			},
			func() error {
				return named("network", every, "network", "ls", "--filter", "label=canyon.build", "--format", "{{.Name}}")
			})
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, nil, err
		}
	}
	return made, kept, nil
}

// demoImagesKept is the file that keeps the images the demo's compose file named when up started it, one a line, as
// the shell's CANYON_IMAGE_* variables made them then: purge removes those, whatever this shell sets (#170).
func demoImagesKept(dir string) string { return filepath.Join(dir, "images") }

// demoComposeImages is what up kept of the compose file's images, else the file's own pins, read without this shell's
// CANYON_IMAGE_* variables (a demo an older CLI started kept none).
func demoComposeImages(ctx context.Context, stack string) ([]string, error) {
	if b, err := os.ReadFile(demoImagesKept(filepath.Dir(stack))); err == nil { //nolint:gosec // this CLI's own file, under ~/.canyon
		return strings.Fields(string(b)), nil
	}
	return demoListed(context.WithValue(ctx, demoNoImageEnv{}, true), stack, demoCompose("config", "--images")...)
}

// demoHeld is why an image the demo pulled by digest stays: on Docker's containerd image store, removing that reference
// takes every tag of its repository at that digest with it (#170). So it stays when a container outside the demo's
// compose project uses it, running or stopped, or when it carries a tag. "" removes it.
func demoHeld(ctx context.Context, ref, id string) (string, error) {
	if why, err := demoUsed(ctx, id); why != "" || err != nil {
		return why, err
	}
	out, err := demoDocker(ctx, "", nil, "image", "inspect", "--format", "{{json .RepoTags}}", ref)
	if err != nil {
		return "", fmt.Errorf("could not read the tags of %s: %w", ref, err)
	}
	var tags []string
	if err := json.Unmarshal([]byte(out), &tags); err != nil {
		return "", fmt.Errorf("docker answered the tags of %s as Canyon cannot read: %w", ref, err)
	}
	for _, tag := range tags {
		// the containerd store names an image pulled by digest by that very reference: no tag. Any other is someone's,
		// even one the compose file writes before its digest: the demo's pull by digest makes none.
		if !strings.Contains(tag, "@") {
			return "it is also tagged " + tag, nil
		}
	}
	return "", nil
}

// demoUsed is why the image id stays because a container that isn't the demo's uses it, running or stopped: "" when
// none does. A container is the demo's in its compose project, or labelled canyon.project=canyon-demo by its executor.
func demoUsed(ctx context.Context, id string) (string, error) {
	users, err := demoListed(ctx, "", "ps", "-a", "--filter", "ancestor="+id, "--format", `{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Label "canyon.project"}}`)
	if err != nil {
		return "", fmt.Errorf("could not list what uses the demo's images: %w", err)
	}
	for _, row := range users {
		if f := strings.Split(row, "|"); len(f) != 3 || f[1] != demoProject && f[2] != demoProject {
			return "the container " + f[0] + " uses it", nil
		}
	}
	return "", nil
}

// demoTransit is the builds and scans the demo's executor started, labelled but naming no app: the
// canyon.build and canyon.scan containers made since its own container was. It is read before the
// demo stops, because Docker refuses a since= that names a container it has removed.
func demoTransit(ctx context.Context, stack string) ([]string, error) {
	ids, err := demoListed(ctx, stack, demoCompose("ps", "-aq", "executor")...)
	if err != nil {
		return nil, fmt.Errorf("could not list the demo's containers: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var names []string
	for _, label := range []string{"canyon.build", "canyon.scan"} {
		rows, err := demoListed(ctx, "", "ps", "-a", "--filter", "label="+label, "--filter", "since="+ids[0], "--format", "{{.Names}}")
		if err != nil {
			return nil, fmt.Errorf("could not list the demo's containers: %w", err)
		}
		names = append(names, rows...)
	}
	return names, nil
}

// demoRemove removes what demoMade found, a kind at a time, containers before the networks they are on.
func demoRemove(ctx context.Context, made []demoThing) {
	for _, kind := range []struct {
		kind string
		rm   []string
	}{
		{"container", []string{"rm", "-f"}},
		{"network", []string{"network", "rm"}},
		{"volume", []string{"volume", "rm"}},
		{"image", []string{"image", "rm", "-f"}},
	} {
		var names []string
		for _, t := range made {
			if t.kind == kind.kind && !slices.Contains(names, t.name) {
				names = append(names, t.name)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			_, _ = demoDocker(ctx, "", nil, append(kind.rm, names...)...) // what stays is found again and named
		}
	}
}

// demoDown is make demo-down for a machine with no checkout, on the demo alone: its compose project
// with its volumes first, so nothing starts another app; then what its executor made, which Compose
// did not create and so leaves running. Nothing another Canyon on this Docker made is touched: what
// the demo's executor made carries its project (#178). With purge, the images go too, and the bundle. It
// says the demo is gone only once Docker holds none of it.
func (a *app) demoDown(ctx context.Context, dir string, purge bool) int {
	stack := demoStack(dir)
	project, where, found, err := demoServer(ctx)
	if err != nil {
		a.errf("\n  %s Docker could not say what runs here: %s\n\n  Is Docker running? Start it, then run: canyon %s\n", a.render.glyph(stateFail), plainly(err), a.form)
		return exitError
	}
	if found && project != demoProject { // another Canyon holds canyon-server, whatever ~/.canyon/demo holds: never asked, never touched (#170)
		at, fix := demoAnother(project, where, "down -v")
		if where == "" && project != "" {
			at, fix = demoAnother(project, where, "down")
		}
		a.errf("\n  %s This Canyon wasn't started by canyon demo up: %s.\n  To stop it%s\n", a.render.glyph(stateFail), at, demoStopping(fix))
		return exitError
	}
	if _, err := os.Stat(filepath.Join(stack, "docker-compose.yml")); err != nil {
		a.printf("\n  %s Nothing to stop: no demo is running (looked in ~/.canyon/demo and for a container named canyon-server).\n", a.render.glyph(statePass))
		return exitOK
	}
	before, _ := os.ReadFile(demoHostBefore(dir)) //nolint:gosec // this CLI's own file, under ~/.canyon
	transit, err := demoTransit(ctx, stack)       // while the executor's container is there for since= to name
	if err != nil {
		a.errf("\n  %s Docker %s\n\n  Is Docker running? Start it, then run: canyon %s\n", a.render.glyph(stateFail), plainly(err), a.form)
		return exitError
	}
	down := demoCompose("down", "-v")
	if purge {
		down = append(down, "--rmi", "all")
	}
	a.printf("\n")
	a.demoStep("Stopping Canyon and removing its data")
	if _, err := demoDocker(ctx, stack, nil, down...); err != nil {
		a.errf("\n  %s docker compose down failed: %s\n\n  Is Docker running? Start it, then run: canyon %s\n", a.render.glyph(stateFail), plainly(err), a.form)
		return exitError
	}
	a.demoStep("Removing the apps Canyon started")
	made, kept, err := demoMade(ctx, stack, transit, purge)
	if err == nil {
		demoRemove(ctx, made)
		made, kept, err = demoMade(ctx, stack, transit, purge)
	}
	if err == nil {
		var rows []string
		if rows, err = demoListed(ctx, stack, demoCompose("ps", "-a", "--format", "{{.Name}}")...); err != nil {
			err = fmt.Errorf("could not list the demo's containers: %w", err)
		} else {
			for _, name := range rows {
				made = append(made, demoThing{"container", name})
			}
		}
	}
	if err != nil {
		a.errf("\n  %s Docker %s\n\n  Is Docker running? Start it, then run: canyon %s\n", a.render.glyph(stateFail), plainly(err), a.form)
		return exitError
	}
	if len(made) > 0 {
		a.errf("\n  %s The demo is stopped, but these are left:\n", a.render.glyph(stateFail))
		for _, t := range made {
			a.errf("    %s %s\n", t.kind, t.name)
		}
		a.errf("\n  To try again, run: canyon %s\n", a.form)
		return exitError
	}
	return a.demoGone(dir, purge, string(before), kept)
}

// demoGone is the end of a down that left nothing of the demo's: the one line, the images it kept because something
// else on this Docker holds them, each with why, and the way back to the host up replaced, when it replaced one.
func (a *app) demoGone(dir string, purge bool, before string, kept []demoThing) int {
	if purge {
		if err := os.RemoveAll(dir); err != nil {
			a.errf("\n  %s %s could not be removed: %s\n", a.render.glyph(stateFail), dir, plainly(err))
			return exitError
		}
		a.printf("\n  %s The demo is gone: its containers, data and images, and its files in ~/.canyon/demo.\n", a.render.paint(ansiGreen, a.render.glyph(statePass)))
		if len(kept) > 0 {
			a.printf("\n  These images stay, because something else on this Docker holds them:\n")
			for _, t := range kept {
				a.printf("    %s %s\n", t.kind, t.name)
			}
		}
	} else {
		_ = os.Remove(demoHostBefore(dir)) // said below, once
		a.printf("\n  %s The demo is stopped and its data is gone. Its images stay, so the next start is quick.\n\n", a.render.paint(ansiGreen, a.render.glyph(statePass)))
		a.printf("  %-28s    %s\n", "canyon demo down --purge", "remove the images too, and get the disk back")
	}
	if host := strings.TrimSpace(before); host != "" {
		a.printf("\n  To use %s again: canyon login %[1]s\n", cell(host))
	}
	return exitOK
}
