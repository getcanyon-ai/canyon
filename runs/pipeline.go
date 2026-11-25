package runs

import (
	"cmp"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/custom"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
	"github.com/getcanyon-ai/canyon/gate/sarif"
	"github.com/getcanyon-ai/canyon/ingest"
	asked "github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/internal/names"
	"github.com/getcanyon-ai/canyon/provisioners"
	"github.com/getcanyon-ai/canyon/vault"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GateFunc reviews the tree at appDir, the package's tree hash tree, under the run's policy snapshot
// and returns its findings as one SARIF log in Canyon's profile (contracts/finding.md), one run per
// producer, reporting each check's start and completion to report as it runs. Gate.Review is the
// gate; tests run the recorded log (runs/runstest).
type GateFunc func(ctx context.Context, appDir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error)

// Gate is this host's gate: its Review is the Pipeline's GateFunc. Model is the host's model and
// ModelID the id it is sent; a nil Model is a host with none (EXPERIENCE D10). Pool holds the
// review cache; nil, every review calls the model.
type Gate struct {
	Model   gate.Model
	ModelID string
	Pool    *pgxpool.Pool
	// SignedIn is a host that signs people in before they reach an app (gate.Reviewer).
	SignedIn bool
}

// Review is the gate: the scanners' run, then each of the snapshot's agents appended — the skipped run
// when the host has no model. A scanner rule that is on, which no tool the policy table maps into it runs, is
// named on the scanners' run, so a clean verdict never reads as that rule checked. Each agent is one
// check reported to report.
func (g Gate) Review(ctx context.Context, appDir, tree string, snap policy.Snapshot, report gate.Report) (finding.Log, error) {
	var unchecked []string
	for _, r := range snap.Rules {
		if r.Producer == policy.Scanner && r.ID != gate.DesignRule && !custom.Checked(r) && r.On() && !filled(snap, r.ID) { // the scanners' run reads design.org-system itself, and the gate step its custom rules checked by code
			unchecked = append(unchecked, r.ID)
		}
	}
	log, err := gate.Scanners(appDir, snap, unchecked, g.Model != nil)
	for _, a := range snap.Agents {
		if err != nil {
			break
		}
		log, err = g.agent(ctx, appDir, tree, snap, a.ID, log, report)
	}
	return log, err
}

// filled reports whether a tool the policy table maps into rule runs under snap, as its row needs it:
// its sarif.<tool> rule on, with run on — gitleaks, which runs in process, takes none — and trivy
// with the scanner its row reads among its settings', unset being vuln and misconfig
// (contracts/snapshot.md; executor.md).
func filled(snap policy.Snapshot, rule string) bool {
	return slices.ContainsFunc(policy.Table, func(m policy.Mapping) bool {
		i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif."+m.Tool })
		if m.Policy != rule || i < 0 || !snap.Rules[i].On() || !snap.Rules[i].Run && m.Tool != "gitleaks" {
			return false
		}
		scanners := snap.Rules[i].Settings.Scanners
		return m.Tool != "trivy" || len(scanners) == 0 || slices.Contains(scanners, trivyScanner[m.Tags[0]])
	})
}

// trivyScanner is the trivy scanner that reports each tag a trivy row of the policy table reads.
var trivyScanner = map[string]string{"vulnerability": "vuln", "misconfiguration": "misconfig"}

// agent is review reported as agent's check: skipped when it read nothing (gate.Unread: no model, out
// of room, or the source too large), failed on an error.
func (g Gate) agent(ctx context.Context, appDir, tree string, snap policy.Snapshot, agent string, log finding.Log, report gate.Report) (finding.Log, error) {
	if err := report(ctx, gate.Check{Name: agent}); err != nil {
		return finding.Log{}, err
	}
	start, done := time.Now(), gate.Check{Name: agent, Status: "failed"}
	log, err := g.review(ctx, appDir, tree, snap, agent, log)
	if err == nil {
		done.Findings = len(log.Runs[len(log.Runs)-1].Results)
		switch {
		case gate.Unread(log.Runs[len(log.Runs)-1]) != "": // whatever it found: a review that left code unread is never read as one that read it
			done.Status = "skipped"
		case done.Findings > 0:
			done.Status = "findings"
		default:
			done.Status = "clean"
		}
	}
	done.Duration = time.Since(start)
	return log, cmp.Or(err, report(ctx, done))
}

// review appends agent's run to log: the cached run, labeled reused, when this tree, snapshot, model
// and agent were reviewed before, else the reviewer's, kept when the model completed it. A skipped
// or failed review is never kept, nor one that read nothing (gate.Unread) but for a file too large for
// any call or not valid UTF-8, which the tree alone decides: a retry reviews again. It runs inside the gate step, so a
// replay reads the journal, not the cache.
func (g Gate) review(ctx context.Context, appDir, tree string, snap policy.Snapshot, agent string, log finding.Log) (finding.Log, error) {
	r := &gate.Reviewer{Snapshot: snap, Agent: agent, ModelID: g.ModelID, Model: g.Model, SignedIn: g.SignedIn}
	if g.Model == nil || g.Pool == nil {
		return r.Review(ctx, appDir, log)
	}
	var run finding.Run
	err := g.Pool.QueryRow(ctx, "SELECT run FROM review_cache WHERE tree_hash = $1 AND snapshot_hash = $2 AND model_id = $3 AND agent = $4",
		tree, snap.Hash, g.ModelID, agent).Scan(&run)
	if err == nil {
		reused := finding.Notification{Level: "note", Message: finding.Message{Text: gate.Note(agent, gate.Reused)}}
		if len(run.Invocations) == 0 {
			run.Invocations = []finding.Invocation{{ExecutionSuccessful: true}}
		}
		run.Invocations[0].ToolExecutionNotifications = append(run.Invocations[0].ToolExecutionNotifications, reused)
		log.Runs = append(log.Runs, run)
		return log, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return finding.Log{}, err
	}
	if log, err = r.Review(ctx, appDir, log); err != nil || !slices.Contains([]string{"", gate.TooLargeForACall, gate.NotUTF8}, gate.Unread(log.Runs[len(log.Runs)-1])) {
		return log, err
	}
	_, err = g.Pool.Exec(ctx, "INSERT INTO review_cache (tree_hash, snapshot_hash, model_id, agent, run) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING",
		tree, snap.Hash, g.ModelID, agent, log.Runs[len(log.Runs)-1])
	return log, err
}

// Pipeline drives a gate run as a DBOS workflow: every write is a checkpointed step, so a run
// resumes after a crash from its last finished step, and every step is safe to run twice.
type Pipeline struct {
	// SignInOff is a host with sign-in off (CANYON_AUTH off or demo), where anyone who reaches it opens every app: a
	// deploy says so in place of the shared line (contracts/events.md, build.log; #2498). Zero, a host with sign-in.
	SignInOff bool
	Pool      *pgxpool.Pool
	Repos     string // <Repos>/<project>.git is the project's bare repository
	Gate      GateFunc
	Executor  *executor.Client     // builds a deploy run's package and starts containers: preview, and prod on a promotion
	PublicURL string               // the host apps are published under: http://<app>.<env>.<its host>; the identity token's issuer
	JWKSURL   string               // where an app fetches the identity key set from its own network: a Docker /run's identity.jwks_url
	Ask       http.RoundTripper    // carries Docker's ask of what a deployed app answered; nil is answered.Direct (contracts/routes.md, envs.<env>.answered)
	Vault     *vault.Vault         // opens the project's secrets: the build values for the build, the runtime values for the container
	Drivers   provisioners.Drivers // make declared resources real, one per type and env; nil on a host that provisions nothing
	// Attach is the server's attach of one report (contracts/finding.md): a run's scans attach each tool's
	// SARIF through it as an upload is, by as the actor, with the rules the executor's answer says the tool
	// could not load, as stored and shown, and the files it did not read whole, and read the stored row's
	// id; another tool's is refused.
	Attach func(ctx context.Context, project, pkg, tool string, raw []byte, unloaded, unparsed []string, ev Evidence, by events.Actor) (int64, error)
	// Access is the server's access file of an app (contracts/executor.md, PUT /access), set on a Kubernetes
	// host alone: a deploy or promotion there needs the front door, /run carries both, and an app's address
	// is the front door's. nil on Docker, whose /run carries neither.
	Access func(ctx context.Context, app string) (json.RawMessage, error)
	// IdentityImage is CANYON_IMAGE_IDENTITY, canyon-identity's image by digest: the executor's own
	// copy runs the sidecar it starts; a release under run_by: org (design/HAND-OFF.md §3) names the
	// same image for the org's pipeline to run instead, so both must be set from the one env var.
	IdentityImage string
	// DatabaseCA is CANYON_APP_DATABASE_CA_FILE's certificates: every Kubernetes /run carries them as
	// database_ca, the bundle each app's DATABASE_URL verifies the apps database against. "" sends none.
	DatabaseCA string
	// Clears is the server's judgement of whether review run clears what a promotion of it ships: the
	// promote receipt's own, read now (contracts/routes.md, the approve rows). nil clears nothing.
	Clears func(ctx context.Context, project, run string) (bool, error)
	// Org is the org's name as the server says it now, which a deploy's access line says when everyone can
	// open the app (contracts/events.md, build.log): never "" (the server's default name stands in). main wires it.
	Org func(ctx context.Context) string
	// EgressProxy is CANYON_EGRESS_PROXY, Canyon's Envoy as a /run under via canyon names it
	// (contracts/executor.md, Egress). "" with via canyon fails the /run closed.
	EgressProxy string
	// EgressConverge bounds a /run's wait for egress-control to take the policy that names its env
	// (design/EGRESS.md §3.2, Convergence): 30 s when zero.
	EgressConverge time.Duration
	// ScanConcurrency and BuildConcurrency are the host's capacities, CANYON_SCAN_CONCURRENCY and
	// CANYON_BUILD_CONCURRENCY: the scans and builds that run at once across every run and server replica
	// (runs/queue.go); 0 is Docker's, 2 and 1.
	ScanConcurrency, BuildConcurrency int
	// RescanEvery is CANYON_PATCH_INTERVAL, how often patch.rescan re-matches the platform layers of the
	// images that run (design/PATCHING.md §3.2); 0 is DefaultRescan.
	RescanEvery time.Duration
	// ProjectPolicy is the server's composition of a project's snapshot through q, as a run's start composes
	// it: the rescan reads and matches the images of a project whose platform.known-vuln is on alone, and a
	// patch run's start composes it on its own transaction, under the ledger lock it holds. nil, none.
	ProjectPolicy Swappable[func(ctx context.Context, q Querier, project string) (policy.Snapshot, error)]
	// PatchConcurrency is CANYON_PATCH_CONCURRENCY, the patch runs of the patches queue at once across every
	// replica (design/PATCHING.md §4.2); 0 is Docker's, 1. The image scans after a deploy have a queue of
	// their own, so a person's never waits on a patch run.
	PatchConcurrency int
	// PatchWatch is CANYON_PATCH_WATCH, how long a patched preview is watched for an exit or a restart
	// (design/PATCHING.md §4.4); 0 is DefaultPatchWatch. A watch reads it as it starts, so it is set atomically.
	PatchWatch Swappable[time.Duration]
	// Patches is whether the operator turned patching on (CANYON_PATCHING on, beta) and sign-in is on (CANYON_AUTH
	// oidc): with it off nobody can decide a patch approval, so patching reads off (design/PATCHING.md §4.5).
	// Off, nothing is planned, a queued patch run ends at step 1, and one past it stops before its /run begins
	// (beginRun); the rescan reads and matches as ever. Every patch workflow reads it live, while it runs, so
	// it is set and read atomically (#2664).
	Patches atomic.Bool
	// Patcher decides and schedules Canyon's patches, Canyon Enterprise's (ee/patching); nil in a server built without
	// it, which reads patching as off. Set before Launch: a resumed watch files through it.
	Patcher Patcher
	// PlatformRead is the server's platform.known-vuln read of a project's image (contracts/finding.md,
	// System libraries and build tools), as the receipt reads it. nil plans none.
	PlatformRead Swappable[func(ctx context.Context, project, digest string) (PlatformImage, error)]
	// DecidePatch is the server's approve route for the patch approval code, asked by the admin who approved its
	// identity for the fleet (contracts/routes.md, Production approvals): its checks under its lock, no
	// promote_anyway. It answers whether it approved; a refusal leaves the approval pending. nil decides none.
	DecidePatch func(ctx context.Context, code string, by events.Actor) (bool, error)
	// Inject is Canyon Enterprise's outbound keys on a /run (egress_ca.go): nil, a build without it, carries no
	// egress.ca and no placeholder (contracts/routes.md, Licence, A server built without Canyon Enterprise).
	Inject Inject
	// RepoTransport carries the deploy repo's own https — its probe and its pulls API (design/HAND-OFF.md §3b);
	// nil is a release's transport, the system's roots, no proxy. git itself trusts SSL_CERT_FILE.
	RepoTransport http.RoundTripper
	named         runImageMemo          // the named run image's decoded read (NamedRunImage)
	queues        map[string]dbos.Queue // by name, once Register has run
	dc            dbos.Client           // the store Register put the workflows on: a run that carries out a stop under run_by: org enqueues its remove there

	hold      atomic.Pointer[chan struct{}]   // tests only: the workflow waits here before its first write
	patching  atomic.Pointer[func()]          // tests only: a patch's deploy step calls it before its /run, after its own config step
	reverting atomic.Pointer[func(at string)] // tests only: a revert calls it before its undeploy lock ("before"), once it holds it ("locked"), and after its compare-and-set ("cas")
}

// Result is what the workflow leaves in DBOS's journal; the runs table is what the API reads.
type Result struct {
	State    string `json:"state"`
	Blocking int    `json:"blocking"`
	Warnings int    `json:"warnings"`
}

// Register puts the workflow and its queue on dc; it must run before dbos.Launch. The policy a
// run derives against arrives in its Input, composed at start (StartInput.Policy), so nothing here
// holds one: the server proves at boot that a snapshot can be composed.
func (p *Pipeline) Register(dc dbos.Context) error {
	p.dc = dc
	dbos.RegisterWorkflow(dc, p.run, dbos.WithWorkflowName(Workflow), dbos.WithMaxRecoveryAttempts(3))
	dbos.RegisterWorkflow(dc, p.remove, dbos.WithWorkflowName(RemoveWorkflow), dbos.WithMaxRecoveryAttempts(3))
	dbos.RegisterWorkflow(dc, p.watchPatch, dbos.WithWorkflowName(WatchWorkflow), dbos.WithMaxRecoveryAttempts(3))
	if _, err := dbos.RegisterQueue(dc, Queue); err != nil {
		return err
	}
	if err := p.registerRescan(dc); err != nil {
		return err
	}
	return p.registerQueues(dc)
}

func (p *Pipeline) run(ctx dbos.Context, in Input) (Result, error) {
	if h := p.hold.Load(); h != nil {
		select {
		case <-*h:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	// First, is the run still ours to drive? A superseded run's workflow can reach here when the
	// cancel after Start's commit did not land; it must write nothing.
	var state string
	if err := p.Pool.QueryRow(ctx, "SELECT state FROM runs WHERE id = $1", in.Run).Scan(&state); err != nil {
		return Result{}, err
	}
	if state != "pending" && state != "running" {
		return Result{State: state}, nil
	}
	// Step names are the checkpoint keys DBOS matches on replay: fixed, never the closure's.
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { return struct{}{}, p.running(c, in) }, dbos.WithStepName("running")); err != nil {
		return Result{}, err
	}
	if p.Access != nil && (in.Deploy || in.Promote != nil) { // on Kubernetes nothing runs without a front door: the run ends before the executor is called
		set, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) { return p.doorSet(c, in) }, dbos.WithStepName("front door"))
		if err != nil {
			return Result{}, err
		}
		if !set {
			return p.finish(ctx, in, "failed", "error", 0, 0)
		}
	}
	if in.Promote != nil {
		return p.promote(ctx, in)
	}
	if in.Patch != nil { // step 1 refuses a patch whose identity paused, or stalled naming its app, since it was enqueued
		refused, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) { return p.patchRefused(c, in) }, dbos.WithStepName("patch refused"))
		if err != nil || refused {
			return p.interrupted(ctx, in, err, 0, 0)
		}
	}
	reports, missed, stop, err := p.scanAll(ctx, in)
	if stop {
		return p.interrupted(ctx, in, err, 0, 0)
	}
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) {
			return Result{}, err
		}
		return p.failed(ctx, in, "scan", err, 0, 0) // the executor unreachable, refusing the request, or answering what the contract does not name
	}
	in.Reports = reports // the gate and the verdict read what the run's own scans attached
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
		return p.interrupted(ctx, in, err, 0, 0)
	}
	// A scan the cluster had no room for is capacity: nothing is reviewed, so nothing reads as a block to approve.
	if slices.ContainsFunc(missed, func(m unfinished) bool { return m.NoRoom }) {
		return p.noRoom(ctx, in, missed)
	}
	g, err := dbos.RunAsStep(ctx, func(c context.Context) (gated, error) {
		res, err := p.gate(c, in, missed)
		if errors.Is(err, gate.ErrModel) { // a result, not an error: a replay reads the reason back, and a journaled error keeps only its text
			log.Printf("runs: run %s (%s): ai review failed: %v", in.Run, in.Project, err) // the wrapped text alone: never a file body or a key (gate/review.go, gate/anthropic.go)
			return gated{Model: true}, nil
		}
		return gated{Log: res}, err
	}, dbos.WithStepName("gate"))
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) { // a shutdown or a cancel, not a verdict: DBOS keeps the run for recovery or ends it itself
			return Result{}, err
		}
		return p.failed(ctx, in, "review", err, 0, 0) // a gate error, or a log the profile refused inside the step
	}
	if g.Model {
		return p.finish(ctx, in, "failed", "model", 0, 0) // the reviewer's model failed or answered what the profile refuses
	}
	log := g.Log
	blocking, warnings, err := counts(log)
	if err != nil {
		return p.failed(ctx, in, "review", err, 0, 0)
	}
	state = "clean"
	if blocking > 0 {
		state = "blocked"
	}
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
		return p.interrupted(ctx, in, err, blocking, warnings)
	}
	ruled, err := dbos.RunAsStep(ctx, func(c context.Context) (ruling, error) {
		return p.verdict(c, in, log, state, blocking, warnings)
	}, dbos.WithStepName("verdict"))
	if err != nil {
		return Result{}, err
	}
	if in.Patch != nil {
		return p.patch(ctx, in, state == "clean" || ruled.Approved, state, blocking, warnings)
	}
	if in.RollbackTo != "" && (state == "clean" || ruled.Approved) { // a rollback review ships the image production ran, never one built now (contracts/routes.md, the gate-runs row)
		shipped, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) { return p.ships(c, in) }, dbos.WithStepName("ships"))
		if err != nil {
			return Result{}, err
		}
		if !shipped {
			return p.finish(ctx, in, "failed", "image_gone", blocking, warnings)
		}
	}
	if in.Deploy && (state == "clean" || ruled.Approved) { // a deploy continues into build, then run, on clean or with every blocking finding approved (the run still ends blocked); otherwise a blocked verdict builds nothing (contracts/routes.md)
		if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
			return p.interrupted(ctx, in, err, blocking, warnings)
		}
		hand, err := dbos.RunAsStep(ctx, func(c context.Context) (handOff, error) { return p.handOffOf(c) }, dbos.WithStepName("run by"))
		if err != nil {
			return Result{}, err
		}
		orgMode := hand.Org
		image, done, end, err := p.image(ctx, in, !orgMode, blocking, warnings) // the org's platform runs it: no /run to reuse an image for
		if done {
			return end, err
		}
		res, err := p.ensureAll(ctx, in, "preview", &orgMode) // the same read: the org's platform provisions what it runs (#2432)
		if err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)) {
			return Result{}, err
		}
		if err == nil && res.Cancelled {
			return p.interrupted(ctx, in, nil, blocking, warnings)
		}
		if err != nil { // no container
			return p.failed(ctx, in, "resources", err, blocking, warnings)
		}
		if res.Failed != "" { // a failed resource's message is the last build.log line: its driver's, not Canyon's
			return p.finish(ctx, in, "failed", "error", blocking, warnings)
		}
		if orgMode { // build → publish already ran above; release replaces /run, then this call waits on the org's report (design/HAND-OFF.md §3)
			if err := p.sendRelease(ctx, in, "preview", image.Image, hand.Via); err != nil {
				if ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) {
					return Result{}, err
				}
				if errors.Is(err, errNotCleared) { // its approvals no longer clear it: the platform was never handed it
					return p.interrupted(ctx, in, nil, blocking, warnings)
				}
				return p.finish(ctx, in, "failed", "error", blocking, warnings)
			}
		} else {
			began, err := p.began(ctx, in, "start")
			if err != nil {
				return Result{}, err
			}
			url, gone, err := p.deployStep(ctx, in, image)
			if gone { // the reused image is not on the host: built now, once (contracts/routes.md, the deploys row)
				// The start goes on once the build is done, so it stays running meanwhile (#1752): the steps that
				// wrote it failed here, then running again, write nothing; its time is from the build's end.
				if err := unwritten[time.Duration](ctx, "ended"); err != nil {
					return Result{}, err
				}
				if err := unwritten[struct{}](ctx, "progress"); err != nil {
					return Result{}, err
				}
				if image, done, end, err = p.image(ctx, in, false, blocking, warnings); done {
					return end, err
				}
				if began, err = now(ctx); err != nil {
					return Result{}, err
				}
				if err := unwritten[struct{}](ctx, "progress"); err != nil {
					return Result{}, err
				}
				url, _, err = p.deployStep(ctx, in, image)
			}
			if ctx.Err() == nil && !errors.Is(err, dbos.ErrWorkflowCancelled) {
				if perr := p.ended(ctx, in, "start", began, err == nil && url != "" && url != notCleared); perr != nil {
					return Result{}, perr
				}
			}
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) {
					return Result{}, err
				}
				return p.failed(ctx, in, "deploy", err, blocking, warnings)
			}
			if url == notCleared { // its approvals no longer clear it: nothing started
				return p.interrupted(ctx, in, nil, blocking, warnings)
			}
			if url == "" {
				return p.finish(ctx, in, "failed", "start", blocking, warnings) // the app did not stay up: the user's fix, its lines and the reason are the last build.log rows
			}
			if err := p.askImage(ctx, in, image.Image); err != nil { // off the deploy's path: its SBOM is taken after Live (design/PATCHING.md §3.1)
				return Result{}, err
			}
		}
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) { // the access lines, then the hold's: the last before the terminal state (contracts/events.md, build.log)
			if err := p.accessLines(c, in); err != nil {
				return struct{}{}, err
			}
			return struct{}{}, p.heldLine(c, in)
		}, dbos.WithStepName("held")); err != nil {
			return Result{}, err
		}
	}
	return p.finish(ctx, in, state, "", blocking, warnings)
}

