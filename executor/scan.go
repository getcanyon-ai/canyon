package executor

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/getcanyon-ai/canyon/internal/names"
)

// ScanRequest is the json part of POST /scan.
type ScanRequest struct {
	App      string        `json:"app,omitempty"`
	Package  string        `json:"package,omitempty"`
	Tool     string        `json:"tool"` // trivy | opengrep | eslint | grype | checkov
	Settings *ScanSettings `json:"settings,omitempty"`
	Target   string        `json:"target,omitempty"` // tree (unset) | image | sbom | packages: trivy alone takes the last three
	Image    string        `json:"image,omitempty"`  // the target image's app image by digest, as /build answered it
}

// ScanSettings is every tool's allow-list in one: check refuses a key the tool does not take.
type ScanSettings struct {
	Scanners   []string `json:"scanners,omitempty"`    // trivy
	Frameworks []string `json:"frameworks,omitempty"`  // checkov
	SkipChecks []string `json:"skip_checks,omitempty"` // checkov
	SkipPaths  []string `json:"skip_paths,omitempty"`  // every tool
}

// ScanResult is the body of a /scan answer: the tool's SARIF, or why there is none.
type ScanResult struct {
	Tool        string          `json:"tool"`
	Image       string          `json:"image,omitempty"`
	DBBuiltAt   string          `json:"db_built_at,omitempty"` // on trivy's and grype's SARIF answer only
	SARIF       json.RawMessage `json:"sarif,omitempty"`
	SBOM        json.RawMessage `json:"sbom,omitempty"`               // an image target's: trivy's CycloneDX
	Referrer    string          `json:"referrer,omitempty"`           // beside an app image's SBOM on Kubernetes: the digest of the referrer that publishes it
	Lifecycle   string          `json:"lifecycle_metadata,omitempty"` // beside SBOM: the image's io.buildpacks.lifecycle.metadata label
	DiffIDs     []string        `json:"diff_ids,omitempty"`           // beside SBOM: the image's layers in order, which trivy's SBOM sorts
	Report      json.RawMessage `json:"report,omitempty"`             // an sbom target's: trivy's JSON report
	Unscanned   []string        `json:"unscanned,omitempty"`          // beside Report: the bom-refs of components trivy lists but never matches
	Packages    json.RawMessage `json:"packages,omitempty"`           // a packages target's: trivy's JSON report of the packages it read
	Unloaded    []string        `json:"unloaded,omitempty"`           // on opengrep's SARIF answer only: the rules it could not load, in name order
	Unparsed    []string        `json:"unparsed,omitempty"`           // on opengrep's SARIF answer only: the files it did not read whole, in path order
	NotFinished string          `json:"not_finished,omitempty"`
	Reason      string          `json:"reason,omitempty"` // on no_room the scheduler's own message, quoted; on an image target's db_missing and db_stale, JavaDB
}

// scanTool is one tool as a release pins it: the image by digest, the argv after it, the config
// written to /scan/<tool>.yaml from the settings, and the whole scan's deadline (at most 10 minutes).
type scanTool struct {
	image    string
	build    *scanImage // instead of image, for a tool that publishes none
	args     []string
	config   func(ScanSettings) []byte // built with a YAML encoder, never string formatting: a value must stay a value; nil for a tool that reads no config file, whose skip paths leave the tree instead
	deadline time.Duration
	db       *toolDB  // nil for a tool without a vulnerability database
	uriRoots []string // a result uri that starts with one of these comes back without it: the package's path
	exits    []int    // the exit statuses that are a scan, any other rules_error; nil, the tool's status is not read
	jsonOut  bool     // it runs under ThenJSON, its stdout its SARIF, a NUL and its JSON output, whose errors are the answer's unloaded and unparsed
	env      []string // KEY=value the scan's container gets beyond its image's: trivy's image target's TMPDIR alone (invariant 7)
	// entrypoint is the image's own ENTRYPOINT, as its config (opengrep: its Dockerfile) names it: a host
	// that wraps the tool's command, as Kubernetes keeps its stderr out of its log, runs this under it.
	entrypoint []string
}

// scanImage is an image the executor builds from a Dockerfile on its digest-pinned base — a tool's that
// publishes none, canyon-run, canyon-builder — on first use, tags repo so a prune of dangling images keeps it, and runs by
// the ID the build printed; a build that fails is tried again on the next scan.
type scanImage struct {
	dockerfile string
	repo       string
	about      string        // what the log says of a build beyond its name, where its size and time are worth knowing
	deadline   time.Duration // one build's, dbDeadline when unset
	mu         sync.Mutex
	id         string
	ready      time.Time // when this executor last built it: a call that began before then waited for it (#2711)
}

// builtAt is when this executor last built the image, zero when it never did: it found it on the daemon.
// on is whether the daemon holds this image already, as built finds it, without building it.
func (s *scanImage) on(ctx context.Context, docker string) bool {
	return child(ctx, docker, "image", "inspect", "--format", "{{.Id}}", s.repo).Run() == nil
}

func (s *scanImage) builtAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

// stillEvery is how often the log says a docker build or pull is still running: a minute, which the nth line
// counts as n minutes; a var so a test can lower it.
var stillEvery = time.Minute

// still logs, every stillEvery until stop returns, that the executor is still doing what it names: docker's
// build and pull print nothing while they run, and a first one can take many minutes (#2697).
func still(doing string) (stop func()) {
	done, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		tick := time.NewTicker(stillEvery)
		defer tick.Stop()
		for n := 1; ; n++ {
			select {
			case <-done:
				return
			case <-tick.C:
				log.Printf("executor: still %s, %s so far", doing, minutes(n))
			}
		}
	}()
	return func() { close(done); <-ended }
}

// minutes is n as the log says it: "1 minute", "3 minutes".
func minutes(n int) string {
	if n == 1 {
		return "1 minute"
	}
	return strconv.Itoa(n) + " minutes"
}

// pastDeadline is a build of an image the executor builds that ran past its deadline: Canyon's own failure, in
// words that say what to do, since the layers it finished stay with the daemon (#2697).
type pastDeadline struct {
	name     string // the image's repository, without its tag
	deadline time.Duration
}

func (e *pastDeadline) Error() string {
	return fmt.Sprintf("%s did not finish in %s, most likely a slow download. Deploy again to carry on: Docker keeps the layers it finished.",
		e.name, minutes(int(e.deadline.Minutes())))
}

var imageIDRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// built is the image's ID, built first when this executor has none or the daemon no longer holds it
// under its tag (an image prune removes it: no container keeps it in use between scans; pack and a
// scan are handed the tag, so an ID that outlived its tag is built again).
func (s *scanImage) built(ctx context.Context, docker string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// the tag names this Dockerfile's hash, so an image under it — this executor's or one before a restart —
	// is a build of it: reused, never built again, so a pruned build cache never makes a new identity
	if out, err := child(ctx, docker, "image", "inspect", "--format", "{{.Id}}", s.repo).Output(); err == nil && imageIDRe.MatchString(strings.TrimSpace(string(out))) {
		s.id = strings.TrimSpace(string(out))
		return s.id, nil
	}
	deadline := cmp.Or(s.deadline, dbDeadline)
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	var stderr strings.Builder
	cmd := child(ctx, docker, "build", "--quiet", "--tag", s.repo, "-")
	cmd.Stdin, cmd.Stderr = strings.NewReader(s.dockerfile), &stderr
	log.Printf("executor: building %s%s", s.repo, s.about)
	began, stop := time.Now(), still("building "+s.repo)
	out, err := cmd.Output()
	stop()
	if id := strings.TrimSpace(string(out)); err == nil && imageIDRe.MatchString(id) {
		log.Printf("executor: built %s in %s", s.repo, time.Since(began).Round(time.Second))
		s.id, s.ready = id, time.Now()
		return id, nil
	}
	err = fmt.Errorf("docker build %s: %v: %q %s", s.repo, err, out, strings.TrimSpace(stderr.String()))
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		name, _, _ := strings.Cut(s.repo, ":")
		err = &pastDeadline{name: name, deadline: deadline}
	}
	log.Printf("executor: %s failed after %s: %v", s.repo, minutes(int(time.Since(began).Minutes())), err) // the log never just stops
	return "", err
}

