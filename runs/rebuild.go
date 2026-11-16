package runs

import (
	"cmp"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"slices"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/trivy"
)

// A rebuild patch (design/PATCHING.md §5; contracts/routes.md, Patch runs, A rebuild patch) builds the package
// preview runs again on the builder Canyon names now, for a counted finding of class runtime or lifecycle, which no
// rebase fixes. Its proof is same_inputs: the two builds had the same inputs, so the rebuild differs from the image
// it replaces only by what the newer builder brought.

// Which input of a rebuild differed from the build preview runs (contracts/events.md, not_same_inputs), in the order
// same_inputs compares them.
const (
	inputPackage      = "package"
	inputValues       = "values"
	inputKeyRotated   = "key_rotated"
	inputBuildpacks   = "buildpacks"
	inputDependencies = "dependencies"
)

// appDep is one of an image's own packages, as same_inputs compares it: a library of any class but Canyon's — os, runtime
// and lifecycle — so class app and class unknown, whose packages a go build, NuGet or another buildpack put there:
// its type, name, version and purl.
type appDep struct {
	Type, Name, Version, PURL string
}

func (d appDep) key() string { return d.Type + "\x00" + d.Name + "\x00" + d.Version + "\x00" + d.PURL }

// buildInputs is what one build recorded that same_inputs compares: its package and that package's tree hash — its
// lockfiles are in its tree, so the tree hash is their digest — its values-only HMAC and the key version it was made
// under (none: Values ""), the buildpack ids that ran in order (BuildpacksRead false when none were recorded), and its
// image's own packages (appDep; DepsRead false when the image's SBOM does not read).
type buildInputs struct {
	Package, Tree  string
	Values         string
	Key            int
	Buildpacks     []string
	BuildpacksRead bool
	Deps           []appDep
	DepsRead       bool
}

// sameInputs is the proof same_inputs (contracts/routes.md, A rebuild patch, step 4): "" when the rebuild's inputs
// are the build preview runs', else the first input that differs, in order, and for dependencies the first package
// one build has and the other lacks. Fail closed: what either build did not record differs. Values made under two key
// versions are never compared: a rotated key cannot tell equal values from different ones (key_rotated).
func sameInputs(was, now buildInputs) (differs, change string) {
	switch {
	case was.Package == "" || was.Package != now.Package || was.Tree == "" || was.Tree != now.Tree:
		return inputPackage, ""
	case was.Values == "" || was.Key == 0 || now.Values == "" || now.Key == 0:
		return inputValues, ""
	case was.Key != now.Key:
		return inputKeyRotated, ""
	case !hmac.Equal([]byte(was.Values), []byte(now.Values)):
		return inputValues, ""
	case !was.BuildpacksRead || !now.BuildpacksRead || len(was.Buildpacks) == 0 || !slices.Equal(was.Buildpacks, now.Buildpacks):
		return inputBuildpacks, ""
	case !was.DepsRead || !now.DepsRead:
		return inputDependencies, ""
	}
	if d, ok := depChange(was.Deps, now.Deps); ok {
		return inputDependencies, d
	}
	return "", ""
}

// depChange is the first package, by type, name, version and purl, one side has more times than the other, as line
// not-pinned names it: <name> <was> → <now>, with the other side's version of the same package, else none.
func depChange(was, now []appDep) (string, bool) {
	count := func(deps []appDep) map[string]int {
		n := map[string]int{}
		for _, d := range deps {
			n[d.key()]++
		}
		return n
	}
	wn, nn := count(was), count(now)
	all := slices.Concat(was, now)
	slices.SortFunc(all, func(a, b appDep) int { return cmp.Compare(a.key(), b.key()) })
	for _, d := range all {
		if wn[d.key()] == nn[d.key()] {
			continue
		}
		other := func(deps []appDep, more, less map[string]int) string { // the same package on the other side, of a version that side has more of
			for _, o := range deps {
				if o.Type == d.Type && o.Name == d.Name && more[o.key()] > less[o.key()] {
					return o.Version
				}
			}
			return "none"
		}
		if wn[d.key()] > nn[d.key()] {
			return d.Name + " " + d.Version + " → " + other(now, nn, wn), true
		}
		return d.Name + " " + other(was, wn, nn) + " → " + d.Version, true
	}
	return "", false
}

// appDepsOf is the image's own packages (appDep) in project's stored scan of digest, sorted; false when the scan, its label
// or any of its layers does not read: never an empty list in place of a scan Canyon could not read.
func appDepsOf(ctx context.Context, q querier, project, digest string) ([]appDep, bool, error) {
	pkgs, ok, err := packagesOf(ctx, q, project, digest)
	if !ok || err != nil {
		return nil, false, err
	}
	var deps []appDep
	for _, pkg := range pkgs {
		if pkg.Class != "os" && pkg.Class != "runtime" && pkg.Class != "lifecycle" { // what Canyon's own layers hold is the newer builder's to change
			deps = append(deps, appDep{Type: pkg.Prop("aquasecurity:trivy:PkgType"), Name: pkg.FullName(), Version: pkg.Version, PURL: pkg.PURL})
		}
	}
	slices.SortFunc(deps, func(a, b appDep) int { return cmp.Compare(a.key(), b.key()) })
	return deps, true, nil
}

