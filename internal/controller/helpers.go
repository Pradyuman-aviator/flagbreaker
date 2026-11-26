package controller

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

// eventTitlePrefix starts the title of every Feature Health event
// Flagbreaker posts, which is how it recognises its own events.
const eventTitlePrefix = "Flagbreaker: "

// withDefaults fills in what the API server's defaults would, so the
// controller also works with objects created before a default existed.
func withDefaults(spec v1alpha1.FlagGuardSpec) v1alpha1.FlagGuardSpec {
	if spec.Window == nil {
		spec.Window = &metav1.Duration{Duration: 10 * time.Minute}
	}
	if spec.StableFor == nil {
		spec.StableFor = &metav1.Duration{Duration: 30 * time.Second}
	}
	if spec.FailingPods < 1 {
		spec.FailingPods = 1
	}
	on := true
	if spec.RestartCrashingPods == nil {
		spec.RestartCrashingPods = &on
	}
	if spec.FollowHealthEvents == nil {
		spec.FollowHealthEvents = &on
	}
	if spec.Workload.Kind == "" {
		spec.Workload.Kind = KindDeployment
	}
	return spec
}

func setCondition(guard *v1alpha1.FlagGuard, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&guard.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: message, ObservedGeneration: guard.Generation,
	})
}

func addTimeline(st *v1alpha1.FlagGuardStatus, at time.Time, event, message string) {
	st.Timeline = append(st.Timeline, v1alpha1.TimelineEntry{Time: metav1.NewTime(at), Event: event, Message: message})
	if n := len(st.Timeline); n > maxTimeline {
		st.Timeline = st.Timeline[n-maxTimeline:]
	}
}

func describeChange(flag, environment string, c *v1alpha1.FlagChange) string {
	onOff := "off"
	if c.Enabled {
		onOff = "on"
	}
	s := fmt.Sprintf("%s was switched %s in %s", flag, onOff, environment)
	if c.ChangedBy != "" {
		s += " by " + c.ChangedBy
	}
	return s
}

