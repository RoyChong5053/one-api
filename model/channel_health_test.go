package model

import (
	"testing"
	"time"

	"github.com/Laisky/one-api/common/config"
)

// resetChannelHealthForTest clears the in-memory health store so each test
// starts from a clean slate.
func resetChannelHealthForTest(t *testing.T) {
	t.Helper()
	channelHealthLock.Lock()
	channelHealthStore = make(map[int]*channelHealth)
	channelHealthLock.Unlock()
	suspendedChannelsMu.Lock()
	suspendedChannels = make(map[int]time.Time)
	suspendedChannelsMu.Unlock()
}

// recordSuccess feeds a healthy observation with the given latency.
func recordSuccess(t *testing.T, channelId int, latencyMs float64) {
	t.Helper()
	RecordChannelObservation(channelId, ChannelObservation{
		Kind:             OutcomeSuccess,
		LatencyMs:        latencyMs,
		TTFTMs:           latencyMs / 4,
		GenerationMs:     latencyMs * 3 / 4,
		CompletionTokens: int(latencyMs / 10),
	})
}

// TestHealthScoreNeedsMinimumSamples asserts that a channel with too little
// evidence is reported as unknown rather than confidently healthy. Without this
// gate a brand new channel and a proven-broken one look identical.
func TestHealthScoreNeedsMinimumSamples(t *testing.T) {
	resetChannelHealthForTest(t)

	if band := GetChannelHealthBand(1); band != BandUnknown {
		t.Fatalf("no observations should be unknown, got %s", band)
	}
	if score := GetChannelHealthScore(1); score != 1.0 {
		t.Fatalf("unknown channel should score 1.0, got %v", score)
	}

	// Below the sample floor the band must stay unknown even after failures.
	for i := 0; i < config.ChannelHealthMinSamples-1; i++ {
		RecordChannelObservation(1, ChannelObservation{Kind: OutcomeServerError, LatencyMs: 9000})
	}
	if band := GetChannelHealthBand(1); band != BandUnknown {
		t.Fatalf("under-sampled channel should be unknown, got %s", band)
	}

	// Crossing the floor must resolve to a real verdict.
	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeServerError, LatencyMs: 9000})
	if band := GetChannelHealthBand(1); band == BandUnknown {
		t.Fatal("channel past the sample floor must be classified")
	}
}

// TestSlowChannelIsDegradedNotFatal asserts the behaviour the operator asked
// for directly: a channel answering in eight seconds should lose priority but
// still be usable, not be taken out of rotation.
func TestSlowChannelIsDegradedNotFatal(t *testing.T) {
	resetChannelHealthForTest(t)

	fast, slow := 1, 2
	for i := 0; i < 10; i++ {
		recordSuccess(t, fast, 400)
		recordSuccess(t, slow, 8000)
	}

	fastScore := GetChannelHealthScore(fast)
	slowScore := GetChannelHealthScore(slow)
	if slowScore >= fastScore {
		t.Fatalf("slow channel (%.3f) should score below fast (%.3f)", slowScore, fastScore)
	}
	if GetChannelHealthBand(fast) != BandHealthy {
		t.Fatalf("a responsive channel must be healthy, got %s (%.3f)", GetChannelHealthBand(fast), fastScore)
	}
	// The operator asked for a slow channel to be downweighted, not removed
	// from rotation, so an 8s responder has to land in the degraded band.
	if GetChannelHealthBand(slow) != BandDegraded {
		t.Fatalf("an 8s channel should be degraded (downweighted), got %s (%.3f)",
			GetChannelHealthBand(slow), slowScore)
	}
}

func TestIncreasingScoreInterpolation(t *testing.T) {
	cases := []struct {
		name  string
		value float64
		floor float64
		good  float64
		want  float64
	}{
		{"unavailable signal is exempt", 0, 5, 20, 1.0},
		{"at or above good", 50, 5, 20, 1.0},
		{"at or below floor", 2, 5, 20, 0.0},
		{"midpoint", 12.5, 5, 20, 0.5},
		{"three quarters", 16.25, 5, 20, 0.75},
		{"degenerate floor==good is exempt", 10, 5, 5, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := increasingScore(tc.value, tc.floor, tc.good)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("increasingScore(%v, %v, %v) = %v, want %v", tc.value, tc.floor, tc.good, got, tc.want)
			}
		})
	}
}

// TestStreamCutsArePenalisedSeparately asserts that streams cut after the
// first token degrade a channel hard even though each request technically
// "succeeded" from the transport's point of view.
func TestStreamCutsArePenalisedSeparately(t *testing.T) {
	resetChannelHealthForTest(t)

	clean, cut := 1, 2
	for i := 0; i < 10; i++ {
		recordSuccess(t, clean, 500)
		RecordChannelObservation(cut, ChannelObservation{
			Kind:      OutcomeStreamCut,
			LatencyMs: 500,
		})
	}

	if GetChannelHealthBand(cut) != BandUnhealthy {
		t.Fatalf("a channel cutting every stream must be unhealthy, got %s (score %.3f)",
			GetChannelHealthBand(cut), GetChannelHealthScore(cut))
	}
	if GetChannelHealthBand(clean) != BandHealthy {
		t.Fatalf("clean channel should stay healthy, got %s", GetChannelHealthBand(clean))
	}
}