// packagesOf is every package in project's stored scan of digest, bottom layer first; false when the scan, its label or
// any of its layers does not read.
func packagesOf(ctx context.Context, q querier, project, digest string) ([]trivy.Package, bool, error) {
	var diffIDs []string
	var label string
	err := q.QueryRow(ctx, "SELECT diff_ids, coalesce(lifecycle, '') FROM scanned_images WHERE project_id = $1 AND digest = $2 AND not_finished IS NULL",
		project, digest).Scan(&diffIDs, &label)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	owners, labelled := trivy.Owners(diffIDs, label)
	if !labelled || !trivy.DiffIDsRead(diffIDs) {
		return nil, false, nil
	}
	rows, err := q.Query(ctx, `SELECT l.diff_id, s.components FROM image_layers l JOIN layer_parts s USING (part_sha)
		WHERE l.project_id = $1 AND l.digest = $2 AND l.diff_id = ANY($3)`, project, digest, diffIDs)
	if err != nil {
		return nil, false, err
	}
	layers, whole := map[string][]trivy.Component{}, true
	var d string
	var raw []byte
	if _, err := pgx.ForEachRow(rows, []any{&d, &raw}, func() error {
		cs, ok := trivy.DecodeComponents(raw)
		whole = whole && ok && trivy.StoredIn(cs, d)
		layers[d] = cs
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !whole || len(layers) != len(diffIDs) {
		return nil, false, nil
	}
	return trivy.Packages(diffIDs, owners, layers), true, nil
}

// buildpackIDs are the ids of a recorded buildpacks list (contracts/executor.md, The buildpacks that ran), in order;
// false when none was recorded or it does not read.
func buildpackIDs(raw []byte) ([]string, bool) {
	var ran []struct {
		ID string `json:"id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &ran) != nil {
		return nil, false
	}
	ids := make([]string, 0, len(ran))
	for _, b := range ran {
		if b.ID == "" {
			return nil, false
		}
		ids = append(ids, b.ID)
	}
	return ids, true
}

// runtimeNames are the runtimes a rebuild names, by the buildpack whose layer holds the package it fixed (line same).
var runtimeNames = map[string]string{
	"paketo-buildpacks/node-engine": "Node.js", "paketo-buildpacks/cpython": "Python",
	"paketo-buildpacks/bellsoft-liberica": "Java", trivy.GoDist: "Go",
}

// RebuildNewer is what a rebuild was rebuilt with, as its lines and its approval's proof say it (contracts/routes.md,
// A rebuild patch, line same), from the findings it fixed: a newer runtime when every one is in one runtime's layers —
// the buildpack that fixes it (trivy.FixedBy) a runtime Canyon names — else newer build tools, never a guess: the app
// launcher's, a helper's, two runtimes'. Canyon can't read a runtime's own version (design/PATCHING.md §2).
func RebuildNewer(fixed []PlatformFinding) string {
	name := ""
	for _, f := range fixed {
		n := ""
		if f.Class == "runtime" && f.Owner != "" { // an owner not read names nothing
			n = runtimeNames[trivy.FixedBy(f.Owner)]
		}
		if n == "" || (name != "" && n != name) {
			return "newer build tools"
		}
		name = n
	}
	if name == "" {
		return "newer build tools"
	}
	return "a newer " + name
}

// RebuildProof is a rebuild patch approval's proof (contracts/routes.md, Production approvals).
func RebuildProof(newer string) string {
	return "Rebuilt from the same code and the same dependencies with " + newer + ". A rebuild is a new program, not a copy of what production runs, so a person on your team decides."
}

// proved is the same_inputs step's checkpoint: which input differed, "" for none, and for dependencies what changed;
// the key version both builds' values were compared under and the buildpack ids. Newer, what it was rebuilt with, is
// the workflow's own, from what the rebuild fixed (RebuildNewer), never journaled here.
type proved struct {
	Differs    string   `json:"differs,omitempty"`
	Change     string   `json:"change,omitempty"`
	Newer      string   `json:"-"`
	ValuesKey  int      `json:"values_key,omitempty"`
	Buildpacks []string `json:"buildpacks,omitempty"`
}

// proveSame reads what the build preview runs and the rebuild recorded, and their images' app packages, then proves
// same_inputs of them (step 4). Preview's row must still name the image the plan named: else nothing is compared.
func (p *Pipeline) proveSame(ctx context.Context, in Input, image string) (proved, error) {
	read := func(sql string, args ...any) (buildInputs, error) {
		var b buildInputs
		var values *string
		var key *int
		var ran []byte
		err := p.Pool.QueryRow(ctx, sql, args...).Scan(&b.Package, &b.Tree, &values, &key, &ran)
		if errors.Is(err, pgx.ErrNoRows) {
			return b, nil
		}
		if values != nil && key != nil {
			b.Values, b.Key = *values, *key
		}
		b.Buildpacks, b.BuildpacksRead = buildpackIDs(ran)
		return b, err
	}
	was, err := read(`SELECT d.package_id, coalesce(k.tree_hash, ''), d.values_hmac, d.values_key, d.buildpacks FROM deploys d LEFT JOIN packages k ON k.id = d.package_id
		WHERE d.project_id = $1 AND d.env = 'preview' AND d.image = $2 AND d.run_by = 'canyon'`, in.Project, in.Patch.Image)
	if err != nil {
		return proved{}, err
	}
	now, err := read(`SELECT r.package_id, coalesce(k.tree_hash, ''), r.values_hmac, r.values_key, r.buildpacks FROM runs r LEFT JOIN packages k ON k.id = r.package_id
		WHERE r.id = $1 AND r.build_image = $2`, in.Run, image)
	if err != nil {
		return proved{}, err
	}
	if was.Deps, was.DepsRead, err = appDepsOf(ctx, p.Pool, in.Project, ImageDigest(in.Patch.Image)); err != nil {
		return proved{}, err
	}
	if now.Deps, now.DepsRead, err = appDepsOf(ctx, p.Pool, in.Project, ImageDigest(image)); err != nil {
		return proved{}, err
	}
	differs, change := sameInputs(was, now)
	return proved{Differs: differs, Change: change, ValuesKey: now.Key, Buildpacks: now.Buildpacks}, nil
}

// rebuild is a rebuild patch's step 2 (contracts/routes.md, A rebuild patch): the build of the package preview runs on
// the build queue, as a deploy builds it, never reusing an image. It answers the image it built, or the run's end.
// Canyon's own failure in it goes to unfinished, whose line names none of it: only the build's own last line is quoted.
func (p *Pipeline) rebuild(ctx dbos.Context, in Input, version string, end func(outcome, state, reason, which, key, text string) (Result, error),
	unfinished func(outcome string, err error) (Result, error), blocking, warnings int) (image string, done bool, res Result, err error) {
	b, stop, err := p.buildAll(ctx, in)
	switch {
	case stop:
		res, err = p.interrupted(ctx, in, err, blocking, warnings)
	case err != nil && (ctx.Err() != nil || errors.Is(err, dbos.ErrWorkflowCancelled)):
	case err != nil:
		res, err = unfinished(PatchFailed, err)
	case b.Failed != "": // the package did not build, or the host could not build it: the build's own lines say why
		res, err = end(PatchFailed, "failed", "error", "", "build-failed", "Canyon could not rebuild "+version+": "+schedulerSaid(b.Failed)+", so it did not deploy it")
	case b.Builder == "" || RunImageDigest(b.Builder) != in.Patch.Identity: // another builder than the plan named
		res, err = end(PatchFailed, "failed", "error", "", "moved", movedLine(version))
	default:
		return b.Image, false, Result{}, nil
	}
	return "", true, res, err
}

// notSameLine is a rebuild's line for the input that differed (contracts/routes.md, Patch runs, its lines).
func notSameLine(p proved, version string) (key, text string) {
	switch p.Differs {
	case inputKeyRotated:
		return "rotated", "Canyon can't compare " + version + "'s build settings: Canyon's key for build settings changed since " + version + " was built, so it did not deploy the rebuild"
	case inputDependencies:
		return "not-pinned", "Its dependencies changed when Canyon rebuilt " + version + " (" + cmp.Or(p.Change, "its packages could not be read") + "), so it did not deploy it: pin them in a lockfile, then deploy again"
	}
	what := map[string]string{inputPackage: "Its code", inputValues: "Its build settings", inputBuildpacks: "How it is built"}[p.Differs]
	return "not-same", what + " changed when Canyon rebuilt " + version + ", so it did not deploy it"
}

// RebuildsOn is rebuildsOn, for the routes that read or decide a rebuild's approval.
func (p *Pipeline) RebuildsOn(ctx context.Context, q Querier) (bool, error) {
	return p.rebuildsOn(ctx, q)
}

// rebuildsOn is whether Canyon rebuilds apps now: patching on (patchingOn) and the org's rebuilds ask (contracts/routes.md,
// the preferences row). off stops every rebuild patch before its /run and files no approval for one.
func (p *Pipeline) rebuildsOn(ctx context.Context, q querier) (bool, error) {
	if on, err := p.patchingOn(ctx, q); err != nil || !on {
		return false, err
	}
	var on bool
	err := q.QueryRow(ctx, "SELECT rebuilds = 'ask' FROM org_preferences").Scan(&on)
	return on, err
}