// promote makes what the promoted package declares real in prod, then starts preview's image there —
// or a rollback review's ships — and records the promotion. A failure or a cancel before the container ends the run with prod
// untouched; a container that does not start leaves the one already running (the executor replaces
// on success). A patch approval's promotion, once live, watches production (watch).
func (p *Pipeline) promote(ctx dbos.Context, in Input) (Result, error) {
	res, err := p.ensureAll(ctx, in, "prod", nil) // its first ensure step reads run_by, and the "run by" step below answers that read (#2432)
	step, url := "resources", ""
	if err == nil && res.Cancelled {
		return p.interrupted(ctx, in, nil, 0, 0)
	}
	if err == nil && res.Failed == "" {
		var hand handOff
		step = "" // reading the org's setting, and a release the org's platform answers, are no step of Canyon's (contracts/routes.md)
		if hand, err = dbos.RunAsStep(ctx, func(c context.Context) (handOff, error) {
			if res.Org != nil { // the one read, which decided whether Canyon provisioned: never a second (#2432)
				return handOff{Org: *res.Org, Via: res.Via}, nil
			}
			return p.handOffOf(c) // a run its ensure step read nothing for
		}, dbos.WithStepName("run by")); err == nil && hand.Org {
			err = p.sendRelease(ctx, in, "prod", in.Promote.Image, hand.Via) // the org's platform runs prod: a release in place of /run, and the run waits on its report or its check-in (design/HAND-OFF.md §3, §3b)
			// A rollback's kept image gone, as /run's 404 says on Canyon's own cluster: no longer kept, its last line why.
			if errors.Is(err, errImageGone) && in.Promote.Rollback {
				if _, kerr := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
					return struct{}{}, p.keptGone(c, in, in.Promote.Image)
				}, dbos.WithStepName("kept gone")); kerr != nil {
					err = kerr
				}
			}
		} else if err == nil {
			step = "deploy"
			var began time.Time
			if began, err = p.began(ctx, in, "start"); err == nil {
				url, err = dbos.RunAsStep(ctx, func(c context.Context) (string, error) { return p.deploy(c, in, "prod", in.Promote.Image, built{}) }, dbos.WithStepName("deploy"))
				if ctx.Err() == nil && !errors.Is(err, dbos.ErrWorkflowCancelled) {
					err = cmpErr(err, p.ended(ctx, in, "start", began, err == nil && url != "" && url != notCleared))
				}
			}
		}
	}
	if err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)) {
		return Result{}, err
	}
	if url == notCleared || errors.Is(err, errNotCleared) { // the review its receipt accepted no longer clears it: production unchanged
		return p.interrupted(ctx, in, nil, 0, 0)
	}
	if err != nil && step != "" {
		return p.failed(ctx, in, step, err, 0, 0)
	}
	if err != nil || res.Failed != "" || step == "deploy" && url == "" { // a rollback's kept image gone: its last line says so, the owner's to act on
		return p.finish(ctx, in, "failed", "error", 0, 0)
	}
	if step == "deploy" { // a patch approval's promotion, recorded live, watches production as its patch watched preview (contracts/routes.md, Production approvals)
		watches, err := dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			var watches bool
			err := p.Pool.QueryRow(c, "SELECT "+watchingSQL+" FROM runs WHERE id = $1", in.Run).Scan(&watches)
			return watches, err
		}, dbos.WithStepName("watches"))
		if err != nil {
			return Result{}, err
		}
		if watches {
			return p.watch(ctx, in, 0, 0)
		}
	}
	return p.finish(ctx, in, "clean", "", 0, 0)
}

// cancelled is the check before a step (each ensure pass makes its own): true when a cancel was
// asked of the run, which then ends failed/interrupted through End, never mid-step. Before the
// deploy (deploy set) it marks the deploy begun, under the ledger lock the cancel takes, so a
// cancel either lands before it or is refused too late (contracts/routes.md). A held production is a
// cancel of a run that writes it, read under the same lock: one that started before the hold stops
// before its deploy (contracts/routes.md, The hold).
func (p *Pipeline) cancelled(ctx dbos.Context, in Input, deploy bool) (bool, error) {
	goOn, err := dbos.RunAsStep(ctx, func(c context.Context) (goOn bool, err error) {
		err = p.write(c, func(tx pgx.Tx) error {
			tag, err := tx.Exec(c, `UPDATE runs SET deploy_started_at = CASE WHEN $2 THEN coalesce(deploy_started_at, now()) ELSE deploy_started_at END
				WHERE id = $1 AND ended_at IS NULL AND cancel_requested_at IS NULL AND NOT ($3 AND EXISTS (SELECT 1 FROM prod_holds WHERE project_id = $4))`, in.Run, deploy, deploy && in.writes() == "prod", in.Project)
			goOn = tag.RowsAffected() == 1
			return err
		})
		return goOn, err
	}, dbos.WithStepName("cancelled"))
	return err == nil && !goOn, err
}

// notCleared is the deploy step's answer when the review no longer clears what the run ships: never a URL, so
// a replay of the step reads it back as what it was.
const notCleared = "canyon:not-cleared"

// errNotCleared is a release the org's platform was never handed, for the same reason.
var errNotCleared = errors.New("runs: the review no longer clears what this run ships")

// clears is the last check before a run starts a container or hands a release to the org's platform
// (contracts/routes.md, the approve rows; #2204), read live inside the step that starts it and before it
// does — so a replay or a recovery that has not started it yet reads it again, and never a journaled answer.
// A deploy run's own review clears preview (Cleared); a promotion's is the review its receipt accepted it
// under, recorded as it started (ReceiptRun), judged as the receipt judges it (Clears). A lift's start again
// promotes no review: the lift clears it (contracts/routes.md, The hold). Fail closed: no review recorded, no
// verdict, a receipt that cannot be read, or any error clears nothing.
func (p *Pipeline) clears(ctx context.Context, in Input) bool {
	var ok bool
	var err error
	switch {
	case in.Promote == nil:
		ok, err = Cleared(ctx, p.Pool, in.Project, in.Run)
	case in.Promote.Restart:
		return true
	case p.Clears != nil: // a promotion started before its review was recorded names none, and clears only what reads no review
		ok, err = p.Clears(ctx, in.Project, in.Promote.ReceiptRun)
	}
	if err != nil {
		log.Printf("runs: run %s (%s): its review could not be read again: %v", in.Run, in.Project, err)
	}
	return err == nil && ok
}

