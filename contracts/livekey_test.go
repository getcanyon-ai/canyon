package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// gitleaksDefault is gitleaks 8.30.1's default ruleset, through its own viper: finding.md's live key.
var gitleaksDefault = sync.OnceValues(func() (*detect.Detector, error) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(config.DefaultConfig)); err != nil {
		return nil, err
	}
	var vc config.ViperConfig
	if err := v.Unmarshal(&vc); err != nil {
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

// No golden carries a live key anywhere: nothing gitleaks 8.30.1's default ruleset finds (finding.md,
// Profile rules and the golden fixture) — the finding logs, and every other contract golden, several
// of which carry finding logs or text that reaches a model.
func TestNoGoldenCarriesALiveKey(t *testing.T) {
	d, err := gitleaksDefault()
	if err != nil {
		t.Fatal(err)
	}
	var goldens []string
	err = filepath.WalkDir("testdata", func(path string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() && strings.HasSuffix(path, ".json") {
			goldens = append(goldens, path)
		}
		return err
	})
	if err != nil || len(goldens) == 0 {
		t.Fatalf("no goldens: %v", err)
	}
	for _, path := range goldens {
		raw, err := os.ReadFile(path) //nolint:gosec // a golden the glob above found
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range d.DetectString(string(raw)) {
			t.Errorf("%s:%d carries a live key (gitleaks %s)", path, f.StartLine+1, f.RuleID)
		}
	}
}
