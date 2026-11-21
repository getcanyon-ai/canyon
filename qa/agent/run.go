package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	parallel    = 2 // a laptop runs two headless sessions at once, not more
	turnTimeout = 30 * time.Minute
	settleFor   = 15 * time.Minute // a deploy or promote run left going by a cut-off turn
	// askedFor bounds a turn's wait for Canyon's patch: the image scan after Live, a rescan tick, the
	// patch run's rebase, preview's watch and its wave, on a host whose CANYON_PATCH_INTERVAL and
	// CANYON_PATCH_WATCH are short (design/PATCHING.md §4.8).
	askedFor = time.Hour
	// refusedFor bounds a turn's wait for Canyon to refuse a call the app makes on its own and file an egress
	// request: the mode reaching egress-control's next policy (5 s), the call, the flush (10 s). allowedFor bounds
	// a person's Allow in the dashboard; reachedFor the app's next call after it, and its flush.
	refusedFor = 10 * time.Minute
	allowedFor = time.Hour
	reachedFor = 3 * time.Minute
)

var (
	settleEvery = 15 * time.Second // between undeploy asks while a run is going; a var so a test need not wait
	patchEvery  = 20 * time.Second // between status reads while a turn waits for Canyon's patch
)

// tools are what the tester lets the agent run without a prompt: the stacks' own tools, canyon,
// and read-only shell. Anything else is denied and shows on the scorecard.
var tools = []string{
	"Bash(canyon:*)", "Bash(git:*)", "Bash(ls:*)", "Bash(cat:*)", "Bash(mkdir:*)", "Bash(cd:*)",
	"Bash(curl:*)", "Bash(grep:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(sleep:*)",
	"Bash(npm:*)", "Bash(npx:*)", "Bash(node:*)",
	"Bash(python3:*)", "Bash(python:*)", "Bash(pip:*)", "Bash(pip3:*)",
	"Bash(go:*)", "Bash(gofmt:*)",
	"Read", "Edit", "Write", "Glob", "Grep",
	"mcp__canyon", // every tool of Canyon's MCP server, which skill install configures (design/specs/mcp.md §1.4)
}

// runAll runs every scenario (two at a time), each turn a headless Claude Code or Codex session under the
// signed-in tester's HOME, then scores the run.
func runAll(home, runDir, suffix string, keepApps bool, files []string) error {
	// both absolute before any path is built from them: canyon and claude run in the app, where a relative one names nothing
	runDir, err := filepath.Abs(runDir)
	if err != nil {
		return err
	}
	if home, err = filepath.Abs(home); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "canyon")); err != nil {
		return fmt.Errorf("the tester HOME has no bin/canyon: %w", err)
	}
	var list []scenario
	for _, f := range files {
		s, err := parseScenario(f)
		if err != nil {
			return err
		}
		if name := appName(s.Name, suffix); len(name) > 55 { // before any turn, so a tooling mistake never reads as the agent's
			return fmt.Errorf("%s: an app name is at most 55 characters (contracts/routes.md, POST /api/v1/projects); shorten --suffix", name)
		}
		list = append(list, s)
	}
	if err := os.MkdirAll(runTmp(runDir), 0o700); err != nil { // every tester process's TMPDIR (testerEnv)
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM) // ctrl-c ends the turns, and each app is still taken offline
	defer stop()
	if slices.ContainsFunc(list, func(s scenario) bool { return s.Agent == agentCursor }) { // once, before any scenario starts
		if err := prepareCursor(ctx, home); err != nil {
			return err
		}
	}
	context.AfterFunc(ctx, stop) // the first signal cancels; a second one ends the runner as usual
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	errs := make([]error, len(list))
	for i, s := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = runOne(ctx, home, runDir, files[i], s, suffix, keepApps)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil { // scrubbed: a Cursor process's message may carry its key
			errs[i] = errors.New(scrubbed(appName(list[i].Name, suffix) + ": " + err.Error()))
		}
	}
	return errors.Join(append(errs, score(runDir))...) // score what ran, and still fail the batch
}