// opengrepDockerfile is Canyon's Opengrep image (opengrep.Dockerfile): the Docker executor builds it on
// first use, and a Kubernetes operator builds it for CANYON_IMAGE_OPENGREP. Another architecture names no
// stage, so FROM looks for docker.io/library/<arch>, which carries no opengrep, and the build fails. The
// binary unpacks itself into its cache on its first run, which the build makes, offline, readable by the
// scan's user; a scan writes only its log, under HOME, the tmpfs.
//
//go:embed opengrep.Dockerfile
var opengrepDockerfile string

// eslintDockerfile is Canyon's ESLint image (eslint.Dockerfile), built as opengrep's is, for
// CANYON_IMAGE_ESLINT on Kubernetes.
//
//go:embed eslint.Dockerfile
var eslintDockerfile string

// toolDB is a tool's vulnerability database, kept in the volume canyon-scandb-<tool>, which each scan
// mounts read-only at /scan/db (DB, which every host runs). The lock is the tool's one lock: a refresh
// holds it alone, scans share it, so the two never overlap.
type toolDB struct {
	DB
	scanUser bool // the refresh runs as the scan's user, who is made the volume's owner first
	lock     sync.RWMutex
	// refreshing is a refresh a scan started, still running; failed, that the last one failed, which the
	// next scan that finds no database answers (db_missing).
	refreshing, failed atomic.Bool
	// read is when the database was built as the executor last read it, after each refresh and at each scan's
	// check: GET /scandb answers it, so the read never waits on a refresh's lock (#2743).
	read atomic.Pointer[time.Time]
}

// The scanners' images, pinned per release where canyon demo down --purge reads them too (internal/names).
const (
	grypeImage   = names.GrypeImage
	trivyImage   = names.TrivyImage
	checkovImage = names.CheckovImage
)

// checkovFrameworks is checkov's allow-list and its default: what trivy leaves to it.
var checkovFrameworks = []string{"terraform", "cloudformation", "dockerfile"}

// scanTools is compiled in; a tool without an entry answers no_image. Each digest lands with its tool.
var scanTools = map[string]scanTool{
	// trivy reads only what the executor wrote (invariant 9): config and ignore file by explicit path, the
	// database read-only, and misconfig only for types whose inline trivy:ignore 0.74.0 does not honour —
	// terraform, cloudformation and dockerfile do (the finding vanishes), so checkov checks those; ansible
	// and plans are unchecked.
	"trivy": {
		image: trivyImage, entrypoint: []string{"trivy"},
		args: []string{"fs", "--config", "/scan/trivy.yaml", "--ignorefile", "/dev/null", "--misconfig-scanners", "kubernetes,helm,azure-arm", "--cache-dir", "/scan/db", "--format", "sarif",
			"--skip-db-update", "--skip-java-db-update", "--skip-check-update", "--offline-scan", "--disable-telemetry", "--skip-version-check", "--no-progress", "/scan/tree"},
		config:   trivyConfig,
		deadline: 10 * time.Minute,
		db: &toolDB{DB: DB{
			Refresh:  []string{"fs", "--cache-dir", "/db", "--download-db-only", "--disable-telemetry", "--skip-version-check", "--no-progress"},
			ReadWith: "head", Read: []string{"-c", "1048576", "/db/db/metadata.json"}, // its first MiB
			BuiltAt: "UpdatedAt",
		}},
	},
	// trivy's image target (contracts/executor.md, target image): its Java database is this entry's, the
	// volume canyon-scandb-trivy-java, never a scan's tool of its own (check refuses the name). --format
	// cyclonedx lists without matching, so the main database is not read; the memory cache keeps trivy off
	// the read-only /scan/db. The SBOM documents the lifecycle writes in /layers/sbom are skipped: they name
	// packages listed again in their own layers. Every other file there is read: an app's own buildpack can
	// write one. The refresh's near-1 GB download lands in the volume, not the tmpfs.
	JavaDB: {
		image: trivyImage, entrypoint: []string{"trivy"},
		args: []string{"image", "--input", "/scan/image.tar", "--format", "cyclonedx", "--cache-dir", "/scan/db", "--cache-backend", "memory", "--ignorefile", "/dev/null",
			"--skip-db-update", "--skip-java-db-update", "--offline-scan", "--disable-telemetry", "--skip-version-check", "--no-progress",
			"--skip-files", "/layers/sbom/**/sbom.cdx.json", "--skip-files", "/layers/sbom/**/sbom.spdx.json",
			"--skip-files", "/layers/sbom/**/sbom.syft.json", "--skip-files", "/layers/sbom/**/sbom.legacy.json"},
		env:      []string{"TMPDIR=/scan/tmp"}, // the scan's disk, not its memory
		deadline: 10 * time.Minute,
		db: &toolDB{DB: DB{
			// the download is staged in the volume's tmp/, emptied first of what a refresh cut short left
			Env: []string{"TMPDIR=/db/tmp"}, RefreshWith: "sh",
			Refresh: []string{"-c", `rm -rf /db/tmp && mkdir /db/tmp && exec trivy "$@"`, "sh", "image", "--cache-dir", "/db", "--download-java-db-only", "--disable-telemetry", "--skip-version-check", "--no-progress"},
			// there when its metadata reads and the database begins with SQLite's header: a file cut short reads as none
			ReadWith: "sh", Read: []string{"-c", `[ "$(head -c 15 /db/java-db/trivy-java.db)" = "SQLite format 3" ] || exit 1; exec head -c 1048576 /db/java-db/metadata.json`},
			BuiltAt: "UpdatedAt",
		}},
	},
	// grype reads only what the executor wrote (invariant 9): -c names its config, which ends its search
	// of its working directory (/tmp, empty) and home (/) for one; it finds no VEX document or ignore file
	// on its own. It writes each uri from the tree's root after a /. Grype makes its database directory
	// 0700, so it refreshes as the scan's user; db status is its own report of when that database was built,
	// its own age check off (it exits 1 past 120h, which would read as no database, not a stale one).
	"grype": {
		image: grypeImage, entrypoint: []string{"/grype"},
		args:     []string{"dir:/scan/tree", "-c", "/scan/grype.yaml", "-o", "sarif"},
		config:   grypeConfig,
		deadline: 10 * time.Minute,
		db: &toolDB{DB: DB{
			Env: []string{"GRYPE_DB_CACHE_DIR=/db"}, Refresh: []string{"db", "update"},
			ReadEnv: []string{"GRYPE_DB_CACHE_DIR=/db", "GRYPE_DB_VALIDATE_AGE=false"}, Read: []string{"db", "status", "-o", "json"},
			BuiltAt:  "built",
			Partials: "grype-db-download*", // os.MkdirTemp in its cache directory
		}, scanUser: true},
		uriRoots: []string{"/"},
	},
	// checkov 3.3.19 has no switch to ignore inline skips; it reports each skipped result with its
	// suppressions, which sarifOf removes. It adds <dir>/.checkov.yaml and .yml to its config only for a
	// -d standing alone in argv, so the tree is --directory=… in one argument; the working directory stays
	// / (a config there is read too), so its uris are paths from /. The trailing comma in "/dev/stdout," sends
	// the SARIF alone to stdout: without it checkov prints its report there and writes results.sarif.
	"checkov": {
		image: checkovImage, entrypoint: []string{"/entrypoint.sh"}, // bash: checkov "$@" outside GitHub Actions
		args:     []string{"--directory=/scan/tree", "--config-file", "/scan/checkov.yaml", "--output", "sarif", "--output-file-path", "/dev/stdout,", "--skip-download", "--quiet"},
		config:   checkovConfig,
		deadline: 10 * time.Minute,
		uriRoots: []string{"scan/tree/", "/scan/tree/"},
	},
	// opengrep reads rules only from /scan/rules, keeping each rule's own id (by default it prefixes the
	// rules' directory to it), and scans the tree as its working directory, so its uris are the package's
	// paths. The tree never steers it (invariant 9): nosem comments are off (1.30.0's SARIF still marks such
	// a result suppressed, which sarifOf removes); the .semgrepignore it reads at the project root, the
	// working directory, is not read, nor its default ignore list (tests/, vendor/, …) with it; and a .git
	// in the tree does not switch it to listing files through git. Nor does it skip a file quietly: by
	// default a file over 1 MB is not read, nor one on which three rules time out, and the SARIF says
	// neither — so no size limit and no per-rule timeout, and a file too slow to read runs into the
	// scan's deadline, answered not_finished. Its SARIF names a file it read none of or only part of only in
	// a message quoting the file; its JSON output, written in the same run, carries the file's path.
	"opengrep": {
		build: &scanImage{dockerfile: opengrepDockerfile, repo: "canyon-scan/opengrep"}, entrypoint: []string{"/usr/local/bin/opengrep"},
		args: []string{"scan", "--config", "/scan/rules", "--no-rewrite-rule-ids", "--sarif", "--json-output=" + opengrepJSON, "--disable-nosem", "--x-ignore-semgrepignore-files", "--no-git-ignore",
			"--disable-version-check", "--max-target-bytes", "0", "--timeout", "0", "."},
		deadline: 10 * time.Minute,
		exits:    []int{0, 1}, // Opengrep's own: any other, it could not load every rules file it was handed
		jsonOut:  true,
	},
	// eslint runs the four rules its image fixes (eslint.Dockerfile): its config by explicit path, the
	// tree's own never looked up and its inline comments off (invariant 9), so nothing in the tree turns a
	// rule off or another on. It writes each uri as a file URL under the tree.
	"eslint": {
		build: &scanImage{dockerfile: eslintDockerfile, repo: "canyon-scan/eslint"}, entrypoint: []string{"/opt/eslint/node_modules/.bin/eslint"},
		args: []string{"--no-config-lookup", "--config", "/opt/eslint/eslint.config.js", "--no-inline-config", "--no-error-on-unmatched-pattern",
			"--format", "/opt/eslint/node_modules/@microsoft/eslint-formatter-sarif/sarif.js", "."},
		deadline: 10 * time.Minute,
		uriRoots: []string{"file:///scan/tree/"},
	},
}

