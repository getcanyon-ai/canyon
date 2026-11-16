package kube

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getcanyon-ai/canyon/executor"
)

const checkovSARIF = `{"version":"2.1.0","runs":[{"results":[{"ruleId":"CKV_AWS_20","locations":[{"physicalLocation":{"artifactLocation":{"uri":"scan/tree/main.tf"}}}]}]}]}`

// scanCluster plays the Job controller and kubelet for scan Jobs: each scan's pod starts with its feed
// container running; once fed, the pod ends as end says. A reader's or a refresh's pod ends after slow,
// exiting as exit says for its container (0 if unsaid). The Job's verdict follows a look later, failed
// for reason; a deleted Job's pod goes two pod lists later, as a pod takes a while to end. A log is its
// container's command run here, its program a fake writing progress to stderr
// and out (the tool's) or meta (the reader's) to stdout, both kept in one log; the JSON output a script
// prints from /tmp is <dir>/opengrep.json, which opengrep's empty errors are unless a test changes it.
// fed keeps each feed's entries, name → content, and modes their modes.
type scanCluster struct {
	*fake.Clientset
	mu        sync.Mutex
	fed       []map[string]string
	modes     []map[string]int64 // each feed's entries, name → mode
	out, meta string
	said      string // the refresh's stderr
	end       func(*corev1.Pod)
	reason    string
	exit      map[string]int32
	slow      time.Duration
	going     map[string]int // a deleted Job's name → pod lists left before its pod goes
	hold      chan struct{}  // when set, each feed waits for it
	dir       string         // the fakes, one per container name
	ran       map[string]corev1.Container
	entry     []string // the tool image's entrypoint
	looked    map[string]bool
}

