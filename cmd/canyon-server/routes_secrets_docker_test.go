package main

import (
	"net/http"
	"testing"
)

// reservedSaid is the refusal of a secret name the Docker executor refuses (contracts/routes.md, the secrets PUT
// row; #2490), byte for byte.
func reservedSaid(name, next string) string {
	return `{"error":"invalid_name","message":"` + name + ` is reserved here: this Canyon runs apps with Docker, and docker or pack reads it, so the app's next ` + next + ` would fail. Choose another name"}`
}

// On a Docker host a secret's name is refused where the executor would refuse it at the app's next deploy, and
// said why, before anything is written (#2490): any DOCKER_ name in any case, the names docker and pack take from
// the executor's environment as written, and on a build value the proxies, certificates and the rest pack reads, in
// any case, and any PACK_ name. A contributor, who may set secrets, is told; a viewer is refused the route before
// the name is read. On Kubernetes none of these is refused.
func TestSecretNamesTheDockerExecutorRefusesAreRefusedOnADockerHost(t *testing.T) {
	pool, ctx, base, _, carl, vic, _, _ := teamOf(t)
	put := func(c *http.Client, name, body string) int {
		code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/"+name, body)
		return code
	}
	for _, tc := range []struct {
		name, body string
		refused    string // "" accepted, else the app's next call the refusal names
	}{
		{"DOCKER_USERNAME", `{"value":"v"}`, "deploy"},
		{"docker_config", `{"value":"v"}`, "deploy"},
		{"HOME", `{"value":"v"}`, "deploy"},
		{"XDG_CACHE_HOME", `{"value":"v"}`, "deploy"},
		{"home", `{"value":"v"}`, ""},        // the executor's environment's names are refused as written
		{"HTTPS_PROXY", `{"value":"v"}`, ""}, // a runtime value: the app's own proxy, which pack never reads
		{"PACK_ANYTHING", `{"value":"v"}`, ""},
		{"HTTPS_PROXY", `{"value":"v","where":"build"}`, "build"},
		{"https_proxy", `{"value":"v","where":"build"}`, "build"},
		{"GODEBUG", `{"value":"v","where":"build"}`, "build"},
		{"pack_anything", `{"value":"v","where":"build"}`, "build"},
		{"DOCKER_HOST", `{"value":"v","where":"build"}`, "build"},
		{"NPM_TOKEN", `{"value":"v","where":"build"}`, ""},
	} {
		code := put(carl, tc.name, tc.body)
		switch {
		case tc.refused == "" && code != 204:
			t.Errorf("%s %s: %d %s, want 204", tc.name, tc.body, code, lastBody)
		case tc.refused != "" && (code != 400 || lastBody != reservedSaid(tc.name, tc.refused)):
			t.Errorf("%s %s: %d %s, want 400 %s", tc.name, tc.body, code, lastBody, reservedSaid(tc.name, tc.refused))
		}
	}
	if code := put(vic, "DOCKER_USERNAME", `{"value":"v"}`); code != 403 || lastBody != viewerTeamRequiredBody {
		t.Errorf("a viewer: %d %s, want the route's team_required before the name is read", code, lastBody)
	}
	if n := count(t, ctx, pool, "SELECT count(*) FROM secrets WHERE name IN ('DOCKER_USERNAME', 'docker_config', 'HOME', 'XDG_CACHE_HOME', 'GODEBUG', 'https_proxy', 'pack_anything', 'DOCKER_HOST') OR name = 'HTTPS_PROXY' AND scope = 'build'"); n != 0 {
		t.Errorf("a refused name stored %d row(s)", n)
	}
}

// On Kubernetes the executor refuses none of these, and neither does the PUT.
func TestSecretNamesTheDockerExecutorRefusesAreSetOnKubernetes(t *testing.T) {
	onKubernetes(t)
	_, _, base, _, carl, _, _, _ := teamOf(t)
	for _, tc := range []struct{ name, body string }{{"DOCKER_USERNAME", `{"value":"v"}`}, {"HTTPS_PROXY", `{"value":"v","where":"build"}`}} {
		if code, _, _ := do(t, carl, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/"+tc.name, tc.body); code != 204 {
			t.Errorf("%s %s on Kubernetes: %d %s", tc.name, tc.body, code, lastBody)
		}
	}
}
