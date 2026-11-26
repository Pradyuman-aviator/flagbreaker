// Package changes receives Flagsmith's FLAG_UPDATED environment webhooks and
// remembers the latest change to each flag. Webhooks give the exact time of a
// change and who made it; polling alone only knows that the flag changed
// sometime since the last check. Webhooks are only a speed-up: if one is lost
// (Flagsmith retries just a few times), polling still sees the change.
package changes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// SignatureHeader carries the HMAC-SHA256 of the body, keyed with the
// webhook's secret, as lowercase hex.
const SignatureHeader = "X-Flagsmith-Signature"

// Change is a change to a flag's environment default.
type Change struct {
	At        time.Time
	Enabled   bool
	ChangedBy string
}

// key keeps switch-ons and switch-offs apart. Flagbreaker's own switch-off
// usually follows a person's switch-on within seconds, and Flagsmith sends
// webhooks a few seconds late, so a single "latest" slot would lose the
// person's change before it could be matched.
type key struct {
	environment, feature string
	enabled              bool
}

// Store holds the latest switch-on and switch-off per environment and flag.
type Store struct {
	mu     sync.Mutex
	latest map[key]Change
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{latest: map[key]Change{}} }

// Record keeps c if it is newer than what is stored.
func (s *Store) Record(environment, feature string, c Change) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{environment, feature, c.Enabled}
	if old, ok := s.latest[k]; !ok || c.At.After(old.At) {
		s.latest[k] = c
	}
}

// Latest returns the latest webhook-reported change that switched the flag
// to `enabled`.
func (s *Store) Latest(environment, feature string, enabled bool) (Change, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.latest[key{environment, feature, enabled}]
	return c, ok
}

// payload is the part of Flagsmith's webhook body that matters here.
type payload struct {
	EventType string `json:"event_type"`
	Data      struct {
		ChangedBy string        `json:"changed_by"`
		Timestamp string        `json:"timestamp"`
		NewState  *featureState `json:"new_state"`
	} `json:"data"`
}

type featureState struct {
	Enabled     bool `json:"enabled"`
	Environment struct {
		Name string `json:"name"`
	} `json:"environment"`
	Feature struct {
		Name string `json:"name"`
	} `json:"feature"`
	// Set for identity and segment overrides, which aren't the
	// environment default.
	Identity       any `json:"identity"`
	FeatureSegment any `json:"feature_segment"`
}

// timestampLayout matches Flagsmith's WEBHOOK_DATETIME_FORMAT.
const timestampLayout = "2006-01-02T15:04:05.999999Z"

// Handler verifies and stores FLAG_UPDATED webhooks, then calls onChange so
// the guards for that flag are checked right away.
func Handler(secret string, store *Store, onChange func(environment, feature string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "can't read body", http.StatusBadRequest)
			return
		}
		if !validSignature(secret, body, r.Header.Get(SignatureHeader)) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}

		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, "not JSON", http.StatusBadRequest)
			return
		}
		state := p.Data.NewState
		if p.EventType != "FLAG_UPDATED" || state == nil || state.Identity != nil || state.FeatureSegment != nil {
			w.WriteHeader(http.StatusNoContent) // not an environment-default change
			return
		}
		at, err := time.Parse(timestampLayout, p.Data.Timestamp)
		if err != nil {
			at = time.Now()
		}
		store.Record(state.Environment.Name, state.Feature.Name, Change{At: at, Enabled: state.Enabled, ChangedBy: p.Data.ChangedBy})
		if onChange != nil {
			onChange(state.Environment.Name, state.Feature.Name)
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func validSignature(secret string, body []byte, got string) bool {
	if secret == "" || got == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(got))
}
