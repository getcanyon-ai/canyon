package executor

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"slices"
	"time"
)

// What a /scan is on every host, which the Kubernetes executor (executor/kube) takes from here: the
// request's check, each tool's pinned entry, the SARIF an answer carries, and the caps.

// MaxScanOut caps a tool's stdout; MaxRules caps opengrep's rules part. A database is refreshed every
// DBEvery, one refresh within DBDeadline, and one built more than DBMaxAge before a scan starts is db_stale.
const (
	MaxScanOut = maxScanOut
	MaxRules   = maxRules
	DBEvery    = dbEvery
	DBDeadline = dbDeadline
	DBMaxAge   = dbMaxAge
)

// ScanDB is one vulnerability database as GET /scandb answers it (contracts/executor.md; #2743): when it was built,
// and when a scan stops running on it, both absent while there is none.
type ScanDB struct {
	Tool    string `json:"tool"`
	BuiltAt string `json:"built_at,omitempty"`
	StaleAt string `json:"stale_at,omitempty"`
}

// ScanDBOf is tool's database built at built, zero being none: stale DBMaxAge after it.
func ScanDBOf(tool string, built time.Time) ScanDB {
	if built.IsZero() {
		return ScanDB{Tool: tool}
	}
	return ScanDB{Tool: tool, BuiltAt: built.UTC().Format(time.RFC3339), StaleAt: built.Add(dbMaxAge).UTC().Format(time.RFC3339)}
}

// Kept is what the executor keeps of a tool's stdout as it streams, its snippets dropped; ErrOverCap when
// that passes MaxScanOut.
func Kept(r io.Reader, jsonOut bool) ([]byte, error) { return kept(r, jsonOut) }

// ErrOverCap is what Canyon keeps of a tool's stdout passing MaxScanOut: over_cap.
var ErrOverCap = errOverCap

// CheckScan refuses a /scan the schema refuses (ErrRequest): names, tool, and only that tool's
// settings keys and values.
func CheckScan(r ScanRequest) error { return r.check() }

// Scanned is whether a tool whose exit statuses are read answered a scan exiting code: one of exits, or
// Opengrep's 2 when every error its SARIF notes is a Rule parse error — a rule it dropped while it ran
// the rest (contracts/executor.md, rules_error).
func Scanned(exits []int, code int, sarif json.RawMessage) bool {
	if slices.Contains(exits, code) {
		return true
	}
	var log struct {
		Runs []struct {
			Invocations []struct {
				Notes []struct {
					Level      string `json:"level"`
					Descriptor struct {
						ID string `json:"id"`
					} `json:"descriptor"`
				} `json:"toolExecutionNotifications"`
			} `json:"invocations"`
		} `json:"runs"`
	}
	if code != 2 || json.Unmarshal(sarif, &log) != nil || len(log.Runs) != 1 {
		return false
	}
	dropped := 0
	for _, inv := range log.Runs[0].Invocations {
		for _, n := range inv.Notes {
			if n.Level != "error" {
				continue
			}
			if n.Descriptor.ID != "Rule parse error" {
				return false
			}
			dropped++
		}
	}
	return dropped > 0
}

// opengrepJSON is where Opengrep writes its JSON output, in the scan's /tmp, beside its SARIF on stdout.
const opengrepJSON = "/tmp/opengrep.json"

// ThenJSON is the script, sh -c ThenJSON sh <argv>, that a tool whose JSON output is on its answer runs
// under on every host: its stdout, the SARIF, then a NUL and the JSON output, the stderr of the tool and
// of the script discarded, and it exits as the tool did. A JSON output that is not there is a NUL alone,
// never an answer (SplitJSON).
const ThenJSON = `exec 2>/dev/null; "$@"; c=$?; printf '\0'; cat ` + opengrepJSON + `; exit $c`

// SplitJSON is the stdout of a tool with JSON output (Tool.JSONOut) as the SARIF and the rules and files its
// JSON errors name, whole false when the JSON output is not there; another tool's is all SARIF.
func SplitJSON(out []byte, jsonOut bool) (sarif []byte, unloaded, unparsed []string, whole bool) {
	return splitJSON(out, jsonOut)
}

