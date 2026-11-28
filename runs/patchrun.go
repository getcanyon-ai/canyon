package runs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	asked "github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/getcanyon-ai/canyon/provisioners"
)

// patchRefused is step 1's own check, before the gate: a patch run whose identity paused, or stalled naming its
// app, after it was enqueued goes no further (design/PATCHING.md §4.2), nor one on a server patching is off on
// (CANYON_PATCHING; contracts/routes.md, Patch runs), nor one of an app Canyon no longer patches — the org's
// patching turned off, or the app's own platform.known-vuln (Patching). An identity not recorded refuses it too.
func (p *Pipeline) patchRefused(ctx context.Context, in Input) (bool, error) {
	if !p.Patches.Load() {
		return true, nil
	}
	var refused bool
	err := p.write(ctx, func(tx pgx.Tx) error {
		if on, err := p.Patching(ctx, tx, in.Project); err != nil || !on {
			refused = true
			return err
		}
		if in.Patch.Rebuild { // the org's rebuilds turned off since the plan
			if on, err := p.rebuildsOn(ctx, tx); err != nil || !on {
				refused = true
				return err
			}
		}
		err := tx.QueryRow(ctx, "SELECT paused_at IS NOT NULL OR $2 = ANY(coalesce(stalled_projects, '{}')) FROM patch_identities WHERE id = $1",
			in.Patch.Identity, in.Project).Scan(&refused)
		if errors.Is(err, pgx.ErrNoRows) {
			refused, err = true, nil
		}
		return err
	})
	return refused, err
}

// rebased is the rebase step's checkpoint: POST /rebase's answer, or its refusal's reason and message, or why
// it failed, as the executor said it.
type rebased struct {
	executor.RebaseResult
	Refused string `json:"refused,omitempty"`
	Message string `json:"message,omitempty"`
	Failed  string `json:"failed,omitempty"`
}

// rebase asks POST /rebase of the image the plan named onto its run image, again while the executor answers
// busy, for busyFor at most.
func (p *Pipeline) rebase(ctx context.Context, in Input) (rebased, error) {
	if p.Executor == nil {
		return rebased{Failed: "no executor configured"}, nil
	}
	until := time.Now().Add(busyFor)
	for {
		call, cancel := context.WithTimeout(ctx, buildCeiling)
		res, err := p.Executor.Rebase(call, executor.RebaseRequest{App: in.Project, Package: in.Package, Image: in.Patch.Image, RunImage: in.Patch.RunImage})
		cancel()
		var busy *executor.Busy
		var refused *executor.Refused
		switch {
		case errors.As(err, &busy) && time.Now().Add(max(busy.RetryAfter, time.Second)).Before(until):
			select {
			case <-ctx.Done():
				return rebased{}, ctx.Err()
			case <-time.After(max(busy.RetryAfter, time.Second)):
			}
			continue
		case errors.As(err, &refused):
			return rebased{Refused: refused.Reason, Message: schedulerSaid(refused.Message)}, nil
		case err != nil && ctx.Err() != nil:
			return rebased{}, err
		case err != nil:
			return rebased{Failed: schedulerSaid(err.Error())}, nil
		}
		return rebased{RebaseResult: res}, nil
	}
}

// proofOf is step 4 (contracts/routes.md, Patch runs): what of the image more than its run image changed — ""
// for nothing, else the first that differs or is missing of the layers above the run image, in order, the
// build metadata, how the app starts and the stack, in the line's words — and whether after is on another run
// image than the plan named.
func proofOf(before, after executor.RebaseFacts, runImage string) (what string, moved bool) {
	above := func(f executor.RebaseFacts) ([]string, bool) {
		i := slices.Index(f.DiffIDs, f.TopLayer)
		if f.TopLayer == "" || i < 0 {
			return nil, false
		}
		return f.DiffIDs[i+1:], true
	}
	was, ok := above(before)
	now, ok2 := above(after)
	switch {
	case !ok || !ok2 || !slices.Equal(was, now) || before.TopLayer == after.TopLayer:
		return "the app's own files", false
	case before.BuildMetadata == "" || before.BuildMetadata != after.BuildMetadata:
		return "the build settings", false
	case before.Config == "" || before.Config != after.Config:
		return "how the app starts", false
	case before.StackID == "" || before.StackID != after.StackID:
		return "the Ubuntu release it's built on", false
	}
	return "", after.RunImage != runImage
}

// changedWhat names the first part of now that differs from what the deploy row recorded, in the line's order
// (contracts/routes.md, Patch runs, the changed line): its outbound setting, who runs it, its front door, a
// secret, a resource — and its configuration where none of them does.
func changedWhat(was, now RunConfig) string {
	door := func(d *SavedDoor) string {
		if d == nil {
			return ""
		}
		return string(d.Door) + d.SetAt.UTC().Format(time.RFC3339Nano)
	}
	switch {
	case was.Egress != now.Egress:
		return "Its outbound setting"
	case was.RunBy != now.RunBy:
		return "Who runs it"
	case door(was.FrontDoor) != door(now.FrontDoor):
		return "Its front door"
	}
	var names []string
	for name := range was.Secrets {
		names = append(names, name)
	}
	for name := range now.Secrets {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		w, inWas := was.Secrets[name]
		n, inNow := now.Secrets[name]
		if inWas != inNow || w.Version != n.Version || !w.SetAt.Equal(n.SetAt) {
			return "The secret " + name
		}
	}
	resources := slices.Concat(was.Resources, now.Resources)
	slices.Sort(resources)
	for _, name := range resources {
		if slices.Contains(was.Resources, name) != slices.Contains(now.Resources, name) {
			return "The resource " + name
		}
	}
	return "Its configuration"
}

