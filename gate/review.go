package gate

import (
	"cmp"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"golang.org/x/sync/errgroup"

	"github.com/getcanyon-ai/canyon/contracts"
	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// TooLarge is the reviewer's run's notification when the source it would show is over maxSource: the
// model is not called, and the run goes on (contracts/finding.md).
const TooLarge = "ai review · not checked: the source is larger than the AI review accepts (16 MiB)"

// TooLargeBefore is TooLarge as a run before the split review said it: a verdict stored then still reads
// as one that read nothing, and why (contracts/routes.md, The promote receipt).
const TooLargeBefore = "ai review · not checked: the source is larger than the AI review accepts (2 MiB)"

// TooLargeForACall, %s a quoted path, is the reviewer's run's warning for a file that alone is more than
// one call takes: no call is shown it, and the run reads as one that read nothing (contracts/finding.md).
const TooLargeForACall = "ai review · not checked: too large for one AI review call: %s"

// NotUTF8, %s a quoted path, is the reviewer's run's warning for a file it would be shown that is not valid
// UTF-8: no call is sent it, and the run reads as one that read nothing (contracts/finding.md).
const NotUTF8 = "ai review · not checked: not valid UTF-8: %s"

// NotUTF8More, %d the files past maxNamed, follows the NotUTF8 warnings that name the first.
const NotUTF8More = "ai review · not checked: %d more files not valid UTF-8"

// Dependencies, %s the quoted folders, is the reviewer's run's one warning naming where code under .git/,
// node_modules/ or vendor/ sits, which it is never shown: the first maxNamed folders, then how many more
// (contracts/finding.md).
const Dependencies = "ai review · not checked: code in dependency folders: %s"

// maxNamed is how many files or folders a warning of the reviewer's run names before it counts the rest.
const maxNamed = 20

// TimedOut is the reviewer's run's warning when its calls did not all answer within the review's time,
// roundCeiling for each round of partsAtOnce calls: the run reads as one that read nothing
// (contracts/finding.md).
const TimedOut = "ai review · not checked: the model did not answer in the time the review allows"

// Parts is the note on a review split across calls, %d the calls (contracts/finding.md).
const Parts = "ai review · reviewed in %d parts: the source is more than one model call takes"

// Skipped is the reviewer's run's notification when the host has no model (EXPERIENCE D10).
const Skipped = "ai review · skipped (no model configured on this Canyon host)"

// OutOfRoom is the reviewer's run's notification when the model's answer stopped at its room
// (ErrOutOfRoom): it answered nothing, and the run goes on, its check never clean (contracts/finding.md).
const OutOfRoom = "ai review · not checked: the model ran out of room for its answer"

// UnreadNotes are the notes Unread reads, each a run that read nothing: every surface that says why has
// words for each.
var UnreadNotes = []string{Skipped, OutOfRoom, TooLarge, TooLargeBefore, TooLargeForACall, NotUTF8, TimedOut}

// Unread is the note of run, an agent's, that says it read nothing — Skipped, OutOfRoom, TooLarge (or a
// stored run's TooLargeBefore), TooLargeForACall or NotUTF8, whichever file those name, or TimedOut — and "" when it read: its check is never clean, it is never
// cached, and a rule of it blocking before preview is not passed unread (contracts/finding.md).
func Unread(run finding.Run) string {
	for _, note := range UnreadNotes {
		lead, _, named := strings.Cut(note, "%s") // TooLargeForACall, NotUTF8: whichever file it names
		said := func(n finding.Notification) bool {
			return n.Message.Text == Note(run.Tool.Driver.Name, note) || named && strings.HasPrefix(n.Message.Text, Note(run.Tool.Driver.Name, lead))
		}
		if slices.ContainsFunc(run.Invocations, func(inv finding.Invocation) bool {
			return slices.ContainsFunc(inv.ToolExecutionNotifications, said)
		}) {
			return note
		}
	}
	return ""
}

// Replayed is the notification on a run answered from a recording, results or none.
const Replayed = "ai review · replayed from a recording"

// Dropped is the note on a run whose model proposed results candidateResult refuses — a rule the
// reviewer may not emit, a file it was not shown, a repeat of one it kept, and so on: those are dropped,
// counted here, and the rest of the review stands (contracts/finding.md). A note, never a warning: it
// is what the model got wrong, never something the review did not read.
const Dropped = "ai review · dropped %d invalid result(s)"

// SecondOpinion is the note on every run the model answered: its answers vary between calls and no
// setting pins them, so a new review, even of a small change, may find different things (#1819).
const SecondOpinion = "ai review · a model's second opinion: another review, even of a small change, may find different things; a blocking finding stays while its code is unchanged"

// CarriedOne and CarriedMany are the note on a run that carries blocking findings from an earlier review whose code
// stands (Carry; contracts/finding.md, A blocking finding an earlier review made stays while its code stands).
const CarriedOne = "ai review · 1 blocking finding from an earlier review: the code it's on hasn't changed"

// CarriedMany is CarriedOne for two or more, %d the count.
const CarriedMany = "ai review · %d blocking findings from an earlier review: the code they're on hasn't changed"

// Reused is the notification on a run the review cache answered: the same tree, policy snapshot,
// model and agent were reviewed before, and that review is replayed with no model call.
const Reused = "ai review · reused from an earlier review of the same code and policy"

// unauthenticatedRoute is the rule whose title, problem and fix are Canyon's on a host that signs people in:
// signedInTitle, signedInProblem after the model's own title, and signedInFix (contracts/finding.md).
const (
	unauthenticatedRoute = "auth.unauthenticated-route"
	signedInTitle        = "Any signed-in person can reach this route"
	signedInProblem      = ". Canyon signs people in before they reach this app, so only people with access to it reach this route; the route itself does not check who is calling."
	signedInFix          = "Don't build a login of your own: Canyon already signs people in. Where this route needs to know or check who is calling, read the identity token Canyon sends with every request in the Authorization: Bearer header, verify it with CANYON_IDENTITY_ISSUER, CANYON_IDENTITY_AUDIENCE and CANYON_IDENTITY_JWKS_URL, and use its sub, email and name. A destructive or admin-only route checks a role; an ordinary write records who did it, from the identity."
)

// inCanyonsWords is c, an unauthenticatedRoute result's, on a host that signs people in: its title, problem
// and fix Canyon's, the model's title the problem's first sentence, its severity the model's (whether a placed rule
// blocks reads it, and Canyon never weakens a placement; #2890), and its governance prose Canyon's own where it blocks under state and none where it
// does not, since the model's would ask for the login again (contracts/finding.md).
func inCanyonsWords(c *finding.Canyon, state policy.State) {
	c.Title, c.Problem, c.Fix = signedInTitle, c.Title+signedInProblem, signedInFix
	c.RequiredEndState, c.DoNot = "", nil
	if finding.Blocks(unauthenticatedRoute, state, c.Severity) {
		c.RequiredEndState, c.DoNot = canyonsGovernance(unauthenticatedRoute)
	}
}

// canyonsGovernance is the governance prose Canyon writes for a reviewer's result under rule that will
// block without the model's own (contracts/finding.md, canyonv2-ihqr).
func canyonsGovernance(rule string) (string, []string) {
	return "Canyon's review no longer reports " + rule + " for this code when it runs again, and the app still does what it did.",
		[]string{"Silence it with an ignore comment or a suppression pragma — Canyon does not read those.",
			"Remove or rename the flagged code just so the review stops matching it."}
}

// The AI review's room (contracts/finding.md). One call takes callRoom bytes of prompt: claude-opus-5's
// window less the answer's reviewMaxTokens and promptRoom tokens for the brief, the rules and the answer's
// schema, at 0.99 bytes of the prompt as sent per token — the fewest count_tokens measured over five real
// codebases, 1.10 on base64-embedded WebAssembly, less 10%. A review takes at most maxSource bytes of source
// in all, and at most maxCalls calls, the ceilings that bound its cost; over either the model is not called
// (TooLarge).
const (
	contextTokens = 1_000_000
	promptRoom    = 64_000
	callRoom      = (contextTokens - reviewMaxTokens - promptRoom) * 99 / 100
	maxSource     = 16 << 20
	maxCalls      = (maxSource + callRoom - 1) / callRoom
)

// partsAtOnce is how many calls of a split review are in flight at once: maxCalls is nineteen, so three
// finish them in seven rounds, while no more than three requests of up to a window each count against the
// org's input-token rate limit together.
const partsAtOnce = 3

// roundCeiling is the time one round of partsAtOnce calls has: a review of n calls has ceil(n/partsAtOnce)
// of them, then is TimedOut (contracts/finding.md).
const roundCeiling = 10 * time.Minute

// errTooLarge is excerpts' answer over maxSource.
var errTooLarge = errors.New("gate: the source is larger than the AI review accepts")

// masked is what the model is sent for every masked literal: bullets only, nothing of the literal.
const masked = "••••"

//go:embed review-gitleaks.toml
var detectorConfig string

// detector is gitleaks' default ruleset extended by detectorConfig. It loads through viper's global
// state, so it is built once; a gitleaks:allow comment is ignored, as it silences a CI scan, not
// what a model provider is sent.
var detector = sync.OnceValues(func() (*detect.Detector, error) {
	viper.SetConfigType("toml")
	if err := viper.ReadConfig(strings.NewReader(detectorConfig)); err != nil {
		return nil, err
	}
	var vc config.ViperConfig
	if err := viper.Unmarshal(&vc); err != nil {
		return nil, err
	}
	cfg, err := vc.Translate()
	if err != nil {
		return nil, err
	}
	d := detect.NewDetector(cfg)
	d.IgnoreGitleaksAllow = true
	return d, nil
})

// ErrModel wraps the model's failures (its error, or an answer the profile refuses): failure_reason model.
var ErrModel = errors.New("ai review failed (model)")

// Model is the reviewer's one call to a model: a wire client (Anthropic, OpenAI) or Replay.
type Model interface {
	Review(ctx context.Context, req Request) (Response, error)
}

// Request is all a model is sent: the resolved model id, the rules it may emit and masked source by
// path.
type Request struct {
	Model string            `json:"model"`
	Rules []finding.Rule    `json:"rules"`
	Files map[string]string `json:"files"`
	// Instructions is each custom rule's text by its id, an admin's (contracts/snapshot.md, Custom
	// rules); absent from every other request, so a recording keys as it always did.
	Instructions map[string]string `json:"instructions,omitempty"`
	// Scope is each AI twin's files, the only ones it checks (contracts/finding.md, The AI review where
	// no scanner reads the language); absent while no language goes unread.
	Scope map[string][]string `json:"scope,omitempty"`
	// Paths is every shown file's path, sent to each call of a split review before its part's files;
	// absent from a review that is one call, so a recording keys as it always did.
	Paths []string `json:"paths,omitempty"`
}

// Response is a model's answer. Recorded is set by Replay, never read off a recording.
type Response struct {
	Recorded bool        `json:"-"`
	Results  []Candidate `json:"results"`
}

// Candidate is one finding a model proposes, on whole lines of a file it was shown; a span past
// finding.MaxSnippetLines is windowed to its first lines (candidateResult).
type Candidate struct {
	Rule      string `json:"rule"`
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Severity  string `json:"severity"`
	Title     string `json:"title"`
	Problem   string `json:"problem"`
	Fix       string `json:"fix"`
	// The governance prose a finding that blocks carries (contracts/finding.md): absent from an
	// answer recorded before the fields were asked for, and then absent from its result.
	RequiredEndState string   `json:"required_end_state,omitempty"`
	DoNot            []string `json:"do_not,omitempty"`
}

// Replay answers from a recording keyed by the request's sha256, labeled recorded; a miss fails.
type Replay map[string]Response

func (r Replay) Review(_ context.Context, req Request) (Response, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	resp, ok := r[key]
	if !ok {
		return Response{}, fmt.Errorf("no recording for request %s", key)
	}
	resp.Recorded = true
	return resp, nil
}

// CustomRules is the agent that reviews an org's custom rules, in one call (contracts/snapshot.md,
// Custom rules).
const CustomRules = "custom-rules"

// Note is one of a reviewer run's notifications as agent's run says it: the custom-rules run's read
// "custom rules ·" where canyon-review's read "ai review ·" (contracts/finding.md, Custom rules).
func Note(agent, text string) string {
	if agent == CustomRules {
		return "custom rules" + strings.TrimPrefix(text, "ai review")
	}
	return text
}

// Reviewer is the agent producer: one run under its snapshot id, emitting only granted rules not
// Off. ModelID is the host's resolved model id; a nil Model is a host with none.
type Reviewer struct {
	Snapshot policy.Snapshot
	Agent    string
	ModelID  string
	Model    Model
	// SignedIn is a host that signs people in before they reach an app (CANYON_AUTH oidc): its
	// auth.unauthenticated-route results say so in Canyon's words (contracts/finding.md).
	SignedIn bool
	// room is one call's prompt bytes, callRoom when zero, and round one round's time, roundCeiling when
	// zero: a test's smaller ones split a small app and run out of time.
	room  int
	round time.Duration
}

// Review returns scanned's runs, untouched, with the reviewer's appended: it may add, never remove.
// The model is not called at all if a live key survives the masking.
func (r *Reviewer) Review(ctx context.Context, root string, scanned finding.Log) (finding.Log, error) {
	i := slices.IndexFunc(r.Snapshot.Agents, func(a policy.Agent) bool { return a.ID == r.Agent })
	if i < 0 {
		return finding.Log{}, fmt.Errorf("gate: agent %s is not in the snapshot", r.Agent)
	}
	tree, err := Walk(root)
	if err != nil {
		return finding.Log{}, err
	}
	allowed, states := map[string]finding.Rule{}, map[string]policy.State{}
	req := Request{Model: r.ModelID, Rules: []finding.Rule{}}
	var twinsUnread []string // an AI twin's files it would read but for not being valid UTF-8
	enabled := false         // a rule of the agent's is on, sent or not
	for _, rule := range r.Snapshot.Rules {
		ns, _, _ := strings.Cut(rule.ID, ".")
		if rule.On() && rule.Producer == policy.LLMReview && slices.Contains(r.Snapshot.Agents[i].Namespace, ns) { // a custom rule checked by code is the scanners' run's
			enabled = true
			if scanner, ok := scannerOf(rule.ID); ok {
				files, notUTF8, err := unread(tree, r.Snapshot, scanner)
				if err != nil {
					return finding.Log{}, err
				}
				if len(files) == 0 { // every file it would read, a scanner reads — or none it could, which the run names
					twinsUnread = append(twinsUnread, notUTF8...)
					continue
				}
				if req.Scope == nil {
					req.Scope = map[string][]string{}
				}
				req.Scope[rule.ID] = files
			}
			allowed[rule.ID] = finding.Rule{ID: rule.ID, Name: rule.Name}
			req.Rules = append(req.Rules, allowed[rule.ID])
			if rule.Instructions != "" {
				if req.Instructions == nil {
					req.Instructions = map[string]string{}
				}
				req.Instructions[rule.ID] = rule.Instructions
			}
			states[rule.ID] = heldTo(r.Snapshot, rule.ID)
		}
	}
	out := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: slices.Clone(scanned.Runs)}
	run := finding.Run{ColumnKind: finding.ColumnKindCodePoints, Tool: finding.Tool{Driver: finding.Driver{Name: r.Agent, Version: DriverVersion, Rules: req.Rules}}, Results: []finding.Result{}}
	notCalled := func(note string) (finding.Log, error) { // the model is not called, and the run says why
		run.Invocations = []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: []finding.Notification{{Level: "warning", Message: finding.Message{Text: Note(r.Agent, note)}}}}}
		out.Runs = append(out.Runs, run)
		return out, nil
	}
	if r.Model == nil {
		return notCalled(Skipped)
	}
	if len(req.Rules) == 0 { // nothing to emit: no answer could name a rule the schema allows — but, a rule on, what it was not shown is named
		var notes []finding.Notification
		if enabled {
			slices.Sort(twinsUnread)
			folders, err := dependencyNote(r.Agent, tree)
			if err != nil {
				return finding.Log{}, err
			}
			notes = append(notUTF8Notes(r.Agent, slices.Compact(twinsUnread)), folders...)
		}
		if len(notes) > 0 {
			run.Invocations = []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: notes}}
		}
		out.Runs = append(out.Runs, run)
		return out, nil
	}
	source, shownText, notUTF8, err := excerpts(root)
	if errors.Is(err, errTooLarge) {
		return notCalled(TooLarge)
	}
	if err != nil {
		return finding.Log{}, err
	}
	for _, text := range shownText {
		if finding.HasLiveKey([]byte(text)) {
			return finding.Log{}, errors.New("gate: a live-key literal survived redaction; the model was not called")
		}
	}
	calls, paths, over, err := parts(slices.Sorted(maps.Keys(source)), source, shownText, scopeBytes(req.Scope), cmp.Or(r.room, callRoom))
	if errors.Is(err, errTooLarge) {
		return notCalled(TooLarge)
	}
	for i := range calls {
		calls[i].req = calls[i].request(req, paths)
	}
	calls = slices.DeleteFunc(calls, func(p part) bool { return len(p.req.Rules) == 0 }) // a part no rule checks: nothing to ask
	resps, err := r.call(ctx, calls)
	if errors.Is(err, ErrOutOfRoom) || errors.Is(err, errTimedOut) { // one part's is the whole review's (contracts/finding.md)
		log.Printf("gate: ai review: %v", err) // the wrapped text alone: never a file body or a key
		return notCalled(map[bool]string{true: OutOfRoom, false: TimedOut}[errors.Is(err, ErrOutOfRoom)])
	}
	if err != nil {
		return finding.Log{}, err
	}
	var notes []finding.Notification
	if slices.ContainsFunc(resps, func(resp Response) bool { return resp.Recorded }) {
		notes = append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: Note(r.Agent, Replayed)}})
	}
	if len(calls) > 1 {
		notes = append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: Note(r.Agent, fmt.Sprintf(Parts, len(calls)))}})
	}
	for _, name := range over {
		notes = append(notes, finding.Notification{Level: "warning", Message: finding.Message{Text: Note(r.Agent, fmt.Sprintf(TooLargeForACall, strconv.Quote(name)))}})
	}
	notes = append(notes, notUTF8Notes(r.Agent, notUTF8)...)
	folders, err := dependencyNote(r.Agent, tree)
	if err != nil {
		return finding.Log{}, err
	}
	notes = append(notes, folders...)
	d, err := detector()
	if err != nil {
		return finding.Log{}, err
	}
	dropped, kept := 0, map[spot]int{} // each kept result's index by its rule and region: the model may say one finding twice
	for i, resp := range resps {
		for _, c := range resp.Results {
			if files, ok := req.Scope[c.Rule]; ok && !slices.Contains(files, c.File) { // the model never widens its scope
				dropped++
				logDrop(ctx, c, allowed, errors.New("outside the rule's scope"))
				continue
			}
			// a file of its own part alone; model noise, not the review: the contract drops it and says how many (canyonv2-4lr6)
			res, err := candidateResult(d, c, allowed, states, calls[i].source, calls[i].shown)
			if err != nil {
				dropped++
				logDrop(ctx, c, allowed, err)
				continue
			}
			if r.SignedIn && res.RuleID == unauthenticatedRoute {
				inCanyonsWords(&res.Properties.Canyon, states[res.RuleID])
				res.Message.Text = res.Properties.Canyon.Title // a result's message is its title (contracts/finding.md)
			}
			res.Properties.Canyon.Recorded = resp.Recorded
			if at, said := kept[spotOf(res)]; said { // one result per rule and region, the highest severity's, the first among equals
				dropped++
				logDrop(ctx, c, allowed, errRepeat)
				if rank(res.Properties.Canyon.Severity) > rank(run.Results[at].Properties.Canyon.Severity) {
					run.Results[at] = res
				}
				continue
			}
			kept[spotOf(res)] = len(run.Results)
			run.Results = append(run.Results, res)
		}
	}
	if dropped > 0 {
		notes = append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: Note(r.Agent, fmt.Sprintf(Dropped, dropped))}})
	}
	if len(calls) > 0 { // a model answered
		notes = append(notes, finding.Notification{Level: "note", Message: finding.Message{Text: Note(r.Agent, SecondOpinion)}})
	}
	run.Invocations = []finding.Invocation{{ExecutionSuccessful: true, ToolExecutionNotifications: notes}}
	fingerprintRun(run.Results)
	own := finding.Log{Version: finding.Version, Schema: finding.SchemaURI, Runs: []finding.Run{run}}
	raw, err := json.Marshal(own)
	if err != nil {
		return finding.Log{}, err
	}
	if err := cmp.Or(own.Validate(), contracts.CheckFindingSchemas(raw)); err != nil {
		return finding.Log{}, fmt.Errorf("%w: %v", ErrModel, err)
	}
	out.Runs = append(out.Runs, run)
	return out, nil
}

