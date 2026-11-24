package names

// The images the executor pulls by digest for its scans, pinned per Canyon release: one list, which the executor pulls
// and canyon demo down --purge removes by the same references (#170).
const (
	// TrivyImage is trivy 0.74.0's multi-arch index (the same digest as docker.io/aquasec/trivy:0.74.0).
	TrivyImage = "ghcr.io/aquasecurity/trivy@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969"
	// GrypeImage is grype 0.119.0's multi-arch index (the same digest as docker.io/anchore/grype:v0.119.0), syft 1.52.0
	// inside.
	GrypeImage = "ghcr.io/anchore/grype@sha256:8c2c9234a345577a6d321a4753aa3ee1276d8975c8452d2344a56b57733ecad3"
	// CheckovImage is checkov 3.3.19's multi-arch index (docker.io/bridgecrew/checkov:3.3.19).
	CheckovImage = "docker.io/bridgecrew/checkov@sha256:d3e96adafdb315ca82e792ca8708c01adae85292800fb064c8b309b3d0cb7b80"
)

// PulledImages are the images above.
var PulledImages = []string{TrivyImage, GrypeImage, CheckovImage}

// LifecycleRepository is where the lifecycle image the executor names comes from.
const LifecycleRepository = "buildpacksio/lifecycle"

// LifecyclePlatforms are the digests of the executor's pinned lifecycle index (0.21.20, sha256:ffdceee5…) for
// linux/amd64 and linux/arm64. pack pulls the lifecycle by its platform's digest, never the index's, so canyon demo
// down --purge looks for it by these and nothing else, never by repository (#170).
var LifecyclePlatforms = []string{
	LifecycleRepository + "@sha256:b5b5f6c4e98b07fdb6e4f300a5eb4ef27573da6f0e4ea2eec696685f000bfce2", // linux/amd64
	LifecycleRepository + "@sha256:86de0d01207444eaa4159e424bc60c9c8c8fb89e74901ce54e42678aa046c2de", // linux/arm64
}
