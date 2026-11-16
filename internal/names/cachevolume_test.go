package names

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// pack rewrites each Windows-reserved name inside a volume name it is passed, so the name Canyon passes is already
// written that way: what the executor creates is what it and canyon demo down remove (contracts/executor.md,
// /forget). Writing it twice changes nothing.
func TestACacheVolumeIsNamedAsPackWritesIt(t *testing.T) {
	for app, want := range map[string]string{
		"claims-portal":     "canyon-cache-claims-portal-build",
		"smooth-falcon-lei": "canyon-cache-smooth-falc_o_n-lei-build",
		"aux-com-con":       "canyon-cache-a_u_x-c_o_m-c_o_n-build",
		"lpt-nul-prn":       "canyon-cache-l_p_t-n_u_l-p_r_n-build",
		"telecom-annul":     "canyon-cache-telec_o_m-ann_u_l-build",
		"conul-nulpt-prnul": "canyon-cache-c_o_n_u_l-n_u_l_p_t-p_r_n_u_l-build", // a replacement completes the next key
	} {
		got := CacheVolume(app, "build")
		if got != want {
			t.Errorf("%s: %q, want %q", app, got, want)
		}
		if again := CacheVolume(strings.TrimSuffix(strings.TrimPrefix(got, "canyon-cache-"), "-build"), "build"); again != got {
			t.Errorf("%s written twice: %q, want %q", app, again, got)
		}
	}
	if got := CacheVolume("smooth-falcon-lei", "launch"); got != "canyon-cache-smooth-falc_o_n-lei-launch" {
		t.Errorf("launch: %q", got)
	}
}

// The table is pack v0.40.3's FilterReservedNames (internal/paths/paths.go): a pack the Makefile pins at another
// version may write volume names otherwise.
func TestPackIsTheVersionItsReservedNamesWereReadFrom(t *testing.T) {
	f, err := os.Open("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for s := bufio.NewScanner(f); s.Scan(); {
		if v, ok := strings.CutPrefix(s.Text(), "PACK_VERSION := "); ok {
			if v != "0.40.3" {
				t.Fatalf("the Makefile pins pack %s: re-read FilterReservedNames in its internal/paths/paths.go, make CacheVolume write volume names as it does, then set this test's version to %s", v, v)
			}
			return
		}
	}
	t.Fatal("no PACK_VERSION := line in the Makefile")
}
