package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
)

// RebaseRequest is the body of POST /rebase.
type RebaseRequest struct {
	App      string `json:"app"`
	Package  string `json:"package"`
	Image    string `json:"image"`     // the image an environment runs, by digest
	RunImage string `json:"run_image"` // GET /builder's run_image, the one the server's plan named
}

// RebaseFacts are what the server compares to prove that only the run image changed (contracts/executor.md,
// POST /rebase): the executor reads them and judges none. BuildMetadata and Config are digests, so no value
// of the image's environment leaves the executor.
type RebaseFacts struct {
	DiffIDs       []string `json:"diff_ids"`
	TopLayer      string   `json:"top_layer"`
	RunImage      string   `json:"run_image"`
	StackID       string   `json:"stack_id"`
	BuildMetadata string   `json:"build_metadata"`
	Config        string   `json:"config"`
}

// RebaseResult is a /rebase's 200.
type RebaseResult struct {
	Image      string      `json:"image"`
	Before     RebaseFacts `json:"before"`
	After      RebaseFacts `json:"after"`
	DurationMS int64       `json:"duration_ms"`
}

// Refused is a /rebase the executor will not run and never forces: 409, and nothing is written. Reason is
// the contract's error: not_rebasable, another_run_image, another_stack or run_image_moved.
type Refused struct{ Reason, Message string }

func (r *Refused) Error() string { return r.Message }

// stackRe is executor.schema.json's stack_id.
var stackRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Rebase moves the app's image onto the run image a build now builds onto, under a fresh tag, with pack's
// rebase: the image the request names keeps its own tag. Pack is handed the request's tag and the run
// image's name, the executor's own, and nothing the app's image says. Every fact is read before pack runs,
// and one that cannot be read fails the rebase: it is never run blind.
func (e *Executor) Rebase(ctx context.Context, req RebaseRequest) (RebaseResult, error) {
	if !validApp(req.App) || !validPackage(req.Package) || req.RunImage == "" || len(req.RunImage) > 512 {
		return RebaseResult{}, fmt.Errorf("%w: app must match %s, package %s, and run_image is GET /builder's (contracts/schema/executor.schema.json)", ErrRequest, appPattern, packagePattern)
	}
	repo, digest, ok := strings.Cut(req.Image, "@sha256:")
	if !ok || repo != "canyon/"+req.App || strings.Trim(digest, "0123456789abcdef") != "" || len(digest) != 64 {
		return RebaseResult{}, fmt.Errorf("%w: image must be this app's digest reference (canyon/<app>@sha256:<64 hex>), never a tag", ErrRequest)
	}
	tags, err := e.images(ctx, req.App)
	if err != nil {
		return RebaseResult{}, err
	}
	id := "sha256:" + digest
	if !slices.ContainsFunc(tags, func(t imageTag) bool { return t.id == id }) {
		return RebaseResult{}, fmt.Errorf("%w: %s", ErrNoImage, req.Image)
	}
	run, err := e.RunImageID(ctx) // what GET /builder answers as run_image
	if err != nil {
		return RebaseResult{}, err
	}
	if req.RunImage != run {
		return RebaseResult{}, &Refused{"run_image_moved", fmt.Sprintf("the run image is now %s, not %s: plan again", run, req.RunImage)}
	}
	before, labels, err := e.facts(ctx, id)
	if err != nil {
		return RebaseResult{}, err
	}
	if v, set := labels["io.buildpacks.rebasable"]; set && v != "true" { // the lifecycle refuses false alone: any other value fails closed
		return RebaseResult{}, &Refused{"not_rebasable", fmt.Sprintf("the image's io.buildpacks.rebasable label is %q: Canyon never forces a rebase", v)}
	}
	named, err := RunImageName(labels[lifecycleLabel])
	if err != nil {
		return RebaseResult{}, fmt.Errorf("image %s: %s names no run image: %w", id, lifecycleLabel, err)
	}
	ours, err := LifecycleName(e.runName())
	if err != nil {
		return RebaseResult{}, fmt.Errorf("run image %s: %w", e.runName(), err)
	}
	if named != ours {
		return RebaseResult{}, &Refused{"another_run_image", fmt.Sprintf("the image was built onto %s and the run image is %s: a new run image keeps its name", named, ours)}
	}
	runLabels, err := e.labels(ctx, run)
	if err != nil {
		return RebaseResult{}, fmt.Errorf("run image %s: %w", run, err)
	}
	if stack := runLabels["io.buildpacks.stack.id"]; stack != before.StackID {
		return RebaseResult{}, &Refused{"another_stack", fmt.Sprintf("the image is on stack %q and the run image on %q", before.StackID, stack)}
	}
	start := time.Now()
	tag := "canyon/" + req.App + ":" + req.Package + "-" + nonce() // a fresh tag, as a build's: the image the request names keeps its own
	done := false
	defer func() { // every failure from the tag on removes the fresh tag, whether or not the caller still waits: a docker tag cut short may have written it
		if done {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		out, err := child(cleanup, e.docker(), "image", "rm", tag).CombinedOutput()
		if err != nil && child(cleanup, e.docker(), "image", "inspect", "--format", "{{.Id}}", tag).Run() == nil { // a tag never written is nothing to keep
			log.Printf("executor: keeping %s: %s", tag, strings.TrimSpace(string(out)))
		}
	}()
	if out, err := child(ctx, e.docker(), "tag", id, tag).CombinedOutput(); err != nil {
		return RebaseResult{}, fmt.Errorf("docker tag: %w: %s", err, strings.TrimSpace(string(out)))
	}
	last := &lastLine{}
	cmd := child(ctx, e.pack(), "rebase", tag, "--run-image", e.runName(), "--pull-policy", "never", "--no-color")
	cmd.Stdout, cmd.Stderr = last, last
	if err := cmd.Run(); err != nil {
		return RebaseResult{}, fmt.Errorf("pack rebase: %w: %s", err, last.String())
	}
	out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{.Id}}", tag).CombinedOutput()
	rebased := strings.TrimSpace(string(out))
	switch {
	case err != nil:
		return RebaseResult{}, fmt.Errorf("image inspect %s: %w: %s", tag, err, rebased)
	case !imageIDRe.MatchString(rebased):
		return RebaseResult{}, fmt.Errorf("image inspect %s answered %q, not an image ID", tag, rebased)
	}
	after, _, err := e.facts(ctx, rebased)
	if err != nil {
		return RebaseResult{}, err
	}
	if after.RunImage != req.RunImage {
		return RebaseResult{}, fmt.Errorf("the rebased image is on run image %s, not %s: the run image moved under pack", after.RunImage, req.RunImage)
	}
	done = true
	return RebaseResult{Image: "canyon/" + req.App + "@" + rebased, Before: before, After: after, DurationMS: time.Since(start).Milliseconds()}, nil
}

