package finding

import (
	"errors"
	"fmt"
	"html"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// Span is where a finding points: the region plus the source lines it covers (StartLine
// through EndLine), unredacted — New redacts them. Columns count characters,
// not bytes, and EndColumn is exclusive. On a credential finding the span is the string
// literal as the producer's parser delimited it — quotes included, as the golden log's is —
// never a guess at where a literal starts inside arbitrary text. An external-SARIF
// producer's adapter must deliver this shape or refuse; New does not guess.
type Span struct {
	URI         string
	StartLine   int
	StartColumn int
	EndLine     int
	EndColumn   int
	Lines       []string

	// Delimited is the string-literal token's source text, delimiters included, exactly as the
	// producer's parser delimited it — empty when nothing declared one. It is checked against the
	// text the span's own coordinates cut out of Lines, so a span that names the wrong columns
	// cannot be declared: the two come from one token or they disagree. That is the bug a scanner
	// mapping a tokenizer's output onto a Span actually makes.
	//
	// What it cannot do is re-derive lexical state. A producer that fabricates a token
	// consistently — text and coordinates both — is believed, because telling a literal's opening
	// delimiter from the closing one of the literal before it needs the lexer that read the file,
	// not the line it sat on. So a declared span is the producer's parser's assertion, and the
	// gate's defence is that in-tree producers are reviewed. The SARIF wire format carries nothing
	// to declare from, so an adapter cannot and keeps the conservative reading below.
	Delimited string
}

// Input is what a producer knows when it constructs a finding. Blocks is absent on
// purpose: the server derives it after policy lookup (seam 5) and overwrites whatever a
// producer wrote.
type Input struct {
	Rule       Rule
	Severity   Severity
	Title      string
	Problem    string
	Fix        string
	SecretName string
	Location   Span
	Related    []Span

	RequiredEndState string
	DoNot            []string

	// Hidden is an imported tool's finding whose region is hidden whole (a secret's, a credential's, a
	// file-level one, …): its snippet is the first line up to the region, the mask, and the last line
	// after it, one line whatever the region spans, and any live key left in it is masked as on
	// secrets.* (contracts/finding.md, In: external SARIF).
	Hidden bool

	// Cut is an imported tool's finding whose region runs past the snippet's three lines: its snippet
	// is the region's first three lines, from the first line's start, shown as any region is
	// (contracts/finding.md, In: external SARIF).
	Cut bool
}

// New constructs one result. Redaction happens here and nowhere later: on a credential
// finding — any rule in the secrets namespace — the text inside every span, Location's and
// each Related one, is a literal. Each is masked in its snippet and replaced wherever it
// appears in any string of the result, longest literal first so one that contains another
// is never broken before it is replaced. Whatever the rule, every live key on a span's lines is
// masked in its snippet too: the snippet is the whole line. New then refuses a result in which any
// literal or live key survives. The half of the profile a
// schema cannot state is checked here too (Result.validate); the lexical half — patterns,
// lengths, bounds — is the two schemas' job and runs on the serialized log.
func New(in Input) (Result, error) {
	for _, s := range append([]string{in.Rule.ID, in.Rule.Name, in.Title, in.Problem, in.Fix, in.SecretName, in.RequiredEndState}, in.DoNot...) {
		if !utf8.ValidString(s) { // invalid bytes would be sanitized to U+FFFD by json marshal, breaking every literal match while the rest of the credential ships
			return Result{}, errors.New("a producer string is not valid UTF-8")
		}
	}
	namespace, _, ok := strings.Cut(in.Rule.ID, ".")
	if !ok || namespace == "" || in.Rule.Name == "" {
		return Result{}, errors.New("the rule has no namespace or no name")
	}
	spans := append([]Span{in.Location}, in.Related...)
	for n, span := range spans {
		if err := span.check(in.Hidden); err != nil {
			return Result{}, fmt.Errorf("span %d: %w", n, err)
		}
	}
	var literals []string
	if namespace == "secrets" && !in.Hidden { // an imported secret's region is hidden whole, so it names no literal
		for n, span := range spans {
			literal, err := span.literal()
			if err != nil {
				return Result{}, fmt.Errorf("span %d: %w", n, err)
			}
			literals = append(literals, literal)
		}
		sort.SliceStable(literals, func(i, j int) bool { return len([]rune(literals[i])) > len([]rune(literals[j])) })
	}
	scrub := func(s string) string {
		for _, literal := range literals {
			s = strings.ReplaceAll(s, literal, mask(literal))
		}
		return s
	}
	// A snippet is the span's whole line, so it can carry a key some other finding names — two on
	// one line are two findings — or a key no rule here names. Whatever the rule, each live key on
	// the spans' lines is masked here as well, or it survives into the snippet and the refusal below
	// takes a legal input for a leak. The keys are found once, over the whole lines: gitleaks reads a
	// key by what surrounds it, so the pieces a snippet is checked in would each find a different set.
	var keys []string
	for _, span := range spans {
		found, err := keysIn(strings.Join(span.Lines, "\n"))
		if err != nil {
			return Result{}, err
		}
		keys = append(keys, found...)
	}
	sort.SliceStable(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	if in.Cut && !in.Hidden && len(in.Location.Lines) > MaxSnippetLines { // a key the cut would show in part: the attach hides the region whole
		whole, at := strings.Join(in.Location.Lines, "\n"), len(strings.Join(in.Location.Lines[:MaxSnippetLines], "\n"))
		for _, k := range keys {
			for from := 0; ; {
				i := strings.Index(whole[from:], k)
				if i < 0 {
					break
				}
				if from+i < at && from+i+len(k) > at {
					return Result{}, errors.New("a live key runs past the snippet's third line")
				}
				from += i + 1
			}
		}
	}
	redact := func(s string) string {
		s = scrub(s)
		if namespace == "secrets" || in.Hidden {
			s = maskCooked(s)
		}
		for _, k := range keys {
			s = strings.ReplaceAll(s, k, mask(k))
		}
		return s
	}
	level, err := in.Severity.Level()
	if err != nil {
		return Result{}, err
	}
	related := make([]Location, 0, len(in.Related))
	for _, span := range in.Related {
		text, from, to, err := span.snippet(in.Hidden, false, redact)
		if err != nil {
			return Result{}, err
		}
		text, _ = window(text, from, to)
		related = append(related, span.location(text))
	}
	text, from, to, err := in.Location.snippet(in.Hidden, in.Cut, redact)
	if err != nil {
		return Result{}, err
	}
	title := scrub(in.Title)
	blocks := true // fail closed until the server derives it: a finding nobody has judged blocks
	r := Result{
		RuleID:           in.Rule.ID,
		Level:            level,
		Message:          Message{Text: title},
		Locations:        []Location{in.Location.location("")},
		RelatedLocations: related,
		// PartialFingerprints stays empty: the gate derives canyon/v1 last, over the final log
		// after namespace drops, by the recipe in contracts/finding.md. A producer never sets it.
		Properties: Properties{Canyon: Canyon{
			Severity:   in.Severity,
			Blocks:     &blocks,
			Title:      title,
			Problem:    scrub(in.Problem),
			Fix:        scrub(in.Fix),
			PolicyName: scrub(in.Rule.Name),

			RequiredEndState: scrub(in.RequiredEndState),
		}},
	}
	r.Show(text, from, to)
	if in.SecretName != "" {
		r.Properties.Canyon.SecretName = &in.SecretName
	}
	if len(in.DoNot) > 0 {
		r.Properties.Canyon.DoNot = make([]string, len(in.DoNot))
		for i, s := range in.DoNot {
			r.Properties.Canyon.DoNot[i] = scrub(s)
		}
	}
	if err := r.validate(map[string]bool{in.Rule.ID: true}); err != nil { // declaration itself is checked at run assembly, against the snapshot
		return Result{}, err
	}
	for _, s := range r.texts() {
		if HasLiveKey([]byte(s)) {
			return Result{}, errors.New("a live key survived construction")
		}
		for _, literal := range literals {
			if strings.Contains(s, literal) {
				return Result{}, errors.New("a credential literal survived redaction")
			}
		}
	}
	return r, nil
}

// texts is every string a result serializes.
func (r Result) texts() []string {
	c := r.Properties.Canyon
	out := []string{r.RuleID, r.Message.Text, r.PartialFingerprints.CanyonV1, c.Title, c.Problem, c.Fix, c.PolicyName, c.RequiredEndState}
	out = append(out, c.DoNot...)
	if c.SecretName != nil {
		out = append(out, *c.SecretName)
	}
	for _, loc := range append(r.Locations, r.RelatedLocations...) {
		out = append(out, loc.PhysicalLocation.ArtifactLocation.URI, loc.PhysicalLocation.Region.Snippet.Text)
	}
	return out
}

// The mask keeps the identifying prefix and the last four (design/specs/cli.md: "The prefix
// and last four are enough to identify it"; cli.md's sk_live_51QarNv…7xT9 and the golden
// sk_live_51MZq8f••••7Kx2 both keep 15 and 4) around a fixed four bullets, so the length is
// not disclosed, and the prefix never exceeds the hidden middle. Too short to hide eight
// characters that way is hidden whole. Shorter than minLiteral is refused: the literal is
// replaced wherever it occurs in the result's prose, and a few characters occur everywhere.
const (
	maskHead   = 15
	maskTail   = 4
	maskHidden = 8
	minLiteral = 8
	bullets    = "••••"
)

// Mask is what New writes over a credential literal; the reviewer's request masks with it too.
func Mask(literal string) string { return mask(literal) }

func mask(literal string) string {
	r := []rune(literal)
	// A literal that spans lines is hidden whole. The head and tail would otherwise carry the
	// template's own line terminators, so the redacted snippet would still be as many lines as
	// the template and the ≤3 cap would refuse it after redaction rather than before — the same
	// aborted scan, one step later. Hiding it whole makes the collapse structural: what is left
	// is the first line up to the opening delimiter, the mask, and the last line after the
	// closing one, which is one line however long the template.
	if strings.ContainsAny(literal, terminators) {
		return bullets
	}
	if len(r) < maskHead+maskTail+maskHidden {
		return bullets
	}
	head := min(maskHead, (len(r)-maskTail)/2)
	return string(r[:head]) + bullets + string(r[len(r)-maskTail:])
}

// literal is the credential a span names: the span's text without the string delimiters
// the parser included. A span that ends inside a word — the character before or after it
// is a letter, digit or underscore — bisects the credential; the rest would stay in the
// snippet, so it is refused.
func (s Span) literal() (string, error) {
	first, last := []rune(s.Lines[0]), []rune(s.Lines[len(s.Lines)-1])
	// Undeclared, the neighbours are all there is to go on, and refusing only word runes was not
	// enough: a credential ends on whatever its alphabet ends on, and base64 padding, a JWT's dots
	// and a UUID's dashes are none of them — so a span from one literal's closing delimiter to the
	// next literal's passed whenever the character before it was punctuation. The rule is now an
	// allowlist of what may stand beside a literal rather than a denylist of what may not, so
	// anything the lexer would not have put there is refused. It is a guess kept conservative, not
	// a verified token; a producer that parsed the file declares one instead.
	if s.Delimited == "" {
		if before := first[:s.StartColumn-1]; len(before) > 0 && !mayAbut(before[len(before)-1]) {
			return "", errors.New("the span does not start where a string literal may start")
		}
		if after := last[s.EndColumn-1:]; len(after) > 0 && !mayAbut(after[0]) {
			return "", errors.New("the span does not end where a string literal may end")
		}
	}
	var text string
	if s.StartLine == s.EndLine {
		text = string(first[s.StartColumn-1 : s.EndColumn-1])
	} else {
		parts := []string{string(first[s.StartColumn-1:])}
		parts = append(parts, s.Lines[1:len(s.Lines)-1]...)
		parts = append(parts, string(last[:s.EndColumn-1]))
		text = strings.Join(parts, "\n")
	}
	// A declaration is checked against the span's own coordinates first: the token the producer
	// names has to be the text those columns cut out of those lines, or one of the two is wrong and
	// nothing here can say which. Then the token has to look like one — opened and closed by the
	// same delimiter, closed by one that is not itself escaped (a span cut at a \" ends inside the
	// literal and leaves the rest of it in the snippet), and carrying no unescaped delimiter in
	// between.
	// Only the declaration is the producer's word against the span's own coordinates. Everything
	// after it is about the text those coordinates cut out, and an UNDECLARED span makes exactly
	// the same claim — that this is one string literal — so it is held to the same rules. Gating
	// them on a declaration left the whole external-SARIF path with only the neighbour check, and
	// two of the three shapes this package refuses came straight back on it.
	if s.Delimited != "" && text != s.Delimited {
		return "", errors.New("the span's lines and columns do not cut out the token the producer declared")
	}
	r := []rune(text)
	if len(r) < 2 || !strings.ContainsRune("\"'`", r[0]) {
		return "", errors.New("the span does not open with a string delimiter")
	}
	// A span cut at a `\"` ends INSIDE the literal and leaves the rest of it in the snippet.
	if r[len(r)-1] != r[0] || escapedAt(r, len(r)-1) {
		return "", errors.New("the span is not closed by the delimiter it opens with")
	}
	// An unescaped delimiter in between means the span closes on some LATER token's end and has
	// swallowed everything to it — masking across two literals and leaving a fragment of each.
	for i := 1; i < len(r)-1; i++ {
		if r[i] == r[0] && !escapedAt(r, i) {
			return "", errors.New("the span carries its own delimiter unescaped")
		}
	}
	literal := unquote(text)
	if len([]rune(literal)) < minLiteral {
		return "", errors.New("the span is too short to be a credential")
	}
	return literal, nil
}

// escapedAt reports whether r[i] is preceded by an odd number of backslashes, which is what makes
// a delimiter the literal's own character rather than the one that ends it.
func escapedAt(r []rune, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && r[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// mayAbut reports whether a rune may stand immediately beside a string literal's delimiter: space,
// or the punctuation an expression is built from. Everything else — a letter, a digit, an
// underscore, and every character a credential's own alphabet ends on — means the span begins or
// ends somewhere a literal does not, which on an undeclared span is all there is to go on. `return
// "x"` written without the space is refused by this, as it was by the word rule before it.
func mayAbut(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("(),[]{}:;?*%!<>&|^~", r)
}

// unquote strips one pair of matching string delimiters; the quotes stay in the snippet
// around the mask.
func unquote(span string) string {
	if len(span) >= 2 && strings.ContainsRune("\"'`", rune(span[0])) && span[len(span)-1] == span[0] {
		return span[1 : len(span)-1]
	}
	return span
}

// check does not cap the span's line count. The ≤3 rule binds the snippet as serialized, which
// Location.validate applies after redaction — the only form a log off the wire ever has, and the
// only form in which a secrets.* template has collapsed to the mask. Capping here as well would
// refuse a long literal before it could collapse, which is canyonv2-xqx exactly, and off secrets.*
// it would refuse nothing Location.validate does not already refuse: with no literals, scrub is
// the identity and the snippet has the span's own lines. A hidden span's line may carry a lone CR:
// an imported region's lines are placed by SARIF's default newlines, \r\n and \n, and the hidden
// snippet is cut at every terminator, so the CR never serializes.
func (s Span) check(hidden bool) error {
	if s.URI == "" {
		return errors.New("a span has no uri")
	}
	n := s.EndLine - s.StartLine + 1
	if s.StartLine < 1 || n < 1 || len(s.Lines) != n {
		return fmt.Errorf("a span covers %d lines but %d were given", n, len(s.Lines))
	}
	if !utf8.ValidString(s.URI) {
		return errors.New("a span's text is not valid UTF-8")
	}
	breaks := "\r\n"
	if hidden {
		breaks = "\n"
	}
	for _, line := range s.Lines {
		if strings.ContainsAny(line, breaks) {
			return errors.New("a line carries a line break")
		}
		if !utf8.ValidString(line) {
			return errors.New("a span's text is not valid UTF-8")
		}
	}
	first, last := len([]rune(s.Lines[0])), len([]rune(s.Lines[n-1]))
	if s.StartColumn < 1 || s.StartColumn > first+1 || s.EndColumn < 1 || s.EndColumn > last+1 || (n == 1 && s.StartColumn >= s.EndColumn) {
		return errors.New("a span's columns are outside its line")
	}
	return nil
}

// maskCooked masks every string or template literal in a snippet whose runtime value — escapes decoded,
// or a JSX attribute's entities — is a live key, which the raw text cannot show in `"sk\x5flive_…"`:
// tdewolff's JavaScript lexer finds the literals, and goja's parser decodes each. What the lexer cannot
// read on its own, such as the rest of a line inside a multi-line construct or after JSX prose, is left
// to the keys found in its raw text.
func maskCooked(s string) string {
	l, ended, out := js.NewLexer(parse.NewInputString(s)), false, s
	for {
		tt, text := l.Next()
		if (tt == js.DivToken || tt == js.DivEqToken) && !ended {
			tt, text = l.RegExp() // a / where no operand ended starts a regular expression, the grammar's rule
		}
		switch tt {
		case js.ErrorToken:
			if len(text) > 0 { // a rune JavaScript has no token for, a decorator's @ among them: the lexer reads on past it
				continue
			}
			return out
		case js.StringToken, js.TemplateToken, js.TemplateStartToken, js.TemplateMiddleToken, js.TemplateEndToken:
			open, raw, end := literal(tt, string(text))
			lit := string(text) // a template's part is read as a template of its own
			if tt != js.StringToken {
				lit = "`" + raw + "`"
			}
			cooked, ok := cookedOf(lit)
			if ok && cooked != raw && HasLiveKey([]byte(cooked)) || tt == js.StringToken && html.UnescapeString(raw) != raw && HasLiveKey([]byte(html.UnescapeString(raw))) {
				out = strings.ReplaceAll(out, string(text), open+mask(raw)+end) // unescaped, the key is one New found on the line
			}
		case js.WhitespaceToken, js.LineTerminatorToken, js.CommentToken, js.CommentLineTerminatorToken:
			continue
		}
		// TypeScript's non-null ! after an operand still ends it, and </ closes a JSX element, not a regular expression.
		ended = endsOperand(tt) || tt == js.NotToken && ended || tt == js.LtToken
	}
}

// literal is a string or template part's text as its opening, its raw content and its close: `"…"`,
// “ `…` “, “ `…${ “, `}…${` or “ }…` “.
func literal(tt js.TokenType, text string) (open, raw, end string) {
	open, end = text[:1], text[len(text)-1:]
	if tt == js.TemplateStartToken || tt == js.TemplateMiddleToken {
		end = "${"
	}
	return open, text[1 : len(text)-len(end)], end
}

// cookedOf is a string or template literal's runtime value, as goja's parser reads the literal alone.
func cookedOf(lit string) (string, bool) {
	prog, err := parser.ParseFile(nil, "", "("+lit+")", 0)
	if err != nil || len(prog.Body) != 1 {
		return "", false
	}
	switch e := prog.Body[0].(*ast.ExpressionStatement).Expression.(type) {
	case *ast.StringLiteral:
		return e.Value.String(), true
	case *ast.TemplateLiteral:
		return e.Elements[0].Parsed.String(), len(e.Elements) == 1
	}
	return "", false
}

// endsOperand is whether a token ends an operand, so a / after it divides.
func endsOperand(tt js.TokenType) bool {
	switch tt {
	case js.StringToken, js.TemplateToken, js.TemplateEndToken, js.RegExpToken, js.CloseParenToken, js.CloseBracketToken,
		js.CloseBraceToken, js.IncrToken, js.DecrToken, js.ThisToken, js.SuperToken, js.NullToken, js.TrueToken, js.FalseToken,
		js.PrivateIdentifierToken:
		return true
	}
	return js.IsNumeric(tt) || js.IsIdentifier(tt) && tt != js.OfToken // for (x of /re/…) starts one
}

// snippet is the span's lines redacted, and the region's code-point offsets in them. Redaction runs
// over the whole text; the text before the region and the region, redacted on their own, must be
// how it begins, or the offsets would not say where the region went, and the finding is refused.
// Hidden, the region is the mask and the text one line: the first line's part before it from the
// last terminator there, the last line's part after it up to the first one. Cut past three lines, the
// text is the first three and the region ends at the third's end.
func (s Span) snippet(hidden, cut bool, redact func(string) string) (text string, from, to int, err error) {
	r := []rune(strings.Join(s.Lines, "\n"))
	start, end := s.StartColumn-1, len(r)-len([]rune(s.Lines[len(s.Lines)-1]))+s.EndColumn-1
	if cut && !hidden && len(s.Lines) > MaxSnippetLines {
		r = []rune(strings.Join(s.Lines[:MaxSnippetLines], "\n"))
		end = len(r)
	}
	if hidden {
		head, tail := r[start-column(r, start)+1:start], r[end:]
		if i := slices.IndexFunc(tail, func(c rune) bool { return strings.ContainsRune(terminators, c) }); i >= 0 {
			tail = tail[:i]
		}
		r = slices.Concat(head, []rune(bullets), tail)
		start, end = len(head), len(head)+utf8.RuneCountInString(bullets)
	}
	text, before, region := redact(string(r)), redact(string(r[:start])), redact(string(r[start:end]))
	if !strings.HasPrefix(text, before+region) {
		return "", 0, 0, errors.New("redaction moved the span within its snippet")
	}
	from = utf8.RuneCountInString(before)
	return text, from, from + utf8.RuneCountInString(region), nil
}

// A snippet over maxSnippet characters — the profile schema's cap — is cut to a window of it:
// from windowLead characters before the region, moved back at most wordBack to the start of the
// word the cut would split, maxSnippet-2 characters long, with an ellipsis wherever text was cut
// (contracts/finding.md). It is cut after redaction, so the window shows
// only what the whole snippet would have, and a region longer than the window is cut with it.
const (
	maxSnippet = 400
	windowLead = 100
	wordBack   = 50 // gitleaks' look-back over a key's name: ublicKey, cut from publicKey, is a name it reads
	ellipsis   = "…"
)

// inWord reports whether c is part of a word the window never cuts: a letter, a digit, _, . or -.
func inWord(c rune) bool {
	return unicode.IsLetter(c) || unicode.IsDigit(c) || strings.ContainsRune("_.-", c)
}

// window cuts text to the window around the region at from…to, code-point offsets, and returns the
// region's place on what is left: its start column on the first line, its end column on the last.
func window(text string, from, to int) (string, Shown) {
	r := []rune(text)
	if len(r) > maxSnippet {
		lo := max(0, from-windowLead)
		for back := 0; lo > 0 && back < wordBack && inWord(r[lo-1]); back++ {
			lo--
		}
		// cut is the window from lo as it ships, an ellipsis at each cut end, and where it ends.
		cut := func(lo int) (string, int) {
			hi := min(len(r), lo+maxSnippet-2)
			head, tail := "", ""
			if lo > 0 {
				head = ellipsis
			}
			if hi < len(r) {
				tail = ellipsis
			}
			return head + string(r[lo:hi]) + tail, hi
		}
		// A cut can drop a match the detector skips whole and leave a key name inside its value
		// reading as a key (publicKey = "token=…"): the start, and the end with it, moves back a word
		// at a time until the window holds no key the whole text did not (#1374). One that would put
		// the region past the window stops it there, and New's backstop refuses the finding, as before.
		fresh := newKeys(text)
		for shown, _ := cut(lo); lo > 0 && fresh(shown); shown, _ = cut(lo) {
			back := lo - 1
			for back > 0 && !inWord(r[back]) {
				back--
			}
			for back > 0 && inWord(r[back-1]) {
				back--
			}
			if from >= back+maxSnippet-2 {
				break
			}
			lo = back
		}
		shown, hi := cut(lo)
		shift := -lo
		if lo > 0 {
			shift += utf8.RuneCountInString(ellipsis)
		}
		from, to = from+shift, min(to, hi)+shift
		r = []rune(shown)
	}
	return string(r), Shown{StartColumn: column(r, from), EndColumn: column(r, to)}
}

// newKeys answers whether a part of whole holds a live key that whole does not. whole is read once,
// and only when a part holds a key. A detector that will not build reads as a key, as everywhere.
func newKeys(whole string) func(part string) bool {
	var all []string
	var err error
	read := false
	return func(part string) bool {
		keys, perr := keysIn(part)
		if perr != nil || len(keys) == 0 {
			return perr != nil
		}
		if !read {
			all, err = keysIn(whole)
			read = true
		}
		return err != nil || slices.ContainsFunc(keys, func(k string) bool { return !slices.Contains(all, k) })
	}
}

// terminators end a line wherever a snippet's lines are counted or cut: LF, CR (CRLF is the two),
// U+2028 and U+2029 (contracts/finding.md).
const terminators = "\n\r\u2028\u2029"

// column is offset's 1-based column on its line of r, lines ending as lines counts them.
func column(r []rune, offset int) int {
	i := offset
	for i > 0 && !strings.ContainsRune(terminators, r[i-1]) {
		i--
	}
	return offset - i + 1
}

// Show writes text — locations[0]'s snippet as redacted, from…to the region's code-point offsets in
// it — as the snippet, windowed when it is too long, and sets canyon.shown to the caret on it only
// when that is not the region's own columns: a masked literal or a window moved it.
func (r *Result) Show(text string, from, to int) {
	g := &r.Locations[0].PhysicalLocation.Region
	var at Shown
	g.Snippet.Text, at = window(text, from, to)
	r.Properties.Canyon.Shown = nil
	if at != (Shown{StartColumn: g.StartColumn, EndColumn: g.EndColumn}) {
		r.Properties.Canyon.Shown = &at
	}
}

// location is the checked span as the profile serializes it, with its already-redacted snippet.
func (s Span) location(snippet string) Location {
	return Location{PhysicalLocation: PhysicalLocation{
		ArtifactLocation: ArtifactLocation{URI: s.URI},
		Region: Region{
			StartLine: s.StartLine, StartColumn: s.StartColumn, EndLine: s.EndLine, EndColumn: s.EndColumn,
			Snippet: Snippet{Text: snippet},
		},
	}}
}
