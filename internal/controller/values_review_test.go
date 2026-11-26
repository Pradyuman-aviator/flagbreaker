package controller

// Regression tests for the review of value incidents. A person often
// switches a flag on and edits its value close together, sometimes in one
// save, so an incident can have both as suspects; Flagbreaker then undoes
// both ("recover first, blame second").

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/changes"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
)

var otherValue = &flagsmith.Value{Type: "string", Value: `{"tax_rate": 0.09}`}

// tolerant reconciles like at, but doesn't fail the test on a failed check
// (for tests that make Flagsmith fail on purpose).
func (w *world) tolerant(d time.Duration) *v1alpha1.FlagGuard {
	w.t.Helper()
	w.now = t0.Add(d)
	key := types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}
	_, _ = w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: key})
	var g v1alpha1.FlagGuard
	if err := w.c.Get(w.ctx, key, &g); err != nil {
		w.t.Fatal(err)
	}
	return &g
}

// offWithGoodValue is a world where the flag is off, with a good value.
func offWithGoodValue(t *testing.T, mode v1alpha1.Mode) *world {
	w := newWorld(t, mode)
	w.fs.set(func(f *fakeFlagsmith) { f.value = goodValue })
	w.at(0)
	return w
}

func TestASwitchOnFollowedByAValueEditIsUndoneWhole(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.value = otherValue })
	w.at(10 * time.Second)
	// It's the switch-on's code path that crashes, but nothing says which.
	w.crashPods(t0.Add(12 * time.Second))
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped, got %+v", g.Status)
	}
	if def, _ := w.flag(); def {
		t.Fatal("the switch-on is a suspect too, so the flag should be off")
	}
	if !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("the value edit is a suspect too, so the previous value should be back, got %+v", w.value())
	}
	if ev := timelineEvents(g); !strings.Contains(ev, "FlagSwitchedOff,ValueReverted,PodsRestarted") {
		t.Fatalf("timeline = %s", ev)
	}
	w.healthyPods(2, t0.Add(17*time.Second))
	w.at(20 * time.Second)
	if g := w.at(41 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
}

func TestAValueSetWhileOffThenASwitchOnIsUndoneWhole(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(10 * time.Second)
	w.crashPods(t0.Add(12 * time.Second))
	if g := w.at(15 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped, got %+v", g.Status)
	}
	if def, _ := w.flag(); def || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("both suspects should be undone: on=%v value=%+v", def, w.value())
	}
}

func TestASwitchOnWithANewValueInOneSaveIsUndoneWhole(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped, got %+v", g.Status)
	}
	if def, _ := w.flag(); def || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("both parts of the save should be undone: on=%v value=%+v", def, w.value())
	}
	if msg := g.Status.Timeline[0].Message; !strings.Contains(msg, "switched on") || !strings.Contains(msg, "value") {
		t.Fatalf("the timeline should name both parts: %s", msg)
	}
}

func TestASwitchOffAndANewValueInOneSaveEndAValueIncident(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = false, goodValue })
	g := w.at(15 * time.Second)
	if g.Status.FlagDisabledAt == nil || !strings.Contains(timelineEvents(g), "PodsRestarted") || len(w.pods()) != 0 {
		t.Fatalf("the person's fix should be recorded and the pods restarted: %s, %d pods", timelineEvents(g), len(w.pods()))
	}
}

func TestANewValueWhileRecoveredIsWatched(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.healthyPods(2, t0.Add(12*time.Second))
	w.at(15 * time.Second)
	if g := w.at(40 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
	// The flag is off now, but the app reads its value anyway.
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(45 * time.Second)
	w.crashPods(t0.Add(47 * time.Second))
	g := w.at(50 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("a bad value on the switched-off flag should trip and be put back: %+v, value %+v", g.Status, w.value())
	}
}

func TestAPersonsValueAfterAFailedRevertIsTheirs(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.fs.set(func(f *fakeFlagsmith) { f.patch500 = true })
	if g := w.tolerant(10 * time.Second); g.Status.SwitchOffAttemptedAt == nil {
		t.Fatalf("the failed revert should be marked: %+v", g.Status)
	}
	// Flagbreaker sent goodValue; a person sets a third one.
	w.fs.set(func(f *fakeFlagsmith) { f.patch500, f.value = false, otherValue })
	g := w.at(15 * time.Second)
	if g.Status.FlagDisabledBy == ChangedByFlagbreaker || g.Status.LastFlagChange.ChangedBy == ChangedByFlagbreaker {
		t.Fatalf("a value Flagbreaker didn't send is a person's: %+v", g.Status)
	}
	if strings.Contains(timelineEvents(g), "ValueReverted") || !strings.Contains(timelineEvents(g), "ValueChangedByPerson") {
		t.Fatalf("timeline = %s", timelineEvents(g))
	}
	w.at(20 * time.Second)
	if !flagsmith.SameValue(w.value(), otherValue) {
		t.Fatalf("the person's value must stand, got %+v", w.value())
	}
}

func TestALateHealthyReportStillSaysTheRecoveryWasVerified(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.healthyPods(2, t0.Add(12*time.Second))
	w.at(15 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.health500 = true })
	w.at(36 * time.Second) // recovered, but the Healthy report fails
	w.at(41 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.health500 = false })
	w.at(46 * time.Second)
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || strings.Contains(ev.FirstText(), "not a verified recovery") ||
		!strings.Contains(ev.FirstText(), "verified healthy again") {
		t.Fatalf("a verified recovery must be reported as one: %s", ev.FirstText())
	}
	if g := w.at(51 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed once Healthy is reported, got %+v", g.Status)
	}
}

