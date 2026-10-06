package model

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/logger"
	"github.com/Laisky/zap"
)

// ---------------------------------------------------------------------------
// Channel health scoring
//
// The router must be able to answer one question for every candidate channel:
// "is this channel currently worth sending traffic to, and if several are, how
// much more does the operator want each one?"
//
// Health answers the first question as a single score in [0, 1] built from
// several independent signals; the channel's configured Weight only answers the
// second, and only among channels that already scored the same band. That
// separation is deliberate: a weight of 100 must never buy a channel immunity
// from being judged unhealthy.
//
// Signals, all exponentially weighted moving averages so a single blip cannot
// condemn a channel and a slow decay cannot rehabilitate one:
//
//	success     binary outcome rate (hard failures count as failures)
//	latency     full-request wall clock
//	ttft        time to first token, streaming only
//	tps         completion tokens divided by generation time
//	cut         streams that ended without a terminal finish_reason
//	rateLimit   upstream 429s, including the "fake" kind where the provider
//	            accepts the request but throttles throughput to a crawl
// ---------------------------------------------------------------------------

// OutcomeKind classifies a single relay observation.
type OutcomeKind string

const (
	// OutcomeSuccess is a relay that completed cleanly.
	OutcomeSuccess OutcomeKind = "success"
	// OutcomeStreamCut is a stream that ended without a terminal
	// finish_reason: HTTP 200, some tokens delivered, then silence. Treated
	// separately from a hard failure because it is the single most
	// damaging failure mode for agent traffic — the caller gets a
	// plausible-looking partial answer.
	OutcomeStreamCut OutcomeKind = "stream_cut"
	// OutcomeRateLimit is an upstream 429. Note that some providers
	// "fake" a rate limit by returning 200 and then streaming at ~1 tps;
	// the tps signal is what catches that, not this kind.
	OutcomeRateLimit OutcomeKind = "rate_limit"
	// OutcomeServerError is an upstream 5xx that is not a stream cut.
	OutcomeServerError OutcomeKind = "server_error"
	// OutcomeTimeout is an upstream timeout or deadline.
	OutcomeTimeout OutcomeKind = "timeout"
	// OutcomeAuthError is 401/403 or an authentication-class failure.
	OutcomeAuthError OutcomeKind = "auth_error"
	// OutcomeClientError is a 4xx attributable to the caller's request
	// (400/413 and friends). It says nothing about channel health, so it
	// is recorded for diagnostics but does not move the score.
	OutcomeClientError OutcomeKind = "client_error"
)

// ChannelObservation is one relay or probe result.
type ChannelObservation struct {
	// Kind classifies the outcome.
	Kind OutcomeKind
	// LatencyMs is the full request wall clock in milliseconds.
	LatencyMs float64
	// TTFTMs is the time to first token in milliseconds. Zero means the
	// response was not streamed or the measurement is unavailable, in which
	// case the ttft signal is left untouched.
	TTFTMs float64
	// GenerationMs is the span from first token to stream end. Combined with
	// CompletionTokens it yields the throughput signal.
	GenerationMs float64
	// CompletionTokens is the number of generated tokens, used for throughput.
	CompletionTokens int
}

// HealthBand is the coarse classification the router gates on.
type HealthBand string

const (
	// BandUnknown means fewer than ChannelHealthMinSamples observations have
	// been recorded. Treated as healthy so a newly added channel is not
	// penalised before it has had a chance to serve traffic.
	BandUnknown HealthBand = "unknown"
	// BandHealthy channels compete normally.
	BandHealthy HealthBand = "healthy"
	// BandDegraded channels are only used when no healthy candidate exists
	// for the same group/model/priority.
	BandDegraded HealthBand = "degraded"
	// BandUnhealthy channels are only used when nothing better exists at all.
	BandUnhealthy HealthBand = "unhealthy"
)