// prodMoved is whether the promote run a patch approval started would ship over a production that moved since the
// approval (contracts/routes.md, Production approvals, approving): production's row no longer the image the approval
// replaces, Canyon's own. Read live before the container, never a journaled answer; false for any other promotion.
// Fail closed: a read that fails is moved.
func (p *Pipeline) prodMoved(ctx context.Context, in Input) bool {
	var moved bool
	err := p.Pool.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = x.project_id AND d.env = 'prod' AND d.image = x.replaces_image AND d.run_by = 'canyon')
		FROM promotion_approvals x WHERE x.run_id = $1 AND x.kind = 'patch'`, in.Run).Scan(&moved)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		log.Printf("runs: run %s (%s): production's row could not be read again: %v", in.Run, in.Project, err)
		return true
	}
	return moved
}

// interrupted ends a run a cancel was asked of, or answers the check's error.
func (p *Pipeline) interrupted(ctx dbos.Context, in Input, err error, blocking, warnings int) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	return p.finish(ctx, in, "failed", "interrupted", blocking, warnings)
}

// A step that talks to the executor is bounded, so a stalled executor cannot pin a run forever:
// a build may legitimately take minutes, a container start seconds.
// scanCeiling is one tool's scan: the executor's deadline is at most 10 minutes once the tree is in, and
// a call past it did not finish (contracts/finding.md). A variable so a test can lower it.
var scanCeiling = 15 * time.Minute

const (
	buildCeiling = 30 * time.Minute
	runCeiling   = 11 * time.Minute // above the executor's worst /run: on Kubernetes 300 s for the pod, then up to 300 s to clean up a failed first run (contracts/executor.md) — the executor decides, this only catches a stalled one
	gateCeiling  = 10 * time.Minute // the package's checkout for review: a stalled one fails the run; the AI reviews keep their own time (gate.TimedOut)
)

// buildLogCap is how many build-log lines a run keeps on the ledger (canyonv2-b5r).
const buildLogCap = 2000

// gated is the gate step's checkpoint: the log, or the model's failure. The log is embedded, so a
// checkpoint an older binary journaled as a bare log reads back the same.
type gated struct {
	finding.Log
	Model bool `json:"model,omitempty"`
}

// built is the build step's checkpoint: the image by digest and the build digest of the values it was
// built with, or the executor's refusal, or its busy answer. A refusal is a result, not an error, so a
// replay after a crash finds the same answer. The reuse step answers one too, Reused.
type built struct {
	Image      string         `json:"image"`
	Digest     string         `json:"digest,omitempty"`     // contracts/routes.md, the deploys row: the deploy row keeps it
	Values     string         `json:"values,omitempty"`     // the values-only HMAC (design/PATCHING.md §5), kept wherever the digest is
	ValuesKey  int            `json:"values_key,omitempty"` // the key version Values was made under
	Builder    string         `json:"builder,omitempty"`    // GET /builder's answer the build asked before it: "" for a reused image, or an executor without it
	Reused     bool           `json:"reused,omitempty"`
	Failed     string         `json:"failed,omitempty"`
	Undetected bool           `json:"undetected,omitempty"` // no buildpack detected the package: Failed is the executor's message, nothing was compiled
	NoRoom     bool           `json:"no_room,omitempty"`    // the cluster had no room to run the build: Failed is the scheduler's message
	Busy       *executor.Busy `json:"busy,omitempty"`
	Ran        time.Duration  `json:"ran,omitempty"` // the executor's call, its running time alone
}

// archive streams the package's tree as a tar. Close it when done: a reader that stops early lets
// the archive's writer fail instead of blocking.
func (p *Pipeline) archive(ctx context.Context, in Input) (io.ReadCloser, error) {
	var commit string
	if err := p.Pool.QueryRow(ctx, "SELECT commit FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&commit); err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		_ = pw.CloseWithError(ingest.Archive(ctx, filepath.Join(p.Repos, in.Project+".git"), commit, pw))
	}()
	return pr, nil
}

// scanner is the actor of a run's own scans' attaches (contracts/finding.md, Canyon-run tools).
var scanner = events.Actor{Kind: "canyon", Subject: "scan", Display: "Canyon"}

// Why a tool could not check the package, in the contract's words (contracts/finding.md, Canyon-run
// tools): each not_finished answer's reason.
var notFinished = map[string]string{"timeout": "did not finish in time", "no_image": "this Canyon host has no image for it",
	"killed": "stopped before it finished", "not_sarif": "did not answer SARIF", "over_cap": "answered more than Canyon reads",
	"no_room": "the cluster had no room to run it", "db_missing": "its vulnerability database could not be downloaded", "db_stale": "its vulnerability database is over 5 days old",
	"rules_error": "could not load all of its rules"}

// downloading is the scan.waiting row's note: the tool's scan answered busy while the executor downloads
// its database (contracts/events.md).
const downloading = "downloading its vulnerability database — the first scan on this host"

// scanned is a scan step's checkpoint: the ids of the reports the gate reads, so a replay never scans
// again, and why the scan did not check the package when it did not; or the executor's busy answer.
type scanned struct {
	Reports    []int64        `json:"reports,omitempty"`
	Unfinished string         `json:"unfinished,omitempty"`
	NoRoom     bool           `json:"no_room,omitempty"` // Unfinished is the cluster's capacity, an admin's to fix
	Busy       *executor.Busy `json:"busy,omitempty"`
	Ran        time.Duration  `json:"ran,omitempty"` // the executor's call, its running time alone
}

// unfinished is a tool a run's scans could not check the package with, and why.
type unfinished struct {
	Tool, Why string
	NoRoom    bool // the cluster's capacity: its fix is an admin's
}

// scanAll scans the package with each tool whose rule carries run and is on, each its own task on the
// host's scan queue (runs/queue.go), and answers the ids of the reports the gate reads: those the run
// journaled at start, each replaced by what a scan of its (tool, category) attached (contracts/finding.md,
// Canyon-run tools), and each tool that could not check it, and why, in tool-name order. The tasks are
// queued in tool-name order under ids the run names, so a replay finds each again. A task waits its turn
// and never fails for waiting; a cancel asked of the run takes every one still waiting out of the queue:
// stop, with the check's error if it had one.
func (p *Pipeline) scanAll(ctx dbos.Context, in Input) (ids []int64, missed []unfinished, stop bool, err error) {
	pending := scannedTools(in.Policy)
	slices.SortFunc(pending, func(a, b policy.Rule) int { return strings.Compare(a.ID, b.ID) })
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
		return nil, nil, true, err
	}
	handles := make([]dbos.WorkflowHandle[scanned], len(pending))
	for i, rule := range pending {
		tool := strings.TrimPrefix(rule.ID, "sarif.")
		if handles[i], err = dbos.RunWorkflow(ctx, p.scanTask, scanJob{In: in, Rule: rule, Reports: in.Reports}, dbos.WithQueue(p.queues[queueOf(tool)]), dbos.WithWorkflowID(in.Run+"-scan-"+tool)); err != nil {
			return nil, nil, false, err
		}
	}
	tasks := make([]string, len(handles))
	for i, h := range handles {
		tasks[i] = h.GetWorkflowID()
	}
	if err := tasksEnded(ctx, tasks...); err != nil {
		return nil, nil, false, err
	}
	results := make([]scanned, len(handles))
	for i, h := range handles { // every task ended before any is read
		if results[i], err = h.GetResult(); err != nil {
			if stop, cerr := p.cancelled(ctx, in, false); cerr != nil || stop { // a cancel took the waiting ones out of the queue (Dequeue)
				return nil, nil, true, cerr
			}
			return nil, nil, false, err
		}
	}
	kept, added := in.Reports, []int64(nil)
	for i, res := range results {
		kept = slices.DeleteFunc(slices.Clone(kept), func(id int64) bool { return !slices.Contains(res.Reports, id) }) // its (tool, category)'s report, replaced
		added = append(added, slices.DeleteFunc(slices.Clone(res.Reports), func(id int64) bool { return slices.Contains(in.Reports, id) })...)
		if res.Unfinished != "" {
			missed = append(missed, unfinished{strings.TrimPrefix(pending[i].ID, "sarif."), res.Unfinished, res.NoRoom})
		}
	}
	slices.SortFunc(missed, func(a, b unfinished) int { return strings.Compare(a.Tool, b.Tool) })
	ids = slices.Concat(kept, added)
	slices.Sort(ids)
	return ids, missed, false, nil
}

// scan asks the executor to run the rule's tool over the package with its settings — opengrep's rules
// repository never among them: opengrep's rules part is the vendored sast-rules and the org's repo, which
// the server fetches — and attaches the SARIF it answers as Canyon: the answer is ids with that report
// in place of any of its (tool, category). A scan that did not finish, a rules repo that could not be
// fetched, or SARIF the attach refuses, attaches nothing: the answer is ids as they were, and why, in
// the contract's words or the refusal's.
func (p *Pipeline) scan(ctx context.Context, in Input, rule policy.Rule, ids []int64) (scanned, error) {
	tool, s := strings.TrimPrefix(rule.ID, "sarif."), rule.Settings
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, scanCeiling)
	defer cancel()
	if tool == "gitleaks" && p.Attach != nil { // in process, never the executor's (contracts/finding.md, Canyon-run tools)
		raw, err := p.gitleaks(ctx, in)
		if errors.Is(err, context.DeadlineExceeded) { // as an executor's tool at its deadline: could not check, which blocks
			return scanned{Reports: ids, Unfinished: notFinished["timeout"]}, nil
		}
		if err != nil {
			return scanned{}, err
		}
		return p.attached(ctx, in, tool, raw, nil, nil, Evidence{}, ids)
	}
	if p.Executor == nil || p.Attach == nil {
		return scanned{}, errors.New("runs: no executor configured: a run cannot scan with " + tool) // fails the run closed, never a panic in the step
	}
	var rules io.Reader // opengrep's rule files: the vendored sast-rules, and the org's repo when set; no other tool takes a part
	if tool == "opengrep" {
		var org map[string][]byte
		var err error
		if r := s.Rules; r != nil {
			var auth transport.AuthMethod
			if auth, err = p.credential(ctx, r.Repo); err != nil {
				return scanned{}, err
			}
			org, err = p.orgRules(ctx, r.Repo, r.Commit, auth)
			if err == nil && r.Paths != nil {
				org, err = under(org, r.Paths)
			}
		}
		if err == nil {
			rules, err = rulesTar(org)
		}
		var why unfetched
		if errors.As(err, &why) {
			return scanned{Reports: ids, Unfinished: why.Error()}, nil
		}
		if err != nil {
			return scanned{}, err
		}
	}
	tree, err := p.archive(ctx, in)
	if err != nil {
		return scanned{}, err
	}
	defer func() { _ = tree.Close() }()
	res, err := p.Executor.Scan(ctx, executor.ScanRequest{App: in.Project, Package: in.Package, Tool: tool,
		Settings: &executor.ScanSettings{Scanners: s.Scanners, Frameworks: s.Frameworks, SkipChecks: s.SkipChecks, SkipPaths: s.SkipPaths}}, tree, rules)
	var busy *executor.Busy
	switch {
	case errors.As(err, &busy):
		if busy.Reason == "database" { // the run says what it waits on, once per tool: a later ask's append is deduped
			if err := p.write(ctx, func(tx pgx.Tx) error {
				_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.ScanWaiting, "waiting:"+tool, map[string]any{"tool": tool, "note": downloading})
				return err
			}); err != nil {
				return scanned{}, err
			}
		}
		return scanned{Busy: busy}, nil
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil && parent.Err() == nil: // no answer in this step's own time: the tool did not finish, never Canyon failing (#1643)
		return scanned{Reports: ids, Unfinished: notFinished["timeout"]}, nil
	case err != nil:
		return scanned{}, err
	case res.NotFinished != "":
		why, ok := notFinished[res.NotFinished]
		if !ok {
			return scanned{}, fmt.Errorf("runs: %s answered not_finished %q, which the contract does not name", tool, res.NotFinished)
		}
		if said := schedulerSaid(res.Reason); res.NotFinished == "no_room" && said != "" { // the scheduler's own words
			why += " (" + said + ")"
		}
		return scanned{Reports: ids, Unfinished: why, NoRoom: res.NotFinished == "no_room"}, nil
	}
	return p.attached(ctx, in, tool, res.SARIF, res.Unloaded, res.Unparsed, Evidence{Image: res.Image, DBBuiltAt: res.DBBuiltAt}, ids)
}

// noRoom ends a run whose scans the cluster had no room for, before the review: each such tool's line, the
// scheduler's words in it, is a build.log row in tool-name order, then the run ends failed scan_no_room —
// capacity, never a finding to approve nor Canyon failing (contracts/finding.md, Canyon-run tools).
func (p *Pipeline) noRoom(ctx dbos.Context, in Input, missed []unfinished) (Result, error) {
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		return struct{}{}, p.write(c, func(tx pgx.Tx) error {
			for _, m := range missed {
				if !m.NoRoom {
					continue
				}
				if _, err := events.AppendRunTx(c, tx, in.Run, in.Project, events.BuildLog, "no room:"+m.Tool, map[string]any{"line": m.Tool + ": " + m.Why}); err != nil {
					return err
				}
			}
			return nil
		})
	}, dbos.WithStepName("no room")); err != nil {
		return Result{}, err
	}
	return p.finish(ctx, in, "failed", "scan_no_room", 0, 0)
}

// gitleaks is gate.Gitleaks' report over the package's tree, extracted as the gate reads it: no byte of
// it carries a secret, and it never leaves the step, the attach storing it. A panic over a tree is an
// error, as the gate's is: the step runs in the server's process, and a crash would replay into it.
func (p *Pipeline) gitleaks(ctx context.Context, in Input) (raw []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			raw, err = nil, fmt.Errorf("runs: gitleaks panicked: %v", r)
		}
	}()
	var commit string
	if err := p.Pool.QueryRow(ctx, "SELECT commit FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&commit); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "canyon-gitleaks-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := ingest.Extract(ctx, filepath.Join(p.Repos, in.Project+".git"), commit, dir); err != nil {
		return nil, err
	}
	tree, err := gate.Walk(dir)
	if err != nil {
		return nil, err
	}
	return gate.Gitleaks(ctx, tree)
}

// attached attaches tool's report as Canyon, with the rules it could not load and the files it did not
// read whole: ids with it in place of any of its (tool, category), or ids as they were and the attach's
// refusal.
func (p *Pipeline) attached(ctx context.Context, in Input, tool string, raw []byte, unloaded, unparsed []string, ev Evidence, ids []int64) (scanned, error) {
	named := make([]string, 0, len(unloaded))
	for _, id := range unloaded {
		named = append(named, ruleShown(id))
	}
	id, err := p.Attach(ctx, in.Project, in.Package, tool, raw, slices.Compact(slices.Sorted(slices.Values(named))), unparsed, ev, scanner)
	if why := refusal(err); why != "" {
		return scanned{Reports: ids, Unfinished: why}, nil
	}
	if err != nil {
		return scanned{}, fmt.Errorf("runs: attach %s: %w", tool, err)
	}
	rows, err := p.Pool.Query(ctx, "SELECT DISTINCT ON (tool, category) id FROM package_reports WHERE package_id = $1 AND id = ANY($2) ORDER BY tool, category, id DESC",
		in.Package, append(slices.Clone(ids), id))
	if err != nil {
		return scanned{}, err
	}
	ids, err = pgx.CollectRows(rows, pgx.RowTo[int64])
	return scanned{Reports: ids}, err
}

// ruleShown is a rule id a tool could not load as it is stored and shown: printable ASCII of at most 128
// characters with no live key; else "", a rule unnamed (contracts/finding.md, In a run).
func ruleShown(id string) string {
	if len(id) > 128 || finding.HasLiveKey([]byte(id)) || strings.ContainsFunc(id, func(r rune) bool { return r < '!' || r > '~' }) {
		return ""
	}
	return id
}

// refusal is the attach's message when err is its refusal of the report itself (routes.md, the reports
// row's 400s), else "".
func refusal(err error) string {
	var invalid sarif.Invalid
	var unknown sarif.UnknownTool
	var tooMany sarif.TooManyResults
	switch {
	case errors.As(err, &invalid):
		return invalid.Error()
	case errors.As(err, &unknown):
		return unknown.Error()
	case errors.As(err, &tooMany):
		return tooMany.Error()
	}
	return ""
}

// image is the image a deploy run runs — with reuse, the one the reuse step found, else the build's —
// and, done, the run's end when there is none.
func (p *Pipeline) image(ctx dbos.Context, in Input, reuse bool, blocking, warnings int) (b built, done bool, end Result, err error) {
	if reuse {
		b, err = dbos.RunAsStep(ctx, func(c context.Context) (built, error) { return p.reuse(c, in) }, dbos.WithStepName("reuse"))
	}
	var stop bool
	if err == nil && b.Image == "" {
		b, stop, err = p.buildAll(ctx, in)
	}
	switch {
	case stop:
		end, err = p.interrupted(ctx, in, err, blocking, warnings)
	case err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)):
	case err != nil:
		end, err = p.failed(ctx, in, "build", err, blocking, warnings) // Canyon's own failure: the executor unreachable, the archive, the ledger
	case b.Undetected:
		end, err = p.finish(ctx, in, "failed", "detect", blocking, warnings) // no buildpack detected the package: nothing was compiled, the run row names what the host builds
	case b.NoRoom:
		end, err = p.finish(ctx, in, "failed", "no_room", blocking, warnings) // the cluster's capacity, an admin's to add: the scheduler's message is the last build.log line (#1637)
	case b.Failed != "":
		end, err = p.finish(ctx, in, "failed", "build", blocking, warnings) // the package did not build: the user's fix, the message is the last build.log line
	default:
		return b, false, Result{}, nil
	}
	return b, true, end, err
}

// reuse is the reuse step (contracts/routes.md, the deploys row): the newest image of this project's
// package whose build digest is the one preview's build values and the executor's builder now give it
// (contracts/executor.md, GET /builder; none from an executor without it) — from a deploy row Canyon
// ran, or a run's build, whatever its start did after — and its build.log row; none, so the run builds,
// when none is. Each candidate's digest is computed again with its own image: a row whose image another
// writer changed never matches.
func (p *Pipeline) reuse(ctx context.Context, in Input) (built, error) {
	values, err := p.secrets(ctx, in.Project, "preview", "build")
	if err != nil {
		return built{}, err
	}
	if p.Executor == nil {
		return built{}, errors.New("runs: no executor configured: a deploy run cannot build")
	}
	builder, waited, err := p.Executor.Builder(ctx)
	if err == nil {
		err = p.prepared(ctx, in, waited)
	}
	if err != nil || builder == "" { // "": an executor from before GET /builder, so nothing is reused
		return built{}, err
	}
	rows, err := p.Pool.Query(ctx, `SELECT image, build_digest FROM (
			SELECT image, build_digest, deployed_at AS at FROM deploys WHERE project_id = $1 AND package_id = $2 AND run_by = 'canyon' AND build_digest IS NOT NULL
			UNION ALL SELECT build_image, build_digest, created_at FROM runs WHERE project_id = $1 AND package_id = $2 AND build_digest IS NOT NULL) c
		ORDER BY at DESC`, in.Project, in.Package)
	if err != nil {
		return built{}, err
	}
	var b built
	for rows.Next() && !b.Reused {
		var image, stored string
		if err := rows.Scan(&image, &stored); err != nil {
			rows.Close()
			return built{}, err
		}
		digest, err := p.Vault.BuildDigest(in.Project, image, builder, values)
		if err != nil {
			rows.Close()
			return built{}, err
		}
		if hmac.Equal([]byte(digest), []byte(stored)) {
			b = built{Image: image, Digest: digest, Reused: true}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || !b.Reused {
		return built{}, err
	}
	if b.Values, b.ValuesKey, err = p.Vault.ValuesHMAC(in.Project, values); err != nil { // a reused image's run makes it as a build does
		return built{}, err
	}
	version := strings.TrimPrefix(in.Package, "pkg_") // routes.md, Naming a version
	var label *string
	if err := p.Pool.QueryRow(ctx, "SELECT label FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&label); err != nil {
		return built{}, err
	}
	if label != nil {
		version = *label
	}
	return b, p.record(ctx, in, provisioners.Key{}, "", "", "", "", "reuse", "Reusing the image built for "+version+" (no build value changed)")
}

// prepared writes a deploy run's build.image row when its GET /builder waited for Canyon's images, inside the step
// that asked (contracts/events.md, build.image; #2711): once per run, under its dedupe, so the build step's ask after
// the reuse step's, or a step run again, writes nothing more. Never after the run's first build.log row: a build step
// resumed after an upgrade that changed an image waits after its first attempt's lines, which the build's own clock
// already spans, so the wait would count twice. A patch run's rows are its own (routes.md).
func (p *Pipeline) prepared(ctx context.Context, in Input, waited time.Duration) error {
	if waited <= 0 || in.Patch != nil {
		return nil
	}
	return p.write(ctx, func(tx pgx.Tx) error { // under the append lock, so no build.log row lands between the read and the write
		var logged bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND kind = 'build.log')", in.Run).Scan(&logged); err != nil || logged {
			return err
		}
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildImage, "build-image", map[string]int64{"waited_ms": waited.Milliseconds()})
		return err
	})
}

// BuilderRestarting is the build.log line a build the executor went away under reads while Canyon waits for
// the executor to come back and builds again (#2671): a restart, an upgrade or a node drain, never the
// package's build failing.
const BuilderRestarting = "Canyon is restarting its builder: the build starts again once it is back."

// builderBack waits for the executor to answer GET /builder again, every two seconds, for as long as ctx
// lets it: the build step's own deadline fails the run as before.
func (p *Pipeline) builderBack(ctx context.Context) error {
	for {
		if _, err := p.Executor.BuilderImage(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("runs: the executor did not come back: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// buildAll is the build, a task on the host's build queue (runs/queue.go): it waits its turn, never failed
// for waiting, and a cancel asked of the run while it waits takes it out of the queue: stop, with the
// check's error if it had one.
func (p *Pipeline) buildAll(ctx dbos.Context, in Input) (b built, stop bool, err error) {
	h, err := dbos.RunWorkflow(ctx, p.buildTask, in, dbos.WithQueue(p.queues[BuildQueue]), dbos.WithWorkflowID(in.Run+"-build"))
	if err != nil {
		return built{}, false, err
	}
	if err := tasksEnded(ctx, h.GetWorkflowID()); err != nil {
		return built{}, false, err
	}
	if b, err = h.GetResult(); err != nil {
		if stop, cerr := p.cancelled(ctx, in, false); cerr != nil || stop { // a cancel took it out of the queue (Dequeue)
			return built{}, true, cerr
		}
	}
	return b, false, err
}

// build streams the package's tree into the executor and each build-log line onto the ledger
// as it arrives. The image reference is the checkpoint: a replay after a successful build
// never rebuilds. Log lines carry no dedupe key on purpose — a replayed build is a second
// attempt and the ledger says so.
func (p *Pipeline) build(ctx context.Context, in Input) (built, error) {
	if p.Executor == nil {
		return built{}, errors.New("runs: no executor configured: a deploy run cannot build") // fails the run closed, never a panic in the step
	}
	ctx, cancel := context.WithTimeout(ctx, buildCeiling)
	defer cancel()
	pr, err := p.archive(ctx, in)
	if err != nil {
		return built{}, err
	}
	defer func() { _ = pr.Close() }()
	row := func(c context.Context, l string) error {
		_, err := events.AppendRun(c, p.Pool, in.Run, in.Project, events.BuildLog, map[string]any{"line": l})
		return err
	}
	// the ledger keeps buildLogCap lines and one row saying the rest was cut: a post-install
	// script printing for half an hour is not millions of rows, each its own transaction under
	// the append lock. The executor's failure message below is appended whatever the count.
	var lines int
	line := func(c context.Context, l string) error {
		if lines++; lines > buildLogCap {
			if lines > buildLogCap+1 {
				return nil
			}
			l = fmt.Sprintf("build log truncated after %d lines; the rest is in the executor's log", buildLogCap)
		}
		return row(c, l)
	}
	values, err := p.secrets(ctx, in.Project, "preview", "build")
	if err != nil {
		return built{}, err
	}
	builder, waited, err := p.Executor.Builder(ctx) // asked before the build: a builder replaced meanwhile only means a rebuild later
	if err == nil {
		err = p.prepared(ctx, in, waited)
	}
	if err != nil {
		return built{}, err
	}
	req := executor.BuildRequest{App: in.Project, Package: in.Package, BuildValues: values}
	res, err := p.Executor.Build(ctx, req, pr, line)
	var lost *executor.Gone
	if errors.As(err, &lost) { // the executor went away under the build (#2671): once it is back, the same build once more
		if err := row(ctx, BuilderRestarting); err != nil {
			return built{}, err
		}
		if err := p.builderBack(ctx); err != nil {
			return built{}, err
		}
		_ = pr.Close()
		if pr, err = p.archive(ctx, in); err != nil {
			return built{}, err
		}
		res, err = p.Executor.Build(ctx, req, pr, line) // lost again, it fails as any lost build did
	}
	var failed *executor.BuildFailed
	var undetected *executor.Undetected
	var noRoom *executor.NoRoom
	var busy *executor.Busy
	if errors.As(err, &busy) {
		return built{Busy: busy}, nil
	}
	if errors.As(err, &undetected) { // the run read names what the host builds, and whether a buildpack errored (contracts/routes.md)
		if _, err := p.Pool.Exec(ctx, "UPDATE runs SET builds = $2, detect_errored = $3 WHERE id = $1", in.Run, undetected.Builds, undetected.Errored); err != nil {
			return built{}, err
		}
		if err := row(ctx, undetected.Error()); err != nil {
			return built{}, err
		}
		return built{Failed: undetected.Error(), Undetected: true}, nil
	}
	if errors.As(err, &noRoom) {
		said := cmp.Or(schedulerSaid(noRoom.Reason), noRoom.Error()) // the scheduler's own words
		if err := row(ctx, said); err != nil {
			return built{}, err
		}
		return built{Failed: said, NoRoom: true}, nil
	}
	if errors.As(err, &failed) {
		if err := row(ctx, failed.Message); err != nil {
			return built{}, err
		}
		return built{Failed: failed.Message}, nil
	}
	var host *executor.HostFailed
	if errors.As(err, &host) { // the executor's host failed it, never the package (#2489): its line is the last build.log row, and the run ends error
		if err := row(ctx, host.Message); err != nil {
			return built{}, err
		}
		return built{}, host
	}
	if err != nil {
		return built{}, err
	}
	// the values this build was handed, the image they made and the buildpacks that ran, kept on the run
	// whatever its start does after, so a redeploy reuses it and every deploy row of the image names its
	// buildpacks (contracts/routes.md, the deploys row)
	digest := ""
	if builder != "" { // an executor from before GET /builder: no digest, so nothing is reused
		if digest, err = p.Vault.BuildDigest(in.Project, res.Image, builder, values); err != nil {
			return built{}, err
		}
	}
	keyed, key, err := p.Vault.ValuesHMAC(in.Project, values) // the values alone, an image and a builder aside (design/PATCHING.md §5)
	if err != nil {
		return built{}, err
	}
	var ran any // NULL from an executor that names none, never a jsonb null
	if res.Buildpacks != nil {
		ran = res.Buildpacks
	}
	if _, err := p.Pool.Exec(ctx, "UPDATE runs SET build_image = $2, build_digest = nullif($3, ''), buildpacks = $4, values_hmac = $5, values_key = $6, builder = nullif($7, '') WHERE id = $1",
		in.Run, res.Image, digest, ran, keyed, key, builder); err != nil {
		return built{}, err
	}
	return built{Image: res.Image, Digest: digest, Values: keyed, ValuesKey: key, Builder: builder}, nil
}

// ensured is the ensure step's checkpoint: a failed resource's message is a result, not an error,
// and so is the resource the driver answered pending, with the instant it was first pending.
type ensured struct {
	Org       *bool     `json:"org,omitempty"` // run_by is org, as the run read it once (#2432); nil in a pass the release before journaled
	Via       string    `json:"via,omitempty"` // delivery, read with it (design/HAND-OFF.md §3b)
	Failed    string    `json:"failed,omitempty"`
	Pending   string    `json:"pending,omitempty"`
	At        time.Time `json:"at,omitzero"`
	Cancelled bool      `json:"cancelled,omitempty"`
}

// pendingEvery is how long a run waits before asking again about a pending resource; a variable
// so a test need not wait it. A resource pending for resourceDeadline fails the run.
var pendingEvery = 10 * time.Second

const resourceDeadline = 30 * time.Minute

// ensureAll runs the ensure step until every resource is ready, one failed or a cancel was asked,
// sleeping durably while one is pending, then begins the deploy: its answer is ready to deploy,
// Failed or Cancelled. When a resource was first pending is a checkpointed answer, so a run that
// resumes after a crash waits against the same deadline, and a replay neither asks again nor sleeps again.
//
// org is the run's one read of run_by, or nil when the run has not read it yet (a promotion): the first ensure
// step reads it then, and every later pass and the caller go by that answer (ensured.Org). Under org the step
// provisions nothing — the org's platform does (#2432) — and the deploy is still begun, so a release is sent
// only past the boundary a /run is. Its steps are the release before's, name for name.
func (p *Pipeline) ensureAll(ctx dbos.Context, in Input, env string, org *bool) (ensured, error) {
	since := map[string]time.Time{}
	var via string // read with run_by when this call reads it: a promotion's
	for {
		res, err := dbos.RunAsStep(ctx, func(c context.Context) (ensured, error) { return p.ensure(c, in, env, since, org) }, dbos.WithStepName("ensure"))
		if err != nil || res.Failed != "" || res.Cancelled {
			return res, err
		}
		if org == nil {
			org, via = res.Org, res.Via
		}
		if res.Pending == "" {
			stop, err := p.cancelled(ctx, in, true)
			return ensured{Cancelled: stop, Org: org, Via: via}, err
		}
		since[res.Pending] = res.At
		if _, err := dbos.Sleep(ctx, pendingEvery); err != nil {
			return ensured{}, err
		}
	}
}

// provisioner is the actor of a provisioned secret row and its audit row, never the run's.
var provisioner = events.Actor{Kind: "canyon", Subject: "provisioner", Display: "Canyon"}

const ensureCeiling = 30 * time.Second // one driver call: a stalled driver cannot pin a run

// ensure makes each resource the deployed package declares real in env, in name order, before the
// container starts. Every declared type needs a driver in env before any Ensure runs, so a failed
// deploy has changed nothing. A ready record whose provisioned rows all exist is kept, no Ensure:
// a replay or a redeploy never changes the running app's credentials. Ensure runs only when
// the record is not ready or a row is missing, which is also the repair after a crash lost one.
// A pending answer ends the pass: the caller sleeps and asks again, until since[name] + resourceDeadline.
// A driver's error is transient (provisioners.Driver), so it ends the pass the same way, with no waiting
// line; one still erring at the deadline fails the run with it (#1013).
// A pass after a cancel was asked touches nothing: what earlier passes made stays, and the next
// deploy keeps it as it keeps any ready record.
func (p *Pipeline) ensure(ctx context.Context, in Input, env string, since map[string]time.Time, org *bool) (ensured, error) {
	var asked bool
	if err := p.Pool.QueryRow(ctx, "SELECT cancel_requested_at IS NOT NULL FROM runs WHERE id = $1", in.Run).Scan(&asked); err != nil || asked {
		return ensured{Cancelled: asked}, err
	}
	var via string
	if org == nil { // the run's one read of run_by and delivery, journaled with this step's answer (#2432, #2470)
		read, err := p.handOffOf(ctx)
		if err != nil {
			return ensured{}, err
		}
		org, via = &read.Org, read.Via
	}
	if *org { // the org's platform provisions what it runs: no driver is asked, no record written
		return ensured{Org: org, Via: via}, nil
	}
	res, err := p.provision(ctx, in, env, since)
	res.Org, res.Via = org, via
	return res, err
}

// provision is ensure's work on a host whose apps Canyon runs.
func (p *Pipeline) provision(ctx context.Context, in Input, env string, since map[string]time.Time) (ensured, error) {
	var declared map[string]string
	if err := p.Pool.QueryRow(ctx, "SELECT coalesce(resources, '{}') FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&declared); err != nil {
		return ensured{}, err
	}
	names, drivers := slices.Sorted(maps.Keys(declared)), map[string]provisioners.Driver{}
	for _, name := range names {
		typ := declared[name]
		d, err := p.Drivers.Select(typ, env)
		if err != nil {
			msg := "this host does not provision " + typ + " in " + env
			if p.Drivers == nil && typ == "postgres" {
				msg += ": set CANYON_APP_DATABASE_URL" // the setting that turns the built-in driver on
			}
			if err := p.record(ctx, in, provisioners.Key{}, typ, "", "", "", "creating:"+env+":"+name, typ+" "+name+": creating"); err != nil {
				return ensured{}, err
			}
			return ensured{Failed: msg}, p.record(ctx, in, provisioners.Key{}, typ, "", "", "", "", typ+" "+name+": failed: "+msg)
		}
		drivers[typ] = d
	}
	for _, name := range names {
		typ, key := declared[name], provisioners.Key{Project: in.Project, Env: env, Name: name}
		label := typ + " " + name
		var kept bool
		if err := p.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM resources WHERE project_id = $1 AND env = $2 AND name = $3 AND state = 'ready')
			AND (SELECT count(*) FROM secrets WHERE project_id = $1 AND env = $2 AND origin = 'provisioned' AND name = ANY($4)) = cardinality($4::text[])`,
			key.Project, key.Env, key.Name, provisioners.Promised(typ)).Scan(&kept); err != nil {
			return ensured{}, err
		}
		if kept {
			if err := p.record(ctx, in, key, typ, "", "", "", "ready:"+env+":"+name, label+": ready"); err != nil {
				return ensured{}, err
			}
			continue
		}
		if err := p.record(ctx, in, key, typ, drivers[typ].Name(), "pending", "", "creating:"+env+":"+name, label+": creating"); errors.Is(err, errDeleting) {
			msg := label + " is being deleted"
			return ensured{Failed: msg}, p.record(ctx, in, key, typ, "", "", "", "", label+": failed: "+msg)
		} else if err != nil {
			return ensured{}, err
		}
		call, cancel := context.WithTimeout(ctx, ensureCeiling)
		res, err := drivers[typ].Ensure(call, key, typ)
		cancel()
		first, waited := since[name]
		var msg string
		if err != nil && (!waited || time.Since(first) < resourceDeadline) { // transient (provisioners.Driver): asked again, as pending is, with no waiting line
			log.Printf("runs: %s %s/%s: %v", label, in.Project, env, err)
			return ensured{Pending: name, At: cmp.Or(first, time.Now())}, nil
		} else if err != nil {
			msg = err.Error()
		} else if res.State == provisioners.Pending && waited && time.Since(first) >= resourceDeadline {
			msg = fmt.Sprintf("not ready after %d minutes", int(resourceDeadline.Minutes()))
		} else if res.State == provisioners.Pending {
			return ensured{Pending: name, At: cmp.Or(first, time.Now())}, p.record(ctx, in, key, typ, "", "", "", "waiting:"+env+":"+name, label+": waiting on the platform")
		} else if res.State != provisioners.Ready {
			msg = cmp.Or(res.Message, "the driver answered "+string(res.State))
		} else if msg, err = p.ready(ctx, in, key, typ, drivers[typ].Name(), res.Outputs, "ready:"+env+":"+name); err != nil {
			return ensured{}, err
		}
		if msg != "" {
			return ensured{Failed: msg}, p.record(ctx, in, key, typ, drivers[typ].Name(), "failed", msg, "", label+": failed: "+msg)
		}
	}
	return ensured{}, nil
}

