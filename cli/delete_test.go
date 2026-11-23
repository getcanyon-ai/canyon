package cli

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// deleting runs the verb against a host whose project read lists one database and one bare declaration, and whose
// delete answers code; sent is every request.
func deleting(t *testing.T, code int, body, input string, tty bool, args ...string) (out, errs string, exit int, sent []string) {
	t.Helper()
	return deletingFrom(t, `{"project":{"id":"claims-portal","resources":[{"name":"db","type":"postgres","env":"preview","state":"ready"},{"name":"files","type":"s3","state":"declared"}]}}`, code, body, input, tty, args...)
}

// deletingFrom is deleting against a host whose project read is project.
func deletingFrom(t *testing.T, project string, code int, body, input string, tty bool, args ...string) (out, errs string, exit int, sent []string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Method+" "+r.URL.RequestURI())
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(project))
			return
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n")
	t.Chdir(dir)
	env := map[string]string{"CANYON_HOST": server.URL, "XDG_CONFIG_HOME": t.TempDir(), "NO_COLOR": "1"}
	out, errs, exit = answer(t, input, env, tty, append([]string{"delete"}, args...)...)
	return pastHeader(out), errs, exit, sent
}

const asked, deleteSent = "GET /api/v1/projects/claims-portal", "DELETE /api/v1/projects/claims-portal?confirm=claims-portal"

// The verb shows what it destroys and deletes only once the person types the app's name: never a y/N.
func TestDeleteListsWhatItDestroysAndNeedsTheNameTyped(t *testing.T) {
	question := "\n  Deleting claims-portal destroys its secrets and these, with their data, immediately:\n    postgres db in preview\n  Undeploy keeps them; this does not.\n  Type claims-portal to delete it: "
	for _, typed := range []string{"\n", "y\n", "claims\n"} {
		out, errs, code, sent := deleting(t, http.StatusNoContent, "", typed, true)
		if code != exitError || errs != question || out != "\n  Nothing was deleted.\n" || len(sent) != 1 || sent[0] != asked {
			t.Errorf("typed %q: exit %d, stderr %q, stdout %q, sent %q", typed, code, errs, out, sent)
		}
	}
	out, _, code, sent := deleting(t, http.StatusNoContent, "", "claims-portal\n", true)
	if code != exitOK || out != "\n  ✓ Deleted claims-portal. Its databases and their data are gone.\n" || len(sent) != 2 || sent[1] != deleteSent {
		t.Errorf("the name typed: exit %d, stdout %q, sent %q", code, out, sent)
	}
	if _, errs, code, sent := deleting(t, http.StatusNoContent, "", "claims-portal\n", false); code != exitError || len(sent) != 1 || !strings.Contains(errs, "--confirm claims-portal") {
		t.Errorf("a pipe answered for a person: exit %d, stderr %q, sent %q", code, errs, sent)
	}
	if _, _, code, sent := deleting(t, http.StatusNoContent, "", "", false, "--confirm", "claims-portal"); code != exitOK || len(sent) != 1 || sent[0] != deleteSent {
		t.Errorf("--confirm: exit %d, sent %q", code, sent)
	}
	if _, errs, code, sent := deleting(t, http.StatusNoContent, "", "", false, "--confirm", "payroll"); code != exitError || len(sent) != 0 || !strings.Contains(errs, "claims-portal") {
		t.Errorf("--confirm naming another app: exit %d, stderr %q, sent %q", code, errs, sent)
	}
}

