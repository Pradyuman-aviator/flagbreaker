// Package settings reads Flagbreaker's own settings from Flagsmith flags, with
// the official Flagsmith Go SDK:
//
//   - flagbreaker_enabled: off pauses Flagbreaker everywhere.
//   - flagbreaker_mode: its value, "watch" or "act", applies to every flag.
//   - flagbreaker_policy: a JSON value with a mode per flag, for example
//     {"new_pricing": {"mode": "watch"}, "db_migration": {"mode": "off"}},
//     and, for guards that read requests, the share of failed requests that
//     counts as failing: {"new_payments": {"mode": "act", "maxErrorPercent": 2}}.
//
// So the breaker is controlled from the same dashboard as the flags it
// guards. Whenever the settings can't be read, Flagbreaker falls back to
// watch mode, which never changes a flag.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	flagsmith "github.com/Flagsmith/flagsmith-go-client/v5"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

const (
	EnabledFlag = "flagbreaker_enabled"
	ModeFlag    = "flagbreaker_mode"
	PolicyFlag  = "flagbreaker_policy"

	// ModeOff, in flagbreaker_policy, pauses Flagbreaker for one flag.
	ModeOff v1alpha1.Mode = "off"
)

// FlagPolicy is one flag's entry in flagbreaker_policy.
type FlagPolicy struct {
	Mode v1alpha1.Mode `json:"mode"`
	// MaxErrorPercent replaces spec.requests.maxErrorPercent of the flag's
	// guards. It never turns request reading on by itself.
	MaxErrorPercent *float64 `json:"maxErrorPercent,omitempty"`
}

// Settings is what Flagbreaker should do right now.
type Settings struct {
	// Enabled false pauses Flagbreaker.
	Enabled bool
	// Mode is flagbreaker_mode, or empty when it says nothing.
	Mode v1alpha1.Mode
	// Policies are the per-flag modes from flagbreaker_policy.
	Policies map[string]FlagPolicy
	// Source says where the settings came from, for status messages.
	Source string
	// Unavailable is true when Flagsmith couldn't be read, so watch mode is
	// forced.
	Unavailable bool
	// Problem describes a setting that couldn't be used, such as invalid JSON.
	Problem string
}

// Resolution is the mode a FlagGuard runs in, and why.
type Resolution struct {
	// Paused means Flagbreaker changes nothing for this flag.
	Paused bool
	Mode   v1alpha1.Mode
	Reason string
}

// Resolve picks the mode for one flag. Flagbreaker switches a flag off only
// when something says act and nothing says watch: the FlagGuard's spec.mode,
// the flag's entry in flagbreaker_policy and flagbreaker_mode are all asked,
// and the most cautious answer wins. With no answer at all, it watches.
func (s Settings) Resolve(flag string, specMode v1alpha1.Mode) Resolution {
	if !s.Enabled {
		return Resolution{Paused: true, Mode: v1alpha1.ModeWatch, Reason: EnabledFlag + " is off in Flagsmith"}
	}
	if s.Unavailable {
		return Resolution{Mode: v1alpha1.ModeWatch, Reason: s.Source}
	}
	policy := s.Policies[flag].Mode
	if policy == ModeOff {
		return Resolution{Paused: true, Mode: v1alpha1.ModeWatch, Reason: fmt.Sprintf("%s sets %s to off", PolicyFlag, flag)}
	}

	type source struct {
		mode v1alpha1.Mode
		name string
	}
	sources := []source{
		{specMode, "the FlagGuard's spec.mode"},
		{policy, fmt.Sprintf("%s for %s", PolicyFlag, flag)},
		{s.Mode, ModeFlag + " in Flagsmith"},
	}
	var act *source
	for i := range sources {
		switch sources[i].mode {
		case v1alpha1.ModeWatch:
			return Resolution{Mode: v1alpha1.ModeWatch, Reason: sources[i].name + " says watch"}
		case v1alpha1.ModeAct:
			if act == nil {
				act = &sources[i]
			}
		}
	}
	if act != nil {
		return Resolution{Mode: v1alpha1.ModeAct, Reason: act.name + " says act and nothing says watch"}
	}
	return Resolution{Mode: v1alpha1.ModeWatch, Reason: "no setting says act, so watch"}
}

// ErrorLimit returns the maxErrorPercent flagbreaker_policy sets for a flag,
// if it sets one. It replaces spec.requests.maxErrorPercent for that flag.
func (s Settings) ErrorLimit(flag string) (float64, bool) {
	if p, ok := s.Policies[flag]; ok && p.MaxErrorPercent != nil {
		return *p.MaxErrorPercent, true
	}
	return 0, false
}

