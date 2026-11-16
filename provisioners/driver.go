package provisioners

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// Driver serves resources: Builtin from one shared server, HTTP handing the calls to a platform team's adapter (contracts/provisioner.md). An error is transient: the
// caller asks again until its own deadline. A Result is the driver's answer: Ensure is Ready, Pending or Failed; Delete is Gone, Pending or Failed. Failed is permanent.
type Driver interface {
	Ensure(ctx context.Context, key Key, typ string) (Result, error)
	Delete(ctx context.Context, key Key) (Result, error)
	Name() string // stable across restarts: a resource record keeps it, and its delete finds the driver by it
}

// Issuer is a driver that logs a person into a database it made, as their own role
// (contracts/provisioner.md, People). A driver that is not one is driver_unsupported.
type Issuer interface {
	// Issue hands connect one login for one person on one key, for one connection, with a context done
	// ConnectTimeout on; once connect returns, the login opens nothing more. An open, which makes no
	// connection, is an Issue whose connect does nothing. An error after connect made its connection
	// means the caller closes that connection.
	Issue(ctx context.Context, key Key, person Person, mode Mode, ttl time.Duration, connect func(context.Context, Login) error) error
	Revoke(ctx context.Context, key Key, person Person) error // no new login; every backend of the person ended
	// Activity, Cancel, Terminate, People and Forget are what a database session's checks, its end and the
	// sweep ask of the person's role. A pid that is not a backend of the person's role is left alone, and is
	// no error. Terminate, like Revoke, runs on a context of its own.
	Activity(ctx context.Context, key Key, person Person) (Activity, error) // the person's backends: live, and running a statement now
	Cancel(ctx context.Context, key Key, person Person, pid int) error      // the statement of one backend of the person's cancelled
	Terminate(ctx context.Context, key Key, person Person, pid int) error   // one backend of the person's ended, and verified gone
	People(ctx context.Context, key Key) ([]string, error)                  // the subjects of the key's person roles, found by their comments
	Forget(ctx context.Context, key Key, person Person) error               // the person's role on the key dropped, its backends ended first
	// ReadOnly is the relay's, inside a read connection's connect before it dials: nil while the login's role can
	// change no data in its database by any path, else *ReadOnlyUnsafe naming what it can (#2826). Any error refuses; one
	// reading what it judges, in a database that answered, is ErrUnchecked.
	ReadOnly(ctx context.Context, login Login) error
	// NoSecondConnection is a read connection's and a read query's, inside connect before it dials: nil while the
	// login's role can open no second connection from inside its database, else *ReadOnlyUnsafe naming how it can, a
	// dblink function it may EXECUTE, any SECURITY DEFINER function it may EXECUTE (one may wrap dblink), or a
	// foreign-data wrapper it holds USAGE on (#2918). Any error refuses; one reading what it judges, in a database that answered, is ErrUnchecked.
	NoSecondConnection(ctx context.Context, login Login) error
}

// Activity is a person role's backends: Live in pg_stat_activity, Active running a statement now.
type Activity struct{ Live, Active int }

// ConnectTimeout bounds the one connect an issued login is for: the person role's lock is held until it
// returns.
const ConnectTimeout = 10 * time.Second

type Person struct{ Subject, Email string } // the identity provider's sub, and the address

type Mode string

const Read, Write Mode = "read", "write"

// Login is held in memory for one connect and dropped.
type Login struct {
	Host           string
	Port           int
	Database, User string
	Secret         Secret
	TLS            TLSMode
	Expires        time.Time
}

// Secret is a login's password. It prints and marshals as [secret], and holds the value in a func, so
// even a struct holding it in an unexported field, which fmt reads past every method, prints none.
type Secret struct{ value func() string }

func NewSecret(s string) Secret { return Secret{func() string { return s }} }

// Reveal is the password itself, for the one connect.
func (s Secret) Reveal() string {
	if s.value == nil {
		return ""
	}
	return s.value()
}

func (Secret) String() string               { return "[secret]" }
func (Secret) GoString() string             { return "[secret]" }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, "[secret]") }
func (Secret) MarshalText() ([]byte, error) { return []byte("[secret]"), nil }

// TLSMode is how a login meets the server, as libpq's sslmode and sslrootcert name it: never weaker
// than the app's own connections.
type TLSMode struct{ SSLMode, RootCert string }

var (
	ErrLimitsUnsupported = errors.New("the apps database refuses a database session's limits")                                    // 409 limits_unsupported
	ErrUnsafeSchema      = errors.New("PUBLIC can still create in schema public, which only its owner or a superuser can revoke") // 409 unsafe_schema
	ErrNoDatabase        = errors.New("the database does not exist")                                                              // 409 database_not_ready
	ErrUnchecked         = errors.New("a read check couldn't read what it judges")                                                // 503 check_failed
)

// NotAsMade is an issue refused over a role or a setting of a database Issue reached that Canyon did not make
// (409 not_as_made; contracts/provisioner.md, Issue): Found says which, a sentence a person reads.
type NotAsMade struct{ Found string }

func (n *NotAsMade) Error() string { return n.Found }

// ReadOnlyUnsafe is a read connection refused because its role could still change data (409 read_only_unsafe;
// contracts/provisioner.md, ReadOnly): Found names the first rights found, sorted, More how many more there are.
type ReadOnlyUnsafe struct {
	Found []string
	More  int
}

// Error is the rights, joined, as the refusal names them: "INSERT on public.ledger, …, and 2 more".
func (e *ReadOnlyUnsafe) Error() string {
	said := strings.Join(e.Found, ", ")
	if e.More > 0 {
		said += fmt.Sprintf(", and %d more", e.More)
	}
	return said
}

// State is where a resource stands after a driver's answer.
type State string

const Ready, Pending, Failed, Gone State = "ready", "pending", "failed", "gone"

// Result is one answer. Outputs are set on Ready only, and hold only the type's promised names.
type Result struct {
	State   State
	Outputs map[string]string
	Message string
}

var (
	// each type's promised env names: adding a type is one entry here (contracts/provisioner.md)
	outputs = map[string][]string{"postgres": {"DATABASE_URL"}, "s3": {"STORAGE_URL"}}
	envs    = []string{"preview", "prod"} // db/migrations/0008
)

// Promised is the env names a type's ready answer carries; none for a type Canyon does not know.
func Promised(typ string) []string { return outputs[typ] }

// check refuses a key whose segments are not a project id, an environment and a resource name,
// each a DNS label, so no segment can reach past its place in an adapter's URL.
func (k Key) check() error {
	label := func(s string) bool { return len(s) <= 63 && names.ValidProject(s) }
	if !label(k.Project) || !label(k.Name) || !slices.Contains(envs, k.Env) {
		return fmt.Errorf("invalid resource key %q", k)
	}
	return nil
}