func TestAPersonsFixIsNeverBlamed(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.value = goodValue })
	if g := w.at(15 * time.Second); g.Status.LastFlagChange == nil || !g.Status.LastFlagChange.Tripped {
		t.Fatalf("the fix belongs to the incident and must never be blamed: %+v", g.Status.LastFlagChange)
	}
}

func TestOwnValueWebhooksAreNotUsedToNameAPerson(t *testing.T) {
	r := &FlagGuardReconciler{Changes: changes.NewStore(), SyncPeriod: 5 * time.Second}
	reverted := t0.Add(10 * time.Second)
	st := &v1alpha1.FlagGuardStatus{EnvironmentID: envID, FeatureID: featureID,
		Trigger:        &v1alpha1.FlagChange{ValueChanged: true},
		FlagDisabledAt: metaNow(reverted), FlagDisabledBy: ChangedByFlagbreaker}

	// Flagsmith's clock is 2s behind: the webhook for Flagbreaker's own
	// revert says 8s. It must not name who made a value change.
	r.Changes.RecordValue(envID, featureID, changes.Change{At: t0.Add(8 * time.Second), ChangedBy: "org-api-key"})
	if _, ok := r.webhookFor(st, true, false); ok {
		t.Fatal("a value webhook this close to Flagbreaker's own revert may be its own")
	}
	// In a switch-on incident Flagbreaker never edits values, so a value
	// webhook after its switch-off is a person's.
	st.Trigger = &v1alpha1.FlagChange{Enabled: true}
	if w, ok := r.webhookFor(st, true, false); !ok || w.ChangedBy != "org-api-key" {
		t.Fatalf("got %+v, %v", w, ok)
	}
}

func TestASwitchOffEndsACombinedIncidentWithoutAFalseRearm(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	if g := w.at(10 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped, got %+v", g.Status)
	}
	// The person switches the flag off and leaves the new value.
	w.fs.set(func(f *fakeFlagsmith) { f.def = false })
	g := w.at(15 * time.Second)
	if g.Status.FlagDisabledAt == nil || !strings.Contains(timelineEvents(g), "FlagSwitchedOffByPerson") {
		t.Fatalf("the switch-off should end the incident: %s", timelineEvents(g))
	}
	// The value was never put back, so it hasn't "come back": no re-arm.
	if g := w.at(20 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("must stay tripped until the pods recover: %+v", g.Status)
	}
	w.healthyPods(2, t0.Add(21*time.Second))
	w.at(25 * time.Second)
	if g := w.at(46 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
}

// Second review round.

func TestAFailedRevertAfterASwitchOffIsRetried(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.fs.set(func(f *fakeFlagsmith) { f.value500 = true })
	w.tolerant(10 * time.Second) // switched off; the value PATCH fails
	w.fs.set(func(f *fakeFlagsmith) { f.value500 = false })
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("the value must still be put back, not the guard re-armed: phase %s, value %+v, %s", g.Status.Phase, w.value(), timelineEvents(g))
	}
	if def, _ := w.flag(); def {
		t.Fatal("the flag should stay off")
	}
}

func TestATimedOutSwitchOffInACombinedIncidentStillPutsTheValueBack(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.fs.set(func(f *fakeFlagsmith) { f.timeout = true })
	w.tolerant(10 * time.Second) // the switch-off is applied, then times out
	w.fs.set(func(f *fakeFlagsmith) { f.timeout = false })
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("want Tripped with the value put back: phase %s, value %+v, %s", g.Status.Phase, w.value(), timelineEvents(g))
	}
}

func TestATimedOutRevertOnAnOffFlagIsNotCalledASwitchOff(t *testing.T) {
	w := offWithGoodValue(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.fs.set(func(f *fakeFlagsmith) { f.timeout = true })
	w.tolerant(10 * time.Second) // the revert is applied, then times out
	w.fs.set(func(f *fakeFlagsmith) { f.timeout = false })
	g := w.at(15 * time.Second)
	if ev := timelineEvents(g); strings.Contains(ev, "FlagSwitchedOff") || !strings.Contains(ev, "ValueReverted") {
		t.Fatalf("the flag was off all along; timeline = %s", ev)
	}
}

func TestAPersonsSwitchOffKeepsItsOwnTimeDuringAnIncident(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "ErrorRateHigh"}})
	})
	if g := w.at(5 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want an alert trip, got %+v", g.Status)
	}
	w.fs.set(func(f *fakeFlagsmith) { f.value = otherValue }) // pods are healthy
	w.at(10 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def = false })
	g := w.at(15 * time.Second)
	if g.Status.FlagDisabledAt == nil || !g.Status.FlagDisabledAt.Time.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("the switch-off happened after the 10s check, not at the value edit: %+v", g.Status.FlagDisabledAt)
	}
}

func TestAReportSentAfterReArmingStillSaysTheRecoveryWasVerified(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.healthyPods(2, t0.Add(12*time.Second))
	w.at(15 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.health500 = true })
	w.at(36 * time.Second) // recovered; the Healthy report fails
	w.fs.set(func(f *fakeFlagsmith) { f.value = otherValue })
	if g := w.at(41 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("a new value should re-arm the guard, got %+v", g.Status)
	}
	w.fs.set(func(f *fakeFlagsmith) { f.health500 = false })
	w.at(46 * time.Second)
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || strings.Contains(ev.FirstText(), "not a verified recovery") ||
		!strings.Contains(ev.FirstText(), "verified healthy again") {
		t.Fatalf("the recovery was verified: %s", ev.FirstText())
	}
}
