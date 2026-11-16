package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	corev1 "k8s.io/api/core/v1"

	"github.com/getcanyon-ai/canyon/executor"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// referringRegistry is go-containerregistry's in-memory registry, with OCI 1.1's Referrers API or without it: one
// without answers its endpoint 404, as registry:2 does.
func referringRegistry(t *testing.T, referrers bool) *Executor {
	t.Helper()
	inner := registry.New(registry.WithReferrersSupport(true))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !referrers && strings.Contains(r.URL.Path, "/referrers/") {
			http.NotFound(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &Executor{Registry: strings.TrimPrefix(srv.URL, "http://")}
}

// pushedImage pushes a random image to <registry>/canyon/claims-portal and answers it by digest.
func pushedImage(t *testing.T, e *Executor) name.Digest {
	t.Helper()
	img, err := random.Image(256, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref := mustRef(t, e.Registry+"/canyon/claims-portal:pkg_7d3e8f-0a1b")
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	sum, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return ref.Context().Digest(sum.String())
}

const sbomDoc = `{"bomFormat":"CycloneDX","specVersion":"1.6","version":1,"components":[]}`

// An app image's SBOM is published beside it as an OCI referrer (contracts/executor.md, target image; design/PATCHING.md
// §3.1): the registry lists it among the image's referrers, artifact type application/vnd.cyclonedx+json, its subject the
// image and its one layer the SBOM, byte for byte, so an org's own tool finds it by the image's digest.
func TestTheSBOMIsPublishedAsAReferrerOfTheImage(t *testing.T) {
	e := referringRegistry(t, true)
	image := pushedImage(t, e)
	referrer, err := e.pushReferrer(context.Background(), image, []byte(sbomDoc))
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Referrers(image)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := index.IndexManifest()
	if err != nil || len(listed.Manifests) != 1 || listed.Manifests[0].Digest.String() != referrer {
		t.Fatalf("the image's referrers: %+v (%v), want one SBOM %s", listed, err, referrer)
	}
	art, err := remote.Image(image.Context().Digest(referrer))
	if err != nil {
		t.Fatal(err)
	}
	m, err := art.Manifest()
	if err != nil || m.ArtifactType != "application/vnd.cyclonedx+json" || m.Config.MediaType != "application/vnd.oci.empty.v1+json" || m.Subject == nil ||
		m.Subject.Digest.String() != image.DigestStr() || len(m.Layers) != 1 || m.Layers[0].MediaType != "application/vnd.cyclonedx+json" {
		t.Fatalf("the referrer's manifest: %+v (%v)", m, err)
	}
	layers, err := art.Layers()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := layers[0].Compressed()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if body, err := io.ReadAll(rc); err != nil || string(body) != sbomDoc {
		t.Fatalf("its SBOM: %q (%v)", body, err)
	}
}

// A registry that does not list referrers keeps none: the manifest pushed is removed again and no referrer is
// answered, never one an org's tool could not find.
func TestARegistryWithoutReferrersKeepsNone(t *testing.T) {
	e := referringRegistry(t, false)
	image := pushedImage(t, e)
	if referrer, err := e.pushReferrer(context.Background(), image, []byte(sbomDoc)); err == nil || referrer != "" {
		t.Fatalf("a registry without the Referrers API answered %q (%v)", referrer, err)
	}
	tags, err := remote.List(image.Context())
	if err != nil || len(tags) != 1 {
		t.Fatalf("the repository's tags: %v (%v), want the image's alone", tags, err)
	}
}

// An image the collect removes takes its SBOM referrer with it.
func TestAnImageDroppedTakesItsSBOM(t *testing.T) {
	e := referringRegistry(t, true)
	image := pushedImage(t, e)
	referrer, err := e.pushReferrer(context.Background(), image, []byte(sbomDoc))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.dropImage(context.Background(), image.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Head(image.Context().Digest(referrer)); !notFound(err) {
		t.Fatalf("the SBOM referrer outlived its image: %v", err)
	}
}

// An app image's scan answers the SBOM and the referrer that publishes it beside the image, which the registry lists
// by the image's digest; a registry that cannot list one answers the SBOM all the same, with no referrer: the
// referrer is never the scan's failure (contracts/executor.md, target image).
func TestAnAppImageScanPublishesItsSBOM(t *testing.T) {
	for _, referrers := range []bool{true, false} {
		e := referringRegistry(t, referrers)
		c := newScanCluster(t, e, cdxOut)
		c.meta = javaDBBuilt(time.Hour)
		addClaim(t, c, executor.JavaDB, corev1.ClaimBound)
		req, _, _ := pushApp(t, e, 2, lifecycle)
		res, err := e.Scan(t.Context(), req, noParts())
		if err != nil || string(res.SBOM) != cdxOut || res.NotFinished != "" {
			t.Fatalf("referrers %v: %+v %v", referrers, res, err)
		}
		if !referrers {
			if res.Referrer != "" {
				t.Fatalf("a registry without the Referrers API answered a referrer %s", res.Referrer)
			}
			continue
		}
		image, err := name.NewDigest(req.Image)
		if err != nil {
			t.Fatal(err)
		}
		index, err := remote.Referrers(image)
		if err != nil {
			t.Fatal(err)
		}
		if listed, err := index.IndexManifest(); err != nil || len(listed.Manifests) != 1 || listed.Manifests[0].Digest.String() != res.Referrer {
			t.Fatalf("the image's referrers %+v (%v), the answer's %q", listed, err, res.Referrer)
		}
	}
}

// An image keeps one SBOM referrer: a second scan of it, whose document differs by trivy's serial number and timestamp
// alone, pushes none and answers the one listed (contracts/executor.md, target image).
func TestTwoScansOfOneImageLeaveOneReferrer(t *testing.T) {
	e := referringRegistry(t, true)
	image := pushedImage(t, e)
	first, err := e.pushReferrer(context.Background(), image, []byte(`{"bomFormat":"CycloneDX","serialNumber":"urn:uuid:1b4e28ba-2fa1-11d2-883f-0016d3cca427","components":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.pushReferrer(context.Background(), image, []byte(`{"bomFormat":"CycloneDX","serialNumber":"urn:uuid:6fa459ea-ee8a-3ca4-894e-db77e160355e","components":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Referrers(image)
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := index.IndexManifest(); err != nil || len(listed.Manifests) != 1 || second != first {
		t.Fatalf("two scans of one image: %d referrers (%v), answered %s then %s", len(listed.Manifests), err, first, second)
	}
}
