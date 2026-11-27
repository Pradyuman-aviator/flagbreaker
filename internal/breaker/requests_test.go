package breaker

// The requests rules: a guard with spec.requests trips when requests start
// failing with 5xx after a flag change, even while every pod stays up, and
// recovers only once they don't fail any more.

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

var ready = workload.Assessment{Pods: 2, Ready: 2}

// every5s is one sample every 5s, from `from` to `to` after t0, each with
// total requests of which failed failed.
func every5s(from, to time.Duration, total, failed float64) requests.Series {
	var s requests.Series
	for d := from; d <= to; d += 5 * time.Second {
		s = append(s, requests.Sample{At: t0.Add(d), Count: requests.Count{Total: total, Failed: failed}})
	}
	return s
}

// fineBefore: in the minute before a change at t0, 300 requests, none failed.
func fineBefore() requests.Series { return every5s(-60*time.Second, -5*time.Second, 25, 0) }

func withRequests(in Input, series ...requests.Series) Input {
	var all requests.Series
	for _, s := range series {
		all = append(all, s...)
	}
	in.Requests = &Requests{Series: all, MaxErrorPercent: 5, MinRequests: 20}
	return in
}

func TestFailingRequestsTripAfterASwitchOnWithEveryPodReady(t *testing.T) {
	d := Decide(withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), fineBefore(), every5s(5*time.Second, 5*time.Second, 20, 19)))
	if d.Action != Trip || d.Cause != CauseRequests || d.Mode != v1alpha1.ModeAct || len(d.Failing) != 0 {
		t.Fatalf("got %+v, want a trip on requests in act mode", d)
	}
	want := "19 of 20 requests failed (95.0%, over the 5% limit) in the 6s after the flag was switched on by admin@example.com at 10:00:00 UTC, " +
		"against 0 of 300 in the minute before; 2 of 2 pods ready"
	if d.Why != want {
		t.Fatalf("Why = %q\nwant  %q", d.Why, want)
	}
}

func TestFailingRequestsTripAfterANewValueWhileTheFlagIsOff(t *testing.T) {
	change := valueChanged(t0)
	change.Enabled = false
	in := withRequests(armed(t0.Add(6*time.Second), change, ready), fineBefore(), every5s(5*time.Second, 5*time.Second, 20, 19))
	in.FlagEnabled = false
	d := Decide(in)
	if d.Action != Trip || d.Cause != CauseRequests || !strings.Contains(d.Why, "after the flag's value was changed by admin@example.com") {
		t.Fatalf("got %+v, want a trip blamed on the new value", d)
	}
}

func TestFailingRequestsNeedMinRequestsAfterTheChange(t *testing.T) {
	failing := every5s(5*time.Second, 5*time.Second, 19, 19)
	if d := Decide(withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), fineBefore(), failing)); d.Action != Wait {
		t.Fatalf("19 requests are too few to judge: %+v", d)
	}
	more := append(failing, every5s(10*time.Second, 10*time.Second, 5, 5)...)
	if d := Decide(withRequests(armed(t0.Add(11*time.Second), switchedOn(t0), ready), fineBefore(), more)); d.Action != Trip {
		t.Fatalf("24 requests are enough: %+v", d)
	}
}

func TestFailingRequestsAreNotBlamedWhenTheMinuteBeforeWasAlreadyFailing(t *testing.T) {
	before := every5s(-60*time.Second, -5*time.Second, 25, 2) // 8%
	if d := Decide(withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), before, every5s(5*time.Second, 5*time.Second, 20, 19))); d.Action != Wait {
		t.Fatalf("requests were failing before the change: %+v", d)
	}
}

func TestFailingRequestsAreNotBlamedWithoutEnoughTrafficBefore(t *testing.T) {
	after := every5s(5*time.Second, 5*time.Second, 20, 19)
	for name, before := range map[string]requests.Series{
		"too few in the minute before": every5s(-60*time.Second, -5*time.Second, 1, 0),
		"traffic only before that":     every5s(-120*time.Second, -70*time.Second, 25, 0),
		"none at all":                  nil,
	} {
		if d := Decide(withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), before, after)); d.Action != Wait {
			t.Errorf("%s: nothing to compare with, so no trip: %+v", name, d)
		}
	}
}

func TestFailingRequestsFollowTheExistingBlameRules(t *testing.T) {
	failingAfter := every5s(5*time.Second, 5*time.Second, 20, 19)
	cases := map[string]func(in *Input){
		"outside the window": func(in *Input) {
			in.Now = t0.Add(11 * time.Minute)
			in.Requests.Series = append(fineBefore(), every5s(10*time.Minute+5*time.Second, 11*time.Minute, 20, 19)...)
		},
		"acknowledged":          func(in *Input) { in.LastChange.Acknowledged = true },
		"already tripped":       func(in *Input) { in.LastChange.Tripped = true },
		"time unknown":          func(in *Input) { in.LastChange.TimeUnknown = true },
		"pods already failing":  func(in *Input) { in.LastChange.WorkloadWasFailing = true },
		"a switch-off":          func(in *Input) { in.LastChange = &v1alpha1.FlagChange{At: metav1.NewTime(t0)}; in.FlagEnabled = false },
		"no change seen at all": func(in *Input) { in.LastChange = nil },
	}
	for name, change := range cases {
		in := withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), fineBefore(), failingAfter)
		change(&in)
		if d := Decide(in); d.Action != Wait {
			t.Errorf("%s: got %+v, want Wait", name, d)
		}
	}
}

