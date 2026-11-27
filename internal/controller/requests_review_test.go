package controller

// Regression tests for what the review of request checking found.

import (
	"strings"
	"testing"
	"time"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

// ms is a time after t0 in milliseconds. In a cluster, checks come at any
// fraction of a second, and the guard's status keeps its times to the second.
func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestATripAtFractionalSecondsCountsOnlyTheRequestsAfterTheChange(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	w.at(ms(700))
	m.traffic(30, 0)
	w.at(ms(5700))
	m.traffic(30, 0)
	w.at(ms(10700)) // the last read before the switch-on: 30 went through
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(25, 1) // the app hasn't read the flag yet
	if g := w.at(ms(15700)); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("1 of 26 is within the limit: %+v", g.Status)
	}
	m.traffic(0, 25)
	g := w.at(ms(20700))
	if g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped: %+v", g.Status)
	}
	// 51 requests came after the change, 26 of them failed. The 30 read at
	// 10.7s came before it.
	want := "Deployment shop/checkout: 26 of 51 requests failed (51.0%, over the 5% limit) in the 11s after the flag was switched on at 10:00:10 UTC, " +
		"against 0 of 30 in the minute before; 2 of 2 pods ready"
	if msg := g.Status.Timeline[1].Message; msg != want {
		t.Fatalf("Tripped = %q\nwant      %q", msg, want)
	}
}

func TestRecoveryAtFractionalSecondsCountsOnlyTheRequestsSinceHealthySince(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	w.at(ms(700))
	m.traffic(30, 0)
	w.at(ms(5700))
	m.traffic(30, 0)
	w.at(ms(10700))
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(1, 29)
	if g := w.at(ms(15700)); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped: %+v", g.Status)
	}
	// The app takes a moment to read the switch-off: 5 more payments fail,
	// all before the first healthy sighting at 20.7s.
	m.traffic(25, 5)
	if g := w.at(ms(20700)); g.Status.HealthySince == nil {
		t.Fatalf("want the first healthy sighting: %+v", g.Status)
	}
	var g *v1alpha1.FlagGuard
	for n := 25700; n <= 35700; n += 5000 {
		m.traffic(30, 0)
		g = w.at(ms(n))
		if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.HealthySince == nil || !g.Status.HealthySince.Time.Equal(t0.Add(20*time.Second)) {
			t.Fatalf("at %dms: the failures before healthySince must not count after it: %+v", n, g.Status)
		}
	}
	m.traffic(30, 0)
	g = w.at(ms(40700))
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered 20s after healthySince: %+v", g.Status)
	}
	for _, e := range g.Status.Timeline {
		if e.Event == "Recovered" && !strings.HasPrefix(e.Message, "Verified recovery: all 2 pods ready with no new crashes for 21s, and 0 of 120 requests failed in that time.") {
			t.Fatalf("Recovered = %q", e.Message)
		}
	}
}
