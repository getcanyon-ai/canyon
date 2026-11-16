package executor

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

// ImageSetting is the image the setting names (contracts/executor.md, Images and database sources): ""
// when it is unset, which is the release's pin; else an image by digest with its registry named,
// <registry>/<repository>@sha256:<hex>, or an error naming the setting.
func ImageSetting(getenv func(string) string, setting string) (string, error) {
	v := getenv(setting)
	if v == "" {
		return "", nil
	}
	if err := ByDigest(v); err != nil {
		return "", fmt.Errorf("%s must be an image by digest, <registry>/<repository>@sha256:<hex>: %w", setting, err)
	}
	return v, nil
}

// DigestSetting is an image setting that may carry a tag beside its digest, as docker-compose.yml's pins do,
// <registry>/<repository>[:<tag>]@sha256:<hex>: the image by its digest alone, which is what runs; unset, "".
// CANYON_IMAGE_EGRESS_HOLDER is one, which compose hands the egress gateway's holder and the executor alike.
func DigestSetting(getenv func(string) string, setting string) (string, error) {
	v := getenv(setting)
	if v == "" {
		return "", nil
	}
	d, err := name.NewDigest(v, name.StrictValidation)
	if err == nil && !strings.HasPrefix(d.DigestStr(), "sha256:") {
		err = errors.New("the digest is not sha256")
	}
	if err != nil {
		return "", fmt.Errorf("%s must be an image by digest, <registry>/<repository>[:<tag>]@sha256:<hex>: %w", setting, err)
	}
	return d.Context().Name() + "@" + d.DigestStr(), nil
}

// ByDigest says why image is not <registry>/<repository>@sha256:<hex>, nil when it is.
func ByDigest(image string) error {
	d, err := name.NewDigest(image, name.StrictValidation)
	if repo, _, _ := strings.Cut(image, "@"); err == nil { // NewDigest takes repository:tag@digest too: what precedes @ is a repository alone
		_, err = name.NewRepository(repo, name.StrictValidation)
	}
	if err == nil && !strings.HasPrefix(d.DigestStr(), "sha256:") {
		err = errors.New("the digest is not sha256")
	}
	return err
}

// Configure points trivy, grype and checkov at CANYON_IMAGE_<TOOL>, trivy's and grype's databases at
// CANYON_SCANDB_<TOOL> and trivy's Java database at CANYON_SCANDB_TRIVY_JAVA, and caps an image scan at
// CANYON_SCAN_IMAGE_MAX (contracts/executor.md, Images and database sources); an unset setting keeps the
// release's pin, or the tool's own source. Each executor calls it once, before it serves: an error names
// the setting, and the executor does not start.
func Configure(getenv func(string) string) error {
	for _, tool := range []string{"trivy", "grype", "checkov"} {
		image, err := ImageSetting(getenv, "CANYON_IMAGE_"+strings.ToUpper(tool))
		if err != nil {
			return err
		}
		if t := scanTools[tool]; image != "" {
			t.image = image
			scanTools[tool] = t
		}
	}
	java := scanTools[JavaDB] // trivy's image target runs trivy's image, whichever it is
	java.image = scanTools["trivy"].image
	scanTools[JavaDB] = java
	for _, source := range []struct{ setting, tool, flag string }{{"CANYON_SCANDB_TRIVY", "trivy", "--db-repository"}, {"CANYON_SCANDB_TRIVY_JAVA", JavaDB, "--java-db-repository"}} {
		setting, v := source.setting, getenv(source.setting)
		if v == "" {
			continue
		}
		if _, err := name.NewTag(v, name.StrictValidation); err != nil {
			return fmt.Errorf("%s must be one OCI repository and tag, <registry>/<repository>:<tag>: %w", setting, err)
		}
		db := scanTools[source.tool].db
		db.Refresh = append(slices.Clip(db.Refresh), source.flag, v)
	}
	if v := getenv("CANYON_SCAN_IMAGE_MAX"); v != "" {
		size, err := sizeOf(v)
		if err != nil {
			return fmt.Errorf("CANYON_SCAN_IMAGE_MAX must be a size above zero, a whole number of bytes or of Ki, Mi, Gi or Ti: %w", err)
		}
		imageMax = size
	}
	if v := getenv("CANYON_SCANDB_GRYPE"); v != "" {
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("CANYON_SCANDB_GRYPE must be an https URL")
		}
		if u.User != nil {
			return errors.New("CANYON_SCANDB_GRYPE must not carry a credential: it would be written into the refresh pod's spec and log")
		}
		db := scanTools["grype"].db
		db.Env = append(slices.Clip(db.Env), "GRYPE_DB_UPDATE_URL="+v)
	}
	return nil
}

// imageMax is CANYON_SCAN_IMAGE_MAX, the largest image an image scan reads (contracts/executor.md): 8 GiB
// unset.
var imageMax int64 = 8 << 30

// sizeOf is a whole number of bytes, or of Ki, Mi, Gi or Ti, above zero.
func sizeOf(v string) (int64, error) {
	digits, shift := v, 0
	for i, suffix := range []string{"Ki", "Mi", "Gi", "Ti"} {
		if d, ok := strings.CutSuffix(v, suffix); ok {
			digits, shift = d, 10*(i+1)
			break
		}
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	switch {
	case err != nil:
		return 0, err
	case n <= 0 || n > math.MaxInt64>>shift || strings.TrimLeft(digits, "0123456789") != "":
		return 0, fmt.Errorf("%q is not a size above zero", v)
	}
	return n << shift, nil
}

// RunAptMirror is CANYON_RUN_APT_MIRROR, the Ubuntu archive canyon-run is built through on Docker: an http
// or https URL with no credential, query or fragment — every app built onto canyon-run can read it — which
// becomes run.Dockerfile's ARG default. Its tag stays the file's
// hash: the archive changes where the packages come from, not which. A character a Dockerfile's single
// quotes cannot carry is refused, as no archive's URL has one.
func RunAptMirror(getenv func(string) string) error {
	v := getenv("CANYON_RUN_APT_MIRROR")
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return errors.New("CANYON_RUN_APT_MIRROR must be an http or https URL")
	case u.User != nil || strings.ContainsAny(v, "?#"): // url.Parse drops an empty query or fragment: the raw string says
		return errors.New("CANYON_RUN_APT_MIRROR must not carry a credential, a query or a fragment: it is part of canyon-run, which every app can read")
	case strings.ContainsAny(v, "'\"\\$` \t\r\n"):
		return errors.New("CANYON_RUN_APT_MIRROR must not hold a quote, a backslash, a $, a backtick or a space")
	}
	before, after, ok := strings.Cut(runDockerfile, "\nARG CANYON_RUN_APT_MIRROR=\n")
	if !ok {
		return errors.New("run.Dockerfile declares no CANYON_RUN_APT_MIRROR")
	}
	runImage.dockerfile = before + "\nARG CANYON_RUN_APT_MIRROR='" + v + "'\n" + after
	return nil
}
