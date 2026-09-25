package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Laisky/errors/v2"
	gmw "github.com/Laisky/gin-middlewares/v7"
	"github.com/Laisky/zap"
	"github.com/gin-gonic/gin"

	"github.com/Laisky/one-api/common/config"
	"github.com/Laisky/one-api/common/ctxkey"
	"github.com/Laisky/one-api/model"
	"github.com/Laisky/one-api/monitor"
	"github.com/Laisky/one-api/relay"
	"github.com/Laisky/one-api/relay/adaptor"
	"github.com/Laisky/one-api/relay/adaptor/openai"
	metalib "github.com/Laisky/one-api/relay/meta"
	relaymodel "github.com/Laisky/one-api/relay/model"
	"github.com/Laisky/one-api/relay/relaymode"
)

const (
	// defaultFanOutMinBatchSize is used only when the runtime config value is
	// unavailable (e.g. in unit tests that do not initialize config).
	defaultFanOutMinBatchSize = 10
)

func fanOutMinBatchSize() int {
	if config.FanOutMinBatchSize > 0 {
		return config.FanOutMinBatchSize
	}
	return defaultFanOutMinBatchSize
}

// fanoutBackoffDuration mirrors the non-auth branch of the relay circuit
// breaker: base * multiplier^(failures-1), capped at max. Keeps fan-out
// sub-request failures on the same suspension schedule as single-channel
// relay errors so an offline node is skipped within minutes, not hours.
func fanoutBackoffDuration(channelId int) time.Duration {
	failures := model.GetConsecutiveChannelFailures(channelId)
	if failures < 1 {
		failures = 1
	}
	duration := config.ChannelSuspendBackoffBase
	for i := 1; i < failures; i++ {
		duration *= time.Duration(config.ChannelSuspendBackoffMultiplier)
		if duration >= config.ChannelSuspendBackoffMax {
			return config.ChannelSuspendBackoffMax
		}
	}
	return duration
}

// reportFanoutSubRequest feeds one fan-out sub-request outcome back into the
// channel health system. Previously fan-out failures were only logged, so an
// offline node kept receiving slices on every request (full tail latency each
// time). Now a failure suspends the ability (fast circuit breaker) and a
// success resets the consecutive-failure counter.
func reportFanoutSubRequest(ctx context.Context, group, modelName string, channelId int, err error) {
	if channelId == 0 {
		return
	}
	if err != nil {
		monitor.Emit(channelId, false)
		backoff := fanoutBackoffDuration(channelId)
		_ = model.SuspendAbility(ctx, group, modelName, channelId, backoff)
		return
	}
	monitor.Emit(channelId, true)
	model.RecordChannelSuccess(channelId)
	model.ResetConsecutiveChannelFailures(channelId)
}

// fanoutRerankResultItem mirrors the rerank result structure for fan-out merging.
type fanoutRerankResultItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Document       string  `json:"document,omitempty"`
}

// fanOutEmbeddingResult holds the result from a single channel's embedding sub-request.
type fanOutEmbeddingResult struct {
	offset int // start index in the original input array
	data   []openai.EmbeddingResponseItem
	usage  relaymodel.Usage
	err    error
	// observability fields, filled in by the fan-out dispatcher (not the adaptor)
	channelId   int
	channelName string
	docCount    int
	latencyMs   int64
}

// FanOutEmbeddingResponse is the merged embedding response from fan-out.
type FanOutEmbeddingResponse struct {
	Response openai.EmbeddingResponse
	Usage    relaymodel.Usage
}