// JavaDB names trivy's image target's entry, whose database is the volume canyon-scandb-trivy-java;
// JavaReason is the reason an image scan without it answers (contracts/executor.md, The Java database).
const (
	JavaDB     = "trivy-java"
	JavaReason = "java"
)

// trivySBOMArgs are trivy's sbom target's arguments, on trivy's database as a tree scan reads it: the
// re-match reads no Java database, matching a jar's purl against the main one.
var trivySBOMArgs = []string{"sbom", "--format", "json", "--list-all-pkgs", "--scanners", "vuln", "--cache-dir", "/scan/db", "--ignorefile", "/dev/null", "--skip-db-update", "--skip-java-db-update",
	"--offline-scan", "--disable-telemetry", "--skip-version-check", "--no-progress", "/scan/sbom.cdx.json"}

// TrivyPackages names trivy's packages target's entry, which reads no database and takes no lock.
const TrivyPackages = "trivy-packages"

// trivyPackagesArgs are trivy's packages target's arguments (contracts/executor.md, target packages): the
// license scanner lists every package and reads no vulnerability database, and --offline-scan keeps trivy
// from fetching a Maven parent or BOM, so a version only that would give is listed as none.
var trivyPackagesArgs = []string{"fs", "--format", "json", "--list-all-pkgs", "--scanners", "license", "--include-dev-deps", "--cache-dir", "/tmp/trivy", "--cache-backend", "memory",
	"--ignorefile", "/dev/null", "--skip-db-update", "--skip-java-db-update", "--offline-scan", "--disable-telemetry", "--skip-version-check", "--no-progress", "/scan/tree"}

const (
	dbEvery    = 6 * time.Hour      // how often the executor refreshes each database
	dbMaxAge   = 5 * 24 * time.Hour // a database built longer before the scan starts is db_stale
	dbDeadline = 10 * time.Minute   // one refresh, or one build of a scanImage
)

const (
	maxScanOut = 10 << 20 // a tool's stdout
	maxRules   = 50 << 20 // opengrep's rules part
)

// scanMemory is /scan's --memory (contracts/executor.md: 2g). A var, not a const, so an e2e
// test can lower it to force a real OOM kill without waiting to fill the production limit.
var scanMemory = "2g"

// imageScans holds an image scan: one at a time on this host, each with its image's copy on its disk.
var imageScans sync.Mutex

var (
	errImageOver = errors.New("image past its bounds")
	errOverCap   = errors.New("over cap")
	errDBMissing = errors.New("no database")
	errDBStale   = errors.New("database stale")
)

// skipPathRe is contracts/schema/executor.schema.json's toolSettings.skip_paths item pattern, verbatim.
var skipPathRe = regexp.MustCompile(`^([^-./\\\x00-\x1F][^/\\\x00-\x1F]*|\.[^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.\.[^/\\\x00-\x1F]+)(/([^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.[^./\\\x00-\x1F][^/\\\x00-\x1F]*|\.\.[^/\\\x00-\x1F]+))*$`)

// check is scan_request's names and tool, only that tool's settings keys, and their values
// (executor.schema.json); an image or sbom target is trivy's, with only its own keys. Each host checks an
// image target's image against its own registry.
func (r ScanRequest) check() error {
	switch r.Target {
	case "image":
		if r.Tool == "trivy" && (r.App == "" || validApp(r.App)) && r.Image != "" && r.Package == "" && r.Settings == nil {
			return nil
		}
		return fmt.Errorf("%w: an image target is trivy's, with image, app unless it is the run image, and nothing else (contracts/schema/executor.schema.json scan_request)", ErrRequest)
	case "sbom":
		if r.Tool == "trivy" && r.App == "" && r.Image == "" && r.Package == "" && r.Settings == nil {
			return nil
		}
		return fmt.Errorf("%w: an sbom target is trivy's, with nothing else (contracts/schema/executor.schema.json scan_request)", ErrRequest)
	case "packages":
		if r.Tool == "trivy" && validApp(r.App) && validPackage(r.Package) && r.Image == "" && r.Settings == nil {
			return nil
		}
		return fmt.Errorf("%w: a packages target is trivy's, with app and package and nothing else (contracts/schema/executor.schema.json scan_request)", ErrRequest)
	case "", "tree":
	default:
		return fmt.Errorf("%w: target must be tree, image, sbom or packages", ErrRequest)
	}
	s := r.Settings
	if validApp(r.App) && validPackage(r.Package) && s != nil && r.Image == "" && map[string]bool{
		"trivy":    s.Frameworks == nil && s.SkipChecks == nil,
		"checkov":  s.Scanners == nil,
		"opengrep": s.Scanners == nil && s.Frameworks == nil && s.SkipChecks == nil,
		"eslint":   s.Scanners == nil && s.Frameworks == nil && s.SkipChecks == nil,
		"grype":    s.Scanners == nil && s.Frameworks == nil && s.SkipChecks == nil,
	}[r.Tool] && validValues(*s) {
		return nil
	}
	return fmt.Errorf("%w: app, package, tool, and settings with only that tool's keys and values (contracts/schema/executor.schema.json scan_request)", ErrRequest)
}

// checkIDRe is every id checkov 3.3.19 lists for its frameworks; BC_ ids need its platform, which --skip-download keeps out.
var checkIDRe = regexp.MustCompile(`^CKV2?_[A-Z0-9_]+$`)