// notUTF8Notes are agent's warnings naming the files, in path order, it would be shown but for not being valid
// UTF-8: the first maxNamed, then how many more.
func notUTF8Notes(agent string, names []string) []finding.Notification {
	var notes []finding.Notification
	for _, name := range names[:min(len(names), maxNamed)] {
		notes = append(notes, finding.Notification{Level: "warning", Message: finding.Message{Text: Note(agent, fmt.Sprintf(NotUTF8, strconv.Quote(name)))}})
	}
	if more := len(names) - maxNamed; more > 0 {
		notes = append(notes, finding.Notification{Level: "warning", Message: finding.Message{Text: Note(agent, fmt.Sprintf(NotUTF8More, more))}})
	}
	return notes
}

// dependencyNote is agent's one warning naming the folders under .git/, node_modules/ or vendor/ that hold
// source, the first maxNamed then how many more; none when no folder does.
func dependencyNote(agent string, tree Tree) ([]finding.Notification, error) {
	folders, err := dependencies(tree)
	if err != nil || len(folders) == 0 {
		return nil, err
	}
	quoted := make([]string, 0, maxNamed)
	for _, f := range folders[:min(len(folders), maxNamed)] {
		quoted = append(quoted, strconv.Quote(f))
	}
	named := strings.Join(quoted, ", ")
	if more := len(folders) - maxNamed; more > 0 {
		named += fmt.Sprintf(", and %d more", more)
	}
	return []finding.Notification{{Level: "warning", Message: finding.Message{Text: Note(agent, fmt.Sprintf(Dependencies, named))}}}, nil
}

