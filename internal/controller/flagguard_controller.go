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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
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
	conditionRequests      = "RequestsRead"

	maxTimeline   = 20
	notReadyGrace = 20 * time.Second
	// recentWebhook bounds how old a webhook may be to date a change found
	// on the first check after Flagbreaker starts.
	recentWebhook = time.Minute

	// cleanupFinalizer lets Flagbreaker clear Feature Health when a guard is
	// deleted mid-incident.
	cleanupFinalizer = "flagbreaker.dev/clear-feature-health"
)

// +kubebuilder:rbac:groups=flagbreaker.dev,resources=flagguards,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=flagbreaker.dev,resources=flagguards/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets;daemonsets;controllerrevisions,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
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
	// RequireNamespaceOptIn makes a guard act only on environments its
	// namespace's flagbreaker.dev/allowed-environments annotation lists.
	RequireNamespaceOptIn bool
	// Requests reads the request counters of guards with spec.requests.
	// Optional: one with the default client is made on first use.
	Requests *requests.Tracker

	mu        sync.Mutex
	lastCheck map[types.NamespacedName]time.Time
	// pending holds a status that couldn't be written, for example after a
	// conflict. Flagbreaker is the only writer of FlagGuard status, so the
	// next check starts from it: an incident whose switch-off already
	// happened in Flagsmith is never forgotten.
	pending map[types.NamespacedName]pendingStatus
	// Webhook-triggered checks arrive on this channel.
	triggers chan event.GenericEvent
	events   eventsCache
}

