// Package workload decides, from a workload's pods, whether it is failing and
// since when. It only reads pod status, so it is a pure function that is easy
// to test.
package workload

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Problem is one pod failing, with the time the failure was seen.
type Problem struct {
	Pod    string
	Reason string
	At     time.Time
}

// Assessment is the state of a workload's pods at one moment.
type Assessment struct {
	// Pods counts the pods that are not being deleted.
	Pods int
	// Ready counts the pods whose Ready condition is True.
	Ready int
	// Problems lists each failing pod once, with its latest failure.
	Problems []Problem
	// CrashLooping lists pods with a container waiting in CrashLoopBackOff.
	CrashLooping []string
}

// Assess looks at the pods of one workload. A pod has a problem when a
// container exited with an error (or was OOM-killed), or when the pod has been
// not ready for at least notReadyGrace.
func Assess(pods []corev1.Pod, now time.Time, notReadyGrace time.Duration) Assessment {
	var a Assessment
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		a.Pods++

		var latest *Problem
		keep := func(p Problem) {
			if latest == nil || p.At.After(latest.At) {
				latest = &p
			}
		}

		for _, cs := range pod.Status.ContainerStatuses {
			if t := cs.LastTerminationState.Terminated; t != nil && isCrash(t) {
				keep(Problem{Pod: pod.Name, Reason: crashReason(cs.Name, t), At: t.FinishedAt.Time})
			}
			if t := cs.State.Terminated; t != nil && isCrash(t) {
				keep(Problem{Pod: pod.Name, Reason: crashReason(cs.Name, t), At: t.FinishedAt.Time})
			}
			if w := cs.State.Waiting; w != nil && w.Reason == "CrashLoopBackOff" {
				a.CrashLooping = append(a.CrashLooping, pod.Name)
			}
		}

		ready := podReady(pod)
		if ready != nil && ready.Status == corev1.ConditionTrue {
			a.Ready++
		} else if ready != nil && now.Sub(ready.LastTransitionTime.Time) >= notReadyGrace {
			// Only a crash explains more than "not ready", so prefer it.
			if latest == nil {
				keep(Problem{
					Pod:    pod.Name,
					Reason: fmt.Sprintf("not ready since %s", ready.LastTransitionTime.UTC().Format(time.TimeOnly)),
					At:     ready.LastTransitionTime.Time,
				})
			}
		}

		if latest != nil {
			a.Problems = append(a.Problems, *latest)
		}
	}
	sort.Slice(a.Problems, func(i, j int) bool { return a.Problems[i].At.Before(a.Problems[j].At) })
	sort.Strings(a.CrashLooping)
	return a
}

// FailingSince returns the problems seen at or after t.
func (a Assessment) FailingSince(t time.Time) []Problem {
	var out []Problem
	for _, p := range a.Problems {
		if !p.At.Before(t) {
			out = append(out, p)
		}
	}
	return out
}

// FailingBetween returns the problems seen at or after from and before to.
// Kubernetes keeps a container's last crash until it crashes again, so a pod
// that crashed an hour ago and has run fine since still shows that crash;
// callers pick a short window to ask "was it failing just before?".
func (a Assessment) FailingBetween(from, to time.Time) []Problem {
	var out []Problem
	for _, p := range a.Problems {
		if !p.At.Before(from) && p.At.Before(to) {
			out = append(out, p)
		}
	}
	return out
}

// HealthySince reports whether the workload is fully up: at least `desired`
// pods ready, every pod ready, and no crash at or after t. A workload scaled
// to zero with no pods left counts as healthy: nothing is failing.
func (a Assessment) HealthySince(t time.Time, desired int32) bool {
	if a.Ready != a.Pods || len(a.FailingSince(t)) > 0 {
		return false
	}
	if desired <= 0 {
		return true
	}
	return a.Pods > 0 && a.Ready >= int(desired)
}

func isCrash(t *corev1.ContainerStateTerminated) bool {
	return t.ExitCode != 0 || t.Reason == "OOMKilled"
}

func crashReason(container string, t *corev1.ContainerStateTerminated) string {
	reason := t.Reason
	if reason == "" {
		reason = "Error"
	}
	return fmt.Sprintf("container %s exited: %s (exit code %d)", container, reason, t.ExitCode)
}

func podReady(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == corev1.PodReady {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}
