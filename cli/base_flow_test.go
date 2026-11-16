package cli

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stale is the upload's 409 with every field (routes.md row 57), moved fourteen minutes ago.
func stale(t *testing.T) *gateHost {
	t.Helper()
	at := time.Now().Add(-14 * time.Minute).UTC().Format(time.RFC3339)
	return &gateHost{run: aRun("clean", 0, 0), upload: reply{http.StatusConflict,
		`{"error":"stale_base","message":"pull, or force","head_package":"pkg_9f2a1c","moved_by":"ada@meridian.example","moved_at":"` + at + `"}`}}
}

// A deploy sends the base this directory recorded, and only that: none is no header, and a base
// recorded under another app is none. It records the package the 201 named, so the next upload
// sends what the host now holds.
func TestADeploySendsTheBaseAndRecordsThe201(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true)}
	if _, errOut, code := verbingIn(t, h, dir, "deploy"); code != exitOK || h.base != "" || h.query != "" || errOut != "" { // a fresh clone: no header and no query
		t.Fatalf("a first deploy: exit %d, base header %q, query %q, stderr %q", code, h.base, h.query, errOut)
	}
	if got := baseIn(t, dir, "claims-portal"); got != "pkg_1" { // the 201's id
		t.Fatalf("after a deploy the base is %q, want pkg_1", got)
	}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	verbingIn(t, h, dir, "deploy")
	if h.base != "pkg_3b7e40" || h.query != "" {
		t.Errorf("the second deploy sent base %q and query %q", h.base, h.query)
	}
	must(t, writeBase(dir, "other-app", "pkg_3b7e40"))
	if verbingIn(t, h, dir, "deploy"); h.base != "" {
		t.Errorf("another app's base reached the wire as %q", h.base)
	}
}

// A check's upload is review-only (#1251): it asks for that, sends no base whatever this directory
// recorded, and records none, so the deploy after it is based on the head it was and a teammate's
// directory is never made stale by it. It takes no --force: there is nothing for one to override.
func TestACheckSendsNoBaseAndRecordsNone(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0)}
	if _, errOut, code := verbingIn(t, h, dir, "check"); code != exitOK || h.base != "" || h.query != "check=true" || errOut != "" || baseIn(t, dir, "claims-portal") != "" {
		t.Fatalf("a first check: exit %d, base header %q, query %q, stderr %q, base recorded %q", code, h.base, h.query, errOut, baseIn(t, dir, "claims-portal"))
	}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	if _, errOut, code := verbingIn(t, h, dir, "check"); code != exitOK || h.base != "" || h.query != "check=true" || errOut != "" || baseIn(t, dir, "claims-portal") != "pkg_3b7e40" {
		t.Errorf("a check with a base: exit %d, base header %q, query %q, stderr %q, base now %q", code, h.base, h.query, errOut, baseIn(t, dir, "claims-portal"))
	}
	if _, errOut, code := verbingIn(t, h, dir, "check", "--force"); code != exitError || !strings.Contains(errOut, "-force") {
		t.Errorf("check --force: exit %d, stderr %q", code, errOut)
	}
}

// The 409 is a refusal, not a verdict: exit 1, the block on stderr naming the forced deploy,
// nothing on stdout under json, and the base left as it was — the upload did not happen.
func TestAStaleBaseRendersItsBlockAndExits1(t *testing.T) {
	dir, h, want := projectDir(t), stale(t), golden(t, "stale-base-deploy.txt") // read before the verb leaves this directory
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	out, errOut, code := verbingIn(t, h, dir, "deploy")
	if errOut != want || code != exitError {
		t.Errorf("a stale deploy exited %d and rendered\n%s\ntestdata wants\n%s", code, errOut, want)
	}
	if h.polled() || strings.Contains(out, "Package") {
		t.Errorf("the refusal went on: polled %v, stdout:\n%s", h.polled(), out)
	}
	if got := baseIn(t, dir, "claims-portal"); got != "pkg_3b7e40" {
		t.Errorf("the refused upload moved the base to %q", got)
	}
	out, errOut, code = verbingIn(t, h, dir, "deploy", "--format", "json")
	if code != exitError || out != "" || !strings.Contains(errOut, "canyon deploy --force") {
		t.Errorf("under json a stale deploy exited %d with stdout %q, stderr:\n%s", code, out, errOut)
	}
}

// A fresh clone records no base, and on an app with a current package the host refuses it as
// stale (#1503): the block names what is there, has no Yours line, and offers both exits; under
// --format ai the agent is told to ask.
func TestAFreshCloneOnAnAppWithAPackageRendersTheBlockWithoutYours(t *testing.T) {
	dir, h, want := projectDir(t), stale(t), golden(t, "stale-base-fresh-clone.txt")
	out, errOut, code := verbingIn(t, h, dir, "deploy")
	if errOut != want || code != exitError || h.base != "" {
		t.Errorf("a fresh clone's deploy sent base %q, exited %d and rendered\n%s\ntestdata wants\n%s", h.base, code, errOut, want)
	}
	if h.polled() || strings.Contains(out, "Package") || baseIn(t, dir, "claims-portal") != "" {
		t.Errorf("the refusal went on: polled %v, base %q, stdout:\n%s", h.polled(), baseIn(t, dir, "claims-portal"), out)
	}
	_, errOut, _ = verbingIn(t, h, dir, "deploy", "--format", "ai")
	if errOut != want+"\n    Do not run either. Show the person both lines and ask: one writes over the version ada@meridian.example deployed, the other over the person's own.\n" {
		t.Errorf("under ai a fresh clone's refusal rendered\n%s", errOut)
	}
}

