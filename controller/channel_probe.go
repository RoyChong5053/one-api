package controller

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gmw "github.com/Laisky/gin-middlewares/v7"
	glog "github.com/Laisky/go-utils/v6/log"
	"github.com/Laisky/zap"
	"github.com/gin-gonic/gin"

	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/ctxkey"
	"github.com/Laisky/one-api/common/logger"
	"github.com/Laisky/one-api/model"
	"github.com/Laisky/one-api/monitor"
	relaymodel "github.com/Laisky/one-api/relay/model"
	"github.com/Laisky/one-api/relay/relaymode"
)

// ---------------------------------------------------------------------------
// Background channel health probing
//
// The health engine learns from live traffic, which leaves a gap: a channel that
// goes bad while idle keeps whatever score it last earned, because nobody is
// sending it anything to score. This file closes that gap by spending a small
// number of probe requests on the channels that most need one, and by gating
// auto-recovery on repeated clean probes rather than a single lucky test.
// ---------------------------------------------------------------------------

// throughputProbePrompt asks for a bounded, mostly-deterministic completion.
// The legacy probe prompt ("2 + 2 = ?") yields a handful of tokens, which is
// enough to measure latency but completely useless for throughput: a provider
// streaming at one token per second returns it just as fast as a fast one.
// Asking for a numbered list gives the health engine a token count and a
// generation span to divide, which is what makes "fake rate limiting" visible
// as a throughput collapse rather than an invisible slow success.
const throughputProbePrompt = "Count from 1 to 60 in decimal, one number per line. Output only the numbers."

// throughputProbeMaxTokens caps the probe completion so a misbehaving model
// cannot burn quota on an unexpectedly long answer.
const throughputProbeMaxTokens = 256

// buildThroughputProbeRequest builds the health probe. It reuses the manual
// test prompt when the operator has explicitly overridden it, because a custom
// prompt usually exists for a reason.
func buildThroughputProbeRequest(modelName string) *relaymodel.GeneralOpenAIRequest {
	prompt := throughputProbePrompt
	maxTokens := throughputProbeMaxTokens
	if strings.TrimSpace(config.TestPrompt) != "" &&
		config.TestPrompt != "2 + 2 = ?" {
		prompt = config.TestPrompt
	}
	if config.TestMaxTokens > 0 {
		maxTokens = config.TestMaxTokens
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = "gpt-4o-mini"
	}
	return &relaymodel.GeneralOpenAIRequest{
		MaxTokens: maxTokens,
		Model:     modelName,
		Messages: []relaymodel.Message{
			{Role: "user", Content: prompt},
		},
	}
}

// populateProbeMetrics fills the health signals a probe can produce.
func populateProbeMetrics(probe *model.ChannelObservation, c *gin.Context, startTime time.Time, usage *relaymodel.Usage) {
	if probe == nil {
		return
	}
	if usage != nil {
		probe.CompletionTokens = usage.CompletionTokens
	}
	if v, ok := c.Get(ctxkey.UpstreamFirstByteAt); ok {
		if firstByteAt, ok := v.(time.Time); ok && !firstByteAt.Before(startTime) {
			probe.TTFTMs = float64(firstByteAt.Sub(startTime).Microseconds()) / 1000.0
			// For a non-streaming probe the whole body arrives at once, so
			// the generation span is the tail after the first byte.
			if gen := time.Since(firstByteAt); gen > 0 {
				probe.GenerationMs = float64(gen.Microseconds()) / 1000.0
			}
		}
	}
}