func newScanCluster(t *testing.T, e *Executor, out string) *scanCluster {
	t.Helper()
	c := &scanCluster{Clientset: fake.NewClientset(), out: out, end: func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "tool", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
	}, reason: batchv1.JobReasonBackoffLimitExceeded, exit: map[string]int32{}, going: map[string]int{}, dir: t.TempDir(), ran: map[string]corev1.Container{}, looked: map[string]bool{}}
	for _, name := range []string{"tool", "reader"} {
		fake := filepath.Join(c.dir, name)
		if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'Scanning 1 file' >&2\nexec cat "+fake+".out\n"), 0o700); err != nil { //nolint:gosec // the fake runs
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(c.dir, "opengrep.json"), []byte(`{"errors":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"refresh", "sweep"} { // says why it failed on its stderr, as trivy, grype and busybox do
		fake := filepath.Join(c.dir, name)
		if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat "+fake+".out >&2\n"), 0o700); err != nil { //nolint:gosec // the fake runs
			t.Fatal(err)
		}
	}
	pods, jobs := corev1.SchemeGroupVersion.WithResource("pods"), batchv1.SchemeGroupVersion.WithResource("jobs")
	c.PrependReactor("get", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GetAction).GetName()
		p, err := c.Tracker().Get(pods, ScanNamespace, name+"-pod")
		if err != nil {
			return false, nil, nil
		}
		verdict := batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: c.reason}
		switch phase := p.(*corev1.Pod).Status.Phase; {
		case phase != corev1.PodSucceeded && phase != corev1.PodFailed:
			return false, nil, nil
		case !c.looked[name]: // the pod has ended; the Job says so from the next look
			c.looked[name] = true
			return false, nil, nil
		case phase == corev1.PodSucceeded:
			verdict = batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}
		}
		if obj, err := c.Tracker().Get(jobs, ScanNamespace, name); err == nil {
			j := obj.(*batchv1.Job).DeepCopy()
			j.Status.Conditions = []batchv1.JobCondition{verdict}
			_ = c.Tracker().Update(jobs, j, ScanNamespace)
		}
		return false, nil, nil
	})
	c.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job)
		labels := map[string]string{"batch.kubernetes.io/job-name": j.Name}
		maps.Copy(labels, j.Spec.Template.Labels)
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: ScanNamespace, Labels: labels}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "feed", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
		switch cs := j.Spec.Template.Spec.Containers; {
		case len(cs) == 0: // not the executor's
		case cs[0].Name == "tool": // canyon-scan-<tool>-<run>
			tool, _ := executor.ToolOf(strings.Split(j.Name, "-")[2])
			c.ran["tool"], c.entry = cs[0], tool.Entrypoint
		default:
			ended := p.DeepCopy()
			ended.Status.Phase = corev1.PodSucceeded
			inits := j.Spec.Template.Spec.InitContainers
			if len(inits) > 0 {
				ended.Status.InitContainerStatuses = nil
			}
			for i, ctr := range append(slices.Clone(inits), cs[0]) { // in order, as the kubelet runs them: one that fails ends the pod
				c.ran[ctr.Name] = ctr
				status := corev1.ContainerStatus{Name: ctr.Name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: c.exit[ctr.Name]}}}
				if i < len(inits) {
					ended.Status.InitContainerStatuses = append(ended.Status.InitContainerStatuses, status)
				} else {
					ended.Status.ContainerStatuses = []corev1.ContainerStatus{status}
				}
				if c.exit[ctr.Name] != 0 {
					ended.Status.Phase = corev1.PodFailed
					break
				}
			}
			if p = ended; c.slow > 0 {
				p = ended.DeepCopy()
				p.Status = corev1.PodStatus{}
				time.AfterFunc(c.slow, func() { _ = c.Tracker().Update(pods, ended, ScanNamespace) })
			}
		}
		if err := c.Tracker().Add(p); err != nil {
			t.Error(err)
		}
		return false, nil, nil
	})
	c.PrependReactor("delete", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		c.going[a.(k8stesting.DeleteAction).GetName()] = 2
		return false, nil, nil
	})
	c.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		for name := range c.going {
			if c.going[name]--; c.going[name] == 0 {
				_ = c.Tracker().Delete(pods, ScanNamespace, name+"-pod")
				delete(c.going, name)
			}
		}
		return false, nil, nil
	})
	c.PrependReactor("get", "pods/log", func(a k8stesting.Action) (bool, runtime.Object, error) {
		name := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions).Container
		out, ok := map[string]string{"tool": c.out, "reader": c.meta, "refresh": c.said, "sweep": c.said}[name]
		if !ok {
			t.Errorf("read the log of %q", name)
		}
		fake := filepath.Join(c.dir, name)
		if err := os.WriteFile(fake+".out", []byte(out), 0o600); err != nil {
			t.Error(err)
		}
		argv := append(slices.Clone(c.ran[name].Command), c.ran[name].Args...)
		if len(c.ran[name].Command) == 0 { // the image's entrypoint
			argv = append(slices.Clone(c.entry), argv...)
		}
		if argv[0] == "/scan/busybox" { // busybox runs the applet its first argument names
			argv = argv[1:]
		}
		if argv[0] == "sh" && name != "refresh" && name != "sweep" { // sh -c 'script' sh program…, the script's /tmp the test's; a refresh's own scripts never run here
			argv[2], argv[4] = strings.ReplaceAll(argv[2], "/tmp/", c.dir+"/"), fake
		} else {
			argv[0] = fake
		}
		var log bytes.Buffer
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the pod's command, the fake tool in its image's place
		cmd.Stdout, cmd.Stderr = &log, &log
		_ = cmd.Run() // its exit status is no part of its log
		return true, &runtime.Unknown{Raw: log.Bytes()}, nil
	})
	e.Client, e.Poll = c, time.Millisecond
	t.Cleanup(func() { // once the refreshes the test's scans started have ended, no tool's last refresh has failed
		e.collecting.Wait()
		for _, l := range scanning {
			l.failed.Store(false)
		}
	})
	e.Exec = func(_ context.Context, ns, pod, container string, command []string, stdin io.Reader) error {
		if ns != ScanNamespace || container != "feed" || !slices.Equal(command, []string{"sh", "-c", "tar -x -f - -C /scan && cp /bin/busybox /scan/busybox && touch /scan/.fed"}) {
			t.Errorf("exec %s/%s %s %v", ns, pod, container, command)
		}
		if c.hold != nil {
			<-c.hold
		}
		entries, modes := map[string]string{}, map[string]int64{}
		tr := tar.NewReader(stdin)
		for {
			h, err := tr.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return err
				}
				break
			}
			b, _ := io.ReadAll(tr)
			entries[h.Name], modes[h.Name] = string(b), h.Mode
		}
		c.mu.Lock()
		c.fed, c.modes = append(c.fed, entries), append(c.modes, modes)
		c.mu.Unlock()
		obj, err := c.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), ns, pod)
		if err != nil {
			return err
		}
		p := obj.(*corev1.Pod).DeepCopy()
		c.end(p)
		return c.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), p, ns)
	}
	return c
}

// scanParts is the /scan body after its json: the tree, then rules when given.
func scanParts(t *testing.T, tree []byte, rules []byte) *multipart.Reader {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range []struct {
		name string
		body []byte
	}{{"tree", tree}, {"rules", rules}} {
		if p.body == nil {
			continue
		}
		w, _ := mw.CreateFormFile(p.name, p.name+".tar")
		_, _ = w.Write(p.body)
	}
	_ = mw.Close()
	return multipart.NewReader(&b, mw.Boundary())
}

func scanTree(t *testing.T, files ...string) []byte {
	var hs []*tar.Header
	for _, f := range files {
		hs = append(hs, &tar.Header{Name: f, Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	}
	return tarOf(t, hs...)
}

func checkov(settings executor.ScanSettings) executor.ScanRequest {
	return executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "checkov", Settings: &settings}
}

// A scan is a Job in canyon-scans under a policy that lets nothing in or out: its pod is the Docker
// executor's container as Kubernetes spells it, fed the tree and the config over pods/exec; it answers
// the tool's SARIF from its log, uris as the package's paths, and the Job is gone once it answers.
func TestScanRunsTheToolAsAJobAndAnswersItsSARIF(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	res, err := e.Scan(t.Context(), checkov(executor.ScanSettings{SkipChecks: []string{"CKV_AWS_1"}}), scanParts(t, scanTree(t, "main.tf"), nil))
	tool, _ := executor.ToolOf("checkov")
	if err != nil || res.Tool != "checkov" || res.Image != tool.Image || !strings.Contains(string(res.SARIF), `"uri":"main.tf"`) || res.NotFinished != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(c.fed) != 1 || c.fed[0]["tree/main.tf"] != "xx" || c.fed[0]["checkov.yaml"] != string(tool.Config(executor.ScanSettings{SkipChecks: []string{"CKV_AWS_1"}})) || len(c.fed[0]) != 3 {
		t.Fatalf("fed %v", c.fed)
	}
	var job *batchv1.Job
	for _, a := range c.Actions() {
		if w, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "jobs" {
			job = w.GetObject().(*batchv1.Job)
		}
	}
	pod := job.Spec.Template.Spec
	sc, run := pod.Containers[0].SecurityContext, pod.Containers[0]
	if job.Labels["canyon.scan"] != "true" || job.Spec.Template.Labels["canyon.scan"] != "true" || *job.Spec.ActiveDeadlineSeconds != 600 || *job.Spec.BackoffLimit != 0 ||
		pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken || pod.InitContainers[0].Image != scanBusybox ||
		run.Image != tool.Image || !slices.Equal(run.Args, tool.Args) || *sc.RunAsUser != 65534 || *sc.RunAsGroup != 65534 || !*sc.ReadOnlyRootFilesystem || *sc.AllowPrivilegeEscalation ||
		!slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || run.Resources.Limits.Cpu().String() != "2" || run.Resources.Limits.Memory().String() != "2Gi" ||
		pod.Volumes[1].EmptyDir.Medium != corev1.StorageMediumMemory || len(run.Env) != 0 || pod.Volumes[0].EmptyDir.SizeLimit == nil || pod.Volumes[0].EmptyDir.SizeLimit.String() != "3Gi" ||
		pod.SecurityContext == nil || pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("the scan's Job %+v", job.Spec)
	}
	np, err := c.NetworkingV1().NetworkPolicies(ScanNamespace).Get(t.Context(), "canyon-scan", metav1.GetOptions{})
	if err != nil || np.Spec.PodSelector.MatchLabels["canyon.scan"] != "true" || len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Ingress)+len(np.Spec.Egress) != 0 {
		t.Fatalf("the scans' policy %+v %v", np, err)
	}
	jobs, _ := c.BatchV1().Jobs(ScanNamespace).List(t.Context(), metav1.ListOptions{})
	if pods, _ := c.CoreV1().Pods(ScanNamespace).List(t.Context(), metav1.ListOptions{}); len(jobs.Items) != 0 || len(pods.Items) != 0 {
		t.Fatalf("the scan's Job or pod outlived its answer: %d, %d", len(jobs.Items), len(pods.Items))
	}
}

// An empty package is still a tree: /scan/tree is fed, which checkov's --directory needs, as Docker
// makes it.
func TestAnEmptyPackageIsStillATree(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	if _, err := e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t), nil)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.fed[0]["tree/"]; !ok || len(c.fed[0]) != 2 {
		t.Fatalf("fed %v", c.fed)
	}
}

// not_finished, the one answer without SARIF: over_cap past 10 MiB of stdout, killed by the OOM killer
// or a signal, not_sarif otherwise, and timeout at the Job's deadline — which ends the pod with a signal
// too, before the Job says why.
func TestScanAnswersNotFinished(t *testing.T) {
	ended := func(reason string, code int32) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "tool", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: reason, ExitCode: code}}}}
		}
	}
	for want, tc := range map[string]struct {
		out, reason string
		end         func(*corev1.Pod)
	}{
		"over_cap":  {`{"version":"2.1.0","runs":[{"results":[{"message":{"text":"` + strings.Repeat("x", executor.MaxScanOut+1) + `"}}]}]}`, "", nil},
		"not_sarif": {"Traceback: …", "", nil},
		"killed":    {"", "", ended("OOMKilled", 137)},
		"timeout":   {`{"version":"2.1.0","ru`, batchv1.JobReasonDeadlineExceeded, ended("Error", 143)},
	} {
		e := &Executor{}
		c := newScanCluster(t, e, tc.out)
		if tc.end != nil {
			c.end, c.reason = tc.end, cmp.Or(tc.reason, c.reason)
		}
		res, err := e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil))
		if err != nil || res.NotFinished != want || res.SARIF != nil {
			t.Errorf("%s: %+v %v", want, res, err)
		}
	}
}

