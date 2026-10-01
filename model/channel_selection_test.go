package model

import (
	"testing"
)

// makeChannel builds a channel at a given priority/weight for selection tests.
func makeChannel(id int, priority int64, weight uint) *Channel {
	p := priority
	w := weight
	return &Channel{
		Id:       id,
		Name:     "channel",
		Priority: &p,
		Weight:   &w,
		Status:   ChannelStatusEnabled,
	}
}

// makeHealthy feeds enough good observations for a channel to reach the
// healthy band.
func makeHealthy(t *testing.T, id int, latencyMs float64) {
	t.Helper()
	for i := 0; i < 20; i++ {
		RecordChannelObservation(id, ChannelObservation{
			Kind: OutcomeSuccess, LatencyMs: latencyMs, TTFTMs: latencyMs / 4,
			GenerationMs: latencyMs * 3 / 4, CompletionTokens: int(latencyMs / 5),
		})
	}
}

// makeUnhealthy feeds enough stream cuts to push a channel into the worst band
// despite a perfect configured weight.
func makeUnhealthy(t *testing.T, id int) {
	t.Helper()
	for i := 0; i < 20; i++ {
		RecordChannelObservation(id, ChannelObservation{
			Kind: OutcomeStreamCut, LatencyMs: 400,
		})
	}
}

// TestWeightCannotOutvoteHealth is the central guarantee: a channel configured
// with an extreme weight must receive no traffic at all while it is unhealthy.
// Under the previous weight-times-floored-health formula a weight of 100 with a
// floored health of 0.1 scored 10, beating a weight of 1 at full health scoring
// 1 — so a broken channel took roughly 91% of the traffic.
func TestWeightCannotOutvoteHealth(t *testing.T) {
	resetChannelHealthForTest(t)

	const trials = 3000
	healthy := makeChannel(1, 0, 1)
	broken := makeChannel(2, 0, 100) // extreme weight, same priority
	makeHealthy(t, 1, 300)
	makeUnhealthy(t, 2)

	if GetChannelHealthBand(2) != BandUnhealthy {
		t.Fatalf("setup: expected unhealthy, got %s", GetChannelHealthBand(2))
	}

	for i := 0; i < trials; i++ {
		picked := selectByHealthBand([]*Channel{healthy, broken}, "test-model")
		if picked.Id == broken.Id {
			t.Fatalf("channel %d was selected %d times despite an unhealthy band and weight=100",
				picked.Id, i+1)
		}
	}
}

// TestWeightBreaksTiesWithinBand asserts the operator's weight still does what
// it is for: among channels of equal health it decides the traffic split.
func TestWeightBreaksTiesWithinBand(t *testing.T) {
	resetChannelHealthForTest(t)

	light := makeChannel(1, 0, 1)
	heavy := makeChannel(2, 0, 3)
	makeHealthy(t, 1, 300)
	makeHealthy(t, 2, 300)

	if GetChannelHealthBand(1) != BandHealthy || GetChannelHealthBand(2) != BandHealthy {
		t.Fatal("setup: both channels should be healthy")
	}

	const trials = 20000
	hits := map[int]int{}
	for i := 0; i < trials; i++ {
		hits[selectByHealthBand([]*Channel{light, heavy}, "test-model").Id]++
	}

	// Expect roughly a 1:3 split. Allow generous slack for randomness.
	ratio := float64(hits[2]) / float64(hits[1])
	if ratio < 2.4 || ratio > 3.6 {
		t.Fatalf("weight 3:1 split not honoured, got ratio %.2f (hits %v)", ratio, hits)
	}
}

