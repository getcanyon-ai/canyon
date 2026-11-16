package runs

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/executor/kube"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/webhook"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Retry and wait timing (design/HAND-OFF.md §3): package vars, so a test overrides them instead
// of waiting real minutes, exactly as pendingEvery is above.
var (
	releaseFastRetry  = time.Minute // once a minute for the first releaseFastWindow
	releaseFastWindow = 5 * time.Minute
	releaseSlowRetry  = 15 * time.Minute // every 15 minutes after, releaseCeiling in all
	releaseCeiling    = time.Hour
	releaseTimeout    = 10 * time.Second // a delivery's own HTTP timeout
	reportCheckEvery  = 10 * time.Second // the release row is read this often while the run waits on its report
	stillWaitingEvery = 5 * time.Minute  // "still waiting on your platform" every this
	reportCeiling     = 30 * time.Minute
)

// newReleaseID is 64 random bits, printed like a gate run's id (runs/start.go, newID).
func newReleaseID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "rel_" + hex.EncodeToString(b[:]), nil
}

// releaseRow is the release send's own checkpoint: its id and the delivery every attempt reuses, and when it was
// written, a repo release's commit date (design/HAND-OFF.md §3b: the same commit every time it is made).
type releaseRow struct {
	ID       string
	Delivery string
	Created  time.Time
	Stopped  bool `json:",omitempty"` // an undeploy asked the run to stop before it was sent: no release (contracts/routes.md, The brake)
	// Platform is the image's, read by its digest before the row is written (#2528); nil only in a row this step
	// journaled before it read one, a run in flight across the upgrade, whose release names none.
	Platform *executor.Platform `json:",omitempty"`
	// Gone is the image the registry no longer holds, read as the platform was (GET /platform's image_gone): a
	// result, not an error, so a replay reads it back; no row is written.
	Gone bool `json:",omitempty"`
}

// errImageGone is a release whose image the registry no longer holds: never sent, as /run's 404 runs nothing.
var errImageGone = errors.New("its image is no longer in the registry")