// Reader caches the settings for a few seconds.
type Reader struct {
	client *flagsmith.Client
	ttl    time.Duration

	mu        sync.Mutex
	cached    Settings
	fetchedAt time.Time
}

// NewReader reads settings from the Flagsmith environment with the given key.
// With an empty key, Flagbreaker uses each FlagGuard's spec.mode, and watches
// when a FlagGuard sets none.
func NewReader(apiURL, environmentKey string, ttl time.Duration) *Reader {
	r := &Reader{ttl: ttl}
	if environmentKey != "" {
		r.client = flagsmith.NewClient(environmentKey,
			flagsmith.WithBaseURL(strings.TrimRight(apiURL, "/")+"/api/v1/"),
			flagsmith.WithRequestTimeout(5*time.Second),
		)
	}
	return r
}

// Get returns the current settings.
func (r *Reader) Get(ctx context.Context) Settings {
	if r == nil || r.client == nil {
		return Settings{Enabled: true, Source: "no settings environment configured"}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.fetchedAt.IsZero() && time.Since(r.fetchedAt) < r.ttl {
		return r.cached
	}
	r.cached = r.fetch(ctx)
	r.fetchedAt = time.Now()
	return r.cached
}

func (r *Reader) fetch(ctx context.Context) Settings {
	flags, err := r.client.GetEnvironmentFlags(ctx)
	if err != nil {
		return Settings{Enabled: true, Unavailable: true,
			Source: fmt.Sprintf("Flagsmith settings unreachable (%v), so watch mode", err)}
	}
	return FromFlags(lookup(flags))
}

// FlagValue is what a flag serves, as the settings see it.
type FlagValue struct {
	Exists  bool
	Enabled bool
	Value   string
}

// FromFlags turns the breaker's flags into settings. A flag that doesn't
// exist says nothing.
func FromFlags(get func(name string) FlagValue) Settings {
	s := Settings{Enabled: true, Source: "Flagsmith"}
	if f := get(EnabledFlag); f.Exists {
		s.Enabled = f.Enabled
	}
	if f := get(ModeFlag); f.Exists && f.Enabled {
		switch m := v1alpha1.Mode(strings.TrimSpace(f.Value)); m {
		case v1alpha1.ModeAct, v1alpha1.ModeWatch:
			s.Mode = m
		default:
			// An unknown value is treated as watch, the safe side.
			s.Mode = v1alpha1.ModeWatch
			s.Problem = fmt.Sprintf("%s has the value %q; use watch or act", ModeFlag, f.Value)
		}
	}
	if f := get(PolicyFlag); f.Exists && f.Enabled && strings.TrimSpace(f.Value) != "" {
		policies, err := ParsePolicy(f.Value)
		if err != nil {
			// A broken policy can't say which flags it meant, so every flag
			// is watched until it is fixed.
			s.Mode = v1alpha1.ModeWatch
			s.Problem = err.Error()
		}
		s.Policies = policies
	}
	return s
}

// ParsePolicy reads flagbreaker_policy's JSON value.
func ParsePolicy(value string) (map[string]FlagPolicy, error) {
	var policies map[string]FlagPolicy
	if err := json.Unmarshal([]byte(value), &policies); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON (%v), so every flag is watched", PolicyFlag, err)
	}
	for flag, p := range policies {
		switch p.Mode {
		case v1alpha1.ModeAct, v1alpha1.ModeWatch, ModeOff, "":
		default:
			return nil, fmt.Errorf("%s gives %s the mode %q; use watch, act or off. Every flag is watched until it is fixed", PolicyFlag, flag, p.Mode)
		}
		if l := p.MaxErrorPercent; l != nil && !(*l > 0 && *l <= 100) {
			return nil, fmt.Errorf("%s gives %s the maxErrorPercent %g; use a number above 0 and up to 100. Every flag is watched until it is fixed", PolicyFlag, flag, *l)
		}
	}
	return policies, nil
}

func lookup(flags flagsmith.Flags) func(string) FlagValue {
	return func(name string) FlagValue {
		f, err := flags.GetFlag(name)
		if err != nil || f.IsDefault {
			return FlagValue{}
		}
		v := FlagValue{Exists: true, Enabled: f.Enabled}
		switch value := f.Value.(type) {
		case string:
			v.Value = value
		case nil:
		default:
			v.Value = fmt.Sprint(value)
		}
		return v
	}
}