// TryFanOutEmbedding attempts to split a large embedding batch across multiple
// channels for parallel processing. Returns nil if fan-out was not performed
// (caller should use normal single-channel path).
func TryFanOutEmbedding(
	c *gin.Context,
	meta *metalib.Meta,
	textRequest *relaymodel.GeneralOpenAIRequest,
) *FanOutEmbeddingResponse {
	inputs := textRequest.ParseInput()
	if len(inputs) < fanOutMinBatchSize() {
		return nil
	}

	// Get all available channels for this group+model
	group := c.GetString(ctxkey.Group)
 modelName := textRequest.Model
	channels, err := model.GetChannelsFromCache(group, modelName)
	if err != nil || len(channels) < 2 {
		return nil
	}

	// Filter to same-priority tier (highest priority only)
	maxPriority := channels[0].GetPriority()
	var candidateChannels []*model.Channel
	for _, ch := range channels {
		if ch.GetPriority() == maxPriority && !model.IsChannelSuspendedInCache(ch.Id) {
			candidateChannels = append(candidateChannels, ch)
		}
	}
	if len(candidateChannels) < 2 {
		return nil
	}

	lg := gmw.GetLogger(c)
	lg.Info("embedding fan-out triggered",
		zap.Int("input_size", len(inputs)),
		zap.Int("channels", len(candidateChannels)))

	// Split inputs across channels proportionally by weight
	splits := splitInputsByWeight(inputs, candidateChannels)

	// Fan-out: send sub-requests in parallel
	results := make([]fanOutEmbeddingResult, len(candidateChannels))
	var wg sync.WaitGroup

	for i, ch := range candidateChannels {
		wg.Add(1)
		go func(idx int, channel *model.Channel) {
			defer wg.Done()
			start := time.Now()
			results[idx] = fanOutEmbeddingSingle(nil, c, meta, channel, textRequest, splits[idx].items, splits[idx].offset)
			results[idx].channelId = channel.Id
			results[idx].channelName = channel.Name
			results[idx].docCount = len(splits[idx].items)
			results[idx].latencyMs = time.Since(start).Milliseconds()
		}(i, ch)
	}
	wg.Wait()

	// Use results from successful sub-requests even if some failed; only fall
	// back to the single-channel path when every sub-request failed.
	// Each outcome is reported to the circuit breaker so an offline node is
	// suspended instead of receiving slices on every request.
	reportCtx := gmw.Ctx(c)
	var successful []fanOutEmbeddingResult
	for _, r := range results {
		reportFanoutSubRequest(reportCtx, group, modelName, r.channelId, r.err)
		if r.err != nil {
			lg.Warn("embedding fan-out sub-request failed, using partial results",
				zap.Int("channel_id", r.channelId),
				zap.String("channel_name", r.channelName),
				zap.Int("doc_count", r.docCount),
				zap.Int64("latency_ms", r.latencyMs),
				zap.Error(r.err))
			continue
		}
		lg.Info("embedding fan-out sub-request completed",
			zap.Int("channel_id", r.channelId),
			zap.String("channel_name", r.channelName),
			zap.Int("doc_count", r.docCount),
			zap.Int64("latency_ms", r.latencyMs))
		successful = append(successful, r)
	}
	if len(successful) == 0 {
		lg.Warn("all embedding fan-out sub-requests failed, falling back to single channel")
		return nil
	}

	// Merge results in original order
	merged := mergeEmbeddingResults(successful)
	merged.Model = textRequest.Model

	return &FanOutEmbeddingResponse{
		Response: merged,
		Usage:    merged.Usage,
	}
}