// regressionOf is step 6 (contracts/routes.md, Patch runs), from the rebased image's read against the replaced
// image's, both read at step 3 on one database: the first counted finding at or above high the replaced image
// lacked, nil for none, and the findings the plan named that the rebased image no longer has, in the plan's
// order. A finding is its package and vulnerability.
func regressionOf(was, now, fixes []PlatformFinding) (regressed *PlatformFinding, fixed []PlatformFinding) {
	has := func(in []PlatformFinding, f PlatformFinding) bool {
		return slices.ContainsFunc(in, func(g PlatformFinding) bool { return g.Name == f.Name && g.Vulnerability == f.Vulnerability })
	}
	for _, f := range now {
		if (f.Severity == "critical" || f.Severity == "high") && !has(was, f) {
			return &f, nil
		}
	}
	for _, f := range fixes {
		if !has(now, f) {
			fixed = append(fixed, f)
		}
	}
	return nil, fixed
}

// patchRead is the image-sbom step's checkpoint: whether both images' reads checked, and why not — Unread when
// it is the replaced image's that did not — and their counted findings, the rebased image's and the replaced
// image's, matched at the step on one database.
type patchRead struct {
	Checked  bool              `json:"checked"`
	Why      string            `json:"why"`
	Unread   bool              `json:"unread,omitempty"`
	Findings []PlatformFinding `json:"findings"`
	Was      []PlatformFinding `json:"was"`
	After    []heldPackage     `json:"after,omitempty"` // the plan's packages the rebased image holds, by name and class: what step 6's to reads
}

// heldPackage is one version of a package an image holds, and its class.
type heldPackage struct {
	Name    string `json:"name"`
	Class   string `json:"class"`
	Version string `json:"version"`
}

// readPatched takes the rebased image's SBOM and stores it as a deploy's is, then matches the covered layers of
// it and of the image it replaces at the step, on the executor's database, and reads both as the receipt does
// (contracts/routes.md, Patch runs, steps 3 and 6): a regression is judged on one database, never against an
// older read. A scan or a match that fails, or reads that did not check or were matched on two databases, are
// never zero findings: they are not checked, and why — never an earlier read that did.
func (p *Pipeline) readPatched(ctx context.Context, in Input, image string) (patchRead, error) {
	digest, was := ImageDigest(image), ImageDigest(in.Patch.Image)
	readOf := p.PlatformRead.Load()
	if digest == "" || was == "" || readOf == nil {
		return patchRead{Why: "no image scan recorded"}, nil
	}
	failed := func(why string, err error) (patchRead, error) {
		if ctx.Err() != nil {
			return patchRead{}, err
		}
		log.Printf("runs: run %s (%s): the patched image's SBOM or its match: %v", in.Run, in.Project, err)
		return patchRead{Why: why}, nil
	}
	// recorded first, so the rescan's sweep keeps the scan stored below while this run reads it (#2587)
	if _, err := p.Pool.Exec(ctx, "UPDATE runs SET rebased_image = $2 WHERE id = $1", in.Run, image); err != nil {
		return failed("could not check: its image scan did not finish", err)
	}
	res, err := p.askScan(ctx, executor.ScanRequest{Tool: "trivy", Target: "image", App: in.Project, Image: image}, nil)
	if err == nil {
		var img storedImage
		if img, err = imageOf(res); err == nil {
			err = storeImage(ctx, p.Pool, in.Project, digest, img)
		}
	}
	if err != nil {
		return failed("could not check: its image scan did not finish", err)
	}
	app, err := p.readsApp(ctx, in.Project) // the patched image's app layers too, while its deps.known-vuln reads the built app
	if err != nil {
		return failed("could not check: its last rescan did not finish", err)
	}
	matched := map[string]bool{} // a layer both images hold is matched once
	var images []rescanImage     // what was matched here: one database is judged over its platform layers alone
	for _, d := range []string{digest, was} {
		m, err := p.toMatch(ctx, in.Project, d, app)
		var parts []string
		if err == nil && m != nil {
			parts, err = p.matchImage(ctx, *m, matched)
			images = append(images, *m)
		}
		if err != nil {
			return failed("could not check: its last rescan did not finish", err)
		}
		for _, part := range parts {
			matched[part] = true
		}
	}
	switch compared, one, err := oneDatabase(ctx, p.Pool, in.Project, images); {
	case err != nil:
		return patchRead{Why: "could not check: the two images were matched on different databases"}, err
	case !compared: // nothing matched to compare, or a layer with no match: never a pass
		return patchRead{Why: "could not check: the image has not been matched yet"}, nil
	case !one:
		return patchRead{Why: "could not check: the two images were matched on different databases"}, nil
	}
	now, err := readOf(ctx, in.Project, digest)
	if err != nil || !now.Checked {
		return patchRead{Why: now.Why}, err
	}
	before, err := readOf(ctx, in.Project, was)
	if err != nil || !before.Checked {
		return patchRead{Why: before.Why, Unread: true}, err
	}
	pkgs, ok, err := packagesOf(ctx, p.Pool, in.Project, digest) // the scan stored above: what each fixed package is now
	if err != nil || !ok {
		return failed("could not check: its image scan did not finish", cmp.Or(err, errors.New("its stored scan does not read")))
	}
	var after []heldPackage
	for _, pkg := range pkgs {
		h := heldPackage{Name: pkg.FullName(), Class: pkg.Class, Version: pkg.Version}
		if !slices.Contains(after, h) && slices.ContainsFunc(in.Patch.Fixes, func(f PlatformFinding) bool { return f.Name == h.Name && f.Class == h.Class }) {
			after = append(after, h)
		}
	}
	return patchRead{Checked: true, Findings: now.Findings, Was: before.Findings, After: after}, nil
}

