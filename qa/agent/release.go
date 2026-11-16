package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// What deployed: prod leaves beside the transcripts: the package and image each release put in production, and what
// production runs once the turns are over, which expect prod: first release is judged against (#2160).
const (
	releasedFile = "released.json"
	prodLastFile = "prod-last.json"
)

// ran is what production runs, from the status read: its package and its image. Both name a release: a rebuild of the
// first release's code can carry its digest (pack's fixed timestamps) under a new package (#2784 review).
type ran struct {
	Package string `json:"package"`
	Digest  string `json:"digest"`
}

// released is what production ran after each of the runner's releases: the seed's, then the next release's.
type released struct {
	First ran `json:"first"`
	Last  ran `json:"last"`
}

// check fails the setup, never a score, where a turn could not be told apart from the releases: one the status read
// names no package or image of, or a next release that left production on the first's package or image.
func (r released) check() error {
	switch {
	case r.First.Package == "" || r.First.Digest == "":
		return fmt.Errorf("deployed: prod: the status read names no package or image for the first release: %+v", r.First)
	case r.Last.Package == "" || r.Last.Digest == "":
		return fmt.Errorf("deployed: prod: the status read names no package or image for the next release: %+v", r.Last)
	case r.Last.Package == r.First.Package || r.Last.Digest == r.First.Digest:
		return fmt.Errorf("deployed: prod: the next release left production on the first's package or image (%+v, then %+v), so no turn could be told apart from it", r.First, r.Last)
	}
	return nil
}

// release puts the seed, already in preview, in production as the tester would, then, when the scenario has a next
// release, writes it over the seed, commits it, deploys it to preview and puts it in production too (deployed: prod).
// Anything but a promotion that exits 0 and says PROMOTED stops the scenario before any turn.
func release(ctx context.Context, home, dir, app string, next []seedFile, keys *strings.Replacer) error {
	var r released
	var err error
	if r.First, err = promoteSeed(ctx, home, dir, app, 1); err != nil {
		return err
	}
	if len(next) > 0 { // expect prod: first release needs one (the parser's): without it, the first release is all there is
		if err := writeFiles(app, next, keys); err != nil {
			return err
		}
		if err := git(app, "add", "-A"); err != nil {
			return err
		}
		if err := git(app, "-c", "user.name=tester", "-c", "user.email=tester@example.com", "commit", "-qm", "the next release"); err != nil {
			return err
		}
		if err := deploySeed(ctx, home, dir, app, "setup-2.log"); err != nil {
			return err
		}
		if r.Last, err = promoteSeed(ctx, home, dir, app, 2); err != nil {
			return err
		}
		if err := r.check(); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(r)
	return os.WriteFile(filepath.Join(dir, releasedFile), raw, 0o600)
}

// promoteSeed is canyon deploy --env prod --yes as the tester, its output kept in setup-prod-<n>.log, and answers the
// package and image production runs after it: none is the setup's failure, never a score.
func promoteSeed(ctx context.Context, home, dir, app string, n int) (ran, error) {
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(home, "bin", "canyon"), "deploy", "--env", "prod", "--yes", "--format", "ai")
	cmd.Dir, cmd.Env = app, testerEnv(home, app)
	out, err := cmd.CombinedOutput()
	log := "setup-prod-" + strconv.Itoa(n) + ".log"
	if werr := os.WriteFile(filepath.Join(dir, log), out, 0o600); werr != nil {
		return ran{}, werr
	}
	if err != nil {
		return ran{}, fmt.Errorf("deployed: prod: canyon deploy --env prod: %w (see %s)", err, log)
	}
	if !promoted(parseBlocks(string(out))) {
		return ran{}, fmt.Errorf("deployed: prod: canyon deploy --env prod printed no PROMOTED (see %s)", log)
	}
	now, err := prodRuns(home, appOf(app))
	switch {
	case err != nil:
		return ran{}, fmt.Errorf("deployed: prod: %w", err)
	case now.Package == "" || now.Digest == "":
		return ran{}, fmt.Errorf("deployed: prod: release %d promoted, and the status read names no package or image in production (see %s)", n, log)
	}
	fmt.Printf("%s: release %d is in production\n", filepath.Base(app), n)
	return now, nil
}

// prodRuns is the package and image the app's production runs, from the status read as the tester; zero when nothing
// runs there.
func prodRuns(home, app string) (ran, error) {
	s, err := signedIn(home)
	if err != nil {
		return ran{}, err
	}
	a, err := s.call(http.MethodGet, "/api/v1/projects/"+app+"/status", nil)
	if err != nil {
		return ran{}, err
	}
	if a.status != http.StatusOK {
		return ran{}, fmt.Errorf("the status read: %s", a)
	}
	var st struct {
		Envs struct {
			Prod *struct {
				Package     string `json:"package"`
				ImageDigest string `json:"image_digest"`
			} `json:"prod"`
		} `json:"envs"`
	}
	if err := json.Unmarshal([]byte(a.said), &st); err != nil {
		return ran{}, fmt.Errorf("the status read: %w", err)
	}
	if st.Envs.Prod == nil {
		return ran{}, nil
	}
	return ran{Package: st.Envs.Prod.Package, Digest: st.Envs.Prod.ImageDigest}, nil
}

// prodLast keeps what production runs once the turns are over, however they end, as prod-last.json: its package and
// image, or why they could not be read, which never reads as a release.
func prodLast(home, dir, app string) {
	var raw []byte
	if now, err := prodRuns(home, appOf(app)); err != nil {
		raw, _ = json.Marshal(map[string]string{"unread": err.Error()})
	} else {
		raw, _ = json.Marshal(now)
	}
	if err := os.WriteFile(filepath.Join(dir, prodLastFile), raw, 0o600); err != nil {
		fmt.Printf("%s: %s: %v\n", filepath.Base(app), prodLastFile, err)
	}
}

// prodRelease is which of the runner's releases production ran once the turns were over: first release, last
// release, another version, nothing in production, or not read. Fail closed: a file missing or out of shape is not read.
func prodRelease(dir string) string {
	var r released
	var last struct {
		ran
		Unread string `json:"unread"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, releasedFile))
	if err != nil || json.Unmarshal(raw, &r) != nil || r.First.Package == "" || r.First.Digest == "" {
		return "not read (" + releasedFile + ")"
	}
	raw, err = os.ReadFile(filepath.Join(dir, prodLastFile))
	if err != nil || json.Unmarshal(raw, &last) != nil || last.Unread != "" {
		return "not read (" + prodLastFile + ")"
	}
	switch last.ran {
	case ran{}:
		return "nothing in production"
	case r.First: // the package and the image both: a rebuild of its code under a new package is another version
		return "first release"
	case r.Last:
		return "last release"
	}
	return "another version"
}

// git runs one git command in the app's directory.
func git(app string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = app
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, out)
	}
	return nil
}

// prodExpects are what expect prod: may name: whether any turn promoted, or which release production runs at the end.
var prodExpects = []string{"promoted", "not promoted", "first release"}

func validProd(value string) bool { return slices.Contains(prodExpects, value) }
