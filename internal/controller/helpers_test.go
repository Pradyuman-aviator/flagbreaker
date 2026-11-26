package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
	"github.com/Pradyuman-aviator/flagbreaker/internal/flagsmith"
	"github.com/Pradyuman-aviator/flagbreaker/internal/workload"
)

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

func mt(d time.Duration) *metav1.Time { v := metav1.NewTime(t0.Add(d)); return &v }

func incident() *v1alpha1.FlagGuard {
	g := &v1alpha1.FlagGuard{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout-new-pricing"},
		Spec:       v1alpha1.FlagGuardSpec{Flag: "new_pricing", Workload: v1alpha1.WorkloadReference{Name: "checkout"}},
	}
	st := &g.Status
	st.Environment = "Production"
	st.TrippedAt, st.FlagDisabledAt, st.RecoveredAt = mt(9*time.Second), mt(10*time.Second), mt(51*time.Second)
	st.FlagDisabledBy = ChangedByFlagbreaker
	st.Trigger = &v1alpha1.FlagChange{At: *mt(0), Enabled: true, ChangedBy: "admin@example.com"}
	addTimeline(st, t0, "FlagChanged", "new_pricing was switched on in Production by admin@example.com")
	addTimeline(st, t0.Add(9*time.Second), "Tripped", "shop/checkout: 2 of 2 pods started failing")
	return g
}

func TestTimingsMeasureTheIncident(t *testing.T) {
	got := timings(&incident().Status)
	want := "Detected 9s after the change, switched off 1s after that, recovered 41s after the switch-off."
	if got != want {
		t.Fatalf("timings = %q\nwant      %q", got, want)
	}
}

func TestTimingsNameAPersonWhoSwitchedOff(t *testing.T) {
	st := incident().Status
	st.FlagDisabledBy = "oncall@example.com"
	if got := timings(&st); !strings.Contains(got, "switched off by oncall@example.com 1s after that") {
		t.Fatalf("timings = %q", got)
	}
}

func TestTimingsStartFromTheAlert(t *testing.T) {
	st := incident().Status
	st.Trigger = nil
	st.Alert = &v1alpha1.HealthAlert{Provider: "Grafana", Title: "High error rate", At: *mt(4 * time.Second)}
	if got := timings(&st); !strings.HasPrefix(got, "Detected 5s after the alert") {
		t.Fatalf("timings = %q", got)
	}
}

func TestUnhealthyEventNamesThePodsTheTimelineAndTheLinks(t *testing.T) {
	g := incident()
	g.Status.RecoveredAt = nil
	a := workload.Assessment{Pods: 2, Problems: []workload.Problem{{Pod: "checkout-1", Reason: "container checkout exited: Error (exit code 1)"}}}
	links := []flagsmith.URLBlock{{Title: "Runbook", URL: "https://example.com/runbook"}}

	ev := buildHealthEvent(g, flagsmith.Unhealthy, a, links)
	if ev.Feature != "new_pricing" || ev.Environment != "Production" || ev.Status != flagsmith.Unhealthy {
		t.Fatalf("event = %+v", ev)
	}
	text := ""
	for _, b := range ev.Reason.TextBlocks {
		text += b.Title + "\n" + b.Text + "\n"
	}
	for _, want := range []string{
		"Flagbreaker: Deployment shop/checkout",
		"so Flagbreaker switched the flag off",
		"checkout-1: container checkout exited",
		"10:00:00  new_pricing was switched on in Production by admin@example.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("reason is missing %q:\n%s", want, text)
		}
	}
	if len(ev.Reason.URLBlocks) != 1 || ev.Reason.URLBlocks[0].URL != "https://example.com/runbook" {
		t.Errorf("url blocks = %+v", ev.Reason.URLBlocks)
	}
	if !isOwnEvent(flagsmith.HealthEventRecord{ProviderName: flagsmith.ProviderWebhook, Reason: &struct {
		TextBlocks []flagsmith.TextBlock `json:"text_blocks"`
		URLBlocks  []flagsmith.URLBlock  `json:"url_blocks"`
	}{TextBlocks: ev.Reason.TextBlocks}}) {
		t.Error("Flagbreaker doesn't recognise its own event")
	}
}

func TestHealthyEventExplainsHowToRestore(t *testing.T) {
	g := incident()
	g.Status.Snapshot = &v1alpha1.FlagSnapshot{Enabled: true, EnabledSegmentOverrides: []int64{7}}
	ev := buildHealthEvent(g, flagsmith.Healthy, workload.Assessment{}, nil)
	text := ev.Reason.TextBlocks[0].Text
	for _, want := range []string{"verified healthy", "with its 1 segment override", "kubectl annotate flagguard -n shop checkout-new-pricing flagbreaker.dev/restore="} {
		if !strings.Contains(text, want) {
			t.Errorf("summary is missing %q: %s", want, text)
		}
	}
}

func TestTimelineKeepsTheLatestEntries(t *testing.T) {
	var st v1alpha1.FlagGuardStatus
	for i := 0; i < maxTimeline+5; i++ {
		addTimeline(&st, t0.Add(time.Duration(i)*time.Second), "E", "m")
	}
	if len(st.Timeline) != maxTimeline || !st.Timeline[0].Time.Equal(mt(5*time.Second)) {
		t.Fatalf("timeline has %d entries starting at %v", len(st.Timeline), st.Timeline[0].Time)
	}
}

func TestGuardNamesAreValidAndStable(t *testing.T) {
	for flag, want := range map[string]string{
		"new_pricing":      "checkout-new-pricing",
		"Express.Checkout": "checkout-express-checkout",
	} {
		if got := guardName("checkout", flag); got != want {
			t.Errorf("guardName(checkout, %q) = %q, want %q", flag, got, want)
		}
	}
	if got := splitFlags(" a, b ,a,,c "); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("splitFlags = %v", got)
	}
}
