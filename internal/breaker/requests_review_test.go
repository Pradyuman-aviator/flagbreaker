package breaker

// Regression tests for what the review of request checking found.

import (
	"testing"
	"time"

	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

func TestTheMinuteBeforeAChangeStartsAtTheLastVerifiedRecovery(t *testing.T) {
	// An incident's failing requests up to 25s before the switch-on at t0,
	// then a verified recovery from there, with 100 requests going through.
	incident := every5s(-60*time.Second, -25*time.Second, 25, 20)
	recovered := every5s(-20*time.Second, -5*time.Second, 25, 0)
	after := every5s(5*time.Second, 5*time.Second, 20, 19)
	in := withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), incident, recovered, after)
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("without the recovery, the minute before was failing: %+v", d)
	}
	in.Requests.NotBefore = t0.Add(-25 * time.Second)
	d := Decide(in)
	want := "19 of 20 requests failed (95.0%, over the 5% limit) in the 6s after the flag was switched on by admin@example.com at 10:00:00 UTC, " +
		"against 0 of 100 in the minute before; 2 of 2 pods ready"
	if d.Action != Trip || d.Cause != CauseRequests || d.Why != want {
		t.Fatalf("got %+v\nwant Why %q", d, want)
	}
}

// noisyBefore: in the minute before t0, 8% of the requests failed, over the
// 5% limit. For some workloads that is normal.
func noisyBefore() requests.Series { return every5s(-60*time.Second, -5*time.Second, 25, 2) }

func TestOnlyATripOnThePodsNotesRequestsThatAlreadyFailedBefore(t *testing.T) {
	pods := failing(2, 0, t0.Add(4*time.Second), t0.Add(5*time.Second))
	for name, c := range map[string]struct {
		before requests.Series
		want   bool
	}{
		"failing before": {noisyBefore(), true},
		"fine before":    {fineBefore(), false},
		"too few before": {every5s(-60*time.Second, -5*time.Second, 1, 1), false},
	} {
		d := Decide(withRequests(armed(t0.Add(20*time.Second), switchedOn(t0), pods), c.before))
		if d.Action != Trip || d.Cause != CausePods || d.RequestsFailingBefore != c.want {
			t.Errorf("%s: got %+v, want a pods trip with RequestsFailingBefore %v", name, d, c.want)
		}
	}
	if d := Decide(armed(t0.Add(20*time.Second), switchedOn(t0), pods)); d.Action != Trip || d.RequestsFailingBefore {
		t.Errorf("a guard that doesn't read requests: got %+v", d)
	}

	// An alert's trip notes nothing. The minute before an alert is when the
	// failures it reports began, not the workload's normal, so failing
	// requests hold its recovery.
	in := withRequests(armed(t0.Add(10*time.Second), switchedOn(t0.Add(-time.Hour)), ready), noisyBefore())
	in.Alerts = []Alert{alert(41, t0)}
	if d := Decide(in); d.Action != Trip || d.Cause != CauseAlert || d.RequestsFailingBefore {
		t.Errorf("an alert that started at t0: got %+v, want no RequestsFailingBefore", d)
	}
}

func TestRequestsThatFailedAsMuchBeforeTheIncidentDoNotHoldItsRecovery(t *testing.T) {
	// 8% fail, as before the incident.
	in := recovering(45*time.Second, every5s(25*time.Second, 45*time.Second, 25, 2))
	if d := Decide(in); d.Action != Wait || d.HealthySince != nil {
		t.Fatalf("unless they failed as much before the incident, failing requests hold the recovery: %+v", d)
	}
	in.RequestsFailingBefore = true
	d := Decide(in)
	if want := "all 2 pods ready with no new crashes for 25s, and 10 of 125 requests failed in that time"; d.Action != Recover || d.Why != want {
		t.Fatalf("got %+v, want Recover with Why %q", d, want)
	}
	// The pods still decide.
	in.Pods = workload.Assessment{Pods: 2, Ready: 1}
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("a pod isn't ready: %+v", d)
	}
}
