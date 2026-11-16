//go:build handoffrepo

// The hand-off journey through a deploy repo (design/HAND-OFF.md §3b; #2470): Canyon under run_by: org and
// delivery: repo, committing each release to a Gitea repository that Argo CD or Flux (HANDOFF_GITOPS, #2479) syncs
// into the cluster, External Secrets fetching the access token and CloudNativePG running the app's database, from the
// templates in deploy/gitops. The platform team's set-up is deploy/gitops applied as it is in this tree. README.md says
// how to run it.
package handoff

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getcanyon-ai/canyon/internal/executortest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// giteaImage is the org's git host, pinned: Gitea's rootless image, a multi-arch index.
const giteaImage = "docker.io/gitea/gitea:1.27.3-rootless@sha256:1c17ecaead42eb3b5391553d8708103a4beb0e86edf5b9ebc1eb269c318845f2"

// owner is the Gitea user the deploy repo belongs to.
const owner = "platform"

// syncs is what differs between the GitOps controllers the journey runs, HANDOFF_GITOPS's argocd (the default) and
// flux: the reference files the platform team applies, in order, and what platformSide prints when the journey fails.
// Every assertion is the same under both.
var syncs = map[string]struct {
	files       []string
	diagnostics [][]string
}{
	"argocd": {
		files: []string{"admission/services.yaml", "argocd/appprojects.yaml", "argocd/repository.yaml", "external-secrets/clustersecretstore.yaml", "argocd/namespaces.yaml", "argocd/applicationsets.yaml"},
		diagnostics: [][]string{
			{"-n", "argocd", "get", "applicationsets,applications", "-o", "yaml"},
			{"-n", "argocd", "logs", "deploy/argocd-applicationset-controller", "--tail=60"},
			{"-n", "argocd", "logs", "statefulset/argocd-application-controller", "--tail=60"},
		},
	},
	"flux": {
		files: []string{"admission/services.yaml", "external-secrets/clustersecretstore.yaml", "flux/namespaces.yaml", "flux/rbac.yaml", "flux/admission.yaml",
			"flux/repository.yaml", "flux/sources.yaml", "flux/generator.yaml"},
		diagnostics: [][]string{
			{"-n", "flux-system", "get", "gitrepositories,artifactgenerators,externalartifacts,helmreleases", "-o", "yaml"},
			{"-n", "canyon-sync", "get", "gitrepositories,kustomizations", "-o", "yaml"},
			{"-n", "flux-system", "logs", "deploy/source-watcher", "--tail=60"},
			{"-n", "flux-system", "logs", "deploy/helm-controller", "--tail=60"},
			{"-n", "flux-system", "logs", "deploy/kustomize-controller", "--tail=60"},
		},
	},
}