type pendingStatus struct {
	uid    types.UID
	status *v1alpha1.FlagGuardStatus
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
	// A status this controller computed but couldn't write is the truth.
	before := guard.Status.DeepCopy()
	pending, hasPending := r.takePending(req.NamespacedName, guard.UID)
	if hasPending {
		guard.Status = *pending
	}
	if !guard.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &guard)
	}
	if !controllerutil.ContainsFinalizer(&guard, cleanupFinalizer) {
		controllerutil.AddFinalizer(&guard, cleanupFinalizer)
		if err := r.Update(ctx, &guard); err != nil {
			if hasPending {
				r.setPending(req.NamespacedName, guard.UID, pending)
			}
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if hasPending {
			guard.Status = *pending // Update replaced it with the stored status
		}
	}

	var notAllowed *notAllowedError
	switch err := r.check(ctx, &guard, r.now()); {
	case errors.As(err, &notAllowed):
		// A configuration state, not a failure: no stack traces every 5s.
		setCondition(&guard, conditionReady, metav1.ConditionFalse, "EnvironmentNotAllowed", err.Error())
	case err != nil:
		logf.FromContext(ctx).Error(err, "check failed")
		metrics.Errors.WithLabelValues(guard.Namespace, guard.Name).Inc()
		setCondition(&guard, conditionReady, metav1.ConditionFalse, "CheckFailed", err.Error())
	default:
		setCondition(&guard, conditionReady, metav1.ConditionTrue, "Watching", "Flagsmith and the workload were read successfully")
		// Only a check that went through is a check of this spec: a failed one
		// must look the flag up again next time.
		guard.Status.ObservedGeneration = guard.Generation
	}
	metrics.SetPhase(guard.Namespace, guard.Name, guard.Spec.Flag, string(guard.Status.Phase))

	if !equality.Semantic.DeepEqual(before, &guard.Status) {
		if err := r.Status().Update(ctx, &guard); err != nil {
			// Keep what this check found and did, and write it next time.
			r.setPending(req.NamespacedName, guard.UID, guard.Status.DeepCopy())
			if apierrors.IsConflict(err) {
				// Someone else wrote the guard (its spec or metadata) since
				// it was read. Check again soon, from the fresh object.
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: r.SyncPeriod}, nil
}

// finalize runs when a guard is deleted. Mid-incident, its Unhealthy event
// would otherwise stay in Feature Health (and the flag's Unhealthy tag) for
// good, so it is cleared, saying why. If Flagsmith can't be reached, deletion
// waits a little, then goes ahead anyway.
func (r *FlagGuardReconciler) finalize(ctx context.Context, guard *v1alpha1.FlagGuard) error {
	if !controllerutil.ContainsFinalizer(guard, cleanupFinalizer) {
		return nil
	}
	st := &guard.Status
	healthyUnsent := (st.Phase == v1alpha1.PhaseRecovered || st.RecoveredAt != nil) && !conditionTrue(st.Conditions, conditionReported)
	if (st.Phase == v1alpha1.PhaseTripped || healthyUnsent) && st.Environment != "" &&
		r.environmentAllowed(ctx, guard.Namespace, guard.Spec.EnvironmentKey) == nil {
		others, err := r.otherTrippedGuards(ctx, guard)
		if err == nil && len(others) == 0 {
			text := fmt.Sprintf("The FlagGuard %s/%s was deleted during an incident, so Flagbreaker no longer watches this flag for that workload. This is not a verified recovery.",
				guard.Namespace, guard.Name)
			if healthyUnsent {
				text = fmt.Sprintf("The workload recovered (verified by Flagbreaker), and the FlagGuard %s/%s was then deleted.", guard.Namespace, guard.Name)
			}
			if st.Snapshot != nil {
				text += " Flagbreaker had switched the flag off; switch it back on in Flagsmith once it is safe."
			}
			event := flagsmith.HealthEvent{Feature: guard.Spec.Flag, Status: flagsmith.Healthy, Environment: st.Environment}
			event.Reason.TextBlocks = []flagsmith.TextBlock{{Title: eventTitlePrefix + "FlagGuard deleted", Text: text}}
			err = r.Flagsmith.ReportHealth(ctx, event)
		}
		if err != nil && r.now().Sub(guard.DeletionTimestamp.Time) < 2*time.Minute {
			return fmt.Errorf("clearing Feature Health before deleting the guard (will retry): %w", err)
		}
	}
	controllerutil.RemoveFinalizer(guard, cleanupFinalizer)
	if err := r.Update(ctx, guard); err != nil {
		return client.IgnoreNotFound(err)
	}
	r.forget(client.ObjectKeyFromObject(guard))
	return nil
}

func (r *FlagGuardReconciler) takePending(key types.NamespacedName, uid types.UID) (*v1alpha1.FlagGuardStatus, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[key]
	delete(r.pending, key)
	if !ok || p.uid != uid {
		return nil, false // none, or it was for a deleted guard of the same name
	}
	return p.status, true
}

func (r *FlagGuardReconciler) setPending(key types.NamespacedName, uid types.UID, st *v1alpha1.FlagGuardStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = map[types.NamespacedName]pendingStatus{}
	}
	r.pending[key] = pendingStatus{uid: uid, status: st}
}

func (r *FlagGuardReconciler) check(ctx context.Context, guard *v1alpha1.FlagGuard, now time.Time) error {
	spec := withDefaults(guard.Spec)
	st := &guard.Status
	if st.Phase == "" {
		st.Phase = v1alpha1.PhaseArmed
	}

	// 0. May this namespace touch that environment at all?
	if err := r.environmentAllowed(ctx, guard.Namespace, spec.EnvironmentKey); err != nil {
		return err
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
			metrics.Forget(guard.Namespace, guard.Name) // drop series labelled with the old flag
		}
		st.Environment, st.EnvironmentID, st.ProjectID, st.FeatureID = env.Name, env.ID, env.ProjectID, id
	}
	flag, err := r.Flagsmith.GetFlag(ctx, spec.EnvironmentKey, st.FeatureID)
	if errors.Is(err, flagsmith.ErrNotFound) {
		// The flag was deleted, perhaps to be created again: look it up by
		// name next time.
		st.FeatureID = 0
	}
	if err != nil {
		return fmt.Errorf("reading flag %q: %w", spec.Flag, err)
	}
	enabled := flag.EnvironmentDefault.Enabled
	value := valueOf(flag.EnvironmentDefault.Value)

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
	// 2b. For a guard with spec.requests, read what the pods counted.
	series, notBefore, readsRequests := r.readRequests(ctx, guard, spec, t, pods, now)

	// 3. Notice flag changes made by people. A switch-off during an
	// incident, or a new value during one a value caused, is what watch mode
	// asks a person to do, unless it is Flagbreaker's own, sent earlier and
	// not confirmed (a timeout).
	var personOff *v1alpha1.FlagChange
	if change := r.trackFlag(guard, enabled, value, assessment, now); change != nil &&
		st.Phase == v1alpha1.PhaseTripped && st.FlagDisabledAt == nil && mitigates(st.Trigger, change) {
		if st.SwitchOffAttemptedAt != nil && ownFix(st.Trigger, change) {
			r.confirmSwitchOff(ctx, guard, spec, change, assessment, now)
		} else {
			st.SwitchOffAttemptedAt = nil // a person got there first
			personOff = change
		}
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
	case mode == v1alpha1.ModeWatch && st.Mode == v1alpha1.ModeAct && !s.Unavailable:
		// A switch to watch in Flagsmith stops an incident from acting
		// further. The reverse never happens mid-incident, and a moment of
		// unreadable settings doesn't count as a switch.
		st.Mode, st.ModeReason = mode, res.Reason
	default:
		mode = st.Mode // finish the incident in the mode it started in
	}

	// 5. A person asked for the flag to be restored.
	if handled, err := r.handleRestore(ctx, guard, spec, flag, assessment, now); handled || err != nil {
		return err
	}

	// 6. What Feature Health says: other providers' alerts, and whether a
	// person dismissed Flagbreaker's own Unhealthy event.
	alerts, dismissed := r.healthSignals(ctx, guard, spec, now)

	// A report that failed last time is sent again.
	if c := meta.FindStatusCondition(st.Conditions, conditionReported); c != nil && c.Status == metav1.ConditionFalse {
		r.reportPhase(ctx, guard, spec, assessment)
	}

	// An incident's recovery waits only for alerts that started with it or
	// after: one already firing before the switch-on is about something else.
	if st.Phase == v1alpha1.PhaseTripped {
		alerts = alertsFrom(alerts, incidentStart(st))
	}

	// 7. Decide, then act.
	valueIncident := isValueIncident(st)
	in := breaker.Input{
		Now: now, Phase: st.Phase, Mode: mode,
		FlagEnabled: enabled, LastChange: st.LastFlagChange,
		ValueIncident: valueIncident, AlsoSwitchedOn: valueIncident && st.Trigger.IsSwitchOn(),
		NewValueServed: valueIncident && sameValue(&value, st.Trigger.Value),
		ValueCameBack:  valueCameBack(st),
		Pods:           assessment, DesiredReplicas: t.desired, RolloutStartedAt: rolloutAt,
		Alerts: alerts, AlertsSince: alertsSince(guard), HandledEventID: st.HandledEventID, Dismissed: dismissed,
		Window: spec.Window.Duration, StableFor: spec.StableFor.Duration, FailingPods: int(spec.FailingPods),
		FlagDisabledAt: timePtr(st.FlagDisabledAt), HealthySince: timePtr(st.HealthySince),
		RequestsIncident: st.RequestsIncident,
	}
	if readsRequests {
		in.Requests = &breaker.Requests{Series: series, NotBefore: notBefore,
			MaxErrorPercent: float64(spec.Requests.MaxErrorPercent), MinRequests: float64(spec.Requests.MinRequests)}
		if limit, ok := s.ErrorLimit(spec.Flag); ok {
			in.Requests.MaxErrorPercent, in.Requests.LimitFrom = limit, settings.PolicyFlag
		}
	}
	d := breaker.Decide(in)
	st.HealthySince = metaTimePtr(d.HealthySince)

	switch d.Action {
	case breaker.Trip:
		return r.trip(ctx, guard, spec, t, flag, assessment, alerts, d, now)
	case breaker.SwitchOff:
		return r.mitigate(ctx, guard, spec, flag, assessment, now)
	case breaker.Recover:
		return r.recover(ctx, guard, spec, assessment, d, now)
	case breaker.Rearm:
		r.rearm(ctx, guard, spec, assessment, alerts, d, now)
	case breaker.Wait:
		return r.retryLeftOnSegments(ctx, guard, spec, flag, now)
	}
	return nil
}

