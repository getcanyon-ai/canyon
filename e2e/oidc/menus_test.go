//go:build oidc

package oidc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMenusAndDialogsOpenOnScreen is e2e/ui's menus project on the team setup: every menu and dialog of the dashboard
// this server serves, opened in five windows, inside each of them or scrolling to its end inside itself (#126). Its reads
// are the contracts' goldens (e2e/ui/menus/host.ts), so nobody signs in and nothing is written.
func TestMenusAndDialogsOpenOnScreen(t *testing.T) {
	cmd := exec.Command("npx", "playwright", "test", "--project=menus")
	cmd.Dir = filepath.Join("..", "ui")
	cmd.Env = append(os.Environ(), "E2E_CANYON_URL="+host)
	out, err := cmd.CombinedOutput()
	t.Logf("playwright, menus:\n%s", out)
	if err != nil {
		t.Fatalf("menus: %v", err)
	}
}