func TestHandOffRepo(t *testing.T) {
	dbURL := os.Getenv("HANDOFF_DATABASE_URL")
	if dbURL == "" {
		t.Fatal("HANDOFF_DATABASE_URL is Canyon's Postgres for the journey (README.md)")
	}
	gitops := or(os.Getenv("HANDOFF_GITOPS"), "argocd")
	if _, ok := syncs[gitops]; !ok {
		t.Fatalf("HANDOFF_GITOPS is %q: argocd or flux", gitops)
	}
	hostIP := or(os.Getenv("HANDOFF_HOST_IP"), "172.30.0.1")
	root, err := filepath.Abs("../..")
	must(t, err)
	work := t.TempDir()
	kube := clientOf(t)
	image := pushApp(t, root, work, or(os.Getenv("HANDOFF_REGISTRY_PUSH"), "localhost:5001"), or(os.Getenv("HANDOFF_REGISTRY"), "172.30.0.200:5000"))
	caFile, leaf := authority(t, work, hostIP)
	ca, err := os.ReadFile(caFile) //nolint:gosec // the file authority wrote
	must(t, err)
	bin := build(t, root, work, "./cmd/canyon-server", "./cmd/canyon")
	t.Cleanup(func() { platformSide(t, gitops) })

	idp := newIDP(t, leaf, hostIP)
	executor := executortest.New(t)
	executor.Image = image
	public, _, toCanyon := front(t, leaf, hostIP, "/access/")
	toCanyon(serve(t, bin["canyon-server"], work, caFile, "CANYON_DATABASE_URL="+dbURL, "CANYON_PUBLIC_URL="+public, "CANYON_EXECUTOR_URL="+executor.URL,
		"CANYON_IMAGE_IDENTITY="+identityImage(t, kube), "CANYON_OIDC_ISSUER="+idp.issuer))

	// The org's git host, and its deploy repo: the reference templates under apps/.canyon on main, with this
	// journey's domain, and preview branched from it.
	forge := startGitea(t, leaf, hostIP, caFile)
	write, read := forge.token(t, "canyon", "write:repository"), forge.token(t, gitops, "read:repository")
	forge.call(t, http.MethodPost, "/user/repos", map[string]any{"name": "deploy", "auto_init": true, "default_branch": "main", "private": true}, http.StatusCreated, nil)
	var files []map[string]string
	for _, name := range []string{"route.yaml", "resources/postgres.yaml"} {
		raw, err := os.ReadFile(filepath.Join(root, "deploy/gitops/templates/.canyon", name)) //nolint:gosec // this tree's templates
		must(t, err)
		files = append(files, map[string]string{"operation": "create", "path": "apps/.canyon/" + name,
			"content": base64.StdEncoding.EncodeToString(bytes.ReplaceAll(raw, []byte("apps.example.com"), []byte(domain)))})
	}
	forge.call(t, http.MethodPost, "/repos/"+owner+"/deploy/contents", map[string]any{"branch": "main", "message": "The platform team's templates", "files": files}, http.StatusCreated, nil)
	forge.call(t, http.MethodPost, "/repos/"+owner+"/deploy/branches", map[string]string{"new_branch_name": "preview", "old_branch_name": "main"}, http.StatusCreated, nil)
	repoURL := forge.url + "/" + owner + "/deploy.git"
	// The platform's own repo, which Canyon's token cannot write: the namespaces' chart, deploy/gitops/namespaces, every
	// file of it. Public, so Argo CD reads it with no credential of its own; Flux reads it with the read token.
	forge.call(t, http.MethodPost, "/user/repos", map[string]any{"name": "platform", "auto_init": true, "default_branch": "main"}, http.StatusCreated, nil)
	files = nil
	chart := filepath.Join(root, "deploy/gitops/namespaces")
	must(t, filepath.WalkDir(chart, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path) //nolint:gosec // this tree's chart
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(chart, path)
		files = append(files, map[string]string{"operation": "create", "path": "namespaces/" + filepath.ToSlash(rel), "content": base64.StdEncoding.EncodeToString(raw)})
		return err
	}))
	forge.call(t, http.MethodPost, "/repos/"+owner+"/platform/contents", map[string]any{"branch": "main", "message": "The namespaces' chart", "files": files}, http.StatusCreated, nil)

	// The admin, in Settings: the deploy repo saved and checked, the two values it shows once stored, the front door
	// at choice 1 (R1); then apps handed to the platform (R2).
	browser := signIn(t, public, caFile)
	api(t, browser, http.MethodPut, public+"/api/v1/preferences", map[string]any{"onboarded_at": time.Now().UTC().Format(time.RFC3339)}, http.StatusOK, nil)
	stored := filepath.Join(t.TempDir(), "values.json")
	settings(t, "R1", public, browser, "HANDOFF_REPO_URL="+repoURL, "HANDOFF_REPO_USER="+owner, "HANDOFF_REPO_TOKEN="+write, "HANDOFF_API_URL="+forge.url+"/api/v1",
		"HANDOFF_CA="+string(ca), "HANDOFF_ISSUER="+idp.issuer, "HANDOFF_AUDIENCE="+audience, "HANDOFF_DOMAIN="+domain, "HANDOFF_VALUES="+stored)
	var values struct{ Fetch, Token string }
	raw, err := os.ReadFile(stored) //nolint:gosec // the file the admin's browser wrote, under t.TempDir
	must(t, err)
	must(t, json.Unmarshal(raw, &values))
	if values.Fetch == "" {
		t.Fatal("the dialog showed no fetch-only token")
	}

	// The platform team, once: deploy/gitops applied as it is here, the fetch-only token in the Secret the store
	// reads, the sync's read-only token, and the CA every piece of the journey is served under: Argo CD's certificates
	// ConfigMap, or ca.crt in each of Flux's read Secrets.
	_, err = kube.CoreV1().Secrets("external-secrets").Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "canyon-fetch-token", Labels: map[string]string{"external-secrets.io/type": "webhook"}},
		StringData: map[string]string{"token": values.Fetch}}, metav1.CreateOptions{})
	must(t, err)
	if gitops == "argocd" {
		certs, err := kube.CoreV1().ConfigMaps("argocd").Get(t.Context(), "argocd-tls-certs-cm", metav1.GetOptions{})
		must(t, err)
		certs.Data = map[string]string{hostIP: string(ca)}
		_, err = kube.CoreV1().ConfigMaps("argocd").Update(t.Context(), certs, metav1.UpdateOptions{})
		must(t, err)
	}
	fill := strings.NewReplacer("${CANYON_URL}", public, "${REPO_URL}", repoURL, "${ROOT}", "apps", "${APP_SEGMENT}", "1", "${PREVIEW_BRANCH}", "preview",
		"${PROD_BRANCH}", "main", "${STORE}", "canyon", "${REQUEUE_SECONDS}", "10", "${READ_USER}", owner, "${READ_TOKEN}", read,
		"${PLATFORM_REPO_URL}", forge.url+"/"+owner+"/platform.git", "${PLATFORM_REVISION}", "main", "${PLATFORM_PATH}", "namespaces",
		"${PLATFORM_READ_USER}", owner, "${PLATFORM_READ_TOKEN}", read, "${INTERVAL}", "15s",
		"${CA_BUNDLE}", "      caBundle: "+base64.StdEncoding.EncodeToString(ca))
	for _, name := range syncs[gitops].files {
		raw, err := os.ReadFile(filepath.Join(root, "deploy/gitops", name)) //nolint:gosec // this tree's reference set-up
		must(t, err)
		apply(t, fill.Replace(string(raw)))
		if name == "flux/repository.yaml" {
			for _, s := range [][2]string{{"canyon-sync", "canyon-deploy-repo"}, {"flux-system", "canyon-deploy-repo"}, {"flux-system", "canyon-platform-repo"}} {
				secret, err := kube.CoreV1().Secrets(s[0]).Get(t.Context(), s[1], metav1.GetOptions{})
				must(t, err)
				secret.Data["ca.crt"] = ca
				_, err = kube.CoreV1().Secrets(s[0]).Update(t.Context(), secret, metav1.UpdateOptions{})
				must(t, err)
			}
		}
	}

	settings(t, "R2", public, browser)
	var prefs struct {
		RunBy    string `json:"run_by"`
		Delivery string `json:"delivery"`
	}
	if api(t, browser, http.MethodGet, public+"/api/v1/preferences", nil, http.StatusOK, &prefs); prefs.RunBy != "org" || prefs.Delivery != "repo" {
		t.Fatalf("after the set-up run_by is %q and delivery %q, not org and repo", prefs.RunBy, prefs.Delivery)
	}

	source, home := appSource(t, work), t.TempDir()
	cli := func(args ...string) *exec.Cmd {
		return command(t, bin["canyon"], source, home, public, caFile, args...)
	}
	login(t, cli("login", public), browser, public)

	// 1. canyon deploy commits the release to preview, the sync applies it, External Secrets fetches its access token
	//    and CloudNativePG its database, and the pod's check-in makes it live. The namespace is the platform repo's:
	//    the app's first sync retries until it is there.
	if out, code := run(t, cli("deploy", "--create")); code != 0 || !strings.Contains(out, "a pod checked in") {
		t.Fatalf("canyon deploy --create under delivery: repo ended %d, not live by a pod's check-in", code)
	}
	preview := "canyon-" + app + "-preview"
	target, token := portForward(t, preview, "svc/"+app+"-preview", "4180"), idp.sign(t, alice, audience, "")
	if _, code := run(t, cli("share", alice)); code != 0 {
		t.Fatalf("canyon share ended %d", code)
	}
	if !within(90*time.Second, func() bool {
		code, body := open(target, token)
		return code == http.StatusOK && strings.Contains(body, "handoff")
	}) {
		t.Fatal("the share did not reach the app in 90 seconds")
	}
	if code, body := ask(http.MethodPost, target+"/notes?v="+rand.Text(), token); code != http.StatusNoContent {
		t.Fatalf("the app did not keep a note in the database CloudNativePG runs: %d %s", code, body)
	}
	settings(t, "H3", public, browser, "HANDOFF_APP="+app)
	// The app's own Service went live past the admission policy; one that reaches past the namespace never does.
	servicesRefused(t, kube, preview)
	envLabelled(t, kube, preview, "preview")

	writersProbe(t, kube, forge, gitops, hostIP, image, func() bool { code, _ := open(target, token); return code == http.StatusOK })

	// 2. canyon deploy --env prod opens a pull request to main; merged in Gitea, the sync applies it and a pod's
	//    check-in makes production live.
	promote := cli("deploy", "--env", "prod", "--yes")
	printed := &bytes.Buffer{}
	promote.Stdout, promote.Stderr = printed, printed
	must(t, promote.Start())
	pr := forge.pullRequest(t)
	if !strings.HasPrefix(pr.Head.Ref, "canyon/"+app+"/prod/") || pr.Base.Ref != "main" {
		t.Fatalf("the pull request is %s into %s, not Canyon's branch for prod into main", pr.Head.Ref, pr.Base.Ref)
	}
	forge.call(t, http.MethodPost, "/repos/"+owner+"/deploy/pulls/"+strconv.Itoa(pr.Number)+"/merge", map[string]string{"Do": "merge"}, http.StatusOK, nil)
	err = promote.Wait()
	t.Logf("deploy --env prod --yes → %v\n%s", err, printed)
	if err != nil || !strings.Contains(printed.String(), "a pod checked in") {
		t.Fatal("canyon deploy --env prod did not end live by a pod's check-in once the pull request was merged")
	}
	envLabelled(t, kube, "canyon-"+app+"-prod", "prod")

	// 3. canyon undeploy commits preview's objects away; the sync prunes the app's Deployment.
	if _, code := run(t, cli("undeploy", "--env", "preview")); code != 0 {
		t.Fatalf("canyon undeploy ended %d", code)
	}
	if !within(3*time.Minute, func() bool { return gone(t, kube, preview) }) {
		t.Fatal("three minutes after the undeploy the sync had not pruned the preview Deployment")
	}
	if left := dataIn(t, kube, preview); left != "the namespace, the database's Cluster and its claim" { // canyon.json and resources/ stay: so does the data
		t.Fatalf("after the undeploy %s has %s, want the namespace, the database's Cluster and its claim", preview, left)
	}

	// 4. canyon delete commits both directories away, production's through a pull request merged in Gitea, and
	//    finishes once it is merged; the sync removes production's app. It is each environment's last app, so this is
	//    also the namespaces generator removing an environment's last namespace.
	if !within(5*time.Minute, func() bool {
		out, code := run(t, cli("delete", "--confirm", app))
		if code == 0 && strings.Contains(out, "Deleted "+app) {
			return true
		}
		forge.mergeOpen(t)
		return false
	}) {
		t.Fatal("canyon delete never finished: its pull request was merged, and the removes never ended removed")
	}
	if !within(3*time.Minute, func() bool { return gone(t, kube, "canyon-"+app+"-prod") }) {
		t.Fatal("three minutes after the delete the sync had not removed production's Deployment")
	}
	// The directories are gone, so the namespaces generator drops them, and each namespace with everything in it: the
	// database's Cluster and its claim, the data with them.
	for _, ns := range []string{preview, "canyon-" + app + "-prod"} {
		if !within(5*time.Minute, func() bool { return dataIn(t, kube, ns) == "" }) {
			t.Fatalf("five minutes after the delete %s still has %s", ns, dataIn(t, kube, ns))
		}
	}
}