// validValues is the schema's rule for each setting: scanners a non-empty set of vuln, misconfig and
// secret; frameworks a non-empty set of checkovFrameworks; at most 256 distinct skip checks, each a
// checkov id of at most 64 characters; at most 64 distinct skip paths, each at most 256 characters and
// a path of the package.
func validValues(s ScanSettings) bool {
	badScanner := func(v string) bool { return v != "vuln" && v != "misconfig" && v != "secret" }
	badFramework := func(v string) bool { return !slices.Contains(checkovFrameworks, v) }
	badCheck := func(c string) bool { return len(c) > 64 || !checkIDRe.MatchString(c) }
	badPath := func(p string) bool { return utf8.RuneCountInString(p) > 256 || !skipPathRe.MatchString(p) }
	return (s.Scanners == nil || len(s.Scanners) > 0 && distinct(s.Scanners) && !slices.ContainsFunc(s.Scanners, badScanner)) &&
		(s.Frameworks == nil || len(s.Frameworks) > 0 && distinct(s.Frameworks) && !slices.ContainsFunc(s.Frameworks, badFramework)) &&
		len(s.SkipChecks) <= 256 && distinct(s.SkipChecks) && !slices.ContainsFunc(s.SkipChecks, badCheck) &&
		len(s.SkipPaths) <= 64 && distinct(s.SkipPaths) && !slices.ContainsFunc(s.SkipPaths, badPath)
}

func distinct(xs []string) bool {
	return len(slices.Compact(slices.Sorted(slices.Values(xs)))) == len(xs)
}

// trivyConfig is /scan/trivy.yaml: the settings' scanners, vuln and misconfig when unset — never
// trivy's own default secret, gitleaks being the one secrets detector — and each skip path, relative
// to the tree, skipped as a directory and as a file.
func trivyConfig(s ScanSettings) []byte {
	type scan struct {
		Scanners  []string `yaml:"scanners,omitempty"`
		SkipDirs  []string `yaml:"skip-dirs,omitempty"`
		SkipFiles []string `yaml:"skip-files,omitempty"`
	}
	scanners := s.Scanners
	if scanners == nil {
		scanners = []string{"vuln", "misconfig"}
	}
	b, _ := yaml.Marshal(map[string]scan{"scan": {scanners, s.SkipPaths, s.SkipPaths}}) // lists of strings: nothing to fail on
	return b
}

// checkovConfig is /scan/checkov.yaml: the settings' frameworks (checkovFrameworks unset), skip checks,
// and each skip path as the regex checkov takes, anchored to that one path under the tree: checkov
// searches every path it walks with it, so "docs" alone would skip mydocs/ too.
func checkovConfig(s ScanSettings) []byte {
	frameworks := s.Frameworks
	if frameworks == nil {
		frameworks = checkovFrameworks
	}
	paths := make([]string, len(s.SkipPaths))
	for i, p := range s.SkipPaths {
		paths[i] = `^` + regexp.QuoteMeta("/scan/tree/"+p) + `\Z`
	}
	b, _ := yaml.Marshal(struct { // lists of strings: nothing to fail on
		Framework []string `yaml:"framework"`
		SkipCheck []string `yaml:"skip-check,omitempty"`
		SkipPath  []string `yaml:"skip-path,omitempty"`
	}{frameworks, s.SkipChecks, paths})
	return b
}

// globChars escapes the characters syft reads as doublestar glob syntax in an exclusion; a skip path
// never holds a backslash (skipPathRe).
var globChars = strings.NewReplacer("*", `\*`, "?", `\?`, "[", `\[`, "]", `\]`, "{", `\{`, "}", `\}`)

// grypeConfig is /scan/grype.yaml: the database where the scan mounts it, never updated (the scan has no
// network), and each skip path excluded as that one path under the tree — a directory with all it holds,
// or a file.
func grypeConfig(s ScanSettings) []byte {
	exclude := make([]string, len(s.SkipPaths))
	for i, p := range s.SkipPaths {
		exclude[i] = "./" + globChars.Replace(p)
	}
	b, _ := yaml.Marshal(map[string]any{ // strings and booleans: nothing to fail on
		"db":      map[string]any{"cache-dir": "/scan/db", "auto-update": false},
		"exclude": exclude,
	})
	return b
}

// Scan runs the tool's pinned image over one package, its tree (and opengrep's rules) checked into a
// private directory as /build does, then copied with the config into the container's /scan volume; or
// trivy over an app's image, saved there once its Java database is there, or over the part sbom.
func (e *Executor) Scan(ctx context.Context, req ScanRequest, parts *multipart.Reader) (ScanResult, error) {
	if err := req.check(); err != nil {
		return ScanResult{}, err
	}
	toolName, tool, ok := toolFor(req)
	if !ok {
		return ScanResult{Tool: req.Tool, NotFinished: "no_image"}, nil
	}
	reason := ""
	if req.Target == "image" {
		reason = JavaReason
	}
	image := tool.image
	if tool.build != nil {
		id, err := tool.build.built(ctx, e.docker())
		if err != nil {
			log.Printf("executor: %s's image: %v", req.Tool, err)
			return ScanResult{Tool: req.Tool, NotFinished: "no_image"}, nil
		}
		tool.image, image = id, tool.build.repo+"@"+id // run by the ID; answered, as /build's images are, with it after @
	}
	started := time.Now()
	dir, err := os.MkdirTemp("", "canyon-scan-")
	if err != nil {
		return ScanResult{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	var app appImage
	var part []byte                      // an sbom target's
	var fill func(context.Context) error // the image, saved once the database is there: a scan that cannot run never saves it
	switch req.Target {
	case "image":
		if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
			return ScanResult{}, fmt.Errorf("%w: an image target takes part json and no other", ErrRequest)
		}
		if !imageScans.TryLock() { // one at a time: each holds an image's copy on the executor's disk
			return ScanResult{}, ErrQueued
		}
		defer imageScans.Unlock()
		switch app, err = e.appImage(ctx, req); {
		case errors.Is(err, errImageOver):
			return ScanResult{Tool: req.Tool, Image: image, NotFinished: "over_cap", Reason: "image"}, nil
		case err != nil:
			return ScanResult{}, err
		}
		fill = func(ctx context.Context) error { return e.save(ctx, filepath.Join(dir, "scan"), app.id) }
		// on Docker trivy reads the OCI layout save unpacks, not the tarball Kubernetes feeds (#2585)
		i := slices.Index(tool.args, "/scan/image.tar")
		if i < 0 {
			return ScanResult{}, errors.New("trivy's image target names no /scan/image.tar")
		}
		tool.args = slices.Clone(tool.args)
		tool.args[i] = "/scan/image"
	case "sbom":
		part, err = stageSBOM(filepath.Join(dir, "scan"), parts)
	default:
		err = stage(filepath.Join(dir, "scan"), req, tool, parts)
	}
	if err != nil {
		return ScanResult{}, err
	}
	if tool.db != nil {
		if !tool.db.lock.TryRLock() { // a refresh holds or waits for the lock: a scan never waits behind it
			return ScanResult{}, ErrBusy
		}
		defer tool.db.lock.RUnlock()
	}
	ctx, cancel := context.WithTimeout(ctx, tool.deadline) // the tool's minutes start once the upload is in
	defer cancel()
	name := "canyon-scan-" + nonce() // removed by name: a create the deadline cut short prints no id
	defer e.remove(ctx, name)
	out, built, code, err := e.runScan(ctx, name, filepath.Join(dir, "scan"), toolName, tool, started, fill)
	out, unloaded, unparsed, whole := splitJSON(out, tool.jsonOut)
	res := ScanResult{Tool: req.Tool, Image: image}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.NotFinished = "timeout"
	case errors.Is(err, errImageOver):
		res.NotFinished, res.Reason = "over_cap", "image"
	case errors.Is(err, errOverCap) && (req.Target == "" || req.Target == "tree"):
		res.NotFinished = "over_cap"
	case errors.Is(err, errOverCap):
		res.NotFinished, res.Reason = "over_cap", "report"
	case errors.Is(err, errDBMissing) && !tool.db.failed.Swap(false): // the tool's first scan: its database is downloaded, and the scan asked again
		e.startRefresh(ctx, toolName, tool)
		return ScanResult{}, ErrBusy
	case errors.Is(err, errDBMissing):
		res.NotFinished, res.Reason = "db_missing", reason
	case errors.Is(err, errDBStale):
		res.NotFinished, res.Reason = "db_stale", reason
	case errors.Is(err, errImageLayout): // never an SBOM of an image Canyon did not unpack (contracts/executor.md, target image)
		log.Printf("executor: image scan of %s: %v", image, err)
		res.NotFinished = "not_sarif"
	case err != nil:
		return ScanResult{}, err
	case req.Target == "packages":
		switch doc, ok := targetDoc(req.Target, out); {
		case ok:
			res.Packages = doc
		case e.killed(ctx, name):
			res.NotFinished = "killed"
		default:
			res.NotFinished = "not_sarif"
		}
	case req.Target == "image" || req.Target == "sbom":
		switch doc, ok := targetDoc(req.Target, out); {
		case ok && req.Target == "image":
			res.SBOM, res.Lifecycle, res.DiffIDs = doc, app.lifecycle, app.diffIDs
		case ok:
			if err := considered(part, doc); err != nil { // a package trivy skipped was never matched: no clean answer
				log.Printf("executor: sbom scan: %v", err)
				res.NotFinished, res.Reason = "not_sarif", "unread"
				break
			}
			res.Report, res.DBBuiltAt, res.Unscanned = doc, built.UTC().Format(time.RFC3339), unscanned(part)
		case e.killed(ctx, name):
			res.NotFinished = "killed"
		default:
			res.NotFinished = "not_sarif"
		}
	default:
		switch res.SARIF, ok = sarifOf(out, tool.uriRoots); {
		case ok && tool.exits != nil && !slices.Contains(tool.exits, code) && e.killed(ctx, name): // an engine the OOM killer ended makes Opengrep itself exit 2
			res.SARIF, res.NotFinished = nil, "killed"
		case ok && tool.exits != nil && !Scanned(tool.exits, code, res.SARIF): // its SARIF is not the answer (contracts/executor.md)
			res.SARIF, res.NotFinished = nil, "rules_error"
		case ok && !whole: // without its JSON output no file it did not read whole could be named: never a clean scan
			res.SARIF, res.NotFinished = nil, "not_sarif"
		case ok:
			res.Unloaded, res.Unparsed = unloaded, unparsed
			if tool.db != nil {
				res.DBBuiltAt = built.UTC().Format(time.RFC3339)
			}
		case e.killed(ctx, name):
			res.NotFinished = "killed"
		default:
			res.NotFinished = "not_sarif"
		}
	}
	return res, nil
}