// An app with no databases is not told it has some, and a live production is named before the name
// is typed (cli.md §1, amended 2026-10-01); a --confirm delete read nothing, so it claims nothing.
func TestDeleteSaysOnlyWhatItDestroys(t *testing.T) {
	bare := `{"project":{"id":"claims-portal","resources":[{"name":"files","type":"s3","state":"declared"}],"envs":{"prod":{"url":"https://claims-portal.prod.meridian.example"}}}}`
	out, errs, code, _ := deletingFrom(t, bare, http.StatusNoContent, "", "claims-portal\n", true)
	if want := "\n  Deleting claims-portal destroys its secrets, immediately.\n  Production is live at https://claims-portal.prod.meridian.example, and goes offline with it.\n" +
		"  Undeploy keeps them; this does not.\n  Type claims-portal to delete it: "; code != exitOK || errs != want || out != "\n  ✓ Deleted claims-portal.\n" {
		t.Errorf("exit %d, stderr %q, stdout %q", code, errs, out)
	}
	if out, _, code, _ := deleting(t, http.StatusNoContent, "", "", false, "--confirm", "claims-portal"); code != exitOK || out != "\n  ✓ Deleted claims-portal.\n" {
		t.Errorf("--confirm: exit %d, stdout %q", code, out)
	}
}

// Every other answer is one line in the person's words.
func TestDeleteRefusalsAreALine(t *testing.T) {
	for _, c := range []struct {
		status, exit int
		body, line   string
	}{
		{http.StatusBadGateway, exitError, `{"error":"teardown_failed","message":"the postgres db in preview could not be deleted: its data is kept, and deleting again resumes"}`, "Not deleted: the postgres db in preview could not be deleted: its data is kept, and deleting again resumes."},
		{http.StatusForbidden, exitBlocked, `{"error":"person_required","message":"no"}`, "Only a person can delete claims-portal: it holds production. Sign in as yourself with canyon login, then run this again."},
		{http.StatusConflict, exitError, `{"error":"run_in_progress","message":"busy","run":{"id":"gr_9a1c","state":"running"}}`, "A run of claims-portal is under way (gr_9a1c). Let it finish, then run this again."},
	} {
		out, errs, code, _ := deleting(t, c.status, c.body, "", false, "--confirm", "claims-portal")
		if code != c.exit || out != "" || errs != "\n  ✗ "+c.line+"\n" {
			t.Errorf("a %d exited %d, stdout %q, stderr %q; want %q", c.status, code, out, errs, c.line)
		}
	}
}

// A delete still under way is exit 0 and says what it waits on (design/HAND-OFF.md §7): a database a
// driver is deleting, an environment the org's platform is removing, or both.
func TestDeletePendingSaysWhatItWaitsOn(t *testing.T) {
	const databases, platform = "\n  · Canyon is still deleting claims-portal's databases. Run this again to finish.\n", "\n  · claims-portal: being removed by your platform. Run this again to finish.\n"
	db, rel := `{"name":"db","type":"postgres","env":"preview","state":"deleting"}`, `{"release":"rel_9b31","env":"preview"}`
	for body, want := range map[string]string{`{"pending":[` + db + `]}`: databases, `{"pending":[` + rel + `]}`: platform, `{"pending":[` + db + `,` + rel + `]}`: databases + platform} {
		out, errs, code, _ := deleting(t, http.StatusAccepted, body, "", false, "--confirm", "claims-portal")
		if code != exitOK || errs != "" || out != want {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want %q", body, code, out, errs, want)
		}
	}
}

// What the org's platform provisioned is listed as its own (#2432): the delete's remove asks the
// platform to delete it, and Canyon claims to destroy only its own.
func TestDeleteListsThePlatformsResourcesAsItsOwn(t *testing.T) {
	project := `{"project":{"id":"claims-portal","resources":[{"name":"db","type":"postgres","env":"preview","state":"ready"},{"name":"files","type":"s3","env":"preview","state":"provided","endpoint":"s3://claims-portal-files"}]}}`
	_, errs, _, _ := deletingFrom(t, project, http.StatusNoContent, "", "\n", true)
	if want := "    postgres db in preview\n    s3 files in preview, your platform's\n"; !strings.Contains(errs, want) {
		t.Errorf("the listing said %q, wanting %q", errs, want)
	}
}
