package controller

// Generated guards that read requests: the flagbreaker.dev/requests-*
// annotations on a workload set spec.requests.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

// generated returns the guard the annotations make for new_payments on
// deployment shop/checkout, and the problem they report.
func generated(t *testing.T, ann map[string]string) (*v1alpha1.FlagGuard, string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	r := &AnnotationReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Kind: KindDeployment, DefaultEnvironmentKey: envKey}
	all := map[string]string{AnnotationFlags: "new_payments"}
	for k, v := range ann {
		all[k] = v
	}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout", UID: "deploy-uid", Annotations: all}}
	guards, problem := r.desiredGuards(d)
	g := guards["checkout-new-payments"]
	if g == nil {
		t.Fatalf("no guard for new_payments: %v (%s)", guards, problem)
	}
	return g, problem
}

func TestTheRequestsPortAnnotationTurnsReadingOnWithEveryDefaultFilledIn(t *testing.T) {
	g, problem := generated(t, map[string]string{AnnotationRequestsPort: "8080"})
	want := &v1alpha1.RequestsSpec{Port: 8080, Path: "/metrics", Metric: "http_requests_total", StatusLabel: "code", MaxErrorPercent: 5, MinRequests: 20}
	if problem != "" || !reflect.DeepEqual(g.Spec.Requests, want) {
		t.Fatalf("got %+v (%q), want %+v", g.Spec.Requests, problem, want)
	}

	g, problem = generated(t, map[string]string{
		AnnotationRequestsPort: " 9090 ", AnnotationRequestsPath: "/stats/prometheus",
		AnnotationRequestsMetric: "http_server_requests_seconds_count", AnnotationRequestsStatusLabel: "status",
	})
	want = &v1alpha1.RequestsSpec{Port: 9090, Path: "/stats/prometheus", Metric: "http_server_requests_seconds_count", StatusLabel: "status", MaxErrorPercent: 5, MinRequests: 20}
	if problem != "" || !reflect.DeepEqual(g.Spec.Requests, want) {
		t.Fatalf("got %+v (%q), want %+v", g.Spec.Requests, problem, want)
	}
}

func TestInvalidRequestsAnnotationsAreReportedAndReadNothing(t *testing.T) {
	cases := map[string]map[string]string{
		AnnotationRequestsPort + " 0":            {AnnotationRequestsPort: "0"},
		AnnotationRequestsPort + " 65536":        {AnnotationRequestsPort: "65536"},
		AnnotationRequestsPort + " named":        {AnnotationRequestsPort: "http"},
		AnnotationRequestsPath + " relative":     {AnnotationRequestsPort: "8080", AnnotationRequestsPath: "metrics"},
		AnnotationRequestsPath + " too long":     {AnnotationRequestsPort: "8080", AnnotationRequestsPath: "/" + strings.Repeat("m", 200)},
		AnnotationRequestsMetric + " dashes":     {AnnotationRequestsPort: "8080", AnnotationRequestsMetric: "http-requests-total"},
		AnnotationRequestsStatusLabel + " bad":   {AnnotationRequestsPort: "8080", AnnotationRequestsStatusLabel: "status:code"},
		AnnotationRequestsPath + " alone":        {AnnotationRequestsPath: "/metrics"},
		AnnotationRequestsMetric + " alone":      {AnnotationRequestsMetric: "http_requests_total"},
		AnnotationRequestsStatusLabel + " alone": {AnnotationRequestsStatusLabel: "code"},
	}
	for name, ann := range cases {
		g, problem := generated(t, ann)
		if g.Spec.Requests != nil || !strings.Contains(problem, "not reading requests") {
			t.Errorf("%s: got %+v, problem %q; want no spec.requests and a problem", name, g.Spec.Requests, problem)
		}
		annotation := strings.Fields(name)[0]
		if !strings.Contains(problem, annotation) {
			t.Errorf("%s: the problem should name %s: %q", name, annotation, problem)
		}
	}
}

func TestWorkloadsWithoutRequestsAnnotationsGetTodaysGuards(t *testing.T) {
	g, problem := generated(t, map[string]string{AnnotationMode: "act", AnnotationStableFor: "20s"})
	stable := metav1.Duration{Duration: 20 * time.Second}
	want := v1alpha1.FlagGuardSpec{Flag: "new_payments", EnvironmentKey: envKey, Mode: v1alpha1.ModeAct, StableFor: &stable,
		Workload: v1alpha1.WorkloadReference{Kind: KindDeployment, Name: "checkout"}}
	if problem != "" || !reflect.DeepEqual(g.Spec, want) {
		t.Fatalf("got %+v (%q), want %+v", g.Spec, problem, want)
	}
}

func TestGeneratedRequestsSurviveTheAPIServersDefaults(t *testing.T) {
	want, _ := generated(t, map[string]string{AnnotationRequestsPort: "8080"})

	// What the API server stores: every default filled in, in spec.requests too.
	window, stable, yes := metav1.Duration{Duration: 10 * time.Minute}, metav1.Duration{Duration: 30 * time.Second}, true
	stored := v1alpha1.FlagGuardSpec{Flag: "new_payments", EnvironmentKey: envKey, Workload: v1alpha1.WorkloadReference{Kind: KindDeployment, Name: "checkout"},
		Window: &window, FailingPods: 1, StableFor: &stable, RestartCrashingPods: &yes, FollowHealthEvents: &yes,
		Requests: &v1alpha1.RequestsSpec{Port: 8080, Path: "/metrics", Metric: "http_requests_total", StatusLabel: "code", MaxErrorPercent: 5, MinRequests: 20}}
	if !managedSpecEqual(stored, want.Spec) {
		t.Fatal("a stored guard with the API server's defaults must not be updated again")
	}

	// Without the annotation, the stored guard's spec.requests goes.
	plain, _ := generated(t, nil)
	if managedSpecEqual(stored, plain.Spec) {
		t.Fatal("removing the annotation must remove spec.requests")
	}
	var dst v1alpha1.FlagGuardSpec
	copyManagedSpec(&dst, want.Spec)
	want.Spec.Requests.Port = 1
	if dst.Requests == nil || dst.Requests.Port != 8080 {
		t.Fatalf("copyManagedSpec must copy spec.requests, not share it: %+v", dst.Requests)
	}
}
