package sarif

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/getcanyon-ai/canyon/gate"
)

func treeAt(t *testing.T, root string) gate.Tree {
	t.Helper()
	tree, err := gate.Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// files is the tree the edge cases read.
func files(t *testing.T) gate.Tree {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"a.ts":              "const x = 1;\nconst y = 2;\n",
		"u.ts":              "a😀b = f(x);\n", // U+1F600 is two UTF-16 code units, one code point
		"crlf.ts":           "one\r\ntwo\r\n",
		"cr.ts":             "a\rb = f(x);\ry\n", // lone CRs: lines under some newlineSequences, characters under SARIF's default
		"bad.ts":            "const \xff = 1;\n",
		"sp ace.ts":         "const z = 3;\n",
		"eof.ts":            "x = f(y);\r", // a lone CR ending the file is its last line's own
		"w.ts":              "x=someIdentifier;\n",
		"five.ts":           "1\n2\n3\n4\nfive = 5;\n",
		"blank.ts":          "\r\n\nlet z;\n", // a file-level result names its first non-empty line
		"blanks.ts":         "\n\r\n",         // and in this file there is none
		"blank.bin":         "\r\n\xff\xfe",   // not UTF-8, its line 1 empty
		"Dockerfile":        "EXPOSE 22\n",
		"docker/Dockerfile": "EXPOSE 22\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return treeAt(t, dir)
}

var crAsLine = []string{"\r\n", "\n", "\r"}

// A region is placed by the run's newlineSequences and columnKind and restated as the profile
// states it: SARIF's default newlines, columns in code points, its lines without their terminators.
func TestARegionIsRestated(t *testing.T) {
	for name, c := range map[string]struct {
		newlines     []string
		units        bool
		uri          string
		region, want [4]int
		lines        []string
		text         string
	}{
		"UTF-16 columns past a surrogate pair": {nil, true, "u.ts", [4]int{1, 8, 1, 12}, [4]int{1, 7, 1, 11}, []string{"a😀b = f(x);"}, "f(x)"},
		"code point columns":                   {nil, false, "u.ts", [4]int{1, 7, 1, 11}, [4]int{1, 7, 1, 11}, []string{"a😀b = f(x);"}, "f(x)"},
		"across CRLF lines":                    {nil, true, "crlf.ts", [4]int{1, 2, 2, 3}, [4]int{1, 2, 2, 3}, []string{"one", "two"}, "ne\r\ntw"},
		"a line's end to the next":             {nil, true, "crlf.ts", [4]int{1, 4, 2, 1}, [4]int{1, 4, 2, 1}, []string{"one", "two"}, "\r\n"},
		"lone CRs as lines":                    {crAsLine, true, "cr.ts", [4]int{2, 5, 2, 9}, [4]int{1, 7, 1, 11}, []string{"a\rb = f(x);\ry"}, "f(x)"},
		"lone CRs inside a line":               {nil, true, "cr.ts", [4]int{1, 7, 1, 11}, [4]int{1, 7, 1, 11}, []string{"a\rb = f(x);\ry"}, "f(x)"},
		"a lone CR ending the file":            {nil, true, "eof.ts", [4]int{1, 5, 1, 11}, [4]int{1, 5, 1, 11}, []string{"x = f(y);\r"}, "f(y);\r"},
		"an omitted end":                       {nil, true, "a.ts", [4]int{2, 1, 2, 0}, [4]int{2, 1, 2, 13}, []string{"const y = 2;"}, "const y = 2;"},
		"a percent-encoded uri":                {nil, true, "sp%20ace.ts", [4]int{1, 7, 1, 8}, [4]int{1, 7, 1, 8}, []string{"const z = 3;"}, "z"},
		"file-level: empty at 1:1":             {nil, true, "a.ts", [4]int{1, 1, 1, 1}, [4]int{1, 1, 1, 13}, []string{"const x = 1;"}, "const x = 1;"},
		"file-level: past blank lines":         {nil, true, "blank.ts", [4]int{1, 1, 1, 0}, [4]int{3, 1, 3, 7}, []string{"let z;"}, "let z;"},
		"file-level: SARIF's default newlines": {crAsLine, true, "cr.ts", [4]int{1, 1, 1, 1}, [4]int{1, 1, 1, 14}, []string{"a\rb = f(x);\ry"}, "a\rb = f(x);\ry"},
	} {
		t.Run(name, func(t *testing.T) {
			g := c.region
			span, text, _, drop, err := newLocator(files(t), c.newlines, c.units).locate(c.uri, nil, g[0], g[1], g[2], g[3])
			if err != nil || drop != "" {
				t.Fatalf("dropped %q: %v", drop, err)
			}
			got := [4]int{span.StartLine, span.StartColumn, span.EndLine, span.EndColumn}
			if got != c.want || !reflect.DeepEqual(span.Lines, c.lines) || text != c.text {
				t.Fatalf("got %v %q %q, want %v %q %q", got, span.Lines, text, c.want, c.lines, c.text)
			}
		})
	}
	for _, root := range []string{"%SRCROOT%", "ROOTPATH"} {
		if _, _, _, drop, err := newLocator(files(t), nil, true).locate("a.ts", &root, 1, 7, 1, 8); err != nil || drop != "" {
			t.Errorf("a uri under %s: dropped %q: %v", root, drop, err)
		}
	}
}

// A location Canyon cannot place in the package drops its result, and says which input rule did.
func TestALocationCanyonCannotPlaceIsDropped(t *testing.T) {
	other, rootpath := "OTHER", "ROOTPATH"
	for name, c := range map[string]struct {
		newlines []string
		uri      string
		base     *string
		region   [4]int
		want     string
	}{
		"another uriBaseId":                {nil, "a.ts", &other, [4]int{1, 7, 1, 8}, "uriBaseId is not %SRCROOT% or ROOTPATH"},
		"a .. segment under ROOTPATH":      {nil, "../x", &rootpath, [4]int{1, 7, 1, 8}, "uri carries a .. segment"},
		"a bad percent":                    {nil, "a%zz.ts", nil, [4]int{1, 7, 1, 8}, "uri is not valid percent-encoding"},
		"an absolute uri":                  {nil, "%2Fa.ts", nil, [4]int{1, 7, 1, 8}, "uri is absolute"},
		"a percent-encoded ../":            {nil, "x/%2E%2E%2Fa.ts", nil, [4]int{1, 7, 1, 8}, "uri carries a .. segment"},
		"a backslash":                      {nil, `x\a.ts`, nil, [4]int{1, 7, 1, 8}, "uri carries a backslash"},
		"a scheme":                         {nil, "file:a.ts", nil, [4]int{1, 7, 1, 8}, "uri carries a scheme"},
		"no such file":                     {nil, "b.ts", nil, [4]int{1, 7, 1, 8}, "uri is not a file of the package"},
		"a file not UTF-8":                 {nil, "bad.ts", nil, [4]int{1, 1, 1, 2}, "the file is not valid UTF-8"},
		"a file not UTF-8, line 1 whole":   {nil, "bad.ts", nil, [4]int{1, 1, 1, 0}, "the file is not valid UTF-8"},
		"a start line of 0":                {nil, "a.ts", nil, [4]int{0, 7, 1, 8}, "the region is past its file"},
		"an end line of 0":                 {nil, "a.ts", nil, [4]int{1, 7, 0, 8}, "the region is past its file"},
		"a line past the file":             {nil, "a.ts", nil, [4]int{1, 7, 4, 1}, "the region is past its file"},
		"an empty region":                  {nil, "a.ts", nil, [4]int{1, 7, 1, 7}, "the region is empty"},
		"an empty line to its end":         {nil, "a.ts", nil, [4]int{3, 1, 3, 0}, "the region is empty"},
		"an empty region at 5:3":           {nil, "five.ts", nil, [4]int{5, 3, 5, 3}, "the region is empty"},
		"file-level, no non-empty line":    {nil, "blanks.ts", nil, [4]int{1, 1, 1, 1}, "the file has no non-empty line"},
		"a column past its line":           {nil, "a.ts", nil, [4]int{1, 7, 1, 14}, "the region is past its file"},
		"a column inside a surrogate pair": {nil, "u.ts", nil, [4]int{1, 3, 1, 5}, "the region is past its file"},
		"an end inside a \\r\\n":           {[]string{"\n"}, "crlf.ts", nil, [4]int{1, 1, 1, 5}, "the region is past its file"},
	} {
		t.Run(name, func(t *testing.T) {
			g := c.region
			if _, _, _, drop, err := newLocator(files(t), c.newlines, true).locate(c.uri, c.base, g[0], g[1], g[2], g[3]); err != nil || drop != c.want {
				t.Fatalf("dropped %q (%v), want %q", drop, err, c.want)
			}
		})
	}
}

// A file-level result on a file that is not valid UTF-8 names the file only: line 1, columns 1-2, a
// placeholder, and nothing of the file kept. An omitted end is 1:1 only where line 1 is empty.
func TestAFileLevelResultOnAFileNotUTF8IsThePlaceholder(t *testing.T) {
	for uri, g := range map[string][4]int{"bad.ts": {1, 1, 1, 1}, "blank.bin": {1, 1, 1, 0}} {
		span, hidden, _, drop, err := newLocator(files(t), nil, true).locate(uri, nil, g[0], g[1], g[2], g[3])
		got := [4]int{span.StartLine, span.StartColumn, span.EndLine, span.EndColumn}
		if err != nil || drop != "" || got != [4]int{1, 1, 1, 2} || span.URI != uri || hidden != "" || !reflect.DeepEqual(span.Lines, []string{placeholder}) {
			t.Errorf("%s %v: got %v %q hidden %q, dropped %q (%v); want the placeholder at 1:1-1:2, nothing kept", uri, g, got, span.Lines, hidden, drop, err)
		}
	}
}

// The unredacted-literal check refuses a string carrying 8 code points of its region in a row, and
// counts them as code points, never bytes.
func TestQuotesIsEightCodePointsOfTheRegion(t *testing.T) {
	hidden := `const kéy = "abcdefgh";`
	for s, want := range map[string]bool{
		"the kéy = \"a": true, "abcdefgh": true, `bcdefgh"`: true,
		"abcdefg": false, "the kéy = x": false, "": false,
		"a longer string than the region, ending on abcdefgh": true, // the region's runs are the set, this one is walked
	} {
		if quotes(s, hidden) != want {
			t.Errorf("%q: quotes %v", s, !want)
		}
	}
}