// fanOutEmbeddingSingle sends an embedding sub-request to a single channel.
func fanOutEmbeddingSingle(
	_ context.Context,
	originalCtx *gin.Context,
	origMeta *metalib.Meta,
	channel *model.Channel,
	origRequest *relaymodel.GeneralOpenAIRequest,
	texts []string,
	offset int,
) fanOutEmbeddingResult {
	if len(texts) == 0 {
		return fanOutEmbeddingResult{offset: offset}
	}

	// Build a sub-request with the sliced input
	subRequest := *origRequest
	subRequest.Input = texts

	// Create a meta clone for this channel
	subMeta := metalib.CloneMeta(origMeta, channel)

	// Create adaptor and build request
	adaptorImpl := relay.GetAdaptor(subMeta.APIType)
	if adaptorImpl == nil {
		return fanOutEmbeddingResult{
			err: errors.Errorf("invalid api type: %d", subMeta.APIType),
		}
	}
	adaptorImpl.Init(subMeta)

	// Convert request
	converted, err := adaptorImpl.ConvertRequest(originalCtx, relaymode.Embeddings, &subRequest)
	if err != nil {
		return fanOutEmbeddingResult{err: errors.Wrap(err, "convert request")}
	}

	bodyBytes, err := json.Marshal(converted)
	if err != nil {
		return fanOutEmbeddingResult{err: errors.Wrap(err, "marshal request")}
	}

	// Send request to upstream
	resp, err := adaptorImpl.DoRequest(originalCtx, subMeta, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fanOutEmbeddingResult{err: errors.Wrap(err, "do request")}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fanOutEmbeddingResult{
			err: errors.Errorf("upstream returned status %d: %s", resp.StatusCode, string(body)),
		}
	}

	// Parse response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fanOutEmbeddingResult{err: errors.Wrap(err, "read response")}
	}

	var embeddingResp openai.EmbeddingResponse
	if err := json.Unmarshal(respBody, &embeddingResp); err != nil {
		return fanOutEmbeddingResult{err: errors.Wrap(err, "unmarshal response")}
	}

	return fanOutEmbeddingResult{
		offset: offset,
		data:   embeddingResp.Data,
		usage:  embeddingResp.Usage,
	}
}

// weightedSplit is a contiguous slice of the original input together with its
// starting offset in the original array. The offset is needed by rerank fan-out
// so that each channel's returned indices can be mapped back to the original
// documents array.
type weightedSplit struct {
	offset int
	items  []string
}

// splitInputsByWeight distributes items across channels proportionally to their
// weights, preserving original order. Each returned split records where in the
// original array it starts.
func splitInputsByWeight(items []string, channels []*model.Channel) []weightedSplit {
	if len(channels) == 0 {
		return nil
	}
	if len(channels) == 1 {
		return []weightedSplit{{offset: 0, items: items}}
	}

	var totalWeight uint
	for _, ch := range channels {
		totalWeight += ch.GetWeight()
	}
	if totalWeight == 0 {
		totalWeight = uint(len(channels))
	}

	total := len(items)
	counts := make([]int, len(channels))
	assigned := 0
	for i, ch := range channels {
		counts[i] = int(math.Round(float64(total) * float64(ch.GetWeight()) / float64(totalWeight)))
		assigned += counts[i]
	}

	// Adjust for rounding errors
	diff := total - assigned
	for i := 0; diff != 0; i++ {
		if diff > 0 {
			counts[i%len(channels)]++
			diff--
		} else {
			idx := len(channels) - 1 - (i % len(channels))
			if counts[idx] > 0 {
				counts[idx]--
				diff++
			}
		}
	}

	result := make([]weightedSplit, len(channels))
	offset := 0
	for i, count := range counts {
		result[i] = weightedSplit{
			offset: offset,
			items:  items[offset : offset+count],
		}
		offset += count
	}

	return result
}

// mergeEmbeddingResults merges results from multiple channels back into a single
// response, preserving the original input order.
func mergeEmbeddingResults(results []fanOutEmbeddingResult) openai.EmbeddingResponse {
	totalItems := 0
	for _, r := range results {
		totalItems += len(r.data)
	}

	merged := make([]openai.EmbeddingResponseItem, 0, totalItems)
	var totalUsage relaymodel.Usage

	// Each result carries its start offset in the original input. A sub-request
	// returns items with indices relative to its own slice, so the global index is
	// offset + item.Index. Sorting by global index restores original input order,
	// which also holds when some sub-requests failed and are skipped.
	for _, r := range results {
		for _, item := range r.data {
			item.Index = r.offset + item.Index
			merged = append(merged, item)
		}
		totalUsage.PromptTokens += r.usage.PromptTokens
		totalUsage.CompletionTokens += r.usage.CompletionTokens
		totalUsage.TotalTokens += r.usage.TotalTokens
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Index < merged[j].Index
	})

	return openai.EmbeddingResponse{
		Object: "list",
		Data:   merged,
		Usage:  totalUsage,
	}
}

