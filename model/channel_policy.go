package model

import (
	"strings"
	"sync"
	"time"

	"github.com/Laisky/zap"

	"github.com/Laisky/one-api/common/logger"
)

// CostClass describes the economics of a channel's upstream. It selects which
// automation policy applies: how aggressively the health engine may disable it,
// whether the background prober may spend requests on it, and how rate limits
// are handled.
//
// The health engine was designed for a fleet of paid, per-call-billed APIs. A
// single global policy mis-handles two other common regimes: quota-limited free
// tiers (where a probe spends scarce quota and a 429 means "wait until reset",
// not "broken") and self-hosted local models (where slow is normal and probing
// is free). CostClass lets one codebase serve all three without scattering
// special cases across controllers.
type CostClass string

const (
	// CostClassPaid is the default: billed per call. Full health automation.
	CostClassPaid CostClass = "paid"
	// CostClassFree is quota-limited (e.g. daily free tiers). Never disabled
	// for being slow or rate limited, and never probed proactively, because a
	// successful probe spends quota that real traffic needs.
	CostClassFree CostClass = "free"
	// CostClassLocal is self-hosted (e.g. llama.cpp). Slow responses are
	// expected and must not disable it; only a liveness failure or a fatal
	// error takes it out of rotation.
	CostClassLocal CostClass = "local"
)

// NormalizeCostClass maps an arbitrary stored value onto a known class. An
// empty or unknown value is treated as paid, preserving the behaviour of
// channels created before this field existed.
func NormalizeCostClass(value string) CostClass {
	switch CostClass(strings.ToLower(strings.TrimSpace(value))) {
	case CostClassFree:
		return CostClassFree
	case CostClassLocal:
		return CostClassLocal
	default:
		return CostClassPaid
	}
}

// AutomationPolicy is the single authority for how the automated channel
// management layers treat a channel. Every decision point (the unhealthy sweep,
// the background prober, the recovery tester, 429/5xx handling) reads its flags
// instead of branching on CostClass itself, so adding a class or changing a
// rule is a one-line change here rather than a hunt across controllers.
type AutomationPolicy struct {
	// ScoreMayDisable allows the composite health-score sweep to auto-disable
	// the channel, including for purely performance reasons (slow latency,
	// slow first token, low throughput).
	ScoreMayDisable bool
	// DisableOnRateLimit escalates repeated upstream 429s to auto-disable.
	DisableOnRateLimit bool
	// DisableOnStreamCut escalates repeated mid-stream cuts to auto-disable.
	DisableOnStreamCut bool
	// SuspendOnRateLimit parks the ability for a cooldown on 429 instead of
	// hammering a rate-limited upstream.
	SuspendOnRateLimit bool
	// SuspendOnServerError parks the ability on a 5xx upstream error. Local
	// channels disable this: a single erroring request from a self-hosted server
	// should fall back per-request, not lock the channel out for a backoff window
	// that ratchets up with every failure.
	SuspendOnServerError bool
	// RateLimitSuspendCap bounds the 429 cooldown when non-zero. Free tiers use
	// a short cap: a 429 costs no quota, so there is no reason to stay parked
	// for an hour before trying to claim quota again.
	RateLimitSuspendCap time.Duration
	// ProactiveProbe allows the background health prober to spend a request on
	// an idle channel. Free channels disable this to protect quota; their
	// recovery rides on real traffic instead.
	ProactiveProbe bool
	// AutoTestOnDisabled allows the periodic recovery test to spend a request on
	// a channel the system itself disabled.
	AutoTestOnDisabled bool
}

// PolicyFor returns the automation policy for a stored cost_class value.
func PolicyFor(costClass string) AutomationPolicy {
	switch NormalizeCostClass(costClass) {
	case CostClassLocal:
		return AutomationPolicy{
			ScoreMayDisable:      false,
			DisableOnRateLimit:   false,
			DisableOnStreamCut:   false,
			SuspendOnRateLimit:   false,
			SuspendOnServerError: false,
			ProactiveProbe:       true,
			AutoTestOnDisabled:   true,
		}
	case CostClassFree:
		return AutomationPolicy{
			ScoreMayDisable:      false,
			DisableOnRateLimit:   false,
			DisableOnStreamCut:   true,
			SuspendOnRateLimit:   true,
			SuspendOnServerError: true,
			RateLimitSuspendCap:  5 * time.Minute,
			ProactiveProbe:       false,
			AutoTestOnDisabled:   true,
		}
	default: // CostClassPaid
		return AutomationPolicy{
			ScoreMayDisable:      true,
			DisableOnRateLimit:   true,
			DisableOnStreamCut:   true,
			SuspendOnRateLimit:   true,
			SuspendOnServerError: true,
			ProactiveProbe:       true,
			AutoTestOnDisabled:   true,
		}
	}
}

// Policy is a convenience for callers holding a channel.
func (channel *Channel) Policy() AutomationPolicy {
	if channel == nil {
		return PolicyFor("")
	}
	return PolicyFor(channel.CostClass)
}

var backfillCostClassOnce sync.Once

// BackfillLocalCostClass marks IP-literal channels that have no explicit
// CostClass as local. Before CostClass existed, self-hosted nodes were already
// treated as liveness-only by the LAN supervisor, so this preserves that
// behaviour instead of suddenly judging them by the paid performance rules. It
// runs once and never overwrites a class an operator set explicitly.
func BackfillLocalCostClass() {
	backfillCostClassOnce.Do(func() {
		var channels []*Channel
		if err := DB.Find(&channels).Error; err != nil {
			logger.Logger.Warn("cost class backfill skipped: failed to load channels", zap.Error(err))
			return
		}
		updated := 0
		for _, ch := range channels {
			if ch == nil || strings.TrimSpace(ch.CostClass) != "" {
				continue
			}
			if !IsLocalIPChannel(ch) {
				continue
			}
			if err := DB.Model(&Channel{}).Where("id = ?", ch.Id).
				Update("cost_class", string(CostClassLocal)).Error; err != nil {
				logger.Logger.Warn("failed to backfill local cost class",
					zap.Int("channel_id", ch.Id), zap.Error(err))
				continue
			}
			updated++
		}
		if updated > 0 {
			logger.Logger.Info("backfilled local cost class for IP-literal channels",
				zap.Int("count", updated))
		}
	})
}