// dataIn says what of an app environment's data namespace has: the namespace, its database's CloudNativePG Cluster
// and the claim it keeps the data on, "" for none of them.
func dataIn(t *testing.T, kube kubernetes.Interface, namespace string) string {
	t.Helper()
	var have []string
	if _, err := kube.CoreV1().Namespaces().Get(t.Context(), namespace, metav1.GetOptions{}); err == nil {
		have = append(have, "the namespace")
	} else if !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	out, _ := exec.Command("kubectl", "-n", namespace, "get", "clusters.postgresql.cnpg.io", "-o", "name").CombinedOutput() //nolint:gosec // the namespace the journey's app runs in
	if strings.Contains(string(out), "cluster.postgresql.cnpg.io/") {
		have = append(have, "the database's Cluster")
	}
	if claims, err := kube.CoreV1().PersistentVolumeClaims(namespace).List(t.Context(), metav1.ListOptions{}); err == nil && len(claims.Items) > 0 {
		have = append(have, "its claim")
	}
	switch len(have) {
	case 0:
		return ""
	case 3:
		return have[0] + ", " + have[1] + " and " + have[2]
	}
	return strings.Join(have, " and ")
}

// gone is whether the app's Deployment has left namespace.
func gone(t *testing.T, kube kubernetes.Interface, namespace string) bool {
	t.Helper()
	_, err := kube.AppsV1().Deployments(namespace).Get(t.Context(), app+"-"+strings.TrimPrefix(namespace, "canyon-"+app+"-"), metav1.GetOptions{})
	return apierrors.IsNotFound(err)
}