// ESLint runs Canyon's own image, CANYON_IMAGE_ESLINT; unset, it answers no_image. It takes no rules
// part, its skip paths leave the tree, and the file URL its notification names a file it could not
// parse by comes back as the package's path, as on Docker.
func TestESLintRunsTheOperatorsImage(t *testing.T) {
	e := &Executor{}
	newScanCluster(t, e, `{"version":"2.1.0","runs":[{"invocations":[{"toolConfigurationNotifications":[{"descriptor":{"id":"ESL0999"},`+
		`"locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///scan/tree/src/a.ts"}}}]}]}],"results":[]}]}`)
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "eslint", Settings: &executor.ScanSettings{SkipPaths: []string{"docs"}}}
	if res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.ts"), nil)); err != nil || res.NotFinished != "no_image" {
		t.Fatalf("no image: %+v %v", res, err)
	}
	e.ESLint = "registry.example/canyon/eslint@" + builtDigest
	if res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.ts", "docs/b.ts"), nil)); err != nil || res.Image != e.ESLint || !strings.Contains(string(res.SARIF), `"uri":"src/a.ts"`) {
		t.Fatalf("%+v %v", res, err)
	}
}

// Opengrep that ends other than 0 or 1 could not load every rules file it was handed: its SARIF, which
// holds no result, is not the answer but rules_error; its 1 is a scan (contracts/executor.md).
func TestAnOpengrepThatCannotLoadItsRulesIsRulesError(t *testing.T) {
	clean := `{"version":"2.1.0","runs":[{"results":[]}]}`
	dropped := `{"version":"2.1.0","runs":[{"invocations":[{"executionSuccessful":true,"toolExecutionNotifications":[{"descriptor":{"id":"Rule parse error"},"level":"error","message":{"text":"Rule parse error in rule org-bad:"}}]}],"results":[]}]}`
	for _, tc := range []struct {
		code   int32
		reason string
		sarif  string
		want   string
	}{{7, "Error", clean, "rules_error"}, {1, "Error", clean, ""}, {137, "Error", clean, "killed"}, {2, "OOMKilled", clean, "killed"}, {0, "", clean, ""}, {-1, "", clean, "rules_error"},
		{2, "Error", dropped, ""}, {2, "Error", clean, "rules_error"}, {2, "OOMKilled", dropped, "killed"}} { // an engine the OOM killer ended makes Opengrep itself exit 2; its 2 for rules it dropped is a scan
		code, reason, want := tc.code, tc.reason, tc.want
		e := &Executor{Opengrep: "registry.example/canyon/opengrep@" + builtDigest}
		c := newScanCluster(t, e, tc.sarif)
		c.end = func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "tool", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason}}}}
			if code == -1 { // a Failed pod whose tool's status never said it ended: unknown, never a scan
				p.Status.ContainerStatuses = nil
			}
		}
		req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "opengrep", Settings: &executor.ScanSettings{}}
		res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.js"), scanTree(t, "rule.yaml")))
		if err != nil || res.NotFinished != want || (want == "") != (res.SARIF != nil) {
			t.Errorf("exit %d: %+v %v", code, res, err)
		}
	}
}

// Opengrep runs under executor.ThenJSON with --json-output, its log its SARIF, a NUL and its JSON output:
// the answer carries the distinct rule_ids, in name order, of the JSON's Rule parse errors ("" for one
// without), and the distinct paths, in path order, of its errors that have one. Without its JSON output —
// not written, or no errors in it — the answer is not_sarif, never a clean scan.
func TestOpengrepsAnswerCarriesTheRulesAndFilesItsJSONNames(t *testing.T) {
	errs := `{"errors":[{"code":3,"level":"warn","type":["PartialParsing",[]],"path":"src/b.ts"},{"code":2,"level":"error","type":"Rule parse error","rule_id":"r","message":"Rule parse error in rule other:"},` +
		`{"type":"Rule parse error"},{"type":"Rule parse error","rule_id":"a"},{"type":"Rule parse error","rule_id":"r"},{"type":"Other","rule_id":"o"},{"path":"src/a.tsx"},{"path":"src/b.ts"}],"results":[]}`
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "opengrep", Settings: &executor.ScanSettings{}}
	for _, tc := range []struct {
		json, want         string
		unloaded, unparsed []string
	}{{errs, "", []string{"", "a", "r"}, []string{"src/a.tsx", "src/b.ts"}}, {`{"errors":[]}`, "", nil, nil}, {`{"results":[]}`, "not_sarif", nil, nil}, {"", "not_sarif", nil, nil}} {
		e := &Executor{Opengrep: "registry.example/canyon/opengrep@" + builtDigest}
		c := newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
		out := filepath.Join(c.dir, "opengrep.json")
		if err := os.Remove(out); err != nil {
			t.Fatal(err)
		}
		if tc.json != "" {
			if err := os.WriteFile(out, []byte(tc.json), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.tsx", "src/b.ts"), scanTree(t, "rule.yaml")))
		if err != nil || res.NotFinished != tc.want || (tc.want == "") != (res.SARIF != nil) || !slices.Equal(res.Unloaded, tc.unloaded) || !slices.Equal(res.Unparsed, tc.unparsed) {
			t.Errorf("JSON output %q: %+v %v", tc.json, res, err)
		}
		tool, _ := executor.ToolOf("opengrep")
		if run := c.ran["tool"]; !slices.Equal(run.Command, append([]string{"/scan/busybox", "sh", "-c", executor.ThenJSON, "sh"}, tool.Entrypoint...)) ||
			!slices.Equal(run.Args, tool.Args) || !slices.Contains(run.Args, "--json-output=/tmp/opengrep.json") {
			t.Errorf("the tool's container: %v %v", run.Command, run.Args)
		}
	}
}

// Opengrep takes its rules part, reads no config, and its skip paths leave the tree instead; without an
// image of its own pinned it answers no_image.
func TestOpengrepTakesItsRulesAndLeavesItsSkipPathsOut(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "opengrep", Settings: &executor.ScanSettings{SkipPaths: []string{"docs"}}}
	if res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.js"), scanTree(t, "rule.yaml"))); err != nil || res.NotFinished != "no_image" {
		t.Fatalf("no image: %+v %v", res, err)
	}
	e.Opengrep = "registry.example/canyon/opengrep@" + builtDigest
	res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "src/a.js", "docs/b.md", "docs/sub/c.md", "docsx/d.md"), scanTree(t, "rule.yaml")))
	if err != nil || res.Image != e.Opengrep || res.SARIF == nil {
		t.Fatalf("%+v %v", res, err)
	}
	var names []string
	for n := range c.fed[0] {
		names = append(names, n)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"rules/rule.yaml", "tree/", "tree/docsx/d.md", "tree/src/a.js"}) {
		t.Fatalf("fed %v", names)
	}
}

