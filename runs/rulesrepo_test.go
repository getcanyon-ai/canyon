package runs

import (
	"bytes"
	"cmp"
	"compress/zlib"
	"context"
	"crypto/sha1" //nolint:gosec // a pack's own checksum is SHA-1
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/cgi" //nolint:gosec // the test's git server runs git's own http-backend; nothing here takes a request's Proxy header
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// rulesRepo serves a one-commit repo of files and branches more branches over TLS with git's http-backend,
// wrapped by wrap, and points the fetch at it: example.com resolves public, dials reach it, its cert is trusted.
func rulesRepo(t *testing.T, files map[string]string, branches int, wrap func(http.Handler) http.Handler) (repo, commit string, srv *httptest.Server) {
	t.Helper()
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...) //nolint:gosec // the test's own git commands
		cmd.Dir, cmd.Env = dir, append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	src, root := t.TempDir(), t.TempDir()
	git(src, "init", "-q")
	for name, body := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		var err error
		if target, ok := strings.CutPrefix(body, "symlink:"); ok {
			err = os.Symlink(target, p)
		} else if bin, ok := strings.CutPrefix(body, "exec:"); ok {
			err = os.WriteFile(p, []byte(bin), 0o700) //nolint:gosec // a 100755 entry, as a repo can hold one
		} else if !strings.HasPrefix(body, "gitlink:") {
			err = os.WriteFile(p, []byte(body), 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	git(src, "add", "-A")
	for name, body := range files {
		if sha, ok := strings.CutPrefix(body, "gitlink:"); ok { // a submodule's entry, its commit nowhere in the repo
			git(src, "update-index", "--add", "--cacheinfo", "160000,"+sha+","+name)
		}
	}
	git(src, "commit", "-q", "-m", "rules")
	commit = git(src, "rev-parse", "HEAD")
	git(root, "clone", "-q", "--bare", src, "rules.git")
	git(filepath.Join(root, "rules.git"), "config", "uploadpack.allowAnySHA1InWant", "true")
	if branches > 0 {
		var refs strings.Builder
		for i := range branches {
			_, _ = fmt.Fprintf(&refs, "create refs/heads/branch-with-a-long-name-%05d %s\n", i, commit)
		}
		cmd := exec.Command("git", "update-ref", "--stdin")
		cmd.Dir, cmd.Stdin = filepath.Join(root, "rules.git"), strings.NewReader(refs.String())
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("update-ref: %v %s", err, out)
		}
	}
	var h http.Handler = &cgi.Handler{Path: filepath.Join(git(src, "--exec-path"), "git-http-backend"), Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	if wrap != nil {
		h = wrap(h)
	}
	srv = httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	wasResolve, wasDial, wasRoots := resolve, dial, roots
	resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	roots = pool
	t.Cleanup(func() { resolve, dial, roots = wasResolve, wasDial, wasRoots })
	return "https://example.com/rules.git", commit, srv
}

// statusOnly is a host that answers every request with status, as a git host does to a stranger.
func statusOnly(status int) func(http.Handler) http.Handler {
	return func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	}
}

// The org's rules are the commit's regular .yml and .yaml files (100644 or 100755) outside dot paths, by
// their paths: never a workflow, a symlink, a submodule or a file under a hidden directory. Kept by (repo, commit), a second
// read fetches nothing: it answers with the server gone.
func TestTheOrgsRulesAreTheCommitsRulesFilesAndKept(t *testing.T) {
	repo, commit, srv := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n", "sub/b.yml": "rules: []\n", ".github/workflows/ci.yml": "on: push\n",
		"sub/.hidden/c.yaml": "rules: []\n", "docs/readme.md": "hi\n", "link.yaml": "symlink:a.yaml", "x.yaml": "exec:rules: []\n", "mod.yaml": "gitlink:" + strings.Repeat("b", 40)}, 0, nil)
	p := &Pipeline{Repos: t.TempDir()}
	for i := range 2 {
		files, err := p.orgRules(t.Context(), repo, commit, nil)
		if got := slices.Sorted(maps.Keys(files)); err != nil || !slices.Equal(got, []string{"a.yaml", "sub/b.yml", "x.yaml"}) || string(files["a.yaml"]) != "rules: []\n" {
			t.Fatalf("read %d: %q %v", i+1, got, err)
		}
		srv.Close()
	}
}

