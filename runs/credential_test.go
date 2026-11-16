package runs

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/getcanyon-ai/canyon/vault"
)

// SameRepo compares as the contract says: scheme and host lower-cased, :443 dropped, then a trailing /
// and a trailing .git dropped; the path's case, another port, scheme or host is another repo.
func TestSameRepo(t *testing.T) {
	for b, same := range map[string]bool{"https://GitHub.com:443/Meridian/Rules/": true, "https://github.com/Meridian/Rules": true,
		"https://github.com/Meridian/Rules.git/": true, "https://github.com/meridian/rules": false, "https://github.com:8443/Meridian/Rules": false,
		"http://github.com/Meridian/Rules": false, "https://gitlab.com/Meridian/Rules": false, "https://gİthub.com/Meridian/Rules": false, "::": false} {
		if SameRepo("https://github.com/Meridian/Rules.git", b) != same {
			t.Errorf("%s: same %v", b, !same)
		}
	}
}

// The credential is read for its own repo alone: a repo that asks for one is read with it, and, set for
// another repo, it is never sent, and the fetch says the repo asks for one and none is set for it (#1124).
func TestTheCredentialGoesToItsRepoAlone(t *testing.T) {
	pool, ctx := seeded(t)
	keys, err := vault.Load(filepath.Join(t.TempDir(), "kek"))
	if err != nil {
		t.Fatal(err)
	}
	repo, commit, _ := rulesRepo(t, map[string]string{"a.yaml": "rules: []\n"}, 0, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, pw, ok := r.BasicAuth(); !ok || u != "x-access-token" || pw != "rules-reader-token" {
				w.Header().Set("WWW-Authenticate", `Basic realm="rules"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	version, key, sealed, err := keys.Seal("scanner", "opengrep", "token", []byte("rules-reader-token"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{Pool: pool, Vault: keys, Repos: t.TempDir()}
	for set, want := range map[string]error{"https://Example.com/rules": nil, "https://example.com/other-rules.git": asksForLogin} {
		if _, err := pool.Exec(ctx, `INSERT INTO scanner_credentials (tool, repo, username, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display)
			VALUES ('opengrep', $1, 'x-access-token', $2, $3, $4, 'person', 'dana', 'Dana') ON CONFLICT (tool) DO UPDATE SET repo = EXCLUDED.repo`, set, version, key, sealed); err != nil {
			t.Fatal(err)
		}
		auth, err := p.credential(ctx, repo)
		if err == nil {
			_, err = (&Pipeline{Repos: t.TempDir()}).orgRules(ctx, repo, commit, auth)
		}
		if err != want {
			t.Errorf("the credential set for %s: %v, want %v", set, err, want)
		}
	}
}
