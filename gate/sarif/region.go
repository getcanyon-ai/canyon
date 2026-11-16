// Package sarif is the SARIF import adapter (contracts/finding.md, In: external SARIF): one tool's
// report, normalised at attach against the policy snapshot and the package's tree into profile
// results, or refused whole. It builds no snippet, mask or fingerprint: every result is finding.New's,
// its region shown or hidden whole, and canyon/v1 is derived in the run by gate.FingerprintImported.
package sarif

import (
	"bytes"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/getcanyon-ai/canyon/gate"
	"github.com/getcanyon-ai/canyon/gate/finding"
)

// minQuoted is the run of a region's code points no string a result takes may carry, on every result
// but a dependency result (sarif.go).
const minQuoted = 8

var scheme = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`) // RFC 3986's scheme and its colon

// locator places one report's regions in the package's tree (finding.md's input rules): each file
// read once, its lines placed by the run's newlineSequences, its columns counted in the run's
// columnKind.
type locator struct {
	tree     gate.Tree
	paths    []string
	files    map[string]*file
	newlines []string
	units    bool // columns count UTF-16 code units, SARIF's default, not code points
}

func newLocator(tree gate.Tree, newlines []string, units bool) *locator {
	if len(newlines) == 0 {
		newlines = []string{"\r\n", "\n"} // SARIF's default
	}
	return &locator{tree: tree, paths: tree.Files(), files: map[string]*file{}, newlines: newlines, units: units}
}

// locate is the span a location names as the profile states it — lines placed by SARIF's default
// newlineSequences, \r\n and \n, which the verdict's run declares, and columns counted in code
// points — with its lines, and the region's text as stored; or the input rule that drops its result.
// An endColumn of 0 is SARIF's default, its line's end. A region empty at 1:1 names its file: the
// span is the file's first non-empty line whole, or, on a file that is not valid UTF-8, the
// placeholder, with nothing of the file kept; fileLevel says it is one of those. Any error is the tree's.
func (l *locator) locate(uri string, base *string, startLine, startColumn, endLine, endColumn int) (span finding.Span, text string, fileLevel bool, drop string, err error) {
	p, drop := path(uri, base)
	if drop != "" {
		return finding.Span{}, "", false, drop, nil
	}
	f, drop, err := l.file(p)
	if err != nil || drop != "" {
		return finding.Span{}, "", false, drop, err
	}
	if f.binary { // only a file-level result names a file with no text Canyon reads: 1:1 to 1:1, or to an empty line 1's end
		if startLine != 1 || startColumn != 1 || endLine != 1 || endColumn != 1 && (endColumn != 0 || !f.blank) {
			return finding.Span{}, "", false, "the file is not valid UTF-8", nil
		}
		return finding.Span{URI: p, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 2, Lines: []string{placeholder}}, "", true, "", nil
	}
	start, placed := f.offset(startLine, startColumn, l.units)
	end, ended := f.offset(endLine, endColumn, l.units)
	if placed && ended && start == end {
		if start > 0 { // a finding with nothing to hide
			return finding.Span{}, "", false, "the region is empty", nil
		}
		if start, end, placed = firstLine(f.src); !placed { // file-level, and nothing for it to name
			return finding.Span{}, "", false, "the file has no non-empty line", nil
		}
		fileLevel = true
	}
	if placed && ended && start < end {
		if got, ok := restate(f.src, start, end); ok {
			got.URI = p
			return got, f.src[start:end], fileLevel, "", nil
		}
	}
	return finding.Span{}, "", false, "the region is past its file", nil
}

// path is the tree path a location names, its uri percent-decoded once, or the input rule that drops
// it. It is only ever looked up in the tree, never joined to a filesystem path.
func path(uri string, base *string) (string, string) {
	if base != nil && *base != "%SRCROOT%" && *base != "ROOTPATH" { // ROOTPATH is trivy's
		return "", "uriBaseId is not %SRCROOT% or ROOTPATH"
	}
	p, err := url.PathUnescape(uri)
	switch {
	case err != nil:
		return "", "uri is not valid percent-encoding"
	case strings.HasPrefix(p, "/"):
		return "", "uri is absolute"
	case strings.Contains(p, `\`):
		return "", "uri carries a backslash"
	case slices.Contains(strings.Split(p, "/"), ".."):
		return "", "uri carries a .. segment"
	case scheme.MatchString(p):
		return "", "uri carries a scheme"
	}
	return p, ""
}

// file is a file of the package, valid UTF-8, with its lines as the report's newlineSequences place
// them: each line's first byte and the byte after it, its terminator excluded. A file that is not
// valid UTF-8 is binary, and all that is kept of it is whether its line 1 is empty (blank): its first
// bytes are one of those sequences, which is all that places a file-level result on it.
type file struct {
	src           string
	lines         [][2]int
	binary, blank bool
}

// placeholder is the one character a file-level result's span covers on a file that is not valid
// UTF-8 (finding.md: line 1, columns 1-2). The span is hidden whole, so it never serializes.
const placeholder = "•"

// file reads the file at p once, or says why a result in it is dropped.
func (l *locator) file(p string) (*file, string, error) {
	if _, listed := slices.BinarySearch(l.paths, p); !listed {
		return nil, "uri is not a file of the package", nil
	}
	f, read := l.files[p]
	if !read {
		raw, err := l.tree.Read(p)
		if err != nil {
			return nil, "", err
		}
		if utf8.Valid(raw) {
			f = &file{src: string(raw)}
			from := 0
			for i := 0; i < len(f.src); {
				n := 0 // the longest sequence ending a line here
				for _, s := range l.newlines {
					if len(s) > n && strings.HasPrefix(f.src[i:], s) {
						n = len(s)
					}
				}
				if n == 0 {
					i++
					continue
				}
				f.lines = append(f.lines, [2]int{from, i})
				i += n
				from = i
			}
			f.lines = append(f.lines, [2]int{from, len(f.src)})
		} else {
			f = &file{binary: true, blank: slices.ContainsFunc(l.newlines, func(s string) bool { return bytes.HasPrefix(raw, []byte(s)) })}
		}
		l.files[p] = f
	}
	return f, "", nil
}

// offset is the byte offset of line:column, the column counted in UTF-16 code units when units is
// set, else in code points, and 0 for its line's end. It is not placed past the file: past its last
// line or before its first, past its line's end, or inside a character.
func (f *file) offset(line, column int, units bool) (int, bool) {
	if line < 1 || line > len(f.lines) {
		return 0, false
	}
	from, to := f.lines[line-1][0], f.lines[line-1][1]
	if column == 0 {
		return to, true
	}
	n := 1 // the column the next character starts at
	for i, c := range f.src[from:to] {
		if n >= column {
			return from + i, n == column
		}
		if n++; units {
			n += utf16.RuneLen(c) - 1
		}
	}
	return to, n == column
}

// firstLine is where src's first non-empty line starts and ends under SARIF's default
// newlineSequences, \r\n and \n, its terminator excluded; false when it has none.
func firstLine(src string) (int, int, bool) {
	for from := 0; from < len(src); {
		to, next := len(src), len(src)
		if n := strings.IndexByte(src[from:], '\n'); n >= 0 {
			to, next = from+n, from+n+1
			if to > from && src[to-1] == '\r' { // the \r\n's
				to--
			}
		}
		if to > from {
			return from, to, true
		}
		from = next
	}
	return 0, 0, false
}

// restate is src[start:end] under SARIF's default newlineSequences, columns in code points, with its
// lines. An end inside a \r\n is on no line.
func restate(src string, start, end int) (finding.Span, bool) {
	for _, o := range []int{start, end} {
		if o > 0 && o < len(src) && src[o-1:o+1] == "\r\n" {
			return finding.Span{}, false
		}
	}
	at := func(o int) (line, column, from int) {
		from = strings.LastIndexByte(src[:o], '\n') + 1
		return strings.Count(src[:from], "\n") + 1, utf8.RuneCountInString(src[from:o]) + 1, from
	}
	var s finding.Span
	var from int
	s.StartLine, s.StartColumn, from = at(start)
	s.EndLine, s.EndColumn, _ = at(end)
	to := len(src)
	if n := strings.IndexByte(src[end:], '\n'); n >= 0 {
		to = end + n
	}
	s.Lines = strings.Split(src[from:to], "\n")
	for i := range s.Lines {
		if i < len(s.Lines)-1 || to < len(src) { // a CR before a line feed is the \r\n's, any other the line's own
			s.Lines[i] = strings.TrimSuffix(s.Lines[i], "\r")
		}
	}
	return s, true
}

// quotes reports whether s carries minQuoted or more code points of hidden in a row: the
// unredacted-literal check's half that reads the region. The shorter string's runs are a set and the
// longer is walked once, so a region of megabytes costs one pass.
func quotes(s, hidden string) bool {
	short, long := s, hidden
	if len(long) < len(short) {
		short, long = long, short
	}
	runs := map[string]bool{}
	each(short, func(run string) bool { runs[run] = true; return false })
	return len(runs) > 0 && each(long, func(run string) bool { return runs[run] })
}

// maskQuoted is s with every stretch of code points lying in a run of minQuoted or more that hidden
// also carries replaced by one mask: the unredacted-literal check's region half, masking rather than
// refusing, so a rule message's code example costs that text and never the report (finding.md). s's
// runs are a map to where each starts and hidden is walked once, as quotes does.
func maskQuoted(s, hidden string) string {
	starts := []int{} // the byte offset of each code point of s, and its end
	for i := range s {
		starts = append(starts, i)
	}
	starts = append(starts, len(s))
	at := map[string][]int{} // a run of s to the code-point indexes it starts at
	for k := 0; k+minQuoted < len(starts); k++ {
		run := s[starts[k]:starts[k+minQuoted]]
		at[run] = append(at[run], k)
	}
	if len(at) == 0 {
		return s
	}
	covered := make([]bool, len(starts)-1)
	each(hidden, func(run string) bool {
		for _, k := range at[run] {
			for m := k; m < k+minQuoted; m++ {
				covered[m] = true
			}
		}
		delete(at, run) // marked once is marked
		return false
	})
	var out strings.Builder
	for k, c := range covered {
		switch {
		case !c:
			out.WriteString(s[starts[k]:starts[k+1]])
		case k == 0 || !covered[k-1]:
			out.WriteString("••••")
		}
	}
	return out.String()
}

// each calls do with every run of minQuoted code points in s, in order, until do returns true.
func each(s string, do func(run string) bool) bool {
	var starts [minQuoted]int // where the last minQuoted code points start, a ring
	n := 0
	for i := range s {
		if n >= minQuoted && do(s[starts[n%minQuoted]:i]) {
			return true
		}
		starts[n%minQuoted], n = i, n+1
	}
	return n >= minQuoted && do(s[starts[n%minQuoted]:])
}
