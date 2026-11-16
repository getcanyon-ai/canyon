package runs

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/events"
	"github.com/getcanyon-ai/canyon/executor"
	asked "github.com/getcanyon-ai/canyon/internal/answered"
	"github.com/jackc/pgx/v5"
)

// Deployed is one environment's deploy row as a run or the org's report writes it (contracts/routes.md,
// the deploys and promotions rows). A by-actor's three columns are null together, a row written before
// them or a promotion's preview row that carried none.
type Deployed struct {
	Project, Env, Package, Image, URL string
	GateRun                           string     // the review behind it: a deploy's own run, a promotion's preview run or rollback review
	Run                               string     // the run that wrote it, or whose release the org reported
	DeployedBy, PromotedBy            [3]*string // kind, subject, display
	RunBy                             string     // canyon | org
	BuildDigest                       string     // a Canyon preview's build; prod reads preview's for the same image
	ValuesHMAC                        string     // the values-only HMAC beside it (design/PATCHING.md §5), read and copied as it is
	ValuesKey                         int        // the key version ValuesHMAC was made under
	Answered                          *asked.Answer
	Egress                            *Egress  // the spec the container runs under; nil where the org runs it, whose platform decides
	Carried                           *Carried // what this row's /run carried; nil where none is known: the org's, or a retry's that found its version running
}

// trustedRoots is the egress_roots a deploy row's write sets: the roots its /run carried once the previous instance
// is gone; nil, which keeps the row's — the intersection its /run wrote before it, or none — when it is not, or no
// /run of this write is known (contracts/routes.md, Interception, Trust).
func trustedRoots(c *Carried) []string {
	if c == nil {
		return nil
	}
	return c.Settled()
}

// ErrNotPreview is a write of production through WritePreviewTx: production has one writer, WriteProdTx.
var ErrNotPreview = errors.New("runs: production is written by WriteProdTx alone")

// By is an actor's three deploy-row columns.
func By(a events.Actor) [3]*string { return [3]*string{&a.Kind, &a.Subject, &a.Display} }

// WritePreviewTx writes d as preview's deploy row, the org's report's write of it; production goes
// through WriteProdTx alone, so its checks cannot be stepped around.
func WritePreviewTx(ctx context.Context, tx pgx.Tx, d Deployed) error {
	if d.Env != "preview" {
		return ErrNotPreview
	}
	return upsertDeployTx(ctx, tx, d)
}