// TestDegradedUsedOnlyWhenNoHealthyExists asserts the middle band behaves as a
// fallback rather than competing with healthy channels.
func TestDegradedUsedOnlyWhenNoHealthyExists(t *testing.T) {
	resetChannelHealthForTest(t)

	healthy := makeChannel(1, 0, 100)
	degraded := makeChannel(2, 0, 1)
	makeHealthy(t, 1, 300)

	// Force the degraded band by flooding failures after a warm-up.
	for i := 0; i < 5; i++ {
		RecordChannelObservation(2, ChannelObservation{Kind: OutcomeSuccess, LatencyMs: 20000, TTFTMs: 2000, GenerationMs: 18000, CompletionTokens: 5})
	}
	for i := 0; i < 20; i++ {
		RecordChannelObservation(2, ChannelObservation{Kind: OutcomeStreamCut, LatencyMs: 20000})
	}
	if band := GetChannelHealthBand(2); band != BandDegraded && band != BandUnhealthy {
		t.Fatalf("setup: expected a non-healthy band, got %s", band)
	}

	// A healthy channel exists, so the degraded one must never be chosen even
	// though the healthy one carries 100x the weight.
	for i := 0; i < 1000; i++ {
		if picked := selectByHealthBand([]*Channel{healthy, degraded}, "test-model"); picked.Id == degraded.Id {
			t.Fatalf("degraded channel chosen while a healthy channel was available (weight 1 vs 100)")
		}
	}

	// With the healthy channel removed, the degraded one has to carry the load
	// rather than the request failing outright.
	picked := selectByHealthBand([]*Channel{degraded}, "test-model")
	if picked == nil || picked.Id != degraded.Id {
		t.Fatal("degraded channel must be used when it is the only option")
	}
}

// TestAllUnhealthyStillServes asserts a total collapse degrades into spreading
// traffic rather than an outright outage.
func TestAllUnhealthyStillServes(t *testing.T) {
	resetChannelHealthForTest(t)

	a := makeChannel(1, 0, 1)
	b := makeChannel(2, 0, 1)
	makeUnhealthy(t, 1)
	makeUnhealthy(t, 2)

	picked := selectByHealthBand([]*Channel{a, b}, "test-model")
	if picked == nil {
		t.Fatal("selection must still return a channel when every candidate is unhealthy")
	}
}

// TestEmptyAndSingleCandidates covers the degenerate inputs.
func TestEmptyAndSingleCandidates(t *testing.T) {
	resetChannelHealthForTest(t)

	if got := selectByHealthBand(nil, "test-model"); got != nil {
		t.Fatalf("empty input should select nothing, got %v", got)
	}

	only := makeChannel(7, 0, 5)
	makeUnhealthy(t, 7)
	if got := selectByHealthBand([]*Channel{only}, "test-model"); got == nil || got.Id != 7 {
		t.Fatal("a lone candidate must be selected regardless of its band")
	}
}

// TestZeroWeightIsTreatedAsOne asserts a channel left at the default weight of
// zero still participates, rather than silently never being picked.
func TestZeroWeightIsTreatedAsOne(t *testing.T) {
	resetChannelHealthForTest(t)

	unweighted := makeChannel(1, 0, 0)
	weighted := makeChannel(2, 0, 1)
	makeHealthy(t, 1, 300)
	makeHealthy(t, 2, 300)

	for i := 0; i < 500; i++ {
		if got := selectByHealthBand([]*Channel{unweighted, weighted}, "test-model"); got == nil {
			t.Fatal("selection returned nil")
		}
	}

	// Both weights normalise to 1, so both must appear.
	hits := map[int]int{}
	for i := 0; i < 5000; i++ {
		hits[selectByHealthBand([]*Channel{unweighted, weighted}, "test-model").Id]++
	}
	if hits[1] == 0 {
		t.Fatal("a weight-0 channel must still receive traffic alongside a weight-1 peer")
	}
}

// TestLegacySelectByHealthWeightAlias asserts the retained wrapper still gates
// on bands, so any caller left on the old name inherits the new behaviour.
func TestLegacySelectByHealthWeightAlias(t *testing.T) {
	resetChannelHealthForTest(t)

	healthy := makeChannel(1, 0, 1)
	broken := makeChannel(2, 0, 100)
	makeHealthy(t, 1, 300)
	makeUnhealthy(t, 2)

	for i := 0; i < 2000; i++ {
		if got := selectByHealthWeight([]*Channel{healthy, broken}, "test-model"); got.Id != 1 {
			t.Fatalf("legacy entry point bypassed the band gate and picked channel %d", got.Id)
		}
	}
}
