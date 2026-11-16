//go:build cluster

// Package cluster is the cluster suite's Go journeys (CASES.md) against a Canyon already installed
// at $E2E_CANYON_URL, its cluster read through $E2E_KUBECONFIG. They carry the cluster build tag, so
// go test ./... never runs them; `go test -tags cluster ./e2e/cluster/...` does (README.md).
package cluster

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// host is Canyon's public URL (README, What the test cluster provides). The suite has no default target.
var host = os.Getenv("E2E_CANYON_URL")

var cli string // the canyon binary TestMain builds

// TestMain builds the CLI, reaches the cluster when $E2E_KUBECONFIG names one, runs the tests, and
// fails the run if its output holds a password or the client secret (README, What the test cluster provides).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "canyon-e2e-cluster-")
	if err != nil {
		panic(err)
	}
	cli = filepath.Join(dir, "canyon")
	build := exec.Command("go", "build", "-o", cli, "./cmd/canyon") //nolint:gosec // make cli's build, into a temp dir
	build.Dir, build.Stdout, build.Stderr = "../..", os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		panic("building the canyon CLI: " + err.Error())
	}
	if path := os.Getenv("E2E_KUBECONFIG"); path != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", path)
		if err == nil {
			kube, err = kubernetes.NewForConfig(cfg)
		}
		if err != nil {
			log.Fatal(err)
		}
	}
	output, code := captured(m.Run)
	_ = os.RemoveAll(dir)
	if leaked := leakedSecrets(output, secretVars()); len(leaked) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: the run's output holds the value of %s\n", strings.Join(leaked, ", "))
		code = 1
	}
	for _, v := range k1Values {
		if bytes.Contains(output, []byte(v)) {
			fmt.Fprintln(os.Stderr, "FAIL: the run's output holds a value K1 set")
			code = 1
		}
	}
	for _, v := range fetchTokens {
		if bytes.Contains(output, []byte(v)) {
			fmt.Fprintln(os.Stderr, "FAIL: the run's output holds a fetch-only token H4 made")
			code = 1
		}
	}
	os.Exit(code)
}

// captured runs run with os.Stdout and os.Stderr, where go test -v and a panic or a failed build
// write, both teed into one buffer: the streams still reach the terminal as they are written, and
// the buffer -- holding either -- is what the end-of-run grep reads.
func captured(run func() int) ([]byte, int) {
	stdout, stderr := os.Stdout, os.Stderr
	var output bytes.Buffer
	sink := &syncWriter{w: &output}
	outR, outW := pipeTo(stdout, sink)
	errR, errW := pipeTo(stderr, sink)
	os.Stdout, os.Stderr = outW, errW
	code := run()
	os.Stdout, os.Stderr = stdout, stderr
	_ = outW.Close()
	_ = errW.Close()
	<-outR
	<-errR
	return output.Bytes(), code
}

// pipeTo copies a new pipe's read side to both real (the terminal, still) and sink (the shared
// buffer) until the write side closes, and answers the write end and a channel that closes then.
func pipeTo(real *os.File, sink io.Writer) (done <-chan struct{}, w *os.File) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	c := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.MultiWriter(real, sink), r)
		close(c)
	}()
	return c, w
}

// syncWriter serializes concurrent writes from the stdout and stderr copiers into one buffer;
// bytes.Buffer itself is not safe for concurrent writers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// secretVars is every variable the suite reads whose value is a secret (README, What the test cluster
// provides): an explicit list, never a naming convention -- E2E_K1_VALUE fell outside "ends in
// _PASSWORD" silently. TestUnitSecretVarsCoverTheHarness fails when a new name
// of that shape is read anywhere in the harness and missing here.
func secretVars() []string {
	return []string{"E2E_OIDC_CLIENT_SECRET", "E2E_ADMIN_PASSWORD", "E2E_ALICE_PASSWORD", "E2E_BOB_PASSWORD", "E2E_K1_VALUE", "E2E_DEPLOY_REPO_TOKEN"}
}

// leakedSecrets is which of the named variables hold a value that appears in output. An unset or
// empty one cannot leak, and every output contains the empty string.
func leakedSecrets(output []byte, names []string) []string {
	var leaked []string
	for _, name := range names {
		if v := os.Getenv(name); v != "" && bytes.Contains(output, []byte(v)) {
			leaked = append(leaked, name)
		}
	}
	return leaked
}

// TestUnitCapturedIncludesStderr is the grep's other half: go test -v's own failure output (a panic, a
// t.Fatal in a goroutine, a build error) lands on stderr, and a secret written there must be seen
// too, not just what a test t.Logf's to stdout.
func TestUnitCapturedIncludesStderr(t *testing.T) {
	const secret = "s3cr3t-on-stderr"
	output, code := captured(func() int {
		fmt.Fprint(os.Stderr, "a secret leaked: "+secret)
		return 0
	})
	if code != 0 {
		t.Fatalf("code = %d; want 0", code)
	}
	if !bytes.Contains(output, []byte(secret)) {
		t.Fatalf("captured() output %q does not hold %q written to stderr", output, secret)
	}
}

// TestUnitSecretVarsCoverTheHarness looks at every Go file of this package and every TypeScript
// file of e2e/ui/harness and e2e/ui/cluster for an E2E_* name holding SECRET, PASSWORD, TOKEN or
// VALUE, and fails when one is not in secretVars: a secret the test cluster hands the suite must be in the
// end-of-run grep. (signin.ts builds E2E_<USER>_PASSWORD at runtime; the three are listed by hand.)
func TestUnitSecretVarsCoverTheHarness(t *testing.T) {
	secretShaped := regexp.MustCompile(`E2E_[A-Z0-9_]*(SECRET|PASSWORD|TOKEN|VALUE)[A-Z0-9_]*`)
	listed := secretVars()
	for _, glob := range []string{"*.go", "../ui/harness/*.ts", "../ui/cluster/*.ts"} {
		files, err := filepath.Glob(glob)
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: %v (no files)", glob, err)
		}
		for _, f := range files {
			body, err := os.ReadFile(f) //nolint:gosec // the harness's own sources
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range secretShaped.FindAllString(string(body), -1) {
				if !slices.Contains(listed, name) {
					t.Errorf("%s reads %s, which secretVars does not list", f, name)
				}
			}
		}
	}
}

func TestUnitLeakedSecrets(t *testing.T) {
	t.Setenv("E2E_ALICE_PASSWORD", "s3cr3t")
	t.Setenv("E2E_BOB_PASSWORD", "")
	t.Setenv("E2E_K1_VALUE", "k1-v4lu3")
	if got := leakedSecrets([]byte("playwright: digest of k1-v4lu3 shown\n"), secretVars()); !slices.Equal(got, []string{"E2E_K1_VALUE"}) {
		t.Errorf("an output holding K1's value leaks %v; want [E2E_K1_VALUE]", got)
	}
	if got := leakedSecrets([]byte("=== RUN TestM1\n    typed s3cr3t into the provider\n"), secretVars()); !slices.Equal(got, []string{"E2E_ALICE_PASSWORD"}) {
		t.Errorf("an output holding alice's password leaks %v; want [E2E_ALICE_PASSWORD]", got)
	}
	if got := leakedSecrets([]byte("=== RUN TestM1\n--- PASS: TestM1\n"), secretVars()); len(got) != 0 {
		t.Errorf("an output holding no value leaks %v; want none", got)
	}
}
