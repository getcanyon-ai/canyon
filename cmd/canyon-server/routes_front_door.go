package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	neturl "net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/destination"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The host's front door on Kubernetes (contracts/routes.md, the front-door row): how people reach apps,
// one per host. The PUT runs the executor's probe and writes only when the cluster enforces
// NetworkPolicy and the certificate covers the form; choice 2's sign-in client secret is sealed as the
// row ("front-door", "signin", "client_secret") and never answered, logged or shown.
func routesFrontDoor(api *http.ServeMux, a *app) {
	api.HandleFunc("GET /front-door", a.getFrontDoor)
	api.HandleFunc("PUT /front-door", a.adminOnly(a.putFrontDoor))
}

// The D5 strings the refusals carry (design/EXPERIENCE.md).
const (
	onDocker          = "This Canyon runs apps on Docker and serves them itself, so there's nothing to set here."
	probeUnavailable  = "Canyon could not finish checking this cluster. Try again in a minute; if it keeps failing, Canyon's log says why."
	noSecretStored    = "signin.client_secret: required, as none is stored for this issuer and client_id"
	signinUnsupported = "Your platform signs people in: set the front door to choice 1 first." // design/HAND-OFF.md §6: choice 2 under run_by: org
	// What run_by: org needs of the server itself (design/HAND-OFF.md §1, #2010).
	identityImageRequired = "Canyon's server has no CANYON_IMAGE_IDENTITY, the sign-in sidecar every release puts in your pods. Set it to the executor's value, the canyon-identity image by digest, and restart the server."
	httpsRequired         = "Canyon's address, CANYON_PUBLIC_URL, is not https, and the sign-in sidecar in your pods won't start without it. Set it to Canyon's https address and restart the server."
	// Whether a grant to a person applies through the front door (design/PERMISSIONS.md, Grants; #2299).
	grantsOtherIssuer = "People sign in at this front door through another issuer than Canyon's, so an app shared with a person won't let them in. Sharing with a group or with everyone still does."
	grantsALB         = "An app shared with a person lets them in only if the load balancer signs people in through Canyon's own sign-in. Sharing with a group or with everyone works either way."
	// Whether apps routed through Canyon reach the front door's sign-in: not one served through the cluster's own gateway (#2316).
	egressGatewayNote = "Apps routed through Canyon can't reach a sign-in served through this cluster's own gateway, so nobody can sign in to them. Use another sign-in provider here, or turn off Through Canyon in Settings › Outbound."
	discoveryNote     = "Canyon couldn't read your sign-in provider's discovery document yet and keeps trying every minute; until it does, apps whose outbound mode is allowlist can't sign people in."
	// The daily read of a recorded discovery failing: the hosts it recorded stand, a moved endpoint's not among them (#2389).
	discoveryRefreshFailed = "Canyon couldn't read your sign-in provider's discovery document in its daily check, and keeps trying every minute. If your provider moved an endpoint, apps whose outbound mode is allowlist can't sign people in until Canyon reads it again."
	// A door stored without passing the probe, and the switch back to canyon behind one (#2438).
	notChecked         = "Not checked on this cluster: your platform can run apps behind this front door, but Canyon won't until a save passes the check."
	notEnforcing       = "Not checked on this cluster: Canyon builds nothing here until a save finds NetworkPolicy enforced, so no deploy goes through, whoever runs the apps."
	frontDoorUnchecked = "The front door hasn't passed Canyon's check of this cluster, so Canyon can't run apps here. Save the front door again so Canyon checks it, then switch."
)

// probeWait bounds the save's wait on the probe: two probes' 60 s (the executor runs one at a time, so
// another save's may be ahead) and slack. Past it the probe gave no answer, probe_unavailable.
const probeWait = 150 * time.Second

// hostDoor is /run's front_door (contracts/executor.md, The front door) without the client secret:
// what the row stores and the GET answers. Comparable, so a save can tell it changes nothing.
type hostDoor struct {
	Choice   int    `json:"choice"`
	Gateway  string `json:"gateway,omitempty"`
	Issuer   string `json:"issuer,omitempty"`
	Audience string `json:"audience,omitempty"`
	JWKSURL  string `json:"jwks_url,omitempty"`
	Cookie   string `json:"cookie,omitempty"`
	ALBARN   string `json:"alb_arn,omitempty"`
	Signin   signin `json:"signin,omitzero"`
	Domain   string `json:"domain"`
	Form     string `json:"form"`
}

