package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/internal/place"
	"github.com/getcanyon-ai/canyon/runs"
	"github.com/jackc/pgx/v5"
)

// routesReleaseReport registers the Open group's report route (contracts/routes.md,
// POST /releases/:id/report; design/HAND-OFF.md §4): no session, authenticated by the host token
// alone, like POST /webhooks/github is by its signature.
func routesReleaseReport(mux *http.ServeMux, a *app) {
	mux.HandleFunc("POST /releases/{id}/report", a.reportRelease)
}

// reasonRe is report_failed.reason's shape (release.schema.json): 1 to 1024 characters, no
// control character — the schema's own pattern, so a reason can never forge a build.log row.
var reasonRe = regexp.MustCompile(`^[^\x00-\x1F\x7F]+$`)

// validReason counts code points, as the schema's maxLength does: a reason in CJK or Cyrillic is
// as long as its characters, not its bytes.
func validReason(s string) bool { return utf8.RuneCountInString(s) <= 1024 && reasonRe.MatchString(s) }

// releaseReport is one of report_deployed, report_failed or report_removed, whichever status names.
type releaseReport struct {
	Status    string
	Address   string
	Digest    string
	Reason    string
	Resources []reportedResource // a deployed report's: nil when the key is absent, which is [] (providedTx); checked against the release once it is read (endpointsFit, missingResources)
	// Proof is what says the release is live: "" for the platform's report, "check-in" for a delivery: repo
	// release's first check-in (#2470), which writes its own audit row and never the platform's words.
	Proof  string
	Commit string // a check-in's: the commit the branch was found to hold
}

// reportedResource is one entry of a deployed report's resources: where the org's platform says a resource
// of the release is (#2432). Never a credential: validEndpoint refuses the places one goes in a URL, its
// userinfo and its query.
type reportedResource struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

// validAddress is a deployed report's address: https alone, its path any segments of unreserved characters
// (#2432; it took https://h/?token=x before). The deploy repo's route template is checked by the same parse.
func validAddress(s string) bool { return place.ValidAddress(s) }

// validEndpoint is where a resource of type typ is: in its type's scheme, at most 255 bytes, its path empty
// or one segment.
func validEndpoint(s, typ string) bool { return place.ValidEndpoint(s, typ) }

// endpointsFit is whether a deployed report's resources name the release's own declared resources, each
// once — so never more than it has — each endpoint valid for its resource's type.
func endpointsFit(reported []reportedResource, declared map[string]string) bool {
	if len(reported) > len(declared) {
		return false
	}
	seen := map[string]bool{}
	for _, r := range reported {
		typ, ok := declared[r.Name]
		if !ok || seen[r.Name] || !validEndpoint(r.Endpoint, typ) {
			return false
		}
		seen[r.Name] = true
	}
	return true
}

