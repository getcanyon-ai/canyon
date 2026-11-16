package kube

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// platformOf asks e's GET /platform of image, answering its status and body.
func platformOf(t *testing.T, e *Executor, image string) (int, string) {
	t.Helper()
	srv := httptest.NewServer(Handler(e))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/platform?" + url.Values{"image": {image}}.Encode()) //nolint:noctx // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// GET /platform answers an arm64 image's platform as the contract's platform_result (#2528): its config's own,
// read from the registry by the digest asked, as a /run reads it.
func TestPlatformIsTheImagesOwn(t *testing.T) {
	raw, err := os.ReadFile("../../contracts/testdata/executor/claims-portal.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Want map[string]string `json:"platform_result"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	e, _ := archRegistry(t)
	status, body := platformOf(t, e, pushed(t, e, "claims-portal", madeFor(t, "linux", "arm64")))
	var got map[string]string
	if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || len(got) != 2 || got["os"] != fixture.Want["os"] || got["architecture"] != fixture.Want["architecture"] {
		t.Fatalf("GET /platform of an arm64 image: %d %s, want %v", status, body, fixture.Want)
	}
}

// GET /platform of an image on another host than CANYON_KUBE_REGISTRY is refused 400, and neither that host nor
// the registry is sent anything: the executor's registry credential goes to its own registry alone (#2528).
func TestPlatformOfAnImageOnAnotherHostAsksNoRegistry(t *testing.T) {
	e, asked := archRegistry(t)
	elsewhere, askedThere := archRegistry(t)
	image := pushed(t, elsewhere, "claims-portal", madeFor(t, "linux", "arm64"))
	e.Auth = &authn.Basic{Username: "AWS", Password: "synthetic-registry-password"} // gitleaks:allow
	before := len(askedThere())
	if status, body := platformOf(t, e, image); status != http.StatusBadRequest {
		t.Fatalf("GET /platform of %s: %d %s, want 400", image, status, body)
	}
	for name, img := range map[string]string{
		"a tag":            e.Registry + "/canyon/claims-portal:latest",
		"another repo":     e.Registry + "/other/claims-portal@sha256:" + strings.Repeat("0", 64),
		"a short digest":   e.Registry + "/canyon/claims-portal@sha256:0",
		"no image at all":  "",
		"a dotless host":   "registry/canyon/claims-portal@sha256:" + strings.Repeat("0", 64),
		"a host with user": "user@" + e.Registry + "/canyon/claims-portal@sha256:" + strings.Repeat("0", 64),
	} {
		if status, body := platformOf(t, e, img); status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, status, body)
		}
	}
	if there := askedThere()[before:]; len(there) != 0 || len(asked()) != 0 {
		t.Fatalf("the other host was sent %v, the registry %v", there, asked())
	}
}

// GET /platform of an image whose architecture cannot be read answers 502 with /run's message, and one the
// registry does not have 404 (#2528): an index, a config not for linux, one naming no architecture, and a config
// past 4 MiB, which is never asked for.
func TestPlatformThatCannotBeReadIsNoAnswer(t *testing.T) {
	big := claimedConfig{madeFor(t, "linux", "arm64"), 4<<20 + 1}
	index, err := random.Index(64, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	e, asked := archRegistry(t)
	bigConfig, err := big.ConfigName()
	if err != nil {
		t.Fatal(err)
	}
	for name, image := range map[string]string{
		"an index of two architectures": pushed(t, e, "a-index", index),
		"a config for windows":          pushed(t, e, "a-windows", madeFor(t, "windows", "amd64")),
		"a config with no architecture": pushed(t, e, "a-noarch", madeFor(t, "linux", "")),
		"a config past its bound":       pushed(t, e, "a-big", big),
	} {
		if status, body := platformOf(t, e, image); status != http.StatusBadGateway || !strings.Contains(body, "'s architecture could not be read, so Canyon runs it nowhere: ") {
			t.Errorf("%s: %d %s, want 502 and /run's message", name, status, body)
		}
	}
	for _, a := range asked() {
		if strings.HasPrefix(a, http.MethodGet+" ") && strings.Contains(a, bigConfig.String()) {
			t.Errorf("a config past its bound was asked for: %s", a)
		}
	}
	status, body := platformOf(t, e, e.Registry+"/canyon/claims-portal@sha256:"+strings.Repeat("0", 64))
	var gone struct{ Error, Reason string }
	if status != http.StatusNotFound || json.Unmarshal([]byte(body), &gone) != nil || gone.Reason != "image_gone" || gone.Error == "" {
		t.Errorf("an image the registry does not have: %d %s, want 404 reason image_gone (the contract's platform_gone)", status, body)
	}
}