// incidentStart is when the current incident began: the change or the alert
// that tripped it.
func incidentStart(st *v1alpha1.FlagGuardStatus) time.Time {
	switch {
	case st.Trigger != nil:
		return st.Trigger.At.Time
	case st.Alert != nil:
		return st.Alert.At.Time
	case st.TrippedAt != nil:
		return st.TrippedAt.Time
	}
	return time.Time{}
}

func alertsFrom(alerts []breaker.Alert, since time.Time) []breaker.Alert {
	var out []breaker.Alert
	for _, a := range alerts {
		if !a.At.Before(since) {
			out = append(out, a)
		}
	}
	return out
}

// retryLeftOnSegments switches off, during an act-mode incident, segment
// overrides that an earlier attempt couldn't (a timeout or a 5xx). Overrides
// Flagsmith refused, such as an experiment's, are no longer in the snapshot.
func (r *FlagGuardReconciler) retryLeftOnSegments(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, flag flagsmith.Flag, now time.Time) error {
	st := &guard.Status
	// An incident only a value caused switches nothing off; overrides in
	// the snapshot are from an earlier incident and wait for a restore.
	if st.Phase != v1alpha1.PhaseTripped || st.Mode != v1alpha1.ModeAct || st.FlagDisabledBy != ChangedByFlagbreaker || st.Snapshot == nil || valueOnly(st) {
		return nil
	}
	var stillOn []int64
	for _, id := range flag.EnabledSegmentIDs() {
		if contains(st.Snapshot.EnabledSegmentOverrides, id) {
			stillOn = append(stillOn, id)
		}
	}
	if len(stillOn) == 0 {
		return nil
	}
	off := false
	if _, err := r.Flagsmith.UpdateFlag(ctx, spec.EnvironmentKey, st.FeatureID, flagsmith.Update{Default: &off, Segments: stillOn}); err != nil {
		return fmt.Errorf("switching off the segment overrides left on: %w", err)
	}
	addTimeline(st, now, "SegmentsSwitchedOff", fmt.Sprintf("Switched off %d segment override%s an earlier attempt couldn't reach", len(stillOn), plural(len(stillOn))))
	return nil
}

// environmentAllowed checks the namespace's flagbreaker.dev/allowed-environments
// annotation. Nothing is read from or written to Flagsmith for a guard that
// fails it.
func (r *FlagGuardReconciler) environmentAllowed(ctx context.Context, namespace, environmentKey string) error {
	if !r.RequireNamespaceOptIn {
		return nil
	}
	var ns corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return fmt.Errorf("reading namespace %q: %w", namespace, err)
	}
	for _, k := range splitFlags(ns.Annotations[v1alpha1.AllowedEnvironmentsAnnotation]) {
		if k == "*" || k == environmentKey {
			return nil
		}
	}
	return &notAllowedError{namespace: namespace}
}

// notAllowedError: the guard's namespace hasn't opted into its environment.
type notAllowedError struct{ namespace string }

func (e *notAllowedError) Error() string {
	return fmt.Sprintf("namespace %s may not act on this Flagsmith environment: a cluster admin has to list its key in the %s annotation of the namespace (or \"*\" for any)",
		e.namespace, v1alpha1.AllowedEnvironmentsAnnotation)
}

func conditionTrue(conditions []metav1.Condition, kind string) bool {
	c := meta.FindStatusCondition(conditions, kind)
	return c != nil && c.Status == metav1.ConditionTrue
}

// confirmSwitchOff records a fix Flagbreaker sent but couldn't confirm at
// the time (it timed out after Flagsmith applied it): the flag is off now, or
// has its previous value back, and it was Flagbreaker's doing, not a
// person's.
func (r *FlagGuardReconciler) confirmSwitchOff(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, change *v1alpha1.FlagChange, a workload.Assessment, now time.Time) {
	st := &guard.Status
	st.SwitchOffAttemptedAt = nil
	st.FlagDisabledAt, st.FlagDisabledBy = metaNow(change.At.Time), ChangedByFlagbreaker
	// Flagbreaker's own change is never blamed for a breakage.
	change.ChangedBy, change.Tripped = ChangedByFlagbreaker, true
	if !change.Enabled && (st.Trigger == nil || st.Trigger.IsSwitchOn()) {
		addTimeline(st, now, "FlagSwitchedOff",
			fmt.Sprintf("Switched %s off in %s; Flagsmith applied the earlier request that timed out. Identity overrides and experiments are not changed.", spec.Flag, st.Environment))
		metrics.SwitchOffs.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "flagbreaker").Inc()
	}
	if change.ValueChanged && isValueIncident(st) {
		addTimeline(st, now, "ValueReverted",
			fmt.Sprintf("Put %s's previous value back in %s; Flagsmith applied the earlier request that timed out.", spec.Flag, st.Environment))
		metrics.ValueReverts.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "flagbreaker").Inc()
	}
	r.restartCrashed(ctx, guard, spec, a, now)
}

// isValueIncident reports whether a new value is among the current
// incident's causes.
func isValueIncident(st *v1alpha1.FlagGuardStatus) bool {
	return st.Trigger != nil && st.Trigger.ValueChanged
}

// valueOnly reports whether a new value is the current incident's only
// cause: nothing was switched on.
func valueOnly(st *v1alpha1.FlagGuardStatus) bool {
	return isValueIncident(st) && !st.Trigger.IsSwitchOn()
}

// mitigates reports whether a change is the fix watch mode asks a person
// for: a switch-off, when a switch-on or an alert is among the causes, or a
// different value, when a new value is the only cause.
func mitigates(trigger, change *v1alpha1.FlagChange) bool {
	if trigger == nil || trigger.IsSwitchOn() {
		return !change.Enabled
	}
	return change.ValueChanged && !sameValue(change.Value, trigger.Value)
}

// ownFix reports whether a change is what Flagbreaker sent to end the
// incident. Flagbreaker knows the value it sent, so a different value is a
// person's.
func ownFix(trigger, change *v1alpha1.FlagChange) bool {
	if trigger != nil && trigger.ValueChanged && change.ValueChanged {
		return sameValue(change.Value, trigger.PreviousValue)
	}
	return true
}

