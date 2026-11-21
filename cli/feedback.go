package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
)

// feedbackRepo is the repository a person asks Canyon for something on.
const feedbackRepo = "getcanyon-ai/canyon"

// What Canyon doesn't do yet, each where a refusal says so. An id is stable, as issues are counted by it: one
// is added the day a refusal of it exists.
const (
	languageUnrecognised = "language_unrecognised"  // no buildpack on the host recognised the app
	s3Needed             = "s3_needed"              // an s3 resource on a host that provisions none
	ownDockerfileIgnored = "own_dockerfile_ignored" // a Dockerfile at the package root, which the build never reads
)

// feedbackForms are .github/ISSUE_TEMPLATE's forms, each with canyon feedback's line for it.
var feedbackForms = []struct{ template, line string }{
	{"missing-feature.yml", "Ask for a feature"},
	{"blocked-wrongly.yml", "Canyon blocked something it shouldn't"},
	{"missed.yml", "Canyon missed something it should have caught"},
	{"install-failed.yml", "Canyon didn't install or start"},
}

// issueLink opens a new issue from template on Canyon's repository, its fields prefilled with this CLI's
// version and OS and with fields. Nothing of the person's goes in: no app, no host, no path.
func issueLink(template string, fields url.Values) string {
	fields.Set("template", template)
	fields.Set("version", Version)
	fields.Set("os", goos)
	return "https://github.com/" + feedbackRepo + "/issues/new?" + fields.Encode()
}

// askFor is the line under a refusal of what Canyon doesn't do yet (#2800).
func askFor(category string) string {
	return "Not supported yet. Ask for it: " + issueLink("missing-feature.yml", url.Values{"category": {category}})
}

// askedFile is beside the config: which app was already asked about what (firstAsk).
const askedFile = "asked.json"

// firstAsk is whether to say an ask line that no refusal follows, which is said once per app on this machine and
// never again: a yes is recorded in asked.json beside the config. A record that cannot be read or written is no
// record: the line is said, as it would be the first time.
func (a *app) firstAsk(app, category string) bool {
	path, err := configPath(a.getenv)
	if err != nil {
		return true
	}
	path = filepath.Join(filepath.Dir(path), askedFile)
	var asked map[string]bool
	if body, err := os.ReadFile(path); err != nil || json.Unmarshal(body, &asked) != nil || asked == nil { //nolint:gosec // beside this machine's own config, at a path composed from XDG_CONFIG_HOME or HOME and nothing else
		asked = map[string]bool{}
	}
	key := app + " " + category
	if asked[key] {
		return false
	}
	asked[key] = true
	if body, err := json.Marshal(asked); err == nil && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = atomicWrite(path, body, true) // a write that fails says the line again next time
	}
	return true
}

func runFeedback(_ context.Context, a *app, args []string) int {
	rest, err := a.parse(args, func(*flag.FlagSet) {})
	if err != nil {
		return a.usage(err)
	}
	if len(rest) > 0 {
		return a.usage(fmt.Errorf("feedback takes no arguments, and got %q", rest[0]))
	}
	for _, f := range feedbackForms {
		a.printf("%s: %s\n", f.line, issueLink(f.template, url.Values{}))
	}
	return exitOK
}

// askS3 adds the ask line under an upload refused while canyon.toml declares an s3 resource this host
// doesn't provision: the refusal's code and the host's own menu say so, never the message's words.
func (a *app) askS3(ctx context.Context, client *Client, dir string, err error) {
	if _, refused := refusal(err, http.StatusBadRequest, "invalid_resources"); !refused || a.remote { // on /mcp there is no canyon.toml to read
		return
	}
	if p, perr := readProject(dir); perr != nil || !slices.Contains(p.Types, "s3") {
		return
	}
	var read componentTypes
	if client.get(ctx, "/api/v1/component-types", &read) != nil {
		return
	}
	if slices.ContainsFunc(read.ComponentTypes, func(c componentType) bool { return c.Type == "s3" && c.Status == "available" }) {
		return
	}
	a.errf("  %s\n", askFor(s3Needed))
}