// missingResources is what a deployed report that fits leaves out of the release's declared resources,
// sorted: every one must be named (#2452), so a non-empty answer is 409 resources_missing.
func missingResources(reported []reportedResource, declared map[string]string) []string {
	named := map[string]bool{}
	for _, r := range reported {
		named[r.Name] = true
	}
	var missing []string
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !named[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// parseReport is the report's body, exactly one of release.schema.json's three shapes —
// additionalProperties: false, so an extra key, a wrong type or an unknown status is refused —
// never a heuristic: the same three key sets the schema pins.
func parseReport(raw map[string]json.RawMessage) (releaseReport, bool) {
	text := func(k string) (string, bool) {
		var s string
		return s, raw[k] != nil && json.Unmarshal(raw[k], &s) == nil
	}
	status, ok := text("status")
	if !ok {
		return releaseReport{}, false
	}
	want := map[string][]string{"deployed": {"status", "address", "digest"}, "failed": {"status", "reason"}, "removed": {"status"}}[status]
	optional := 0
	if status == "deployed" && raw["resources"] != nil { // the one key whose need the release says (#2432, #2452): judged once it is read
		want, optional = append(want, "resources"), 1
	}
	if want == nil || len(raw) != len(want) {
		return releaseReport{}, false
	}
	for _, k := range slices.Sorted(maps.Keys(raw)) {
		if !slices.Contains(want, k) {
			return releaseReport{}, false
		}
	}
	switch status {
	case "deployed":
		address, ok1 := text("address")
		digest, ok2 := text("digest")
		if !ok1 || !ok2 || !validAddress(address) || !digestRe.MatchString(digest) {
			return releaseReport{}, false
		}
		var resources []reportedResource
		if optional == 1 && !exactEntries(raw["resources"], &resources) {
			return releaseReport{}, false
		}
		return releaseReport{Status: status, Address: address, Digest: digest, Resources: resources}, true
	case "failed":
		reason, ok := text("reason")
		if !ok || !validReason(reason) {
			return releaseReport{}, false
		}
		return releaseReport{Status: status, Reason: reason}, true
	default: // removed
		return releaseReport{Status: status}, true
	}
}

// digestRe is release.schema.json's digest: sha256:<64 lowercase hex>.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// exactEntries decodes a report's resources: a list, each entry exactly {name, endpoint}, both strings.
func exactEntries(raw json.RawMessage, into *[]reportedResource) bool {
	var entries []map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || entries == nil {
		return false
	}
	*into = []reportedResource{}
	for _, e := range entries {
		var name, endpoint *string
		if len(e) != 2 || json.Unmarshal(e["name"], &name) != nil || json.Unmarshal(e["endpoint"], &endpoint) != nil || name == nil || endpoint == nil {
			return false
		}
		*into = append(*into, reportedResource{Name: *name, Endpoint: *endpoint})
	}
	return true
}

// release is the reported-on row, as the report needs it: never a step's input, this route has no run.
type release struct {
	ID, ProjectID, Env, RunID, PackageID, Image, State, Event string
	GateRun                                                   *string           // a promotion's: the gate run preview was deployed from (0045)
	DeployedBy                                                [3]*string        // a promotion's: who deployed preview, kind, subject, display
	Deletes                                                   bool              // a project delete's remove (0104)
	Declared                                                  map[string]string // its package's resources, name to type
	PackageGone                                               bool              // its package row is gone: what it declares is unknown (#2452)
}

// readRelease is the reported-on row with its package's declared resources: a package that declares none
// (NULL, every row before 0021) is none, and a release whose package row is gone is PackageGone, never a
// release that declares nothing (#2452): a deployed report on it fails closed, since every declared resource
// must be reported, and a failed or removed one, which names none, is recorded as ever.
func readRelease(ctx context.Context, q querier, id string) (release, error) {
	var r release
	var declared *map[string]string
	r.ID = id
	err := q.QueryRow(ctx, `SELECT r.project_id, r.env, r.run_id, r.package_id, r.image, r.state, r.event, r.gate_run, r.deployed_by_kind, r.deployed_by_subject, r.deployed_by_display, r.deletes_resources,
		(SELECT coalesce(resources, '{}') FROM packages WHERE id = r.package_id AND project_id = r.project_id)
		FROM releases r WHERE r.id = $1`, id).
		Scan(&r.ProjectID, &r.Env, &r.RunID, &r.PackageID, &r.Image, &r.State, &r.Event, &r.GateRun, &r.DeployedBy[0], &r.DeployedBy[1], &r.DeployedBy[2], &r.Deletes, &declared)
	if declared != nil {
		r.Declared = *declared
	}
	r.PackageGone = err == nil && declared == nil
	return r, err
}

// newerRelease is the id of a later release of rel's project and environment, "" when rel is
// still the newest (design/HAND-OFF.md §4: "a newer release of the same environment superseded").
func newerRelease(ctx context.Context, q querier, rel release) (string, error) {
	var newer string
	err := q.QueryRow(ctx, `SELECT id FROM releases WHERE project_id = $1 AND env = $2
		AND created_at > (SELECT created_at FROM releases WHERE id = $3) ORDER BY created_at LIMIT 1`,
		rel.ProjectID, rel.Env, rel.ID).Scan(&newer)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return newer, err
}

// removeAgain sends another remove when a deployed report lands on rel, a deploy release an
// undeploy's remove superseded, and the environment is still offline: that remove is its newest
// release, no deploy — Canyon's or the org's — has followed it, and Canyon has run nothing there since
// rel (since). The platform started what was taken offline, so it is removed again, once — the same
// report again sends nothing more — the remove naming rel's own package and image, and Canyon writes the
// trail row (contracts/routes.md, The brake). An environment a person has put back since, or a run is
// writing now, is never touched.
func (a *app) removeAgain(ctx context.Context, tx pgx.Tx, rel release, since bool) error {
	if since {
		return nil
	}
	var offline bool
	if err := tx.QueryRow(ctx, `SELECT (SELECT event FROM releases WHERE id = $1) = 'deploy' AND n.event = 'remove'
		AND n.created_at > (SELECT created_at FROM releases WHERE id = $1)
		AND NOT EXISTS (SELECT 1 FROM deploys WHERE project_id = n.project_id AND env = n.env AND deployed_at > n.created_at)
		AND (SELECT count(*) FROM releases WHERE project_id = $2 AND env = $3 AND event = 'remove' AND created_at > (SELECT created_at FROM releases WHERE id = $1)) = 1
		AND NOT EXISTS (SELECT 1 FROM runs WHERE project_id = $2 AND state IN ('pending', 'running')
			AND CASE $3 WHEN 'prod' THEN kind = 'promote' ELSE kind = 'gate' AND deploys END)
		FROM (SELECT event, created_at, project_id, env FROM releases WHERE project_id = $2 AND env = $3 ORDER BY created_at DESC LIMIT 1) n`,
		rel.ID, rel.ProjectID, rel.Env).Scan(&offline); err != nil || !offline {
		return err
	}
	again, _, err := runs.SendRemoveOfTx(ctx, tx, a.durable, rel.ID)
	if err != nil {
		return err
	}
	return events.AuditTx(ctx, tx, brakeActor, "send remove again", rel.ProjectID, rel.RunID, map[string]any{"release": again, "env": rel.Env, "superseded": rel.ID})
}

// removeHeld sends a remove naming rel's own package and image when the org's platform reported rel
// deployed while production is held: every such report, superseded or not, whatever run is still waiting
// and whatever remove is open, since the platform says it runs it now. The 409 names no hold and no
// admins: the token is the platform's, no person's.
func (a *app) removeHeld(ctx context.Context, tx pgx.Tx, rel release) error {
	again, _, err := runs.SendRemoveOfTx(ctx, tx, a.durable, rel.ID)
	if err != nil {
		return err
	}
	return events.AuditTx(ctx, tx, brakeActor, "send remove again", rel.ProjectID, rel.RunID, map[string]any{"release": again, "env": rel.Env, "superseded": rel.ID})
}

// brakeActor is Canyon itself on the trail when it removes again what an org's platform started after
// an undeploy.
var brakeActor = events.Actor{Kind: "canyon", Subject: "brake", Display: "Canyon"}

// deletedProjectReport is the rule for a deployed report on a release of a project being deleted or deleted
// (#2432): going is true, and nothing of the report is written. Whatever release it names — superseded,
// undelivered, failed — the platform says it runs the app, so one more remove with delete goes for its
// environment, with its trail row; none while one is open there, so the same report again sends nothing.
func (a *app) deletedProjectReport(ctx context.Context, tx pgx.Tx, rel release) (going bool, err error) {
	var open bool
	err = tx.QueryRow(ctx, `SELECT deleting_since IS NOT NULL OR deleted_at IS NOT NULL,
		EXISTS (SELECT 1 FROM releases WHERE project_id = $1 AND env = $2 AND event = 'remove' AND deletes_resources AND state IN ('sent', 'accepted'))
		FROM projects WHERE id = $1`, rel.ProjectID, rel.Env).Scan(&going, &open)
	if err != nil || !going || open {
		return going, err
	}
	again, err := runs.SendDeleteRemoveTx(ctx, tx, a.durable, rel.ProjectID, rel.Env, true)
	if err != nil {
		return true, err
	}
	return true, events.AuditTx(ctx, tx, brakeActor, "send remove again", rel.ProjectID, rel.RunID, map[string]any{"release": again, "env": rel.Env, "superseded": rel.ID, "delete": true})
}

// canyonSince is whether a deploy Canyon ran has followed rel in its environment — a switch back to
// run_by: canyon, which rel records as canyon_since (0111) — which supersedes rel too (contracts/routes.md, the
// report row): no report on it overwrites or removes what Canyon runs there, nor puts the org's back once
// Canyon has undeployed its own (#2469).
func canyonSince(ctx context.Context, q querier, rel release) (bool, error) {
	var since bool
	err := q.QueryRow(ctx, "SELECT canyon_since IS NOT NULL FROM releases WHERE id = $1", rel.ID).Scan(&since)
	return since, err
}

// tellAnotherDigest tells the run waiting on rel that its platform reported another image
// (contracts/events.md): once per digest it names, the run keeps waiting, the release open for the
// reviewed one; a run that has ended is told nothing, its timeline frozen (events.ErrEnded).
func tellAnotherDigest(ctx context.Context, tx pgx.Tx, rel release, expected, reported string) error {
	short := func(d string) string { return d[:min(len(d), len("sha256:")+12)] + "…" }
	line := "Your platform reported another image, " + short(reported) + ", not the reviewed " + short(expected) + ": nothing went live"
	_, err := events.AppendRunTx(ctx, tx, rel.RunID, rel.ProjectID, events.BuildLog, "another digest:"+rel.ID+":"+reported, map[string]any{"line": line})
	if errors.Is(err, events.ErrEnded) {
		return nil
	}
	return err
}

// whereNames is the names a report left out as its lines say them (EXPERIENCE.md D5, #2452): joined with
// ", ", then is for one and are for more.
func whereNames(missing []string) string {
	verb := " is"
	if len(missing) > 1 {
		verb = " are"
	}
	return strings.Join(missing, ", ") + verb
}

// refuseMissing is 409 resources_missing's writes (contracts/routes.md, the report row; #2452): nothing goes
// live, the release keeps the names for its environment's pending line, the run waiting on it is told once
// per release (contracts/events.md) and keeps waiting — a run that has ended is told nothing — and one
// audit row names them.
func refuseMissing(ctx context.Context, tx pgx.Tx, rel release, missing []string, by actor) error {
	if _, err := tx.Exec(ctx, "UPDATE releases SET missing_resources = $2 WHERE id = $1", rel.ID, missing); err != nil {
		return err
	}
	var version string
	if err := tx.QueryRow(ctx, "SELECT coalesce(label, substr(id, 5)) FROM packages WHERE id = $1 AND project_id = $2", rel.PackageID, rel.ProjectID).Scan(&version); err != nil {
		return err
	}
	line := "Your platform reported " + version + " deployed without saying where " + whereNames(missing) + ": nothing went live, Canyon is still waiting"
	if _, err := events.AppendRunTx(ctx, tx, rel.RunID, rel.ProjectID, events.BuildLog, "resources missing:"+rel.ID, map[string]any{"line": line}); err != nil && !errors.Is(err, events.ErrEnded) {
		return err
	}
	return events.AuditTx(ctx, tx, by, "platform reported deployed without resources", rel.ProjectID, rel.RunID, map[string]any{"release": rel.ID, "env": rel.Env, "missing": missing})
}

// reporter is the actor every report's audit and env-write rows carry (design/HAND-OFF.md §4).
func (a *app) reporter(ctx context.Context) actor {
	return actor{Kind: "agent", Subject: "platform", Display: a.orgNow(ctx).name + "'s platform"}
}

// outcome is what the mutation decided, once it has committed: the HTTP answer this call gets.
type outcome struct {
	status int
	body   map[string]any
}

func (a *app) reportRelease(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	prov := events.Provenance{Via: "platform", Source: a.proxies.source(r)} // its audit rows are the platform's report
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	const badToken = "the bearer token is missing, unknown or a replaced one"
	if !ok || token == "" {
		writeErr(w, http.StatusUnauthorized, "bad_token", badToken, nil)
		return
	}
	// Decoded here, not by decodeJSON: a body that is not JSON is unshaped like any other, judged
	// in the row's order — after the token and the release — as 400 invalid_report, never a
	// bad_request ahead of the 401 (security review of #509).
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	var report releaseReport
	shaped := false
	if dec.Decode(&raw) == nil {
		if _, err := dec.Token(); err == io.EOF { //nolint:errorlint // sentinel from the decoder itself
			report, shaped = parseReport(raw)
		}
	}
	out := outcome{status: http.StatusOK, body: map[string]any{}}
	err := a.inMutation(ctx, func(tx pgx.Tx) error {
		// The token is checked inside the lock this mutation already takes (a.mutation → events.LockTx),
		// serialized against a concurrent rotation: a token replaced mid-request is refused, never raced.
		valid, err := hostTokenValid(ctx, tx, token)
		if err != nil {
			return err
		}
		if !valid {
			out.status, out.body = http.StatusUnauthorized, map[string]any{"error": "bad_token", "message": badToken}
			return nil
		}
		// Bounded per token and taken only once the token validates (contracts/routes.md, "bounded
		// per token"): a wrong token drains nothing, so garbage cannot lock the real pipeline out
		// (security review of #509). The key is the token's hash, never the token.
		sum := sha256.Sum256([]byte(token))
		if wait, ok := a.reports.take(hex.EncodeToString(sum[:])); !ok {
			out.status, out.body = http.StatusTooManyRequests, map[string]any{"error": "rate_limited", "message": "60 reports a minute; try again shortly", "retry_after_s": int(wait.Seconds()) + 1}
			return nil
		}
		rel, err := readRelease(ctx, tx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			out.status, out.body = http.StatusNotFound, map[string]any{"error": "not_found", "message": "no such release"}
			return nil
		}
		if err != nil {
			return err
		}
		if !shaped || !endpointsFit(report.Resources, rel.Declared) { // the endpoints once the release is read: 404 first (#2432)
			out.status, out.body = http.StatusBadRequest, map[string]any{"error": "invalid_report", "message": "the body is not one of deployed, failed or removed, exactly shaped"}
			return nil
		}
		out, err = a.settle(ctx, tx, rel, report, a.reporter(ctx), prov)
		return err
	})
	if err != nil {
		a.log.Printf("canyon-server: report on release %s: %v", id, err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error", nil)
		return
	}
	writeJSON(w, out.status, out.body)
}

// settle decides a well-shaped report on rel inside the caller's mutation: every refusal in the report row's
// order (contracts/routes.md), then its write. The caller has authenticated the report and checked its shape;
// by and prov are who its audit and environment rows name, and how it came.
func (a *app) settle(ctx context.Context, tx pgx.Tx, rel release, report releaseReport, by actor, prov events.Provenance) (outcome, error) {
	ctx = events.WithProvenance(ctx, prov)
	done := outcome{status: http.StatusOK, body: map[string]any{}}
	refuse := func(body map[string]any) outcome { return outcome{status: http.StatusConflict, body: body} }
	if report.Status == "deployed" && rel.PackageGone { // what the release declares is unknown: never read as nothing (#2452, fail closed)
		return done, fmt.Errorf("release %s names package %s, which its project does not have", rel.ID, rel.PackageID)
	}
	if report.Status == "deployed" && rel.Event != "deploy" { // a remove — the brake's, a hold's, a delete's — never goes live, held or not: before anything is written (#2432)
		return refuse(map[string]any{"error": "not_a_deploy", "message": "this release is a remove: report removed or failed"}), nil
	}
	if report.Status == "deployed" { // a project being deleted or deleted takes no deployed report, of any release (#2432)
		going, err := a.deletedProjectReport(ctx, tx, rel)
		if err != nil {
			return done, err
		}
		if going {
			return refuse(map[string]any{"error": "project_deleted", "message": "this app is deleted or being deleted: Canyon asks your platform to remove it"}), nil
		}
	}
	newer, err := newerRelease(ctx, tx, rel)
	if err != nil {
		return done, err
	}
	var held bool // while production is held, every deployed report on it, superseded or not, sends a remove of what it started (contracts/routes.md, The hold)
	if report.Status == "deployed" && rel.Env == "prod" {
		h, err := runs.ReadHold(ctx, tx, rel.ProjectID)
		if err != nil {
			return done, err
		}
		if held = h != nil; held {
			if err := a.removeHeld(ctx, tx, rel); err != nil {
				return done, err
			}
		}
	}
	since, err := canyonSince(ctx, tx, rel)
	if err != nil {
		return done, err
	}
	if newer != "" || since || (report.Status == "deployed" && rel.State == "removed") {
		if report.Status == "deployed" && newer != "" && !held { // the platform started what an undeploy's remove took off: it is removed again (contracts/routes.md, The brake); held, it was above
			if err := a.removeAgain(ctx, tx, rel, since); err != nil {
				return done, err
			}
		}
		if newer == "" { // deployed after removed, or a Canyon deploy since, no newer release: this one is named
			newer = rel.ID
		}
		return refuse(map[string]any{"error": "superseded", "release": newer}), nil
	}
	if report.Status == "deployed" { // the digest before the duplicate's no-op: a wrong digest never rides a same-status 200
		_, expected, _ := strings.Cut(rel.Image, "@")
		if expected != report.Digest {
			if err := tellAnotherDigest(ctx, tx, rel, expected, report.Digest); err != nil {
				return done, err
			}
			return refuse(map[string]any{"error": "digest_mismatch", "expected": expected}),
				events.AuditTx(ctx, tx, by, "platform reported another digest", rel.ProjectID, rel.RunID, map[string]any{"release": rel.ID, "env": rel.Env, "expected": expected, "reported": report.Digest})
		}
	}
	if rel.State == report.Status { // the same status again: 200, nothing written
		return done, nil
	}
	switch report.Status {
	case "deployed":
		if missing := missingResources(report.Resources, rel.Declared); len(missing) > 0 && !held { // every resource the release lists, after every other refusal; held, the hold's 409 below stands (#2452)
			return refuse(map[string]any{"error": "resources_missing", "missing": missing,
					"message": "the report has no endpoint for " + strings.Join(missing, ", ") + ": send one for every resource the release lists"}),
				refuseMissing(ctx, tx, rel, missing, by)
		}
		err := a.recordDeployed(ctx, tx, rel, report, by)
		if errors.Is(err, runs.ErrHeld) { // nothing was written, and the remove went out above
			return refuse(map[string]any{"error": "held", "message": "production is held"}), nil
		}
		return done, err
	case "failed": // the reason beside the state: the run waiting on this release ends on it (runs/release.go, awaitReport)
		if _, err := tx.Exec(ctx, "UPDATE releases SET state = 'failed', reason = $2 WHERE id = $1", rel.ID, report.Reason); err != nil {
			return done, err
		}
		return done, events.AuditTx(ctx, tx, by, "platform reported failed", rel.ProjectID, rel.RunID, map[string]any{"release": rel.ID, "env": rel.Env, "reason": report.Reason})
	default: // removed
		if err := setReleaseState(ctx, tx, rel.ID, "removed"); err != nil {
			return done, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM deploys WHERE project_id = $1 AND env = $2", rel.ProjectID, rel.Env); err != nil {
			return done, err
		}
		if rel.Deletes { // a project delete's remove: the platform deleted what it provisioned there (#2432)
			if _, err := tx.Exec(ctx, "DELETE FROM platform_resources WHERE project_id = $1 AND env = $2", rel.ProjectID, rel.Env); err != nil {
				return done, err
			}
		}
		return done, events.AuditTx(ctx, tx, by, "platform reported removed", rel.ProjectID, rel.RunID, map[string]any{"release": rel.ID, "env": rel.Env})
	}
}

func setReleaseState(ctx context.Context, tx pgx.Tx, id, state string) error {
	_, err := tx.Exec(ctx, "UPDATE releases SET state = $1 WHERE id = $2", state, id)
	return err
}

// recordDeployed writes the environment live at address and the run's deploy.completed row, one
// transaction with the caller's audit row: only a digest the release's own image carries ever
// goes live (design/HAND-OFF.md §4). A deploy's release is its run's: its gate run and deployer. A
// promotion's (0045) repeats preview's gate run and deployer, promoted_by its run's actor, as a
// Canyon-run promotion writes them; any other clears promoted_by, so a value a promotion left never
// survives a later deploy's report as if it were that deploy's.
func (a *app) recordDeployed(ctx context.Context, tx pgx.Tx, rel release, report releaseReport, by actor) error {
	address, resources := report.Address, report.Resources
	var startedBy [3]*string
	if err := tx.QueryRow(ctx, "SELECT started_by_kind, started_by_subject, started_by_display FROM runs WHERE id = $1", rel.RunID).Scan(&startedBy[0], &startedBy[1], &startedBy[2]); err != nil {
		return err
	}
	gateRun, deployedBy, promotedBy := rel.RunID, startedBy, [3]*string{}
	if rel.GateRun != nil {
		gateRun, deployedBy, promotedBy = *rel.GateRun, rel.DeployedBy, startedBy
	}
	_, digest, _ := strings.Cut(rel.Image, "@")
	d := runs.Deployed{Project: rel.ProjectID, Env: rel.Env, Package: rel.PackageID, Image: rel.Image, URL: address, GateRun: gateRun, Run: rel.RunID, DeployedBy: deployedBy, PromotedBy: promotedBy, RunBy: "org"}
	detail := map[string]any{"release": rel.ID, "env": rel.Env, "digest": digest, "url": address}
	if rel.Env == "prod" { // production goes through its one writer, which appends its history; its row says what it replaced, as deploy prod does
		replaces, _, err := runs.WriteProdTx(ctx, tx, d)
		if err != nil {
			return err
		}
		var rollback bool
		var reason *string
		if err := tx.QueryRow(ctx, "SELECT rollback_to IS NOT NULL, rollback_reason FROM runs WHERE id = $1", gateRun).Scan(&rollback, &reason); err != nil {
			return err
		}
		detail["replaces"], detail["rollback"] = nil, rollback
		if replaces != "" {
			detail["replaces"] = replaces
		}
		if reason != nil {
			detail["reason"] = *reason
		}
	} else {
		if err := runs.WritePreviewTx(ctx, tx, d); err != nil {
			return err
		}
		// A late report (design/HAND-OFF.md §3, past the run's 30 minutes) still lands here, but the
		// run's timeline is frozen once it has ended (events.End's invariant: no row after ended_at) —
		// so a late deploy.completed is skipped, never a 500. The environment still goes live above.
		if _, err := events.AppendRunTx(ctx, tx, rel.RunID, rel.ProjectID, events.DeployCompleted, "deploy:"+rel.Env, map[string]any{"env": rel.Env, "url": address, "package": rel.PackageID, "image_digest": digest}); err != nil && !errors.Is(err, events.ErrEnded) {
			return err
		}
	}
	if err := setReleaseState(ctx, tx, rel.ID, "deployed"); err != nil {
		return err
	}
	if err := providedTx(ctx, tx, rel, resources); err != nil {
		return err
	}
	// Every older release's access token dies here (contracts/routes.md, GET /releases/:id/access-token:
	// "it dies when a newer release of the environment reports deployed"), cleared with its sealed copy,
	// so no later report on this release brings one back.
	if _, err := tx.Exec(ctx, `UPDATE releases SET key_ref = NULL, sealed_key = NULL, sealed_value = NULL, access_token_hash = NULL
		WHERE project_id = $1 AND env = $2 AND created_at < (SELECT created_at FROM releases WHERE id = $3)`, rel.ProjectID, rel.Env, rel.ID); err != nil {
		return err
	}
	if report.Proof == "check-in" { // Canyon's own finding, never the platform's report (#2471): its own action, which the fleet's count and the trail's categories read too
		detail["commit"], detail["proof"] = report.Commit, "check-in"
		return events.AuditTx(ctx, tx, by, "release live by check-in", rel.ProjectID, rel.RunID, detail)
	}
	return events.AuditTx(ctx, tx, by, "platform reported deployed", rel.ProjectID, rel.RunID, detail)
}

// providedTx replaces where the org's platform says the environment's resources are with what this
// release's deployed report named (#2432): written on the deployed record's own path alone, never on a 409
// or a duplicate. Always whole (#2452): a report without the key is [], so a release that lists no
// resource clears them, and one that lists any named each or was refused before this.
func providedTx(ctx context.Context, tx pgx.Tx, rel release, resources []reportedResource) error {
	if _, err := tx.Exec(ctx, "DELETE FROM platform_resources WHERE project_id = $1 AND env = $2", rel.ProjectID, rel.Env); err != nil {
		return err
	}
	for _, r := range resources {
		if _, err := tx.Exec(ctx, "INSERT INTO platform_resources (project_id, env, name, endpoint, release_id) VALUES ($1, $2, $3, $4, $5)",
			rel.ProjectID, rel.Env, r.Name, r.Endpoint, rel.ID); err != nil {
			return err
		}
	}
	return nil
}