// valueCameBack reports whether, during a value incident, the new value was
// set again after the incident began.
func valueCameBack(st *v1alpha1.FlagGuardStatus) bool {
	c := st.LastFlagChange
	return isValueIncident(st) && st.TrippedAt != nil && c != nil && c.ValueChanged &&
		sameValue(c.Value, st.Trigger.Value) && !c.At.Time.Before(st.TrippedAt.Time)
}

// healthyReportPending reports whether a Healthy report failed or is
// waiting for other guards of the flag.
func healthyReportPending(st *v1alpha1.FlagGuardStatus) bool {
	c := meta.FindStatusCondition(st.Conditions, conditionReported)
	return c != nil && c.Status == metav1.ConditionFalse && (c.Reason == "HealthyReportFailed" || c.Reason == "WaitingForOtherGuards")
}

// alertsSince is when an alert must have started to be about the flag as
// it is now: the latest switch-on, or when the guard was created.
func alertsSince(guard *v1alpha1.FlagGuard) time.Time {
	since := guard.CreationTimestamp.Time
	if c := guard.Status.LastFlagChange; c != nil && c.Enabled && c.At.After(since) {
		since = c.At.Time
	}
	return since
}

// resetForNewFlag starts over when a guard's spec now points at another flag
// or environment: nothing about the old one applies.
func resetForNewFlag(st *v1alpha1.FlagGuardStatus, spec v1alpha1.FlagGuardSpec, environment string, now time.Time) {
	msg := fmt.Sprintf("Now guarding %s in %s; the previous flag's state was dropped", spec.Flag, environment)
	if st.Snapshot != nil {
		msg += fmt.Sprintf(". Flagbreaker had switched off feature %d in %s; turn it back on in Flagsmith if it should be on", st.FeatureID, st.Environment)
	}
	*st = v1alpha1.FlagGuardStatus{Phase: v1alpha1.PhaseArmed, Conditions: st.Conditions}
	addTimeline(st, now, "GuardChanged", msg)
}

// trackFlag records a change to the flag's environment default, a switch or
// a new value, and returns it, or nil when nothing changed. The first time a
// guard looks, it only remembers the state: a flag that was already on, or
// already had its value, is never blamed.
func (r *FlagGuardReconciler) trackFlag(guard *v1alpha1.FlagGuard, enabled bool, value v1alpha1.FlagValue, a workload.Assessment, now time.Time) *v1alpha1.FlagChange {
	st := &guard.Status
	key := client.ObjectKeyFromObject(guard)

	r.mu.Lock()
	prev, seen := r.lastCheck[key]
	r.lastCheck[key] = now
	r.mu.Unlock()

	if st.FlagEnabled == nil {
		v := value
		st.FlagEnabled, st.FlagValue = &enabled, &v
		return nil
	}
	if st.FlagValue == nil {
		// A guard from before Flagbreaker tracked values: remember this one.
		v := value
		st.FlagValue = &v
	}
	switched := *st.FlagEnabled != enabled
	if !switched && sameValue(st.FlagValue, &value) {
		r.enrich(guard, a)
		return nil
	}
	old, current := *st.FlagValue, value
	st.FlagEnabled, st.FlagValue = &enabled, &current
	revalued := !sameValue(&old, &value)

	// One poll can see a switch and a new value together: one save in the
	// dashboard can make both.
	change := &v1alpha1.FlagChange{At: metav1.NewTime(prev), Enabled: enabled, SwitchedOn: switched && enabled}
	if revalued {
		v := value
		change.ValueChanged, change.Value, change.PreviousValue = true, &v, &old
	}
	// The change happened after the previous check, which may be long ago
	// if checks failed in between. If the webhook is already here, it says
	// exactly when, and who made it.
	if !seen {
		// The first check since Flagbreaker started: the change happened
		// while it wasn't running, at a time it can't know.
		change.At, change.TimeUnknown = metav1.NewTime(now), true
	}
	from := prev
	if !seen {
		from = now.Add(-recentWebhook)
	}
	if c, ok := r.webhookFor(st, !switched, enabled); ok && matches(c, from, now, r.SyncPeriod) {
		change.At, change.ChangedBy, change.TimeUnknown = metav1.NewTime(c.At), c.ChangedBy, false
	}
	// Recover first, blame second: a switch-on or a new value from just
	// before is still a suspect, so undoing this change undoes it too, and
	// the suspects count from the first of them.
	spec := withDefaults(guard.Spec)
	if p := st.LastFlagChange; st.Phase != v1alpha1.PhaseTripped && stillSuspect(p, now, spec.Window.Duration) {
		carried := false
		if p.IsSwitchOn() && enabled && !change.SwitchedOn {
			change.SwitchedOn, carried = true, true
		}
		if p.ValueChanged && !change.ValueChanged {
			v := value
			change.ValueChanged, change.Value, change.PreviousValue, carried = true, &v, p.PreviousValue.DeepCopy(), true
		}
		if carried {
			change.At, change.ChangedBy, change.TimeUnknown = p.At, p.ChangedBy, p.TimeUnknown
		}
	}
	// Was it failing just before? Old crashes stay in pod status long after
	// a pod recovers, so look back only a little.
	change.WorkloadWasFailing = change.IsSuspect() && wasFailing(a, change.At.Time, failingFrom(st, spec, change.At.Time))
	st.LastFlagChange = change
	return change
}

// stillSuspect reports whether a change can still be blamed for a breakage
// that starts now.
func stillSuspect(c *v1alpha1.FlagChange, now time.Time, window time.Duration) bool {
	return c != nil && c.IsSuspect() && !c.Tripped && !c.Acknowledged && !c.TimeUnknown && !c.WorkloadWasFailing &&
		now.Sub(c.At.Time) <= window
}

