package requests

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var t0 = time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC)

var cfg = Config{Port: 8080, Path: "/metrics", Metric: "http_requests_total", StatusLabel: "code"}

var guard = types.NamespacedName{Namespace: "shop", Name: "checkout-new-payments"}

func parse(t *testing.T, body, metric, label string) Count {
	t.Helper()
	c, err := Parse(strings.NewReader(body), metric, label)
	if err != nil {
		t.Fatalf("Parse(%s): %v", metric, err)
	}
	return c
}

func TestParseSumsTheCounterAndCountsOnly5xxAsFailed(t *testing.T) {
	body := `# HELP go_goroutines Number of goroutines.
# TYPE go_goroutines gauge
go_goroutines 12
# HELP http_requests_total Requests handled, by status code.
# TYPE http_requests_total counter
http_requests_total{code="200",method="get"} 100
http_requests_total{code="404",method="get"} 7
# a comment in the middle
http_requests_total{code="500",method="post"} 3
http_requests_total{code="503",method="get"} 2 1764248400000
http_requests_total_created{code="200"} 1.7e9
process_cpu_seconds_total 0.5
`
	if c := parse(t, body, "http_requests_total", "code"); c != (Count{Total: 112, Failed: 5}) {
		t.Fatalf("got %+v, want 112 requests with 5 failed (4xx is handled, not failed)", c)
	}
}

func TestParseReadsAHistogramOrSummaryCountByEitherName(t *testing.T) {
	body := `# TYPE http_server_requests_seconds histogram
http_server_requests_seconds_bucket{status="200",le="0.1"} 5
http_server_requests_seconds_bucket{status="200",le="+Inf"} 7
http_server_requests_seconds_sum{status="200"} 0.5
http_server_requests_seconds_count{status="200"} 7
http_server_requests_seconds_bucket{status="503",le="+Inf"} 2
http_server_requests_seconds_sum{status="503"} 0.1
http_server_requests_seconds_count{status="503"} 2
# TYPE rpc_seconds summary
rpc_seconds{status="500",quantile="0.5"} 0.1
rpc_seconds_sum{status="500"} 1
rpc_seconds_count{status="500"} 4
rpc_seconds_count{status="200"} 6
`
	for _, c := range []struct {
		metric string
		want   Count
	}{
		{"http_server_requests_seconds_count", Count{Total: 9, Failed: 2}},
		{"http_server_requests_seconds", Count{Total: 9, Failed: 2}},
		{"rpc_seconds_count", Count{Total: 10, Failed: 4}},
		{"rpc_seconds", Count{Total: 10, Failed: 4}},
	} {
		if got := parse(t, body, c.metric, "status"); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.metric, got, c.want)
		}
	}
}

func TestParseIgnoresMalformedLinesOfOtherMetrics(t *testing.T) {
	body := `broken line with no value
go_goroutines{oops 3
# TYPE http_requests_total counter
http_requests_total{code="200"} 40
http_requests_total{code="502"} 1
`
	if c := parse(t, body, "http_requests_total", "code"); c != (Count{Total: 41, Failed: 1}) {
		t.Fatalf("got %+v", c)
	}
	// OpenMetrics text reads the same: its samples come back untyped, and
	// # EOF and _created are dropped.
	om := "# TYPE http_requests counter\nhttp_requests_total{code=\"200\"} 5\nhttp_requests_total{code=\"500\"} 1\nhttp_requests_created{code=\"200\"} 1.7e9\n# EOF\n"
	if c := parse(t, om, "http_requests_total", "code"); c != (Count{Total: 6, Failed: 1}) {
		t.Fatalf("OpenMetrics: got %+v", c)
	}
}

