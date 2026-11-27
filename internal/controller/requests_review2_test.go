package controller

// Regression tests for what the second review of request checking found:
// which incidents failing requests hold the recovery of.

import (
	"strings"
	"testing"
	"time"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
)

func TestAnAlertIncidentStaysTrippedWhileRequestsStillFail(t *testing.T) {
	// The flag was on before Flagbreaker first looked, so no change of it is
	// blamed. Requests start failing, 30% of them, and a minute later a
	// Grafana error-rate alert fires.
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(0)
	for s := 5; s <= 30; s += 5 {
		m.traffic(30, 0)
		w.at(time.Duration(s) * time.Second)
	}
	for s := 35; s <= 90; s += 5 {
		m.traffic(21, 9)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
			t.Fatalf("at %ds: no change to blame: %+v", s, g.Status)
		}
	}
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "CheckoutErrorRateHigh", Text: "5xx over 20%"}})
	})
	m.traffic(21, 9)
	g := w.at(95 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Alert == nil || g.Status.RequestsIncident {
		t.Fatalf("want a trip on the alert: %+v", g.Status)
	}
	if def, _ := w.flag(); def {
		t.Fatal("the flag should be off")
	}

	// With the flag off, 13% still fail: under the alert's own threshold, so
	// it resolves, but over the 5% limit. The minute before the alert was
	// when the failures it reported began, not how this workload always is,
	// so the incident isn't over.
	for s := 100; s <= 115; s += 5 {
		m.traffic(26, 4)
		w.at(time.Duration(s) * time.Second)
	}
	w.fs.set(func(f *fakeFlagsmith) { f.addEvent(flagsmith.ProviderGrafana, "fp1", "HEALTHY", nil) })
	for s := 120; s <= 160; s += 5 {
		m.traffic(26, 4)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
			t.Fatalf("at %ds: 13%% of the requests still fail, so the workload hasn't recovered: %v", s, g.Status.Timeline)
		}
	}
	if ev := w.fs.lastOwnEvent(t); !ev.Unhealthy() {
		t.Fatalf("Feature Health must still say Unhealthy: %+v", ev)
	}

	// The cause is fixed and the requests go through: it recovers once they
	// have for stableFor.
	for s := 165; s <= 175; s += 5 {
		m.traffic(30, 0)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
			t.Fatalf("at %ds: recovery needs stableFor (20s): %+v", s, g.Status)
		}
	}
	m.traffic(30, 0)
	g = w.at(180 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered: %v", g.Status.Timeline)
	}
	for _, e := range g.Status.Timeline {
		if e.Event == "Recovered" && !strings.HasPrefix(e.Message, "Verified recovery: all 2 pods ready with no new crashes for 20s, and 0 of 120 requests failed in that time.") {
			t.Fatalf("Recovered = %q", e.Message)
		}
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() {
		t.Fatalf("want Healthy reported: %+v", ev)
	}
}