// webhookFor finds the webhook that reported a change polling saw: for a
// switch, the latest switch to the same state; for a value edit, the latest
// value edit. Flagbreaker edits a value only to put one back during a value
// incident, so a value edit reported from then on, give or take the clocks'
// skew, may be its own and isn't used to name who made a change.
func (r *FlagGuardReconciler) webhookFor(st *v1alpha1.FlagGuardStatus, valueEdit, enabled bool) (changes.Change, bool) {
	if !valueEdit {
		return r.Changes.Latest(st.EnvironmentID, st.FeatureID, enabled)
	}
	w, ok := r.Changes.LatestValue(st.EnvironmentID, st.FeatureID)
	if !ok || w.ChangedBy == ChangedByFlagbreaker { // the demo's API key has that name
		return w, false
	}
	if isValueIncident(st) && st.FlagDisabledBy == ChangedByFlagbreaker && st.FlagDisabledAt != nil &&
		!w.At.Before(st.FlagDisabledAt.Add(-r.SyncPeriod)) {
		return w, false
	}
	return w, true
}

// isValueEdit reports whether a recorded change came from a value edit
// rather than a switch, for matching it to a webhook.
func isValueEdit(c *v1alpha1.FlagChange) bool {
	return c.ValueChanged && !c.IsSwitchOn()
}

// valueOf turns a Flagsmith value into the API's FlagValue; no value is the
// zero FlagValue.
func valueOf(v *flagsmith.Value) v1alpha1.FlagValue {
	if v == nil {
		return v1alpha1.FlagValue{}
	}
	return v1alpha1.FlagValue{Type: v.Type, Value: v.Value}
}

func sameValue(a, b *v1alpha1.FlagValue) bool {
	conv := func(v *v1alpha1.FlagValue) *flagsmith.Value {
		if v == nil {
			return nil
		}
		return &flagsmith.Value{Type: v.Type, Value: v.Value}
	}
	return flagsmith.SameValue(conv(a), conv(b))
}

// enrich adds a webhook's exact time and author to a change that polling saw
// first. Flagsmith sends FLAG_UPDATED a few seconds after the change, from a
// background task, so polling often wins the race.
func (r *FlagGuardReconciler) enrich(guard *v1alpha1.FlagGuard, a workload.Assessment) {
	st := &guard.Status
	// A change first seen after a restart gets its real time from a webhook;
	// whether the workload was failing before it is then checked again.
	if c := st.LastFlagChange; c != nil && c.TimeUnknown {
		if w, ok := r.webhookFor(st, isValueEdit(c), c.Enabled); ok && matches(w, c.At.Add(-recentWebhook), c.At.Add(r.SyncPeriod), r.SyncPeriod) {
			c.At, c.ChangedBy, c.TimeUnknown = metav1.NewTime(w.At), w.ChangedBy, false
			c.WorkloadWasFailing = c.IsSuspect() && wasFailing(a, c.At.Time, failingFrom(st, withDefaults(guard.Spec), c.At.Time))
		}
	}
	for _, c := range []*v1alpha1.FlagChange{st.LastFlagChange, st.Trigger} {
		if c == nil || c.ChangedBy != "" || c.TimeUnknown {
			continue
		}
		w, ok := r.webhookFor(st, isValueEdit(c), c.Enabled)
		if !ok || w.ChangedBy == "" || !matches(w, c.At.Time, c.At.Time.Add(r.SyncPeriod), r.SyncPeriod) {
			continue
		}
		c.At, c.ChangedBy = metav1.NewTime(w.At), w.ChangedBy
	}
	if st.Trigger != nil && len(st.Timeline) > 0 && st.Timeline[0].Event == "FlagChanged" {
		st.Timeline[0].Time = st.Trigger.At
		st.Timeline[0].Message = describeChange(guard.Spec.Flag, st.Environment, st.Trigger)
	}
	// A person's switch-off (or new value) during an incident gets its
	// author too.
	if c := st.LastFlagChange; st.FlagDisabledAt != nil && st.FlagDisabledBy == "" && c != nil && c.ChangedBy != "" && mitigates(st.Trigger, c) {
		st.FlagDisabledBy = c.ChangedBy
		for i := range st.Timeline {
			switch st.Timeline[i].Event {
			case "FlagSwitchedOffByPerson":
				st.Timeline[i].Message = personSwitchOffMessage(guard.Spec.Flag, st.Environment, c.ChangedBy)
			case "ValueChangedByPerson":
				st.Timeline[i].Message = personValueMessage(guard.Spec.Flag, st.Environment, c)
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
	st.ReportedEventID = 0
	// Recovery from an incident that failing requests caused waits for
	// requests that succeed.
	st.RequestsIncident = d.Cause == breaker.CauseRequests
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
	// A change is blamed for one incident only, so a workload that heals
	// with the flag still on, or the new value still in place (watch mode),
	// isn't tripped again by it.
	if c := st.LastFlagChange; c != nil && c.IsSuspect() {
		c.Tripped = true
	}
	if d.Cause == breaker.CauseAlert {
		addTimeline(st, now, "Tripped", fmt.Sprintf("Tripped for %s %s/%s: %s", t.kind, guard.Namespace, t.obj.GetName(), d.Why))
	} else {
		addTimeline(st, now, "Tripped", fmt.Sprintf("%s %s/%s: %s", t.kind, guard.Namespace, t.obj.GetName(), d.Why))
	}
	r.Recorder.Eventf(guard, t.obj, corev1.EventTypeWarning, "Tripped", "Trip", "%s", d.Why)
	metrics.Trips.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, string(d.Mode), string(d.Cause)).Inc()

	var err error
	switch {
	case d.Mode == v1alpha1.ModeAct:
		err = r.mitigate(ctx, guard, spec, flag, a, now)
	case d.ModeReason == "" && valueOnly(st):
		addTimeline(st, now, "WatchOnly", fmt.Sprintf("Watch mode (%s): the new value was left in place. Put %s's previous value back in Flagsmith to recover.", st.ModeReason, spec.Flag))
	case d.ModeReason == "" && isValueIncident(st):
		addTimeline(st, now, "WatchOnly", fmt.Sprintf("Watch mode (%s): the flag was left on, with its new value. Switch %s off and put its previous value back in Flagsmith to recover.", st.ModeReason, spec.Flag))
	case d.ModeReason == "":
		addTimeline(st, now, "WatchOnly", fmt.Sprintf("Watch mode (%s): the flag was left on. Switch %s off in Flagsmith to recover.", st.ModeReason, spec.Flag))
	}
	r.report(ctx, guard, spec, flagsmith.Unhealthy, a)
	return err
}

// mitigate undoes, in act mode, what caused the incident: it switches the
// flag off for a switch-on or an alert, puts the previous value back for a
// new value, or both when both are suspects. Then it restarts the crashed
// pods, once.
func (r *FlagGuardReconciler) mitigate(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	flag flagsmith.Flag, a workload.Assessment, now time.Time) error {
	st := &guard.Status
	did := false
	if (st.Trigger == nil || st.Trigger.IsSwitchOn()) && flag.EnvironmentDefault.Enabled {
		done, err := r.switchOff(ctx, guard, spec, flag, now)
		if err != nil {
			return err
		}
		did = done
	}
	if isValueIncident(st) && st.Mode == v1alpha1.ModeAct { // change requests drop it to watch
		done, err := r.putValueBack(ctx, guard, spec, flag, now)
		if err != nil {
			return err
		}
		did = did || done
	}
	if did {
		r.restartCrashed(ctx, guard, spec, a, now)
	}
	return nil
}

// putValueBack undoes the value change that caused the incident: it sets
// the environment default's value back to what it was, and leaves whether
// the flag is on alone. Many apps read a remote config value whether or not
// the flag is on, so switching the flag off wouldn't help them.
func (r *FlagGuardReconciler) putValueBack(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	flag flagsmith.Flag, now time.Time) (bool, error) {
	st := &guard.Status
	trigger := st.Trigger
	if current := valueOf(flag.EnvironmentDefault.Value); !sameValue(&current, trigger.Value) {
		return false, nil // someone changed it again since; their change stands
	}
	previous := v1alpha1.FlagValue{}
	if trigger.PreviousValue != nil {
		previous = *trigger.PreviousValue
	}

	// Marked before sending, as for a switch-off: if the request times out
	// after Flagsmith applied it, the next check knows it was Flagbreaker's.
	st.SwitchOffAttemptedAt = metaNow(now)
	_, err := r.Flagsmith.SetValue(ctx, spec.EnvironmentKey, st.FeatureID, flagsmith.Value{Type: previous.Type, Value: previous.Value})
	switch {
	case errors.Is(err, flagsmith.ErrChangeRequestsEnabled):
		st.SwitchOffAttemptedAt = nil
		st.Mode = v1alpha1.ModeWatch
		st.ModeReason = st.Environment + " requires change requests"
		setCondition(guard, conditionBlocked, metav1.ConditionTrue, "ChangeRequestsEnabled",
			fmt.Sprintf("%s requires change requests, so Flagbreaker won't change flags there", st.Environment))
		addTimeline(st, now, "BlockedByChangeRequests",
			fmt.Sprintf("%s requires change requests, so Flagbreaker left the new value in place. Approve a change request to put the previous value back.", st.Environment))
		return false, nil
	case err != nil:
		return false, fmt.Errorf("putting %q's previous value back: %w", spec.Flag, err)
	}

	st.SwitchOffAttemptedAt = nil
	st.FlagValue = &previous
	st.FlagDisabledAt, st.FlagDisabledBy = metaNow(now), ChangedByFlagbreaker
	// Flagbreaker's own change is never blamed for a breakage: one after it
	// belongs to this incident, and blaming it would put the bad value back.
	bad := *trigger.Value
	enabled := st.FlagEnabled != nil && *st.FlagEnabled // after a switch-off in the same check, off
	st.LastFlagChange = &v1alpha1.FlagChange{At: metav1.NewTime(now), Enabled: enabled, ValueChanged: true,
		Value: &previous, PreviousValue: &bad, ChangedBy: ChangedByFlagbreaker, Tripped: true}
	addTimeline(st, now, "ValueReverted", fmt.Sprintf("Put %s's previous value back in %s: %s instead of %s.",
		spec.Flag, st.Environment, showValue(previous), showValue(bad)))
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "ValueReverted", "Revert", "Put %s's previous value back in %s", spec.Flag, st.Environment)
	metrics.ValueReverts.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "flagbreaker").Inc()
	if st.TrippedAt != nil {
		metrics.Observe(metrics.SwitchOffSeconds, now.Sub(st.TrippedAt.Time), spec.Flag, st.Environment)
	}
	return true, nil
}

