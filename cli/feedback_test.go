package cli

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// canyon feedback prints one link per issue template, each carrying the CLI's version and OS and nothing
// of the person's: no app, no host, no path, though it runs in an app's directory signed in to a host.
func TestFeedbackLinksEachTemplateAndNothingOfYours(t *testing.T) {
	want, dir := golden(t, "feedback.txt"), projectDir(t)
	t.Chdir(dir)
	out, errOut, code := run(t, map[string]string{"CANYON_HOST": "https://canyon.meridian.example", "XDG_CONFIG_HOME": t.TempDir()}, false, "feedback")
	if code != exitOK || errOut != "" || out != want {
		t.Errorf("exit %d, stderr %q, printed\n%s\ntestdata/feedback.txt wants\n%s", code, errOut, out, want)
	}
	for _, theirs := range []string{"meridian", "claims-portal", dir} {
		if strings.Contains(out, theirs) {
			t.Errorf("the links carry %q:\n%s", theirs, out)
		}
	}
}

// The ask line's link is a new issue on Canyon's repository from the missing-feature template, and its
// query is the category, the version and the OS: nothing else can reach a public issue.
func TestTheAskLineCarriesOnlyTheCategoryVersionAndOS(t *testing.T) {
	link, ok := strings.CutPrefix(askFor(s3Needed), "Not supported yet. Ask for it: ")
	u, err := url.Parse(link)
	if !ok || err != nil || u.Scheme != "https" || u.Host != "github.com" || u.Path != "/getcanyon-ai/canyon/issues/new" ||
		!reflect.DeepEqual(u.Query(), url.Values{"category": {"s3_needed"}, "os": {"linux"}, "template": {"missing-feature.yml"}, "version": {"dev"}}) {
		t.Errorf("the ask line is %q", askFor(s3Needed))
	}
}

// Each template a link names is in .github/ISSUE_TEMPLATE, carries one label no other template does, so
// a count can tell them apart, and has a field for each value the link prefills: GitHub fills a form's
// field from the query parameter named by its id, and drops one no field has.
func TestEachLinkedTemplateHasItsLabelAndTheFieldsTheLinkFills(t *testing.T) {
	labels := map[string]bool{}
	for _, f := range feedbackForms {
		raw, err := os.ReadFile(filepath.Join("..", ".github", "ISSUE_TEMPLATE", f.template)) //nolint:gosec // the repository's own template
		if err != nil {
			t.Fatal(err)
		}
		var form struct {
			Labels []string
			Body   []struct{ ID string }
		}
		if err := yaml.Unmarshal(raw, &form); err != nil {
			t.Fatalf("%s: %v", f.template, err)
		}
		if len(form.Labels) != 1 || labels[form.Labels[0]] {
			t.Errorf("%s: labels %v, want one no other template has", f.template, form.Labels)
		} else {
			labels[form.Labels[0]] = true
		}
		fills := []string{"os", "version"}
		if f.template == "missing-feature.yml" {
			fills = append(fills, "category")
		}
		for _, id := range fills {
			if !slices.ContainsFunc(form.Body, func(field struct{ ID string }) bool { return field.ID == id }) {
				t.Errorf("%s has no field with the id %q, so the link's %s is dropped", f.template, id, id)
			}
		}
	}
}

// A package whose canyon.toml declares an s3 resource this host can't provision is refused by the host,
// and the CLI adds the ask line under it. A host that provisions s3 refused it for another reason, and
// a refusal of a package declaring no s3 is about something else: neither gets the line.
func TestAnS3DeclarationThisHostCantProvisionAsksForIt(t *testing.T) {
	refused := reply{http.StatusBadRequest, `{"error":"invalid_resources","message":"resource \"files\" has type \"s3\", which this host does not provision yet: ask whoever runs it to set that up, or remove the declaration"}`}
	planned := reply{http.StatusOK, `{"component_types":[{"type":"postgres","status":"available"},{"type":"s3","status":"planned"}]}`}
	available := reply{http.StatusOK, `{"component_types":[{"type":"postgres","status":"available"},{"type":"s3","status":"available"}]}`}
	for _, c := range []struct {
		name, declares string
		menu           reply
		asks           bool
	}{
		{"s3 planned", "s3", planned, true},
		{"s3 available", "s3", available, false},
		{"no s3 declared", "postgres", planned, false},
	} {
		for _, verb := range []string{"deploy", "check"} {
			t.Run(c.name+" "+verb, func(t *testing.T) {
				dir := projectDir(t)
				writeFile(t, filepath.Join(dir, projectFile), "app = \"claims-portal\"\n\n[resources]\nfiles = { type = \""+c.declares+"\" }\n")
				_, errOut, code := verbingIn(t, &gateHost{upload: refused, components: c.menu}, dir, verb)
				want := "\n  ✗ resource \"files\" has type \"s3\", which this host does not provision yet: ask whoever runs it to set that up, or remove the declaration\n"
				if c.asks {
					want += "  " + askFor(s3Needed) + "\n"
				}
				if code != exitError || !strings.HasSuffix(errOut, want) {
					t.Errorf("exit %d, stderr\n%s\nwant it to end\n%s", code, errOut, want)
				}
			})
		}
	}
}
