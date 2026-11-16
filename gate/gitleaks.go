package gate

import (
	"bytes"
	"cmp"
	"context"
	"io"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
	"github.com/zricethezav/gitleaks/v8/sources"
)

// secretsRules is gitleaks' own default ruleset, and nothing of Canyon's: the tool decides what a secret
// is. It reads through its own viper, never the global the reviewer's masking detector uses.
var secretsRules = sync.OnceValues(func() (config.Config, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(config.DefaultConfig)); err != nil {
		return config.Config{}, err
	}
	var vc config.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
		return config.Config{}, err
	}
	return vc.Translate()
})

// Gitleaks is gitleaks' SARIF over the package tree's files, read as gitleaks reads a file — in chunks,
// binaries and archives left unread, its default allowlist (lockfiles, node_modules) applied — ready to
// attach as the tool gitleaks (contracts/snapshot.md, Tool results mapped to a policy). Before the
// report is written, each finding is made safe to keep: its secret, match and line are cleared, so the
// raw secret is in no byte of it, and its region is widened to its whole lines, 1-based, column 1 to
// the last line's end in SARIF's default UTF-16 units, so gitleaks' byte columns never reach the
// attach; a finding on a file's name alone is the whole file, 1:1. A gitleaks:allow comment is
// ignored. Findings are written in (file, line, rule) order, so the same tree makes the same bytes.
func Gitleaks(ctx context.Context, tree Tree) ([]byte, error) {
	cfg, err := secretsRules()
	if err != nil {
		return nil, err
	}
	d := detect.NewDetectorContext(ctx, cfg)
	d.IgnoreGitleaksAllow, d.MaxDecodeDepth = true, 5 // the CLI's default decoding depth; archives stay unread (0)
	found, err := d.DetectSource(ctx, packageFiles{tree})
	if err == nil {
		err = ctx.Err() // a cancel inside a file ends gitleaks' read of it quietly, with what it found so far
	}
	if err != nil {
		return nil, err
	}
	lines := map[string][]string{} // only the files a finding names
	for i := range found {
		f := &found[i]
		f.EndLine = max(f.EndLine, f.StartLine+strings.Count(f.Match, "\n")) // gitleaks ends a match on a last line without a newline at its start
		f.Secret, f.Match, f.Line = "", "", ""
		if f.StartLine < 1 { // a rule on the file's name alone (pkcs12-file): the whole file, as SARIF's 1:1 names it
			f.StartLine, f.StartColumn, f.EndLine, f.EndColumn = 1, 1, 1, 1
			continue
		}
		if lines[f.File] == nil {
			text, err := tree.Read(f.File)
			if err != nil {
				return nil, err
			}
			lines[f.File] = strings.Split(string(text), "\n")
		}
		f.EndLine = min(f.EndLine, len(lines[f.File]))
		last := strings.TrimSuffix(lines[f.File][f.EndLine-1], "\r")
		f.StartColumn, f.EndColumn = 1, len(utf16.Encode([]rune(last)))+1
	}
	slices.SortStableFunc(found, func(a, b report.Finding) int {
		return cmp.Or(strings.Compare(a.File, b.File), cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.EndLine, b.EndLine), strings.Compare(a.RuleID, b.RuleID))
	})
	var out bytes.Buffer
	err = (&report.SarifReporter{OrderedRules: cfg.GetOrderedRules()}).Write(nopCloser{&out}, found)
	return out.Bytes(), err
}

// packageFiles is the tree's files as gitleaks' own file source reads each.
type packageFiles struct{ tree Tree }

func (s packageFiles) Fragments(ctx context.Context, yield sources.FragmentsFunc) error {
	for _, name := range s.tree.Files() {
		text, err := s.tree.Read(name)
		if err != nil {
			return err
		}
		if err := (&sources.File{Content: bytes.NewReader(text), Path: name}).Fragments(ctx, yield); err != nil {
			return err
		}
	}
	return nil
}

// HasSecret reports whether gitleaks' default ruleset finds a secret in text: what a custom rule's name
// and instructions may not carry, since a snapshot is journaled and sent to a model (contracts/routes.md).
func HasSecret(text string) (bool, error) {
	cfg, err := secretsRules()
	if err != nil {
		return false, err
	}
	d := detect.NewDetector(cfg)
	d.IgnoreGitleaksAllow = true
	return len(d.DetectString(text)) > 0, nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