// A request the contract refuses is 400 before a Job exists or during its feed: settings outside the
// tool's allow-list, a rules part for another tool, a link in the tree. A tool without its database
// answers busy, its tree still checked.
func TestScanRefusesWhatTheContractRefuses(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	link := tarOf(t, &tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	for name, call := range map[string]func() (executor.ScanResult, error){
		"trivy with checkov's frameworks": func() (executor.ScanResult, error) {
			return e.Scan(t.Context(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{Frameworks: []string{"terraform"}}}, scanParts(t, scanTree(t, "a"), nil))
		},
		"rules for checkov": func() (executor.ScanResult, error) {
			return e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "a"), scanTree(t, "rule.yaml")))
		},
		"a link in checkov's tree": func() (executor.ScanResult, error) {
			return e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, link, nil))
		},
		"a link in trivy's tree": func() (executor.ScanResult, error) {
			return e.Scan(t.Context(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}, scanParts(t, link, nil))
		},
	} {
		if res, err := call(); !errors.Is(err, executor.ErrRequest) {
			t.Errorf("%s: %+v %v, want a refusal", name, res, err)
		}
	}
	res, err := e.Scan(t.Context(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}, scanParts(t, scanTree(t, "a"), nil))
	if !errors.Is(err, executor.ErrBusy) {
		t.Fatalf("trivy: %+v %v", res, err)
	}
	if _, err := c.AppsV1().Deployments(ScanNamespace).Get(t.Context(), "canyon-trivy", metav1.GetOptions{}); err != nil {
		t.Fatalf("no scan refused but trivy's made its server: %v", err)
	}
	if jobs, _ := c.BatchV1().Jobs(ScanNamespace).List(t.Context(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatal("a refused scan left its Job")
	}
}

// Scans of a tool with no database run side by side, each its own Job: nothing they share attaches to one
// node (contracts/executor.md, On Kubernetes; #1643).
func TestScansOfAToolWithNoDatabaseRunSideBySide(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	c.hold = make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := e.Scan(context.Background(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil))
			done <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	created := len(jobsNamed(c, "canyon-scan-checkov-"))
	close(c.hold)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if created != 2 {
		t.Fatalf("%d of two checkov scans ran at once", created)
	}
}

// A second scan of a tool with a database, while one runs, answers queued at once — 503 busy with no
// reason — never waiting inside its call, where it would hold one of the work slots /build and /scan
// share (contracts/executor.md, On Kubernetes; #1643).
func TestASecondScanOfAToolWithADatabaseAnswersQueued(t *testing.T) {
	e := &Executor{}
	newScanCluster(t, e, "")
	srv := httptest.NewServer(executor.Handler(e))
	defer srv.Close()                                // after the release below: a scan waiting inside its call would hold Close forever
	if err := scanning["grype"].scan(); err != nil { // a scan in flight
		t.Fatal(err)
	}
	defer scanning["grype"].release(false)
	answered := make(chan error, 1)
	go func() {
		_, err := (&executor.Client{Base: srv.URL}).Scan(context.Background(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}, bytes.NewReader(scanTree(t, "a")), nil)
		answered <- err
	}()
	select {
	case err := <-answered:
		var busy *executor.Busy
		if !errors.As(err, &busy) || busy.Reason != "" || busy.RetryAfter != 10*time.Second {
			t.Fatalf("a second grype scan: %#v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second grype scan waited inside its call")
	}
}

// At start the executor deletes every Job labelled canyon.scan or canyon.scandb a crash left, and nothing else.
func TestSweepScansDeletesWhatACrashLeft(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	for name, labels := range map[string]map[string]string{"canyon-scan-left": {"canyon.scan": "true"}, "canyon-scandb-left": {"canyon.scandb": "true"}, "someone-else": {}} {
		if _, err := c.BatchV1().Jobs(ScanNamespace).Create(t.Context(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ScanNamespace, Labels: labels}}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	e.SweepScans(t.Context())
	jobs, _ := c.BatchV1().Jobs(ScanNamespace).List(t.Context(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "someone-else" {
		t.Fatalf("left %v", jobs.Items)
	}
}

// jobsNamed is every Job created whose name starts with prefix.
func jobsNamed(c *scanCluster, prefix string) []*batchv1.Job {
	var jobs []*batchv1.Job
	for _, a := range c.Actions() {
		if w, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "jobs" && strings.HasPrefix(w.GetObject().(*batchv1.Job).Name, prefix) {
			jobs = append(jobs, w.GetObject().(*batchv1.Job))
		}
	}
	return jobs
}

func hasMount(mounts []corev1.VolumeMount, want corev1.VolumeMount) bool {
	return slices.ContainsFunc(mounts, func(m corev1.VolumeMount) bool { return equality.Semantic.DeepEqual(m, want) })
}

func addClaim(t *testing.T, c *scanCluster, tool string, phase corev1.PersistentVolumeClaimPhase) {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "canyon-scandb-" + tool}, Status: corev1.PersistentVolumeClaimStatus{Phase: phase}}
	if _, err := c.CoreV1().PersistentVolumeClaims(ScanNamespace).Create(t.Context(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A tool with a database runs on it read-only at /scan/db once a reader — a Job like a scan's, the volume
// read-only at /db, its stderr discarded — has said when it was built, which the SARIF answer carries.
// The reader's time comes off the tool's deadline: the scan Job has what a slow reader left.
func TestScanReadsItsDatabaseReadOnlyAndAnswersWhenItWasBuilt(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	c.meta, c.slow = `{"schemaVersion":"v6.1.0","built":"`+built.Format(time.RFC3339)+`"}`, 1500*time.Millisecond
	addClaim(t, c, "grype", corev1.ClaimBound)
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}
	res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "package-lock.json"), nil))
	if err != nil || res.SARIF == nil || res.DBBuiltAt != built.Format(time.RFC3339) || res.NotFinished != "" {
		t.Fatalf("%+v %v", res, err)
	}
	readers, scans := jobsNamed(c, "canyon-scan-read-grype-"), jobsNamed(c, "canyon-scan-grype-")
	if len(readers) != 1 || len(scans) != 1 {
		t.Fatalf("%d readers, %d scans", len(readers), len(scans))
	}
	r, s := readers[0].Spec.Template.Spec, scans[0].Spec.Template.Spec
	ro := func(v corev1.Volume) bool {
		return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "canyon-scandb-grype" && v.PersistentVolumeClaim.ReadOnly
	}
	if readers[0].Spec.Template.Labels["canyon.scan"] != "true" || !slices.Equal(r.Containers[0].Command, stdoutOnly("/grype", "db", "status", "-o", "json")) ||
		!slices.Equal(r.Containers[0].Env, []corev1.EnvVar{{Name: "GRYPE_DB_CACHE_DIR", Value: "/db"}, {Name: "GRYPE_DB_VALIDATE_AGE", Value: "false"}}) ||
		!hasMount(r.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: "/db", ReadOnly: true}) || !slices.ContainsFunc(r.Volumes, ro) ||
		*r.Containers[0].SecurityContext.RunAsUser != 65534 || *r.AutomountServiceAccountToken {
		t.Errorf("the reader %+v", r)
	}
	if !hasMount(s.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: "/scan/db", ReadOnly: true}) || !slices.ContainsFunc(s.Volumes, ro) ||
		len(s.Containers[0].Env) != 0 || *scans[0].Spec.ActiveDeadlineSeconds > 598 || *scans[0].Spec.ActiveDeadlineSeconds < 590 {
		t.Errorf("the scan %+v", s)
	}
	if jobs, _ := c.BatchV1().Jobs(ScanNamespace).List(t.Context(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatal("a Job outlived the answer")
	}
}

