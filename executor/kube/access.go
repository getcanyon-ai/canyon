package kube

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// parseAccess is the access file as the schema spells it, its updated_at whole seconds UTC: two of
// them order as strings.
func parseAccess(raw []byte) (access, error) {
	var file access
	if err := strict(raw, &file); err != nil {
		return file, refuse("access: %v", err)
	}
	if at, err := time.Parse(updatedAt, file.UpdatedAt); err != nil || at.Format(updatedAt) != file.UpdatedAt { // Parse takes fractional seconds the layout does not name
		return file, refuse("access.updated_at must be whole seconds UTC, %s", updatedAt)
	}
	return file, nil
}

// PutAccess rewrites the ConfigMap canyon-access-<app>-<target> and nothing else: the sidecar reads the
// file on every request, so a share or an unshare reaches the app without a restart. A ConfigMap that
// is not there is nothing running (ErrNoContainer, 404): /run writes it before the Deployment and /stop
// deletes it after, so a share made while the first pod starts still lands. One holding a newer
// updated_at is executor.ErrStale.
func (e *Executor) PutAccess(ctx context.Context, app, target string, raw []byte) error {
	if !validApp(app) || !validTarget(target) {
		return refuse("app must be a project id of at most 55 characters, target preview or prod")
	}
	file, err := parseAccess(raw)
	if err != nil {
		return err
	}
	existed, kept, err := e.putAccess(ctx, app, target, raw, file.UpdatedAt, false)
	switch {
	case err != nil:
		return err
	case !existed:
		return fmt.Errorf("%w: %s-%s", executor.ErrNoContainer, app, target)
	case kept:
		return fmt.Errorf("%w: canyon-access-%s-%s holds a newer updated_at", executor.ErrStale, app, target)
	}
	return nil
}

// Logs streams the container app of the newest pod of <app>-<target> — never a sidecar's
// (invariant 5): the last tail lines, then, when follow is set, live until that pod or ctx ends,
// which is a clean end.
// ready is called once the pod is found and before the log request, because the API answers that
// request only with the app's first byte: a caller answers 200 while a quiet app has written none.
// No pod is ErrNoContainer before it; a request refused after it ends the answered body on its
// reason, the handler's last line (contracts/executor.md, GET /logs).
func (e *Executor) Logs(ctx context.Context, app, target string, tail int, follow bool, out io.Writer, ready func()) error {
	if !validApp(app) || !validTarget(target) {
		return refuse("app must be a project id of at most 55 characters, target preview or prod")
	}
	pods, err := e.pods(ctx, app, target, "")
	if err != nil {
		return err
	}
	newest := newestPod(pods)
	if newest == nil {
		return fmt.Errorf("%w: no pod of %s-%s", executor.ErrNoContainer, app, target)
	}
	if ready != nil {
		ready()
	}
	lines := int64(tail)
	opts := &corev1.PodLogOptions{Container: "app", Follow: follow, TailLines: &lines}
	last := lastRun(newest, opts) // a crashed app's lines are its last run's (contracts/executor.md, A container that isn't running)
	stream, err := e.Client.CoreV1().Pods(Namespace).GetLogs(newest.Name, opts).Stream(ctx)
	if err != nil && last != nil && last.Message != "" { // that run's log is gone: the tail its status keeps
		_, err = io.WriteString(out, strings.TrimSuffix(last.Message, "\n")+"\ncanyon: the cluster kept only the end of the app's last run\n")
		return err
	}
	if err != nil {
		return fmt.Errorf("logs of %s: %w", newest.Name, err)
	}
	defer func() { _ = stream.Close() }()
	if _, err := io.Copy(out, stream); err != nil && ctx.Err() == nil {
		return fmt.Errorf("logs of %s: %w", newest.Name, err)
	}
	return nil
}

// lastRun points opts at the app container's last run when it isn't running and has ended: the run before while it
// waits to start again, its own once it has ended for good, never followed. It answers how that run ended, nil
// for a running container, or one that never ended.
func lastRun(p *corev1.Pod, opts *corev1.PodLogOptions) *corev1.ContainerStateTerminated {
	for _, cs := range p.Status.ContainerStatuses {
		switch {
		case cs.Name != "app" || cs.State.Running != nil:
		case cs.State.Terminated != nil:
			opts.Follow = false
			return cs.State.Terminated
		case cs.LastTerminationState.Terminated != nil:
			opts.Follow, opts.Previous = false, true
			return cs.LastTerminationState.Terminated
		}
	}
	return nil
}

// newestPod is the pod created last, nil for none.
func newestPod(pods []corev1.Pod) *corev1.Pod {
	var newest *corev1.Pod
	for i, p := range pods {
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods[i]
		}
	}
	return newest
}