// fixedOf is step 6's fixed list (contracts/routes.md, Patch runs): each finding the plan named that the patched image no
// longer has, its to the versions of its package, by name and class, the patched image holds (after), in layer order —
// "removed" when it holds none — and its class a rebuild's alone: a rebase fixes os packages alone.
func fixedOf(fixes []PlatformFinding, after []heldPackage, rebuild bool) []PatchFixed {
	out := make([]PatchFixed, 0, len(fixes))
	for _, f := range fixes {
		var to []string
		for _, h := range after {
			if h.Name == f.Name && h.Class == f.Class && !slices.Contains(to, h.Version) {
				to = append(to, h.Version)
			}
		}
		x := PatchFixed{Name: f.Name, From: f.Version, To: cmp.Or(strings.Join(to, ", "), "removed"), Vulnerability: f.Vulnerability}
		if rebuild {
			x.Class = f.Class
		}
		out = append(out, x)
	}
	return out
}

// fixedLines are step 6's lines fixed, one per package the patch moved, its vulnerabilities in the plan's order.
func fixedLines(fixed []PatchFixed) []string {
	var moves []string
	vulns := map[string][]string{}
	for _, f := range fixed {
		move := f.Name + " " + f.From + " → " + f.To
		if _, ok := vulns[move]; !ok {
			moves = append(moves, move)
		}
		vulns[move] = append(vulns[move], f.Vulnerability)
	}
	lines := make([]string, len(moves))
	for i, move := range moves {
		lines[i] = "Fixed: " + move + " (" + strings.Join(vulns[move], ", ") + ")"
	}
	return lines
}

// oneDatabase is whether every platform layer of project's images, the layers readPatched just matched, has a
// match, compared, and whether they were last matched on one database build: a regression is judged only between
// reads of one database, never a layer of either left on another or unmatched, and with no layer to compare there
// is no comparison. An app layer deps.known-vuln alone reads, matched before on another database or by another
// project sharing its slice, is none of the patch's (#2613's review).
func oneDatabase(ctx context.Context, q querier, project string, images []rescanImage) (compared, one bool, err error) {
	var digests, diffIDs []string // each (digest, diff ID) pair once: two images may hold one layer, and an image be named twice
	pairs := map[[2]string]bool{}
	for _, img := range images {
		for _, l := range img.Layers {
			if pair := [2]string{img.Digest, l.DiffID}; !pairs[pair] {
				pairs[pair] = true
				digests, diffIDs = append(digests, img.Digest), append(diffIDs, l.DiffID)
			}
		}
	}
	if len(pairs) == 0 {
		return false, false, nil
	}
	var n, layers int
	err = q.QueryRow(ctx, `SELECT count(DISTINCT newest), count(*) FROM (SELECT DISTINCT ON (l.digest, l.diff_id) m.db_built_at AS newest
		FROM image_layers l JOIN part_matches m USING (part_sha) JOIN unnest($2::text[], $3::text[]) AS c (digest, diff_id) ON l.digest = c.digest AND l.diff_id = c.diff_id
		WHERE l.project_id = $1 ORDER BY l.digest, l.diff_id, m.db_built_at DESC) n`,
		project, digests, diffIDs).Scan(&n, &layers)
	return layers == len(pairs), n == 1, err
}

// configCheck is the run-config step's checkpoint: what changed, "" for nothing, and what the /run carries
// then — the spec preview's row recorded, never the host's now (design/EGRESS.md §2.5), and the fingerprint it
// recorded, which the /run must carry again.
type configCheck struct {
	Changed   string `json:"changed,omitempty"`
	Spec      Egress `json:"spec"`
	Expect    string `json:"expect,omitempty"`
	Unchecked bool   `json:"unchecked,omitempty"` // the front door has not passed the probe: no /run behind it (#2438); absent from a step journaled before
}

// checkConfig is step 5 (contracts/routes.md, Patch runs): the fingerprint the patch's /run would carry, built
// as RunApp builds it, against the one preview's row recorded. Equality is on the stored fingerprint; the
// stored parts only name what changed. A row that recorded none, whose parts or spec do not read, or that no
// longer names the image the plan named, has changed (fail closed).
func (p *Pipeline) checkConfig(ctx context.Context, in Input) (configCheck, error) {
	var spec *Egress
	var stored []byte
	var sha *string
	err := p.Pool.QueryRow(ctx, "SELECT egress, run_config, run_config_sha256 FROM deploys WHERE project_id = $1 AND env = 'preview' AND image = $2 AND run_by = 'canyon'",
		in.Project, in.Patch.Image).Scan(&spec, &stored, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return configCheck{Changed: "Its configuration"}, nil
	}
	if err != nil {
		return configCheck{}, err
	}
	if !ConfigRecorded(spec, stored, sha) {
		return configCheck{Changed: "Its configuration"}, nil
	}
	var was RunConfig
	_ = json.Unmarshal(stored, &was) // it reads: ConfigRecorded said so
	now, _, _, err := p.runConfig(ctx, in.Project, "preview", *spec, false)
	if errors.Is(err, errDoorUnchecked) {
		return configCheck{Unchecked: true}, nil
	}
	if err != nil {
		return configCheck{}, err
	}
	fingerprint, err := now.Fingerprint()
	if err != nil {
		return configCheck{}, err
	}
	check := configCheck{Spec: *spec, Expect: *sha}
	if fingerprint != *sha {
		check.Changed = changedWhat(was, now)
	}
	return check, nil
}