type signin struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// storedDoor is the row: the GET's front_door, and the sealed client secret, choice 2's alone.
type storedDoor struct {
	hostDoor
	SetBy  actor  `json:"set_by"`
	SetAt  string `json:"set_at"`
	Probed bool   `json:"probed"` // the save that stored it passed the probe: Canyon runs apps behind no other (#2438)
	// Enforcing is whether the save that stored it found NetworkPolicy enforced, whatever the certificate: nothing builds
	// behind any other, under either run_by (#2507).
	Enforcing bool `json:"enforcing"`

	keyRef                 *int
	sealedKey, sealedValue []byte
	warning                string // the probe's at the save that stored it: the GET's warning, beside the door
	probeFix               string // why it is not probed: the probe's fix, or probeUnavailable; "" when probed, or stored before #2438
	recorded               bool   // a choice 2's issuer has its discovery's hosts recorded (front_door_discovery)
	refreshFailed          bool   // and the loop's daily read of it has failed since it last read it (#2389)
}

var (
	domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	httpsRe  = regexp.MustCompile(`^https://[^@/?#\s]+(/\S*)?$`)
	tokenRe  = regexp.MustCompile(`^[\x21-\x7e]+$`) // at most 1024: validToken
	cookieRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	albARNRe = regexp.MustCompile(`^arn:aws:elasticloadbalancing:[a-z]{2}-[a-z]+-[0-9]:[0-9]{12}:loadbalancer/app/[A-Za-z0-9-]{1,32}/[0-9a-f]{16}$`)
)

func validHTTPS(s string) bool { return utf8.RuneCountInString(s) <= 2048 && httpsRe.MatchString(s) } // maxLength counts code points

func validToken(s string) bool { return len(s) <= 1024 && tokenRe.MatchString(s) }

// validDomain is the schema's domain: two labels or more, lower case, at most 189 characters, and a
// first label that is neither preview nor prod.
func validDomain(d string) bool {
	return len(d) <= 189 && domainRe.MatchString(d) && !strings.HasPrefix(d, "preview.") && !strings.HasPrefix(d, "prod.")
}

// text is raw when it is a JSON string; false for anything else, null among it.
func text(raw json.RawMessage) (string, bool) {
	var s *string
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return "", false
	}
	return *s, true
}