// part is one call of a review: its files' text as written and as the model is sent it, by path, and
// the request that sends them.
type part struct {
	source, shown map[string]string
	req           Request
}

// scopeBytes is what each file adds, quoted, to the AI twins' scope lines a split review's call is sent
// (reviewPrompt): its part's call carries them.
func scopeBytes(scope map[string][]string) map[string]int {
	out := map[string]int{}
	for _, files := range scope {
		for _, f := range files {
			out[f] += len(strconv.Quote(f)) + len(", ")
		}
	}
	return out
}

// parts is the calls of a review over the shown files, in order (the tree's path order, so a folder's files
// stay together), each at most room bytes as sent: one call holding every file, sent as before, while they
// fit; else each call is sent paths, every shown file's, and is filled with files and their scope lines
// until the next would pass room, and a file that alone would is in none, named in over. More than
// maxCalls calls, or paths that leave no room, is errTooLarge (contracts/finding.md).
func parts(order []string, source, shown map[string]string, scoped map[string]int, room int) (calls []part, paths, over []string, err error) {
	sizes, total := make(map[string]int, len(order)), 0
	for _, name := range order {
		sizes[name] = len(fileBlock(name, shown[name])) + scoped[name]
		total += sizes[name]
	}
	if total <= room {
		return []part{{source: source, shown: shown}}, nil, nil, nil
	}
	paths, room = order, room-len(pathsBlock(order))
	if room <= 0 {
		return nil, nil, nil, errTooLarge
	}
	var p part
	used := 0
	for _, name := range order {
		size := sizes[name]
		if size > room {
			over = append(over, name)
			continue
		}
		if p.source != nil && used+size > room {
			calls, p = append(calls, p), part{}
		}
		if p.source == nil {
			p, used = part{source: map[string]string{}, shown: map[string]string{}}, 0
		}
		p.source[name], p.shown[name], used = source[name], shown[name], used+size
	}
	if p.source != nil {
		calls = append(calls, p)
	}
	if len(calls) > maxCalls {
		return nil, nil, nil, errTooLarge
	}
	return calls, paths, over, nil
}

