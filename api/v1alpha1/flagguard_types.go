package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Mode says whether Flagbreaker may change flags.
// +kubebuilder:validation:Enum=watch;act
type Mode string

const (
	// ModeWatch only reports breakages to Flagsmith Feature Health.
	ModeWatch Mode = "watch"
	// ModeAct also switches the flag off.
	ModeAct Mode = "act"
)

// Phase is where a FlagGuard is in its cycle.
type Phase string

const (
	// PhaseArmed means the guard is watching and the workload is fine.
	PhaseArmed Phase = "Armed"
	// PhaseTripped means the workload broke after the flag changed.
	PhaseTripped Phase = "Tripped"
	// PhaseRecovered means the flag is off and the workload was verified
	// healthy. The guard waits for a person to turn the flag back on.
	PhaseRecovered Phase = "Recovered"
)

// FlagGuardSpec says which flag a workload depends on and when to trip.
type FlagGuardSpec struct {
	// Flag is the name of the Flagsmith feature the workload depends on.
	// +kubebuilder:validation:MinLength=1
	Flag string `json:"flag"`

	// EnvironmentKey is the client-side key of the Flagsmith environment the
	// workload reads its flags from. Flagbreaker looks up the environment's
	// name and project from it.
	// +kubebuilder:validation:MinLength=1
	EnvironmentKey string `json:"environmentKey"`

	// Workload is the Deployment, StatefulSet or DaemonSet, in this
	// namespace, that depends on the flag.
	Workload WorkloadReference `json:"workload"`

	// Mode is this guard's say in the mode: watch reports to Feature Health
	// only; act also switches the flag off. Flagbreaker also asks
	// flagbreaker_policy and flagbreaker_mode in Flagsmith, and the most
	// cautious answer wins: it acts only when something says act and nothing
	// says watch.
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// Window is how long after a flag change a breakage is blamed on it.
	// +kubebuilder:default="10m"
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`

	// FailingPods is how many pods must be failing to trip the breaker.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailingPods int32 `json:"failingPods,omitempty"`

	// StableFor is how long every pod must stay ready, with no new crashes,
	// before the workload counts as recovered.
	// +kubebuilder:default="30s"
	// +optional
	StableFor *metav1.Duration `json:"stableFor,omitempty"`

	// RestartCrashingPods deletes, once the flag is off, the pods that crashed
	// after the flag change or are stuck in CrashLoopBackOff. They restart now
	// instead of after the kubelet's back-off, which can reach five minutes.
	// This also happens when a person switches the flag off during an
	// incident in watch mode.
	// +kubebuilder:default=true
	// +optional
	RestartCrashingPods *bool `json:"restartCrashingPods,omitempty"`

	// FollowHealthEvents trips the breaker on Unhealthy events that other
	// Feature Health providers report for this flag in this environment, such
	// as a Grafana or Prometheus Alertmanager alert labelled with the flag.
	// Recovery then also waits until those alerts are resolved or dismissed.
	// +kubebuilder:default=true
	// +optional
	FollowHealthEvents *bool `json:"followHealthEvents,omitempty"`

	// Links are added to every Feature Health event, so the flag's Health tab
	// in Flagsmith links to the runbook, dashboards or the workload.
	// +optional
	// +kubebuilder:validation:MaxItems=5
	Links []Link `json:"links,omitempty"`
}

// Link is a titled URL.
type Link struct {
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`
}

