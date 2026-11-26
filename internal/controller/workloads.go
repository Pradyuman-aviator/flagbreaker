package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

// Workload kinds a FlagGuard can protect.
const (
	KindDeployment  = "Deployment"
	KindStatefulSet = "StatefulSet"
	KindDaemonSet   = "DaemonSet"
)

// target is the workload a guard protects, as Flagbreaker needs it.
type target struct {
	obj      client.Object
	kind     string
	selector labels.Selector
	// desired is how many pods should be ready.
	desired int32
}

func kindOf(ref v1alpha1.WorkloadReference) string {
	if ref.Kind == "" {
		return KindDeployment
	}
	return ref.Kind
}

// getTarget reads the guard's workload.
func (r *FlagGuardReconciler) getTarget(ctx context.Context, namespace string, ref v1alpha1.WorkloadReference) (target, error) {
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	t := target{kind: kindOf(ref)}
	var sel *metav1.LabelSelector
	switch t.kind {
	case KindDeployment:
		var d appsv1.Deployment
		if err := r.Get(ctx, key, &d); err != nil {
			return t, fmt.Errorf("reading Deployment %q: %w", ref.Name, err)
		}
		t.obj, sel, t.desired = &d, d.Spec.Selector, replicasOr1(d.Spec.Replicas)
	case KindStatefulSet:
		var s appsv1.StatefulSet
		if err := r.Get(ctx, key, &s); err != nil {
			return t, fmt.Errorf("reading StatefulSet %q: %w", ref.Name, err)
		}
		t.obj, sel, t.desired = &s, s.Spec.Selector, replicasOr1(s.Spec.Replicas)
	case KindDaemonSet:
		var ds appsv1.DaemonSet
		if err := r.Get(ctx, key, &ds); err != nil {
			return t, fmt.Errorf("reading DaemonSet %q: %w", ref.Name, err)
		}
		t.obj, sel, t.desired = &ds, ds.Spec.Selector, ds.Status.DesiredNumberScheduled
	default:
		return t, fmt.Errorf("workload kind %q is not supported; use Deployment, StatefulSet or DaemonSet", t.kind)
	}
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return t, fmt.Errorf("%s %q has an invalid selector: %w", t.kind, ref.Name, err)
	}
	if selector.Empty() {
		return t, fmt.Errorf("%s %q has an empty selector", t.kind, ref.Name)
	}
	t.selector = selector
	return t, nil
}

// podsAndRollout returns the workload's pods that it controls and when its
// newest revision (its latest rollout) was created.
func (r *FlagGuardReconciler) podsAndRollout(ctx context.Context, t target) ([]corev1.Pod, time.Time, error) {
	ns := t.obj.GetNamespace()
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: t.selector}); err != nil {
		return nil, time.Time{}, err
	}

	var newest time.Time
	var owners map[types.UID]bool // the ReplicaSets of a Deployment
	switch t.kind {
	case KindDeployment:
		var sets appsv1.ReplicaSetList
		if err := r.List(ctx, &sets, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: t.selector}); err != nil {
			return nil, time.Time{}, err
		}
		owners = map[types.UID]bool{}
		for i := range sets.Items {
			rs := &sets.Items[i]
			if !metav1.IsControlledBy(rs, t.obj) {
				continue
			}
			owners[rs.UID] = true
			if rs.CreationTimestamp.After(newest) {
				newest = rs.CreationTimestamp.Time
			}
		}
	default:
		var revs appsv1.ControllerRevisionList
		if err := r.List(ctx, &revs, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: t.selector}); err != nil {
			return nil, time.Time{}, err
		}
		for i := range revs.Items {
			rev := &revs.Items[i]
			if metav1.IsControlledBy(rev, t.obj) && rev.CreationTimestamp.After(newest) {
				newest = rev.CreationTimestamp.Time
			}
		}
	}

	// Another workload's pods can match the selector; count only ours.
	var mine []corev1.Pod
	for _, p := range pods.Items {
		ref := metav1.GetControllerOf(&p)
		switch {
		case ref == nil:
			continue
		case t.kind == KindDeployment && owners[ref.UID]:
			mine = append(mine, p)
		case t.kind != KindDeployment && ref.UID == t.obj.GetUID():
			mine = append(mine, p)
		}
	}
	return mine, newest, nil
}

func replicasOr1(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}