// request is req as p's call sends it: p's files, and on a split review every shown path and each AI
// twin's scope narrowed to p's files — a twin with none of them left out of the call. A review that is one
// call sends req as it always did.
func (p part) request(req Request, paths []string) Request {
	req.Files = p.shown
	if paths == nil {
		return req
	}
	req.Paths = paths
	if req.Scope != nil {
		scope := make(map[string][]string, len(req.Scope))
		for rule, files := range req.Scope {
			if mine := slices.DeleteFunc(slices.Clone(files), func(f string) bool { _, ok := p.shown[f]; return !ok }); len(mine) > 0 {
				scope[rule] = mine
			}
		}
		req.Rules = slices.DeleteFunc(slices.Clone(req.Rules), func(r finding.Rule) bool { _, twin := req.Scope[r.ID]; _, mine := scope[r.ID]; return twin && !mine })
		req.Scope = scope
	}
	return req
}

// errTimedOut is call's answer when the review's time ran out before every call answered.
var errTimedOut = errors.New("gate: the model did not answer in the time the review allows")

// call sends each part's request, partsAtOnce at a time, within roundCeiling for each round, and answers
// their responses in order. A part that fails stops the others and fails the review (ErrModel), whatever
// another part answered; else time running out is errTimedOut, and a part out of room ErrOutOfRoom.
func (r *Reviewer) call(ctx context.Context, calls []part) ([]Response, error) {
	resps, errs, failed, late := make([]Response, len(calls)), make([]error, len(calls)), make([]bool, len(calls)), make([]bool, len(calls))
	timed, cancel := context.WithTimeout(ctx, time.Duration((len(calls)+partsAtOnce-1)/partsAtOnce)*cmp.Or(r.round, roundCeiling))
	defer cancel()
	failing, fail := context.WithCancel(timed)
	defer fail()
	var g errgroup.Group
	g.SetLimit(partsAtOnce)
	for i, p := range calls {
		g.Go(func() error {
			resps[i], errs[i] = r.Model.Review(failing, p.req)
			late[i] = timed.Err() != nil // an answer after the time, even one with no error, is not the review's
			// the model's own failure, not one a stopped context caused
			if failed[i] = errs[i] != nil && !errors.Is(errs[i], ErrOutOfRoom) && failing.Err() == nil; failed[i] {
				fail()
			}
			return nil
		})
	}
	_ = g.Wait()
	if ctx.Err() != nil { // the run's own end, not the model's
		return nil, ctx.Err()
	}
	if i := slices.Index(failed, true); i >= 0 {
		return nil, fmt.Errorf("%w: %v", ErrModel, errs[i])
	}
	if slices.Contains(late, true) {
		return nil, errTimedOut
	}
	if i := slices.IndexFunc(errs, func(err error) bool { return err != nil }); i >= 0 {
		return nil, errs[i] // ErrOutOfRoom
	}
	return resps, nil
}

