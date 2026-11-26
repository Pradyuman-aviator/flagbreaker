// Package controller runs the FlagGuard loop: read the flag and its Feature
// Health from Flagsmith, read the workload's pods from Kubernetes, ask the
// breaker what to do, do it, and record what happened in the FlagGuard's
// status and in Feature Health.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/breaker"
	"github.com/Pradyuman-aviator/flagbreaker/internal/changes"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/metrics"
	"github.com/Pradyuman-aviator/flagbreaker/internal/settings"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

const (
	// ChangedByFlagbreaker marks changes Flagbreaker made itself.
	ChangedByFlagbreaker = "flagbreaker"
	// ChangedByRestore marks the switch-on of a restore.
	ChangedByRestore = "flagbreaker (restore)"

	conditionReady         = "Ready"
	conditionReported      = "Reported"
	conditionBlocked       = "BlockedByChangeRequests"
	conditionPaused        = "Paused"
	conditionSettings      = "SettingsValid"
	conditionFeatureHealth = "FeatureHealthRead"

	maxTimeline   = 20
	notReadyGrace = 20 * time.Second
)

// +kubebuilder:rbac:groups=flagbreaker.dev,resources=flagguards,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=flagbreaker.dev,resources=flagguards/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets;daemonsets;controllerrevisions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// FlagGuardReconciler reconciles FlagGuards.
type FlagGuardReconciler struct {
	client.Client
	// APIReader reads guards past the cache, so a check never starts from a
	// status older than the last one written. Optional.
	APIReader  client.Reader
	Recorder   recorder.EventRecorder
	Flagsmith  *flagsmith.Client
	Settings   *settings.Reader
	Changes    *changes.Store
	SyncPeriod time.Duration
	Now        func() time.Time
	// WorkloadLink, when set, renders a link to the workload (for example in
	// Headlamp or Grafana) for every Feature Health event. It gets
	// .Namespace, .Kind, .Name and .Guard.
	WorkloadLink *template.Template

	mu        sync.Mutex
	lastCheck map[types.NamespacedName]time.Time
	// Webhook-triggered checks arrive on this channel.
	triggers chan event.GenericEvent
	events   eventsCache
}

