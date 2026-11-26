package changes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// body is shaped like a real FLAG_UPDATED webhook from Flagsmith 2.295.2.
const body = `{"data": {"changed_by": "admin@flagbreaker.local", "new_state": {"enabled": true,
 "environment": {"id": 1, "name": "Production"}, "feature": {"id": 1, "name": "new_pricing"},
 "feature_segment": null, "feature_state_value": null, "identity": null, "identity_identifier": null},
 "timestamp": "2026-11-26T10:31:05.123456Z"}, "event_type": "FLAG_UPDATED"}`

func sign(secret, s string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(s))
	return hex.EncodeToString(mac.Sum(nil))
}

func post(h http.Handler, payload, signature string) int {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
	req.Header.Set(SignatureHeader, signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRecordsASignedChange(t *testing.T) {
	store := NewStore()
	var notified string
	h := Handler("s3cret", store, func(env, feature string) { notified = env + "/" + feature })

	if code := post(h, body, sign("s3cret", body)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	c, ok := store.Latest("Production", "new_pricing", true)
	if !ok || !c.Enabled || c.ChangedBy != "admin@flagbreaker.local" {
		t.Fatalf("Latest = %+v, %v", c, ok)
	}
	if want := time.Date(2026, 11, 26, 10, 31, 5, 123456000, time.UTC); !c.At.Equal(want) {
		t.Fatalf("At = %v, want %v", c.At, want)
	}
	if notified != "Production/new_pricing" {
		t.Fatalf("notified %q", notified)
	}
}

func TestRejectsABadSignature(t *testing.T) {
	store := NewStore()
	h := Handler("s3cret", store, nil)
	if code := post(h, body, sign("wrong", body)); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
	if _, ok := store.Latest("Production", "new_pricing", true); ok {
		t.Fatal("stored an unsigned change")
	}
}

func TestIgnoresSegmentOverrides(t *testing.T) {
	store := NewStore()
	h := Handler("s3cret", store, nil)
	segment := strings.Replace(body, `"feature_segment": null`, `"feature_segment": {"segment": {"id": 3}}`, 1)
	if code := post(h, segment, sign("s3cret", segment)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if _, ok := store.Latest("Production", "new_pricing", true); ok {
		t.Fatal("stored a segment override as an environment default change")
	}
}

func TestKeepsTheNewestChange(t *testing.T) {
	store := NewStore()
	newer := Change{At: time.Unix(200, 0), Enabled: false}
	store.Record("Production", "f", newer)
	store.Record("Production", "f", Change{At: time.Unix(100, 0), Enabled: false})
	if c, _ := store.Latest("Production", "f", false); c != newer {
		t.Fatalf("Latest = %+v, want the newer change", c)
	}
}

func TestKeepsSwitchOnsAndSwitchOffsApart(t *testing.T) {
	store := NewStore()
	on := Change{At: time.Unix(100, 0), Enabled: true, ChangedBy: "admin@example.com"}
	store.Record("Production", "f", on)
	store.Record("Production", "f", Change{At: time.Unix(104, 0), Enabled: false, ChangedBy: "flagbreaker"})
	if c, ok := store.Latest("Production", "f", true); !ok || c != on {
		t.Fatalf("Latest switch-on = %+v, %v; want the person's change", c, ok)
	}
}
