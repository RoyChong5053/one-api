package model

import (
	"encoding/json"
	"testing"
)

// TestHealthSnapshotJSONShape pins the wire format of the snapshot the admin UI
// consumes. The frontend ChannelHealth interface in
// web/modern/src/pages/channels/components/ChannelHealthBadge.tsx depends on
// these keys, so a rename here has to be mirrored there.

func TestHealthSnapshotJSONShape(t *testing.T) {
	resetChannelHealthForTest(t)
	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeSuccess, LatencyMs: 900, TTFTMs: 240, GenerationMs: 660, CompletionTokens: 33})
	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeSuccess, LatencyMs: 900, TTFTMs: 240, GenerationMs: 660, CompletionTokens: 33})
	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeStreamCut, LatencyMs: 900})
	RecordChannelObservation(1, ChannelObservation{Kind: OutcomeRateLimit, LatencyMs: 900})

	AttachHealthSnapshots([]*Channel{{Id: 1, Name: "c"}})
	snap := ChannelHealthSnapshots()[0]
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	// The key the frontend reads must always be present.
	if snap.Reasons == nil {
		t.Fatal("reasons must serialise as [] not null")
	}
	var probe map[string]any
	_ = json.Unmarshal(raw, &probe)
	for _, key := range []string{"score", "band", "success_rate", "latency_ms", "ttft_ms", "tps", "cut_rate", "rate_limit_rate", "samples", "reasons"} {
		if _, ok := probe[key]; !ok {
			t.Fatalf("missing JSON key %q", key)
		}
	}
}