// appName is the scenario's app on the host: qa-<scenario>, and -<suffix> when a rerun asks for fresh
// apps, since a fresh copy cannot deploy over an app another copy made (cli.md, the base guard).
func appName(scenario, suffix string) string {
	if suffix == "" {
		return "qa-" + scenario
	}
	return "qa-" + scenario + "-" + suffix
}

func runOne(ctx context.Context, home, runDir, file string, s scenario, suffix string, keepApps bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Join(runDir, s.Name)
	name := appName(s.Name, suffix)
	app := filepath.Join(dir, name)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s exists: use a new run directory", dir)
	}
	if err := os.MkdirAll(app, 0o700); err != nil {
		return err
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "scenario.md"), src, 0o600); err != nil {
		return err
	}
	if err := hostDBProd(home, s.DBProd); err != nil { // before the app is made: a host set otherwise runs no turn
		return err
	}
	if err := claim(home, name, s.Rules); err != nil {
		return err
	}
	// the app the tester just made, named in canyon.toml as a teammate's clone names it: without the
	// line, a first deploy makes an app of its own (cli.md §3.2); a seed's canyon.toml replaces it
	if err := os.WriteFile(filepath.Join(app, "canyon.toml"), []byte("app = \""+name+"\"\n"), 0o600); err != nil {
		return err
	}
	if !keepApps { // its transcripts are the record; a batch left running filled the test cluster (2026-10-08)
		defer undeploy(home, dir, app)
	}
	keys, err := fakeKeys(dir, src, name)
	if err != nil {
		return err
	}
	if err := seed(app, s.Seed, keys); err != nil {
		return err
	}
	if err := setSecrets(ctx, home, dir, app, s.Secrets); err != nil { // before the deploy, so the running version reads them
		return err
	}
	if s.Deployed {
		if err := deploySeed(ctx, home, dir, app, "setup.log"); err != nil {
			return err
		}
	}
	if s.Released { // deployed: prod: the seed in production, then the next release over it (#2160)
		if err := release(ctx, home, dir, app, s.Next, keys); err != nil {
			return err
		}
		defer prodLast(home, dir, app) // once the turns are over, however they end, before the app is taken offline
	}
	if err := restrict(home, name, s.Egress); err != nil {
		return err
	}
	if s.Agent == agentCodex {
		if err := installCodex(home, app); err != nil {
			return err
		}
		defer func() { // the trust Codex gives the app on turn 1 goes with the scenario, so batches don't pile it up
			if err := forgetProject(home, app); err != nil {
				fmt.Printf("%s: Codex still trusts %s: %v\n", name, app, err)
			}
		}()
	}
	if s.MCP { // after Codex's install, which writes its MCP config: a run with no server scores no tools
		if err := mcpConfigured(home, s.Agent); err != nil {
			return err
		}
	}
	if s.Playbook != "" {
		if err := playbookExists(home, s.Playbook); err != nil {
			return err
		}
		defer func() { // what the session hook synced, once the turns are over, however they end
			if err := recordSkill(home, dir, s.Agent, s.Playbook); err != nil {
				fmt.Printf("%s: the playbook's skill was not recorded: %v\n", name, err)
			}
		}()
	}
	var cursor cursorApp
	if s.Agent == agentCursor {
		if cursor, err = setUpCursorApp(home, dir, app); err != nil {
			return err
		}
	}
	var refused *outboundRead // what the refusal wait read: the allow wait's and the last read's yardstick
	for i, t := range s.Turns {
		if t.OnceRefused {
			fmt.Printf("%s turn %d: waiting for Canyon to refuse a call the app makes out\n", name, i+1)
			body, err := waitRead(ctx, home, name, outboundPath(name), "outbound reads", "Canyon filed no egress request", refusedFor, refusedYet)
			if err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
			if refused == nil { // the first refusal is the run's: what an allow waits for, and what a reach comes after
				if err := os.WriteFile(filepath.Join(dir, outboundFirst), body, 0o600); err != nil {
					return err
				}
				if refused = readOutbound(filepath.Join(dir, outboundFirst)); refused == nil {
					return fmt.Errorf("turn %d: %s does not read", i+1, outboundFirst)
				}
				defer lastRead(ctx, home, dir, name, s.Last, refused) // once the turns are over, however they end
			}
		}
		if t.OnceAllowed {
			host := ""
			if sess, err := signedIn(home); err == nil {
				host = sess.host
			}
			fmt.Printf("%s turn %d: waiting for a person on the app's team to allow %s, in Canyon's dashboard: %s/#/apps/%s/outbound\n",
				name, i+1, waitingFor(*refused), host, name)
			if _, err := waitRead(ctx, home, name, outboundPath(name), "outbound reads", "no person allowed it", allowedFor, allowedYet(*refused)); err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
		}
		if last, ok := unfixed(dir, s.Agent); t.IfUnfixed && !ok { // nothing to fix: the agent already did, or never reached Canyon
			why := "nothing to fix (last result " + last + ")"
			fmt.Printf("%s turn %d: skipped, %s\n", name, i+1, why)
			if err := os.WriteFile(filepath.Join(dir, "turn-"+strconv.Itoa(i+1)+".skipped"), []byte(why+"\n"), 0o600); err != nil {
				return err
			}
			continue
		}
		if t.OnceAsked {
			fmt.Printf("%s turn %d: waiting for Canyon to ask to approve its fix for production\n", name, i+1)
			if err := waitAsked(ctx, home, appOf(app), askedFor); err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
		}
		if err := turn(ctx, s.Agent, home, dir, app, i+1, keys.Replace(t.Text)); err != nil {
			return err
		}
		if s.Agent == agentCodex && i == 0 { // fail closed: a Codex that skipped Canyon's hook is no run of it
			if err := codexHookRan(home, dir); err != nil {
				return fmt.Errorf("turn 1: %w", err)
			}
		}
		if s.Agent == agentCodex { // and none that trusts a hook the agent made
			if err := codexTrustsOnlyCanyon(home, app); err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
		}
		if s.Agent == agentCursor { // no hook or permission the agent made, and fail closed: a Cursor Canyon's manifest never reached is no run of it
			if err := cursorUnchanged(home, app, cursor); err != nil {
				return fmt.Errorf("turn %d: %w", i+1, err)
			}
			if i == 0 { // ponytail: always stops until the probe's transcript gives it a reader to read the manifest with
				return fmt.Errorf("turn 1: %w", cursorManifestRead(dir))
			}
		}
	}
	return nil
}

