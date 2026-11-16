package main

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/gitops"
	"github.com/jackc/pgx/v5"
)

// The deploy repo (design/HAND-OFF.md §3b; contracts/routes.md, the platform-repo row; #2470): where releases go
// under delivery: repo. Any member reads it, never its token; an admin saves it, checks it and replaces either
// token.
func routesPlatformRepo(api *http.ServeMux, a *app) {
	a.checks = make(chan struct{}, 1)
	api.HandleFunc("GET /platform-repo", a.getPlatformRepo)
	api.HandleFunc("PUT /platform-repo", a.adminOnly(a.putPlatformRepo))
	api.HandleFunc("POST /platform-repo/check", a.adminOnly(a.checkPlatformRepo))
	api.HandleFunc("POST /platform-repo/token", a.adminOnly(a.replacePlatformRepoToken))
	api.HandleFunc("POST /platform-repo/fetch-token", a.adminOnly(a.rotateFetchToken))
	api.HandleFunc("POST /platform-repo/host-token", a.adminOnly(a.rotateRepoHostToken))
}

// checkInFlight is D5's line: the check's 429, one at a time per server.
const checkInFlight = "A check is running already. Try again once it answers."

var errNoRepo = errors.New("no deploy repo is saved")

// storedRepo is the platform-repo row as every member reads it: never the token.
type storedRepo struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Preview  struct {
		Branch string `json:"branch"`
	} `json:"preview"`
	Prod struct {
		Branch string `json:"branch"`
		Mode   string `json:"mode"`
	} `json:"prod"`
	Root       string         `json:"root"`
	API        string         `json:"api"`
	APIURL     string         `json:"api_url"`
	Store      string         `json:"store"`
	CA         *string        `json:"ca"`
	SetBy      actor          `json:"set_by"`
	SetAt      string         `json:"set_at"`
	CheckedAt  *string        `json:"checked_at"`
	LastCommit map[string]any `json:"last_commit"`
}

func readRepo(ctx context.Context, q querier) (*storedRepo, error) {
	var s storedRepo
	var at time.Time
	var checked *time.Time
	err := q.QueryRow(ctx, `SELECT url, username, preview_branch, prod_branch, prod_mode, root, api, api_url, store, ca, set_by_kind, set_by_subject, set_by_display, set_at, checked_at FROM platform_repo`).
		Scan(&s.URL, &s.Username, &s.Preview.Branch, &s.Prod.Branch, &s.Prod.Mode, &s.Root, &s.API, &s.APIURL, &s.Store, &s.CA, &s.SetBy.Kind, &s.SetBy.Subject, &s.SetBy.Display, &at, &checked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.SetAt = at.UTC().Format(time.RFC3339)
	if checked != nil {
		c := checked.UTC().Format(time.RFC3339)
		s.CheckedAt = &c
	}
	var id, env, state string
	var when time.Time
	var commit, pr, problem *string
	var number *int
	err = q.QueryRow(ctx, `SELECT id, env, state, coalesce(attempted_at, created_at), commit_sha, pull_request, pull_number, repo_problem FROM releases
		WHERE via = 'repo' ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&id, &env, &state, &when, &commit, &pr, &number, &problem)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		s.LastCommit = map[string]any{"release": id, "env": env, "state": state, "at": when.UTC().Format(time.RFC3339)}
		if commit != nil {
			s.LastCommit["commit"] = *commit
		}
		if pr != nil && number != nil {
			s.LastCommit["pull_request"] = map[string]any{"url": *pr, "number": *number}
		}
		if problem != nil {
			s.LastCommit["problem"] = *problem
		}
	}
	return &s, nil
}

func (a *app) getPlatformRepo(w http.ResponseWriter, r *http.Request) {
	stored, err := readRepo(r.Context(), a.pool)
	if err != nil {
		a.log.Printf("canyon-server: read the deploy repo: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	noStore(w) // every answer of these routes, a token's or not (#2471)
	// null before any is saved: a defined answer, never an error status a browser logs (#2560)
	writeJSON(w, http.StatusOK, stored)
}

var (
	repoSegment = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]{0,199}$`)
	rootRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)
	usernameRe  = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,128}$`)
	storeRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// validRepoURL is the deploy repo's address (contracts/routes.md, the platform-repo row): https, a lower-case host,
// a path of one or more segments — with api: github exactly /<owner>/<repo>, with api: gitlab two or more,
// /<group>[/<subgroup>…]/<project>, .git or not — and no userinfo, query, fragment or percent-escape, parsed with
// net/url and printed back as sent.
func validRepoURL(s, api string) bool {
	u, ok := cleanHTTPS(s)
	if !ok {
		return false
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	for _, seg := range segments {
		if !repoSegment.MatchString(seg) || strings.Trim(seg, ".") == "" {
			return false
		}
	}
	switch api {
	case "github":
		return len(segments) == 2
	case "gitlab":
		return len(segments) >= 2
	}
	return true
}

// cleanHTTPS is s parsed: https, a host of lower-case letters, digits, . and - alone with an optional port, and
// no userinfo, query, fragment, percent-escape or backslash, printing back exactly as sent. The host holds nothing
// a shell reads: the platform team's card hands the repo's URL to a kubectl command (#2473).
func cleanHTTPS(s string) (*url.URL, bool) {
	if len(s) > 2048 || strings.ContainsAny(s, `%\@?#`) {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != s ||
		!plainHost.MatchString(u.Host) {
		return nil, false
	}
	return u, true
}