// An empty moved_by carries the time alone, and a moved_at this build cannot read is a dash. (A host
// with no current package answers no stale base at all: contracts/routes.md, #1678.)
func TestThePartial409(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0)}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	h.upload = reply{http.StatusConflict, `{"error":"stale_base","message":"pull, or force","head_package":"pkg_9f2a1c","moved_by":"","moved_at":"yesterday"}`}
	if _, errOut, _ := verbingIn(t, h, dir, "deploy"); !strings.Contains(errOut, "    Theirs   pkg_9f2a1c   —\n") {
		t.Errorf("an empty moved_by beside an unreadable moved_at rendered\n%s", errOut)
	}
	if _, errOut, _ := verbingIn(t, h, dir, "deploy", "--format", "ai"); !strings.Contains(errOut, "one writes over the version deployed there now, the other") {
		t.Errorf("under ai an empty moved_by named someone:\n%s", errOut)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.upload = reply{http.StatusConflict, `{"error":"stale_base","message":"pull, or force","head_package":"pkg_9f2a1c","moved_by":"ada@meridian.example","moved_at":"` + now + `"}`}
	if _, errOut, _ := verbingIn(t, h, dir, "deploy"); !strings.Contains(errOut, "   ada@meridian.example, now\n") { // under a minute is "now", never "now ago"
		t.Errorf("a move seconds old rendered\n%s", errOut)
	}
	h.upload = reply{http.StatusConflict, `{"error":"stale_base","message":"pull, or force","head_package":"pkg_9f2a1c","moved_by":"ada\u009b31m@x","moved_at":"yesterday"}`}
	if _, errOut, _ := verbingIn(t, h, dir, "deploy"); strings.ContainsRune(errOut, 0x9b) || !strings.Contains(errOut, `ada\u009b31m@x`) { // an 8-bit CSI is spelled out like ESC
		t.Errorf("a C1 control in moved_by reached the terminal raw:\n%q", errOut)
	}
}

// --force sends the one query the route reads as force, the base still goes with it, and the
// package the 201 names is recorded like any other: one refusal never becomes a loop.
func TestForceSendsTheQueryAndRecordsThe201(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true)}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	if _, errOut, code := verbingIn(t, h, dir, "deploy", "--force"); code != exitOK || h.query != "force=true" || h.base != "pkg_3b7e40" || errOut != "" {
		t.Errorf("deploy --force: exit %d, query %q, base %q, stderr %q", code, h.query, h.base, errOut)
	}
	if got := baseIn(t, dir, "claims-portal"); got != "pkg_1" {
		t.Errorf("after deploy --force the base is %q, want pkg_1", got)
	}
}

// A base that cannot be written after a 201 is a warning: the upload succeeded, and the run's
// exit is the run's.
func TestABaseThatCannotBeWrittenWarnsAndDoesNotChangeTheExit(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true)}
	must(t, os.WriteFile(filepath.Join(dir, baseDir), []byte("not a directory"), 0o600))
	out, errOut, code := verbingIn(t, h, dir, "deploy")
	if code != exitOK || !strings.Contains(out, "✓ Review passed") {
		t.Fatalf("a deploy whose base could not be written exited %d:\n%s\n%s", code, out, errOut)
	}
	if !strings.HasPrefix(errOut, "\n  ! The base could not be recorded, so the next deploy from here will ask you to pull or use --force: ") || strings.Count(errOut, "\n") != 2 {
		t.Errorf("the warning is not one line on stderr:\n%q", errOut)
	}
}

// Only the 409 whose code is stale_base earns the block: another 409 is the plain error line,
// with the host's message and no exits offered. And --force in a directory with no base sends
// the query and no header, since there is nothing recorded to name.
func TestOtherConflictsAndAForceWithoutABase(t *testing.T) {
	dir, h := projectDir(t), &gateHost{run: aRun("clean", 0, 0), upload: reply{http.StatusConflict, `{"error":"label_taken","message":"the label is in use"}`}}
	must(t, writeBase(dir, "claims-portal", "pkg_3b7e40"))
	_, errOut, code := verbingIn(t, h, dir, "deploy")
	if code != exitError || !strings.Contains(errOut, "✗ the label is in use\n") || strings.Contains(errOut, "--force") {
		t.Errorf("a 409 that is not stale_base exited %d and rendered\n%s", code, errOut)
	}
	dir, h = projectDir(t), &gateHost{run: aRun("clean", 0, 0), page: buildPage(t, buildLines, true)}
	if _, errOut, code := verbingIn(t, h, dir, "deploy", "--force"); code != exitOK || h.query != "force=true" || h.base != "" || errOut != "" {
		t.Errorf("deploy --force with no base: exit %d, query %q, base header %q, stderr %q", code, h.query, h.base, errOut)
	}
}
