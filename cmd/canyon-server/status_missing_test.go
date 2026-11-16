package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// The status read names, on production, each runtime value a person set for preview that production has none of
// (contracts/routes.md, the status row; #2786): the cause an agent couldn't name when production answered 500 in the
// rollback journey (#2160). A build value is in the image already and is never named; a value set for production
// takes its name off; preview carries none.
func TestTheStatusReadNamesAValueProductionIsMissing(t *testing.T) {
	_, _, base, c, deploy := previewed(t)
	for _, put := range []struct{ name, body string }{
		{"ORDERS_TOKEN", `{"value":"orders-preview"}`},
		{"NPM_TOKEN", `{"value":"npm-preview","where":"build"}`},
		{"API_KEY", `{"value":"api-preview"}`},
	} {
		if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/"+put.name, put.body); code != 204 {
			t.Fatalf("set %s: %d %s", put.name, code, lastBody)
		}
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/API_KEY", `{"value":"api-prod","env":"prod"}`); code != 204 {
		t.Fatalf("set API_KEY in prod: %d %s", code, lastBody)
	}
	missing := func() (preview, prod any) {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		for _, value := range []string{"orders-preview", "npm-preview", "api-preview", "api-prod", "orders-prod"} { // names, never values
			if strings.Contains(lastBody, value) {
				t.Fatalf("the status read holds a secret's value, %s: %s", value, lastBody)
			}
		}
		envs := status["envs"].(map[string]any)
		if p, ok := envs["prod"].(map[string]any); ok {
			prod = p["missing"]
		}
		return envs["preview"].(map[string]any)["missing"], prod
	}
	if preview, prod := missing(); preview != nil || prod != nil {
		t.Fatalf("before production runs anything: preview %v, prod %v", preview, prod)
	}
	if promoted(t, c, base, deploy)["state"] != "clean" {
		t.Fatalf("the promotion: %s", lastBody)
	}
	if preview, prod := missing(); preview != nil || fmt.Sprint(prod) != "[ORDERS_TOKEN]" {
		t.Errorf("production's missing: %v, preview's %v; want [ORDERS_TOKEN] and none", prod, preview)
	}
	if code, _, _ := do(t, c, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/ORDERS_TOKEN", `{"value":"orders-prod","env":"prod"}`); code != 204 {
		t.Fatalf("set ORDERS_TOKEN in prod: %d %s", code, lastBody)
	}
	if _, prod := missing(); fmt.Sprint(prod) != "[]" {
		t.Errorf("once production has every value: %v, want []", prod)
	}
}

// The read names production's missing values to its team, and to a responder with no grant, who reads production as a
// viewer reads it, never (design/PERMISSIONS.md: one allowed and one refused caller at the boundary; #2786).
func TestAResponderReadsNoneOfProductionsMissingValues(t *testing.T) {
	pool, ctx, base, olive := responderHost(t)
	if code, _, _ := do(t, olive, http.MethodPut, base+"/api/v1/projects/claims-portal/secrets/ORDERS_TOKEN", `{"value":"orders-preview"}`); code != 204 {
		t.Fatalf("set ORDERS_TOKEN: %d %s", code, lastBody)
	}
	prodOf := func(c *http.Client) map[string]any {
		t.Helper()
		_, status, _ := do(t, c, http.MethodGet, base+"/api/v1/projects/claims-portal/status", "")
		envs, _ := status["envs"].(map[string]any)
		prod, _ := envs["prod"].(map[string]any)
		if prod == nil {
			t.Fatalf("no production on the status read: %s", lastBody)
		}
		return prod
	}
	if prod := prodOf(olive); fmt.Sprint(prod["missing"]) != "[ORDERS_TOKEN]" {
		t.Fatalf("the owner's read: missing %v, want [ORDERS_TOKEN]", prod["missing"])
	}
	rhea := signedIn(t, ctx, pool, base, "sub-rhea", responderEmail, false)
	if prod := prodOf(rhea); prod["missing"] != nil || strings.Contains(lastBody, "ORDERS_TOKEN") {
		t.Errorf("a responder with no grant reads production's missing: %s", lastBody)
	}
}
