package controller

// Whole incidents, run through Reconcile against a fake Kubernetes API
// (controller-runtime's fake client) and a fake Flagsmith that speaks the
// same HTTP API as Flagsmith 2.295. The end-to-end scripts in hack/demo run
// the same stories against the real thing.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/changes"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/settings"
)

const (
	envKey     = "envkey"
	envID      = 3
	projectID  = 2
	featureID  = 11
	segmentID  = 7
	healthPath = "/api/v1/feature-health/secret-path"
)

// fakeFlagsmith is the slice of Flagsmith's API that Flagbreaker calls.
type fakeFlagsmith struct {
	mu       sync.Mutex
	def      bool
	value    *flagsmith.Value
	segments map[int64]bool
	conflict bool
	patch500 bool // every PATCH fails with a 500
	value500 bool // a PATCH that sets a value fails with a 500
	// timeout: Flagsmith applies a PATCH, then answers 500, as when the
	// request times out after Flagsmith applied it
	timeout   bool
	health500 bool // every Feature Health report fails with a 500
	patches   int
	events    []fakeEvent
	nextEvent int64
	clock     func() time.Time
	// experimentSegment is a system segment: any PATCH that lists it is
	// refused with 409, as Flagsmith refuses changes to experiments.
	experimentSegment int64
}

type fakeEvent struct {
	flagsmith.HealthEventRecord
	slot string
}

func (f *fakeFlagsmith) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	flagURL := fmt.Sprintf("/api/__future__/environments/%s/features/%d/", envKey, featureID)
	switch {
	case r.URL.Path == "/api/v1/environments/"+envKey+"/":
		writeJSON(w, map[string]any{"id": envID, "name": "Production", "project": projectID})
	case r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/features/", projectID):
		writeJSON(w, map[string]any{"results": []any{
			map[string]any{"id": featureID + 1, "name": "new_pricing_v2"},
			map[string]any{"id": featureID, "name": "new_pricing"},
		}})
	case r.URL.Path == flagURL && r.Method == http.MethodGet:
		writeJSON(w, f.flagJSON())
	case r.URL.Path == flagURL && r.Method == http.MethodPatch:
		if f.patch500 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if f.conflict {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail": "Cannot make this change where change requests are enabled."}`))
			return
		}
		var body struct {
			Default *struct {
				Enabled *bool           `json:"enabled"`
				Value   json.RawMessage `json:"value"`
			} `json:"environment_default"`
			Segments []struct {
				Segment struct{ ID int64 } `json:"segment"`
				Enabled bool               `json:"enabled"`
			} `json:"segment_overrides"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if f.value500 && body.Default != nil && len(body.Default.Value) > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		for _, s := range body.Segments {
			if f.experimentSegment != 0 && s.Segment.ID == f.experimentSegment {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"detail": "System segment overrides can't be modified.", "code": "system_segment_modification"}`))
				return
			}
		}
		f.patches++
		if d := body.Default; d != nil {
			// As in Flagsmith: what isn't sent is left alone, and a null
			// value clears it.
			if d.Enabled != nil {
				f.def = *d.Enabled
			}
			if len(d.Value) > 0 {
				f.value = nil
				if string(d.Value) != "null" {
					f.value = &flagsmith.Value{}
					_ = json.Unmarshal(d.Value, f.value)
				}
			}
		}
		for _, s := range body.Segments {
			f.segments[s.Segment.ID] = s.Enabled // Flagsmith creates missing overrides too
		}
		if f.timeout {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(w, f.flagJSON())
	case r.URL.Path == fmt.Sprintf("/api/v1/projects/%d/feature-health/events/", projectID):
		writeJSON(w, f.latestEvents())
	case r.URL.Path == healthPath && r.Method == http.MethodPost && f.health500:
		w.WriteHeader(http.StatusInternalServerError)
	case r.URL.Path == healthPath && r.Method == http.MethodPost:
		var ev flagsmith.HealthEvent
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev.Reason.URLBlocks == nil || ev.Reason.TextBlocks == nil {
			w.WriteHeader(http.StatusBadRequest) // as Flagsmith 2.295 does
			return
		}
		var blocks []flagsmith.TextBlock = ev.Reason.TextBlocks
		f.addEvent(flagsmith.ProviderWebhook, "", strings.ToUpper(string(ev.Status)), blocks)
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeFlagsmith) flagJSON() map[string]any {
	var overrides []any
	ids := make([]int64, 0, len(f.segments))
	for id := range f.segments {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		overrides = append(overrides, map[string]any{"segment": map[string]any{"id": id}, "enabled": f.segments[id], "priority": 0})
	}
	return map[string]any{"environment_default": map[string]any{"enabled": f.def, "value": f.value}, "segment_overrides": overrides}
}