// No volume, one never bound, a reader that exits 1 or prints what does not parse is busy while the
// database is downloaded, one built more than 5 days before the scan starts db_stale, and the tool never runs.
func TestScanWithoutAUsableDatabaseRunsNoTool(t *testing.T) {
	fresh := `{"built":"` + time.Now().Add(-time.Hour).Format(time.RFC3339Nano) + `"}`
	for name, tc := range map[string]struct {
		claim corev1.PersistentVolumeClaimPhase
		exit  int32
		meta  string
		want  string
	}{
		"no volume":            {"", 0, fresh, "busy"},
		"a volume never bound": {corev1.ClaimPending, 0, fresh, "busy"},
		"the reader exits 1":   {corev1.ClaimBound, 1, fresh, "busy"},
		"the reader's garbage": {corev1.ClaimBound, 0, "Traceback: …", "busy"},
		"built 6 days ago":     {corev1.ClaimBound, 0, `{"built":"` + time.Now().Add(-6*24*time.Hour).Format(time.RFC3339Nano) + `"}`, "db_stale"},
	} {
		e := &Executor{}
		c := newScanCluster(t, e, checkovSARIF)
		c.exit["reader"], c.meta = tc.exit, tc.meta
		if tc.claim != "" {
			addClaim(t, c, "grype", tc.claim)
		}
		req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}
		res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil))
		e.collecting.Wait() // the download a busy scan started
		if got := cmp.Or(res.NotFinished, fmt.Sprint(err)); got != tc.want || res.SARIF != nil || res.DBBuiltAt != "" || len(jobsNamed(c, "canyon-scan-grype-")) != 0 {
			t.Errorf("%s: %+v %v", name, res, err)
		}
		// the scan's check reads it, and a busy scan's refresh reads it again after (GET /scandb's age, #2743)
		if r := jobsNamed(c, "canyon-scan-read-"); tc.claim == corev1.ClaimBound && (len(r) != map[string]int{"busy": 2, "db_stale": 1}[tc.want] ||
			slices.ContainsFunc(r, func(j *batchv1.Job) bool {
				return !slices.Equal(j.Spec.Template.Spec.Containers[0].Command, stdoutOnly("/grype", "db", "status", "-o", "json"))
			})) {
			t.Errorf("%s: grype's reader %v", name, r)
		}
	}
}

// A tool's first scan finds no database: it starts the tool's one refresh and answers busy with the
// reason database, as a scan while that refresh runs does, and once the database exists a scan runs the
// tool on it.
func TestAToolsFirstScanDownloadsItsDatabaseThenScans(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
	c.meta = `{"built":"` + time.Now().Add(-time.Hour).Format(time.RFC3339Nano) + `"}`
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	for range 2 {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		j, _ := mw.CreateFormField("json")
		_ = json.NewEncoder(j).Encode(req)
		tree, _ := mw.CreateFormFile("tree", "tree.tar")
		_, _ = tree.Write(scanTree(t, "a"))
		_ = mw.Close()
		r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/scan", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 503 || strings.TrimSpace(string(answer)) != `{"error":"busy","reason":"database"}` {
			t.Fatalf("a scan before the database exists: %d %s", res.StatusCode, answer)
		}
	}
	e.collecting.Wait()
	claims := c.CoreV1().PersistentVolumeClaims(ScanNamespace)
	claim, err := claims.Get(t.Context(), "canyon-scandb-grype", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	claim.Status.Phase = corev1.ClaimBound // the refresh's pod bound it
	if _, err := claims.UpdateStatus(t.Context(), claim, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil)); err != nil || res.SARIF == nil {
		t.Fatalf("a scan once the database exists: %+v %v", res, err)
	}
	if refreshes := jobsNamed(c, "canyon-scandb-grype-"); len(refreshes) != 1 || len(jobsNamed(c, "canyon-scan-grype-")) != 1 {
		t.Errorf("%d refreshes, then one scan", len(refreshes))
	}
}

// Scans that find no database — the next taking the tool's lock before the refresh the first started —
// start one refresh.
func TestScansWithoutADatabaseStartOneRefresh(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	_ = scanning["grype"].scan() // the second scan in flight
	e.startRefresh(t.Context(), "grype", grype)
	e.startRefresh(t.Context(), "grype", grype)
	scanning["grype"].release(false)
	if e.collecting.Wait(); len(jobsNamed(c, "canyon-scandb-grype-")) != 1 {
		t.Errorf("%d refreshes", len(jobsNamed(c, "canyon-scandb-grype-")))
	}
}

// A refresh that fails is db_missing to the next scan, once; the scan after that downloads again.
func TestAFailedRefreshIsDBMissingOnceThenDownloadsAgain(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	c.exit["refresh"], c.said = 1, "FATAL failed to download artifact from mirror.example/aquasec/trivy-db:2: no such host"
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}
	var got []string
	for range 3 {
		res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil))
		got = append(got, cmp.Or(res.NotFinished, fmt.Sprint(err)))
		e.collecting.Wait()
	}
	if want := []string{"busy", "db_missing", "busy"}; !slices.Equal(got, want) || len(jobsNamed(c, "canyon-scandb-grype-")) != 2 || len(jobsNamed(c, "canyon-scan-grype-")) != 0 {
		t.Errorf("got %q, want %q; %d refreshes", got, want, len(jobsNamed(c, "canyon-scandb-grype-")))
	}
	if !strings.Contains(logged.String(), "executor: refresh grype database: job canyon-scandb-grype-") || !strings.Contains(logged.String(), c.said) {
		t.Errorf("the failed refresh's log does not say what the tool said:\n%s", logged.String())
	}
}

// Only a tool whose volume exists — one a scan asked for — is refreshed, when the executor starts and
// every DBEvery: with none, nothing is downloaded.
func TestRefreshDBsRefreshesOnlyAToolWithAVolume(t *testing.T) {
	for _, volume := range []bool{false, true} {
		e := &Executor{}
		c := newScanCluster(t, e, "")
		if volume {
			addClaim(t, c, "grype", corev1.ClaimBound)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { e.RefreshDBs(ctx); close(done) }()
		time.Sleep(time.Second)
		cancel()
		<-done
		if grype := len(jobsNamed(c, "canyon-scandb-grype-")); grype != map[bool]int{true: 1}[volume] || len(jobsNamed(c, "canyon-scandb-trivy-")) != 0 {
			t.Errorf("a grype volume %v: %d grype refreshes, %d trivy", volume, grype, len(jobsNamed(c, "canyon-scandb-trivy-")))
		}
	}
}

// A refresh downloads the tool's database into its volume, a 5 Gi ReadWriteOnce claim of the default
// class, from a Job labelled canyon.scandb that reaches DNS and TCP 443 at public addresses alone, as the
// scans' user, the volume writable at /db; the Job is gone once it ends, and one that fails is an error.
func TestRefreshDownloadsTheDatabaseFromAJobReachingDNSAnd443(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	if err := e.refreshDB(t.Context(), "grype", grype); err != nil {
		t.Fatal(err)
	}
	claim, err := c.CoreV1().PersistentVolumeClaims(ScanNamespace).Get(t.Context(), "canyon-scandb-grype", metav1.GetOptions{})
	if err != nil || !slices.Equal(claim.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) || claim.Spec.Resources.Requests.Storage().String() != "5Gi" || claim.Spec.StorageClassName != nil {
		t.Fatalf("the volume %+v %v", claim, err)
	}
	np, err := c.NetworkingV1().NetworkPolicies(ScanNamespace).Get(t.Context(), "canyon-scandb", metav1.GetOptions{})
	if err != nil || np.Spec.PodSelector.MatchLabels["canyon.scandb"] != "true" || len(np.Spec.PolicyTypes) != 2 || len(np.Spec.Ingress) != 0 ||
		!equality.Semantic.DeepEqual(np.Spec.Egress, []networkingv1.NetworkPolicyEgressRule{dnsEgress(), publicEgress(443)}) {
		t.Fatalf("the refresh's policy %+v %v", np, err)
	}
	jobs := jobsNamed(c, "canyon-scandb-grype-")
	if len(jobs) != 1 {
		t.Fatalf("%d refreshes", len(jobs))
	}
	pod := jobs[0].Spec.Template.Spec
	ctr := pod.Containers[0]
	if jobs[0].Spec.Template.Labels["canyon.scandb"] != "true" || jobs[0].Labels["canyon.scan"] != "" || pod.SecurityContext.FSGroup == nil || *pod.SecurityContext.FSGroup != 65534 || *ctr.SecurityContext.RunAsUser != 65534 ||
		!slices.Equal(ctr.Command, []string{"/grype", "db", "update"}) || !slices.Equal(ctr.Env, []corev1.EnvVar{{Name: "GRYPE_DB_CACHE_DIR", Value: "/db"}}) ||
		!hasMount(ctr.VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: "/db"}) || pod.Volumes[0].PersistentVolumeClaim.ReadOnly || *jobs[0].Spec.ActiveDeadlineSeconds != 600 {
		t.Fatalf("the refresh %+v", jobs[0].Spec)
	}
	if jobs, _ := c.BatchV1().Jobs(ScanNamespace).List(t.Context(), metav1.ListOptions{}); len(jobs.Items) != 0 {
		t.Fatal("the refresh's Job outlived it")
	}
	c.exit["refresh"] = 1
	if err := e.refreshDB(t.Context(), "grype", grype); err == nil {
		t.Fatal("a refresh that failed answered nil")
	}
}

