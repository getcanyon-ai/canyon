package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/getcanyon-ai/canyon/identity"
	"github.com/jackc/pgx/v5"
)

// accessCeiling bounds one PUT /access: a stalled executor fails the change waiting on it, never pins it.
const accessCeiling = 30 * time.Second

// accessWriters is how many PUT /access one change has in flight at once (#2935): enough that a fleet's files are
// written in a few rounds rather than one after another, and few enough that one change never floods the executor,
// whose own Kubernetes client paces its API calls.
const accessWriters = 16

// accessFiles serialises every access file the server hands the executor on Kubernetes — a /run's and
// each re-PUT — and stamps each a second after the one before, never ahead of the clock: the ConfigMap
// keeps the newer of two updated_at (contracts/executor.md, PUT /access), and a /run lands after the
// call that built its file, so a file built later has to be the newer.
type accessFiles struct {
	sync.Mutex
	last time.Time
}

// stamp is the next file's updated_at, whole seconds UTC; called under the lock.
func (f *accessFiles) stamp() string {
	next := time.Now().UTC().Truncate(time.Second)
	if !next.After(f.last) {
		next = f.last.Add(time.Second)
		time.Sleep(time.Until(next))
	}
	f.last = next
	return next.Format(time.RFC3339)
}

// accessFile is the app's access file (contracts/executor.md, PUT /access): its grants, the sign-in
// their subjects are from, the org's membership lists, admins and groups claim as sign-in reads them
// (o), and the roles its current package declares. A grant bound under another issuer than that
// sign-in's goes as not yet bound, its subject "", as do all with sign-in off; one bound under an empty
// issuer, by a binary that recorded none, is that sign-in's (design/PERMISSIONS.md, Grants).
// While the host is in setup its admins alone are members, as they alone may sign in.
func (a *app) accessFile(ctx context.Context, o org, id, at string) (json.RawMessage, error) {
	var roles map[string]string
	var entries []identity.Grant
	issuer := a.signinIssuer() // the sign-in the entries' subjects are read under, which the sidecar compares only on its tokens; "" with sign-in off, where every entry goes unbound
	if err := a.pool.QueryRow(ctx, `SELECT
		coalesce((SELECT coalesce(roles, '{}') FROM packages WHERE project_id = $1 AND source_kind NOT IN ('repo-pr', 'check') ORDER BY created_at DESC, id DESC LIMIT 1), '{}'),
		coalesce((SELECT json_agg(json_build_object('kind', kind, 'grantee', grantee, 'subject', CASE WHEN $2 <> '' AND subject_issuer IN ($2, '') THEN subject ELSE '' END, 'level', level, 'roles', roles) ORDER BY kind, grantee)
			FROM project_access WHERE project_id = $1), '[]')`, id, issuer).Scan(&roles, &entries); err != nil {
		return nil, err
	}
	members := map[string][]string{"domains": o.domains, "emails": o.emails}
	if o.setup { // design/ONBOARDING.md §2
		members = map[string][]string{"domains": {}, "emails": o.admins}
	}
	return json.Marshal(map[string]any{"updated_at": at, "groups_claim": o.groupsClaim, "subject_issuer": issuer, "members": members,
		"admins": map[string]any{"emails": o.admins, "claim": o.adminClaim, "value": o.adminValue},
		"roles":  roles, "entries": entries})
}

// denyAll is the access file to the token of a release Canyon stopped under hand-off (contracts/routes.md, the access row;
// release.schema.json's access_deny_all): accessFile's shape naming nobody, so the sidecar admits no one.
func denyAll(at string) (json.RawMessage, error) {
	return json.Marshal(map[string]any{"updated_at": at, "groups_claim": "", "subject_issuer": "", "members": map[string][]string{"domains": {}, "emails": {}},
		"admins": map[string]any{"emails": []string{}, "claim": "", "value": ""}, "roles": map[string]string{}, "entries": []identity.Grant{}})
}

// filesOf stamps once and reads each app's file under the lock, so a file stamped later was read later;
// what is done with them happens after, without it.
func (a *app) filesOf(ctx context.Context, ids []string) ([]json.RawMessage, error) {
	a.files.Lock()
	defer a.files.Unlock()
	at := a.files.stamp()
	o, err := a.orgOf(ctx, a.pool)
	files := make([]json.RawMessage, len(ids))
	for i := 0; err == nil && i < len(ids); i++ {
		files[i], err = a.accessFile(ctx, o, ids[i], at)
	}
	return files, err
}

// runAccess is the access file a Kubernetes /run carries (runs.Pipeline.Access).
func (a *app) runAccess(ctx context.Context, id string) (json.RawMessage, error) {
	files, err := a.filesOf(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	return files[0], nil
}

// reput writes each app's access file wherever it runs, before the change that asked answers (the
// front-door spec's B1): both targets, as a first /run writes its file before its pod is up, accessWriters at a time,
// and it returns once every one has answered. The PUTs run
// outside the lock — the executor keeps the newer of two files — so a stalled one holds up nothing else.
// It answers how many the executor took and how many found nothing running (404); a newer file already
// there (409 stale) counts as taken. Docker hosts have no access file.
func (a *app) reput(ctx context.Context, ids ...string) (written, idle int, err error) {
	if !a.kube {
		return 0, 0, nil
	}
	files, err := a.filesOf(ctx, ids)
	if err != nil {
		return 0, 0, err
	}
	envs := []string{"preview", "prod"}
	puts := make([]error, len(ids)*len(envs)) // each PUT's answer, in app and target order, so the first failure named is the same however they finish
	slots := make(chan struct{}, accessWriters)
	var wg sync.WaitGroup
	for i, id := range ids {
		for j, env := range envs {
			slots <- struct{}{}
			wg.Go(func() {
				defer func() { <-slots }()
				call, cancel := context.WithTimeout(ctx, accessCeiling)
				defer cancel()
				puts[i*len(envs)+j] = a.pipe.Executor.PutAccess(call, id, env, files[i])
			})
		}
	}
	wg.Wait() // before the answer: every file is written, or its failure fails the change (the front door's B1)
	var failed int
	for k, put := range puts {
		switch {
		case errors.Is(put, executor.ErrNoContainer):
			idle++
		case put == nil || errors.Is(put, executor.ErrStale):
			written++
		default:
			if failed++; err == nil {
				err = fmt.Errorf("the access file of %s-%s: %w", ids[k/len(envs)], envs[k%len(envs)], put)
			}
		}
	}
	if err != nil {
		err = fmt.Errorf("%d PUT /access failed, the first: %w", failed, err)
	}
	return written, idle, err
}

// reputAll is reput of every live app: who is a member or an admin changed, or the reconcile ticked.
func (a *app) reputAll(ctx context.Context) (written, idle int, err error) {
	rows, err := a.pool.Query(ctx, "SELECT id FROM projects WHERE deleted_at IS NULL ORDER BY id")
	if err != nil {
		return 0, 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, 0, err
	}
	return a.reput(ctx, ids...)
}

// reconcileAccess re-PUTs every app's access file when the server starts and every `every` after, until
// ctx ends, one log line a pass: a change whose re-PUT failed, a list the environment changed across a
// restart, an executor that came up after the server — each reaches every running app within one tick,
// with no record of what failed.
func (a *app) reconcileAccess(ctx context.Context, logger *log.Logger, every time.Duration) {
	for {
		written, idle, err := a.reputAll(ctx)
		if line := fmt.Sprintf("canyon-server: access files: %d written, %d not running", written, idle); err != nil && ctx.Err() == nil {
			logger.Printf("%s; %v", line, err)
		} else if err == nil {
			logger.Print(line)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