// createRelease inserts the release row, state sent, once: replayed by DBOS's own journal, never
// by a second INSERT. A promotion's row carries preview's gate run and deployer (0045), which the
// org's deployed report writes on the prod row.
func (p *Pipeline) createRelease(ctx context.Context, in Input, env, image, via string) (releaseRow, error) {
	// The platform the image runs on, read by its digest as a /run reads it (contracts/executor.md, GET /platform),
	// before anything is written: an image whose platform is not read is never handed off (#2528).
	platform, err := p.Executor.Platform(ctx, image)
	if errors.Is(err, executor.ErrNoImage) {
		return releaseRow{Gone: true}, nil
	}
	if err != nil {
		return releaseRow{}, fmt.Errorf("runs: the platform of %s: %w", image, err)
	}
	id, err := newReleaseID()
	if err != nil {
		return releaseRow{}, err
	}
	delivery := uuid.NewString()
	var gateRun, byKind, bySubject, byDisplay *string
	if pr := in.Promote; pr != nil {
		gateRun = &pr.GateRun
		if pr.DeployedBy.Kind != "" { // a preview row written before its deployer was recorded names none
			byKind, bySubject, byDisplay = &pr.DeployedBy.Kind, &pr.DeployedBy.Subject, &pr.DeployedBy.Display
		}
	}
	var stopped bool
	var created time.Time
	err = p.write(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT stop_requested_at IS NOT NULL FROM runs WHERE id = $1", in.Run).Scan(&stopped); err != nil || stopped {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display, created_at, via)
			VALUES ($1, $2, $3, $4, $5, 'deploy', $6, $7, 'sent', now(), $8, $9, $10, $11, clock_timestamp(), $12) RETURNING created_at`, // created_at as newRemove writes it
			id, in.Project, env, in.Run, in.Package, image, delivery, gateRun, byKind, bySubject, byDisplay, via).Scan(&created)
	})
	if stopped {
		return releaseRow{Stopped: true}, err
	}
	return releaseRow{ID: id, Delivery: delivery, Created: created.UTC().Truncate(time.Second), Platform: &platform}, err
}

// markRelease moves the release from sent to one of Canyon's own states — accepted, refused,
// undelivered — and never off a report: a platform that reports from inside its handler, before its
// 2xx reaches Canyon, keeps the state it reported.
func (p *Pipeline) markRelease(ctx context.Context, id, state string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE releases SET state = $1 WHERE id = $2 AND state = 'sent'", state, id)
		return err
	})
}

// deployRelease is release.schema.json's "deploy" shape, sent as the webhook's signed body.
type deployRelease struct {
	Event       string `json:"event"`
	Delivery    string `json:"delivery"`
	Release     string `json:"release"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Package     struct {
		ID    string  `json:"id"`
		Label *string `json:"label,omitempty"`
	} `json:"package"`
	GateRun   string             `json:"gate_run"`
	GateURL   string             `json:"gate_url"`
	Image     string             `json:"image"`
	Platform  *executor.Platform `json:"platform,omitempty"` // the image's (#2528): nil only from a release row journaled before
	Port      int                `json:"port"`
	Variables []string           `json:"variables"`
	Resources []releaseResource  `json:"resources"`
	AppEnv    map[string]string  `json:"app_env"`
	Sidecar   struct {
		Image  string            `json:"image"`
		Port   int               `json:"port"`
		Env    map[string]string `json:"env"`
		Secret string            `json:"secret"`
	} `json:"sidecar"`
	ReportURL string `json:"report_url"`
	SentAt    string `json:"sent_at"`
}

// releaseResource is one canyon.toml [resources] entry as the release names it: its promised
// runtime values, never a value.
type releaseResource struct {
	Name  string   `json:"name"`
	Type  string   `json:"type"`
	Gives []string `json:"gives"`
}

// releaseBody composes the release's body but sent_at, re-stamped on every attempt. app_env and
// the sidecar's shared keys come from executor/kube's AppEnv and SidecarEnv, the one producer
// /run builds a pod's environment from too (design/HAND-OFF.md §3).
func (p *Pipeline) releaseBody(ctx context.Context, in Input, rel releaseRow, env, image string) (deployRelease, error) {
	var label *string
	var declared map[string]string
	if err := p.Pool.QueryRow(ctx, "SELECT label, coalesce(resources, '{}') FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&label, &declared); err != nil {
		return deployRelease{}, err
	}
	names, err := p.releaseVariables(ctx, in.Project, env)
	if err != nil {
		return deployRelease{}, err
	}
	for _, typ := range declared { // the platform provides what a resource gives, with the resource: never a variable too (#2432)
		names = slices.DeleteFunc(names, func(n string) bool { return slices.Contains(provisioners.Promised(typ), n) })
	}
	// false: unlike RunApp (which needs the client secret live in the pod), a release travels over
	// the wire to the org and is journaled as this step's return value — the secret must never be
	// in either (security review of #492).
	door, _, err := p.frontDoor(ctx, false)
	if err != nil {
		return deployRelease{}, err
	}
	doorJSON, err := json.Marshal(door)
	if err != nil {
		return deployRelease{}, err
	}
	// The row's access_grace as this body is built (design/HAND-OFF.md §1: "it applies to
	// releases sent after the change"): the body is this step's journaled output, so a release sent
	// before a change keeps the value it was sent with, retries included.
	var accessGrace string
	if err := p.Pool.QueryRow(ctx, "SELECT access_grace FROM org_preferences").Scan(&accessGrace); err != nil {
		return deployRelease{}, err
	}
	identity := executor.Identity{Issuer: p.PublicURL, Audience: in.Project, JWKSURL: sidecarJWKS}
	sidecarEnv := kube.SidecarEnv(doorJSON, identity, env)
	sidecarEnv["CANYON_ACCESS_URL"] = p.PublicURL + "/access/" + in.Project + "/" + env
	sidecarEnv["CANYON_ACCESS_GRACE"] = accessGrace

	var body deployRelease
	body.Event, body.Delivery, body.Release = "deploy", rel.Delivery, rel.ID
	body.App, body.Environment = in.Project, env
	body.Package.ID, body.Package.Label = in.Package, label
	gateRun := in.Run // a promotion's release names the gate run preview was deployed from: the review it ships
	if in.Promote != nil {
		gateRun = in.Promote.GateRun
	}
	body.GateRun, body.GateURL = gateRun, p.PublicURL+"/#/apps/"+in.Project+"/runs/"+gateRun
	platform := rel.Platform
	if platform == nil { // a row the release before journaled, in flight across the upgrade: read it now, as the row's step does
		read, err := p.Executor.Platform(ctx, image)
		if err != nil {
			return deployRelease{}, fmt.Errorf("runs: the platform of %s: %w", image, err)
		}
		platform = &read
	}
	body.Image, body.Platform, body.Port = image, platform, 3000
	body.Variables = names
	if body.Variables == nil {
		body.Variables = []string{}
	}
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		typ := declared[name]
		body.Resources = append(body.Resources, releaseResource{Name: name, Type: typ, Gives: provisioners.Promised(typ)})
	}
	if body.Resources == nil {
		body.Resources = []releaseResource{}
	}
	body.AppEnv = kube.AppEnv(identity)
	body.Sidecar.Image, body.Sidecar.Port, body.Sidecar.Env, body.Sidecar.Secret = p.IdentityImage, 4180, sidecarEnv, "CANYON_ACCESS_TOKEN"
	body.ReportURL = p.PublicURL + "/releases/" + rel.ID + "/report"
	return body, nil
}

