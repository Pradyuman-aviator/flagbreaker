// Package breaker holds Flagbreaker's decisions as one pure function. The
// controller gathers the facts (what the flag serves, what the pods are doing,
// what Feature Health says) and Decide says what to do. Keeping the rules
// here, away from any API call, makes them easy to read and to test.
package breaker

import (
	"fmt"
	"time"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

// Action is what the controller should do next.
type Action string

const (
	// Wait means there is nothing to do.
	Wait Action = ""
	// Trip means the workload broke after the flag changed, or another
	// Feature Health provider marked the flag unhealthy: report Unhealthy to
	// Feature Health and, in act mode, switch the flag off.
	Trip Action = "Trip"
	// SwitchOff retries switching the flag off after a failed attempt.
	SwitchOff Action = "SwitchOff"
	// Recover means the workload is verified healthy again: report Healthy.
	Recover Action = "Recover"
	// Rearm means a person turned the flag back on, or dismissed the
	// incident: watch the flag again.
	Rearm Action = "Rearm"
)

// Cause says what tripped the breaker.
type Cause string

const (
	// CausePods means the workload's pods failed after a flag change.
	CausePods Cause = "pods"
	// CauseAlert means another Feature Health provider reported the flag
	// unhealthy.
	CauseAlert Cause = "alert"
)

// rolloutSlack is how close to a flag change a rollout must start for the
// breakage to count as possibly caused by the deploy.
const rolloutSlack = 30 * time.Second

// Alert is an active Unhealthy event from another Feature Health provider.
type Alert struct {
	EventID  int64
	Provider string
	Title    string
	At       time.Time
}

// Input is everything Decide needs to know.
type Input struct {
	Now   time.Time
	Phase v1alpha1.Phase
	// Mode is the mode in effect: from the FlagGuard and Flagsmith, or, while
	// tripped, the mode the trip happened in.
	Mode v1alpha1.Mode

	// FlagEnabled is what the flag's environment default serves now.
	FlagEnabled bool
	// LastChange is the latest change to the flag that Flagbreaker saw.
	LastChange *v1alpha1.FlagChange

	Pods            workload.Assessment
	DesiredReplicas int32
	// RolloutStartedAt is when the workload's newest revision was created.
	RolloutStartedAt time.Time

	// Alerts are the Unhealthy events other Feature Health providers report
	// for this flag and environment right now.
	Alerts []Alert
	// HandledEventID is the newest alert event already acted on.
	HandledEventID int64
	// Dismissed is true when a person dismissed Flagbreaker's Unhealthy event
	// for the current incident in Flagsmith.
	Dismissed bool

	Window      time.Duration
	StableFor   time.Duration
	FailingPods int

	FlagDisabledAt *time.Time
	HealthySince   *time.Time
}

// Decision is what to do, and why.
type Decision struct {
	Action Action
	// Mode is the mode to act in. A trip drops to watch when a rollout
	// started around the same time, because the deploy may be the cause.
	Mode v1alpha1.Mode
	// ModeReason, when set, says why Decide changed the mode.
	ModeReason string
	// Cause is what tripped the breaker, for a Trip.
	Cause Cause
	// Alert is the alert behind a Trip caused by an alert.
	Alert *Alert
	// Why explains the decision in one sentence, for the incident timeline.
	Why string
	// Failing lists the pod problems behind a trip.
	Failing []workload.Problem
	// HealthySince is the new value for status.healthySince.
	HealthySince *time.Time
	// Acknowledge marks the triggering change as dismissed by a person, so
	// it is never blamed again.
	Acknowledge bool
}

// Decide applies Flagbreaker's rules.
func Decide(in Input) Decision {
	switch in.Phase {
	case v1alpha1.PhaseTripped:
		return decideTripped(in)
	case v1alpha1.PhaseRecovered:
		if in.FlagEnabled {
			return Decision{Action: Rearm, Mode: in.Mode, Why: "the flag is on again, so Flagbreaker is watching it again"}
		}
		return Decision{Mode: in.Mode}
	default:
		return decideArmed(in)
	}
}

// decideArmed trips in two cases. Pods: the flag was just switched on, the
// workload was fine before that, and enough pods failed after it. Alert:
// another Feature Health provider marked the flag unhealthy while it is on.
func decideArmed(in Input) Decision {
	d := Decision{Mode: in.Mode}
	if !in.FlagEnabled {
		return d // nothing is on, so nothing to switch off or blame
	}
	if a := newestNewAlert(in.Alerts, in.HandledEventID); a != nil {
		d.Action, d.Cause, d.Alert = Trip, CauseAlert, a
		d.Why = fmt.Sprintf("%s reported %q in Flagsmith Feature Health at %s UTC", a.Provider, a.Title, a.At.UTC().Format(time.TimeOnly))
		return d
	}

	change := in.LastChange
	switch {
	case change == nil, !change.Enabled:
		return d // nothing switched on
	case in.Now.Sub(change.At.Time) > in.Window:
		return d // the change is too old to blame
	case change.WorkloadWasFailing:
		return d // the breakage came first
	case change.Acknowledged:
		return d // a person dismissed the incident this change caused
	}

	failing := in.Pods.FailingSince(change.At.Time)
	if len(failing) == 0 || len(failing) < in.FailingPods {
		return d
	}

	d.Action, d.Cause, d.Failing = Trip, CausePods, failing
	d.Why = fmt.Sprintf("%d of %d pods started failing %s after %s",
		len(failing), in.Pods.Pods, roundDuration(failing[0].At.Sub(change.At.Time)), describeChange(change))

	if !in.RolloutStartedAt.IsZero() && !in.RolloutStartedAt.Before(change.At.Time.Add(-rolloutSlack)) {
		d.Mode = v1alpha1.ModeWatch
		d.ModeReason = "a rollout started within 30s of the flag change, so the deploy may be the cause"
		d.Why += fmt.Sprintf("; a rollout also started at %s, so the deploy may be the cause and the flag is left on",
			in.RolloutStartedAt.UTC().Format(time.TimeOnly))
	}
	return d
}

// decideTripped retries a failed switch-off, re-arms when a person turns the
// flag back on or dismisses the incident, and otherwise waits for the pods to
// be healthy for StableFor and every other provider's alert to clear.
func decideTripped(in Input) Decision {
	if in.Dismissed {
		return Decision{Action: Rearm, Mode: in.Mode, Acknowledge: true,
			Why: "a person dismissed the incident in Flagsmith, so Flagbreaker is watching the flag again and won't blame this change again"}
	}
	if in.FlagEnabled {
		if in.FlagDisabledAt != nil {
			return Decision{Action: Rearm, Mode: in.Mode, Why: "a person turned the flag back on, so Flagbreaker is watching it again"}
		}
		if in.Mode == v1alpha1.ModeAct {
			return Decision{Action: SwitchOff, Mode: in.Mode, Why: "retrying the switch-off"}
		}
		// Watch mode: the flag stays on until a person acts. If the pods heal
		// anyway, that still counts as recovered.
	}

	// The first healthy sighting only needs every pod ready now. After that,
	// no pod may crash again until StableFor has passed.
	start := in.Now
	if in.HealthySince != nil {
		start = *in.HealthySince
	}
	if !in.Pods.HealthySince(start, in.DesiredReplicas) {
		return Decision{Mode: in.Mode}
	}
	d := Decision{Mode: in.Mode, HealthySince: &start}
	if len(in.Alerts) > 0 {
		// Healthy pods don't overrule another provider: its alert must
		// resolve, or a person must dismiss it, first.
		return d
	}
	if stable := in.Now.Sub(start); stable >= in.StableFor {
		d.Action = Recover
		d.Why = fmt.Sprintf("all %d pods ready with no new crashes for %s", in.Pods.Pods, roundDuration(stable))
	}
	return d
}

func newestNewAlert(alerts []Alert, handled int64) *Alert {
	var newest *Alert
	for i := range alerts {
		a := &alerts[i]
		if a.EventID > handled && (newest == nil || a.EventID > newest.EventID) {
			newest = a
		}
	}
	return newest
}

func describeChange(c *v1alpha1.FlagChange) string {
	s := "the flag was switched on"
	if c.ChangedBy != "" {
		s += " by " + c.ChangedBy
	}
	return s + " at " + c.At.UTC().Format(time.TimeOnly) + " UTC"
}

func roundDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}