// Reconcile checks one FlagGuard. It runs on every change to the guard or its
// pods, on every Flagsmith webhook for its flag, and every SyncPeriod.
func (r *FlagGuardReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var guard v1alpha1.FlagGuard
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	if err := reader.Get(ctx, req.NamespacedName, &guard); err != nil {
		if apierrors.IsNotFound(err) {
			r.forget(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	before := guard.Status.DeepCopy()

	if err := r.check(ctx, &guard, r.now()); err != nil {
		logf.FromContext(ctx).Error(err, "check failed")
		metrics.Errors.WithLabelValues(guard.Namespace, guard.Name).Inc()
		setCondition(&guard, conditionReady, metav1.ConditionFalse, "CheckFailed", err.Error())
	} else {
		setCondition(&guard, conditionReady, metav1.ConditionTrue, "Watching", "Flagsmith and the workload were read successfully")
	}
	guard.Status.ObservedGeneration = guard.Generation
	metrics.SetPhase(guard.Namespace, guard.Name, guard.Spec.Flag, string(guard.Status.Phase))

	if !equality.Semantic.DeepEqual(before, &guard.Status) {
		if err := r.Status().Update(ctx, &guard); apierrors.IsConflict(err) {
			// Someone else wrote the guard since it was read. Check again
			// from the fresh object; every action is idempotent in Flagsmith.
			return ctrl.Result{RequeueAfter: time.Second}, nil
		} else if err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.SyncPeriod}, nil
}

func (r *FlagGuardReconciler) check(ctx context.Context, guard *v1alpha1.FlagGuard, now time.Time) error {
	spec := withDefaults(guard.Spec)
	st := &guard.Status
	if st.Phase == "" {
		st.Phase = v1alpha1.PhaseArmed
	}

	// 1. Find the flag in Flagsmith, once per spec change.
	if st.Environment == "" || st.FeatureID == 0 || st.EnvironmentID == 0 || st.ProjectID == 0 || st.ObservedGeneration != guard.Generation {
		env, err := r.Flagsmith.Environment(ctx, spec.EnvironmentKey)
		if err != nil {
			return fmt.Errorf("looking up the Flagsmith environment: %w", err)
		}
		id, err := r.Flagsmith.FeatureID(ctx, env.ProjectID, spec.Flag)
		if err != nil {
			return fmt.Errorf("looking up flag %q: %w", spec.Flag, err)
		}
		if (st.FeatureID != 0 && st.FeatureID != id) || (st.EnvironmentID != 0 && st.EnvironmentID != env.ID) {
			resetForNewFlag(st, spec, env.Name, now)
		}
		st.Environment, st.EnvironmentID, st.ProjectID, st.FeatureID = env.Name, env.ID, env.ProjectID, id
	}
	flag, err := r.Flagsmith.GetFlag(ctx, spec.EnvironmentKey, st.FeatureID)
	if err != nil {
		return fmt.Errorf("reading flag %q: %w", spec.Flag, err)
	}
	enabled := flag.EnvironmentDefault.Enabled

	// 2. Look at the workload.
	t, err := r.getTarget(ctx, guard.Namespace, spec.Workload)
	if err != nil {
		return err
	}
	pods, rolloutAt, err := r.podsAndRollout(ctx, t)
	if err != nil {
		return err
	}
	assessment := workload.Assess(pods, now, notReadyGrace)

	// 3. Notice flag changes made by people. A switch-off during an
	// incident is what watch mode asks a person to do.
	var personOff *v1alpha1.FlagChange
	if change := r.trackFlag(guard, enabled, assessment, now); change != nil &&
		!change.Enabled && st.Phase == v1alpha1.PhaseTripped && st.FlagDisabledAt == nil {
		personOff = change
	}

	// 4. Settle the mode. Flagsmith's own flags can pause the breaker or
	// keep it to watching.
	s := r.Settings.Get(ctx)
	if s.Problem != "" {
		setCondition(guard, conditionSettings, metav1.ConditionFalse, "InvalidSetting", s.Problem)
	} else {
		meta.RemoveStatusCondition(&st.Conditions, conditionSettings)
	}
	res := s.Resolve(spec.Flag, spec.Mode)
	if res.Paused {
		if personOff != nil {
			r.personSwitchedOff(ctx, guard, spec, personOff, assessment, false, now)
		}
		st.ModeReason = res.Reason
		setCondition(guard, conditionPaused, metav1.ConditionTrue, "PausedInFlagsmith", res.Reason+", so Flagbreaker changes nothing")
		return nil
	}
	meta.RemoveStatusCondition(&st.Conditions, conditionPaused)
	if personOff != nil {
		r.personSwitchedOff(ctx, guard, spec, personOff, assessment, true, now)
	}
	mode := res.Mode
	switch {
	case st.Phase != v1alpha1.PhaseTripped || st.Mode == "":
		st.Mode, st.ModeReason = mode, res.Reason
	case mode == v1alpha1.ModeWatch && st.Mode == v1alpha1.ModeAct:
		// A switch to watch in Flagsmith stops an incident from acting
		// further. The reverse never happens mid-incident.
		st.Mode, st.ModeReason = mode, res.Reason
	default:
		mode = st.Mode // finish the incident in the mode it started in
	}

	// 5. A person asked for the flag to be restored.
	if handled, err := r.handleRestore(ctx, guard, spec, flag, now); handled || err != nil {
		return err
	}

	// 6. What Feature Health says: other providers' alerts, and whether a
	// person dismissed Flagbreaker's own Unhealthy event.
	alerts, dismissed := r.healthSignals(ctx, guard, spec, now)

	// A report that failed last time is sent again.
	if c := meta.FindStatusCondition(st.Conditions, conditionReported); c != nil && c.Status == metav1.ConditionFalse {
		r.reportPhase(ctx, guard, spec, assessment)
	}

	// 7. Decide, then act.
	in := breaker.Input{
		Now: now, Phase: st.Phase, Mode: mode,
		FlagEnabled: enabled, LastChange: st.LastFlagChange,
		Pods: assessment, DesiredReplicas: t.desired, RolloutStartedAt: rolloutAt,
		Alerts: alerts, HandledEventID: st.HandledEventID, Dismissed: dismissed,
		Window: spec.Window.Duration, StableFor: spec.StableFor.Duration, FailingPods: int(spec.FailingPods),
		FlagDisabledAt: timePtr(st.FlagDisabledAt), HealthySince: timePtr(st.HealthySince),
	}
	d := breaker.Decide(in)
	st.HealthySince = metaTimePtr(d.HealthySince)

	switch d.Action {
	case breaker.Trip:
		return r.trip(ctx, guard, spec, t, flag, assessment, alerts, d, now)
	case breaker.SwitchOff:
		return r.switchOff(ctx, guard, spec, flag, assessment, now)
	case breaker.Recover:
		return r.recover(ctx, guard, spec, assessment, d, now)
	case breaker.Rearm:
		r.rearm(guard, spec, d, now)
	}
	return nil
}

// resetForNewFlag starts over when a guard's spec now points at another flag
// or environment: nothing about the old one applies.
func resetForNewFlag(st *v1alpha1.FlagGuardStatus, spec v1alpha1.FlagGuardSpec, environment string, now time.Time) {
	*st = v1alpha1.FlagGuardStatus{Phase: v1alpha1.PhaseArmed, Conditions: st.Conditions}
	addTimeline(st, now, "GuardChanged", fmt.Sprintf("Now guarding %s in %s; the previous flag's state was dropped", spec.Flag, environment))
}

// trackFlag records a change to the flag's environment default and returns
// it, or nil when nothing changed. The first time a guard looks, it only
// remembers the state: a flag that was already on is never blamed.
func (r *FlagGuardReconciler) trackFlag(guard *v1alpha1.FlagGuard, enabled bool, a workload.Assessment, now time.Time) *v1alpha1.FlagChange {
	st := &guard.Status
	key := client.ObjectKeyFromObject(guard)

	r.mu.Lock()
	prev, seen := r.lastCheck[key]
	r.lastCheck[key] = now
	r.mu.Unlock()

	if st.FlagEnabled == nil {
		st.FlagEnabled = &enabled
		return nil
	}
	if *st.FlagEnabled == enabled {
		r.enrich(guard)
		return nil
	}
	st.FlagEnabled = &enabled

	// The change happened after the previous check. If the webhook is
	// already here, it says exactly when, and who made it.
	at := now.Add(-r.SyncPeriod)
	if seen && prev.After(at) {
		at = prev
	}
	change := &v1alpha1.FlagChange{At: metav1.NewTime(at), Enabled: enabled}
	if c, ok := r.Changes.Latest(st.Environment, guard.Spec.Flag, enabled); ok && matches(c, at, now, r.SyncPeriod) {
		change.At, change.ChangedBy = metav1.NewTime(c.At), c.ChangedBy
	}
	// Was it failing just before? Old crashes stay in pod status long after
	// a pod recovers, so look back only a little.
	lookback := withDefaults(guard.Spec).StableFor.Duration
	if lookback < time.Minute {
		lookback = time.Minute
	}
	change.WorkloadWasFailing = enabled && len(a.FailingBetween(change.At.Add(-lookback), change.At.Time)) > 0
	st.LastFlagChange = change
	return change
}

// enrich adds a webhook's exact time and author to a change that polling saw
// first. Flagsmith sends FLAG_UPDATED a few seconds after the change, from a
// background task, so polling often wins the race.
func (r *FlagGuardReconciler) enrich(guard *v1alpha1.FlagGuard) {
	st := &guard.Status
	for _, c := range []*v1alpha1.FlagChange{st.LastFlagChange, st.Trigger} {
		if c == nil || c.ChangedBy != "" {
			continue
		}
		w, ok := r.Changes.Latest(st.Environment, guard.Spec.Flag, c.Enabled)
		if !ok || w.ChangedBy == "" || !matches(w, c.At.Time, c.At.Time.Add(r.SyncPeriod), r.SyncPeriod) {
			continue
		}
		c.At, c.ChangedBy = metav1.NewTime(w.At), w.ChangedBy
	}
	if st.Trigger != nil && len(st.Timeline) > 0 && st.Timeline[0].Event == "FlagChanged" {
		st.Timeline[0].Time = st.Trigger.At
		st.Timeline[0].Message = describeChange(guard.Spec.Flag, st.Environment, st.Trigger)
	}
	// A person's switch-off during an incident gets its author too.
	if c := st.LastFlagChange; st.FlagDisabledAt != nil && st.FlagDisabledBy == "" && c != nil && !c.Enabled && c.ChangedBy != "" {
		st.FlagDisabledBy = c.ChangedBy
		for i := range st.Timeline {
			if st.Timeline[i].Event == "FlagSwitchedOffByPerson" {
				st.Timeline[i].Message = personSwitchOffMessage(guard.Spec.Flag, st.Environment, c.ChangedBy)
			}
		}
	}
}

// matches reports whether a webhook change could be the change polling saw
// between `after` and `before`, allowing for clock skew between the hosts.
func matches(c changes.Change, after, before time.Time, slack time.Duration) bool {
	return !c.At.Before(after.Add(-slack)) && !c.At.After(before.Add(slack))
}

func (r *FlagGuardReconciler) trip(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, t target,
	flag flagsmith.Flag, a workload.Assessment, alerts []breaker.Alert, d breaker.Decision, now time.Time) error {
	st := &guard.Status
	st.Phase = v1alpha1.PhaseTripped
	st.Mode = d.Mode
	if d.ModeReason != "" {
		st.ModeReason = d.ModeReason
	}
	st.TrippedAt = metaNow(now)
	st.FlagDisabledAt, st.FlagDisabledBy, st.RecoveredAt, st.HealthySince = nil, "", nil, nil
	// Alerts that are active now belong to this incident; none of them may
	// trip the guard again once it is re-armed.
	for _, al := range alerts {
		if al.EventID > st.HandledEventID {
			st.HandledEventID = al.EventID
		}
	}

	// Each incident gets a fresh timeline, starting with what caused it.
	st.Timeline = nil
	if d.Cause == breaker.CauseAlert && d.Alert != nil {
		st.Trigger = nil
		st.Alert = &v1alpha1.HealthAlert{EventID: d.Alert.EventID, Provider: d.Alert.Provider, Title: d.Alert.Title, At: metav1.NewTime(d.Alert.At)}
		addTimeline(st, d.Alert.At, "AlertFired", fmt.Sprintf("%s reported %q for %s in %s", d.Alert.Provider, d.Alert.Title, spec.Flag, st.Environment))
		metrics.Observe(metrics.DetectionSeconds, now.Sub(d.Alert.At), spec.Flag, st.Environment)
	} else {
		st.Alert = nil
		st.Trigger = st.LastFlagChange.DeepCopy()
		if c := st.Trigger; c != nil {
			addTimeline(st, c.At.Time, "FlagChanged", describeChange(spec.Flag, st.Environment, c))
			metrics.Observe(metrics.DetectionSeconds, now.Sub(c.At.Time), spec.Flag, st.Environment)
		}
	}
	if d.Cause == breaker.CauseAlert {
		addTimeline(st, now, "Tripped", fmt.Sprintf("Tripped for %s %s/%s: %s", t.kind, guard.Namespace, t.obj.GetName(), d.Why))
	} else {
		addTimeline(st, now, "Tripped", fmt.Sprintf("%s %s/%s: %s", t.kind, guard.Namespace, t.obj.GetName(), d.Why))
	}
	r.Recorder.Eventf(guard, t.obj, corev1.EventTypeWarning, "Tripped", "Trip", "%s", d.Why)
	metrics.Trips.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, string(d.Mode), string(d.Cause)).Inc()

	var err error
	if d.Mode == v1alpha1.ModeAct {
		err = r.switchOff(ctx, guard, spec, flag, a, now)
	} else if d.ModeReason == "" {
		addTimeline(st, now, "WatchOnly", fmt.Sprintf("Watch mode (%s): the flag was left on. Switch %s off in Flagsmith to recover.", st.ModeReason, spec.Flag))
	}
	r.report(ctx, guard, spec, flagsmith.Unhealthy, a)
	return err
}

// switchOff turns off the flag's environment default and its segment
// overrides that are on, then restarts crash-looping pods.
func (r *FlagGuardReconciler) switchOff(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	flag flagsmith.Flag, a workload.Assessment, now time.Time) error {
	st := &guard.Status
	segments := flag.EnabledSegmentIDs()

	_, err := r.Flagsmith.SwitchOff(ctx, spec.EnvironmentKey, st.FeatureID, segments)
	if err != nil && !errors.Is(err, flagsmith.ErrChangeRequestsEnabled) && len(segments) > 0 {
		// An experiment's override can't be changed through this API. Switch
		// off the environment default at least, and say what was left on.
		if _, retryErr := r.Flagsmith.SwitchOff(ctx, spec.EnvironmentKey, st.FeatureID, nil); retryErr == nil {
			addTimeline(st, now, "SegmentsLeftOn", fmt.Sprintf("Couldn't switch off the segment overrides (%v); they still serve the flag", err))
			segments, err = nil, nil
		}
	}
	switch {
	case errors.Is(err, flagsmith.ErrChangeRequestsEnabled):
		st.Mode = v1alpha1.ModeWatch
		st.ModeReason = st.Environment + " requires change requests"
		setCondition(guard, conditionBlocked, metav1.ConditionTrue, "ChangeRequestsEnabled",
			fmt.Sprintf("%s requires change requests, so Flagbreaker won't change flags there", st.Environment))
		addTimeline(st, now, "BlockedByChangeRequests",
			fmt.Sprintf("%s requires change requests, so Flagbreaker left the flag on. Approve a change request to switch it off.", st.Environment))
		return nil
	case err != nil:
		return fmt.Errorf("switching %q off: %w", spec.Flag, err)
	}

	off := false
	st.FlagEnabled = &off
	st.FlagDisabledAt, st.FlagDisabledBy = metaNow(now), ChangedByFlagbreaker
	// Overrides still off from an earlier incident stay in the snapshot, so a
	// restore turns them back on too.
	var pending []int64
	if st.Snapshot != nil {
		pending = st.Snapshot.EnabledSegmentOverrides
	}
	st.Snapshot = &v1alpha1.FlagSnapshot{Enabled: flag.EnvironmentDefault.Enabled, EnabledSegmentOverrides: union(pending, segments)}
	st.LastFlagChange = &v1alpha1.FlagChange{At: metav1.NewTime(now), Enabled: false, ChangedBy: ChangedByFlagbreaker}
	what := "its environment default"
	if n := len(segments); n > 0 {
		what += fmt.Sprintf(" and %d segment override%s", n, plural(n))
	}
	addTimeline(st, now, "FlagSwitchedOff",
		fmt.Sprintf("Switched %s off in %s (%s). Identity overrides and experiments are not changed.", spec.Flag, st.Environment, what))
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "FlagSwitchedOff", "SwitchOff", "Switched %s off in %s", spec.Flag, st.Environment)
	metrics.SwitchOffs.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "flagbreaker").Inc()
	if st.TrippedAt != nil {
		metrics.Observe(metrics.SwitchOffSeconds, now.Sub(st.TrippedAt.Time), spec.Flag, st.Environment)
	}
	r.restartCrashed(ctx, guard, spec, a, now)
	return nil
}

