package controller

// Regressions found in review: each test is a scenario that went wrong.

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/changes"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
)

// healAfterCrash makes every pod ready again but keeps the last crash in
// LastTerminationState, as the kubelet does.
func (w *world) healAfterCrash(crashAt, readyAt time.Time) {
	for _, p := range w.pods() {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readyAt)}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "checkout", RestartCount: 1, Ready: true,
			State:                corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(crashAt)}},
		}}
		if err := w.c.Status().Update(w.ctx, &p); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) addPod(name string, created time.Time, status corev1.PodStatus) {
	yes := true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: name, Labels: map[string]string{"app": "checkout"},
		CreationTimestamp: metav1.NewTime(created),
		OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: w.rs.Name, UID: w.rs.UID, Controller: &yes}},
	}}
	if err := w.c.Create(w.ctx, p); err != nil {
		w.t.Fatal(err)
	}
	p.Status = status
	if err := w.c.Status().Update(w.ctx, p); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) ownEvents(typ string) int {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	n := 0
	for _, e := range w.fs.events {
		if isOwnEvent(e.HealthEventRecord) && strings.EqualFold(e.Type, typ) {
			n++
		}
	}
	return n
}

// tripNewPricing switches the flag on, crashes the pods and trips the guard.
func (w *world) tripNewPricing() *v1alpha1.FlagGuard {
	w.t.Helper()
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped {
		w.t.Fatalf("want Tripped, got %+v", g.Status)
	}
	return g
}

func TestWatchModeDoesNotFlapWhenPodsHealWithTheFlagOn(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeWatch)
	w.tripNewPricing()
	w.healAfterCrash(t0.Add(7*time.Second), t0.Add(12*time.Second))
	trips := 0
	last := v1alpha1.PhaseTripped
	for s := 15; s <= 300; s += 5 {
		g := w.at(time.Duration(s) * time.Second)
		if g.Status.Phase == v1alpha1.PhaseTripped && last != v1alpha1.PhaseTripped {
			trips++
		}
		last = g.Status.Phase
	}
	if trips != 0 || last != v1alpha1.PhaseArmed || w.ownEvents("UNHEALTHY") != 1 {
		t.Fatalf("got %d re-trips, last phase %s and %d Unhealthy events; want 0, Armed and 1", trips, last, w.ownEvents("UNHEALTHY"))
	}
}

func TestASlowNewPodDoesNotTrip(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.at(60 * time.Second)
	// An HPA scale-up: a pod created after the switch-on, slow to start.
	w.addPod("checkout-abc-scaleup", t0.Add(61*time.Second), corev1.PodStatus{
		Phase:             corev1.PodRunning,
		Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(t0.Add(62 * time.Second))}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "checkout", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	})
	for _, s := range []int{85, 120, 180} {
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
			t.Fatalf("at %ds a slow-starting pod tripped the guard: %v", s, g.Status.Timeline)
		}
	}
	if def, _ := w.flag(); !def {
		t.Fatal("the flag must stay on")
	}
}

func TestAnEvictedPodDoesNotBlockRecovery(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.tripNewPricing()
	w.healthyPods(2, t0.Add(12*time.Second))
	w.addPod("checkout-abc-evicted", t0.Add(-time.Hour), corev1.PodStatus{
		Phase: corev1.PodFailed, Reason: "Evicted",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(t0.Add(-time.Hour))}},
	})
	w.at(15 * time.Second)
	if g := w.at(36 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("an evicted pod kept the guard from recovering: %+v", g.Status)
	}
}