// servicesRefused is the reference set-up's admission policy (deploy/gitops/admission, #2476) refusing, in an app's
// namespace, each Service that reaches past it: a server-side dry run of a LoadBalancer, a NodePort and a ClusterIP
// with externalIPs, each denied by canyon-app-services. A plain ClusterIP is admitted, so a refusal is the policy's.
func servicesRefused(t *testing.T, kube kubernetes.Interface, namespace string) {
	t.Helper()
	dry := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	service := func(name string, spec corev1.ServiceSpec) *corev1.Service {
		spec.Selector = map[string]string{"canyon.app": app}
		spec.Ports = []corev1.ServicePort{{Port: 4180}}
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	}
	if _, err := kube.CoreV1().Services(namespace).Create(t.Context(), service("plain", corev1.ServiceSpec{}), dry); err != nil {
		t.Fatalf("a ClusterIP Service in %s was refused: %v", namespace, err)
	}
	for _, s := range []*corev1.Service{
		service("balancer", corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}),
		service("nodeport", corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort}),
		service("external", corev1.ServiceSpec{ExternalIPs: []string{"203.0.113.7"}}),
	} {
		_, err := kube.CoreV1().Services(namespace).Create(t.Context(), s, dry)
		if err == nil || !strings.Contains(err.Error(), "canyon-app-services") {
			t.Errorf("the %s Service in %s was not refused by canyon-app-services: %v", s.Name, namespace, err)
		}
	}
}