// personSwitchedOff records that a person switched the flag off during an
// incident, as watch mode asks them to, and restarts crashed pods.
func (r *FlagGuardReconciler) personSwitchedOff(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	change *v1alpha1.FlagChange, a workload.Assessment, restart bool, now time.Time) {
	st := &guard.Status
	at := change.At
	st.FlagDisabledAt, st.FlagDisabledBy = &at, change.ChangedBy
	addTimeline(st, at.Time, "FlagSwitchedOffByPerson", personSwitchOffMessage(spec.Flag, st.Environment, change.ChangedBy))
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "FlagSwitchedOff", "SwitchOff", "%s was switched off in %s by a person", spec.Flag, st.Environment)
	metrics.SwitchOffs.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "person").Inc()
	if st.TrippedAt != nil {
		metrics.Observe(metrics.SwitchOffSeconds, at.Sub(st.TrippedAt.Time), spec.Flag, st.Environment)
	}
	if restart {
		r.restartCrashed(ctx, guard, spec, a, now)
	}
}

func personSwitchOffMessage(flag, environment, by string) string {
	if by == "" {
		by = "a person"
	}
	return fmt.Sprintf("%s was switched off in %s by %s", flag, environment, by)
}

// restartCrashed deletes the pods that crashed after the triggering change,
// plus any stuck in CrashLoopBackOff, so they read the flag again now.
func (r *FlagGuardReconciler) restartCrashed(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, a workload.Assessment, now time.Time) {
	if !*spec.RestartCrashingPods {
		return
	}
	crashed := crashedPods(a, guard.Status.Trigger)
	if len(crashed) == 0 {
		return
	}
	if restarted := r.restartPods(ctx, guard.Namespace, crashed); restarted > 0 {
		addTimeline(&guard.Status, now, "PodsRestarted",
			fmt.Sprintf("Restarted %d crashed pod%s, so the workload reads the flag again now instead of after the kubelet's back-off", restarted, plural(restarted)))
	}
}