func TestParseRefusesWhatItCannotCount(t *testing.T) {
	for _, c := range []struct {
		name, body, want string
	}{
		{"missing metric", "# TYPE other_total counter\nother_total{code=\"200\"} 4\n", "no http_requests_total metric"},
		{"a gauge", "# TYPE http_requests_total gauge\nhttp_requests_total{code=\"200\"} 4\n", "is a gauge, not a counter"},
		{"no series has the label", "http_requests_total{status=\"200\"} 4\n", "no http_requests_total series has a code label"},
		{"a malformed line of the metric", "http_requests_total{code=\"200\"} four\n", "reading http_requests_total"},
		{"a negative value", "http_requests_total{code=\"200\"} -4\n", "not a count"},
		{"NaN", "http_requests_total{code=\"200\"} NaN\n", "not a count"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.body), "http_requests_total", "code")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

// fakePods answers for pods by address, from counters a test sets.
type fakePods struct {
	mu     sync.Mutex
	counts map[string]Count
	down   map[string]bool
	calls  int
}

func newFakePods() *fakePods {
	return &fakePods{counts: map[string]Count{}, down: map[string]bool{}}
}

func (f *fakePods) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	c, ok := f.counts[req.URL.Host]
	if !ok || f.down[req.URL.Host] {
		return nil, errors.New("connection refused")
	}
	body := fmt.Sprintf("# TYPE http_requests_total counter\nhttp_requests_total{code=\"200\"} %g\nhttp_requests_total{code=\"500\"} %g\n", c.Total-c.Failed, c.Failed)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain; version=0.0.4"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

func (f *fakePods) set(pod int, total, failed float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[fmt.Sprintf("10.0.0.%d:8080", pod)] = Count{Total: total, Failed: failed}
}

func (f *fakePods) setDown(pod int, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[fmt.Sprintf("10.0.0.%d:8080", pod)] = down
}

func pod(n int) Target {
	return Target{Name: fmt.Sprintf("checkout-%d", n), IP: fmt.Sprintf("10.0.0.%d", n)}
}

func newTracker() (*Tracker, *fakePods) {
	f := newFakePods()
	return NewTracker(&http.Client{Transport: f}), f
}

func read(tr *Tracker, at time.Duration, targets ...Target) Result {
	return tr.Read(context.Background(), guard, "uid-1", cfg, "Deployment/checkout", targets, t0.Add(at), time.Hour)
}

func last(t *testing.T, r Result) Count {
	t.Helper()
	if len(r.Series) == 0 {
		t.Fatalf("no samples: %+v", r)
	}
	return r.Series[len(r.Series)-1].Count
}

func TestTheFirstReadIsABaselineAndLaterReadsGiveTheSummedDeltas(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 100, 1)
	f.set(2, 50, 0)
	r := read(tr, 0, pod(1), pod(2))
	if r.Read != 2 || r.Of != 2 || r.Err != nil || len(r.Series) != 1 || last(t, r) != (Count{}) {
		t.Fatalf("the first read only sets where counting starts: %+v", r)
	}
	f.set(1, 130, 3)
	f.set(2, 60, 2)
	r = read(tr, 5*time.Second, pod(1), pod(2))
	if len(r.Series) != 2 || !r.Series[1].At.Equal(t0.Add(5*time.Second)) || last(t, r) != (Count{Total: 40, Failed: 4}) {
		t.Fatalf("want one sample of 40 requests with 4 failed, got %+v", r.Series)
	}
	r = read(tr, 10*time.Second, pod(1), pod(2))
	if len(r.Series) != 3 || last(t, r) != (Count{}) {
		t.Fatalf("no new requests is a sample of zero: %+v", r.Series)
	}
}

func TestACounterThatGoesDownCountsFromZero(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 100, 10)
	read(tr, 0, pod(1))
	f.set(1, 7, 1) // the container restarted
	if c := last(t, read(tr, 5*time.Second, pod(1))); c != (Count{Total: 7, Failed: 1}) {
		t.Fatalf("got %+v, want the new counter itself", c)
	}
	f.set(1, 20, 0) // failed went down: a restart too
	if c := last(t, read(tr, 10*time.Second, pod(1))); c != (Count{Total: 20}) {
		t.Fatalf("got %+v", c)
	}
}

func TestANewPodStartsWithABaselineAndAGonePodIsForgotten(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	f.set(2, 500, 50)
	read(tr, 0, pod(1))
	f.set(1, 20, 0)
	if c := last(t, read(tr, 5*time.Second, pod(1), pod(2))); c != (Count{Total: 10}) {
		t.Fatalf("a new pod's counter so far isn't new requests: got %+v", c)
	}
	f.set(2, 510, 51)
	if c := last(t, read(tr, 10*time.Second, pod(1), pod(2))); c != (Count{Total: 10, Failed: 1}) {
		t.Fatalf("got %+v", c)
	}
	read(tr, 15*time.Second, pod(1)) // pod 2 is gone
	f.set(2, 600, 60)
	if c := last(t, read(tr, 20*time.Second, pod(1), pod(2))); c != (Count{}) {
		t.Fatalf("a pod that was gone starts over with a baseline: got %+v", c)
	}
}

func TestAPodThatDoesNotAnswerKeepsItsLastReading(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	f.set(2, 10, 0)
	read(tr, 0, pod(1), pod(2))
	f.setDown(2, true)
	f.set(1, 15, 0)
	f.set(2, 30, 5)
	r := read(tr, 5*time.Second, pod(1), pod(2))
	if r.Read != 1 || r.Of != 2 || r.Err == nil || !strings.Contains(r.Err.Error(), "checkout-2") || last(t, r) != (Count{Total: 5}) {
		t.Fatalf("pod 2 didn't answer: %+v", r)
	}
	f.setDown(2, false)
	if c := last(t, read(tr, 10*time.Second, pod(1), pod(2))); c != (Count{Total: 20, Failed: 5}) {
		t.Fatalf("pod 2's next answer counts from its last reading: got %+v", c)
	}
}