func (f *fakeFlagsmith) addEvent(provider, externalID, typ string, blocks []flagsmith.TextBlock) {
	f.nextEvent++
	e := fakeEvent{slot: provider + "/" + externalID}
	e.ID, e.ProviderName, e.Type, e.Feature, e.CreatedAt = f.nextEvent, provider, typ, featureID, f.clock()
	env := int64(envID)
	e.Environment = &env
	e.Reason = &struct {
		TextBlocks []flagsmith.TextBlock `json:"text_blocks"`
		URLBlocks  []flagsmith.URLBlock  `json:"url_blocks"`
	}{TextBlocks: blocks, URLBlocks: []flagsmith.URLBlock{}}
	f.events = append(f.events, e)
}

// latestEvents returns the newest event per slot, like Flagsmith.
func (f *fakeFlagsmith) latestEvents() []flagsmith.HealthEventRecord {
	latest := map[string]flagsmith.HealthEventRecord{}
	for _, e := range f.events {
		latest[e.slot] = e.HealthEventRecord
	}
	out := []flagsmith.HealthEventRecord{}
	for _, e := range latest {
		out = append(out, e)
	}
	return out
}

func (f *fakeFlagsmith) set(fn func(f *fakeFlagsmith)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// lastOwnEvent is the newest event Flagbreaker posted.
func (f *fakeFlagsmith) lastOwnEvent(t *testing.T) flagsmith.HealthEventRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.events) - 1; i >= 0; i-- {
		if isOwnEvent(f.events[i].HealthEventRecord) {
			return f.events[i].HealthEventRecord
		}
	}
	t.Fatal("Flagbreaker posted no Feature Health event")
	return flagsmith.HealthEventRecord{}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// world is one test's cluster, Flagsmith and clock.
type world struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	fs  *fakeFlagsmith
	r   *FlagGuardReconciler
	now time.Time
	rs  *appsv1.ReplicaSet
	pod int
}

