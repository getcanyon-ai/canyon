package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// Client speaks contracts/executor.md to one executor: the control plane's side of the seam.
// Base is the executor's URL (CANYON_EXECUTOR_URL); HTTP defaults to http.DefaultClient.
type Client struct {
	Base string
	HTTP *http.Client
}

// BuildFailed is a build the executor ran and refused: the last line of the stream was an
// error, so the package did not build — the user's fix, not Canyon's (contracts/routes.md
// row 62: failure_reason build). Anything else Build returns is Canyon's own failure.
type BuildFailed struct{ Message string }

func (e *BuildFailed) Error() string { return e.Message }

// HostFailed is a build the executor's own host failed before the package's steps ran (contracts/executor.md, The
// executor's own failure): the line it streamed with own. Canyon's failure, never the package's, so the run ends
// failure_reason error, never build.
type HostFailed struct{ Message string }

func (e *HostFailed) Error() string { return e.Message }

// Undetected is a build no buildpack group of the builder detected: nothing was compiled, so the run
// ends failure_reason detect, never build. Builds is what the builder builds, its order's buildpack
// ids; never nil, so an order that names none still says so (contracts/executor.md, /build).
type Undetected struct {
	Builds  []string
	Errored bool // the detect exited 21: a buildpack errored while detecting, so a malformed manifest is possible
}

// undetectedLine is a failed detect's message, exactly (contracts/executor.md, /build): an error line
// carrying builds and any other message is a failed build.
const undetectedLine = "no buildpack group passed detection"

func (e *Undetected) Error() string { return undetectedLine }

// NoRoom is a build the cluster had no room to run: its pod still unscheduled at the build's deadline,
// Reason the scheduler's own message — capacity, an admin's to add, never the package's nor Canyon's
// (contracts/executor.md, /build; #1637).
type NoRoom struct{ Reason string }

// noRoomLine is a no-room build's message, exactly.
const noRoomLine = "the cluster had no room to run the build"

func (e *NoRoom) Error() string { return noRoomLine }

// buildName is the shape of what the contract names a buildpack, at most maxBuilds of them: a name is
// the builder's text and reaches a terminal, so only printable ASCII is named, never a control or bidi
// character or a run-on name.
var buildName = regexp.MustCompile(`^[A-Za-z0-9][ -~]{0,127}$`)

const maxBuilds = 64

// MetadataLabel is a builder image's label naming its buildpacks, {buildpacks: [{id, name, …}]}.
const MetadataLabel = "io.buildpacks.builder.metadata"

// NamesOf is a builder's metadata label as each buildpack's name by its id, the first named for an id.
func NamesOf(metadata []byte) (map[string]string, error) {
	var m struct {
		Buildpacks []struct{ ID, Name string } `json:"buildpacks"`
	}
	if len(bytes.TrimSpace(metadata)) == 0 {
		return nil, fmt.Errorf("the builder has no %s label", MetadataLabel)
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return nil, fmt.Errorf("the builder's %s label: %w", MetadataLabel, err)
	}
	names := map[string]string{}
	for _, bp := range m.Buildpacks {
		if _, ok := names[bp.ID]; !ok && bp.Name != "" {
			names[bp.ID] = bp.Name
		}
	}
	if len(names) == 0 && len(m.Buildpacks) > 0 { // a kpack ClusterBuilder's image names none (executor.md): the caller logs this
		return nil, errors.New("the builder image's metadata has no buildpack names; showing ids")
	}
	return names, nil
}

