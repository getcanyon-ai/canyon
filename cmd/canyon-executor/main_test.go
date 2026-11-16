package main

import (
	"strings"
	"testing"
)

// The Docker executor reads its image settings before it serves: unset is the pin, a digest is taken,
// and anything else refuses the start with an error that names the setting.
func TestTheEnvironmentNamesTheDockerExecutorsImages(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	lifecycle := "mirror.example/buildpacksio/lifecycle@sha256:" + strings.Repeat("4", 64)
	busybox := "mirror.example/library/busybox@sha256:" + strings.Repeat("5", 64)
	if e, err := executorOf(env(nil)); err != nil || e.Lifecycle != "" || e.Busybox != "" || e.RunImage != "" {
		t.Fatalf("unset: %+v %v", e, err)
	}
	run := "mirror.example/golden/run@sha256:" + strings.Repeat("6", 64)
	holder := "mirror.example/build-image/distroless-iptables@sha256:" + strings.Repeat("7", 64)
	if e, err := executorOf(env(map[string]string{"CANYON_IMAGE_LIFECYCLE": lifecycle, "CANYON_IMAGE_BUSYBOX": busybox, "CANYON_IMAGE_RUN": run, "CANYON_IMAGE_EGRESS_HOLDER": holder})); err != nil || e.Lifecycle != lifecycle || e.Busybox != busybox || e.RunImage != run || e.Holder != holder {
		t.Fatalf("set: %+v %v", e, err)
	}
	tagged := "registry.k8s.io/build-image/distroless-iptables:v0.11.0@sha256:" + strings.Repeat("8", 64) // compose's own shape
	if e, err := executorOf(env(map[string]string{"CANYON_IMAGE_EGRESS_HOLDER": tagged})); err != nil || e.Holder != "registry.k8s.io/build-image/distroless-iptables@sha256:"+strings.Repeat("8", 64) {
		t.Errorf("CANYON_IMAGE_EGRESS_HOLDER with a tag beside its digest: %+v %v, want the image by its digest", e, err)
	}
	for _, setting := range []string{"CANYON_BUILD_ALLOW", "CANYON_APPS_ALLOW"} {
		for _, v := range []string{
			"169.254.169.254/32:80", // the metadata address
			"10.214.2.0/24:7778",    // Canyon's own networks, CANYON_EGRESS_POOLS unset
			"172.28.3.1/32:7777",    // an app network's gateway, the host's address on it, in CANYON_APPS_POOL unset
		} {
			if _, err := executorOf(env(map[string]string{setting: v})); err == nil || !strings.HasPrefix(err.Error(), setting+": ") {
				t.Errorf("%s=%s started: %v", setting, v, err)
			}
		}
		if _, err := executorOf(env(map[string]string{"CANYON_EGRESS_POOLS": "10.230.0.0/16", setting: "10.230.1.0/24:443"})); err == nil {
			t.Errorf("%s overlapping CANYON_EGRESS_POOLS started", setting)
		}
	}
	// host.docker.internal on Docker Desktop (192.168.65.254, .2 before it), Colima (192.168.5.0/24, the VM among it) and Podman: no app entry opens them
	for _, v := range []string{"192.168.65.0/24:7777", "192.168.65.254/32:443", "192.168.65.2/32:443", "192.168.5.0/24:5432", "192.168.5.15/32:22", "192.168.0.0/16:443", "10.0.2.0/24:80"} {
		if _, err := executorOf(env(map[string]string{"CANYON_APPS_ALLOW": v})); err == nil || !strings.HasPrefix(err.Error(), "CANYON_APPS_ALLOW: ") || !strings.Contains(err.Error(), "host.docker.internal") {
			t.Errorf("CANYON_APPS_ALLOW=%s started, or its refusal names no host alias: %v", v, err)
		}
	}
	if e, err := executorOf(env(map[string]string{"CANYON_APPS_ALLOW": "10.40.0.0/16:5432", "CANYON_BUILD_ALLOW": "10.20.0.0/16:443"})); err != nil || len(e.AppsAllow) != 1 || e.AppsAllow[0] != "10.40.0.0/16:5432" || len(e.Allow) != 1 || e.Allow[0] != "10.20.0.0/16:443" {
		t.Errorf("both settings: %+v %v, want each read into its own field", e, err)
	}
	if _, err := executorOf(env(map[string]string{"CANYON_EGRESS_POOLS": "10.230.0.1/16"})); err == nil || !strings.HasPrefix(err.Error(), "CANYON_EGRESS_POOLS: ") {
		t.Errorf("CANYON_EGRESS_POOLS that is no CIDR started: %v", err)
	}
	for setting, value := range map[string]string{
		"CANYON_IMAGE_LIFECYCLE":     "buildpacksio/lifecycle:0.21.20",
		"CANYON_IMAGE_TRIVY":         "ghcr.io/aquasecurity/trivy:0.74.0",
		"CANYON_IMAGE_BUSYBOX":       "busybox:1.37.0",
		"CANYON_SCANDB_GRYPE":        "http://mirror.example/databases",
		"CANYON_IMAGE_RUN":           "golden/run:noble",
		"CANYON_IMAGE_EGRESS_HOLDER": "registry.k8s.io/build-image/distroless-iptables:v0.11.0",
		"CANYON_RUN_APT_MIRROR":      "ftp://mirror.example/ubuntu",
	} {
		if _, err := executorOf(env(map[string]string{setting: value})); err == nil || !strings.HasPrefix(err.Error(), setting+" must be") {
			t.Errorf("%s=%s started: %v", setting, value, err)
		}
	}
}