func newWorld(t *testing.T, mode v1alpha1.Mode) *world {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	replicas := int32(2)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout", UID: "deploy-uid"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout"}},
		},
	}
	yes := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: "shop", Name: "checkout-abc", UID: "rs-uid", Labels: map[string]string{"app": "checkout"},
		CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour)),
		OwnerReferences:   []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "checkout", UID: "deploy-uid", Controller: &yes}},
	}}
	stable := metav1.Duration{Duration: 20 * time.Second}
	guard := &v1alpha1.FlagGuard{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout-new-pricing", Generation: 1},
		Spec: v1alpha1.FlagGuardSpec{
			Flag: "new_pricing", EnvironmentKey: envKey, Mode: mode, StableFor: &stable,
			Workload: v1alpha1.WorkloadReference{Kind: KindDeployment, Name: "checkout"},
			Links:    []v1alpha1.Link{{Title: "Runbook", URL: "https://example.com/runbook"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy, rs, guard).
		WithStatusSubresource(&v1alpha1.FlagGuard{}).Build()

	fs := &fakeFlagsmith{segments: map[int64]bool{segmentID: true}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)

	w := &world{t: t, ctx: context.Background(), c: c, fs: fs, now: t0, rs: rs}
	fs.clock = func() time.Time { return w.now }
	w.r = &FlagGuardReconciler{
		Client:     c,
		Recorder:   events.NewFakeRecorder(1000),
		Flagsmith:  &flagsmith.Client{BaseURL: srv.URL, APIKey: "key", HealthWebhookPath: healthPath},
		Settings:   settings.NewReader("", "", time.Second),
		Changes:    changes.NewStore(),
		SyncPeriod: 5 * time.Second,
		Now:        func() time.Time { return w.now },
		lastCheck:  map[types.NamespacedName]time.Time{},
	}
	w.healthyPods(2, t0.Add(-time.Hour))
	return w
}

// at moves the clock and reconciles.
func (w *world) at(d time.Duration) *v1alpha1.FlagGuard {
	w.t.Helper()
	w.now = t0.Add(d)
	key := types.NamespacedName{Namespace: "shop", Name: "checkout-new-pricing"}
	if _, err := w.r.Reconcile(w.ctx, ctrl.Request{NamespacedName: key}); err != nil {
		w.t.Fatalf("reconcile at %s: %v", d, err)
	}
	var g v1alpha1.FlagGuard
	if err := w.c.Get(w.ctx, key, &g); err != nil {
		w.t.Fatal(err)
	}
	if c := findCondition(&g, conditionReady); c != nil && c.Status == metav1.ConditionFalse {
		w.t.Fatalf("check failed at %s: %s", d, c.Message)
	}
	return &g
}

func findCondition(g *v1alpha1.FlagGuard, kind string) *metav1.Condition {
	for i := range g.Status.Conditions {
		if g.Status.Conditions[i].Type == kind {
			return &g.Status.Conditions[i]
		}
	}
	return nil
}

func (w *world) pods() []corev1.Pod {
	var list corev1.PodList
	if err := w.c.List(w.ctx, &list, client.InNamespace("shop")); err != nil {
		w.t.Fatal(err)
	}
	return list.Items
}

// healthyPods replaces all pods with n ready ones.
func (w *world) healthyPods(n int, since time.Time) {
	for _, p := range w.pods() {
		_ = w.c.Delete(w.ctx, &p)
	}
	yes := true
	for i := 0; i < n; i++ {
		w.pod++
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "shop", Name: fmt.Sprintf("checkout-abc-%d", w.pod), Labels: map[string]string{"app": "checkout"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: w.rs.Name, UID: w.rs.UID, Controller: &yes}},
			},
		}
		if err := w.c.Create(w.ctx, p); err != nil {
			w.t.Fatal(err)
		}
		// Pod status is a subresource, as in a real API server.
		p.Status = corev1.PodStatus{
			Phase:             corev1.PodRunning,
			Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(since)}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "checkout", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		}
		if err := w.c.Status().Update(w.ctx, p); err != nil {
			w.t.Fatal(err)
		}
	}
}

// crashPods makes every pod crash at the given time.
func (w *world) crashPods(at time.Time) {
	for _, p := range w.pods() {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(at)}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: "checkout", RestartCount: 1,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: metav1.NewTime(at)}},
		}}
		if err := w.c.Status().Update(w.ctx, &p); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) flag() (bool, bool) {
	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	return w.fs.def, w.fs.segments[segmentID]
}

func timelineEvents(g *v1alpha1.FlagGuard) string {
	var names []string
	for _, e := range g.Status.Timeline {
		names = append(names, e.Event)
	}
	return strings.Join(names, ",")
}