// record writes the resource's record in state with message, made by driver (no state: no record), and its line
// on the run's timeline, once per run when dedupe is set.
func (p *Pipeline) record(ctx context.Context, in Input, key provisioners.Key, typ, driver, state, message, dedupe, line string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		if state != "" {
			if err := upsertResource(ctx, tx, key, typ, driver, state, message); err != nil {
				return err
			}
		}
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, dedupe, map[string]any{"line": line})
		return err
	})
}

// errDeleting refuses a record a project delete marked deleting: ensure never flips it back.
var errDeleting = errors.New("the resource is being deleted")

func upsertResource(ctx context.Context, tx pgx.Tx, key provisioners.Key, typ, driver, state, message string) error {
	tag, err := tx.Exec(ctx, `INSERT INTO resources (project_id, env, name, type, driver, state, message) VALUES ($1, $2, $3, $4, $7, $5, $6)
		ON CONFLICT (project_id, env, name) DO UPDATE SET type = EXCLUDED.type, driver = EXCLUDED.driver, state = EXCLUDED.state, message = EXCLUDED.message, updated_at = now()
		WHERE resources.state <> 'deleting'`,
		key.Project, key.Env, key.Name, typ, state, message, driver)
	if err == nil && tag.RowsAffected() == 0 {
		return errDeleting
	}
	return err
}

// ready seals outputs as provisioned runtime rows, marks the record ready, writes the audit row and
// the once-per-run ready line in one transaction; an output name a person's row holds writes nothing.
func (p *Pipeline) ready(ctx context.Context, in Input, key provisioners.Key, typ, driver string, outputs map[string]string, done string) (refused string, err error) {
	names := slices.Sorted(maps.Keys(outputs))
	err = p.write(ctx, func(tx pgx.Tx) error {
		if refused, err = sealOutputs(ctx, tx, p.Vault, key, typ, outputs); err != nil {
			return err
		}
		if err := upsertResource(ctx, tx, key, typ, driver, "ready", ""); err != nil {
			return err
		}
		if err := events.AuditTx(ctx, tx, provisioner, "ensure resource", in.Project, in.Run, map[string]any{"name": key.Name, "type": typ, "env": key.Env, "outputs": names}); err != nil {
			return err
		}
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, done, map[string]any{"line": typ + " " + key.Name + ": ready"})
		return err
	})
	if refused != "" {
		return refused, nil
	}
	return "", err
}