// switchOff turns off the flag's environment default and its segment
// overrides that are on. It reports whether it switched anything off.
func (r *FlagGuardReconciler) switchOff(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	flag flagsmith.Flag, now time.Time) (bool, error) {
	st := &guard.Status
	segments := flag.EnabledSegmentIDs()

	// Remember what is about to be switched off first. If the request times
	// out after Flagsmith applied it, a restore can still undo it. Overrides
	// still off from an earlier incident stay in the snapshot too.
	var pending []int64
	if st.Snapshot != nil {
		pending = st.Snapshot.EnabledSegmentOverrides
	}
	st.Snapshot = &v1alpha1.FlagSnapshot{Enabled: flag.EnvironmentDefault.Enabled || (st.Snapshot != nil && st.Snapshot.Enabled),
		EnabledSegmentOverrides: union(pending, segments)}

	// Marked before sending: if the request times out after Flagsmith
	// applied it, the next check knows the switch-off was Flagbreaker's.
	st.SwitchOffAttemptedAt = metaNow(now)
	_, err := r.Flagsmith.SwitchOff(ctx, spec.EnvironmentKey, st.FeatureID, segments)
	if flagsmith.IsRejected(err) && len(segments) > 0 {
		// Flagsmith refused the whole change, typically because one override
		// belongs to an experiment, which this API can't change. Switch off
		// the default, then each override on its own, and say what stayed on.
		segments, err = r.switchOffOneByOne(ctx, guard, spec, segments, now)
	}
	switch {
	case errors.Is(err, flagsmith.ErrChangeRequestsEnabled):
		st.SwitchOffAttemptedAt = nil
		st.Mode = v1alpha1.ModeWatch
		st.ModeReason = st.Environment + " requires change requests"
		setCondition(guard, conditionBlocked, metav1.ConditionTrue, "ChangeRequestsEnabled",
			fmt.Sprintf("%s requires change requests, so Flagbreaker won't change flags there", st.Environment))
		addTimeline(st, now, "BlockedByChangeRequests",
			fmt.Sprintf("%s requires change requests, so Flagbreaker left the flag on. Approve a change request to switch it off.", st.Environment))
		return false, nil
	case err != nil:
		return false, fmt.Errorf("switching %q off: %w", spec.Flag, err)
	}

	off := false
	st.FlagEnabled = &off
	st.SwitchOffAttemptedAt = nil
	st.FlagDisabledAt, st.FlagDisabledBy = metaNow(now), ChangedByFlagbreaker
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
	return true, nil
}

