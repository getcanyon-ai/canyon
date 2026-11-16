package finding

import (
	"cmp"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
)

// liveKeys is gitleaks 8.30.1's default ruleset, through its own viper, a gitleaks:allow comment
// ignored: a live key is a secret it finds (contracts/finding.md, Profile rules).
var liveKeys = sync.OnceValues(func() (*detect.Detector, error) {
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

// keysIn is every live key in s, longest first, or the error the detector would not build with. No
// decoding: a key is masked where it stands, and a decoded one is nowhere in s to mask.
func keysIn(s string) ([]string, error) {
	d, err := liveKeys()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, f := range d.DetectString(s) {
		keys = append(keys, f.Secret)
	}
	slices.SortStableFunc(keys, func(x, y string) int { return cmp.Or(len(y)-len(x), strings.Compare(x, y)) })
	return slices.Compact(keys), nil
}

// HasLiveKey reports whether b carries a live key: the backstop anything leaving the gate runs. A
// detector that would not build reads as a key found: nothing passes on an error.
func HasLiveKey(b []byte) bool {
	keys, err := keysIn(string(b))
	return err != nil || len(keys) > 0
}