// releaseVariables is env's runtime secret names alone, never a value (design/HAND-OFF.md §3:
// "values are the org's").
func (p *Pipeline) releaseVariables(ctx context.Context, project, env string) ([]string, error) {
	rows, err := p.Pool.Query(ctx, "SELECT name FROM secrets WHERE project_id = $1 AND env = $2 AND scope = 'runtime' ORDER BY name", project, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// webhookTarget is the platform webhook's URL and its signing secret, opened from the vault at the
// call: never a step's input or output, never a timeline row (routes_platform_webhook.go seals it
// the same way the front door's client secret is).
func (p *Pipeline) webhookTarget(ctx context.Context) (url string, secret []byte, err error) {
	var keyRef int
	var sealedKey, sealedValue []byte
	if err := p.Pool.QueryRow(ctx, "SELECT url, key_ref, sealed_key, sealed_value FROM platform_webhook").Scan(&url, &keyRef, &sealedKey, &sealedValue); err != nil {
		return "", nil, err
	}
	secret, err = p.Vault.Open(keyRef, "platform-webhook", "webhook", "secret", sealedKey, sealedValue)
	return url, secret, err
}

// attempt is one delivery try's checkpointed result: the status the org answered (0: no
// connection) and the instant it was sent, re-stamped into the signed body.
type attempt struct {
	Status     int    `json:"status"`
	SentAt     string `json:"sent_at"`
	Superseded bool   `json:"superseded,omitempty"` // a deploy release an undeploy's remove superseded: nothing was posted
	Uncleared  bool   `json:"uncleared,omitempty"`  // a deploy release its review no longer clears (Pipeline.clears): nothing was posted
}

// sending is one release as every attempt sends it: its event, its delivery id, and its body stamped
// with an attempt's sent_at — release.schema.json's deploy or remove.
type sending struct {
	Event, Delivery, Release string
	body                     func(ctx context.Context, sentAt string) (any, error) // read by each attempt
	cleared                  func(context.Context) bool                            // a deploy's last check before each attempt posts it (Pipeline.clears); nil for a remove
}

// deliverOnce re-stamps the release's sent_at, signs it under the stored secret and posts it, once. A
// transport error (a timeout or no connection) is Status 0, never a Go error: the caller's loop
// decides to retry, exactly as a 5xx does.
func (p *Pipeline) deliverOnce(ctx context.Context, s sending) (attempt, error) {
	if s.Event == "deploy" { // an undeploy's remove superseded it: no attempt may put the app back (contracts/routes.md, The brake)
		if gone, err := p.removedSince(ctx, s.Release); err != nil || gone {
			return attempt{Superseded: gone}, err
		}
		if s.cleared != nil && !s.cleared(ctx) { // read live by each attempt, as the deploy step reads it before /run
			return attempt{Uncleared: true}, nil
		}
	}
	at := time.Now().UTC()
	sentAt := at.Format(time.RFC3339)
	shaped, err := s.body(ctx, sentAt)
	if err != nil {
		return attempt{}, err
	}
	body, err := json.Marshal(shaped)
	if err != nil {
		return attempt{}, err
	}
	url, secret, err := p.webhookTarget(ctx)
	if err != nil {
		return attempt{}, err
	}
	status, err := post(ctx, url, secret, body, s.Event, s.Delivery)
	if err != nil {
		return attempt{}, err
	}
	// The webhook's last delivery (GET /api/v1/platform-webhook, #2439): shown, never a gate, so a failed write is
	// logged and the attempt stands — the platform has the release, and failing the step would fail the run for nothing.
	if _, err := p.Pool.Exec(ctx, "UPDATE releases SET attempted_at = $1, attempt_status = $2 WHERE id = $3", at, status, s.Release); err != nil {
		log.Printf("runs: release %s: record its attempt, the webhook's last delivery: %v", s.Release, err)
	}
	// status 0 (no response at all — refused as not public, timed out, or no connection) and any
	// 3xx (a redirect, never followed) both fall to the retry path below: neither is a 2xx or a 4xx.
	return attempt{Status: status, SentAt: sentAt}, nil
}

// post signs body under secret and posts it to url as event's delivery, as every delivery is posted (design/HAND-OFF.md
// §3): bounded by releaseTimeout, through releaseDeliveryTransport, no redirect followed. It answers the HTTP status, 0
// for no connection or no answer in time, and never why: a dial or certificate error names the network behind url.
func post(ctx context.Context, url string, secret, body []byte, event, delivery string) (int, error) {
	call, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(call, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Canyon-Event", event)
	req.Header.Set("Canyon-Delivery", delivery)
	req.Header.Set("Canyon-Signature-256", webhook.Sign(secret, body))
	transport := releaseDeliveryTransport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}
	resp, _ := client.Do(req) //nolint:bodyclose // closed below, when there is one: a blocked redirect and a dial refusal both answer no response
	var status int
	if resp != nil { // Go's Client, even when CheckRedirect refuses, answers the redirect's own response and its error together: the status is real
		status = resp.StatusCode
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if status < 100 || status > 599 { // not an HTTP status: no answer, as webhook_ping's range and attempt_status's CHECK have it
		status = 0
	}
	return status, nil
}

// ErrNoWebhook is Ping's answer before a platform webhook is saved.
var ErrNoWebhook = errors.New("runs: no platform webhook is saved")

// Ping is a test delivery's answer (release.schema.json's webhook_ping): its delivery and sent_at, and the HTTP status
// the url answered, 0 for none.
type Ping struct {
	Delivery string `json:"delivery"`
	SentAt   string `json:"sent_at"`
	Status   int    `json:"status"`
}

// Ping sends a test delivery (contracts/routes.md, POST /api/v1/platform-webhook/ping; release.schema.json's ping): one
// signed post of {event, delivery, sent_at} to the saved url, Canyon-Event: ping, never retried and never recorded.
// The target is a plain read: nothing here holds a transaction or a lock while the post waits on the org's platform.
func (p *Pipeline) Ping(ctx context.Context) (Ping, error) {
	url, secret, err := p.webhookTarget(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ping{}, ErrNoWebhook
	}
	if err != nil {
		return Ping{}, err
	}
	sent := Ping{Delivery: uuid.NewString(), SentAt: time.Now().UTC().Format(time.RFC3339)}
	body, err := json.Marshal(struct {
		Event    string `json:"event"`
		Delivery string `json:"delivery"`
		SentAt   string `json:"sent_at"`
	}{"ping", sent.Delivery, sent.SentAt})
	if err != nil {
		return Ping{}, err
	}
	sent.Status, err = post(ctx, url, secret, body, "ping", sent.Delivery)
	return sent, err
}

// errRedirect refuses every redirect a delivery's target answers: the org's platform-webhook
// endpoint, never wherever it points next (security review of #492).
var errRedirect = errors.New("a redirect")

// releaseDeliveryTransport is a fresh transport per attempt: it dials the address the admin named,
// whatever it is (design/HAND-OFF.md §3, "No address filtering" — where the server may reach is the
// operator's NetworkPolicy's to say, deploy/k8s/README.md), and TLS verification is never weakened for it.
func releaseDeliveryTransport() *http.Transport {
	return &http.Transport{Proxy: nil, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
}

// appendBuildLog is its own step (security review of #492): events.AppendRun has no dedupe key, so
// a bare call inside the workflow would re-append every past "retrying in" and "still waiting" row
// on a crash-replay. Wrapped, DBOS's own journal answers a replayed call from its cache instead.
func (p *Pipeline) appendBuildLog(ctx dbos.Context, in Input, line string) error {
	_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		_, err := events.AppendRun(c, p.Pool, in.Run, in.Project, events.BuildLog, map[string]any{"line": line})
		return struct{}{}, err
	}, dbos.WithStepName("log"))
	return err
}

// shortDuration is "1m" or "15m", the contract's spelling (design/HAND-OFF.md §3), never Go's "1m0s".
func shortDuration(d time.Duration) string { return strconv.Itoa(int(d.Minutes())) + "m" }

// sendRelease's own failures: the caller (run()) ends the workflow failed/error on each, as a plain
// deploy's failure does.
var (
	errReleaseRefused     = errors.New("the org's platform refused the release")
	errReleaseUndelivered = errors.New("the org's platform did not accept the release in an hour")
	errReleaseFailed      = errors.New("the org's platform reported the release failed")
	errReleaseRemoved     = errors.New("the org's platform removed the release before it went live")
	errReleaseOpen        = errors.New("the release is still open: no report within 30 minutes")
)

// sendRelease sends one deploy release for env under run_by: org (design/HAND-OFF.md §3) in place
// of calling /run, retries it per the contract, and waits up to reportCeiling for the org's report
// (awaitReport): nil once the platform reports the release deployed, else one of sendRelease's own
// errors — refused, undelivered, a failed or removed report, or the wait's bound. The caller fails the run
// failed/error on each, exactly as a plain deploy's failure does.
func (p *Pipeline) sendRelease(ctx dbos.Context, in Input, env, image, via string) error {
	via = cmp.Or(via, "webhook") // a run that read run_by alone
	// unsent ends a release never sent (contracts/events.md, under run_by: org) on the line a /run's failure ends on:
	// production's Canyon's own cluster's, the version running there unchanged, preview's with no tail (#2539); a
	// rollback's kept image gone is keptGone's to say.
	unsent := func(err error) error {
		log.Printf("runs: run %s (%s): no release sent: %v", in.Run, in.Project, err)
		if errors.Is(err, errImageGone) && in.Promote != nil && in.Promote.Rollback {
			return err
		}
		l := "deploying to preview: failed: " + executorSaid(err)
		if env == "prod" {
			l = "deploying to production: failed: " + executorSaid(err) + "; the version running there is unchanged"
		}
		if lerr := p.appendBuildLog(ctx, in, l); lerr != nil {
			return lerr
		}
		return err
	}
	rel, err := dbos.RunAsStep(ctx, func(c context.Context) (releaseRow, error) { return p.createRelease(c, in, env, image, via) }, dbos.WithStepName("release"))
	if err != nil {
		return unsent(err)
	}
	if rel.Stopped {
		return errBraked
	}
	if rel.Gone {
		return unsent(fmt.Errorf("%w: %s", errImageGone, image))
	}
	base, err := dbos.RunAsStep(ctx, func(c context.Context) (deployRelease, error) { return p.releaseBody(c, in, rel, env, image) }, dbos.WithStepName("release body"))
	if err != nil {
		return unsent(err)
	}
	if via == "repo" { // committed to the deploy repo in place of a webhook (design/HAND-OFF.md §3b)
		return p.sendByCommit(ctx, in, rel, base)
	}
	stamp := func(_ context.Context, sentAt string) (any, error) { b := base; b.SentAt = sentAt; return b, nil }
	say := func(line string) error { return p.appendBuildLog(ctx, in, line) }
	// Said before the first delivery: a platform may report inside its webhook handler, and its
	// deploy.completed row must not land ahead of this one.
	if err := say("handed to your platform · waiting for it to report"); err != nil {
		return err
	}
	cleared := func(c context.Context) bool { return p.clears(c, in) }
	if err := p.deliver(ctx, rel.ID, sending{Event: "deploy", Delivery: rel.Delivery, Release: rel.ID, body: stamp, cleared: cleared}, say); err != nil {
		return err
	}
	return p.awaitReport(ctx, in, rel.ID)
}

// deliver sends release id per the contract's retries (design/HAND-OFF.md §3) and marks it accepted,
// refused or undelivered, each mark and each attempt its own step. say writes the run's lines — each
// retry's, the refusal's, the hour's — and says nothing for a remove, which has no run. nil once the
// platform accepts it, else errReleaseRefused or errReleaseUndelivered.
func (p *Pipeline) deliver(ctx dbos.Context, id string, s sending, say func(string) error) error {
	mark := func(state string) error {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.markRelease(c, id, state) }, dbos.WithStepName(state))
		return err
	}
	var first time.Time
	for {
		att, err := dbos.RunAsStep(ctx, func(c context.Context) (attempt, error) { return p.deliverOnce(c, s) }, dbos.WithStepName("deliver"))
		if err != nil || att.Superseded {
			return cmp.Or(err, errBraked)
		}
		if att.Uncleared {
			if err := mark("undelivered"); err != nil {
				return err
			}
			return errNotCleared
		}
		sentAt, perr := time.Parse(time.RFC3339, att.SentAt)
		if perr != nil {
			return perr
		}
		if first.IsZero() {
			first = sentAt
		}
		switch {
		case att.Status/100 == 2:
			return mark("accepted")
		case att.Status >= 400 && att.Status < 500:
			if err := mark("refused"); err != nil {
				return err
			}
			if err := say(fmt.Sprintf("Your platform refused the release: HTTP %d", att.Status)); err != nil {
				return err
			}
			return errReleaseRefused
		default:
			elapsed := sentAt.Sub(first)
			interval := releaseFastRetry
			if elapsed >= releaseFastWindow {
				interval = releaseSlowRetry
			}
			// "for one hour in all" (design/HAND-OFF.md §3): refuse before scheduling a sleep whose
			// attempt would land past the ceiling, rather than sleep and find out — the last real
			// attempt is always within the hour, never one past it.
			if elapsed+interval > releaseCeiling {
				if err := mark("undelivered"); err != nil {
					return err
				}
				if err := say("Your platform did not accept the release in an hour"); err != nil {
					return err
				}
				return errReleaseUndelivered
			}
			status := "no connection"
			if att.Status > 0 {
				status = strconv.Itoa(att.Status)
			}
			if err := say(fmt.Sprintf("your platform answered %s · retrying in %s", status, shortDuration(interval))); err != nil {
				return err
			}
			if _, err := dbos.Sleep(ctx, interval); err != nil {
				return err
			}
		}
	}
}

