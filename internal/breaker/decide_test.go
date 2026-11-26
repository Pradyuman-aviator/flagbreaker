package breaker

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func switchedOn(at time.Time) *v1alpha1.FlagChange {
	return &v1alpha1.FlagChange{At: metav1.NewTime(at), Enabled: true, ChangedBy: "admin@example.com"}
}

func failing(pods, ready int, at ...time.Time) workload.Assessment {
	a := workload.Assessment{Pods: pods, Ready: ready}
	for i, t := range at {
		a.Problems = append(a.Problems, workload.Problem{Pod: string(rune('a' + i)), Reason: "crash", At: t})
	}
	return a
}

func armed(now time.Time, change *v1alpha1.FlagChange, pods workload.Assessment) Input {
	return Input{
		Now: now, Phase: v1alpha1.PhaseArmed, Mode: v1alpha1.ModeAct,
		FlagEnabled: true, LastChange: change, Pods: pods, DesiredReplicas: int32(pods.Pods),
		RolloutStartedAt: t0.Add(-time.Hour),
		Window:           10 * time.Minute, StableFor: 30 * time.Second, FailingPods: 1,
	}
}

func TestTripsWhenPodsFailAfterFlagIsSwitchedOn(t *testing.T) {
	d := Decide(armed(t0.Add(20*time.Second), switchedOn(t0), failing(2, 0, t0.Add(4*time.Second), t0.Add(5*time.Second))))
	if d.Action != Trip || d.Mode != v1alpha1.ModeAct {
		t.Fatalf("got %+v, want Trip in act mode", d)
	}
	if !strings.Contains(d.Why, "2 of 2 pods started failing 4s after the flag was switched on by admin@example.com") {
		t.Fatalf("Why = %q", d.Why)
	}
}

