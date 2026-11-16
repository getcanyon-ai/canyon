package kube

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/getcanyon-ai/canyon/executor"
)

// rollsPerCall bounds the Deployments one POST /sidecars rolls, each roll a pod surged at once, and
// triesPerCall those it tries to write, so ones the cluster refuses never hold up the rest: the rest roll
// on the server's later calls.
const (
	rollsPerCall = 10
	triesPerCall = 3 * rollsPerCall
)

// The annotations a roll writes on the Deployment with the image: the image it rolled onto, and the
// roll's own name. A /run writes a Deployment without them.
const (
	rolledTo = "canyon.sidecar-rolled-to"
	rollName = "canyon.sidecar-roll"
)

// Sidecars rolls canyon-identity onto Identity in the app Deployments a /run made, at most rollsPerCall
// of them and trying at most triesPerCall, and answers each a roll has put on Identity whose rollout is done (contracts/executor.md, POST
// /sidecars). A roll changes that one image and adds its two annotations, so the app's own image,
// environment and every other field stay as its /run wrote them, and its pod is replaced as a /run's is.
// The answer is state, not news: an environment rolled is answered on every call while the roll's
// annotations stand, until its next /run, so an answer the server lost is answered again. A Deployment being deleted is passed over. Any the executor
// did not make, could not read or write, or whose rolled pod missed its progress deadline is never
// touched again by this call: it is logged and counted in Failed. A stuck rollout is not undone: with
// maxUnavailable 0 the previous pod serves on.
func (e *Executor) Sidecars(ctx context.Context) (executor.Sidecars, error) {
	deployments := e.Client.AppsV1().Deployments(Namespace)
	list, err := deployments.List(ctx, metav1.ListOptions{LabelSelector: "canyon.app,canyon.target"})
	if err != nil {
		return executor.Sidecars{}, fmt.Errorf("deployments: %w", err)
	}
	res := executor.Sidecars{Image: e.Identity, Rolled: []executor.SidecarRoll{}}
	failed := func(name string, err error) {
		log.Printf("executor: canyon-identity of deployment %s is not rolled onto %s: %v", name, e.Identity, err)
		res.Failed++
	}
	rolls, tries := 0, 0
	for i := range list.Items {
		d := &list.Items[i]
		if d.DeletionTimestamp != nil {
			continue
		}
		c, err := sidecarOf(d)
		switch {
		case err != nil:
			failed(d.Name, err)
		case d.Spec.Template.Spec.InitContainers[c].Image != e.Identity:
			if rolls == rollsPerCall || tries == triesPerCall {
				continue
			}
			tries++
			switch rolled, err := e.rollSidecar(ctx, d.Name, d); {
			case err != nil && !apierrors.IsNotFound(err): // not found: stopped since the list
				failed(d.Name, err)
			case rolled:
				rolls++
			}
		case d.Annotations[rolledTo] != e.Identity || d.Annotations[rollName] == "": // on Identity by its /run: nothing to roll or answer
		case stuck(d):
			failed(d.Name, errors.New("its rolled pod did not become available by the Deployment's progress deadline; the previous pod serves on"))
		case rolledOut(d):
			res.Rolled = append(res.Rolled, executor.SidecarRoll{App: d.Labels["canyon.app"], Target: d.Labels["canyon.target"], Roll: d.Annotations[rollName]})
		}
	}
	return res, nil
}

// rollSidecar sets canyon-identity's image to Identity in the Deployment name, d as listed, with the
// roll's annotations, answering whether it did. It is read again after a conflict — a /run wrote it
// since — and rolled only if it is still /run's, not being deleted, and not on Identity.
func (e *Executor) rollSidecar(ctx context.Context, name string, d *appsv1.Deployment) (rolled bool, err error) {
	deployments := e.Client.AppsV1().Deployments(Namespace)
	err = retry.RetryOnConflict(retry.DefaultRetry, func() (err error) {
		if d == nil {
			if d, err = deployments.Get(ctx, name, metav1.GetOptions{}); err != nil {
				return err
			}
		}
		c, err := sidecarOf(d)
		if err != nil || d.DeletionTimestamp != nil || d.Spec.Template.Spec.InitContainers[c].Image == e.Identity {
			return err
		}
		next := d.DeepCopy()
		next.Spec.Template.Spec.InitContainers[c].Image = e.Identity
		if next.Annotations == nil {
			next.Annotations = map[string]string{}
		}
		next.Annotations[rolledTo], next.Annotations[rollName] = e.Identity, nonce()
		if _, err = deployments.Update(ctx, next, metav1.UpdateOptions{}); apierrors.IsConflict(err) {
			d = nil
		}
		rolled = err == nil
		return err
	})
	return rolled, err
}

// sidecarOf is the index of d's canyon-identity among its init containers when d is one a /run made
// (deployment): named <app>-<target> by its labels, its template labelled canyon.run, and its
// canyon-identity mounting the access file the /run writes, canyon-access-<app>-<target>.
func sidecarOf(d *appsv1.Deployment) (int, error) {
	app, target, t := d.Labels["canyon.app"], d.Labels["canyon.target"], d.Spec.Template
	if !validApp(app) || !validTarget(target) || d.Name != app+"-"+target {
		return 0, errors.New("not named <app>-<target> by its labels, as a /run names it")
	}
	if t.Labels["canyon.run"] == "" {
		return 0, errors.New("its template has no canyon.run, which every /run labels")
	}
	i := slices.IndexFunc(t.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == "canyon-identity" })
	if i < 0 {
		return 0, errors.New("no canyon-identity sidecar")
	}
	v := slices.IndexFunc(t.Spec.Volumes, func(v corev1.Volume) bool { return v.Name == "access" })
	if v < 0 || t.Spec.Volumes[v].ConfigMap == nil || t.Spec.Volumes[v].ConfigMap.Name != "canyon-access-"+d.Name ||
		!slices.ContainsFunc(t.Spec.InitContainers[i].VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == "access" }) {
		return 0, errors.New("its canyon-identity does not mount the access file a /run writes, canyon-access-" + d.Name)
	}
	return i, nil
}

// stuck is a Deployment whose current rollout missed its progress deadline.
func stuck(d *appsv1.Deployment) bool {
	return d.Status.ObservedGeneration >= d.Generation && slices.ContainsFunc(d.Status.Conditions, func(c appsv1.DeploymentCondition) bool {
		return c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded"
	})
}