// fakeKeys builds a key for each placeholder the scenario uses and keeps them, mode 600, beside
// the transcripts so the scorer can redact them. They are never printed. {{APP}} is the app's name,
// for a seed that names it (canyon.toml).
func fakeKeys(dir string, src []byte, app string) (*strings.Replacer, error) {
	pairs, kept := []string{"{{APP}}", app}, []string(nil)
	for _, p := range placeholders {
		if !strings.Contains(string(src), p) {
			continue
		}
		b := make([]byte, 14)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		key := "sk_" + "live_" + hex.EncodeToString(b)
		pairs, kept = append(pairs, p, key), append(kept, key)
	}
	if len(kept) > 0 {
		if err := os.WriteFile(filepath.Join(dir, ".fakekey"), []byte(strings.Join(kept, "\n")), 0o600); err != nil {
			return nil, err
		}
	}
	return strings.NewReplacer(pairs...), nil
}

// seed writes the user's existing app and commits it, as a user's repo would be; a file that held
// a placeholder is mode 600.
func seed(app string, files []seedFile, keys *strings.Replacer) error {
	if len(files) == 0 {
		return nil
	}
	if err := writeFiles(app, files, keys); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=tester", "-c", "user.email=tester@example.com", "commit", "-qm", "our app"},
	} {
		if err := git(app, args...); err != nil {
			return err
		}
	}
	return nil
}