// crashedPods lists the pods that crashed after the triggering change, plus
// any stuck in CrashLoopBackOff.
func crashedPods(a workload.Assessment, trigger *v1alpha1.FlagChange) []string {
	seen := map[string]bool{}
	var names []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	for _, name := range a.CrashLooping {
		add(name)
	}
	if trigger != nil {
		for _, p := range a.FailingSince(trigger.At.Time) {
			add(p.Pod)
		}
	}
	return names
}

func (r *FlagGuardReconciler) recover(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, a workload.Assessment, d breaker.Decision, now time.Time) error {
	st := &guard.Status
	st.Phase = v1alpha1.PhaseRecovered
	st.RecoveredAt = metaNow(now)
	addTimeline(st, now, "Recovered", strings.TrimSpace("Verified recovery: "+d.Why+". "+timings(st)))
	if st.Snapshot != nil && (st.Snapshot.Enabled || pendingSegments(st) > 0) {
		addTimeline(st, now, "RestoreAvailable", restoreHint(guard))
	}
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "Recovered", "Recover", "%s", d.Why)
	metrics.Recoveries.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment).Inc()
	if st.FlagDisabledAt != nil {
		metrics.Observe(metrics.RecoverySeconds, now.Sub(st.FlagDisabledAt.Time), spec.Flag, st.Environment)
	}

	// Feature Health keeps one status per flag and environment, so Healthy
	// waits until every guard of this flag has recovered.
	others, err := r.otherTrippedGuards(ctx, guard)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		addTimeline(st, now, "HealthyNotReported",
			fmt.Sprintf("Not reporting Healthy yet: %s still tripped for this flag", strings.Join(others, ", ")))
		return nil
	}
	r.report(ctx, guard, spec, flagsmith.Healthy, a)
	return nil
}