// envLabelled is the namespaces chart's canyon.dev/env on namespace, as Argo CD rendered it from the platform repo:
// the label the Gateway's listeners admit routes by (deploy/gitops/gateway, #2476).
func envLabelled(t *testing.T, kube kubernetes.Interface, namespace, env string) {
	t.Helper()
	ns, err := kube.CoreV1().Namespaces().Get(t.Context(), namespace, metav1.GetOptions{})
	must(t, err)
	if got := ns.Labels["canyon.dev/env"]; got != env {
		t.Errorf("%s is labelled canyon.dev/env=%q, not %s", namespace, got, env)
	}
}

// writersProbe is step 1b (#2479): what anyone who can write the deploy repo does with a directory of their own,
// committed straight to preview through Gitea's API, never through Canyon, under a canyon.json naming another Canyon
// so this one never writes over it. The same under either controller:
//  1. a second app's canyon.json and a ConfigMap: its namespace is made from the platform repo with no hand step,
//     and the ConfigMap applied into it;
//  2. a kustomization file naming a remote base, served by a listener here that records every request: two minutes
//     on, nothing has fetched it and the main app still answers, and under Flux the app's sync has applied that
//     commit with the kustomization file left out. A pod fetching from the same listener proves the cluster reaches it;
//  3. the kustomization file and the ConfigMap removed, canyon.json kept, an app with no resources undeployed: the
//     ConfigMap is pruned;
//  4. canyon.json removed: the namespace goes, and the main app's stays.
//
// The kustomization file is named Kustomization, one of the three names kustomize reads: Argo CD's directory source
// reads YAML files alone and skips it, so its sync never fails on a file that is no manifest and holds later commits
// in its retries.
//
// Under Flux, then, a namespace someone else made under an app's name is never taken over (notTakenOver).
func writersProbe(t *testing.T, kube kubernetes.Interface, forge *gitea, gitops, hostIP, image string, mainAnswers func() bool) {
	t.Helper()
	const ns = "canyon-probe-preview"
	listener := listen(t, hostIP)
	forge.change(t, "1b.1: a second app, the writer's own", map[string]string{
		"apps/probe/preview/canyon.json":    `{"canyon":"https://another-canyon.handoff.test","app":"probe","env":"preview"}` + "\n",
		"apps/probe/preview/configmap.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: probe\ndata:\n  committed: by the writer\n",
	})
	if !within(3*time.Minute, func() bool { return probeConfigMapIn(t, kube, ns) }) {
		t.Fatalf("three minutes after a second app's commit %s has no ConfigMap probe: its namespace was not made, or the sync did not apply it", ns)
	}
	envLabelled(t, kube, ns, "preview")

	sha := forge.change(t, "1b.2: a kustomization with a remote base", map[string]string{
		"apps/probe/preview/Kustomization": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - configmap.yaml\n  - " + listener.url + "/remote.yaml\n",
	})
	time.Sleep(2 * time.Minute)
	if listener.saw("/remote.yaml") {
		t.Error("the sync fetched the remote base a kustomization file in the deploy repo named: content Canyon never checked, from inside the cluster")
	}
	if !mainAnswers() {
		t.Error("with the probe's kustomization file on preview, the main app no longer answers")
	}
	if gitops == "flux" {
		if rev := kubectlOut("-n", "canyon-sync", "get", "kustomization", ns, "-o", "jsonpath={.status.lastAppliedRevision}"); !strings.HasSuffix(rev, sha) {
			t.Errorf("two minutes after the kustomization file's commit %s, %s's sync last applied %q: the kustomization file was not left out of its source", sha, ns, rev)
		}
	}
	reach(t, kube, image, listener.url+"/reach")
	if !within(2*time.Minute, func() bool { return listener.saw("/reach") }) {
		t.Fatalf("no pod reached %s in two minutes, so nothing fetching from it says nothing of the sync", listener.url)
	}

	forge.change(t, "1b.3: the probe undeployed, canyon.json kept", nil, "apps/probe/preview/Kustomization", "apps/probe/preview/configmap.yaml")
	if !within(3*time.Minute, func() bool { return !probeConfigMapIn(t, kube, ns) }) {
		t.Fatalf("three minutes after its ConfigMap left git %s still has it: a directory left with canyon.json alone was not pruned", ns)
	}
	forge.change(t, "1b.4: the probe deleted", nil, "apps/probe/preview/canyon.json")
	if !within(5*time.Minute, func() bool { return dataIn(t, kube, ns) == "" }) {
		t.Fatalf("five minutes after the probe's directory left git %s still has %s", ns, dataIn(t, kube, ns))
	}
	if dataIn(t, kube, "canyon-"+app+"-preview") == "" {
		t.Fatal("removing the probe took the main app's preview namespace with it")
	}
	if gitops == "flux" {
		notTakenOver(t, kube, forge)
	}
}