// templates are the files that render pages, shown beside the application languages' files.
var templates = []string{".html", ".htm", ".erb", ".jinja", ".jinja2", ".j2", ".vue", ".svelte"}

// shown is whether the reviewer is shown a file: source only — every file of an application language,
// by Coverage's reading of it, and the templates (contracts/finding.md) — never a dotenv file, nor anything
// vendored.
func shown(tree Tree, name string) (bool, error) {
	if dotenv(path.Base(name)) || vendored(name) {
		return false, nil
	}
	return reviewable(tree, name)
}

// dotenv is whether base names a dotenv file: .env, or .env.<anything> (.env.local, .env.production) —
// never .envoy.ts or .environment.js, which are code (contracts/finding.md; #2280).
func dotenv(base string) bool {
	return base == ".env" || strings.HasPrefix(base, ".env.")
}

// reviewable is whether name is a file of an application language or a template. go-enry names no language
// for content it takes for binary — one NUL byte in a .ts comment — so a file it names none for, whose
// extension is an application language's, is one when its first 16 KiB are text: a byte never hides code
// from the review, and a video named .ts is never read whole (#2280). Past those bytes excerpts reads it
// whole, and names it when it is not valid UTF-8.
func reviewable(tree Tree, name string) (bool, error) {
	if slices.Contains(templates, path.Ext(name)) {
		return true, nil
	}
	lang, err := languageOf(tree, name)
	if err != nil || lang != "" || !appExtension(name) {
		return applications[lang], err
	}
	head, err := tree.head(name)
	if err != nil {
		return false, err
	}
	if i := len(head) - 1; len(head) == 16<<10 { // a character the 16 KiB cut through is no invalid byte
		for i > 0 && len(head)-i < utf8.UTFMax && !utf8.RuneStart(head[i]) {
			i--
		}
		if !utf8.FullRune(head[i:]) {
			head = head[:i]
		}
	}
	return utf8.Valid(head), nil
}

