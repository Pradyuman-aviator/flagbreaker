package controller

// Incidents where requests fail while every pod stays up and ready. A guard
// with spec.requests reads the workload's request counter from each pod; the
// fake here serves those counters, and the tests raise them between checks.
// Checks are 5s apart, since a guard's pods are read at most every 4s.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/requests"
)

// fakeMetrics serves each pod's http_requests_total, by pod IP and port.
type fakeMetrics struct {
	mu    sync.Mutex
	calls int
	fail  bool // every answer is a 500
	pods  map[string]*requests.Count
}

func (m *fakeMetrics) RoundTrip(req *http.Request) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	c, ok := m.pods[req.URL.Host]
	switch {
	case !ok:
		return nil, errors.New("connection refused")
	case m.fail:
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	body := fmt.Sprintf("# TYPE http_requests_total counter\nhttp_requests_total{code=\"200\"} %g\nhttp_requests_total{code=\"500\"} %g\n", c.Total-c.Failed, c.Failed)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/plain; version=0.0.4; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
}

// traffic adds requests that went through and requests that failed with
// 500, shared out over the pods.
func (m *fakeMetrics) traffic(ok, failed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	hosts := make([]string, 0, len(m.pods))
	for h := range m.pods {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	share := func(n, i int) int {
		s := n / len(hosts)
		if i < n%len(hosts) {
			s++
		}
		return s
	}
	for i, h := range hosts {
		o, f := share(ok, i), share(failed, i)
		m.pods[h].Total += float64(o + f)
		m.pods[h].Failed += float64(f)
	}
}

func (m *fakeMetrics) set(fn func(m *fakeMetrics)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m)
}

func (m *fakeMetrics) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// withMetrics gives the world's reconciler a tracker that reads the fake, and
// the pods IPs it serves.
func (w *world) withMetrics() *fakeMetrics {
	w.t.Helper()
	m := &fakeMetrics{pods: map[string]*requests.Count{}}
	w.r.Requests = requests.NewTracker(&http.Client{Transport: m})
	w.giveIPs(m)
	return m
}

// giveIPs gives every pod an IP, as the kubelet does, and the fake a counter
// for each.
func (w *world) giveIPs(m *fakeMetrics) {
	w.t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range w.pods() {
		p.Status.PodIP = "10.0.0." + p.Name[strings.LastIndex(p.Name, "-")+1:]
		if err := w.c.Status().Update(w.ctx, &p); err != nil {
			w.t.Fatal(err)
		}
		if m.pods[p.Status.PodIP+":8080"] == nil {
			m.pods[p.Status.PodIP+":8080"] = &requests.Count{}
		}
	}
}

// readRequests sets the guard's spec.requests. Only the port: the rest
// comes from the defaults, which the fake client doesn't fill in.
func (w *world) readRequests() {
	w.t.Helper()
	var g v1alpha1.FlagGuard
	key := types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}
	if err := w.c.Get(w.ctx, key, &g); err != nil {
		w.t.Fatal(err)
	}
	g.Spec.Requests = &v1alpha1.RequestsSpec{Port: 8080}
	if err := w.c.Update(w.ctx, &g); err != nil {
		w.t.Fatal(err)
	}
}

func requestsWorld(t *testing.T, mode v1alpha1.Mode) (*world, *fakeMetrics) {
	w := newWorld(t, mode)
	w.readRequests()
	return w, w.withMetrics()
}