// upsertDeployTx writes d as its environment's one deploy row. run_by is the writer's on the insert and
// on the upsert: a row an org release wrote goes back to canyon with the next deploy Canyon runs itself
// (design/HAND-OFF.md §1, Switching). build_digest is the build's, and in prod the one of the preview row
// Canyon runs whose exact image a promotion ships — none when preview runs another image by now, or the
// org runs it (contracts/routes.md, the deploys row). It covers the image, so a copy is never another
// image's; the values-only HMAC and its key version travel with it, from the same row. The buildpacks are those recorded for the image's build (imageBuildpacks). run_started_at is
// when the run that handed the container its values started, and answered what Canyon's ask of it got:
// neither is the org's (contracts/routes.md, envs.<env>.stopped and .answered). egress is the spec the
// container runs under (design/EGRESS.md §2.5), and run_config what its /run carried, with its fingerprint
// (contracts/routes.md, Patch runs, step 5), and run_config_at that this server wrote them, which a server from
// before them never does (0099's trigger). egress_roots is the env's trusted roots (trustedRoots), none where the
// org runs it.
func upsertDeployTx(ctx context.Context, tx pgx.Tx, d Deployed) error {
	var config *RunConfig
	var fingerprint *string
	if d.Carried != nil {
		config, fingerprint = &d.Carried.Config, &d.Carried.Fingerprint
	}
	_, err := tx.Exec(ctx, `INSERT INTO deploys (project_id, env, package_id, image, url, gate_run, deployed_by_kind, deployed_by_subject, deployed_by_display, promoted_by_kind, promoted_by_subject, promoted_by_display, run_by, build_digest, buildpacks, run_started_at, answered, egress, values_hmac, values_key, run_config, run_config_sha256, run_config_at, builder, egress_roots, container_started_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
			CASE WHEN $13 = 'org' THEN NULL WHEN $2 = 'prod' THEN (SELECT build_digest FROM deploys WHERE project_id = $1 AND env = 'preview' AND run_by = 'canyon' AND image = $4) ELSE nullif($14, '') END, `+imageBuildpacks("$1", "$4")+`,
			CASE WHEN $13 = 'canyon' THEN (SELECT created_at FROM runs WHERE id = $15) END, $16, $17,
			CASE WHEN $13 = 'org' THEN NULL WHEN $2 = 'prod' THEN (SELECT values_hmac FROM deploys WHERE project_id = $1 AND env = 'preview' AND run_by = 'canyon' AND image = $4) ELSE nullif($18, '') END,
			CASE WHEN $13 = 'org' THEN NULL WHEN $2 = 'prod' THEN (SELECT values_key FROM deploys WHERE project_id = $1 AND env = 'preview' AND run_by = 'canyon' AND image = $4) ELSE nullif($19, 0) END, $20, $21, now(), CASE WHEN $13 = 'org' THEN NULL ELSE `+imageBuilder("$1", "$4")+` END,
			CASE WHEN $13 = 'canyon' THEN $22::text[] END, CASE WHEN $13 = 'canyon' THEN $23::timestamptz END)
		ON CONFLICT (project_id, env) DO UPDATE SET package_id = EXCLUDED.package_id, image = EXCLUDED.image, url = EXCLUDED.url, gate_run = EXCLUDED.gate_run, deployed_at = now(),
			deployed_by_kind = EXCLUDED.deployed_by_kind, deployed_by_subject = EXCLUDED.deployed_by_subject, deployed_by_display = EXCLUDED.deployed_by_display,
			promoted_by_kind = EXCLUDED.promoted_by_kind, promoted_by_subject = EXCLUDED.promoted_by_subject, promoted_by_display = EXCLUDED.promoted_by_display, run_by = EXCLUDED.run_by,
			build_digest = EXCLUDED.build_digest, buildpacks = EXCLUDED.buildpacks, run_started_at = EXCLUDED.run_started_at, answered = EXCLUDED.answered, egress = EXCLUDED.egress,
			values_hmac = EXCLUDED.values_hmac, values_key = EXCLUDED.values_key, run_config = EXCLUDED.run_config, run_config_sha256 = EXCLUDED.run_config_sha256, run_config_at = EXCLUDED.run_config_at, builder = EXCLUDED.builder,
			egress_roots = CASE WHEN EXCLUDED.run_by = 'org' THEN NULL ELSE coalesce(EXCLUDED.egress_roots, deploys.egress_roots) END,
			container_started_at = EXCLUDED.container_started_at`,
		d.Project, d.Env, d.Package, d.Image, d.URL, d.GateRun, d.DeployedBy[0], d.DeployedBy[1], d.DeployedBy[2], d.PromotedBy[0], d.PromotedBy[1], d.PromotedBy[2], d.RunBy, d.BuildDigest, d.Run, d.Answered, d.Egress, d.ValuesHMAC, d.ValuesKey, config, fingerprint,
		trustedRoots(d.Carried), startedOf(d.Carried))
	return err
}