// A commit whose write fails is not kept, so no later read decodes half of it: its file is opened for
// reading only, and every write to it fails.
func TestACommitWhoseWriteFailsIsNotKept(t *testing.T) {
	repo, commit, _ := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n"}, 0, nil)
	was := createTemp
	t.Cleanup(func() { createTemp = was })
	createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		_ = f.Close()
		return os.Open(f.Name())
	}
	p := &Pipeline{Repos: t.TempDir()}
	if files, err := p.orgRules(t.Context(), repo, commit, nil); err == nil || files != nil {
		t.Errorf("a failed write answered %v, %v", slices.Collect(maps.Keys(files)), err)
	}
	if kept, _ := filepath.Glob(filepath.Join(p.Repos, ".opengrep-rules", "*", "*")); len(kept) != 0 {
		t.Fatalf("a failed write kept %v", kept)
	}
}

// A fetch that fails says why, in the contract's words, and keeps nothing.
func TestAFetchThatFailsSaysWhy(t *testing.T) {
	for name, c := range map[string]struct {
		files    map[string]string
		branches int
		wrap     func(http.Handler) http.Handler
		repo     string
		auth     transport.AuthMethod
		set      func()
		want     unfetched
	}{
		"a host resolving to one private address among public ones": {set: func() {
			resolve = func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("10.0.0.7")}, nil
			}
		}, want: notPublic},
		"over the cap as received, a small pack under a long advertisement": {branches: 2000, set: func() { rulesCap = 64 << 10 }, want: tooBig},
		"over the cap as unpacked": {files: map[string]string{"big.yaml": strings.Repeat("x", 1<<20)}, set: func() { rulesCap = 64 << 10 }, want: tooBig},
		"over 60 seconds": {wrap: func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(time.Second); h.ServeHTTP(w, r) })
		}, set: func() { rulesTimeout = 200 * time.Millisecond }, want: tooSlow},
		"no rules file": {files: map[string]string{"readme.md": "hi\n"}, want: noRules},
		"a redirect": {repo: "https://example.com/moved.git", wrap: func(h http.Handler) http.Handler { // moved.git's refs redirect to rules.git's; its packs are served
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rest, ok := strings.CutPrefix(r.URL.Path, "/moved.git/"); ok && r.Method == http.MethodGet {
					http.Redirect(w, r, "/rules.git/"+rest+"?"+r.URL.RawQuery, http.StatusFound) //nolint:gosec // the redirect under test, to the test's own server
					return
				}
				r.URL.Path = strings.Replace(r.URL.Path, "/moved.git/", "/rules.git/", 1)
				h.ServeHTTP(w, r)
			})
		}, want: redirected},
		"a certificate the system does not trust": {set: func() { roots = nil }, want: badCert},
		"a repository the host does not have":     {repo: "https://example.com/missing.git", wrap: statusOnly(http.StatusNotFound), want: notFound},
		"a host that asks for a credential":       {wrap: statusOnly(http.StatusUnauthorized), want: asksForLogin},
		"a host that refuses access":              {wrap: statusOnly(http.StatusForbidden), want: asksForLogin},
		"a host that refuses the credential":      {wrap: statusOnly(http.StatusUnauthorized), auth: &githttp.BasicAuth{Username: "u", Password: "not-it"}, want: refusedLogin},
	} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			wasCap, wasTimeout := rulesCap, rulesTimeout
			t.Cleanup(func() { rulesCap, rulesTimeout = wasCap, wasTimeout })
			if c.files == nil {
				c.files = map[string]string{"a.yaml": "rules: []\n"}
			}
			repo, commit, _ := rulesRepo(t, c.files, c.branches, c.wrap)
			if c.set != nil {
				c.set()
			}
			p := &Pipeline{Repos: t.TempDir()}
			if _, err := p.orgRules(t.Context(), cmp.Or(c.repo, repo), commit, c.auth); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if kept, _ := filepath.Glob(filepath.Join(p.Repos, ".opengrep-rules", "*", "*")); len(kept) != 0 {
				t.Fatalf("a failed fetch kept %v", kept)
			}
		})
	}
}