// writeFiles writes files into the app, each placeholder replaced; a file that held a key is mode 600.
func writeFiles(app string, files []seedFile, keys *strings.Replacer) error {
	for _, f := range files {
		path := filepath.Join(app, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if slices.ContainsFunc(placeholders, func(p string) bool { return strings.Contains(f.Body, p) }) {
			mode = 0o600 // it holds a key; {{APP}} alone is no secret
		}
		body := keys.Replace(f.Body)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			return err
		}
	}
	return nil
}

// turn is one user message: claude -p, codex exec or Cursor's agent -p, in the app directory under the tester's
// HOME and nothing else of the operator's environment but where the agent is installed, and Cursor's key.
func turn(ctx context.Context, agent, home, dir, app string, n int, text string) (err error) {
	name := "claude"
	args := []string{"-p", text, "--output-format", "stream-json", "--verbose", "--include-hook-events",
		"--permission-mode", "acceptEdits", "--allowedTools"}
	args = append(args, tools...)
	if n > 1 {
		args = append(args, "--continue")
	}
	if agent == agentCodex {
		var thread string
		if n > 1 { // turn 1 always runs: no marker may hold it back
			var err error
			if thread, err = codexThread(dir); err != nil {
				return fmt.Errorf("turn %d: %w", n, err)
			}
		}
		name, args = "codex", codexArgs(text, thread)
	}
	env := testerEnv(home, app)
	if agent == agentCursor {
		chat, cerr := cursorChat(dir) // not err: the scrub below sets the named result
		if cerr != nil {
			return fmt.Errorf("turn %d: %w", n, cerr)
		}
		name, args, env = cursorBin(home), cursorArgs(text, chat, app), cursorEnv(home, app)
		defer func() { // last, once both files are closed, whatever the turn's end: a turn left unscrubbed fails
			if serr := scrubCursorKey(dir, n); serr != nil {
				err = errors.Join(err, fmt.Errorf("turn %d: %s not scrubbed from its transcript: %w", n, cursorKeyName, serr))
			}
		}()
	}
	out, err := os.OpenFile(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	errf, err := os.OpenFile(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".stderr"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = errf.Close() }()
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.Env = app, out, errf, env
	began := time.Now()
	err = cmd.Run()
	took := time.Since(began).Round(time.Second)
	if agent != agentClaude { // Codex's events carry no duration, nor does a Cursor turn the scorer cannot read yet: it reads the runner's
		if werr := os.WriteFile(filepath.Join(dir, "turn-"+strconv.Itoa(n)+".wall"), []byte(took.String()+"\n"), 0o600); werr != nil {
			return werr
		}
	}
	if err != nil { // a later --continue or resume would resume a broken session: stop the scenario
		return fmt.Errorf("turn %d: %s: %w (see turn-%d.stderr)", n, name, err, n)
	}
	fmt.Printf("%s turn %d: done in %s\n", filepath.Base(app), n, took)
	return nil
}

// testerEnv is the tester's environment and nothing else of the operator's but where claude and codex
// are installed: the turns and the runner's own canyon calls run under it alike. Codex finds its
// sign-in in the tester's ~/.codex. TMPDIR is the run directory's tmp/, two up from the app
// (<run-dir>/<name>/<app>), so the packages the CLI builds there stay with the run, never in the
// operator's temp directory.
func testerEnv(home, app string) []string {
	return []string{
		"PATH=" + strings.Join([]string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), filepath.Join(os.Getenv("HOME"), ".local", "bin"),
			"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}, ":"),
		"HOME=" + home, "USER=" + os.Getenv("USER"), "TERM=xterm", "LANG=en_US.UTF-8",
		"TMPDIR=" + runTmp(filepath.Dir(filepath.Dir(app))),
	}
}

// runTmp is the run directory's tmp/, made before any scenario.
func runTmp(runDir string) string { return filepath.Join(runDir, "tmp") }

// unfixed is the last review result the turns so far printed ("none" before any), and whether it
// leaves the agent something to fix: BLOCKED or FAILED. A promote's own result is no review to fix.
func unfixed(dir, agent string) (string, bool) {
	last := "none"
	for _, st := range readTranscript(dir, agent).Steps {
		if st.Result != nil && st.Result.Kind == "REVIEW" {
			last = st.Result.Result
		}
	}
	return last, last == "BLOCKED" || last == "FAILED"
}

// setSecrets sets each secret: line's value as the tester would, canyon secret set NAME --env <env> in the app, the
// value on stdin. The value is made up here, sk-proj- and 40 random hex digits, the shape of an OpenAI key no account
// holds, so a call that sends it is answered 401 by the service and never charged; it is never printed, logged or
// written. The CLI's answer is kept in secrets.log, which names the secret and never its value. A refusal stops the
// scenario before any turn.
func setSecrets(ctx context.Context, home, dir, app string, secrets []seededSecret) error {
	var said []byte
	for _, sec := range secrets {
		raw := make([]byte, 20)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, filepath.Join(home, "bin", "canyon"), "secret", "set", sec.Name, "--env", sec.Env)
		cmd.Dir, cmd.Env, cmd.Stdin = app, testerEnv(home, app), strings.NewReader("sk-"+"proj-"+hex.EncodeToString(raw)+"\n")
		out, err := cmd.CombinedOutput()
		said = append(said, out...)
		if werr := os.WriteFile(filepath.Join(dir, "secrets.log"), said, 0o600); werr != nil {
			return werr
		}
		if err != nil {
			return fmt.Errorf("secret: canyon secret set %s --env %s: %w (see secrets.log)", sec.Name, sec.Env, err)
		}
		fmt.Printf("%s: %s set in %s, a made-up value\n", filepath.Base(app), sec.Name, sec.Env)
	}
	return nil
}

