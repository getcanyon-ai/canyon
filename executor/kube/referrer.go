package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// sbomType is the media type of the SBOM an image's referrer carries, and the referrer's artifact type
// (contracts/executor.md, target image; design/PATCHING.md §3.1).
const sbomType types.MediaType = "application/vnd.cyclonedx+json"

// referrerManifest is an OCI 1.1 artifact manifest: Put sends its bytes as they are.
type referrerManifest []byte

func (m referrerManifest) RawManifest() ([]byte, error) { return m, nil }

func (referrerManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

// pushReferrer publishes sbom, the CycloneDX document an image scan answered, in the image's own repository as
// an OCI referrer of the image image names by digest — artifact type and its one layer application/vnd.cyclonedx+json,
// its config OCI's empty one, its subject the image's manifest — for the org's own tools to find by the image's
// digest. It answers the referrer's digest once the registry lists it among the image's referrers; a registry that
// does not, as one without OCI 1.1's Referrers API, has the manifest removed again and answers an error. An image
// keeps one: one listed already is answered as it is and nothing is pushed, since a scan of the same image may run
// again — a scan whose answer the server did not store, two asks racing — and trivy's documents differ by their
// serial number and timestamp alone.
func (e *Executor) pushReferrer(ctx context.Context, image name.Digest, sbom []byte) (string, error) {
	opts := referrersOnly(e.registry(ctx))
	if listed, err := referrersOf(image, opts); err == nil && len(listed) > 0 {
		return listed[0].Digest.String(), nil
	}
	subject, err := remote.Head(image, opts...)
	if err != nil {
		return "", fmt.Errorf("the image: %w", err)
	}
	repo := image.Context()
	descriptor := func(l v1.Layer) (v1.Descriptor, error) {
		if err := remote.WriteLayer(repo, l, opts...); err != nil {
			return v1.Descriptor{}, err
		}
		d, err := l.Digest()
		if err != nil {
			return v1.Descriptor{}, err
		}
		size, err := l.Size()
		if err != nil {
			return v1.Descriptor{}, err
		}
		mt, err := l.MediaType()
		return v1.Descriptor{MediaType: mt, Digest: d, Size: size}, err
	}
	config, err := descriptor(static.NewLayer([]byte("{}"), types.OCIEmptyJSON))
	if err != nil {
		return "", fmt.Errorf("its config: %w", err)
	}
	doc, err := descriptor(static.NewLayer(sbom, sbomType))
	if err != nil {
		return "", fmt.Errorf("its SBOM: %w", err)
	}
	raw, err := json.Marshal(v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, ArtifactType: string(sbomType),
		Config: config, Layers: []v1.Descriptor{doc}, Subject: &v1.Descriptor{MediaType: subject.MediaType, Digest: subject.Digest, Size: subject.Size}})
	if err != nil {
		return "", err
	}
	sum, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	referrer := repo.Digest(sum.String())
	if err := remote.Put(referrer, referrerManifest(raw), opts...); err != nil {
		return "", fmt.Errorf("its manifest: %w", err)
	}
	if listed, err := referredBy(image, sum, opts); err != nil || !listed {
		if derr := remote.Delete(referrer, opts...); derr != nil && !notFound(derr) {
			log.Printf("executor: the SBOM referrer %s, which %s does not list: %v", referrer, repo.RegistryStr(), derr)
		}
		return "", fmt.Errorf("%s does not list it among the image's referrers (OCI 1.1's Referrers API): %v", repo.RegistryStr(), err)
	}
	return sum.String(), nil
}

// referrersOnly are opts with the referrers tag schema off: a registry without OCI 1.1's Referrers API gets no
// sha256-<hex> tag, which the collect would take for an image (Collect), so an SBOM is published only where the API is.
func referrersOnly(opts []remote.Option) []remote.Option {
	return append(opts, remote.WithReferrersTagFallback(false))
}

// referrersOf is what the registry lists as image's referrers. The repository is Canyon's own, canyon/<app>, so
// whatever refers to its image is Canyon's SBOM.
func referrersOf(image name.Digest, opts []remote.Option) ([]v1.Descriptor, error) {
	index, err := remote.Referrers(image, opts...)
	if err != nil {
		return nil, err
	}
	m, err := index.IndexManifest()
	if err != nil {
		return nil, err
	}
	return m.Manifests, nil
}

// referredBy is whether the registry lists want among image's referrers.
func referredBy(image name.Digest, want v1.Hash, opts []remote.Option) (bool, error) {
	listed, err := referrersOf(image, opts)
	return slices.ContainsFunc(listed, func(d v1.Descriptor) bool { return d.Digest == want }), err
}

// dropReferrers removes the referrers of the image digest names, as the image goes: none outlives what it describes.
// A registry that lists none, or cannot, leaves nothing to remove.
func (e *Executor) dropReferrers(ctx context.Context, image name.Digest) {
	opts := referrersOnly(e.registry(ctx))
	listed, err := referrersOf(image, opts)
	if err != nil {
		return
	}
	for _, d := range listed {
		if err := remote.Delete(image.Context().Digest(d.Digest.String()), opts...); err != nil && !notFound(err) {
			log.Printf("executor: the SBOM referrer %s of %s: %v", d.Digest, image, err)
		}
	}
}
