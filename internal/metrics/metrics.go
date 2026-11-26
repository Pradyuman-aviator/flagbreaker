// Package metrics exports Flagbreaker's Prometheus metrics on the
// controller's metrics endpoint (:8080/metrics by default), next to
// controller-runtime's own.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// Trips counts trips by guard, flag, environment, mode and cause (pods or
	// alert).
	Trips = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_trips_total",
		Help: "Times a FlagGuard tripped.",
	}, []string{"namespace", "guard", "flag", "environment", "mode", "cause"})

	// SwitchOffs counts flags switched off during incidents, by who did it.
	SwitchOffs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_switch_offs_total",
		Help: "Flags switched off during an incident, by Flagbreaker or a person.",
	}, []string{"namespace", "guard", "flag", "environment", "by"})

	// ValueReverts counts flags whose previous value was put back during an
	// incident a value change caused, by who did it.
	ValueReverts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_value_reverts_total",
		Help: "Flag values changed back during an incident a value change caused, by Flagbreaker or a person.",
	}, []string{"namespace", "guard", "flag", "environment", "by"})

	// Recoveries counts verified recoveries.
	Recoveries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_recoveries_total",
		Help: "Incidents that ended with a verified recovery.",
	}, []string{"namespace", "guard", "flag", "environment"})

	// Restores counts flags restored with the flagbreaker.dev/restore annotation.
	Restores = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_restores_total",
		Help: "Flags restored with the flagbreaker.dev/restore annotation.",
	}, []string{"namespace", "guard", "flag", "environment"})

	// Errors counts failed calls to Flagsmith and Kubernetes during checks.
	Errors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "flagbreaker_check_errors_total",
		Help: "FlagGuard checks that failed, for example because Flagsmith was unreachable.",
	}, []string{"namespace", "guard"})

	buckets = []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600}

	// DetectionSeconds measures the time from the flag change to the trip.
	DetectionSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "flagbreaker_detection_seconds",
		Help:    "Seconds from the flag change to the trip.",
		Buckets: buckets,
	}, []string{"flag", "environment"})

	// SwitchOffSeconds measures the time from the trip to the switch-off.
	SwitchOffSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "flagbreaker_switch_off_seconds",
		Help:    "Seconds from the trip to the flag being switched off, or its previous value put back.",
		Buckets: buckets,
	}, []string{"flag", "environment"})

	// RecoverySeconds measures the time from the switch-off to verified
	// recovery.
	RecoverySeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "flagbreaker_recovery_seconds",
		Help:    "Seconds from the switch-off to verified recovery.",
		Buckets: buckets,
	}, []string{"flag", "environment"})

	// Phase is 1 for each guard's current phase and 0 for the others.
	Phase = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "flagbreaker_guard_phase",
		Help: "1 for the phase a FlagGuard is in (Armed, Tripped or Recovered).",
	}, []string{"namespace", "guard", "flag", "phase"})
)

func init() {
	crmetrics.Registry.MustRegister(Trips, SwitchOffs, ValueReverts, Recoveries, Restores, Errors,
		DetectionSeconds, SwitchOffSeconds, RecoverySeconds, Phase)
}

// Observe records d in h, ignoring negative durations from clock skew.
func Observe(h *prometheus.HistogramVec, d time.Duration, labels ...string) {
	if d < 0 {
		d = 0
	}
	h.WithLabelValues(labels...).Observe(d.Seconds())
}

// SetPhase marks the guard's current phase.
func SetPhase(namespace, guard, flag, phase string) {
	for _, p := range []string{"Armed", "Tripped", "Recovered"} {
		v := 0.0
		if p == phase {
			v = 1
		}
		Phase.WithLabelValues(namespace, guard, flag, p).Set(v)
	}
}

// Forget drops a deleted guard's series.
func Forget(namespace, guard string) {
	labels := prometheus.Labels{"namespace": namespace, "guard": guard}
	Phase.DeletePartialMatch(labels)
	Trips.DeletePartialMatch(labels)
	SwitchOffs.DeletePartialMatch(labels)
	ValueReverts.DeletePartialMatch(labels)
	Recoveries.DeletePartialMatch(labels)
	Restores.DeletePartialMatch(labels)
	Errors.DeletePartialMatch(labels)
}