// A server that answers another commit than the one asked is refused: the commit is read by its id. It
// is served the want rewritten to its own commit, so the pack it sends is sound, and not the one asked.
func TestAnotherCommitAnsweredIsRefused(t *testing.T) {
	asked, answered := strings.Repeat("a", 40), ""
	repo, commit, _ := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n"}, 0, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(bytes.ReplaceAll(body, []byte(asked), []byte(answered))))
			}
			h.ServeHTTP(w, r)
		})
	})
	answered = commit
	if _, err := (&Pipeline{Repos: t.TempDir()}).orgRules(t.Context(), repo, asked, nil); !errors.Is(err, noCommit) {
		t.Fatalf("another commit answered: %v", err)
	}
}

// A delta over the cap is over it before its stream is inflated, whether it builds past the cap (the sum
// never wraps) or its own stream is past it: no more of the pack than the cap is read. The pack is built
// by hand, a blob and then an offset delta on it, its streams stored, not compressed, so what is read is
// what is inflated.
func TestADeltaOverTheCapIsOverItBeforeItIsInflated(t *testing.T) {
	was := rulesCap
	t.Cleanup(func() { rulesCap = was })
	rulesCap = 64 << 10
	const base = "rules: []\n"
	for name, delta := range map[string][]byte{
		"building nearly 2^63 bytes":        {byte(len(base)), 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
		"a stream of 1 MiB building 1 byte": append([]byte{byte(len(base)), 1}, make([]byte, 1<<20)...),
	} {
		t.Run(strings.ReplaceAll(name, " ", "_"), func(t *testing.T) {
			pack := bytes.NewBufferString("PACK\x00\x00\x00\x02\x00\x00\x00\x02")
			object := func(typ byte, data, extra []byte) {
				b, n := typ<<4|byte(len(data)&0x0f), len(data)>>4
				for ; n > 0; n >>= 7 {
					pack.WriteByte(b | 0x80)
					b = byte(n & 0x7f)
				}
				pack.WriteByte(b)
				pack.Write(extra)
				zw, _ := zlib.NewWriterLevel(pack, zlib.NoCompression)
				_, _ = zw.Write(data)
				_ = zw.Close()
			}
			at := pack.Len()
			object(3, []byte(base), nil)
			object(6, delta, []byte{byte(pack.Len() - at)}) //nolint:gosec // the offset back to the blob, under 128
			sum := sha1.Sum(pack.Bytes())                   //nolint:gosec // a pack's own checksum is SHA-1
			pack.Write(sum[:])
			var read bytes.Buffer
			if err := unpackedWithin(io.TeeReader(pack, &read)); !errors.Is(err, tooBig) || read.Len() > rulesCap {
				t.Fatalf("%v, after reading %d bytes of the pack", err, read.Len())
			}
		})
	}
}

// What a delta's stream may write is what the cap has left, whatever its header declared.
func TestADeltasHeadTakesNoMoreThanTheCapHasLeft(t *testing.T) {
	if _, err := (&headOf{left: 4}).Write(make([]byte, 5)); !errors.Is(err, tooBig) {
		t.Fatalf("5 bytes with 4 left: %v", err)
	}
}

// The host is resolved once per fetch and every connection dials the address checked, never the name.
func TestTheHostIsResolvedOnceAndTheCheckedAddressDialled(t *testing.T) {
	repo, commit, srv := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n"}, 0, nil)
	var resolved atomic.Int32
	var dialled []string
	resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		resolved.Add(1)
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialled = append(dialled, addr)
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	srv.Config.SetKeepAlivesEnabled(false) // each request a connection of its own
	if _, err := (&Pipeline{Repos: t.TempDir()}).orgRules(t.Context(), repo, commit, nil); err != nil || resolved.Load() != 1 || len(dialled) < 2 ||
		slices.ContainsFunc(dialled, func(a string) bool { return a != "93.184.215.14:443" }) {
		t.Fatalf("%v: resolved %d times, dialled %q", err, resolved.Load(), dialled)
	}
}

// Every address class the contract refuses is refused, an IPv6 address carrying an IPv4 one as that
// IPv4 address, and public addresses are not.
func TestOnlyPublicAddressesAreDialled(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "fc00::1", "fd00:ec2::254", "169.254.169.254",
		"fe80::1%eth0", "0.0.0.0", "0.1.2.3", "::", "255.255.255.255", "224.0.0.1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a9fe:a9fe", "2002:0a00:0001::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2"} {
		if public(netip.MustParseAddr(a)) {
			t.Errorf("%s is dialled", a)
		}
	}
	for _, a := range []string{"93.184.215.14", "140.82.112.3", "2606:4700::1111", "::ffff:140.82.112.3", "64:ff9b::8c52:7003", "2002:8c52:7003::1"} {
		if !public(netip.MustParseAddr(a)) {
			t.Errorf("%s is refused", a)
		}
	}
}