// BuildsOf is a builder's order — the image label io.buildpacks.buildpack.order on Docker, the
// ClusterBuilder's status.order on Kubernetes, both [{group: [{id, optional?}]}] — as what the contract
// names its buildpacks: in order, optional ones left out, each by its name in names when that is of the
// contract's shape, else by its id when that is, else not at all; each once, at most maxBuilds. An empty
// order names none.
func BuildsOf(order []byte, names map[string]string) ([]string, error) {
	var groups []struct {
		Group []struct {
			ID       string `json:"id"`
			Optional bool   `json:"optional"`
		} `json:"group"`
	}
	if len(bytes.TrimSpace(order)) > 0 {
		if err := json.Unmarshal(order, &groups); err != nil {
			return nil, fmt.Errorf("the builder's order: %w", err)
		}
	}
	builds := []string{}
	for _, g := range groups {
		for _, bp := range g.Group {
			named := names[bp.ID]
			if !buildName.MatchString(named) {
				named = bp.ID
			}
			if !bp.Optional && buildName.MatchString(named) && !slices.Contains(builds, named) && len(builds) < maxBuilds {
				builds = append(builds, named)
			}
		}
	}
	return builds, nil
}

// Buildpack is one buildpack a build ran (contracts/executor.md, The buildpacks that ran): its id, and
// its name where the builder's metadata gives one.
type Buildpack struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// buildpackID is a CNB buildpack id: letters, digits, '.', '/' and '-'.
var buildpackID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9./-]{0,127}$`)

// RanOf is the group a build ran — [{id, …}]: the app image's io.buildpacks.build.metadata buildpacks on
// Docker, a kpack Build's status.buildMetadata on Kubernetes — as the contract names it: in order, each
// once, named by names where the name is of the contract's shape. A group with an id of another shape, or
// of more than maxBuilds, is none and an error: a list missing what ran would be judged as if it were all.
// Never nil.
func RanOf(group []byte, names map[string]string) ([]Buildpack, error) {
	ran := []Buildpack{}
	var bps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(group, &bps); err != nil {
		return ran, fmt.Errorf("the buildpacks that ran: %w", err)
	}
	for _, bp := range bps {
		if !buildpackID.MatchString(bp.ID) {
			return []Buildpack{}, fmt.Errorf("the buildpacks that ran: an id %q is not a buildpack id", bp.ID)
		}
		if slices.ContainsFunc(ran, func(r Buildpack) bool { return r.ID == bp.ID }) {
			continue
		}
		if len(ran) == maxBuilds {
			return []Buildpack{}, fmt.Errorf("the buildpacks that ran: more than %d", maxBuilds)
		}
		r := Buildpack{ID: bp.ID}
		if buildName.MatchString(names[bp.ID]) {
			r.Name = names[bp.ID]
		}
		ran = append(ran, r)
	}
	return ran, nil
}

// upload is a request to path with the multipart body /build and /scan take, json then tree, then rules
// when not nil, never buffered. Close its body when the call ends: whatever ends it early, the writer
// fails instead of blocking forever.
func (c *Client) upload(ctx context.Context, path string, req any, tree, rules io.Reader) (*http.Request, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			part, err := mw.CreateFormField("json")
			if err != nil {
				return err
			}
			if err := json.NewEncoder(part).Encode(req); err != nil {
				return err
			}
			part, err = mw.CreateFormFile("tree", "tree.tar")
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, tree); err != nil {
				return err
			}
			if rules != nil {
				if part, err = mw.CreateFormFile("rules", "rules.tar"); err != nil {
					return err
				}
				if _, err := io.Copy(part, rules); err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(err) // a nil closes the pipe cleanly; the executor sees the end of the body
	}()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, pr)
	if err != nil {
		_ = pr.Close()
		return nil, err
	}
	hreq.Header.Set("Content-Type", mw.FormDataContentType())
	return hreq, nil
}

// Build streams the tree into POST /build and hands each build-log line to line as it arrives.
// The result is the last line.
func (c *Client) Build(ctx context.Context, req BuildRequest, tree io.Reader, line func(context.Context, string) error) (BuildResult, error) {
	hreq, err := c.upload(ctx, "/build", req, tree, nil)
	if err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = hreq.Body.Close() }()
	resp, err := c.http().Do(hreq)
	if err != nil {
		return BuildResult{}, gone(fmt.Errorf("executor: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode == http.StatusServiceUnavailable && err == nil && s >= 0 {
		return BuildResult{}, &Busy{RetryAfter: time.Duration(s) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		return BuildResult{}, fmt.Errorf("executor: /build answered %d%s", resp.StatusCode, message(resp.Body))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 6*maxLine+1<<10) // the executor caps a raw line at maxLine; JSON escaping can grow it six-fold
	for sc.Scan() {
		var row struct {
			Line    *string   `json:"line"`
			Error   *string   `json:"error"`
			Builds  *[]string `json:"builds"`
			Errored bool      `json:"errored"`
			Own     bool      `json:"own"`
			NoRoom  *string   `json:"no_room"`
			Image   string    `json:"image"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return BuildResult{}, fmt.Errorf("executor: /build stream: %w", err)
		}
		switch {
		case row.Line != nil:
			if err := line(ctx, *row.Line); err != nil {
				return BuildResult{}, err
			}
		case row.Error != nil && *row.Error == noRoomLine && row.NoRoom != nil:
			return BuildResult{}, &NoRoom{Reason: *row.NoRoom}
		case row.Error != nil && *row.Error == undetectedLine && row.Builds != nil:
			return BuildResult{}, &Undetected{Builds: *row.Builds, Errored: row.Errored}
		case row.Error != nil && row.Own:
			return BuildResult{}, &HostFailed{Message: *row.Error}
		case row.Error != nil:
			return BuildResult{}, &BuildFailed{Message: *row.Error}
		case row.Image != "":
			var res BuildResult
			if err := json.Unmarshal(sc.Bytes(), &res); err != nil {
				return BuildResult{}, err
			}
			return res, nil
		default: // a row the contract does not name ends the build, never skipped: what follows it is unread (#728)
			return BuildResult{}, errors.New("executor: /build stream: a row with no line, error or image")
		}
	}
	if err := sc.Err(); err != nil {
		return BuildResult{}, gone(fmt.Errorf("executor: /build stream: %w", err))
	}
	return BuildResult{}, &Gone{Err: errors.New("executor: /build ended without a result")} // the stream closed before its final status
}