// patchDeployed is the patch's deploy step's checkpoint: the URL, with what the app answered and what its /run
// carried — names, versions and times, never a value — or what changed at the /run, or that its review no longer
// clears it, or that it may no longer be patched, or that a /run that began may have run, unknown, or that the
// app did not stay up, or Canyon's own failure at the /run.
type patchDeployed struct {
	URL        string        `json:"url,omitempty"`
	Answered   *asked.Answer `json:"answered,omitempty"`
	Carried    *Carried      `json:"carried,omitempty"`
	Recorded   bool          `json:"recorded,omitempty"`
	Changed    string        `json:"changed,omitempty"`
	NotCleared bool          `json:"not_cleared,omitempty"`
	Stopped    bool          `json:"stopped,omitempty"`
	MayRun     bool          `json:"may_run,omitempty"`
	Exited     bool          `json:"exited,omitempty"`
	Failed     string        `json:"failed,omitempty"`
}

// PatchFixed is one of the patch audit row's fixed[] (contracts/routes.md, Patch runs, step 7).
type PatchFixed struct {
	Name          string `json:"name"`
	From          string `json:"from"`
	To            string `json:"to"`
	Vulnerability string `json:"vulnerability"`
	Class         string `json:"class,omitempty"` // a rebuild's alone: a rebase fixes os packages alone
}

// deployPatch is step 7's /run (contracts/routes.md, Patch runs): of the rebased image with the spec preview's
// row recorded, keeping the image it replaces (keep: the row names it until recordPatch writes). Its review must
// still clear it, read live here so a replay reads it again. A /run not yet begun begins here, in one
// transaction with what may stop it (beginRun): it is marked begun only when it may go. A replay that finds it
// begun — the /run may have happened — asks the executor what preview runs before it reads any stop: the
// rebased image running is recorded as it is; one that cannot tell may have run (MayRun); another image running
// means that /run never took, and it runs again only as one not begun would (mayRunAgain). Its run-config is compared
// inside RunApp, before the executor is touched. A replay that finds the patch recorded answers its URL.
func (p *Pipeline) deployPatch(ctx context.Context, in Input, image string, check configCheck) (patchDeployed, error) {
	var url string
	var begun bool
	err := p.Pool.QueryRow(ctx, `SELECT coalesce((SELECT data->>'url' FROM timeline_rows WHERE run_id = $1 AND dedupe = 'deploy:preview'), ''),
		deploy_started_at IS NOT NULL FROM runs WHERE id = $1`, in.Run).Scan(&url, &begun)
	switch {
	case err != nil:
		return patchDeployed{}, err
	case url != "":
		return patchDeployed{URL: url, Recorded: true}, nil
	case p.Executor == nil:
		return patchDeployed{Failed: "no executor configured"}, nil
	case !p.clears(ctx, in):
		return patchDeployed{NotCleared: true}, nil
	}
	if begun {
		running, err := p.Executor.Running(ctx, in.Project, "preview")
		switch {
		case err != nil && !errors.Is(err, executor.ErrNoContainer):
			log.Printf("runs: run %s (%s): what preview runs after its patch's /run began: %v", in.Run, in.Project, err)
			return patchDeployed{MayRun: true}, nil
		case running == image: // what that /run carried is not known: the row records none, and a later patch fails closed on it
			door, _, err := p.frontDoor(ctx, false)
			if err == nil {
				url, err = p.appURL(door, in.Project, "preview")
			}
			return patchDeployed{URL: url}, err
		}
		if ok, err := p.mayRunAgain(ctx, in); err != nil || !ok { // that /run never took: it runs again only as one not begun would
			return patchDeployed{Stopped: !ok}, err
		}
	} else {
		if h := p.patching.Load(); h != nil {
			(*h)()
		}
		if ok, err := p.beginRun(ctx, in); err != nil || !ok {
			return patchDeployed{Stopped: !ok}, err
		}
	}
	url, ran, carried, err := p.RunApp(ctx, in.Run, in.Project, "preview", image, check.Spec, check.Expect)
	var exited *executor.Exited
	switch {
	case errors.Is(err, ErrConfigChanged):
		var stored []byte
		var was RunConfig
		if err := p.Pool.QueryRow(ctx, "SELECT run_config FROM deploys WHERE project_id = $1 AND env = 'preview'", in.Project).Scan(&stored); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return patchDeployed{}, err
		}
		if json.Unmarshal(stored, &was) != nil {
			return patchDeployed{Changed: "Its configuration"}, nil
		}
		return patchDeployed{Changed: changedWhat(was, carried.Config)}, nil
	case errors.As(err, &exited): // the app exited: a failed start, whether or not its lines are written
		if err := p.exitedLines(ctx, in, exited); err != nil {
			log.Printf("runs: run %s (%s): the patched app's last lines: %v", in.Run, in.Project, err)
		}
		return patchDeployed{Exited: true}, nil
	case err != nil && ctx.Err() != nil:
		return patchDeployed{}, err
	case err != nil:
		return patchDeployed{Failed: schedulerSaid(err.Error())}, nil
	}
	return patchDeployed{URL: url, Answered: p.answered(ctx, in.Project, "preview", url, ran), Carried: &carried}, nil
}