// dependencies is each folder of tree that holds source under .git/, node_modules/ or vendor/, its path up to
// that directory, in path order: the reviewer is never shown it, and its run names them (contracts/finding.md).
func dependencies(tree Tree) ([]string, error) {
	var out []string
	for _, name := range tree.Files() {
		if !vendored(name) {
			continue
		}
		dirs := strings.Split(path.Dir(name), "/")
		folder := strings.Join(dirs[:slices.IndexFunc(dirs, dependencyDir)+1], "/")
		if slices.Contains(out, folder) {
			continue
		}
		ok, err := reviewable(tree, name)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, folder)
		}
	}
	slices.Sort(out)
	return out, nil
}

// excerpts is every shown UTF-8 file, with each secret gitleaks finds in one replaced by masked
// wherever it appears, longest first as New does; a multi-line secret keeps its line breaks, so line
// numbers hold — the same files as written, and the shown files that are not valid UTF-8, in path order,
// which no call is sent. Over maxSource it fails.
func excerpts(root string) (source, shownText map[string]string, notUTF8 []string, err error) {
	tree, err := Walk(root)
	if err != nil {
		return nil, nil, nil, err
	}
	source, size := map[string]string{}, 0
	for _, name := range tree.Files() {
		ok, err := shown(tree, name)
		if err != nil {
			return nil, nil, nil, err
		}
		if !ok {
			continue
		}
		data, err := tree.Read(name)
		if err != nil {
			return nil, nil, nil, err
		}
		if !utf8.Valid(data) {
			notUTF8 = append(notUTF8, name)
			continue
		}
		if size += len(data); size > maxSource {
			return nil, nil, nil, errTooLarge
		}
		source[name] = string(data)
	}
	d, err := detector()
	if err != nil {
		return nil, nil, nil, err
	}
	var literals []string
	for _, text := range source {
		for _, f := range d.DetectString(text) {
			literals = append(literals, cmp.Or(f.Secret, f.Match))
		}
	}
	slices.Sort(literals)
	literals = slices.Compact(literals) // one per secret, not one per place it was found
	shownText = map[string]string{}
	for name, text := range source {
		shownText[name] = maskAll(text, literals)
	}
	return source, shownText, notUTF8, nil
}

