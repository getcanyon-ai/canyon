package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The platform row says each patch state in words (design/PATCHING.md §8; cli.md §6.1): asked names the fix and how
// to approve it; reverted and a held patch run say its own last line; held by the hold, a pause or a preview ahead
// says which; could_not_rescan says why the image read could not check; wait and planned say Canyon's part; and the
// rest say the image was checked, only when its read checked. Below contributor the read carries no state and there
// is no row.
func TestStatusPlatformLines(t *testing.T) {
	s, _ := reads(t, page(t), "status")
	s.Envs["preview"].Label = "v1.4"
	reverted := "Canyon reverted preview at 10:41 UTC: the patched app restarted, so the image it replaced runs again"
	prodReverted := "Canyon reverted production at 11:02 UTC: the patched app exited, so the image it replaced runs again"
	ineffective := "The patched image still has every package the patch was for: Canyon did not deploy it"
	for _, c := range []struct {
		name  string
		env   string
		patch envPatch
		facts platformFacts
		want  string
	}{
		// #2631: each package once, the approval listing one fix per vulnerability
		{"asked", "prod", envPatch{State: "asked", Approval: "K7QF-2M9X"}, platformFacts{Fixes: []patchFix{{Name: "libssl3", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"},
			{Name: "libssl3", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"}}},
			"  platform     ! a fix is ready for production: libssl3 3.0.13-0ubuntu3.4 → 3.0.13-0ubuntu3.5, app unchanged\n  " + strings.Repeat(" ", statusLabel) + "   approve it in Canyon: http://localhost:7777/#/approve?code=K7QF-2M9X\n"},
		// #2601: each package the declined fix was for once, as production still runs it
		{"denied", "prod", envPatch{State: "denied", Approval: "K7QF-2M9X"}, platformFacts{Fixes: []patchFix{{Name: "libssl3", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"},
			{Name: "libssl3", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"}, {Name: "openssl", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"}}},
			"  platform     ! Your team turned down Canyon's fix for production. Production still has libssl3 3.0.13-0ubuntu3.4, openssl 3.0.13-0ubuntu3.4. Canyon will ask again when a newer fix is out.\n"},
		{"reverted", "preview", envPatch{State: "reverted", Run: "gr_patch"}, platformFacts{Line: reverted}, "  platform     ✗ " + reverted + "\n"},
		{"production reverted", "prod", envPatch{State: "reverted", Run: "gr_promote"}, platformFacts{Line: prodReverted}, "  platform     ✗ " + prodReverted + "\n"},
		{"production reverted, its run unnamed", "prod", envPatch{State: "reverted"}, platformFacts{}, "  platform     ✗ Canyon reverted its patch of production\n"},
		{"held by a patch run's end", "preview", envPatch{State: "held", Why: "patch_ineffective", Run: "gr_patch"}, platformFacts{Line: ineffective}, "  platform     ! held: " + ineffective + "\n"},
		{"held by its review", "preview", envPatch{State: "held", Why: "blocked"}, platformFacts{}, "  platform     ! held: v1.4 is blocked by its review\n"},
		{"held by the hold", "prod", envPatch{State: "held", Why: "production_held"}, platformFacts{}, "  platform     ! held: production is held\n"},
		{"held by a pause", "prod", envPatch{State: "held", Why: "paused"}, platformFacts{}, "  platform     ! held: Canyon paused this fix until an admin resumes it\n"},
		{"held while preview is ahead", "prod", envPatch{State: "held", Why: "preview_ahead"}, platformFacts{}, "  platform     ! held: promote v1.4 to take the fix to production\n"},
		{"couldn't rescan", "preview", envPatch{State: "could_not_rescan"}, platformFacts{Why: "could not check: its vulnerability database is over 5 days old"},
			"  platform     ! couldn't rescan: its vulnerability database is over 5 days old\n"},
		{"couldn't rescan the system libraries", "preview", envPatch{State: "could_not_rescan"}, platformFacts{Checked: true}, "  platform     ! couldn't rescan: Canyon couldn't check which fixes its system libraries carry\n"},
		{"wait", "preview", envPatch{State: "wait"}, platformFacts{}, "  platform     · Ubuntu has a fix; Canyon hasn't picked it up yet\n"},
		{"planned", "preview", envPatch{State: "planned"}, platformFacts{}, "  platform     · Canyon is patching it\n"},
		{"none, checked", "preview", envPatch{State: "none"}, platformFacts{Checked: true, CheckedAt: "2026-09-02T09:00:00Z"},
			"  platform     ✓ checked: the system libraries and build tools Canyon adds · vulnerability data from 3h ago\n"},
		{"patched, checked", "prod", envPatch{State: "patched"}, platformFacts{Checked: true, CheckedAt: "2026-09-02T09:00:00Z"},
			"  platform     ✓ checked: the system libraries and build tools Canyon adds · vulnerability data from 3h ago\n"},
		{"none, checked, with findings", "preview", envPatch{State: "none"}, platformFacts{Checked: true, CheckedAt: "2026-09-02T09:00:00Z", Why: "2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds", Counted: 3},
			"  platform     · checked: 2 known-vulnerable packages with a published fix, in the system libraries and build tools Canyon adds · vulnerability data from 3h ago\n"},
		{"none, never checked", "preview", envPatch{State: "none"}, platformFacts{}, ""},
	} {
		var out strings.Builder
		a := &app{out: &out}
		a.platformRow(platformLineOf(c.env, c.patch, c.facts, s, "http://localhost:7777", at, a.ago))
		if out.String() != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, out.String(), c.want)
		}
	}
	if env, p := pickPlatform(s.Envs); p != nil {
		t.Errorf("a read with no patch state has a platform row: %s %+v", env, p)
	}
}

// The row is of the most pressing environment's state, and the env rows say what runs Canyon's patch: an asked
// production comes before a patched preview, which says patched by Canyon beside its age (design/PATCHING.md §8).
// Under --format ai the agent reads the same (#2615): preview patched by Canyon at the host's timestamp, and the
// asked fix with its approval url and a line saying a person approves it in the dashboard, which a production
// not asked never carries.
func TestStatusSaysPatchedByCanyon(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	s.Envs["preview"].Patch = &envPatch{State: "patched_preview", Since: "2026-09-02T10:00:00Z"}
	s.Envs["prod"] = &env{URL: "http://claims-portal.prod.localhost:7777", Label: s.Envs["preview"].Label, DeployedAt: "2026-08-30T12:00:00Z", State: "running", RunBy: "canyon",
		Patch: &envPatch{State: "asked", Approval: "K7QF-2M9X"}}
	if env, p := pickPlatform(s.Envs); env != "prod" || p.State != "asked" {
		t.Fatalf("the row is of %s %+v, want production asked", env, p)
	}
	s.Platform = platformLineOf("prod", *s.Envs["prod"].Patch, platformFacts{Fixes: []patchFix{{Name: "libssl3", From: "3.0.13-0ubuntu3.4", To: "3.0.13-0ubuntu3.5"}}}, s,
		"http://localhost:7777", at, (&app{}).ago)
	wants(t, "status-patched.txt", rendered(t, s, latest))
	// a rebuild is built again: its asked line says so, never that the app is unchanged (#2117)
	rebuilt := s
	rebuilt.Platform = platformLineOf("prod", *s.Envs["prod"].Patch, platformFacts{Fixes: []patchFix{{Name: "cross-spawn", From: "7.0.3", To: "7.0.5"}}, Rebuild: true}, s,
		"http://localhost:7777", at, (&app{}).ago)
	wants(t, "status-rebuilt.txt", rendered(t, rebuilt, latest))
	var ai strings.Builder
	agent := &app{out: &ai, render: Render{Format: FormatAI}}
	agent.renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	wants(t, "status-ai-patched.txt", ai.String())
	s.Envs["prod"].Patch = &envPatch{State: "held", Why: "production_held"}
	s.Platform = platformLineOf("prod", *s.Envs["prod"].Patch, platformFacts{}, s, "http://localhost:7777", at, (&app{}).ago)
	ai.Reset()
	agent.renderStatus("claims-portal", "http://localhost:7777", s, latest, at)
	if got := ai.String(); !strings.Contains(got, "\nplatform: held: production is held\naccess: ") {
		t.Errorf("a production not asked carries the approval's lines under --format ai:\n%s", got)
	}
}

// A team's denial is production's to say, over a patched preview, naming the packages from the denied approval's read
// (#2601): production still runs them, and Canyon asks again only for a newer fix.
func TestStatusSaysProductionDeclined(t *testing.T) {
	s, latest := reads(t, page(t), "status")
	s.Envs["preview"].Patch = &envPatch{State: "patched_preview", Since: "2026-09-02T10:00:00Z"}
	s.Envs["prod"] = &env{URL: "http://claims-portal.prod.localhost:7777", Label: s.Envs["preview"].Label, DeployedAt: "2026-08-30T12:00:00Z", State: "running", RunBy: "canyon",
		Patch: &envPatch{State: "denied", Approval: "K7QF-2M9X"}}
	if env, p := pickPlatform(s.Envs); env != "prod" || p.State != "denied" {
		t.Fatalf("the row is of %s %+v, want production denied", env, p)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/promotion-approvals/K7QF-2M9X" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"state":"denied","kind":"patch","fixes":[{"name":"libssl3","from":"3.0.13-0ubuntu3.4","to":"3.0.13-0ubuntu3.5","vulnerability":"CVE-2024-5535","class":"os"}]}`))
	}))
	defer server.Close()
	s.Platform = platformOf(context.Background(), newClient(server.URL), wallClock, "/api/v1/projects/claims-portal", s, at, (&app{}).ago)
	wants(t, "status-declined.txt", rendered(t, s, latest))
}

// Production's revert is said in the words of the run that watched production: the promote run the patch approval
// started, which the status read names as reverted's run (contracts/routes.md, envs.<env>.patch) — its last
// build.log line, the revert, never the patch run's passed watch in preview nor the watch's own first line.
func TestStatusSaysProductionsRevertFromItsPromoteRun(t *testing.T) {
	lines := map[string][]string{
		"gr_promote": {"Watching production until 11:15 UTC: if the patched app exits or restarts, Canyon puts back the image it replaced",
			"Canyon reverted production at 11:02 UTC: the patched app exited, so the image it replaced runs again"},
		"gr_patch": {"The watch passed: the patched app neither exited nor restarted"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		run := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/gate-runs/"), "/timeline")
		said, ok := lines[run]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var page timelinePage
		for i, line := range said {
			data, _ := json.Marshal(map[string]string{"line": line})
			page.Rows = append(page.Rows, timelineRow{ID: int64(i + 1), TS: "2026-09-02T11:02:00Z", Kind: "build.log", Data: data})
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	defer server.Close()
	s, _ := reads(t, page(t), "status")
	s.Envs["prod"] = &env{URL: "http://claims-portal.prod.localhost:7777", Label: "v1.3", State: "running", RunBy: "canyon", Patch: &envPatch{State: "reverted", Run: "gr_promote"}}
	line := platformOf(context.Background(), newClient(server.URL), wallClock, "/api/v1/projects/claims-portal", s, at, (&app{}).ago)
	if line == nil || line.State != stateFail || line.Text != lines["gr_promote"][1] {
		t.Fatalf("production's revert reads %+v, want ✗ %q", line, lines["gr_promote"][1])
	}
}

// An image read that could not check reads with the cross mark, its own why (D5's `could not check: <tool>: <reason>`), never
// as nothing to say: a check that never finished is not a pass (#2079). One never scanned says nothing, as before.
func TestPlatformLineCouldNotCheck(t *testing.T) {
	s, _ := reads(t, page(t), "status")
	why := "could not check: its last rescan did not finish"
	for _, c := range []struct {
		name  string
		patch envPatch
		facts platformFacts
		want  string
	}{
		{"none", envPatch{State: "none"}, platformFacts{Read: "could_not_check", Why: why}, "  platform     ✗ " + why + "\n"},
		{"patched", envPatch{State: "patched_preview"}, platformFacts{Read: "could_not_check", Why: why}, "  platform     ✗ " + why + "\n"},
		{"never scanned", envPatch{State: "none"}, platformFacts{Read: "not_scanned", Why: "no image scan recorded"}, ""},
	} {
		var out strings.Builder
		a := &app{out: &out}
		a.platformRow(platformLineOf("preview", c.patch, c.facts, s, "http://localhost:7777", at, a.ago))
		if out.String() != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, out.String(), c.want)
		}
	}
}