func TestActModeSwitchesOffEverywhereRecoversAndRestores(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	if g := w.at(0); g.Status.Phase != v1alpha1.PhaseArmed || g.Status.FlagEnabled == nil || *g.Status.FlagEnabled {
		t.Fatalf("first look: %+v", g.Status)
	}

	// A person switches the flag on; two seconds later the pods crash.
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeAct {
		t.Fatalf("want Tripped in act mode, got %s/%s: %v", g.Status.Phase, g.Status.Mode, g.Status.Timeline)
	}
	if def, seg := w.flag(); def || seg {
		t.Fatalf("the default and the segment override should both be off, got default=%v segment=%v", def, seg)
	}
	if s := g.Status.Snapshot; s == nil || !s.Enabled || len(s.EnabledSegmentOverrides) != 1 || s.EnabledSegmentOverrides[0] != segmentID {
		t.Fatalf("snapshot = %+v", g.Status.Snapshot)
	}
	if len(w.pods()) != 0 {
		t.Fatalf("crashed pods should have been restarted (deleted), %d left", len(w.pods()))
	}
	ev := w.fs.lastOwnEvent(t)
	if !ev.Unhealthy() || !strings.Contains(ev.FirstText(), "switched the flag off") {
		t.Fatalf("Unhealthy event = %+v", ev)
	}
	if want := "FlagChanged,Tripped,FlagSwitchedOff,PodsRestarted"; timelineEvents(g) != want {
		t.Fatalf("timeline = %s, want %s", timelineEvents(g), want)
	}

	// The replacements come up healthy; recovery needs 20s of that.
	w.healthyPods(2, t0.Add(12*time.Second))
	if g := w.at(15 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped || g.Status.HealthySince == nil {
		t.Fatalf("first healthy sighting: %+v", g.Status)
	}
	g = w.at(36 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %s: %v", g.Status.Phase, g.Status.Timeline)
	}
	if ev := w.fs.lastOwnEvent(t); ev.Unhealthy() || !strings.Contains(ev.FirstText(), "flagbreaker.dev/restore") {
		t.Fatalf("Healthy event should explain the restore: %+v", ev)
	}

	// The restore annotation turns both back on and re-arms the guard.
	g.Annotations = map[string]string{v1alpha1.RestoreAnnotation: "1"}
	if err := w.c.Update(w.ctx, g); err != nil {
		t.Fatal(err)
	}
	g = w.at(40 * time.Second)
	if def, seg := w.flag(); !def || !seg {
		t.Fatalf("restore should turn both back on, got default=%v segment=%v", def, seg)
	}
	if g.Status.Phase != v1alpha1.PhaseArmed || g.Status.Snapshot != nil || g.Status.LastFlagChange.ChangedBy != ChangedByRestore {
		t.Fatalf("after restore: %+v", g.Status)
	}
	patches := w.fs.patches
	w.at(45 * time.Second)
	if w.fs.patches != patches {
		t.Fatal("the same restore request must not be applied twice")
	}

	// The bug is still there: the restore trips the breaker again.
	w.crashPods(t0.Add(47 * time.Second))
	g = w.at(50 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Trigger == nil || g.Status.Trigger.ChangedBy != ChangedByRestore {
		t.Fatalf("want a second trip blamed on the restore, got %+v", g.Status)
	}
}

func TestWatchModeLeavesTheFlagOnUntilAPersonSwitchesItOff(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeWatch)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeWatch {
		t.Fatalf("want Tripped in watch mode, got %+v", g.Status)
	}
	if def, _ := w.flag(); !def || w.fs.patches != 0 {
		t.Fatal("watch mode must not change the flag")
	}
	if ev := w.fs.lastOwnEvent(t); !ev.Unhealthy() || !strings.Contains(ev.FirstText(), "watch mode") {
		t.Fatalf("Unhealthy event = %+v", ev)
	}
	if len(w.pods()) != 2 {
		t.Fatal("watch mode must not restart pods while the flag is on")
	}

	// A person switches it off. Flagbreaker records that and restarts the
	// crashed pods.
	w.fs.set(func(f *fakeFlagsmith) { f.def = false })
	g = w.at(20 * time.Second)
	if g.Status.FlagDisabledAt == nil || !strings.Contains(timelineEvents(g), "FlagSwitchedOffByPerson,PodsRestarted") {
		t.Fatalf("person's switch-off not recorded: %v", g.Status.Timeline)
	}
	w.healthyPods(2, t0.Add(22*time.Second))
	w.at(25 * time.Second)
	if g := w.at(46 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered, got %+v", g.Status)
	}
	if w.fs.patches != 0 {
		t.Fatal("watch mode must never change the flag")
	}
}

func TestAGrafanaAlertTripsTheGuardAndHoldsRecovery(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(0)
	if g := w.at(5 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("healthy pods and no alert: %+v", g.Status)
	}

	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderGrafana, "fp1", "UNHEALTHY", []flagsmith.TextBlock{{Title: "CheckoutErrorRateHigh", Text: "5xx at 7%"}})
	})
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Alert == nil || g.Status.Alert.Provider != flagsmith.ProviderGrafana {
		t.Fatalf("want a trip caused by the Grafana alert, got %+v", g.Status)
	}
	if def, _ := w.flag(); def {
		t.Fatal("the flag should be off")
	}

	// Healthy pods don't end it while the alert still fires.
	w.at(15 * time.Second)
	if g := w.at(45 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("must stay tripped while the alert fires: %+v", g.Status)
	}
	w.fs.set(func(f *fakeFlagsmith) { f.addEvent(flagsmith.ProviderGrafana, "fp1", "HEALTHY", nil) })
	if g := w.at(50 * time.Second); g.Status.Phase != v1alpha1.PhaseRecovered {
		t.Fatalf("want Recovered once the alert resolved: %+v", g.Status)
	}

	// A person turns the flag back on; the old alert is resolved and doesn't
	// trip it again.
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	if g := w.at(55 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("want Armed: %+v", g.Status)
	}
	if g := w.at(60 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("an old alert must not trip it again: %+v", g.Status)
	}
}

