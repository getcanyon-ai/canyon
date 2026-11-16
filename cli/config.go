package cli

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// sessionChars is a session's only shape: cmd/canyon-server/session.go mints 32 random bytes as hex
// and refuses a cookie of any other length before the database. The same rule on the file keeps a
// host from parking anything it likes on this machine.
const sessionChars = 64

func validSession(s string) bool {
	if len(s) != sessionChars {
		return false
	}
	_, err := hex.DecodeString(s) // the decoder, not a guess at what hex looks like
	return err == nil
}

// config is what this machine remembers between invocations: the host it talks to and the session
// that host minted, with the public URL it answered, which names its cookie. Only `canyon login`
// writes `host` and `public` — a one-off `--host` must not become sticky.
type config struct {
	Host    string `json:"host"`
	Public  string `json:"public,omitempty"`  // GET /auth/config's public at login; absent in a config saved before it
	Session string `json:"session,omitempty"` // absent after canyon logout: the host stays, so the next login offers it
}

// configPath is $XDG_CONFIG_HOME/canyon/config.json, else $HOME/.config/canyon/config.json — never
// os.UserConfigDir(), which on macOS is ~/Library/Application Support, a directory no builder looks
// in. Read through app.getenv, so no test mutates the environment.
func configPath(getenv func(string) string) (string, error) {
	dir := getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := getenv("HOME")
		if home == "" {
			return "", errors.New("neither XDG_CONFIG_HOME nor HOME is set, so there is nowhere to keep a session")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "canyon", "config.json"), nil
}

// config loads the file once per invocation. Every reason it cannot be read — no file yet, no HOME,
// unreadable, not JSON — is first contact, not a failure, so the caller always gets one.
func (a *app) config() *config {
	if a.conf == nil {
		a.conf = a.loadConfig()
	}
	return a.conf
}

func (a *app) loadConfig() *config {
	path, err := configPath(a.getenv)
	if err != nil {
		return &config{} // nowhere to keep a session is not a fault of the machine's, and says nothing
	}
	c, refused := readSession(path, "")
	switch {
	case refused != nil:
		a.ignore(refused)
	case c != nil:
		return c
	}
	return &config{} // no file yet is first contact
}

// hostsDir is hosts/ beside config.json: a file for every other host this machine is signed in to
// (cli.md §8.4). Older CLIs never touch it, so they cannot clobber another host's session.
func hostsDir(getenv func(string) string) (string, error) {
	path, err := configPath(getenv)
	return filepath.Join(filepath.Dir(path), "hosts"), err
}

// hostFileMax is the longest name a host's file may have; a host whose name would pass it gets none.
const hostFileMax = 200

// hostFile is a canonical host's file name in hosts/: the scheme, the host, and the port when one
// remains, joined by _ — a character no part can hold, so no name is ambiguous and none needs
// escaping, and none can step out of the directory. "" for a host outside [a-z0-9.-] (an IPv6
// literal, a Unicode name) or too long, which gets no file: it can be the default host, in
// config.json, and signing in to another forgets it, as login did before.
func hostFile(host string) string {
	u, err := url.Parse(host)
	if err != nil || u.Hostname() == "" || strings.ContainsFunc(u.Hostname(), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '-'
	}) {
		return ""
	}
	name := u.Scheme + "_" + u.Hostname()
	if port := u.Port(); port != "" {
		name += "_" + port
	}
	if name += ".json"; len(name) > hostFileMax {
		return ""
	}
	return name
}

// hostPath is a canonical host's file in hosts/ and that file's name: "" for a host that gets none,
// or on a machine with nowhere to keep one.
func hostPath(getenv func(string) string, host string) (path, name string, refused *unusable) {
	if name = hostFile(host); name == "" {
		return "", "", nil
	}
	dir, refused := hostsHome(getenv)
	if dir == "" {
		return "", "", refused
	}
	return filepath.Join(dir, name), name, nil
}