// WriteProdTx is the shared production writer (design/OPERATIONS.md §3.4): the one place production's
// deploy row is written, by the promote run and the org's report alike — but for a patch's revert, which puts
// back the row it replaced and records no deploy (watch.go) — and the one place its history is
// appended. In the caller's transaction it upserts the row; then, unless an undeploy recorded a stop on
// the run (contracts/routes.md, The brake: the row says what runs there, and nothing says it went live),
// it appends the history row — kept, as every image is until a /run leaves it out: under the org no
// /run cleans the org's registry (design/OPERATIONS.md §1.6) — with its egress, which a rollback to it
// carries, marked patched when its review is a patch run or the image is one a patch made (a rollback to it: the
// mark is the image's, #2390), and the run's deploy.completed row — a late report's run has ended, and its row is skipped. replaces is the package production ran until now, "" for none; live is false when a
// stop is pending, and the caller writes nothing more. While production is held it writes nothing and is
// ErrHeld, unless a stop is truly pending on the run — recorded, and no deploy release of it sent: the hold
// met it mid-switch, and the row says what runs until the run stops it (contracts/routes.md, The hold). A
// release the org's platform reports on is never that: the hold's remove takes it off.
func WriteProdTx(ctx context.Context, tx pgx.Tx, d Deployed) (replaces string, live bool, err error) {
	var stopping, pending bool
	if err := tx.QueryRow(ctx, `SELECT stop_requested_at IS NOT NULL, stop_requested_at IS NOT NULL AND NOT EXISTS (SELECT 1 FROM releases WHERE run_id = $1 AND event = 'deploy')
		FROM runs WHERE id = $1`, d.Run).Scan(&stopping, &pending); err != nil {
		return "", false, err
	}
	if !pending {
		if err := held(ctx, tx, d.Project); err != nil {
			return "", false, err
		}
	}
	var was *string // the image production ran until now
	err = tx.QueryRow(ctx, "SELECT package_id, image FROM deploys WHERE project_id = $1 AND env = 'prod'", d.Project).Scan(&replaces, &was)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	d.Env = "prod"
	if err := upsertDeployTx(ctx, tx, d); err != nil || stopping {
		return replaces, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO deploy_history (project_id, package_id, image, replaces, gate_run, run, promoted_by_kind, promoted_by_subject, promoted_by_display, egress, patched)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, EXISTS (SELECT 1 FROM runs WHERE id = $5 AND kind = 'patch')
			OR EXISTS (SELECT 1 FROM deploy_history WHERE project_id = $1 AND package_id = $2 AND image = $3 AND patched))`, d.Project, d.Package, d.Image, was, d.GateRun, d.Run, d.PromotedBy[0], d.PromotedBy[1], d.PromotedBy[2], d.Egress); err != nil {
		return "", false, err
	}
	_, digest, _ := strings.Cut(d.Image, "@")
	data := map[string]any{"env": "prod", "url": d.URL, "package": d.Package, "image_digest": digest}
	if d.Answered != nil {
		data["answered"] = d.Answered
	}
	if _, err := events.AppendRunTx(ctx, tx, d.Run, d.Project, events.DeployCompleted, "deploy:prod", data); err != nil && !errors.Is(err, events.ErrEnded) {
		return "", false, err
	}
	return replaces, true, nil
}

// Earlier is one of production's earlier versions (contracts/routes.md, the status row's envs.prod.earlier[]).
type Earlier struct {
	Package    string        `json:"package"`
	Version    string        `json:"version"`
	Digest     string        `json:"image_digest"`
	DeployedAt string        `json:"deployed_at"` // RFC 3339, whole seconds, UTC
	PromotedBy *events.Actor `json:"promoted_by"`
	Patched    bool          `json:"patched,omitempty"`
	Kept       bool          `json:"kept"`
}

// earlierSQL is production's history per (package, image) at its newest row, newest first, without
// what production runs now, at most five, each version named as contracts/routes.md's *Naming a version*
// says; deploy_history_newest serves the app's rows.
const earlierSQL = `SELECT h.package_id, coalesce(p.label, substr(h.package_id, 5)), split_part(h.image, '@', 2), h.deployed_at, h.promoted_by_kind, h.promoted_by_subject, h.promoted_by_display, h.patched, h.kept
FROM (SELECT DISTINCT ON (package_id, image) * FROM deploy_history WHERE project_id = $1 ORDER BY package_id, image, deployed_at DESC, id DESC) h
LEFT JOIN packages p ON p.project_id = h.project_id AND p.id = h.package_id
WHERE NOT EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = $1 AND d.env = 'prod' AND d.package_id = h.package_id AND d.image = h.image)
ORDER BY h.deployed_at DESC, h.id DESC LIMIT 5`

// ReadEarlier is the app's earlier versions in production, [] for none.
func ReadEarlier(ctx context.Context, q querier, project string) ([]Earlier, error) {
	rows, err := q.Query(ctx, earlierSQL, project)
	if err != nil {
		return nil, err
	}
	out := []Earlier{}
	for rows.Next() {
		var e Earlier
		var at time.Time
		var kind, subject, display *string
		if err := rows.Scan(&e.Package, &e.Version, &e.Digest, &at, &kind, &subject, &display, &e.Patched, &e.Kept); err != nil {
			rows.Close()
			return nil, err
		}
		if kind != nil {
			e.PromotedBy = &events.Actor{Kind: *kind, Subject: *subject, Display: *display}
		}
		e.DeployedAt = at.UTC().Format(time.RFC3339)
		out = append(out, e)
	}
	return out, rows.Err()
}

// keepSQL is what the server keeps on a /run of an app (design/OPERATIONS.md §1.5), built as a set: the
// call's own image, production's previous image — the one the newest history row replaced — each image a
// deploy row names, a rollback review's recorded ships while no promotion has started since, and the
// image a hold recorded, so a lift can start it again (contracts/routes.md, The hold), and the image a patch
// replaced while its watch runs, so a revert can put it back, and after a blind watch while preview's row is
// still the patch's (design/PATCHING.md §4.4; contracts/routes.md, Patch runs). Only a
// digest reference names a digest; the executor refuses anything else in keep.
const keepSQL = `SELECT COALESCE(array_agg(DISTINCT split_part(k.image, '@', 2)) FILTER (WHERE split_part(k.image, '@', 2) ~ '^sha256:[0-9a-f]{64}$'), '{}') FROM (
  SELECT $2::text AS image
  UNION ALL (SELECT replaces FROM deploy_history WHERE project_id = $1 ORDER BY deployed_at DESC, id DESC LIMIT 1)
  UNION ALL SELECT image FROM deploys WHERE project_id = $1
  UNION ALL SELECT image FROM prod_holds WHERE project_id = $1
  UNION ALL SELECT r.patch_replaced->>'image' FROM runs r WHERE r.project_id = $1 AND r.kind = 'patch' AND (r.ended_at IS NULL
    OR EXISTS (SELECT 1 FROM timeline_rows t WHERE t.run_id = r.id AND t.dedupe = 'patch:blind')
    AND EXISTS (SELECT 1 FROM deploys d WHERE d.project_id = $1 AND d.env = 'preview' AND d.gate_run = r.id))
  UNION ALL (SELECT r.ships FROM runs r WHERE r.project_id = $1 AND r.ships IS NOT NULL
    AND NOT EXISTS (SELECT 1 FROM runs n WHERE n.project_id = $1 AND n.kind = 'promote' AND n.created_at > r.created_at)
    ORDER BY r.created_at DESC LIMIT 1)) k