// notTakenOver is the generator never adopting a namespace it did not make (generator.yaml's disableTakeOwnership):
// someone else's canyon-taken-preview, labelled as Canyon's so the admission policy would admit the change, and a
// path naming it. The preview release's upgrade fails on it, the namespace stays as it was, and once the path is
// gone the release is Ready again, before step 4 needs it.
func notTakenOver(t *testing.T, kube kubernetes.Interface, forge *gitea) {
	t.Helper()
	const ns = "canyon-taken-preview"
	_, err := kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns,
		Labels: map[string]string{"pod-security.kubernetes.io/enforce": "baseline", "canyon.dev/managed": "true", "canyon.dev/env": "preview"}}}, metav1.CreateOptions{})
	must(t, err)
	forge.change(t, "1c: a path naming a namespace someone else made", map[string]string{
		"apps/taken/preview/canyon.json": `{"canyon":"https://another-canyon.handoff.test","app":"taken","env":"preview"}` + "\n",
	})
	ready := func() string {
		return kubectlOut("-n", "flux-system", "get", "helmrelease", "canyon-namespaces-preview", "-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].message}`)
	}
	taken := func() bool {
		got, err := kube.CoreV1().Namespaces().Get(t.Context(), ns, metav1.GetOptions{})
		must(t, err)
		_, labelled := got.Labels["helm.toolkit.fluxcd.io/name"]
		return labelled || got.Annotations["meta.helm.sh/release-name"] != ""
	}
	if !within(3*time.Minute, func() bool { return taken() || strings.Contains(ready(), ns) }) {
		t.Fatalf("three minutes after a path named %s the preview namespaces release never tried to make it: %s", ns, ready())
	}
	if taken() {
		t.Errorf("the preview namespaces release took over %s, which someone else made", ns)
	}
	t.Logf("the preview namespaces release, on a path naming %s: %s", ns, ready())
	forge.change(t, "1c: the path removed", nil, "apps/taken/preview/canyon.json")
	must(t, kube.CoreV1().Namespaces().Delete(t.Context(), ns, metav1.DeleteOptions{}))
	if !within(3*time.Minute, func() bool { return strings.HasPrefix(ready(), "True ") }) {
		t.Fatalf("three minutes after the path naming %s was removed the preview namespaces release is not Ready: %s", ns, ready())
	}
}