// deploySeed deploys the user's app to preview as the tester before turn 1 (deployed: preview), as the user had:
// canyon deploy in the app under the tester's environment, its output kept in log. Anything but a review that
// PASSED and an exit 0 stops the scenario before any turn, so a tooling failure never reads as the agent's.
func deploySeed(ctx context.Context, home, dir, app, log string) error {
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(home, "bin", "canyon"), "deploy", "--format", "ai")
	cmd.Dir, cmd.Env = app, testerEnv(home, app)
	out, err := cmd.CombinedOutput()
	if werr := os.WriteFile(filepath.Join(dir, log), out, 0o600); werr != nil {
		return werr
	}
	if err != nil {
		return fmt.Errorf("deployed: canyon deploy: %w (see %s)", err, log)
	}
	if !slices.ContainsFunc(parseBlocks(string(out)), func(b *block) bool { return b.Kind == "REVIEW" && b.Result == "PASSED" }) {
		return fmt.Errorf("deployed: canyon deploy printed no review that PASSED (see %s)", log)
	}
	fmt.Printf("%s: the seed is deployed to preview\n", filepath.Base(app))
	return nil
}

// lastRead keeps the app's outbound records once the turns are over, as outbound-last.json, the last expect's
// evidence: read again until it holds what an outbound last expect names, for reachedFor at most, and kept either way.
func lastRead(ctx context.Context, home, dir, app string, last expect, first *outboundRead) {
	body, err := waitRead(ctx, home, app, outboundPath(app), "outbound reads", "the records did not show "+last.String(), reachedFor, reachedYet(last, first))
	if err != nil {
		fmt.Println(err) // it names the app
	}
	if body != nil {
		if err := os.WriteFile(filepath.Join(dir, outboundLast), body, 0o600); err != nil {
			fmt.Printf("%s: %v\n", app, err)
		}
	}
}
