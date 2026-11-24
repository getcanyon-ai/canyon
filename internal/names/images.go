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

// LifecycleRepository is where the lifecycle image the executor names comes from. pack pulls it by its platform's
// digest, never the pinned index's, so it is found by repository.
const LifecycleRepository = "buildpacksio/lifecycle"