// probeChannel sends one health probe and returns the observation to record.
func probeChannel(ctx context.Context, lg *glog.LoggerT, channel *model.Channel) (model.ChannelObservation, bool) {
	obs := model.ChannelObservation{}

	modelName := probeModelFor(channel)
	if modelName == "" {
		// A channel that advertises no models cannot be probed, and
		// guessing would spend quota to learn something already known.
		obs.Kind = model.OutcomeClientError
		return obs, false
	}

	probe := &model.ChannelObservation{}
	tik := time.Now()
	var err error
	var openaiErr *relaymodel.Error

	switch getChannelTestMode(channel) {
	case relaymode.Rerank:
		// Rerank and embedding endpoints have no streaming notion of
		// throughput; the latency signal still applies.
		_, err, openaiErr = testRerankChannel(ctx, channel, buildRerankTestRequest(modelName))
	case relaymode.Embeddings:
		_, err, openaiErr = testEmbeddingChannel(ctx, channel, buildEmbeddingTestRequest(modelName))
	default:
		_, err, openaiErr = testChannel(ctx, channel, buildThroughputProbeRequest(modelName), probe)
	}

	latency := time.Since(tik)
	obs.LatencyMs = float64(latency.Microseconds()) / 1000.0
	obs.CompletionTokens = probe.CompletionTokens
	obs.TTFTMs = probe.TTFTMs
	obs.GenerationMs = probe.GenerationMs
	if obs.GenerationMs <= 0 {
		// Non-streaming probe: the entire latency is the generation span.
		obs.GenerationMs = obs.LatencyMs
	}

	if err != nil || openaiErr != nil {
		obs.Kind = classifyProbeFailure(openaiErr, err)
		return obs, false
	}

	// A probe that answers correctly but far too slowly is a degraded
	// channel, not a working one. Recording the latency as a success and
	// letting the score penalise it keeps a single decision in one place.
	obs.Kind = model.OutcomeSuccess
	lg.Debug("channel health probe completed",
		zap.Int("channel_id", channel.Id),
		zap.String("model", modelName),
		zap.Float64("latency_ms", obs.LatencyMs),
		zap.Float64("ttft_ms", obs.TTFTMs),
		zap.Float64("tps", probeTPS(obs)),
	)
	return obs, true
}

func probeTPS(obs model.ChannelObservation) float64 {
	if obs.GenerationMs <= 0 || obs.CompletionTokens <= 0 {
		return 0
	}
	return float64(obs.CompletionTokens) / (obs.GenerationMs / 1000.0)
}

func classifyProbeFailure(openaiErr *relaymodel.Error, err error) model.OutcomeKind {
	statusCode := 0
	code := ""
	message := ""
	if openaiErr != nil {
		code = fmt.Sprint(openaiErr.Code)
		message = openaiErr.Message
	}
	if err != nil {
		if message == "" {
			message = err.Error()
		}
		// testChannel reports non-2xx upstream responses as a plain error
		// carrying the status in its text, since the typed carrier only
		// carries the message.
		if m := probeStatusPattern.FindStringSubmatch(err.Error()); len(m) == 2 {
			if parsed, convErr := strconv.Atoi(m[1]); convErr == nil {
				statusCode = parsed
			}
		}
	}
	kind, scoring := model.ClassifyRelayError(statusCode, code, message, false)
	if !scoring {
		// A probe the channel could not even answer is a channel problem
		// regardless of the status it replied with.
		return model.OutcomeServerError
	}
	return kind
}

// probeStatusPattern extracts the upstream status from the error text built by
// the channel test helpers ("http status code: [429] ...").
var probeStatusPattern = regexp.MustCompile(`http status code: \[(\d{3})\]`)

// probeModelFor picks which model a probe should exercise.
func probeModelFor(channel *model.Channel) string {
	if channel.TestingModel != nil && strings.TrimSpace(*channel.TestingModel) != "" {
		return *channel.TestingModel
	}
	return channel.GetCheapestSupportedModel()
}

