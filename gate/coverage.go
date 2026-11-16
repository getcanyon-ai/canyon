package gate

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/go-enry/go-enry/v2"
	"gopkg.in/yaml.v3"

	"github.com/getcanyon-ai/canyon/gate/finding"
	"github.com/getcanyon-ai/canyon/gate/opengrep"
	"github.com/getcanyon-ai/canyon/gate/policy"
)

// Coverage is the scanners' run's notes of what nothing read (contracts/finding.md, What nothing read is
// named), after the not-checked-on-this-host one: per enabled rule a tool fills, each application
// language of the package no rule of a running tool mapped into that rule reads; per requirements*.txt,
// its requirements trivy cannot read, pinning no version with ==; and, while auth.unauthenticated-route
// is the AI review's and on, that it read nothing on a host with no model.
// Warnings, never blocking.
func Coverage(tree Tree, snap policy.Snapshot, model bool) ([]finding.Notification, error) {
	files, err := appFiles(tree)
	if err != nil {
		return nil, err
	}
	var notes []finding.Notification
	// Each line names its rule, and carries the rule, its policy's name and the reason apart (#1668).
	note := func(level, lead string, rule policy.Rule, format string, args ...any) {
		why := fmt.Sprintf(format, args...)
		notes = append(notes, finding.Notification{Level: level, Message: finding.Message{Text: lead + rule.ID + ": " + why},
			AssociatedRule: &finding.RuleRef{ID: rule.ID}, Properties: &finding.NoteProperties{Canyon: finding.Covered{PolicyName: rule.Name, Why: why}}})
	}
	on := func(id string) (policy.Rule, bool) {
		i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == id })
		if i < 0 {
			return policy.Rule{}, false
		}
		return snap.Rules[i], snap.Rules[i].On()
	}
	split := map[string][2][]string{} // each language's files the AI review is shown and the rest, read once for every twin
	for _, rule := range snap.Rules {
		read, filled, err := languagesRead(snap, rule.ID, "")
		if err != nil {
			return nil, err
		}
		if !rule.On() || !filled {
			continue
		}
		_, aiOn := on(twins[rule.ID])
		for _, lang := range slices.Sorted(maps.Keys(files)) {
			switch {
			case read[lang]:
			case aiOn && model: // its AI twin reads those it is shown (contracts/finding.md; #1667)
				s, ok := split[lang]
				if !ok {
					if s[0], s[1], _, err = reviewerShown(tree, files[lang]); err != nil {
						return nil, err
					}
					split[lang] = s
				}
				ai, rest := s[0], s[1]
				if len(ai) > 0 {
					note("note", "checked by the AI review: ", rule, "%s (%s) — no scanner rule reads %s", lang, count(len(ai)), lang)
				}
				if len(rest) > 0 {
					note("warning", "not checked: ", rule, "%s (%s) — no rule Canyon runs reads %s", lang, count(len(rest)), lang)
				}
			default:
				note("warning", "not checked: ", rule, "%s (%s) — no rule Canyon runs reads %s", lang, count(len(files[lang])), lang)
			}
		}
	}
	if trivy, ok := on("sarif.trivy"); ok && trivy.Run && (len(trivy.Settings.Scanners) == 0 || slices.Contains(trivy.Settings.Scanners, "vuln")) {
		if deps, ok := on("deps.known-vuln"); ok {
			for _, f := range tree.Files() {
				if vendored(f) {
					continue
				}
				if path.Base(f) == "npm-shrinkwrap.json" { // trivy 0.74's filesystem scan reads no shrinkwrap (#1399)
					note("warning", "not checked: ", deps, "%s — trivy does not read npm-shrinkwrap.json", f)
					continue
				}
				if !requirementsFile(f) {
					continue
				}
				b, err := tree.Read(f)
				if err != nil {
					return nil, err
				}
				if n := unpinned(b); n > 0 {
					note("warning", "not checked: ", deps, "%d unpinned %s in %s — trivy reads == pins only", n, map[bool]string{true: "requirement", false: "requirements"}[n == 1], f)
				}
			}
		}
	}
	auth, ok := on("auth.unauthenticated-route")
	ok = ok && auth.Producer == policy.LLMReview // the AI review's rule
	if ok && !model && len(files) > 0 {
		note("warning", "not checked: ", auth, "no model is configured on this Canyon host, so the AI review read nothing")
	}
	return notes, nil
}

// vendored is whether f is under .git/, node_modules/ or vendor/, not the app's own: never Linguist's
// vendor list, which takes any external/ or cache/, where an app's own routes live.
func vendored(f string) bool {
	return slices.ContainsFunc(strings.Split(path.Dir(f), "/"), dependencyDir)
}

// dependencyDir is whether a directory's name is .git, node_modules or vendor.
func dependencyDir(d string) bool { return d == ".git" || d == "node_modules" || d == "vendor" }

