package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/internal/enterprise"
	"github.com/getcanyon-ai/canyon/vault"
)

// The policy egress-control pulls (design/EGRESS.md §3.2; contracts/routes.md, Egress control): GET
// /egress/policy on a listener of its own, which no app and no load balancer reaches — on Docker the server's
// address on canyon-egress-ctl, the app pool closed at accept; on Kubernetes TLS under the certificate
// egress-control pins, and a projected token of egress-control's. Policy, never secrets. POST /egress/dropped, on
// the same listener under the same proof, is what egress-control read of what Envoy lost, GET /egress/route-value
// the one route that answers a route's value, to egress-control alone, and GET /egress/leaf an inject destination's
// leaf, its key and the intermediate, the one route that answers a key.

// policyServer answers GET /egress/policy, POST /egress/dropped, GET /egress/route-value and GET /egress/leaf, and
// nothing else.
type policyServer struct {
	pool     *pgxpool.Pool
	public   string                                                     // the host's public URL, which a not_allowed body links to
	platform func(context.Context, querier) ([]string, error)           // the platform destinations, from the front door
	callers  func(context.Context, []string) ([]executor.Caller, error) // the executor's POST /callers
	replica  func(*http.Request) (string, bool)                         // who is asking: Kubernetes the token's pod, Docker the peer; false is 401
	lost     func(*int64) bool                                          // the receiver's envoyLost.add; nil with no receiver
	kube     *kubeTokens                                                // a Kubernetes host's: the key set the policy names; nil on Docker
	log      *log.Logger
	ee       eeHooks // Canyon Enterprise's: a route's value, a leaf and the routes the policy names; none without it

	started func(context.Context, pgx.Tx) (time.Time, error) // when the database's server started; pg_postmaster_start_time() when nil

	mu      sync.Mutex
	highest map[string]int64 // the highest seq this process answered under each epoch: a lower one is a restore or a failover
	pgStart time.Time        // the database server's start the last answer read: another is a restore or a failover
}

type egressPolicy struct {
	pgStart     time.Time      // the database server's start, read in the policy's snapshot; never answered
	aged        bool           // the snapshot held a caller unconfirmed for callerKept, which it does not name
	recorded    *string        // the signing root egress_policy says the last answer named; never answered
	Format      int            `json:"format"`
	Epoch       string         `json:"epoch"`
	Seq         int64          `json:"seq"`
	SigningRoot *string        `json:"signing_root"` // the root signing the current leaves, which each leaf chains to; nil while none is live
	PublicURL   string         `json:"public_url"`
	Callers     []policyCaller `json:"callers"`
	Envs        []policyEnv    `json:"envs"`
	Platform    []string       `json:"platform"`
	Kubernetes  *policyKube    `json:"kubernetes,omitempty"` // a Kubernetes host's alone
	keysAt      *string        // the key set's fingerprint egress_policy says the last answer named; never answered
	Caps        policyCaps     `json:"caps"`
}

// policyCaps is what the check needs of the admin's caps (egress_settings; #2691): the new names an app env may call
// a minute, across every replica.
type policyCaps struct {
	NewNames int `json:"new_names"`
}

// policyKube is the cluster's service-account issuer and key set as the server last read them, within
// keySetMaxAge, else null and none: what egress-control verifies a pod's projected token against, offline
// (design/EGRESS.md §4.3, §14 decision 1).
type policyKube struct {
	Issuer *string           `json:"issuer"`
	Keys   []jose.JSONWebKey `json:"keys"`
}

type policyCaller struct {
	Address string `json:"address"`
	App     string `json:"app"`
	Env     string `json:"env"`
}

type policyEnv struct {
	App     string   `json:"app"`
	Env     string   `json:"env"`
	Mode    string   `json:"mode"`
	Allowed []string `json:"allowed"`
	Known   []string `json:"known"` // the names a call of it went through to, which the rate limit counts as no new one; it moves no version
	// TokenOnly is egress_envs' mark on Kubernetes: its pods are named by their token alone (design/EGRESS.md §4.3)
	TokenOnly bool `json:"token_only,omitempty"`
	// Account is its ServiceAccount on a Kubernetes host, named from before its first /run; none on Docker
	Account string `json:"account,omitempty" db:"-"`
	// Routes are its value routes, never a value, named only while the signing root is among its trusted roots
	Routes []policyRoute `json:"routes" db:"-"`
}