// HealthSnapshot is the externally visible view of a channel's health. It is
// served to the admin UI and is the same data the router gates on, so what an
// operator sees is exactly what is in effect.
type HealthSnapshot struct {
	ChannelId           int     `json:"channel_id"`
	Score               float64 `json:"score"`
	Band                string  `json:"band"`
	SuccessRate         float64 `json:"success_rate"`
	LatencyMs           float64 `json:"latency_ms"`
	TTFTMs              float64 `json:"ttft_ms"`
	TPS                 float64 `json:"tps"`
	CutRate             float64 `json:"cut_rate"`
	RateLimitRate       float64 `json:"rate_limit_rate"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	ConsecutiveProbes   int     `json:"consecutive_probes"`
	Samples             int     `json:"samples"`
	Suspended           bool    `json:"suspended"`
	SuspendUntil        int64   `json:"suspend_until"`
	UpdatedTime         int64   `json:"updated_time"`
	// LinkReachable/LinkRTTMs are the cached TCP link-probe result for
	// IP-literal upstreams. They are a hard selection gate and are never
	// folded into the latency score. LinkChecked is false until measured.
	LinkReachable bool    `json:"link_reachable"`
	LinkRTTMs     float64 `json:"link_rtt_ms"`
	LinkChecked   bool    `json:"link_checked"`
	// Reasons lists the human-readable reasons the score is not 1.0, so the
	// UI can explain a downgrade instead of just showing a number.
	Reasons []string `json:"reasons"`
}

// ewmaState holds the smoothed signals for a single channel.
type ewmaState struct {
	// successes is the ring buffer of recent binary outcomes. Retained
	// because ChannelHealthWindowSize drives both this and the legacy
	// sliding-window success rate.
	successes           []bool
	consecutiveFailures int

	successEwma       float64
	windowSuccessRate float64
	latencyEwma       float64
	ttftEwma          float64
	tpsEwma           float64
	cutEwma           float64
	rateLimitEwma     float64
	samples           int
	lastObservedAt    time.Time
	consecutivePass   int // consecutive clean probes, drives auto-recovery
}

// channelHealth is the per-channel health record. All access goes through
// h.mu; the map itself is guarded by channelHealthLock.
type channelHealth struct {
	mu sync.RWMutex
	ewmaState
}

var (
	// channelHealthStore tracks per-channel health for band gating and for
	// the admin UI. Entries are removed by gcChannelHealth once their
	// channel no longer exists and the record has gone idle.
	channelHealthStore = make(map[int]*channelHealth)
	channelHealthLock  sync.RWMutex
)

// getOrCreateChannelHealth returns the health record for a channel, creating one
// if it does not yet exist.
func getOrCreateChannelHealth(channelId int) *channelHealth {
	channelHealthLock.RLock()
	h, ok := channelHealthStore[channelId]
	channelHealthLock.RUnlock()
	if ok {
		return h
	}

	channelHealthLock.Lock()
	defer channelHealthLock.Unlock()
	h, ok = channelHealthStore[channelId]
	if ok {
		return h
	}
	h = &channelHealth{}
	h.successes = make([]bool, 0, config.ChannelHealthWindowSize)
	channelHealthStore[channelId] = h
	return h
}

// ewmaUpdate folds sample into prev. An unseen signal (prev <= 0) adopts the
// sample directly rather than decaying toward it, so the first observation is
// trusted in full instead of being diluted by an assumed zero.
func ewmaUpdate(prev float64, sample float64, alpha float64) float64 {
	if prev <= 0 {
		return sample
	}
	return prev*(1-alpha) + sample*alpha
}

// RecordChannelObservation folds one relay or probe result into a channel's
// health record.
func RecordChannelObservation(channelId int, obs ChannelObservation) {
	if channelId == 0 {
		return
	}
	h := getOrCreateChannelHealth(channelId)
	h.mu.Lock()
	defer h.mu.Unlock()

	alpha := config.ChannelHealthEWMAAlpha
	if alpha <= 0 || alpha > 1 {
		alpha = 0.3
	}

	h.samples++
	h.lastObservedAt = time.Now()

	// Binary outcome. Client errors are the caller's fault, not the
	// channel's, so they neither help nor hurt the success rate.
	isSuccess := obs.Kind == OutcomeSuccess
	switch obs.Kind {
	case OutcomeSuccess, OutcomeRateLimit, OutcomeServerError, OutcomeTimeout, OutcomeAuthError, OutcomeStreamCut:
		if len(h.successes) >= config.ChannelHealthWindowSize {
			h.successes = h.successes[1:]
		}
		h.successes = append(h.successes, isSuccess)
	}

	if obs.Kind == OutcomeSuccess {
		h.consecutiveFailures = 0
		h.consecutivePass++
	} else {
		h.consecutiveFailures++
		h.consecutivePass = 0
	}

	// Smoothed signals. Latency is recorded for both outcomes: a request
	// that failed after 20s is evidence the channel is slow even though it
	// did not succeed.
	if obs.LatencyMs > 0 {
		h.latencyEwma = ewmaUpdate(h.latencyEwma, obs.LatencyMs, alpha)
	}
	if obs.Kind == OutcomeSuccess {
		if obs.TTFTMs > 0 {
			h.ttftEwma = ewmaUpdate(h.ttftEwma, obs.TTFTMs, alpha)
		}
		// Throughput needs both a generation span and a token count; a
		// non-streaming response has neither and is skipped.
		if obs.GenerationMs > 0 && obs.CompletionTokens > 0 {
			tps := float64(obs.CompletionTokens) / (obs.GenerationMs / 1000.0)
			if tps > 0 {
				h.tpsEwma = ewmaUpdate(h.tpsEwma, tps, alpha)
			}
		}
	}

	switch obs.Kind {
	case OutcomeStreamCut:
		h.cutEwma = ewmaUpdate(h.cutEwma, 1.0, alpha)
		h.successEwma = ewmaUpdate(h.successEwma, 0.0, alpha)
	case OutcomeRateLimit:
		h.rateLimitEwma = ewmaUpdate(h.rateLimitEwma, 1.0, alpha)
		h.successEwma = ewmaUpdate(h.successEwma, 0.0, alpha)
	case OutcomeSuccess:
		h.cutEwma = ewmaUpdate(h.cutEwma, 0.0, alpha)
		h.rateLimitEwma = ewmaUpdate(h.rateLimitEwma, 0.0, alpha)
		h.successEwma = ewmaUpdate(h.successEwma, 1.0, alpha)
	default:
		h.successEwma = ewmaUpdate(h.successEwma, 0.0, alpha)
	}

	// Sliding-window success rate, kept for the legacy GetChannelHealthScore
	// contract and for diagnostics alongside the composite score.
	h.recomputeWindowSuccessLocked()
}

func (h *channelHealth) recomputeWindowSuccessLocked() {
	if len(h.successes) == 0 {
		return
	}
	var successCount int
	for _, s := range h.successes {
		if s {
			successCount++
		}
	}
	h.windowSuccessRate = float64(successCount) / float64(len(h.successes))
}

// RecordChannelSuccess records a successful relay for the given channel and
// resets its consecutive-failure counter.
func RecordChannelSuccess(channelId int) {
	RecordChannelObservation(channelId, ChannelObservation{Kind: OutcomeSuccess})
}

// RecordChannelFailure records a failed relay for the given channel and
// increments its consecutive-failure counter. Callers that know the failure
// mode should prefer RecordChannelObservation so the cut and rate-limit
// signals can be attributed correctly.
func RecordChannelFailure(channelId int) {
	RecordChannelObservation(channelId, ChannelObservation{Kind: OutcomeServerError})
}

// linearScore maps a "lower is better" signal onto [0, 1], where good returns
// 1.0 and bad returns 0.0, interpolating linearly in between. An unset signal
// is exempt and returns 1.0 rather than scoring as zero, so a mode that cannot
// produce the measurement is not treated as failing it.
func linearScore(value float64, good float64, bad float64) float64 {
	if value <= 0 {
		return 1.0
	}
	if good > 0 && value <= good {
		return 1.0
	}
	if bad <= 0 || good <= 0 || bad <= good {
		// A degenerate range carries no signal; treat it as exempt rather
		// than inventing a gradient between two identical bounds.
		return 1.0
	}
	if value >= bad {
		return 0.0
	}
	// Interpolate as the fraction of the way from good to bad, so good maps to
	// 1.0 and bad to 0.0. Writing this as 1-(value-bad)/(good-bad) looks
	// equivalent but inverts the gradient: both endpoint guards above would
	// still fire, and the midpoint would still land on 0.5, hiding the error
	// for every value in between.
	return 1.0 - (value-good)/(bad-good)
}

// increasingScore maps a "higher is better" signal onto [0, 1], interpolating
// between floor (0.0) and good (1.0).
//
// This is deliberately a separate function rather than a flag on linearScore:
// throughput inverts the ordering of the other two signals, and reusing one
// helper for both silently clamps the *good* end of an inverted range to zero,
// which would score a fast channel as the worst possible one.
func increasingScore(value float64, floor float64, good float64) float64 {
	if value <= 0 {
		return 1.0
	}
	if good > 0 && value >= good {
		return 1.0
	}
	if floor <= 0 || good <= floor {
		return 1.0
	}
	if value <= floor {
		return 0.0
	}
	return (value - floor) / (good - floor)
}

// healthScoreLocked computes the composite score and the reasons behind it.
// Caller must hold h.mu (read or write).
func (h *channelHealth) healthScoreLocked() (float64, []string) {
	var reasons []string

	// Not enough evidence yet. Report a neutral score and no reasons rather
	// than a confident-looking 1.0.
	if h.samples < config.ChannelHealthMinSamples {
		return 1.0, nil
	}

	base := h.successEwma
	if base <= 0 {
		// A channel whose every recent observation failed is at the floor
		// regardless of how fast it answers.
		return 0.0, []string{"no successful observations in window"}
	}
	if h.windowSuccessRate > 0 {
		// Prefer the sliding window for the success term when available:
		// it is windowed exactly like the rest of the state and does not
		// decay once every failure has aged out of a short history.
		base = h.windowSuccessRate
	}

	latencyM := linearScore(h.latencyEwma, config.ChannelHealthLatencyFastMs, config.ChannelHealthLatencySlowMs)
	if latencyM < 1.0 {
		reasons = append(reasons, "slow responses")
	}

	ttftM := linearScore(h.ttftEwma, config.ChannelHealthTTFTFastMs, config.ChannelHealthTTFTSlowMs)
	if ttftM < 1.0 {
		reasons = append(reasons, "slow first token")
	}

	tpsM := increasingScore(h.tpsEwma, config.ChannelHealthTPSFloor, config.ChannelHealthTPSGood)
	if tpsM < 1.0 {
		reasons = append(reasons, "low throughput")
	}

	// Time to first token is a component of total latency, so multiplying both
	// would penalise a uniformly slow channel twice for one underlying fault.
	// Taking the worse of the two keeps the signal without the double count.
	responsiveM := latencyM
	if ttftM < responsiveM {
		responsiveM = ttftM
	}

	// Each factor keeps a floor, so one poor dimension reduces a channel's
	// traffic share without starving it outright. That is the intended
	// behaviour for a merely sluggish channel: it is downranked, and the
	// router prefers livelier peers, but it is still used when nothing better
	// exists. Genuinely broken channels are excluded by the success-rate gate
	// above and by the cut/rate-limit penalties below, not by slow quality.
	quality := (0.5 + 0.5*responsiveM) * (0.6 + 0.4*tpsM)

	penalty := 1.0
	if h.cutEwma > 0 {
		penalty -= config.ChannelHealthStreamCutPenalty * h.cutEwma
		reasons = append(reasons, "streams cut mid-response")
	}
	if h.rateLimitEwma > 0 {
		penalty -= config.ChannelHealthRateLimitPenalty * h.rateLimitEwma
		reasons = append(reasons, "rate limited upstream")
	}
	if penalty < 0 {
		penalty = 0
	}

	score := base * quality * penalty
	if score < 0 {
		score = 0
	}
	if score > 1 {
		score = 1
	}
	return score, reasons
}

// bandForScore maps a composite score onto a band.
func bandForScore(score float64, samples int) HealthBand {
	if samples < config.ChannelHealthMinSamples {
		return BandUnknown
	}
	if score >= config.ChannelHealthThreshold {
		return BandHealthy
	}
	if score >= config.ChannelHealthDegradedThreshold {
		return BandDegraded
	}
	return BandUnhealthy
}

// GetChannelHealthScore returns the composite health score in [0, 1].
// 1.0 means healthy; 0 means the router should route around this channel.
// Unlike the old success-ratio implementation this folds in latency, time to
// first token, throughput, stream cuts and upstream rate limiting.
func GetChannelHealthScore(channelId int) float64 {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	score, _ := h.healthScoreLocked()
	return score
}

// GetChannelHealthBand returns the band the router gates on for this channel.
func GetChannelHealthBand(channelId int) HealthBand {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	score, _ := h.healthScoreLocked()
	return bandForScore(score, h.samples)
}

// GetChannelHealthSnapshot returns the full health record for the admin UI.
func GetChannelHealthSnapshot(channelId int) HealthSnapshot {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	snapshot := h.snapshotLocked(channelId)
	applyLinkSnapshot(&snapshot, channelId)
	return snapshot
}

func (h *channelHealth) snapshotLocked(channelId int) HealthSnapshot {
	score, reasons := h.healthScoreLocked()
	snapshot := HealthSnapshot{
		ChannelId:           channelId,
		Score:               round2(score),
		Band:                string(bandForScore(score, h.samples)),
		SuccessRate:         round2(h.windowSuccessRate),
		LatencyMs:           round2(h.latencyEwma),
		ConsecutiveFailures: h.consecutiveFailures,
		ConsecutiveProbes:   h.consecutivePass,
		Samples:             h.samples,
		UpdatedTime:         lastObservedUnix(h.lastObservedAt),
	}
	if h.ttftEwma > 0 {
		snapshot.TTFTMs = round2(h.ttftEwma)
	}
	if h.tpsEwma > 0 {
		snapshot.TPS = round2(h.tpsEwma)
	}
	snapshot.CutRate = round2(h.cutEwma)
	snapshot.RateLimitRate = round2(h.rateLimitEwma)

	// Surface the live circuit-breaker state so the UI can explain why a
	// channel is temporarily absent from selection even when its score is
	// fine.
	if until, ok := suspendedUntil(channelId); ok {
		snapshot.Suspended = true
		snapshot.SuspendUntil = until.Unix()
	}

	if reasons == nil {
		reasons = []string{}
	}
	snapshot.Reasons = reasons
	return snapshot
}

func lastObservedUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ChannelHealthSnapshots returns a snapshot for every channel with recorded
// observations, sorted worst-first so the admin UI leads with problems.
func ChannelHealthSnapshots() []HealthSnapshot {
	channelHealthLock.RLock()
	ids := make([]int, 0, len(channelHealthStore))
	for id := range channelHealthStore {
		ids = append(ids, id)
	}
	channelHealthLock.RUnlock()

	snapshots := make([]HealthSnapshot, 0, len(ids))
	for _, id := range ids {
		h := getOrCreateChannelHealth(id)
		h.mu.RLock()
		snapshot := h.snapshotLocked(id)
		h.mu.RUnlock()
		if snapshot.Samples == 0 {
			continue
		}
		applyLinkSnapshot(&snapshot, id)
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].Score != snapshots[j].Score {
			return snapshots[i].Score < snapshots[j].Score
		}
		return snapshots[i].ChannelId < snapshots[j].ChannelId
	})
	return snapshots
}

// GetConsecutiveChannelFailures returns the number of consecutive failures
// for the given channel since its last success.
func GetConsecutiveChannelFailures(channelId int) int {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.consecutiveFailures
}

// ResetConsecutiveChannelFailures resets the consecutive-failure counter.
func ResetConsecutiveChannelFailures(channelId int) {
	h := getOrCreateChannelHealth(channelId)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.consecutiveFailures = 0
}

// ResetChannelHealthToFull clears a channel's accumulated health history and
// returns it to a clean, fully-healthy state. It is called when an
// auto-disabled channel answers a recovery probe, so the score reflects the
// live service rather than the failures that caused the disable.
//
// Everything is cleared, including the latency/throughput EWMAs: the goal is a
// fresh verdict from the next observations, not a half-buried grudge. The
// sample counter is kept at or above the floor so the channel reads healthy
// rather than unknown; the selector groups unknown with degraded, which would
// keep a freshly recovered channel out of the winning pool.
func ResetChannelHealthToFull(channelId int) {
	if channelId == 0 {
		return
	}
	h := getOrCreateChannelHealth(channelId)
	h.mu.Lock()
	defer h.mu.Unlock()

	h.successes = h.successes[:0]
	h.consecutiveFailures = 0
	h.consecutivePass = 0
	h.successEwma = 1.0
	h.windowSuccessRate = 1.0
	h.latencyEwma = 0
	h.ttftEwma = 0
	h.tpsEwma = 0
	h.cutEwma = 0
	h.rateLimitEwma = 0
	if h.samples < config.ChannelHealthMinSamples {
		h.samples = config.ChannelHealthMinSamples
	}
	h.lastObservedAt = time.Now()
}

// GetChannelHealthScoreBelowThreshold reports whether an enabled channel's
// composite score has fallen below threshold with enough observations to trust
// it. Used by the enforcement loop that auto-disables sleeping channels so
// they enter the recovery probe cycle instead of sitting skipped forever.
func GetChannelHealthScoreBelowThreshold(channelId int, threshold float64) bool {
	if threshold <= 0 {
		return false
	}
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.samples < config.ChannelHealthMinSamples {
		return false
	}
	score, _ := h.healthScoreLocked()
	return score < threshold
}

// ConsecutiveCleanProbes returns how many clean probes in a row this channel
// has recorded. Used as the auto-recovery gate.
func ConsecutiveCleanProbes(channelId int) int {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.consecutivePass
}

// ShouldAutoRecoverChannel reports whether an auto-disabled channel has earned
// its way back into rotation: it must have cleared the degraded threshold and
// served enough consecutive clean probes.
//
// This is intentionally stricter than monitor.ShouldEnableChannel, which
// re-enables on any single clean test.
func ShouldAutoRecoverChannel(channelId int) (bool, string) {
	if !config.ChannelAutoRecoverEnabled {
		return false, "auto recover disabled"
	}
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	score, _ := h.healthScoreLocked()
	passes := h.consecutivePass
	samples := h.samples
	h.mu.RUnlock()

	if samples < config.ChannelHealthMinSamples {
		return false, "not enough observations"
	}
	if score < config.ChannelHealthDegradedThreshold {
		return false, "score still below degraded threshold"
	}
	required := config.ChannelAutoRecoverConsecutive
	if required <= 0 {
		required = 1
	}
	if passes < required {
		return false, "waiting for consecutive clean probes"
	}
	return true, "recovered after clean probes"
}

// ChannelNeedsProbe reports whether a channel's health record is stale enough
// to justify spending a probe request on it. Channels receiving live traffic
// keep themselves fresh, so probing them would be wasted quota.
func ChannelNeedsProbe(channelId int) bool {
	h := getOrCreateChannelHealth(channelId)
	h.mu.RLock()
	defer h.mu.RUnlock()

	// Anything the circuit breaker has already removed is not worth
	// probing: the backoff schedule will hand it back when it is ready.
	if h.consecutiveFailures > 0 && IsChannelSuspendedInCache(channelId) {
		return false
	}
	if h.samples == 0 {
		return true
	}
	idle := time.Since(h.lastObservedAt)
	return idle >= time.Duration(config.ChannelHealthProbeIdleMin)*time.Minute
}

// ClassifyRelayError maps a relay failure onto an OutcomeKind so the health
// engine can attribute it. ok is false when the failure says nothing about
// channel health (a client error) and the observation should not move the
// score.
func ClassifyRelayError(statusCode int, errCode string, errMessage string, isStreamCut bool) (OutcomeKind, bool) {
	if isStreamCut {
		return OutcomeStreamCut, true
	}

	// Check the error code before the status class. A 403 carrying
	// insufficient_user_quota is the caller's own quota running out, not the
	// channel rejecting our credentials; treating it as an auth failure would
	// have the health engine disable channels for something one-api itself
	// caused.
	lowerCode := strings.ToLower(strings.TrimSpace(errCode))
	if lowerCode == "insufficient_user_quota" {
		return OutcomeClientError, false
	}

	switch {
	case statusCode == 401 || statusCode == 403:
		return OutcomeAuthError, true
	case statusCode == 429:
		return OutcomeRateLimit, true
	case statusCode == 408:
		return OutcomeTimeout, true
	case statusCode >= 500:
		return OutcomeServerError, true
	}

	// A 200-class response can still signal a dead channel when the body
	// carries a provider error object.
	lowerMsg := strings.ToLower(errMessage)
	switch {
	case strings.Contains(lowerCode, "upstream_cut"):
		return OutcomeStreamCut, true
	case strings.Contains(lowerMsg, "insufficient_user_quota"):
		return OutcomeClientError, false
	case strings.Contains(lowerMsg, "rate limit") || strings.Contains(lowerMsg, "rate_limit"):
		return OutcomeRateLimit, true
	case strings.Contains(lowerMsg, "context deadline exceeded") ||
		strings.Contains(lowerMsg, "timeout") || strings.Contains(lowerMsg, "timed out"):
		return OutcomeTimeout, true
	case strings.Contains(lowerMsg, "invalid_api_key") ||
		strings.Contains(lowerMsg, "unauthorized") ||
		strings.Contains(lowerMsg, "api key"):
		return OutcomeAuthError, true
	case statusCode >= 400 && statusCode < 500:
		return OutcomeClientError, false
	}
	return OutcomeServerError, true
}

// gcChannelHealth drops health records for channels that no longer exist and
// removes records that have gone completely idle. Without this the map grows
// for the lifetime of the process as channels are created and deleted.
func gcChannelHealth() {
	if DB == nil {
		return
	}

	var liveIds []int
	if err := DB.Model(&Channel{}).Pluck("id", &liveIds).Error; err != nil {
		// A failed lookup must not wipe the store; skip this round.
		return
	}
	live := make(map[int]struct{}, len(liveIds))
	for _, id := range liveIds {
		live[id] = struct{}{}
	}

	channelHealthLock.Lock()
	defer channelHealthLock.Unlock()
	for id := range channelHealthStore {
		if _, ok := live[id]; !ok {
			delete(channelHealthStore, id)
			logger.Logger.Debug("gc channel health record for deleted channel", zap.Int("channel_id", id))
		}
	}
}

// StartChannelHealthJanitor runs the periodic in-memory cleanup for the health
// and suspension stores. Call it once from main.
func StartChannelHealthJanitor() {
	go func() {
		for {
			time.Sleep(30 * time.Second)
			removeExpiredSuspensions()
			gcChannelHealth()
		}
	}()
}