func TestNoAnswerAtAllAddsNoSample(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	read(tr, 0, pod(1))
	f.setDown(1, true)
	r := read(tr, 5*time.Second, pod(1))
	if r.Read != 0 || r.Err == nil || len(r.Series) != 1 {
		t.Fatalf("no answer must add nothing: %+v", r)
	}
	if r := read(tr, 10*time.Second); r.Of != 0 || len(r.Series) != 1 {
		t.Fatalf("no pods to read must add nothing: %+v", r)
	}
}

func TestReadsHappenAtMostEvery4s(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	read(tr, 0, pod(1))
	calls := f.calls
	if r := read(tr, 3*time.Second, pod(1)); !r.Skipped || f.calls != calls || len(r.Series) != 1 {
		t.Fatalf("a read 3s after the last must be skipped: %+v, %d calls", r, f.calls-calls)
	}
	if r := read(tr, 4*time.Second, pod(1)); r.Skipped || f.calls != calls+1 {
		t.Fatalf("a read 4s after the last must happen: %+v", r)
	}
}

func TestTheHistoryStartsOverWhenTheGuardItsConfigOrItsWorkloadChanges(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 10, 0)
	at := time.Duration(0)
	next := func(uid types.UID, c Config, workload string) Result {
		at += 5 * time.Second
		return tr.Read(context.Background(), guard, uid, c, workload, []Target{pod(1)}, t0.Add(at), time.Hour)
	}
	next("uid-1", cfg, "Deployment/checkout")
	if r := next("uid-1", cfg, "Deployment/checkout"); len(r.Series) != 2 {
		t.Fatalf("same guard: %+v", r.Series)
	}
	other := cfg
	other.Path = "/stats"
	f.set(1, 20, 0)
	for _, c := range []struct {
		name     string
		uid      types.UID
		cfg      Config
		workload string
	}{
		{"config", "uid-1", other, "Deployment/checkout"},
		{"workload", "uid-1", other, "StatefulSet/checkout"},
		{"UID", "uid-2", other, "StatefulSet/checkout"},
	} {
		if r := next(c.uid, c.cfg, c.workload); len(r.Series) != 1 || last(t, r) != (Count{}) {
			t.Fatalf("a new %s starts over with a baseline: %+v", c.name, r.Series)
		}
	}
	tr.Forget(guard)
	if r := next("uid-2", other, "StatefulSet/checkout"); len(r.Series) != 1 {
		t.Fatalf("a forgotten guard starts over: %+v", r.Series)
	}
}

func TestRetentionAndTheCap(t *testing.T) {
	tr, f := newTracker()
	f.set(1, 0, 0)
	keep := 30 * time.Second
	var r Result
	for s := 0; s <= 60; s += 5 {
		r = tr.Read(context.Background(), guard, "uid-1", cfg, "Deployment/checkout", []Target{pod(1)}, t0.Add(time.Duration(s)*time.Second), keep)
	}
	if len(r.Series) != 7 || !r.Series[0].At.Equal(t0.Add(30*time.Second)) {
		t.Fatalf("want the samples from 30s to 60s, got %d from %s", len(r.Series), r.Series[0].At)
	}

	tr, f = newTracker()
	f.set(1, 0, 0)
	for i := 0; i < maxSamples+100; i++ {
		r = tr.Read(context.Background(), guard, "uid-1", cfg, "Deployment/checkout", []Target{pod(1)}, t0.Add(time.Duration(i)*5*time.Second), 1000*time.Hour)
	}
	if len(r.Series) != maxSamples || !r.Series[0].At.Equal(t0.Add(100*5*time.Second)) {
		t.Fatalf("want the newest %d samples, got %d from %s", maxSamples, len(r.Series), r.Series[0].At)
	}
}

func TestBetweenAndOver(t *testing.T) {
	s := Series{
		{At: t0, Count: Count{Total: 10, Failed: 1}},
		{At: t0.Add(5 * time.Second), Count: Count{Total: 20, Failed: 2}},
		{At: t0.Add(10 * time.Second), Count: Count{Total: 30, Failed: 3}},
	}
	if c := s.Between(t0, t0.Add(10*time.Second)); c != (Count{Total: 50, Failed: 5}) {
		t.Fatalf("from is exclusive and to inclusive: got %+v", c)
	}
	if c := s.Between(t0.Add(-time.Second), t0.Add(5*time.Second)); c != (Count{Total: 30, Failed: 3}) {
		t.Fatalf("got %+v", c)
	}
	if (Count{Total: 100, Failed: 5}).Over(5) {
		t.Fatal("exactly at the limit is not over it")
	}
	if !(Count{Total: 100, Failed: 6}).Over(5) || !(Count{Total: 1000, Failed: 6}).Over(0.5) {
		t.Fatal("more than the limit is over it")
	}
	if (Count{}).Over(5) || (Count{}).Percent() != 0 {
		t.Fatal("no requests are never over the limit")
	}
	if p := (Count{Total: 20, Failed: 19}).Percent(); p != 95 {
		t.Fatalf("Percent = %g", p)
	}
}

