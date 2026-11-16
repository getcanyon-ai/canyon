package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The once-a-day notice, gh's shape (cli.md §8.5): after any verb, at a terminal, in human format,
// one line on stderr when the host was released with a newer canyon than this binary, and one when
// an installed skill block is older than the text this binary carries. The check runs in one
// goroutine while the verb works and is read once, without waiting, when the verb is done: an
// answer that has not arrived is dropped, the state file is left as it was, and the next verb asks
// again. No verb is ever slower for it.

const (
	noticeEvery  = 24 * time.Hour
	noticeWithin = 2 * time.Second
	noticeGrace  = 100 * time.Millisecond // how long a due run waits at its end for an answer still in flight
	noticeVar    = "CANYON_NO_UPDATE_NOTICE"
	noticeFile   = "notice.json"
	staleLimit   = 1 << 20 // a skill file past a mebibyte is not one this binary wrote, and is not read

	staleLine = "Your Canyon instructions are from an older canyon. Run: canyon skill install"
)

// noticeState is notice.json beside the config: when the last check finished and what the host
// said its cli was. Anything the file cannot say — absent, unreadable, not this JSON — is due now.
type noticeState struct {
	Checked time.Time `json:"checked"`
}

// noticeCheck is the version half of the pass, on a goroutine of its own: the host's answer, or ""
// when it gave none, and done for the tests that wait on it. The skill-block half needs no host
// and is read after the verb, so it is not here.
type noticeCheck struct {
	cli  chan string
	done chan struct{}
}

func noticePath(getenv func(string) string) (string, error) {
	path, err := configPath(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(path), noticeFile), nil
}

// quiet is every reason not to speak: a harness is reading (ai, json, or the variables §5.1
// detects), the person opted out, or stderr is not a terminal. It is read once the verb has
// parsed its flags, so `--format` given after the verb's name counts before any request is sent.
func (a *app) quiet() bool {
	return a.render.Format != FormatHuman || a.getenv(noticeVar) != "" || !a.stderrIsTerminal()
}

func (a *app) stderrIsTerminal() bool {
	if f, ok := a.err.(*os.File); ok {
		return IsTTY(f)
	}
	return a.tty
}

// noticeStart begins the pass, once per invocation, from the verb's parse: only when one is due
// and nothing silences it. Everything the goroutine needs is read here and handed over as values.
func (a *app) noticeStart() {
	if a.noticed {
		return
	}
	a.noticed = true
	if a.quiet() {
		return
	}
	path, err := noticePath(a.getenv)
	if err != nil {
		return
	}
	if last, ok := readNotice(path); ok && a.clock.now().Sub(last.Checked) < noticeEvery {
		return
	}
	host := a.versionHost()
	n := &noticeCheck{cli: make(chan string, 1), done: make(chan struct{})}
	go func() {
		defer close(n.done)
		n.cli <- hostCLI(host)
	}()
	a.notice = n
}

// noticeEnd is the one read, after the verb's own output. A verb is usually faster than the round
// trip started beside it, so a due run waits noticeGrace for an answer still in flight — once a
// day, and never longer: past it the answer is dropped, the file untouched, and the next verb
// asks again. The skill blocks are read here, after the verb, so `canyon skill install` never
// reports the block it has just written.
func (a *app) noticeEnd() {
	n := a.notice
	if n == nil || a.quiet() {
		return
	}
	var cli string
	select {
	case cli = <-n.cli:
	case <-time.After(noticeGrace):
		return
	}
	var lines []string
	if host, ok := releaseOf(cli); ok {
		if mine, _ := releaseOf(Version); slices.Compare(host[:], mine[:]) > 0 {
			lines = append(lines, "canyon "+cell(cli)+" is out; you have "+Version+". Download it from "+releases)
		}
	}
	if a.getenv("HOME") != "" && staleBlock(a.skillTargets()) { // no HOME, and ~ would resolve under the working directory
		lines = append(lines, staleLine)
	}
	if len(lines) > 0 {
		a.errf("\n")
	}
	for _, line := range lines {
		a.errf("  %s %s\n", a.render.glyph(statePending), line)
	}
	writeNotice(a.getenv, noticeState{Checked: a.clock.now()})
}

func (a *app) versionHost() string {
	if _, ok := releaseOf(Version); !ok {
		return ""
	}
	host, err := a.host()
	if err != nil {
		return ""
	}
	return host
}

// release is the MAJOR.MINOR.PATCH a stamped version begins with. The Makefile stamps `git
// describe`, so a tag is `v0.5.0` and a commit past it `v0.5.0-3-gabcdef`: only the tag is compared,
// numerically, and a version with no tag in front — "dev", a bare sha — never notices.
// releaseOf is the MAJOR.MINOR.PATCH a version string begins with: an optional v, and anything
// from the first "-" on dropped, so git describe's v0.5.0-3-gabcdef reads as 0.5.0. A string with
// no release in front — dev, a bare commit — is no release, and never notices.
func releaseOf(v string) ([3]int, bool) {
	var n [3]int
	tag, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		k, err := strconv.Atoi(p)
		if err != nil || k < 0 {
			return n, false
		}
		n[i] = k
	}
	return n, true
}

// hostCLI is GET /auth/config's cli, on a client of the pass's own, within noticeWithin. Every way
// the host can fail to say it is one answer: "".
func hostCLI(host string) string {
	if host == "" {
		return ""
	}
	var auth authConfig
	if newClient(host).do(context.Background(), http.MethodGet, "/auth/config", "", nil, 0, nil, http.StatusOK, noticeWithin, answerLimit, &auth) != nil {
		return ""
	}
	return auth.CLI
}

// staleBlock is whether an installed skill block carries a hash other than this binary's: the
// files skill install writes, read the way it reads them. A file that is not there, or not one
// findBlock can read, is not stale — skill install is the verb that says what is wrong with it.
// staleBlock is whether an installed skill block carries a hash other than this binary's: the
// files skill install writes, read the way it reads them. A file that is not there, not one
// findBlock can read, or past the size any of them has — skill install is the verb that says what
// is wrong with it — is not stale.
func staleBlock(targets []skillTarget) bool {
	want := stamp(skillBody)
	for _, t := range targets {
		f, err := os.Open(t.path) //nolint:gosec // skill install's own three paths, none from input
		if err != nil {
			continue
		}
		file, err := io.ReadAll(io.LimitReader(f, staleLimit+1))
		_ = f.Close()
		if err != nil || len(file) > staleLimit {
			continue
		}
		if ref, found, err := findBlock(t.shown, string(file)); err == nil && found && ref.Hash != want {
			return true
		}
	}
	return false
}

func readNotice(path string) (noticeState, bool) {
	var s noticeState
	body, err := os.ReadFile(path) //nolint:gosec // beside this machine's own config, at a path composed from XDG_CONFIG_HOME or HOME and nothing else
	if err != nil || json.Unmarshal(body, &s) != nil || s.Checked.IsZero() {
		return s, false
	}
	return s, true
}

// writeNotice records the pass. A write that fails is not reported: the verb has already said what
// it had to, and the next verb asks the host again.
func writeNotice(getenv func(string) string, s noticeState) {
	path, err := noticePath(getenv)
	if err != nil {
		return
	}
	body, err := json.Marshal(s)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	_ = atomicWrite(path, body, true)
}
