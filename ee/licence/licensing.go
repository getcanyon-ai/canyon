package licence

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/getcanyon-ai/canyon/internal/enterprise"
)

// Keys are the public keys Canyon signs licences with, standard base64 of the 32 raw bytes, each verifying on its own
// so a new key is added before the old one is retired (ee/tools/licence -public prints one). The private half is the
// licence signing key, kept in Canyon's vault alone (#2881).
var Keys = []string{"PM4nCIp55J9N/Z69xKP9WwOdh9eD1Lwd5jZ8JPlTd3c="}

// Features are the Enterprise features a licence unlocks on this server, in the schema's order.
var Features = []string{"outbound_keys", "platform_patching", "group_sync"}

// Licensing is where a server's licence comes from: CANYON_LICENSE, else the row an admin's PUT stores, verified
// against its keys at each use, at the time Now says (contracts/routes.md, Licence). The core reaches it through its
// licence hook alone (design/ARCHITECTURE.md §8).
type Licensing struct {
	keys []ed25519.PublicKey
	Env  string           // CANYON_LICENSE as set: the file's text, or the path of a file holding it; "" unset
	Now  func() time.Time // time.Now when nil
}

// FromEnv is a server's licensing: the compiled keys, and the environment's licence.
func FromEnv(getenv func(string) string) (*Licensing, error) {
	l := &Licensing{Env: strings.TrimSpace(getenv("CANYON_LICENSE"))}
	for _, k := range Keys {
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("licence key %q is not a base64 Ed25519 public key", k)
		}
		l.keys = append(l.keys, raw)
	}
	return l, nil
}

// New is licensing that trusts keys alone, as a test's does.
func New(keys []ed25519.PublicKey, env string) *Licensing {
	return &Licensing{keys: keys, Env: env}
}

// Read is the licence as it stands now, whole: an admin's read.
func (l *Licensing) Read(ctx context.Context, q enterprise.Querier) (enterprise.Licence, error) {
	out := enterprise.Licence{State: "none", Allows: []string{}}
	text, source, err := l.text(ctx, q)
	if err != nil || source == "" {
		return out, err
	}
	out.Source = source
	if text == nil {
		out.State, out.Problem = "invalid", Unreadable
		return out, nil
	}
	p, problem := Verify(text, l.keys)
	if problem != "" {
		out.State, out.Problem = "invalid", problem
		return out, nil
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	out.State, out.Customer, out.Seats, out.Features = p.State(now()), p.Customer, p.Seats, p.Features
	out.Issued, out.Expires, out.GraceUntil = stamp(p.Issued), stamp(p.Expires), stamp(p.Expires.Add(Grace))
	if out.State == "valid" || out.State == "grace" {
		out.Allows = slices.DeleteFunc(slices.Clone(Features), func(f string) bool { return !slices.Contains(p.Features, f) })
	}
	return out, nil
}

// text is the licence's text and where it came from: env or dashboard, "" with none. A path the server can't read is
// env's with no text, which Read answers unreadable.
func (l *Licensing) text(ctx context.Context, q enterprise.Querier) ([]byte, string, error) {
	if l.Env != "" {
		if strings.HasPrefix(l.Env, "{") {
			return []byte(l.Env), "env", nil
		}
		raw, err := os.ReadFile(l.Env)
		if err != nil {
			return nil, "env", nil //nolint:nilerr // an unreadable path is the licence's problem, answered as one, never the request's failure
		}
		return raw, "env", nil
	}
	var body string
	switch err := q.QueryRow(ctx, "SELECT body FROM licence").Scan(&body); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, "", nil
	case err != nil:
		return nil, "", err
	}
	return []byte(body), "dashboard", nil
}

// Require is nil when a licence in force unlocks feature, else enterprise.Required; an error reading it is the
// caller's 500.
func (l *Licensing) Require(ctx context.Context, q enterprise.Querier, feature string) error {
	r, err := l.Read(ctx, q)
	if err != nil {
		return err
	}
	if !slices.Contains(r.Allows, feature) {
		return enterprise.Required{Feature: feature}
	}
	return nil
}

// SetByOperator is whether CANYON_LICENSE sets the licence, so no PUT changes it.
func (l *Licensing) SetByOperator() bool { return l.Env != "" }

// Store keeps the licence an admin pasted, on tx, once a key verifies it, replacing the one before:
// enterprise.ErrInvalid when none does, and nothing stored.
func (l *Licensing) Store(ctx context.Context, tx pgx.Tx, text string) (enterprise.Stored, error) {
	p, problem := Verify([]byte(text), l.keys)
	if problem != "" {
		return enterprise.Stored{}, enterprise.ErrInvalid
	}
	if _, err := tx.Exec(ctx, "INSERT INTO licence (body) VALUES ($1) ON CONFLICT (one) DO UPDATE SET body = EXCLUDED.body", text); err != nil {
		return enterprise.Stored{}, err
	}
	return enterprise.Stored{Customer: p.Customer, Expires: p.Expires, Features: p.Features}, nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