// Gone is a build or a scan the executor went away under, before its answer: the connection refused, its dial
// timed out or reset, or the answer cut short or closed — what a restart, an upgrade or a node drain does to a
// call in flight (#2671, #248). Nothing of it is left to read; the same call may be asked again once the
// executor is back.
type Gone struct{ Err error }

func (g *Gone) Error() string { return g.Err.Error() }
func (g *Gone) Unwrap() error { return g.Err }

// gone is err as Gone when it is the executor going away, else err. A dial that failed reached no executor,
// whatever stopped it; io.EOF is a connection closed before any answer.
func gone(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return &Gone{Err: err}
	}
	return err
}

// Busy is the executor's 503 busy on /build or /scan: ask again once RetryAfter has passed. Reason is
// "database" for a scan waiting on its tool's database, "" past the shared limit.
type Busy struct {
	RetryAfter time.Duration
	Reason     string
}

func (e *Busy) Error() string { return fmt.Sprintf("executor: busy, ask again in %s", e.RetryAfter) }

// Scan streams the tree into POST /scan as Build does, then rules, opengrep's rule files as a tar (nil
// for any other tool), and answers the tool's SARIF, or why there is none. A 503 with a Retry-After in
// seconds is *Busy, with the body's reason; the executor gone before its answer is *Gone; any other answer
// but a 200 is an error.
func (c *Client) Scan(ctx context.Context, req ScanRequest, tree, rules io.Reader) (ScanResult, error) {
	hreq, err := c.upload(ctx, "/scan", req, tree, rules)
	if err != nil {
		return ScanResult{}, err
	}
	defer func() { _ = hreq.Body.Close() }()
	resp, err := c.http().Do(hreq)
	if err != nil {
		return ScanResult{}, gone(fmt.Errorf("executor: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode == http.StatusServiceUnavailable && err == nil && s >= 0 {
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<10)).Decode(&body) // a body without one is a plain busy
		return ScanResult{}, &Busy{RetryAfter: time.Duration(s) * time.Second, Reason: body.Reason}
	}
	if resp.StatusCode != http.StatusOK {
		return ScanResult{}, fmt.Errorf("executor: /scan answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res ScanResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 6*maxScanOut+1<<20)).Decode(&res); err != nil { // the SARIF the executor re-encodes: escaping can grow it six-fold
		return ScanResult{}, gone(fmt.Errorf("executor: /scan: %w", err))
	}
	return res, nil
}

// Run asks the executor to start the image as <app>-<target> on the app network.
func (c *Client) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RunResult{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/run", bytes.NewReader(body))
	if err != nil {
		return RunResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return RunResult{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusBadGateway { // an app that did not stay up carries its last lines (contracts/executor.md, /run)
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var exited struct {
			Error  string  `json:"error"`
			Output *string `json:"output"`
		}
		if json.Unmarshal(raw, &exited) == nil && exited.Error != "" {
			if exited.Output != nil {
				return RunResult{}, &Exited{Reason: exited.Error, Output: *exited.Output}
			}
			return RunResult{}, &Answered{Path: "/run", Status: http.StatusBadGateway, Message: exited.Error} // whole: a sidecar's last words end it (#1790)
		}
		return RunResult{}, answerOf("/run", resp.StatusCode, bytes.NewReader(raw))
	}
	if resp.StatusCode == http.StatusBadRequest {
		return RunResult{}, fmt.Errorf("%w (%w)", answerOf("/run", http.StatusBadRequest, resp.Body), ErrRequest)
	}
	if resp.StatusCode == http.StatusNotFound { // the image is not on this host (contracts/executor.md, /run)
		return RunResult{}, fmt.Errorf("executor: /run: %w%s", ErrNoImage, message(resp.Body))
	}
	if resp.StatusCode != http.StatusOK {
		return RunResult{}, answerOf("/run", resp.StatusCode, resp.Body)
	}
	var res RunResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return RunResult{}, fmt.Errorf("executor: /run: %w", err)
	}
	return res, nil
}

// BuilderImage asks which builder a /build now builds with (contracts/executor.md, GET /builder): ""
// from an executor that predates the call (404), so the caller reuses nothing and records nothing.
func (c *Client) BuilderImage(ctx context.Context) (string, error) {
	builder, _, err := c.Builder(ctx)
	return builder, err
}

// Builder is BuilderImage, and how long the answer waited for Canyon's own images to be built first:
// zero when it did not, or the executor does not say (contracts/executor.md, waited_ms; #2711).
func (c *Client) Builder(ctx context.Context) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/builder", nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("executor: /builder answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		Builder  string
		WaitedMs int64 `json:"waited_ms"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&res); err != nil || res.Builder == "" {
		return "", 0, fmt.Errorf("executor: /builder: no builder (%v)", err)
	}
	return res.Builder, time.Duration(max(res.WaitedMs, 0)) * time.Millisecond, nil
}

// ScanDBs asks how old each vulnerability database is (contracts/executor.md, GET /scandb; #2743): none from an
// executor before the call (404).
func (c *Client) ScanDBs(ctx context.Context) ([]ScanDB, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/scandb", nil) //nolint:gosec // c.Base is the executor the operator configured, never a caller's
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req) //nolint:gosec // as above
	if err != nil {
		return nil, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("executor: /scandb answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		Databases []ScanDB `json:"databases"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&res); err != nil {
		return nil, fmt.Errorf("executor: /scandb: %w", err)
	}
	return res.Databases, nil
}

