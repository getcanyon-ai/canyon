package demo

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runDemo runs apps/run.sh, the demo-apps image's entrypoint, beside the sample apps it deploys, with a canyon on
// PATH whose fleet and login answer and whose deploy runs deploy, a shell command, and a curl that keeps its
// arguments, prints status where it is asked for one, and fails, exit 22, the calls whose arguments hold fail ("" fails
// none), in env besides PATH. It answers what run.sh printed, how it exited, and what it sent with curl.
func runDemo(t *testing.T, deploy, fail, status string, env ...string) (string, int, string) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "canyon"), []byte("#!/bin/sh\ncase \"$1\" in fleet|login) exit 0;; esac\n"+deploy+"\n"), 0o700); err != nil { //nolint:gosec // a test fake must be executable
		t.Fatal(err)
	}
	sent := filepath.Join(bin, "sent")
	fails := ""
	if fail != "" {
		fails = fmt.Sprintf("case \"$*\" in *%s*) exit 22;; esac\n", fail)
	}
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fmt.Sprintf("#!/bin/sh\necho \"$*\" >>%s\n%scase \"$*\" in *http_code*) printf %s;; esac\nexit 0\n", sent, fails, status)), 0o700); err != nil { //nolint:gosec // a test fake must be executable
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join("apps", "run.sh")) //nolint:gosec // the demo's own script, from this tree
	cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin"}, env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(sent) //nolint:gosec // the test's own file; none when curl never ran
	return string(out), cmd.ProcessState.ExitCode(), string(body)
}

const (
	building = "Building the sample apps: claims-portal, endorsement-hub, claims-archive and premium-ledger.\n" +
		"A first start takes 15 to 20 minutes at 25 Mbit/s, longer on a slower line: it downloads about 1.5 GB, most of it Canyon's images and its Node build image. The executor-1 lines below show how it's going. Once everything is here, a start takes 5 to 10 minutes.\n"
	carryOn = "To carry on, start the demo again (canyon demo up, or make demo in a checkout).\n"
	// running is the line each sample app's deploy ends on, which canyon demo up says as progress.
	running = "canyon-demo: running claims-portal\ncanyon-demo: running endorsement-hub\ncanyon-demo: running claims-archive\ncanyon-demo: running premium-ledger\n"
)

// A first start says what it costs before any deploy. A deploy that fails — a first one past its ceiling on a slow
// line among them — ends on how to carry on, with its exit status, and so does the blocked version's deploy when it
// fails rather than blocks; a start that goes through says so (#2697).
func TestAFailedDeploySaysHowToCarryOn(t *testing.T) {
	slow := `echo "  claims-portal is still building after 35 minutes, and it carries on without this command: canyon status says when it's live."; exit 1`
	blocks := `case "$PWD" in */claims-portal-next) exit 2;; esac`
	for name, c := range map[string]struct {
		deploy, out string
		code        int
		fail        string // the curl call that fails
	}{
		"the first deploy fails": {slow, building + "  claims-portal is still building after 35 minutes, and it carries on without this command: canyon status says when it's live.\n" + carryOn, 1, ""},
		"the blocked version's deploy fails": {`case "$PWD" in */claims-portal-next) exit 1;; esac`, building + running +
			"claims-portal's next version was meant to be blocked at review; canyon deploy exited 1\n" + carryOn, 1, ""},
		"the blocked version deploys": {`exit 0`, building + running + "claims-portal's next version was meant to be blocked at review; canyon deploy exited 0\n", 1, ""},
		"it goes through": {blocks, building + running +
			"The sample apps are running, and claims-portal's next version is blocked at review.\n", 0, ""},
		"the design system cannot be set": {blocks, building + running +
			"The demo's design system could not be set: PUT /api/v1/preferences failed.\n" + carryOn, 1, "/api/v1/preferences"},
		"Trivy cannot be left off": {blocks, "The demo could not leave Trivy off: PUT /api/v1/policies/sarif.trivy failed.\n" + carryOn, 1, "sarif.trivy"},
	} {
		if out, code, _ := runDemo(t, c.deploy, c.fail, "404"); out != c.out || code != c.code {
			t.Errorf("%s: exit %d, want %d, and run.sh printed\n%s\nwant\n%s", name, code, c.code, out, c.out)
		}
	}
}

