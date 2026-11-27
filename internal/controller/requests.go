package controller

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
)

// requestsTracker returns the tracker, made with the default client on
// first use.
func (r *FlagGuardReconciler) requestsTracker() *requests.Tracker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Requests == nil {
		r.Requests = requests.NewTracker(nil)
	}
	return r.Requests
}

// readRequests reads, for a guard with spec.requests, the request counter of
// the workload's running pods, at most once every few seconds, and records
// the last minute in status.requests and the RequestsRead condition. It
// returns the guard's history, where its last verified recovery began, and
// whether the guard reads requests at all. A guard without spec.requests
// reads nothing; what an earlier spec left in its status is cleared.
func (r *FlagGuardReconciler) readRequests(ctx context.Context, guard *v1alpha1.FlagGuard, spec v1alpha1.FlagGuardSpec,
	t target, pods []corev1.Pod, now time.Time) (requests.Series, time.Time, bool) {
	st := &guard.Status
	key := client.ObjectKeyFromObject(guard)
	if spec.Requests == nil {
		r.requestsTracker().Forget(key)
		st.Requests = nil
		meta.RemoveStatusCondition(&st.Conditions, conditionRequests)
		return nil, time.Time{}, false
	}
	rq := spec.Requests
	cfg := requests.Config{Port: rq.Port, Path: rq.Path, Metric: rq.Metric, StatusLabel: rq.StatusLabel}
	res := r.requestsTracker().Read(ctx, key, guard.UID, cfg, t.kind+"/"+t.obj.GetName(), requests.Targets(pods), now, requestsHistory(spec))
	if res.Skipped {
		return res.Series, res.NotBefore, true
	}
	switch {
	case res.Read > 0:
		c := res.Series.Between(now.Add(-time.Minute), now)
		st.Requests = &v1alpha1.RequestsStatus{At: metav1.NewTime(now), PodsRead: int32(res.Read),
			Total: int64(math.Round(c.Total)), Failed: int64(math.Round(c.Failed))}
		if c.Total > 0 {
			st.Requests.ErrorRate = fmt.Sprintf("%.1f%%", c.Percent())
		}
		setCondition(guard, conditionRequests, metav1.ConditionTrue, "Read", fmt.Sprintf("Read %s from %d of %d pods", rq.Metric, res.Read, res.Of))
	case res.Of == 0:
		setCondition(guard, conditionRequests, metav1.ConditionFalse, "NoPodsToRead", "No running pod with an IP")
	case res.Err != nil:
		// No pod answered. The last counts stay in status.requests, and the
		// requests rules have nothing new to judge.
		setCondition(guard, conditionRequests, metav1.ConditionFalse, "ReadFailed", res.Err.Error())
	}
	return res.Series, res.NotBefore, true
}

// requestsHistory is how long a guard's readings are kept: the window for
// blaming a change and the minute before it, or stableFor for recovery,
// whichever is longer, with some to spare.
func requestsHistory(spec v1alpha1.FlagGuardSpec) time.Duration {
	keep := spec.Window.Duration
	if spec.StableFor.Duration > keep {
		keep = spec.StableFor.Duration
	}
	return keep + 2*time.Minute
}