// Stop asks the executor to remove <app>-<env>; a container already gone is success.
func (c *Client) Stop(ctx context.Context, app, env string) error {
	body, err := json.Marshal(StopRequest{App: app, Target: env})
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/stop", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("executor: /stop answered %d%s", resp.StatusCode, message(resp.Body))
	}
	return nil
}

// Forget asks the executor to remove the app's images and build caches; a 409 means a container of it still exists.
func (c *Client) Forget(ctx context.Context, app string) error {
	body, err := json.Marshal(ForgetRequest{App: app})
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/forget", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("executor: /forget answered %d%s", resp.StatusCode, message(resp.Body))
	}
	return nil
}

// PutAccess rewrites the access file of <app>-<env> (Kubernetes only): a 404 is ErrNoContainer, nothing
// runs there, and a 409 ErrStale, the ConfigMap already holds a newer file.
func (c *Client) PutAccess(ctx context.Context, app, env string, access json.RawMessage) error {
	body, err := json.Marshal(map[string]any{"app": app, "target": env, "access": access})
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Base+"/access", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s-%s", ErrNoContainer, app, env)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s-%s", ErrStale, app, env)
	}
	return fmt.Errorf("executor: /access answered %d%s", resp.StatusCode, message(resp.Body))
}

// Logs opens the log stream of <app>-<env>: the last tail lines, then, when follow is set, live
// until the caller closes the body or ctx ends. A 404 is ErrNoContainer, the sentinel the executor itself
// returns, so one errors.Is serves both sides of the seam. An executor too old to serve /logs
// answers the mux's 404 and reads the same way; the two binaries ship from one compose file, so
// that is a partial deploy, and the server logs the detail either way.
func (c *Client) Logs(ctx context.Context, app, env string, tail int, follow bool) (io.ReadCloser, error) {
	q := url.Values{"app": {app}, "target": {env}, "tail": {strconv.Itoa(tail)}, "follow": {strconv.FormatBool(follow)}}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("executor: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s-%s", ErrNoContainer, app, env)
	}
	return nil, fmt.Errorf("executor: /logs answered %d%s", resp.StatusCode, message(resp.Body))
}