// sealOutputs seals a driver's outputs as the key's provisioned runtime rows, each row's version moved on; an
// output name a person's row holds is refused, named, and the transaction is to roll back.
func sealOutputs(ctx context.Context, tx pgx.Tx, keys *vault.Vault, key provisioners.Key, typ string, outputs map[string]string) (refused string, err error) {
	for _, name := range slices.Sorted(maps.Keys(outputs)) {
		keyRef, sealedKey, sealedValue, err := keys.Seal(key.Project, key.Env, name, []byte(outputs[name]))
		if err != nil {
			return "", err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO secrets (project_id, env, name, scope, key_ref, sealed_key, sealed_value, set_by_kind, set_by_subject, set_by_display, origin)
			VALUES ($1, $2, $3, 'runtime', $4, $5, $6, $7, $8, $9, 'provisioned')
			ON CONFLICT (project_id, env, name) DO UPDATE SET version = secrets.version + 1, key_ref = EXCLUDED.key_ref, sealed_key = EXCLUDED.sealed_key, sealed_value = EXCLUDED.sealed_value, set_at = now()
			WHERE secrets.origin = 'provisioned'`,
			key.Project, key.Env, name, keyRef, sealedKey, sealedValue, provisioner.Kind, provisioner.Subject, provisioner.Display)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			refused = name + " is set by hand; remove it with canyon secret rm " + name + " so Canyon can provide " + typ + " " + key.Name
			return refused, errors.New(refused) // rolls back: nothing of this answer is written
		}
	}
	return "", nil
}

// deploy runs the image in env and records it once: the environment, the
// deploy.completed row and the deploy's own audit row naming who triggered the run commit
// together (contracts/routes.md row 62). A replay finds the row first and answers its URL: the
// container is not replaced (replacing it is destructive) and nothing is written. In prod it is
// the promotion: the row repeats preview's, promoted_by the run's actor, and the timeline says so.
// imageBuildpacks is the SQL for the buildpacks recorded for the build of the image the parameter image
// names, of the project the parameter project names: the newest run that built it, else a deploy row that
// carries them — a reused or promoted image's travel with it (contracts/routes.md, the deploys row). NULL
// when no build of the image recorded any.
func imageBuildpacks(project, image string) string {
	return imageRecorded("buildpacks", project, image)
}

// imageBuilder is the same for the builder the image's build asked GET /builder for (contracts/routes.md, the deploys
// row): it travels with the image as its buildpacks do. NULL when no build of the image recorded one.
func imageBuilder(project, image string) string {
	return imageRecorded("builder", project, image)
}

// imageRecorded is the SQL for what the build of the image recorded in column, as imageBuildpacks says.
func imageRecorded(column, project, image string) string {
	return `coalesce((SELECT ` + column + ` FROM runs WHERE project_id = ` + project + ` AND build_image = ` + image + ` AND ` + column + ` IS NOT NULL ORDER BY created_at DESC LIMIT 1),
	(SELECT ` + column + ` FROM deploys WHERE project_id = ` + project + ` AND image = ` + image + ` AND ` + column + ` IS NOT NULL LIMIT 1))`
}

// ImageBuildpacks is what the build of image ran, nil when none was recorded: a rollback's receipt reads
// the image it ships (contracts/routes.md, The promote receipt).
func ImageBuildpacks(ctx context.Context, q querier, project, image string) ([]executor.Buildpack, error) {
	var ran []executor.Buildpack
	err := q.QueryRow(ctx, "SELECT "+imageBuildpacks("$1", "$2"), project, image).Scan(&ran)
	return ran, err
}

func (p *Pipeline) deploy(ctx context.Context, in Input, env, image string, b built) (string, error) {
	if p.Executor == nil { // a promotion starts prod with no build before it to refuse first (#728)
		return "", errors.New("runs: no executor configured: a deploy run cannot start the app")
	}
	var url string
	err := p.Pool.QueryRow(ctx, "SELECT data->>'url' FROM timeline_rows WHERE run_id = $1 AND dedupe = $2", in.Run, "deploy:"+env).Scan(&url)
	if err == nil {
		return url, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// everything the record needs is checked before the container is touched: a bad public URL or
	// image reference must not leave a running, unrecorded preview
	_, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || len(digest) != 64 {
		return "", fmt.Errorf("runs: image %q is not a digest reference", image)
	}
	digest = "sha256:" + digest
	line := func(dedupe, l string) error { return p.record(ctx, in, provisioners.Key{}, "", "", "", "", dedupe, l) }
	var ran *asked.Answer // what the app answered, on Kubernetes /run's
	var carried *Carried  // what this attempt's /run carried; none known when a retry finds its version running
	var retry bool        // an attempt of this step reached the container: a crash may have left it started, unrecorded
	var spec Egress       // the spec this attempt's /run carries
	specOf := false       // spec is this attempt's, read before its deploying:prod row
	if env == "prod" {
		if err := p.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'deploying:prod')", in.Run).Scan(&retry); err != nil {
			return "", err
		}
		data := map[string]any{"line": "deploying to production"}
		if !retry { // the door this attempt runs with, kept for a retry even if an admin changes the front door meanwhile (contracts/executor.md, The front door: a running app keeps the front door it was deployed with)
			door, _, ferr := p.frontDoor(ctx, false)
			if ferr != nil { // no deploying:prod row was written yet: without a failure line here, the run ends with neither it nor an opening line
				return "", errors.Join(ferr, line("", "deploying to production: failed: "+executorSaid(ferr)+"; the version running there is unchanged"))
			}
			if door != nil {
				data["domain"], data["form"] = door["domain"], door["form"]
			}
			if spec, err = p.egressFor(ctx, in, env); err != nil {
				return "", errors.Join(err, line("", "deploying to production: failed: "+executorSaid(err)+"; the version running there is unchanged"))
			}
			specOf = true
		}
		if err := p.write(ctx, func(tx pgx.Tx) error {
			if specOf { // the spec this attempt starts prod under, kept for a retry that finds it running (design/EGRESS.md §2.2)
				if _, err := tx.Exec(ctx, "UPDATE runs SET egress = $2 WHERE id = $1", in.Run, spec); err != nil {
					return err
				}
			}
			_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "deploying:prod", data)
			return err
		}); err != nil {
			return "", err
		}
	}
	if retry {
		running, err := p.Executor.Running(ctx, in.Project, env)
		if err != nil && !errors.Is(err, executor.ErrNoContainer) { // it cannot tell: a guess of "none" would start prod twice
			return "", errors.Join(err, line("", "deploying to production: failed: Canyon could not tell whether this version is already running; nothing was started"))
		}
		retry = running == image
	}
	ranWith := &spec // the spec the container the record names runs under
	if !specOf {
		spec, err = p.egressFor(ctx, in, env)
	} else {
		err = nil
	}
	switch {
	case err != nil:
	case retry: // that start is up: record it, never replace it — under the spec its attempt kept, else none known
		ranWith = nil
		if p.Access == nil { // on Kubernetes none is: the attempt may have crashed before its /run, so its pod's policy is not known to be the floor (fail closed, #2286)
			err = p.Pool.QueryRow(ctx, "SELECT egress FROM runs WHERE id = $1", in.Run).Scan(&ranWith)
		}
		if err != nil {
			break
		}
		var domain, form *string // both nullable: a Docker-era row (p.Access nil, below) carried neither, and form alone is optional (appURL's default)
		if err = p.Pool.QueryRow(ctx, "SELECT data->>'domain', data->>'form' FROM timeline_rows WHERE run_id = $1 AND dedupe = 'deploying:prod'", in.Run).Scan(&domain, &form); err == nil {
			var door map[string]any
			if domain != nil {
				door = map[string]any{"domain": *domain}
				if form != nil {
					door["form"] = *form
				}
			}
			url, err = p.appURL(door, in.Project, env)
		}
	case !p.clears(ctx, in): // the last check, before the container: never a journaled answer
		if env == "prod" {
			return notCleared, line("", "deploying to production: failed: its review no longer clears it, an approval expired or withdrawn; the version running there is unchanged")
		}
		return notCleared, nil
	case env == "prod" && p.prodMoved(ctx, in): // a patch approval's promotion never ships over a production that moved since
		return notCleared, line("", "deploying to production: failed: production runs another version now: Canyon asks again if the patch still applies; the version running there is unchanged")
	default:
		var c Carried
		url, ran, c, err = p.RunApp(ctx, in.Run, in.Project, env, image, spec, "") // a first attempt always starts: prod's values may have changed
		carried = &c
	}
	if err != nil && env == "prod" && in.Promote.Rollback && errors.Is(err, executor.ErrNoImage) { // no URL: the run fails on its line, production unchanged
		return "", p.keptGone(ctx, in, image)
	}
	if err != nil {
		if env == "prod" {
			return "", errors.Join(err, line("", "deploying to production: failed: "+executorSaid(err)+"; the version running there is unchanged"))
		}
		return "", err
	}
	var answered *asked.Answer // asked of a container this step started, never of one a retry found up
	if !retry {
		answered = p.answered(ctx, in.Project, env, url, ran)
	}
	return url, p.write(ctx, func(tx pgx.Tx) error {
		var recorded bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = $2)", in.Run, "deploy:"+env).Scan(&recorded); err != nil || recorded {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE runs SET egress = $2 WHERE id = $1", in.Run, ranWith); err != nil { // what a promotion of this package carries
			return err
		}
		if !retry && p.Access == nil { // this attempt's /run: Docker's executor started it on a network of its spec alone; Kubernetes has none
			if err := RecordNetworkTx(ctx, tx, in.Project, spec.Via == "canyon"); err != nil {
				return err
			}
		}
		d := Deployed{Project: in.Project, Env: env, Package: in.Package, Image: image, URL: url, GateRun: in.Run, Run: in.Run, DeployedBy: By(in.Actor), RunBy: "canyon", BuildDigest: b.Digest, ValuesHMAC: b.Values, ValuesKey: b.ValuesKey, Answered: answered, Egress: ranWith, Carried: carried}
		if env == "prod" {
			return recordProd(ctx, tx, in, d, digest)
		}
		var stopping bool // an undeploy asked the run to stop (contracts/routes.md, The brake): the row says what runs, and nothing says it went live
		if err := tx.QueryRow(ctx, "SELECT stop_requested_at IS NOT NULL FROM runs WHERE id = $1", in.Run).Scan(&stopping); err != nil {
			return err
		}
		if err := upsertDeployTx(ctx, tx, d); err != nil || stopping {
			return err
		}
		data := map[string]any{"env": env, "url": url, "package": in.Package, "image_digest": digest}
		if answered != nil {
			data["answered"] = answered
		}
		if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.DeployCompleted, "deploy:"+env, data); err != nil {
			return err
		}
		return events.AuditTx(ctx, tx, in.Actor, "deploy "+env, in.Project, in.Run, map[string]any{"package": in.Package, "image_digest": digest, "url": url})
	})
}

// recordProd records a promotion live through the shared production writer — preview's run and deployer,
// null on a row written before the columns, or the rollback review's, and who promoted — then its live
// line and its deploy prod row (contracts/routes.md, the promotions row), patch prod in its place when the
// review it ships under is a patch run (Patch runs). The promote run a patch approval started first keeps
// production's row as it found it, which its watch's revert puts back (contracts/routes.md, Production approvals):
// only the row the approval replaces, Canyon's own, as a patch run keeps preview's. Another keeps none, and a
// revert then fails closed on it, never /running an image Canyon did not record.
func recordProd(ctx context.Context, tx pgx.Tx, in Input, d Deployed, digest string) error {
	d.GateRun, d.DeployedBy, d.PromotedBy = in.Promote.GateRun, [3]*string{}, d.DeployedBy
	if a := in.Promote.DeployedBy; a.Kind != "" {
		d.DeployedBy = By(a)
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET patch_replaced = (SELECT to_jsonb(d) FROM deploys d JOIN promotion_approvals x ON x.run_id = $1 AND x.kind = 'patch'
			WHERE d.project_id = $2 AND d.env = 'prod' AND d.image = x.replaces_image AND d.run_by = 'canyon')
		WHERE id = $1 AND patch_replaced IS NULL AND EXISTS (SELECT 1 FROM promotion_approvals WHERE run_id = $1 AND kind = 'patch')`, in.Run, in.Project); err != nil {
		return err
	}
	replaces, live, err := WriteProdTx(ctx, tx, d)
	if err != nil || !live {
		return err
	}
	if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "live:prod", map[string]any{"line": "live at " + d.URL}); err != nil {
		return err
	}
	detail := map[string]any{"package": in.Package, "image_digest": digest, "url": d.URL, "replaces": nil, "rollback": in.Promote.Rollback}
	if replaces != "" {
		detail["replaces"] = replaces
	}
	if in.Promote.Reason != "" {
		detail["reason"] = in.Promote.Reason
	}
	if in.Promote.Restart {
		detail["restart"] = true
	}
	action := "deploy prod"
	var patched bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1 AND kind = 'patch')", d.GateRun).Scan(&patched); err != nil {
		return err
	}
	if patched {
		action = "patch prod"
	}
	if in.Promote.Rollback { // a person's rollback to an image a patch made: still deploy prod, saying so (#2390)
		var made bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM deploy_history WHERE project_id = $1 AND package_id = $2 AND image = $3 AND patched)", in.Project, in.Package, d.Image).Scan(&made); err != nil {
			return err
		}
		if made {
			detail["patched"] = true
		}
	}
	return events.AuditTx(ctx, tx, in.Actor, action, in.Project, in.Run, detail)
}

// executorSaid is a deploy failure as its line says it (contracts/events.md, build.log, #2539): the executor's own words when
// it answered some, never the server's wording around them, else the failure as Canyon knows it.
func executorSaid(err error) string {
	var a *executor.Answered
	if errors.As(err, &a) && a.Message != "" {
		return a.Message
	}
	return err.Error()
}

// deployStep runs deployPreview as the deploy step. A reused image's is its own step, whose answer says
// whether /run found the image gone (contracts/executor.md, /run's 404) or refused it (400): a result,
// not an error, so a replay reads it back and the run builds, once.
func (p *Pipeline) deployStep(ctx dbos.Context, in Input, b built) (url string, gone bool, err error) {
	if !b.Reused {
		url, err = dbos.RunAsStep(ctx, func(c context.Context) (string, error) { return p.deployPreview(c, in, b) }, dbos.WithStepName("deploy"))
		return url, false, err
	}
	r, err := dbos.RunAsStep(ctx, func(c context.Context) (reusedRun, error) {
		url, err := p.deployPreview(c, in, b)
		if errors.Is(err, executor.ErrNoImage) || errors.Is(err, executor.ErrRequest) { // gone, or refused: an executor from before the 404, a registry or a host that changed
			return reusedRun{Gone: true}, nil
		}
		return reusedRun{URL: url}, err
	}, dbos.WithStepName("deploy reused"))
	return r.URL, r.Gone, err
}

// reusedRun is the deploy step's checkpoint for a reused image: the URL, or the image gone.
type reusedRun struct {
	URL  string `json:"url"`
	Gone bool   `json:"gone,omitempty"`
}

// deployPreview is a deploy run's deploy step: the live URL, or none when the app did not stay up,
// once its last lines and then the executor's reason are build.log rows (contracts/events.md); any other
// failure but a reused image's gone or refused, which deployStep builds once, ends on its own row (#2539) —
// written here, so the lines are on the ledger and never in the step's journaled answer, which stays
// the string it always was, so a step journaled before this answer existed still reads back. The rows
// go in one transaction, each under its own dedupe key, so a replayed step writes none of them twice.
func (p *Pipeline) deployPreview(ctx context.Context, in Input, b built) (string, error) {
	url, err := p.deploy(ctx, in, "preview", b.Image, b)
	var exited *executor.Exited
	switch {
	case errors.As(err, &exited):
		return "", p.exitedLines(ctx, in, exited)
	case err == nil, b.Reused && (errors.Is(err, executor.ErrNoImage) || errors.Is(err, executor.ErrRequest)):
		return url, err
	}
	// nothing runs there: no tail, as production's says the version running there is unchanged
	return "", errors.Join(err, p.record(ctx, in, provisioners.Key{}, "", "", "", "", "failed:preview", "deploying to preview: failed: "+executorSaid(err)))
}

// secrets opens one environment's rows of one scope (contracts/routes.md, the secrets rows:
// where = build reaches the build, where = runtime the container, never the other way) inside
// the step that hands them on, so no value is a step's input or output and the journal keeps
// none. A run without a vault fails closed; a row that does not open names itself, never a value.
func (p *Pipeline) secrets(ctx context.Context, project, env, scope string) (map[string]string, error) {
	values, _, err := p.versioned(ctx, project, env, scope)
	return values, err
}

// versioned is secrets with each one's version and set time, read in the same query as its value: a
// run-config's (contracts/routes.md, Patch runs, step 5) are those of the values a /run was handed.
func (p *Pipeline) versioned(ctx context.Context, project, env, scope string) (map[string]string, map[string]Secret, error) {
	if p.Vault == nil {
		return nil, nil, errors.New("runs: no vault configured: a deploy run cannot read secrets") // fails the run closed, never a panic in the step
	}
	rows, err := p.Pool.Query(ctx, "SELECT name, version, set_at, coalesce(key_ref, 1), sealed_key, sealed_value FROM secrets WHERE project_id = $1 AND env = $2 AND scope = $3 ORDER BY name", project, env, scope) // key_ref NULL is version 1: every row written before the label
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out, versions := map[string]string{}, map[string]Secret{}
	for rows.Next() {
		var name string
		var version, keyRef int
		var setAt time.Time
		var key, sealed []byte
		if err := rows.Scan(&name, &version, &setAt, &keyRef, &key, &sealed); err != nil {
			return nil, nil, err
		}
		value, err := p.Vault.Open(keyRef, project, env, name, key, sealed)
		if err != nil {
			return nil, nil, err
		}
		out[name], versions[name] = string(value), Secret{Version: version, SetAt: setAt.UTC()}
	}
	return out, versions, rows.Err()
}

// Masked is line, the app's last line in env as POST /running read it, raw, masked with that
// environment's runtime values by /run's rule (contracts/routes.md, envs.<env>.stopped). It is "" when a
// secret of env was set or removed at or after the start of the run that started what runs there, or
// the row was written before that start was recorded: the app may have been handed other values than
// these, and a line is never masked with the wrong ones.
func (p *Pipeline) Masked(ctx context.Context, app, env, line string) (string, error) {
	if changed, err := p.maskChanged(ctx, app, env, false); err != nil || changed {
		return "", err
	}
	values, err := p.secrets(ctx, app, env, "runtime")
	if err != nil {
		return "", err
	}
	return executor.Mask(line, values), nil
}

// maskChanged says Masked's rule can't mask env's log: no recorded start of what runs there, or a secret of env set or
// removed since it started. With build, a build value of preview's set or removed since the image was built too (#223's
// security review): the start of the first run whose build is that image, else the deploy's gate run, and none is a
// change, since an image whose build no run records can't be told apart from one built with other values.
func (p *Pipeline) maskChanged(ctx context.Context, app, env string, build bool) (bool, error) {
	var changed bool
	err := p.Pool.QueryRow(ctx, `WITH d AS (SELECT run_started_at AS since,
			coalesce((SELECT min(r.created_at) FROM runs r WHERE r.project_id = $1 AND r.build_image = deploys.image), (SELECT r.created_at FROM runs r WHERE r.id = deploys.gate_run)) AS built
			FROM deploys WHERE project_id = $1 AND env = $2 AND run_started_at IS NOT NULL)
		SELECT NOT EXISTS (SELECT 1 FROM d)
			OR EXISTS (SELECT 1 FROM secrets, d WHERE project_id = $1 AND env = $2 AND set_at >= d.since)
			OR EXISTS (SELECT 1 FROM audit_events, d WHERE project_id = $1 AND action = 'DELETE /api/v1/projects/{id}/secrets/{name}' AND detail->>'env' = $2 AND ts >= d.since)
			OR ($3 AND (EXISTS (SELECT 1 FROM d WHERE built IS NULL)
				OR EXISTS (SELECT 1 FROM secrets, d WHERE project_id = $1 AND env = 'preview' AND scope = 'build' AND set_at >= d.built)
				OR EXISTS (SELECT 1 FROM audit_events, d WHERE project_id = $1 AND action = 'DELETE /api/v1/projects/{id}/secrets/{name}' AND detail->>'env' = 'preview' AND ts >= d.built)))`,
		app, env, build).Scan(&changed)
	return changed, err
}