// timings measures the incident: from the flag change (or the alert) to the
// trip, from the trip to the switch-off, and from the switch-off to verified
// recovery.
func timings(st *v1alpha1.FlagGuardStatus) string {
	var parts []string
	var startedAt time.Time
	cause := "the change"
	switch {
	case st.Alert != nil:
		startedAt, cause = st.Alert.At.Time, "the alert"
	case st.Trigger != nil:
		startedAt = st.Trigger.At.Time
	}
	if st.TrippedAt != nil && !startedAt.IsZero() {
		parts = append(parts, fmt.Sprintf("detected %s after %s", seconds(st.TrippedAt.Sub(startedAt)), cause))
	}
	if st.FlagDisabledAt != nil && st.TrippedAt != nil {
		by := ""
		if st.FlagDisabledBy != "" && st.FlagDisabledBy != ChangedByFlagbreaker {
			by = " by " + st.FlagDisabledBy
		}
		parts = append(parts, fmt.Sprintf("switched off%s %s after that", by, seconds(st.FlagDisabledAt.Sub(st.TrippedAt.Time))))
	}
	if st.RecoveredAt != nil && st.FlagDisabledAt != nil {
		parts = append(parts, fmt.Sprintf("recovered %s after the switch-off", seconds(st.RecoveredAt.Sub(st.FlagDisabledAt.Time))))
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

func seconds(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// pendingSegments counts the segment overrides Flagbreaker switched off and
// hasn't restored.
func pendingSegments(st *v1alpha1.FlagGuardStatus) int {
	if st.Snapshot == nil {
		return 0
	}
	return len(st.Snapshot.EnabledSegmentOverrides)
}

// restoreHint tells a person how to undo what Flagbreaker switched off. A
// restore undoes only Flagbreaker's own changes.
func restoreHint(guard *v1alpha1.FlagGuard) string {
	st := &guard.Status
	n := pendingSegments(st)
	var what string
	switch {
	case st.Snapshot != nil && st.Snapshot.Enabled && n > 0:
		what = fmt.Sprintf("restore the flag with its %d segment override%s, as it was", n, plural(n))
	case st.Snapshot != nil && st.Snapshot.Enabled:
		what = "restore the flag as it was"
	default:
		what = fmt.Sprintf("turn back on the %d segment override%s Flagbreaker switched off", n, plural(n))
	}
	return fmt.Sprintf("Once the cause is fixed, %s with: kubectl annotate flagguard -n %s %s %s=$(date +%%s) --overwrite",
		what, guard.Namespace, guard.Name, v1alpha1.RestoreAnnotation)
}

// buildHealthEvent turns the incident into a Feature Health event. Flagsmith
// shows the reason's text and URL blocks in the flag's Health tab.
func buildHealthEvent(guard *v1alpha1.FlagGuard, status flagsmith.HealthStatus, a workload.Assessment, links []flagsmith.URLBlock) flagsmith.HealthEvent {
	st := &guard.Status
	event := flagsmith.HealthEvent{Feature: guard.Spec.Flag, Status: status, Environment: st.Environment}
	kind := guard.Spec.Workload.Kind
	if kind == "" {
		kind = KindDeployment
	}
	title := fmt.Sprintf("%s%s %s/%s", eventTitlePrefix, kind, guard.Namespace, guard.Spec.Workload.Name)

	cause := "The workload broke after this flag changed"
	if st.Alert != nil {
		cause = fmt.Sprintf("%s reported %q for this flag", st.Alert.Provider, st.Alert.Title)
	}
	var summary string
	switch {
	case status == flagsmith.Healthy && st.Phase == v1alpha1.PhaseArmed:
		summary = "A person turned the flag back on before Flagbreaker verified recovery, so Flagbreaker is watching it again. " +
			"This is not a verified recovery: if the workload breaks again, the breaker trips again."
		if pendingSegments(st) > 0 {
			summary += " " + restoreHint(guard) + "."
		}
	case status == flagsmith.Healthy:
		summary = "The workload is verified healthy again. " + timings(st)
		if st.Snapshot != nil {
			summary += " " + restoreHint(guard) + ". Switching the flag on in Flagsmith works too; Flagbreaker watches it again as soon as it is on."
		}
	case st.FlagDisabledAt != nil && st.FlagDisabledBy == ChangedByFlagbreaker:
		summary = cause + ", so Flagbreaker switched the flag off."
	case st.FlagDisabledAt != nil:
		summary = cause + ". It was switched off by " + orA(st.FlagDisabledBy) + "; Flagbreaker is waiting for the workload to recover."
	default:
		summary = fmt.Sprintf("%s. Flagbreaker is in watch mode (%s), so the flag is still on.", cause, st.ModeReason)
	}
	event.Reason.TextBlocks = append(event.Reason.TextBlocks, flagsmith.TextBlock{Title: title, Text: strings.TrimSpace(summary)})

	if status == flagsmith.Unhealthy {
		var lines []string
		for i, p := range a.Problems {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("and %d more", len(a.Problems)-5))
				break
			}
			lines = append(lines, fmt.Sprintf("%s: %s", p.Pod, p.Reason))
		}
		if len(lines) > 0 {
			event.Reason.TextBlocks = append(event.Reason.TextBlocks, flagsmith.TextBlock{Title: "Failing pods", Text: strings.Join(lines, "\n")})
		}
	}

	var lines []string
	for _, e := range st.Timeline {
		lines = append(lines, fmt.Sprintf("%s  %s", e.Time.UTC().Format(time.TimeOnly), e.Message))
	}
	if len(lines) > 0 {
		event.Reason.TextBlocks = append(event.Reason.TextBlocks, flagsmith.TextBlock{Title: "Timeline (UTC)", Text: strings.Join(lines, "\n")})
	}
	event.Reason.URLBlocks = links
	return event
}

func orA(by string) string {
	if by == "" {
		return "a person"
	}
	return by
}

// union returns the IDs in a or b, sorted, without duplicates.
func union(a, b []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, list := range [][]int64{a, b} {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// wasFailing reports whether the workload was already failing at t: a crash
// or a not-ready spell from `since` on, or a pod still crash-looping (or not
// ready) from a problem that began before t. Restarts in a crash loop can be
// minutes apart, so the last crash alone can look older than it is.
func wasFailing(a workload.Assessment, t, since time.Time) bool {
	return len(a.FailingBetween(since, t)) > 0 || len(a.BrokenBefore(t)) > 0
}

// failingFrom is where the look-back for "was the workload already failing
// at t?" starts: lookback before t, but never before the last verified
// recovery. Kubernetes keeps a pod's last crash in its status, and crashes
// from before a verified recovery belong to that incident, not to now.
func failingFrom(st *v1alpha1.FlagGuardStatus, spec v1alpha1.FlagGuardSpec, t time.Time) time.Time {
	from := t.Add(-lookback(spec))
	if st.RecoveredAt != nil && spec.StableFor != nil {
		if healthy := st.RecoveredAt.Add(-spec.StableFor.Duration); healthy.After(from) && healthy.Before(t) {
			from = healthy
		}
	}
	return from
}

// lookback is how far before a change Flagbreaker looks to decide whether
// the workload was failing already: Kubernetes keeps a container's last
// crash long after it recovers, so not far.
func lookback(spec v1alpha1.FlagGuardSpec) time.Duration {
	if spec.StableFor != nil && spec.StableFor.Duration > time.Minute {
		return spec.StableFor.Duration
	}
	return time.Minute
}

func contains(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ", ")
}

func itThey(n int) string {
	if n == 1 {
		return "it"
	}
	return "they"
}

func sIfOne(n int) string {
	if n == 1 {
		return "s"
	}
	return ""
}

func labelSet(m map[string]string) labels.Set { return labels.Set(m) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func timePtr(t *metav1.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.Time
	return &v
}

func metaTimePtr(t *time.Time) *metav1.Time {
	if t == nil {
		return nil
	}
	v := metav1.NewTime(*t)
	return &v
}

func metaNow(t time.Time) *metav1.Time {
	v := metav1.NewTime(t)
	return &v
}
