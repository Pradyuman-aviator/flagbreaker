package breaker

// Regression tests for what the review of request checking found.

import (
	"testing"
	"time"
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