func (r *FlagGuardReconciler) rearm(guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, d breaker.Decision, now time.Time) {
	st := &guard.Status
	if d.Acknowledge && st.LastFlagChange != nil {
		st.LastFlagChange.Acknowledged = true
	}
	st.Phase = v1alpha1.PhaseArmed
	st.TrippedAt, st.FlagDisabledAt, st.FlagDisabledBy, st.HealthySince, st.RecoveredAt = nil, nil, "", nil, nil
	st.Trigger, st.Alert = nil, nil
	meta.RemoveStatusCondition(&st.Conditions, conditionBlocked)
	addTimeline(st, now, "Rearmed", strings.ToUpper(d.Why[:1])+d.Why[1:])
	// A person turned the environment default on by hand. Overrides that
	// Flagbreaker switched off stay off until someone restores them.
	if st.Snapshot != nil && st.FlagEnabled != nil && *st.FlagEnabled {
		st.Snapshot.Enabled = false
		if n := pendingSegments(st); n == 0 {
			st.Snapshot = nil
		} else {
			addTimeline(st, now, "SegmentsStillOff", fmt.Sprintf("%d segment override%s that Flagbreaker switched off %s still off. %s",
				n, plural(n), isAre(n), restoreHint(guard)))
		}
	}
}

// handleRestore applies a new flagbreaker.dev/restore annotation. It turns
// back on what Flagbreaker switched off, but only overrides that still exist
// and are still off: a person's later change always wins.
func (r *FlagGuardReconciler) handleRestore(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, flag flagsmith.Flag, now time.Time) (bool, error) {
	st := &guard.Status
	want := guard.Annotations[v1alpha1.RestoreAnnotation]
	if want == "" || want == st.RestoreRequest {
		return false, nil
	}
	previous := st.RestoreRequest
	st.RestoreRequest = want
	refuse := func(why string) (bool, error) {
		addTimeline(st, now, "RestoreRefused", "Not restored: "+why)
		r.Recorder.Eventf(guard, nil, corev1.EventTypeWarning, "RestoreRefused", "Restore", "%s", why)
		return true, nil
	}
	switch {
	case st.Phase == v1alpha1.PhaseTripped:
		return refuse("the guard is still tripped. Flagbreaker restores a flag only after verified recovery.")
	case st.Snapshot == nil:
		return refuse("Flagbreaker has nothing to restore; it hasn't switched this flag off.")
	}

	existing := map[int64]bool{}
	for _, o := range flag.SegmentOverrides {
		existing[o.Segment.ID] = o.Enabled
	}
	var segments []int64
	for _, id := range st.Snapshot.EnabledSegmentOverrides {
		if on, ok := existing[id]; ok && !on {
			segments = append(segments, id)
		}
	}
	turnOn := st.Snapshot.Enabled && !flag.EnvironmentDefault.Enabled
	if !turnOn && len(segments) == 0 {
		st.Snapshot = nil
		addTimeline(st, now, "Restored", "Nothing left to restore: the flag is already serving what it served before the incident, or a person changed it since.")
		return true, nil
	}

	update := flagsmith.Update{Segments: segments, SegmentsEnabled: true}
	if turnOn {
		on := true
		update.Default = &on
	}
	_, err := r.Flagsmith.UpdateFlag(ctx, spec.EnvironmentKey, st.FeatureID, update)
	switch {
	case errors.Is(err, flagsmith.ErrChangeRequestsEnabled):
		return refuse(st.Environment + " requires change requests, so Flagbreaker won't change the flag there. Approve a change request instead.")
	case err != nil:
		st.RestoreRequest = previous // try again next check
		return true, fmt.Errorf("restoring %q: %w", spec.Flag, err)
	}

	var parts []string
	if turnOn {
		parts = append(parts, "its environment default")
	}
	if n := len(segments); n > 0 {
		parts = append(parts, fmt.Sprintf("%d segment override%s", n, plural(n)))
	}
	st.Snapshot = nil
	if turnOn {
		on := true
		st.FlagEnabled = &on
		st.Phase = v1alpha1.PhaseArmed
		st.TrippedAt, st.FlagDisabledAt, st.FlagDisabledBy, st.HealthySince, st.RecoveredAt = nil, nil, "", nil, nil
		st.Trigger, st.Alert = nil, nil
		meta.RemoveStatusCondition(&st.Conditions, conditionBlocked)
		// The restore is a flag change like any other: if the workload
		// breaks again, the breaker trips again.
		st.LastFlagChange = &v1alpha1.FlagChange{At: metav1.NewTime(now), Enabled: true, ChangedBy: ChangedByRestore}
	}
	msg := fmt.Sprintf("Restored %s in %s: turned %s back on, as the %s annotation asked. Flagbreaker is watching it; a new breakage trips it again.",
		spec.Flag, st.Environment, strings.Join(parts, " and "), v1alpha1.RestoreAnnotation)
	addTimeline(st, now, "Restored", msg)
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "Restored", "Restore", "%s", msg)
	metrics.Restores.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment).Inc()
	return true, nil
}