// fanOutRerankResult holds the result from a single channel's rerank sub-request.
type fanOutRerankResult struct {
	results []fanoutRerankResultItem
	usage   relaymodel.Usage
	err     error
	// observability fields, filled in by the fan-out dispatcher (not the adaptor)
	channelId   int
	channelName string
	docCount    int
	latencyMs   int64
}

// FanOutRerankResponse is the merged rerank response from fan-out.
type FanOutRerankResponse struct {
	Response map[string]any
	Usage    relaymodel.Usage
}

// TryFanOutRerank attempts to split a large rerank batch across multiple
// channels for parallel processing. Returns nil if fan-out was not performed.
func TryFanOutRerank(
	c *gin.Context,
	meta *metalib.Meta,
	rerankRequest *relaymodel.RerankRequest,
) *FanOutRerankResponse {
	if len(rerankRequest.Documents) < fanOutMinBatchSize() {
		return nil
	}

	group := c.GetString(ctxkey.Group)
	modelName := rerankRequest.Model
	channels, err := model.GetChannelsFromCache(group, modelName)
	if err != nil || len(channels) < 2 {
		return nil
	}

	maxPriority := channels[0].GetPriority()
	var candidateChannels []*model.Channel
	for _, ch := range channels {
		if ch.GetPriority() == maxPriority && !model.IsChannelSuspendedInCache(ch.Id) {
			candidateChannels = append(candidateChannels, ch)
		}
	}
	if len(candidateChannels) < 2 {
		return nil
	}

	lg := gmw.GetLogger(c)
	lg.Info("rerank fan-out triggered",
		zap.Int("document_count", len(rerankRequest.Documents)),
		zap.Int("channels", len(candidateChannels)))

	docSplits := splitInputsByWeight(rerankRequest.Documents, candidateChannels)

	results := make([]fanOutRerankResult, len(candidateChannels))
	var wg sync.WaitGroup

	for i, ch := range candidateChannels {
		wg.Add(1)
		go func(idx int, channel *model.Channel) {
			defer wg.Done()
			start := time.Now()
			results[idx] = fanOutRerankSingle(nil, c, meta, channel, rerankRequest, docSplits[idx].items, docSplits[idx].offset)
			results[idx].channelId = channel.Id
			results[idx].channelName = channel.Name
			results[idx].docCount = len(docSplits[idx].items)
			results[idx].latencyMs = time.Since(start).Milliseconds()
		}(i, ch)
	}
	wg.Wait()

	// Keep results from successful sub-requests even if some failed; only fall
	// back to the single-channel path when every sub-request failed.
	// Each outcome is reported to the circuit breaker so an offline node is
	// suspended instead of receiving slices on every request.
	reportCtx := gmw.Ctx(c)
	var successful []fanOutRerankResult
	for _, r := range results {
		reportFanoutSubRequest(reportCtx, group, modelName, r.channelId, r.err)
		if r.err != nil {
			lg.Warn("rerank fan-out sub-request failed, using partial results",
				zap.Int("channel_id", r.channelId),
				zap.String("channel_name", r.channelName),
				zap.Int("doc_count", r.docCount),
				zap.Int64("latency_ms", r.latencyMs),
				zap.Error(r.err))
			continue
		}
		lg.Info("rerank fan-out sub-request completed",
			zap.Int("channel_id", r.channelId),
			zap.String("channel_name", r.channelName),
			zap.Int("doc_count", r.docCount),
			zap.Int64("latency_ms", r.latencyMs))
		successful = append(successful, r)
	}
	if len(successful) == 0 {
		lg.Warn("all rerank fan-out sub-requests failed, falling back to single channel")
		return nil
	}

	merged := mergeRerankResults(successful, rerankRequest.TopN)
	return &FanOutRerankResponse{
		Response: merged,
		Usage:    merged["usage"].(relaymodel.Usage),
	}
}

