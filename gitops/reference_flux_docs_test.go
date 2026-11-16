package gitops

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U6 (#2479): the Flux page shows each of Flux's reference files and the namespaces chart's templates as they are, as
// the Argo CD page shows the Gateway, so a platform team copying from the docs applies what Canyon's CI runs.
func TestFluxPageShowsTheReferenceAsItIs(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "docs", "src", "content", "docs", "self-host", "hand-off-flux.md")) //nolint:gosec // this tree's docs
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"flux/namespaces.yaml", "flux/rbac.yaml", "flux/admission.yaml", "flux/repository.yaml", "flux/sources.yaml", "flux/generator.yaml",
		"namespaces/templates/namespace.yaml", "namespaces/templates/flux.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "deploy", "gitops", rel)) //nolint:gosec // this tree's reference set-up
		if err != nil {
			t.Fatal(err)
		}
		body := raw
		if strings.HasPrefix(rel, "flux/") { // the file's own header comment is the page's prose
			body = raw[bytes.Index(raw, []byte("apiVersion:")):]
		}
		if !bytes.Contains(page, append(append([]byte("```yaml\n"), body...), "```"...)) {
			t.Errorf("the Flux page does not show deploy/gitops/%s as it is", rel)
		}
	}
}
