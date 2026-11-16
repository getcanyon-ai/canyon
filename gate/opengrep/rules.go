// Package opengrep is the rules Canyon sends every opengrep scan: GitLab's sast-rules at the pinned
// commit, its permissive folders only (make vendor-sast-rules), and Canyon's own rule, canyon.yaml
// (contracts/finding.md, Canyon-run tools).
package opengrep

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"path"

	_ "embed"
)

// Bundle is sast-rules.tar as make vendor-sast-rules writes it: the rule files at their paths in
// GitLab's repository, and their licences.
//
//go:embed sast-rules.tar
var Bundle []byte

//go:embed canyon.yaml
var canyon []byte

// Rules is the bundle's rule files, path to content, and Canyon's at canyon.yaml: the rules part of
// every opengrep scan.
func Rules() (map[string][]byte, error) {
	rules, tr := map[string][]byte{"canyon.yaml": canyon}, tar.NewReader(bytes.NewReader(Bundle))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return rules, nil
		}
		if err != nil {
			return nil, err
		}
		if ext := path.Ext(h.Name); h.Typeflag == tar.TypeReg && (ext == ".yml" || ext == ".yaml") {
			if rules[h.Name], err = io.ReadAll(tr); err != nil {
				return nil, err
			}
		}
	}
}
