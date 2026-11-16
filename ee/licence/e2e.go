//go:build e2e

package licence

import (
	"os"
	"strings"
)

// The E2E stack's server (e2e/compose.e2e.yml builds it with GOTAGS=e2e) trusts one more licence key: the public half
// of the key e2e/fixtures/gen.sh makes for each run and mounts here, so the journeys run licensed. A release is never
// built with the tag, so no release reads this file.
func init() {
	if raw, err := os.ReadFile("/etc/canyon/licence/key.pub"); err == nil {
		Keys = append(Keys, strings.TrimSpace(string(raw)))
	}
}