// leaveTrivyOff is what run.sh sends first: sarif.trivy's run off, so no deploy's scan fetches Trivy (#237).
const leaveTrivyOff = "-fsS -o /dev/null -X PUT -H Content-Type: application/json -d {\"run\":false} http://localhost:7777/api/v1/policies/sarif.trivy?apply=true\n"

// The laptop demo leaves Trivy off unless canyon demo up --with-trivy asked for it (cli.md §6.14; #237): before any
// deploy, run.sh turns sarif.trivy's run off; with CANYON_DEMO_TRIVY=on it leaves the rule as it ships.
func TestTheDemoLeavesTrivyOffUnlessAskedFor(t *testing.T) {
	blocks := `case "$PWD" in */claims-portal-next) exit 2;; esac`
	if out, code, sent := runDemo(t, blocks, "", "200"); code != 0 || !strings.HasPrefix(sent, leaveTrivyOff) || !strings.HasPrefix(out, building) {
		t.Errorf("without CANYON_DEMO_TRIVY: exit %d, and run.sh sent\n%s", code, sent)
	}
	if _, code, sent := runDemo(t, blocks, "", "200", "CANYON_DEMO_TRIVY=on"); code != 0 || strings.Contains(sent, "sarif.trivy") {
		t.Errorf("with CANYON_DEMO_TRIVY=on: exit %d, and run.sh sent\n%s", code, sent)
	}
}

// Once the sample apps run, the demo's company picks Canyon as its design system, as the dashboard does, and
// leaves its rule Advisory: the tour's first chapter has the visitor make it Blocking (cli.md §6.14.1).
func TestTheDemoStartsWithCanyonAsItsDesignSystem(t *testing.T) {
	_, code, sent := runDemo(t, `case "$PWD" in */claims-portal-next) exit 2;; esac`, "", "200")
	want := leaveTrivyOff +
		"-fsS -o /dev/null -X PUT -H Content-Type: application/json -d {\"design_system_id\":\"ds_0002\"} http://localhost:7777/api/v1/preferences\n" +
		"-sS -o /dev/null -w %{http_code} http://localhost:7777/api/v1/playbooks/claims-portal-conventions\n"
	if code != 0 || sent != want {
		t.Errorf("exit %d, and run.sh sent\n%s\nwant\n%s", code, sent, want)
	}
}

// Once the sample apps run, the demo's org has one playbook, the one the tour's security chapter has the agent read
// (design/specs/mcp.md §11): saved where the demo has none, left as it is where a start before saved it, and a read
// that fails stops the start, saying how to carry on.
func TestTheDemoSavesItsPlaybookOnce(t *testing.T) {
	blocks := `case "$PWD" in */claims-portal-next) exit 2;; esac`
	const save = "-fsS -o /dev/null -X PUT -H Content-Type: application/json --data-binary @apps/claims-portal-conventions.json http://localhost:7777/api/v1/playbooks/claims-portal-conventions\n"
	_, code, sent := runDemo(t, blocks, "", "404")
	if code != 0 || !strings.HasSuffix(sent, save) {
		t.Errorf("a demo with no playbook: exit %d, and run.sh sent\n%s", code, sent)
	}
	out, code, sent := runDemo(t, blocks, "", "500")
	if code != 1 || strings.Contains(sent, "-X PUT -H Content-Type: application/json --data-binary") ||
		!strings.HasSuffix(out, "The demo's playbook could not be read: GET /api/v1/playbooks/claims-portal-conventions failed.\n"+carryOn) {
		t.Errorf("a read that fails: exit %d, run.sh printed\n%s\nand sent\n%s", code, out, sent)
	}
	var doc struct{ Name, Kind, When, Body string }
	raw, err := os.ReadFile(filepath.Join("apps", "claims-portal-conventions.json"))
	if err != nil || json.Unmarshal(raw, &doc) != nil || doc.Kind != "skill" || doc.Name == "" || doc.When == "" ||
		!strings.Contains(doc.Body, `db.prepare("SELECT * FROM claims WHERE policy_number = ?").all(req.query.policy)`) {
		t.Errorf("the playbook the demo saves: %v %+v", err, doc)
	}
}