func (w *world) podNames() string {
	var names []string
	for _, p := range w.pods() {
		names = append(names, p.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// fineUntil10s: requests go through, 30 every 5s, up to t0+10s.
func fineUntil10s(w *world, m *fakeMetrics) {
	w.t.Helper()
	w.at(0) // where counting starts
	m.traffic(30, 0)
	w.at(5 * time.Second)
	m.traffic(30, 0)
	w.at(10 * time.Second)
}

func textBlock(ev flagsmith.HealthEventRecord, title string) string {
	if ev.Reason == nil {
		return ""
	}
	for _, b := range ev.Reason.TextBlocks {
		if b.Title == title {
			return b.Text
		}
	}
	return ""
}

func TestFailingRequestsTripTheBreakerWhileEveryPodStaysUp(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	if c := findCondition(w.at(0), conditionRequests); c == nil || c.Status != metav1.ConditionTrue || c.Message != "Read http_requests_total from 2 of 2 pods" {
		t.Fatalf("RequestsRead = %+v", c)
	}
	m.traffic(30, 0)
	w.at(5 * time.Second)
	m.traffic(30, 0)
	w.at(10 * time.Second)
	pods := w.podNames()

	// A person switches the flag on, and payments start failing. Every pod
	// stays up and ready.
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(1, 19)
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || !g.Status.RequestsIncident {
		t.Fatalf("want a trip on failing requests, got %+v", g.Status)
	}
	want := "Deployment shop/checkout: 19 of 20 requests failed (95.0%, over the 5% limit) in the 5s after the flag was switched on at 10:00:10 UTC, " +
		"against 0 of 30 in the minute before; 2 of 2 pods ready"
	if msg := g.Status.Timeline[1].Message; msg != want {
		t.Fatalf("Tripped = %q\nwant      %q", msg, want)
	}
	if def, seg := w.flag(); def || seg || w.fs.patches != 1 {
		t.Fatalf("want the flag off everywhere in one PATCH: default=%v segment=%v patches=%d", def, seg, w.fs.patches)
	}
	if got := w.podNames(); got != pods {
		t.Fatalf("no pod may be restarted: %s, was %s", got, pods)
	}
	if want := "FlagChanged,Tripped,FlagSwitchedOff"; timelineEvents(g) != want {
		t.Fatalf("timeline = %s, want %s", timelineEvents(g), want)
	}
	if rq := g.Status.Requests; rq == nil || rq.Total != 80 || rq.Failed != 19 || rq.ErrorRate != "23.8%" || rq.PodsRead != 2 || !rq.At.Time.Equal(t0.Add(15*time.Second)) {
		t.Fatalf("status.requests = %+v", g.Status.Requests)
	}
	ev := w.fs.lastOwnEvent(t)
	if !ev.Unhealthy() || ev.FirstText() != "Requests to the workload started failing after this flag changed, so Flagbreaker switched the flag off." {
		t.Fatalf("Unhealthy event = %q", ev.FirstText())
	}
	if text := textBlock(ev, "Requests"); text != "In the minute up to 10:00:15 UTC, 19 of 80 requests failed (23.8%), counted by http_requests_total on 2 pods." {
		t.Fatalf("Requests block = %q", text)
	}

	// The app reads the flag again a moment later, and payments go through.
	m.traffic(1, 1)
	w.at(20 * time.Second)
	for s := 25; s <= 35; s += 5 {
		m.traffic(30, 0)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
			t.Fatalf("recovery needs stableFor (20s): %+v", g.Status)
		}
	}
	m.traffic(30, 0)
	g = w.at(40 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %s: %v", g.Status.Phase, g.Status.Timeline)
	}
	if last := g.Status.Timeline[len(g.Status.Timeline)-1]; last.Event != "RestoreAvailable" ||
		!strings.HasPrefix(g.Status.Timeline[len(g.Status.Timeline)-2].Message, "Verified recovery: all 2 pods ready with no new crashes for 20s, and 0 of 120 requests failed in that time.") {
		t.Fatalf("Recovered = %v", g.Status.Timeline)
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() {
		t.Fatalf("want Healthy reported: %+v", ev)
	}
	if got := w.podNames(); got != pods || w.fs.patches != 1 {
		t.Fatalf("pods %s (was %s), patches %d", got, pods, w.fs.patches)
	}
}

func TestWatchModeOnlyReportsFailingRequests(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeWatch)
	fineUntil10s(w, m)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(2, 18)
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeWatch {
		t.Fatalf("want Tripped in watch mode, got %+v", g.Status)
	}
	if ev := w.fs.lastOwnEvent(t); !strings.HasPrefix(ev.FirstText(), "Requests to the workload started failing after this flag changed. Flagbreaker is in watch mode") {
		t.Fatalf("Unhealthy event = %q", ev.FirstText())
	}
	// While the flag is on, requests keep failing, and the guard stays
	// tripped however ready the pods are.
	for s := 20; s <= 60; s += 5 {
		m.traffic(2, 18)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
			t.Fatalf("at %ds: failing requests must hold the incident: %+v", s, g.Status)
		}
	}
	// A person switches it off, as the Health tab asks.
	w.fs.set(func(f *fakeFlagsmith) { f.def = false })
	m.traffic(20, 0)
	g = w.at(65 * time.Second)
	if g.Status.FlagDisabledAt == nil || !strings.Contains(timelineEvents(g), "FlagSwitchedOffByPerson") || strings.Contains(timelineEvents(g), "PodsRestarted") {
		t.Fatalf("the person's switch-off, and no restart: %v", g.Status.Timeline)
	}
	for s := 70; s <= 80; s += 5 {
		m.traffic(30, 0)
		g = w.at(time.Duration(s) * time.Second)
	}
	if g.Status.Phase != v1alpha1.PhaseRecovered || w.fs.patches != 0 || len(w.pods()) != 2 {
		t.Fatalf("want Recovered with nothing changed by Flagbreaker: %+v, %d patches", g.Status, w.fs.patches)
	}
}

func TestABadValueThatFailsRequestsIsPutBack(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def, f.value = true, goodValue })
	fineUntil10s(w, m)
	w.fs.set(func(f *fakeFlagsmith) { f.value = badValue })
	m.traffic(2, 18)
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || !g.Status.RequestsIncident || g.Status.Trigger == nil || !g.Status.Trigger.ValueChanged {
		t.Fatalf("want a trip blamed on the value, got %+v", g.Status)
	}
	if def, seg := w.flag(); !def || !seg || !flagsmith.SameValue(w.value(), goodValue) {
		t.Fatalf("the previous value should be back and the flag still on: default=%v segment=%v value=%+v", def, seg, w.value())
	}
	if want := "FlagChanged,Tripped,ValueReverted"; timelineEvents(g) != want {
		t.Fatalf("timeline = %s, want %s", timelineEvents(g), want)
	}
	if ev := w.fs.lastOwnEvent(t); ev.FirstText() != "Requests to the workload started failing after this flag's value changed, so Flagbreaker put the previous value back. Whether the flag is on is unchanged." {
		t.Fatalf("Unhealthy event = %q", ev.FirstText())
	}
	for s := 20; s <= 40; s += 5 {
		m.traffic(30, 0)
		g = w.at(time.Duration(s) * time.Second)
	}
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
	m.traffic(30, 0)
	if g := w.at(45 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed || g.Status.RequestsIncident {
		t.Fatalf("nothing is left to wait for after a value was put back: %+v", g.Status)
	}
}

