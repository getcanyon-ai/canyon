package sarif

import (
	"cmp"
	"encoding/json"
	"fmt"
	pathpkg "path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-enry/go-enry/v2"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Report is a normalised report: its tool, category and driver version, its results sorted by (uri,
// startLine, startColumn, ruleId) and not yet fingerprinted, its drops, the first one's reason, and the
// package's files the report says its tool did not read whole, in path order.
type Report struct {
	Tool, Category, Version string
	Results                 []finding.Result
	Dropped                 int
	FirstDropped            string
	Unparsed                []string
}

// unparsedFile is the descriptor of ESLint's notification for a file it could not parse, which names
// the file by its first location (contracts/finding.md, In a run).
const unparsedFile = "ESL0999"

// Invalid refuses a report whole (routes.md: 400 invalid_report), naming its first error, never quoting it.
type Invalid struct{ Message string }

func (e Invalid) Error() string { return e.Message }

// UnknownTool refuses a report of a tool the snapshot carries no sarif.<tool> for (400 unknown_tool).
type UnknownTool struct{ Tool string }

func (e UnknownTool) Error() string {
	return fmt.Sprintf(`This report is from "%s", which isn't a tool your admin has added to Canyon. Ask an admin to add it in Guardrails.`, e.Tool)
}

// TooManyResults refuses a report of over 1,000 results (400 too_many_results).
type TooManyResults struct {
	Tool    string
	Results int
}

func (e TooManyResults) Error() string {
	n := strconv.Itoa(e.Results)
	for i := len(n) - 3; i > 0; i -= 3 {
		n = n[:i] + "," + n[i:]
	}
	return fmt.Sprintf("%s reported %s results; Canyon takes 1,000 per tool. Narrow the scan to this app's source.", e.Tool, n)
}

var (
	ruleID = regexp.MustCompile(`^[!-~]{1,128}$`) // printable ASCII without whitespace
	notTag = regexp.MustCompile(`[^a-z0-9]+`)
	levels = map[string]finding.Severity{"error": finding.High, "warning": finding.Medium, "note": finding.Low}
	// words is a security-severity a rule rates in words, as GitLab's sast-rules do (High, CRITICAL), read in ASCII case.
	words = map[string]finding.Severity{"critical": finding.Critical, "high": finding.High, "medium": finding.Medium, "low": finding.Low, "info": finding.Low}
	kinds = []string{"fail", "pass", "open", "informational", "notApplicable", "review"}
)

// The fields of a report finding.md reads; encoding/json matches SARIF's camelCase names untagged.
type (
	report struct {
		Version string
		Runs    []struct {
			Tool struct {
				Driver     component
				Extensions []component
			}
			AutomationDetails struct{ ID string }
			ColumnKind        string
			NewlineSequences  []string
			Results           []result
			Artifacts         *[]json.RawMessage // nil when the report has no artifacts key
			Invocations       []struct {
				ToolConfigurationNotifications, ToolExecutionNotifications []notification
			}
		}
	}
	notification struct {
		Descriptor struct{ ID string }
		Locations  []struct {
			PhysicalLocation struct{ ArtifactLocation struct{ URI string } }
		}
	}
	component struct {
		Name, Version string
		Rules         []rule
	}
	rule struct {
		ID                   string
		DefaultConfiguration struct{ Level string }
		Help                 struct{ Text string }
		ShortDescription     struct{ Text string }
		FullDescription      struct{ Text string }
		Properties           struct {
			SecuritySeverity json.RawMessage `json:"security-severity"`
			Tags             []string
		}
	}
	result struct {
		RuleID, Kind, Level string
		Rule                struct {
			ID            string
			ToolComponent *struct {
				Name  string
				Index *int
			}
		}
		Message      struct{ Text string }
		Locations    []struct{ PhysicalLocation *physical }
		Suppressions []json.RawMessage
		Fixes        []struct{ Description struct{ Text string } }
		Properties   struct {
			Canyon struct{ Package *ImagePackage } // an image report's (image.go): the package a result is in
		}
	}
	physical struct {
		ArtifactLocation struct {
			URI       string
			URIBaseID *string
		}
		Region *struct{ StartLine, StartColumn, EndLine, EndColumn *int }
	}
)

// Normalize reads one report against the snapshot and the package's tree (finding.md, In: external
// SARIF). An error in the report is an Invalid, UnknownTool or TooManyResults; any other, the tree's.
func Normalize(raw []byte, snap *policy.Snapshot, tree gate.Tree) (Report, error) {
	return NormalizeAs(raw, snap, tree, "")
}

// NormalizeAs is Normalize for a report of tool, one Canyon ran itself: the tool is the one scanned,
// never read from tool.driver.name (contracts/finding.md, Canyon-run tools). An empty tool is the name's.
func NormalizeAs(raw []byte, snap *policy.Snapshot, tree gate.Tree, tool string) (Report, error) {
	var in report
	if err := json.Unmarshal(raw, &in); err != nil || in.Version != finding.Version {
		return Report{}, Invalid{"the report is not SARIF 2.1.0 JSON"}
	}
	if len(in.Runs) != 1 {
		return Report{}, Invalid{"the report is not exactly one run"}
	}
	run := in.Runs[0]
	a := attach{tool: cmp.Or(tool, tag(run.Tool.Driver.Name)), ran: tool != "", snap: snap, rules: map[ruleKey]rule{}, extensions: map[string]int{},
		locator: newLocator(tree, run.NewlineSequences, run.ColumnKind != "unicodeCodePoints")}
	for in, c := range slices.Backward(append([]component{run.Tool.Driver}, run.Tool.Extensions...)) {
		for _, r := range slices.Backward(c.Rules) { // the first of a repeated id stands
			a.rules[ruleKey{in, r.ID}] = r
		}
		if in > 0 && c.Name != "" { // the first extension of a repeated name stands
			a.extensions[c.Name] = in
		}
	}
	rep := Report{Tool: a.tool, Category: tag(run.AutomationDetails.ID), Version: cmp.Or(run.Tool.Driver.Version, "unknown"), Results: []finding.Result{}}
	if len(a.tool) < 1 || len(a.tool) > 40 {
		return Report{}, Invalid{"tool.driver.name is not 1 to 40 characters once normalised"}
	}
	if len(rep.Category) > 128 {
		return Report{}, Invalid{"automationDetails.id is over 128 characters once normalised"}
	}
	if v := run.Tool.Driver.Version; utf8.RuneCountInString(v) > 128 || finding.HasLiveKey([]byte(v)) { // stored and shown
		return Report{}, Invalid{"driver.version is over 128 characters or carries a live key"}
	}
	i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif."+a.tool && r.Producer == policy.Sarif })
	if i < 0 {
		return Report{}, UnknownTool{a.tool}
	}
	a.policy = snap.Rules[i].Name
	if len(run.Results) > 1000 { // counted before any file is read
		return Report{}, TooManyResults{a.tool, len(run.Results)}
	}
	if !slices.Contains([]string{"", "utf16CodeUnits", "unicodeCodePoints"}, run.ColumnKind) {
		return Report{}, Invalid{"columnKind is not one SARIF 2.1.0 names"}
	}
	if n := run.NewlineSequences; len(n) > 8 || len(slices.Compact(slices.Sorted(slices.Values(n)))) != len(n) ||
		slices.ContainsFunc(n, func(s string) bool { return s == "" || len(s) > 8 }) { // so splitting a file stays linear
		return Report{}, Invalid{"newlineSequences is not up to 8 distinct sequences of 1 to 8 bytes"}
	}
	for i, res := range run.Results {
		out, drop, err := a.result(res)
		if refused, ok := err.(Invalid); ok {
			return Report{}, Invalid{fmt.Sprintf("result %d: %s", i, refused.Message)}
		}
		if err != nil {
			return Report{}, err
		}
		if drop == "" {
			rep.Results = append(rep.Results, out)
		} else if rep.Dropped++; rep.Dropped == 1 {
			rep.FirstDropped = fmt.Sprintf("result %d: %s", i, drop)
		}
	}
	for _, inv := range run.Invocations {
		for _, n := range slices.Concat(inv.ToolConfigurationNotifications, inv.ToolExecutionNotifications) {
			if a.tool != "eslint" || n.Descriptor.ID != unparsedFile || len(n.Locations) == 0 {
				continue
			}
			path := n.Locations[0].PhysicalLocation.ArtifactLocation.URI
			if _, held := slices.BinarySearch(a.paths, path); held { // exactly a file of the package
				rep.Unparsed = append(rep.Unparsed, path)
			}
		}
	}
	if a.tool == "eslint" && a.ran && (run.Artifacts == nil || len(*run.Artifacts) == 0) { // Canyon's names each file it linted: none read none of them
		rep.Unparsed = append(rep.Unparsed, slices.DeleteFunc(slices.Clone(a.paths), func(p string) bool { return !eslintReads(p) })...)
	}
	rep.Unparsed = slices.Compact(slices.Sorted(slices.Values(rep.Unparsed)))
	slices.SortStableFunc(rep.Results, func(x, y finding.Result) int { // so the tool's order cannot move a fingerprint
		p, q := x.Locations[0].PhysicalLocation, y.Locations[0].PhysicalLocation
		return cmp.Or(strings.Compare(p.ArtifactLocation.URI, q.ArtifactLocation.URI), cmp.Compare(p.Region.StartLine, q.Region.StartLine),
			cmp.Compare(p.Region.StartColumn, q.Region.StartColumn), strings.Compare(x.RuleID, y.RuleID))
	})
	return rep, nil
}

// eslintReads is whether Canyon's ESLint lints a path: its config's files (executor/eslint.Dockerfile),
// outside the node_modules ESLint never reads.
func eslintReads(p string) bool {
	return slices.ContainsFunc([]string{".js", ".cjs", ".mjs", ".jsx", ".ts", ".tsx", ".mts", ".cts"}, func(ext string) bool { return strings.HasSuffix(p, ext) }) &&
		!slices.Contains(strings.Split(p, "/"), "node_modules")
}

// tag normalises a tool's or category's name: lowercased, each run outside [a-z0-9] one -, trimmed of -.
func tag(s string) string { return strings.Trim(notTag.ReplaceAllString(strings.ToLower(s), "-"), "-") }

// attach is one report being normalised.
type attach struct {
	tool, policy string
	ran          bool // Canyon ran the tool: its rule ids name rules Canyon sent or the tool carries, never the code
	snap         *policy.Snapshot
	rules        map[ruleKey]rule
	extensions   map[string]int // an extension's name to its component
	*locator
}

// ruleKey is a rule by the component defining it, 0 the driver and i+1 tool.extensions[i], and its id.
type ruleKey struct {
	in int
	id string
}

// rated is a result's rule id, its rule as the report defines it, and its severity: the level, else the rule's
// default, rated by the rule's security-severity where it names one. An Invalid refuses the report.
func (a *attach) rated(res result) (string, rule, finding.Severity, error) {
	id := cmp.Or(res.RuleID, res.Rule.ID)
	if !ruleID.MatchString(id) {
		return "", rule{}, "", Invalid{"no ruleId of 1 to 128 printable ASCII characters without whitespace"}
	}
	in := 0 // SARIF: a result's rule is its rule.toolComponent's, by index into tool.extensions or by name, else the driver's
	if c := res.Rule.ToolComponent; c != nil {
		in = -1 // a component the report does not carry: no rule, so the level rates the result
		if i, ok := a.extensions[c.Name]; ok {
			in = i
		}
		if c.Index != nil && *c.Index >= 0 {
			in = *c.Index + 1
		}
	}
	r := a.rules[ruleKey{in, id}]
	level := cmp.Or(res.Level, r.DefaultConfiguration.Level, "warning")
	severity, known := levels[level]
	if level == "none" {
		return "", rule{}, "", Invalid{"level is none"}
	}
	if !known {
		return "", rule{}, "", Invalid{"level is not one SARIF 2.1.0 names"}
	}
	if s := r.Properties.SecuritySeverity; s != nil { // GitHub's 0-10 scale, a number or a numeric string, and its bands; or a word
		var score float64
		var text string
		err := json.Unmarshal(s, &score)
		if json.Unmarshal(s, &text) == nil {
			score, err = strconv.ParseFloat(text, 64)
		}
		word, isWord := words[asciiLower(text)]
		switch {
		case isWord:
			severity = word
		case err != nil || !(score >= 0 && score <= 10):
			return "", rule{}, "", Invalid{"the rule's security-severity is not a number from 0 to 10 or a severity word"}
		case score >= 9:
			severity = finding.Critical
		case score >= 7:
			severity = finding.High
		case score >= 4:
			severity = finding.Medium
		default:
			severity = finding.Low
		}
	}
	return id, r, severity, nil
}

// result is one result as the profile carries it, or why it is dropped; an Invalid refuses the report.
// What the report alone shows is refused before any drop; the unredacted-literal check reads the
// region, so it runs after them. Its live-key half reads every string; its region-text half stands
// down on a dependency result alone.
func (a *attach) result(res result) (finding.Result, string, error) {
	id, r, severity, err := a.rated(res)
	if err != nil {
		return finding.Result{}, "", err
	}
	text, short := res.Message.Text, r.ShortDescription.Text
	if strings.TrimRight(short, ". ") == "" { // a description of dots or spaces says nothing
		short = ""
	}
	about := text // the result's own words, or its rule's first paragraph when they are the rule's document
	if full := strings.TrimSpace(r.FullDescription.Text); full != "" && strings.TrimSpace(text) == full {
		about = firstParagraph(full)
	}
	title := titleOf(described(short, about))
	fix := r.Help.Text
	if len(res.Fixes) > 0 {
		fix = cmp.Or(res.Fixes[0].Description.Text, fix)
	}
	if full := strings.TrimSpace(r.FullDescription.Text); full != "" && strings.TrimSpace(fix) == full { // the rule's whole document, no fix of the tool's own
		fix = ""
	}
	switch {
	case text == "":
		return finding.Result{}, "", Invalid{"no message.text"}
	case utf8.RuneCountInString(text) > 4096:
		return finding.Result{}, "", Invalid{"message.text is over 4,096 characters"}
	case utf8.RuneCountInString(fix) > 4096: // the tool's fix or its rule's help, the other text the check below reads
		return finding.Result{}, "", Invalid{"its fix is over 4,096 characters"}
	case utf8.RuneCountInString(short) > 1024:
		return finding.Result{}, "", Invalid{"its rule's shortDescription is over 1,024 characters"}
	case title == "":
		return finding.Result{}, "", Invalid{`message.text has nothing before its first ". "`}
	}
	fix = cmp.Or(fix, "Change this code so "+a.tool+" no longer reports "+id+" here; the line above says what it found.")
	if len(res.Locations) == 0 || res.Locations[0].PhysicalLocation == nil || res.Locations[0].PhysicalLocation.ArtifactLocation.URI == "" ||
		res.Locations[0].PhysicalLocation.Region != nil && res.Locations[0].PhysicalLocation.Region.StartLine == nil {
		return finding.Result{}, "", Invalid{"no locations[0].physicalLocation with an artifactLocation.uri, or one whose region has no startLine"}
	}
	pl := res.Locations[0].PhysicalLocation
	startLine, startColumn, endLine, endColumn := 1, 1, 1, 1 // no region names the file, as an empty one at 1:1 does
	if g := pl.Region; g != nil {
		for _, n := range []*int{g.StartLine, g.StartColumn, g.EndLine, g.EndColumn} {
			if n != nil && (*n < 1 || *n > 1_000_000_000) {
				return finding.Result{}, "", Invalid{"a region number is below 1 or above 1,000,000,000"}
			}
		}
		startLine, startColumn = *g.StartLine, or(g.StartColumn, 1)
		endLine, endColumn = or(g.EndLine, startLine), or(g.EndColumn, 0)
		if endLine < startLine || endLine == startLine && g.EndColumn != nil && *g.EndColumn < startColumn {
			return finding.Result{}, "", Invalid{"the region ends before it starts"}
		}
	}
	if kind := cmp.Or(res.Kind, "fail"); kind != "fail" {
		if !slices.Contains(kinds, kind) { // never the report's own text: first_dropped is one line
			kind = "not one SARIF names"
		}
		return finding.Result{}, "kind is " + kind, nil
	}
	if len(res.Suppressions) > 0 {
		return finding.Result{}, "it is suppressed", nil
	}
	span, hidden, fileLevel, drop, err := a.locate(pl.ArtifactLocation.URI, pl.ArtifactLocation.URIBaseID, startLine, startColumn, endLine, endColumn)
	if err != nil || drop != "" {
		return finding.Result{}, drop, err
	}
	// A dependency result — trivy's, its rule tagged vulnerability — may quote its region, a lockfile's
	// package entry: its advisory link and the entry's resolved URL share https:// (finding.md).
	dependency := a.tool == "trivy" && slices.Contains(r.Properties.Tags, "vulnerability")
	for _, s := range []struct{ name, text string }{{"ruleId", id}, {"message.text", text}, {"its fix", fix}, {"its rule's shortDescription", short}} {
		if finding.HasLiveKey([]byte(s.text)) {
			return finding.Result{}, "", Invalid{s.name + " carries a live key"}
		}
	}
	rule, name, secret, tableFix := "sarif."+a.tool+"."+id, a.policy, "", ""
	m, ok, spared := a.mapping(r.Properties.Tags, id, span.URI)
	if spared { // the row's severity all the same, so its tool's rule set blocking blocks it
		severity = cmp.Or(finding.Severity(m.Severity), severity)
	}
	if ok { // the table's: its policy, severity and fix (contracts/snapshot.md)
		rule, name, tableFix = m.Policy+"."+a.tool+"."+id, a.name(m.Policy), m.Fix
		severity = cmp.Or(finding.Severity(m.Severity), severity)
		if strings.HasPrefix(m.Policy, "secrets.") {
			secret = policy.SecretName(id)
			tableFix = strings.ReplaceAll(tableFix, "<SECRET_NAME>", secret)
		}
	}
	// A tool's region is shown as Canyon's own are, live keys masked, and past the snippet's three lines
	// by its first three. It is hidden whole under secrets.*, where the tool's own tags call it a
	// credential, on a file-level result, where a line of it carries a terminator other than LF, and on a
	// dependency result past three lines, a lockfile entry (contracts/finding.md, In: external SARIF). A
	// shown region that will not build — a key straddling its edge or the third line's end — is hidden whole.
	whole := strings.HasPrefix(rule, "secrets.") || a.tool == "gitleaks" || credential.Selects(r.Properties.Tags) || fileLevel ||
		dependency && len(span.Lines) > maxShownLines || strings.ContainsAny(strings.Join(span.Lines, ""), "\r\u2028\u2029")
	out, err := a.build(rule, name, secret, id, short, about, fix, tableFix, hidden, severity, span, whole, dependency)
	if err != nil && !whole {
		out, err = a.build(rule, name, secret, id, short, about, fix, tableFix, hidden, severity, span, true, dependency)
	}
	if err != nil {
		return finding.Result{}, "", err
	}
	out.RelatedLocations = nil  // an imported finding has none, and the contract's golden omits the field
	c := &out.Properties.Canyon // a Dockerfile Canyon never builds from, said in Canyon's sentence, never masked
	if p := strings.TrimRight(c.Problem, ". "); finding.DockerfileCheck(rule) && p != "" {
		c.Problem = p + notReadDockerfile
		c.Title = titleOf(c.Problem)
		out.Message.Text = c.Title
	}
	return out, "", nil
}

// notReadDockerfile ends the problem of a result of checkov's dockerfile checks, on any Dockerfile:
// Canyon builds with buildpacks and from none, so the result never blocks (contracts/finding.md).
const notReadDockerfile = ". Canyon builds this app with buildpacks and never builds from this Dockerfile, so this does not block; it matters only where something else builds from it."

// credential is the tags a tool marks a secret with: its result's region is hidden whole, as a
// secrets.* one is, whatever policy it maps under (contracts/finding.md, In: external SARIF).
var credential = policy.Mapping{Tags: []string{"secret", "CWE-798", "CWE-259", "CWE-321"}}

// build is one imported result, its region shown or hidden whole. Hidden whole, the region's text
// is kept out of the stored strings too: an uploaded ruleId quoting it is refused, and the problem and
// fix are masked where they quote it (finding.md).
func (a *attach) build(rule, name, secret, id, short, text, fix, tableFix, hidden string, severity finding.Severity, span finding.Span, whole, dependency bool) (finding.Result, error) {
	problem := described(short, text)
	if whole && !dependency { // an uploaded id is never rewritten, so it is refused; other text is masked, once joined so no run straddles the join
		if !a.ran && quotes(id, hidden) {
			return finding.Result{}, Invalid{fmt.Sprintf("ruleId carries %d or more characters of the code it flags", minQuoted)}
		}
		problem, fix = maskQuoted(problem, hidden), maskQuoted(fix, hidden)
	}
	out, err := finding.New(finding.Input{Rule: finding.Rule{ID: rule, Name: name},
		Severity: severity, Title: titleOf(problem), Problem: problem, Fix: cmp.Or(tableFix, fix), SecretName: secret, Hidden: whole, Cut: !whole && len(span.Lines) > maxShownLines, Location: span,
		// on every result: blocks is derived in the run, after the attach (finding.md, From the tool into the profile)
		RequiredEndState: a.tool + " no longer reports " + id + " for this code when it runs again, and the app still does what it did.",
		DoNot: []string{"Silence it in " + a.tool + "'s settings or with an ignore comment — Canyon does not read those.",
			"Remove or rename the flagged code just so the tool stops matching it."}})
	if err != nil {
		return finding.Result{}, Invalid{err.Error()}
	}
	return out, nil
}

// described is a result's problem: the rule's description, when it has one, then the message —
// gitleaks' message names only its rule and file — or the description alone when the message is it,
// as checkov's is (contracts/finding.md, From the tool into the profile).
func described(short, text string) string {
	if short == "" {
		return text
	}
	short = strings.TrimRight(short, ". ")
	if strings.TrimRight(text, ". ") == short {
		return once(short)
	}
	return once(short) + ". " + text
}

// firstParagraph is doc's text before its first blank line, each line break with the spaces around it
// one space: a rule's document, as its YAML wrapped it, read as the sentences it is (contracts/finding.md).
func firstParagraph(doc string) string {
	var lines []string
	for _, line := range strings.Split(doc, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.Join(lines, " ")
}

// once is a description whose text before its first ": " comes again, in any ASCII case, before the
// next — trivy's "lodash: lodash: Arbitrary code execution…", its advisory database's title — with the
// first copy and its ": " left out, once (contracts/finding.md).
func once(short string) string {
	head, rest, ok := strings.Cut(short, ": ")
	if again, _, twice := strings.Cut(rest, ": "); ok && head != "" && twice && asciiLower(head) == asciiLower(again) {
		return rest
	}
	return short
}

// titleOf is a problem's first sentence, clipped to 120 code points.
func titleOf(problem string) string {
	title, _, _ := strings.Cut(problem, ". ")
	return clip(title, 120)
}

// maxShownLines is the most lines a shown snippet holds (contracts/finding.md: a snippet is at most
// three lines); a region past it is hidden whole.
const maxShownLines = 3

// mapping is the policy table's row for a result of this tool's rule id at path whose rule carries
// tags: the first row that selects it, by the tags alone — whatever the policy's state, which each run
// reads — unless the snapshot does not carry its policy, when the result stays the tool's; or spared,
// the row's rule it spares in a documentation file of prose, when it stays the tool's too
// (contracts/snapshot.md).
func (a *attach) mapping(tags []string, id, path string) (m policy.Mapping, ok, spared bool) {
	i := slices.IndexFunc(policy.Table, func(m policy.Mapping) bool { return m.Tool == a.tool && m.Selects(tags) })
	if i < 0 {
		return policy.Mapping{}, false, false
	}
	m = policy.Table[i]
	if m.Spared == id && a.prose(path) {
		return m, false, true
	}
	return m, slices.ContainsFunc(a.snap.Rules, func(r policy.Rule) bool { return r.ID == m.Policy }), false
}

// prose is whether Linguist reads path as documentation and its content as prose (Markdown, Text,
// reStructuredText…): a README is, a License.tsx or a docs/ app's config.ts is code.
func (a *attach) prose(path string) bool {
	if !enry.IsDocumentation(path) {
		return false
	}
	text, err := a.tree.Read(path)
	return err == nil && enry.GetLanguageType(enry.GetLanguage(pathpkg.Base(path), text)) == enry.Prose
}

// name is the snapshot's name of rule, which mapping found there.
func (a *attach) name(rule string) string {
	return a.snap.Rules[slices.IndexFunc(a.snap.Rules, func(r policy.Rule) bool { return r.ID == rule })].Name
}

// asciiLower is s lowercased in ASCII alone, so a word reads the same in every language (finding.md).
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// clip cuts s to at most n code points, the last an ellipsis: its first n-1 cut back to their last
// space, or all of them when there is none, trailing space dropped — never mid-word, as a reviewer's
// title is (contracts/finding.md, From the tool into the profile).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := string(r[:n-1])
	if !unicode.IsSpace(r[n-1]) { // the cut falls in a word: end at the space before it
		if i := strings.LastIndexFunc(head, unicode.IsSpace); i > 0 {
			head = head[:i]
		}
	}
	return strings.TrimRightFunc(head, unicode.IsSpace) + "…"
}

func or(n *int, otherwise int) int {
	if n == nil {
		return otherwise
	}
	return *n
}
