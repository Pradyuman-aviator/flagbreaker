package requests

// Regression tests for what the review of request checking found.

import (
	"testing"
	"time"
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
