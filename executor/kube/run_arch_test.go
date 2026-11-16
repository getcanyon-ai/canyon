package kube

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getcanyon-ai/canyon/executor"
)

// archRegistry is an executor whose registry is a test registry that records each request it is sent, as
// "<method> <path>", and what it was sent so far.
func archRegistry(t *testing.T) (*Executor, func() []string) {
	t.Helper()
	inner := registry.New()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Method+" "+r.URL.Path)
		mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	base, _, _ := fixture(t)
	e := &Executor{Registry: strings.TrimPrefix(srv.URL, "http://"), DBCIDRs: base.DBCIDRs, Identity: base.Identity}
	return e, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(asked)
	}
}

// otherArch is an architecture this test does not run on.
func otherArch() string {
	if goruntime.GOARCH == "s390x" {
		return "riscv64"
	}
	return "s390x"
}

// madeFor is an image whose config names os and arch.
func madeFor(t *testing.T, os, arch string) v1.Image {
	t.Helper()
	img, err := random.Image(64, 1)
	if err == nil {
		img, err = mutate.ConfigFile(img, &v1.ConfigFile{OS: os, Architecture: arch, RootFS: v1.RootFS{Type: "layers"}})
	}
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// claimedConfig is an image whose manifest claims its config is size bytes, whatever it is.
type claimedConfig struct {
	v1.Image
	size int64
}

func (c claimedConfig) Manifest() (*v1.Manifest, error) {
	m, err := c.Image.Manifest()
	if err != nil {
		return nil, err
	}
	m = m.DeepCopy()
	m.Config.Size = c.size
	return m, nil
}

func (c claimedConfig) RawManifest() ([]byte, error) {
	m, err := c.Manifest()
	if err != nil {
		return nil, err
	}
	return json.Marshal(m)
}

func (c claimedConfig) Digest() (v1.Hash, error) { return partial.Digest(c) }

func (c claimedConfig) Size() (int64, error) { return partial.Size(c) }

// pushed writes img, or an index, to e's registry as canyon/<app> and answers it by digest, as a /run names it.
func pushed(t *testing.T, e *Executor, app string, img partial.Describable) string {
	t.Helper()
	tag := mustRef(t, e.Registry+"/canyon/"+app+":x")
	var err error
	switch i := img.(type) {
	case v1.ImageIndex:
		err = remote.WriteIndex(tag, i)
	case v1.Image:
		err = remote.Write(tag, i)
	}
	sum, derr := img.Digest()
	if err = errors.Join(err, derr); err != nil {
		t.Fatal(err)
	}
	return e.Registry + "/canyon/" + app + "@" + sum.String()
}

// wrote is every request c was sent that writes.
func wrote(c *fake.Clientset) []string {
	var w []string
	for _, a := range c.Actions() {
		if v := a.GetVerb(); v != "get" && v != "list" && v != "watch" {
			w = append(w, v+" "+a.GetResource().Resource)
		}
	}
	return w
}

// An app runs on the nodes of its own image's architecture, whatever the executor's (#2440): the /run reads the
// image's config from the registry by its digest and pins the pods to the architecture it names, so on a
// cluster of both architectures a promotion, a rollback or a patch of either kind of image starts where it can.
func TestAnAppRunsOnTheNodesOfItsImagesArchitecture(t *testing.T) {
	e, _ := archRegistry(t)
	_, req, _ := fixture(t)
	req.Image = pushed(t, e, req.App, madeFor(t, "linux", otherArch()))
	c := cluster(t, e, healthy)
	if _, err := e.Run(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	d, err := c.AppsV1().Deployments(Namespace).Get(t.Context(), "claims-portal-preview", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sel := d.Spec.Template.Spec.NodeSelector; len(sel) != 1 || sel["kubernetes.io/arch"] != otherArch() {
		t.Fatalf("the app's nodeSelector %v, want kubernetes.io/arch: %s alone, its image's (the executor's is %s)", sel, otherArch(), goruntime.GOARCH)
	}
}

// An image whose architecture cannot be read runs nowhere, and its /run writes nothing (#2440): an index of
// several, a config not for linux or naming no architecture, a config its manifest says is past 4 MiB, never
// asked for, and an image the registry does not have, which is ErrNoImage.
func TestAnImageWhoseArchitectureCannotBeReadRunsNowhere(t *testing.T) {
	big := claimedConfig{madeFor(t, "linux", goruntime.GOARCH), executor.MaxImageConfig + 1}
	bigConfig, err := big.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	for name, image := range map[string]func(t *testing.T, e *Executor, app string) string{
		"an index of two architectures": func(t *testing.T, e *Executor, app string) string {
			index, err := random.Index(64, 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			return pushed(t, e, app, index)
		},
		"a config for windows": func(t *testing.T, e *Executor, app string) string {
			return pushed(t, e, app, madeFor(t, "windows", "amd64"))
		},
		"a config with no architecture": func(t *testing.T, e *Executor, app string) string { return pushed(t, e, app, madeFor(t, "linux", "")) },
		"a config past its bound":       func(t *testing.T, e *Executor, app string) string { return pushed(t, e, app, big) },
		"an image not there": func(t *testing.T, e *Executor, app string) string {
			return e.Registry + "/canyon/" + app + "@sha256:" + strings.Repeat("0", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e, asked := archRegistry(t)
			_, req, _ := fixture(t)
			req.Image = image(t, e, req.App)
			c := cluster(t, e, healthy)
			_, err := e.Run(t.Context(), req)
			switch {
			case name == "an image not there" && !errors.Is(err, executor.ErrNoImage):
				t.Errorf("a /run of it: %v, want ErrNoImage", err)
			case name != "an image not there" && (err == nil || !strings.Contains(err.Error(), "'s architecture could not be read, so Canyon runs it nowhere: ")):
				t.Errorf("a /run of it: %v", err)
			}
			if w := wrote(c); len(w) != 0 {
				t.Errorf("the /run wrote %v", w)
			}
			for _, a := range asked() {
				if strings.HasPrefix(a, http.MethodGet+" ") && strings.Contains(a, bigConfig.String()) {
					t.Errorf("a config past its bound was asked for: %s", a)
				}
			}
		})
	}
}

// A /run of an image on another host than CANYON_KUBE_REGISTRY sends that host, and the registry, nothing: the
// registry's credential goes to its own host alone (#2440's security review).
func TestARunOfAnImageOnAnotherHostAsksNoRegistry(t *testing.T) {
	e, asked := archRegistry(t)
	elsewhere, askedThere := archRegistry(t)
	_, req, _ := fixture(t)
	req.Image = pushed(t, elsewhere, req.App, madeFor(t, "linux", goruntime.GOARCH))
	e.Auth = &authn.Basic{Username: "AWS", Password: "synthetic-registry-password"} // gitleaks:allow
	c := cluster(t, e, healthy)
	before := len(askedThere())
	if _, err := e.Run(t.Context(), req); !errors.Is(err, executor.ErrRequest) {
		t.Fatalf("a /run of %s: %v, want it refused", req.Image, err)
	}
	if there := askedThere()[before:]; len(there) != 0 || len(asked()) != 0 || len(wrote(c)) != 0 {
		t.Fatalf("the other host was sent %v, the registry %v, the cluster %v", there, asked(), wrote(c))
	}
}

// The builder's buildpack names are read from its config only once its manifest names the config's size within
// 4 MiB (#2440's security review): a config claimed past it is never asked for.
func TestBuildpackNamesReadNoConfigPastItsBound(t *testing.T) {
	e, asked := archRegistry(t)
	big := claimedConfig{madeFor(t, "linux", goruntime.GOARCH), executor.MaxImageConfig + 1}
	builder := pushed(t, e, "builder", big)
	config, err := big.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.names(t.Context(), builder); err == nil {
		t.Fatal("the names of a builder whose config is past its bound were read")
	}
	for _, a := range asked() {
		if strings.HasPrefix(a, http.MethodGet+" ") && strings.Contains(a, config.String()) {
			t.Fatalf("its config was asked for: %s", a)
		}
	}
}