// toolFor is the pinned entry a request runs and its name, which names its database and lock: trivy's
// image target is JavaDB's entry, its sbom target trivy's own with the sbom arguments, and its packages
// target trivy's image with the packages arguments, no database and no config, named TrivyPackages.
func toolFor(req ScanRequest) (string, scanTool, bool) {
	name := req.Tool
	if req.Target == "image" {
		name = JavaDB
	}
	tool, ok := scanTools[name]
	switch req.Target {
	case "sbom":
		tool.args, tool.uriRoots = trivySBOMArgs, nil
	case "packages":
		name, tool = TrivyPackages, scanTool{image: tool.image, entrypoint: tool.entrypoint, args: trivyPackagesArgs, deadline: tool.deadline}
	}
	return name, tool, ok
}

// appImage is an app's image, or the run image, as an image scan reads it: its ID, its size, its
// io.buildpacks.lifecycle.metadata label and its layers' diff IDs in order.
type appImage struct {
	id        string
	size      int64
	lifecycle string
	diffIDs   []string
}

// appImage is the request's image when it is one of the app's builds this daemon holds, as /run takes it,
// or, with no app, the run image GET /builder answers now: ErrRequest for another reference, ErrNoImage
// (404) for an app's image it does not hold, *Refused run_image_moved for another run image.
func (e *Executor) appImage(ctx context.Context, req ScanRequest) (appImage, error) {
	if req.App == "" {
		run, err := e.RunImageID(ctx)
		if err != nil {
			return appImage{}, err
		}
		if req.Image != run {
			return appImage{}, &Refused{"run_image_moved", fmt.Sprintf("the run image is now %s, not %s: plan again", run, req.Image)}
		}
		return e.inspectImage(ctx, run)
	}
	repo, digest, ok := strings.Cut(req.Image, "@sha256:")
	if !ok || repo != "canyon/"+req.App || strings.Trim(digest, "0123456789abcdef") != "" || len(digest) != 64 {
		return appImage{}, fmt.Errorf("%w: image must be this app's digest reference (canyon/<app>@sha256:<64 hex>), never a tag", ErrRequest)
	}
	tags, err := e.images(ctx, req.App)
	if err != nil {
		return appImage{}, err
	}
	id := "sha256:" + digest
	if !slices.ContainsFunc(tags, func(t imageTag) bool { return t.id == id }) {
		return appImage{}, fmt.Errorf("%w: %s", ErrNoImage, req.Image)
	}
	return e.inspectImage(ctx, id)
}

// inspectImage is the image id as the daemon's inspect has it: errImageOver for one past a bound,
// CANYON_SCAN_IMAGE_MAX uncompressed, MaxImageLayers layers, a lifecycle label of MaxLifecycle.
func (e *Executor) inspectImage(ctx context.Context, id string) (appImage, error) {
	out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{json .}}", id).Output()
	if err != nil {
		return appImage{}, fmt.Errorf("image inspect %s: %w", id, err)
	}
	var info struct {
		Size   int64
		Config struct{ Labels map[string]string }
		RootFS struct{ Layers []string }
	}
	switch err := json.Unmarshal(out, &info); {
	case err != nil:
		return appImage{}, fmt.Errorf("image inspect %s: %w", id, err)
	case len(info.RootFS.Layers) == 0:
		return appImage{}, fmt.Errorf("image inspect %s: no layers", id)
	case info.Size > imageMax || len(info.RootFS.Layers) > MaxImageLayers || len(info.Config.Labels[lifecycleLabel]) > MaxLifecycle:
		return appImage{}, errImageOver
	}
	return appImage{id: id, size: info.Size, lifecycle: info.Config.Labels[lifecycleLabel], diffIDs: info.RootFS.Layers}, nil
}

// lifecycleLabel is the label in which the lifecycle says whose each layer of an image is.
const lifecycleLabel = "io.buildpacks.lifecycle.metadata"

// save writes the image into dir as image/, the OCI layout docker save writes, readable by the scan's user,
// and makes tmp/, trivy's TMPDIR, the scan's user's to write (contracts/executor.md, target image). The
// tarball docker save writes lands beside dir, never copied in, and is gone once unpacked:
// errImageOver when it is past CANYON_SCAN_IMAGE_MAX, errImageLayout when it holds anything unpackLayout
// refuses.
func (e *Executor) save(ctx context.Context, dir, id string) error {
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil { //nolint:gosec // the tool, running as nobody, reads it; dir's parent is private
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "tmp"), os.ModeSticky|0o777); err != nil { //nolint:gosec // as /tmp is: nobody writes there
		return err
	}
	tarball := filepath.Join(filepath.Dir(dir), "image.tar")
	defer func() { _ = os.Remove(tarball) }()
	if out, err := child(ctx, e.docker(), "save", "--output", tarball, id).CombinedOutput(); err != nil {
		return fmt.Errorf("docker save %s: %w: %s", id, err, strings.TrimSpace(string(out)))
	}
	f, err := os.Open(tarball) //nolint:gosec // our own staging
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	switch info, err := f.Stat(); {
	case err != nil:
		return err
	case info.Size() > imageMax:
		return errImageOver
	}
	return unpackLayout(f, filepath.Join(dir, "image"))
}