// policyRoute is one value route as the policy names it: what egress-control needs to choose it, never its value.
type policyRoute = enterprise.PolicyRoute

// policyFormat is the policy's format: raised only for a change an older egress-control must not read as it
// reads this one, never for a field added, which egress-control ignores (contracts/routes.md, The check). A
// Kubernetes host's is policyFormatKube: an egress-control from before #2142 ignores token_only and would name a
// token_only env by its pods' address, so it must refuse the policy whole and fail closed (#2142's review).
const (
	policyFormat     = 1
	policyFormatKube = 2
)

// policyVersion is have's shape, <epoch>:<seq>.
var policyVersion = regexp.MustCompile(`^([0-9a-f]{32}):([1-9][0-9]{0,18})$`)

func (s *policyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	report := r.URL.Path == "/egress/dropped" && r.Method == http.MethodPost
	value := r.URL.Path == "/egress/route-value" && r.Method == http.MethodGet
	leaf := r.URL.Path == "/egress/leaf" && r.Method == http.MethodGet
	if !report && !value && !leaf && (r.URL.Path != "/egress/policy" || r.Method != http.MethodGet) {
		http.NotFound(w, r)
		return
	}
	replica, ok := s.replica(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if report {
		s.dropped(w, r)
		return
	}
	if value {
		s.routeValue(w, r, replica)
		return
	}
	if leaf {
		s.leaf(w, r, replica)
		return
	}
	ctx, q := r.Context(), r.URL.Query()
	// how long each step of an answer took, said with a failure, whose step is the last, or an answer past a second:
	// egress-control gives a pull 5 s, and past 30 s without one it knows no caller (#86)
	began := time.Now()
	since, steps := began, []string(nil)
	took := func(step string) {
		now := time.Now()
		steps, since = append(steps, fmt.Sprintf("%s %v", step, now.Sub(since).Round(time.Millisecond))), now
	}
	asked, refetch := q["caller"]
	waited := func() string {
		st, ask := s.pool.Stat(), "a poll"
		if refetch {
			ask = "a refetch"
		}
		return fmt.Sprintf("%s from %s in %v: %s; pool %d of %d in use, %d idle", ask, replica, since.Sub(began).Round(time.Millisecond), strings.Join(steps, ", "), st.AcquiredConns(), st.MaxConns(), st.IdleConns())
	}
	if refetch {
		if addr, err := netip.ParseAddr(asked[0]); len(asked) != 1 || err != nil || !addr.Is4() || addr.String() != asked[0] {
			writeErr(w, http.StatusBadRequest, "invalid_caller", "caller is one IPv4 address", nil)
			return
		}
	}
	if refetch {
		err := s.resolve(ctx, asked[0])
		took("resolve")
		if err != nil { // never the policy as if no one held it, which egress-control would remember
			s.log.Printf("canyon-server: egress policy: caller %s: %v (%s)", asked[0], err, waited())
			writeErr(w, http.StatusServiceUnavailable, "caller_unresolved", "the executor could not be asked who holds the address", nil)
			return
		}
	}
	p, err := s.answer(ctx)
	took("answer")
	if err == nil {
		err = s.seen(ctx, replica, q.Get("have"), p)
		took("seen")
	}
	if err != nil {
		s.log.Printf("canyon-server: egress policy: %v (%s)", err, waited())
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if since.Sub(began) > time.Second {
		s.log.Printf("canyon-server: egress policy: slow answer (%s)", waited())
	}
	if q.Get("have") == p.Epoch+":"+strconv.FormatInt(p.Seq, 10) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// droppedMost is the most one report counts (egress.schema.json's dropped_report): past anything Envoy drops between
// two reads. It bounds a report, never the sum of them, in a flush or on a gap's row.
const droppedMost = 1 << 40

// dropped takes what egress-control read of what Envoy lost, which the next flush writes (contracts/routes.md,
// Egress control): 503 keeps it with egress-control until a receiver can take it.
func (s *policyServer) dropped(w http.ResponseWriter, r *http.Request) {
	calls, ok := droppedCalls(http.MaxBytesReader(w, r.Body, 1<<10))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_report", "the body is {calls}, a whole number from 1 to 2^40, or null when no one counted them", nil)
		return
	}
	if s.lost == nil || !s.lost(calls) {
		writeErr(w, http.StatusServiceUnavailable, "no_receiver", "no receiver takes what Envoy lost", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// droppedCalls reads a report's body, exactly {calls}: its calls, nil for null, which is no count of none, and false
// for any other body. It walks the tokens, as decoding into a struct would take CALLS for calls and the last of two
// calls, and leave what follows the closing brace unread.
func droppedCalls(body io.Reader) (*int64, bool) {
	dec := json.NewDecoder(body)
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	if t, err := dec.Token(); err != nil || t != "calls" {
		return nil, false
	}
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	if string(raw) == "null" {
		return nil, true
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	return &n, err == nil && n >= 1 && n <= droppedMost
}

// answer is the policy to answer, read, and read again when what it read cannot be answered as it is:
//   - a caller unconfirmed for callerKept, which the read names in no answer: expire drops it, under a new
//     version, so no replica is told 304 for a version that named it;
//   - another start of the database's server than the last answer read — a restore or a failover always restarts
//     it — or a seq lower than one this process answered under the same epoch, which the latest confirms: a
//     new epoch is drawn first, so a version a replica holds is never answered again with other content, even
//     once writes have taken seq back past it (contracts/routes.md);
//   - another signing root than the last answer named, which a root passing its end changes with no row written that
//     moves seq: signed moves seq and records it, so no replica is told 304 for a version under another signing root.
func (s *policyServer) answer(ctx context.Context) (egressPolicy, error) {
	for range 4 {
		p, err := s.read(ctx)
		if err != nil {
			return p, err
		}
		if p.aged {
			if err := s.expire(ctx); err != nil {
				return p, err
			}
			continue
		}
		if deref(p.SigningRoot) != deref(p.recorded) {
			if err := s.signed(ctx, p.SigningRoot); err != nil {
				return p, err
			}
			continue
		}
		if at, err := keysFingerprint(p.Kubernetes); err != nil || at != deref(p.keysAt) {
			if err == nil {
				err = s.keyed(ctx, at)
			}
			if err != nil {
				return p, err
			}
			continue
		}
		redraw, err := s.moved(ctx, p)
		if err != nil || !redraw {
			return p, err
		}
		if err := drawEpoch(ctx, s.pool); err != nil {
			return p, err
		}
	}
	return egressPolicy{}, errors.New("the policy kept changing under its read")
}

// moved says whether p must be answered under a new epoch: the database's server started again since the last
// answer, or p's seq is below one this process answered under p's epoch and the latest seq is too (a request
// whose snapshot is older than another's is no restore). Else it records p as answered.
func (s *policyServer) moved(ctx context.Context, p egressPolicy) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.highest == nil {
		s.highest = map[string]int64{}
	}
	if !s.pgStart.IsZero() && !p.pgStart.Equal(s.pgStart) {
		s.log.Printf("canyon-server: egress policy: the database started again at %s: a new epoch", p.pgStart.UTC().Format(time.RFC3339))
		s.pgStart = p.pgStart
		return true, nil
	}
	s.pgStart = p.pgStart
	if p.Seq >= s.highest[p.Epoch] {
		s.highest[p.Epoch] = p.Seq
		return false, nil
	}
	var latest int64
	err := s.pool.QueryRow(ctx, "SELECT seq FROM egress_policy WHERE epoch = $1", p.Epoch).Scan(&latest)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && latest >= s.highest[p.Epoch] {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.log.Printf("canyon-server: egress policy: seq %d under epoch %s, below the %d answered: a new epoch", latest, p.Epoch, s.highest[p.Epoch])
	return true, nil
}

// signed records root as the signing root the policy names, moving seq when it was another (contracts/routes.md,
// the policy row).
func (s *policyServer) signed(ctx context.Context, root *string) error {
	_, err := s.pool.Exec(ctx, "UPDATE egress_policy SET signing_root = $1, seq = seq + 1 WHERE signing_root IS DISTINCT FROM $1", root)
	return err
}

// keyed records at as the key set's fingerprint the policy names, moving seq when it was another.
func (s *policyServer) keyed(ctx context.Context, at string) error {
	_, err := s.pool.Exec(ctx, "UPDATE egress_policy SET kube_keys = $1, seq = seq + 1 WHERE kube_keys IS DISTINCT FROM $1", nullable(at != "", at))
	return err
}

// keysFingerprint is the sha256 of k's JSON, hex: "" on Docker, where the policy names none.
func keysFingerprint(k *policyKube) (string, error) {
	if k == nil {
		return "", nil
	}
	b, err := json.Marshal(k)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), err
}

// expire drops each caller unconfirmed for callerKept, a version moved for them.
func (s *policyServer) expire(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM egress_callers WHERE resolved_at < now() - make_interval(secs => $1)", callerKept.Seconds())
	return err
}

// read is the policy as one snapshot: its version and everything it says, read together, and the database
// server's start; it names no caller unconfirmed for callerKept at the snapshot's own now(), and says whether it
// left one out (aged).
func (s *policyServer) read(ctx context.Context) (egressPolicy, error) {
	p := egressPolicy{Format: policyFormat, PublicURL: s.public, Callers: []policyCaller{}, Envs: []policyEnv{}}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var cut time.Time
		if err := tx.QueryRow(ctx, `SELECT epoch, seq, now() - make_interval(secs => $1), EXISTS (SELECT 1 FROM egress_callers WHERE resolved_at < now() - make_interval(secs => $1)), signing_root, kube_keys
			FROM egress_policy`, callerKept.Seconds()).Scan(&p.Epoch, &p.Seq, &cut, &p.aged, &p.recorded, &p.keysAt); err != nil {
			return err
		}
		var err error
		if p.pgStart, err = s.start(ctx, tx); err != nil {
			return err
		}
		if p.Platform, err = s.platform(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT address, project_id, env FROM egress_callers WHERE resolved_at >= $1 ORDER BY address COLLATE "C"`, cut)
		if err != nil {
			return err
		}
		if p.Callers, err = pgx.AppendRows(p.Callers, rows, pgx.RowToStructByPos[policyCaller]); err != nil {
			return err
		}
		var destinations int
		if err := tx.QueryRow(ctx, "SELECT cap_new_names, cap_destinations FROM egress_settings").Scan(&p.Caps.NewNames, &destinations); err != nil {
			return err
		}
		// known: the names a call of the env went through to within the records' keep, the latest first up to the
		// destinations cap or 500, the smaller: egress.schema.json's maxItems, past which every egress-control, an older
		// one too, refuses the policy whole — never one only refused, so no app makes a name known by being refused
		rows, err = tx.Query(ctx, `SELECT e.project_id, e.env, coalesce(m.mode, s.mode),
				coalesce(array_agg(a.entry ORDER BY a.entry COLLATE "C") FILTER (WHERE a.entry IS NOT NULL AND a.entry <> ALL($1)), '{}'),
				coalesce((SELECT array_agg(k.destination) FROM (SELECT x.destination FROM egress_destinations x
					WHERE x.app = e.project_id AND x.env = e.env AND x.destination <> 'invalid' AND x.admitted_at >= now() - make_interval(secs => $2)
					GROUP BY x.destination ORDER BY max(x.admitted_at) DESC, x.destination COLLATE "C" LIMIT least($3, 500)) k), '{}'),
				e.token_only
			FROM egress_envs e CROSS JOIN egress_settings s LEFT JOIN egress_modes m USING (project_id, env) LEFT JOIN egress_allowed a USING (project_id, env)
			GROUP BY e.project_id, e.env, e.token_only, m.mode, s.mode ORDER BY e.project_id COLLATE "C", e.env`, p.Platform, egressKept.Seconds(), destinations)
		if err != nil {
			return err
		}
		if p.Envs, err = pgx.AppendRows(p.Envs, rows, pgx.RowToStructByPos[policyEnv]); err != nil {
			return err
		}
		for i := range p.Envs { // a dotted IPv4 address a record keeps is no name
			p.Envs[i].Known = slices.DeleteFunc(p.Envs[i].Known, func(d string) bool { _, err := netip.ParseAddr(d); return err == nil })
			slices.Sort(p.Envs[i].Known)
			if s.kube != nil { // every env's, from before its first /run: its new pod's first call finds it
				p.Envs[i].Account = executor.ServiceAccount(p.Envs[i].App, p.Envs[i].Env)
			}
		}
		envs := make([]enterprise.AppEnv, len(p.Envs))
		for i, e := range p.Envs {
			envs[i] = enterprise.AppEnv{App: e.App, Env: e.Env}
		}
		routes, signing, err := s.ee.policyRoutes(ctx, tx, envs)
		for i := range p.Envs {
			p.Envs[i].Routes = append([]policyRoute{}, routes[envs[i]]...)
		}
		p.SigningRoot = nullable(signing != "", signing)
		return err
	})
	if err == nil && s.kube != nil {
		p.Kubernetes, p.Format = s.kube.policy(ctx), policyFormatKube
	}
	return p, err
}

// routeValue answers one route's value to egress-control, which asks on first use (contracts/routes.md, GET
// /egress/route-value): exactly app, env and destination, each once, else 400; 404 when the policy names no such app
// env or the env holds no route of that destination, and always without Canyon Enterprise; else {rev, value}, never
// stored by a cache. The value is in no log line or error: one line names who asked, the app, the env and the
// destination.
func (s *policyServer) routeValue(w http.ResponseWriter, r *http.Request, replica string) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	for _, k := range []string{"app", "env", "destination"} {
		if err != nil || len(q) != 3 || len(q[k]) != 1 {
			writeErr(w, http.StatusBadRequest, "invalid_request", "the query is app, env and destination, each once", nil)
			return
		}
	}
	app, env, dest := q.Get("app"), q.Get("env"), q.Get("destination")
	s.log.Printf("canyon-server: route value of %q %q %q for %q", app, env, dest, replica)
	rev, value, err := s.ee.routeValue(r.Context(), s.pool, app, env, dest)
	if errors.Is(err, enterprise.ErrNoRoute) {
		writeErr(w, http.StatusNotFound, "no_route", "the policy names no such app env, or it holds no route to that destination", nil)
		return
	}
	if err != nil {
		s.log.Printf("canyon-server: route value of %q %q %q: %v", app, env, dest, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"rev": rev, "value": value})
}

// leaf answers one inject destination's leaf to egress-control (contracts/routes.md, GET /egress/leaf): exactly
// destination, once, else 400; then Canyon Enterprise's, read in one snapshot — 404 unless an app env holds a route
// to it, and always without Canyon Enterprise; 503 while no root is live; else a leaf minted for this answer and kept
// nowhere, never stored by a cache. No key is in a log line or an error: one line names who asked and the
// destination.
func (s *policyServer) leaf(w http.ResponseWriter, r *http.Request, replica string) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(q) != 1 || len(q["destination"]) != 1 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "the query is destination, once", nil)
		return
	}
	dest := q.Get("destination")
	s.log.Printf("canyon-server: leaf of %q for %q", dest, replica)
	var answer enterprise.Leaf
	err = pgx.BeginTxFunc(r.Context(), s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		answer, err = s.ee.leaf(r.Context(), tx, dest, time.Now())
		return err
	})
	var no *enterprise.Refusal
	switch {
	case errors.As(err, &no):
		writeErr(w, no.Status, no.Code, no.Message, no.Fields)
		return
	case err != nil:
		s.log.Printf("canyon-server: leaf of %q: %v", dest, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, answer)
}

// start is when the database's server started: s.started, else pg_postmaster_start_time().
func (s *policyServer) start(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	if s.started != nil {
		return s.started(ctx, tx)
	}
	var at time.Time
	err := tx.QueryRow(ctx, "SELECT pg_postmaster_start_time()").Scan(&at)
	return at, err
}

// seen records that replica asked holding have, and whether it is answered p: its policy is current now when
// have is p's version, as of the last answer it was served when have is that one, and never when it holds none.
func (s *policyServer) seen(ctx context.Context, replica, have string, p egressPolicy) error {
	var haveEpoch *string
	var haveSeq *int64
	if m := policyVersion.FindStringSubmatch(have); m != nil {
		if seq, err := strconv.ParseInt(m[2], 10, 64); err == nil {
			haveEpoch, haveSeq = &m[1], &seq
		}
	}
	served := have != p.Epoch+":"+strconv.FormatInt(p.Seq, 10)
	_, err := s.pool.Exec(ctx, `INSERT INTO egress_replicas AS r (replica, have_epoch, have_seq, seen_at, current_at, served_epoch, served_seq, served_at)
		VALUES ($1, $2, $3, now(), CASE WHEN NOT $6 THEN now() END, CASE WHEN $6 THEN $4 END, CASE WHEN $6 THEN $5::bigint END, CASE WHEN $6 THEN now() END)
		ON CONFLICT (replica) DO UPDATE SET have_epoch = $2, have_seq = $3, seen_at = now(),
			current_at = CASE WHEN NOT $6 THEN now()
				WHEN $3::bigint IS NULL THEN NULL
				WHEN $2 = r.served_epoch AND $3 = r.served_seq THEN greatest(r.current_at, r.served_at)
				ELSE r.current_at END,
			served_epoch = CASE WHEN $6 THEN $4 ELSE r.served_epoch END,
			served_seq = CASE WHEN $6 THEN $5::bigint ELSE r.served_seq END,
			served_at = CASE WHEN $6 THEN now() ELSE r.served_at END`,
		replica, haveEpoch, haveSeq, p.Epoch, p.Seq, served)
	return err
}

// resolve asks the executor who holds address, once, when the policy does not name it: an app env Canyon runs
// under via canyon is its caller from the next version on; anyone else, or no one, leaves it unknown.
func (s *policyServer) resolve(ctx context.Context, address string) error {
	var known bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM egress_callers WHERE address = $1)", address).Scan(&known); err != nil || known {
		return err
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	held, err := s.callers(call, []string{address})
	if err != nil {
		return err
	}
	for _, c := range held {
		if c.Address != address {
			continue
		}
		_, err = s.pool.Exec(ctx, `INSERT INTO egress_callers (address, project_id, env) SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM egress_envs WHERE project_id = $2 AND env = $3)
			ON CONFLICT (address) DO UPDATE SET project_id = EXCLUDED.project_id, env = EXCLUDED.env, resolved_at = now()`, address, c.App, c.Target)
		return err
	}
	return nil
}

// callerKept bounds how long a caller the executor does not confirm keeps its app env's policy: no answer names
// one unconfirmed for it at its own snapshot, and one that finds such a caller drops it first, under a new
// version (answer, expire), however long a confirm takes — two confirms; with egress-control's own 30 s, a
// minute in all (design/EGRESS.md §3.2). One the executor names otherwise, or not at all, goes at the confirm that
// asks, every confirmEvery. An env's callers go with it at once (the migration's egress_env_of_deploy: an
// undeploy, a delete, a hold, a hand-off, via off).
const (
	confirmEvery = 15 * time.Second
	callerKept   = 30 * time.Second
)

// confirm drops each caller unconfirmed for callerKept and each replica unseen for a day, first; then asks the
// executor again who holds each caller's address, in one call per 1,024: an address it names as before stays,
// confirmed; one it names otherwise, or not at all, leaves the policy at once.
func (s *policyServer) confirm(ctx context.Context) error {
	if err := s.expire(ctx); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM egress_replicas WHERE seen_at < now() - interval '1 day'"); err != nil {
		return err
	}
	type held struct {
		address, app, env string
		at                time.Time
	}
	rows, err := s.pool.Query(ctx, `SELECT address, project_id, env, resolved_at FROM egress_callers ORDER BY address COLLATE "C"`)
	if err != nil {
		return err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (h held, err error) {
		return h, r.Scan(&h.address, &h.app, &h.env, &h.at)
	})
	if err != nil {
		return err
	}
	for start := 0; start < len(all); start += 1024 {
		batch := all[start:min(start+1024, len(all))]
		addrs, asOf := make([]string, len(batch)), time.Time{}
		for i, h := range batch {
			addrs[i] = h.address
			if h.at.After(asOf) {
				asOf = h.at
			}
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		answer, err := s.callers(call, addrs)
		cancel()
		if err != nil { // unconfirmed: each goes once callerKept has passed
			s.log.Printf("canyon-server: egress policy: confirm %d caller(s): %v", len(addrs), err)
			continue
		}
		now := map[string]executor.Caller{}
		for _, c := range answer {
			now[c.Address] = c
		}
		var confirmed, gone []string
		for _, h := range batch {
			if c, ok := now[h.address]; ok && c.App == h.app && c.Target == h.env {
				confirmed = append(confirmed, h.address)
			} else {
				gone = append(gone, h.address)
			}
		}
		if _, err := s.pool.Exec(ctx, "UPDATE egress_callers SET resolved_at = now() WHERE address = ANY($1)", confirmed); err != nil {
			return err
		}
		if _, err := s.pool.Exec(ctx, "DELETE FROM egress_callers WHERE address = ANY($1) AND resolved_at <= $2", gone, asOf); err != nil { // never one resolved again meanwhile
			return err
		}
	}
	return nil
}

// confirming runs confirm every confirmEvery until ctx ends.
func (s *policyServer) confirming(ctx context.Context) {
	t := time.NewTicker(confirmEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.confirm(ctx); err != nil && ctx.Err() == nil {
				s.log.Printf("canyon-server: egress policy: confirm callers: %v", err)
			}
		}
	}
}

// egressPolicyAddr is where the server serves the policy egress-control pulls (contracts/routes.md, Egress control):
// CANYON_EGRESS_POLICY_ADDR, set; unset, on Docker with the receiver on, the receiver's address and port 7782, which
// the holder's floor admits by the same default, so the append-only compose file names it nowhere; else none, "".
// receiver is CANYON_EGRESS_RECEIVER_ADDR, already held to its own rules; one on 7782 leaves the policy no port.
func egressPolicyAddr(set, receiver string, kube bool) (string, error) {
	if set != "" || kube || receiver == "" {
		return set, nil
	}
	ap, err := netip.ParseAddrPort(receiver)
	if err != nil {
		return "", fmt.Errorf("CANYON_EGRESS_POLICY_ADDR: unset, it is the receiver's address on 7782, and %q is no address and port", receiver)
	}
	if ap.Port() == 7782 {
		return "", errors.New("CANYON_EGRESS_POLICY_ADDR: unset, it is the receiver's address on 7782, the receiver's own port: set it")
	}
	return netip.AddrPortFrom(ap.Addr(), 7782).String(), nil
}

// serveEgressPolicy serves GET /egress/policy, POST /egress/dropped, GET /egress/route-value and GET /egress/leaf at
// addr until the process ends, and confirms the callers every 15 s; first Canyon Enterprise makes sure the host holds
// an interception root, then asks every hour whether Canyon's own rotation of it is due. On
// Kubernetes it serves TLS under policyCert and takes egress-control's token, read against the key set with what the
// server's projected volume at tokens holds; on Docker it closes the app pool at accept.
func serveEgressPolicy(ctx context.Context, addr string, kube bool, apps netip.Prefix, a *app, keys *vault.Vault, tokens string) error {
	if err := egressAddrOf("CANYON_EGRESS_POLICY_ADDR", "the policy", "7782", addr, kube, apps); err != nil {
		return err
	}
	if err := a.ee.startOutbound(ctx, a.pool, log.Default()); err != nil {
		return err
	}
	s := &policyServer{pool: a.pool, public: a.public, platform: a.platformDestinations, callers: a.pipe.Executor.Callers, replica: peerReplica, ee: a.ee, log: log.Default()}
	if a.egress != nil {
		s.lost = a.egress.envoy.add
	}
	var tlsConfig *tls.Config
	if kube {
		k, err := kubeTokensOf(tokens, s.log)
		if err != nil {
			return err
		}
		cert, err := policyCert(ctx, a.pool, keys)
		if err != nil {
			return err
		}
		s.replica, s.kube, tlsConfig = k.replica, k, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	}
	var gateway netip.Addr // Docker's: canyon-egress-ctl's gateway, the host's own address on it
	if !kube {
		addrs, err := net.InterfaceAddrs()
		if err == nil {
			gateway, err = gatewayOf(netip.MustParseAddrPort(addr).Addr(), addrs)
		}
		if err != nil {
			return fmt.Errorf("CANYON_EGRESS_POLICY_ADDR: %w, so its network's gateway, the host, cannot be refused", err)
		}
	}
	ln, err := listenReceiver(addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	go func() { log.Fatal(srv.Serve(policyListener(ln, apps, tlsConfig, gateway))) }()
	go s.confirming(ctx)
	return nil
}

// policyListener is what admits a caller to the policy: on Kubernetes TLS, the token checked per request; on
// Docker, which has no token, the listener itself — every address of the apps' pool, each /24's gateway too,
// canyon-egress-ctl's gateway, which is the host, and every IPv6 peer but loopback is closed at accept, before a
// byte is read, so neither an app nor a process of the host reads the policy, makes the server ask the executor
// for anyone, or poses as a replica a deploy waits on.
func policyListener(ln net.Listener, apps netip.Prefix, tlsConfig *tls.Config, gateway netip.Addr) net.Listener {
	if tlsConfig != nil {
		return tls.NewListener(ln, tlsConfig)
	}
	return refusePeer{refuseAppPool(ln, apps), gateway}
}

// gatewayOf is the gateway of the network addr is the server's address on, among the host's addrs: the
// network's first address, which Docker gives its gateway, the host's own on that bridge.
func gatewayOf(addr netip.Addr, addrs []net.Addr) (netip.Addr, error) {
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if ones, bits := n.Mask.Size(); ok && ip.Unmap() == addr && bits == 32 {
			if ones > 30 { // a /31 or /32 holds no gateway: its next address is another host's, such as a Calico pod's neighbour
				return netip.Addr{}, fmt.Errorf("%s is on %s, a /%d, which has no gateway", addr, netip.PrefixFrom(addr, ones).Masked(), ones)
			}
			return netip.PrefixFrom(addr, ones).Masked().Addr().Next(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s is on no network of this server's", addr)
}

// refusePeer closes every connection from peer at accept.
type refusePeer struct {
	net.Listener
	peer netip.Addr
}

func (l refusePeer) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok && a.AddrPort().Addr().Unmap() == l.peer {
			_ = c.Close()
			continue
		}
		return c, nil
	}
}

// peerReplica is a Docker replica's name: its address on canyon-egress-ctl, the only network that reaches the
// listener.
func peerReplica(r *http.Request) (string, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return host, err == nil
}

// drawEpoch draws the policy's epoch for this start: 128 random bits, so a database restored to an older seq
// never answers it under the epoch it was answered under before (contracts/routes.md, Egress control).
func drawEpoch(ctx context.Context, pool *pgxpool.Pool) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	tag, err := pool.Exec(ctx, "UPDATE egress_policy SET epoch = $1", hex.EncodeToString(b))
	if err == nil && tag.RowsAffected() != 1 {
		err = errors.New("egress_policy has no row")
	}
	return err
}