WHERE k.image IS NOT NULL`

// keep is what a /run of app starting image keeps. An image of the app's history it leaves out may be
// removed by this /run's cleanup, so its rows stop reading kept before the call — never after it.
func (p *Pipeline) keep(ctx context.Context, app, image string) (executor.Digests, error) {
	var keep []string
	err := p.write(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, keepSQL, app, image).Scan(&keep); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE deploy_history SET kept = false WHERE project_id = $1 AND kept AND NOT split_part(image, '@', 2) = ANY($2)", app, keep)
		return err
	})
	return keep, err
}

// shipsGoneLine is a rollback review's last build.log line when the image it would ship is not kept, and
// keptGoneLine a rollback's promote run's when its /run finds the kept image gone (contracts/routes.md, the
// gate-runs and promotions rows).
func shipsGoneLine(version string) string {
	return version + "'s image is no longer on this Canyon, so it can't be put back as it ran. Deploy " + version + " to preview and promote it if you want it rebuilt."
}

func keptGoneLine(version string) string {
	return version + "'s kept image is gone, so it can't be put back as it ran."
}

// versionOf names a package of the project as contracts/routes.md's *Naming a version* says.
func versionOf(ctx context.Context, q querier, project, pkg string) (string, error) {
	var v string
	err := q.QueryRow(ctx, "SELECT coalesce(label, substr(id, 5)) FROM packages WHERE id = $1 AND project_id = $2", pkg, project).Scan(&v)
	return v, err
}

// ships records what a rollback review whose review passed ships: the image production ran under its
// digest, when Canyon still keeps it. false, its last line says why and nothing is recorded: a rollback
// never builds. Once: a replay finds the ships or the line.
func (p *Pipeline) ships(ctx context.Context, in Input) (bool, error) {
	var shipped bool
	err := p.write(ctx, func(tx pgx.Tx) error {
		var image string
		err := tx.QueryRow(ctx, `SELECT image FROM deploy_history WHERE project_id = $1 AND package_id = $2 AND split_part(image, '@', 2) = $3 AND kept
			ORDER BY deployed_at DESC, id DESC LIMIT 1`, in.Project, in.Package, in.RollbackTo).Scan(&image)
		if err == nil {
			shipped = true
			_, err = tx.Exec(ctx, "UPDATE runs SET ships = $2 WHERE id = $1", in.Run, image)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		v, err := versionOf(ctx, tx, in.Project, in.Package)
		if err != nil {
			return err
		}
		_, err = events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "ships:gone", map[string]any{"line": shipsGoneLine(v)})
		return err
	})
	return shipped, err
}

// keptGone is a rollback's promote run whose /run found the kept image gone (contracts/routes.md, the
// promotions row): the image is no longer kept, so the next rollback to it refuses, and the run's last
// line says why. Once: a replay finds the line.
func (p *Pipeline) keptGone(ctx context.Context, in Input, image string) error {
	return p.write(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "UPDATE deploy_history SET kept = false WHERE project_id = $1 AND image = $2", in.Project, image); err != nil {
			return err
		}
		v, err := versionOf(ctx, tx, in.Project, in.Package)
		if err != nil {
			return err
		}
		_, err = events.AppendRunTx(ctx, tx, in.Run, in.Project, events.BuildLog, "gone:prod", map[string]any{"line": keptGoneLine(v)})
		return err
	})
}
