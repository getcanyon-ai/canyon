package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	"github.com/jackc/pgx/v5"
)

// Actor is the typed triple every stored actor is (design/ARCHITECTURE.md): a person, an
// agent, or Canyon itself; OIDC lands day 2 with no backfill.
type Actor struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Display string `json:"display"`
}

// Provenance is how and from where an action was done (contracts/routes.md, the audit read): the channel, the
// person's role when a session of theirs acted, the session's digest — never its secret — and the client address.
// A request's stage puts it on the context; a row written with none on its context is Canyon's own, via canyon.
type Provenance struct {
	Via, Role, Session, Source string
}

type provenanceKey struct{}

// WithProvenance puts how and from where on ctx for every audit row written under it.
func WithProvenance(ctx context.Context, p Provenance) context.Context {
	return context.WithValue(ctx, provenanceKey{}, p)
}

// ProvenanceOf is ctx's provenance, via canyon when none was put there: a workflow's step, Canyon's own write.
func ProvenanceOf(ctx context.Context) Provenance {
	if p, ok := ctx.Value(provenanceKey{}).(Provenance); ok {
		return p
	}
	return Provenance{Via: "canyon"}
}

// AuditTx records a state change that took place, inside the transaction that made it
// (design/ARCHITECTURE.md: audit is not logging — a refused or failed request leaves no row).
// The caller took LockTx first, the order every appender keeps. The org timeline row follows
// when the change names a project; its foreign key is what makes naming a project that does
// not exist an error, not a row. audit_events itself has no foreign keys: the trail outlives
// everything it names (a deleted project, a run retention removed). Steps a workflow completes
// outside any request (a deploy) write their row through here too, naming the step as the
// action and the actor who triggered the run.
func AuditTx(ctx context.Context, tx pgx.Tx, act Actor, action, project, run string, detail any) error {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	id := "ev_" + hex.EncodeToString(b[:])
	if detail == nil {
		detail = map[string]any{}
	}
	var proj, runID *string
	if project != "" {
		proj = &project
	}
	if run != "" {
		runID = &run
	}
	// ts never runs backwards: the trail reads newest first, and a host clock that steps back (a VM's
	// does) would otherwise put an action before the one it followed. Writers hold LockTx, so max is stable.
	p := ProvenanceOf(ctx)
	if _, err := tx.Exec(ctx, "INSERT INTO audit_events (id, actor_kind, actor_subject, actor_display, action, project_id, run_id, detail, via, role, session, source, ts) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), GREATEST(now(), (SELECT max(ts) + interval '1 microsecond' FROM audit_events)))",
		id, act.Kind, act.Subject, act.Display, action, proj, runID, detail, p.Via, p.Role, p.Session, p.Source); err != nil {
		return err
	}
	if project != "" {
		if _, err := AppendOrgTx(ctx, tx, project, AuditAppended, map[string]any{"event_id": id}); err != nil {
			return err
		}
	}
	return nil
}