func TestFailingRequestsNearARolloutOnlyReport(t *testing.T) {
	in := withRequests(armed(t0.Add(6*time.Second), switchedOn(t0), ready), fineBefore(), every5s(5*time.Second, 5*time.Second, 20, 19))
	in.RolloutStartedAt = t0.Add(-30 * time.Second)
	d := Decide(in)
	if d.Action != Trip || d.Cause != CauseRequests || d.Mode != v1alpha1.ModeWatch || d.ModeReason == "" ||
		!strings.Contains(d.Why, "; a rollout also started at 09:59:30, so the deploy may be the cause and the flag is left on") {
		t.Fatalf("got %+v, want a watch-mode trip that names the rollout", d)
	}
}

func TestFailingPodsStillTripFirst(t *testing.T) {
	pods := failing(2, 0, t0.Add(4*time.Second), t0.Add(5*time.Second))
	without := Decide(armed(t0.Add(20*time.Second), switchedOn(t0), pods))
	d := Decide(withRequests(armed(t0.Add(20*time.Second), switchedOn(t0), pods), fineBefore(), every5s(5*time.Second, 20*time.Second, 20, 19)))
	if d.Action != Trip || d.Cause != CausePods || d.Why != without.Why || len(d.Failing) != 2 {
		t.Fatalf("got %+v, want the pods trip, as without requests: %q", d, without.Why)
	}
}

func TestTheWhyNamesTheLimitAndWhereItCameFrom(t *testing.T) {
	now := t0.Add(4*time.Minute + 10*time.Second)
	in := withRequests(armed(now, switchedOn(t0), ready), fineBefore(), every5s(3*time.Minute+15*time.Second, 4*time.Minute+10*time.Second, 50, 2))
	if d := Decide(in); d.Action != Wait {
		t.Fatalf("4%% failing is within the 5%% limit: %+v", d)
	}
	in.Requests.MaxErrorPercent, in.Requests.LimitFrom = 2, "flagbreaker_policy"
	d := Decide(in)
	want := "24 of 600 requests failed (4.0%, over the 2% limit in flagbreaker_policy) in the last minute, 4m10s after the flag was switched on by admin@example.com at 10:00:00 UTC, " +
		"against 0 of 300 in the minute before; 2 of 2 pods ready"
	if d.Action != Trip || d.Why != want {
		t.Fatalf("got %+v\nwant Why %q", d, want)
	}
}

// recovering is a tripped guard whose pods have been healthy since t0+20s.
func recovering(now time.Duration, series requests.Series) Input {
	since := t0.Add(20 * time.Second)
	in := withRequests(tripped(t0.Add(now), ready), series)
	in.StableFor = 20 * time.Second
	in.HealthySince = &since
	return in
}

func TestRecoveryWaitsWhileRequestsFail(t *testing.T) {
	d := Decide(recovering(45*time.Second, every5s(25*time.Second, 45*time.Second, 20, 10)))
	if d.Action != Wait || d.HealthySince != nil {
		t.Fatalf("failing requests count like a crash: got %+v, want Wait with HealthySince cleared", d)
	}
}

func TestARequestsIncidentRecoversOnlyAfterEnoughRequestsSucceed(t *testing.T) {
	for name, series := range map[string]requests.Series{
		"no traffic":         every5s(25*time.Second, 45*time.Second, 0, 0),
		"too little traffic": every5s(25*time.Second, 45*time.Second, 3, 0),
	} {
		in := recovering(45*time.Second, series)
		in.RequestsIncident = true
		if d := Decide(in); d.Action != Wait || d.HealthySince == nil || !d.HealthySince.Equal(t0.Add(20*time.Second)) {
			t.Fatalf("%s: got %+v, want Wait with HealthySince kept", name, d)
		}
	}
	in := recovering(45*time.Second, every5s(25*time.Second, 45*time.Second, 30, 1))
	in.RequestsIncident = true
	d := Decide(in)
	if want := "all 2 pods ready with no new crashes for 25s, and 5 of 150 requests failed in that time"; d.Action != Recover || d.Why != want {
		t.Fatalf("got %+v, want Recover with Why %q", d, want)
	}
}

func TestAPodsIncidentOfAGuardThatReadsRequestsDoesNotWaitForTraffic(t *testing.T) {
	d := Decide(recovering(45*time.Second, nil))
	if want := "all 2 pods ready with no new crashes for 25s"; d.Action != Recover || d.Why != want {
		t.Fatalf("got %+v, want Recover with Why %q", d, want)
	}
}

func TestARequestsIncidentRecoversOnPodsOnceRequestsAreNoLongerRead(t *testing.T) {
	in := recovering(45*time.Second, nil)
	in.RequestsIncident, in.Requests = true, nil // spec.requests was removed
	if d := Decide(in); d.Action != Recover {
		t.Fatalf("got %+v, want Recover on the pods alone", d)
	}
}
