package enterprise

import (
	"context"
	"errors"
	"time"

	"github.com/getcanyon-ai/canyon/events"
)

// The shapes outbound keys' code in ee/ answers the core in (design/EGRESS.md §4; contracts/routes.md, Licence, A
// server built without Canyon Enterprise; #2922). Each JSON tag is egress.schema.json's.

// Refusal is a change ee/ refuses: the core answers it as writeErr does, Status, Code, Message and Fields.
type Refusal struct {
	Status  int
	Code    string
	Message string
	Fields  map[string]any
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

// ErrNoRoute is GET /egress/route-value's 404: the policy names no such app env, or it holds no route there.
var ErrNoRoute = errors.New("no route to that destination")

// AppEnv is one app env.
type AppEnv struct{ App, Env string }

// PolicyRoute is one value route as the policy names it (policy_route): never its value.
type PolicyRoute struct {
	Destination string `json:"destination"`
	Header      string `json:"header"`
	Scheme      string `json:"scheme"`
	Rev         int64  `json:"rev"`
}

// RouteWrite is a route's write as the core hands it on, under the app env's advisory lock, every refusal before the
// write's answered: the route, its plain value — a PUT's, or the secret's the core opened — and whether the env holds
// a route to Destination already. Refused answers the core's refusals past the licence, in the contract's order —
// credential_routes_off, held, destination_not_allowed, trust_secret — with an error the core recognises.
type RouteWrite struct {
	Project, Env, Destination string
	Header, Scheme            string // the header lower-cased; the scheme bearer, basic or raw
	Placeholder               *string
	Secret                    string // a from-secret's; "" for a PUT
	Value                     string
	Held                      bool
	Actor                     events.Actor
	Refused                   func(ctx context.Context) error
}

// RouteWait is why a route is not added to calls yet (route_wait); nil is live.
type RouteWait struct {
	Reason string  `json:"reason"`
	Secret *string `json:"secret"`
}

// Leaf is GET /egress/leaf's 200 (leaf).
type Leaf struct {
	Root     string    `json:"root"`
	Cert     string    `json:"cert"`
	Key      string    `json:"key"`
	Chain    string    `json:"chain"`
	NotAfter time.Time `json:"not_after"`
}

// CARoot is one interception root as an admin reads it (ca_read).
type CARoot struct {
	Fingerprint string    `json:"fingerprint"`
	BYO         bool      `json:"byo"`
	CreatedAt   time.Time `json:"created_at"`
	NotAfter    time.Time `json:"not_after"`
	EndsAt      time.Time `json:"ends_at"`
}

// CARead is the interception roots' key dates, an admin's (ca_read), never a key.
type CARead struct {
	Signing   *string    `json:"signing"`
	Roots     []CARoot   `json:"roots"`
	RotatesAt *time.Time `json:"rotates_at"`
}