// served starts a server and returns a target and config that reach it.
func served(t *testing.T, h http.HandlerFunc) (Target, Config) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(u.Host)
	n, _ := strconv.Atoi(port)
	c := cfg
	c.Port = int32(n)
	return Target{Name: "checkout-1", IP: host}, c
}

const good = "http_requests_total{code=\"200\"} 3\n"

func TestFetchRefusesWhatItShouldNotCount(t *testing.T) {
	if tr := NewTracker(nil); tr.HTTP.Timeout != 2*time.Second || tr.HTTP.CheckRedirect == nil || tr.HTTP.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the default client must time out after 2s, follow no redirects and use no proxy")
	}
	followed := false
	cases := []struct {
		name string
		h    http.HandlerFunc
		want string
	}{
		{"an error status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, "returned 503"},
		{"a redirect", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				followed = true
				_, _ = io.WriteString(w, good)
				return
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, "returned 302"},
		{"protobuf", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.google.protobuf; proto=io.prometheus.client.MetricFamily; encoding=delimited")
			_, _ = io.WriteString(w, good)
		}, "protobuf"},
		{"over 8 MiB", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, good)
			line := "# " + strings.Repeat("x", 1000) + "\n"
			for i := 0; i < 9*1024; i++ {
				_, _ = io.WriteString(w, line)
			}
		}, "over 8 MiB"},
		{"too slow", func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}, "Timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target, conf := served(t, c.h)
			tr := NewTracker(nil)
			tr.HTTP.Timeout = 200 * time.Millisecond // the real one is 2s
			r := tr.Read(context.Background(), guard, "uid-1", conf, "Deployment/checkout", []Target{target}, t0, time.Hour)
			if r.Read != 0 || len(r.Series) != 0 || r.Err == nil || !strings.Contains(r.Err.Error(), c.want) {
				t.Fatalf("got %+v, want no reading and an error containing %q", r, c.want)
			}
		})
	}
	if followed {
		t.Fatal("a redirect must not be followed")
	}

	// Error text is capped, so an answer is never echoed into status.
	target, conf := served(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "http_requests_total{code=\"200\"} "+strings.Repeat("x", 5000)+"\n")
	})
	r := NewTracker(nil).Read(context.Background(), guard, "uid-1", conf, "Deployment/checkout", []Target{target}, t0, time.Hour)
	if r.Err == nil || len([]rune(r.Err.Error())) > maxErrorText {
		t.Fatalf("want an error of at most %d characters, got %v", maxErrorText, r.Err)
	}

	// A good answer from a real server reads.
	target, conf = served(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "flagbreaker" || !strings.HasPrefix(r.Header.Get("Accept"), "text/plain") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, good)
	})
	if r := NewTracker(nil).Read(context.Background(), guard, "uid-1", conf, "Deployment/checkout", []Target{target}, t0, time.Hour); r.Read != 1 || r.Err != nil {
		t.Fatalf("a good answer: %+v", r)
	}
}

func TestTargetsPicksRunningPodsWithAnIP(t *testing.T) {
	running := func(name, ip string) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip}}
	}
	now := metav1.NewTime(t0)
	pending := running("pending", "10.0.0.5")
	pending.Status.Phase = corev1.PodPending
	deleting := running("deleting", "10.0.0.6")
	deleting.DeletionTimestamp = &now
	host := running("host", "192.168.1.10")
	host.Spec.HostNetwork = true
	pods := []corev1.Pod{running("b", "10.0.0.2"), pending, running("no-ip", ""), deleting, host, running("a", "fd00::1")}

	got := Targets(pods)
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" || got[0].UID != "uid-a" {
		t.Fatalf("want a and b, by name: %+v", got)
	}
	if u := cfg.URL(got[0]); u != "http://[fd00::1]:8080/metrics" {
		t.Fatalf("an IPv6 address is bracketed: %s", u)
	}

	var many []corev1.Pod
	for i := 0; i < 25; i++ {
		many = append(many, running(fmt.Sprintf("p-%02d", i), fmt.Sprintf("10.0.1.%d", i)))
	}
	if got := Targets(many); len(got) != MaxPods || got[0].Name != "p-00" || got[MaxPods-1].Name != "p-19" {
		t.Fatalf("want the first %d by name, got %d", MaxPods, len(got))
	}
}