// WorkloadReference names the workload a FlagGuard protects.
type WorkloadReference struct {
	// Kind of the workload.
	// +kubebuilder:validation:Enum=Deployment;StatefulSet;DaemonSet
	// +kubebuilder:default=Deployment
	// +optional
	Kind string `json:"kind,omitempty"`

	// Name of the workload.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AllowedEnvironmentsAnnotation goes on a Namespace. It lists, comma
// separated, the client-side keys of the Flagsmith environments that
// FlagGuards in that namespace may act on, or "*" for any. Flagbreaker acts
// with an organisation API key, so without this a user who can create a
// FlagGuard anywhere could switch off any flag; namespaces are cluster-scoped,
// so only a cluster admin can grant it. The controller's
// --require-namespace-opt-in=false turns the check off.
const AllowedEnvironmentsAnnotation = "flagbreaker.dev/allowed-environments"

// RestoreAnnotation asks Flagbreaker to undo its switch-off: set it on a
// FlagGuard (any new value, such as a timestamp) once the cause is fixed.
// Flagbreaker turns the environment default and the segment overrides it
// switched off back on, then watches the flag again, so a second breakage
// trips the breaker again.
const RestoreAnnotation = "flagbreaker.dev/restore"

// FlagValue is a flag's remote config value, as Flagsmith types it. An empty
// Type means the flag has no value.
type FlagValue struct {
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Value string `json:"value,omitempty"`
}

// FlagChange records a change to the flag's environment default: switching
// it on or off, or changing its value.
type FlagChange struct {
	// At is when the change happened, from Flagsmith's webhook. Without one,
	// it is the check before the change was seen: the earliest it can have
	// happened.
	At metav1.Time `json:"at"`
	// Enabled is whether the flag is on after the change.
	Enabled bool `json:"enabled"`
	// ValueChanged is true when the change set a new value and didn't switch
	// the flag on or off. Flagbreaker blames a breakage on it like on a
	// switch-on, and in act mode puts PreviousValue back.
	// +optional
	ValueChanged bool `json:"valueChanged,omitempty"`
	// Value is the value the change set, for a value change.
	// +optional
	Value *FlagValue `json:"value,omitempty"`
	// PreviousValue is the value before the change, for a value change.
	// +optional
	PreviousValue *FlagValue `json:"previousValue,omitempty"`
	// ChangedBy is who made the change, when Flagsmith says so.
	// +optional
	ChangedBy string `json:"changedBy,omitempty"`
	// WorkloadWasFailing is true when the workload was already failing
	// before the change. Flagbreaker never blames a flag for a breakage that
	// came first.
	// +optional
	WorkloadWasFailing bool `json:"workloadWasFailing,omitempty"`
	// Acknowledged is true when a person dismissed the incident this change
	// caused, in Flagsmith's Health tab. The change is not blamed again.
	// +optional
	Acknowledged bool `json:"acknowledged,omitempty"`
	// Tripped is true once this change has caused an incident. A change is
	// blamed for one incident only.
	// +optional
	Tripped bool `json:"tripped,omitempty"`
	// TimeUnknown is true when Flagbreaker wasn't running when the flag
	// changed, so it can't tell when. Such a change is not blamed unless a
	// Flagsmith webhook later says exactly when it happened.
	// +optional
	TimeUnknown bool `json:"timeUnknown,omitempty"`
}

// FlagSnapshot is what the flag served before Flagbreaker switched it off.
type FlagSnapshot struct {
	// Enabled is the environment default.
	Enabled bool `json:"enabled"`
	// EnabledSegmentOverrides lists, by segment ID, the segment overrides
	// that were on and that Flagbreaker switched off.
	// +optional
	EnabledSegmentOverrides []int64 `json:"enabledSegmentOverrides,omitempty"`
}

// HealthAlert is an Unhealthy event another Feature Health provider reported.
type HealthAlert struct {
	// EventID is Flagsmith's ID for the event.
	EventID int64 `json:"eventID"`
	// Provider is the Feature Health provider, such as Grafana.
	Provider string `json:"provider"`
	// Title is the alert's name, from the event's first text block.
	// +optional
	Title string `json:"title,omitempty"`
	// At is when the provider says the alert started.
	At metav1.Time `json:"at"`
}

// TimelineEntry is one step of an incident.
type TimelineEntry struct {
	Time metav1.Time `json:"time"`
	// Event is a short name, such as FlagChanged, Tripped or Recovered.
	Event string `json:"event"`
	// Message says what happened.
	Message string `json:"message"`
}

// FlagGuardStatus is what Flagbreaker has observed and done.
type FlagGuardStatus struct {
	// Phase is Armed, Tripped or Recovered.
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// ObservedGeneration is the spec generation the status is for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Mode is the mode in effect at the last check.
	// +optional
	Mode Mode `json:"mode,omitempty"`

	// ModeReason says which setting decided the mode.
	// +optional
	ModeReason string `json:"modeReason,omitempty"`

	// Environment is the Flagsmith environment's name.
	// +optional
	Environment string `json:"environment,omitempty"`

	// EnvironmentID is the Flagsmith environment's ID.
	// +optional
	EnvironmentID int64 `json:"environmentID,omitempty"`

	// ProjectID is the Flagsmith project's ID.
	// +optional
	ProjectID int64 `json:"projectID,omitempty"`

	// FeatureID is the Flagsmith feature's ID.
	// +optional
	FeatureID int64 `json:"featureID,omitempty"`

	// FlagEnabled is what the flag's environment default served at the last
	// check.
	// +optional
	FlagEnabled *bool `json:"flagEnabled,omitempty"`

	// FlagValue is the environment default's value at the last check.
	// +optional
	FlagValue *FlagValue `json:"flagValue,omitempty"`

	// LastFlagChange is the latest change Flagbreaker saw.
	// +optional
	LastFlagChange *FlagChange `json:"lastFlagChange,omitempty"`

	// Trigger is the flag change that caused the current incident.
	// +optional
	Trigger *FlagChange `json:"trigger,omitempty"`

	// Alert is the other provider's Feature Health alert that caused the
	// current incident, when an alert rather than the pods tripped it.
	// +optional
	Alert *HealthAlert `json:"alert,omitempty"`

	// HandledEventID is the newest Feature Health event from another
	// provider that Flagbreaker has acted on. Older events never trip it
	// again.
	// +optional
	HandledEventID int64 `json:"handledEventID,omitempty"`

	// ReportedEventID is the ID of the Unhealthy event Flagbreaker reported
	// for the current incident. A dismissal is any newer event in its slot.
	// +optional
	ReportedEventID int64 `json:"reportedEventID,omitempty"`

	// TrippedAt is when the breaker tripped.
	// +optional
	TrippedAt *metav1.Time `json:"trippedAt,omitempty"`

	// FlagDisabledAt is when the flag was switched off during the current
	// incident, by Flagbreaker or by a person. For an incident caused by a
	// value change, it is when the value was changed again: put back by
	// Flagbreaker, or changed by a person.
	// +optional
	FlagDisabledAt *metav1.Time `json:"flagDisabledAt,omitempty"`

	// FlagDisabledBy is who switched the flag off during the current
	// incident.
	// +optional
	FlagDisabledBy string `json:"flagDisabledBy,omitempty"`

	// SwitchOffAttemptedAt is set while a switch-off Flagbreaker sent hasn't
	// been confirmed, for example after a timeout. If the flag then turns out
	// to be off, it was Flagbreaker's switch-off, not a person's.
	// +optional
	SwitchOffAttemptedAt *metav1.Time `json:"switchOffAttemptedAt,omitempty"`

	// HealthySince is when every pod was last seen ready with no new crashes.
	// +optional
	HealthySince *metav1.Time `json:"healthySince,omitempty"`

	// RecoveredAt is when the workload was verified healthy again.
	// +optional
	RecoveredAt *metav1.Time `json:"recoveredAt,omitempty"`

	// Snapshot is what the flag served before Flagbreaker switched it off. It
	// is kept until the flag is restored, so the restore annotation can turn
	// the segment overrides back on even after a person re-enabled the
	// environment default by hand.
	// +optional
	Snapshot *FlagSnapshot `json:"snapshot,omitempty"`

	// RestoreRequest is the last value of the flagbreaker.dev/restore
	// annotation that Flagbreaker handled.
	// +optional
	RestoreRequest string `json:"restoreRequest,omitempty"`

	// Timeline is the current incident, oldest first, at most 20 entries.
	// +optional
	Timeline []TimelineEntry `json:"timeline,omitempty"`

	// Conditions report problems such as an unreachable Flagsmith.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FlagGuard ties a Flagsmith flag to the workload that depends on it. When the
// workload breaks right after the flag changes, Flagbreaker reports it to
// Flagsmith Feature Health and, in act mode, switches the flag off.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fg
// +kubebuilder:printcolumn:name="Flag",type=string,JSONPath=`.spec.flag`
// +kubebuilder:printcolumn:name="On",type=boolean,JSONPath=`.status.flagEnabled`
// +kubebuilder:printcolumn:name="Environment",type=string,JSONPath=`.status.environment`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.workload.kind`,priority=1
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.workload.name`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.status.mode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type FlagGuard struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FlagGuardSpec   `json:"spec,omitempty"`
	Status FlagGuardStatus `json:"status,omitempty"`
}

// FlagGuardList is a list of FlagGuards.
//
// +kubebuilder:object:root=true
type FlagGuardList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FlagGuard `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FlagGuard{}, &FlagGuardList{})
}