func readDoor(ctx context.Context, q querier) (*storedDoor, error) {
	var d storedDoor
	var at time.Time
	err := q.QueryRow(ctx, `SELECT door, set_at, set_by_kind, set_by_subject, set_by_display, key_ref, sealed_key, sealed_value, warning,
		probed_at IS NOT NULL, network_policy_enforced_at IS NOT NULL, probe_fix, f.issuer IS NOT NULL, f.refresh_failed_at IS NOT NULL
		FROM front_doors LEFT JOIN front_door_discovery f ON f.issuer = door->'signin'->>'issuer'`).
		Scan(&d.hostDoor, &at, &d.SetBy.Kind, &d.SetBy.Subject, &d.SetBy.Display, &d.keyRef, &d.sealedKey, &d.sealedValue, &d.warning, &d.Probed, &d.Enforcing, &d.probeFix, &d.recorded, &d.refreshFailed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	d.SetAt = at.UTC().Format(time.RFC3339)
	return &d, err
}

// doorAnswer is the GET's shape: the runtime, the public URL's host, the front door, null on Docker,
// the warning its save's probe gave, "" without one, whether person grants apply through it (grantsNote),
// whether apps routed through Canyon reach its sign-in under the host's via (egress_note), and what the
// server lacks to serve run_by: org, none on Docker, and, for a door not probed, D5's line and why (#2438).
func (a *app) doorAnswer(d *storedDoor, via string) map[string]any {
	runtime, needs := "docker", []handOffNeed{}
	if a.kube {
		runtime, needs = "kubernetes", handOffNeeds(a.pipe)
	} else {
		d = nil
	}
	warning, note, egress, discovery, unchecked := "", "", "", "", ""
	if d != nil {
		switch {
		case !d.Enforcing: // nothing builds behind it, whoever runs the apps (#2507)
			unchecked = strings.TrimSpace(notEnforcing + " " + d.probeFix)
		case !d.Probed:
			unchecked = strings.TrimSpace(notChecked + " " + d.probeFix)
		}
		warning, note = d.warning, grantsNote(d.hostDoor, a.signinIssuer())
		if via == "canyon" && a.behind != "" && signsInAt(d.hostDoor, a.behind) {
			egress = egressGatewayNote
		}
		if u, err := neturl.Parse(d.Signin.Issuer); d.Choice == 2 && err == nil && isDestination(u.Hostname()) {
			switch {
			case !d.recorded:
				discovery = discoveryNote
			case d.refreshFailed:
				discovery = discoveryRefreshFailed
			}
		}
	}
	return map[string]any{"runtime": runtime, "default_domain": a.defaultDomain(), "front_door": d, "warning": warning, "grants_note": note, "egress_note": egress, "discovery_note": discovery, "hand_off_needs": needs, "probe_note": unchecked}
}

// signinURLs are the URLs the door signs people in at, its issuer first: choice 2's signin.issuer, else choice
// 1's issuer and jwks_url — "" each for an alb, whose issuer Canyon does not hold.
func signinURLs(door hostDoor) []string {
	if door.Choice == 2 {
		return []string{door.Signin.Issuer}
	}
	return []string{door.Issuer, door.JWKSURL}
}

// signsInAt is whether the door signs people in at host, one of its signinURLs' hosts compared without case —
// never an alb's (contracts/routes.md, the egress row; #2316).
func signsInAt(door hostDoor, host string) bool {
	return slices.ContainsFunc(signinURLs(door), func(s string) bool {
		u, err := neturl.Parse(s)
		return s != "" && err == nil && strings.EqualFold(u.Hostname(), host)
	})
}

// hostVia is the host's egress via, off or canyon (design/EGRESS.md §2.5).
func hostVia(ctx context.Context, q querier) (string, error) {
	var via string
	err := q.QueryRow(ctx, "SELECT via FROM egress_settings").Scan(&via)
	return via, err
}

// grantsNote is the read's grants_note (contracts/routes.md, the front-door row): the sidecar matches a
// person's bound grant only on a token from Canyon's own sign-in's issuer, compared exactly — the door's
// issuer, choice 2's signin.issuer — and an alb's it learns only from a request the load balancer signs.
// "" on Canyon's issuer, and with sign-in off, where every entry goes unbound and matches by address.
func grantsNote(door hostDoor, issuer string) string {
	from := signinURLs(door)[0]
	switch {
	case issuer == "":
		return ""
	case door.Gateway == "alb":
		return grantsALB
	case from != issuer:
		return grantsOtherIssuer
	}
	return ""
}

// defaultDomain is the configured public URL's host, without its port.
func (a *app) defaultDomain() string {
	u, err := neturl.Parse(a.public)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (a *app) getFrontDoor(w http.ResponseWriter, r *http.Request) {
	d, err := readDoor(r.Context(), a.pool)
	var via string
	if err == nil {
		via, err = hostVia(r.Context(), a.pool)
	}
	if err != nil {
		a.log.Printf("canyon-server: read the front door: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, http.StatusOK, a.doorAnswer(d, via))
}

func (a *app) putFrontDoor(w http.ResponseWriter, r *http.Request) {
	if !a.kube {
		writeErr(w, http.StatusConflict, "not_kubernetes", onDocker, nil)
		return
	}
	var body map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil || body == nil || dec.Decode(&struct{}{}) != io.EOF { //nolint:errorlint // sentinel from the decoder itself
		writeErr(w, http.StatusBadRequest, "invalid_front_door", "the body is not one JSON object", nil)
		return
	}
	form, _ := text(body["form"])
	if form != "app.env.domain" && form != "app-env.domain" {
		writeErr(w, http.StatusBadRequest, "invalid_form", "Choose how app addresses look: app.env.domain or app-env.domain.", nil)
		return
	}
	domain, ok := a.defaultDomain(), true
	if raw, named := body["domain"]; named {
		domain, ok = text(raw)
	}
	if !ok || !validDomain(domain) {
		writeErr(w, http.StatusBadRequest, "invalid_domain", "Enter a domain like apps.example.com: lower-case letters, digits and dashes, two or more parts separated by dots, at most 189 characters, and not starting with preview. or prod.", nil)
		return
	}
	ctx := r.Context()
	stored, err := readDoor(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: read the front door: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	door, secret, refused := doorOf(body, stored)
	if refused != "" {
		writeErr(w, http.StatusBadRequest, "invalid_front_door", refused, nil)
		return
	}
	door.Domain, door.Form = domain, form
	// Under run_by: org the door goes in each release's sidecar spec and choice 2 is refused
	// (contracts/routes.md, the front-door row; design/HAND-OFF.md §6). Read here, and again under
	// the lock below, where a preferences PUT switching run_by is serialized against this save.
	org, err := runByOrg(ctx, a.pool)
	if err != nil {
		a.log.Printf("canyon-server: read run_by: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	if org && door.Choice == 2 {
		writeErr(w, http.StatusConflict, "signin_unsupported", signinUnsupported, nil)
		return
	}
	var discovered []string // nil: none read, which never refuses the save — the minute loop tries again
	if door.Choice == 2 {
		if discovered, err = a.discoveredHosts(ctx, door.Signin.Issuer); err != nil && !errors.Is(err, errGoesDirect) {
			a.log.Printf("canyon-server: the front door's discovery, which the server tries again every minute: %s", oneLine(err.Error()))
		}
	}
	// Every save asks the probe (#2438). A door that passes is probed. One the probe found unenforced or uncovered is
	// refused — but a choice 1 while a platform webhook or a deploy repo is saved, always so under org, which is stored not probed: the
	// org's platform can run apps behind it, and Canyon runs none (runs' doorSet and runConfig). A probe that could not
	// finish refuses the save under canyon, changing nothing — a passing blip must never demote a probed door and stop
	// every deploy — and under org stores a choice 1 not probed, as nothing there is ever refused. Every save stored
	// records whether the probe found NetworkPolicy enforced: under either run_by nothing builds where it was not (#2507).
	var handing bool // where releases go is saved: the platform webhook, or the deploy repo (#2470)
	if err := a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM platform_webhook) OR EXISTS (SELECT 1 FROM platform_repo)").Scan(&handing); err != nil {
		a.log.Printf("canyon-server: read where releases go: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	mayGoUnprobed := door.Choice == 1 && handing
	pctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	probe, err := a.pipe.Executor.Probe(pctx, domain, form)
	probed, warning, fix := false, "", ""
	enforced := err == nil && probe.NetworkPolicy == "enforced" // whatever the certificate: what a build needs (#2507)
	switch {
	case err != nil:
		a.log.Printf("canyon-server: the front door's probe: %v", err)
		if !mayGoUnprobed || !org {
			writeErr(w, http.StatusBadGateway, "probe_unavailable", probeUnavailable, nil)
			return
		}
		fix = probeUnavailable
	case probe.NetworkPolicy != "enforced" || probe.Certificate != "ok":
		if !mayGoUnprobed {
			// The cluster was just seen not to enforce NetworkPolicy: whatever door the PUT names, the stored one is no
			// longer probed or enforcing, so nothing runs or builds behind it until a save passes, and its why is this
			// refusal's fix (#2507). A log line, no audit row: nothing was saved.
			if probe.NetworkPolicy != "enforced" {
				var cleared int64
				if err := a.inMutation(ctx, func(tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, "UPDATE front_doors SET probed_at = NULL, network_policy_enforced_at = NULL, probe_fix = $1", probe.Fix)
					cleared = tag.RowsAffected()
					return err
				}); err != nil {
					a.log.Printf("canyon-server: clear the front door the probe found unenforced: %v", err)
					writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
					return
				}
				if cleared > 0 {
					a.log.Printf("canyon-server: the front door's probe found NetworkPolicy %s: the stored front door is no longer probed or enforcing, so no deploy builds or runs behind it until a save passes", probe.NetworkPolicy)
				}
			}
			writeErr(w, http.StatusConflict, "probe_failed", probe.Fix, map[string]any{"network_policy": probe.NetworkPolicy, "certificate": probe.Certificate})
			return
		}
		warning, fix = probe.Warning, probe.Fix
	default:
		probed, warning = true, probe.Warning
	}
	var saved *storedDoor
	var via string
	errSecretGone := errors.New(noSecretStored)
	err = a.inMutation(ctx, func(tx pgx.Tx) error {
		if org, err := runByOrg(ctx, tx); err != nil {
			return err
		} else if org && door.Choice == 2 {
			return errSigninUnsupported
		}
		before, err := readDoor(ctx, tx)
		if err == nil {
			via, err = hostVia(ctx, tx)
		}
		if err != nil {
			return err
		}
		if discovered != nil { // added to the issuer's hosts, never in place of one: a sidecar started before keeps the host its
			// document named then, as for jwks_uri moved to another host; each is a host of the admin's own issuer's discovery
			// (design/EGRESS.md §3.3), so keeping it widens nothing that issuer did not name
			if _, err := tx.Exec(ctx, `INSERT INTO front_door_discovery (issuer, hosts) VALUES ($1, $2) ON CONFLICT (issuer)
				DO UPDATE SET hosts = ARRAY(SELECT DISTINCT h FROM unnest(front_door_discovery.hosts || EXCLUDED.hosts) h ORDER BY h), recorded_at = now(), refresh_failed_at = NULL`,
				door.Signin.Issuer, discovered); err != nil {
				return err
			}
		}
		if secret == "" && before != nil && before.hostDoor == door { // a save that changes nothing: probed, no row, what this probe found
			before.warning, before.Probed, before.Enforcing, before.probeFix, saved = warning, probed, enforced, fix, before
			before.recorded, before.refreshFailed = before.recorded || discovered != nil, before.refreshFailed && discovered == nil
			_, err := tx.Exec(ctx, "UPDATE front_doors SET warning = $1, probed_at = CASE WHEN $2 THEN now() END, probe_fix = $3, network_policy_enforced_at = CASE WHEN $4 THEN now() END", warning, probed, fix, enforced)
			return err
		}
		after := storedDoor{hostDoor: door, SetBy: actorOf(ctx), Probed: probed, Enforcing: enforced, warning: warning, probeFix: fix}
		switch {
		case secret != "":
			version, sealedKey, sealedValue, err := a.vault.Seal("front-door", "signin", "client_secret", []byte(secret))
			if err != nil {
				return err
			}
			after.keyRef, after.sealedKey, after.sealedValue = &version, sealedKey, sealedValue
		case door.Choice == 2: // the stored secret, for the same client: read again under the lock another save takes
			if before == nil || before.Signin != door.Signin || before.sealedValue == nil {
				return errSecretGone
			}
			after.keyRef, after.sealedKey, after.sealedValue = before.keyRef, before.sealedKey, before.sealedValue
		}
		var at time.Time
		if _, err := tx.Exec(ctx, "INSERT INTO replaced_front_doors (door) SELECT door FROM front_doors"); err != nil { // in use for 30 minutes more
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM front_doors"); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO front_doors (door, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display, warning, probed_at, probe_fix, network_policy_enforced_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, CASE WHEN $10 THEN now() END, $11, CASE WHEN $12 THEN now() END) RETURNING set_at, EXISTS (SELECT 1 FROM front_door_discovery WHERE issuer = $9),
				EXISTS (SELECT 1 FROM front_door_discovery WHERE issuer = $9 AND refresh_failed_at IS NOT NULL)`,
			door, after.keyRef, after.sealedKey, after.sealedValue, after.SetBy.Kind, after.SetBy.Subject, after.SetBy.Display, warning, door.Signin.Issuer, probed, fix, enforced).Scan(&at, &after.recorded, &after.refreshFailed); err != nil {
			return err
		}
		after.SetAt, saved = at.UTC().Format(time.RFC3339), &after
		return events.AuditTx(ctx, tx, after.SetBy, "set front door", "", "", map[string]any{"before": before, "after": after, "secret_replaced": secret != ""})
	})
	switch {
	case errors.Is(err, errSigninUnsupported):
		writeErr(w, http.StatusConflict, "signin_unsupported", signinUnsupported, nil)
	case errors.Is(err, errSecretGone):
		writeErr(w, http.StatusBadRequest, "invalid_front_door", noSecretStored, nil)
	case err != nil:
		a.log.Printf("canyon-server: write the front door: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
	default:
		writeJSON(w, http.StatusOK, a.doorAnswer(saved, via))
	}
}

// doorKeys are the PUT's keys but domain and form, in the order a refusal names the first.
var doorKeys = []string{"choice", "gateway", "issuer", "audience", "jwks_url", "cookie", "alb_arn", "signin"}

// doorValid is the shape of each of doorKeys' string values but gateway's, which picks the fields.
var doorValid = map[string]func(string) bool{"issuer": validHTTPS, "audience": validToken, "jwks_url": validHTTPS, "cookie": cookieRe.MatchString, "alb_arn": albARNRe.MatchString}

// doorOf reads the body's front door but its domain and form, and the client secret when the body
// carries one, or answers the refusal naming the first key /run's front_door refuses, never a value.
// A choice 2 without a client_secret keeps the stored one, so stored must hold one for the same client.
func doorOf(body map[string]json.RawMessage, stored *storedDoor) (door hostDoor, secret, refused string) {
	for _, k := range slices.Sorted(maps.Keys(body)) {
		if k != "domain" && k != "form" && !slices.Contains(doorKeys, k) {
			return door, "", k + ": not a key of the front door"
		}
	}
	if json.Unmarshal(body["choice"], &door.Choice) != nil || door.Choice != 1 && door.Choice != 2 {
		return door, "", "choice: 1 or 2"
	}
	door.Gateway, _ = text(body["gateway"])
	var need, may []string // the keys this choice requires, and those it allows besides
	switch {
	case door.Choice == 2:
		need = []string{"signin"}
	case door.Gateway == "oidc":
		need = []string{"gateway", "issuer", "audience", "jwks_url", "cookie"}
	case door.Gateway == "alb":
		need, may = []string{"gateway", "alb_arn"}, []string{"cookie"}
	default:
		return door, "", "gateway: oidc or alb"
	}
	to := map[string]*string{"issuer": &door.Issuer, "audience": &door.Audience, "jwks_url": &door.JWKSURL, "cookie": &door.Cookie, "alb_arn": &door.ALBARN}
	for _, k := range doorKeys[1:] {
		raw, has := body[k]
		switch {
		case has && !slices.Contains(need, k) && !slices.Contains(may, k):
			return door, "", k + ": not part of this choice"
		case !has && slices.Contains(need, k):
			return door, "", k + ": required by this choice"
		case !has || to[k] == nil:
			continue
		}
		v, ok := text(raw)
		if !ok || !doorValid[k](v) {
			return door, "", k + ": out of shape"
		}
		*to[k] = v
	}
	if door.Choice == 1 {
		return door, "", ""
	}
	var s map[string]json.RawMessage
	if json.Unmarshal(body["signin"], &s) != nil || s == nil {
		return door, "", "signin: an object of issuer, client_id and client_secret"
	}
	for _, k := range slices.Sorted(maps.Keys(s)) {
		if k != "issuer" && k != "client_id" && k != "client_secret" {
			return door, "", "signin." + k + ": not a key of signin"
		}
	}
	var ok bool
	if door.Signin.Issuer, ok = text(s["issuer"]); !ok || !validHTTPS(door.Signin.Issuer) {
		return door, "", "signin.issuer: missing or out of shape"
	}
	if door.Signin.ClientID, ok = text(s["client_id"]); !ok || !validToken(door.Signin.ClientID) {
		return door, "", "signin.client_id: missing or out of shape"
	}
	if raw, has := s["client_secret"]; has {
		if secret, ok = text(raw); !ok || !validToken(secret) {
			return door, "", "signin.client_secret: out of shape"
		}
	} else if stored == nil || stored.Signin != door.Signin || stored.sealedValue == nil {
		return door, "", noSecretStored
	}
	return door, secret, ""
}

// discoveryCap bounds a discovery document's body: go-oidc reads it whole, and a provider's is a few kilobytes.
const discoveryCap = 1 << 20

// errGoesDirect is an issuer whose host destination.Entry refuses — an IP literal, a single label: it is never
// fetched, and its hosts are no platform destination, as such a provider goes direct (design/EGRESS.md §3.3).
var errGoesDirect = errors.New("the issuer's host is no destination: its provider goes direct, and its discovery is not read")

// discoveredHosts are the hosts of the token, user-info and key set endpoints issuer's discovery document names
// (design/EGRESS.md §3.3), each as destination.Entry canonicalises it, sorted, each once, never nil: a host Entry
// refuses — an IP literal, a single label — is none, as the platform destinations name none. An issuer whose own
// host Entry refuses is errGoesDirect, never fetched. The fetch is the sign-in's own (discoveryClient,
// fetchDiscovery): ten seconds, no redirect followed, the issuer checked, the endpoints https — the
// userinfo_endpoint optional — and the body at most discoveryCap, over a.discovery.
func (a *app) discoveredHosts(ctx context.Context, issuer string) ([]string, error) {
	if u, err := neturl.Parse(issuer); err != nil || !isDestination(u.Hostname()) {
		return nil, errGoesDirect
	}
	client := discoveryClient()
	client.Transport = cappedBody{a.discovery}
	p, err := fetchDiscovery(ctx, client, issuer)
	if err != nil {
		return nil, err
	}
	var doc struct {
		TokenEndpoint    string `json:"token_endpoint"`
		UserinfoEndpoint string `json:"userinfo_endpoint"`
		JWKSURI          string `json:"jwks_uri"`
	}
	if err := p.Claims(&doc); err != nil {
		return nil, fmt.Errorf("discovery document: %w", err)
	}
	hosts := []string{}
	for _, endpoint := range []string{doc.TokenEndpoint, doc.UserinfoEndpoint, doc.JWKSURI} {
		u, err := neturl.Parse(endpoint)
		switch {
		case endpoint == "": // the userinfo_endpoint alone: fetchDiscovery refused the others
			continue
		case err != nil || u.Scheme != "https" || u.Host == "":
			return nil, errors.New("discovery document's userinfo_endpoint is not an https URL")
		}
		if n, err := destination.Entry(u.Hostname()); err == nil && !strings.HasPrefix(n, "*.") {
			hosts = append(hosts, n)
		}
	}
	slices.Sort(hosts)
	return slices.Compact(hosts), nil
}

func isDestination(host string) bool {
	n, err := destination.Entry(host)
	return err == nil && !strings.HasPrefix(n, "*.")
}

// cappedBody is next, http.DefaultTransport when nil, with each response's body ending in an error past
// discoveryCap.
type cappedBody struct{ next http.RoundTripper }

func (c cappedBody) RoundTrip(r *http.Request) (*http.Response, error) {
	next := c.next
	if next == nil {
		next = http.DefaultTransport
	}
	res, err := next.RoundTrip(r)
	if err == nil {
		res.Body = http.MaxBytesReader(nil, res.Body, discoveryCap)
	}
	return res, err
}

// doorsInUse names each front door in use (contracts/routes.md, the front-door row) as the CTE doors(door): the
// stored one, each it replaced in the last 30 minutes, and the one each running deploy under via canyon was
// given, as its run_config recorded it. A deploy recorded without one names none.
const doorsInUse = `WITH doors AS (
	SELECT door FROM front_doors
	UNION SELECT door FROM replaced_front_doors WHERE replaced_at > now() - interval '30 minutes'
	UNION SELECT d.run_config->'front_door'->'door' FROM deploys d JOIN egress_envs e USING (project_id, env)
		WHERE d.run_config->'front_door'->'door' IS NOT NULL)`

// discoveryDue is a front_door_discovery row the minute loop reads again: one recorded a day ago or more (#2389).
const discoveryDue = "recorded_at <= now() - interval '24 hours'"

// discoveryRow is a choice 2 issuer in use the minute loop reads the discovery of, and the hosts recorded for it.
type discoveryRow struct {
	issuer   string
	recorded bool
	hosts    []string
}

// recordDiscovery records the discovery of each choice 2 issuer in use with none recorded — a door saved while its
// provider could not be read, before saves recorded it, or by a server of the release before — every interval for
// as long as ctx lasts. An issuer's first failure is logged, then at most one an hour; a save that recorded it
// between this fetch and its write stands, and is logged. Until it is recorded the issuer's endpoints are no
// platform destination (fail closed). An issuer recorded a day ago or more is read again, so a provider that moves
// an endpoint to a new host is noticed (#2389): its hosts are added to those recorded, each new one logged, and a
// read that fails keeps every one, is tried again every interval, and marks the row once, which the front door's
// read answers as discovery_note until a read succeeds. Each pass first forgets a replaced door past its 30
// minutes in use, so the policy's version moves as its hosts drop.
func (a *app) recordDiscovery(ctx context.Context, logger *log.Logger, every time.Duration) {
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	logged := map[string]time.Time{} // each failure's last line, by issuer, or by the read that failed
	hourly := func(key, format string, args ...any) {
		if at := now(); at.Sub(logged[key]) >= time.Hour {
			logged[key] = at
			logger.Printf(format, args...)
		}
	}
	for {
		if _, err := a.pool.Exec(ctx, "DELETE FROM replaced_front_doors WHERE replaced_at <= now() - interval '30 minutes'"); err != nil && ctx.Err() == nil {
			hourly("replaced", "canyon-server: the replaced front doors, tried again every minute (logged at most hourly): %s", oneLine(err.Error()))
		}
		rows, err := a.pool.Query(ctx, doorsInUse+` SELECT DISTINCT d.door->'signin'->>'issuer', f.issuer IS NOT NULL, f.hosts FROM doors d
			LEFT JOIN front_door_discovery f ON f.issuer = d.door->'signin'->>'issuer'
			WHERE d.door->>'choice' = '2' AND (f.issuer IS NULL OR f.`+discoveryDue+`)`)
		var issuers []discoveryRow
		if err == nil {
			issuers, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (r discoveryRow, err error) {
				return r, row.Scan(&r.issuer, &r.recorded, &r.hosts)
			})
		}
		if err != nil && ctx.Err() == nil {
			hourly("doors", "canyon-server: the front doors in use, read again every minute (logged at most hourly): %s", oneLine(err.Error()))
		}
		for _, r := range issuers {
			hosts, err := a.discoveredHosts(ctx, r.issuer)
			var tag pgconn.CommandTag
			switch {
			case errors.Is(err, errGoesDirect):
				continue
			case err != nil && r.recorded: // every host recorded stands; the row is marked once, at the first failure, and the mark moves no policy version (0109)
				_, werr := a.pool.Exec(ctx, "UPDATE front_door_discovery SET refresh_failed_at = now() WHERE issuer = $1 AND refresh_failed_at IS NULL AND "+discoveryDue, r.issuer)
				err = errors.Join(err, werr)
			case err != nil: // none recorded: discovery_note says so already
			case r.recorded: // added to the hosts recorded, never in place of one, as a save adds them; a save meanwhile stands
				if tag, err = a.pool.Exec(ctx, `UPDATE front_door_discovery SET hosts = ARRAY(SELECT DISTINCT h FROM unnest(hosts || $2::text[]) h ORDER BY h),
					recorded_at = now(), refresh_failed_at = NULL WHERE issuer = $1 AND `+discoveryDue, r.issuer, hosts); err == nil && tag.RowsAffected() != 0 {
					if added := slices.DeleteFunc(hosts, func(h string) bool { return slices.Contains(r.hosts, h) }); len(added) != 0 {
						logger.Printf("canyon-server: the discovery of %.200q names %s anew; recorded as platform destinations", r.issuer, strings.Join(added, ", "))
					}
				}
			default:
				if tag, err = a.pool.Exec(ctx, "INSERT INTO front_door_discovery (issuer, hosts) VALUES ($1, $2) ON CONFLICT (issuer) DO NOTHING", r.issuer, hosts); err == nil && tag.RowsAffected() == 0 {
					logger.Printf("canyon-server: the discovery of %.200q was recorded by a save meanwhile; its hosts stand", r.issuer)
				}
			}
			if err != nil {
				hourly(r.issuer, "canyon-server: the discovery of %.200q, tried again every minute (logged at most hourly): %s", r.issuer, oneLine(err.Error()))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// runByOrg is the preferences row's run_by (design/HAND-OFF.md §1): whether releases, and so this
// door, go to the org's own platform.
func runByOrg(ctx context.Context, q querier) (bool, error) {
	var runBy string
	err := q.QueryRow(ctx, "SELECT run_by FROM org_preferences").Scan(&runBy)
	return runBy == "org", err
}