// fanOutRerankSingle sends a rerank sub-request to a single channel. The offset
// is the position of this channel's document slice in the original documents
// array, so each returned index can be mapped back to the global array.
func fanOutRerankSingle(
	_ context.Context,
	originalCtx *gin.Context,
	origMeta *metalib.Meta,
	channel *model.Channel,
	origRequest *relaymodel.RerankRequest,
	documents []string,
	offset int,
) fanOutRerankResult {
	if len(documents) == 0 {
		return fanOutRerankResult{}
	}

	subRequest := origRequest.Clone()
	subRequest.Documents = documents

	subMeta := metalib.CloneMeta(origMeta, channel)

	adaptorImpl := relay.GetAdaptor(subMeta.APIType)
	if adaptorImpl == nil {
		return fanOutRerankResult{
			err: errors.Errorf("invalid api type: %d", subMeta.APIType),
		}
	}
	adaptorImpl.Init(subMeta)

	rerankAdaptor, ok := adaptorImpl.(adaptor.RerankAdaptor)
	if !ok {
		return fanOutRerankResult{
			err: errors.Errorf("rerank not supported by adaptor %s", adaptorImpl.GetChannelName()),
		}
	}

	converted, err := rerankAdaptor.ConvertRerankRequest(originalCtx, subRequest)
	if err != nil {
		return fanOutRerankResult{err: errors.Wrap(err, "convert rerank request")}
	}

	bodyBytes, err := json.Marshal(converted)
	if err != nil {
		return fanOutRerankResult{err: errors.Wrap(err, "marshal rerank request")}
	}

	resp, err := adaptorImpl.DoRequest(originalCtx, subMeta, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return fanOutRerankResult{err: errors.Wrap(err, "do rerank request")}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fanOutRerankResult{
			err: errors.Errorf("upstream returned status %d: %s", resp.StatusCode, string(body)),
		}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fanOutRerankResult{err: errors.Wrap(err, "read rerank response")}
	}

	var rerankResp struct {
		Results []fanoutRerankResultItem     `json:"results,omitempty"`
		Data    []fanoutRerankResultItem     `json:"data,omitempty"`
		Usage   *relaymodel.Usage  `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(respBody, &rerankResp); err != nil {
		return fanOutRerankResult{err: errors.Wrap(err, "unmarshal rerank response")}
	}

	results := rerankResp.Results
	if len(results) == 0 {
		results = rerankResp.Data
	}

	// Map each result back to its original global position in the documents array.
	for i := range results {
		results[i].Index = offset + results[i].Index
	}

	var usage relaymodel.Usage
	if rerankResp.Usage != nil {
		usage = *rerankResp.Usage
	}

	return fanOutRerankResult{
		results: results,
		usage:   usage,
	}
}

// mergeRerankResults merges rerank results from multiple channels, sorts by
// relevance score, and applies top_n. The original (global) document indices
// are preserved so the caller can map results back to the documents it sent.
func mergeRerankResults(results []fanOutRerankResult, topN *int) map[string]any {
	var allResults []fanoutRerankResultItem
	var totalUsage relaymodel.Usage

	for _, r := range results {
		allResults = append(allResults, r.results...)
		totalUsage.PromptTokens += r.usage.PromptTokens
		totalUsage.CompletionTokens += r.usage.CompletionTokens
		totalUsage.TotalTokens += r.usage.TotalTokens
	}

	sort.Slice(allResults, func(i, j int) bool {
		return allResults[i].RelevanceScore > allResults[j].RelevanceScore
	})

	if topN != nil && *topN < len(allResults) {
		allResults = allResults[:*topN]
	}

	return map[string]any{
		"object":  "list",
		"results": allResults,
		"usage":   totalUsage,
	}
}