// probeConfigMapIn is whether namespace holds the probe's ConfigMap.
func probeConfigMapIn(t *testing.T, kube kubernetes.Interface, namespace string) bool {
	t.Helper()
	_, err := kube.CoreV1().ConfigMaps(namespace).Get(t.Context(), "probe", metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// reach starts a pod, in a namespace of its own, that fetches u once: the cluster's way to this machine's listener.
// It runs the app's image, busybox's wget under it.
func reach(t *testing.T, kube kubernetes.Interface, image, u string) {
	t.Helper()
	const namespace = "handoff-reach"
	if _, err := kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	_, err := kube.CoreV1().Pods(namespace).Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{GenerateName: "reach-"}, Spec: corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		Containers:    []corev1.Container{{Name: "reach", Image: image, Command: []string{"wget", "-q", "-O", "/dev/null", u}}},
	}}, metav1.CreateOptions{})
	must(t, err)
}

// recorder is a plain HTTP listener on this machine's address, where the cluster reaches it, that records the path of
// every request and answers each with a ConfigMap.
type recorder struct {
	url  string
	mu   sync.Mutex
	seen []string
}

func listen(t *testing.T, hostIP string) *recorder {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(hostIP, "0"))
	must(t, err)
	r := &recorder{url: "http://" + ln.Addr().String()}
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.URL.Path)
		r.mu.Unlock()
		_, _ = io.WriteString(w, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: remote\n")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return r
}

func (r *recorder) saw(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.seen, path)
}

// kubectlOut is kubectl's output for fixed arguments, trimmed, its error in it.
func kubectlOut(args ...string) string {
	out, _ := exec.Command("kubectl", args...).CombinedOutput() //nolint:gosec // fixed arguments
	return strings.TrimSpace(string(out))
}

// apply is kubectl apply of manifests on its stdin: a credential among them never reaches a command line.
func apply(t *testing.T, manifests string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifests)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kubectl apply: %v\n%s", err, out)
	}
}

// platformSide prints, when the test failed, what the sync, External Secrets and CloudNativePG made of the repo.
func platformSide(t *testing.T, gitops string) {
	if !t.Failed() {
		return
	}
	for _, args := range slices.Concat(syncs[gitops].diagnostics, [][]string{
		{"get", "clustersecretstores,externalsecrets,clusters.postgresql.cnpg.io,pods,events", "-A", "-o", "wide"},
		{"-n", "external-secrets", "logs", "deploy/external-secrets", "--tail=60"},
		{"-n", "canyon-" + app + "-preview", "logs", "-l", "canyon.app", "--all-containers", "--prefix", "--tail=50"},
		{"-n", "canyon-" + app + "-prod", "logs", "-l", "canyon.app", "--all-containers", "--prefix", "--tail=50"},
	}) {
		out, _ := exec.Command("kubectl", args...).CombinedOutput() //nolint:gosec // fixed arguments
		t.Logf("kubectl %s:\n%s", strings.Join(args, " "), out)
	}
}

// gitea is the org's git host: Gitea in Docker, behind the journey's TLS on this machine's address, where Canyon,
// Argo CD and this test reach it, its one user's password made here.
type gitea struct {
	url, password string
	client        *http.Client
}

func startGitea(t *testing.T, leaf tls.Certificate, hostIP, caFile string) *gitea {
	t.Helper()
	const name = "handoff-gitea"
	u, _, to := front(t, leaf, hostIP, "")
	_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput() // one an earlier run left
	sh(t, "docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::3000", "-e", "GITEA__security__INSTALL_LOCK=true", "-e", "GITEA__database__DB_TYPE=sqlite3",
		"-e", "GITEA__server__ROOT_URL="+u+"/", "-e", "GITEA__service__DISABLE_REGISTRATION=true", giteaImage)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.Command("docker", "logs", "--tail", "80", name).CombinedOutput()
			t.Logf("gitea:\n%s", out)
		}
		_, _ = exec.Command("docker", "rm", "-f", name).CombinedOutput()
	})
	port := strings.TrimSpace(strings.SplitN(sh(t, "docker", "port", name, "3000/tcp"), "\n", 2)[0])
	to("http://" + port)
	g := &gitea{url: u, password: rand.Text(), client: &http.Client{Timeout: time.Minute, Transport: trusting(t, caFile)}}
	if !within(2*time.Minute, func() bool {
		res, err := g.client.Get(u + "/api/v1/version") //nolint:noctx // the client's timeout bounds it
		if err != nil {
			return false
		}
		_ = res.Body.Close()
		return res.StatusCode == http.StatusOK
	}) {
		t.Fatal("Gitea never answered")
	}
	// the password on no command line: docker exec reads it from the environment it passes on
	cmd := exec.Command("docker", "exec", "-e", "PASSWORD", name, "sh", "-c", `gitea admin user create --admin --username `+owner+` --email `+owner+`@handoff.test --password "$PASSWORD" --must-change-password=false`)
	cmd.Env = append(os.Environ(), "PASSWORD="+g.password)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitea admin user create: %v\n%s", err, out)
	}
	return g
}

