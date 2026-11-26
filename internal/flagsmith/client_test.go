package flagsmith

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The response bodies below were copied from Flagsmith 2.295.2.

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "org-key", HealthWebhookPath: "/api/v1/feature-health/secret/path"}
}

func TestGetFlagSendsTheOrgAPIKey(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Api-Key org-key" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/api/__future__/environments/envkey/features/7/" {
			t.Errorf("path = %q", r.URL.Path)
		}
		io.WriteString(w, `{"environment_default":{"enabled":true,"value":null,"variants":[]},
			"segment_overrides":[{"segment":{"id":3},"priority":0,"enabled":true,"value":null,"variants":[]},
			{"segment":{"id":4},"priority":1,"enabled":false,"value":null,"variants":[]}]}`)
	})
	flag, err := c.GetFlag(context.Background(), "envkey", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !flag.EnvironmentDefault.Enabled {
		t.Error("want the environment default enabled")
	}
	if ids := flag.EnabledSegmentIDs(); len(ids) != 1 || ids[0] != 3 {
		t.Errorf("EnabledSegmentIDs = %v, want [3]", ids)
	}
}

func TestSwitchOffPatchesDefaultAndSegments(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		want := `{"environment_default":{"enabled":false},"segment_overrides":[{"segment":{"id":3},"enabled":false}]}`
		if string(body) != want {
			t.Errorf("body = %s\nwant   %s", body, want)
		}
		io.WriteString(w, `{"environment_default":{"enabled":false,"value":null,"variants":[]},"segment_overrides":[]}`)
	})
	if _, err := c.SwitchOff(context.Background(), "envkey", 7, []int64{3}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeRequestsAreReported(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"detail":"Cannot make this change where change requests are enabled."}`)
	})
	_, err := c.SwitchOff(context.Background(), "envkey", 7, nil)
	if !errors.Is(err, ErrChangeRequestsEnabled) {
		t.Fatalf("err = %v, want ErrChangeRequestsEnabled", err)
	}
}

func TestFeatureIDPicksTheExactName(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("search"); got != "new_pricing" {
			t.Errorf("search = %q", got)
		}
		io.WriteString(w, `{"count":2,"results":[{"id":1,"name":"new_pricing_v2"},{"id":2,"name":"new_pricing"}]}`)
	})
	id, err := c.FeatureID(context.Background(), 1, "new_pricing")
	if err != nil || id != 2 {
		t.Fatalf("FeatureID = %d, %v; want 2", id, err)
	}
}

func TestReportHealthAlwaysSendsBothReasonLists(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the webhook path is the credential; no API key should be sent")
		}
		if r.URL.Path != "/api/v1/feature-health/secret/path" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		reason := got["reason"].(map[string]any)
		if _, ok := reason["url_blocks"]; !ok {
			t.Error("url_blocks missing; Flagsmith 2.295 rejects that with 400")
		}
		if _, ok := reason["text_blocks"]; !ok {
			t.Error("text_blocks missing")
		}
	})
	err := c.ReportHealth(context.Background(), HealthEvent{Feature: "new_pricing", Status: Unhealthy, Environment: "Production"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestErrorsHideTheWebhookPath(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	err := c.ReportHealth(context.Background(), HealthEvent{Feature: "x", Status: Healthy, Environment: "Production"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, want an error without the secret path", err)
	}
}

func TestNetworkErrorsHideTheWebhookPath(t *testing.T) {
	// Nothing listens on this port: the dial fails, and Go's error repeats
	// the URL, secret path included, unless the client strips it.
	c := &Client{BaseURL: "http://127.0.0.1:1", HealthWebhookPath: "/api/v1/feature-health/SECRETSIG/"}
	err := c.ReportHealth(context.Background(), HealthEvent{Feature: "f", Status: Unhealthy})
	if err == nil || strings.Contains(err.Error(), "SECRETSIG") {
		t.Fatalf("error = %v, want one without the secret path", err)
	}
}

func TestARedirectDoesNotTurnASwitchOffIntoAGet(t *testing.T) {
	var methods []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path == "/moved/" {
			io.WriteString(w, `{"environment_default":{"enabled":true},"segment_overrides":[]}`)
			return
		}
		http.Redirect(w, r, "/moved/", http.StatusMovedPermanently)
	})
	if _, err := c.SwitchOff(context.Background(), "envkey", 7, nil); err == nil {
		t.Fatal("a redirected PATCH must fail, not succeed as a GET")
	}
	if len(methods) != 1 {
		t.Fatalf("requests = %v, want only the PATCH", methods)
	}
}

func TestASwitchOffFlagsmithDidNotApplyIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"environment_default":{"enabled":true},"segment_overrides":[]}`)
	})
	if _, err := c.SwitchOff(context.Background(), "envkey", 7, nil); err == nil {
		t.Fatal("want an error when Flagsmith answers 200 but the flag is still on")
	}
}

func TestAnExperimentOverrideIsARejection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"detail": "System segment overrides can't be modified.", "code": "system_segment_modification"}`)
	})
	_, err := c.SwitchOff(context.Background(), "envkey", 7, []int64{9})
	if !IsRejected(err) || errors.Is(err, ErrChangeRequestsEnabled) {
		t.Fatalf("err = %v, want a rejection that isn't change requests", err)
	}
}
