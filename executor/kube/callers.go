package kube

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getcanyon-ai/canyon/executor"
)

// Callers answers who holds each address now (contracts/executor.md, POST /callers): the status.podIP of
// exactly one pod of canyon-apps — every pod that lists it claims it, one that ended among them — when that
// pod is Pending or Running, not terminating, and its canyon.app and canyon.target labels name an app and
// preview or prod. Any other address is unknown and left out. In the order asked; run is the label canyon.gate_run. Read only, one list of the
// pods; a cluster that does not answer is an error.
// Callers answers who holds each address now (contracts/executor.md, POST /callers): an address that exactly one
// pod of canyon-apps lists, that pod Pending or Running, not terminating, naming an app and preview or prod, with
// a container running; since is the earliest running container's startedAt, after the pod's sandbox had the
// address — never the pod's status.startTime, which is before. In the order asked; any other address is left out.
func (e *Executor) Callers(ctx context.Context, addresses []string) ([]executor.Caller, error) {
	asked, err := executor.CallerAddresses(addresses)
	if err != nil {
		return nil, err
	}
	pods, err := e.Client.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{LabelSelector: "canyon.app,canyon.target"})
	if err != nil {
		return nil, fmt.Errorf("pods of %s: %w", Namespace, err)
	}
	claims := map[netip.Addr]int{}
	found := map[netip.Addr]executor.Caller{}
	for _, p := range pods.Items {
		a, err := netip.ParseAddr(p.Status.PodIP)
		if err != nil || !asked[a] {
			continue
		}
		claims[a]++
		app, target := p.Labels["canyon.app"], p.Labels["canyon.target"]
		live := p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning
		since := runningSince(p)
		if !live || p.DeletionTimestamp != nil || !validApp(app) || !validTarget(target) || since.IsZero() {
			continue
		}
		c := executor.Caller{Address: a.String(), App: app, Target: target, Kind: "web", Since: since}
		if run := p.Labels["canyon.gate_run"]; executor.ValidRun(run) {
			c.Run = &run
		}
		found[a] = c
	}
	answer := []executor.Caller{}
	for _, s := range addresses {
		a := netip.MustParseAddr(s)
		if c, ok := found[a]; ok && claims[a] == 1 {
			answer = append(answer, c)
		}
	}
	return answer, nil
}

// runningSince is when the earliest of the pod's running containers started, in UTC, zero when none runs: the
// next whole second past its startedAt, which the API keeps to the second, so it is never before the start.
func runningSince(p corev1.Pod) time.Time {
	var since time.Time
	for _, c := range slices.Concat(p.Status.InitContainerStatuses, p.Status.ContainerStatuses) {
		if r := c.State.Running; r != nil && !r.StartedAt.IsZero() && (since.IsZero() || r.StartedAt.Time.Before(since)) {
			since = r.StartedAt.Time
		}
	}
	if since.IsZero() {
		return since
	}
	return since.UTC().Truncate(time.Second).Add(time.Second)
}