// switchOffOneByOne switches off the environment default, then each segment
// override separately. It returns the overrides it switched off; the ones
// Flagsmith refused are named in the timeline and left out of the snapshot.
func (r *FlagGuardReconciler) switchOffOneByOne(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	segments []int64, now time.Time) ([]int64, error) {
	st := &guard.Status
	if _, err := r.Flagsmith.SwitchOff(ctx, spec.EnvironmentKey, st.FeatureID, nil); err != nil {
		return nil, err
	}
	var done, refused, later []int64
	for _, id := range segments {
		off := false
		_, err := r.Flagsmith.UpdateFlag(ctx, spec.EnvironmentKey, st.FeatureID, flagsmith.Update{Default: &off, Segments: []int64{id}})
		switch {
		case err == nil:
			done = append(done, id)
		case flagsmith.IsRejected(err):
			refused = append(refused, id)
		default:
			// A timeout or a 5xx: it stays in the snapshot and is retried.
			later = append(later, id)
		}
	}
	if len(later) > 0 {
		addTimeline(st, now, "SegmentsRetrying", fmt.Sprintf("Couldn't reach Flagsmith to switch off the override%s for segment%s %s; retrying",
			plural(len(later)), plural(len(later)), joinIDs(later)))
	}
	if len(refused) > 0 {
		var kept []int64
		for _, id := range st.Snapshot.EnabledSegmentOverrides {
			if !contains(refused, id) {
				kept = append(kept, id)
			}
		}
		st.Snapshot.EnabledSegmentOverrides = kept
		addTimeline(st, now, "SegmentsLeftOn", fmt.Sprintf("Flagsmith refused to switch off the override%s for segment%s %s (an experiment's overrides can't be changed this way); %s still serve%s the flag",
			plural(len(refused)), plural(len(refused)), joinIDs(refused), itThey(len(refused)), sIfOne(len(refused))))
	}
	return done, nil
}

// personSwitchedOff records that a person switched the flag off during an
// incident, as watch mode asks them to, and restarts crashed pods.
func (r *FlagGuardReconciler) personSwitchedOff(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	change *v1alpha1.FlagChange, a workload.Assessment, restart bool, now time.Time) {
	st := &guard.Status
	at := change.At
	st.FlagDisabledAt, st.FlagDisabledBy = &at, change.ChangedBy
	// The fix belongs to this incident and is never blamed: blaming it would
	// undo it.
	change.Tripped = true
	if valueOnly(st) {
		addTimeline(st, at.Time, "ValueChangedByPerson", personValueMessage(spec.Flag, st.Environment, change))
		r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "ValueChanged", "Revert", "%s's value was changed in %s by a person", spec.Flag, st.Environment)
		metrics.ValueReverts.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "person").Inc()
	} else {
		addTimeline(st, at.Time, "FlagSwitchedOffByPerson", personSwitchOffMessage(spec.Flag, st.Environment, change.ChangedBy))
		r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "FlagSwitchedOff", "SwitchOff", "%s was switched off in %s by a person", spec.Flag, st.Environment)
		metrics.SwitchOffs.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment, "person").Inc()
	}
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

func personValueMessage(flag, environment string, c *v1alpha1.FlagChange) string {
	by := c.ChangedBy
	if by == "" {
		by = "a person"
	}
	s := fmt.Sprintf("%s's value was changed in %s by %s", flag, environment, by)
	if c.Value != nil {
		s += " to " + showValue(*c.Value)
	}
	return s
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
	if spec.Requests != nil && d.HealthySince != nil {
		// The requests the pods counted up to here belong to the incident: a
		// later change, such as a restore, is compared with the minute before
		// it from here on, not with the incident's failures.
		r.requestsTracker().Recovered(client.ObjectKeyFromObject(guard), *d.HealthySince)
	}
	addTimeline(st, now, "Recovered", strings.TrimSpace("Verified recovery: "+d.Why+". "+timings(st)))
	if st.Snapshot != nil && (st.Snapshot.Enabled || pendingSegments(st) > 0) {
		addTimeline(st, now, "RestoreAvailable", restoreHint(guard))
	}
	r.Recorder.Eventf(guard, nil, corev1.EventTypeNormal, "Recovered", "Recover", "%s", d.Why)
	metrics.Recoveries.WithLabelValues(guard.Namespace, guard.Name, spec.Flag, st.Environment).Inc()
	if st.FlagDisabledAt != nil {
		metrics.Observe(metrics.RecoverySeconds, now.Sub(st.FlagDisabledAt.Time), spec.Flag, st.Environment)
	}

	return r.reportHealthy(ctx, guard, spec, a, now)
}

// reportHealthy reports Healthy, unless another guard of this flag in this
// environment is still tripped: Feature Health keeps one status per flag and
// environment. Until then the Reported condition stays False, so every check
// tries again.
func (r *FlagGuardReconciler) reportHealthy(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, a workload.Assessment, now time.Time) error {
	others, err := r.otherTrippedGuards(ctx, guard)
	if err != nil {
		return err
	}
	if len(others) > 0 {
		msg := fmt.Sprintf("Not reporting Healthy yet: %s still tripped for this flag", strings.Join(others, ", "))
		if c := meta.FindStatusCondition(guard.Status.Conditions, conditionReported); c == nil || c.Reason != "WaitingForOtherGuards" {
			addTimeline(&guard.Status, now, "HealthyNotReported", msg)
		}
		setCondition(guard, conditionReported, metav1.ConditionFalse, "WaitingForOtherGuards", msg)
		return nil
	}
	r.report(ctx, guard, spec, flagsmith.Healthy, a)
	return nil
}

