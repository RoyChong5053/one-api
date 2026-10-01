package monitor

import (
	"github.com/Laisky/one-api/common/config"
)

// Emit reports a channel outcome.
//
// Historically this fed a second, independent success-rate store that
// auto-disabled any channel dipping below MetricSuccessRateThreshold. That
// store has been retired: channel health now lives in the model layer's health
// engine (model.RecordChannelObservation), which is also what channel selection
// gates on.
//
// Keeping both was actively harmful rather than merely redundant. The two used
// different windows and different thresholds, so they disagreed about the same
// channel: a channel at 85% success rated healthy by the routing engine — and
// therefore given traffic — was simultaneously being disabled by this path for
// being below the 80% bar. Worse, this store was a plain map read and written
// by two independent consumer goroutines with no lock, so under
// ENABLE_METRIC=true it raced on every relay.
//
// Emit is kept as a no-op so the ~15 call sites across the relay path do not
// have to change; it remains a useful seam for future metrics.
func Emit(channelId int, success bool) {
	if !config.EnableMetric {
		return
	}
	// Intentionally empty: see the note above.
	_ = channelId
	_ = success
}