// facts reads an image's RebaseFacts, and its labels, from docker image inspect: any fact missing, or out
// of the shape executor.schema.json gives it, is an error.
func (e *Executor) facts(ctx context.Context, id string) (RebaseFacts, map[string]string, error) {
	out, err := child(ctx, e.docker(), "image", "inspect", "--format", "{{json .}}", id).Output()
	if err != nil {
		return RebaseFacts{}, nil, fmt.Errorf("image inspect %s: %w", id, err)
	}
	var info struct {
		Config struct {
			Labels                                 map[string]string
			Entrypoint, Cmd, Env, WorkingDir, User json.RawMessage
		}
		RootFS struct{ Layers []string }
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return RebaseFacts{}, nil, fmt.Errorf("image inspect %s: %w", id, err)
	}
	f, err := Facts(id, info.Config.Labels, map[string]any{"Entrypoint": info.Config.Entrypoint, "Cmd": info.Config.Cmd, "Env": info.Config.Env, "WorkingDir": info.Config.WorkingDir, "User": info.Config.User}, info.RootFS.Layers,
		func(reference string) (string, error) { // the lifecycle records the daemon's image ID as its 64 hex; answered in GET /builder's form
			if run := "sha256:" + reference; imageIDRe.MatchString(run) {
				return run, nil
			}
			return "", fmt.Errorf("%q, not the daemon's image ID", reference)
		})
	return f, info.Config.Labels, err
}

// Facts are an image's /rebase facts as either executor reads its config: labels, the five fields config
// digests, keys sorted, and its layers bottom first. runImage answers the run image runImage.reference
// names in GET /builder's form on the host, or why the reference is none; id names the image in an error.
func Facts(id string, labels map[string]string, config map[string]any, diffIDs []string, runImage func(reference string) (string, error)) (RebaseFacts, error) {
	var lifecycle struct {
		RunImage struct{ TopLayer, Reference string } `json:"runImage"`
	}
	if err := json.Unmarshal([]byte(labels[lifecycleLabel]), &lifecycle); err != nil {
		return RebaseFacts{}, fmt.Errorf("image %s: %s: %w", id, lifecycleLabel, err)
	}
	meta, ok := labels["io.buildpacks.build.metadata"]
	if !ok {
		return RebaseFacts{}, fmt.Errorf("image %s has no io.buildpacks.build.metadata label", id)
	}
	fields, err := json.Marshal(config) // keys sorted
	if err != nil {
		return RebaseFacts{}, fmt.Errorf("image %s: its config: %w", id, err)
	}
	f := RebaseFacts{DiffIDs: diffIDs, TopLayer: lifecycle.RunImage.TopLayer, StackID: labels["io.buildpacks.stack.id"],
		BuildMetadata: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(meta))), Config: fmt.Sprintf("sha256:%x", sha256.Sum256(fields))}
	switch {
	case len(f.DiffIDs) == 0 || len(f.DiffIDs) > 128 || slices.ContainsFunc(f.DiffIDs, func(d string) bool { return !imageIDRe.MatchString(d) }):
		return RebaseFacts{}, fmt.Errorf("image %s: its layers are not 1 to 128 sha256 diff IDs", id)
	case !imageIDRe.MatchString(f.TopLayer):
		return RebaseFacts{}, fmt.Errorf("image %s: %s names no run image top layer", id, lifecycleLabel)
	case !stackRe.MatchString(f.StackID):
		return RebaseFacts{}, fmt.Errorf("image %s: its io.buildpacks.stack.id label is %q", id, f.StackID)
	}
	if f.RunImage, err = runImage(lifecycle.RunImage.Reference); err != nil {
		return RebaseFacts{}, fmt.Errorf("image %s: %s's runImage.reference is %w", id, lifecycleLabel, err)
	}
	return f, nil
}