func TestAGapBetweenChecksStillBlamesTheChange(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.r.Changes.Record("Production", "new_pricing", changes.Change{At: t0.Add(5 * time.Second), Enabled: true, ChangedBy: "admin@example.com"})
	w.crashPods(t0.Add(80 * time.Second))
	// The next check comes two minutes later (Flagsmith was unreachable).
	g := w.at(120 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Trigger == nil || g.Status.Trigger.ChangedBy != "admin@example.com" ||
		!g.Status.Trigger.At.Equal(mt(5*time.Second)) {
		t.Fatalf("want a trip blamed on admin's change at 0:05, got %+v", g.Status)
	}
}

func TestAChangeMadeWhileFlagbreakerWasDownIsNotBlamedOnAGuess(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	// Flagbreaker restarts: a new process has no memory of earlier checks.
	w.r.lastCheck = map[types.NamespacedName]time.Time{}
	w.r.Changes = changes.NewStore()
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.crashPods(t0.Add(time.Hour + 38*time.Second))
	g := w.at(time.Hour + 40*time.Second)
	if g.Status.Phase != v1alpha1.PhaseArmed || g.Status.LastFlagChange == nil || !g.Status.LastFlagChange.TimeUnknown {
		t.Fatalf("want an unblamed change of unknown time, got %+v", g.Status)
	}
	if def, _ := w.flag(); !def {
		t.Fatal("a change of unknown time must not be switched off")
	}
	// The webhook, sent a few seconds after the change, says exactly when:
	// before the crash. Now the change can be blamed.
	w.r.Changes.Record("Production", "new_pricing", changes.Change{At: t0.Add(time.Hour + 35*time.Second), Enabled: true, ChangedBy: "admin@example.com"})
	g = w.at(time.Hour + 45*time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Trigger.ChangedBy != "admin@example.com" {
		t.Fatalf("with the webhook's time the change should be blamed: %+v", g.Status)
	}
}

func TestACrashBeforeAChangeOfUnknownTimeIsNotBlamedOnIt(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.r.lastCheck = map[types.NamespacedName]time.Time{}
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.crashPods(t0.Add(time.Hour + 30*time.Second)) // before the change at 1:00:35
	w.at(time.Hour + 40*time.Second)
	w.r.Changes.Record("Production", "new_pricing", changes.Change{At: t0.Add(time.Hour + 35*time.Second), Enabled: true, ChangedBy: "admin@example.com"})
	if g := w.at(time.Hour + 45*time.Second); g.Status.Phase != v1alpha1.PhaseArmed || !g.Status.LastFlagChange.WorkloadWasFailing {
		t.Fatalf("the workload was failing before the change; want Armed, got %+v", g.Status)
	}
}

func TestAStatusConflictAfterTheSwitchOffKeepsTheIncident(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	failNext := true
	w.r.Client = interceptor.NewClient(w.c.(client.WithWatch), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*v1alpha1.FlagGuard); ok && failNext {
				failNext = false
				return apierrors.NewConflict(schema.GroupResource{Group: "flagbreaker.dev", Resource: "flagguards"}, obj.GetName(), nil)
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	w.at(10 * time.Second) // trips and switches off, but the status write fails
	g := w.at(11 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Snapshot == nil || len(g.Status.Snapshot.EnabledSegmentOverrides) != 1 {
		t.Fatalf("the incident was lost: %+v", g.Status)
	}
}

func TestAnAlertFromBeforeTheSwitchOnDoesNotTrip(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.now = t0.Add(2 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "CheckoutErrorRateHigh"}})
	})
	w.at(30 * time.Minute)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	g := w.at(60 * time.Minute)
	if def, _ := w.flag(); g.Status.Phase != v1alpha1.PhaseArmed || !def {
		t.Fatalf("an alert that was already firing tripped the guard: %+v", g.Status)
	}
	// A new alert after the switch-on does trip it.
	w.now = t0.Add(61 * time.Minute)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp2", "UNHEALTHY", []flagsmith.TextBlock{{Title: "CheckoutLatencyHigh"}})
	})
	if g := w.at(62 * time.Minute); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("a new alert should trip the guard: %+v", g.Status)
	}
}

