//go:build stack

package e2e

import (
	"maps"
	"slices"
	"testing"
)

// j10Audit reads the core app's trail after J6 deleted it: still reachable, its milestones in the
// order J4 to J6 made them, Canyon's own steps under Canyon, and a secret's rows naming it, never
// holding its value.
func j10Audit(t *testing.T) {
	var page struct {
		Events []struct {
			Actor  struct{ Kind string }
			Action string
			Detail map[string]any
		}
		Next *string
	}
	read(t, "/api/v1/audit?project="+coreApp+"&limit=200", &page)
	if page.Next != nil {
		t.Fatal("the core app's trail runs past one page of 200; want it on one")
	}
	var actions []string
	for _, e := range slices.Backward(page.Events) { // newest first on the wire
		actions = append(actions, e.Action)
		switch e.Action {
		case "verdict", "ensure resource":
			if e.Actor.Kind != "canyon" {
				t.Errorf("%s by %s; want Canyon's", e.Action, e.Actor.Kind)
			}
		case "PUT /api/v1/projects/{id}/secrets/{name}":
			if keys := slices.Sorted(maps.Keys(e.Detail)); !slices.Equal(keys, []string{"env", "name", "where"}) {
				t.Errorf("a secret's row holds %v; want env, name and where only", keys)
			}
		}
	}
	milestones := []string{"verdict", "POST /api/v1/gate-runs/{run}/findings/{fingerprint}/approve", "declare resource",
		"ensure resource", "deploy preview", "deploy prod", "DELETE /api/v1/projects/{id}/envs/{env}", "delete resource",
		"DELETE /api/v1/projects/{id}"}
	at := 0
	for _, a := range actions {
		if at < len(milestones) && a == milestones[at] {
			at++
		}
	}
	if at < len(milestones) {
		t.Errorf("the trail %q misses %q in order", actions, milestones[at])
	}
	if actions[len(actions)-1] != "DELETE /api/v1/projects/{id}" {
		t.Errorf("the trail ends %q; want the delete last", actions[len(actions)-1])
	}
}