// twins are the scanner rules the AI review checks where no scanner rule reads the language, each with
// its AI twin (contracts/finding.md, The AI review where no scanner reads the language; #1667).
var twins = map[string]string{"sql.string-concat": "ai.sql-string-concat", "code.injection": "ai.code-injection"}

// scannerOf is the scanner rule whose AI twin ai is.
func scannerOf(ai string) (string, bool) {
	for scanner, twin := range twins {
		if twin == ai {
			return scanner, true
		}
	}
	return "", false
}

// appFiles is each application language's files of the tree, outside vendored directories.
func appFiles(tree Tree) (map[string][]string, error) {
	files := map[string][]string{}
	for _, f := range tree.Files() {
		if vendored(f) {
			continue
		}
		lang, err := languageOf(tree, f)
		if err != nil {
			return nil, err
		}
		if applications[lang] {
			files[lang] = append(files[lang], f)
		}
	}
	return files, nil
}

// unread is the files no running scanner rule mapped into rule reads that the AI review is shown, in path
// order: the AI twin's scope, the files Coverage names checked by it for rule — and those it would be shown
// that are not valid UTF-8, which no call is sent; none when rule is off or no running tool fills it.
func unread(tree Tree, snap policy.Snapshot, rule string) (scope, notUTF8 []string, err error) {
	if !slices.ContainsFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == rule && r.On() }) {
		return nil, nil, nil
	}
	files, err := appFiles(tree)
	if err != nil {
		return nil, nil, err
	}
	read, filled, err := languagesRead(snap, rule, "")
	if err != nil || !filled {
		return nil, nil, err
	}
	out, err := unnamed(tree) // no rule is known to read them, so the twin takes them
	if err != nil {
		return nil, nil, err
	}
	for lang, fs := range files {
		if !read[lang] {
			out = append(out, fs...)
		}
	}
	scope, _, notUTF8, err = reviewerShown(tree, out)
	slices.Sort(scope)
	slices.Sort(notUTF8)
	return scope, notUTF8, err
}

// unnamed is each file outside vendored directories with an application language's extension that go-enry
// names no language for, taking its bytes for binary (one NUL byte in a comment): no scanner rule is known
// to read it.
func unnamed(tree Tree) ([]string, error) {
	var out []string
	for _, f := range tree.Files() {
		if vendored(f) || !appExtension(f) {
			continue
		}
		lang, err := languageOf(tree, f)
		if err != nil {
			return nil, err
		}
		if lang == "" {
			out = append(out, f)
		}
	}
	return out, nil
}

// reviewerShown splits fs into the files the AI review is shown, as excerpts picks them, and the rest, and
// names among the rest those it would be shown but for not being valid UTF-8.
func reviewerShown(tree Tree, fs []string) (in, rest, notUTF8 []string, err error) {
	for _, f := range fs {
		ok, err := shown(tree, f)
		if err != nil {
			return nil, nil, nil, err
		}
		var b []byte
		if ok {
			if b, err = tree.Read(f); err != nil {
				return nil, nil, nil, err
			}
		}
		switch {
		case ok && utf8.Valid(b):
			in = append(in, f)
		case ok:
			rest, notUTF8 = append(rest, f), append(notUTF8, f)
		default:
			rest = append(rest, f)
		}
	}
	return in, rest, notUTF8, nil
}

// applications are the languages Canyon names what it did not read in: the ones apps are written in,
// Linguist's names — never a build or shell script, which no rule here is for.
var applications = map[string]bool{"JavaScript": true, "TypeScript": true, "TSX": true, "Python": true, "Go": true, "Java": true, "Kotlin": true,
	"Ruby": true, "PHP": true, "C#": true, "Rust": true, "Scala": true, "Swift": true, "C": true, "C++": true}

// languageOf is go-enry's language of f: by its name or extension when that settles it, else by its
// first 16 KiB as well — opened only when it has no extension or one an application language has, so
// a .txt is never opened.
func languageOf(tree Tree, f string) (string, error) {
	if lang, safe := enry.GetLanguageByFilename(f); safe {
		return lang, nil
	}
	if lang, safe := enry.GetLanguageByExtension(f); safe {
		return lang, nil
	}
	if path.Ext(f) != "" && !appExtension(f) {
		return "", nil
	}
	head, err := tree.head(f)
	if err != nil {
		return "", err
	}
	return enry.GetLanguage(path.Base(f), head), nil
}

// appExtension is whether f's extension is one an application language has.
func appExtension(f string) bool {
	return slices.ContainsFunc(enry.GetLanguagesByExtension(f, nil, nil), func(l string) bool { return applications[l] })
}

