package controller

// Incidents caused by a change to a flag's value (its remote config), not
// by switching it on. A bad value can crash every pod that reads it, and
// switching the flag off doesn't help an app that reads the value anyway,
// so in act mode Flagbreaker puts the previous value back.

import (
	"strings"
	"testing"
	"time"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
)

var (
	goodValue = &flagsmith.Value{Type: "string", Value: `{"tax_rate": 0.08}`}
	badValue  = &flagsmith.Value{Type: "string", Value: `{"tax_rate": "8%"}`}
)

// valueWorld is a world where the flag is on, with a good value, and the
// guard has had its first look.
func valueWorld(t *testing.T, mode v1alpha1.Mode) *world {
	w := newWorld(t, mode)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, goodValue })
	w.at(0)
	return w
}

func (w *world) value() *flagsmith.Value {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	return w.fs.value
}

func TestABadValueIsPutBackAndTheWorkloadRecovers(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	if g := w.at(5 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed || g.Status.FlagValue == nil {
		t.Fatalf("first checks: %+v", g.Status)
	}

	// A person sets a bad value; two seconds later the pods crash.
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	if g := w.at(10 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed || g.Status.LastFlagChange == nil || !g.Status.LastFlagChange.ValueChanged {
		t.Fatalf("the value change should be recorded while the pods are fine: %+v", g.Status)
	}
	w.crashPods(t0.Add(12 * time.Second))
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Trigger == nil || !g.Status.Trigger.ValueChanged {
		t.Fatalf("want a trip blamed on the value change, got %+v", g.Status)
	}
	if v := w.value(); !flagsmith.SameValue(v, goodValue) {
		t.Fatalf("the previous value should be back, got %+v", v)
	}
	if def, seg := w.flag(); !def || !seg {
		t.Fatalf("putting the value back must not switch anything off: default=%v segment=%v", def, seg)
	}
	if want := "FlagChanged,Tripped,ValueReverted,PodsRestarted"; timelineEvents(g) != want {
		t.Fatalf("timeline = %s, want %s", timelineEvents(g), want)
	}
	if msg := g.Status.Timeline[0].Message; !strings.Contains(msg, `from "{\"tax_rate\": 0.08}" to "{\"tax_rate\": \"8%\"}"`) {
		t.Fatalf("the timeline should show both values: %s", msg)
	}
	if ev := w.fs.lastOwnEvent(t); !ev.Unhealthy() || !strings.Contains(ev.FirstText(), "put the previous value back") {
		t.Fatalf("Unhealthy event = %+v", ev)
	}
	if len(w.pods()) != 0 {
		t.Fatalf("crashed pods should have been restarted, %d left", len(w.pods()))
	}

	// The replacements come up healthy; recovery needs 20s of that.
	w.healthyPods(2, t0.Add(17*time.Second))
	w.at(20 * time.Second)
	g = w.at(41 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %s: %v", g.Status.Phase, g.Status.Timeline)
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || !strings.Contains(ev.FirstText(), "previous value put back") {
		t.Fatalf("the Healthy event should give the timings: %+v", ev)
	}
	// Nothing is left to wait for: the flag stayed on, with a value that works.
	if g := w.at(46 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed right after recovery, got %+v", g.Status)
	}

	// Flagbreaker's own change is never blamed. If it were, a later crash
	// would put the bad value back.
	w.crashPods(t0.Add(48 * time.Second))
	if g := w.at(50 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("a crash after Flagbreaker's own fix must not trip: %+v", g.Status)
	}
	if v := w.value(); !flagsmith.SameValue(v, goodValue) || w.fs.patches != 1 {
		t.Fatalf("Flagbreaker must not undo its own fix: value=%+v patches=%d", v, w.fs.patches)
	}
}

func TestWatchModeWaitsForAPersonToChangeTheValue(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeWatch {
		t.Fatalf("want Tripped in watch mode, got %+v", g.Status)
	}
	if !flagsmith.SameValue(w.value(), badValue) || w.fs.patches != 0 {
		t.Fatal("watch mode must not change the flag")
	}
	if last := g.Status.Timeline[len(g.Status.Timeline)-1]; last.Event != "WatchOnly" || !strings.Contains(last.Message, "previous value back") {
		t.Fatalf("the timeline should say what to do: %+v", last)
	}
	if ev := w.fs.lastOwnEvent(t); !strings.Contains(ev.FirstText(), "the new value is still in place") {
		t.Fatalf("Unhealthy event = %+v", ev)
	}

	// A person puts the good value back. Flagbreaker records that and
	// restarts the crashed pods.
	w.fs.set(func(f *fakeFlagsmith) { f.value = goodValue })
	g = w.at(20 * time.Second)
	if g.Status.FlagDisabledAt == nil || !strings.Contains(timelineEvents(g), "ValueChangedByPerson,PodsRestarted") {
		t.Fatalf("the person's change was not recorded: %v", g.Status.Timeline)
	}
	w.healthyPods(2, t0.Add(22*time.Second))
	w.at(25 * time.Second)
	if g := w.at(46 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
	if g := w.at(50 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed again, got %+v", g.Status)
	}
	if w.fs.patches != 0 {
		t.Fatal("watch mode must never change the flag")
	}
}

func TestSwitchingOffDoesNotEndAValueIncident(t *testing.T) {
	// Many apps read a value whether or not the flag is on, so a switch-off
	// isn't taken as the fix: only a different value is.
	w := valueWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	w.at(10 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def = false })
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.FlagDisabledAt != nil {
		t.Fatalf("a switch-off must not count as fixing a bad value: %+v", g.Status)
	}
	// The pods still decide: if they heal anyway, that is a recovery.
	w.healthyPods(2, t0.Add(16*time.Second))
	w.at(20 * time.Second)
	if g := w.at(41 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered once the pods are healthy, got %+v", g.Status)
	}
}

func TestChangeRequestsKeepAValueIncidentToWatching(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.conflict, f.value = true, badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeWatch {
		t.Fatalf("change requests should keep it to watching: %+v", g.Status)
	}
	if c := findCondition(g, conditionBlocked); c == nil || !strings.Contains(timelineEvents(g), "BlockedByChangeRequests") {
		t.Fatalf("want the BlockedByChangeRequests condition and timeline entry: %v", g.Status.Timeline)
	}
	if !flagsmith.SameValue(w.value(), badValue) {
		t.Fatal("Flagbreaker must never get around change requests")
	}
}

func TestAGuardFromBeforeValuesWereTrackedIsNotTrippedByItsFirstValue(t *testing.T) {
	w := valueWorld(t, v1alpha1.ModeAct)
	// A guard written by v0.1.0 has no status.flagValue.
	g := w.at(5 * time.Second)
	g.Status.FlagValue = nil
	if err := w.c.Status().Update(w.ctx, g); err != nil {
		t.Fatal(err)
	}
	w.crashPods(t0.Add(8 * time.Second))
	g = w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseArmed || g.Status.FlagValue == nil {
		t.Fatalf("the first value seen is remembered, not blamed: %+v", g.Status)
	}
	if c := g.Status.LastFlagChange; c != nil && c.ValueChanged {
		t.Fatalf("no value change happened: %+v", c)
	}
}

func TestANewValueOnAnOffFlagTripsOnlyOnce(t *testing.T) {
	// The flag is off, but the app reads its value anyway.
	w := newWorld(t, v1alpha1.ModeWatch)
	w.fs.set(func(f *fakeFlagsmith) { f.value = goodValue })
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	if g := w.at(10 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("a bad value on an off flag should trip: %+v", g.Status)
	}
	// The pods heal with the new value still in place, then crash again.
	w.healthyPods(2, t0.Add(12*time.Second))
	w.at(15 * time.Second)
	w.at(36 * time.Second) // recovered
	if g := w.at(40 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed, got %+v", g.Status)
	}
	w.crashPods(t0.Add(42 * time.Second))
	if g := w.at(45 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed || w.ownEvents("UNHEALTHY") != 1 {
		t.Fatalf("the same change must not trip twice: %+v", g.Status)
	}
}