// TestLowThroughputIsDetected covers the "fake rate limit" case: HTTP 200,
// tokens delivered, but at roughly one per second.
func TestLowThroughputIsDetected(t *testing.T) {
	resetChannelHealthForTest(t)

	fast, slow := 1, 2
	for i := 0; i < 10; i++ {
		// 200 tokens in 1s = 200 tps
		RecordChannelObservation(fast, ChannelObservation{
			Kind: OutcomeSuccess, LatencyMs: 1000, TTFTMs: 100,
			GenerationMs: 1000, CompletionTokens: 200,
		})
		// 20 tokens in 20s = 1 tps, latency still "fine" for a short answer
		RecordChannelObservation(slow, ChannelObservation{
			Kind: OutcomeSuccess, LatencyMs: 1000, TTFTMs: 200,
			GenerationMs: 20000, CompletionTokens: 20,
		})
	}

	fastScore := GetChannelHealthScore(fast)
	slowScore := GetChannelHealthScore(slow)
	if slowScore >= fastScore {
		t.Fatalf("1 tps channel (%.3f) should score below 200 tps channel (%.3f)", slowScore, fastScore)
	}

	snapshot := GetChannelHealthSnapshot(slow)
	// The whole point is that a 1 tps channel is recorded as such, so that the
	// throughput term has something to act on.
	if diff := snapshot.TPS - 1.0; diff > 0.1 || diff < -0.1 {
		t.Fatalf("expected the 1 tps channel to be recorded as ~1 tps, got %v", snapshot.TPS)
	}
	if snapshot.TPS >= config.ChannelHealthTPSGood {
		t.Fatalf("1 tps must not clear the good-throughput threshold, got %v", snapshot.TPS)
	}
	if len(snapshot.Reasons) == 0 {
		t.Fatal("a throughput-collapse should report at least one reason")
	}
	t.Logf("1tps channel: score=%.3f band=%s reasons=%v", snapshot.Score, snapshot.Band, snapshot.Reasons)
}

// TestClientErrorsDoNotDegradeHealth asserts a malformed caller request never
// counts against the channel it was sent to.
func TestClientErrorsDoNotDegradeHealth(t *testing.T) {
	resetChannelHealthForTest(t)

	for i := 0; i < 20; i++ {
		recordSuccess(t, 1, 400)
	}
	before := GetChannelHealthScore(1)

	for i := 0; i < 20; i++ {
		RecordChannelObservation(1, ChannelObservation{Kind: OutcomeClientError, LatencyMs: 50})
	}
	after := GetChannelHealthScore(1)

	if after < before {
		t.Fatalf("client errors degraded health: %.3f -> %.3f", before, after)
	}
}

// TestRateLimitIsAttributed asserts 429s land on the rate-limit signal rather
// than being conflated with generic server errors.
func TestRateLimitIsAttributed(t *testing.T) {
	resetChannelHealthForTest(t)

	for i := 0; i < 10; i++ {
		RecordChannelObservation(1, ChannelObservation{Kind: OutcomeRateLimit, LatencyMs: 100})
	}

	snapshot := GetChannelHealthSnapshot(1)
	if snapshot.RateLimitRate <= 0 {
		t.Fatal("expected a non-zero rate-limit rate")
	}
	if snapshot.CutRate != 0 {
		t.Fatalf("rate limiting should not register as stream cuts, got %v", snapshot.CutRate)
	}
	if GetChannelHealthBand(1) == BandHealthy {
		t.Fatal("a channel rate limited on every request should not be healthy")
	}
}

// TestAutoRecoverRequiresConsecutiveCleanProbes asserts the recovery gate is
// stricter than "the last test passed".
func TestAutoRecoverRequiresConsecutiveCleanProbes(t *testing.T) {
	resetChannelHealthForTest(t)

	for i := 0; i < 5; i++ {
		RecordChannelObservation(1, ChannelObservation{Kind: OutcomeServerError, LatencyMs: 500})
	}
	if recovered, _ := ShouldAutoRecoverChannel(1); recovered {
		t.Fatal("a failing channel must not be eligible for recovery")
	}

	// The success-rate window is what ultimately lifts the score, so keep
	// probing until it has enough clean results to clear the degraded
	// threshold, then assert the consecutive-probe gate is what blocks
	// recovery in the meantime.
	required := config.ChannelAutoRecoverConsecutive
	if required < 1 {
		required = 1
	}
	for i := 0; i < 20; i++ {
		recordSuccess(t, 1, 300)
		if ConsecutiveCleanProbes(1) < required {
			if recovered, why := ShouldAutoRecoverChannel(1); recovered {
				t.Fatalf("recovered after only %d clean probes (required %d): %s",
					ConsecutiveCleanProbes(1), required, why)
			}
		}
		if GetChannelHealthScore(1) >= config.ChannelHealthDegradedThreshold &&
			ConsecutiveCleanProbes(1) >= required {
			if recovered, why := ShouldAutoRecoverChannel(1); !recovered {
				t.Fatalf("score recovered but still blocked after %d clean probes: %s",
					ConsecutiveCleanProbes(1), why)
			}
			return
		}
	}
	t.Fatalf("never became eligible after 20 clean probes (score %.3f)",
		GetChannelHealthScore(1))
}