// healthSignals reads the project's Feature Health events. It returns the
// Unhealthy events other providers report for this flag in this environment
// (or for every environment), and whether a person dismissed Flagbreaker's
// Unhealthy event for the current incident.
func (r *FlagGuardReconciler) healthSignals(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, now time.Time) ([]breaker.Alert, bool) {
	st := &guard.Status
	events, err := r.events.get(ctx, r.Flagsmith, st.ProjectID, r.SyncPeriod/2, now)
	if err != nil {
		setCondition(guard, conditionFeatureHealth, metav1.ConditionFalse, "ReadFailed", err.Error())
		return nil, false
	}
	meta.RemoveStatusCondition(&st.Conditions, conditionFeatureHealth)

	var alerts []breaker.Alert
	dismissed := false
	for _, e := range events {
		if e.Feature != st.FeatureID || (e.Environment != nil && *e.Environment != st.EnvironmentID) {
			continue
		}
		switch {
		case isOwnEvent(e):
			continue
		case e.ProviderName == flagsmith.ProviderWebhook && !e.Unhealthy() && strings.HasPrefix(e.FirstText(), flagsmith.DismissedPrefix):
			// Dismissing Flagbreaker's event writes a Healthy event into
			// the Webhook provider's slot.
			if st.Phase == v1alpha1.PhaseTripped && st.TrippedAt != nil && e.CreatedAt.After(st.TrippedAt.Time) {
				dismissed = true
			}
		case e.Unhealthy() && *spec.FollowHealthEvents:
			alerts = append(alerts, breaker.Alert{EventID: e.ID, Provider: e.ProviderName, Title: e.Title(), At: e.CreatedAt})
		}
	}
	sort.Slice(alerts, func(i, j int) bool { return alerts[i].EventID < alerts[j].EventID })
	return alerts, dismissed
}