// ProbeResult is POST /probe's answer (contracts/executor.md, Kubernetes only).
type ProbeResult struct {
	NetworkPolicy string `json:"network_policy"` // enforced | not_enforced
	Certificate   string `json:"certificate"`    // ok | missing
	Fix           string `json:"fix"`            // "" exactly when enforced and ok, else the sentences to show verbatim
	Warning       string `json:"warning"`        // the node ports an app reaches on its own server, never a refusal; "" when none
}

// Probe asks whether this cluster keeps an app's port behind its sign-in and whether people reach the
// addresses domain and form give. Any answer but a 200 carrying the contract's shape is an error: the
// Docker executor routes no /probe, so it answers the mux's 404.
func (c *Client) Probe(ctx context.Context, domain, form string) (ProbeResult, error) {
	body, err := json.Marshal(map[string]string{"domain": domain, "form": form})
	if err != nil {
		return ProbeResult{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/probe", bytes.NewReader(body))
	if err != nil {
		return ProbeResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ProbeResult{}, fmt.Errorf("executor: /probe answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res ProbeResult
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&res); err != nil {
		return ProbeResult{}, fmt.Errorf("executor: /probe: %w", err)
	}
	passed := res.NetworkPolicy == "enforced" && res.Certificate == "ok"
	if res.NetworkPolicy != "enforced" && res.NetworkPolicy != "not_enforced" || res.Certificate != "ok" && res.Certificate != "missing" || passed != (res.Fix == "") {
		return ProbeResult{}, fmt.Errorf("executor: /probe answered outside the contract: network_policy %q, certificate %q, fix %d bytes", res.NetworkPolicy, res.Certificate, len(res.Fix))
	}
	return res, nil
}

// Platform is the platform an image runs on, its config's own (contracts/executor.md, GET /platform): os linux and
// its architecture, never the variant.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// platformArch is executor.schema.json's platform_result architecture pattern, verbatim.
var platformArch = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// Platform asks the platform image runs on, read by its digest as a Kubernetes /run reads it. Any answer but a
// 200 carrying the contract's shape is an error: the Docker executor routes no /platform, so it answers the
// mux's 404, and a hand-off release is never sent naming a platform the executor did not read.
func (c *Client) Platform(ctx context.Context, image string) (Platform, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/platform?"+url.Values{"image": {image}}.Encode(), nil)
	if err != nil {
		return Platform{}, err
	}
	resp, err := c.http().Do(hreq)
	if err != nil {
		return Platform{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		var gone struct{ Error, Reason string }
		if json.Unmarshal(raw, &gone) == nil && gone.Reason == "image_gone" && gone.Error != "" { // the registry has no such image (platform_gone)
			return Platform{}, fmt.Errorf("executor: /platform: %w: %s", ErrNoImage, gone.Error)
		}
		return Platform{}, answerOf("/platform", http.StatusNotFound, bytes.NewReader(raw)) // an executor without the call
	}
	if resp.StatusCode != http.StatusOK {
		return Platform{}, answerOf("/platform", resp.StatusCode, resp.Body)
	}
	var res Platform
	dec := json.NewDecoder(io.LimitReader(resp.Body, 4<<10))
	dec.DisallowUnknownFields()
	err = dec.Decode(&res)
	if err == nil {
		if after := dec.Decode(&struct{}{}); after != io.EOF {
			err = fmt.Errorf("something after the object: %v", after)
		}
	}
	if err != nil {
		return Platform{}, fmt.Errorf("executor: /platform answered out of the contract: one JSON object of its keys alone: %w", err)
	}
	if res.OS != "linux" || !platformArch.MatchString(res.Architecture) {
		return Platform{}, fmt.Errorf("executor: /platform answered outside the contract: os %q, architecture %q", res.OS, res.Architecture)
	}
	return res, nil
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// message is ": " and the executor's {"error": …} text, or nothing when the body is not that.
func message(r io.Reader) string {
	if e := errorOf(r); e != "" {
		return ": " + e
	}
	return ""
}

// errorOf is the executor's own error text in an answer's body, "" when the body is not the executor's error: a
// run's journal keeps it, and a proxy's page or anything else stays out of it (#728).
func errorOf(r io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return ""
	}
	return e.Error
}

// Answered is an executor's answer that is not the one asked for: Message is the executor's own error text, "" when
// the body was not its error. A deploy's failure line says Message alone (contracts/events.md, build.log, #2539);
// Error is the whole, as the journal has always kept it.
type Answered struct {
	Path    string
	Status  int
	Message string
}

func (a *Answered) Error() string {
	if a.Message == "" {
		return fmt.Sprintf("executor: %s answered %d", a.Path, a.Status)
	}
	return fmt.Sprintf("executor: %s answered %d: %s", a.Path, a.Status, a.Message)
}

func answerOf(path string, status int, body io.Reader) *Answered {
	return &Answered{Path: path, Status: status, Message: errorOf(body)}
}

// RunningOf answers those of containers that run, in the order asked, and, asked why, why those that
// have exited stopped: one call for a read that shows many environments. An older executor's answer
// says no why.
func (c *Client) RunningOf(ctx context.Context, containers []Container, why bool) (Runs, error) {
	body, err := json.Marshal(map[string]any{"containers": containers, "why": why})
	if err != nil {
		return Runs{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/running", bytes.NewReader(body))
	if err != nil {
		return Runs{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return Runs{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Runs{}, fmt.Errorf("executor: POST /running answered %d%s", resp.StatusCode, message(resp.Body))
	}
	limit := int64(1 << 20)
	if why { // a line of at most SaidBytes for each asked, each byte at most six escaped, and its names
		limit = int64(len(containers)) * (6*SaidBytes + 256)
	}
	var res Runs
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(&res); err != nil {
		return Runs{}, fmt.Errorf("executor: POST /running: %w", err)
	}
	return res, nil
}

// Callers asks who holds each address now, at most 1,024 (contracts/executor.md, POST /callers). An
// executor from before the call answers 404, which names no one: every address is unknown.
func (c *Client) Callers(ctx context.Context, addresses []string) ([]Caller, error) {
	body, err := json.Marshal(map[string]any{"addresses": addresses})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/callers", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("executor: POST /callers answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		Callers []Caller `json:"callers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return nil, fmt.Errorf("executor: POST /callers: %w", err)
	}
	return res.Callers, nil
}

// sidecarRoll is executor.schema.json's sidecars_result roll pattern, verbatim.
var sidecarRoll = regexp.MustCompile(`^[0-9a-f]{8}$`)

// Sidecars asks the Kubernetes executor to roll every app's canyon-identity onto its own image, and which
// environments a roll has moved onto it (contracts/executor.md, POST /sidecars). Anything but 200 is an
// error: an executor from before the call, or the Docker executor, answers 404, and nothing is rolled. So
// is an answer out of the contract's shape — a key it does not name or lacks, anything after it, an image
// not by digest, an environment out of shape or named twice: nothing it says is recorded.
func (c *Client) Sidecars(ctx context.Context) (Sidecars, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/sidecars", nil)
	if err != nil {
		return Sidecars{}, err
	}
	resp, err := c.http().Do(hreq)
	if err != nil {
		return Sidecars{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Sidecars{}, fmt.Errorf("executor: POST /sidecars answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		Image  *string        `json:"image"`
		Rolled *[]SidecarRoll `json:"rolled"`
		Failed *int           `json:"failed"`
	}
	// ponytail: 1 MiB holds about 10,000 rolled environments, each answered until its next /run; an
	// answer past it is cut and refused, so nothing is recorded. A fleet that size takes a cursor on POST
	// /sidecars, or an answer of the rolls the server has not recorded.
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.DisallowUnknownFields()
	err = dec.Decode(&res)
	if err == nil {
		if after := dec.Decode(&struct{}{}); after != io.EOF {
			err = fmt.Errorf("something after the object: %v", after)
		}
	}
	if err != nil {
		return Sidecars{}, fmt.Errorf("executor: POST /sidecars answered out of the contract: one JSON object of its keys alone: %w", err)
	}
	if res.Image == nil || res.Rolled == nil || res.Failed == nil || *res.Failed < 0 {
		return Sidecars{}, errors.New("executor: POST /sidecars answered out of the contract: image, rolled and failed, failed at least 0")
	}
	if err := ByDigest(*res.Image); err != nil {
		return Sidecars{}, fmt.Errorf("executor: POST /sidecars answered out of the contract: image %q is not by digest: %w", *res.Image, err)
	}
	for i, r := range *res.Rolled {
		if !validApp(r.App) || (r.Target != "preview" && r.Target != "prod") || !sidecarRoll.MatchString(r.Roll) ||
			slices.ContainsFunc((*res.Rolled)[:i], func(o SidecarRoll) bool { return o.App == r.App && o.Target == r.Target }) {
			return Sidecars{}, fmt.Errorf("executor: POST /sidecars answered out of the contract: rolled[%d] is not an app's preview or prod, named once, with its roll", i)
		}
	}
	return Sidecars{Image: *res.Image, Rolled: *res.Rolled, Failed: *res.Failed}, nil
}

// RecreateNetwork asks the executor to make the app's network internal, or open, leaving its containers
// stopped (contracts/executor.md, POST /networks/recreate). Anything but 204 is an error: an executor from
// before the call answers 404, and the network is not recreated.
func (c *Client) RecreateNetwork(ctx context.Context, app string, internal bool) error {
	body, err := json.Marshal(RecreateRequest{App: app, Internal: internal})
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/networks/recreate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("executor: POST /networks/recreate answered %d%s", resp.StatusCode, message(resp.Body))
	}
	return nil
}

// Running answers the image <app>-<env> runs, or ErrNoContainer when nothing runs there.
func (c *Client) Running(ctx context.Context, app, env string) (string, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/running?"+url.Values{"app": {app}, "target": {env}}.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http().Do(hreq)
	if err != nil {
		return "", fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: %s-%s", ErrNoContainer, app, env)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("executor: /running answered %d%s", resp.StatusCode, message(resp.Body))
	}
	var res struct {
		Image string `json:"image"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
		return "", fmt.Errorf("executor: /running: %w", err)
	}
	return res.Image, nil
}
