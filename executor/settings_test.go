package executor

import (
	"bytes"
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	mirrorTrivy   = "mirror.example:5000/aquasecurity/trivy@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	mirrorGrype   = "mirror.example:5000/anchore/grype@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	mirrorCheckov = "mirror.example:5000/bridgecrew/checkov@sha256:3333333333333333333333333333333333333333333333333333333333333333"
	trivySource   = "mirror.example:5000/aquasec/trivy-db:2"
	grypeSource   = "https://mirror.example/grype/databases"
)

// configured runs Configure on env and puts the release's pins and sources back when the test ends.
func configured(t *testing.T, env map[string]string) error {
	old, trivy, grype, java, max := maps.Clone(scanTools), scanTools["trivy"].db, scanTools["grype"].db, scanTools[JavaDB].db, imageMax
	refresh, grypeEnv, javaRefresh := trivy.Refresh, grype.Env, java.Refresh
	t.Cleanup(func() {
		scanTools, trivy.Refresh, grype.Env, java.Refresh, imageMax = old, refresh, grypeEnv, javaRefresh, max
	})
	return Configure(func(k string) string { return env[k] })
}

// Unset, every scanner runs the release's pin and each database comes from the tool's own source (the
// lifecycle's pin: TestBuildNeverTrustsTheBuilder).
func TestAnUnsetImageSettingIsThePin(t *testing.T) {
	if err := configured(t, nil); err != nil {
		t.Fatal(err)
	}
	for tool, pin := range map[string]string{"trivy": trivyImage, "grype": grypeImage, "checkov": checkovImage} {
		if got, _ := ToolOf(tool); got.Image != pin {
			t.Errorf("%s runs %s, not the pin %s", tool, got.Image, pin)
		}
	}
	trivy, _ := ToolOf("trivy")
	grype, _ := ToolOf("grype")
	if slices.Contains(trivy.DB.Refresh, "--db-repository") || slices.ContainsFunc(grype.DB.Env, func(v string) bool { return strings.HasPrefix(v, "GRYPE_DB_UPDATE_URL=") }) {
		t.Errorf("an unset source names one: %v %v", trivy.DB.Refresh, grype.DB.Env)
	}
}

// A setting's image runs everywhere its pin ran — the scan, the database refresh and its reader, and
// /scan's answer — and the pin nowhere; each database source reaches its tool's refresh.
func TestASetImageRunsEverywhereItsPinRan(t *testing.T) {
	if err := configured(t, map[string]string{"CANYON_IMAGE_TRIVY": mirrorTrivy, "CANYON_IMAGE_GRYPE": mirrorGrype, "CANYON_IMAGE_CHECKOV": mirrorCheckov,
		"CANYON_SCANDB_TRIVY": trivySource, "CANYON_SCANDB_GRYPE": grypeSource}); err != nil {
		t.Fatal(err)
	}
	trivy, _ := ToolOf("trivy")
	grype, _ := ToolOf("grype")
	checkov, _ := ToolOf("checkov")
	if trivy.Image != mirrorTrivy || grype.Image != mirrorGrype || checkov.Image != mirrorCheckov || // what the Kubernetes executor runs
		!slices.Equal(trivy.DB.Refresh[len(trivy.DB.Refresh)-2:], []string{"--db-repository", trivySource}) || !slices.Contains(grype.DB.Env, "GRYPE_DB_UPDATE_URL="+grypeSource) {
		t.Errorf("ToolOf: %+v %+v %+v %+v %+v", trivy, grype, checkov, trivy.DB, grype.DB)
	}
	built := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	srv, e, dir := dbFake(t, `{"UpdatedAt":"`+built+`","built":"`+built+`"}`)
	for _, tool := range []string{"trivy", "grype"} {
		must(t, e.refreshDB(context.Background(), tool, scanTools[tool]))
	}
	for tool, image := range map[string]string{"trivy": mirrorTrivy, "grype": mirrorGrype, "checkov": mirrorCheckov} {
		status, body := postScan(t, srv.URL, `{"app":"claims-portal","package":"pkg_7d3e8f","tool":"`+tool+`","settings":{}}`, tarOf(t, map[string]string{"a": "b"}))
		if want := `{"tool":"` + tool + `","image":"` + image + `"`; status != 200 || !strings.HasPrefix(body, want) {
			t.Errorf("%s: want %s…, got %d %s", tool, want, status, body)
		}
	}
	argv := readIf(dir, "argv")
	for _, want := range []string{
		mirrorTrivy + "\nfs\n--cache-dir\n/db\n--download-db-only\n--disable-telemetry\n--skip-version-check\n--no-progress\n--db-repository\n" + trivySource + "\n", // the refresh
		"--entrypoint\nhead\n" + mirrorTrivy + "\n", mirrorTrivy + "\nfs\n--config\n", // the reader, the scan
		"--volume\ncanyon-scandb-grype:/db\n" + mirrorGrype + "\ncp\n", // the volume's owner
		"--env\nGRYPE_DB_CACHE_DIR=/db\n--env\nGRYPE_DB_UPDATE_URL=" + grypeSource + "\n" + mirrorGrype + "\ndb\nupdate\n",
		"--env\nGRYPE_DB_VALIDATE_AGE=false\n" + mirrorGrype + "\ndb\nstatus\n", mirrorGrype + "\ndir:/scan/tree\n",
		mirrorCheckov + "\n--directory=/scan/tree\n",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("docker argv lacks %q", want)
		}
	}
	for _, pin := range []string{trivyImage, grypeImage, checkovImage} {
		if strings.Contains(argv, pin) {
			t.Errorf("the pin %s still ran:\n%s", pin, argv)
		}
	}
	if n := strings.Count(argv, "GRYPE_DB_UPDATE_URL="); n != 1 {
		t.Errorf("grype's source reached %d containers, want the refresh alone:\n%s", n, argv)
	}
	e.Lifecycle = "mirror.example:5000/buildpacksio/lifecycle@sha256:" + strings.Repeat("4", 64)
	if _, err := e.Build(context.Background(), BuildRequest{App: "claims-portal", Package: "pkg_7d3e8f"}, bytes.NewReader(tarOf(t, map[string]string{"package.json": "{}"})), io.Discard); err != nil {
		t.Fatal(err)
	}
	if argv := readIf(dir, "pack-argv"); !strings.Contains(argv, "\n--lifecycle-image\n"+e.Lifecycle+"\n") || strings.Contains(argv, lifecycleImage) {
		t.Errorf("pack argv does not run the set lifecycle alone:\n%s", argv)
	}
}