// isOwnEvent recognises events Flagbreaker posted, by the title of their
// first text block.
func isOwnEvent(e flagsmith.HealthEventRecord) bool {
	return e.ProviderName == flagsmith.ProviderWebhook && strings.HasPrefix(e.Title(), eventTitlePrefix)
}

// reportPhase re-sends the report that matches the current phase.
func (r *FlagGuardReconciler) reportPhase(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, a workload.Assessment) {
	switch guard.Status.Phase {
	case v1alpha1.PhaseTripped:
		r.report(ctx, guard, spec, flagsmith.Unhealthy, a)
	case v1alpha1.PhaseRecovered:
		r.report(ctx, guard, spec, flagsmith.Healthy, a)
	default:
		meta.RemoveStatusCondition(&guard.Status.Conditions, conditionReported)
	}
}

// report posts the incident to Flagsmith Feature Health, where it shows in
// the flag's Health tab.
func (r *FlagGuardReconciler) report(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, status flagsmith.HealthStatus, a workload.Assessment) {
	event := buildHealthEvent(guard, status, a, r.links(guard, spec))
	if err := r.Flagsmith.ReportHealth(ctx, event); err != nil {
		logf.FromContext(ctx).Error(err, "reporting to Feature Health failed; will retry")
		setCondition(guard, conditionReported, metav1.ConditionFalse, "ReportFailed", err.Error())
		return
	}
	r.events.invalidate(guard.Status.ProjectID)
	setCondition(guard, conditionReported, metav1.ConditionTrue, "Reported", fmt.Sprintf("Reported %s to Feature Health", status))
}

