// Package enterprise holds the types Canyon Enterprise's code (ee/) and the core share, so ee/ answers the core in
// the core's own types and no core package imports ee/ (design/ARCHITECTURE.md §8).
package enterprise

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Querier is what a hook reads on: the pool, or the transaction the caller is still inside, so a licence is read in
// the same transaction as the change it decides.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Required is a change to an Enterprise feature refused for want of a licence in force that names it: the core
// answers it 409 licence_required, by Feature (contracts/routes.md, Licence).
type Required struct{ Feature string }

func (e Required) Error() string { return "licence_required: " + e.Feature }

// ErrInvalid is a licence an admin pasted that is not a licence file, or that no compiled key verifies.
var ErrInvalid = errors.New("not a Canyon licence, or not signed by Canyon")

// Licence is the licence as GET /api/v1/licence answers an admin (contracts/schema/licence.schema.json's state).
type Licence struct {
	State      string   `json:"state"`
	Allows     []string `json:"allows"`
	Source     string   `json:"source,omitempty"`
	Customer   string   `json:"customer,omitempty"`
	Seats      int      `json:"seats,omitempty"`
	Issued     string   `json:"issued,omitempty"`
	Expires    string   `json:"expires,omitempty"`
	GraceUntil string   `json:"grace_until,omitempty"`
	Features   []string `json:"features,omitempty"`
	Problem    string   `json:"problem,omitempty"`
}

// Stored is a licence an admin's PUT stored: what its audit row names.
type Stored struct {
	Customer string
	Expires  time.Time
	Features []string
}
