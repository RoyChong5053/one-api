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

	"github.com/Laisky/errors/v2"
	gmw "github.com/Laisky/gin-middlewares/v7"
	"github.com/Laisky/zap"
	"github.com/gin-gonic/gin"

	"github.com/Laisky/one-api/common/ctxkey"
	"github.com/Laisky/one-api/model"
	"github.com/Laisky/one-api/relay"
	"github.com/Laisky/one-api/relay/adaptor"
	"github.com/Laisky/one-api/relay/adaptor/openai"
	metalib "github.com/Laisky/one-api/relay/meta"
	relaymodel "github.com/Laisky/one-api/relay/model"
	"github.com/Laisky/one-api/relay/relaymode"
)

const (
	// FanOutMinBatchSize is the minimum input array length to trigger fan-out.
	FanOutMinBatchSize = 10
)

// fanoutRerankResultItem mirrors the rerank result structure for fan-out merging.
type fanoutRerankResultItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
	Document       string  `json:"document,omitempty"`
}

// fanOutEmbeddingResult holds the result from a single channel's embedding sub-request.
type fanOutEmbeddingResult struct {
	data  []openai.EmbeddingResponseItem
	usage relaymodel.Usage
	err   error
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
	if len(inputs) < FanOutMinBatchSize {
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
			results[idx] = fanOutEmbeddingSingle(nil, c, meta, channel, textRequest, splits[idx])
		}(i, ch)
	}
	wg.Wait()

	// Check for errors — if any sub-request failed, fall back to single channel
	for _, r := range results {
		if r.err != nil {
			lg.Warn("fan-out sub-request failed, falling back to single channel",
				zap.Error(r.err))
			return nil
		}
	}

	// Merge results in original order
	merged := mergeEmbeddingResults(results)
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
) fanOutEmbeddingResult {
	if len(texts) == 0 {
		return fanOutEmbeddingResult{}
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
		data:  embeddingResp.Data,
		usage: embeddingResp.Usage,
	}
}

// splitInputsByWeight distributes items across channels proportionally to their weights.
func splitInputsByWeight(items []string, channels []*model.Channel) [][]string {
	if len(channels) == 0 {
		return nil
	}
	if len(channels) == 1 {
		return [][]string{items}
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

	result := make([][]string, len(channels))
	offset := 0
	for i, count := range counts {
		result[i] = items[offset : offset+count]
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

	offset := 0
	for _, r := range results {
		for _, item := range r.data {
			item.Index = offset
			merged = append(merged, item)
			offset++
		}
		totalUsage.PromptTokens += r.usage.PromptTokens
		totalUsage.CompletionTokens += r.usage.CompletionTokens
		totalUsage.TotalTokens += r.usage.TotalTokens
	}

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
	if len(rerankRequest.Documents) < FanOutMinBatchSize {
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
			results[idx] = fanOutRerankSingle(nil, c, meta, channel, rerankRequest, docSplits[idx])
		}(i, ch)
	}
	wg.Wait()

	for _, r := range results {
		if r.err != nil {
			lg.Warn("rerank fan-out sub-request failed, falling back",
				zap.Error(r.err))
			return nil
		}
	}

	merged := mergeRerankResults(results, rerankRequest.TopN)
	return &FanOutRerankResponse{
		Response: merged,
		Usage:    merged["usage"].(relaymodel.Usage),
	}
}

// fanOutRerankSingle sends a rerank sub-request to a single channel.
func fanOutRerankSingle(
	_ context.Context,
	originalCtx *gin.Context,
	origMeta *metalib.Meta,
	channel *model.Channel,
	origRequest *relaymodel.RerankRequest,
	documents []string,
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
// relevance score, and applies top_n.
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

	for i := range allResults {
		allResults[i].Index = i
	}

	return map[string]any{
		"object":  "list",
		"results": allResults,
		"usage":   totalUsage,
	}
}