func TestChangeRequestsKeepFlagbreakerToWatching(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.fs.set(func(f *fakeFlagsmith) { f.conflict = true })
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseTripped || g.Status.Mode != v1alpha1.ModeWatch {
		t.Fatalf("want Tripped and dropped to watch, got %+v", g.Status)
	}
	if c := findCondition(g, conditionBlocked); c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("want the BlockedByChangeRequests condition, got %v", g.Status.Conditions)
	}
	if def, _ := w.flag(); !def {
		t.Fatal("Flagbreaker must never get around change requests")
	}
}

func TestDismissingTheIncidentInFlagsmithRearmsWithoutRetripping(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeWatch)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	if g := w.at(10 * time.Second); g.Status.Phase != v1alpha1.PhaseTripped {
		t.Fatalf("want Tripped: %+v", g.Status)
	}
	// Dismissing writes a Healthy event into the Webhook provider's slot.
	w.now = t0.Add(13 * time.Second)
	w.fs.set(func(f *fakeFlagsmith) {
		f.addEvent(flagsmith.ProviderWebhook, "", "HEALTHY", []flagsmith.TextBlock{{Text: "Manually dismissed by admin@example.com"}})
	})
	g := w.at(15 * time.Second)
	if g.Status.Phase != v1alpha1.PhaseArmed || g.Status.LastFlagChange == nil || !g.Status.LastFlagChange.Acknowledged {
		t.Fatalf("want Armed with the change acknowledged: %+v", g.Status)
	}
	if g := w.at(20 * time.Second); g.Status.Phase != v1alpha1.PhaseArmed {
		t.Fatalf("an acknowledged change must not trip again: %+v", g.Status)
	}
}

func TestRestoreIsRefusedWhileTripped(t *testing.T) {
	w := newWorld(t, v1alpha1.ModeAct)
	w.at(0)
	w.fs.set(func(f *fakeFlagsmith) { f.def = true })
	w.at(5 * time.Second)
	w.crashPods(t0.Add(7 * time.Second))
	g := w.at(10 * time.Second)
	g.Annotations = map[string]string{v1alpha1.RestoreAnnotation: "now"}
	if err := w.c.Update(w.ctx, g); err != nil {
		t.Fatal(err)
	}
	g = w.at(12 * time.Second)
	if def, _ := w.flag(); def || !strings.Contains(timelineEvents(g), "RestoreRefused") {
		t.Fatalf("restore must wait for verified recovery: flag=%v timeline=%s", def, timelineEvents(g))
	}
}