// probeTargets picks the channels worth spending a probe on this tick.
//
// The selection is deliberately narrow: channels that are demonstrably working
// on live traffic need no probe, and probing all of them would burn upstream
// quota to re-learn facts the traffic already established. What gets probed is
// the set the engine actually has questions about — channels carrying a recent
// failure, and channels whose record has gone stale.
func probeTargets() []*model.Channel {
	channels, err := model.GetAllChannels(0, 0, "all", "", "")
	if err != nil {
		return nil
	}

	targets := make([]*model.Channel, 0, len(channels))
	for _, channel := range channels {
		if channel.Status != model.ChannelStatusEnabled {
			continue
		}
		// Free channels opt out of proactive probing to protect quota; local
		// channels keep it (probing them is free).
		if !channel.Policy().ProactiveProbe {
			continue
		}
		if model.ChannelNeedsProbe(channel.Id) {
			targets = append(targets, channel)
		}
	}

	// Stalest first, so when the per-run cap bites it drops the channels
	// that need the answer least.
	sort.SliceStable(targets, func(i, j int) bool {
		si := model.GetChannelHealthSnapshot(targets[i].Id)
		sj := model.GetChannelHealthSnapshot(targets[j].Id)
		if si.UpdatedTime != sj.UpdatedTime {
			return si.UpdatedTime < sj.UpdatedTime
		}
		return si.Score < sj.Score
	})
	return targets
}

// runHealthProbeTick probes the selected channels concurrently and folds the
// results into the health engine. Exported for tests and for the manual
// "probe now" endpoint.
func runHealthProbeTick(ctx context.Context) int {
	targets := probeTargets()
	limit := config.ChannelHealthProbeMaxPerRun
	if limit <= 0 {
		limit = 8
	}
	if len(targets) > limit {
		targets = targets[:limit]
	}
	if len(targets) == 0 {
		return 0
	}

	concurrency := config.ChannelHealthProbeConcurrency
	if concurrency <= 0 {
		concurrency = 4
	}
	if concurrency > len(targets) {
		concurrency = len(targets)
	}

	lg := logger.Logger.Named("channel_health_probe")
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	probed := 0

	for _, channel := range targets {
		wg.Add(1)
		go func(ch *model.Channel) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Spread probes across the tick window instead of bursting, so
			// a fleet-wide sweep does not look like a load spike to the
			// upstreams being probed.
			time.Sleep(time.Duration(rand.Int63n(int64(time.Second * 5))))

			obs, ok := probeChannel(ctx, lg, ch)
			model.RecordChannelObservation(ch.Id, obs)
			probed++

			if ok {
				ch.UpdateResponseTime(int64(obs.LatencyMs))
				applyProbeToChannelState(ctx, lg, ch, obs)
			}
		}(channel)
	}
	wg.Wait()

	lg.Info("channel health probe tick finished",
		zap.Int("probed", probed),
		zap.Int("candidates", len(targets)),
	)
	return probed
}

// applyProbeToChannelState reacts to a probe result at the channel level rather
// than the score level: a hung channel gets disabled, and an auto-disabled
// channel that has proven itself gets re-enabled.
func applyProbeToChannelState(ctx context.Context, lg *glog.LoggerT, ch *model.Channel, obs model.ChannelObservation) {
	// Latency past the hang threshold means the channel is not slow, it is
	// wedged. Disable it outright rather than merely downranking it.
	if obs.Kind == model.OutcomeSuccess && obs.LatencyMs >= float64(config.ChannelHealthDisableLatencyMs) {
		reason := formatProbeLatencyDisable(obs.LatencyMs)
		lg.Warn("channel auto-disabled after hung health probe",
			zap.Int("channel_id", ch.Id),
			zap.String("channel_name", ch.Name),
			zap.Float64("latency_ms", obs.LatencyMs),
		)
		monitor.DisableChannel(ch.Id, ch.Name, reason)
		return
	}

	// Only auto-disabled channels are eligible for automatic recovery; a
	// manually disabled channel is never touched.
	if ch.Status != model.ChannelStatusAutoDisabled {
		return
	}
	// One clean probe is enough: clear the accumulated failure history so the
	// channel returns at full score and is judged again from live traffic.
	model.ResetChannelHealthToFull(ch.Id)
	monitor.EnableChannel(ch.Id, ch.Name)
	lg.Info("auto-disabled channel recovered by health probe",
		zap.Int("channel_id", ch.Id),
		zap.String("channel_name", ch.Name),
		zap.Float64("latency_ms", obs.LatencyMs),
	)
}

