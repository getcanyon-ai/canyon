package runs

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/getcanyon-ai/canyon/vault"
	"github.com/jackc/pgx/v5"
)

// row is the platform-repo row as DeployRepo scans it, in its SELECT's order.
type row []any

func (r row) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("scan %d into %d", len(r), len(dest))
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(r[i]))
	}
	return nil
}

// oneRow answers every query with r.
type oneRow struct{ r row }

func (q oneRow) QueryRow(context.Context, string, ...any) pgx.Row { return q.r }

func (q oneRow) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("no rows here")
}

// A deploy repo whose API Canyon does not speak, or production through a pull request with no API, is an error
// when it is read: never a repo whose production commit would call a nil client (#2486's review).
func TestADeployRepoNeverHasProductionPullRequestsWithNoAPI(t *testing.T) {
	keys, err := vault.Load(t.TempDir() + "/kek")
	if err != nil {
		t.Fatal(err)
	}
	version, sealedKey, sealedValue, err := keys.Seal("platform-repo", "repo", "token", []byte("deploy-token-fixture-not-real"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{Vault: keys, Repos: t.TempDir(), PublicURL: "https://canyon.meridian.example"}
	read := func(mode, api, apiURL string) error {
		var ca *string
		_, err := p.DeployRepo(t.Context(), oneRow{row{"https://git.meridian.example/meridian/deploy", "x-access-token", version, sealedKey, sealedValue,
			"preview", "main", mode, "apps", api, apiURL, "canyon", ca}})
		return err
	}
	for name, c := range map[string]struct {
		mode, api, apiURL string
		ok                bool
	}{
		"github, through a pull request":     {"pull_request", "github", "https://api.git.meridian.example", true},
		"gitlab, through a merge request":    {"pull_request", "gitlab", "https://git.meridian.example/api/v4", true},
		"none, pushed":                       {"push", "none", "https://api.github.com", true},
		"an API Canyon does not speak":       {"push", "bitbucket", "https://api.bitbucket.example", false},
		"no API, through a pull request":     {"pull_request", "none", "https://api.github.com", false},
		"an unknown API, through a pull req": {"pull_request", "bitbucket", "https://api.bitbucket.example", false},
	} {
		if err := read(c.mode, c.api, c.apiURL); (err == nil) != c.ok {
			t.Errorf("%s: %v, want ok %v", name, err, c.ok)
		}
	}
}
