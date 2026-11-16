// Package finding is seam 1: a Canyon finding is a SARIF 2.1.0 result in a strict
// profile (contracts/finding.md). The types here mirror that profile field for field
// and Validate fails closed on anything the profile requires and a log lacks.
package finding

import "errors"

// SchemaURI is the $schema every Canyon SARIF log carries.
const SchemaURI = "https://json.schemastore.org/sarif-2.1.0.json"

// Version is the only SARIF version the profile accepts.
const Version = "2.1.0"

// Severity is canonical; level is a projection of it.
type Severity string

// The four severities — exactly four.
const (
	Critical Severity = "critical"
	High     Severity = "high"
	Medium   Severity = "medium"
	Low      Severity = "low"
)

// Level projects a severity onto the SARIF level tools read. Canyon never reads it back.
func (s Severity) Level() (string, error) {
	switch s {
	case Critical, High:
		return "error", nil
	case Medium:
		return "warning", nil
	case Low:
		return "note", nil
	}
	return "", errors.New("severity is not one of critical, high, medium, low") // never the value: it is producer text
}

// Log is one SARIF log: one run per producer.
type Log struct {
	Version string `json:"version"`
	Schema  string `json:"$schema"`
	Runs    []Run  `json:"runs"`
}

// ColumnKindCodePoints is the only columnKind the profile accepts: columns count Unicode
// code points, and every run says so, since SARIF's default is UTF-16 code units
// (contracts/finding.md).
const ColumnKindCodePoints = "unicodeCodePoints"

// Run is one producer's results.
type Run struct {
	ColumnKind        string             `json:"columnKind"`
	Tool              Tool               `json:"tool"`
	AutomationDetails *AutomationDetails `json:"automationDetails,omitempty"`
	Invocations       []Invocation       `json:"invocations,omitempty"`
	Results           []Result           `json:"results"`
}

// AutomationDetails is SARIF's run.automationDetails: an imported tool's run carries its report's
// category as id, when the category is not empty (contracts/finding.md).
type AutomationDetails struct {
	ID string `json:"id"`
}

// Invocation is SARIF's record of a producer's run: the reviewer's carries D10's skipped string.
type Invocation struct {
	ExecutionSuccessful        bool           `json:"executionSuccessful"`
	ToolExecutionNotifications []Notification `json:"toolExecutionNotifications"`
}

type Notification struct {
	Level   string  `json:"level"`
	Message Message `json:"message"`
	// A coverage line that names one rule carries it, its policy's name and the reason apart
	// (contracts/finding.md, What nothing read is named; #1668).
	AssociatedRule *RuleRef        `json:"associatedRule,omitempty"`
	Properties     *NoteProperties `json:"properties,omitempty"`
}

// RuleRef is SARIF's reportingDescriptorReference, by id alone.
type RuleRef struct {
	ID string `json:"id"`
}

type NoteProperties struct {
	Canyon Covered `json:"canyon"`
}

// Covered is a coverage line's properties.canyon: the policy it is about and why it reads so.
type Covered struct {
	PolicyName string `json:"policy_name"`
	Why        string `json:"why"`
}

type Tool struct {
	Driver Driver `json:"driver"`
}

// Driver names the producer and declares the rules its results may carry.
type Driver struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Rule is a policy: id = policy id = --explain arg = audit event type.
type Rule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Result is one finding. RelatedLocations is optional: absent stays absent (a nil slice) and an
// empty list stays [], never null, which SARIF refuses.
type Result struct {
	RuleID              string       `json:"ruleId"`
	Level               string       `json:"level"`
	Message             Message      `json:"message"`
	Locations           []Location   `json:"locations"`
	RelatedLocations    []Location   `json:"relatedLocations,omitzero"`
	PartialFingerprints Fingerprints `json:"partialFingerprints"`
	Properties          Properties   `json:"properties"`
}

type Message struct {
	Text string `json:"text"`
}

type Location struct {
	PhysicalLocation PhysicalLocation `json:"physicalLocation"`
}

type PhysicalLocation struct {
	ArtifactLocation ArtifactLocation `json:"artifactLocation"`
	Region           Region           `json:"region"`
}

type ArtifactLocation struct {
	URI string `json:"uri"`
	// URIBaseID is "IMAGE" on a platform.known-vuln finding alone, a path inside the image an app runs
	// (contracts/finding.md, System libraries and build tools); the schema refuses it on any other.
	URIBaseID string `json:"uriBaseId,omitempty"`
}

// Region is the span — 1-based, columns counted in characters, end column exclusive —
// with the pre-redacted snippet.
type Region struct {
	StartLine   int     `json:"startLine"`
	StartColumn int     `json:"startColumn"`
	EndLine     int     `json:"endLine"`
	EndColumn   int     `json:"endColumn"`
	Snippet     Snippet `json:"snippet"`
}

type Snippet struct {
	Text string `json:"text"`
}

// Shown is a caret on the snippet as serialized: StartColumn on its first line, EndColumn on its
// last, code points, 1-based, end exclusive.
type Shown struct {
	StartColumn int `json:"startColumn"`
	EndColumn   int `json:"endColumn"`
}

type Fingerprints struct {
	CanyonV1 string `json:"canyon/v1"`
}

type Properties struct {
	Canyon Canyon `json:"canyon"`
}

// Canyon is the properties.canyon bag. Nothing here is optional except SecretName.
// Blocks is a pointer so that a log which omits it is told apart from one that says
// false: the field is required, and absent must fail closed, not read as "does not block".
// SecretName is a pointer for the opposite reason: absent is fine, present-but-empty is not.
type Canyon struct {
	Severity   Severity `json:"severity"`
	Blocks     *bool    `json:"blocks"`
	Title      string   `json:"title"`
	Problem    string   `json:"problem"`
	Fix        string   `json:"fix"`
	PolicyName string   `json:"policy_name"`
	SecretName *string  `json:"secret_name,omitempty"`

	// Shown is where the caret goes on locations[0]'s snippet when that is not the region's own
	// columns: the snippet is masked or windowed, the region stays on the file (finding.md).
	Shown *Shown `json:"shown,omitempty"`

	// Recorded labels a row replayed from a recording, not reviewed by a model (EXPERIENCE D10).
	Recorded bool `json:"recorded,omitempty"`

	// Governance prose the surfaces print verbatim: required once the server has said the
	// finding blocks (Log.ValidateAgainst), permitted on any other finding, never forbidden.
	RequiredEndState string   `json:"required_end_state,omitempty"`
	DoNot            []string `json:"do_not,omitempty"`
}
