//go:build oidc

package oidc

import (
	"net/http"
	"testing"
	"time"
)

// Settings › People & sign-in › Connect an agent as a member and as an admin (design/specs/mcp.md §6 item 14, §12):
// both get Canyon's address for agents and each agent's way to it; only the admin gets the org-managed snippets.
func TestConnectAnAgentAsAMemberAndAnAdmin(t *testing.T) {
	var config struct{ Auth string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/auth/config", &config); code != http.StatusOK || config.Auth != "oidc" {
		t.Fatalf("no Canyon with sign-in on answers %s (%d, auth %q): start one with make e2e-up OIDC=1", host, code, config.Auth)
	}
	onboard(t, signIn(t, admin))
	var metadata struct{ Resource string }
	if code := get(t, &http.Client{Timeout: 10 * time.Second}, host+"/.well-known/oauth-protected-resource/mcp", &metadata); code != http.StatusOK || metadata.Resource == "" {
		t.Fatalf("the protected-resource metadata: %d, resource %q", code, metadata.Resource)
	}
	for who, email := range map[string]string{"member": teammate, "admin": admin} {
		walk(t, signIn(t, email), "connect", who, "CONNECT_ADDRESS="+metadata.Resource)
	}
}