func TestAnExperimentOverrideDoesNotBlockTheOtherOverrides(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	const experiment = 9
	w.fs.set(func(f *fakeFlagsmith) { f.segments[experiment] = true; f.experimentSegment = experiment })
	g := w.tripNewPricing()
	def, seg := w.flag()
	w.fs.mu.Lock()
	exp := w.fs.segments[experiment]
	w.fs.mu.Unlock()
	if def || seg || !exp {
		t.Fatalf("want the default and beta_testers off and the experiment untouched, got default=%v beta=%v experiment=%v", def, seg, exp)
	}
	if !strings.Contains(timelineEvents(g), "SegmentsLeftOn") || contains(g.Status.Snapshot.EnabledSegmentOverrides, experiment) {
		t.Fatalf("the timeline should name the experiment override, and the snapshot leave it out: %v %+v", g.Status.Timeline, g.Status.Snapshot)
	}
}

func TestAPersonTurningTheFlagBackOnClearsUnhealthy(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.tripNewPricing()
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed, got %+v", g.Status)
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || !strings.Contains(ev.FirstText(), "not a verified recovery") {
		t.Fatalf("want an honest Healthy event, got %+v", ev)
	}
}

func TestDeletingATrippedGuardClearsFeatureHealth(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	g := w.tripNewPricing()
	if err := w.c.Delete(w.ctx, g); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}
	if _, err := w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := w.c.Get(w.ctx, key, &v1alpha1.FlagGuard{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the guard should be gone, got %v", err)
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || !strings.Contains(ev.FirstText(), "was deleted during an incident") {
		t.Fatalf("want a Healthy event explaining the deletion, got %+v", ev)
	}
}

func TestARestoreAfterRecoveryIsBlamedWhenItBreaksAgain(t *testing.T) {
	// As on a real cluster: the pods heal without being replaced, so their
	// last crash stays in LastTerminationState.
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped: %+v", g.Status)
	}
	// The restarted pods come back, and one crashes once more before it reads
	// the flag as off, then runs fine: the kubelet keeps that crash.
	w.healthyPods(2, t0.Add(12*time.Second))
	w.healAfterCrash(t0.Add(11*time.Second), t0.Add(12*time.Second))
	w.at(15 * time.Second)
	if g = w.at(36 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered: %+v", g.Status)
	}
	g.Annotations = map[string]string{v1alpha1.RestoreAnnotation: "1"}
	if err := w.c.Update(w.ctx, g); err != nil {
		t.Fatal(err)
	}
	g = w.at(40 * time.Second)
	if g.Status.LastFlagChange == nil || g.Status.LastFlagChange.WorkloadWasFailing {
		t.Fatalf("the crash from before the verified recovery must not count as failing now: %+v", g.Status.LastFlagChange)
	}
	w.crashPods(t0.Add(42 * time.Second))
	if g = w.at(45 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Trigger.ChangedBy != ChangedByRestore {
		t.Fatalf("the restore should trip the breaker again: %+v", g.Status)
	}
}

func TestANamespaceMustOptIntoAnEnvironment(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.r.RequireNamespaceOptIn = true
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	if err := w.c.Create(w.ctx, ns); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}
	if _, err := w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	var g v1alpha1.FlagGuard
	_ = w.c.Get(w.ctx, key, &g)
	if c := findCondition(&g, conditionReady); c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, v1alpha1.AllowedEnvironmentsAnnotation) {
		t.Fatalf("want Ready=False naming the annotation, got %v", g.Status.Conditions)
	}
	if g.Status.FeatureID != 0 {
		t.Fatal("nothing should be read from Flagsmith for a namespace that hasn't opted in")
	}

	ns.Annotations = map[string]string{v1alpha1.AllowedEnvironmentsAnnotation: "other-key, " + envKey}
	if err := w.c.Update(w.ctx, ns); err != nil {
		t.Fatal(err)
	}
	if g := w.at(5 * time.Second); g.Status.FeatureID != featureID {
		t.Fatalf("an allowed environment should be checked: %+v", g.Status)
	}
}