// The rules part is checked against the executor's cap before it is sent: over it is tooBig.
func TestARulesPartOverTheExecutorsCapIsNotSent(t *testing.T) {
	was := rulesCap
	t.Cleanup(func() { rulesCap = was })
	part, err := rulesTar(nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(part)
	if err != nil {
		t.Fatal(err)
	}
	rulesCap = len(b) + 64<<10 // the vendored rules fit; the org's 64 KiB tips the part over
	if _, err := rulesTar(map[string][]byte{"small.yaml": make([]byte, 1<<10)}); err != nil {
		t.Fatalf("a part under the cap: %v", err)
	}
	if _, err := rulesTar(map[string][]byte{"big.yaml": make([]byte, 64<<10)}); !errors.Is(err, tooBig) {
		t.Fatalf("a part over the cap: %v", err)
	}
}

// An org rule with the id of a vendored rule, or of Canyon's own, is refused before the scan: opengrep
// keeps ids as written, and its SARIF one rule entry per id, so the org rule would take that rule's tags
// and read as its finding. A file that is not YAML is opengrep's to name, and passes.
func TestAnOrgRuleReusingABundledIdIsRefused(t *testing.T) {
	for _, id := range []string{"python_sql_rule-hardcoded-sql-expression", "canyon_sql_rule-request-in-sql-text"} {
		org := map[string][]byte{"a.yaml": []byte("rules:\n- id: mine\n"), "b/sql.yml": []byte("rules:\n- id: other\n- id: " + id + "\n")}
		_, err := rulesTar(org)
		if want := unfetched("its rule " + id + " has the id of one of Canyon's rules"); !errors.Is(err, want) {
			t.Fatalf("an org rule %s: %v, want %q", id, err, want)
		}
	}
	if _, err := rulesTar(map[string][]byte{"a.yaml": []byte("rules:\n- id: mine\n"), "notes.yml": []byte(":\t[")}); err != nil {
		t.Fatalf("ids of the org's own, and a file that is not YAML: %v", err)
	}
}

// With opengrep's settings naming a rules repo, the scan carries the org's rules under org/ beside
// Canyon's; a repo that cannot be fetched is opengrep's could-not-check, and nothing is scanned.
func TestOpengrepScansWithTheOrgsRulesRepo(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	repo, commit, _ := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n"}, 0, nil)
	f.SARIF = opengrepSARIF
	base := slices.Clone(snap.Rules[:len(snap.Rules)-2]) // trivy and grype out
	for _, c := range []struct{ commit, state, unchecked string }{{commit, "clean", ""},
		{strings.Repeat("a", 40), "blocked", "could not check: opengrep: its rules repo could not be fetched: git could not fetch that commit; the server's log has git's own error"}} {
		snap.Rules = append(slices.Clone(base), policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true,
			Settings: policy.Settings{Rules: &policy.OpengrepRules{Repo: repo, Commit: c.commit}}})
		pipe.Attach = attachAs(t, snap)
		scans := f.Scans.Load()
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
		if err != nil {
			t.Fatal(err)
		}
		if state, failure := waitEnded(t, ctx, p, run.ID); state != c.state || failure != nil {
			t.Fatalf("commit %s: the run ended %s %s, want %s", c.commit, state, why(failure), c.state)
		}
		_, rules := f.Scanned()
		unchecked, want := strings.Join(notChecked(verdictOf(t, ctx, p, run.ID)), ""), ""
		if c.unchecked != "" {
			want = "canyon/scanners: " + c.unchecked + " (blocks true)"
		}
		if unchecked != want {
			t.Fatalf("commit %s: could not check %q, want %q", c.commit, unchecked, want)
		}
		if c.unchecked == "" && !maps.Equal(rules, withOrg(shipped(t))) || c.unchecked != "" && f.Scans.Load() != scans {
			t.Fatalf("commit %s: rules %v, %d scans after %d", c.commit, slices.Collect(maps.Keys(rules)), f.Scans.Load(), scans)
		}
	}
}