// removedSince is whether a remove release of id's environment was created after id: an undeploy took
// the environment offline, superseding id.
func (p *Pipeline) removedSince(ctx context.Context, id string) (bool, error) {
	var gone bool
	err := p.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM releases r JOIN releases d ON d.project_id = r.project_id AND d.env = r.env
		WHERE d.id = $1 AND r.event = 'remove' AND r.created_at > d.created_at)`, id).Scan(&gone)
	return gone, err
}

// reported is one read of the release while the run waits on it: its state, and what the report
// route committed with that state — a deployed report's address, a failed report's reason.
type reported struct {
	State   string `json:"state"`
	URL     string `json:"url,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Stopped bool   `json:"stopped,omitempty"` // an undeploy's remove of the environment, newer than this release, superseded it
}

// readReport reads the release row and the run's deploy.completed row, which the report route writes
// in the same transaction as a deployed state (cmd/canyon-server, recordDeployed): its url is the
// address the platform reported.
func (p *Pipeline) readReport(ctx context.Context, id string) (reported, error) {
	var r reported
	err := p.Pool.QueryRow(ctx, `SELECT r.state, coalesce(r.reason, ''), coalesce(t.data->>'url', ''), EXISTS (SELECT 1 FROM releases x WHERE x.project_id = r.project_id AND x.env = r.env AND x.event = 'remove' AND x.created_at > r.created_at)
		FROM releases r LEFT JOIN timeline_rows t ON t.run_id = r.run_id AND t.dedupe = 'deploy:' || r.env
		WHERE r.id = $1`, id).Scan(&r.State, &r.Reason, &r.URL, &r.Stopped)
	return r, err
}