// mayRunAgain is whether a patch whose /run began, and never took — preview runs another image — may run again,
// read as beginRun reads it. When it may not, its /run is marked not begun again, so it ends as one stopped
// before its /run; when it may, the mark stands.
func (p *Pipeline) mayRunAgain(ctx context.Context, in Input) (bool, error) {
	ok := false
	err := p.write(ctx, func(tx pgx.Tx) error {
		var err error
		if ok, err = p.mayGo(ctx, tx, in); err != nil || ok {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE runs SET deploy_started_at = NULL WHERE id = $1", in.Run)
		return err
	})
	return ok, err
}

// mayGo is whether a patch may go to its /run, read live in tx under the ledger lock: the run neither ended nor
// asked to end, Canyon patching the app (Patching), and its identity neither paused nor stalled naming its app.
func (p *Pipeline) mayGo(ctx context.Context, tx pgx.Tx, in Input) (bool, error) {
	if on, err := p.Patching(ctx, tx, in.Project); err != nil || !on {
		return false, err
	}
	if in.Patch.Rebuild {
		if on, err := p.rebuildsOn(ctx, tx); err != nil || !on {
			return false, err
		}
	}
	var may bool
	err := tx.QueryRow(ctx, `SELECT NOT (i.paused_at IS NOT NULL OR $2 = ANY(coalesce(i.stalled_projects, '{}'))) AND r.ended_at IS NULL AND r.cancel_requested_at IS NULL
		FROM patch_identities i, runs r WHERE i.id = $1 AND r.id = $3`, in.Patch.Identity, in.Project, in.Run).Scan(&may)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return may, err
}

// beginRun marks a patch's /run begun, under the ledger lock a person's start and an undeploy take, so from here
// a start is 409 and an undeploy records a stop on it (contracts/routes.md, Patch runs) — only when it may go,
// read live in the same transaction: the run neither ended nor asked to end, patching on on this server, and
// its identity neither paused nor stalled naming its app. It answers whether it may go.
func (p *Pipeline) beginRun(ctx context.Context, in Input) (bool, error) {
	ok := false
	err := p.write(ctx, func(tx pgx.Tx) error {
		var err error
		if ok, err = p.mayGo(ctx, tx, in); err != nil || !ok {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE runs SET deploy_started_at = now() WHERE id = $1", in.Run)
		return err
	})
	return ok, err
}

// recordPatch records a patch preview runs, once (contracts/routes.md, Patch runs, step 7): preview's row takes
// the rebased image, the patch run as its gate run and what the /run carried, keeping its package, label,
// deployed_by and buildpacks, its build digest and values HMAC none, as no build made the image; the run keeps
// the spec it ran with, which a promotion of it carries (egressFor); then its deploy.completed row and the patch
// preview audit row, unless an undeploy recorded a stop on the run meanwhile. The run keeps preview's row as the
// patch found it, which a revert puts back (patch_replaced). A replay finds it recorded. live is whether it went
// live: false when a stop is pending on the run, which carries it out as it ends, and nothing is watched.
func (p *Pipeline) recordPatch(ctx context.Context, in Input, image string, d patchDeployed, spec Egress, fixed []PatchFixed, same *proved) (live bool, err error) {
	err = p.write(ctx, func(tx pgx.Tx) error {
		live = false         // each attempt reads it afresh
		if p.Access == nil { // as a deploy's /run: Docker's executor started it on a network of its spec alone
			if err := RecordNetworkTx(ctx, tx, in.Project, spec.Via == "canyon"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET patch_replaced = (SELECT to_jsonb(d) FROM deploys d WHERE d.project_id = $2 AND d.env = 'preview' AND d.image = $3 AND d.run_by = 'canyon')
			WHERE id = $1 AND patch_replaced IS NULL`, in.Run, in.Project, in.Patch.Image); err != nil {
			return err
		}
		var config *RunConfig
		var fingerprint *string
		if d.Carried != nil {
			config, fingerprint = &d.Carried.Config, &d.Carried.Fingerprint
		}
		// a rebase keeps the values-only HMAC and the builder, as it changes no build value; a rebuild's row takes what its build recorded
		tag, err := tx.Exec(ctx, `UPDATE deploys d SET image = $3, url = $4, gate_run = $5, deployed_at = now(),
			build_digest = CASE WHEN $9 THEN r.build_digest END, values_hmac = CASE WHEN $9 THEN r.values_hmac ELSE d.values_hmac END,
			values_key = CASE WHEN $9 THEN r.values_key ELSE d.values_key END, buildpacks = CASE WHEN $9 THEN r.buildpacks ELSE d.buildpacks END,
			builder = CASE WHEN $9 THEN r.builder ELSE d.builder END,
			run_started_at = r.created_at, answered = $6, run_config = $7, run_config_sha256 = $8, run_config_at = now(), egress_roots = coalesce($10::text[], d.egress_roots),
			container_started_at = $11
			FROM runs r WHERE r.id = $5 AND d.project_id = $1 AND d.env = 'preview' AND d.image = $2 AND d.run_by = 'canyon'`,
			in.Project, in.Patch.Image, image, d.URL, in.Run, d.Answered, config, fingerprint, in.Patch.Rebuild, trustedRoots(d.Carried), startedOf(d.Carried))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var recorded bool // a replay of this transaction, committed before
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deploys WHERE project_id = $1 AND env = 'preview' AND image = $2 AND gate_run = $3),
				EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $3 AND dedupe = 'deploy:preview')`, in.Project, image, in.Run).Scan(&recorded, &live); err != nil || recorded {
				return err
			}
			return errors.New("preview's row no longer names the image the patch replaces")
		}
		newer := "" // a rebuild's: what it was rebuilt with, which its approval's proof names
		if same != nil {
			newer = same.Newer
		}
		if _, err := tx.Exec(ctx, "UPDATE runs SET egress = $2, rebuilt_with = nullif($3, '') WHERE id = $1", in.Run, spec, newer); err != nil {
			return err
		}
		var stopping bool // an undeploy asked the run to stop (contracts/routes.md, The brake): the row says what runs, and nothing says it went live
		if err := tx.QueryRow(ctx, "SELECT stop_requested_at IS NOT NULL FROM runs WHERE id = $1", in.Run).Scan(&stopping); err != nil || stopping {
			return err
		}
		digest, replaces := ImageDigest(image), ImageDigest(in.Patch.Image)
		data := map[string]any{"env": "preview", "url": d.URL, "package": in.Package, "image_digest": digest}
		if d.Answered != nil {
			data["answered"] = d.Answered
		}
		if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.DeployCompleted, "deploy:preview", data); err != nil {
			return err
		}
		live = true
		detail := map[string]any{"package": in.Package, "run": in.Run, "replaces": replaces, "image": digest, "proof": "layers", "fixed": fixed}
		if same != nil {
			detail["proof"], detail["same_inputs"] = "same_inputs", map[string]any{"values_key": same.ValuesKey, "buildpacks": same.Buildpacks}
		}
		return events.AuditTx(ctx, tx, PatchActor, "patch preview", in.Project, in.Run, detail)
	})
	return live, err
}

// exitedLines writes the app's last lines, then the executor's reason, of a start that did not stay up. A
// replay runs /run again, and the app's new start answers its own lines, as many or as few as it printed this
// time: the keys are positions, so the block the first attempt wrote stands whole and the replay's is dropped,
// rather than their positions interleaving.
func (p *Pipeline) exitedLines(ctx context.Context, in Input, exited *executor.Exited) error {
	var lines []string
	if exited.Output != "" {
		lines = strings.Split(strings.TrimSuffix(exited.Output, "\n"), "\n")
	}
	return p.write(ctx, func(tx pgx.Tx) error {
		var written bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM timeline_rows WHERE run_id = $1 AND dedupe = 'start:0')", in.Run).Scan(&written); err != nil || written {
			return err
		}
		for i, l := range append(lines, exited.Reason) {
			if _, err := events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, fmt.Sprintf("start:%d", i), map[string]any{"line": l}); err != nil {
				return err
			}
		}
		return nil
	})
}