// links are the spec's links plus the rendered workload link.
func (r *FlagGuardReconciler) links(guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec) []flagsmith.URLBlock {
	var out []flagsmith.URLBlock
	for _, l := range spec.Links {
		out = append(out, flagsmith.URLBlock{Title: l.Title, URL: l.URL})
	}
	if r.WorkloadLink != nil {
		var b strings.Builder
		data := map[string]string{"Namespace": guard.Namespace, "Kind": kindOf(spec.Workload), "Name": spec.Workload.Name, "Guard": guard.Name}
		if err := r.WorkloadLink.Execute(&b, data); err == nil && b.Len() > 0 {
			out = append(out, flagsmith.URLBlock{Title: "Workload", URL: b.String()})
		}
	}
	return out
}

func (r *FlagGuardReconciler) restartPods(ctx context.Context, namespace string, names []string) int {
	n := 0
	for _, name := range names {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			logf.FromContext(ctx).Error(err, "restarting pod", "pod", name)
			continue
		}
		n++
	}
	return n
}

func (r *FlagGuardReconciler) otherTrippedGuards(ctx context.Context, guard *v1alpha1.FlagGuard) ([]string, error) {
	var all v1alpha1.FlagGuardList
	if err := r.List(ctx, &all); err != nil {
		return nil, err
	}
	var names []string
	for _, g := range all.Items {
		if g.UID != guard.UID && g.Status.FeatureID == guard.Status.FeatureID && g.Status.EnvironmentID == guard.Status.EnvironmentID &&
			g.Status.Phase == v1alpha1.PhaseTripped {
			names = append(names, g.Namespace+"/"+g.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// SetupWithManager starts the controller. It checks a guard when the guard's
// spec or annotations change, when its pods change, when Flagsmith sends a
// webhook for its flag, and every SyncPeriod.
func (r *FlagGuardReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Now == nil {
		r.Now = time.Now
	}
	r.lastCheck = map[types.NamespacedName]time.Time{}
	r.triggers = make(chan event.GenericEvent, 64)

	return ctrl.NewControllerManagedBy(mgr).
		Named("flagguard").
		For(&v1alpha1.FlagGuard{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.guardsForPod)).
		WatchesRawSource(source.Channel(r.triggers, &handler.EnqueueRequestForObject{})).
		Complete(r)
}

// FlagChanged is called by the webhook receiver: it queues a check of every
// guard of that flag in that environment.
func (r *FlagGuardReconciler) FlagChanged(ctx context.Context, environment, feature string) {
	var all v1alpha1.FlagGuardList
	if err := r.List(ctx, &all); err != nil {
		return
	}
	logf.FromContext(ctx).Info("Flagsmith webhook: flag changed", "environment", environment, "flag", feature)
	for i := range all.Items {
		g := &all.Items[i]
		if g.Spec.Flag == feature && g.Status.Environment == environment {
			select {
			case r.triggers <- event.GenericEvent{Object: g}:
			default: // the queue is full; polling will catch up
			}
		}
	}
}

// guardsForPod maps a pod to the guards of the workload it belongs to.
func (r *FlagGuardReconciler) guardsForPod(ctx context.Context, obj client.Object) []reconcile.Request {
	var guards v1alpha1.FlagGuardList
	if err := r.List(ctx, &guards, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range guards.Items {
		g := &guards.Items[i]
		t, err := r.getTarget(ctx, g.Namespace, g.Spec.Workload)
		if err == nil && t.selector.Matches(labelSet(obj.GetLabels())) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(g)})
		}
	}
	return reqs
}

func (r *FlagGuardReconciler) forget(key types.NamespacedName) {
	r.mu.Lock()
	delete(r.lastCheck, key)
	r.mu.Unlock()
	metrics.Forget(key.Namespace, key.Name)
}

func (r *FlagGuardReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// eventsCache keeps each project's Feature Health events for a moment, so
// many guards in one project share one request.
type eventsCache struct {
	mu      sync.Mutex
	fetched map[int64]time.Time
	events  map[int64][]flagsmith.HealthEventRecord
}

func (c *eventsCache) get(ctx context.Context, fs *flagsmith.Client, project int64, ttl time.Duration, now time.Time) ([]flagsmith.HealthEventRecord, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetched == nil {
		c.fetched, c.events = map[int64]time.Time{}, map[int64][]flagsmith.HealthEventRecord{}
	}
	if at, ok := c.fetched[project]; ok && now.Sub(at) < ttl && now.Sub(at) >= 0 {
		return c.events[project], nil
	}
	events, err := fs.HealthEvents(ctx, project)
	if err != nil {
		return nil, err
	}
	c.fetched[project], c.events[project] = now, events
	return events, nil
}

func (c *eventsCache) invalidate(project int64) {
	c.mu.Lock()
	delete(c.fetched, project)
	c.mu.Unlock()
}