// A grype refresh Job cut short leaves its download, grype-db-download<N>, in the claim beside the live
// database (#2527): each refresh Job first sweeps it, an init container on the scans' busybox mounting the
// claim, before grype runs. A sweep that fails fails the Job before grype runs, the database as it was.
func TestAGrypeRefreshJobSweepsWhatOneCutShortLeft(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	if err := e.refreshDB(t.Context(), "grype", grype); err != nil {
		t.Fatal(err)
	}
	jobs := jobsNamed(c, "canyon-scandb-grype-")
	if len(jobs) != 1 {
		t.Fatalf("%d refreshes", len(jobs))
	}
	if inits := jobs[0].Spec.Template.Spec.InitContainers; len(inits) != 1 {
		t.Fatalf("the refresh's init containers %+v: want the sweep alone", inits)
	}
	sweep := jobs[0].Spec.Template.Spec.InitContainers[0]
	if sweep.Name != "sweep" || sweep.Image != scanBusybox || !slices.Equal(sweep.Command, []string{"sh", "-c", "rm -rf /db/grype-db-download*"}) ||
		len(sweep.VolumeMounts) != 1 || !hasMount(sweep.VolumeMounts, corev1.VolumeMount{Name: "db", MountPath: "/db"}) || *sweep.SecurityContext.RunAsUser != 65534 || !*sweep.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("the sweep %+v", sweep)
	}
	delete(c.ran, "refresh")
	c.exit["sweep"] = 1
	err := e.refreshDB(t.Context(), "grype", grype)
	if _, ran := c.ran["refresh"]; err == nil || ran || !strings.Contains(err.Error(), "sweep") {
		t.Errorf("a sweep that failed: %v, grype ran %t; want the refresh failed, naming the sweep, before grype ran", err, ran)
	}
}

// One lock per tool: a refresh waits for a scan in flight, and from the moment it waits every scan of
// the tool answers busy at once — one already waiting behind the scan in flight, and one that comes after.
func TestScanDuringARefreshAnswersBusy(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	grype, _ := executor.ToolOf("grype")
	scan := func() chan error {
		answered := make(chan error, 1)
		go func() {
			req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "grype", Settings: &executor.ScanSettings{}}
			_, err := e.Scan(context.Background(), req, scanParts(t, scanTree(t, "a"), nil))
			answered <- err
		}()
		return answered
	}
	_ = scanning["grype"].scan()                                                   // a scan in flight
	for want, answered := range map[error]chan error{executor.ErrQueued: scan()} { // behind a scan, before any refresh: queued
		if err := <-answered; !errors.Is(err, want) {
			t.Errorf("a scan behind a scan: %v", err)
		}
	}
	refreshed := make(chan error, 1)
	go func() { refreshed <- e.refreshDB(context.Background(), "grype", grype) }()
	if time.Sleep(100 * time.Millisecond); len(jobsNamed(c, "canyon-scandb-")) != 0 {
		t.Error("a refresh ran during a scan")
	}
	select { // a refresh waits for the lock: the database is what the next scan waits on
	case err := <-scan():
		if !errors.Is(err, executor.ErrBusy) {
			t.Errorf("the scan after the refresh asked: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("the scan after the refresh asked waited behind it")
	}
	scanning["grype"].release(false)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
}

// A scan policy an earlier release wrote is rewritten to this release's rules before a pod relies on it.
func TestTheScanPoliciesAreRewrittenNotKept(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, checkovSARIF)
	nps := c.NetworkingV1().NetworkPolicies(ScanNamespace)
	for name, label := range map[string]string{"canyon-scan": "canyon.scan", "canyon-scandb": "canyon.scandb"} {
		np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{label: "true"}}, Egress: []networkingv1.NetworkPolicyEgressRule{{}}, // lets everything out
		}}
		if _, err := nps.Create(t.Context(), np, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	grype, _ := executor.ToolOf("grype")
	if _, err := e.Scan(t.Context(), checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil)); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshDB(t.Context(), "grype", grype); err != nil {
		t.Fatal(err)
	}
	scan, _ := nps.Get(t.Context(), "canyon-scan", metav1.GetOptions{})
	db, _ := nps.Get(t.Context(), "canyon-scandb", metav1.GetOptions{})
	if len(scan.Spec.Egress) != 0 || len(scan.Spec.PolicyTypes) != 2 || !equality.Semantic.DeepEqual(db.Spec.Egress, []networkingv1.NetworkPolicyEgressRule{dnsEgress(), publicEgress(443)}) {
		t.Fatalf("kept: %+v, %+v", scan.Spec, db.Spec)
	}
}