// patch is a patch run past its gate (contracts/routes.md, Patch runs): steps 2 to 7 in order, each ending the
// run with nothing deployed when it fails, its line then the last build.log row and the app's outcome in its
// identity's waves recorded first (RecordPatchOutcome; a refusal for good RecordPatchRefusal). cleared is
// whether its gate let it through: clean, or blocked with every blocking finding covered by an approval of
// someone other than the author of the run whose image it rebases. A patch that deployed is watched in a workflow
// of its own (watchPatch), which records its outcome and ends the run.
func (p *Pipeline) patch(ctx dbos.Context, in Input, cleared bool, state string, blocking, warnings int) (Result, error) {
	line := func(key, text string) error {
		_, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			return struct{}{}, p.record(c, in, provisioners.Key{}, "", "", "", "", "patch:"+key, text)
		}, dbos.WithStepName("patch line"))
		return err
	}
	end := func(outcome, state, reason, notPlatform, key, text string) (Result, error) {
		if state == "failed" {
			log.Print(patchFailedLine(in.Run, in.Project, reason, notPlatform))
		}
		if text != "" {
			if err := line(key, text); err != nil {
				return Result{}, err
			}
		}
		if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
			if outcome == PatchRefused {
				return struct{}{}, nil
			}
			return struct{}{}, p.RecordPatchOutcome(c, in.Patch.Identity, in.Project, outcome)
		}, dbos.WithStepName("patch outcome")); err != nil {
			return Result{}, err
		}
		return p.finishIn(ctx, in, state, reason, "", notPlatform, blocking, warnings)
	}
	stopped := func(err error) bool { return ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled) }
	if !cleared {
		return end(PatchFailed, state, "", "", "", "")
	}
	version, err := dbos.RunAsStep(ctx, func(c context.Context) (string, error) { return versionOf(c, p.Pool, in.Project, in.Package) }, dbos.WithStepName("version"))
	if err != nil {
		return Result{}, err
	}
	unfinished := func(outcome string, err error) (Result, error) { // Canyon's own failure: its cause logged under the run's id alone, its line says it failed
		log.Printf("runs: run %s (%s): patching: %v", in.Run, in.Project, err)
		return end(outcome, "failed", "error", "", "unfinished", "Canyon could not finish patching "+version+", so it did not deploy it")
	}
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
		return p.interrupted(ctx, in, err, blocking, warnings)
	}
	var image string // the patched image: the rebase's, or the rebuild's (contracts/routes.md, A rebuild patch, step 2)
	var r rebased
	if in.Patch.Rebuild {
		var done bool
		var res Result
		if image, done, res, err = p.rebuild(ctx, in, version, end, unfinished, blocking, warnings); done || err != nil {
			return res, err
		}
	} else {
		began, err := p.began(ctx, in, "rebase")
		if err != nil {
			return Result{}, err
		}
		r, err = dbos.RunAsStep(ctx, func(c context.Context) (rebased, error) { return p.rebase(c, in) }, dbos.WithStepName("rebase"))
		if err != nil {
			if stopped(err) {
				return Result{}, err
			}
			return unfinished(PatchFailed, err)
		}
		if err := p.ended(ctx, in, "rebase", began, r.Refused == "" && r.Failed == ""); err != nil {
			return Result{}, err
		}
		switch {
		case r.Refused == "run_image_moved":
			return end(PatchFailed, "failed", "error", "", "moved", movedLine(version))
		case r.Refused != "":
			if _, err := dbos.RunAsStep(ctx, func(c context.Context) (struct{}, error) {
				return struct{}{}, RecordPatchRefusal(c, p.Pool, in.Patch.Identity, in.Project, ImageDigest(in.Patch.Image), in.Patch.RunImage, r.Refused)
			}, dbos.WithStepName("patch refusal")); err != nil {
				return Result{}, err
			}
			log.Printf("runs: run %s (%s): the executor refused the patch: %s: %s", in.Run, in.Project, r.Refused, r.Message)
			return end(PatchRefused, "failed", "error", "", "refused", refusedLine(version, r.Refused))
		case r.Failed != "":
			return end(PatchFailed, "failed", "error", "", "rebase", "Canyon could not apply its fix to "+version+": "+r.Failed+", so it did not deploy it")
		}
		image = r.Image
	}
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop {
		return p.interrupted(ctx, in, err, blocking, warnings)
	}
	began, err := p.began(ctx, in, "image-sbom")
	if err != nil {
		return Result{}, err
	}
	read, err := dbos.RunAsStep(ctx, func(c context.Context) (patchRead, error) { return p.readPatched(c, in, image) }, dbos.WithStepName("image-sbom"))
	if err != nil && stopped(err) {
		return Result{}, err
	}
	if perr := p.ended(ctx, in, "image-sbom", began, err == nil && read.Checked); perr != nil {
		return Result{}, perr
	}
	switch {
	case err != nil:
		return unfinished(PatchFailed, err)
	case read.Unread:
		return end(PatchFailed, "failed", "error", "", "unread", "Canyon could not check the image "+version+" runs for vulnerabilities: "+strings.TrimPrefix(read.Why, couldNotCheck)+", so it did not deploy the patch")
	case !read.Checked:
		return end(PatchFailed, "failed", "error", "", "unchecked", "Canyon could not check the patched image for vulnerabilities: "+strings.TrimPrefix(read.Why, couldNotCheck)+", so it did not deploy it")
	}
	regressed, fixes := regressionOf(read.Was, read.Findings, in.Patch.Fixes) // step 6's, read once: a rebuild's line names what it fixed
	var same *proved                                                          // a rebuild's same_inputs (step 4), in place of a rebase's proof
	if in.Patch.Rebuild {
		s, err := dbos.RunAsStep(ctx, func(c context.Context) (proved, error) { return p.proveSame(c, in, image) }, dbos.WithStepName("same inputs"))
		if err != nil {
			if stopped(err) {
				return Result{}, err
			}
			return unfinished(PatchFailed, err)
		}
		if s.Differs != "" {
			key, text := notSameLine(s, version)
			return end(PatchFailed, "failed", "not_same_inputs", s.Differs, key, text)
		}
		if s.Newer = RebuildNewer(fixes); len(fixes) == 0 { // it fixed nothing: what it was rebuilt for
			s.Newer = RebuildNewer(in.Patch.Fixes)
		}
		if err := line("same", "Rebuilt with "+s.Newer+": "+version+"'s code, build settings and dependencies are the same"); err != nil {
			return Result{}, err
		}
		same = &s
	} else {
		what, moved := proofOf(r.Before, r.After, in.Patch.RunImage)
		switch {
		case what != "":
			return end(PatchFailed, "failed", "not_platform_only", "layers_changed", "layers", "More than Canyon's system libraries changed: "+what+" changed too, so Canyon did not deploy it")
		case moved:
			return end(PatchFailed, "failed", "error", "", "moved", movedLine(version))
		}
		if err := line("proof", "Only the system libraries Canyon adds changed: "+version+"'s own code, its build settings and how it starts are the same"); err != nil {
			return Result{}, err
		}
	}
	check, err := dbos.RunAsStep(ctx, func(c context.Context) (configCheck, error) { return p.checkConfig(c, in) }, dbos.WithStepName("run config"))
	if err != nil {
		if stopped(err) {
			return Result{}, err
		}
		return unfinished(PatchFailed, err)
	}
	if check.Unchecked { // D5's line, as a deploy behind such a door ends (doorSet): never a raw error
		return end(PatchFailed, "failed", "error", "", "front-door", doorUnchecked)
	}
	if check.Changed != "" {
		return end(PatchFailed, "failed", "not_platform_only", "config_changed", "changed", changedLine(check.Changed, version))
	}
	if err := line("unchanged", "What "+version+" runs with is unchanged since it was deployed"); err != nil {
		return Result{}, err
	}
	switch {
	case regressed != nil:
		return end(PatchFailed, "failed", "patch_regressed", "", "regressed", fmt.Sprintf("The patched image has %s %s (%s, %s), which %s did not: Canyon did not deploy it",
			regressed.Name, regressed.Version, regressed.Vulnerability, regressed.Severity, version))
	case len(fixes) == 0 && same != nil:
		return end(PatchFailed, "failed", "patch_ineffective", "", "rebuild-ineffective", "Canyon rebuilt "+version+" with "+same.Newer+", but the rebuild fixes none of its findings, so it did not deploy it")
	case len(fixes) == 0:
		return end(PatchFailed, "failed", "patch_ineffective", "", "ineffective", "The patched image still has every package the patch was for: Canyon did not deploy it")
	}
	fixed := fixedOf(fixes, read.After, same != nil)
	for i, text := range fixedLines(fixed) {
		if err := line(fmt.Sprintf("fixed:%d", i), text); err != nil {
			return Result{}, err
		}
	}
	if err := line("no-new", "No new vulnerability at high or above"); err != nil {
		return Result{}, err
	}
	if stop, err := p.cancelled(ctx, in, false); err != nil || stop { // its /run begins in the deploy step (beginRun)
		return p.interrupted(ctx, in, err, blocking, warnings)
	}
	if began, err = p.began(ctx, in, "start"); err != nil {
		return Result{}, err
	}
	d, err := dbos.RunAsStep(ctx, func(c context.Context) (patchDeployed, error) { return p.deployPatch(c, in, image, check) }, dbos.WithStepName("deploy"))
	if err != nil && stopped(err) {
		return Result{}, err
	}
	if perr := p.ended(ctx, in, "start", began, err == nil && d.URL != ""); perr != nil {
		return Result{}, perr
	}
	switch {
	case err != nil:
		return unfinished(PatchFailed, err)
	case d.NotCleared, d.Stopped:
		return p.interrupted(ctx, in, nil, blocking, warnings)
	case d.Changed != "":
		return end(PatchFailed, "failed", "not_platform_only", "config_changed", "changed", changedLine(d.Changed, version))
	case d.Exited: // the app did not stay up: a failed start, which the breaker counts
		return end(PatchFailedStart, "failed", "start", "", "", "")
	case d.Failed != "": // Canyon's or the executor's own failure, never the app's: no failed start
		return unfinished(PatchFailed, errors.New(d.Failed))
	case d.MayRun: // its /run began and preview may run it: never read as not deployed
		return end(PatchFailed, "failed", "error", "", "unrecorded", unrecordedLine(version))
	}
	live := d.Recorded // a replay that found its deploy.completed row: it went live
	if !d.Recorded {   // preview runs the rebased image: it is recorded, tried again before the run says it could not be
		live, err = dbos.RunAsStep(ctx, func(c context.Context) (bool, error) {
			return p.recordPatch(c, in, image, d, check.Spec, fixed, same)
		},
			dbos.WithStepName("record patch"), dbos.WithStepMaxRetries(recordRetries))
		if err != nil && stopped(err) {
			return Result{}, err
		}
		if err != nil {
			log.Printf("runs: run %s (%s): recording the patch: %v", in.Run, in.Project, err)
			return end(PatchFailed, "failed", "error", "", "unrecorded", unrecordedLine(version))
		}
	}
	if !live { // a stop is pending on the run: it carries it out as it ends, and nothing is left to watch
		return p.finish(ctx, in, state, "", blocking, warnings)
	}
	// A gate-run canyon-v10 journaled watched inline, its "watch until" step where the enqueue below now stands: handOver
	// gives it to this release once no v10 server runs, and it replays its watch inline, as journaled. The journal is read
	// outside the workflow (p.dc, never ctx: inside it the read is a step of its own and shifts every step after it).
	steps, err := dbos.GetWorkflowSteps(p.dc, in.Run, dbos.WithStepsLoadOutput(false))
	if err != nil {
		return Result{}, err
	}
	if slices.ContainsFunc(steps, func(s dbos.StepInfo) bool { return s.StepName == "watch until" }) {
		return p.watch(ctx, in, blocking, warnings)
	}
	// its watch goes on in a workflow of its own, which ends the run, and this one ends here, freeing its slot (#2567)
	if _, err := dbos.Enqueue[any](ctx, Queue, WatchWorkflow, watchInput{Input: in, Blocking: blocking, Warnings: warnings}, dbos.WithEnqueueWorkflowID(watchID(in.Run))); err != nil {
		return Result{}, err
	}
	return Result{State: "running", Blocking: blocking, Warnings: warnings}, nil
}

