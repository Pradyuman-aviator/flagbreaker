package settings

import (
	"strings"
	"testing"

	"github.com/Pradyuman-aviator/flagbreaker/api/v1alpha1"
)

func TestThePolicySetsTheErrorLimitPerFlag(t *testing.T) {
	s := FromFlags(flags(map[string]FlagValue{
		PolicyFlag: {Exists: true, Enabled: true,
			Value: `{"new_payments": {"mode": "act", "maxErrorPercent": 2}, "search": {"maxErrorPercent": 0.5}, "new_pricing": {"mode": "watch"}}`},
	}))
	if s.Problem != "" {
		t.Fatalf("a valid policy: %+v", s)
	}
	for flag, want := range map[string]float64{"new_payments": 2, "search": 0.5} {
		if got, ok := s.ErrorLimit(flag); !ok || got != want {
			t.Errorf("ErrorLimit(%s) = %g, %v; want %g", flag, got, ok, want)
		}
	}
	for _, flag := range []string{"new_pricing", "unknown"} {
		if _, ok := s.ErrorLimit(flag); ok {
			t.Errorf("%s has no limit in the policy", flag)
		}
	}
	if r := s.Resolve("new_payments", ""); r.Mode != v1alpha1.ModeAct {
		t.Errorf("the limit doesn't change the mode: %+v", r)
	}
}

func TestAnInvalidErrorLimitWatchesEveryFlag(t *testing.T) {
	for _, limit := range []string{"0", "-1", "101", `"5"`} {
		s := FromFlags(flags(map[string]FlagValue{
			ModeFlag:   {Exists: true, Enabled: true, Value: "act"},
			PolicyFlag: {Exists: true, Enabled: true, Value: `{"new_payments": {"mode": "act", "maxErrorPercent": ` + limit + `}}`},
		}))
		if s.Problem == "" || s.Mode != v1alpha1.ModeWatch || s.Policies != nil {
			t.Errorf("maxErrorPercent %s: got %+v, want an invalid policy and watch mode", limit, s)
		}
		if limit != `"5"` && !strings.Contains(s.Problem, "use a number above 0 and up to 100") {
			t.Errorf("maxErrorPercent %s: the problem should say what to use: %s", limit, s.Problem)
		}
		if _, ok := s.ErrorLimit("new_payments"); ok {
			t.Errorf("maxErrorPercent %s: an invalid policy sets no limit", limit)
		}
	}
}