func TestRequestsFailingBeforeTheSwitchOnAreNotBlamed(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	w.at(0)
	m.traffic(10, 20)
	w.at(5 * time.Second)
	m.traffic(10, 20)
	w.at(10 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	for s := 15; s <= 40; s += 5 {
		m.traffic(10, 20)
		if g := w.at(time.Duration(s) * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
			t.Fatalf("at %ds: requests were failing before the switch-on: %+v", s, g.Status)
		}
	}
	if def, _ := w.flag(); !def || w.fs.patches != 0 {
		t.Fatal("the flag must stay on")
	}
}

func TestUnreadableMetricsTripNothingAndHoldNoPodsIncident(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	m.set(func(m *fakeMetrics) { m.fail = true })
	m.traffic(30, 0)
	g := w.at(0)
	if c := findCondition(g, conditionRequests); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ReadFailed" ||
		!strings.Contains(c.Message, "returned 500") || g.Status.Requests != nil {
		t.Fatalf("RequestsRead = %+v, status.requests = %+v", c, g.Status.Requests)
	}
	if c := findCondition(g, conditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("unreadable metrics are no check failure: %+v", c)
	}
	m.traffic(30, 0)
	w.at(5 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(2, 18) // failing, but nobody can tell
	if g := w.at(10 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("no readings, no trip on requests: %+v", g.Status)
	}

	// A crash still trips on the pods, and recovers on them.
	w.crashPods(t0.Add(12 * time.Second))
	g = w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.RequestsIncident || !strings.Contains(timelineEvents(g), "PodsRestarted") {
		t.Fatalf("want a pods trip: %+v", g.Status)
	}
	w.healthyPods(2, t0.Add(17*time.Second)) // the replacements have no IP yet
	if c := findCondition(w.at(20*time.Second), conditionRequests); c == nil || c.Reason != "NoPodsToRead" {
		t.Fatalf("RequestsRead = %+v", c)
	}
	if g := w.at(41 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("a pods incident doesn't wait for readings: %+v", g.Status)
	}
}

func TestARequestsIncidentWaitsForTraffic(t *testing.T) {
	w, m := requestsWorld(t, v1alpha1.ModeAct)
	fineUntil10s(w, m)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	m.traffic(1, 19)
	if g := w.at(15 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped: %+v", g.Status)
	}
	// Nobody shops: the pods are fine, but nothing shows that payments work.
	var g *v1alpha1.FlagGuard
	for s := 20; s <= 60; s += 5 {
		g = w.at(time.Duration(s) * time.Second)
	}
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.HealthySince == nil || !g.Status.HealthySince.Time.Equal(t0.Add(20*time.Second)) {
		t.Fatalf("want Tripped, healthy since 20s: %+v", g.Status)
	}
	m.traffic(10, 0)
	if g := w.at(65 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("10 requests are too few: %+v", g.Status)
	}
	m.traffic(10, 0)
	g = w.at(70 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseRecovered || !strings.Contains(timelineEvents(g), "Recovered") {
		t.Fatalf("want Recovered once 20 requests went through: %+v", g.Status)
	}
	for _, e := range g.Status.Timeline {
		if e.Event == "Recovered" && !strings.Contains(e.Message, "for 50s, and 0 of 20 requests failed in that time") {
			t.Fatalf("Recovered = %q", e.Message)
		}
	}
}

func TestAGuardWithoutSpecRequestsReadsNoPods(t *testing.T) {
	// The same pods incident, once with a tracker that counts its calls and
	// pods that have IPs, once as before spec.requests existed.
	run := func(withTracker bool) (*v1alpha1.FlagGuard, flagsmith.HealthEventRecord, int) {
		w := newWorld(t, v1alpha1.ModeAct)
		var m *fakeMetrics
		if withTracker {
			m = w.withMetrics()
		}
		w.tripNewPricing()
		w.healthyPods(2, t0.Add(12*time.Second))
		if withTracker {
			w.giveIPs(m)
		}
		w.at(15 * time.Second)
		g := w.at(36 * time.Second)
		if g.Status.Phase != v1alpha1.PhaseRecovered {
			t.Fatalf("want Recovered: %+v", g.Status)
		}
		calls := 0
		if m != nil {
			calls = m.callCount()
		}
		return g, w.fs.lastOwnEvent(t), calls
	}
	g, ev, calls := run(true)
	if calls != 0 {
		t.Fatalf("a guard without spec.requests must send no request to a pod, sent %d", calls)
	}
	if g.Status.Requests != nil || g.Status.RequestsIncident || findCondition(g, conditionRequests) != nil {
		t.Fatalf("no requests status without spec.requests: %+v", g.Status)
	}
	before, evBefore, _ := run(false)
	if fmt.Sprint(g.Status.Timeline) != fmt.Sprint(before.Status.Timeline) || fmt.Sprint(ev.Reason) != fmt.Sprint(evBefore.Reason) {
		t.Fatalf("the incident must read as before:\n%v\n%v", g.Status.Timeline, before.Status.Timeline)
	}
}
