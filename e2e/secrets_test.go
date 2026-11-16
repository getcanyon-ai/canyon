//go:build stack

package e2e

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

// rotated is rotate-kek's count line; every row is moved, none left behind.
var rotated = regexp.MustCompile(`rotate-kek: \d+ row\(s\) moved to key version \d+, 0 remaining`)

// j7Secrets sets a prod secret the app shows the digest of, rotates the key it is sealed under,
// and promotes: the running server opens the moved row without a restart. After a restart it
// seals a new value under the new key, and a second rotation leaves nothing behind.
func j7Secrets(t *testing.T, core string) {
	first := setToken(t, core)
	if _, body := api(t, "GET", "/api/v1/projects/"+coreApp+"/secrets", nil); strings.Contains(string(body), first) {
		t.Fatal("the secrets list answered the value; want names only")
	}
	if row := secretOf(t, coreApp, "prod", "API_TOKEN"); row == nil || row.Origin != "user" {
		t.Fatalf("API_TOKEN in prod: %+v; want a user row", row)
	}
	rotate(t)
	promoteShows(t, core, first)

	compose(t, "restart", "server")
	compose(t, "up", "-d", "--no-deps", "--force-recreate", "adapter") // the restarted server has a new namespace
	waitFor(t, 2*time.Minute, "the restarted server to be ready", ready)
	second := setToken(t, core)
	promoteShows(t, core, second)
	rotate(t)
	if _, code := canyon(t, core, "", "secret", "rm", "--env", "prod", "API_TOKEN"); code != 0 {
		t.Fatalf("secret rm: exit %d", code)
	}
}

// setToken sets API_TOKEN in prod to a fresh value through the CLI and answers the value.
func setToken(t *testing.T, core string) string {
	t.Helper()
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	value := hex.EncodeToString(b)
	if _, code := canyon(t, core, value+"\n", "secret", "set", "--env", "prod", "API_TOKEN"); code != 0 {
		t.Fatalf("secret set: exit %d", code)
	}
	return value
}

func rotate(t *testing.T) {
	t.Helper()
	if out := compose(t, "exec", "-T", "server", "/canyon-server", "rotate-kek"); !rotated.MatchString(out) {
		t.Fatalf("rotate-kek: %s; want every row moved and none remaining", out)
	}
}

// promoteShows promotes and checks prod's /secret shows the digest of value.
func promoteShows(t *testing.T, core, value string) {
	t.Helper()
	if stdout, code := canyon(t, core, "", "deploy", "--env", "prod", "--yes"); code != 0 {
		t.Fatalf("promote: exit %d\n%s", code, stdout)
	}
	sum := sha256.Sum256([]byte(value))
	if got := appPage(t, coreApp, "prod", "/secret", "digest"); got != hex.EncodeToString(sum[:]) {
		t.Errorf("prod /secret shows digest %s; want the value set's", got)
	}
}
