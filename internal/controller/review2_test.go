package controller

// Regressions from the second review round: each test is a scenario that
// went wrong before the fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
)

var r2key = types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}

// flaky sits in front of the fake Flagsmith and fails requests a predicate picks.
type flaky struct {
	mu   sync.Mutex
	h    http.Handler
	fail func(r *http.Request, body []byte) int // 0 = pass through
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.mu.Lock()
	code := 0
	if f.fail != nil {
		code = f.fail(r, body)
	}
	f.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		return
	}
	f.h.ServeHTTP(w, r)
}

func (w *world) withFlaky() *flaky {
	f := &flaky{h: w.fs}
	srv := httptest.NewServer(f)
	w.t.Cleanup(srv.Close)
	w.r.Flagsmith.BaseURL = srv.URL
	return f
}

func (w *world) reconcileRaw(d time.Duration) (v1alpha1.FlagGuard, error) {
	w.now = t0.Add(d)
	_, err := w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: r2key})
	var g v1alpha1.FlagGuard
	_ = w.c.Get(w.ctx, r2key, &g)
	return g, err
}

// 1. A spec change whose lookup fails once leaves the guard on the old flag.
func TestR2SpecChangeWithFailedLookupKeepsGuardingTheOldFlag(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	var g v1alpha1.FlagGuard
	_ = w.c.Get(w.ctx, r2key, &g)
	g.Spec.Flag = "pricing_v3" // not created in Flagsmith yet
	g.Generation = 2
	if err := w.c.Update(w.ctx, &g); err != nil {
		t.Fatal(err)
	}
	g, _ = w.reconcileRaw(5 * time.Second)
	t.Logf("after failed lookup: gen=%d observedGen=%d featureID=%d ready=%v", g.Generation, g.Status.ObservedGeneration, g.Status.FeatureID, findCondition(&g, conditionReady))
	g, _ = w.reconcileRaw(10 * time.Second)
	c := findCondition(&g, conditionReady)
	t.Logf("next check: spec.flag=%s featureID=%d ready=%s %s", g.Spec.Flag, g.Status.FeatureID, c.Status, c.Message)
	// The old flag is switched on and the pods crash: what gets switched off?
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.reconcileRaw(15 * time.Second)
	w.crashPods(t0.Add(17 * time.Second))
	g, _ = w.reconcileRaw(20 * time.Second)
	def, _ := w.flag()
	t.Logf("phase=%s new_pricing(old flag) default now=%v timeline=%s", g.Status.Phase, def, timelineEvents(&g))
	// The guard no longer names new_pricing, so it must never switch it off;
	// it keeps looking pricing_v3 up, and says so, until that flag exists.
	if !def || g.Status.Phase == v1alpha1.PhaseTripped {
		t.Errorf("the old flag was acted on: phase=%s new_pricing on=%v", g.Status.Phase, def)
	}
	if c := findCondition(&g, conditionReady); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "pricing_v3") {
		t.Errorf("want Ready=False about pricing_v3, got %v", c)
	}
}

// 2. A Healthy report that fails on rearm is never retried.
func TestR2RearmHealthyFailureIsNeverRetried(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	fl := w.withFlaky()
	w.tripNewPricing()
	fl.mu.Lock()
	fl.fail = func(r *http.Request, body []byte) int {
		if r.URL.Path == healthPath {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	fl.mu.Unlock()
	w.fs.set(func(f *fakeFlagsmith) { f.def = true }) // a person turns it back on
	g := w.at(15 * time.Second)
	t.Logf("after rearm: phase=%s reported=%+v", g.Status.Phase, findCondition(g, conditionReported))
	fl.mu.Lock()
	fl.fail = nil
	fl.mu.Unlock()
	for s := 20; s <= 60; s += 5 {
		g = w.at(time.Duration(s) * time.Second)
	}
	t.Logf("40s later: phase=%s reported=%+v", g.Status.Phase, findCondition(g, conditionReported))
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() {
		t.Errorf("BUG: Feature Health still shows Flagbreaker's Unhealthy event; Healthy was never retried")
	}
}

// 3. A 5xx on one override in the one-by-one path is treated as a refusal.
func TestR2TransientErrorOnOneOverrideIsTreatedAsRefusal(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	const experiment, flakySeg = 9, 8
	w.fs.set(func(f *fakeFlagsmith) {
		f.segments[experiment] = true
		f.segments[flakySeg] = true
		f.experimentSegment = experiment
	})
	fl := w.withFlaky()
	failed := false
	fl.fail = func(r *http.Request, body []byte) int {
		if r.Method != http.MethodPatch || failed {
			return 0
		}
		var b struct {
			Segs []struct {
				Segment struct{ ID int64 } `json:"segment"`
			} `json:"segment_overrides"`
		}
		_ = json.Unmarshal(body, &b)
		if len(b.Segs) == 1 && b.Segs[0].Segment.ID == flakySeg {
			failed = true
			return http.StatusServiceUnavailable
		}
		return 0
	}
	g := w.tripNewPricing()
	for s := 15; s <= 40; s += 5 {
		g = w.at(time.Duration(s) * time.Second)
	}
	w.fs.mu.Lock()
	seg8 := w.fs.segments[flakySeg]
	w.fs.mu.Unlock()
	var msg string
	for _, e := range g.Status.Timeline {
		if e.Event == "SegmentsLeftOn" {
			msg = e.Message
		}
	}
	t.Logf("segment 8 on=%v snapshot=%+v msg=%q", seg8, g.Status.Snapshot, msg)
	if seg8 {
		t.Errorf("BUG: segment %d stayed on after a one-off 503 and was never retried", flakySeg)
	}
}

// 4. An alert that was firing before the switch-on (so can't trip) still blocks recovery.
func TestR2PreexistingAlertBlocksRecoveryForever(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.now = t0.Add(2 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "UnrelatedDiskAlert"}})
	})
	w.at(30 * time.Minute)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(30*time.Minute + 5*time.Second)
	w.crashPods(t0.Add(30*time.Minute + 7*time.Second))
	g := w.at(30*time.Minute + 10*time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Alert != nil {
		t.Fatalf("want a pod trip: %+v", g.Status)
	}
	w.healthyPods(2, t0.Add(30*time.Minute+12*time.Second))
	for s := 15; s <= 600; s += 15 {
		g = w.at(30*time.Minute + time.Duration(s)*time.Second)
	}
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Errorf("BUG: 10 minutes of healthy pods, still %s, because an alert that predates the switch-on (and could not trip the guard) is still firing", g.Status.Phase)
	}
}