// SARIF is a tool's stdout as the answer carries it: one SARIF 2.1.0 run, suppressions removed, and
// uris under one of uriRoots as the package's paths; false when it is not that.
func SARIF(out []byte, uriRoots []string) (json.RawMessage, bool) { return sarifOf(out, uriRoots) }

// Tool is a tool's pinned entry as another host runs it. Image is empty for a tool the Docker executor
// builds (opengrep, eslint), whose image a Kubernetes host is handed pinned by digest instead.
type Tool struct {
	Image    string
	Args     []string
	Config   func(ScanSettings) []byte // /scan/<tool>.yaml; nil for a tool whose skip paths leave the tree instead
	Deadline time.Duration
	URIRoots []string
	DB       *DB   // the tool's vulnerability database, mounted read-only at /scan/db; nil for none
	Exits    []int // the exit statuses that are a scan, any other rules_error, or killed; nil, the status is not read
	JSONOut  bool  // it runs under ThenJSON: its stdout is its SARIF, a NUL and its JSON output (SplitJSON)
	// Entrypoint is the image's ENTRYPOINT, which Args follow.
	Entrypoint []string
	Env        []string // KEY=value the scan's container gets beyond its image's (invariant 7)
}

// DB is a tool's vulnerability database as every host keeps it, in a volume mounted at /db. Refresh, the
// argv after the image's entrypoint, with Env, downloads it there, with network. Read, after ReadWith in
// place of the image's entrypoint ("": the image's own), with ReadEnv, prints as JSON when it was built,
// under BuiltAt, without network and the volume read-only.
type DB struct {
	Env, Refresh  []string
	RefreshWith   string // in place of the image's entrypoint for Refresh; "": the image's own
	ReadWith      string
	ReadEnv, Read []string
	BuiltAt       string
	// Partials is a glob in /db of what a refresh cut short leaves, which no flag moves out of it: each refresh
	// removes it first, so the volume holds one cut-short download at most (#2504, #2527); "": none
	Partials string
}

// ToolOf is the tool's pinned entry; false for a tool this release has none for (no_image).
func ToolOf(name string) (Tool, bool) {
	t, ok := scanTools[name]
	return toolOf(t), ok
}

// ToolFor is the pinned entry a request runs and its name, which names its database (canyon-scandb-<name>)
// and its lock: trivy's image target is JavaDB's entry, its sbom target trivy's own with the sbom
// arguments; false for none (no_image).
func ToolFor(req ScanRequest) (string, Tool, bool) {
	name, t, ok := toolFor(req)
	return name, toolOf(t), ok
}

func toolOf(t scanTool) Tool {
	var db *DB
	if t.db != nil {
		d := t.db.DB
		db = &d
	}
	return Tool{Image: t.image, Entrypoint: t.entrypoint, Args: t.args, Config: t.config, Deadline: t.deadline, URIRoots: t.uriRoots, DB: db, Exits: t.exits, JSONOut: t.jsonOut, Env: t.env}
}

// An image target's other bounds (contracts/executor.md): more layers, a larger lifecycle label, or on
// Kubernetes a larger manifest or config, is over_cap with reason image.
const (
	MaxImageLayers = 128
	MaxLifecycle   = 1 << 20
	MaxImageConfig = 4 << 20
)

// ImageMax is the largest image an image target reads, CANYON_SCAN_IMAGE_MAX: over it, over_cap.
func ImageMax() int64 { return imageMax }

// SBOMPart is an sbom target's parts after its json: part sbom, one CycloneDX JSON document of at most
// MaxScanOut, and no other; else ErrRequest.
func SBOMPart(parts *multipart.Reader) ([]byte, error) { return sbomPart(parts) }

// Considered is nil when an sbom target's report lists, among the packages trivy read, every component
// of the part that carries a purl, joined by bom-ref; else the answer is not_sarif, reason unread.
func Considered(part, report []byte) error { return considered(part, report) }

// Unscanned is the bom-ref of every component of an sbom target's part whose purl trivy lists but never
// matches: the answer's unscanned.
func Unscanned(part []byte) []string { return unscanned(part) }

// TargetDoc is an image target's stdout as its answer's sbom, or an sbom target's as its report; false
// when it is not that document (not_sarif).
func TargetDoc(target string, out []byte) (json.RawMessage, bool) { return targetDoc(target, out) }

// LifecycleLabel is the label an image answer's lifecycle_metadata carries.
const LifecycleLabel = lifecycleLabel