// hostsHome is hosts/, "" on a machine with nowhere to keep it. It has to be a directory only this
// account can enter, or another account could have swapped a file in under it; one that isn't is refused.
func hostsHome(getenv func(string) string) (string, *unusable) {
	dir, err := hostsDir(getenv)
	if err != nil {
		return "", nil
	}
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", &unusable{path: dir, why: "it cannot be read: " + cause(err), fix: "rm -r " + dir}
	case !info.IsDir():
		return "", &unusable{path: dir, why: "it is not a directory", fix: "rm " + dir}
	case info.Mode().Perm()&0o077 != 0:
		return "", &unusable{path: dir, why: "other accounts can enter it", fix: "chmod 700 " + dir}
	}
	return dir, nil
}

// signedIn is every host but except that this machine holds a session for (cli.md §8.4), sorted:
// config.json's, and each hosts/ file's but a stale one for config.json's host. Each is the file's
// host, which passed the origin rule when it was read, and none holds a character a terminal acts on.
// A hosts/ file this CLI won't use is skipped with its one line.
func (a *app) signedIn(except string) []string {
	held, hosts := a.config(), []string{}
	if held.Session != "" {
		hosts = append(hosts, held.Host)
	}
	dir, refused := hostsHome(a.getenv)
	if refused != nil {
		a.ignore(refused)
		return sortedHosts(hosts, except)
	}
	entries, _ := os.ReadDir(dir) // none, when there is no hosts/
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".json") { // a write in flight is <name>.json-<random>
			c, refused := readSession(filepath.Join(dir, name), name)
			if refused != nil {
				a.ignore(refused)
			} else if c != nil && c.Session != "" && c.Host != held.Host {
				hosts = append(hosts, c.Host)
			}
		}
	}
	return sortedHosts(hosts, except)
}

// sortedHosts is hosts without except and any host a terminal would act on, sorted, each once.
func sortedHosts(hosts []string, except string) []string {
	hosts = slices.DeleteFunc(hosts, func(h string) bool { return h == except || visible(h) != h })
	slices.Sort(hosts)
	return slices.Compact(hosts)
}

// hostConfig is the hosts/ file for a canonical host: nil when there is none, and when there is one
// this CLI will not use, which is said in its one line.
func (a *app) hostConfig(host string) *config {
	path, name, refused := hostPath(a.getenv, host)
	if refused == nil && path != "" {
		var c *config
		if c, refused = readSession(path, name); refused == nil {
			return c
		}
	}
	if refused != nil {
		a.ignore(refused)
	}
	return nil
}

// unusable is a session file, or hosts/ itself, that this CLI will not use: one line names the path
// and the way out (cli.md §8.4).
type unusable struct {
	path, why, fix string
	loose          bool // only its mode is wrong: what it holds parsed, so a logout can still end it
}

// readSession reads one session file, config.json or a hosts/ file, under the same rules: nil and no
// refusal when there is none. name is the hosts/ file's, which its host has to re-derive; "" for
// config.json. The file is opened once and checked on that handle — a regular file, the one the
// path named rather than a link's target, private to this account — so nothing can be swapped in
// between the check and the read.
func readSession(path, name string) (*config, *unusable) {
	unreadable := func(err error) (*config, *unusable) {
		return nil, &unusable{path: path, why: "it cannot be read: " + cause(err), fix: "rm " + path}
	}
	link, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return unreadable(err)
	case !link.Mode().IsRegular():
		return nil, &unusable{path: path, why: "it is not a regular file", fix: "rm " + path}
	}
	f, err := os.Open(path) //nolint:gosec // this machine's own config, at a path composed from XDG_CONFIG_HOME or HOME and, in hosts/, a name in [a-z0-9._-]
	if err != nil {
		return unreadable(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return unreadable(err)
	}
	if !os.SameFile(link, info) {
		return nil, &unusable{path: path, why: "it changed while it was read", fix: "rm " + path}
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return unreadable(err)
	}
	c, why := parseSession(body, name)
	if info.Mode().Perm()&0o077 != 0 {
		return c, &unusable{path: path, why: "other accounts can read it", fix: "chmod 600 " + path, loose: true}
	}
	if why != "" {
		return nil, &unusable{path: path, why: why, fix: "rm " + path}
	}
	return c, nil
}