// LogValues is what a masked log read masks with (contracts/routes.md, the logs row): env's runtime values and
// preview's build values together, preview having built the image, and the only environment that holds any; a build
// value under a runtime value's name, but another, stands as "<NAME> (build)". ok is false where Masked's rule can't
// mask it, a build value of preview's changed since the image was built counted too (maskChanged).
func (p *Pipeline) LogValues(ctx context.Context, app, env string) (values map[string]string, ok bool, err error) {
	if changed, err := p.maskChanged(ctx, app, env, true); err != nil || changed {
		return nil, false, err
	}
	if values, err = p.secrets(ctx, app, env, "runtime"); err != nil {
		return nil, false, err
	}
	build, err := p.secrets(ctx, app, "preview", "build")
	if err != nil {
		return nil, false, err
	}
	for name, v := range build {
		if was, taken := values[name]; taken && was != v {
			name += " (build)" // the runtime value keeps the name; a build one has no other name to take
		}
		values[name] = v
	}
	return values, true, nil
}

// Egress is the egress spec a run used, recorded on its deploy row (design/EGRESS.md §2.5): Via is off or
// canyon.
type Egress struct {
	Via string `json:"via"`
}

// egressFor is the egress spec env's /run carries (design/EGRESS.md §2.5). Preview's, any deploy there — a
// redeploy of the same package included — is the host's now. Prod's, a promotion's or a rollback's, is the
// spec recorded for it — the deploy run that started its package in preview, preview being the canary, or
// production's history for a rollback — or the host's now where none was recorded: never via off by
// omission (fail closed). A recorded via this server does not know starts nothing. Onto an app whose network
// is internal (NetworkInternal) every /run carries canyon, a recorded off and the host's off included: the
// network wins, and only an admin's Recreate networks moves an app off the floor (§2.5, the owner's decision, #2254).
// On Kubernetes, which has no app network, the host's canyon wins over a recorded off instead (A′, #2286).
func (p *Pipeline) egressFor(ctx context.Context, in Input, env string) (Egress, error) {
	var recorded *Egress
	if env == "prod" {
		q, args := "SELECT egress FROM runs WHERE id = $1", []any{in.Promote.GateRun}
		if in.Promote.Rollback { // what production ran it under, as its history recorded
			q, args = "SELECT egress FROM deploy_history WHERE project_id = $1 AND image = $2 ORDER BY deployed_at DESC, id DESC LIMIT 1", []any{in.Project, in.Promote.Image}
		}
		if err := p.Pool.QueryRow(ctx, q, args...).Scan(&recorded); err != nil {
			return Egress{}, err
		}
	}
	var spec Egress
	switch {
	case recorded == nil || recorded.Via == "":
		if err := p.Pool.QueryRow(ctx, "SELECT via FROM egress_settings").Scan(&spec.Via); err != nil {
			return Egress{}, err
		}
	case recorded.Via != "off" && recorded.Via != "canyon":
		return Egress{}, fmt.Errorf("runs: a recorded egress via %q this server does not know", recorded.Via)
	default:
		spec = *recorded
	}
	if spec.Via == "canyon" {
		return spec, nil
	}
	if p.Access != nil { // a recorded off gives way to the host's canyon; with none recorded spec is the host's already
		if recorded == nil || recorded.Via == "" {
			return spec, nil
		}
		err := p.Pool.QueryRow(ctx, "SELECT via FROM egress_settings").Scan(&spec.Via)
		return spec, err
	}
	err := p.Pool.QueryRow(ctx, "SELECT CASE WHEN "+NetworkInternal("$1")+" THEN 'canyon' ELSE 'off' END", in.Project).Scan(&spec.Via)
	return spec, err
}

// NetworkInternal is the SQL for whether the network of the app the parameter project names is internal
// (design/EGRESS.md §2.2): while Canyon runs an env of it — the executor removes the network with the app's
// last container — as its app_networks row records it, and nothing else: an app with no row, or one recorded
// unknown, is not internal (fail closed: never a floor enforced on a network no record vouches for).
func NetworkInternal(project string) string {
	return `(EXISTS (SELECT 1 FROM deploys WHERE project_id = ` + project + ` AND run_by = 'canyon') AND coalesce((SELECT internal FROM app_networks WHERE project_id = ` + project + `), false))`
}

// NetworkUnknown is the SQL for whether the network of the app the parameter project names is recorded
// unknown: a recreation under way, or one the executor failed.
func NetworkUnknown(project string) string {
	return `EXISTS (SELECT 1 FROM app_networks WHERE project_id = ` + project + ` AND internal IS NULL)`
}

// RecordNetworkTx records the app's network as internal or open once a /run a deploy made started an env on
// it (design/EGRESS.md §2.2), where that is not what is recorded — no row reading open: a start that leaves it
// as it was writes nothing.
func RecordNetworkTx(ctx context.Context, tx pgx.Tx, project string, internal bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO app_networks (project_id, internal) SELECT $1, $2
		WHERE CASE WHEN EXISTS (SELECT 1 FROM app_networks WHERE project_id = $1) THEN (SELECT internal FROM app_networks WHERE project_id = $1) ELSE false END IS DISTINCT FROM $2
		ON CONFLICT (project_id) DO UPDATE SET internal = EXCLUDED.internal, recorded_at = now()`, project, internal)
	return err
}

// SetNetworkTx records the app's network as a recreation leaves it: internal, open, or nil — unknown, before
// the executor's call and after one that failed (design/EGRESS.md §2.2).
func SetNetworkTx(ctx context.Context, tx pgx.Tx, project string, internal *bool) error {
	_, err := tx.Exec(ctx, `INSERT INTO app_networks (project_id, internal) VALUES ($1, $2)
		ON CONFLICT (project_id) DO UPDATE SET internal = EXCLUDED.internal, recorded_at = now()`, project, internal)
	return err
}

// replicaLive is how recently an egress-control replica must have asked for the policy to count as live: egress-control's
// own callersKept, as long as a replica cut off from the server still serves the policy it holds (design/EGRESS.md
// §3.2; #2789's review): one seen 16 s ago may still let through what a change refuses, so it is waited for.
const replicaLive = 30 * time.Second

// converge narrows env's trusted roots, before its /run, to those it trusted already that the /run carries too —
// old and new instances may both serve until the old are gone (contracts/routes.md, Interception, Trust) — and,
// under via canyon, names app env in the egress policy, both in one transaction, so the narrowing moves seq and the
// wait below covers it. Under via canyon it then waits, at most EgressConverge, until every live
// egress-control replica holds that version or a later one of this epoch, so the app's first call finds its env
// (design/EGRESS.md §3.2, Convergence). Past it nothing starts: the /run fails closed naming the first replica
// behind. With no replica live — read so, never assumed — there is nothing to wait for: one that starts later
// takes the current version before it is ready, and one cut off refuses the env it does not know. A replica
// holding another epoch's version, or none it could say, is behind; an error reading the version or the
// replicas, the database's or the run's context's, fails the /run: the wait is never skipped. named is whether
// this call named the env, which a failure of the /run drops again (unname).
func (p *Pipeline) converge(ctx context.Context, app, env string, roots []string, canyon bool) (named bool, err error) {
	var target int64
	var inserted bool
	if err := pgx.BeginFunc(ctx, p.Pool, func(tx pgx.Tx) error {
		if canyon {
			tag, err := tx.Exec(ctx, "INSERT INTO egress_envs (project_id, env) VALUES ($1, $2) ON CONFLICT DO NOTHING", app, env)
			if err != nil {
				return err
			}
			inserted = tag.RowsAffected() == 1
		}
		if _, err := tx.Exec(ctx, "UPDATE deploys SET egress_roots = ARRAY(SELECT r FROM unnest(egress_roots) r WHERE r = ANY($3::text[]) ORDER BY r) WHERE project_id = $1 AND env = $2",
			app, env, roots); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT seq FROM egress_policy").Scan(&target)
	}); err != nil || !canyon {
		return false, err
	}
	named = inserted
	behind, err := EgressBehind(ctx, p.Pool, target, cmp.Or(p.EgressConverge, EgressConverge))
	if err == nil && behind != "" {
		err = fmt.Errorf("the egress gateway has not taken this version (%s)", behind)
	}
	return named, err
}

// EgressConverge is how long a write that changes the egress policy waits for egress-control to take it, a /run's
// and an outbound rule's alike (design/EGRESS.md §3.2, Convergence; #2789).
const EgressConverge = 30 * time.Second

// EgressBehind waits, at most max, until every egress-control replica live within replicaLive holds version target
// of the policy's epoch, or a later one: "" once none is behind, else the first behind at max, by name. With no
// replica live — read so, never assumed — none is behind. A replica holding another epoch's version, or none it could
// say, is behind. An error reading the version or the replicas, or ctx's, is answered: the wait is never skipped.
// converge's, before a /run, and every outbound rule change's, before it answers (#2789).
func EgressBehind(ctx context.Context, pool *pgxpool.Pool, target int64, max time.Duration) (string, error) {
	deadline := time.Now().Add(max)
	for {
		var behind *string
		if err := pool.QueryRow(ctx, `SELECT min(replica COLLATE "C") FROM egress_replicas WHERE seen_at > now() - make_interval(secs => $1)
			AND (have_epoch IS DISTINCT FROM (SELECT epoch FROM egress_policy) OR have_seq IS NULL OR have_seq < $2)`, replicaLive.Seconds(), target).Scan(&behind); err != nil {
			return "", err
		}
		if behind == nil {
			return "", nil
		}
		if time.Now().After(deadline) {
			return *behind, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// egressOf is /run's egress under spec: nil for via off; for via canyon the proxy and the host of the app's
// DATABASE_URL, its database inside the network (contracts/executor.md, Egress). A host with no proxy fails
// closed: the app would call out past Canyon's Envoy.
func (p *Pipeline) egressOf(spec Egress, values map[string]string) (*executor.Egress, error) {
	if spec.Via != "canyon" {
		return nil, nil
	}
	if p.EgressProxy == "" {
		return nil, errors.New("runs: egress is through Canyon and this server has no CANYON_EGRESS_PROXY")
	}
	out := &executor.Egress{Proxy: p.EgressProxy, NoProxy: []string{}}
	if u, err := url.Parse(values["DATABASE_URL"]); err == nil && u.Hostname() != "" {
		out.NoProxy = append(out.NoProxy, strings.ToLower(u.Hostname()))
	}
	return out, nil
}

// RunApp starts image as the app's container in env — preview after a deploy run's build, prod on a
// promotion — with that environment's runtime values, under the egress spec given, labelled with the run,
// and answers the URL it is published under,
// checked before the container is touched, on Kubernetes what the app answered its sidecar, and the run-config
// its deploy row records: what this /run carried, read with what it hands on, and its fingerprint, taken before
// the container is touched. On Kubernetes /run also carries
// the front door set at this call and the app's access file, and on both what the server keeps (keep). It
// records nothing but the history rows that keep leaves out: the caller does. expect, when set, is the
// fingerprint a patch's /run must carry (contracts/routes.md, Patch runs, step 5): another is ErrConfigChanged,
// with what it would carry, before the executor is touched.
func (p *Pipeline) RunApp(ctx context.Context, run, app, env, image string, spec Egress, expect string) (string, *asked.Answer, Carried, error) {
	ctx, cancel := context.WithTimeout(ctx, runCeiling)
	defer cancel()
	config, values, door, err := p.runConfig(ctx, app, env, spec, true)
	if err != nil {
		return "", nil, Carried{}, err
	}
	carried := Carried{Config: config, Roots: []string{}} // its egress.ca's roots, below: none without egress
	if carried.Fingerprint, err = config.Fingerprint(); err != nil {
		return "", nil, Carried{}, err
	}
	if expect != "" && carried.Fingerprint != expect {
		return "", nil, carried, ErrConfigChanged
	}
	url, err := p.appURL(door, app, env)
	if err != nil {
		return "", nil, Carried{}, err
	}
	if err := p.placeholders(ctx, app, env, values); err != nil {
		return "", nil, Carried{}, err
	}
	identity := executor.Identity{Issuer: p.PublicURL, Audience: app, JWKSURL: p.JWKSURL} // the audience is the app id (contracts/executor.md)
	req := executor.RunRequest{App: app, Target: env, Image: image, Env: values, Network: "canyon-app-" + app, Identity: identity}
	if req.Keep, err = p.keep(ctx, app, image); err != nil {
		return "", nil, Carried{}, err
	}
	if req.Egress, err = p.egressOf(spec, values); err != nil {
		return "", nil, Carried{}, err
	}
	if req.Egress != nil { // run labels a caller for its records, which only egress makes: an executor that knows one knows the other
		req.Run = run
		if req.Egress.CA, carried.Roots, err = p.egressCA(ctx, app, env, values); err != nil { // before converge narrows the env's trusted roots to them
			return "", nil, Carried{}, err
		}
	}
	if door != nil {
		req.Identity.JWKSURL = sidecarJWKS
		if req.FrontDoor, err = json.Marshal(door); err == nil {
			req.Access, err = p.Access(ctx, app)
		}
		if err != nil {
			return "", nil, Carried{}, err
		}
		if p.DatabaseCA != "" {
			req.DatabaseCA = &p.DatabaseCA
		}
	}
	named, err := p.converge(ctx, app, env, carried.Roots, req.Egress != nil) // named: this /run named its env in the egress policy, which a failure drops again
	if err != nil {
		return "", nil, Carried{}, p.unname(ctx, named, app, env, err)
	}
	carried.Started = time.Now() // a refusal counts against this version from here (contracts/routes.md; #2738)
	res, err := p.Executor.Run(ctx, req)
	if err != nil {
		return "", nil, Carried{}, p.unname(ctx, named, app, env, err)
	}
	carried.Gone = p.Access == nil || res.PreviousGone != nil && *res.PreviousGone // Docker's 200 answers once the previous container is removed
	if p.Access != nil && req.Egress != nil && carried.Gone && res.TokenCallers {  // never an answer that does not say its pods carry the token: an older executor's
		p.tokenOnly(ctx, app, env)
	}
	return url, res.Answered, carried, nil
}

// ErrConfigChanged is a patch's /run that would carry another run-config than its deploy row's.
var ErrConfigChanged = errors.New("runs: what the app runs with changed since it was deployed")

// runConfig is what a /run of app in env under spec carries beside its image now (contracts/routes.md, Patch
// runs, step 5): its run-config, read with the runtime values and the front door it hands on — the door's
// client secret opened only with secret.
func (p *Pipeline) runConfig(ctx context.Context, app, env string, spec Egress, secret bool) (RunConfig, map[string]string, map[string]any, error) {
	config := RunConfig{Egress: spec, RunBy: "canyon"}
	values, versions, err := p.versioned(ctx, app, env, "runtime")
	if err != nil {
		return RunConfig{}, nil, nil, err
	}
	config.Secrets = versions
	if err := p.Pool.QueryRow(ctx, "SELECT coalesce(array_agg(name ORDER BY name), '{}') FROM resources WHERE project_id = $1 AND env = $2", app, env).Scan(&config.Resources); err != nil {
		return RunConfig{}, nil, nil, err
	}
	door, saved, err := p.frontDoor(ctx, secret)
	if err != nil {
		return RunConfig{}, nil, nil, err
	}
	if saved != nil && !saved.Probed { // Canyon never runs an app behind a door that has not passed the probe (#2438): doorSet said so first
		return RunConfig{}, nil, nil, errDoorUnchecked
	}
	config.FrontDoor = saved
	return config, values, door, nil
}

// tokenOnly marks app env token_only (design/EGRESS.md §4.3): on Kubernetes once a /run with egress succeeded, its
// answer saying its pods carry the token (token_callers) and no pod of an earlier template remaining
// (previous_gone), every pod of the env carries canyon-identity's forwarder, so the policy
// names them by their token alone from now on. A failed /run never gets here. A mark that cannot be written is
// logged and left: the env stays its pods' address's too, as before layer 3, until its next /run marks it.
func (p *Pipeline) tokenOnly(ctx context.Context, app, env string) {
	if _, err := p.Pool.Exec(ctx, "UPDATE egress_envs SET token_only = true WHERE project_id = $1 AND env = $2 AND NOT token_only", app, env); err != nil {
		log.Printf("runs: marking %s-%s's pods named by their token alone: %v", app, env, err)
	}
}

// unname drops the env a failed /run named in the egress policy, unless a deploy row Canyon runs under via
// canyon names it now — an earlier version still running there — and answers err with any failure of its own.
func (p *Pipeline) unname(ctx context.Context, named bool, app, env string, err error) error {
	if !named {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second) // the run's context may be what ended
	defer cancel()
	_, dropErr := p.Pool.Exec(ctx, `DELETE FROM egress_envs WHERE project_id = $1 AND env = $2
		AND NOT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = $2 AND run_by = 'canyon' AND egress->>'via' = 'canyon')`, app, env)
	return errors.Join(err, dropErr)
}

// answered is what the app answered past Canyon's sign-in (contracts/routes.md, envs.<env>.answered): on
// Kubernetes the executor's, from /run; on Docker, where p.Access is nil, the server's own ask of
// <app>-<env>:3000 on the app's network as its proxy reaches it, with addr's host as Host and said masked
// with env's runtime values — "" when they could not be read, never a raw line. Then what could hide text
// goes and said is cut to 200 characters. nil when the executor could not ask.
func (p *Pipeline) answered(ctx context.Context, app, env, addr string, ran *asked.Answer) *asked.Answer {
	if p.Access == nil {
		u, err := url.Parse(addr)
		if err != nil {
			return nil
		}
		a := asked.Ask(ctx, p.Ask, "http://"+app+"-"+env+":3000/", u.Host)
		if values, err := p.secrets(ctx, app, env, "runtime"); err != nil {
			log.Printf("runs: what %s-%s answered: %v", app, env, err)
			a.Said = ""
		} else {
			a.Said = executor.Mask(a.Said, values)
		}
		ran = &a
	}
	if ran == nil {
		return nil
	}
	shown := []rune(strings.Map(func(r rune) rune {
		if r == 0x7F || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) { // the server's hidesText
			return -1
		}
		return r
	}, ran.Said))
	ran.Said = string(shown[:min(len(shown), 200)])
	return ran
}

// sidecarJWKS is the key set canyon-identity serves in the app's pod: a Kubernetes /run's identity.jwks_url,
// exactly (contracts/executor.md, On Kubernetes).
const sidecarJWKS = "http://127.0.0.1:4182/.well-known/jwks.json"

// noFrontDoor is D5's line (design/EXPERIENCE.md): the last build.log line of a deploy or promotion on
// a Kubernetes host with no front door.
const noFrontDoor = "No front door is set, so no app can run on this Canyon yet. An admin sets it in Settings › App hosting."

// doorUnchecked is D5's line (design/EXPERIENCE.md; #2438): the last build.log line of a deploy or promotion under
// run_by: canyon behind a front door that has not passed the probe; errDoorUnchecked is any /run asked behind one.
const doorUnchecked = "The front door hasn't passed Canyon's check of this cluster, so Canyon won't run apps behind it. An admin saves it again in Settings › App hosting."

var errDoorUnchecked = errors.New("runs: the front door has not passed the cluster probe: no /run behind it")

// notEnforced is D5's line (design/EXPERIENCE.md; #2507): the last build.log line of a deploy, under either run_by, on a
// Kubernetes host whose front door's save did not find NetworkPolicy enforced. A build's own NetworkPolicy holds only
// where the cluster enforces it, so no /build is sent.
const notEnforced = `Canyon hasn't seen this cluster enforce NetworkPolicy, so it builds nothing here: a build could reach the cluster's network and its credentials. Turn enforcement on (on EKS: enableNetworkPolicy "true" and NETWORK_POLICY_ENFORCING_MODE "strict" in the vpc-cni add-on), then an admin saves the front door again in Settings › App hosting.`

// doorSet answers whether the host has a front door Canyon may run apps behind, and when it has none writes D5's
// line: the deploy or promotion then fails before the executor is called (contracts/routes.md, the front-door row).
// Under run_by: canyon that door must have passed the probe (#2438); under org the org's platform runs the app,
// and RunApp refuses an unprobed door whatever this read said. Under either, a deploy builds only on a cluster its
// door's save found enforcing NetworkPolicy (#2507).
func (p *Pipeline) doorSet(ctx context.Context, in Input) (bool, error) {
	var set, unchecked, enforced bool
	err := p.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM front_doors),
		EXISTS (SELECT 1 FROM front_doors WHERE probed_at IS NULL) AND (SELECT run_by FROM org_preferences) IS DISTINCT FROM 'org',
		EXISTS (SELECT 1 FROM front_doors WHERE network_policy_enforced_at IS NOT NULL)`).Scan(&set, &unchecked, &enforced)
	switch {
	case err != nil:
		return false, err
	case !set:
		return false, p.record(ctx, in, provisioners.Key{}, "", "", "", "", "front-door", noFrontDoor)
	case unchecked:
		return false, p.record(ctx, in, provisioners.Key{}, "", "", "", "", "front-door", doorUnchecked)
	case in.Deploy && !enforced:
		return false, p.record(ctx, in, provisioners.Key{}, "", "", "", "", "front-door", notEnforced)
	}
	return true, nil
}