func formatProbeLatencyDisable(latencyMs float64) string {
	return fmt.Sprintf("health probe took longer than the hang threshold (%.2fs)", latencyMs/1000.0)
}

// ProbeChannels runs one health probe tick on demand and returns how many
// channels were probed. It lets an operator force a refresh after changing a
// channel instead of waiting for the background interval.
func ProbeChannels(c *gin.Context) {
	probed := runHealthProbeTick(gmw.Ctx(c))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    gin.H{"probed": probed},
	})
}

// EnforceUnhealthyChannelDisable auto-disables enabled channels whose composite
// health score has fallen below ChannelHealthAutoDisableThreshold.
//
// Band gating alone only downranks a low-scoring channel: with healthy peers
// present it receives no traffic, so it never accumulates the observations that
// would let it recover and simply sleeps. Auto-disabling it instead routes it
// through the periodic recovery test, which clears the history and returns it
// at full score as soon as it answers. Manually disabled channels are never
// touched.
func EnforceUnhealthyChannelDisable() int {
	threshold := config.ChannelHealthAutoDisableThreshold
	if threshold <= 0 {
		return 0
	}
	channels, err := model.GetAllChannels(0, 0, "all", "", "")
	if err != nil {
		return 0
	}
	lg := logger.Logger.Named("channel_health")
	disabled := 0
	for _, ch := range channels {
		if ch == nil || ch.Status != model.ChannelStatusEnabled {
			continue
		}
		// Performance-driven auto-disable is a paid-fleet optimisation. Free and
		// local channels are never taken out of rotation for being slow; they are
		// only downranked by the health band.
		if !ch.Policy().ScoreMayDisable {
			continue
		}
		if !model.GetChannelHealthScoreBelowThreshold(ch.Id, threshold) {
			continue
		}
		snap := model.GetChannelHealthSnapshot(ch.Id)
		reason := fmt.Sprintf("health score %.2f fell below the auto-disable threshold %.2f", snap.Score, threshold)
		lg.Warn("auto-disabling unhealthy channel",
			zap.Int("channel_id", ch.Id),
			zap.String("channel_name", ch.Name),
			zap.Float64("score", snap.Score),
			zap.Float64("threshold", threshold),
			zap.Strings("reasons", snap.Reasons),
		)
		monitor.DisableChannel(ch.Id, ch.Name, reason)
		disabled++
	}
	return disabled
}

// AutomaticallyDisableUnhealthyChannels periodically sweeps enabled channels
// and auto-disables any whose health score has collapsed.
func AutomaticallyDisableUnhealthyChannels(ctx context.Context) {
	if config.ChannelHealthAutoDisableThreshold <= 0 {
		return
	}
	lg := logger.Logger.Named("channel_health")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			lg.Info("unhealthy channel enforcement stopped")
			return
		case <-ticker.C:
			if n := EnforceUnhealthyChannelDisable(); n > 0 {
				lg.Info("auto-disabled unhealthy channels", zap.Int("count", n))
			}
		}
	}
}

// AutomaticallyProbeChannelHealth runs the health prober on an interval until
// the context is cancelled.
//
// Unlike AutomaticallyTestChannels this uses a cancellable context, so the
// prober shuts down with the rest of the process instead of outliving it.
func AutomaticallyProbeChannelHealth(ctx context.Context, frequencyMinutes int) {
	if frequencyMinutes <= 0 {
		return
	}
	lg := logger.Logger.Named("channel_health_probe")
	ticker := time.NewTicker(time.Duration(frequencyMinutes) * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			lg.Info("channel health prober stopped")
			return
		case <-ticker.C:
			runHealthProbeTick(ctx)
		}
	}
}
