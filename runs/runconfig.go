package runs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// RunConfig is what a /run carried beside its image, the parts of the run-config fingerprint its deploy row
// records (contracts/routes.md, Patch runs, step 5): names, versions, via, run_by and the saved door, never a
// value, so a fingerprint that differs names what changed.
type RunConfig struct {
	Egress    Egress            `json:"egress"`              // the spec the row records, whole
	RunBy     string            `json:"run_by"`              // canyon: only Canyon's /run carries one
	FrontDoor *SavedDoor        `json:"front_door"`          // nil on Docker
	Secrets   map[string]Secret `json:"secrets,omitempty"`   // each runtime secret by its name; none, omitted
	Resources []string          `json:"resources,omitempty"` // the environment's resources' names, sorted; none, omitted
}

// ConfigRecorded is whether a deploy row recorded what a patch's step 5 compares against (contracts/routes.md, Patch
// runs): a fingerprint, the spec — its via one this server runs — and parts that read. A row without one fails that
// check every time, so the plan never names it (design/PATCHING.md §4.2) and the check reads it as changed (fail closed).
func ConfigRecorded(spec *Egress, parts []byte, fingerprint *string) bool {
	var was RunConfig
	return fingerprint != nil && spec != nil && (spec.Via == "off" || spec.Via == "canyon") && json.Unmarshal(parts, &was) == nil
}

// Secret is a runtime secret as a /run was handed it, never its value: its version, and when it was set, which
// moves on every write — a secret removed and set again starts at version 1 again — and a key rotation leaves.
type Secret struct {
	Version int       `json:"version"`
	SetAt   time.Time `json:"set_at"`
}

// Carried is what a /run carried and its fingerprint, taken before the /run, as everything its record needs. Roots
// are the interception roots its egress.ca carried, by fingerprint — none, as no /run carries egress.ca before #2143
// — and Gone whether the executor answered that the new version is up and the previous instance gone: Docker's 200,
// a Kubernetes 200 whose previous_gone is true (contracts/routes.md, Interception, Trust).
type Carried struct {
	Config      RunConfig
	Fingerprint string
	Roots       []string
	Gone        bool
	Started     time.Time // the moment before the /run: the deploy row's container_started_at (#2738); zero from a journal before it
}

// startedOf is the deploy row's container_started_at: none where no /run is known, or its journal predates it.
func startedOf(c *Carried) *time.Time {
	if c == nil || c.Started.IsZero() {
		return nil
	}
	return &c.Started
}

// Settled is the env's trusted roots once its /run is answered: the roots it carried, once the previous instance is
// gone; else nil, which leaves the row's, the intersection the /run wrote before it.
func (c Carried) Settled() []string {
	if !c.Gone {
		return nil
	}
	return c.Roots
}

// SavedDoor is the front door as it was saved: its row's door, which holds no secret, and when it was saved,
// which a new client secret moves (contracts/executor.md, The front door). Probed is whether that save passed
// the cluster probe (#2438), read with the door and never recorded: a deploy row's run-config is unchanged by it.
type SavedDoor struct {
	Door   json.RawMessage `json:"door"`
	SetAt  time.Time       `json:"set_at"`
	Probed bool            `json:"-"`
}

// Fingerprint is the sha256 of c's JSON, hex: encoding/json writes a struct's fields in order and a map's
// keys sorted, so equal parts give an equal fingerprint whatever order they were read in.
func (c RunConfig) Fingerprint() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
