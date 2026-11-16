package runs

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/jackc/pgx/v5"
)

// credential is the scanner credential for the rules repo repo, opened, as HTTP Basic; nil when none is
// set or it was set for another repo (routes.md, the scanner credential row). Opened inside the step
// that fetches, it is never a step's output, and never journaled.
func (p *Pipeline) credential(ctx context.Context, repo string) (transport.AuthMethod, error) {
	var set, username string
	var version int
	var key, sealed []byte
	err := p.Pool.QueryRow(ctx, "SELECT repo, username, key_ref, sealed_key, sealed_value FROM scanner_credentials WHERE tool = 'opengrep'").Scan(&set, &username, &version, &key, &sealed)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !SameRepo(set, repo) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Vault == nil {
		return nil, errors.New("runs: no vault configured: the rules repo's credential cannot open") // fails the run closed
	}
	token, err := p.Vault.Open(version, "scanner", "opengrep", "token", key, sealed)
	if err != nil {
		return nil, err
	}
	return &githttp.BasicAuth{Username: username, Password: string(token)}, nil
}

// SameRepo is whether a and b name one rules repo, as the scanner credential binds to it
// (contracts/finding.md, Canyon-run tools): scheme and host lower-cased, the port :443 dropped, then a
// trailing / and a trailing .git dropped from the path. A name that does not parse is no repo, nor a
// host past ASCII: Unicode lower-casing reads gİthub.com as github.com, which the fetch dials as another.
func SameRepo(a, b string) bool {
	ka, okA := repoKey(a)
	kb, okB := repoKey(b)
	return okA && okB && ka == kb
}

func repoKey(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || strings.ContainsFunc(u.Host, func(r rune) bool { return r > unicode.MaxASCII }) {
		return "", false
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.EscapedPath(), "/"), ".git")
	return strings.ToLower(u.Scheme) + "://" + strings.TrimSuffix(strings.ToLower(u.Host), ":443") + path, true
}