// TestAutoRecoverBrokenByIntermittentFailure asserts the consecutive-probe
// counter resets when a probe fails, so a provider that alternates
// pass/fail cannot ratchet its way back into rotation.
func TestAutoRecoverBrokenByIntermittentFailure(t *testing.T) {
	resetChannelHealthForTest(t)

	for i := 0; i < 30; i++ {
		recordSuccess(t, 1, 300)
	}
	if ConsecutiveCleanProbes(1) < 2 {
		t.Fatal("setup expected a clean-probe streak")
	}

	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeServerError, LatencyMs: 500})
	if got := ConsecutiveCleanProbes(1); got != 0 {
		t.Fatalf("a failure must reset the clean-probe streak, got %d", got)
	}
	if recovered, why := ShouldAutoRecoverChannel(1); recovered {
		t.Fatalf("recovered immediately after a failure: %s", why)
	}
}

// TestAutoRecoverRejectedBySluggishProbes asserts a channel that answers but
// answers badly does not get put back into rotation.
func TestAutoRecoverRejectedBySluggishProbes(t *testing.T) {
	resetChannelHealthForTest(t)

	for i := 0; i < 5; i++ {
		RecordChannelObservation(1, ChannelObservation{Kind: OutcomeTimeout, LatencyMs: 60000})
	}
	for i := 0; i < config.ChannelAutoRecoverConsecutive+2; i++ {
		// Correct answers, but far past the slow threshold.
		RecordChannelObservation(1, ChannelObservation{
			Kind: OutcomeSuccess, LatencyMs: 30000, TTFTMs: 1000,
			GenerationMs: 29000, CompletionTokens: 10,
		})
	}
	if recovered, why := ShouldAutoRecoverChannel(1); recovered {
		t.Fatalf("a channel answering in 30s must not recover: %s", why)
	}
}

// TestLinearScoreInterpolation covers the threshold helper directly, including
// the "signal unavailable" case that must not be scored as zero.
func TestLinearScoreInterpolation(t *testing.T) {
	cases := []struct {
		name  string
		value float64
		good  float64
		bad   float64
		want  float64
	}{
		{"unavailable signal is exempt", 0, 1000, 5000, 1.0},
		{"at or better than good", 500, 1000, 5000, 1.0},
		{"at or worse than bad", 9000, 1000, 5000, 0.0},
		{"midpoint", 3000, 1000, 5000, 0.5},
		// 2000ms is a quarter of the way from good(1000) to bad(5000), so
		// it keeps three quarters of the score.
		{"three quarters of the score", 2000, 1000, 5000, 0.75},
		{"past bad clamps to zero", 9999, 1000, 5000, 0.0},
		{"degenerate good==bad is exempt", 3000, 5000, 5000, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := linearScore(tc.value, tc.good, tc.bad)
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("linearScore(%v, %v, %v) = %v, want %v", tc.value, tc.good, tc.bad, got, tc.want)
			}
		})
	}
}

// TestClassifyRelayError covers the mapping from relay failures to health
// signals, including the distinction between scoring and non-scoring failures.
func TestClassifyRelayError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		code      string
		message   string
		streamCut bool
		wantKind  OutcomeKind
		wantScore bool
	}{
		{"stream cut wins over status", 200, "", "upstream_cut", true, OutcomeStreamCut, true},
		{"unauthorized", 401, "", "", false, OutcomeAuthError, true},
		{"forbidden", 403, "", "", false, OutcomeAuthError, true},
		{"rate limited", 429, "", "", false, OutcomeRateLimit, true},
		{"timeout status", 408, "", "", false, OutcomeTimeout, true},
		{"server error", 502, "", "", false, OutcomeServerError, true},
		{"bad request is not the channel's fault", 400, "", "", false, OutcomeClientError, false},
		{"not found is not the channel's fault", 404, "", "", false, OutcomeClientError, false},
		{"caller quota is not the channel's fault", 403, "insufficient_user_quota", "", false, OutcomeClientError, false},
		{"deadline in body", 200, "", "context deadline exceeded", false, OutcomeTimeout, true},
		{"rate limit in body", 200, "", "Rate limit exceeded", false, OutcomeRateLimit, true},
		{"upstream cut code", 200, "upstream_cut", "", false, OutcomeStreamCut, true},
		{"unrecognised 5xx stays server error", 599, "", "", false, OutcomeServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, scoring := ClassifyRelayError(tc.status, tc.code, tc.message, tc.streamCut)
			if kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tc.wantKind)
			}
			if scoring != tc.wantScore {
				t.Fatalf("scoring = %v, want %v", scoring, tc.wantScore)
			}
		})
	}
}
