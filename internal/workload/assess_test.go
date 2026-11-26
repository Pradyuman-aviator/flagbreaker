package workload

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) metav1.Time { return metav1.NewTime(t0.Add(d)) }

func readyPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: at(-time.Hour)}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func crashLoopingPod(name string, crashedAt time.Duration) corev1.Pod {
	p := readyPod(name)
	p.Status.Conditions[0] = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: at(crashedAt)}
	p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name:         "app",
		RestartCount: 3,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: 2, Reason: "Error", FinishedAt: at(crashedAt),
		}},
	}
	return p
}

func TestHealthyPods(t *testing.T) {
	a := Assess([]corev1.Pod{readyPod("a"), readyPod("b")}, t0, 30*time.Second)
	if a.Pods != 2 || a.Ready != 2 || len(a.Problems) != 0 {
		t.Fatalf("got %+v, want 2 ready pods and no problems", a)
	}
	if !a.HealthySince(t0.Add(-time.Minute), 2) {
		t.Fatal("want healthy")
	}
}

func TestCrashAfterChangeIsFailing(t *testing.T) {
	pods := []corev1.Pod{crashLoopingPod("a", 5*time.Second), readyPod("b")}
	a := Assess(pods, t0.Add(10*time.Second), 30*time.Second)

	if got := a.FailingSince(t0); len(got) != 1 || got[0].Pod != "a" {
		t.Fatalf("FailingSince = %+v, want pod a", got)
	}
	if len(a.CrashLooping) != 1 {
		t.Fatalf("CrashLooping = %v, want [a]", a.CrashLooping)
	}
	if a.HealthySince(t0, 2) {
		t.Fatal("want not healthy")
	}
}

func TestCrashBeforeChangeIsNotBlamedOnIt(t *testing.T) {
	a := Assess([]corev1.Pod{crashLoopingPod("a", -time.Minute)}, t0, 30*time.Second)
	if got := a.FailingSince(t0); len(got) != 0 {
		t.Fatalf("FailingSince = %+v, want none", got)
	}
	if got := a.FailingBetween(t0.Add(-2*time.Minute), t0); len(got) != 1 {
		t.Fatalf("FailingBetween = %+v, want pod a", got)
	}
	// An old crash doesn't count as "failing just before".
	if got := a.FailingBetween(t0.Add(-30*time.Second), t0); len(got) != 0 {
		t.Fatalf("FailingBetween = %+v, want none for the last 30s", got)
	}
}

func TestNotReadyCountsOnlyAfterGrace(t *testing.T) {
	p := readyPod("a")
	p.Status.Conditions[0] = corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: at(0)}

	if a := Assess([]corev1.Pod{p}, t0.Add(10*time.Second), 30*time.Second); len(a.Problems) != 0 {
		t.Fatalf("within grace: problems = %+v, want none", a.Problems)
	}
	if a := Assess([]corev1.Pod{p}, t0.Add(31*time.Second), 30*time.Second); len(a.Problems) != 1 {
		t.Fatalf("after grace: problems = %+v, want one", a.Problems)
	}
}

func TestDeletingPodsAreIgnored(t *testing.T) {
	p := crashLoopingPod("a", time.Second)
	now := metav1.NewTime(t0)
	p.DeletionTimestamp = &now
	if a := Assess([]corev1.Pod{p}, t0, time.Second); a.Pods != 0 || len(a.Problems) != 0 {
		t.Fatalf("got %+v, want the deleting pod ignored", a)
	}
}

func TestOOMKilledIsACrash(t *testing.T) {
	p := readyPod("a")
	p.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		ExitCode: 0, Reason: "OOMKilled", FinishedAt: at(time.Second),
	}
	if got := Assess([]corev1.Pod{p}, t0.Add(2*time.Second), time.Minute).FailingSince(t0); len(got) != 1 {
		t.Fatalf("FailingSince = %+v, want the OOM kill", got)
	}
}