// awaitReport waits on the org's report on release id (design/HAND-OFF.md §3), reading the release
// every reportCheckEvery, each read its own step so a replay answers it from the journal. deployed
// writes events.md's "live at <url> · reported by your platform" and ends the wait, the run then
// ending as a deploy does; failed writes "Your platform reported: <reason>" and removed "Your platform
// removed the release before it went live", each failing the run; a stop an undeploy recorded on the run
// — a remove of the environment newer than the release — ends the wait at once, the run ending through
// the brake. Any other state keeps the wait: every stillWaitingEvery a "still waiting" row, and at reportCeiling the
// run fails, the release still open — a report after that lands late and the run's state stays.
func (p *Pipeline) awaitReport(ctx dbos.Context, in Input, id string) error {
	note := stillWaitingEvery
	for waited := time.Duration(0); ; {
		r, err := dbos.RunAsStep(ctx, func(c context.Context) (reported, error) { return p.readReport(c, id) }, dbos.WithStepName("report"))
		if err != nil {
			return err
		}
		switch r.State {
		case "deployed":
			return p.appendBuildLog(ctx, in, "live at "+r.URL+" · reported by your platform")
		case "failed":
			if err := p.appendBuildLog(ctx, in, "Your platform reported: "+r.Reason); err != nil {
				return err
			}
			return errReleaseFailed
		case "removed": // it never goes live: a deployed after it is 409 superseded (contracts/routes.md, the report row)
			if err := p.appendBuildLog(ctx, in, "Your platform removed the release before it went live"); err != nil {
				return err
			}
			return errReleaseRemoved
		}
		if r.Stopped { // the run ends at once, through the brake: never waiting out its 30 minutes
			return errBraked
		}
		if waited >= reportCeiling {
			break
		}
		if _, err := dbos.Sleep(ctx, reportCheckEvery); err != nil {
			return err
		}
		waited += reportCheckEvery
		if waited >= note && waited < reportCeiling {
			if err := p.appendBuildLog(ctx, in, fmt.Sprintf("still waiting on your platform (%s)", shortDuration(note))); err != nil {
				return err
			}
			note += stillWaitingEvery
		}
	}
	var label *string
	if err := p.Pool.QueryRow(ctx, "SELECT label FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&label); err != nil {
		return err
	}
	version := strings.TrimPrefix(in.Package, "pkg_") // routes.md, Naming a version
	if label != nil {
		version = *label
	}
	// "30 minutes" is the contract's fixed wording (design/HAND-OFF.md §3), not reportCeiling's
	// value: a test shrinks the wait, never the sentence a person reads.
	msg := fmt.Sprintf("Your platform has not reported on %s in 30 minutes. The release stays open: when it reports, the environment goes live.", version)
	if err := p.appendBuildLog(ctx, in, msg); err != nil {
		return err
	}
	return errReleaseOpen
}