// frontDoor is the host's front door as /run carries it (contracts/executor.md, The front door), nil
// on Docker: the saved row and, with secret, choice 2's client secret opened from the vault here, at
// the call, so it is handed on and never a step's output or a timeline row. saved is the row as it was
// saved, which holds no secret: its door and when it was saved.
func (p *Pipeline) frontDoor(ctx context.Context, secret bool) (door map[string]any, saved *SavedDoor, err error) {
	if p.Access == nil {
		return nil, nil, nil
	}
	saved = &SavedDoor{}
	var keyRef *int
	var sealedKey, sealedValue []byte
	err = p.Pool.QueryRow(ctx, "SELECT door, set_at, probed_at IS NOT NULL, key_ref, sealed_key, sealed_value FROM front_doors").Scan(&saved.Door, &saved.SetAt, &saved.Probed, &keyRef, &sealedKey, &sealedValue)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, errors.New("runs: this Kubernetes host has no front door")
	}
	if err == nil {
		saved.SetAt = saved.SetAt.UTC()
		err = json.Unmarshal(saved.Door, &door)
	}
	if err != nil || !secret || keyRef == nil {
		return door, saved, err
	}
	value, err := p.Vault.Open(*keyRef, "front-door", "signin", "client_secret", sealedKey, sealedValue)
	if err != nil {
		return nil, nil, err
	}
	signin, ok := door["signin"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("runs: the front door's client secret is sealed but it names no signin")
	}
	signin["client_secret"] = string(value)
	return door, saved, nil
}

// appURL is where the app's environment is published (contracts/routes.md): under the front door's
// domain in its form on Kubernetes, https; else the public host's scheme and port, the app and the
// environment as the first two labels (App surfaces).
func (p *Pipeline) appURL(door map[string]any, app, env string) (string, error) {
	if door != nil {
		if door["form"] == "app-env.domain" {
			return fmt.Sprintf("https://%s-%s.%s", app, env, door["domain"]), nil
		}
		return fmt.Sprintf("https://%s.%s.%s", app, env, door["domain"]), nil
	}
	u, err := url.Parse(p.PublicURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("runs: public url %q: not a URL with a host", p.PublicURL)
	}
	return u.Scheme + "://" + app + "." + env + "." + u.Host, nil
}

// running is the first write: pending → running, once.
func (p *Pipeline) running(ctx context.Context, in Input) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE runs SET state = 'running' WHERE id = $1 AND state = 'pending'", in.Run); err != nil {
			return err
		}
		_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.RunState, "state:running", map[string]any{"state": "running"})
		return err
	})
}

// ValidProject is the seam's project id pattern, which the CLI checks against too (internal/names);
// the id names a directory here, so nothing else may reach this package.
var ValidProject = names.Project

// gate materialises the package and reviews it, then adds each tool the run's scans could not check
// it with, after the review, so none is ever judged. The tree is not a step of its own: a temp path
// in a checkpoint is gone after a restart; the log is the checkpoint — validated before it is
// returned, so a log the profile refuses (an unredacted literal, a missing flag) is never
// journaled, only its refusal.
func (p *Pipeline) gate(ctx context.Context, in Input, missed []unfinished) (log finding.Log, err error) {
	defer func() { // a gate that panics is a gate that errored: the run fails closed instead of sitting running forever
		if r := recover(); r != nil {
			log, err = finding.Log{}, fmt.Errorf("runs: the gate panicked: %v", r)
		}
	}()
	if !ValidProject.MatchString(in.Project) {
		return finding.Log{}, fmt.Errorf("runs: project id %q is not a project id", in.Project)
	}
	if len(in.Policy.Rules) == 0 { // a run enqueued before the release that journals the policy snapshot, recovered by dbos.Launch: refused by name, not as a derive error against a zero snapshot
		return finding.Log{}, errors.New("runs: this run was enqueued before the release that records the policy snapshot at run start; start it again")
	}
	var commit, tree string
	if err := p.Pool.QueryRow(ctx, "SELECT commit, tree_hash FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&commit, &tree); err != nil {
		return finding.Log{}, err
	}
	dir, err := os.MkdirTemp("", "canyon-gate-")
	if err != nil {
		return finding.Log{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	checkout, cancel := context.WithTimeout(ctx, gateCeiling)
	defer cancel()
	if err := ingest.Extract(checkout, filepath.Join(p.Repos, in.Project+".git"), commit, dir); err != nil {
		return finding.Log{}, err
	}
	if log, err = p.Gate(gate.WithRun(ctx, in.Run, in.Project), dir, tree, in.Policy, p.checked(in)); err != nil { // the run a dropped AI candidate's log line names
		return finding.Log{}, err
	}
	if err := p.reviewedBefore(ctx, in, dir, &log); err != nil {
		return finding.Log{}, err
	}
	if err := p.customByCode(ctx, in, dir, &log); err != nil {
		return finding.Log{}, err
	}
	missed = append(slices.Clip(missed), unreadBlocking(log, in.Policy)...)
	if len(missed) > 0 {
		walked, err := gate.Walk(dir)
		if err != nil {
			return finding.Log{}, err
		}
		for _, m := range missed {
			if err := gate.CouldNotCheck(&log, walked, m.Tool, m.Why); err != nil {
				return finding.Log{}, err
			}
		}
	}
	if err := p.imports(ctx, in, dir, &log, missed); err != nil {
		return finding.Log{}, err
	}
	if err := finding.Derive(&log, in.Policy); err != nil { // blocks is the server's: the rule's state in the snapshot the run started with and the finding's severity, whatever the producer wrote
		return finding.Log{}, fmt.Errorf("runs: the gate's log cannot be derived against the policy snapshot: %w", err)
	}
	if err := finding.Check(&log, in.Policy); err != nil { // the two schemas, the profile, and the three the snapshot owns
		return finding.Log{}, fmt.Errorf("runs: the gate's log fails the profile: %w", err)
	}
	seen := map[string]bool{}
	for _, r := range log.Runs { // one producer.sarif row per producer per run (contracts/events.md): a producer named twice is a log the ledger cannot hold
		if seen[producer(r)] {
			return finding.Log{}, fmt.Errorf("runs: the gate's log names producer %q twice", producer(r))
		}
		seen[producer(r)] = true
	}
	return log, nil
}

// reviewedBefore holds the log's AI review results to what the project's last 20 completed reviews, its newest
// verdicts of any other run, said of the same code: each result raised to the severity they gave it (gate.Floor;
// contracts/finding.md, A region reviewed before keeps its severity), then each blocking result of theirs this review
// left out carried while its code stands in the package at dir (gate.Carry; A blocking finding an earlier review made
// stays while its code stands). A review that read nothing, a branch check or a rollback review in between never
// resets either. Before blocks is derived, so what blocks blocks. A project with no verdict has nothing to hold; a
// read that fails, or findings that are not a log in Canyon's profile, fail the run.
func (p *Pipeline) reviewedBefore(ctx context.Context, in Input, dir string, log *finding.Log) error {
	// ponytail: last 20 verdicts; a region absent from 20 in a row gets a fresh rating, and is carried no more
	rows, err := p.Pool.Query(ctx, "SELECT run_id, data->'findings' FROM timeline_rows WHERE project_id = $1 AND kind = 'verdict' AND run_id <> $2 ORDER BY id DESC LIMIT 20", // timeline_verdicts
		in.Project, in.Run)
	if err != nil {
		return fmt.Errorf("runs: the project's last reviews could not be read: %w", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Run string
		Raw []byte
	}])
	if err != nil {
		return fmt.Errorf("runs: the project's last reviews could not be read: %w", err)
	}
	var earlier finding.Log
	verdicts := make([]gate.Verdict, 0, len(found))
	for _, f := range found {
		var l finding.Log
		if err := json.Unmarshal(f.Raw, &l); err != nil {
			return fmt.Errorf("runs: a recent review's findings are not a log: %w", err)
		}
		if l.Version != finding.Version || l.Schema != finding.SchemaURI { // null and {} unmarshal: they are no log either
			return fmt.Errorf("runs: a recent review's findings are not a log in Canyon's profile: version %q, $schema %q", l.Version, l.Schema)
		}
		earlier.Runs = append(earlier.Runs, l.Runs...)
		verdicts = append(verdicts, gate.Verdict{Run: f.Run, Log: l})
	}
	gate.Floor(log, earlier, in.Policy)
	gate.Carry(log, verdicts, in.Policy, dir)
	return nil
}

// customByCode adds to log's scanners' run the results of the run's custom rules checked by code, over
// the package's facts and the app's secrets by name in every environment, as they stand now
// (contracts/finding.md, A custom rule checked by code): after the gate, never cached with its review,
// so a secret set since is read.
func (p *Pipeline) customByCode(ctx context.Context, in Input, dir string, log *finding.Log) error {
	if !slices.ContainsFunc(in.Policy.Rules, custom.Checked) {
		return nil
	}
	secrets, pkgs, err := p.customInputs(ctx, in)
	if err != nil {
		return err
	}
	return addCustom(ctx, log, dir, in.Policy, secrets, pkgs)
}

// customInputs is what the custom rules checked by code read beside the package's tree: the app's
// secrets by name in every environment, as they stand now, and trivy's packages.
func (p *Pipeline) customInputs(ctx context.Context, in Input) ([]custom.Secret, custom.Packages, error) {
	rows, err := p.Pool.Query(ctx, "SELECT name, env, scope FROM secrets WHERE project_id = $1 ORDER BY env, scope, name", in.Project)
	if err != nil {
		return nil, custom.Packages{}, err
	}
	secrets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (s custom.Secret, err error) {
		return s, row.Scan(&s.Name, &s.Env, &s.Where)
	})
	if err != nil {
		return nil, custom.Packages{}, err
	}
	pkgs, err := p.packages(ctx, in)
	return append([]custom.Secret{}, secrets...), pkgs, err
}

