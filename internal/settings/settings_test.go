package settings

import (
	"strings"
	"testing"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

func flags(values map[string]FlagValue) func(string) FlagValue {
	return func(name string) FlagValue { return values[name] }
}

func TestTheMostCautiousSettingWins(t *testing.T) {
	act, watch := v1alpha1.ModeAct, v1alpha1.ModeWatch
	cases := []struct {
		name           string
		spec, global   v1alpha1.Mode
		policy         v1alpha1.Mode
		want           v1alpha1.Mode
		reasonContains string
	}{
		{"nothing says anything", "", "", "", watch, "no setting says act"},
		{"global act", "", act, "", act, "flagbreaker_mode"},
		{"spec act, global silent", act, "", "", act, "spec.mode"},
		{"spec act, global watch", act, watch, "", watch, "flagbreaker_mode in Flagsmith says watch"},
		{"global act, policy watch", "", act, watch, watch, "flagbreaker_policy for new_pricing says watch"},
		{"spec watch beats global act", watch, act, "", watch, "spec.mode says watch"},
		{"policy act, global act", "", act, act, act, "flagbreaker_policy"},
	}
	for _, c := range cases {
		s := Settings{Enabled: true, Mode: c.global}
		if c.policy != "" {
			s.Policies = map[string]FlagPolicy{"new_pricing": {Mode: c.policy}}
		}
		got := s.Resolve("new_pricing", c.spec)
		if got.Mode != c.want || got.Paused || !strings.Contains(got.Reason, c.reasonContains) {
			t.Errorf("%s: got %+v, want %s with a reason mentioning %q", c.name, got, c.want, c.reasonContains)
		}
	}
}

func TestPausing(t *testing.T) {
	if r := (Settings{Enabled: false, Mode: v1alpha1.ModeAct}).Resolve("x", v1alpha1.ModeAct); !r.Paused {
		t.Errorf("flagbreaker_enabled off should pause: %+v", r)
	}
	s := Settings{Enabled: true, Mode: v1alpha1.ModeAct, Policies: map[string]FlagPolicy{"db_migration": {Mode: ModeOff}}}
	if r := s.Resolve("db_migration", ""); !r.Paused || !strings.Contains(r.Reason, "db_migration to off") {
		t.Errorf("policy off should pause that flag: %+v", r)
	}
	if r := s.Resolve("new_pricing", ""); r.Paused || r.Mode != v1alpha1.ModeAct {
		t.Errorf("policy off for one flag must not touch another: %+v", r)
	}
}

func TestUnreachableSettingsForceWatch(t *testing.T) {
	s := Settings{Enabled: true, Unavailable: true, Source: "Flagsmith settings unreachable"}
	if r := s.Resolve("new_pricing", v1alpha1.ModeAct); r.Mode != v1alpha1.ModeWatch {
		t.Errorf("got %+v, want watch", r)
	}
}

func TestFromFlags(t *testing.T) {
	s := FromFlags(flags(map[string]FlagValue{
		EnabledFlag: {Exists: true, Enabled: true},
		ModeFlag:    {Exists: true, Enabled: true, Value: " act "},
		PolicyFlag:  {Exists: true, Enabled: true, Value: `{"new_pricing": {"mode": "watch"}}`},
	}))
	if !s.Enabled || s.Mode != v1alpha1.ModeAct || s.Policies["new_pricing"].Mode != v1alpha1.ModeWatch || s.Problem != "" {
		t.Fatalf("got %+v", s)
	}

	// A disabled mode flag says nothing; a missing enabled flag doesn't pause.
	s = FromFlags(flags(map[string]FlagValue{ModeFlag: {Exists: true, Enabled: false, Value: "act"}}))
	if !s.Enabled || s.Mode != "" {
		t.Fatalf("got %+v", s)
	}
}

func TestBrokenSettingsFallBackToWatch(t *testing.T) {
	s := FromFlags(flags(map[string]FlagValue{
		ModeFlag:   {Exists: true, Enabled: true, Value: "act"},
		PolicyFlag: {Exists: true, Enabled: true, Value: `{"new_pricing": `},
	}))
	if s.Mode != v1alpha1.ModeWatch || !strings.Contains(s.Problem, "not valid JSON") {
		t.Fatalf("invalid JSON: got %+v", s)
	}
	s = FromFlags(flags(map[string]FlagValue{PolicyFlag: {Exists: true, Enabled: true, Value: `{"x": {"mode": "yolo"}}`}}))
	if s.Mode != v1alpha1.ModeWatch || !strings.Contains(s.Problem, `"yolo"`) {
		t.Fatalf("unknown mode: got %+v", s)
	}
	s = FromFlags(flags(map[string]FlagValue{ModeFlag: {Exists: true, Enabled: true, Value: "maybe"}}))
	if s.Mode != v1alpha1.ModeWatch || s.Problem == "" {
		t.Fatalf("unknown flagbreaker_mode value: got %+v", s)
	}
}