// RemoveWorkflow is the durable send of a remove release (design/HAND-OFF.md §3, §4): no run and no
// timer — it ends once the org's platform accepts, refuses, or has not accepted it in an hour, and
// the environment waits on the report with no bound, as `pending` on the project read.
const RemoveWorkflow = "release-remove"

// RemoveInput is a remove release's send: the row SendRemoveTx wrote.
type RemoveInput struct {
	Release, Delivery, Project, Env string
}

// removeRelease is release.schema.json's "remove" shape: what to stop, where to report, and nothing else.
type removeRelease struct {
	Delete      bool   `json:"delete,omitempty"` // a project delete's alone (#2432): never written false
	Event       string `json:"event"`
	Delivery    string `json:"delivery"`
	Release     string `json:"release"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	ReportURL   string `json:"report_url"`
	SentAt      string `json:"sent_at"`
}

// remove delivers in's release as the send loop delivers a deploy's, saying nothing: there is no run to
// tell. It answers the state the release was left in.
func (p *Pipeline) remove(ctx dbos.Context, in RemoveInput) (string, error) {
	via, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) {
		var via string
		err := p.Pool.QueryRow(c, "SELECT via FROM releases WHERE id = $1", in.Release).Scan(&via)
		return via, err
	}, dbos.WithStepName("via"))
	if err != nil {
		return "", err
	}
	if via == "repo" { // the environment's last deploy went through the deploy repo: so does its remove (design/HAND-OFF.md §3b)
		return p.removeByCommit(ctx, in)
	}
	body := func(ctx context.Context, sentAt string) (any, error) {
		var deletes bool // the row's, on every attempt: only a project delete's remove was written true
		if err := p.Pool.QueryRow(ctx, "SELECT deletes_resources FROM releases WHERE id = $1", in.Release).Scan(&deletes); err != nil {
			return nil, err
		}
		return removeRelease{Delete: deletes, Event: "remove", Delivery: in.Delivery, Release: in.Release, App: in.Project, Environment: in.Env,
			ReportURL: p.PublicURL + "/releases/" + in.Release + "/report", SentAt: sentAt}, nil
	}
	err = p.deliver(ctx, in.Release, sending{Event: "remove", Delivery: in.Delivery, Release: in.Release, body: body}, func(string) error { return nil })
	switch {
	case errors.Is(err, errReleaseRefused):
		return "refused", nil
	case errors.Is(err, errReleaseUndelivered):
		return "undelivered", nil
	case err != nil:
		return "", err
	}
	return "accepted", nil
}

// Open is the environment's newest release while it is open — sent or accepted — and newer than what
// is recorded there: a remove an undeploy sent, or a deploy release the org has not reported on yet.
type Open struct {
	ID, Event, Package string
}

// OpenRelease reads env's open release; a zero Open when there is none.
func OpenRelease(ctx context.Context, q querier, project, env string) (Open, error) {
	var o Open
	var open bool
	err := q.QueryRow(ctx, `SELECT id, event, package_id, state IN ('sent', 'accepted') FROM releases WHERE project_id = $1 AND env = $2
		AND created_at > coalesce((SELECT deployed_at FROM deploys WHERE project_id = $1 AND env = $2), '-infinity')
		ORDER BY created_at DESC, id DESC LIMIT 1`, project, env).Scan(&o.ID, &o.Event, &o.Package, &open)
	if errors.Is(err, pgx.ErrNoRows) {
		return Open{}, nil
	}
	if err != nil || !open {
		return Open{}, err
	}
	return o, nil
}

// SendRemoveTx answers the environment's open remove release, or records a new one and enqueues its
// send, inside the caller's transaction, so the row and its send land with the caller's audit row or not
// at all. The row names what it removes: the row the org runs there — its package, image and gate run —
// else the open deploy release's: the org's first deploy there, or one taking over from what Canyon ran
// (contracts/routes.md, The brake). sent is false when an open one was answered. ErrNoRows when the org
// runs nothing there and no deploy release is open.
func SendRemoveTx(ctx context.Context, tx pgx.Tx, dc dbos.Client, project, env string) (release string, sent bool, err error) {
	open, err := OpenRelease(ctx, tx, project, env)
	if err != nil || open.Event == "remove" {
		return open.ID, false, err
	}
	id, err := newRemove(ctx, tx, dc, project, env, false, "SELECT $1::text, project_id, env, gate_run, package_id, 'remove', image, $2::uuid, 'sent', now() FROM deploys WHERE project_id = $3 AND env = $4 AND run_by = 'org'", project, env)
	if errors.Is(err, pgx.ErrNoRows) && open.Event == "deploy" {
		return SendRemoveOfTx(ctx, tx, dc, open.ID)
	}
	return id, err == nil, err
}

// SendRemoveOfTx records and enqueues a remove release naming release's own package and image, inside
// the caller's transaction: what an org's platform started from release is taken off again.
func SendRemoveOfTx(ctx context.Context, tx pgx.Tx, dc dbos.Client, release string) (string, bool, error) {
	var project, env string
	if err := tx.QueryRow(ctx, "SELECT project_id, env FROM releases WHERE id = $1", release).Scan(&project, &env); err != nil {
		return "", false, err
	}
	id, err := newRemove(ctx, tx, dc, project, env, false, "SELECT $1::text, project_id, env, run_id, package_id, 'remove', image, $2::uuid, 'sent', now() FROM releases WHERE id = $3", release)
	return id, err == nil, err
}

// SendDeleteRemoveTx is a project delete's remove for env, which has a release of the org's (#2432): one that
// carries delete, so the org's platform also deletes what it provisioned there. It answers the release the
// delete waits on — a delete's remove still open is answered again, never sent twice — or "" once the
// platform reported one removed. Any other newest release, an open remove without delete among them, gets a
// new one, and so does a delete's remove the platform refused, never took, or reported failed: the same
// delete again resumes. "" too when env has no release at all. again sends a new one over one reported removed
// too: the platform started the app again after the delete was done (the report route's brake).
func SendDeleteRemoveTx(ctx context.Context, tx pgx.Tx, dc dbos.Client, project, env string, again bool) (string, error) {
	var newest, event, state string
	var deletes bool
	err := tx.QueryRow(ctx, "SELECT id, event, state, deletes_resources FROM releases WHERE project_id = $1 AND env = $2 ORDER BY created_at DESC, id DESC LIMIT 1", project, env).
		Scan(&newest, &event, &state, &deletes)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if event == "remove" && deletes {
		switch {
		case state == "removed" && !again:
			return "", nil
		case state == "sent" || state == "accepted":
			return newest, nil
		}
	}
	return newRemove(ctx, tx, dc, project, env, true, "SELECT $1::text, project_id, env, run_id, package_id, 'remove', image, $2::uuid, 'sent', now() FROM releases WHERE id = $3", newest)
}

// newRemove inserts the remove release row picks — its id $1 and its delivery $2, then args — and
// enqueues its send in the caller's transaction. ErrNoRows when row picks none. deletes is true on a
// project delete's alone (SendDeleteRemoveTx): an undeploy's, a hold's and the brake's keep the data.
func newRemove(ctx context.Context, tx pgx.Tx, dc dbos.Client, project, env string, deletes bool, row string, args ...any) (string, error) {
	id, err := newReleaseID()
	if err != nil {
		return "", err
	}
	delivery := uuid.NewString()
	// A remove goes the way the environment's last deploy went, never the way the setting says now
	// (design/HAND-OFF.md §1): what the org runs there came through it.
	var via string
	if err := tx.QueryRow(ctx, "SELECT coalesce((SELECT via FROM releases WHERE project_id = $1 AND env = $2 AND event = 'deploy' ORDER BY created_at DESC, id DESC LIMIT 1), 'webhook')", project, env).Scan(&via); err != nil {
		return "", err
	}
	// created_at is the insert's own instant, under the ledger lock every release insert takes, never the
	// transaction's start: what supersedes what is then the order the releases were written in.
	tag, err := tx.Exec(ctx, "INSERT INTO releases (id, project_id, env, run_id, package_id, event, image, delivery, state, sent_at, created_at, deletes_resources, via) SELECT r.*, clock_timestamp(), $"+strconv.Itoa(len(args)+3)+"::boolean, $"+strconv.Itoa(len(args)+4)+"::text FROM ("+row+") r",
		append(append([]any{id, delivery}, args...), deletes, via)...)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", pgx.ErrNoRows
	}
	if _, err := dbos.Enqueue[any](dc, Queue, RemoveWorkflow, RemoveInput{Release: id, Delivery: delivery, Project: project, Env: env},
		dbos.WithEnqueueWorkflowID(id), dbos.WithEnqueueTransaction(tx)); err != nil {
		return "", fmt.Errorf("runs: enqueue: %w", err)
	}
	return id, nil
}