// errImageLayout is a saved image unpackLayout refuses: the scan answers not_sarif, never an SBOM.
var errImageLayout = errors.New("the saved image is not an OCI layout Canyon unpacks")

// blobName is an OCI layout's blob: blobs/sha256/ and 64 lowercase hex.
var blobName = regexp.MustCompile(`^blobs/sha256/[0-9a-f]{64}$`)

// unpackLayout writes into dst, readable by the scan's user, the OCI layout of the tar r, as docker save
// writes it: oci-layout, index.json and each blobs/sha256/<64 hex>, regular files each once. It drops the
// legacy manifest.json and repositories, which trivy reads as if every layer were compressed as the
// first (#2585), and the directory entries blobs/ and blobs/sha256/. Anything else — another name, a link,
// a device, a second entry of one name, or no oci-layout or index.json — is errImageLayout, fail closed.
func unpackLayout(r io.Reader, dst string) error {
	if err := os.MkdirAll(filepath.Join(dst, "blobs", "sha256"), 0o755); err != nil { //nolint:gosec // see save
		return err
	}
	tr, seen := tar.NewReader(r), map[string]bool{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errImageLayout, err)
		}
		switch name := h.Name; {
		case (name == "blobs/" || name == "blobs/sha256/") && h.Typeflag == tar.TypeDir:
			continue
		case h.Typeflag != tar.TypeReg:
			return fmt.Errorf("%w: %q is not a regular file", errImageLayout, name)
		case seen[name]:
			return fmt.Errorf("%w: %q twice", errImageLayout, name)
		case name == "manifest.json" || name == "repositories":
			seen[name] = true
			continue
		case name != "oci-layout" && name != "index.json" && !blobName.MatchString(name):
			return fmt.Errorf("%w: %q", errImageLayout, name)
		}
		seen[h.Name] = true
		f, err := os.OpenFile(filepath.Join(dst, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // a name matched above, read by the scan's user
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr) //nolint:gosec // bounded: the tarball is within CANYON_SCAN_IMAGE_MAX
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		switch {
		case errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EIO):
			return err
		case err != nil: // an entry cut short
			return fmt.Errorf("%w: %q: %v", errImageLayout, h.Name, err)
		}
	}
	if !seen["oci-layout"] || !seen["index.json"] {
		return fmt.Errorf("%w: no oci-layout or index.json", errImageLayout)
	}
	return nil
}

// stageSBOM writes part sbom into dir as sbom.cdx.json, and answers it.
func stageSBOM(dir string, parts *multipart.Reader) ([]byte, error) {
	b, err := sbomPart(parts)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see save
		return nil, err
	}
	return b, os.WriteFile(filepath.Join(dir, "sbom.cdx.json"), b, 0o644) //nolint:gosec // see save
}

// sbomPart is an sbom target's parts after its json: part sbom, one CycloneDX JSON document of at most
// maxScanOut, and no other.
func sbomPart(parts *multipart.Reader) ([]byte, error) {
	p, err := parts.NextPart()
	if err != nil || p.FormName() != "sbom" {
		return nil, fmt.Errorf("%w: second part must be sbom", ErrRequest)
	}
	b, err := io.ReadAll(io.LimitReader(p, maxScanOut+1))
	if err != nil {
		return nil, fmt.Errorf("%w: sbom: %v", ErrRequest, err)
	}
	if len(b) > maxScanOut {
		return nil, fmt.Errorf("%w: sbom must be one CycloneDX JSON document of at most %d bytes", ErrRequest, maxScanOut)
	}
	if b, err = checkPart(b); err != nil { // what trivy reads is what was checked: the part written again
		return nil, fmt.Errorf("%w: sbom: %v", ErrRequest, err)
	}
	if _, err := parts.NextPart(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: an sbom target takes part sbom and no other", ErrRequest)
	}
	return b, nil
}

// targetDoc is an image or sbom target's stdout as its answer carries it, false when it is not that.
func targetDoc(target string, out []byte) (json.RawMessage, bool) {
	if target == "image" {
		return sbomOf(out)
	}
	return reportOf(out, target != "packages")
}

// sbomOf is out when it is one CycloneDX JSON document.
func sbomOf(out []byte) (json.RawMessage, bool) {
	var doc struct {
		BOMFormat string `json:"bomFormat"`
	}
	if json.Unmarshal(out, &doc) != nil || doc.BOMFormat != "CycloneDX" {
		return nil, false
	}
	return out, true
}

// reportOf is out when it is one trivy JSON report, schema version 2, whose Results are a list and each
// of whose vulnerabilities carries the layer and the bom-ref of the component it matched: a match whose
// layer or component cannot be told is never an answer.
//
// A packages target's report may leave Results out: trivy does when it lists nothing (contracts/executor.md,
// target packages), so results is false for it.
func reportOf(out []byte, results bool) (json.RawMessage, bool) {
	var report struct {
		SchemaVersion int
		Results       *[]struct {
			Vulnerabilities []struct {
				Layer         struct{ DiffID string }
				PkgIdentifier struct{ BOMRef string }
			}
		}
	}
	if json.Unmarshal(out, &report) != nil || report.SchemaVersion != 2 || results && report.Results == nil {
		return nil, false
	}
	if report.Results == nil {
		return out, true
	}
	for _, r := range *report.Results {
		for _, v := range r.Vulnerabilities {
			if !imageIDRe.MatchString(v.Layer.DiffID) || v.PkgIdentifier.BOMRef == "" { // a diff ID is spelled as an image ID is
				return nil, false
			}
		}
	}
	return out, true
}

// killed is whether name died involuntarily — out of memory or a signal — rather than the tool
// exiting on its own: State.OOMKilled is docker's own signal for the former, and an ExitCode of
// 128+n is how docker reports a process a signal ended, never guessed from stdout (contracts/
// executor.md: not_finished killed). Read after the tool's run and before the deferred removal;
// a container this call cannot inspect is not killed, the same as one that exited normally.
func (e *Executor) killed(ctx context.Context, name string) bool {
	out, err := child(ctx, e.docker(), "inspect", "--format", "{{.State.OOMKilled}} {{.State.ExitCode}}", name).Output()
	if err != nil {
		return false
	}
	oom, code, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	n, _ := strconv.Atoi(code)
	return oom == "true" || n >= 128
}

// stage writes what the tool reads into dir: tree/, rules/ (opengrep only) and <tool>.yaml when it reads one.
func stage(dir string, req ScanRequest, tool scanTool, parts *multipart.Reader) error {
	p, err := parts.NextPart()
	if err != nil || p.FormName() != "tree" {
		return fmt.Errorf("%w: second part must be tree", ErrRequest)
	}
	if err := untarPart(p, filepath.Join(dir, "tree")); err != nil {
		return err
	}
	switch p, err := parts.NextPart(); {
	case errors.Is(err, io.EOF) && req.Tool != "opengrep":
	case err != nil || p.FormName() != "rules" || req.Tool != "opengrep": // opengrep with no rules would find nothing
		return fmt.Errorf("%w: after tree, opengrep takes part rules and no other tool takes a part", ErrRequest)
	default:
		rules := &countingReader{r: io.LimitReader(p, maxRules+1)} // counted, not cut: a cut on a block boundary reads as a shorter archive
		err := untarPart(rules, filepath.Join(dir, "rules"))
		if _, _ = io.Copy(io.Discard, rules); rules.n > maxRules {
			return fmt.Errorf("%w: rules exceed %d bytes", ErrRequest, maxRules)
		}
		if err != nil {
			return err
		}
	}
	if req.Settings == nil { // a packages target: the tree as it came, no config
		return nil
	}
	if tool.config == nil { // each skip path, which skipPathRe keeps inside the tree, removed: a file, or a directory with all it holds
		for _, s := range req.Settings.SkipPaths {
			if err := os.RemoveAll(filepath.Join(dir, "tree", s)); err != nil && !errors.Is(err, syscall.ENOTDIR) { // under a file: nothing to remove
				return err
			}
		}
		return nil
	}
	return os.WriteFile(filepath.Join(dir, req.Tool+".yaml"), tool.config(*req.Settings), 0o644) //nolint:gosec // the tool, running as nobody, reads it; dir's parent is private
}