func (r *FlagGuardReconciler) rearm(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, a workload.Assessment,
	alerts []breaker.Alert, d breaker.Decision, now time.Time) {
	st := &guard.Status
	wasTripped := st.Phase == v1alpha1.PhaseTripped
	if d.Acknowledge {
		if st.LastFlagChange != nil {
			st.LastFlagChange.Acknowledged = true
		}
		// The person dismissed the incident, alerts and all: the ones firing
		// now don't trip the guard again.
		for _, al := range alerts {
			if al.EventID > st.HandledEventID {
				st.HandledEventID = al.EventID
			}
		}
	}
	st.SwitchOffAttemptedAt = nil
	st.Phase = v1alpha1.PhaseArmed
	recoveredAt := st.RecoveredAt
	st.TrippedAt, st.FlagDisabledAt, st.FlagDisabledBy, st.HealthySince, st.RecoveredAt = nil, nil, "", nil, nil
	if recoveredAt != nil && healthyReportPending(st) {
		// The recovery was verified, but Feature Health hasn't heard yet. The
		// report, once it goes out, still says so.
		st.RecoveredAt = recoveredAt
	}
	// The trigger and the alert are cleared last: the Healthy report below
	// still says what the incident was about.
	defer func() { st.Trigger, st.Alert, st.RequestsIncident = nil, nil, false }()
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
	// A person took over before recovery was verified (a dismissal already
	// wrote Healthy itself): clear Flagbreaker's Unhealthy event, saying so.
	if wasTripped && !d.Acknowledge {
		r.reportHealthy(ctx, guard, spec, a, now) //nolint:errcheck // a failure leaves Reported False and is retried
	}
}

// handleRestore applies a new flagbreaker.dev/restore annotation. It turns
// back on what Flagbreaker switched off, but only overrides that still exist
// and are still off: a person's later change always wins.
func (r *FlagGuardReconciler) handleRestore(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, flag flagsmith.Flag, a workload.Assessment, now time.Time) (bool, error) {
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
		// The restore is a flag change like any other: if the workload
		// breaks again, the breaker trips again. If it is failing already
		// (say, still crash-looping after a dismissal), the restore isn't
		// blamed for that. Crashes from before a verified recovery belong to
		// the incident, not to now: Kubernetes keeps a pod's last crash.
		failing := wasFailing(a, now, failingFrom(st, spec, now))

		on := true
		st.FlagEnabled = &on
		st.Phase = v1alpha1.PhaseArmed
		st.TrippedAt, st.FlagDisabledAt, st.FlagDisabledBy, st.HealthySince, st.RecoveredAt = nil, nil, "", nil, nil
		st.Trigger, st.Alert, st.RequestsIncident = nil, nil, false
		meta.RemoveStatusCondition(&st.Conditions, conditionBlocked)
		st.LastFlagChange = &v1alpha1.FlagChange{At: metav1.NewTime(now), Enabled: true, ChangedBy: ChangedByRestore, WorkloadWasFailing: failing}
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
			// Remember the ID of this incident's Unhealthy event, so a
			// dismissal is recognised by a newer ID, whatever the clocks say.
			if e.Unhealthy() && st.Phase == v1alpha1.PhaseTripped && e.ID > st.ReportedEventID {
				st.ReportedEventID = e.ID
			}
		case e.ProviderName == flagsmith.ProviderWebhook && !e.Unhealthy() && strings.HasPrefix(e.FirstText(), flagsmith.DismissedPrefix):
			// Dismissing Flagbreaker's event writes a Healthy event into
			// the Webhook provider's slot.
			if st.Phase != v1alpha1.PhaseTripped {
				break
			}
			if st.ReportedEventID != 0 {
				dismissed = e.ID > st.ReportedEventID
			} else if st.TrippedAt != nil && e.CreatedAt.After(st.TrippedAt.Time) {
				dismissed = true // dismissed before Flagbreaker saw its own event
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
		r.reportHealthy(ctx, guard, spec, a, r.now()) //nolint:errcheck // retried next check
	default:
		// Armed: a Healthy that couldn't be sent, or was waiting for other
		// guards, is still owed; anything else is history.
		if c := meta.FindStatusCondition(guard.Status.Conditions, conditionReported); c != nil &&
			(c.Reason == "HealthyReportFailed" || c.Reason == "WaitingForOtherGuards") {
			r.reportHealthy(ctx, guard, spec, a, r.now()) //nolint:errcheck // retried next check
			return
		}
		meta.RemoveStatusCondition(&guard.Status.Conditions, conditionReported)
	}
}

// report posts the incident to Flagsmith Feature Health, where it shows in
// the flag's Health tab.
func (r *FlagGuardReconciler) report(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec, status flagsmith.HealthStatus, a workload.Assessment) {
	event := buildHealthEvent(guard, status, a, r.links(guard, spec))
	if err := r.Flagsmith.ReportHealth(ctx, event); err != nil {
		logf.FromContext(ctx).Error(err, "reporting to Feature Health failed; will retry")
		reason := "UnhealthyReportFailed"
		if status == flagsmith.Healthy {
			reason = "HealthyReportFailed"
		}
		setCondition(guard, conditionReported, metav1.ConditionFalse, reason, err.Error())
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
		// A guard being deleted reports for itself, or not at all.
		if g.UID != guard.UID && g.DeletionTimestamp.IsZero() &&
			g.Status.FeatureID == guard.Status.FeatureID && g.Status.EnvironmentID == guard.Status.EnvironmentID &&
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
func (r *FlagGuardReconciler) FlagChanged(ctx context.Context, environmentID, featureID int64) {
	var all v1alpha1.FlagGuardList
	if err := r.List(ctx, &all); err != nil {
		return
	}
	logf.FromContext(ctx).Info("Flagsmith webhook: flag changed", "environmentID", environmentID, "featureID", featureID)
	for i := range all.Items {
		g := &all.Items[i]
		if g.Status.FeatureID == featureID && g.Status.EnvironmentID == environmentID {
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
	delete(r.pending, key)
	r.mu.Unlock()
	r.requestsTracker().Forget(key)
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
