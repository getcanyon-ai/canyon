package cli

import (
	"os"
	"testing"
)

// The hook names this binary by path (Self); a test binary's own path would differ on every
// machine, so the goldens read the path a release installed by §8.1 runs from.
func TestMain(m *testing.M) {
	Self = "/usr/local/bin/canyon"
	goos = "linux" // the feedback links name the OS: one for every machine the goldens run on
	os.Exit(m.Run())
}