// A value that is not what its setting takes refuses the start, and the error names the setting; one that
// is, docker.io's or a registry with a port, is taken.
func TestABadImageSettingRefusesTheStartNamingIt(t *testing.T) {
	sha := func(n int) string { return strings.Repeat("a", n) }
	for _, tc := range []struct{ setting, value string }{
		{"CANYON_IMAGE_TRIVY", "ghcr.io/aquasecurity/trivy:0.74.0"},
		{"CANYON_IMAGE_TRIVY", "ghcr.io/aquasecurity/trivy"},
		{"CANYON_IMAGE_GRYPE", "anchore/grype@sha256:" + sha(64)},
		{"CANYON_IMAGE_CHECKOV", "docker.io/bridgecrew/checkov@sha512:" + sha(128)},
		{"CANYON_IMAGE_CHECKOV", "docker.io/bridgecrew/checkov@sha256:abc"},
		{"CANYON_IMAGE_CHECKOV", "docker.io/Bridgecrew/checkov@sha256:" + sha(64)},
		{"CANYON_IMAGE_TRIVY", "ghcr.io/aquasecurity/trivy:0.74.0@sha256:" + sha(64)}, // a tag, beside the digest
		{"CANYON_SCANDB_TRIVY", "mirror.example/aquasec/trivy-db"},
		{"CANYON_SCANDB_TRIVY", "mirror.example/aquasec/trivy-db@sha256:" + sha(64)},
		{"CANYON_SCANDB_TRIVY", "aquasec/trivy-db:2"},
		{"CANYON_SCANDB_TRIVY", trivySource + ",ghcr.io/aquasecurity/trivy-db:2"},
		{"CANYON_SCANDB_GRYPE", "http://mirror.example/grype/databases"},
		{"CANYON_SCANDB_GRYPE", "mirror.example/grype/databases"},
		{"CANYON_SCANDB_GRYPE", "https:///databases"},
		{"CANYON_SCANDB_GRYPE", "https://mirror:token@mirror.example/grype/databases"}, // a credential: into the refresh pod's spec and log
		{"CANYON_SCANDB_TRIVY", "mirror:token@mirror.example/aquasec/trivy-db:2"},      // an OCI reference has no userinfo
		{"CANYON_SCANDB_TRIVY_JAVA", "mirror.example/aquasec/trivy-java-db"},
		{"CANYON_SCANDB_TRIVY_JAVA", "aquasec/trivy-java-db:1"},
		{"CANYON_SCAN_IMAGE_MAX", "8G"},
		{"CANYON_SCAN_IMAGE_MAX", "0"},
		{"CANYON_SCAN_IMAGE_MAX", "-1Gi"},
		{"CANYON_SCAN_IMAGE_MAX", "+8Gi"},
		{"CANYON_SCAN_IMAGE_MAX", "8.5Gi"},
		{"CANYON_SCAN_IMAGE_MAX", "9999999999Ti"}, // past int64
	} {
		if err := configured(t, map[string]string{tc.setting: tc.value}); err == nil || !strings.HasPrefix(err.Error(), tc.setting+" must ") {
			t.Errorf("%s=%s: %v", tc.setting, tc.value, err)
		}
	}
	for _, good := range []string{"docker.io/library/busybox@sha256:" + sha(64), "mirror.example:5000/busybox@sha256:" + sha(64)} { // go-containerregistry names docker.io index.docker.io
		if _, err := ImageSetting(func(string) string { return good }, "CANYON_IMAGE_BUSYBOX"); err != nil {
			t.Errorf("%s refused: %v", good, err)
		}
	}
}

