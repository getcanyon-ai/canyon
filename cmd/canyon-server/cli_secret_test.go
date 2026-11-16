package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/getcanyon-ai/canyon/cli"
)

// canyon secret set, run as the CLI runs, in a directory whose canyon.toml names no app yet: it makes
// the app as a first deploy does, writes its id, and stores the value, so the app's first deploy
// starts with it rather than failing for want of it (#1672; cli.md §3.2). Beside an app line naming an
// app this host doesn't have, it is guard (a)'s refusal instead: exit 1, and nothing is made or stored.
func TestTheCLIsSecretSetBeforeAnAppsFirstDeploy(t *testing.T) {
	cleanGate(t)
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "CANYON_HOST": base, "NO_COLOR": "1"}
	setIn := func(dir string) (int, string) {
		t.Chdir(dir)
		var out, errOut strings.Builder
		code := cli.Main([]string{"secret", "set", "STRIPE_SECRET_KEY"}, &out, &errOut, strings.NewReader("pancake\n"), func(k string) string { return env[k] }, false)
		return code, out.String() + errOut.String()
	}
	lacking := t.TempDir()
	if err := os.WriteFile(filepath.Join(lacking, "canyon.toml"), []byte("app = \"nowhere\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, said := setIn(lacking); code != 1 || !strings.Contains(said, "nowhere isn't on "+base+". If it should be, run: canyon deploy --create") ||
		count(t, ctx, pool, "SELECT count(*) FROM secrets")+count(t, ctx, pool, "SELECT count(*) FROM projects WHERE id = 'nowhere'") != 0 {
		t.Fatalf("secret set on an app line this host lacks exited %d, and made or stored something:\n%s", code, said)
	}
	dir := filepath.Join(t.TempDir(), "checkout")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "canyon.toml"), []byte("description = \"Checkout\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, said := setIn(dir); code != 0 {
		t.Fatalf("secret set on a new app exited %d:\n%s", code, said)
	}
	written, err := os.ReadFile(filepath.Join(dir, "canyon.toml")) //nolint:gosec // the file the CLI just wrote in this test's directory
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		App string `toml:"app"`
	}
	if err := toml.Unmarshal(written, &file); err != nil || file.App == "" {
		t.Fatalf("secret set wrote no app line: %q (%v)", written, err)
	}
	app := file.App
	c := anonymous(t, ctx, pool, base)
	uploadPulled(t, c, base, app, tarOf(t, map[string]string{"server.js": "require('http').createServer().listen(process.env.PORT)\n"}))
	_, started, _ := do(t, c, http.MethodPost, base+"/api/v1/projects/"+app+"/deploys", "")
	run, _ := runOf(started)["id"].(string)
	if got := waitVerdict(t, c, base, run); got["state"] != "clean" {
		t.Fatalf("the first deploy: %s", lastBody)
	}
	if got := fake.Ran(); got.Env["STRIPE_SECRET_KEY"] != "pancake" {
		t.Errorf("the first deploy started without the value set before it: %v", got.Env["STRIPE_SECRET_KEY"] != "")
	}
}

// Guard (b) against this server (cli.md §6.6): a checkout whose base this host doesn't have is refused
// before the value is asked for, and nothing is stored; a base it has passes — a check's package
// too, which the packages list never shows.
func TestTheCLIsSecretSetChecksTheBaseAgainstThisHost(t *testing.T) {
	pool, ctx := migrated(t)
	base, _ := start(t, pool, time.Hour)
	seedAppPage(t, ctx, pool)
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "CANYON_HOST": base, "NO_COLOR": "1"}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".canyon"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "canyon.toml"), []byte("app = \"claims-portal\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	setOn := func(pkg string) (int, string) {
		if err := os.WriteFile(filepath.Join(dir, ".canyon", "base"), []byte("app     = \"claims-portal\"\npackage = \""+pkg+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut strings.Builder
		code := cli.Main([]string{"secret", "set", "STRIPE_SECRET_KEY"}, &out, &errOut, strings.NewReader("pancake\n"), func(k string) string { return env[k] }, false)
		return code, out.String() + errOut.String()
	}
	if code, said := setOn("pkg_elsewhere"); code != 1 || !strings.Contains(said, "pkg_elsewhere isn't on "+base+".") || count(t, ctx, pool, "SELECT count(*) FROM secrets") != 0 {
		t.Fatalf("a base this host doesn't have: exit %d, and stored something or said:\n%s", code, said)
	}
	if code, said := setOn("pkg_2b17"); code != 0 || count(t, ctx, pool, "SELECT count(*) FROM secrets") != 1 {
		t.Fatalf("a check's package as the base: exit %d:\n%s", code, said)
	}
}