// untarPart is untar with the archive to blame for all but the executor's disk (full, quota, I/O).
func untarPart(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see stage
		return err
	}
	switch err := untar(r, dir); {
	case errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) || errors.Is(err, syscall.EIO):
		return err
	case err != nil:
		return fmt.Errorf("%w: %s: %v", ErrRequest, filepath.Base(dir), err)
	}
	return nil
}

// runScan creates the container, the tool's database volume mounted read-only at /scan/db, copies dir
// into its /scan — after fill, when there is one, writes into dir — and runs the tool; unless the
// database is missing, or was built more than dbMaxAge before started, when nothing is made.
func (e *Executor) runScan(ctx context.Context, name, dir, toolName string, tool scanTool, started time.Time, fill func(context.Context) error) ([]byte, time.Time, int, error) {
	var built time.Time
	if tool.db != nil {
		var err error
		if built, err = e.dbBuilt(ctx, toolName, tool); err != nil {
			return nil, built, 0, err
		}
		tool.db.read.Store(&built) // what GET /scandb answers (#2743)
		switch {
		case built.IsZero():
			return nil, built, 0, errDBMissing
		case started.Sub(built) > dbMaxAge:
			return nil, built, 0, errDBStale
		}
	}
	if fill != nil {
		if err := fill(ctx); err != nil {
			return nil, built, 0, err
		}
	}
	args := []string{"create", "--name", name, "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", scanMemory, "--pids-limit", "512", "--cpus", "2", "--tmpfs", "/tmp:mode=1777", "--label", "canyon.scan", "--volume", "/scan"}
	args = append(args, envFlags(tool.env)...)
	if tool.db != nil {
		args = append(args, "--volume", "canyon-scandb-"+toolName+":/scan/db:ro")
	}
	image, argv := []string{tool.image}, tool.args
	if tool.jsonOut { // its entrypoint under the image's sh, which prints the JSON output after the SARIF
		image, argv = []string{"--entrypoint", "/bin/sh", tool.image, "-c", ThenJSON, "sh"}, append(slices.Clone(tool.entrypoint), tool.args...)
	}
	if out, err := child(ctx, e.docker(), append(append(args, image...), argv...)...).CombinedOutput(); err != nil {
		return nil, built, 0, fmt.Errorf("docker create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := child(ctx, e.docker(), "cp", dir+"/.", name+":/scan").CombinedOutput(); err != nil {
		return nil, built, 0, fmt.Errorf("docker cp: %w: %s", err, strings.TrimSpace(string(out)))
	}
	cmd := child(ctx, e.docker(), "start", "-a", name)
	stdout, _ := cmd.StdoutPipe() // fails only when Stdout is set or the process started: neither
	if err := cmd.Start(); err != nil {
		return nil, built, 0, fmt.Errorf("docker start: %w", err)
	}
	out, err := kept(stdout, tool.jsonOut)
	if err != nil { // what Canyon keeps passed its cap
		_ = cmd.Process.Kill()
	}
	code := -1 // docker start -a exits as the tool did, which Scan reads for a tool with exits alone; -1, unknown, is no scan
	switch werr, exit := cmd.Wait(), (*exec.ExitError)(nil); {
	case werr == nil:
		code = 0
	case errors.As(werr, &exit):
		code = exit.ExitCode()
	}
	return out, built, code, err
}

// reread keeps when the tool's database was built, read from the database itself; a read that fails keeps what
// was read before.
func (e *Executor) reread(ctx context.Context, name string, tool scanTool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	built, err := e.dbBuilt(ctx, name, tool)
	if err != nil {
		log.Printf("executor: read %s database's age: %v", name, err)
		return
	}
	tool.db.read.Store(&built)
}

// ScanDBs is each vulnerability database's age, as the executor last read it (contracts/executor.md, GET /scandb;
// #2743), in tool order: none for a tool whose database it has not read, or found none.
func (e *Executor) ScanDBs(context.Context) ([]ScanDB, error) {
	var dbs []ScanDB
	for _, name := range slices.Sorted(maps.Keys(scanTools)) {
		if tool := scanTools[name]; tool.db != nil {
			var built time.Time
			if at := tool.db.read.Load(); at != nil {
				built = *at
			}
			dbs = append(dbs, ScanDBOf(name, built))
		}
	}
	return dbs, nil
}

// dbBuilt answers when the tool's database was built, the value under BuiltAt in the JSON its reader
// prints as the scan's user with the volume read-only at /db: trivy's db/metadata.json (head), grype's
// db status. Zero, and the tool never runs, when the reader exits 1 — the volume holds no database the
// scan's user can read, or docker's attach failed, which exits 1 too — or when what it prints does not
// parse; docker's other failures (125 to 127) are errors.
func (e *Executor) dbBuilt(ctx context.Context, name string, tool scanTool) (time.Time, error) {
	args := append([]string{"run", "--rm", "--network", "none", "--read-only", "--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--label", "canyon.scan", "--volume", "canyon-scandb-" + name + ":/db:ro"}, envFlags(tool.db.ReadEnv)...)
	if tool.db.ReadWith != "" {
		args = append(args, "--entrypoint", tool.db.ReadWith)
	}
	var stderr strings.Builder
	cmd := child(ctx, e.docker(), append(append(args, tool.image), tool.db.Read...)...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.ExitCode() == 1 {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("docker run %s database reader: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	var meta map[string]json.RawMessage
	var built time.Time
	if json.Unmarshal(out, &meta) != nil || json.Unmarshal(meta[tool.db.BuiltAt], &built) != nil {
		return time.Time{}, nil
	}
	return built, nil
}

// startRefresh starts the tool's refresh for a scan that found no database, unless the one a scan
// started still runs. It outlives the scan, which answers busy; whether it failed is the next scan's.
func (e *Executor) startRefresh(ctx context.Context, name string, tool scanTool) {
	if !tool.db.refreshing.CompareAndSwap(false, true) {
		return
	}
	e.collecting.Go(func() {
		err := e.refreshDB(context.WithoutCancel(ctx), name, tool)
		if err != nil {
			log.Printf("executor: refresh %s database: %v", name, err)
		}
		tool.db.failed.Store(err != nil)
		tool.db.refreshing.Store(false)
	})
}

// RefreshDBs refreshes the database of each tool whose volume exists — one a scan asked for — now, then
// every dbEvery, one refresh at a time, until ctx is done: the executor's own schedule. A refresh that
// fails is logged.
func (e *Executor) RefreshDBs(ctx context.Context) {
	tick := time.NewTicker(dbEvery)
	defer tick.Stop()
	for {
		// by name: a scan's docker create makes the volume, unlabelled, before its first refresh runs
		out, err := child(ctx, e.docker(), "volume", "ls", "--quiet", "--filter", "name=canyon-scandb-").Output()
		if err != nil {
			log.Printf("executor: list the database volumes: %v", err)
		}
		volumes := strings.Fields(string(out))
		for name, tool := range scanTools {
			if tool.db == nil || !slices.Contains(volumes, "canyon-scandb-"+name) {
				continue
			}
			if err := e.refreshDB(ctx, name, tool); err != nil {
				log.Printf("executor: refresh %s database: %v", name, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// refreshDB runs the tool's pinned image with network to download its database into the volume
// canyon-scandb-<tool>, holding the tool's lock alone: no scan reads a database half-written. The
// volume is labelled canyon.scandb, never canyon.scan, so the sweep keeps it; the containers are
// labelled canyon.scan, so the sweep removes one a crash left.
func (e *Executor) refreshDB(ctx context.Context, name string, tool scanTool) error {
	tool.db.lock.Lock()
	defer tool.db.lock.Unlock()
	defer e.reread(ctx, name, tool) // under the lock, whatever the refresh did: the age is the database's own (#2743)
	ctx, cancel := context.WithTimeout(ctx, dbDeadline)
	defer cancel()
	volume := "canyon-scandb-" + name
	if out, err := child(ctx, e.docker(), "volume", "create", "--label", "canyon.scandb", volume).CombinedOutput(); err != nil {
		return fmt.Errorf("docker volume create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var user []string
	if tool.db.scanUser {
		if err := e.ownVolume(ctx, volume, tool.image); err != nil {
			return err
		}
		user = []string{"--user", "65534:65534"}
	}
	if tool.db.Partials != "" {
		if err := e.sweepDB(ctx, volume, tool.db.Partials, user); err != nil {
			return err
		}
	}
	ctr := "canyon-scandb-refresh-" + nonce()
	defer e.remove(ctx, ctr)
	args := append([]string{"run", "--name", ctr, "--label", "canyon.scan", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "2g", "--pids-limit", "512", "--cpus", "2", "--tmpfs", "/tmp:mode=1777", "--volume", volume + ":/db"}, user...)
	args = append(args, envFlags(tool.db.Env)...)
	if tool.db.RefreshWith != "" {
		args = append(args, "--entrypoint", tool.db.RefreshWith)
	}
	args = append(append(args, tool.image), tool.db.Refresh...)
	if out, err := child(ctx, e.docker(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sweepDB removes what matches glob in the volume, from the executor's busybox without network, as user: the
// tool's own image may have no shell. Failing, the refresh does not run, and the database stays as it was.
func (e *Executor) sweepDB(ctx context.Context, volume, glob string, user []string) error {
	ctr := "canyon-scandb-sweep-" + nonce()
	defer e.remove(ctx, ctr)
	args := append([]string{"run", "--name", ctr, "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", "2g", "--pids-limit", "512", "--cpus", "2", "--label", "canyon.scan", "--volume", volume + ":/db"}, user...)
	args = append(args, cmp.Or(e.Busybox, busyboxImage), "sh", "-c", "rm -rf /db/"+glob)
	if out, err := child(ctx, e.docker(), args...).CombinedOutput(); err != nil {
		return fmt.Errorf("sweep %s: docker run: %w: %s", volume, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ownVolume makes the volume's root the scan's user's (65534): docker cp -a writes the owner of a tar's
// db/ entry onto a created container's mount at /db. The container is never started.
func (e *Executor) ownVolume(ctx context.Context, volume, image string) error {
	ctr := "canyon-scandb-owner-" + nonce()
	defer e.remove(ctx, ctr) // by name: a create the deadline cut short prints no id
	if out, err := child(ctx, e.docker(), "create", "--name", ctr, "--label", "canyon.scan", "--volume", volume+":/db", image).CombinedOutput(); err != nil {
		return fmt.Errorf("docker create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var root bytes.Buffer
	tw := tar.NewWriter(&root)
	_ = tw.WriteHeader(&tar.Header{Name: "db/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 65534, Gid: 65534}) // into memory: nothing to fail on
	_ = tw.Close()
	cmd := child(ctx, e.docker(), "cp", "-a", "-", ctr+":/")
	cmd.Stdin = &root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker cp: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// envFlags is docker's --env for each KEY=value.
func envFlags(env []string) []string {
	var flags []string
	for _, v := range env {
		flags = append(flags, "--env", v)
	}
	return flags
}

// remove removes a container this executor created and its anonymous volumes, bounded like /run's
// cleanup and past the caller's deadline: a /scan holds its slot of the shared limit until it returns.
func (e *Executor) remove(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if out, err := child(ctx, e.docker(), "rm", "-f", "-v", name).CombinedOutput(); err != nil {
		log.Printf("executor: docker rm %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// splitJSON is a tool's stdout, for one that runs under ThenJSON, cut at its first NUL — JSON text holds
// none — into the SARIF before it and, from the JSON output after it, the distinct rule_ids, in name
// order, of its Rule parse errors ("" for one without), and the distinct paths, in path order, of its
// errors that carry one (contracts/executor.md, unloaded and unparsed); whole is false when there is no
// NUL or what follows is not JSON with errors. Another tool's stdout is all SARIF, and whole.
func splitJSON(out []byte, jsonOut bool) (sarif []byte, unloaded, unparsed []string, whole bool) {
	if !jsonOut {
		return out, nil, nil, true
	}
	sarif, after, cut := bytes.Cut(out, []byte{0})
	var doc struct {
		Errors []struct {
			Type   any    `json:"type"` // a string, or a list for some types
			RuleID string `json:"rule_id"`
			Path   string `json:"path"`
		} `json:"errors"`
	}
	if !cut || json.Unmarshal(after, &doc) != nil || doc.Errors == nil {
		return sarif, nil, nil, false
	}
	for _, e := range doc.Errors {
		if e.Type == "Rule parse error" {
			unloaded = append(unloaded, e.RuleID)
		}
		if e.Path != "" {
			unparsed = append(unparsed, e.Path)
		}
	}
	return sarif, slices.Compact(slices.Sorted(slices.Values(unloaded))), slices.Compact(slices.Sorted(slices.Values(unparsed))), true
}

// sarifOf is out as one SARIF 2.1.0 run, results' suppressions removed, and a suppressed result's level
// with them: the tree never steers its scan. Each result's and invocation notification's uris that
// start with one of uriRoots come back without it, as the package's paths.
func sarifOf(out []byte, uriRoots []string) (json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	var runs []map[string]json.RawMessage
	if json.Unmarshal(out, &doc) != nil || string(doc["version"]) != `"2.1.0"` || json.Unmarshal(doc["runs"], &runs) != nil || len(runs) != 1 {
		return nil, false
	}
	var results []map[string]json.RawMessage
	if json.Unmarshal(runs[0]["results"], &results) == nil {
		for _, r := range results {
			var s []json.RawMessage
			if json.Unmarshal(r["suppressions"], &s) == nil && len(s) > 0 {
				delete(r, "level") // the tool's level for a result it skips (checkov: warning): the rule's default stands
			}
			delete(r, "suppressions")
			if l, ok := r["locations"]; ok && uriRoots != nil {
				r["locations"] = treeURIs(l, uriRoots)
			}
		}
		runs[0]["results"], _ = json.Marshal(results)
	}
	var invocations []map[string]json.RawMessage
	if uriRoots != nil && json.Unmarshal(runs[0]["invocations"], &invocations) == nil {
		for _, inv := range invocations {
			for _, kind := range []string{"toolConfigurationNotifications", "toolExecutionNotifications"} {
				var notes []map[string]json.RawMessage
				if json.Unmarshal(inv[kind], &notes) != nil {
					continue
				}
				for _, n := range notes {
					if l, ok := n["locations"]; ok {
						n["locations"] = treeURIs(l, uriRoots)
					}
				}
				inv[kind], _ = json.Marshal(notes)
			}
		}
		runs[0]["invocations"], _ = json.Marshal(invocations)
	}
	doc["runs"], _ = json.Marshal(runs)
	b, err := json.Marshal(doc)
	return b, err == nil
}

// treeURIs is locations with each artifactLocation.uri that starts with one of roots cut after the first
// such root; any other uri stays as it is, for the attach to drop and count.
func treeURIs(locations json.RawMessage, roots []string) json.RawMessage {
	var locs []map[string]any
	if json.Unmarshal(locations, &locs) != nil {
		return locations
	}
	for _, l := range locs {
		p, _ := l["physicalLocation"].(map[string]any)
		a, _ := p["artifactLocation"].(map[string]any)
		uri, _ := a["uri"].(string)
		for _, root := range roots {
			if rest, ok := strings.CutPrefix(uri, root); ok {
				a["uri"] = rest
				break
			}
		}
	}
	b, _ := json.Marshal(locs) // decoded from JSON: nothing to fail on
	return b
}