// parseSession is a session file's body under §8.4's rules, its host in the canonical form; why is
// the reason it is refused, and c nil with it.
func parseSession(body []byte, name string) (c *config, why string) {
	c = &config{}
	if json.Unmarshal(body, c) != nil {
		return nil, "it is not the JSON Canyon writes"
	}
	if c.Session != "" && !validSession(c.Session) {
		return nil, "the session in it is not one a Canyon host minted"
	}
	if c.Host != "" {
		host, err := validHost("the host it saved", c.Host)
		if err != nil {
			return nil, err.Error()
		}
		c.Host = host // normalised here, so every comparison that seeds the jar is byte-for-byte; the file is left as it is
	}
	if name != "" && hostFile(c.Host) != name {
		return nil, "the host in it is not the one its name says"
	}
	return c, ""
}

// cause is what the system said about a path, without the path a line has already named.
func cause(err error) string {
	var p *fs.PathError
	if errors.As(err, &p) {
		return p.Err.Error()
	}
	return err.Error()
}

// ignore is the one answer to a file this CLI will not use: one line naming it and the way out, once
// an invocation, then first contact. Nothing is repaired in place, or a chmod would hide the theft it fixed.
func (a *app) ignore(r *unusable) {
	if a.ignored[r.path] {
		return
	}
	if a.ignored == nil {
		a.ignored = map[string]bool{}
	}
	a.ignored[r.path] = true
	a.errf("\n  %s %s: %s, so it is ignored. Run: %s\n", a.render.glyph(stateWarn), r.path, r.why, r.fix)
}

// save writes through atomicWrite as private, so the file is 0600 whatever it was: load ignores a
// config with any group or other bit, so keeping a looser mode would make save and load disagree.
func (c *config) save(getenv func(string) string) error {
	path, err := configPath(getenv)
	if err != nil {
		return err
	}
	return c.write(path)
}

// saveHost writes c to its host's file in hosts/, by the same rules as config.json, and never into a
// hosts/ this CLI refuses to read.
func (c *config) saveHost(getenv func(string) string) error {
	path, _, refused := hostPath(getenv, c.Host)
	switch {
	case refused != nil:
		return fmt.Errorf("%s: %s", refused.path, refused.why)
	case path == "":
		return fmt.Errorf("%s gets no file in hosts/", c.Host)
	}
	return c.write(path)
}

func (c *config) write(path string) error {
	if c.Session != "" && !validSession(c.Session) { // a value that is not a session is not written, whoever offered it
		return fmt.Errorf("a session is %d hex characters, and this one is %d long", sessionChars, len(c.Session))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicWrite(path, body, true)
}

// remember writes back a session the host rotated, and nothing else, to whichever file holds that
// host: the host that answered has to be the stored one, or a call made with `--host` would file a
// stranger's cookie under it. The file is read again first, and left alone when another invocation
// has changed it since: what that one wrote is as current as this, and may be a login.
func (a *app) remember() {
	if a.http == nil {
		return
	}
	held, name := a.config(), ""
	if held.Host != a.http.Base {
		held, name = a.other, hostFile(a.http.Base) // a.other is nil or holds this host: client() read it for the host it dialled
	}
	got := a.http.session()
	if held == nil || got == "" || got == held.Session {
		return
	}
	path, err := configPath(a.getenv)
	if name != "" {
		path, _, _ = hostPath(a.getenv, held.Host)
	}
	if now, refused := readSession(path, name); err != nil || path == "" || refused != nil || now == nil || *now != *held {
		return
	}
	next := &config{Host: held.Host, Public: held.Public, Session: got} // written first, adopted second: a refused write leaves the invocation with what it read
	if err := next.write(path); err != nil {
		a.errf("\n  %s This session could not be saved: %s\n", a.render.glyph(stateWarn), err)
		return
	}
	held.Session = got
}