// maskAll is text with every character any literal covers masked: each run of overlapping occurrences
// becomes one mask holding its line breaks, so line numbers hold. A literal that overlaps another is
// masked whole with it — masking one and then the other left the second's tail, or the first's start,
// for the model (#2279).
func maskAll(text string, literals []string) string {
	var spans [][2]int
	for _, literal := range literals {
		for at := 0; literal != ""; {
			i := strings.Index(text[at:], literal)
			if i < 0 {
				break
			}
			spans = append(spans, [2]int{at + i, at + i + len(literal)})
			at += i + 1
		}
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return cmp.Or(a[0]-b[0], b[1]-a[1]) })
	var out strings.Builder
	done := 0
	for i := 0; i < len(spans); {
		start, end := spans[i][0], spans[i][1]
		for i++; i < len(spans) && spans[i][0] < end; i++ {
			end = max(end, spans[i][1])
		}
		out.WriteString(text[done:start] + masked + strings.Repeat("\n", strings.Count(text[start:end], "\n")))
		done = end
	}
	return out.String() + text[done:]
}

type runKey struct{}

// WithRun is ctx carrying the run and project a review serves, which its drop log lines name.
func WithRun(ctx context.Context, run, project string) context.Context {
	return context.WithValue(ctx, runKey{}, [2]string{run, project})
}

// Why candidateResult refuses a candidate: a fixed key its log line names, never the candidate's text.
var (
	errUnknownRule  = errors.New("unknown_rule")
	errFileNotShown = errors.New("file_not_shown")
	errCredential   = errors.New("credential")
	errLinesOutside = errors.New("lines_outside")
	errBlank        = errors.New("blank")
	errRepeat       = errors.New("repeat")
)

// spot is a result's rule and region, which a repeat of it shares whatever its words.
type spot struct {
	rule, uri                                  string
	startLine, startColumn, endLine, endColumn int
}

func spotOf(res finding.Result) spot {
	pl := res.Locations[0].PhysicalLocation
	return spot{res.RuleID, pl.ArtifactLocation.URI, pl.Region.StartLine, pl.Region.StartColumn, pl.Region.EndLine, pl.Region.EndColumn}
}

// rank orders the four severities, low to critical.
func rank(s finding.Severity) int {
	return slices.Index([]finding.Severity{finding.Low, finding.Medium, finding.High, finding.Critical}, s)
}

// logDrop is one server log line per dropped candidate: the run and project, why, and its rule when
// the rule is one the reviewer may emit — a rule the model made up is its text, and never logged. A
// refusal finding.New makes is "profile" with New's own words, which name no value.
func logDrop(ctx context.Context, c Candidate, allowed map[string]finding.Rule, why error) {
	at, _ := ctx.Value(runKey{}).([2]string)
	line := fmt.Sprintf("gate: run %s (%s): ai review dropped a candidate: %v", at[0], at[1], why)
	if _, ok := allowed[c.Rule]; ok {
		line += ", rule " + c.Rule
	}
	log.Print(line)
}