// With paths, only the rules files under them reach the scan — a community repo's statistics YAML,
// which Opengrep cannot load, stays out — and paths holding none is the could-not-check that says so.
func TestOpengrepReadsOnlyTheRulesUnderItsPaths(t *testing.T) {
	p, ctx, f, snap := scanning(t)
	repo, commit, _ := rulesRepo(t, map[string]string{"ruby/sqli.yaml": "rules: []\n", "php/lang/sqli.yml": "rules: []\n", "rubyish/x.yaml": "rules: []\n",
		"stats/web_frameworks.yml": "frameworks: [rails]\n"}, 0, nil)
	f.SARIF = opengrepSARIF
	base := slices.Clone(snap.Rules[:len(snap.Rules)-2]) // trivy and grype out
	for _, c := range []struct {
		paths     []string
		state     string
		unchecked string
	}{{[]string{"ruby", "php/lang"}, "clean", ""},
		{[]string{"java"}, "blocked", "could not check: opengrep: its rules repo could not be fetched: it has no .yml or .yaml file under its paths"}} {
		snap.Rules = append(slices.Clone(base), policy.Rule{ID: "sarif.opengrep", Name: "Findings from opengrep", State: policy.Advisory, Producer: policy.Sarif, Run: true,
			Settings: policy.Settings{Rules: &policy.OpengrepRules{Repo: repo, Commit: commit, Paths: c.paths}}})
		pipe.Attach = attachAs(t, snap)
		run, err := Start(ctx, p, durable, StartInput{Project: project, Package: pkg, Policy: snap})
		if err != nil {
			t.Fatal(err)
		}
		if state, failure := waitEnded(t, ctx, p, run.ID); state != c.state || failure != nil {
			t.Fatalf("paths %q: the run ended %s %s, want %s", c.paths, state, why(failure), c.state)
		}
		if c.unchecked != "" {
			if got, want := strings.Join(notChecked(verdictOf(t, ctx, p, run.ID)), ""), "canyon/scanners: "+c.unchecked+" (blocks true)"; got != want {
				t.Fatalf("paths %q: could not check %q, want %q", c.paths, got, want)
			}
			continue
		}
		_, rules := f.Scanned()
		want := shipped(t)
		want["org/ruby/sqli.yaml"], want["org/php/lang/sqli.yml"] = "rules: []\n", "rules: []\n"
		if !maps.Equal(rules, want) {
			t.Fatalf("paths %q: rules %v", c.paths, slices.Sorted(maps.Keys(rules)))
		}
	}
}

// withOrg is rules and the org repo's one file the tests above fetch, under org/.
func withOrg(rules map[string]string) map[string]string {
	rules["org/a.yaml"] = "rules: []\n"
	return rules
}