// 5. Deleting a guard whose status write failed: finalize reads the stale stored status.
func TestR2FinalizeIgnoresPendingStatus(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	failNext := true
	base := w.c
	w.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*v1alpha1.FlagGuard); ok && failNext {
				failNext = false
				return apierrors.NewInternalError(context.DeadlineExceeded)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	_, err := w.reconcileRaw(10 * time.Second) // trips, switches off, reports Unhealthy; status write fails
	t.Logf("trip reconcile err=%v pending=%d", err, len(w.r.pending))
	var g v1alpha1.FlagGuard
	_ = w.c.Get(w.ctx, r2key, &g)
	t.Logf("stored phase=%q", g.Status.Phase)
	if err := w.c.Delete(w.ctx, &g); err != nil {
		t.Fatal(err)
	}
	if _, err := w.reconcileRaw(11 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Get(w.ctx, r2key, &v1alpha1.FlagGuard{}); !apierrors.IsNotFound(err) {
		t.Fatalf("guard should be gone: %v", err)
	}
	t.Logf("pending entries left after deletion: %d", len(w.r.pending))
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() {
		t.Errorf("BUG: guard deleted mid-incident, but its Unhealthy event stays: finalize used the stale stored status, not the pending one")
	}
}

// 6. Watch mode: dismissing the incident re-trips at once on an alert that fired during it.
func TestR2DismissalRetripsOnAlertFromTheSameIncident(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeWatch)
	w.tripNewPricing()
	w.now = t0.Add(12 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "CheckoutErrorRateHigh"}})
	})
	w.at(15 * time.Second)
	w.now = t0.Add(18 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderWebhook, "", "HEALTHY", []flagsmith.TextBlock{{Text: "Manually dismissed by admin@example.com"}})
	})
	g := w.at(20 * time.Second)
	t.Logf("after dismissal: %s", g.Status.Phase)
	g = w.at(25 * time.Second)
	t.Logf("next check: %s alert=%+v unhealthy events=%d", g.Status.Phase, g.Status.Alert, w.ownEvents("UNHEALTHY"))
	if g.Status.Phase == v1alpha1.PhaseTripped {
		t.Errorf("BUG: a dismissed incident re-tripped 5s later on an alert that fired during it")
	}
}

// 7. A switch-off that timed out after Flagsmith applied it is recorded as a person's.
func TestR2TimedOutSwitchOffIsBlamedOnAPerson(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	fl := w.withFlaky()
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	fl.fail = func(r *http.Request, body []byte) int {
		if r.Method == http.MethodPatch {
			// apply it, then fail the response, as a timeout after commit would
			w.fs.ServeHTTP(httptest.NewRecorder(), r)
			fl.fail = nil
			return http.StatusGatewayTimeout
		}
		return 0
	}
	g, err := w.reconcileRaw(10 * time.Second)
	t.Logf("trip err=%v phase=%s", err, g.Status.Phase)
	g = *w.at(15 * time.Second)
	t.Logf("flagDisabledBy=%q timeline=%s", g.Status.FlagDisabledBy, timelineEvents(&g))
	if strings.Contains(timelineEvents(&g), "FlagSwitchedOffByPerson") {
		t.Errorf("BUG: Flagbreaker's own switch-off is recorded as a person's")
	}
}

// 8. A workload already crash-looping (back-off > 1 minute) is blamed on a switch-on.
func TestR2AlreadyCrashLoopingWorkloadIsBlamedOnTheFlag(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	loop := func(lastCrash time.Time) {
		for _, p := range w.pods() {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(t0.Add(-20 * time.Minute))}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: "checkout", RestartCount: 6,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(lastCrash)}},
			}}
			if err := w.c.Status().Update(w.ctx, &p); err != nil {
				t.Fatal(err)
			}
		}
	}
	loop(t0.Add(-2 * time.Minute)) // crash-looping for 20 minutes, 5-minute back-off
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	g := w.at(5 * time.Second)
	t.Logf("switch-on seen: workloadWasFailing=%v", g.Status.LastFlagChange.WorkloadWasFailing)
	loop(t0.Add(3 * time.Minute)) // the next crash of the same loop
	g = w.at(3*time.Minute + 5*time.Second)
	def, _ := w.flag()
	t.Logf("phase=%s flag on=%v", g.Status.Phase, def)
	if g.Status.Phase == v1alpha1.PhaseTripped {
		t.Errorf("BUG: a crash loop that started 20 minutes before the switch-on was blamed on the flag (flag on=%v)", def)
	}
}