// token is a new access token of the user's, with scope alone.
func (g *gitea) token(t *testing.T, name, scope string) string {
	t.Helper()
	var made struct {
		SHA1 string `json:"sha1"`
	}
	g.call(t, http.MethodPost, "/users/"+owner+"/tokens", map[string]any{"name": name, "scopes": []string{scope}}, http.StatusCreated, &made)
	if made.SHA1 == "" {
		t.Fatalf("Gitea made the %s token empty", name)
	}
	return made.SHA1
}

// call is one call to Gitea's API as its user, answered want, its body decoded into out when out is not nil.
func (g *gitea) call(t *testing.T, method, path string, in any, want int, out any) {
	t.Helper()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		must(t, err)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, g.url+"/api/v1"+path, body)
	must(t, err)
	req.SetBasicAuth(owner, g.password)
	req.Header.Set("Content-Type", "application/json")
	res, err := g.client.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	answer, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode != want {
		t.Fatalf("gitea %s %s: %d %.500s", method, path, res.StatusCode, answer)
	}
	if out != nil {
		must(t, json.Unmarshal(answer, out))
	}
}

type pull struct {
	Number int `json:"number"`
	Head   struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// open is the deploy repo's open pull requests.
func (g *gitea) open(t *testing.T) []pull {
	t.Helper()
	var prs []pull
	g.call(t, http.MethodGet, "/repos/"+owner+"/deploy/pulls?state=open", nil, http.StatusOK, &prs)
	return prs
}

// pullRequest is the one pull request Canyon opens, once it has.
func (g *gitea) pullRequest(t *testing.T) pull {
	t.Helper()
	var prs []pull
	if !within(5*time.Minute, func() bool { prs = g.open(t); return len(prs) > 0 }) {
		t.Fatal("Canyon opened no pull request in five minutes")
	}
	if len(prs) != 1 {
		t.Fatalf("Canyon opened %d pull requests, not one", len(prs))
	}
	return prs[0]
}

// change commits to the deploy repo's preview branch as its owner, as anyone who can write the repo could: each of
// write's files made or replaced, each of remove's deleted. It answers the commit's SHA.
func (g *gitea) change(t *testing.T, message string, write map[string]string, remove ...string) string {
	t.Helper()
	var files []map[string]string
	for path, content := range write {
		op := map[string]string{"operation": "create", "path": path, "content": base64.StdEncoding.EncodeToString([]byte(content))}
		var existing struct {
			SHA string `json:"sha"`
		}
		if g.exists(t, path, &existing) {
			op["operation"], op["sha"] = "update", existing.SHA
		}
		files = append(files, op)
	}
	for _, path := range remove {
		var existing struct {
			SHA string `json:"sha"`
		}
		if !g.exists(t, path, &existing) {
			t.Fatalf("%s is not on preview to remove", path)
		}
		files = append(files, map[string]string{"operation": "delete", "path": path, "sha": existing.SHA})
	}
	var made struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	g.call(t, http.MethodPost, "/repos/"+owner+"/deploy/contents", map[string]any{"branch": "preview", "message": message, "files": files}, http.StatusCreated, &made)
	if made.Commit.SHA == "" {
		t.Fatalf("Gitea answered no commit for %q", message)
	}
	return made.Commit.SHA
}

// exists is whether path is a file on preview, its contents' metadata in out.
func (g *gitea) exists(t *testing.T, path string, out any) bool {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, g.url+"/api/v1/repos/"+owner+"/deploy/contents/"+path+"?ref=preview", nil)
	must(t, err)
	req.SetBasicAuth(owner, g.password)
	res, err := g.client.Do(req)
	must(t, err)
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return false
	}
	answer, err := io.ReadAll(res.Body)
	must(t, err)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("gitea GET contents/%s: %d %.500s", path, res.StatusCode, answer)
	}
	must(t, json.Unmarshal(answer, out))
	return true
}

// mergeOpen merges every open pull request, as the person who reviews production does.
func (g *gitea) mergeOpen(t *testing.T) {
	t.Helper()
	for _, pr := range g.open(t) {
		g.call(t, http.MethodPost, "/repos/"+owner+"/deploy/pulls/"+strconv.Itoa(pr.Number)+"/merge", map[string]string{"Do": "merge"}, http.StatusOK, nil)
	}
}