// RunImageName is the name of the run image an image's lifecycle metadata names, as the lifecycle's
// rebaser reads it: runImage.image, which platform API 0.12 and later write (pack's builds), else
// stack.runImage.image, which the platform APIs before it write (kpack 0.18's builds, at 0.9).
func RunImageName(metadata string) (string, error) {
	var md struct {
		RunImage struct{ Image string } `json:"runImage"`
		Stack    struct {
			RunImage struct{ Image string } `json:"runImage"`
		} `json:"stack"`
	}
	if err := json.Unmarshal([]byte(metadata), &md); err != nil {
		return "", err
	}
	return LifecycleName(cmp.Or(md.RunImage.Image, md.Stack.RunImage.Image))
}

// LifecycleName is the name a run image is compared by, as the lifecycle's rebaser compares it with the
// image's runImage.image: repository and tag, any digest stripped and a missing tag read as latest.
func LifecycleName(image string) (string, error) {
	repo, _, _ := strings.Cut(image, "@")
	ref, err := name.ParseReference(repo)
	if err != nil {
		return "", err
	}
	return ref.Name(), nil
}

// RebaseHandler serves POST /rebase, which counts against CANYON_EXECUTOR_BUILDS beside /build.
func RebaseHandler(rebase func(ctx context.Context, req RebaseRequest) (RebaseResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case builds <- struct{}{}:
			defer func() { <-builds }()
		default:
			w.Header().Set("Retry-After", "10")
			fail(w, http.StatusServiceUnavailable, "busy")
			return
		}
		var req RebaseRequest
		if err := decode(r.Body, &req); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		res, err := rebase(r.Context(), req)
		var refused *Refused
		var noRoom *NoRoom
		switch {
		case errors.Is(err, ErrRequest):
			fail(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNoImage):
			fail(w, http.StatusNotFound, err.Error())
		case errors.As(err, &refused):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = encode(w, map[string]string{"error": refused.Reason, "message": refused.Message})
		case errors.As(err, &noRoom): // on Kubernetes, its pod still unscheduled at the deadline
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = encode(w, map[string]string{"error": "the cluster had no room to run the rebase", "no_room": noRoom.Reason})
		case err != nil:
			fail(w, http.StatusBadGateway, err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = encode(w, res)
		}
	}
}

// Rebase asks the executor for POST /rebase. A 409 is *Refused, its reason one the contract names; a 503 with
// a Retry-After in seconds is *Busy; a 404 is ErrNoImage; anything else but a 200 is an error.
func (c *Client) Rebase(ctx context.Context, req RebaseRequest) (RebaseResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return RebaseResult{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/rebase", bytes.NewReader(body))
	if err != nil {
		return RebaseResult{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := c.http().Do(hreq)
	if err != nil {
		return RebaseResult{}, fmt.Errorf("executor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); resp.StatusCode == http.StatusServiceUnavailable && err == nil && s >= 0 {
		return RebaseResult{}, &Busy{RetryAfter: time.Duration(s) * time.Second}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var res RebaseResult
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&res); err != nil {
			return RebaseResult{}, fmt.Errorf("executor: /rebase: %w", err)
		}
		return res, nil
	case http.StatusConflict:
		var refused struct{ Error, Message string }
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&refused); err != nil || !slices.Contains(refusals, refused.Error) {
			return RebaseResult{}, fmt.Errorf("executor: /rebase answered 409 with no refusal the contract names (%v)", err)
		}
		return RebaseResult{}, &Refused{Reason: refused.Error, Message: refused.Message}
	case http.StatusNotFound:
		return RebaseResult{}, fmt.Errorf("executor: /rebase: %w%s", ErrNoImage, message(resp.Body))
	}
	return RebaseResult{}, fmt.Errorf("executor: /rebase answered %d%s", resp.StatusCode, message(resp.Body))
}

// refusals are POST /rebase's 409 reasons (contracts/executor.md).
var refusals = []string{"not_rebasable", "another_run_image", "another_stack", "run_image_moved"}
