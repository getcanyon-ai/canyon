package provisioners

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestTheFileChoosesADriverPerTypeAndEnvAndUnsetIsBuiltinPostgres(t *testing.T) {
	builtin := &Builtin{}
	unset, err := Load("", builtin)
	if err != nil {
		t.Fatal(err)
	}
	set, err := Load(writeFile(t, "p.json", `{"postgres": {"preview": {"builtin": true},
		"prod": {"url": "https://provision.internal/canyon", "token_file": "`+tokenFile(t)+`"}},
		"s3": {"preview": {"url": "https://storage.internal/canyon", "token_file": "`+tokenFile(t)+`"}}}`), builtin)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ds       Drivers
		typ, env string
		want     string // "" is no driver
	}{
		{unset, "postgres", "preview", "builtin"},
		{unset, "postgres", "prod", "builtin"},
		{unset, "s3", "prod", ""},
		{set, "postgres", "preview", "builtin"},
		{set, "postgres", "prod", "https://provision.internal/canyon"},
		{set, "s3", "preview", "https://storage.internal/canyon"},
		{set, "s3", "prod", ""},
	} {
		d, err := c.ds.Select(c.typ, c.env)
		got := ""
		if err == nil {
			got = d.Name() // the name a record keeps, and a delete finds its driver by
		}
		if named, ok := c.ds.Named(got); got != c.want || (err == nil) != (c.want != "") || err == nil && (!ok || named != d) {
			t.Errorf("%s in %s: %q %v, want %q", c.typ, c.env, got, err, c.want)
		}
	}
	if _, ok := set.Named("https://gone.internal/canyon"); ok {
		t.Error("a driver the file no longer names was found")
	}
}

func TestTheFileIsRefusedWhole(t *testing.T) {
	adapter := `"url": "https://provision.internal", "token_file": "` + tokenFile(t) + `"`
	for _, body := range []string{
		`{"postgres": {"prod": {"builtin": true, "driver": "x"}}}`, // an unknown field
		`{"redis": {"prod": {"builtin": true}}}`,                   // an unknown type
		`{"postgres": {"production": {"builtin": true}}}`,          // an unknown env
		`{"s3": {"preview": {"builtin": true}}}`,                   // the built-in driver serves postgres only
		`{"postgres": {"prod": {"url": "http://provision.internal", "token_file": "` + tokenFile(t) + `"}}}`,
		`{"postgres": {"prod": {"url": "https://provision.internal", "token_file": "/nonexistent/token"}}}`,
		`{"postgres": {"prod": {"builtin": true, ` + adapter + `}}}`, // both
		`{"postgres": {"prod": {}}}`,                                 // neither
		`{"postgres": {"prod": {"builtin": true}}} }`,                // trailing data
		`["postgres"]`,
	} {
		if _, err := Load(writeFile(t, "p.json", body), &Builtin{}); err == nil || strings.Contains(err.Error(), token) || strings.HasSuffix(err.Error(), "<nil>") {
			t.Errorf("%s: %v", body, err)
		}
	}
	if _, err := Load(writeFile(t, "p.json", `{"postgres": {"prod": {"builtin": true}}}`), nil); err == nil || !strings.Contains(err.Error(), "CANYON_APP_DATABASE_URL") {
		t.Errorf("builtin named on a host without the built-in driver: %v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json"), &Builtin{}); err == nil {
		t.Error("a missing config file was accepted")
	}
}