// On Kubernetes GET /scandb reads trivy's server's database age from its GET /version, never over pods/exec, and
// the claims' as the executor last read them (contracts/executor.md, GET /scandb; #2743): a server that holds no
// database, or one a claim the executor has not read, answers none.
func TestScannerDataAgeOnKubernetes(t *testing.T) {
	for _, l := range scanning {
		l.read.Store(nil)
		t.Cleanup(func() { l.read.Store(nil) })
	}
	built := time.Now().Add(-26 * time.Hour).UTC().Truncate(time.Second)
	grype := built.Add(-time.Hour)
	scanning["grype"].read.Store(&grype)
	e := &Executor{Trivy: trivyVersion(t, `{"Version":"0.74.0","VulnerabilityDB":{"Version":2,"UpdatedAt":"`+built.Format(time.RFC3339Nano)+`"}}`)}
	dbs, err := e.ScanDBs(t.Context())
	want := []executor.ScanDB{executor.ScanDBOf("grype", grype), executor.ScanDBOf("trivy", built), {Tool: executor.JavaDB}}
	if err != nil || !slices.Equal(dbs, want) || dbs[1].StaleAt != built.Add(5*24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("ScanDBs: %+v %v, want %+v", dbs, err, want)
	}
	e.Trivy = trivyVersion(t, `{"Version":"0.74.0"}`) // a server still downloading its first database
	if dbs, _ := e.ScanDBs(t.Context()); dbs[1] != (executor.ScanDB{Tool: "trivy"}) {
		t.Fatalf("a server with no database: %+v", dbs[1])
	}
	// a server not there yet says nothing, as one not ready does; a real failure is logged
	var logged bytes.Buffer
	log.SetOutput(&logged)
	defer log.SetOutput(os.Stderr)
	for err, said := range map[error]bool{
		&net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}:      false,
		&net.DNSError{Err: "no such host", Name: "canyon-trivy.canyon-scans", IsNotFound: true}: false,
		errors.New("tls: handshake failure"):                                                    true,
	} {
		logged.Reset()
		e.Trivy = roundTrip(func(*http.Request) (*http.Response, error) { return nil, err })
		if dbs, _ := e.ScanDBs(t.Context()); dbs[1] != (executor.ScanDB{Tool: "trivy"}) || (logged.Len() > 0) != said {
			t.Errorf("%v: %+v, logged %q", err, dbs[1], logged.String())
		}
	}
}

// trivyVersion is trivy's server answering GET /version with body, as trivy 0.74.0 does (pkg/rpc/server); any other
// request is an error.
func trivyVersion(t *testing.T, body string) http.RoundTripper {
	return roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != trivyURL+"/version" {
			t.Errorf("asked trivy's server %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
}

// serveTrivy is trivy's server up: made as a scan would make it, with a ready pod whose database was built
// at built (contracts/executor.md, On Kubernetes; #1652).
func serveTrivy(t *testing.T, c *scanCluster, e *Executor, built time.Time) {
	t.Helper()
	trivy, _ := executor.ToolOf("trivy")
	if _, err := e.trivyServer(t.Context(), trivy.Image); !errors.Is(err, executor.ErrBusy) { // made: no pod yet
		t.Fatalf("the server's making: %v", err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-trivy-0", Namespace: ScanNamespace, Labels: map[string]string{trivyLabel: "true"}},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
	if _, err := c.CoreV1().Pods(ScanNamespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.Trivy = trivyVersion(t, `{"Version":"0.74.0","VulnerabilityDB":{"Version":2,"UpdatedAt":"`+built.UTC().Format(time.RFC3339Nano)+`"}}`)
}

// trivy's scans are clients of one server, side by side on any node: each a Job labelled canyon.scan-trivy
// with no volume and the server's address, the server one Deployment of the scan's image as the scan's
// user, its Service on 4954, and the policies letting only trivy's scan pods in to it and them out to DNS
// and it alone; the answer carries when the server's database was built (contracts/executor.md, On
// Kubernetes; #1652).
func TestTrivyScansAreClientsOfOneServerSideBySide(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, `{"version":"2.1.0","runs":[{"results":[]}]}`)
	built := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	serveTrivy(t, c, e, built)
	c.hold = make(chan struct{})
	answered := make(chan executor.ScanResult, 2)
	for range 2 {
		go func() {
			res, err := e.Scan(context.Background(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}, scanParts(t, scanTree(t, "package-lock.json"), nil))
			if err != nil {
				t.Error(err)
			}
			answered <- res
		}()
	}
	time.Sleep(300 * time.Millisecond)
	scans := jobsNamed(c, "canyon-scan-trivy-")
	close(c.hold)
	for range 2 {
		if res := <-answered; res.DBBuiltAt != built.Format(time.RFC3339) || res.SARIF == nil {
			t.Fatalf("a trivy scan: %+v", res)
		}
	}
	if len(scans) != 2 {
		t.Fatalf("%d of two trivy scans ran at once", len(scans))
	}
	s := scans[0].Spec.Template
	args := s.Spec.Containers[0].Args
	if s.Labels[clientLabel] != "true" || slices.ContainsFunc(s.Spec.Volumes, func(v corev1.Volume) bool { return v.PersistentVolumeClaim != nil }) ||
		args[len(args)-1] != "/scan/tree" || !slices.Contains(args, trivyURL) || args[slices.Index(args, "--cache-dir")+1] != "/tmp/trivy" {
		t.Errorf("the client's Job: labels %v, args %v, volumes %v", s.Labels, args, s.Spec.Volumes)
	}
	trivy, _ := executor.ToolOf("trivy")
	d, err := c.AppsV1().Deployments(ScanNamespace).Get(t.Context(), trivyName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ct := d.Spec.Template.Spec.Containers[0]
	if *d.Spec.Replicas != 1 || ct.Image != trivy.Image || !slices.Equal(ct.Args[:5], []string{"server", "--listen", "0.0.0.0:4954", "--cache-dir", "/db"}) ||
		*ct.SecurityContext.RunAsUser != 65534 || !*ct.SecurityContext.ReadOnlyRootFilesystem || *d.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Errorf("the server: %+v", d.Spec.Template.Spec)
	}
	if svc, err := c.CoreV1().Services(ScanNamespace).Get(t.Context(), trivyName, metav1.GetOptions{}); err != nil || svc.Spec.Ports[0].Port != 4954 || svc.Spec.Selector[trivyLabel] != "true" {
		t.Errorf("the server's Service: %+v %v", svc, err)
	}
	server, _ := c.NetworkingV1().NetworkPolicies(ScanNamespace).Get(t.Context(), trivyName, metav1.GetOptions{})
	clients, _ := c.NetworkingV1().NetworkPolicies(ScanNamespace).Get(t.Context(), "canyon-scan-trivy", metav1.GetOptions{})
	executorIn := server.Spec.Ingress[len(server.Spec.Ingress)-1] // the executor, asking /version how old the database is (#2743)
	if len(server.Spec.Ingress) != 2 || server.Spec.Ingress[0].From[0].PodSelector.MatchLabels[clientLabel] != "true" || !equality.Semantic.DeepEqual(server.Spec.Egress, e.refreshEgress()) ||
		executorIn.From[0].PodSelector.MatchLabels["app.kubernetes.io/name"] != "canyon-executor" || executorIn.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "canyon" || executorIn.Ports[0].Port.IntVal != trivyPort ||
		len(clients.Spec.Ingress) != 0 || len(clients.Spec.Egress) != 2 || clients.Spec.Egress[1].To[0].PodSelector.MatchLabels[trivyLabel] != "true" {
		t.Errorf("the policies: server %+v, clients %+v", server.Spec, clients.Spec)
	}
}

// The server follows this executor's image and database source: a Deployment of another image is updated
// to it, and the scan answers busy while it rolls (contracts/executor.md, On Kubernetes; #1652).
func TestTrivysServerFollowsTheExecutorsImage(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	trivy, _ := executor.ToolOf("trivy")
	old := trivyDeployment("registry.example/trivy@sha256:"+strings.Repeat("0", 64), "", time.Now().Add(-time.Hour))
	if _, err := c.AppsV1().Deployments(ScanNamespace).Create(t.Context(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}
	if _, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil)); !errors.Is(err, executor.ErrBusy) {
		t.Fatalf("a scan while the server is updated: %v", err)
	}
	if d, _ := c.AppsV1().Deployments(ScanNamespace).Get(t.Context(), trivyName, metav1.GetOptions{}); d.Spec.Template.Spec.Containers[0].Image != trivy.Image || d.Annotations[specAt] != trivyDeployment(trivy.Image, "", time.Now()).Annotations[specAt] {
		t.Fatalf("the server runs %s", d.Spec.Template.Spec.Containers[0].Image)
	}
}

// A server with no ready pod 15 minutes after it was made cannot check (db_missing); one whose database
// was built more than 5 days ago is db_stale; neither runs a scan (contracts/executor.md; #1652).
func TestTrivysServerNotReadyOrStaleRunsNoScan(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	trivy, _ := executor.ToolOf("trivy")
	if _, err := c.AppsV1().Deployments(ScanNamespace).Create(t.Context(), trivyDeployment(trivy.Image, "", time.Now().Add(-16*time.Minute)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	stuck := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-trivy-0", Namespace: ScanNamespace, Labels: map[string]string{trivyLabel: "true"}, CreationTimestamp: metav1.NewTime(time.Now().Add(-16 * time.Minute))}}
	if _, err := c.CoreV1().Pods(ScanNamespace).Create(t.Context(), stuck, metav1.CreateOptions{}); err != nil { // never ready since it was made
		t.Fatal(err)
	}
	req := executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}
	if res, err := e.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil)); err != nil || res.NotFinished != "db_missing" {
		t.Fatalf("no ready server past its making: %+v %v", res, err)
	}
	e2 := &Executor{}
	c2 := newScanCluster(t, e2, "")
	serveTrivy(t, c2, e2, time.Now().Add(-6*24*time.Hour))
	if res, err := e2.Scan(t.Context(), req, scanParts(t, scanTree(t, "a"), nil)); err != nil || res.NotFinished != "db_stale" {
		t.Fatalf("a database 6 days old: %+v %v", res, err)
	}
	if len(jobsNamed(c, "canyon-scan-trivy-"))+len(jobsNamed(c2, "canyon-scan-trivy-")) != 0 {
		t.Fatal("a scan ran")
	}
}

// A client whose server went away under it — no SARIF, and no ready server now — answers busy with the
// reason database, never a scan that did not finish (contracts/executor.md, On Kubernetes; #1652).
func TestATrivyScanCutByItsServerRestartingIsBusy(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "FATAL: connection refused")
	serveTrivy(t, c, e, time.Now().Add(-time.Hour))
	c.hold = make(chan struct{})
	answered := make(chan error, 1)
	go func() {
		_, err := e.Scan(context.Background(), executor.ScanRequest{App: "claims-portal", Package: "pkg_7d3e8f", Tool: "trivy", Settings: &executor.ScanSettings{}}, scanParts(t, scanTree(t, "a"), nil))
		answered <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := c.CoreV1().Pods(ScanNamespace).Delete(t.Context(), "canyon-trivy-0", metav1.DeleteOptions{}); err != nil { // the server's node goes under the scan
		t.Fatal(err)
	}
	if d, err := c.AppsV1().Deployments(ScanNamespace).Get(t.Context(), trivyName, metav1.GetOptions{}); err == nil { // made long ago: only the new pod's own clock runs
		d.Annotations[madeAt] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		_, _ = c.AppsV1().Deployments(ScanNamespace).Update(t.Context(), d, metav1.UpdateOptions{})
	}
	replaced := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "canyon-trivy-1", Namespace: ScanNamespace, Labels: map[string]string{trivyLabel: "true"}, CreationTimestamp: metav1.Now()}}
	if _, err := c.CoreV1().Pods(ScanNamespace).Create(t.Context(), replaced, metav1.CreateOptions{}); err != nil { // its replacement, downloading
		t.Fatal(err)
	}
	close(c.hold)
	if err := <-answered; !errors.Is(err, executor.ErrBusy) {
		t.Fatalf("a scan cut by its server: %v", err)
	}
}

