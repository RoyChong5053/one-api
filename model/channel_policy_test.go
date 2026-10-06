package model

import (
	"testing"
	"time"
)

func TestNormalizeCostClass(t *testing.T) {
	cases := map[string]CostClass{
		"":        CostClassPaid,
		"paid":    CostClassPaid,
		"PAID":    CostClassPaid,
		"free":    CostClassFree,
		" Free ":  CostClassFree,
		"local":   CostClassLocal,
		"LOCAL":   CostClassLocal,
		"unknown": CostClassPaid,
	}
	for input, want := range cases {
		if got := NormalizeCostClass(input); got != want {
			t.Errorf("NormalizeCostClass(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPolicyForPaidMatchesLegacyBehaviour(t *testing.T) {
	p := PolicyFor("")
	if !p.ScoreMayDisable || !p.DisableOnRateLimit || !p.DisableOnStreamCut ||
		!p.SuspendOnRateLimit || !p.SuspendOnServerError || !p.ProactiveProbe || !p.AutoTestOnDisabled {
		t.Fatalf("paid policy must keep every automation enabled, got %+v", p)
	}
	if p.RateLimitSuspendCap != 0 {
		t.Fatalf("paid 429 cooldown must be uncapped (config-driven), got %v", p.RateLimitSuspendCap)
	}
}

func TestPolicyForFreeProtectsQuota(t *testing.T) {
	p := PolicyFor("free")
	if p.ScoreMayDisable {
		t.Error("free channel must not be auto-disabled by the health score")
	}
	if p.DisableOnRateLimit {
		t.Error("free channel must not be disabled for exhausting its quota")
	}
	if p.ProactiveProbe {
		t.Error("free channel must not be probed proactively (burns quota)")
	}
	if !p.SuspendOnRateLimit {
		t.Error("free channel should park briefly on 429")
	}
	if p.RateLimitSuspendCap != 5*time.Minute {
		t.Errorf("free 429 cap = %v, want 5m", p.RateLimitSuspendCap)
	}
	if !p.AutoTestOnDisabled {
		t.Error("free channel may still be recovered by the periodic test")
	}
}

func TestPolicyForLocalStaysLivenessOnly(t *testing.T) {
	p := PolicyFor("local")
	if p.ScoreMayDisable || p.DisableOnRateLimit || p.DisableOnStreamCut {
		t.Error("local channel must not be auto-disabled for performance/rate limits/cuts")
	}
	if p.SuspendOnRateLimit || p.SuspendOnServerError {
		t.Error("local channel must not be suspended across requests; fall back per-request instead")
	}
	if !p.ProactiveProbe {
		t.Error("probing a local channel is free, it should stay enabled")
	}
	if !p.AutoTestOnDisabled {
		t.Error("local channel should be recoverable by the periodic test")
	}
}