func TestDoesNotTripOnCrashesFromBeforeTheChange(t *testing.T) {
	d := Decide(armed(t0.Add(20*time.Second), switchedOn(t0), failing(1, 0, t0.Add(-time.Minute))))
	if d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func TestDoesNotTripWhenWorkloadWasAlreadyFailing(t *testing.T) {
	change := switchedOn(t0)
	change.WorkloadWasFailing = true
	if d := Decide(armed(t0.Add(20*time.Second), change, failing(1, 0, t0.Add(5*time.Second)))); d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func TestDoesNotTripAfterTheWindow(t *testing.T) {
	if d := Decide(armed(t0.Add(11*time.Minute), switchedOn(t0), failing(1, 0, t0.Add(11*time.Minute)))); d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func TestDoesNotTripWhenFlagWasSwitchedOff(t *testing.T) {
	in := armed(t0.Add(20*time.Second), &v1alpha1.FlagChange{At: metav1.NewTime(t0), Enabled: false}, failing(1, 0, t0.Add(5*time.Second)))
	in.FlagEnabled = false
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func TestNeedsEnoughFailingPods(t *testing.T) {
	in := armed(t0.Add(20*time.Second), switchedOn(t0), failing(3, 2, t0.Add(5*time.Second)))
	in.FailingPods = 2
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("got %+v, want Wait with 1 of the 2 required pods failing", d)
	}
}

func TestRolloutAtTheSameTimeOnlyReports(t *testing.T) {
	in := armed(t0.Add(20*time.Second), switchedOn(t0), failing(1, 0, t0.Add(5*time.Second)))
	in.RolloutStartedAt = t0.Add(2 * time.Second)
	d := Decide(in)
	if d.Action != Trip || d.Mode != v1alpha1.ModeWatch {
		t.Fatalf("got %+v, want Trip in watch mode", d)
	}
	if !strings.Contains(d.Why, "the deploy may be the cause") || !strings.Contains(d.ModeReason, "rolled out") {
		t.Fatalf("Why = %q, ModeReason = %q", d.Why, d.ModeReason)
	}
}

func tripped(now time.Time, pods workload.Assessment) Input {
	in := armed(now, switchedOn(t0), pods)
	in.Phase = v1alpha1.PhaseTripped
	in.FlagEnabled = false
	off := t0.Add(10 * time.Second)
	in.FlagDisabledAt = &off
	return in
}

func TestRecoversAfterPodsStayHealthy(t *testing.T) {
	healthy := workload.Assessment{Pods: 2, Ready: 2}

	first := Decide(tripped(t0.Add(40*time.Second), healthy))
	if first.Action != Wait || first.HealthySince == nil || !first.HealthySince.Equal(t0.Add(40*time.Second)) {
		t.Fatalf("first healthy sighting: got %+v, want Wait with HealthySince set", first)
	}

	in := tripped(t0.Add(71*time.Second), healthy)
	in.HealthySince = first.HealthySince
	if d := Decide(in); d.Action != Recover || !strings.Contains(d.Why, "for 31s") {
		t.Fatalf("after StableFor: got %+v, want Recover", d)
	}
}

func TestCrashResetsRecovery(t *testing.T) {
	since := t0.Add(40 * time.Second)
	in := tripped(t0.Add(60*time.Second), failing(2, 2, t0.Add(50*time.Second)))
	in.HealthySince = &since
	if d := Decide(in); d.Action != Wait || d.HealthySince != nil {
		t.Fatalf("got %+v, want Wait with HealthySince cleared", d)
	}
}

func TestRetriesSwitchOffInActMode(t *testing.T) {
	in := tripped(t0.Add(20*time.Second), failing(1, 0, t0.Add(5*time.Second)))
	in.FlagEnabled = true
	in.FlagDisabledAt = nil
	if d := Decide(in); d.Action != SwitchOff {
		t.Fatalf("got %+v, want SwitchOff", d)
	}
}

func TestWatchModeWaitsForAPerson(t *testing.T) {
	in := tripped(t0.Add(20*time.Second), failing(1, 0, t0.Add(5*time.Second)))
	in.Mode = v1alpha1.ModeWatch
	in.FlagEnabled = true
	in.FlagDisabledAt = nil
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func alert(id int64, at time.Time) Alert {
	return Alert{EventID: id, Provider: "Grafana", Title: "CheckoutErrorRateHigh", At: at}
}

func TestTripsOnAnotherProvidersAlertWhileTheFlagIsOn(t *testing.T) {
	// No recent change and healthy pods: only the alert says something is wrong.
	in := armed(t0.Add(time.Hour), switchedOn(t0), workload.Assessment{Pods: 2, Ready: 2})
	in.Alerts = []Alert{alert(41, t0.Add(59*time.Minute))}
	d := Decide(in)
	if d.Action != Trip || d.Cause != CauseAlert || d.Alert == nil || d.Alert.EventID != 41 {
		t.Fatalf("got %+v, want a Trip caused by alert 41", d)
	}
	if !strings.Contains(d.Why, `Grafana reported "CheckoutErrorRateHigh"`) {
		t.Fatalf("Why = %q", d.Why)
	}
}

func TestAnAlertAlreadyHandledDoesNotTripAgain(t *testing.T) {
	in := armed(t0.Add(time.Hour), switchedOn(t0), workload.Assessment{Pods: 2, Ready: 2})
	in.Alerts = []Alert{alert(41, t0)}
	in.HandledEventID = 41
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("got %+v, want Wait for an alert a person already re-enabled the flag past", d)
	}
}

func TestAnAlertDoesNothingWhileTheFlagIsOff(t *testing.T) {
	in := armed(t0.Add(time.Hour), nil, workload.Assessment{Pods: 2, Ready: 2})
	in.FlagEnabled = false
	in.Alerts = []Alert{alert(41, t0)}
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("got %+v, want Wait", d)
	}
}

func TestRecoveryWaitsForOtherProvidersAlertsToClear(t *testing.T) {
	healthy := workload.Assessment{Pods: 2, Ready: 2}
	since := t0.Add(20 * time.Second)
	in := tripped(t0.Add(5*time.Minute), healthy)
	in.HealthySince = &since
	in.Alerts = []Alert{alert(41, t0)}
	d := Decide(in)
	if d.Action != Wait || d.HealthySince == nil {
		t.Fatalf("got %+v, want Wait while the alert fires, still counting healthy time", d)
	}
	in.Alerts = nil
	if d := Decide(in); d.Action != Recover {
		t.Fatalf("got %+v, want Recover once the alert cleared", d)
	}
}

func TestADismissedIncidentRearmsAndIsNotBlamedAgain(t *testing.T) {
	in := tripped(t0.Add(20*time.Second), failing(2, 0, t0.Add(5*time.Second)))
	in.Mode, in.FlagEnabled, in.FlagDisabledAt = v1alpha1.ModeWatch, true, nil
	in.Dismissed = true
	d := Decide(in)
	if d.Action != Rearm || !d.Acknowledge {
		t.Fatalf("got %+v, want an acknowledging Rearm", d)
	}

	change := switchedOn(t0)
	change.Acknowledged = true
	if d := Decide(armed(t0.Add(30*time.Second), change, failing(2, 0, t0.Add(5*time.Second)))); d.Action != Wait {
		t.Fatalf("got %+v, want Wait: the change was acknowledged", d)
	}
}

func TestAWorkloadScaledToZeroCountsAsRecovered(t *testing.T) {
	in := tripped(t0.Add(time.Minute), workload.Assessment{})
	in.DesiredReplicas = 0
	since := t0.Add(20 * time.Second)
	in.HealthySince = &since
	if d := Decide(in); d.Action != Recover {
		t.Fatalf("got %+v, want Recover: nothing is left to fail", d)
	}
}

func TestRearmsWhenAPersonTurnsTheFlagBackOn(t *testing.T) {
	in := tripped(t0.Add(20*time.Second), failing(1, 0, t0.Add(5*time.Second)))
	in.FlagEnabled = true
	if d := Decide(in); d.Action != Rearm {
		t.Fatalf("tripped: got %+v, want Rearm", d)
	}

	in.Phase = v1alpha1.PhaseRecovered
	if d := Decide(in); d.Action != Rearm {
		t.Fatalf("recovered: got %+v, want Rearm", d)
	}
}