// Facts is the facts a review of in.Package under in.Policy reads (customByCode), its tree extracted
// afresh from the app's repository: what a dry run of a custom rule judges (contracts/routes.md, the
// dry-run row), never a second reader.
func (p *Pipeline) Facts(ctx context.Context, in Input) (custom.Facts, error) {
	var commit string
	if err := p.Pool.QueryRow(ctx, "SELECT commit FROM packages WHERE id = $1 AND project_id = $2", in.Package, in.Project).Scan(&commit); err != nil {
		return custom.Facts{}, err
	}
	dir, err := os.MkdirTemp("", "canyon-facts-")
	if err != nil {
		return custom.Facts{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := ingest.Extract(ctx, filepath.Join(p.Repos, in.Project+".git"), commit, dir); err != nil {
		return custom.Facts{}, err
	}
	tree, err := gate.Walk(dir)
	if err != nil {
		return custom.Facts{}, err
	}
	secrets, pkgs, err := p.customInputs(ctx, in)
	if err != nil {
		return custom.Facts{}, err
	}
	return custom.ReadFacts(tree, secrets, pkgs), nil
}

// packages is what trivy lists of the package's files, asked of the executor while a rule checked by code
// reads the dependencies and sarif.trivy is on and carries run (contracts/finding.md, A custom rule
// checked by code), or why there is no list, in words a could-not-check finding carries: trivy off, busy
// past busyFor, a scan that did not finish or answered too much, or an executor that could not be asked.
// Only the run's own cancellation is an error: nothing else of this one scan fails the run. A busy
// executor is asked again once its Retry-After has passed; the scan is the review's own, never a task of
// the scan queue.
func (p *Pipeline) packages(ctx context.Context, in Input) (custom.Packages, error) {
	switch {
	case !custom.ReadsDependencies(in.Policy.Rules):
		return custom.Packages{Why: custom.TrivyUnread}, nil
	case !slices.ContainsFunc(scannedTools(in.Policy), func(r policy.Rule) bool { return r.ID == "sarif.trivy" }):
		return custom.Packages{Why: custom.TrivyOff}, nil
	case p.Executor == nil:
		return custom.Packages{Why: custom.TrivyUnasked}, nil
	}
	until := time.Now().Add(busyFor)
	for {
		res, err := p.packagesOnce(ctx, in)
		var busy *executor.Busy
		switch {
		case ctx.Err() != nil:
			return custom.Packages{}, ctx.Err()
		case errors.As(err, &busy):
			wait := max(busy.RetryAfter, time.Second)
			if time.Now().Add(wait).After(until) {
				return custom.Packages{Why: custom.TrivyBusy}, nil
			}
			select {
			case <-ctx.Done():
				return custom.Packages{}, ctx.Err()
			case <-time.After(wait):
			}
			continue
		case errors.Is(err, context.DeadlineExceeded): // no answer in the scan's own time: it did not finish
			return custom.Packages{Why: custom.TrivyUnfinished}, nil
		case err != nil:
			log.Printf("runs: run %s (%s): the packages scan: %v", in.Run, in.Project, err)
			return custom.Packages{Why: custom.TrivyUnasked}, nil
		case res.NotFinished == "over_cap":
			return custom.Packages{Why: custom.TrivyTooLarge}, nil
		case res.NotFinished != "" || res.Packages == nil:
			return custom.Packages{Why: custom.TrivyUnfinished}, nil
		}
		return custom.Packages{Report: res.Packages}, nil
	}
}

func (p *Pipeline) packagesOnce(ctx context.Context, in Input) (executor.ScanResult, error) {
	ctx, cancel := context.WithTimeout(ctx, scanCeiling)
	defer cancel()
	tree, err := p.archive(ctx, in)
	if err != nil {
		return executor.ScanResult{}, err
	}
	defer func() { _ = tree.Close() }()
	return p.Executor.Scan(ctx, executor.ScanRequest{App: in.Project, Package: in.Package, Tool: "trivy", Target: "packages"}, tree, nil)
}

// addCustom is customByCode on secrets and packages already read.
func addCustom(ctx context.Context, log *finding.Log, dir string, snap policy.Snapshot, secrets []custom.Secret, pkgs custom.Packages) error {
	i := slices.IndexFunc(log.Runs, func(r finding.Run) bool { return r.Tool.Driver.Name == policy.Scanner.Driver() })
	if i < 0 {
		return errors.New("runs: a custom rule checked by code needs the scanners' run")
	}
	tree, err := gate.Walk(dir)
	if err != nil {
		return err
	}
	results, rules, err := custom.Review(ctx, tree, snap.Rules, secrets, pkgs)
	if err != nil {
		return err
	}
	log.Runs[i].Results = append(log.Runs[i].Results, results...)
	log.Runs[i].Tool.Driver.Rules = append(log.Runs[i].Tool.Driver.Rules, rules...)
	return nil
}

// unreadBlocking is each agent of snap whose run read nothing (gate.Unread) while a rule of its
// namespace is blocking before preview, as a tool that could not check the package: a review that did
// not happen never passes a blocking rule (contracts/finding.md, What Canyon could not check).
func unreadBlocking(log finding.Log, snap policy.Snapshot) []unfinished {
	var out []unfinished
	for _, a := range snap.Agents {
		i := slices.IndexFunc(log.Runs, func(r finding.Run) bool { return r.Tool.Driver.Name == a.ID })
		blocking := slices.ContainsFunc(snap.Rules, func(r policy.Rule) bool {
			ns, _, _ := strings.Cut(r.ID, ".")
			return r.State == policy.Blocking && slices.Contains(a.Namespace, ns)
		})
		switch {
		case !blocking:
		case i < 0: // no run of it at all: a review that did not happen, as much as one that read nothing
			out = append(out, unfinished{Tool: cmp.Or(agentNames[a.ID], "the AI review"), Why: "it did not run"})
		case gate.Unread(log.Runs[i]) != "":
			out = append(out, unfinished{Tool: cmp.Or(agentNames[a.ID], "the AI review"), Why: unreadWhy[gate.Unread(log.Runs[i])]})
		}
	}
	return out
}

// agentNames are the agents as a gate.unreadable finding names them; unreadWhy its reason, by the note
// of the agent's run that read nothing.
var (
	agentNames = map[string]string{"canyon-review": "the AI review", gate.CustomRules: "custom rules"}
	unreadWhy  = map[string]string{
		gate.Skipped:          "no model is configured on this Canyon host",
		gate.OutOfRoom:        "the model ran out of room for its answer",
		gate.TooLarge:         "the source is larger than it accepts (16 MiB)",
		gate.TooLargeForACall: "a file is too large for one AI review call",
		gate.NotUTF8:          "a file is not valid UTF-8",
		gate.TimedOut:         "the model did not answer in the time the review allows",
		gate.TooLargeBefore:   "the source is larger than it accepted then (2 MiB)",
	}
)

// producer is a run's key on the ledger: its driver, then an imported tool's category when it has one, so
// one tool's reports under two categories are two producer.sarif rows (contracts/events.md).
func producer(r finding.Run) string {
	if r.AutomationDetails == nil {
		return r.Tool.Driver.Name
	}
	return r.Tool.Driver.Name + " " + r.AutomationDetails.ID
}

// imports appends the imported tools' runs to log (contracts/finding.md, In: external SARIF): for each tool
// whose rule is on, in the snapshot's order, one run per report the run journaled at start, read by
// id, or one run saying the package has none — each run of a tool the run's scan could not check with
// saying so first, whatever report the package had; then canyon/v1 over the assembled log under the
// project's report key, which is read here and handled nowhere else.
func (p *Pipeline) imports(ctx context.Context, in Input, dir string, log *finding.Log, missed []unfinished) error {
	reports, err := ReadReports(ctx, p.Pool, in.Package, in.Reports)
	if err != nil {
		return err
	}
	walked, err := gate.Walk(dir)
	if err != nil {
		return err
	}
	files := walked.Files() // what a path the tool did not read whole must be exactly one of to be named
	// A tool with no file to read says so, never a clean line (#1742).
	nothing, err := gate.ReadNothing(walked, in.Policy)
	if err != nil {
		return err
	}
	say := func(level, text string) finding.Notification {
		return finding.Notification{Level: level, Message: finding.Message{Text: text}}
	}
	for _, tool := range importedTools(in.Policy) {
		its := slices.DeleteFunc(slices.Clone(reports), func(r Report) bool { return r.Tool != tool })
		var first []finding.Notification // a report from before this run never reads as this run's clean scan (#1658)
		if i := slices.IndexFunc(missed, func(m unfinished) bool { return m.Tool == tool }); i >= 0 {
			first = append(first, say("warning", tool+" · could not check: "+missed[i].Why))
		}
		if len(its) == 0 && first == nil {
			first = []finding.Notification{say("warning", tool+" · no report for this package")}
		}
		var none []finding.Notification // on a run that holds no result alone: a result is a file the tool read
		if why, ok := nothing[tool]; ok {
			none = []finding.Notification{say("note", tool+" · read no file: "+why)}
		}
		if len(its) == 0 {
			log.Runs = append(log.Runs, importedRun(tool, "unknown", "", []finding.Result{}, append(first, none...)...))
		}
		for _, r := range its {
			notes := slices.Clone(first)
			if len(r.Results) == 0 {
				notes = append(notes, none...)
			}
			notes = append(notes, say("note", tool+" · report attached by "+r.AttachedBy.Display))
			if r.Dropped > 0 {
				notes = append(notes, say("warning", fmt.Sprintf("%s · dropped %d result(s) at attach", tool, r.Dropped)))
			}
			for _, id := range r.Unloaded[:min(len(r.Unloaded), 20)] { // a rule the tool dropped never ran: named, never a clean line
				which := "a rule"
				if id != "" {
					which = "rule " + id
				}
				notes = append(notes, say("warning", tool+" · not checked: "+which+" could not load"))
			}
			if more := len(r.Unloaded) - 20; more > 0 {
				notes = append(notes, say("warning", fmt.Sprintf("%s · not checked: %d more rules could not load", tool, more)))
			}
			unparsed := slices.DeleteFunc(slices.Clone(r.Unparsed), func(path string) bool { _, held := slices.BinarySearch(files, path); return !held })
			for _, path := range unparsed[:min(len(unparsed), 20)] { // a file the tool did not read whole: named, never a clean line
				notes = append(notes, say("warning", tool+" · not checked: "+path+" was not read whole"))
			}
			if more := len(unparsed) - 20; more > 0 {
				notes = append(notes, say("warning", fmt.Sprintf("%s · not checked: %d more files were not read whole", tool, more)))
			}
			log.Runs = append(log.Runs, importedRun(tool, r.Version, r.Category, r.Results, notes...))
		}
	}
	if len(reports) == 0 {
		return nil
	}
	key, err := openReportKey(ctx, p.Pool, p.Vault, in.Project)
	if err != nil {
		return err
	}
	return gate.FingerprintImported(log, walked, key)
}

// importedRun is one imported tool's run: driver sarif/<tool>, its category as automationDetails.id when
// not empty, and each rule its results carry declared once, under the rule's name.
func importedRun(tool, version, category string, results []finding.Result, notes ...finding.Notification) finding.Run {
	run := finding.Run{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: finding.Driver{Name: "sarif/" + tool, Version: version, Rules: []finding.Rule{}}},
		Invocations: []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: notes}}, Results: results}
	if category != "" {
		run.AutomationDetails = &finding.AutomationDetails{ID: category}
	}
	for _, r := range results {
		if !slices.ContainsFunc(run.Tool.Driver.Rules, func(d finding.Rule) bool { return d.ID == r.RuleID }) {
			run.Tool.Driver.Rules = append(run.Tool.Driver.Rules, finding.Rule{ID: r.RuleID, Name: r.Properties.Canyon.PolicyName})
		}
	}
	return run
}

// checked writes a check's check.started, then its check.completed, once per run: the gate step
// may run again after a crash, and a replay finds the rows the first attempt wrote.
func (p *Pipeline) checked(in Input) gate.Report {
	return func(ctx context.Context, c gate.Check) error {
		kind, data := events.CheckStarted, map[string]any{"check": c.Name}
		if c.Status != "" {
			kind, data["status"], data["findings"], data["duration_ms"] = events.CheckCompleted, c.Status, c.Findings, c.Duration.Milliseconds()
		}
		return p.write(ctx, func(tx pgx.Tx) error {
			_, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, kind, string(kind)+":"+c.Name, data)
			return err
		})
	}
}

// counts tallies the verdict in entries (contracts/finding.md, One problem, one entry). An absent
// blocks is an error, never "does not block": gate derives every value before its step returns, so
// nothing reaches here undrived, but the guard is what makes that a checked fact rather than an assumption.
func counts(log finding.Log) (blocking, warnings int, err error) {
	for i, r := range log.Runs {
		for j, res := range r.Results {
			if res.Properties.Canyon.Blocks == nil {
				return 0, 0, fmt.Errorf("runs: run %d result %d: blocks is missing", i, j)
			}
		}
	}
	blocking, warnings = finding.Count(log)
	return blocking, warnings, nil
}

// ruling is the verdict step's checkpoint. An older binary's checkpoint, the empty struct,
// decodes as the zero value: not approved.
type ruling struct {
	Approved bool `json:"approved"`
}

// canyon is the actor of the verdict's audit row: the policy engine acting on its own, never the
// one who started the run (design/specs/screens.md, three actor types).
var canyon = events.Actor{Kind: "canyon", Subject: "gate", Display: "Canyon"}

// verdict writes the producers' logs, the verdict and its audit row in one transaction, each once
// per run. A blocked verdict whose every blocking finding has
// an unexpired approval lists those approvals, and the stored row is the answer: a replay reads what
// was written, whatever expired since.
func (p *Pipeline) verdict(ctx context.Context, in Input, log finding.Log, state string, blocking, warnings int) (ruling, error) {
	var out ruling
	err := p.write(ctx, func(tx pgx.Tx) error {
		var recorded bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'verdict')", in.Run).Scan(&recorded); err != nil {
			return err
		}
		imported, err := importedRows(ctx, tx, in)
		if err != nil {
			return err
		}
		for _, r := range log.Runs {
			data := map[string]any{"producer": r.Tool.Driver.Name, "sarif": finding.Log{Version: log.Version, Schema: log.Schema, Runs: []finding.Run{r}}}
			if by, _ := policy.ByDriver(r.Tool.Driver.Name); by == policy.Sarif {
				if data = imported[producer(r)]; data == nil {
					continue // a tool with no report has no row, which holds a report's digest: its line is the verdict's run
				}
			}
			if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.ProducerSARIF, "sarif:"+producer(r), data); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(log)
		if err != nil {
			return err
		}
		approved, unapproved, err := Approvals(ctx, tx, in.Project, in.Run, raw)
		if err != nil {
			return err
		}
		data := map[string]any{"state": state, "blocking": blocking, "warnings": warnings, "findings": log}
		if (in.Deploy || in.RollbackTo != "") && blocking > 0 && unapproved == 0 { // a deploy goes ahead under them, and a rollback review records what it ships
			data["approved"] = approved
		}
		row, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.Verdict, "verdict", data)
		if err != nil {
			return err
		}
		if !recorded {
			if err := events.AuditTx(ctx, tx, canyon, "verdict", in.Project, in.Run, map[string]any{"package": in.Package, "state": state, "blocking": blocking, "warnings": warnings}); err != nil {
				return err
			}
		}
		var stored struct {
			Approved []Approval `json:"approved"`
		}
		if err := json.Unmarshal(row.Data, &stored); err != nil {
			return err
		}
		out.Approved = len(stored.Approved) > 0
		return nil
	})
	return out, err
}

// importedRows is the producer.sarif row of each report the run includes, by its run's producer key: the
// report as stored, its results without partialFingerprints, never the raw report (contracts/events.md).
func importedRows(ctx context.Context, tx pgx.Tx, in Input) (map[string]map[string]any, error) {
	rows, err := tx.Query(ctx, "SELECT tool, category, version, digest, attached_by_display, dropped, coalesce(first_dropped, ''), results::text FROM package_reports WHERE package_id = $1 AND id = ANY($2)", in.Package, in.Reports)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	var tool, category, version, digest, by, first, results string
	var dropped int
	_, err = pgx.ForEachRow(rows, []any{&tool, &category, &version, &digest, &by, &dropped, &first, &results}, func() error {
		key, row := "sarif/"+tool, map[string]any{"producer": "sarif/" + tool, "tool": tool, "category": category, "version": version, "digest": digest, "attached_by": by, "dropped": dropped, "results": json.RawMessage(results)}
		if category != "" {
			key += " " + category
		}
		if first != "" {
			row["first_dropped"] = first
		}
		out[key] = row
		return nil
	})
	return out, err
}

// finish is the terminal step.
func (p *Pipeline) finish(ctx dbos.Context, in Input, state, reason string, blocking, warnings int) (Result, error) {
	return p.finishIn(ctx, in, state, reason, "", "", blocking, warnings)
}

// failed ends the run failed with error in step, the step Canyon itself failed in, and logs why under the
// run's id, which the owner is told to hand an admin (contracts/routes.md, A failure reason in words):
// the error's text alone, never a request body or a value.
func (p *Pipeline) failed(ctx dbos.Context, in Input, step string, err error, blocking, warnings int) (Result, error) {
	log.Printf("runs: run %s (%s) failed while %s: %v", in.Run, in.Project, step, err)
	return p.finishIn(ctx, in, "failed", "error", step, "", blocking, warnings)
}

// finishIn ends the run, notPlatform beside a patch run's failure reason not_platform_only, or a rebuild's
// not_same_inputs, saying which (contracts/events.md).
func (p *Pipeline) finishIn(ctx dbos.Context, in Input, state, reason, step, notPlatform string, blocking, warnings int) (Result, error) {
	braked, breason, bstep, err := p.stopOnEnd(ctx, in, state, reason, step)
	if err != nil {
		return Result{}, err
	}
	if braked != state || breason != reason { // the brake's end, never a patch's reason
		notPlatform = ""
	}
	state, reason, step = braked, breason, bstep
	if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
		return struct{}{}, p.end(c, in, state, reason, step, notPlatform)
	}, dbos.WithStepName("terminal")); err != nil {
		return Result{}, err
	}
	return Result{State: state, Blocking: blocking, Warnings: warnings}, nil
}

// End writes the end of a run in the agreed order: the state, the terminal row, ended_at. The
// workflow's terminal step and the sweep that ends a run DBOS gave up on both come through here.
func (p *Pipeline) End(ctx context.Context, in Input, state, reason, step string) error {
	return p.end(ctx, in, state, reason, step, "")
}

func (p *Pipeline) end(ctx context.Context, in Input, state, reason, step, notPlatform string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		which := "not_platform_only" // the field beside the reason that says which: a rebuild's own, else a patch's
		if reason == "not_same_inputs" {
			which = "not_same_inputs"
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET state = $2, failure_reason = nullif($3, ''), failed_in = nullif($4, ''),
			not_platform_only = CASE WHEN $3 = 'not_same_inputs' THEN NULL ELSE nullif($5, '') END, not_same_inputs = CASE WHEN $3 = 'not_same_inputs' THEN nullif($5, '') END
			WHERE id = $1 AND ended_at IS NULL`, in.Run, state, reason, step, notPlatform); err != nil {
			return err
		}
		data := map[string]any{"state": state}
		if reason != "" {
			data["failure_reason"] = reason
		}
		if step != "" {
			data["failed_in"] = step
		}
		if notPlatform != "" {
			data[which] = notPlatform
		}
		if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.RunState, "state:"+state, data); err != nil {
			return err
		}
		if action, ok := map[string]string{"build": "build failed", "detect": "detect failed", "start": "start failed", "no_room": "build had no room"}[reason]; ok { // what broke after the review let it through, on the trail beside the verdict's row (contracts/routes.md, the audit read)
			if err := events.AuditTx(ctx, tx, canyon, action, in.Project, in.Run, map[string]any{"package": in.Package}); err != nil {
				return err
			}
		}
		return events.EndTx(ctx, tx, in.Run)
	})
}

// write runs fn in one transaction under the append lock, taken first (the order every appender uses).
func (p *Pipeline) write(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := events.LockTx(ctx, tx); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return fmt.Errorf("runs: %w", err)
	}
	return tx.Commit(ctx)
}

// schedulerSaid is the scheduler's message as Canyon quotes it (contracts/finding.md, Canyon-run tools):
// every control or format character dropped, runs of spaces one, at most 500 characters.
func schedulerSaid(message string) string {
	kept := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return ' '
		}
		return r
	}, message)
	said := []rune(strings.Join(strings.Fields(kept), " "))
	return string(said[:min(len(said), 500)])
}