// head is f's first 16 KiB, or all of it when it is shorter.
func (t Tree) head(f string) ([]byte, error) {
	fh, err := os.Open(filepath.Join(t.root, filepath.FromSlash(f)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	head := make([]byte, 16<<10)
	n, err := io.ReadFull(fh, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head[:n], nil
}

// languagesRead is the languages a running tool the policy table maps into rule reads for it, and whether
// one does: Opengrep's, the languages of each rule it is sent, the vendored ones and Canyon's, whose CWE
// its row selects; ESLint's, JavaScript and TypeScript. A row of any other tool reads a rule whatever the
// language, so names none.
func languagesRead(snap policy.Snapshot, rule, tool string) (map[string]bool, bool, error) {
	read, filled := map[string]bool{}, false
	for _, m := range policy.Table {
		i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif."+m.Tool })
		if m.Policy != rule || i < 0 || !snap.Rules[i].On() || !snap.Rules[i].Run || m.Tool != "opengrep" && m.Tool != "eslint" || tool != "" && m.Tool != tool {
			continue
		}
		filled = true
		if m.Tool == "eslint" {
			read["JavaScript"], read["TypeScript"], read["TSX"] = true, true, true // its image's files: js, cjs, mjs, jsx, ts, tsx, mts, cts
			continue
		}
		langs, err := bundleLanguages()
		if err != nil {
			return nil, false, err
		}
		for cwe, ls := range langs {
			if m.Selects([]string{cwe}) {
				for _, l := range ls {
					read[l] = true
				}
			}
		}
	}
	return read, filled, nil
}

// opengrepLanguage is each Opengrep language id its rules name, by Linguist's names for what it
// reads: a javascript rule reads TypeScript too (gate/opengrep/testdata/probes/tsx).
var opengrepLanguage = map[string][]string{"python": {"Python"}, "go": {"Go"}, "java": {"Java"}, "kotlin": {"Kotlin"}, "scala": {"Scala"},
	"csharp": {"C#"}, "javascript": {"JavaScript", "TypeScript", "TSX"}, "typescript": {"TypeScript", "TSX"}}

// bundleLanguages is Opengrep's rules' languages, Canyon's included, by each rule's CWE, Linguist's names.
var bundleLanguages = sync.OnceValues(func() (map[string][]string, error) {
	rules, err := opengrep.Rules()
	if err != nil {
		return nil, err
	}
	langs := map[string][]string{}
	for name, b := range rules {
		var doc struct {
			Rules []struct {
				Languages []string
				Metadata  struct{ CWE string }
			}
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("gate: %s: %w", name, err)
		}
		for _, r := range doc.Rules {
			for _, l := range r.Languages {
				names, ok := opengrepLanguage[l]
				if !ok {
					return nil, fmt.Errorf("gate: %s names Opengrep language %q, which Canyon cannot name", name, l)
				}
				langs[r.Metadata.CWE] = append(langs[r.Metadata.CWE], names...)
			}
		}
	}
	return langs, nil
})

// requirementsFile is a pip requirements file: requirements.txt, or requirements-<anything>.txt.
func requirementsFile(f string) bool {
	base := path.Base(f)
	return strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt")
}

// unpinned is the requirement lines of a pip requirements file that pin no version with ==: neither a
// comment, an option (-r, -e, --index-url…) nor blank.
func unpinned(b []byte) int {
	n := 0
	for s := bufio.NewScanner(bytes.NewReader(b)); s.Scan(); {
		line := strings.TrimSpace(strings.SplitN(s.Text(), "#", 2)[0])
		if line != "" && !strings.HasPrefix(line, "-") && !strings.Contains(line, "==") {
			n++
		}
	}
	return n
}

// count is n files, one file as 1 file.
func count(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

// ReadNothing is each tool Canyon runs that had no file of the package to read, and why: ESLint with no
// JavaScript, TypeScript or TSX, Opengrep with none of the languages of the rules it is sent, by the
// languages What nothing read is named reads (contracts/finding.md; #1742).
func ReadNothing(tree Tree, snap policy.Snapshot) (map[string]string, error) {
	files, err := appFiles(tree)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for tool, why := range map[string]string{"eslint": "no JavaScript or TypeScript here", "opengrep": "no language its rules read is here"} {
		if i := slices.IndexFunc(snap.Rules, func(r policy.Rule) bool { return r.ID == "sarif."+tool }); i >= 0 && snap.Rules[i].Settings.Rules != nil {
			continue // an org's rules repo: Canyon does not read its rules' languages
		}
		reads, sent := map[string]bool{}, false
		for _, rule := range snap.Rules {
			read, filled, err := languagesRead(snap, rule.ID, tool)
			if err != nil {
				return nil, err
			}
			if rule.On() && filled {
				sent = true
				maps.Copy(reads, read)
			}
		}
		if sent && !slices.ContainsFunc(slices.Collect(maps.Keys(files)), func(lang string) bool { return reads[lang] }) {
			out[tool] = why
		}
	}
	return out, nil
}
