package requests

// Regression tests for what the review of request checking found.

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestReadingsAreStampedToTheSecondAsStatusTimesAre(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	read(tr, 700*time.Millisecond, pod(1))
	f.set(1, 40, 0)
	r := read(tr, 5700*time.Millisecond, pod(1))
	if len(r.Series) != 2 || !r.Series[0].At.Equal(t0) || !r.Series[1].At.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("want the samples at whole seconds: %+v", r.Series)
	}
	// A change seen at the next check happened after the 5.7s read. The guard
	// keeps its time as 5.7s until the status is written, and as 5s after:
	// the read must stay out of the time after the change either way.
	for _, change := range []time.Time{t0.Add(5700 * time.Millisecond), t0.Add(5 * time.Second)} {
		if c := r.Series.Between(change, t0.Add(10*time.Second)); c != (Count{}) {
			t.Fatalf("after a change at %s: the read before it counted: %+v", change.Format(time.StampMilli), c)
		}
	}
}

func TestARecoverySaysWhereTheMinuteBeforeAChangeMayStart(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	if r := read(tr, 0, pod(1)); !r.NotBefore.IsZero() {
		t.Fatalf("no recovery yet: %+v", r)
	}
	tr.Recovered(guard, t0.Add(2*time.Second))
	tr.Recovered(guard, t0.Add(time.Second)) // an older one changes nothing
	if r := read(tr, 2*time.Second, pod(1)); !r.Skipped || !r.NotBefore.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("a skipped read says where the recovery began too: %+v", r)
	}
	if r := read(tr, 5*time.Second, pod(1)); r.Skipped || !r.NotBefore.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("want NotBefore where the recovery began: %+v", r)
	}
	other := cfg
	other.Port = 9090
	if r := tr.Read(context.Background(), guard, "uid-1", other, "Deployment/checkout", []Target{pod(1)}, t0.Add(10*time.Second), time.Hour); !r.NotBefore.IsZero() {
		t.Fatalf("a history that starts over has no recovery in it: %+v", r)
	}
	unread := types.NamespacedName{Namespace: "shop", Name: "never-read"}
	tr.Recovered(unread, t0)
	if _, ok := tr.guards[unread]; ok {
		t.Fatal("a guard with no history must not get one from Recovered")
	}
}