// unrecordedLine is a patch run's line unrecorded (contracts/routes.md, Patch runs).
func unrecordedLine(version string) string {
	return "Preview runs the patched " + version + ", but Canyon could not record it: deploy again so Canyon records what runs there"
}

// recordRetries is how often a patch that runs is recorded again before its run says it could not be.
const recordRetries = 5

// refusedLine is a patch run's line refused (contracts/routes.md, Patch runs): the executor's 409 code in words a
// developer knows, its message for the server log alone (#2614). A code this build does not know names no reason.
func refusedLine(version, code string) string {
	why := map[string]string{
		"not_rebasable":     ": how it was built doesn't let Canyon swap in new system libraries",
		"another_run_image": ": it was built on different system libraries from the ones Canyon fixes",
		"another_stack":     ": it was built on another Ubuntu release",
	}[code]
	return "Canyon can't patch " + version + why + ", so its next deploy picks up the fix"
}

// couldNotCheck opens an image read's why that did not check (contracts/routes.md, platform.known-vuln's row); the
// lines unchecked and unread say the reason after it.
const couldNotCheck = "could not check: "

// patchFailedLine is the server's line for a patch run that ended failed (#265): the run, its app, and its
// failure_reason with the not_platform_only sub-reason where it has one, as the runs row records them.
func patchFailedLine(run, project, reason, notPlatform string) string {
	line := fmt.Sprintf("runs: run %s (%s): the patch run failed: %s", run, project, cmp.Or(reason, "no reason recorded"))
	if notPlatform != "" {
		line += " (" + notPlatform + ")"
	}
	return line
}

// movedLine and changedLine are a patch run's lines moved and changed (contracts/routes.md, Patch runs).
func movedLine(version string) string {
	return "A newer fix from Canyon came out while it patched " + version + ", so Canyon did not deploy it: Canyon tries again at its next check"
}

func changedLine(what, version string) string {
	return what + " changed since " + version + " was deployed: deploy again to apply it, then Canyon patches."
}