// plainHost is a host of lower-case letters, digits, . and - alone, with an optional port.
var plainHost = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]{1,5})?$`)

// validBranch is a branch Canyon may write: git's shape, and never under canyon/, which is Canyon's own.
func validBranch(s string) bool {
	return branchRe.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") && !strings.Contains(s, "@{") &&
		!strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock") && s != "canyon" && !strings.HasPrefix(s, "canyon/")
}

// validRoot is a clean relative path of lower-case segments: never . or .., never Canyon's .canyon.
func validRoot(s string) bool {
	if len(s) > 200 || !rootRe.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if strings.Trim(seg, ".") == "" {
			return false
		}
	}
	return true
}

// apiURLOK is the pulls API's address: clean https, its path any (GitHub's none, Gitea's /api/v1), and under gitlab
// GitLab's own as its client takes it — on the repository's host and port, /api/v4 at the root the url's path starts
// with — so the token goes to no other host, and the project is the one the url names (#2480).
func apiURLOK(s, api, repoURL string) bool {
	if _, ok := cleanHTTPS(s); !ok {
		return false
	}
	if api == "gitlab" {
		_, err := gitops.NewMergeRequests(s, repoURL, "", nil)
		return err == nil
	}
	return true
}

// defaultAPIURL is api_url when a save leaves it out: GitHub's, or under gitlab GitLab's on the repository's host.
func defaultAPIURL(api, repoURL string) string {
	if api == "gitlab" {
		if u, err := url.Parse(repoURL); err == nil {
			return "https://" + u.Host + "/api/v4"
		}
	}
	return "https://api.github.com"
}

// validCA is one or more PEM certificates and nothing else, at most 64 KiB.
func validCA(s string) bool {
	if s == "" || len(s) > 64<<10 {
		return false
	}
	rest, n := []byte(s), 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return false
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return false
		}
		n++
	}
	return n > 0 && strings.TrimSpace(string(rest)) == ""
}

// repoEdit is a PUT's body, every absent key its default but the token, which a save keeps when absent.
type repoEdit struct {
	URL      string  `json:"url"`
	Username *string `json:"username"`
	Token    *string `json:"token"`
	Preview  struct {
		Branch string `json:"branch"`
	} `json:"preview"`
	Prod struct {
		Branch string  `json:"branch"`
		Mode   *string `json:"mode"`
	} `json:"prod"`
	Root   *string `json:"root"`
	API    *string `json:"api"`
	APIURL *string `json:"api_url"`
	Store  *string `json:"store"`
	CA     *string `json:"ca"`
}

// check answers the PUT's 400, the first key that fails, and fills every default.
func (e *repoEdit) check() (code, message string) {
	or := func(p **string, v string) {
		if *p == nil {
			*p = &v
		}
	}
	or(&e.Username, "x-access-token")
	or(&e.Root, "apps")
	or(&e.API, "github")
	or(&e.APIURL, defaultAPIURL(*e.API, e.URL))
	or(&e.Store, "canyon")
	or(&e.Prod.Mode, "pull_request")
	switch {
	case *e.API != "github" && *e.API != "gitlab" && *e.API != "none":
		return "invalid_api", "api is github, gitlab or none"
	case !validRepoURL(e.URL, *e.API):
		return "invalid_repo_url", "url: https://, a lower-case host and the repository's path, with api github exactly /<owner>/<repo>, with api gitlab /<group>/<project>; no user, password, query or fragment"
	case !usernameRe.MatchString(*e.Username):
		return "invalid_username", "username: 1 to 128 letters, digits and . _ @ + -"
	case e.Token != nil && !validToken(*e.Token):
		return "invalid_token", "token: 1 to 1024 characters, printable ASCII without spaces"
	case !validBranch(e.Preview.Branch) || !validBranch(e.Prod.Branch):
		return "invalid_branch", "each branch: a branch name, never under canyon/"
	case *e.Prod.Mode != "pull_request" && *e.Prod.Mode != "push", *e.API == "none" && *e.Prod.Mode != "push":
		return "invalid_mode", "prod.mode is pull_request or push, and push with no pull requests API"
	case !validRoot(*e.Root):
		return "invalid_root", "root: a relative path of lower-case segments, at most 200 bytes"
	case !validHTTPS(*e.APIURL) || !apiURLOK(*e.APIURL, *e.API, e.URL):
		return "invalid_api", "api_url: an https:// address, under gitlab GitLab's /api/v4 on the repository's own host, at the root its url starts with"
	case !storeRe.MatchString(*e.Store):
		return "invalid_store", "store: a ClusterSecretStore's name, a DNS label"
	case e.CA != nil && !validCA(*e.CA):
		return "invalid_ca", "ca: one or more PEM certificates, at most 64 KiB, or null"
	}
	return "", ""
}

func (a *app) putPlatformRepo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var e repoEdit
	if !decodeJSON(w, r, &e) {
		return
	}
	if code, message := e.check(); code != "" {
		writeErr(w, http.StatusBadRequest, code, message, nil)
		return
	}
	var fetchToken, hostToken string
	var saved *storedRepo
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		before, err := readRepo(ctx, tx)
		if err != nil {
			return err
		}
		if e.Token == nil && (before == nil || tokenMoves(before, e)) {
			return errTokenRequired
		}
		by := actorOf(ctx)
		if e.Token != nil {
			version, sealedKey, sealedValue, err := a.vault.Seal("platform-repo", "repo", "token", []byte(*e.Token))
			if err != nil {
				return err
			}
			if before == nil { // the first save: the row's own host token and fetch-only token, made here and answered once, never on a platform_webhook row (decision 26)
				if hostToken, err = token(); err != nil {
					return err
				}
				if fetchToken, err = token(); err != nil {
					return err
				}
				host, fetch := sha256.Sum256([]byte(hostToken)), sha256.Sum256([]byte(fetchToken))
				if _, err := tx.Exec(ctx, `INSERT INTO platform_repo (id, url, username, key_ref, sealed_key, sealed_value, preview_branch, prod_branch, prod_mode, root, api, api_url, store, ca,
					token_hash, fetch_token_hash, checked_at, set_at, set_by_kind, set_by_subject, set_by_display)
					VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, NULL, now(), $16, $17, $18)`,
					e.URL, *e.Username, version, sealedKey, sealedValue, e.Preview.Branch, e.Prod.Branch, *e.Prod.Mode, *e.Root, *e.API, *e.APIURL, *e.Store, e.CA,
					host[:], fetch[:], by.Kind, by.Subject, by.Display); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE platform_repo SET url = $1, username = $2, key_ref = $3, sealed_key = $4, sealed_value = $5, preview_branch = $6, prod_branch = $7,
				prod_mode = $8, root = $9, api = $10, api_url = $11, store = $12, ca = $13, checked_at = NULL, set_at = now(), set_by_kind = $14, set_by_subject = $15, set_by_display = $16 WHERE id = 1`,
				e.URL, *e.Username, version, sealedKey, sealedValue, e.Preview.Branch, e.Prod.Branch, *e.Prod.Mode, *e.Root, *e.API, *e.APIURL, *e.Store, e.CA, by.Kind, by.Subject, by.Display); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE platform_repo SET url = $1, username = $2, preview_branch = $3, prod_branch = $4, prod_mode = $5, root = $6, api = $7, api_url = $8,
			store = $9, ca = $10, checked_at = NULL, set_at = now(), set_by_kind = $11, set_by_subject = $12, set_by_display = $13 WHERE id = 1`,
			e.URL, *e.Username, e.Preview.Branch, e.Prod.Branch, *e.Prod.Mode, *e.Root, *e.API, *e.APIURL, *e.Store, e.CA, by.Kind, by.Subject, by.Display); err != nil {
			return err
		}
		if saved, err = readRepo(ctx, tx); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, by, "set platform repo", "", "", map[string]any{"url": e.URL, "preview": e.Preview, "prod": e.Prod, "root": *e.Root,
			"api": *e.API, "api_url": *e.APIURL, "store": *e.Store, "token_replaced": e.Token != nil && before != nil})
	})
	switch {
	case errors.Is(err, errTokenRequired):
		writeErr(w, http.StatusBadRequest, "invalid_token", "token: required on the first save, and on a save that moves the repository or its API to another host, changes the CA or changes the pull requests API", nil)
		return
	case err != nil:
		a.log.Printf("canyon-server: write the deploy repo: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	out := map[string]any{}
	raw, _ := json.Marshal(saved)
	_ = json.Unmarshal(raw, &out)
	if fetchToken != "" {
		out["fetch_token"] = fetchToken
	}
	if hostToken != "" {
		out["token"] = hostToken
	}
	noStore(w)
	writeJSON(w, http.StatusOK, out)
}

var errTokenRequired = errors.New("the deploy repo's first save, and a save that moves its host or changes its CA, carries its token")

// tokenMoves is whether e sends the stored token somewhere it never went: another host for the repository or for
// its pulls API, the same address under another trust (a changed CA), or another pulls API, which a token made for
// one is not (#2486's review). Such a save carries the token again, so a stored token never follows an address to a
// new host, a trust or an API it was not given for.
func tokenMoves(before *storedRepo, e repoEdit) bool {
	host := func(s string) string {
		u, err := url.Parse(s)
		if err != nil {
			return "" // never a host, so never the same one
		}
		return u.Host
	}
	api := before.APIURL
	if e.APIURL != nil {
		api = *e.APIURL
	}
	was, now := host(before.URL), host(e.URL)
	ca := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	kind := before.API
	if e.API != nil {
		kind = *e.API
	}
	return was == "" || was != now || host(before.APIURL) == "" || host(before.APIURL) != host(api) || ca(before.CA) != ca(e.CA) || before.API != kind
}

// checkPlatformRepo is the check of the repo (contracts/routes.md, the platform-repo row's POST …/check): outside
// any transaction, one at a time per server, answering problem codes and the templates found; a pass writes
// checked_at, which the next save clears.
func (a *app) checkPlatformRepo(w http.ResponseWriter, r *http.Request) {
	select {
	case a.checks <- struct{}{}:
		defer func() { <-a.checks }()
	default:
		writeErr(w, http.StatusTooManyRequests, "check_in_flight", checkInFlight, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	repo, err := a.pipe.DeployRepo(ctx, a.pool)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", errNoRepo.Error(), nil)
		return
	}
	if err != nil {
		a.log.Printf("canyon-server: check the deploy repo: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	sample := gitops.Release{Canyon: a.public, ID: "rel_0000000000000000", App: "sample", GateRun: "gr_0000", Package: "pkg_0000", Version: "check",
		Image: "registry.example/canyon/sample@sha256:" + strings.Repeat("0", 64), Identity: a.pipe.IdentityImage,
		AppEnv: map[string]string{"PORT": "3000"}, SidecarEnv: map[string]string{}}
	problems, templates, err := repo.Check(ctx, sample)
	if err != nil {
		a.log.Printf("canyon-server: check the deploy repo: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if problems == nil {
		problems = []gitops.Problem{}
	}
	if len(problems) == 0 {
		if _, err := a.pool.Exec(ctx, "UPDATE platform_repo SET checked_at = now() WHERE id = 1"); err != nil {
			a.log.Printf("canyon-server: record the deploy repo's check: %v", err)
		}
	}
	codes := make([]string, len(problems))
	for i, p := range problems {
		codes[i] = p.Code
	}
	a.log.Printf("canyon-server: a check of the deploy repo answered %v", codes) // the codes alone, never a message the remote could have shaped
	noStore(w)
	writeJSON(w, http.StatusOK, map[string]any{"passed": len(problems) == 0, "problems": problems, "templates": templates})
}

func (a *app) replacePlatformRepoToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validToken(body.Token) {
		writeErr(w, http.StatusBadRequest, "invalid_token", "token: 1 to 1024 characters, printable ASCII without spaces", nil)
		return
	}
	var saved *storedRepo
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		version, sealedKey, sealedValue, err := a.vault.Seal("platform-repo", "repo", "token", []byte(body.Token))
		if err != nil {
			return err
		}
		by := actorOf(ctx)
		tag, err := tx.Exec(ctx, "UPDATE platform_repo SET key_ref = $1, sealed_key = $2, sealed_value = $3, checked_at = NULL, set_at = now(), set_by_kind = $4, set_by_subject = $5, set_by_display = $6 WHERE id = 1",
			version, sealedKey, sealedValue, by.Kind, by.Subject, by.Display)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoRepo
		}
		if saved, err = readRepo(ctx, tx); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, by, "replace platform repo token", "", "", nil)
	})
	switch {
	case errors.Is(err, errNoRepo):
		writeErr(w, http.StatusNotFound, "not_found", errNoRepo.Error(), nil)
	case err != nil:
		a.log.Printf("canyon-server: replace the deploy repo's token: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		noStore(w)
		writeJSON(w, http.StatusOK, map[string]any{"set_by": saved.SetBy, "set_at": saved.SetAt})
	}
}

func (a *app) rotateFetchToken(w http.ResponseWriter, r *http.Request) {
	a.replaceRepoHash(w, r, "fetch_token_hash", "fetch_token", "rotate fetch token")
}

// rotateRepoHostToken replaces the deploy repo's own host token (#2470): the reports and access-token fetches its
// platform makes with the old one fail from that instant.
func (a *app) rotateRepoHostToken(w http.ResponseWriter, r *http.Request) {
	a.replaceRepoHash(w, r, "token_hash", "token", "replace platform repo host token")
}

// replaceRepoHash mints a token, keeps its SHA-256 in column of the platform-repo row in place of the one there, and
// answers it once as key, the row's audit row action beside it, never a value.
func (a *app) replaceRepoHash(w http.ResponseWriter, r *http.Request, column, key, action string) {
	ctx := r.Context()
	minted, err := token()
	if err != nil {
		a.log.Printf("canyon-server: mint a %s: %v", key, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	sum := sha256.Sum256([]byte(minted))
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "UPDATE platform_repo SET "+column+" = $1 WHERE id = 1", sum[:]) //nolint:gosec // column is one of this file's two constants
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoRepo
		}
		return events.AuditTx(ctx, tx, actorOf(ctx), action, "", "", nil)
	})
	switch {
	case errors.Is(err, errNoRepo):
		writeErr(w, http.StatusNotFound, "not_found", errNoRepo.Error(), nil)
	case err != nil:
		a.log.Printf("canyon-server: %s: %v", action, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		noStore(w)
		writeJSON(w, http.StatusOK, map[string]any{key: minted})
	}
}