// candidateResult builds a candidate under its allowed rule via finding.New on its named lines, first to last non-space
// character of the real text, its snippet the masked lines the model was shown (windowed, with the
// caret re-measured on them, as finding.Result.Show does); a candidate under a rule not allowed, on
// a file not shown, or whose text carries a credential d finds is refused, and a blank title, problem
// or fix reads Canyon's words for the rule; a severity outside the four is read as critical, the highest. What the model gets
// wrong by size is fitted to the profile, not refused: lines past the contract's snippet cap are windowed
// off (canyonv2-1x5f) — the region runs from the first non-blank line of the span to the last one
// within MaxSnippetLines of it — and a title over the profile's cap is cut to it (canyonv2-p97l). A
// span of blank lines alone names no code and is refused. The governance prose is fitted too — each
// do_not entry to one line, an empty one left out — and a candidate that will block (its rule blocking,
// at critical or high) without a required_end_state or a do_not carries Canyon's own in their place,
// never dropped (contracts/finding.md, canyonv2-ihqr). A refused candidate is dropped, counted and
// logged by its key (logDrop), never the review's failure (canyonv2-4lr6).
func candidateResult(d *detect.Detector, c Candidate, allowed map[string]finding.Rule, states map[string]policy.State, source, shownText map[string]string) (finding.Result, error) {
	rule, ok := allowed[c.Rule]
	if !ok {
		return finding.Result{}, errUnknownRule
	}
	text, ok := source[c.File]
	if !ok {
		return finding.Result{}, errFileNotShown
	}
	// A blank title, problem or fix is Canyon's words for the rule, never a drop: the model named a place
	// under a rule it may emit, and every rule has a name (contracts/finding.md, #1962).
	if strings.TrimSpace(c.Title) == "" {
		c.Title = rule.Name
	}
	if strings.TrimSpace(c.Problem) == "" {
		c.Problem = fmt.Sprintf("Canyon's AI review found that this code breaks \"%s\" (%s) and gave no more detail.", rule.Name, rule.ID)
	}
	if strings.TrimSpace(c.Fix) == "" {
		c.Fix = fmt.Sprintf("Change this code so it follows \"%s\": Canyon's review then no longer reports %s here.", rule.Name, rule.ID)
	}
	var doNot []string // each entry fitted to one line, an empty one left out (contracts/finding.md)
	for _, entry := range c.DoNot {
		if line := strings.Join(strings.Fields(entry), " "); line != "" {
			doNot = append(doNot, line)
		}
	}
	endState := strings.TrimSpace(c.RequiredEndState)
	severity := finding.Severity(c.Severity)
	if _, err := severity.Level(); err != nil {
		severity = finding.Critical
	}
	if strings.HasPrefix(c.Rule, "custom.") { // a custom rule's finding is high whatever the model says, so Blocking blocks it
		severity = finding.High
	}
	if finding.Blocks(c.Rule, states[c.Rule], severity) && (endState == "" || len(doNot) == 0) {
		endState, doNot = canyonsGovernance(c.Rule)
	}
	if len(d.DetectString(strings.Join(append([]string{c.Title, c.Problem, c.Fix, endState}, doNot...), "\n"))) > 0 {
		return finding.Result{}, errCredential
	}
	lines := strings.Split(text, "\n")
	if c.StartLine < 1 || c.EndLine < c.StartLine || c.EndLine > len(lines) {
		return finding.Result{}, errLinesOutside
	}
	startLine, endLine := c.StartLine, c.EndLine
	for startLine < endLine && strings.TrimSpace(lines[startLine-1]) == "" {
		startLine++
	}
	endLine = min(endLine, startLine+finding.MaxSnippetLines-1)
	for endLine > startLine && strings.TrimSpace(lines[endLine-1]) == "" {
		endLine--
	}
	span, joined, from, to, ok := reviewSpan(lines, strings.Split(shownText[c.File], "\n"), startLine, endLine)
	if !ok {
		return finding.Result{}, errBlank
	}
	span.URI = c.File
	res, err := finding.New(finding.Input{
		Rule: rule, Severity: severity, Title: clip(c.Title, maxTitle), Problem: c.Problem, Fix: c.Fix, RequiredEndState: endState, DoNot: doNot,
		Location: span,
	})
	if err != nil {
		return finding.Result{}, fmt.Errorf("profile (%w)", err)
	}
	res.Show(joined, from, to)
	return res, nil
}

// reviewSpan is a reviewer's result over lines start to end of a file, both 1-based and inclusive: its span on the
// file as written (lines), the first to the last non-space, and its lines as shown (shown, the file masked as the
// model is shown it) with the caret on them, the same first to last non-space, for Result.Show. ok is false when its
// first or last line is blank. The reviewer builds a candidate's snippet with it, and Carry rebuilds an earlier
// result's to tell whether its code stands (contracts/finding.md).
func reviewSpan(lines, shown []string, start, end int) (span finding.Span, joined string, from, to int, ok bool) {
	src := lines[start-1 : end]
	first, last := []rune(src[0]), []rune(src[len(src)-1])
	at := slices.IndexFunc(first, func(r rune) bool { return !unicode.IsSpace(r) })
	upTo := len([]rune(strings.TrimRightFunc(string(last), unicode.IsSpace)))
	if at < 0 || upTo == 0 {
		return finding.Span{}, "", 0, 0, false
	}
	span = finding.Span{StartLine: start, StartColumn: at + 1, EndLine: end, EndColumn: upTo + 1, Lines: src}
	// The snippet is the lines as shown, and the caret the same first-to-last non-space on them.
	view := shown[start-1 : end]
	joined, tail := strings.Join(view, "\n"), view[len(view)-1]
	from = max(0, slices.IndexFunc([]rune(view[0]), func(r rune) bool { return !unicode.IsSpace(r) })) // a line inside a masked literal is shown blank
	to = utf8.RuneCountInString(joined) - utf8.RuneCountInString(tail) + utf8.RuneCountInString(strings.TrimRightFunc(tail, unicode.IsSpace))
	return span, joined, from, to, true
}

// maxTitle is the reviewer's title cap, under the profile's 120 (contracts/schema/finding.schema.json): a
// reviewer writes its own title, so it keeps the shorter one a card reads at a glance.
const maxTitle = 80

// clip cuts s to at most n code points, the last an ellipsis: its first n-1 cut back to their last
// space, or all of them when there is none, trailing space dropped — never mid-word.
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