// A scan whose pod the cluster has no room to schedule answers no_room at the Job's deadline, with the
// scheduler's own message from the pod's PodScheduled condition, quoted: capacity, never an error the
// server reads as Canyon failing (contracts/executor.md, /scan; #1637: the test cluster's full nodes made it 502).
func TestAScanTheClusterHasNoRoomForSaysSo(t *testing.T) {
	e := &Executor{}
	c := newScanCluster(t, e, "")
	const said = "0/4 nodes are available: 4 Insufficient cpu. preemption: 0/4 nodes are available: 4 No preemption victims found for incoming pod."
	pods, jobs := corev1.SchemeGroupVersion.WithResource("pods"), batchv1.SchemeGroupVersion.WithResource("jobs")
	c.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
		j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: ScanNamespace, Labels: j.Spec.Template.Labels}}
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: said}}
		if err := c.Tracker().Create(pods, p, ScanNamespace); err != nil {
			return true, nil, err
		}
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded}}
		return true, j, c.Tracker().Create(jobs, j, ScanNamespace)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second) // main waited for a feed that never ran, to this
	defer cancel()
	res, err := e.Scan(ctx, checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil))
	if err != nil || res.NotFinished != "no_room" || res.Reason != said || res.SARIF != nil {
		t.Fatalf("%+v %v", res, err)
	}
}

// A pod placed after it waited for room, whose feed then did not start by the Job's deadline, timed out:
// a stale "no room" never sends an admin for nodes (#1701's review). A Job that failed otherwise — its pod
// evicted before the feed ran — is an error naming why, never a wait to the server's ceiling.
func TestAScanPlacedLateTimesOutAndAnEvictedOneSaysWhy(t *testing.T) {
	for want, tc := range map[string]struct {
		scheduled corev1.ConditionStatus
		reason    string
	}{
		"timeout":                   {corev1.ConditionTrue, batchv1.JobReasonDeadlineExceeded},
		"failed before its pod ran": {corev1.ConditionFalse, batchv1.JobReasonBackoffLimitExceeded},
	} {
		e := &Executor{}
		c := newScanCluster(t, e, "")
		pods, jobs := corev1.SchemeGroupVersion.WithResource("pods"), batchv1.SchemeGroupVersion.WithResource("jobs")
		c.PrependReactor("create", "jobs", func(a k8stesting.Action) (bool, runtime.Object, error) {
			j := a.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
			p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-pod", Namespace: ScanNamespace, Labels: j.Spec.Template.Labels}}
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: tc.scheduled, Reason: corev1.PodReasonUnschedulable, Message: "0/4 nodes are available: 4 Insufficient cpu."}}
			if tc.scheduled == corev1.ConditionTrue {
				p.Status.Conditions[0].Reason, p.Status.Conditions[0].Message = "", ""
			}
			if err := c.Tracker().Create(pods, p, ScanNamespace); err != nil {
				return true, nil, err
			}
			j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: tc.reason}}
			return true, j, c.Tracker().Create(jobs, j, ScanNamespace)
		})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		res, err := e.Scan(ctx, checkov(executor.ScanSettings{}), scanParts(t, scanTree(t, "main.tf"), nil))
		cancel()
		switch want {
		case "timeout":
			if err != nil || res.NotFinished != "timeout" || res.Reason != "" {
				t.Errorf("placed late: %+v %v", res, err)
			}
		default:
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tc.reason) {
				t.Errorf("evicted: %+v %v", res, err)
			}
		}
	}
}