// Trivy's image target runs trivy's image, the pin or CANYON_IMAGE_TRIVY; its Java database comes from
// CANYON_SCANDB_TRIVY_JAVA, its refresh alone; CANYON_SCAN_IMAGE_MAX caps the image, 8 GiB unset.
func TestTheImageTargetFollowsTrivysImageAndItsOwnSettings(t *testing.T) {
	if err := configured(t, nil); err != nil {
		t.Fatal(err)
	}
	if java, _ := ToolOf(JavaDB); java.Image != trivyImage || slices.Contains(java.DB.Refresh, "--java-db-repository") || ImageMax() != 8<<30 {
		t.Errorf("unset: %+v %v, cap %d", java, java.DB.Refresh, ImageMax())
	}
	source := "mirror.example:5000/aquasec/trivy-java-db:1"
	if err := configured(t, map[string]string{"CANYON_IMAGE_TRIVY": mirrorTrivy, "CANYON_SCANDB_TRIVY_JAVA": source, "CANYON_SCAN_IMAGE_MAX": "512Mi"}); err != nil {
		t.Fatal(err)
	}
	java, _ := ToolOf(JavaDB)
	trivy, _ := ToolOf("trivy")
	if java.Image != mirrorTrivy || !slices.Equal(java.DB.Refresh[len(java.DB.Refresh)-2:], []string{"--java-db-repository", source}) ||
		slices.Contains(trivy.DB.Refresh, source) || ImageMax() != 512<<20 {
		t.Errorf("set: %+v %v, trivy's refresh %v, cap %d", java, java.DB.Refresh, trivy.DB.Refresh, ImageMax())
	}
}

// CANYON_RUN_APT_MIRROR becomes run.Dockerfile's ARG default, so canyon-run's packages come from it, and the
// tag stays the file's hash; a value that is not an http or https URL, carries a credential, or holds what
// the Dockerfile's quoting cannot carry refuses the start, naming the setting.
func TestTheAptMirrorIsTheRunImagesArchive(t *testing.T) {
	saved := runImage.dockerfile
	t.Cleanup(func() { runImage.dockerfile = saved })
	env := func(v string) func(string) string {
		return func(k string) string { return map[string]string{"CANYON_RUN_APT_MIRROR": v}[k] }
	}
	tag := runImage.repo
	if err := RunAptMirror(env("http://mirror.example/ubuntu/")); err != nil || runImage.repo != tag ||
		!strings.Contains(runImage.dockerfile, "\nARG CANYON_RUN_APT_MIRROR='http://mirror.example/ubuntu/'\nRUN if [ -n \"$CANYON_RUN_APT_MIRROR\" ]; then printf 'Types: deb\\nURIs: %s\\nSuites: noble noble-updates noble-security\\n") {
		t.Fatalf("%v, tag %s:\n%s", err, runImage.repo, runImage.dockerfile)
	}
	for _, bad := range []string{"ftp://mirror.example/ubuntu", "mirror.example/ubuntu", "https://", "https://user:token@mirror.example/ubuntu",
		"http://mirror.example/ubuntu'; touch /x; '", "http://mirror.example/$HOME", "http://mirror.example/a b", "http://mirror.example/a\\b",
		"https://mirror.example/ubuntu?token=s3cr3t", "https://mirror.example/ubuntu#s3cr3t", "https://mirror.example/ubuntu?", "https://mirror.example/ubuntu#"} {
		runImage.dockerfile = saved
		if err := RunAptMirror(env(bad)); err == nil || !strings.HasPrefix(err.Error(), "CANYON_RUN_APT_MIRROR must ") || runImage.dockerfile != saved {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
