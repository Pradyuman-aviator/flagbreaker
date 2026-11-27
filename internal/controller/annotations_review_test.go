package controller

// Regression tests for what the review of request checking found, on guards
// made from annotations.

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

func TestRequestLimitsSetOnAGeneratedGuardAreKept(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deploy := types.NamespacedName{Namespace: "shop", Name: "checkout"}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: deploy.Namespace, Name: deploy.Name, UID: "deploy-uid",
		Annotations: map[string]string{AnnotationFlags: "new_payments", AnnotationRequestsPort: "8080"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(d).Build()
	r := &AnnotationReconciler{Client: c, Recorder: events.NewFakeRecorder(100), Kind: KindDeployment, DefaultEnvironmentKey: envKey}
	reconcile := func() *v1alpha1.FlagGuard {
		t.Helper()
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: deploy}); err != nil {
			t.Fatal(err)
		}
		var g v1alpha1.FlagGuard
		if err := c.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "checkout-new-payments"}, &g); err != nil {
			t.Fatal(err)
		}
		return &g
	}
	annotate := func(fn func(ann map[string]string)) {
		t.Helper()
		var d appsv1.Deployment
		if err := c.Get(ctx, deploy, &d); err != nil {
			t.Fatal(err)
		}
		fn(d.Annotations)
		if err := c.Update(ctx, &d); err != nil {
			t.Fatal(err)
		}
	}

	g := reconcile()
	// A person tunes the limits on the guard, as the runbook says.
	g.Spec.Requests.MaxErrorPercent, g.Spec.Requests.MinRequests = 10, 100
	if err := c.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	want := &v1alpha1.RequestsSpec{Port: 8080, Path: "/metrics", Metric: "http_requests_total", StatusLabel: "code", MaxErrorPercent: 10, MinRequests: 100}
	if g := reconcile(); !reflect.DeepEqual(g.Spec.Requests, want) {
		t.Fatalf("the limits set on the guard were changed: %+v, want %+v", g.Spec.Requests, want)
	}

	// The annotations still say where to read, and the limits stay.
	annotate(func(ann map[string]string) { ann[AnnotationRequestsPort] = "9090" })
	want.Port = 9090
	if g := reconcile(); !reflect.DeepEqual(g.Spec.Requests, want) {
		t.Fatalf("got %+v, want %+v", g.Spec.Requests, want)
	}

	// And without the port annotation, the guard reads no requests.
	annotate(func(ann map[string]string) { delete(ann, AnnotationRequestsPort) })
	if g := reconcile(); g.Spec.Requests != nil {
		t.Fatalf("removing the annotation must turn reading off: %+v", g.Spec.Requests)
	}
}
