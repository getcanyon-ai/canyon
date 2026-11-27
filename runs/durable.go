package runs

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"log/slog"
	"slices"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/getcanyon-ai/canyon/db"
)

// durableVersion is the application version DBOS stamps on every journaled run, and a server dequeues
// only work stamped with its own. It names one set of workflows, recorded in workflowSets: a release
// that adds, removes or renames one, or changes one's input or steps so a run journaled by the release
// before would not resume, bumps it, so a server still on the release before never claims a workflow it
// has no handler for, which would hold the workflow, and its queue's one slot, until that server
// stopped (#2271). Left at DBOS's default, a hash of the binary, every build would be a version of its
// own. handOver moves queued work across versions. DBOS__APPVERSION and DBOS__VMID in the environment
// would override this and the executor id; neither is ever set for the server.
const durableVersion = "canyon-v13"

// workflowSets records, sorted, the workflows each application version names; a recorded set never
// changes. Launch refuses a server whose workflows are not its version's set.
var workflowSets = map[string][]string{
	"canyon-v1":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"},
	"canyon-v2":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // gate-run's input carries a patch, on the patches queue; patch.rescan plans (#2096)
	"canyon-v3":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // a patch run's gate-run rebases, proves, checks and deploys to preview, recording it in a step of its own (#2098)
	"canyon-v4":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // release-remove sends delete: true on a project delete's remove, which v3 never does: a v3 server must never claim one (#2432)
	"canyon-v5":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // a patch run's run-config step answers unchecked, which v4 reads as unchanged (#2438)
	"canyon-v6":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // gate-run's "run by" step answers run_by and delivery together, a release may be a commit to the deploy repo, and release-remove commits a repo release's remove: a v5 server would post either to a webhook (#2470)
	"canyon-v7":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // gate-run's and release-remove's commit may go through GitLab's merge requests, which a v6 server has no client for and would crash on. The version keeps a v6 server from claiming one only while a v7 server runs: handOver gives the work to the release before about 30 s after the last v7 server stops, and from then on the platform_repo row's switch protects it (#2480)
	"canyon-v8":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // a release names its image's platform, which a v7 server never reads, and one never sent, its image gone or unread, ends in steps a v7 server has not: it must not claim one (#2528)
	"canyon-v9":  {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // a patch run's gate-run records what it replaced and watches the patched preview, then reverts it on an exit or a restart, in steps a v8 server has not: the version keeps a v8 server from claiming one while a v9 server runs, and handOver gives the work to the release before about 30 s after the last v9 server stops (#2100)
	"canyon-v10": {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "release-remove", "scan"}, // a patch approval's promote gate-run records what production ran and watches it, then reverts it on an exit or a restart, in steps a v9 server has not: the version keeps a v9 server from claiming one while a v10 server runs, and handOver gives the work to the release before about 30 s after the last v10 server stops (#2570)
	// a patch run's gate-run ends once its /run is recorded, freeing its slot of the patches queue, and enqueues its watch as patch.watch on the gate queue, which a v10 server has no handler for and whose steps a v10 gate-run has in its place: the version keeps a v10 server from claiming either while a v11 server runs. Forward, a v10 gate-run journaled mid-watch that handOver gives to v11 once no v10 server runs replays its watch inline, as journaled (patch reads its journal for "watch until"). Back, handOver gives queued work to the release before about 30 s after the last v11 server stops, but v10 has no handler for patch.watch: rolling back below this release leaves any patch watch in flight unwatched until the app's next deploy or a roll forward, so let watches end before rolling back (#2567)
	"canyon-v11": {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "patch.watch", "release-remove", "scan"},
	// a patch run's gate-run may be a rebuild (Patch.Rebuild), which builds, proves same_inputs and records its build in
	// steps a v11 gate-run has not, and would rebase instead: the version keeps a v11 server from claiming one while a v12
	// server runs. Back, handOver gives queued work to the release before about 30 s after the last v12 server stops: a
	// rebuild patch queued or under way then fails on it with nothing deployed, so let rebuild patches end before rolling
	// back (#2117)
	"canyon-v12": {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "patch.watch", "release-remove", "scan"},
	// patch.rescan reads the run image before it queues the fleet's image.sbom reads, where a v12 tick queued them
	// first (#264): a v12 tick a v13 server resumed would meet its journaled steps out of order, and the version keeps
	// it from claiming one. Back, a queued tick handOver gives the release before runs as v12's did
	"canyon-v13": {"build", "gate-run", "github-check", "github-keep", "image.sbom", "patch.rescan", "patch.watch", "release-remove", "scan"},
}

// beatEvery is how often a server writes its durable_executors row, and staleAfter how long another's
// may go unwritten, while this server wrote its own all along, before that server counts as dead;
// vars so a test can lower them.
var (
	beatEvery  = 5 * time.Second
	staleAfter = 30 * time.Second
)

// NewDurable builds the DBOS Transact context the run driver journals into (decided
// 2026-09-10, design/ARCHITECTURE.md): a pool of its own on the same database (handed over, so
// DBOS closes it on Shutdown and never connects to the postgres maintenance database to create
// one) and never in plaintext once it has TLS (db.Open), its own schema, its own migrations run here, and an executor id of this process alone —
// DBOS's default, "local", is every server's, so a server's launch would resume the runs another
// is driving (#1715). Workflows register on the context before Launch.
func NewDurable(ctx context.Context, url, schema string, logOut io.Writer) (dbos.Context, error) {
	pool, err := db.Open(ctx, url, 0)
	if err != nil {
		return nil, err
	}
	dc, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            "canyon",
		SystemDBPool:       pool,
		DatabaseSchema:     schema,
		ApplicationVersion: durableVersion,
		ExecutorID:         rand.Text(),
		Logger:             slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	return dc, nil
}

// Launch refuses dc unless its workflows are the set its version names. It enters dc's executor and its
// version in durable_executors and starts it, then keeps its row fresh, recovers stopped servers' runs
// and takes over the queued work no running version will (ARCHITECTURE.md, Durable gate state) until
// ctx ends or stop. stop shuts dc down and marks the row stopped, so a server still up takes its runs
// over at once, while its version counts as running until the row is stale — unless a run outlived the
// shutdown, when the row stays unmarked for them to move once it is stale.
func Launch(ctx context.Context, pool *pgxpool.Pool, dc dbos.Context, schema string) (stop func(), err error) {
	var names []string
	for _, w := range dbos.ListRegisteredWorkflows(dc) {
		names = append(names, w.Name)
	}
	slices.Sort(names)
	if v := dc.GetApplicationVersion(); !slices.Equal(names, workflowSets[v]) {
		return nil, fmt.Errorf("runs: this server registers the workflows %q, but application version %s names %q: a changed set of workflows needs a version of its own. Bump durableVersion in runs/durable.go and record the new set under it in workflowSets, leaving %s's as it is (#2271)", names, v, workflowSets[v], v)
	}
	id := dc.GetExecutorID()
	// Before any of its runs exists. A row of its id, which only DBOS__VMID can give a second process, is
	// taken over when it stopped or went stale, and that server's journaled workflows go back on their
	// queues with it: once its id beats again no other server recovers them, and DBOS's own recovery at
	// launch takes only this server's version's. A live row refuses, so two processes never share an id.
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO durable_executors (id, version) VALUES ($1, $2)
		ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, seen_at = now(), stopped_at = NULL
		WHERE durable_executors.stopped_at IS NOT NULL OR durable_executors.seen_at < now() - make_interval(secs => $3)`,
			id, dc.GetApplicationVersion(), staleAfter.Seconds())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("runs: a server still running holds executor id %s: two processes never share one", id)
		}
		_, err = tx.Exec(ctx, requeue(schema)+"executor_id = $1", id)
		return err
	}); err != nil {
		return nil, err
	}
	if err := dbos.Launch(dc); err != nil {
		return nil, err
	}
	life, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		keepAlive(life, pool, dc, schema, names)
	}()
	return func() {
		err := dbos.Shutdown(dc, 10*time.Second)
		cancel()
		<-done
		if err != nil {
			log.Printf("runs: %v: this server's executor row stays, so its runs move after %s", err, staleAfter)
			return
		}
		gone, cancelGone := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancelGone()
		if _, err := pool.Exec(gone, "UPDATE durable_executors SET stopped_at = now() WHERE id = $1", id); err != nil {
			log.Printf("runs: this server stopped but its executor row says it runs, so its runs move after %s: %v", staleAfter, err)
		}
	}, nil
}

// keepAlive writes dc's row every beatEvery and, after each write, recovers the stopped servers'
// runs and hands their queued work over; names are the workflows dc has a handler for. A server counts
// as stopped when its row is gone or marked stopped, or when it is staleAfter old while this one wrote
// its own without a gap all that time: an outage of the database, or a pause of the host, stalls every
// server's writes at once, and condemns none of them.
func keepAlive(ctx context.Context, pool *pgxpool.Pool, dc dbos.Context, schema string, names []string) {
	id := dc.GetExecutorID()
	var since, last time.Time // since: when this server's unbroken run of writes began
	for {
		if err := beat(ctx, pool, id, dc.GetApplicationVersion()); err != nil {
			log.Printf("runs: writing this server's executor row: %v", err)
		} else {
			now := time.Now()
			if now.Sub(last) > 2*beatEvery {
				since = now
			}
			last = now
			if n, err := recoverStopped(ctx, pool, dc, schema, time.Since(since) >= staleAfter); err != nil {
				log.Printf("runs: recovering stopped servers' runs: %v", err)
			} else if n > 0 {
				log.Printf("runs: put %d journaled workflow(s) of stopped servers back on their queues", n)
			}
			if n, err := handOver(ctx, pool, dc, schema, names); err != nil {
				log.Printf("runs: taking over the queued work of stopped versions: %v", err)
			} else if n > 0 {
				log.Printf("runs: took over %d queued workflow(s) no running version will dequeue", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(beatEvery):
		}
	}
}

// beat writes the row fresh, bounded by beatEvery so a stalled write breaks the run of writes. A row
// gone means another server took this one for dead and has its runs: it is written again, and said.
func beat(ctx context.Context, pool *pgxpool.Pool, id, version string) error {
	ctx, cancel := context.WithTimeout(ctx, beatEvery)
	defer cancel()
	tag, err := pool.Exec(ctx, "UPDATE durable_executors SET seen_at = now() WHERE id = $1", id)
	if err != nil || tag.RowsAffected() > 0 {
		return err
	}
	log.Print("runs: this server was taken for dead: a server still up has its runs, and a run both drive fails")
	_, err = pool.Exec(ctx, "INSERT INTO durable_executors (id, version) VALUES ($1, $2) ON CONFLICT DO NOTHING", id, version)
	return err
}

// recoverStopped puts the journaled workflows still PENDING under a stopped server's executor back on
// their queues, for whichever server dequeues each to resume from its last step — DBOS's own recovery
// for those executor ids, its ReenqueueForRecovery, which dbos-transact-golang v1.3.0 exports only
// through its admin server and its Conductor; the statement is that one's, for every version: each
// keeps its own, and handOver moves it on once no server of that version runs. A row gone or marked
// stopped counts at once; a stale one only when steady, and goes with them, a stopped one too. Only
// PENDING moves: a workflow cancelled, failed or dead-lettered stays as it ended. Two servers recovering
// the same one is safe: a workflow moves once, while it is still PENDING under the stopped id.
func recoverStopped(ctx context.Context, pool *pgxpool.Pool, dc dbos.Context, schema string, steady bool) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, beatEvery)
	defer cancel()
	tag, err := pool.Exec(ctx, `WITH stale AS (
		DELETE FROM durable_executors WHERE $1 AND id <> $2 AND seen_at < now() - make_interval(secs => $3) RETURNING id)
	`+requeue(schema)+`executor_id <> $2
		AND (executor_id IN (SELECT id FROM stale) OR executor_id NOT IN (SELECT id FROM durable_executors WHERE stopped_at IS NULL))`,
		steady, dc.GetExecutorID(), staleAfter.Seconds())
	return tag.RowsAffected(), err
}

// requeue is the head of the statement that puts journaled workflows still PENDING back on their
// queues, each keeping its version, its condition left for the caller to finish: DBOS's
// ReenqueueForRecovery's, for every version.
func requeue(schema string) string {
	return `UPDATE ` + pgx.Identifier{schema}.Sanitize() + `.workflow_status
	SET status = '` + string(dbos.WorkflowStatusEnqueued) + `', started_at_epoch_ms = NULL, updated_at = (extract(epoch FROM clock_timestamp()) * 1000)::bigint,
		queue_name = COALESCE(NULLIF(queue_name, ''), '_dbos_internal_queue') -- DBOS's models.InternalQueueName
	WHERE status = '` + string(dbos.WorkflowStatusPending) + `' AND `
}

// handOver stamps with dc's version the queued workflows this server has a handler for, among those
// stamped with a version no row in durable_executors names: a release rolled forward or back, or a
// stopped server's runs put back on their queues. A version still running drains its own work, so two
// running versions never pass one workflow back and forth, and a stopped server's row names its
// version until it is stale, so a release rolled back takes a newer one's work only once that has been
// gone staleAfter, never at a clean stop. One no running server has a handler for stays queued,
// holding no slot (a queue's concurrency counts the workflows under way), until one starts. A row with
// no version, a schedule tick DBOS could not stamp with the latest, is taken too. The stamp moves only
// while the workflow is still ENQUEUED, so it is dequeued once, and a workflow ended stays ended.
func handOver(ctx context.Context, pool *pgxpool.Pool, dc dbos.Context, schema string, names []string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, beatEvery)
	defer cancel()
	tag, err := pool.Exec(ctx, `UPDATE `+pgx.Identifier{schema}.Sanitize()+`.workflow_status w
	SET application_version = $1, updated_at = $2
	WHERE status = $3 AND name = ANY($4) AND application_version IS DISTINCT FROM $1
		AND NOT EXISTS (SELECT FROM durable_executors e WHERE e.version = w.application_version)`,
		dc.GetApplicationVersion(), time.Now().UnixMilli(), dbos.WorkflowStatusEnqueued, names)
	return tag.RowsAffected(), err
}
